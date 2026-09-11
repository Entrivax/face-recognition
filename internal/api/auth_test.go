package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recogn/internal/auth"
	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
)

// testAdminPassword is the password every auth test configures; tests use
// its precomputed argon2id hash (testAdminPasswordHash).
const testAdminPassword = "test-password-123"

var testAdminPasswordHash = func() string {
	h, err := auth.HashPassword([]byte(testAdminPassword))
	if err != nil {
		panic(err)
	}
	return h
}()

// newAuthTestServer builds a Server with admin password auth enabled and a
// hermetic DataDir (passkey store). Open-mode tests reuse newTestServer.
func newAuthTestServer(t *testing.T, eng Engine) (*Server, *db.DB) {
	t.Helper()
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
	cfg.AdminPasswordHash = testAdminPasswordHash
	s, err := New(cfg, eng, database, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, database
}

// authReq builds a request from a given RemoteAddr so rate-limit buckets can
// be isolated per test (the s parameter keeps call sites reading like the
// rest of the file).
func authReq(t *testing.T, _ *Server, method, target, remoteIP string, body io.Reader, header map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = remoteIP + ":1234"
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

// login POSTs a password to /api/login and returns the recorder.
func login(t *testing.T, s *Server, remoteIP, password string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"password": password})
	req := authReq(t, s, http.MethodPost, "/api/login", remoteIP, bytes.NewReader(b), map[string]string{"Content-Type": "application/json"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// sessionCookie extracts the recogn_session cookie from a login response.
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

// loginAdmin logs in via POST /api/login and returns the admin's session
// cookie. It replaces the old bearer-password helper: the password itself is
// never accepted as a Bearer credential anymore.
func loginAdmin(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := login(t, s, "1.2.3.4", testAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login: got %d (%s)", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec)
}

// bearerSession sets a Bearer header carrying a session token on a request.
func bearerSession(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestAuthOpenModeStaysPublic(t *testing.T) {
	// newTestServer has no password and no passkeys: open mode. Everything
	// keeps working exactly as before, including the admin routes.
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newTestServer(t, eng)
	img := pngBytes(t, 120, 120, 3)
	if rec := enrollPerson(t, s, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("open-mode enroll: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Admin POST /api/enroll succeeds unauthenticated.
	req := httptest.NewRequest(http.MethodPost, "/api/enroll", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open-mode rescan: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Login reports "not configured".
	rec = postJSON(t, s, "/api/login", map[string]string{"password": "whatever"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("open-mode login: got %d, want 401", rec.Code)
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "admin authentication is not configured" {
		t.Fatalf("error body = %q", body["error"])
	}

	// Health reports both methods off.
	var health struct {
		Auth map[string]bool `json:"auth"`
	}
	if r := getJSON(t, s, "/api/health", &health); r.Code != http.StatusOK {
		t.Fatalf("health: got %d", r.Code)
	}
	if health.Auth["password"] || health.Auth["passkey"] {
		t.Fatalf("open-mode auth flags: %v", health.Auth)
	}

	// Session endpoint: open mode is always authenticated, no methods.
	var session struct {
		Authenticated bool            `json:"authenticated"`
		Methods       map[string]bool `json:"methods"`
	}
	if r := getJSON(t, s, "/api/auth/session", &session); r.Code != http.StatusOK {
		t.Fatalf("session: got %d", r.Code)
	}
	if !session.Authenticated || session.Methods["password"] || session.Methods["passkey"] {
		t.Fatalf("open-mode session shape: %+v", session)
	}

	// Public reads stay 200.
	if code := getJSON(t, s, "/api/people", nil).Code; code != http.StatusOK {
		t.Errorf("people list: got %d", code)
	}
	if code := getJSON(t, s, "/api/people/Alice", nil).Code; code != http.StatusOK {
		t.Errorf("person detail: got %d", code)
	}
	_ = database
	_ = img
}

func TestAuthAnonymousAdminRoutesReturn401(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newAuthTestServer(t, eng)
	img := pngBytes(t, 120, 120, 5)
	photo := db.HashBytes(img)[:12] + ".png"
	if err := database.AddPhoto("Alice", photo, img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(s.cfg.PeopleDir, "Alice"), 0o755)
	os.WriteFile(filepath.Join(s.cfg.PeopleDir, "Alice", photo), img, 0o644)

	body, ct := multipartBody(t, "images", "x.jpg", []byte("img"))
	cases := []struct {
		name   string
		method string
		path   string
		body   io.Reader // io.Reader (not *bytes.Buffer) so nil stays a nil interface
		ct     string
	}{
		{"person detail", http.MethodGet, "/api/people/Alice", nil, ""},
		{"photo file", http.MethodGet, "/api/people/Alice/photos/" + photo, nil, ""},
		{"photo detect", http.MethodGet, "/api/people/Alice/photos/" + photo + "/detect", nil, ""},
		{"enroll person", http.MethodPost, "/api/people/Alice/enroll", body, ct},
		{"enroll-face", http.MethodPost, "/api/people/Alice/enroll-face", body, ct},
		{"rename", http.MethodPost, "/api/people/Alice/rename", bytes.NewBufferString(`{"name":"Bob"}`), "application/json"},
		{"thumbnail", http.MethodPost, "/api/people/Alice/thumbnail", bytes.NewBufferString(`{"photo":"` + photo + `"}`), "application/json"},
		{"delete photo", http.MethodDelete, "/api/people/Alice/photos/" + photo, nil, ""},
		{"delete person", http.MethodDelete, "/api/people/Alice", nil, ""},
		{"enroll folder", http.MethodPost, "/api/enroll", nil, ""},
		{"config GET", http.MethodGet, "/api/config", nil, ""},
		{"config POST", http.MethodPost, "/api/config", bytes.NewBufferString(`{"threshold":0.5}`), "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authReq(t, s, tc.method, tc.path, "9.9.9.9", tc.body, map[string]string{"Content-Type": tc.ct})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got %d, want 401", tc.name, rec.Code)
			}
			var errBody map[string]string
			json.NewDecoder(rec.Body).Decode(&errBody)
			if errBody["error"] != "authentication required" {
				t.Fatalf("%s: error body = %q", tc.name, errBody["error"])
			}
		})
	}
}

func TestAuthSessionBearerGrantsAccess(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newAuthTestServer(t, eng)

	// Log in once; the cookie value is the session token.
	c := loginAdmin(t, s)

	// The session token as a Bearer credential authorizes admin routes.
	req := authReq(t, s, http.MethodGet, "/api/config", "1.1.1.1", nil, nil)
	bearerSession(req, c.Value)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer session on /api/config: got %d (%s)", rec.Code, rec.Body.String())
	}

	// ...also for a mutating admin route.
	req = authReq(t, s, http.MethodPost, "/api/config", "1.1.1.1", bytes.NewBufferString(`{"threshold":0.5}`), map[string]string{"Content-Type": "application/json"})
	bearerSession(req, c.Value)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer session on POST /api/config: got %d (%s)", rec.Code, rec.Body.String())
	}

	// The password itself must NEVER authenticate as a Bearer token — this
	// is the fix: Bearer accepts sessions only.
	req = authReq(t, s, http.MethodGet, "/api/config", "1.1.1.1", nil, nil)
	bearerSession(req, testAdminPassword)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer password must not authenticate: got %d, want 401", rec.Code)
	}

	// Unknown Bearer tokens are rejected too.
	req = authReq(t, s, http.MethodGet, "/api/config", "1.1.1.1", nil, nil)
	bearerSession(req, "not-a-live-session-token")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer garbage: got %d, want 401", rec.Code)
	}
}

func TestAuthPublicRoutesStayAnonymous(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, database := newAuthTestServer(t, eng)
	img := pngBytes(t, 120, 120, 5)
	// Enroll via the DB directly (people list + thumbnails only read).
	if err := database.AddPhoto("Alice", "a.png", img, []float32{1}); err != nil {
		t.Fatal(err)
	}
	database.SetThumbnail("alice", pngBytes(t, 32, 32, 9), "a.png")

	// recognize (multipart) is public.
	body, ct := multipartBody(t, "image", "photo.jpg", img)
	req := authReq(t, s, http.MethodPost, "/api/recognize", "2.2.2.2", body, map[string]string{"Content-Type": ct})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anon recognize: got %d (%s)", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/", "/api/health", "/api/people", "/api/thumbs/alice.jpg"} {
		req := authReq(t, s, http.MethodGet, path, "2.2.2.2", nil, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("anon GET %s: got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthLoginLogoutFlow(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newAuthTestServer(t, eng)

	// Wrong password → 401 with the specific error.
	rec := login(t, s, "3.3.3.3", "not-the-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", rec.Code)
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "invalid password" {
		t.Fatalf("error body = %q", body["error"])
	}

	// Right password → 200, cookie with sane attributes.
	rec = login(t, s, "3.3.3.3", testAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", rec.Code, rec.Body.String())
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("unexpected cookie %+v", c)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", c.SameSite)
	}
	if rec.Header().Get("X-Forwarded-Proto") != "" && c.Secure {
		t.Fatal("plain-http login must not set Secure")
	}

	// The cookie authorizes admin routes.
	req := authReq(t, s, http.MethodGet, "/api/config", "3.3.3.3", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin with cookie: got %d", rec.Code)
	}

	// Session endpoint shows authenticated + methods.
	var session struct {
		Authenticated bool `json:"authenticated"`
		Methods       struct {
			Password bool `json:"password"`
			Passkey  bool `json:"passkey"`
		} `json:"methods"`
	}
	req = authReq(t, s, http.MethodGet, "/api/auth/session", "3.3.3.3", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	json.NewDecoder(rec.Body).Decode(&session)
	if !session.Authenticated || !session.Methods.Password || session.Methods.Passkey {
		t.Fatalf("session shape: %+v", session)
	}

	// Logout clears the session server-side.
	req = authReq(t, s, http.MethodPost, "/api/logout", "3.3.3.3", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: got %d", rec.Code)
	}
	cleared := false
	for _, cc := range rec.Result().Cookies() {
		if cc.Name == "recogn_session" && cc.MaxAge == 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout must clear the cookie (Max-Age=0)")
	}
	// The old cookie no longer authorizes anything.
	req = authReq(t, s, http.MethodGet, "/api/config", "3.3.3.3", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale session after logout: got %d, want 401", rec.Code)
	}
}

func TestAuthLoginSecureCookieBehindProxy(t *testing.T) {
	eng := &stubEngine{}
	s, _ := newAuthTestServer(t, eng)
	rec := httptest.NewRecorder()
	req := authReq(t, s, http.MethodPost, "/api/login", "4.4.4.4",
		bytes.NewReader([]byte(`{"password":"`+testAdminPassword+`"}`)),
		map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"})
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d", rec.Code)
	}
	if c := sessionCookie(t, rec); !c.Secure {
		t.Fatal("X-Forwarded-Proto: https must set the Secure flag")
	}
}

func TestAuthLoginRateLimit(t *testing.T) {
	eng := &stubEngine{}
	s, _ := newAuthTestServer(t, eng)

	// 11 wrong passwords from one IP are each answered 401...
	for i := 0; i < 11; i++ {
		rec := login(t, s, "5.5.5.5", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, rec.Code)
		}
	}
	// ...and then the limiter denies with 429 + Retry-After.
	rec := login(t, s, "5.5.5.5", testAdminPassword)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 11 failures: got %d (%s), want 429", rec.Code, rec.Body.String())
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "too many attempts" {
		t.Fatalf("error body = %q", body["error"])
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 must carry a Retry-After header")
	}

	// A fresh server (or a different IP) is not affected.
	rec = login(t, s, "6.6.6.6", testAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", rec.Code)
	}
	s2, _ := newAuthTestServer(t, &stubEngine{})
	if rec := login(t, s2, "5.5.5.5", testAdminPassword); rec.Code != http.StatusOK {
		t.Fatalf("fresh server: got %d, want 200", rec.Code)
	}
}

func TestAuthHealthFlags(t *testing.T) {
	eng := &stubEngine{}
	s, _ := newAuthTestServer(t, eng)
	var health struct {
		Auth map[string]bool `json:"auth"`
	}
	if r := getJSON(t, s, "/api/health", &health); r.Code != http.StatusOK {
		t.Fatalf("health: got %d", r.Code)
	}
	if !health.Auth["password"] || health.Auth["passkey"] {
		t.Fatalf("password-mode health auth flags: %v", health.Auth)
	}

	// Mode() passthrough.
	if s.Mode() != "password" {
		t.Fatalf("mode = %q, want password", s.Mode())
	}
}

func TestAuthPasskeyRoutes(t *testing.T) {
	eng := &stubEngine{}
	s, _ := newAuthTestServer(t, eng)
	c := loginAdmin(t, s) // admin session cookie replaces the old bearer-password helper

	// Register begin requires admin: anonymous → 401.
	req := authReq(t, s, http.MethodPost, "/api/auth/passkey/register/begin", "7.7.7.7", nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon register begin: got %d, want 401", rec.Code)
	}

	// As admin: returns the CredentialCreation options JSON.
	req = authReq(t, s, http.MethodPost, "/api/auth/passkey/register/begin", "7.7.7.7", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin register begin: got %d (%s)", rec.Code, rec.Body.String())
	}
	var creation struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"rp"`
			User struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&creation); err != nil {
		t.Fatalf("register begin body is not JSON options: %v", err)
	}
	if creation.PublicKey.Challenge == "" || creation.PublicKey.User.Name != "admin" {
		t.Fatalf("unexpected creation options: %+v", creation)
	}

	// Register finish with a garbage body → 400 (never 500).
	req = authReq(t, s, http.MethodPost, "/api/auth/passkey/register/finish", "7.7.7.7", bytes.NewBufferString("not-json"), nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register finish garbage: got %d, want 400", rec.Code)
	}

	// Login begin with no registered passkeys → 401 "not configured".
	req = authReq(t, s, http.MethodPost, "/api/auth/passkey/login/begin", "7.7.7.7", nil, nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login begin without passkeys: got %d, want 401", rec.Code)
	}
	json.NewDecoder(rec.Body).Decode(&creation)
	_ = creation

	// Public routes: unknown suffix → 404; wrong method → 405.
	req = authReq(t, s, http.MethodGet, "/api/auth/passkey/login/begin", "7.7.7.7", nil, nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login begin: got %d, want 405", rec.Code)
	}

	// Passkey list (admin): empty shape.
	req = authReq(t, s, http.MethodGet, "/api/auth/passkeys", "7.7.7.7", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("passkeys list: got %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Passkeys []map[string]string `json:"passkeys"`
	}
	json.NewDecoder(rec.Body).Decode(&list)
	if list.Passkeys == nil || len(list.Passkeys) != 0 {
		t.Fatalf("expected empty passkeys array, got %+v", list)
	}

	// DELETE an unknown passkey id → 404.
	req = authReq(t, s, http.MethodDelete, "/api/auth/passkeys/"+strings.Repeat("A", 22), "7.7.7.7", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown passkey: got %d, want 404", rec.Code)
	}

	// Session endpoint reflects the passkey method state.
	var session struct {
		Methods struct {
			Passkey bool `json:"passkey"`
		} `json:"methods"`
	}
	req = authReq(t, s, http.MethodGet, "/api/auth/session", "7.7.7.7", nil, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	json.NewDecoder(rec.Body).Decode(&session)
	if session.Methods.Passkey {
		t.Fatal("no passkey registered yet, methods.passkey must be false")
	}
}

// TestAuthCorruptPasskeyStoreFailsStartup mirrors the DB's behaviour for a
// corrupt interchange file: a corrupt data/passkeys.json must fail server
// startup loudly, not silently drop the registered credentials.
func TestAuthCorruptPasskeyStoreFailsStartup(t *testing.T) {
	t.Setenv("RECOGN_ADMIN_PASSWORD_HASH", "")
	t.Setenv("RECOGN_WEBAUTHN_RPID", "")
	t.Setenv("RECOGN_WEBAUTHN_ORIGIN", "")
	t.Setenv("RECOGN_WEBAUTHN_RP_NAME", "")
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Addr = ":0"
	cfg.PeopleDir = filepath.Join(t.TempDir(), "people")
	cfg.DataDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "passkeys.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, &stubEngine{}, database, nil); err == nil {
		t.Fatal("a corrupt passkeys.json must fail startup")
	}
}
