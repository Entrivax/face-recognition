package auth

import (
	"net/http"
	"strings"

	"recogn/internal/users"
)

// Principal is the resolved identity of a request: who it belongs to and
// how it authenticated. The zero value is an anonymous request.
type Principal struct {
	Username string // canonical (lowercase) username
	Admin    bool   // fresh from the user record (grant changes apply live)
	ViaKey   bool   // authenticated by an API key ("rk_" Bearer token)
	Authed   bool   // any valid credential
}

// Principal resolves the request to a Principal: the recogn_session cookie
// or the Authorization Bearer header. A Bearer value starting with "rk_" is
// an API key (users.LookupKey); any other Bearer value is a session token
// (sessions.Valid). The Principal is built from the FRESH user record — a
// session or key of a deleted user stops authenticating, and admin flags or
// grants changed after login apply immediately.
func (s *Service) Principal(r *http.Request) Principal {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if name, ok := s.sessions.Valid(c.Value); ok {
			return s.principalFor(name, false)
		}
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, "rk_") {
			if u, found := s.users.LookupKey(token); found {
				return Principal{Username: u.Username, Admin: u.Admin, ViaKey: true, Authed: true}
			}
			return Principal{} // unknown or orphaned key: not authenticated
		}
		if name, ok := s.sessions.Valid(token); ok {
			return s.principalFor(name, false)
		}
	}
	return Principal{}
}

// principalFor loads the fresh user record behind a resolved session/credential.
func (s *Service) principalFor(name string, viaKey bool) Principal {
	u, ok := s.users.GetUser(name)
	if !ok {
		// The user record vanished (deleted while the session/key lived):
		// the credential no longer names a real account.
		return Principal{}
	}
	return Principal{Username: u.Username, Admin: u.Admin, ViaKey: viaKey, Authed: true}
}

// RoleFor maps a Principal to a role on repoID: "" when unauthenticated,
// "writer" for admins (on every repo), otherwise the user's stored grant
// ("reader"/"writer"/"").
func (s *Service) RoleFor(p Principal, repoID string) string {
	if !p.Authed {
		return ""
	}
	if p.Admin {
		return users.RoleWriter
	}
	return s.users.Role(p.Username, repoID)
}

// RequireAuth gates a handler behind any authenticated principal (session
// or API key). Anonymous requests get 401 {"error":"authentication required"}.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Principal(r).Authed {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin gates a handler behind an admin principal. Anonymous
// requests get 401, authenticated non-admins get 403
// {"error":"administrator access required"}.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.Principal(r)
		if !p.Authed {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !p.Admin {
			writeError(w, http.StatusForbidden, "administrator access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
