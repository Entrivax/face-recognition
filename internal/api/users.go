package api

// Admin user-management endpoints: account CRUD over the auth service's
// users store, password resets and per-repository grants. All are
// RequireAdmin-mounted; the handlers stay thin (validation + status
// mapping) and let the users store own its rules.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"recogn/internal/auth"
	"recogn/internal/users"
)

// userEntry is one account in GET /api/users: identity, the admin flag, the
// per-repo grants and the creation time. Password hashes never travel.
type userEntry struct {
	Username  string        `json:"username"`
	Admin     bool          `json:"admin"`
	Grants    []users.Grant `json:"grants"`
	CreatedAt string        `json:"created_at"`
}

// handleUsers dispatches /api/users: GET lists accounts, POST creates one.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleUsersList(w, r)
	case http.MethodPost:
		s.handleCreateUser(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST required")
	}
}

// handleUsersList answers GET /api/users (admin): every account with its
// grants, username-sorted (the store's order).
func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	list := s.auth.Users()
	out := make([]userEntry, 0, len(list))
	for _, u := range list {
		grants := u.Grants
		if grants == nil {
			grants = []users.Grant{}
		}
		out = append(out, userEntry{
			Username:  u.Username,
			Admin:     u.Admin,
			Grants:    grants,
			CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "count": len(out)})
}

// handleCreateUser answers POST /api/users {"username","password","admin"}.
// Validation errors (username shape, password length) carry the users
// store's own message; a taken name is 409.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.auth.CreateUser(body.Username, body.Password, body.Admin); err != nil {
		switch {
		case errors.Is(err, users.ErrUsernameTaken):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, auth.ErrAdminProtected): // unreachable here, defensive
			writeErr(w, http.StatusForbidden, err.Error())
		default:
			// Username/password validation errors carry their own text.
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUserByID dispatches /api/users/{u} (DELETE only today).
func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE required")
		return
	}
	s.handleDeleteUser(w, r, r.PathValue("u"))
}

// handleDeleteUser answers DELETE /api/users/{u}. Two refusals: deleting
// the caller's own account (403) and deleting the LAST remaining admin
// (403) — with several admins, any of them may delete another; the caller
// is always an admin (RequireAdmin). auth.DeleteUser applies the same rule
// as the defensive backstop and cascades the target's API keys and live
// sessions.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, username string) {
	caller := s.auth.Principal(r).Username
	// Look the target up first so a refused target is answered before the
	// store sees the delete (readable 403, not the store's generic error),
	// and an unknown name is 404 without ambiguity.
	exists := false
	for _, u := range s.auth.Users() {
		if strings.EqualFold(u.Username, username) {
			if strings.EqualFold(caller, u.Username) {
				writeErr(w, http.StatusForbidden, auth.ErrSelfProtected.Error())
				return
			}
			if u.Admin {
				admins := 0
				for _, cand := range s.auth.Users() {
					if cand.Admin {
						admins++
					}
					if admins > 1 {
						break
					}
				}
				if admins <= 1 {
					writeErr(w, http.StatusForbidden, auth.ErrAdminProtected.Error())
					return
				}
			}
			exists = true
			break
		}
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	deleted, err := s.auth.DeleteUser(caller, username)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSelfProtected):
			writeErr(w, http.StatusForbidden, auth.ErrSelfProtected.Error())
		case errors.Is(err, auth.ErrAdminProtected):
			writeErr(w, http.StatusForbidden, auth.ErrAdminProtected.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	slog.Info("user deleted", "user", username, "by", caller)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetNotice is the informational field the admin reset response carries:
// the user's API keys are deliberately left intact (a password reset is not
// a compromise verdict and keys are unrecoverable once lost), so the caller
// is reminded to have the user review them instead.
const resetNotice = "the user's API keys were not revoked — ask them to review their API keys if the account may be compromised"

// handleUserPassword answers POST /api/users/{u}/password {"password"}: the
// admin reset path (self-service changes go through /api/auth/password).
// Sessions are revoked; API keys are left intact (see resetNotice).
func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request, username string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.auth.SetPassword(username, body.Password); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		// The minimum-length error carries its own message.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notice": resetNotice})
}

// grantReq is one entry of POST /api/users/{u}/repos: the repository (ID or
// display name) and the role on it. Grants are a full replacement — the
// list sent is the list the user keeps.
type grantReq struct {
	Repo string `json:"repo"`
	Role string `json:"role"`
}

// handleUserGrants answers POST /api/users/{u}/repos
// {"grants":[{"repo","role"}]}: every repository is validated against the
// face DB (unknown → 400) and canonicalized to its ID, every role must be
// reader or writer, then the whole grant list replaces the user's old one.
func (s *Server) handleUserGrants(w http.ResponseWriter, r *http.Request, username string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		Grants []grantReq `json:"grants"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	grants := make([]users.Grant, 0, len(body.Grants))
	for _, g := range body.Grants {
		if g.Role != users.RoleReader && g.Role != users.RoleWriter {
			writeErr(w, http.StatusBadRequest, "role must be \"reader\" or \"writer\"")
			return
		}
		if strings.TrimSpace(g.Repo) == "" {
			writeErr(w, http.StatusBadRequest, "repository is required")
			return
		}
		repo, ok := s.db.GetRepo(g.Repo)
		if !ok {
			if byName, found := s.db.GetRepoByName(g.Repo); found {
				repo = byName
			} else {
				writeErr(w, http.StatusBadRequest, "unknown repository: "+g.Repo)
				return
			}
		}
		grants = append(grants, users.Grant{RepoID: repo.ID, Role: g.Role})
	}
	if err := s.auth.SetUserGrants(username, grants); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		if errors.Is(err, users.ErrInvalidRole) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
