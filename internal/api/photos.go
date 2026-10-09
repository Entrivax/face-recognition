package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"recogn/internal/db"
	"recogn/internal/engine"
	"recogn/internal/enroll"
)

// handlePersonPhoto serves one of a person's enrolled photo files from the
// people folder, for the thumbnail chooser modal. The requested path must be
// one of the person's enrolled photos and resolve inside their folder.
func (s *Server) handlePersonPhoto(w http.ResponseWriter, r *http.Request, name, photoPath string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	full := personPhotoPath(s.cfg.PeopleDir, p, photoPath)
	if full == "" {
		writeErr(w, http.StatusNotFound, "photo not found")
		return
	}
	http.ServeFile(w, r, full)
}

// handlePersonPhotoDetect runs recognition on one of a person's enrolled
// photos and returns the detected faces (with identity matches), so the UI
// can draw them over the image and the user can judge the photo's quality.
func (s *Server) handlePersonPhotoDetect(w http.ResponseWriter, r *http.Request, name, photoPath string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	full := personPhotoPath(s.cfg.PeopleDir, p, photoPath)
	if full == "" {
		writeErr(w, http.StatusNotFound, "photo not found")
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		writeErr(w, http.StatusNotFound, "photo file is missing from the people folder")
		return
	}
	faces, err := s.eng.Recognize(b)
	if err != nil {
		writeErr(w, engineErrStatus(err), "recognition failed: "+err.Error())
		return
	}
	for i := range faces {
		faces[i].Embedding = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"photo": photoPath,
		"count": len(faces),
		"faces": faces,
	})
}

// handleDeletePhoto removes one photo from a person: the DB entry and, when
// present, the image file in the people folder. If the deleted photo was the
// thumbnail source, the avatar is regenerated from another photo (or cleared
// when none remain). Body-less; photo path comes from the URL.
func (s *Server) handleDeletePhoto(w http.ResponseWriter, r *http.Request, name, photoPath string) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	full := personPhotoPath(s.cfg.PeopleDir, p, photoPath)
	if full == "" {
		writeErr(w, http.StatusNotFound, "photo not found")
		return
	}
	removed, err := s.db.RemovePhoto(name, photoPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "photo not found")
		return
	}
	// Best-effort: a missing file must not block the DB cleanup.
	_ = os.Remove(full)
	s.regenerateThumbAfterDelete(p, photoPath)
	s.reload()
	remaining := s.db.Get(name)
	count := 0
	if remaining != nil {
		count = len(remaining.Photos)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": photoPath,
		"photos":  count,
	})
}

// regenerateThumbAfterDelete repairs the thumbnail sidecar when the deleted
// photo was its source: the next remaining photo that still detects a face
// becomes the new source; when none does (or no photos remain), the
// thumbnail is cleared. Best-effort throughout — never fails the delete.
func (s *Server) regenerateThumbAfterDelete(p *db.Person, deletedPhoto string) {
	if p.ThumbSrc != deletedPhoto {
		return // thumbnail not based on the removed photo
	}
	for _, ph := range p.Photos {
		full := personPhotoPath(s.cfg.PeopleDir, p, ph.Path)
		if full == "" {
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		faces, err := s.eng.Detect(b)
		if err != nil || len(faces) == 0 {
			continue
		}
		best, _ := engine.LargestFace(faces)
		jpg, err := engine.FaceThumb(b, best, enroll.ThumbSize)
		if err != nil {
			continue
		}
		if err := s.db.SetThumbnail(p.ID, jpg, ph.Path); err == nil {
			return // regenerated from the first usable photo
		}
	}
	_ = s.db.ClearThumbnail(p.ID) // no usable source left
}

// handleSelectThumb regenerates a person's face thumbnail from one of their
// enrolled photos, chosen via the UI's thumbnail modal. Body: {"photo": path}.
func (s *Server) handleSelectThumb(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Photo string `json:"photo"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	full := personPhotoPath(s.cfg.PeopleDir, p, body.Photo)
	if full == "" {
		writeErr(w, http.StatusNotFound, "photo not found")
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		writeErr(w, http.StatusNotFound, "photo file is missing from the people folder")
		return
	}
	faces, err := s.eng.Detect(b)
	if err != nil {
		writeErr(w, engineErrStatus(err), "detect failed: "+err.Error())
		return
	}
	if len(faces) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "no face detected in that photo")
		return
	}
	// Use the largest face, mirroring enrollment.
	best, _ := engine.LargestFace(faces)
	jpg, err := engine.FaceThumb(b, best, enroll.ThumbSize)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "could not crop face: "+err.Error())
		return
	}
	if err := s.db.SetThumbnail(p.ID, jpg, body.Photo); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"person": p.Name,
		"thumb":  thumbURL(p.ID, body.Photo),
	})
}

// enrolledPhoto reports whether photoPath is one of the person's stored
// photos and is a safe relative path (no absolute, no escaping).
func enrolledPhoto(p *db.Person, photoPath string) bool {
	if photoPath == "" || filepath.IsAbs(photoPath) {
		return false
	}
	for _, ph := range p.Photos {
		if ph.Path == photoPath {
			return true
		}
	}
	return false
}

// personPhotoPath resolves an enrolled photo path to a file under the
// person's folder in the people dir, or "" when the path is unsafe.
func personPhotoPath(peopleDir string, p *db.Person, photoPath string) string {
	if !enrolledPhoto(p, photoPath) {
		return ""
	}
	dir := filepath.Join(peopleDir, p.Name)
	full := filepath.Join(dir, filepath.FromSlash(photoPath))
	if !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return ""
	}
	return full
}

// thumbURL builds the thumbnail URL with a cache-buster derived from the
// source photo, so browsers refetch after a re-selection.
func thumbURL(personID, srcPhoto string) string {
	u := "/api/thumbs/" + personID + ".jpg"
	if srcPhoto != "" {
		u += "?v=" + db.HashBytes([]byte(srcPhoto))[:8]
	}
	return u
}
