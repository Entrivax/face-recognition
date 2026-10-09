package api

import (
	"net/http"
	"strings"
)

// handleThumb serves a person's face thumbnail sidecar as JPEG. The id is
// validated and resolved through the DB before any filesystem access.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/thumbs/"), ".jpg")
	if !validThumbID(id) || s.db.GetByID(id) == nil {
		http.NotFound(w, r)
		return
	}
	path := s.db.ThumbFile(id)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	// Thumbnails can be re-selected by the user, so cache briefly only.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, path)
}

// validThumbID restricts thumbnail ids to the charset db.newID produces,
// keeping the value safe to use as a file name.
func validThumbID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
