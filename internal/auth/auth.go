// Package auth implements the admin authentication for recogn's API: an
// optional password (configured as an argon2id hash in
// RECOGN_ADMIN_PASSWORD_HASH) and/or registered WebAuthn passkeys gate the
// mutating admin routes behind a session cookie or a Bearer session token.
// When neither method is configured the middleware is a pass-through (open
// mode) and the server behaves exactly as before — except that passkey
// registration is refused (403), so a fresh deployment cannot be captured by
// the first network peer to reach it (SECURITY-REVIEW.md H2).
//
// The password itself is only ever compared inside POST /api/login (against
// its argon2id hash, rate-limited); it is never accepted as a Bearer
// credential on the gated routes.
//
// The package is self-contained on purpose: it must not import internal/api
// (which imports it), so it carries its own tiny writeJSON/writeError pair
// matching the api package's {"error": msg} style.
package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"recogn/internal/config"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// defaultNow is the wall clock; tests swap an injectable one in.
func defaultNow() time.Time { return time.Now() }

// maxBody caps JSON request bodies (login, WebAuthn assertions) at 1 MiB,
// matching the api package's JSON body cap.
const maxBody = 1 << 20

// Service owns every admin-auth mechanism: password check, session store,
// login rate limiter and the passkey credential store. (Each sub-store keeps
// its own injectable clock; tests drive them directly.)
type Service struct {
	cfg      config.Config
	sessions *SessionStore
	limiter  *RateLimiter
	store    *passkeyStore
	pending  *pendingCeremonies
}

// New builds the auth service from cfg, loading any previously registered
// passkeys from <DataDir>/passkeys.json. A corrupt store fails startup
// loudly rather than silently dropping the admin's second factor; a
// malformed password hash does the same, so a typo in the env var cannot
// quietly disable password logins.
func New(cfg config.Config) (*Service, error) {
	store, err := loadPasskeyStore(passkeyPath(cfg), defaultNow)
	if err != nil {
		return nil, err
	}
	if cfg.AdminPasswordHash != "" {
		if _, _, _, err := parsePasswordHash(cfg.AdminPasswordHash); err != nil {
			return nil, fmt.Errorf("invalid RECOGN_ADMIN_PASSWORD_HASH: %w (generate one with 'recogn hash-password')", err)
		}
	}
	return &Service{
		cfg:      cfg,
		sessions: newSessionStore(cfg.SessionTTL, defaultNow),
		limiter:  newRateLimiter(defaultNow),
		pending:  newPendingCeremonies(defaultNow),
		store:    store,
	}, nil
}

// passkeyPath is where registered credentials persist.
func passkeyPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, passkeysFileName)
}

// Enabled reports whether any admin credential is configured. When false the
// middleware is a pass-through: the server stays fully public (open mode).
func (s *Service) Enabled() bool {
	return s.cfg.AdminPasswordHash != "" || s.store.count() > 0
}

// PasswordEnabled reports whether password login is available.
func (s *Service) PasswordEnabled() bool { return s.cfg.AdminPasswordHash != "" }

// PasskeyEnabled reports whether at least one passkey is registered.
func (s *Service) PasskeyEnabled() bool { return s.store.count() > 0 }

// Mode names the effective auth posture: "open", "password", "passkey" or
// "password+passkey".
func (s *Service) Mode() string {
	switch {
	case s.PasswordEnabled() && s.PasskeyEnabled():
		return "password+passkey"
	case s.PasswordEnabled():
		return "password"
	case s.PasskeyEnabled():
		return "passkey"
	default:
		return "open"
	}
}

// Authorized reports whether the request may access admin endpoints: either
// a valid session cookie or a Bearer credential carrying a live session
// token (the value of the recogn_session cookie — handy for scripts that
// prefer headers over a cookie jar). Passwords are never accepted here; they
// are only verified by POST /api/login, behind the login rate limiter. In
// open mode every request is allowed.
func (s *Service) Authorized(r *http.Request) bool {
	if !s.Enabled() {
		return true // open mode: nothing is gated
	}
	if c, err := r.Cookie(SessionCookie); err == nil && s.sessions.Valid(c.Value) {
		return true
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.sessions.Valid(strings.TrimSpace(token))
	}
	return false
}

