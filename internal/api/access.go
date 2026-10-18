package api

// Access resolution for the repository-scoped routes (the multi-user model).
//
// Every scoped request carries its repository in the ?repo= query parameter
// (an ID, or the display name — both resolve to the canonical ID). The
// caller's role on that repository comes from one of two credentials:
//
//   - an authenticated principal (session cookie or API key): auth.RoleFor
//     answers "writer" for the admin and otherwise the user's stored grant
//     ("reader"/"writer"/"" — empty means no access);
//   - an X-Share-Token header ("<repoID>/<token>", one enabled share link):
//     a constant-time token comparison mints a "reader" role LIMTED to that
//     one repository.
//
// An authenticated user never falls through to their share token: the
// grant (or lack of it) decides. Anonymous requests without a valid share
// token are refused with the uniform 401; authenticated callers without the
// needed role get 403 from requireRepoRole, worded per route.

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Role names used by the guards. auth.RoleFor answers users.RoleReader /
// users.RoleWriter (admins as "writer"); share access mints roleReader.
const (
	roleReader = "reader"
	roleWriter = "writer"
)

// authRequiredMsg is the uniform 401 body for the scoped routes: the same
// "authentication required" prefix auth.Service answers with, plus the hint
// that a share token is a second way in (they exist exactly so anonymous
// visitors can read one repository).
const authRequiredMsg = "authentication required (or a valid share token)"

// resolveRepoParam canonicalizes a ?repo= value to the repository's ID: IDs
// match case-insensitively and display names are accepted too (the
// API/UI pass IDs, but a name is friendlier for scripts). A value that
// matches no repository is returned as-is — the role check then refuses it,
// which is the safe direction (no grant can ever match a non-canonical ID).
func (s *Server) resolveRepoParam(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if repo, ok := s.db.GetRepo(v); ok {
		return repo.ID
	}
	if repo, ok := s.db.GetRepoByName(v); ok {
		return repo.ID
	}
	return v
}

// requireRepo resolves the ?repo= parameter and refuses requests without
// one. Person-scoped routes need a repository to act on; the parameter may
// not be implied (GET /api/thumbs is the exception: it derives the repo
// from the person's record).
func (s *Server) requireRepo(w http.ResponseWriter, r *http.Request) (string, bool) {
	repoID := s.resolveRepoParam(r.URL.Query().Get("repo"))
	if repoID == "" {
		writeErr(w, http.StatusBadRequest, "repository parameter required")
		return "", false
	}
	return repoID, true
}

// validShare validates the request's X-Share-Token header against repoID.
// When repoID is "" the token's own repository is accepted (used by the
// mount guard, which has no repo context yet). Returns the token's
// canonical repository ID and whether the share access is valid.
//
// The header format is "<repoID>/<token>": trimmed, split on the first
// "/" (base64url tokens contain none, but splitting leniently keeps a
// stray second slash from mattering). The presented token is compared to
// the repository's stored token in constant time; an empty stored token
// (a malformed record) can never match, since presented tokens must be
// non-empty.
func (s *Server) validShare(r *http.Request, repoID string) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("X-Share-Token"))
	if h == "" {
		return "", false
	}
	repoPart, token, found := strings.Cut(h, "/")
	repoPart, token = strings.TrimSpace(repoPart), strings.TrimSpace(token)
	if !found || repoPart == "" || token == "" {
		return "", false
	}
	if repoID != "" && !strings.EqualFold(repoPart, repoID) {
		return "", false // a share token is valid on exactly its own repo
	}
	repo, ok := s.db.GetRepo(repoPart)
	if !ok || !repo.Shared || repo.ShareToken == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(repo.ShareToken)) != 1 {
		return "", false
	}
	return repo.ID, true
}

// requestRepoRole resolves the caller's role on repoID for one request.
// An authenticated principal's grant decides ("" = no access — an authed
// user never falls back to a share header); an anonymous caller gets
// "reader" when a valid share token for repoID is presented, else "".
func (s *Server) requestRepoRole(r *http.Request, repoID string) string {
	if p := s.auth.Principal(r); p.Authed {
		return s.auth.RoleFor(p, repoID)
	}
	if _, ok := s.validShare(r, repoID); ok {
		return roleReader // share access is always read-only
	}
	return ""
}

// requireRepoRole is the scoped-route guard: it refuses the request (and
// returns false) unless the caller holds at least wantRole on repoID —
// "reader" accepts reader/writer (and a valid share token), "writer"
// accepts writer only (share principals are never writers; the admin maps
// to writer). Refusals are distinguished by credential: an anonymous caller
// without a valid share token gets 401 (the wording carries the share-token
// hint — they may still present one), while an authenticated caller whose
// grants fall short gets 403 with the role-specific wording.
func (s *Server) requireRepoRole(w http.ResponseWriter, r *http.Request, repoID, wantRole string) bool {
	insufficient := "read access to this repository is required"
	if wantRole == roleWriter {
		insufficient = "write access to this repository is required"
	}
	if p := s.auth.Principal(r); p.Authed {
		role := s.auth.RoleFor(p, repoID)
		if role == "" || (wantRole == roleWriter && role != roleWriter) {
			writeErr(w, http.StatusForbidden, insufficient)
			return false
		}
		return true
	}
	// Anonymous: share-token fallback, always reader.
	if _, ok := s.validShare(r, repoID); !ok {
		writeErr(w, http.StatusUnauthorized, authRequiredMsg)
		return false
	}
	if wantRole == roleWriter {
		writeErr(w, http.StatusForbidden, insufficient)
		return false
	}
	return true
}

// requireAdminRole is the principal-only guard used where share access must
// not apply and the role is really "the admin": same 401/403 shapes as
// auth.RequireAdmin, usable inside a handler that already passed RequireAuth
// (e.g. POST /api/config).
func (s *Server) requireAdminRole(w http.ResponseWriter, r *http.Request) bool {
	p := s.auth.Principal(r)
	if !p.Authed {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	if !p.Admin {
		writeErr(w, http.StatusForbidden, "administrator access required")
		return false
	}
	return true
}

// scoped gates a mounted handler behind any accepted credential (an
// authenticated principal or a valid share token). The per-route role rules
// stay in the handlers via requireRepoRole; the mount guard's job is the
// uniform anonymous 401 — including on the paths that would otherwise leak
// their shape to unauthenticated probes (unknown subpaths and all).
func (s *Server) scoped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hasAnyAccess(r) {
			writeErr(w, http.StatusUnauthorized, authRequiredMsg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hasAnyAccess reports whether the request carries any credential the
// scoped guard accepts: an authenticated principal, or a valid share token
// for any repository (the route's own repo check happens in the handler).
func (s *Server) hasAnyAccess(r *http.Request) bool {
	if s.auth.Principal(r).Authed {
		return true
	}
	_, ok := s.validShare(r, "")
	return ok
}
