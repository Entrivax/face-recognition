// Package api exposes the recogn engine over HTTP: a JSON REST API plus the
// embedded web UI. It depends on a small Engine interface (not the concrete
// type) so handlers are testable with a stub.
package api

import (
	"log/slog"
	"net/http"
	"strings"
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
	auth    *auth.Service            // admin auth (sessions, passkeys, rate limit)
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
// engine's identity set from the DB (may be nil). The admin auth service is
// built here too: a corrupt passkeys.json fails startup loudly.
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

// Mode reports the effective admin-auth mode (see auth.Service.Mode): one of
// "open", "password", "passkey" or "password+passkey". main logs it at
// startup.
func (s *Server) Mode() string { return s.auth.Mode() }

// admin wraps a handler in the admin auth middleware.
func (s *Server) admin(next http.Handler) http.Handler { return s.auth.Middleware(next) }

// resolveWorkers returns the batch-worker count for a config: an explicit
// positive RECOGN_CONCURRENCY wins, otherwise the engine default. (Mirrors
// main.resolveWorkers; kept here because main cannot be imported.)
func resolveWorkers(cfg config.Config) int {
	if cfg.Concurrency > 0 {
		return cfg.Concurrency
	}
	return engine.DefaultConcurrency()
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("/", s.handleIndex)
	m.HandleFunc("/api/health", s.handleHealth)
	m.HandleFunc("/api/recognize", s.handleRecognize)
	m.HandleFunc("/api/people", s.handlePeople) // public read-only list
	// GET /api/people/{name} is the public details view (name, optional
	// metadata, photo count). Every other path/method under /api/people/ —
	// mutations, the admin photos list, full-res photo files — stays behind
	// the admin middleware.
	adminPeople := s.admin(http.HandlerFunc(s.handlePersonSubroutes))
	m.Handle("/api/people/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/people/")
		if r.Method == http.MethodGet && rest != "" && !strings.Contains(rest, "/") {
			// Trim like the admin dispatcher, so "Alice%20" resolves the same.
			s.handleGetPerson(w, r, strings.TrimSpace(rest))
			return
		}
		adminPeople.ServeHTTP(w, r)
	}))
	m.Handle("/api/enroll", s.admin(http.HandlerFunc(s.handleEnrollFolder)))
	m.Handle("/api/config", s.admin(http.HandlerFunc(s.handleConfig)))
	m.Handle("/api/compare", s.admin(http.HandlerFunc(s.handleCompare)))
	m.HandleFunc("/api/thumbs/", s.handleThumb) // face thumbnails
	m.Handle("/api/login", s.auth.LoginHandler())
	m.Handle("/api/logout", s.auth.LogoutHandler())
	m.Handle("/api/auth/session", s.auth.SessionHandler())
	// Passkey ceremonies: begin/finish are dispatched by path suffix inside
	// the handlers, so each mount needs both the exact path and its subtree.
	// Registration mutates credentials → admin-only; login is public.
	m.Handle("/api/auth/passkey/register", s.admin(s.auth.PasskeyRegisterHandler()))
	m.Handle("/api/auth/passkey/register/", s.admin(s.auth.PasskeyRegisterHandler()))
	m.Handle("/api/auth/passkey/login", s.auth.PasskeyLoginHandler())
	m.Handle("/api/auth/passkey/login/", s.auth.PasskeyLoginHandler())
	m.Handle("/api/auth/passkeys", s.admin(s.auth.PasskeyListHandler()))
	m.Handle("/api/auth/passkeys/", s.admin(s.auth.PasskeyListHandler()))
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
	// Serve index.html at "/" and the built UI bundle (assets/, logo.svg,
	// ...) for any other non-API path. API routes are registered on more
	// specific patterns, so they take precedence over this catch-all.
	web.ServeUI(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"people":    len(s.db.People()),
		"threshold": s.eng.Threshold(),
		"auth": map[string]bool{
			"password": s.auth.PasswordEnabled(),
			"passkey":  s.auth.PasskeyEnabled(),
		},
	})
}

// reload pushes DB identities into the engine after a mutation.
func (s *Server) reload() {
	if s.refresh != nil {
		s.refresh(s.eng, s.db)
	}
}
