package api

// Tests for the multi-user API surface: the shared test fixtures (hermetic
// server, admin/reader/writer sessions, share tokens) and the core flows —
// repositories, people, recognize, enroll, thumbs, the index/share route.
// The access matrix lives in access_test.go, the session/user-management
// surface in auth_test.go, admission control in admission_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
)

// stubEngine implements the Engine interface without any real inference.
// RecognizeScoped records the allowed set so tests can assert what the
// handlers scoped the matching to.
type stubEngine struct {
	faces        []engine.Face
	threshold    float64
	recognizeErr error // when set, Recognize returns it (tests the error mapping)

	mu      sync.Mutex
	allowed map[string]struct{}
}

func (s *stubEngine) RecognizeScoped(b []byte, allowed map[string]struct{}) ([]engine.Face, error) {
	s.mu.Lock()
	s.allowed = allowed
	s.mu.Unlock()
	return s.faces, s.recognizeErr
}

func (s *stubEngine) Recognize(b []byte) ([]engine.Face, error) { return s.RecognizeScoped(b, nil) }
func (s *stubEngine) Detect(b []byte) ([]engine.Face, error)    { return s.faces, nil }
func (s *stubEngine) EmbedFace(b []byte, f engine.Face) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (s *stubEngine) SetThreshold(t float64) { s.threshold = t }
func (s *stubEngine) Threshold() float64     { return s.threshold }
func (s *stubEngine) Ping() error            { return nil }

// recordedAllowed returns the allowed set of the last RecognizeScoped call.
func (s *stubEngine) recordedAllowed() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed
}

// ---- fixtures ----

// newTestServer builds a Server over hermetic temp dirs (faces.db, users.db,
// people/ all fresh) with NO users: the caller opts into accounts via
// createAdmin / seedUser, which also keeps the first-run setup flow
// testable.
func newTestServer(t *testing.T, eng Engine) (*Server, *db.DB) {
	t.Helper()
	t.Setenv("RECOGN_ADMIN_PASSWORD_HASH", "")
	t.Setenv("RECOGN_TRUSTED_PROXY_CIDR", "")
	t.Setenv("RECOGN_CONCURRENCY", "")
	database, err := db.Open(filepath.Join(t.TempDir(), "faces.db"))
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
	t.Cleanup(func() { _ = s.auth.Close() })
	return s, database
}

// adminName/adminPass are the credentials of the admin account the tests
// create; the login flow mints every session (no direct session-store
// pokes — that path is covered by internal/auth).
const (
	adminName = "admin"
	adminPass = "correct-horse-battery"
)

// createAdmin ensures an admin account exists (idempotent: newEnv already
// provisions one) and logs in through the real /api/login route, returning
// the session cookie.
func createAdmin(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	exists := false
	for _, u := range s.auth.Users() {
		if u.Username == adminName {
			exists = true
			break
		}
	}
	if !exists {
		if err := s.auth.CreateUser(adminName, adminPass, true); err != nil {
			t.Fatalf("create admin: %v", err)
		}
	}
	return loginAs(t, s, adminName, adminPass)
}

// loginAs POSTs the credentials to /api/login and returns the session
// cookie.
func loginAs(t *testing.T, s *Server, username, password string) *http.Cookie {
	t.Helper()
	b, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, http.MethodPost, "/api/login", bytes.NewReader(b), "application/json", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: got %d (%s)", username, rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec)
}

// sessionCookie extracts the recogn_session cookie from a response.
func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "recogn_session" {
			return c
		}
	}
	t.Fatalf("no recogn_session cookie in response: %v", rec.Result().Cookies())
	return nil
}

