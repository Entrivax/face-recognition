package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
func (s *stubEngine) SetThreshold(t float64) { s.threshold = t }
func (s *stubEngine) Threshold() float64     { return s.threshold }
func (s *stubEngine) Ping() error            { return nil }

func newTestServer(t *testing.T, eng Engine) (*Server, *db.DB) {
	t.Helper()
	// Hermetic env: admin auth must be off unless a test opts in, and the
	// passkey store must not touch the real data/ directory.
	t.Setenv("RECOGN_ADMIN_PASSWORD_HASH", "")
	t.Setenv("RECOGN_WEBAUTHN_RPID", "")
	t.Setenv("RECOGN_WEBAUTHN_ORIGIN", "")
	t.Setenv("RECOGN_WEBAUTHN_RP_NAME", "")
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := config.Default()
	cfg.Addr = ":0"
	cfg.PeopleDir = filepath.Join(t.TempDir(), "people")
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	s, err := New(cfg, eng, database, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
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
		{BBox: [4]float64{10, 10, 50, 60}, Name: "Alice", PersonID: "alice", Confidence: 0.9, Score: 0.8,
			Matches: []engine.Match{
				{PersonID: "alice", Name: "Alice", Score: 0.9},
				{PersonID: "bob", Name: "Bob", Score: 0.55},
			}},
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
	// Ranked candidate list passes through; embeddings never do.
	if len(resp.Faces[0].Matches) != 2 || resp.Faces[0].Matches[0].Name != "Alice" ||
		resp.Faces[0].Matches[1].Name != "Bob" {
		t.Errorf("unexpected matches: %+v", resp.Faces[0].Matches)
	}
	if len(resp.Faces[1].Matches) != 0 {
		t.Errorf("unknown face should have no matches, got %+v", resp.Faces[1].Matches)
	}
	if strings.Contains(rec.Body.String(), "embedding") {
		t.Errorf("response must not leak embeddings")
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

// TestConfigThresholdPersists checks that POST /api/config writes the
// threshold into the DB file so it survives a restart (openEngine restores it
// unless an explicit --threshold flag overrides).
func TestConfigThresholdPersists(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{threshold: 0.45})
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		bytes.NewReader([]byte(`{"threshold":0.61}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set threshold: got %d (%s)", rec.Code, rec.Body.String())
	}
	if database.Threshold() == nil || *database.Threshold() != 0.61 {
		t.Fatalf("threshold not persisted, db=%v", database.Threshold())
	}
	// An invalid value must not touch the stored setting.
	req = httptest.NewRequest(http.MethodPost, "/api/config",
		bytes.NewReader([]byte(`{"threshold":7}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if *database.Threshold() != 0.61 {
		t.Fatalf("invalid request clobbered the stored threshold: %v", *database.Threshold())
	}
}

// TestRecognizeDraw checks ?draw=1: the response is the annotated JPEG image.
func TestRecognizeDraw(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 50, 60}, Name: "Alice", Confidence: 0.9},
	}}
	s, _ := newTestServer(t, eng)

	// Real decodable image content — Annotate must decode it.
	img := image.NewRGBA(image.Rect(0, 0, 160, 120))
	for i := range img.Pix {
		img.Pix[i] = 60
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	body, ct := multipartBody(t, "image", "photo.png", pngBuf.Bytes())
	req := httptest.NewRequest(http.MethodPost, "/api/recognize?draw=1", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("draw recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", got)
	}
	gotImg, format, err := image.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not a decodable image: %v", err)
	}
	if format != "jpeg" || gotImg.Bounds().Dx() != 160 || gotImg.Bounds().Dy() != 120 {
		t.Fatalf("unexpected image %v format=%q", gotImg.Bounds(), format)
	}

	// Without the flag the JSON shape is unchanged.
	body, ct = multipartBody(t, "image", "photo.png", pngBuf.Bytes())
	req = httptest.NewRequest(http.MethodPost, "/api/recognize", body)
	req.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("plain recognize content type = %q, want application/json", got)
	}
}

