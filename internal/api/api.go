// Package api exposes the recogn engine over HTTP: a JSON REST API plus the
// embedded web UI, in the multi-user model. It depends on a small Engine
// interface (not the concrete type) so handlers are testable with a stub.
//
// Access model (see access.go): the admin manages repositories, users,
// grants, share links and the threshold; authenticated users act on a
// repository per their reader/writer grant; share tokens read exactly one
// repository. Scoped routes mount behind s.scoped (the uniform
// anonymous-401 gate that share tokens pass without a session) and enforce
// their role via requireRepoRole; admin routes behind auth.RequireAdmin.
package api

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"recogn/internal/auth"
	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
	"recogn/internal/web"
)

// Engine is the behaviour the API needs from the recognition engine.
// *engine.Engine satisfies it; tests provide a stub.
type Engine interface {
	Recognize(imgBytes []byte) ([]engine.Face, error)
	// RecognizeScoped narrows matching to the given person-ID set (nil =
	// unscoped). All scoped handlers call it so a request can only ever
	// name identities of the repository it acts on.
	RecognizeScoped(imgBytes []byte, allowed map[string]struct{}) ([]engine.Face, error)
	Detect(imgBytes []byte) ([]engine.Face, error)
	EmbedFace(imgBytes []byte, f engine.Face) ([]float32, error)
	SetThreshold(t float64)
	Threshold() float64
	Ping() error
}

// maxUpload caps a single request body to 32 MiB.
const maxUpload = 32 << 20

// Server wires the HTTP routes.
type Server struct {
	cfg     config.Config
	eng     Engine
	db      *db.DB
	auth    *auth.Service            // sessions, API keys, principals, grants
	refresh func(e Engine, d *db.DB) // push DB identities into the engine
	workers int                      // batch worker count (uploads, rescan)
	// Admission control for the inference-heavy endpoints (M1): the
	// recognize token bucket and the shared recognize/compare in-flight
	// gate. See admission.go.
	inflight      *inflightGate
	recognLimiter *ipRateLimiter
	mux           *http.ServeMux
}

// New builds a Server. refresh is called after any mutation to reload the
// engine's identity set from the DB (may be nil). The auth service is built
// here too: a locked or unreadable users.db fails startup loudly.
func New(cfg config.Config, eng Engine, database *db.DB, refresh func(Engine, *db.DB)) (*Server, error) {
	authSvc, err := auth.New(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, eng: eng, db: database, auth: authSvc, refresh: refresh, workers: resolveWorkers(cfg)}
	// M1 admission control: capacity scales with the inference concurrency
	// so a slow CPU gets a proportionally smaller memory-exposure window.
	s.inflight = newInflightGate(max(s.workers*admitPerWorker, minAdmitSlots))
	s.recognLimiter = newIPRateLimiter(nil, recognizeRefillPerSec, recognizeBurst)
	s.routes()
	return s, nil
}

// Mode reports the effective auth posture (see auth.Service.Mode): "setup"
// while no user exists, else "users". main logs it at startup.
func (s *Server) Mode() string { return s.auth.Mode() }

// SetupKey exposes the one-time first-run setup key (present only while no
// user exists). main prints it to the console so the operator can complete
// the web UI's setup view.
func (s *Server) SetupKey() (string, bool) { return s.auth.SetupKey() }

// resolveWorkers returns the batch-worker count for a config: an explicit
// positive RECOGN_CONCURRENCY wins, otherwise the engine default. (Mirrors
// main.resolveWorkers; kept here because main cannot be imported.)
func resolveWorkers(cfg config.Config) int {
	if cfg.Concurrency > 0 {
		return cfg.Concurrency
	}
	return engine.DefaultConcurrency()
}

// sharePathRe matches the share-UI route: /share/<repoID>/<token> — the
// repoID is the 36-character UUID, the token the base64url segment (which
// may contain "-" and "_").
var sharePathRe = regexp.MustCompile(`^/share/[A-Za-z0-9-]{36}/[A-Za-z0-9_-]+/?$`)