// do sends one request through the root handler (cookie optional; hdr adds
// extra headers such as X-Share-Token).
func do(t *testing.T, s *Server, method, target string, body io.Reader, ct string, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// get sends a GET as the given actor (nil cookie = anonymous).
func get(t *testing.T, s *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	return do(t, s, http.MethodGet, path, nil, "", cookie, nil)
}

// getJSON sends a GET as the given actor and decodes the JSON body into out
// (out may be nil to skip decoding).
func getJSON(t *testing.T, s *Server, cookie *http.Cookie, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := get(t, s, cookie, path)
	if out != nil {
		decodeBody(t, rec, out)
	}
	return rec
}

// sendJSON POSTs v as a JSON body as the given actor.
func sendJSON(t *testing.T, s *Server, cookie *http.Cookie, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return do(t, s, http.MethodPost, path, bytes.NewReader(b), "application/json", cookie, nil)
}

// delete sends a DELETE as the given actor.
func del(t *testing.T, s *Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	return do(t, s, http.MethodDelete, path, nil, "", cookie, nil)
}

// decodeBody decodes a JSON response into out.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// seedRepo creates a repository through the real admin route and returns
// its ID.
func seedRepo(t *testing.T, s *Server, admin *http.Cookie, name string) string {
	t.Helper()
	rec := sendJSON(t, s, admin, "/api/repos", map[string]string{"name": name})
	if rec.Code != http.StatusOK {
		t.Fatalf("create repo %q: got %d (%s)", name, rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &resp)
	if resp.ID == "" {
		t.Fatalf("repo creation returned no id: %s", rec.Body.String())
	}
	return resp.ID
}

// seedRoleUser creates a user through the admin routes, grants them role on
// the given repository and logs them in, returning their session cookie.
func seedRoleUser(t *testing.T, s *Server, admin *http.Cookie, name, password, repoID, role string) *http.Cookie {
	t.Helper()
	rec := sendJSON(t, s, admin, "/api/users", map[string]any{"username": name, "password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("create user %s: got %d (%s)", name, rec.Code, rec.Body.String())
	}
	rec = sendJSON(t, s, admin, "/api/users/"+name+"/repos", map[string]any{
		"grants": []map[string]string{{"repo": repoID, "role": role}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("grant %s: got %d (%s)", name, rec.Code, rec.Body.String())
	}
	return loginAs(t, s, name, password)
}

// enableShare toggles the repo's share link on through the admin route and
// extracts the plaintext token from the returned share_url.
func enableShare(t *testing.T, s *Server, admin *http.Cookie, repoID string) string {
	t.Helper()
	rec := sendJSON(t, s, admin, "/api/repos/"+repoID+"/share", map[string]bool{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable share: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ShareURL string `json:"share_url"`
	}
	decodeBody(t, rec, &resp)
	if resp.ShareURL == "" {
		t.Fatalf("share_url missing: %s", rec.Body.String())
	}
	i := strings.LastIndex(resp.ShareURL, "/")
	if i < 0 {
		t.Fatalf("share_url shape: %q", resp.ShareURL)
	}
	return resp.ShareURL[i+1:]
}

// env is a fully wired test fixture: hermetic server, an admin session, one
// seeded repository and (opt-in) reader/writer users plus a share token.
type env struct {
	t          *testing.T
	s          *Server
	db         *db.DB
	eng        *stubEngine
	admin      *http.Cookie
	reader     *http.Cookie // nil without users
	writer     *http.Cookie // nil without users
	repoID     string       // "" without a repo name
	shareToken string       // "" without share
}

// newEnv builds the fixture: a hermetic server with an admin session; a
// non-empty repoName seeds one repository; users seeds reader/writer
// accounts on it; share enables its share link.
func newEnv(t *testing.T, eng Engine, repoName string, users, share bool) *env {
	t.Helper()
	e := &env{t: t}
	se, ok := eng.(*stubEngine)
	if ok {
		e.eng = se
	}
	e.s, e.db = newTestServer(t, eng)
	e.admin = createAdmin(t, e.s)
	if repoName != "" {
		e.repoID = seedRepo(t, e.s, e.admin, repoName)
	}
	if users {
		e.writer = seedRoleUser(t, e.s, e.admin, "walter", "writer-password-1", e.repoID, "writer")
		e.reader = seedRoleUser(t, e.s, e.admin, "rita", "reader-password-1", e.repoID, "reader")
	}
	if share {
		e.shareToken = enableShare(t, e.s, e.admin, e.repoID)
	}
	return e
}

// shareHdr builds the X-Share-Token header map for a token.
func shareHdr(repoID, token string) map[string]string {
	return map[string]string{"X-Share-Token": repoID + "/" + token}
}

// multipartBody builds a multipart body with one file under field.
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

// enrollPerson POSTs files to /api/people/{name}/enroll?repo=<repo> as the
// given actor.
func enrollPerson(t *testing.T, s *Server, cookie *http.Cookie, repoID, name string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartFiles(t, files)
	return do(t, s, http.MethodPost, "/api/people/"+name+"/enroll?repo="+repoID, body, ct, cookie, nil)
}

// enrollFaceRequest POSTs a single image plus a face_index field to
// /api/people/{name}/enroll-face?repo=<repo>.
func enrollFaceRequest(t *testing.T, s *Server, cookie *http.Cookie, repoID, name string, img []byte, idx string) *httptest.ResponseRecorder {
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
	return do(t, s, http.MethodPost, "/api/people/"+name+"/enroll-face?repo="+repoID, &buf, w.FormDataContentType(), cookie, nil)
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

func slicesContain(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---- tests ----

// TestHealthMinimized pins the health body: a plain status, no server
// details (people counts, thresholds, auth modes).
func TestHealthMinimized(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{threshold: 0.45})
	rec := get(t, s, nil, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("health: got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if body["status"] != "ok" {
		t.Errorf("unexpected health body: %v", body)
	}
	if _, leaked := body["people"]; leaked {
		t.Errorf("health must not leak people count")
	}
	if _, leaked := body["auth"]; leaked {
		t.Errorf("health must not leak auth flags")
	}
}

// TestShareIndexServesSPA pins the /share/<repoID>/<token> route: the SPA
// shell (no-cache), including the trailing-slash variant; malformed share
// paths fall through to the plain 404.
func TestShareIndexServesSPA(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{})
	fakeID := strings.Repeat("a", 8) + "-" + strings.Repeat("b", 4) + "-" + strings.Repeat("c", 4) + "-" + strings.Repeat("d", 4) + "-" + strings.Repeat("e", 12)
	paths := []string{
		"/share/" + fakeID + "/" + "tok",
		"/share/" + fakeID + "/a-b_c-token/",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d (%s), want 200", p, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", p, got)
		}
		if !strings.Contains(rec.Body.String(), "recogn") {
			t.Errorf("GET %s: body does not look like the SPA: %.80s", p, rec.Body.String())
		}
	}
	// Wrong shapes: short id, empty token, extra segment.
	for _, p := range []string{"/share/short/tok", "/share/" + fakeID + "/", "/share/" + fakeID + "/tok/extra"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", p, rec.Code)
		}
	}
}

// shareResp is the GET/POST /api/repos/{id}/share reply shape.
type shareResp struct {
	ID       string  `json:"id"`
	Shared   bool    `json:"shared"`
	ShareURL *string `json:"share_url"`
}

// TestShareReveal pins GET /api/repos/{id}/share: the admin can re-read the
// current share link (the very token the enable minted — reveal never
// rotates), everyone else is refused, and unshared/disabled repositories
// answer share_url null.
func TestShareReveal(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, true)

	// Admin: the reveal carries the token the enable minted.
	rec := get(t, e.s, e.admin, "/api/repos/"+e.repoID+"/share")
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal: got %d (%s)", rec.Code, rec.Body.String())
	}
	var got shareResp
	decodeBody(t, rec, &got)
	if !got.Shared || got.ID != e.repoID || got.ShareURL == nil ||
		!strings.HasSuffix(*got.ShareURL, "/share/"+e.repoID+"/"+e.shareToken) {
		t.Fatalf("reveal: got %+v (want the minted token)", got)
	}
	if c := rec.Header().Get("Cache-Control"); c != "no-store" {
		t.Errorf("reveal Cache-Control = %q, want no-store", c)
	}

	// The revealed token still works: the same link can be handed out again.
	rec = do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", nil, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusOK {
		t.Errorf("share token after reveal: got %d (%s)", rec.Code, rec.Body.String())
	}

	// A reader session is refused (admin-only route).
	if rec = get(t, e.s, e.reader, "/api/repos/"+e.repoID+"/share"); rec.Code != http.StatusForbidden {
		t.Errorf("reader reveal: got %d, want 403", rec.Code)
	}
	// Anonymous: 401 from the RequireAdmin mount.
	if rec = get(t, e.s, nil, "/api/repos/"+e.repoID+"/share"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous reveal: got %d, want 401", rec.Code)
	}
	// Unknown repository: 404.
	if rec = get(t, e.s, e.admin, "/api/repos/no-such-repo/share"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown repo reveal: got %d, want 404", rec.Code)
	}

	// After disabling: shared false, share_url null.
	if rec = sendJSON(t, e.s, e.admin, "/api/repos/"+e.repoID+"/share", map[string]bool{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("disable share: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = get(t, e.s, e.admin, "/api/repos/"+e.repoID+"/share")
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal after disable: got %d (%s)", rec.Code, rec.Body.String())
	}
	got = shareResp{}
	decodeBody(t, rec, &got)
	if got.Shared || got.ShareURL != nil {
		t.Errorf("reveal after disable: got %+v, want shared false / share_url null", got)
	}
}

// TestRepoLifecycle covers the admin repository CRUD: create (record +
// dataset folder), duplicate name (409), empty name (400), rename (200 /
// 409 / 404) and delete (record + persons + grants + folder gone).
func TestRepoLifecycle(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", false, false)
	admin := e.admin
	_ = admin

	// The dataset folder exists right after creation.
	if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID)); err != nil {
		t.Fatalf("repo folder not created: %v", err)
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		People int    `json:"people"`
	}
	// Response shape (fresh create).
	rec := sendJSON(t, e.s, e.admin, "/api/repos", map[string]string{"name": "Second"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create second repo: got %d (%s)", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &created)
	if created.Name != "Second" || created.People != 0 || created.ID == "" {
		t.Errorf("unexpected create response: %+v", created)
	}

	// Duplicate name (case-insensitive) → 409.
	if rec := sendJSON(t, e.s, e.admin, "/api/repos", map[string]string{"name": "team"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name: got %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// Empty name → 400.
	if rec := sendJSON(t, e.s, e.admin, "/api/repos", map[string]string{"name": "   "}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", rec.Code)
	}
	// Broken JSON → 400.
	if rec := do(t, e.s, http.MethodPost, "/api/repos", strings.NewReader("{nope"), "application/json", e.admin, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: got %d, want 400", rec.Code)
	}

	// Rename: success, then the old name is gone.
	rec = sendJSON(t, e.s, e.admin, "/api/repos/"+e.repoID+"/rename", map[string]string{"name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", rec.Code, rec.Body.String())
	}
	var renamed struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decodeBody(t, rec, &renamed)
	if renamed.ID != e.repoID || renamed.Name != "Renamed" {
		t.Errorf("unexpected rename response: %+v", renamed)
	}
	if _, ok := e.db.GetRepoByName("Team"); ok {
		t.Errorf("old repo name still resolves")
	}
	// Rename conflicts: taken name and unknown repo.
	if rec := sendJSON(t, e.s, e.admin, "/api/repos/"+e.repoID+"/rename", map[string]string{"name": "Second"}); rec.Code != http.StatusConflict {
		t.Errorf("rename to taken name: got %d, want 409", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/repos/00000000-0000-4000-8000-000000000000/rename", map[string]string{"name": "X"}); rec.Code != http.StatusNotFound {
		t.Errorf("rename unknown repo: got %d, want 404", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/repos/"+e.repoID+"/rename", map[string]string{"name": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("rename to empty name: got %d, want 400", rec.Code)
	}
	if rec := get(t, e.s, e.admin, "/api/repos/"+e.repoID+"/rename"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET rename: got %d, want 405", rec.Code)
	}
}

// TestRepoDeleteReportsCounts covers DELETE /api/repos/{id}: persons and
// grants go away, the folder is removed and the response reports both.
func TestRepoDeleteReportsCounts(t *testing.T) {
	e := newEnv(t, &stubEngine{faces: oneFace}, "Team", true, true)
	// Enroll one person so the repo has something to lose.
	img := pngBytes(t, 80, 80, 0)
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}

	rec := del(t, e.s, e.admin, "/api/repos/"+e.repoID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete repo: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK            bool   `json:"ok"`
		Removed       string `json:"removed"`
		PeopleRemoved int    `json:"people_removed"`
		FolderRemoved bool   `json:"folder_removed"`
	}
	decodeBody(t, rec, &resp)
	if !resp.OK || resp.Removed != e.repoID || resp.PeopleRemoved != 1 || !resp.FolderRemoved {
		t.Errorf("unexpected delete response: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID)); !os.IsNotExist(err) {
		t.Errorf("repo folder should be gone, err=%v", err)
	}
	if len(e.db.PeopleByRepo(e.repoID)) != 0 {
		t.Errorf("repo persons should be gone from the DB")
	}
	// The writer's grant referencing the dead repo is stripped.
	users := e.s.auth.Users()
	for _, u := range users {
		for _, g := range u.Grants {
			if g.RepoID == e.repoID {
				t.Errorf("user %s still holds a grant on the deleted repo", u.Username)
			}
		}
	}
	// The share token no longer grants anything.
	rec2 := do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", nil, shareHdr(e.repoID, e.shareToken))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("share token of deleted repo: got %d, want 401", rec2.Code)
	}
	// Unknown repo → 404.
	if rec := del(t, e.s, e.admin, "/api/repos/00000000-0000-4000-8000-000000000000"); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown repo: got %d, want 404", rec.Code)
	}
}

// TestRepoListRoleFiltering pins GET /api/repos: the admin sees every repo
// with role "admin" and the shared flag; granted users see only their
// repos, without the shared flag; ungranted users see none.
func TestRepoListRoleFiltering(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, true)
	other := seedRepo(t, e.s, e.admin, "Other")

	var resp struct {
		Repos []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			People int    `json:"people"`
			Shared bool   `json:"shared"`
			Role   string `json:"role"`
		} `json:"repos"`
		Count int `json:"count"`
	}

	// Admin: all repos, role "admin", shared flag present.
	rec := get(t, e.s, e.admin, "/api/repos")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin repo list: got %d (%s)", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &resp)
	if resp.Count != 2 || len(resp.Repos) != 2 {
		t.Fatalf("admin sees %d repos, want 2", resp.Count)
	}
	byID := map[string]bool{}
	for _, r := range resp.Repos {
		byID[r.ID] = true
		if r.Role != "admin" {
			t.Errorf("admin role = %q, want admin", r.Role)
		}
		if r.ID == e.repoID && !r.Shared {
			t.Errorf("admin should see the shared flag")
		}
	}
	if !byID[e.repoID] || !byID[other] {
		t.Errorf("admin list misses repos: %+v", resp.Repos)
	}

	// Writer: only the granted repo, role "writer", no shared flag.
	decodeBody(t, get(t, e.s, e.writer, "/api/repos"), &resp)
	if resp.Count != 1 || len(resp.Repos) != 1 || resp.Repos[0].ID != e.repoID {
		t.Fatalf("writer list: %+v", resp.Repos)
	}
	if resp.Repos[0].Role != "writer" {
		t.Errorf("writer role = %q", resp.Repos[0].Role)
	}
	if resp.Repos[0].Shared {
		t.Errorf("non-admin must not see the shared flag")
	}

	// Reader: same, role "reader".
	decodeBody(t, get(t, e.s, e.reader, "/api/repos"), &resp)
	if resp.Count != 1 || resp.Repos[0].Role != "reader" {
		t.Fatalf("reader list: %+v", resp.Repos)
	}

	// Anonymous: 401 (RequireAuth).
	if rec := get(t, e.s, nil, "/api/repos"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon repo list: got %d, want 401", rec.Code)
	}
}

// TestEnrollPersonPersistsImages checks a repo-scoped upload: images land
// in people/<repoID>/<Person>/ with content-derived names, the DB points at
// the same basenames through the repo mutator, and a rescan keeps them.
func TestEnrollPersonPersistsImages(t *testing.T) {
	e := newEnv(t, &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}, "Team", true, false)
	imgA, imgB := []byte("fake-jpeg-A"), []byte("fake-jpeg-B")

	rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.jpg": imgA, "b.jpg": imgB})
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Added int      `json:"added"`
		Saved []string `json:"saved"`
	}
	decodeBody(t, rec, &resp)
	if resp.Added != 2 || len(resp.Saved) != 2 {
		t.Fatalf("expected added=2 saved=2, got added=%d saved=%v", resp.Added, resp.Saved)
	}

	p := e.db.GetInRepo(e.repoID, "Alice")
	if p == nil || len(p.Photos) != 2 {
		t.Fatalf("expected Alice with 2 photos, got %+v", p)
	}
	if p.RepoID != e.repoID {
		t.Errorf("person RepoID = %q, want %q", p.RepoID, e.repoID)
	}
	for _, img := range map[string][]byte{"a.jpg": imgA, "b.jpg": imgB} {
		h := db.HashBytes(img)
		rel := e.repoID + "/Alice/" + h[:12] + ".jpg"
		if !slicesContain(resp.Saved, rel) {
			t.Errorf("saved %q missing from response %v", rel, resp.Saved)
		}
		if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, rel)); err != nil {
			t.Errorf("image not written to people folder: %v", err)
		}
	}
	for _, ph := range p.Photos {
		if strings.Contains(ph.Path, "/") {
			t.Errorf("DB photo path should be a folder-relative basename, got %q", ph.Path)
		}
		if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Alice", ph.Path)); err != nil {
			t.Errorf("DB path %q has no file on disk: %v", ph.Path, err)
		}
	}

	// A rescan keeps both photos (incremental by hash).
	rec = do(t, e.s, http.MethodPost, "/api/enroll?repo="+e.repoID, nil, "", e.writer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rescan: got %d (%s)", rec.Code, rec.Body.String())
	}
	var scan struct {
		People int `json:"people"`
		Added  int `json:"added"`
		Kept   int `json:"kept"`
	}
	decodeBody(t, rec, &scan)
	if scan.People != 1 || scan.Added != 0 || scan.Kept != 2 {
		t.Errorf("unexpected rescan response: %+v", scan)
	}
}

