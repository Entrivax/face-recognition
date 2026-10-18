package api

import (
	"net/http"
	"strings"
)

// handleThumb serves a person's face thumbnail sidecar as JPEG. The id is
// validated and resolved through the DB before any filesystem access.
//
// Access: the thumbnail belongs to the person's repository — for a repo
// person, reader+ on that repo WITHOUT ?repo= (the repo comes from the
// person's record; a share token names that repo in its header). Legacy
// persons (RepoID "") are not repo-scoped, so their thumbnails are visible
// to the admin only. Anonymous requests without a share token get the
// scoped guard's 401 before reaching here.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/thumbs/"), ".jpg")
	if !validThumbID(id) {
		http.NotFound(w, r)
		return
	}
	p := s.db.GetByID(id) // global by ID: fine for thumbs, the ID is unique
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if p.RepoID == "" {
		// Legacy person: no repo to be granted on — admin only.
		if !s.requireAdminRole(w, r) {
			return
		}
	} else if !s.requireRepoRole(w, r, p.RepoID, roleReader) {
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

// validThumbID restricts thumbnail ids to the charset db.newID produces
// (plus the repo UUID prefix, lowercase hex + hyphens — same alphabet),
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
