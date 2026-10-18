package api

// Tests for the person-details surface: the reader-visible
// GET /api/people/{name}?repo= document, the writer-only
// POST /api/people/{name}/meta full-replace endpoint and its validation
// rules, and the writer-only photos list the photos manager consumes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// detailDoc is the public details document shape the UI consumes.
type detailDoc struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Photos   int      `json:"photos"`
	Repo     string   `json:"repo"`
	RepoName string   `json:"repo_name"`
	Aliases  []string `json:"aliases"`
	Birth    *struct {
		Year  *int `json:"year"`
		Month *int `json:"month"`
		Day   *int `json:"day"`
	} `json:"birthdate"`
	URLs        []string `json:"urls"`
	Description string   `json:"description"`
}

func TestPersonDetail(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	img := []byte("fake-image")
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "a.png", db.HashBytes(img), []float32{1}); err != nil {
		t.Fatal(err)
	}
	y, mo, d := 1965, 3, 2
	if _, err := e.db.SetMetaInRepo(e.repoID, "Alice", &db.PersonMeta{
		Aliases:     []string{"A. Smith"},
		Birth:       &db.BirthDate{Year: &y, Month: &mo, Day: &d},
		URLs:        []string{"https://example.org/alice"},
		Description: "born **Alice**",
	}); err != nil {
		t.Fatal(err)
	}

	var doc detailDoc
	if rec := getJSON(t, e.s, e.reader, "/api/people/Alice?repo="+e.repoID, &doc); rec.Code != http.StatusOK {
		t.Fatalf("person detail: got %d (%s)", rec.Code, rec.Body.String())
	}
	if doc.Name != "Alice" || doc.Photos != 1 || doc.Repo != e.repoID || doc.RepoName != "Team" {
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

	// The details document must not carry enrolled photo paths/hashes (the
	// photos manager reads the writer-only /photos endpoint instead).
	var raw map[string]any
	if rec := getJSON(t, e.s, e.reader, "/api/people/Alice?repo="+e.repoID, &raw); rec.Code != http.StatusOK {
		t.Fatalf("person detail: got %d", rec.Code)
	}
	if _, ok := raw["thumb_src"]; ok {
		t.Errorf("detail leaks thumb_src: %v", raw)
	}
	if _, ok := raw["photos"].([]any); ok {
		t.Errorf("detail leaks the photo list: %v", raw["photos"])
	}

	// The same person name in another repo resolves to THAT repo's person.
	other := seedRepo(t, e.s, e.admin, "Other")
	if err := e.db.AddPhotoHashedForRepo(other, "Alice", "b.png", db.HashBytes(img), []float32{1}); err != nil {
		t.Fatal(err)
	}
	var otherDoc detailDoc
	if rec := getJSON(t, e.s, e.admin, "/api/people/Alice?repo="+other, &otherDoc); rec.Code != http.StatusOK {
		t.Fatalf("other repo detail: got %d (%s)", rec.Code, rec.Body.String())
	}
	if otherDoc.ID == doc.ID || otherDoc.RepoName != "Other" {
		t.Errorf("same name in two repos must stay two people: %+v vs %+v", otherDoc, doc)
	}

	// Unknown person / unknown repo / invalid name / missing repo param.
	if rec := get(t, e.s, e.reader, "/api/people/Nobody?repo="+e.repoID); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person: got %d", rec.Code)
	}
	if rec := get(t, e.s, e.reader, "/api/people/Alice?repo=nosuch"); rec.Code != http.StatusForbidden {
		t.Errorf("unknown repo: got %d, want 403", rec.Code)
	}
	if rec := get(t, e.s, e.reader, "/api/people/%2e%2e?repo="+e.repoID); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid name: got %d", rec.Code)
	}
	if rec := get(t, e.s, e.reader, "/api/people/Alice"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing repo param: got %d, want 400", rec.Code)
	}
}