// TestEnrollPersonNoFacePersistsNothing: a no-face upload answers 422 and
// touches neither the dataset folder nor the DB.
func TestEnrollPersonNoFacePersistsNothing(t *testing.T) {
	e := newEnv(t, &stubEngine{faces: nil}, "Team", true, false)

	rec := enrollPerson(t, e.s, e.writer, e.repoID, "Bob", map[string][]byte{"x.jpg": []byte("no-face")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Bob")); !os.IsNotExist(err) {
		t.Errorf("no-face upload should not create a people folder, err=%v", err)
	}
	if e.db.GetInRepo(e.repoID, "Bob") != nil {
		t.Errorf("no-face upload should not create a DB entry")
	}
}

// TestEnrollPersonIdempotent: re-uploading the same bytes adds exactly one
// photo.
func TestEnrollPersonIdempotent(t *testing.T) {
	e := newEnv(t, &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}, "Team", true, false)
	img := []byte("same-image-bytes")
	h := db.HashBytes(img)

	for i := 0; i < 2; i++ {
		rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"again.jpg": img})
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d: got %d (%s)", i, rec.Code, rec.Body.String())
		}
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("expected exactly 1 photo after duplicate uploads, got %+v", p)
	}
	if p.Photos[0].Path != h[:12]+".jpg" {
		t.Errorf("unexpected DB path %q", p.Photos[0].Path)
	}
	entries, err := os.ReadDir(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Alice"))
	if err != nil || len(entries) != 1 {
		t.Errorf("expected exactly 1 file in the people folder (err=%v, n=%d)", err, len(entries))
	}
}