func TestIndexServed(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{})

	// The single-page UI itself (built by `make ui` into internal/web/dist).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "recogn") {
		t.Fatalf("GET /: body does not look like the UI: %.80s", rec.Body.String())
	}

	// Every local asset the page references (hashed bundles, logo) serves too.
	body := rec.Body.String()
	seen := map[string]bool{}
	for _, m := range assetRefRe.FindAllStringSubmatch(body, -1) {
		u := m[1]
		if seen[u] {
			continue
		}
		seen[u] = true
		req := httptest.NewRequest(http.MethodGet, u, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d", u, rec.Code)
		}
	}
	if len(seen) == 0 {
		t.Fatal("GET /: no local assets referenced")
	}

	// Unknown paths are not rewritten to the SPA (no fallback).
	req = httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nonexistent: got %d, want 404", rec.Code)
	}
}

// assetRefRe extracts local (root-relative) src/href URLs from the built
// index.html so the test can verify each one is served.
var assetRefRe = regexp.MustCompile(`(?:src|href)="(/[^"]+)"`)

// enrollFaceRequest POSTs a single image plus a face_index field to
// /api/people/{name}/enroll-face.
func enrollFaceRequest(t *testing.T, s *Server, name string, img []byte, idx string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image", "photo.jpg")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(img); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := w.WriteField("face_index", idx); err != nil {
		t.Fatalf("write field: %v", err)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/people/"+name+"/enroll-face", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEnrollFace(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{0, 0, 10, 10}},
		{BBox: [4]float64{20, 20, 12, 12}},
	}}
	s, database := newTestServer(t, eng)
	img := []byte("fake-jpeg-bytes")

	// Index 2 enrolls the second detected face (1-based, list-row order).
	rec := enrollFaceRequest(t, s, "Alice", img, "2")
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll-face: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := database.Get("Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("person not enrolled: %+v", p)
	}
	// The uploaded image is saved into the person's people folder.
	if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Alice", p.Photos[0].Path)); err != nil {
		t.Fatalf("image not saved into the people folder: %v", err)
	}

	// Out-of-range index → 400, nothing enrolled.
	rec = enrollFaceRequest(t, s, "Bob", img, "3")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range index, got %d", rec.Code)
	}
	if database.Get("Bob") != nil {
		t.Fatal("failed enroll must not create a person")
	}
	// Non-integer index → 400.
	rec = enrollFaceRequest(t, s, "Bob", img, "x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-integer index, got %d", rec.Code)
	}
	// Leading-dot name → 400. (Path-like names containing ".." never reach
	// the handler: http.ServeMux redirects them during path cleaning.)
	rec = enrollFaceRequest(t, s, ".hidden", img, "1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad name, got %d", rec.Code)
	}

	// No faces at all → 422.
	empty, _ := newTestServer(t, &stubEngine{})
	rec = enrollFaceRequest(t, empty, "Carol", img, "1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when no face is detected, got %d", rec.Code)
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