func TestSetMetaRoundTrip(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "a.png", db.HashBytes([]byte("img")), []float32{1}); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"aliases":     []string{"A. Smith", "smith", "SMITH", "  A. Smith  "},
		"birthdate":   map[string]any{"year": 1965, "month": 3, "day": 2},
		"urls":        []string{"https://example.org/a", "  ", "https://example.org/a"},
		"description": "## Notes\n\n- born Alice",
	}
	rec := sendJSON(t, e.s, e.writer, "/api/people/Alice/meta?repo="+e.repoID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("set meta: got %d (%s)", rec.Code, rec.Body.String())
	}
	// The reply is the refreshed details document.
	var doc detailDoc
	decodeBody(t, rec, &doc)
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

	// Persisted: visible through the detail view and the list summary.
	var again detailDoc
	if rec := getJSON(t, e.s, e.writer, "/api/people/Alice?repo="+e.repoID, &again); rec.Code != http.StatusOK || len(again.Aliases) != 2 {
		t.Fatalf("meta not persisted: %d %+v", rec.Code, again.Aliases)
	}
	var list struct {
		People []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"people"`
	}
	if rec := getJSON(t, e.s, e.writer, "/api/people?repo="+e.repoID, &list); rec.Code != http.StatusOK {
		t.Fatalf("people list: got %d", rec.Code)
	}
	if len(list.People) != 1 || len(list.People[0].Aliases) != 2 {
		t.Errorf("aliases missing from the list summary: %+v", list.People)
	}

	// Full-replace: an empty body clears everything.
	if rec := sendJSON(t, e.s, e.writer, "/api/people/Alice/meta?repo="+e.repoID, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("clear meta: got %d (%s)", rec.Code, rec.Body.String())
	}
	var cleared map[string]any
	if rec := getJSON(t, e.s, e.writer, "/api/people/Alice?repo="+e.repoID, &cleared); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	for _, key := range []string{"aliases", "birthdate", "urls", "description"} {
		if _, ok := cleared[key]; ok {
			t.Errorf("cleared meta still carries %q: %v", key, cleared[key])
		}
	}
	if p := e.db.GetInRepo(e.repoID, "Alice"); p.Meta != nil {
		t.Errorf("cleared meta still stored: %+v", p.Meta)
	}
}

func TestSetMetaValidation(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "a.png", db.HashBytes([]byte("img")), []float32{1}); err != nil {
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
			rec := sendJSON(t, e.s, e.writer, "/api/people/Alice/meta?repo="+e.repoID, tc.body)
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
		rec := sendJSON(t, e.s, e.writer, "/api/people/Alice/meta?repo="+e.repoID, map[string]any{"birthdate": birth})
		if rec.Code != http.StatusOK {
			t.Fatalf("partial birthdate %v: got %d (%s)", birth, rec.Code, rec.Body.String())
		}
	}

	// Unknown person and wrong method.
	if rec := sendJSON(t, e.s, e.writer, "/api/people/Nobody/meta?repo="+e.repoID, map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person: got %d", rec.Code)
	}
	if rec := do(t, e.s, http.MethodGet, "/api/people/Alice/meta?repo="+e.repoID, nil, "", e.writer, nil); rec.Code != http.StatusMethodNotAllowed {
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

// TestPersonPhotosEndpoint covers the photos-manager list (writer-only):
// paths + content hashes, no embeddings, the thumbnail source named.
func TestPersonPhotosEndpoint(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	img := []byte("fake-image")
	hash := db.HashBytes(img)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "a.png", hash, []float32{1}); err != nil {
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
	if rec := getJSON(t, e.s, e.writer, "/api/people/Alice/photos?repo="+e.repoID, &list); rec.Code != http.StatusOK {
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

	if rec := sendJSON(t, e.s, e.writer, "/api/people/Alice/photos?repo="+e.repoID, map[string]any{}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST photos: got %d", rec.Code)
	}
	if rec := get(t, e.s, e.writer, "/api/people/Nobody/photos?repo="+e.repoID); rec.Code != http.StatusNotFound {
		t.Errorf("unknown person photos: got %d", rec.Code)
	}
	// Reader (and share) → 403: the photos list is writer tooling.
	if rec := get(t, e.s, e.reader, "/api/people/Alice/photos?repo="+e.repoID); rec.Code != http.StatusForbidden {
		t.Errorf("reader photos list: got %d, want 403", rec.Code)
	}
}