// TestEnrollFace enrolls one detected face by index into a repo person.
func TestEnrollFace(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{0, 0, 10, 10}},
		{BBox: [4]float64{20, 20, 12, 12}},
	}}
	e := newEnv(t, eng, "Team", true, false)
	img := []byte("fake-jpeg-bytes")

	rec := enrollFaceRequest(t, e.s, e.writer, e.repoID, "Alice", img, "2")
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll-face: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("person not enrolled: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Alice", p.Photos[0].Path)); err != nil {
		t.Fatalf("image not saved into the people folder: %v", err)
	}

	// Out-of-range index → 400, nothing enrolled.
	rec = enrollFaceRequest(t, e.s, e.writer, e.repoID, "Bob", img, "3")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range index, got %d", rec.Code)
	}
	if e.db.GetInRepo(e.repoID, "Bob") != nil {
		t.Fatal("failed enroll must not create a person")
	}
	// Non-integer index → 400.
	rec = enrollFaceRequest(t, e.s, e.writer, e.repoID, "Bob", img, "x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-integer index, got %d", rec.Code)
	}
	// Leading-dot name → 400.
	rec = enrollFaceRequest(t, e.s, e.writer, e.repoID, ".hidden", img, "1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad name, got %d", rec.Code)
	}
	// No faces at all → 422.
	empty := newEnv(t, &stubEngine{faces: nil}, "Team2", true, false)
	rec = enrollFaceRequest(t, empty.s, empty.writer, empty.repoID, "Carol", img, "1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when no face is detected, got %d", rec.Code)
	}
}

