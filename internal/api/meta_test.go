package api

// Tests for the person-details surface: the public GET /api/people/{name}
// document, the admin POST /api/people/{name}/meta full-replace endpoint and
// its validation rules, and the admin GET /api/people/{name}/photos list the
// photos manager consumes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// detailDoc is the public details document shape the UI consumes.
type detailDoc struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Photos  int      `json:"photos"`
	Aliases []string `json:"aliases"`
	Birth   *struct {
		Year  *int `json:"year"`
		Month *int `json:"month"`
		Day   *int `json:"day"`
	} `json:"birthdate"`
	URLs        []string `json:"urls"`
	Description string   `json:"description"`
}

func TestPersonDetailPublic(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng) // open mode: anonymous requests hit the handler
	img := []byte("fake-image")
	if err := database.AddPhoto("Alice", "a.png", img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	y, mo, d := 1965, 3, 2
	if _, err := database.SetMeta("Alice", &db.PersonMeta{
		Aliases:     []string{"A. Smith"},
		Birth:       &db.BirthDate{Year: &y, Month: &mo, Day: &d},
		URLs:        []string{"https://example.org/alice"},
		Description: "born **Alice**",
	}); err != nil {
		t.Fatal(err)
	}

	var doc detailDoc
	if rec := getJSON(t, s, "/api/people/Alice", &doc); rec.Code != http.StatusOK {
		t.Fatalf("anonymous person detail: got %d (%s)", rec.Code, rec.Body.String())
	}
	if doc.Name != "Alice" || doc.ID != "alice" || doc.Photos != 1 {
		t.Errorf("unexpected identity block: %+v", doc)
	}
	if len(doc.Aliases) != 1 || doc.Aliases[0] != "A. Smith" {
		t.Errorf("aliases missing: %+v", doc.Aliases)
	}
	if doc.Birth == nil || doc.Birth.Year == nil || *doc.Birth.Year != 1965 {
		t.Errorf("birthdate missing: %+v", doc.Birth)
	}
	if len(doc.URLs) != 1 || doc.URLs[0] != "https://example.org/alice" {
		t.Errorf("urls missing: %+v", doc.URLs)
	}
	if doc.Description != "born **Alice**" {
		t.Errorf("description missing: %q", doc.Description)
	}

	// The public document must not carry enrolled photo paths/hashes (the
	// photos manager reads the admin /photos endpoint instead).
	var raw map[string]any
	if rec := getJSON(t, s, "/api/people/Alice", &raw); rec.Code != http.StatusOK {
		t.Fatalf("person detail: got %d", rec.Code)
	}
	if _, ok := raw["thumb_src"]; ok {
		t.Errorf("public detail leaks thumb_src: %v", raw)
	}
	if _, ok := raw["photos"].([]any); ok {
		t.Errorf("public detail leaks the photo list: %v", raw["photos"])
	}

	// Unknown person and invalid name.
	if rec := getJSON(t, s, "/api/people/Nobody", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person: got %d", rec.Code)
	}
	if rec := getJSON(t, s, "/api/people/%2e%2e", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid name: got %d", rec.Code)
	}
}

