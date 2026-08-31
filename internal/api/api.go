// Package api exposes the recogn engine over HTTP: a JSON REST API plus the
// embedded web UI. It depends on a small Engine interface (not the concrete
// type) so handlers are testable with a stub.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
	"recogn/internal/enroll"
	"recogn/internal/web"
)

// Engine is the behaviour the API needs from the recognition engine.
// *engine.Engine satisfies it; tests provide a stub.
type Engine interface {
	Recognize(imgBytes []byte) ([]engine.Face, error)
	Detect(imgBytes []byte) ([]engine.Face, error)
	EmbedFace(imgBytes []byte, f engine.Face) ([]float32, error)
	SetThreshold(t float64)
	Threshold() float64
	Ping() error
}

// maxUpload caps a single request body to 32 MiB.
const maxUpload = 32 << 20

// Server wires the HTTP routes.
type Server struct {
	cfg     config.Config
	eng     Engine
	db      *db.DB
	refresh func(e Engine, d *db.DB) // push DB identities into the engine
	mux     *http.ServeMux
}

// New builds a Server. refresh is called after any mutation to reload the
// engine's identity set from the DB (may be nil).
func New(cfg config.Config, eng Engine, database *db.DB, refresh func(Engine, *db.DB)) *Server {
	s := &Server{cfg: cfg, eng: eng, db: database, refresh: refresh}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("/", s.handleIndex)
	m.HandleFunc("/api/health", s.handleHealth)
	m.HandleFunc("/api/recognize", s.handleRecognize)
	m.HandleFunc("/api/people", s.handlePeople)              // GET list
	m.HandleFunc("/api/people/", s.handlePersonSubroutes)     // enroll/delete
	m.HandleFunc("/api/enroll", s.handleEnrollFolder)         // rescan people/
	m.HandleFunc("/api/config", s.handleConfig)               // GET/POST threshold
	m.HandleFunc("/api/thumbs/", s.handleThumb)               // face thumbnails
	s.mux = m
}

// Handler returns the root http.Handler (useful for httptest), wrapped in the
// request logger.
func (s *Server) Handler() http.Handler { return s.loggingMiddleware(s.mux) }

// ListenAndServe starts the HTTP server on the configured address.
func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Addr, s.Handler())
}

// loggingMiddleware logs one line per request: method, path, status, duration.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// statusWriter captures the response status code for logging while passing
// every Write/WriteHeader through untouched.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// ---- handlers ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Serve index.html at "/" and static assets (app.js, style.css, ...) for
	// any other non-API path. API routes are registered on more specific
	// patterns, so they take precedence over this catch-all.
	web.ServeUI(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"people":    len(s.db.People()),
		"threshold": s.eng.Threshold(),
	})
}

// handleRecognize accepts a multipart image under the field "image" (or a raw
// body) and returns every detected face with its identity.
func (s *Server) handleRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	img, err := readImage(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := s.eng.Recognize(img)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "recognition failed: "+err.Error())
		return
	}
	for i := range faces {
		faces[i].Embedding = nil
	}
	// ?draw=1: respond with the annotated image (boxes + labels) instead of
	// the JSON report.
	if r.URL.Query().Get("draw") == "1" {
		out, err := engine.Annotate(img, faces)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "annotate failed: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(faces),
		"faces": faces,
	})
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	people := s.db.People()
	type summary struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Photos  int    `json:"photos"`
		Embeds  int    `json:"embeddings"`
		Thumb   string `json:"thumb"`
	}
	out := make([]summary, 0, len(people))
	for _, p := range people {
		var thumb string
		if p.Thumb != "" {
			thumb = thumbURL(p.ID, p.ThumbSrc)
		}
		out = append(out, summary{
			ID: p.ID, Name: p.Name,
			Photos: len(p.Photos), Embeds: len(p.Photos),
			Thumb: thumb,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out, "count": len(out)})
}

// handlePersonSubroutes dispatches /api/people/{name} and
// /api/people/{name}/enroll.
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

func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request, name string) {
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	// Do not leak raw embeddings in the simple GET.
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
		writeErr(w, http.StatusBadGateway, "recognition failed: "+err.Error())
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
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
		writeErr(w, http.StatusBadGateway, "detect failed: "+err.Error())
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

func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request, name string) {
	removed, err := s.db.RemovePerson(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{"removed": name})
}

