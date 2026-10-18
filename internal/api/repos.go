package api

// Repository management endpoints (admin): create/rename/delete datasets,
// toggle and re-read their share links, and the authenticated listing every
// client uses to discover what it may see.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"recogn/internal/db"
)

// repoEntry is one repository in GET /api/repos: the identity block plus
// the caller's role on it ("admin"/"writer"/"reader"). Shared and the
// share flag are only shown to the admin (a user must not learn which other
// repos have share links — and they cannot see ungranted repos at all).
type repoEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	People int    `json:"people"`
	Shared bool   `json:"shared,omitempty"`
	Role   string `json:"role"`
}

// handleRepos dispatches /api/repos: GET lists the caller's repositories
// (RequireAuth-mounted: any authenticated principal, no share tokens) and
// POST creates one (admin — double-gated here so the method dispatcher can
// answer before the role check fails).
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleReposList(w, r)
	case http.MethodPost:
		if !s.requireAdminRole(w, r) {
			return
		}
		s.handleCreateRepo(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST required")
	}
}

// handleReposList answers GET /api/repos for the authenticated caller: the
// admin sees every repository with the "admin" role; a user only the ones
// they hold a grant on (reader or writer), each with their stored role.
func (s *Server) handleReposList(w http.ResponseWriter, r *http.Request) {
	p := s.auth.Principal(r)
	repos := s.db.Repos()
	out := make([]repoEntry, 0, len(repos))
	for _, repo := range repos {
		role := s.auth.RoleFor(p, repo.ID)
		if !p.Admin && role == "" {
			continue // no grant: the repository stays invisible
		}
		entry := repoEntry{
			ID:     repo.ID,
			Name:   repo.Name,
			People: s.db.PeopleCount(repo.ID),
			Role:   role,
		}
		if p.Admin {
			entry.Role = "admin"
			entry.Shared = repo.Shared
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": out, "count": len(out)})
}

// handleCreateRepo answers POST /api/repos {"name"}: the repository record
// is created first (names are unique case-insensitively), then its dataset
// folder people/<repoID> is created so uploads and rescans have somewhere
// to go. A folder failure leaves the record in place and is reported via
// "folder_created": false (deleting the record again is a one-click fix;
// silently pretending the repo works is worse).
func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "repository name is required")
		return
	}
	repo, err := s.db.CreateRepo(body.Name)
	switch {
	case errors.Is(err, db.ErrRepoNameTaken):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(s.cfg.PeopleDir, repo.ID), 0o755); err != nil {
		// The record stays; the response reports the missing folder
		// (deleting the record again is a one-click fix; silently
		// pretending the repo works is worse).
		slog.Warn("create repo folder", "dir", filepath.Join(s.cfg.PeopleDir, repo.ID), "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":          "create repo folder: " + err.Error(),
			"folder_created": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     repo.ID,
		"name":   repo.Name,
		"people": s.db.PeopleCount(repo.ID),
	})
}

// handleRenameRepo answers POST /api/repos/{id}/rename {"name"}.
func (s *Server) handleRenameRepo(w http.ResponseWriter, r *http.Request, repoID string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "repository name is required")
		return
	}
	repo, err := s.db.RenameRepo(repoID, body.Name)
	switch {
	case errors.Is(err, db.ErrRepoNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, db.ErrRepoNameTaken):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"id": repo.ID, "name": repo.Name})
	}
}

// handleRepoShare answers /api/repos/{id}/share. GET reveals the current
// share link (the token is stored with the repo record, so the URL can be
// rebuilt at any time — no rotation); POST {"enabled": bool} toggles it.
// Enabling mints a fresh token (base64url of 16 random bytes, stored as
// plaintext + sha256 by the DB) and returns the share URL — every re-enable
// rotates it. Disabling clears the stored token; previous links stop
// working. The GET reply mirrors the POST shape: {id, shared, share_url},
// with share_url null while unshared.
func (s *Server) handleRepoShare(w http.ResponseWriter, r *http.Request, repoID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleRepoShareGet(w, r, repoID)
	case http.MethodPost:
		s.handleRepoShareToggle(w, r, repoID)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST required")
	}
}

