// Package api exposes the recogn engine over HTTP: a JSON REST API plus the
// embedded web UI. It depends on a small Engine interface (not the concrete
// type) so handlers are testable with a stub.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	s.mux = m
}

// Handler returns the root http.Handler (useful for httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the HTTP server on the configured address.
func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Addr, s.mux)
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
	}
	out := make([]summary, 0, len(people))
	for _, p := range people {
		out = append(out, summary{ID: p.ID, Name: p.Name, Photos: len(p.Photos), Embeds: len(p.Photos)})
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
		"id": p.ID, "name": p.Name, "photos": photos,
	})
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

// handleEnrollFolder rescans the people/ directory (incremental).
func (s *Server) handleEnrollFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	res, err := enroll.Scan(s.eng, s.db, enroll.Options{
		PeopleDir: s.cfg.PeopleDir,
		Force:     force,
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
		"skipped_count": len(res.Skipped),
	})
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