// TestRecognizeScopedToRepo pins the recognize contract on the multi-user
// model: ?repo= is required (400 without), the handler matches against the
// repo's person IDs (the stub records the allowed set), and the response
// shape is unchanged.
func TestRecognizeScopedToRepo(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 50, 60}, Name: "Alice", Confidence: 0.9, Score: 0.8},
	}}
	e := newEnv(t, eng, "Team", true, true)

	// No repo parameter → 400, before admission control.
	rec := do(t, e.s, http.MethodPost, "/api/recognize", strings.NewReader("img"), "", e.reader, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("recognize without repo: got %d (%s), want 400", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	if errBody["error"] != "repository parameter required" {
		t.Errorf("error body = %q", errBody["error"])
	}

	// Reader (and share token) may recognize their repo.
	img := pngBytes(t, 60, 60, 0)
	body, ct := multipartBody(t, "image", "photo.png", img)
	rec = do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, e.reader, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int           `json:"count"`
		Faces []engine.Face `json:"faces"`
	}
	decodeBody(t, rec, &resp)
	if resp.Count != 1 || resp.Faces[0].Name != "Alice" {
		t.Fatalf("unexpected faces: %+v", resp.Faces)
	}
	if strings.Contains(rec.Body.String(), "embedding") {
		t.Errorf("response must not leak embeddings")
	}

	// The allowed set equals the repo's person IDs (empty repo → empty set).
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	body, ct = multipartBody(t, "image", "photo.png", img)
	if rec := do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, e.reader, nil); rec.Code != http.StatusOK {
		t.Fatalf("recognize after enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	allowed := eng.recordedAllowed()
	if len(allowed) != 1 {
		t.Fatalf("allowed set = %v, want one ID", allowed)
	}
	if _, ok := allowed[p.ID]; !ok {
		t.Errorf("allowed set misses the repo person %q", p.ID)
	}

	// Share token recognizes too (reader role).
	body, ct = multipartBody(t, "image", "photo.png", img)
	rec = do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, nil, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("share recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestRecognizeDraw checks ?draw=1: the response is the annotated JPEG.
func TestRecognizeDraw(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 50, 60}, Name: "Alice", Confidence: 0.9},
	}}
	e := newEnv(t, eng, "Team", true, false)

	img := pngBytes(t, 160, 120, 0)
	body, ct := multipartBody(t, "image", "photo.png", img)
	rec := do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID+"&draw=1", body, ct, e.reader, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("draw recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", got)
	}
	_, format, err := image.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}

	// Without the flag the JSON shape is unchanged.
	body, ct = multipartBody(t, "image", "photo.png", img)
	rec = do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, e.reader, nil)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("plain recognize content type = %q, want application/json", got)
	}
}

