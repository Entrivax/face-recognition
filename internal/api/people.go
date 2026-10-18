package api

import (
	"net/http"
	"strings"

	"recogn/internal/db"
	"recogn/internal/enroll"
)

// handlePeople answers GET /api/people?repo=<id|name>: the person list of
// one repository, for callers holding at least the reader role on it
// (share tokens included). The summary shape is unchanged; the repo ID
// echoes back so clients can pin which dataset the rows belong to.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	repoID, ok := s.requireRepo(w, r)
	if !ok {
		return
	}
	if !s.requireRepoRole(w, r, repoID, roleReader) {
		return
	}
	people := s.db.PeopleByRepo(repoID)
	type summary struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Photos  int      `json:"photos"`
		Embeds  int      `json:"embeddings"`
		Thumb   string   `json:"thumb"`
		Aliases []string `json:"aliases,omitempty"`
	}
	out := make([]summary, 0, len(people))
	for _, p := range people {
		var thumb string
		if p.Thumb != "" {
			thumb = thumbURL(p.ID, p.ThumbSrc)
		}
		var aliases []string
		if p.Meta != nil {
			aliases = p.Meta.Aliases
		}
		out = append(out, summary{
			ID: p.ID, Name: p.Name,
			Photos: len(p.Photos), Embeds: len(p.Photos),
			Thumb: thumb, Aliases: aliases,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out, "count": len(out), "repo": repoID})
}

// handlePersonSubroutes dispatches /api/people/{name}/… — the reader-visible
// details view, the writer-gated mutations (enroll, enroll-face, thumbnail,
// rename, meta, photo management) and person deletion. Every route needs
// ?repo= and resolves the person through that repository (db.GetInRepo), so
// the same person name in two repositories stays two people.
func (s *Server) handlePersonSubroutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/people/")
	parts := strings.SplitN(rest, "/", 2)
	name := strings.TrimSpace(parts[0])
	if name == "" {
		writeErr(w, http.StatusBadRequest, "person name required")
		return
	}
	repoID, ok := s.requireRepo(w, r)
	if !ok {
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		// The details document (name, optional metadata, photo count).
		s.handleGetPerson(w, r, name, repoID)
	case len(parts) == 1:
		// DELETE /api/people/{name} and everything else.
		if r.Method != http.MethodDelete {
			writeErr(w, http.StatusMethodNotAllowed, "unsupported method")
			return
		}
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleDeletePerson(w, r, name, repoID)
	case parts[1] == "enroll":
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleEnrollPerson(w, r, name, repoID)
	case parts[1] == "enroll-face":
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleEnrollFace(w, r, name, repoID)
	case parts[1] == "thumbnail":
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleSelectThumb(w, r, name, repoID)
	case parts[1] == "rename":
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleRenamePerson(w, r, name, repoID)
	case parts[1] == "photos":
		// The photos-manager list (writer tooling); the handler answers
		// 405 for methods other than GET.
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handlePersonPhotos(w, r, name, repoID)
	case parts[1] == "meta":
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		s.handleSetMeta(w, r, name, repoID)
	case strings.HasPrefix(parts[1], "photos/"):
		photoPath := strings.TrimPrefix(parts[1], "photos/")
		// /photos/{path}/detect inspects the stored photo's faces; basenames
		// cannot contain "/", so the suffix is unambiguous.
		if strings.HasSuffix(photoPath, "/detect") {
			if !s.requireRepoRole(w, r, repoID, roleReader) {
				return
			}
			s.handlePersonPhotoDetect(w, r, name, repoID, strings.TrimSuffix(photoPath, "/detect"))
			return
		}
		// The full-resolution file is writer-only (a reader gets the
		// thumbnail and the detect view).
		if !s.requireRepoRole(w, r, repoID, roleWriter) {
			return
		}
		if r.Method == http.MethodDelete {
			s.handleDeletePhoto(w, r, name, repoID, photoPath)
			return
		}
		s.handlePersonPhoto(w, r, name, repoID, photoPath)
	default:
		writeErr(w, http.StatusNotFound, "unknown endpoint")
	}
}

// handleGetPerson serves the reader-visible details document for one person
// of the repository: identity, the photo count (already public via the list
// endpoint) and the optional metadata — aliases, partial birthdate, URLs,
// markdown description — plus the repo context (id and display name).
// Enrolled photo paths and hashes are not listed here: the full-res photo
// files stay writer-only, and the photos manager reads GET …/photos instead.
func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request, name, repoID string) {
	if !s.requireRepoRole(w, r, repoID, roleReader) {
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.GetInRepo(repoID, name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	resp := personDetailDoc(p)
	resp["repo"] = repoID
	if repo, ok := s.db.GetRepo(repoID); ok {
		resp["repo_name"] = repo.Name
	}
	writeJSON(w, http.StatusOK, resp)
}

// personDetailDoc renders the reader-visible details document for a person.
// Meta values must already be validated (the API layer validates writes;
// stored values are trusted — the store is only written through validated
// paths).
func personDetailDoc(p *db.Person) map[string]any {
	resp := map[string]any{
		"id":     p.ID,
		"name":   p.Name,
		"photos": len(p.Photos),
	}
	if m := p.Meta; m != nil {
		if len(m.Aliases) > 0 {
			resp["aliases"] = m.Aliases
		}
		if m.Birth != nil {
			resp["birthdate"] = m.Birth
		}
		if len(m.URLs) > 0 {
			resp["urls"] = m.URLs
		}
		if m.Description != "" {
			resp["description"] = m.Description
		}
	}
	return resp
}

// handlePersonPhotos serves a person's enrolled photo list for the
// photos-manager modal (writer-only: it exposes the enrolled photo paths
// and content hashes; no embeddings). thumb_src names the photo the avatar
// comes from.
func (s *Server) handlePersonPhotos(w http.ResponseWriter, r *http.Request, name, repoID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	p := s.db.GetInRepo(repoID, name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	type photo struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}
	photos := make([]photo, 0, len(p.Photos))
	for _, ph := range p.Photos {
		photos = append(photos, photo{Path: ph.Path, Hash: ph.Hash})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "name": p.Name, "thumb_src": p.ThumbSrc, "photos": photos,
	})
}