// handleRepoShareGet answers GET /api/repos/{id}/share: the share URL of the
// repository's current share token, or share_url null while unshared. The
// answer is built from the caller's own request (scheme + host) exactly like
// the enable reply, and no-store keeps intermediaries from caching a secret.
func (s *Server) handleRepoShareGet(w http.ResponseWriter, r *http.Request, repoID string) {
	repo, ok := s.db.GetRepo(repoID)
	if !ok {
		writeErr(w, http.StatusNotFound, db.ErrRepoNotFound.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	url := ""
	if repo.Shared && repo.ShareToken != "" {
		url = s.shareURL(r, repo.ID, repo.ShareToken)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        repo.ID,
		"shared":    repo.Shared,
		"share_url": nilOrString(url),
	})
}

// nilOrString maps an empty string to JSON null (share_url when no link is
// available) and passes anything else through.
func nilOrString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// handleRepoShareToggle answers POST /api/repos/{id}/share {"enabled": bool}.
func (s *Server) handleRepoShareToggle(w http.ResponseWriter, r *http.Request, repoID string) {
	repo, ok := s.db.GetRepo(repoID)
	if !ok {
		writeErr(w, http.StatusNotFound, db.ErrRepoNotFound.Error())
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !body.Enabled {
		if err := s.db.SetRepoShare(repo.ID, false, ""); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":        repo.ID,
			"shared":    false,
			"share_url": nil,
		})
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "generate share token: "+err.Error())
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if err := s.db.SetRepoShare(repo.ID, true, token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        repo.ID,
		"shared":    true,
		"share_url": s.shareURL(r, repo.ID, token),
	})
}

// handleRepoByID dispatches /api/repos/{id} (DELETE only today).
func (s *Server) handleRepoByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE required")
		return
	}
	s.handleDeleteRepo(w, r, r.PathValue("id"))
}

// handleDeleteRepo answers DELETE /api/repos/{id}: the repository record,
// its persons and their thumbnails go in one DB transaction, every grant
// referencing it is stripped from the users (best-effort), and its dataset
// folder people/<repoID> is removed best-effort. The response reports how
// many persons and whether the folder went away.
func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request, repoID string) {
	repo, ok := s.db.GetRepo(repoID)
	if !ok {
		writeErr(w, http.StatusNotFound, db.ErrRepoNotFound.Error())
		return
	}
	// Capture before the deletion: afterwards the count is 0.
	peopleRemoved := s.db.PeopleCount(repo.ID)
	deleted, err := s.db.DeleteRepo(repo.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, db.ErrRepoNotFound.Error())
		return
	}
	// Best-effort housekeeping: users lose their grants on the dead repo.
	if err := s.auth.RemoveRepo(repo.ID); err != nil {
		slog.Warn("strip grants of deleted repo", "repo", repo.ID, "err", err)
	}
	// Best-effort: the dataset folder is the caller's tree (the DB layer
	// never touches it), but a left-behind folder would resurrect persons
	// on the next import — so remove it here.
	folderRemoved := true
	dir := filepath.Join(s.cfg.PeopleDir, repo.ID)
	if err := os.RemoveAll(dir); err != nil {
		folderRemoved = false
		slog.Warn("remove repo folder", "dir", dir, "err", err)
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"removed":        repo.ID,
		"people_removed": peopleRemoved,
		"folder_removed": folderRemoved,
	})
}

// shareURL builds the share link out of the request's own scheme and host,
// so the URL a client can open is derived from how they reached the API
// (TLS or a forwarded https proxy means https).
func (s *Server) shareURL(r *http.Request, repoID, token string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/share/" + repoID + "/" + token
}
