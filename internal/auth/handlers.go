package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"recogn/internal/users"
)

// ---- handlers ----
//
// Mount paths are the api package's job; these handlers only parse bodies,
// dispatch on method and write the responses.

// LoginHandler handles POST /api/login: body {"username","password"} →
// session cookie. Failures are rate-limited per client IP; both an unknown
// username and a wrong password answer the same message (and burn the same
// argon2 cost, so latency does not leak which accounts exist).
func (s *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		// M4: behind a configured trusted proxy, key on the X-Forwarded-For
		// client IP so clients don't share one lockout bucket; otherwise the
		// network peer with XFF ignored (anti-spoofing default).
		ip := s.clientIP(r)
		if ok, retry := s.limiter.Allow(ip); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		u, ok := s.users.GetUser(body.Username)
		if ok && !users.VerifyPassword(u.PasswordHash, body.Password) {
			ok = false
		}
		if !ok {
			s.dummyVerify(body.Password) // uniform latency for unknown usernames
			s.limiter.RecordFailure(ip)
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		s.limiter.Reset(ip)
		s.mintSession(w, r, u.Username)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"user": map[string]any{"name": u.Username, "admin": u.Admin},
		})
	})
}

// dummyVerify hashes-then-compares the candidate against a throwaway hash
// so a failed login for an UNKNOWN username costs the same argon2 work as a
// wrong password for a real one (otherwise the 401 latency would leak which
// usernames exist). The throwaway hash is generated once, lazily.
var (
	dummyHashOnce sync.Once
	dummyHashVal  string
)

func (s *Service) dummyVerify(password string) {
	dummyHashOnce.Do(func() {
		if h, err := users.HashPassword("unused-throwaway-password"); err == nil {
			dummyHashVal = h
		}
	})
	if dummyHashVal != "" {
		users.VerifyPassword(dummyHashVal, password)
	}
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
// UI, reporting who (if anyone) the request authenticates as and whether
// the first-admin setup is still pending.
func (s *Service) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		p := s.Principal(r)
		var user any
		if p.Authed {
			user = map[string]any{"name": p.Username, "admin": p.Admin}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":  p.Authed,
			"user":           user,
			"setup_required": s.SetupRequired(),
		})
	})
}

// SetupHandler handles POST /api/auth/setup: body {"key","username",
// "password"} creates the first ADMIN account. The setup key is compared
// constant-time after normalization (trim, strip spaces/dashes, uppercase);
// attempts are rate-limited per client IP; once any user exists the
// endpoint refuses (403) — the in-memory key is consumed on success.
func (s *Service) SetupHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		ip := s.clientIP(r)
		if ok, retry := s.setupLim.Allow(ip); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "too many attempts")
			return
		}
		var body struct {
			Key      string `json:"key"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		// Serialize check-consume-create so two racing setups cannot both
		// pass the count check and mint two admins.
		s.setupMu.Lock()
		defer s.setupMu.Unlock()
		if s.users.Count() != 0 {
			// Setup already completed — also the response for a loser of
			// the race above (its CreateUser then reports a taken name).
			writeError(w, http.StatusForbidden, "setup already completed")
			return
		}
		if !s.checkSetupKey(body.Key) {
			s.setupLim.RecordFailure(ip)
			writeError(w, http.StatusForbidden, "invalid setup key")
			return
		}
		if _, err := s.users.CreateUser(body.Username, body.Password, true); err != nil {
			if errors.Is(err, users.ErrUsernameTaken) {
				// A concurrent setup created the first user already.
				writeError(w, http.StatusForbidden, "setup already completed")
				return
			}
			// Username/password validation errors carry their own message.
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.setupKey = "" // consumed
		s.setupLim.Reset(ip)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// ChangePasswordHandler handles POST /api/auth/password: body
// {"current","new"} behind RequireAuth (session or API-key principal).
// Verifies the current password against the stored hash, then re-hashes.
func (s *Service) ChangePasswordHandler() http.Handler {
	return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		p := s.Principal(r)
		var body struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		u, ok := s.users.GetUser(p.Username)
		if !ok {
			// The account vanished mid-request.
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !users.VerifyPassword(u.PasswordHash, body.Current) {
			writeError(w, http.StatusForbidden, "current password is incorrect")
			return
		}
		if err := s.users.SetPassword(u.Username, body.New); err != nil {
			if errors.Is(err, users.ErrUserNotFound) {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			// The minimum-length error carries its own message.
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
}

// APIKeysHandler handles the owner's own API keys: GET /api/auth/keys
// (list), POST /api/auth/keys {"name"} (create — the plaintext token
// appears exactly once, in this response) and DELETE /api/auth/keys/{id}
// (owner-scoped). Behind RequireAuth (session or API-key principal).
func (s *Service) APIKeysHandler() http.Handler {
	return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.Principal(r)
		rest := strings.TrimPrefix(r.URL.Path, "/api/auth/keys")
		switch {
		case rest == "":
			switch r.Method {
			case http.MethodGet:
				s.listKeys(w, p.Username)
			case http.MethodPost:
				s.createKey(w, r, p)
			default:
				writeError(w, http.StatusMethodNotAllowed, "GET or POST required")
			}
		case strings.HasPrefix(rest, "/"):
			if r.Method != http.MethodDelete {
				writeError(w, http.StatusMethodNotAllowed, "DELETE required")
				return
			}
			s.deleteKey(w, p.Username, strings.TrimPrefix(rest, "/"))
		default:
			writeError(w, http.StatusNotFound, "unknown endpoint")
		}
	}))
}

// keyInfo is the listed API-key shape: everything except the token itself.
type keyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
}

func toKeyInfo(k users.APIKey) keyInfo {
	return keyInfo{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339)}
}

func (s *Service) listKeys(w http.ResponseWriter, username string) {
	keys := s.users.ListKeys(username)
	out := make([]keyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyInfo(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Service) createKey(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	token, key, err := s.users.CreateKey(p.Username, body.Name)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			// The account vanished mid-request.
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		writeError(w, http.StatusInternalServerError, "create api key: "+err.Error())
		return
	}
	// The plaintext token travels here exactly once and is never stored.
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         key.ID,
		"name":       key.Name,
		"prefix":     key.Prefix,
		"token":      token,
		"created_at": key.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Service) deleteKey(w http.ResponseWriter, username, id string) {
	removed, err := s.users.DeleteKey(username, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete api key: "+err.Error())
		return
	}
	if !removed {
		// Unknown ID or someone else's key: the same answer either way.
		writeError(w, http.StatusNotFound, "api key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": id})
}

// ---- session cookie helpers ----

// mintSession creates a new session for username and sets its cookie on the
// response. The cookie's Max-Age mirrors the configured TTL (the sliding
// refresh is server-side only; the browser just needs a sane lifetime
// hint).
func (s *Service) mintSession(w http.ResponseWriter, r *http.Request, username string) {
	token, _ := s.sessions.New(username)
	http.SetCookie(w, s.sessions.cookie(token, int(s.cfg.SessionTTL.Seconds()), r))
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
