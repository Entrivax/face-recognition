package api

// Tests for POST /api/compare: the face-to-face similarity endpoint. The
// stub here distinguishes the two uploaded payloads by their bytes ("A" vs
// anything else), so one stub serves both photos with different
// faces/embeddings. The endpoint is mounted under RequireAuth (any
// authenticated principal; share tokens excluded), so the probes carry a
// session.

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recogn/internal/engine"
)

// compareStub implements Engine with per-photo behaviour keyed on the raw
// payload, which is all the compare handler needs.
type compareStub struct {
	facesA, facesB []engine.Face
	embA, embB     []float32
	detectErr      error
	threshold      float64
}

func (s *compareStub) RecognizeScoped(_ []byte, _ map[string]struct{}) ([]engine.Face, error) {
	return nil, nil
}
func (s *compareStub) Recognize([]byte) ([]engine.Face, error) { return nil, nil }
func (s *compareStub) Detect(b []byte) ([]engine.Face, error) {
	if s.detectErr != nil {
		return nil, s.detectErr
	}
	if string(b) == "A" {
		return s.facesA, nil
	}
	return s.facesB, nil
}
func (s *compareStub) EmbedFace(b []byte, _ engine.Face) ([]float32, error) {
	if string(b) == "A" {
		return s.embA, nil
	}
	return s.embB, nil
}
func (s *compareStub) SetThreshold(t float64) { s.threshold = t }
func (s *compareStub) Threshold() float64     { return s.threshold }
func (s *compareStub) Ping() error            { return nil }

// compareBody builds a multipart body with one file under each compare field.
func compareBody(t *testing.T, dataA, dataB []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image1", "a.jpg")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	fw.Write(dataA)
	fw, err = w.CreateFormFile("image2", "b.jpg")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	fw.Write(dataB)
	w.Close()
	return &buf, w.FormDataContentType()
}

// postCompare POSTs a compare request built from the two payloads, as the
// given actor (cookie; hdr adds e.g. an X-Share-Token probe).
func postCompare(t *testing.T, s *Server, cookie *http.Cookie, dataA, dataB []byte, hdr ...map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := compareBody(t, dataA, dataB)
	var h map[string]string
	if len(hdr) > 0 {
		h = hdr[0]
	}
	return do(t, s, http.MethodPost, "/api/compare", body, ct, cookie, h)
}

type compareFaceJSON struct {
	Index int        `json:"index"`
	BBox  [4]float64 `json:"bbox"`
	Score float64    `json:"score"`
	Used  bool       `json:"used"`
}

type compareImageJSON struct {
	Count int               `json:"count"`
	Faces []compareFaceJSON `json:"faces"`
}

type compareResp struct {
	Similarity float64          `json:"similarity"`
	Threshold  float64          `json:"threshold"`
	Image1     compareImageJSON `json:"image1"`
	Image2     compareImageJSON `json:"image2"`
}

// checkUsedFace asserts that exactly one face is flagged used and that it is
// the expected 1-based detection index.
func checkUsedFace(t *testing.T, name string, img compareImageJSON, wantIndex int) {
	t.Helper()
	used := 0
	for _, f := range img.Faces {
		if !f.Used {
			continue
		}
		used++
		if f.Index != wantIndex {
			t.Errorf("%s: used face index = %d, want %d", name, f.Index, wantIndex)
		}
	}
	if used != 1 {
		t.Errorf("%s: %d faces flagged used, want 1", name, used)
	}
}

func TestCompareSimilarity(t *testing.T) {
	cases := []struct {
		name string
		embA []float32
		embB []float32
	}{
		{"identical embeddings", []float32{1, 0, 0}, []float32{1, 0, 0}},
		{"orthogonal embeddings", []float32{1, 0, 0}, []float32{0, 1, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &compareStub{
				facesA: []engine.Face{
					{BBox: [4]float64{10, 10, 50, 60}, Score: 0.98}, // largest
					{BBox: [4]float64{100, 100, 30, 40}, Score: 0.9},
				},
				facesB:    []engine.Face{{BBox: [4]float64{5, 5, 20, 20}, Score: 0.95}},
				embA:      tc.embA,
				embB:      tc.embB,
				threshold: 0.45,
			}
			e := newEnv(t, eng, "", false, false)
			_ = createAdmin(t, e.s)
			rec := postCompare(t, e.s, e.admin, []byte("A"), []byte("B"))
			if rec.Code != http.StatusOK {
				t.Fatalf("compare: got %d (%s)", rec.Code, rec.Body.String())
			}
			var resp compareResp
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if want := engine.Cosine(tc.embA, tc.embB); resp.Similarity != want {
				t.Errorf("similarity = %v, want %v", resp.Similarity, want)
			}
			if resp.Threshold != 0.45 {
				t.Errorf("threshold = %v, want 0.45", resp.Threshold)
			}
			if resp.Image1.Count != 2 || len(resp.Image1.Faces) != 2 {
				t.Errorf("image1 = %+v", resp.Image1)
			}
			if resp.Image2.Count != 1 || len(resp.Image2.Faces) != 1 {
				t.Errorf("image2 = %+v", resp.Image2)
			}
			// Exactly the largest face of each photo is flagged as used.
			checkUsedFace(t, "image1", resp.Image1, 1)
			checkUsedFace(t, "image2", resp.Image2, 1)
			// Embeddings must never reach the client.
			if strings.Contains(rec.Body.String(), "embedding") {
				t.Errorf("response must not leak embeddings")
			}
		})
	}
}