// Middleware gates admin routes. In open mode it is a pass-through;
// otherwise a valid session cookie or Bearer session token is required.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "authentication required")
	})
}

// ---- handlers ----

// LoginHandler handles POST /api/login: body {"password": "..."} → session
// cookie. Failures are rate-limited per client IP.
func (s *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		if !s.PasswordEnabled() {
			// No password configured: nothing to compare against. The exact
			// wording doubles for passkey-only mode, where password login is
			// likewise "not configured".
			writeError(w, http.StatusUnauthorized, "admin authentication is not configured")
			return
		}
		ip := remoteIP(r)
		if ok, retry := s.limiter.Allow(ip); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if !verifyPassword(body.Password, s.cfg.AdminPasswordHash) {
			s.limiter.RecordFailure(ip)
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		s.limiter.Reset(ip)
		s.mintSession(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// LogoutHandler handles POST /api/logout: drop the session server-side and
// clear the cookie. Always 200, even without a cookie (idempotent).
func (s *Service) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		if c, err := r.Cookie(SessionCookie); err == nil {
			s.sessions.Delete(c.Value)
		}
		http.SetCookie(w, s.sessions.cookie("", 0, r)) // Max-Age=0 clears the browser cookie
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// SessionHandler handles GET /api/auth/session: a capability probe for the
// UI, reporting whether this request is authorized and which auth methods
// the server offers.
func (s *Service) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": s.Authorized(r),
			"methods": map[string]bool{
				"password": s.PasswordEnabled(),
				"passkey":  s.PasskeyEnabled(),
			},
		})
	})
}

// PasskeyRegisterHandler handles POST /api/auth/passkey/register[/begin|/finish].
// Mounted behind the admin middleware: only an authenticated admin may add a
// credential. The path suffix selects the ceremony phase. In open mode — no
// password hash configured and zero registered passkeys — the middleware is
// a pass-through, so this handler itself refuses (SECURITY-REVIEW.md H2).
func (s *Service) PasskeyRegisterHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		// Open-mode bootstrap guard: with no admin credential at all, the
		// middleware above lets every request through, which made public
		// passkey registration first-come-first-served — the first network
		// peer to complete a ceremony would own admin permanently (the
		// store keeps every registered credential; only an admin could
		// remove one). Refuse until a credential exists; bootstrap a
		// passkey-only install with a temporary RECOGN_ADMIN_PASSWORD_HASH
		// instead (log in, register the passkey, remove the hash, restart).
		if !s.Enabled() {
			slog.Warn("passkey registration refused in open mode",
				"remote", remoteIP(r))
			writeError(w, http.StatusForbidden,
				"passkey registration is disabled until admin authentication is configured — set RECOGN_ADMIN_PASSWORD_HASH (generate one with 'recogn hash-password'), log in, then register passkeys")
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/auth/passkey/register")
		switch rest {
		case "/begin":
			s.passkeyRegisterBegin(w, r)
		case "/finish":
			s.passkeyRegisterFinish(w, r)
		default:
			writeError(w, http.StatusNotFound, "unknown passkey endpoint")
		}
	})
}

func (s *Service) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := s.webAuthnFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn setup: "+err.Error())
		return
	}
	user := s.adminUser()
	creation, session, err := wa.BeginRegistration(user,
		// Passkeys are discoverable by definition and verified by the
		// platform: the login flow is discoverable-only, so the credential
		// must live on the authenticator.
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin registration: "+err.Error())
		return
	}
	s.pending.put(session)
	writeJSON(w, http.StatusOK, creation)
}

