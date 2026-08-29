package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
)

// stubEngine implements the Engine interface without any real inference.
type stubEngine struct {
	faces     []engine.Face
	threshold float64
}

func (s *stubEngine) Recognize(b []byte) ([]engine.Face, error) { return s.faces, nil }
func (s *stubEngine) Detect(b []byte) ([]engine.Face, error)    { return s.faces, nil }
func (s *stubEngine) EmbedFace(b []byte, f engine.Face) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (s *stubEngine) SetThreshold(t float64)   { s.threshold = t }
func (s *stubEngine) Threshold() float64        { return s.threshold }
func (s *stubEngine) Ping() error               { return nil }

func newTestServer(t *testing.T, eng Engine) (*Server, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := config.Default()
	cfg.Addr = ":0"
	cfg.PeopleDir = filepath.Join(t.TempDir(), "people")
	s := New(cfg, eng, database, nil)
	return s, database
}

func multipartBody(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	fw.Write(data)
	w.Close()
	return &buf, w.FormDataContentType()
}

// multipartFiles builds a multipart body with several files under the
// "images" field, as the enroll endpoint expects.
func multipartFiles(t *testing.T, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, err := w.CreateFormFile("images", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{threshold: 0.45})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health: got %d", rec.Code)
	}
	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("unexpected health body: %v", body)
	}
}

func TestRecognizeMultiFace(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 50, 60}, Name: "Alice", PersonID: "alice", Confidence: 0.9, Score: 0.8},
		{BBox: [4]float64{100, 100, 40, 50}, Name: "unknown", Confidence: 0.3, Score: 0.7},
	}}
	s, _ := newTestServer(t, eng)

	body, ct := multipartBody(t, "image", "photo.jpg", []byte("fake-image-bytes"))
	req := httptest.NewRequest(http.MethodPost, "/api/recognize", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int           `json:"count"`
		Faces []engine.Face `json:"faces"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 2 || len(resp.Faces) != 2 {
		t.Fatalf("expected 2 faces, got %d", resp.Count)
	}
	if resp.Faces[0].Name != "Alice" || resp.Faces[1].Name != "unknown" {
		t.Errorf("unexpected faces: %+v", resp.Faces)
	}
}

func TestRecognizeEmpty(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{faces: nil})
	body, ct := multipartBody(t, "image", "x.jpg", []byte("img"))
	req := httptest.NewRequest(http.MethodPost, "/api/recognize", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["count"].(float64) != 0 {
		t.Errorf("expected count 0, got %v", resp["count"])
	}
}

func TestRecognizeMethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{})
	req := httptest.NewRequest(http.MethodGet, "/api/recognize", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestPeopleListAndDelete(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{})
	database.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1})
	database.AddPhoto("Bob", "b/1.jpg", []byte("y"), []float32{2})

	req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var resp struct {
		Count  int `json:"count"`
		People []struct {
			Name   string `json:"name"`
			Photos int    `json:"photos"`
		} `json:"people"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Count != 2 {
		t.Fatalf("expected 2 people, got %d", resp.Count)
	}

	// Delete Bob.
	req = httptest.NewRequest(http.MethodDelete, "/api/people/Bob", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d", rec.Code)
	}
	if database.Get("Bob") != nil {
		t.Errorf("Bob should be removed")
	}

	// Delete missing -> 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/people/Nobody", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing person, got %d", rec.Code)
	}
}

func TestConfigThreshold(t *testing.T) {
	eng := &stubEngine{threshold: 0.45}
	s, _ := newTestServer(t, eng)

	// Valid update.
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		bytes.NewReader([]byte(`{"threshold":0.6}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || eng.threshold != 0.6 {
		t.Errorf("threshold not updated: code=%d t=%v", rec.Code, eng.threshold)
	}

	// Invalid value.
	req = httptest.NewRequest(http.MethodPost, "/api/config",
		bytes.NewReader([]byte(`{"threshold":9}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for out-of-range threshold, got %d", rec.Code)
	}
}

func TestIndexServed(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{})
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d", path, rec.Code)
		}
	}
}