// TestRecognizeRejectsOversizedImage pins the C1 HTTP contract: an image
// whose declared dimensions exceed the decode pixel cap is a client error
// (400).
func TestRecognizeRejectsOversizedImage(t *testing.T) {
	eng := &stubEngine{
		recognizeErr: fmt.Errorf("decode source image: %w", engine.ErrImageTooLarge),
	}
	e := newEnv(t, eng, "Team", true, false)
	body, ct := multipartBody(t, "image", "photo.jpg", []byte("tiny"))
	rec := do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, e.reader, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized image: got %d (%s), want 400", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	decodeBody(t, rec, &resp)
	if resp.Error == "" {
		t.Errorf("expected an error message in the response body")
	}
}

// TestThumbAccess covers the thumbnail route's repo-derived access: repo
// persons serve to readers (and share tokens) of that repo; legacy persons
// (RepoID "") serve to the admin only.
func TestThumbAccess(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, true)
	img := pngBytes(t, 120, 120, 0)
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p == nil || p.Thumb == "" {
		t.Fatalf("expected a thumbnail sidecar, got %+v", p)
	}

	// Reader + share: 200 with cache headers.
	for _, actor := range []struct {
		cookie *http.Cookie
		hdr    map[string]string
	}{{e.reader, nil}, {nil, shareHdr(e.repoID, e.shareToken)}} {
		rec := do(t, e.s, http.MethodGet, "/api/thumbs/"+p.ID+".jpg", nil, "", actor.cookie, actor.hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("thumb: got %d (%s)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
			t.Errorf("content type %q, want image/jpeg", ct)
		}
		if rec.Header().Get("Cache-Control") == "" {
			t.Errorf("expected Cache-Control header")
		}
	}

	// Anonymous → 401 (guard).
	if rec := get(t, e.s, nil, "/api/thumbs/"+p.ID+".jpg"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon thumb: got %d, want 401", rec.Code)
	}

	// A legacy person's thumbnail is admin-only.
	if err := e.db.AddPhoto("Legacy", "l.png", img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	legacy := e.db.Get("Legacy")
	if legacy == nil || legacy.RepoID != "" {
		t.Fatalf("legacy fixture: %+v", legacy)
	}
	if err := e.db.SetThumbnail(legacy.ID, pngBytes(t, 32, 32, 9), "l.png"); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, e.s, e.admin, "/api/thumbs/"+legacy.ID+".jpg"); rec.Code != http.StatusOK {
		t.Errorf("admin legacy thumb: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := get(t, e.s, e.reader, "/api/thumbs/"+legacy.ID+".jpg"); rec.Code != http.StatusForbidden {
		t.Errorf("reader legacy thumb: got %d, want 403", rec.Code)
	}
	// A share token does not authenticate a principal, so the legacy
	// (admin-only) thumb answers the same 401 RequireAdmin would.
	if rec := do(t, e.s, http.MethodGet, "/api/thumbs/"+legacy.ID+".jpg", nil, "", nil, shareHdr(e.repoID, e.shareToken)); rec.Code != http.StatusUnauthorized {
		t.Errorf("share legacy thumb: got %d, want 401", rec.Code)
	}

	// Unknown id → 404; POST → 405.
	if rec := get(t, e.s, e.admin, "/api/thumbs/nosuch.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: got %d, want 404", rec.Code)
	}
	if rec := do(t, e.s, http.MethodPost, "/api/thumbs/"+p.ID+".jpg", nil, "", e.admin, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST thumb: got %d, want 405", rec.Code)
	}
}

// TestBodyTooLarge checks that request bodies over the size cap are
// rejected with 400 instead of being buffered.
func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, &stubEngine{threshold: 0.45}, "Team", true, false)

	// /api/config POST caps at 1 MiB (admin).
	rec := do(t, e.s, http.MethodPost, "/api/config", bytes.NewReader(make([]byte, (1<<20)+1)), "application/json", e.admin, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized config body: got %d, want 400", rec.Code)
	}

	// /api/recognize raw body caps at maxUpload (reader with a repo).
	rec = do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, bytes.NewReader(make([]byte, maxUpload+1)), "", e.reader, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized recognize body: got %d, want 400", rec.Code)
	}
}

// TestIndexServed covers the SPA and its assets at "/".
func TestIndexServed(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{})

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