func (s *Service) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	parsed, err := protocol.ParseCredentialCreationResponse(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid credential response")
		return
	}
	session, ok := s.pending.take(parsed.Response.CollectedClientData.Challenge)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown or expired registration ceremony")
		return
	}
	wa, err := s.webAuthnFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn setup: "+err.Error())
		return
	}
	cred, err := wa.CreateCredential(s.adminUser(), session, parsed)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "registration failed: "+err.Error())
		return
	}
	if err := s.store.add(*cred); err != nil {
		writeError(w, http.StatusInternalServerError, "persist passkey: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PasskeyLoginHandler handles POST /api/auth/passkey/login[/begin|/finish]
// (public): discoverable-credential login against the registered passkeys.
// The path suffix selects the ceremony phase.
func (s *Service) PasskeyLoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/auth/passkey/login")
		switch rest {
		case "/begin":
			s.passkeyLoginBegin(w, r)
		case "/finish":
			s.passkeyLoginFinish(w, r)
		default:
			writeError(w, http.StatusNotFound, "unknown passkey endpoint")
		}
	})
}

func (s *Service) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.PasskeyEnabled() {
		writeError(w, http.StatusUnauthorized, "passkey authentication is not configured")
		return
	}
	wa, err := s.webAuthnFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn setup: "+err.Error())
		return
	}
	assertion, session, err := wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin login: "+err.Error())
		return
	}
	s.pending.put(session)
	writeJSON(w, http.StatusOK, assertion)
}

func (s *Service) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.PasskeyEnabled() {
		writeError(w, http.StatusUnauthorized, "passkey authentication is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	parsed, err := protocol.ParseCredentialRequestResponse(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid assertion response")
		return
	}
	session, ok := s.pending.take(parsed.Response.CollectedClientData.Challenge)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unknown or expired login ceremony")
		return
	}
	wa, err := s.webAuthnFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn setup: "+err.Error())
		return
	}
	// Discoverable login: the authenticator tells us which credential it
	// used; the lookup hands back the single admin user that owns every
	// registered credential (validateLogin then verifies the credential id
	// against the user's stored set).
	_, cred, err := wa.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		return s.adminUser(), nil
	}, session, parsed)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "passkey login failed")
		return
	}
	// Keep the sign counter fresh so cloned-authenticator detection works.
	s.store.update(*cred)
	s.mintSession(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PasskeyListHandler handles GET /api/auth/passkeys (list) and
// DELETE /api/auth/passkeys/{id} (remove one). Admin-only via the middleware.
func (s *Service) PasskeyListHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/auth/passkeys")
		switch {
		case rest == "":
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "GET required")
				return
			}
			s.passkeyList(w, r)
		case strings.HasPrefix(rest, "/"):
			if r.Method != http.MethodDelete {
				writeError(w, http.StatusMethodNotAllowed, "DELETE required")
				return
			}
			s.passkeyDelete(w, r, strings.TrimPrefix(rest, "/"))
		default:
			writeError(w, http.StatusNotFound, "unknown passkey endpoint")
		}
	})
}

type passkeyInfo struct {
	ID      string `json:"id"`
	AddedAt string `json:"added_at"`
}

func (s *Service) passkeyList(w http.ResponseWriter, r *http.Request) {
	records := s.store.records()
	out := make([]passkeyInfo, 0, len(records))
	for _, rec := range records {
		out = append(out, passkeyInfo{
			ID:      base64URL(string(rec.Credential.ID)),
			AddedAt: rec.AddedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out})
}

func (s *Service) passkeyDelete(w http.ResponseWriter, r *http.Request, id string) {
	removed, err := s.store.removeByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "persist passkeys: "+err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "passkey not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": id})
}

// ---- session cookie helpers ----

// mintSession creates a new session and sets its cookie on the response.
// The cookie's Max-Age mirrors the configured TTL (the sliding refresh is
// server-side only; the browser just needs a sane lifetime hint).
func (s *Service) mintSession(w http.ResponseWriter, r *http.Request) {
	token, _ := s.sessions.New()
	http.SetCookie(w, s.sessions.cookie(token, int(s.cfg.SessionTTL.Seconds()), r))
}

// adminUser returns the synthetic admin user with the currently stored
// credentials.
func (s *Service) adminUser() *adminUser {
	return &adminUser{rpName: s.cfg.WebAuthnRPName, creds: s.store.credentials()}
}

// ---- JSON helpers (mirror internal/api's writeJSON/writeErr) ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