// pngBytes renders a deterministic w x h gradient as PNG bytes (real,
// decodable image content, needed for thumbnail generation). seed shifts the
// gradient so different calls produce different pixels.
func pngBytes(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8((x + seed) % 256), uint8((y + seed) % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestPeopleThumbServing(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newTestServer(t, eng)

	// Alice: real image → thumbnail generated. Bob: fake bytes → crop fails,
	// enrollment still succeeds but without a thumbnail (best-effort).
	rec := enrollPerson(t, s, "Alice", map[string][]byte{"a.png": pngBytes(t, 120, 120, 0)})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = enrollPerson(t, s, "Bob", map[string][]byte{"b.jpg": []byte("not-an-image")})
	if rec.Code != http.StatusOK {
		t.Fatalf("bob enroll: got %d (%s)", rec.Code, rec.Body.String())
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, req)
		return r
	}

	// People list carries thumb URLs: set for Alice, empty for Bob.
	var resp struct {
		People []struct {
			ID    string `json:"id"`
			Thumb string `json:"thumb"`
		} `json:"people"`
	}
	if err := json.NewDecoder(get("/api/people").Body).Decode(&resp); err != nil {
		t.Fatalf("decode people: %v", err)
	}
	thumbs := map[string]string{}
	for _, p := range resp.People {
		thumbs[p.ID] = p.Thumb
	}
	if !strings.HasPrefix(thumbs["alice"], "/api/thumbs/alice.jpg?v=") {
		t.Errorf("alice thumb = %q, want a cache-busted URL", thumbs["alice"])
	}
	if thumbs["bob"] != "" {
		t.Errorf("bob should have no thumb, got %q", thumbs["bob"])
	}

	// Serving: JPEG bytes with cache headers.
	r := get("/api/thumbs/alice.jpg")
	if r.Code != http.StatusOK {
		t.Fatalf("GET thumb: got %d", r.Code)
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Errorf("content type %q, want image/jpeg", ct)
	}
	if r.Header().Get("Cache-Control") == "" {
		t.Errorf("expected Cache-Control header")
	}
	if r.Body.Len() == 0 {
		t.Errorf("empty thumbnail body")
	}

	// Unknown person / no-thumbnail person / bad method.
	if code := get("/api/thumbs/nosuch.jpg").Code; code != http.StatusNotFound {
		t.Errorf("unknown id: got %d, want 404", code)
	}
	if code := get("/api/thumbs/bob.jpg").Code; code != http.StatusNotFound {
		t.Errorf("missing thumbnail: got %d, want 404", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/thumbs/alice.jpg", nil)
	r2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST thumb: got %d, want 405", r2.Code)
	}
}

// TestPhotoDetect covers inspecting an enrolled photo's detected faces via
// the /detect subroute (used by the photos manager modal).
func TestPhotoDetect(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 40, 40}, Name: "Alice", PersonID: "alice", Confidence: 0.9},
	}}
	s, database := newTestServer(t, eng)
	img := pngBytes(t, 120, 120, 0)
	photo := db.HashBytes(img)[:12] + ".png"
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}

	r := getJSON(t, s, "/api/people/Alice/photos/"+photo+"/detect", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("detect: got %d (%s)", r.Code, r.Body.String())
	}
	var resp struct {
		Photo string        `json:"photo"`
		Count int           `json:"count"`
		Faces []engine.Face `json:"faces"`
	}
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Photo != photo || resp.Count != 1 || len(resp.Faces) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Faces[0].Name != "Alice" {
		t.Errorf("face identity = %q, want Alice", resp.Faces[0].Name)
	}
	if strings.Contains(r.Body.String(), "embedding") {
		t.Errorf("detect response must not leak embeddings")
	}

	// Unknown photo / missing file / unknown person / bad method.
	if code := getJSON(t, s, "/api/people/Alice/photos/nope.png/detect", nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo: got %d, want 404", code)
	}
	// Enrolled in the DB but the file is gone from disk.
	if err := database.AddPhoto("Alice", "ghost.jpg", img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	if code := getJSON(t, s, "/api/people/Alice/photos/ghost.jpg/detect", nil).Code; code != http.StatusNotFound {
		t.Errorf("missing file: got %d, want 404", code)
	}
	if code := getJSON(t, s, "/api/people/Nobody/photos/"+photo+"/detect", nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown person: got %d, want 404", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/people/Alice/photos/"+photo+"/detect", nil)
	r2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST detect: got %d, want 405", r2.Code)
	}
}