// enrollPerson POSTs files to /api/people/{name}/enroll.
func enrollPerson(t *testing.T, s *Server, name string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartFiles(t, files)
	req := httptest.NewRequest(http.MethodPost, "/api/people/"+name+"/enroll", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEnrollPersonPersistsImages(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}
	s, database := newTestServer(t, eng)
	imgA, imgB := []byte("fake-jpeg-A"), []byte("fake-jpeg-B")

	rec := enrollPerson(t, s, "Alice", map[string][]byte{"a.jpg": imgA, "b.jpg": imgB})
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Added int      `json:"added"`
		Saved []string `json:"saved"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Added != 2 || len(resp.Saved) != 2 {
		t.Fatalf("expected added=2 saved=2, got added=%d saved=%v", resp.Added, resp.Saved)
	}

	p := database.Get("Alice")
	if p == nil || len(p.Photos) != 2 {
		t.Fatalf("expected Alice with 2 photos, got %+v", p)
	}
	for _, img := range map[string][]byte{"a.jpg": imgA, "b.jpg": imgB} {
		h := db.HashBytes(img)
		rel := "Alice/" + h[:12] + ".jpg"
		if !slicesContain(resp.Saved, rel) {
			t.Errorf("saved %q missing from response %v", rel, resp.Saved)
		}
		if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, rel)); err != nil {
			t.Errorf("image not written to people folder: %v", err)
		}
	}
	for _, ph := range p.Photos {
		if strings.Contains(ph.Path, "/") {
			t.Errorf("DB photo path should be a folder-relative basename, got %q", ph.Path)
		}
		if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Alice", ph.Path)); err != nil {
			t.Errorf("DB path %q has no file on disk: %v", ph.Path, err)
		}
	}
}

func TestEnrollPersonNoFacePersistsNothing(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{faces: nil}) // detects nothing

	rec := enrollPerson(t, s, "Bob", map[string][]byte{"x.jpg": []byte("no-face")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Bob")); !os.IsNotExist(err) {
		t.Errorf("no-face upload should not create a people folder, err=%v", err)
	}
	if database.Get("Bob") != nil {
		t.Errorf("no-face upload should not create a DB entry")
	}
}

func TestEnrollPersonIdempotent(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}
	s, database := newTestServer(t, eng)
	img := []byte("same-image-bytes")
	h := db.HashBytes(img)

	for i := 0; i < 2; i++ { // upload the same bytes twice
		rec := enrollPerson(t, s, "Alice", map[string][]byte{"again.jpg": img})
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d: got %d (%s)", i, rec.Code, rec.Body.String())
		}
	}

	p := database.Get("Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("expected exactly 1 photo after duplicate uploads, got %+v", p)
	}
	if p.Photos[0].Path != h[:12]+".jpg" {
		t.Errorf("unexpected DB path %q", p.Photos[0].Path)
	}
	entries, err := os.ReadDir(filepath.Join(s.cfg.PeopleDir, "Alice"))
	if err != nil || len(entries) != 1 {
		t.Errorf("expected exactly 1 file in the people folder (err=%v, n=%d)", err, len(entries))
	}
}

func TestEnrollPersonRejectsBadName(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}})

	rec := enrollPerson(t, s, `a\b`, map[string][]byte{"x.jpg": []byte("img")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path-like name, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(s.cfg.PeopleDir); !os.IsNotExist(err) {
		t.Errorf("rejected enroll must not create the people dir, err=%v", err)
	}
}

func TestEnrollPersonWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	s, database := newTestServer(t, &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}})
	if err := os.MkdirAll(s.cfg.PeopleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(s.cfg.PeopleDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(s.cfg.PeopleDir, 0o755) })

	rec := enrollPerson(t, s, "Alice", map[string][]byte{"x.jpg": []byte("img")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when the people dir is read-only, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "people folder") {
		t.Errorf("failure should mention the people folder, got %s", rec.Body.String())
	}
	if database.Get("Alice") != nil {
		t.Errorf("failed persistence must not leave a DB entry")
	}
}

func slicesContain(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