// handleRenamePerson renames a person end-to-end: the people/<Name> dataset
// folder, the derived person ID, the thumbnail sidecar file, and the DB
// record. Body: {"name": "<new name>"}. The folder is moved first; a DB
// failure rolls it back so the dataset never drifts from the database.
func (s *Server) handleRenamePerson(w http.ResponseWriter, r *http.Request, oldName string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(oldName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	newName := strings.TrimSpace(body.Name)
	if err := enroll.CheckName(newName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(oldName)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	oldCanonical := p.Name // p aliases the DB record; capture before mutating
	if newName == oldCanonical {
		writeErr(w, http.StatusBadRequest, "new name is unchanged")
		return
	}
	if other := s.db.Get(newName); other != nil && other.ID != p.ID {
		writeErr(w, http.StatusConflict, fmt.Sprintf("a person named %q is already enrolled", newName))
		return
	}
	// Folder: refuse to clobber a different existing folder (this also
	// blocks case-collision siblings like Serra/serra on case-sensitive
	// filesystems), while os.SameFile keeps case-only renames working on
	// case-insensitive ones. A missing old folder (person without a dataset
	// folder) is fine — only the DB changes then.
	oldDir := filepath.Join(s.cfg.PeopleDir, oldCanonical)
	newDir := filepath.Join(s.cfg.PeopleDir, newName)
	folderMoved := false
	if oldFi, err := os.Stat(oldDir); err == nil {
		if newFi, err2 := os.Stat(newDir); err2 == nil && !os.SameFile(oldFi, newFi) {
			writeErr(w, http.StatusConflict, "a people folder with that name already exists")
			return
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			writeErr(w, http.StatusInternalServerError, "rename people folder: "+err.Error())
			return
		}
		folderMoved = true
	}
	upd, err := s.db.RenamePerson(oldCanonical, newName)
	if err != nil {
		if folderMoved { // keep dataset and DB consistent
			_ = os.Rename(newDir, oldDir)
		}
		switch {
		case errors.Is(err, db.ErrPersonNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, db.ErrNameTaken):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed":  true,
		"old_name": oldCanonical,
		"name":     upd.Name,
		"id":       upd.ID,
	})
}

// handleEnrollPerson adds one or more uploaded photos to a (new or existing)
// person. Accepts multipart files under "images" (or "image"). On success each
// photo's embedding goes into the DB and the image file itself is written into
// the person's folder under the people directory, so runtime uploads become
// part of the dataset.
func (s *Server) handleEnrollPerson(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	var files []struct {
		name string
		data []byte
	}
	for _, field := range []string{"images", "image", "files"} {
		if r.MultipartForm == nil {
			break
		}
		for _, fh := range r.MultipartForm.File[field] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, maxUpload))
			f.Close()
			if err != nil {
				continue
			}
			files = append(files, struct {
				name string
				data []byte
			}{fh.Filename, b})
		}
		if len(files) > 0 {
			break
		}
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "no image files provided (field 'images')")
		return
	}
	added := 0
	var failures []string
	saved := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := enroll.EnrollBytes(s.eng, s.db, s.cfg.PeopleDir, name, f.name, f.data)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f.name, err))
			continue
		}
		added++
		saved = append(saved, rel)
	}
	s.reload()
	resp := map[string]any{"person": name, "added": added, "total": len(files), "saved": saved}
	if len(failures) > 0 {
		resp["failures"] = failures
	}
	status := http.StatusOK
	if added == 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, resp)
}

// handleEnrollFace enrolls one specific face of an uploaded photo as (or
// into) the named person. Body: multipart "image" plus "face_index" — a
// 1-based index into the detection order /api/recognize reported for the same
// bytes (detection is deterministic). The UI's "name this face" action on
// unknown recognition results uses it.
func (s *Server) handleEnrollFace(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(r.FormValue("face_index")))
	if err != nil || idx < 1 {
		writeErr(w, http.StatusBadRequest, "face_index must be a positive integer")
		return
	}
	img, err := readMultipartFile(r, []string{"image", "file", "photo"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := enroll.EnrollFace(s.eng, s.db, s.cfg.PeopleDir, name, "upload", img, idx)
	if err != nil {
		switch {
		case errors.Is(err, enroll.ErrFaceIndex):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, enroll.ErrNoFace):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{"person": name, "saved": saved})
}

// readMultipartFile returns the bytes of the first uploaded file under one of
// the given multipart fields, capped at maxUpload.
func readMultipartFile(r *http.Request, fields []string) ([]byte, error) {
	if r.MultipartForm == nil {
		return nil, errors.New("no file provided")
	}
	for _, field := range fields {
		for _, fh := range r.MultipartForm.File[field] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, maxUpload))
			f.Close()
			if err != nil {
				continue
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no image file provided (field '%s')", fields[0])
}

// handleEnrollFolder rescans the people/ directory (incremental).
func (s *Server) handleEnrollFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	prune := r.URL.Query().Get("prune") == "true"
	res, err := enroll.Scan(s.eng, s.db, enroll.Options{
		PeopleDir: s.cfg.PeopleDir,
		Force:     force,
		Prune:     prune,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"people":        res.PeopleSeen,
		"added":         res.PhotosAdded,
		"kept":          res.PhotosKept,
		"failed":        res.PhotosFailed,
		"pruned":        res.PhotosPruned,
		"skipped_count": len(res.Skipped),
	})
}

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

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"threshold": s.eng.Threshold()})
	case http.MethodPost:
		var body struct {
			Threshold float64 `json:"threshold"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if body.Threshold < 0 || body.Threshold > 1 {
			writeErr(w, http.StatusBadRequest, "threshold must be between 0 and 1")
			return
		}
		s.eng.SetThreshold(body.Threshold)
		// Persist so the value survives restarts (restored in openEngine
		// unless an explicit --threshold flag overrides it).
		if err := s.db.SetThreshold(body.Threshold); err != nil {
			writeErr(w, http.StatusInternalServerError, "persist threshold: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"threshold": s.eng.Threshold()})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "unsupported method")
	}
}

// reload pushes DB identities into the engine after a mutation.
func (s *Server) reload() {
	if s.refresh != nil {
		s.refresh(s.eng, s.db)
	}
}

// ---- helpers ----

// readImage extracts image bytes from a multipart "image" field or raw body.
func readImage(r *http.Request) ([]byte, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			return nil, fmt.Errorf("parse multipart: %w", err)
		}
		for _, field := range []string{"image", "file", "photo"} {
			f, _, err := r.FormFile(field)
			if err == nil {
				defer f.Close()
				return io.ReadAll(io.LimitReader(f, maxUpload))
			}
		}
		return nil, errors.New("multipart field 'image' not found")
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxUpload))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("empty body")
	}
	return b, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