// TestPhotoDelete covers removing a single photo: DB entry and file are gone,
// the other photo survives, and errors match the rest of the photo routes.
func TestPhotoDelete(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 120, 120, 7)
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	photoA := db.HashBytes(imgA)[:12] + ".png"

	req := httptest.NewRequest(http.MethodDelete, "/api/people/Alice/photos/"+photoA, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Removed string `json:"removed"`
		Photos  int    `json:"photos"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Removed != photoA || resp.Photos != 1 {
		t.Errorf("unexpected delete response: %+v", resp)
	}
	p := database.Get("Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("expected Alice with 1 remaining photo, got %+v", p)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Alice", photoA)); !os.IsNotExist(err) {
		t.Errorf("photo file should be gone from the people folder, err=%v", err)
	}

	if code := req2(t, s, http.MethodDelete, "/api/people/Alice/photos/nope.png").Code; code != http.StatusNotFound {
		t.Errorf("unknown photo delete: got %d, want 404", code)
	}
	if code := req2(t, s, http.MethodPost, "/api/people/Alice/photos/"+photoA).Code; code != http.StatusMethodNotAllowed {
		t.Errorf("POST to delete route: got %d, want 405", code)
	}
}

// TestPhotoDeleteThumbnail checks that deleting the thumbnail source photo
// regenerates the avatar from another photo, and clearing the last photo
// removes the thumbnail entirely while the person survives.
func TestPhotoDeleteThumbnail(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 160, 90, 100)

	// One upload per request so the thumbnail source is deterministic.
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	photoA := db.HashBytes(imgA)[:12] + ".png"
	photoB := db.HashBytes(imgB)[:12] + ".png"
	p := database.Get("Alice")
	if p.ThumbSrc != photoA {
		t.Fatalf("ThumbSrc = %q, want %q", p.ThumbSrc, photoA)
	}

	// Delete the thumbnail source: avatar regenerates from the other photo.
	if code := req2(t, s, http.MethodDelete, "/api/people/Alice/photos/"+photoA).Code; code != http.StatusOK {
		t.Fatalf("delete thumb-src photo: got %d", code)
	}
	p = database.Get("Alice")
	if p.ThumbSrc != photoB {
		t.Errorf("ThumbSrc = %q, want %q (regenerated from the remaining photo)", p.ThumbSrc, photoB)
	}
	if p.Thumb == "" {
		t.Errorf("thumbnail sidecar should have been regenerated, not cleared")
	}
	if _, err := os.Stat(filepath.Join(database.ThumbDir(), p.Thumb)); err != nil {
		t.Errorf("regenerated sidecar missing: %v", err)
	}

	// Delete the last photo: thumbnail cleared, person stays enrolled.
	if code := req2(t, s, http.MethodDelete, "/api/people/Alice/photos/"+photoB).Code; code != http.StatusOK {
		t.Fatalf("delete last photo: got %d", code)
	}
	p = database.Get("Alice")
	if p == nil {
		t.Fatalf("person should remain after removing their last photo")
	}
	if p.Thumb != "" || p.ThumbSrc != "" {
		t.Errorf("thumbnail should be cleared, got thumb=%q src=%q", p.Thumb, p.ThumbSrc)
	}
	if _, err := os.Stat(filepath.Join(database.ThumbDir(), "alice.jpg")); !os.IsNotExist(err) {
		t.Errorf("sidecar file should be removed, err=%v", err)
	}
}

// req2 sends a request with the given method and returns the recorder.
func req2(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// postJSON sends a JSON body to the server and returns the recorder.
func postJSON(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func getJSON(t *testing.T, s *Server, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if out != nil {
		if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return rec
}

// TestPersonRename covers POST /api/people/{name}/rename: the DB record, the
// people/<Name> folder and the thumbnail sidecar all move together, and the
// old name/id stop resolving.
func TestPersonRename(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	img := pngBytes(t, 120, 120, 0)
	photo := db.HashBytes(img)[:12] + ".png"
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	// Bob is enrolled too (DB-only), so renaming Alice→Bob must conflict.
	_ = database.AddPhoto("Bob", "b.png", img, []float32{1})
	reloadCount := 0
	s.refresh = func(Engine, *db.DB) { reloadCount++ }

	r := postJSON(t, s, "/api/people/Alice/rename", map[string]string{"name": "Alicia"})
	if r.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", r.Code, r.Body.String())
	}
	var resp struct {
		Renamed bool   `json:"renamed"`
		OldName string `json:"old_name"`
		Name    string `json:"name"`
		ID      string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Renamed || resp.OldName != "Alice" || resp.Name != "Alicia" || resp.ID != "alicia" {
		t.Errorf("unexpected rename response: %+v", resp)
	}
	if reloadCount != 1 {
		t.Errorf("engine should be reloaded after a rename, got %d reloads", reloadCount)
	}

	// DB: old name gone, new name resolves, photos intact.
	if database.Get("Alice") != nil {
		t.Errorf("old name should be gone from the DB")
	}
	p := database.Get("Alicia")
	if p == nil || p.ID != "alicia" || len(p.Photos) != 1 || p.Photos[0].Path != photo {
		t.Fatalf("unexpected DB person: %+v", p)
	}

	// Disk: folder renamed, sidecar renamed, photo file still inside.
	if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Alice")); !os.IsNotExist(err) {
		t.Errorf("old folder should be gone, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.PeopleDir, "Alicia", photo)); err != nil {
		t.Errorf("photo file should be under the new folder: %v", err)
	}
	oldThumb := filepath.Join(database.ThumbDir(), "alice.jpg")
	newThumb := filepath.Join(database.ThumbDir(), "alicia.jpg")
	if _, err := os.Stat(oldThumb); !os.IsNotExist(err) {
		t.Errorf("old sidecar should be gone, err=%v", err)
	}
	if _, err := os.Stat(newThumb); err != nil {
		t.Errorf("renamed sidecar missing: %v", err)
	}

	// Routes follow the new identity; the old ones 404.
	if code := getJSON(t, s, "/api/people/Alicia", nil).Code; code != http.StatusOK {
		t.Errorf("GET new name: got %d, want 200", code)
	}
	if code := getJSON(t, s, "/api/people/Alice", nil).Code; code != http.StatusNotFound {
		t.Errorf("GET old name: got %d, want 404", code)
	}
	if code := getJSON(t, s, "/api/thumbs/alicia.jpg", nil).Code; code != http.StatusOK {
		t.Errorf("GET new thumb: got %d, want 200", code)
	}
	if code := getJSON(t, s, "/api/thumbs/alice.jpg", nil).Code; code != http.StatusNotFound {
		t.Errorf("GET old thumb: got %d, want 404", code)
	}
	if code := getJSON(t, s, "/api/people/Alicia/photos/"+photo, nil).Code; code != http.StatusOK {
		t.Errorf("photo under new name: got %d, want 200", code)
	}

	// Errors: collision with an enrolled person, invalid/empty/unchanged
	// name, unknown person, method.
	if code := postJSON(t, s, "/api/people/Alicia/rename", map[string]string{"name": "Bob"}).Code; code != http.StatusConflict {
		t.Errorf("name collision: got %d, want 409", code)
	}
	if code := postJSON(t, s, `/api/people/Alicia/rename`, map[string]string{"name": `a\b`}).Code; code != http.StatusBadRequest {
		t.Errorf("path-like name: got %d, want 400", code)
	}
	if code := postJSON(t, s, "/api/people/Alicia/rename", map[string]string{"name": "   "}).Code; code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", code)
	}
	if code := postJSON(t, s, "/api/people/Alicia/rename", map[string]string{"name": "Alicia"}).Code; code != http.StatusBadRequest {
		t.Errorf("unchanged name: got %d, want 400", code)
	}
	if code := postJSON(t, s, "/api/people/Nobody/rename", map[string]string{"name": "X"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown person: got %d, want 404", code)
	}
	if code := req2(t, s, http.MethodGet, "/api/people/Alicia/rename").Code; code != http.StatusMethodNotAllowed {
		t.Errorf("GET rename: got %d, want 405", code)
	}

	// A folder on disk without a DB entry blocks the rename (409), and a
	// failed rename leaves the person untouched.
	if err := os.MkdirAll(filepath.Join(s.cfg.PeopleDir, "Carol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := postJSON(t, s, "/api/people/Alicia/rename", map[string]string{"name": "Carol"}).Code; code != http.StatusConflict {
		t.Errorf("folder collision: got %d, want 409", code)
	}
	if database.Get("Carol") != nil || database.Get("Alicia") == nil {
		t.Errorf("rejected rename must not change anything")
	}

	// Case-only rename works and keeps the ID.
	if r = postJSON(t, s, "/api/people/Alicia/rename", map[string]string{"name": "ALICIA"}); r.Code != http.StatusOK {
		t.Fatalf("case-only rename: got %d (%s), want 200", r.Code, r.Body.String())
	}
	if p := database.Get("alicia"); p == nil || p.Name != "ALICIA" || p.ID != "alicia" {
		t.Errorf("unexpected person after case-only rename: %+v", p)
	}
	if _, err := os.Stat(newThumb); err != nil {
		t.Errorf("sidecar should be untouched by a case-only rename: %v", err)
	}
}

// TestPersonRenameWithoutFolder covers a DB-only person (no dataset folder):
// the rename still succeeds, touching just the DB and thumbnail bookkeeping.
func TestPersonRenameWithoutFolder(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{})
	if err := database.AddPhoto("Ghost", "g.png", []byte("img"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	r := postJSON(t, s, "/api/people/Ghost/rename", map[string]string{"name": "Spirit"})
	if r.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", r.Code, r.Body.String())
	}
	if p := database.Get("Ghost"); p != nil {
		t.Errorf("old name should be gone")
	}
	p := database.Get("Spirit")
	if p == nil || p.ID != "spirit" || len(p.Photos) != 1 {
		t.Fatalf("unexpected renamed person: %+v", p)
	}
}

// TestThumbChooser covers serving enrolled photos and regenerating the
// thumbnail from a chosen photo.
func TestThumbChooser(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 160, 90, 100)
	photoA := db.HashBytes(imgA)[:12] + ".png"
	photoB := db.HashBytes(imgB)[:12] + ".png"

	// One upload per request so the thumbnail source is deterministic.
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("upload a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("upload a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := database.Get("Alice")
	if p.ThumbSrc != photoA {
		t.Fatalf("initial ThumbSrc = %q, want %q (first upload wins)", p.ThumbSrc, photoA)
	}
	thumbPath := filepath.Join(database.ThumbDir(), p.Thumb)
	thumbBefore, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	// Serving an enrolled photo returns the stored bytes as image/png.
	r := getJSON(t, s, "/api/people/Alice/photos/"+photoB, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("photo serve: got %d", r.Code)
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("photo content type %q, want image/png", ct)
	}
	if !bytes.Equal(r.Body.Bytes(), imgB) {
		t.Errorf("served photo bytes differ from the stored upload")
	}

	// Choose photo B as the thumbnail source.
	type thumbResp struct {
		Person string `json:"person"`
		Thumb  string `json:"thumb"`
	}
	r = postJSON(t, s, "/api/people/Alice/thumbnail", map[string]string{"photo": photoB})
	if r.Code != http.StatusOK {
		t.Fatalf("select thumb: got %d (%s)", r.Code, r.Body.String())
	}
	var tr thumbResp
	if err := json.NewDecoder(r.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantURL := "/api/thumbs/alice.jpg?v=" + db.HashBytes([]byte(photoB))[:8]
	if tr.Thumb != wantURL {
		t.Errorf("thumb URL = %q, want %q", tr.Thumb, wantURL)
	}
	p = database.Get("Alice")
	if p.ThumbSrc != photoB {
		t.Errorf("ThumbSrc = %q, want %q", p.ThumbSrc, photoB)
	}
	thumbAfter, _ := os.ReadFile(thumbPath)
	if bytes.Equal(thumbBefore, thumbAfter) {
		t.Errorf("sidecar was not regenerated from the chosen photo")
	}
	// People list carries the updated cache-busted URL.
	var people struct {
		People []struct {
			ID    string `json:"id"`
			Thumb string `json:"thumb"`
		} `json:"people"`
	}
	getJSON(t, s, "/api/people", &people)
	for _, pp := range people.People {
		if pp.ID == "alice" && pp.Thumb != wantURL {
			t.Errorf("people list thumb = %q, want %q", pp.Thumb, wantURL)
		}
	}

	// Errors: unknown photo, unenrolled/unsafe path, missing file, unknown
	// person, bad name, wrong methods.
	if code := getJSON(t, s, "/api/people/Alice/photos/nope.png", nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo: got %d, want 404", code)
	}
	if code := getJSON(t, s, "/api/people/Nobody/photos/"+photoB, nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown person photo: got %d, want 404", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/people/Alice/photos/"+photoB, nil)
	r3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r3, req)
	if r3.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST photo: got %d, want 405", r3.Code)
	}
	if code := postJSON(t, s, "/api/people/Alice/thumbnail", map[string]string{"photo": "nope.png"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo select: got %d, want 404", code)
	}
	if code := postJSON(t, s, "/api/people/Alice/thumbnail", map[string]string{"photo": "../../etc/passwd"}).Code; code != http.StatusNotFound {
		t.Errorf("unsafe path select: got %d, want 404", code)
	}
	// Enrolled in the DB but the file is gone from disk.
	if err := database.AddPhoto("Alice", "ghost.jpg", imgA, []float32{1}); err != nil {
		t.Fatal(err)
	}
	if code := postJSON(t, s, "/api/people/Alice/thumbnail", map[string]string{"photo": "ghost.jpg"}).Code; code != http.StatusNotFound {
		t.Errorf("missing file select: got %d, want 404", code)
	}
	if code := postJSON(t, s, "/api/people/Nobody/thumbnail", map[string]string{"photo": "x.jpg"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown person select: got %d, want 404", code)
	}
	if code := postJSON(t, s, `/api/people/a\b/thumbnail`, map[string]string{"photo": "x.jpg"}).Code; code != http.StatusBadRequest {
		t.Errorf("bad name select: got %d, want 400", code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/people/Alice/thumbnail", nil)
	r4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r4, req)
	if r4.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET select: got %d, want 405", r4.Code)
	}
}

// TestThumbChooserNoFace checks that a photo without a detectable face is
// rejected and leaves the current thumbnail untouched.
func TestThumbChooserNoFace(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{faces: nil})
	img := pngBytes(t, 60, 60, 0)
	if err := database.AddPhoto("Bob", "b.png", img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	// The photo file must exist on disk; only detection fails.
	if err := os.MkdirAll(filepath.Join(s.cfg.PeopleDir, "Bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.PeopleDir, "Bob", "b.png"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	r := postJSON(t, s, "/api/people/Bob/thumbnail", map[string]string{"photo": "b.png"})
	if r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no-face select: got %d (%s), want 422", r.Code, r.Body.String())
	}
	if p := database.Get("Bob"); p.Thumb != "" {
		t.Errorf("failed selection must not create a thumbnail")
	}
}

// TestBodyTooLarge checks that request bodies over the size cap are rejected
// with 400 instead of being buffered (or spilling to temp files).
func TestBodyTooLarge(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{threshold: 0.45})

	// /api/config caps at 1 MiB.
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		bytes.NewReader(make([]byte, (1<<20)+1)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized config body: got %d, want 400", rec.Code)
	}

	// /api/recognize raw body caps at maxUpload.
	req = httptest.NewRequest(http.MethodPost, "/api/recognize",
		bytes.NewReader(make([]byte, maxUpload+1)))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized recognize body: got %d, want 400", rec.Code)
	}
}

// TestDeletePersonRemovesFolder checks that deleting a person also removes
// their dataset folder, so a rescan cannot re-enroll them.
func TestDeletePersonRemovesFolder(t *testing.T) {
	s, database := newTestServer(t, &stubEngine{})
	if err := database.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.cfg.PeopleDir, "Alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/people/Alice", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		FolderRemoved bool `json:"folder_removed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.FolderRemoved {
		t.Errorf("folder_removed = false, want true")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("people folder still exists after delete")
	}
	if database.Get("Alice") != nil {
		t.Errorf("Alice should be removed from the DB")
	}

	// A bad name is rejected before anything happens.
	req = httptest.NewRequest(http.MethodDelete, "/api/people/.hidden", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: got %d, want 400", rec.Code)
	}
}
