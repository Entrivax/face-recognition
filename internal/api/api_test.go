package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