func TestSetMetaRoundTrip(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	if err := database.AddPhoto("Alice", "a.png", []byte("img"), []float32{1}); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"aliases":     []string{"A. Smith", "smith", "SMITH", "  A. Smith  "},
		"birthdate":   map[string]any{"year": 1965, "month": 3, "day": 2},
		"urls":        []string{"https://example.org/a", "  ", "https://example.org/a"},
		"description": "## Notes\n\n- born Alice",
	}
	rec := postJSON(t, s, "/api/people/Alice/meta", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("set meta: got %d (%s)", rec.Code, rec.Body.String())
	}
	// The reply is the refreshed details document.
	var doc detailDoc
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	// Trimmed, deduped (case-insensitive, first spelling wins), no empties.
	if len(doc.Aliases) != 2 || doc.Aliases[0] != "A. Smith" || doc.Aliases[1] != "smith" {
		t.Errorf("aliases not normalized: %v", doc.Aliases)
	}
	if doc.Birth == nil || doc.Birth.Month == nil || *doc.Birth.Month != 3 {
		t.Errorf("birthdate not stored: %+v", doc.Birth)
	}
	if len(doc.URLs) != 1 { // the blank and duplicate entries are gone
		t.Errorf("urls not normalized: %v", doc.URLs)
	}

	// Persisted: visible through the public detail and the list summary.
	var again detailDoc
	if rec := getJSON(t, s, "/api/people/Alice", &again); rec.Code != http.StatusOK || len(again.Aliases) != 2 {
		t.Fatalf("meta not persisted: %d %+v", rec.Code, again.Aliases)
	}
	var list struct {
		People []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"people"`
	}
	if rec := getJSON(t, s, "/api/people", &list); rec.Code != http.StatusOK {
		t.Fatalf("people list: got %d", rec.Code)
	}
	if len(list.People) != 1 || len(list.People[0].Aliases) != 2 {
		t.Errorf("aliases missing from the list summary: %+v", list.People)
	}

	// Full-replace: an empty body clears everything.
	if rec := postJSON(t, s, "/api/people/Alice/meta", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("clear meta: got %d (%s)", rec.Code, rec.Body.String())
	}
	var cleared map[string]any
	if rec := getJSON(t, s, "/api/people/Alice", &cleared); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	for _, key := range []string{"aliases", "birthdate", "urls", "description"} {
		if _, ok := cleared[key]; ok {
			t.Errorf("cleared meta still carries %q: %v", key, cleared[key])
		}
	}
	if p := database.Get("Alice"); p.Meta != nil {
		t.Errorf("cleared meta still stored: %+v", p.Meta)
	}
}

func TestSetMetaValidation(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	if err := database.AddPhoto("Alice", "a.png", []byte("img"), []float32{1}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"month too high", map[string]any{"birthdate": map[string]any{"month": 13}}},
		{"month too low", map[string]any{"birthdate": map[string]any{"month": 0}}},
		{"year too high", map[string]any{"birthdate": map[string]any{"year": 2101}}},
		{"bare day", map[string]any{"birthdate": map[string]any{"day": 5}}},
		{"day too high without month", map[string]any{"birthdate": map[string]any{"year": 1965, "day": 32}}},
		{"day too high", map[string]any{"birthdate": map[string]any{"month": 3, "day": 32}}},
		{"feb 30", map[string]any{"birthdate": map[string]any{"year": 2000, "month": 2, "day": 30}}},
		{"javascript URL", map[string]any{"urls": []string{"javascript:alert(1)"}}},
		{"relative URL", map[string]any{"urls": []string{"/etc/passwd"}}},
		{"hostless URL", map[string]any{"urls": []string{"mailto:x@y.z"}}},
		{"too many aliases", map[string]any{"aliases": manyStrings(21, "a%d")}},
		{"alias too long", map[string]any{"aliases": []string{strings.Repeat("x", 121)}}},
		{"too many urls", map[string]any{"urls": manyStrings(51, "https://h%d.example")}},
		{"description too long", map[string]any{"description": strings.Repeat("x", 20_001)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, s, "/api/people/Alice/meta", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(rec.Body).Decode(&e)
			if e.Error == "" {
				t.Errorf("missing error message")
			}
		})
	}

	// Lenient partial dates must be accepted: year-only, month-only,
	// month+day without a year (Feb 29 with unknown year), year+day without
	// a month (the YYYY--DD template), and a full leap-day date.
	for _, birth := range []map[string]any{
		{"year": 1965},
		{"month": 2},
		{"month": 2, "day": 29},
		{"year": 2000, "month": 2, "day": 29},
		{"year": 1965, "day": 2},
	} {
		rec := postJSON(t, s, "/api/people/Alice/meta", map[string]any{"birthdate": birth})
		if rec.Code != http.StatusOK {
			t.Fatalf("partial birthdate %v: got %d (%s)", birth, rec.Code, rec.Body.String())
		}
	}

	// Unknown person and wrong method.
	if rec := postJSON(t, s, "/api/people/Nobody/meta", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person: got %d", rec.Code)
	}
	if rec := req2(t, s, http.MethodGet, "/api/people/Alice/meta"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET meta: got %d", rec.Code)
	}
}

func manyStrings(n int, format string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(format, i)
	}
	return out
}

// TestPersonPhotosEndpoint covers the admin photos list the photos manager
// loads: open mode answers it anonymously, secured mode requires the session.
func TestPersonPhotosEndpoint(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	img := []byte("fake-image")
	hash := db.HashBytes(img)
	if err := database.AddPhoto("Alice", "a.png", img, []float32{1}); err != nil {
		t.Fatal(err)
	}

	var list struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ThumbSrc string `json:"thumb_src"`
		Photos   []struct {
			Path string `json:"path"`
			Hash string `json:"hash"`
		} `json:"photos"`
	}
	if rec := getJSON(t, s, "/api/people/Alice/photos", &list); rec.Code != http.StatusOK {
		t.Fatalf("photos list: got %d (%s)", rec.Code, rec.Body.String())
	}
	if list.Name != "Alice" || len(list.Photos) != 1 || list.Photos[0].Path != "a.png" || list.Photos[0].Hash != hash {
		t.Errorf("unexpected photos list: %+v", list)
	}
	// No embeddings on the wire.
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "embedding") {
		t.Errorf("photos list leaks embeddings: %s", b)
	}

	if rec := postJSON(t, s, "/api/people/Alice/photos", map[string]any{}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST photos: got %d", rec.Code)
	}
	if rec := getJSON(t, s, "/api/people/Nobody/photos", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person photos: got %d", rec.Code)
	}
}

// TestSecuredModeMetaAccess checks the secured-server side of the new
// surface: the details GET stays public, while the photos list and the meta
// write need the admin session.
func TestSecuredModeMetaAccess(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newAuthTestServer(t, eng)
	if err := database.AddPhoto("Alice", "a.png", []byte("img"), []float32{1}); err != nil {
		t.Fatal(err)
	}

	// Anonymous: public detail OK, admin surfaces refused.
	if rec := getJSON(t, s, "/api/people/Alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("public detail behind auth: got %d", rec.Code)
	}
	if rec := getJSON(t, s, "/api/people/Alice/photos", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous photos list: got %d", rec.Code)
	}
	if rec := postJSON(t, s, "/api/people/Alice/meta", map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous meta write: got %d", rec.Code)
	}

	// With the admin session both admin routes answer.
	cookie := loginAdmin(t, s)
	post := func(path string) *int {
		req := authReq(t, s, http.MethodPost, path, "1.2.3.4",
			strings.NewReader(`{"aliases":["A. Smith"]}`), map[string]string{"Content-Type": "application/json"})
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin %s: got %d (%s)", path, rec.Code, rec.Body.String())
		}
		return nil
	}
	post("/api/people/Alice/meta")
	req := authReq(t, s, http.MethodGet, "/api/people/Alice/photos", "1.2.3.4", nil, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin photos list: got %d", rec.Code)
	}
	if p := database.Get("Alice"); p.Meta == nil || len(p.Meta.Aliases) != 1 {
		t.Errorf("admin meta write not persisted: %+v", p.Meta)
	}
}