func TestCompareMissingImage(t *testing.T) {
	e := newEnv(t, &compareStub{}, "", false, false)
	_ = createAdmin(t, e.s)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image1", "a.jpg")
	fw.Write([]byte("A"))
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/compare", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(e.admin)
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing image2: got %d, want 400", rec.Code)
	}
	var errBody map[string]string
	json.NewDecoder(rec.Body).Decode(&errBody)
	if !strings.Contains(errBody["error"], "second photo") {
		t.Errorf("error = %q, want it to name the second photo", errBody["error"])
	}
}

func TestCompareNoFace(t *testing.T) {
	// First photo without faces.
	e := newEnv(t, &compareStub{facesA: nil, facesB: []engine.Face{{BBox: [4]float64{1, 1, 2, 2}}}}, "", false, false)
	_ = createAdmin(t, e.s)
	if rec := postCompare(t, e.s, e.admin, []byte("A"), []byte("B")); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no face in first photo: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	// Second photo without faces.
	e2 := newEnv(t, &compareStub{facesA: []engine.Face{{BBox: [4]float64{1, 1, 2, 2}}}, facesB: nil}, "", false, false)
	_ = createAdmin(t, e2.s)
	rec := postCompare(t, e2.s, e2.admin, []byte("A"), []byte("B"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no face in second photo: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	json.NewDecoder(rec.Body).Decode(&errBody)
	if !strings.Contains(errBody["error"], "second photo") {
		t.Errorf("error = %q, want it to name the second photo", errBody["error"])
	}
}

func TestCompareDetectError(t *testing.T) {
	e := newEnv(t, &compareStub{detectErr: errors.New("decode source image: broken")}, "", false, false)
	_ = createAdmin(t, e.s)
	if rec := postCompare(t, e.s, e.admin, []byte("A"), []byte("B")); rec.Code != http.StatusBadGateway {
		t.Errorf("detect failure: got %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCompareMethodNotAllowed(t *testing.T) {
	e := newEnv(t, &compareStub{}, "", false, false)
	_ = createAdmin(t, e.s)
	req := httptest.NewRequest(http.MethodGet, "/api/compare", nil)
	req.AddCookie(e.admin)
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/compare: got %d, want 405", rec.Code)
	}
}

// TestCompareRequiresAuth pins the relaxed mount: /api/compare is no longer
// admin-only — any authenticated principal may use it (readers included);
// share tokens do not (RequireAuth, not the scoped guard).
func TestCompareRequiresAuth(t *testing.T) {
	eng := &compareStub{
		facesA:    []engine.Face{{BBox: [4]float64{10, 10, 50, 60}}},
		facesB:    []engine.Face{{BBox: [4]float64{5, 5, 20, 20}}},
		embA:      []float32{1, 0},
		embB:      []float32{1, 0},
		threshold: 0.45,
	}
	e := newEnv(t, eng, "Team", true, true)

	// Anonymous: gated by RequireAuth.
	if rec := postCompare(t, e.s, nil, []byte("A"), []byte("B")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous compare: got %d, want 401", rec.Code)
	}
	// A share token does not authorize compare either.
	if rec := postCompare(t, e.s, nil, []byte("A"), []byte("B"), shareHdr(e.repoID, e.shareToken)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("share-token compare: got %d, want 401", rec.Code)
	}

	// With a plain reader session: allowed.
	rec := postCompare(t, e.s, e.reader, []byte("A"), []byte("B"))
	if rec.Code != http.StatusOK {
		t.Fatalf("reader compare: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp compareResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Similarity != 1 {
		t.Errorf("similarity = %v, want 1", resp.Similarity)
	}
}