func (s *Server) routes() {
	m := http.NewServeMux()
	// ---- public ----
	m.HandleFunc("/", s.handleIndex) // SPA, its assets and the /share/<id>/<token> view
	m.HandleFunc("/api/health", s.handleHealth)
	m.Handle("/api/setup", s.auth.SetupHandler()) // first-run only (403 once an admin exists)
	m.Handle("/api/login", s.auth.LoginHandler()) // rate-limited per client IP
	m.Handle("/api/logout", s.auth.LogoutHandler())
	m.Handle("/api/auth/session", s.auth.SessionHandler())
	// The owner's API keys and password change; both handlers gate
	// themselves with RequireAuth (the extra wrapper keeps the mount self-
	// documenting and costs nothing).
	m.Handle("/api/auth/keys", s.auth.RequireAuth(s.auth.APIKeysHandler()))
	m.Handle("/api/auth/keys/", s.auth.RequireAuth(s.auth.APIKeysHandler()))
	m.Handle("/api/auth/password", s.auth.RequireAuth(s.auth.ChangePasswordHandler()))

	// ---- any authenticated principal (share tokens do not apply) ----
	m.Handle("/api/repos", s.auth.RequireAuth(http.HandlerFunc(s.handleRepos)))
	m.Handle("/api/compare", s.auth.RequireAuth(http.HandlerFunc(s.handleCompare)))
	m.Handle("/api/config", s.auth.RequireAuth(http.HandlerFunc(s.handleConfig)))

	// ---- admin only ----
	m.Handle("/api/repos/{id}", s.auth.RequireAdmin(http.HandlerFunc(s.handleRepoByID)))
	m.Handle("/api/repos/{id}/rename", s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleRenameRepo(w, r, r.PathValue("id"))
	})))
	m.Handle("/api/repos/{id}/share", s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleRepoShare(w, r, r.PathValue("id"))
	})))
	m.Handle("/api/users", s.auth.RequireAdmin(http.HandlerFunc(s.handleUsers)))
	m.Handle("/api/users/{u}", s.auth.RequireAdmin(http.HandlerFunc(s.handleUserByID)))
	m.Handle("/api/users/{u}/password", s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleUserPassword(w, r, r.PathValue("u"))
	})))
	m.Handle("/api/users/{u}/repos", s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleUserGrants(w, r, r.PathValue("u"))
	})))

	// ---- repository-scoped: principals AND share tokens (access.go) ----
	// The scoped guard answers anonymous-without-share with the uniform
	// 401; each handler then enforces its role via requireRepoRole.
	scoped := s.scoped
	m.Handle("/api/recognize", scoped(http.HandlerFunc(s.handleRecognize)))
	m.Handle("/api/enroll", scoped(http.HandlerFunc(s.handleEnrollFolder)))
	m.Handle("/api/people", scoped(http.HandlerFunc(s.handlePeople)))
	// GET /api/people/{name} is the reader-visible details view; every
	// other path/method under /api/people/ is dispatched by
	// handlePersonSubroutes with per-subroute roles (writer for
	// mutations and the photos manager, reader for the detect probe).
	m.Handle("/api/people/", scoped(http.HandlerFunc(s.handlePersonSubroutes)))
	m.Handle("/api/thumbs/", scoped(http.HandlerFunc(s.handleThumb)))
	s.mux = m
}

// Handler returns the root http.Handler (useful for httptest), wrapped in the
// request logger.
func (s *Server) Handler() http.Handler { return s.loggingMiddleware(s.mux) }

// ListenAndServe starts the HTTP server on the configured address.
func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Addr, s.Handler())
}

// loggingMiddleware logs one line per request: method, path, status, duration.
// It also stamps conservative security headers on every response: the UI and
// the photo/thumbnail files are never MIME-sniffed or framed by third pages.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// statusWriter captures the response status code for logging while passing
// every Write/WriteHeader through untouched.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// ---- handlers ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Share links serve the SPA shell (root-relative asset URLs resolve
	// against "/" as usual), any other non-API path serves the built UI
	// bundle (assets/, logo.svg, ...). API routes are registered on more
	// specific patterns, so they take precedence over this catch-all.
	if sharePathRe.MatchString(r.URL.Path) {
		web.ServeUIIndex(w, r)
		return
	}
	web.ServeUI(w, r)
}

// handleHealth is the process-liveness probe (the Docker healthcheck polls
// it): a plain {"status":"ok"} with no server details.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// reload pushes DB identities into the engine after a mutation.
func (s *Server) reload() {
	if s.refresh != nil {
		s.refresh(s.eng, s.db)
	}
}
