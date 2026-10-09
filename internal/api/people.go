package api

import (
	"net/http"
	"strings"

	"recogn/internal/db"
	"recogn/internal/enroll"
)

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	people := s.db.People()
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
	writeJSON(w, http.StatusOK, map[string]any{"people": out, "count": len(out)})
}

// handlePersonSubroutes dispatches /api/people/{name}/… admin subroutes:
// enroll, enroll-face, thumbnail, rename, meta, photos and their subpaths.
// The public details GET (exactly one path segment) never reaches here —
// routes() answers it before the admin middleware.
func (s *Server) handlePersonSubroutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/people/")
	parts := strings.SplitN(rest, "/", 2)
	name := strings.TrimSpace(parts[0])
	if name == "" {
		writeErr(w, http.StatusBadRequest, "person name required")
		return
	}
	if len(parts) == 2 && parts[1] == "enroll" {
		s.handleEnrollPerson(w, r, name)
		return
	}
	if len(parts) == 2 && parts[1] == "enroll-face" {
		s.handleEnrollFace(w, r, name)
		return
	}
	if len(parts) == 2 && parts[1] == "thumbnail" {
		s.handleSelectThumb(w, r, name)
		return
	}
	if len(parts) == 2 && parts[1] == "rename" {
		s.handleRenamePerson(w, r, name)
		return
	}
	if len(parts) == 2 && parts[1] == "photos" {
		s.handlePersonPhotos(w, r, name)
		return
	}
	if len(parts) == 2 && parts[1] == "meta" {
		s.handleSetMeta(w, r, name)
		return
	}
	if len(parts) == 2 && strings.HasPrefix(parts[1], "photos/") {
		photoPath := strings.TrimPrefix(parts[1], "photos/")
		// /photos/{path}/detect inspects the stored photo's faces; basenames
		// cannot contain "/", so the suffix is unambiguous.
		if strings.HasSuffix(photoPath, "/detect") {
			s.handlePersonPhotoDetect(w, r, name, strings.TrimSuffix(photoPath, "/detect"))
			return
		}
		if r.Method == http.MethodDelete {
			s.handleDeletePhoto(w, r, name, photoPath)
			return
		}
		s.handlePersonPhoto(w, r, name, photoPath)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.handleDeletePerson(w, r, name)
	case http.MethodGet:
		s.handleGetPerson(w, r, name)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "unsupported method")
	}
}

// handleGetPerson serves the public person-details document: identity, the
// photo count (already public via the list endpoint) and the optional
// metadata — aliases, partial birthdate, URLs, markdown description. Fields
// that were never filled are omitted so the UI can hide empty sections.
// Enrolled photo paths and hashes are not listed here: the full-res photo
// files stay admin-only, and the photos manager reads GET …/photos instead.
func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request, name string) {
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	writeJSON(w, http.StatusOK, personDetailDoc(p))
}

// personDetailDoc renders the public details document for a person. Meta
// values must already be validated (the API layer validates writes; stored
// values are trusted — the store is only written through validated paths).
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

// handlePersonPhotos serves a person's enrolled photo list for the admin
// photos manager (paths + content hashes, no embeddings; thumb_src names the
// photo the avatar comes from).
func (s *Server) handlePersonPhotos(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	p := s.db.Get(name)
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
