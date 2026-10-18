package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestService builds a Service over a temp users.db with the recorder
// plumbing to drive its handlers. When pw != "", an admin account
// "admin"/pw is created first (which consumes the pending setup key).
func newTestService(t *testing.T, pw string) *Service {
	t.Helper()
	cfg := newTestConfig(t)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if pw != "" {
		if err := s.CreateUser("admin", pw, true); err != nil {
			t.Fatalf("create admin user: %v", err)
		}
	}
	return s
}

// doJSON sends a request with an optional JSON body to a handler and
// returns the recorder.
func doJSON(t *testing.T, h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// loginJSON is the new multi-user login body.
func loginJSON(username, password string) map[string]string {
	return map[string]string{"username": username, "password": password}
}

// errBodyOf decodes a handler response's {"error": ...} body.
func errBodyOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response body is not a JSON error object: %s", rec.Body.String())
	}
	return body["error"]
}

func TestModeSetupUntilUsersExist(t *testing.T) {
	s := newTestService(t, "")
	if s.Mode() != "setup" || !s.SetupRequired() {
		t.Fatalf("no users: mode=%q setup_required=%v", s.Mode(), s.SetupRequired())
	}
	if err := s.CreateUser("admin", "long-enough", true); err != nil {
		t.Fatal(err)
	}
	if s.Mode() != "users" || s.SetupRequired() {
		t.Fatalf("after first user: mode=%q setup_required=%v", s.Mode(), s.SetupRequired())
	}
}

func TestLoginMatrix(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	h := s.LoginHandler()

	// Wrong method.
	rec := doJSON(t, h, "GET", "/api/login", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login: got %d, want 405", rec.Code)
	}

	// Unknown username → 401 with the uniform message.
	rec = doJSON(t, h, "POST", "/api/login", loginJSON("nobody", "whatever-pw"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: got %d, want 401", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg != "invalid username or password" {
		t.Fatalf("unknown-user error body = %q", msg)
	}

	// Known username, wrong password → the SAME status and message.
	rec = doJSON(t, h, "POST", "/api/login", loginJSON("admin", "nope"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg != "invalid username or password" {
		t.Fatalf("wrong-password error body = %q", msg)
	}

	// Empty password → same treatment.
	rec = doJSON(t, h, "POST", "/api/login", loginJSON("admin", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty password: got %d, want 401", rec.Code)
	}

	// Success: sets the session cookie and returns the user summary.
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"Admin","password":"sekret-pw"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK   bool `json:"ok"`
		User struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"user"`
	}
	json.NewDecoder(rec.Body).Decode(&ok)
	if !ok.OK || ok.User.Name != "admin" || !ok.User.Admin {
		t.Fatalf("login body = %s", rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookiesNamed(cookies, SessionCookie)) != 1 {
		t.Fatalf("no session cookie set: %v", cookies)
	}
	c := cookiesNamed(cookies, SessionCookie)[0]
	if !c.HttpOnly || c.Path != "/" || c.MaxAge != int(s.cfg.SessionTTL.Seconds()) {
		t.Fatalf("unexpected cookie %+v", c)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Secure {
		t.Fatal("plain-http login must not set Secure")
	}
	// The minted session actually authenticates requests.
	p := func() Principal {
		req := newTestRequest("GET", "/api/config")
		req.AddCookie(c)
		return s.Principal(req)
	}()
	if !p.Authed || p.Username != "admin" || !p.Admin || p.ViaKey {
		t.Fatalf("login cookie principal = %+v", p)
	}
}

func TestLoginInvalidJSON(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	rec := httptest.NewRecorder()
	s.LoginHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/login", strings.NewReader("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: got %d, want 400", rec.Code)
	}
}

func TestLoginRateLimited(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	h := s.LoginHandler()

	for i := 0; i < 11; i++ { // 11 failures trip the limiter
		rec := doJSON(t, h, "POST", "/api/login", loginJSON("admin", "bad"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, rec.Code)
		}
	}
	rec := doJSON(t, h, "POST", "/api/login", loginJSON("admin", "bad"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 11 failures: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	// Even the CORRECT credentials are denied while locked out.
	rec = doJSON(t, h, "POST", "/api/login", loginJSON("admin", "sekret-pw"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct credentials while locked: got %d, want 429", rec.Code)
	}

	// A different IP is unaffected.
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"sekret-pw"}`))
	req.RemoteAddr = "10.0.0.9:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", rec.Code)
	}
}

func TestLogoutHandler(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	token, _ := s.sessions.New("admin")

	req := newTestRequest("POST", "/api/logout")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	s.LogoutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: got %d", rec.Code)
	}
	if _, ok := s.sessions.Valid(token); ok {
		t.Fatal("session must be deleted server-side")
	}
	// The cookie is cleared: Max-Age 0.
	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge != 0 {
		t.Fatalf("logout must clear the cookie, got %+v", cleared)
	}

	// Idempotent: logout without a cookie is still 200.
	rec = httptest.NewRecorder()
	s.LogoutHandler().ServeHTTP(rec, newTestRequest("POST", "/api/logout"))
	if rec.Code != http.StatusOK {
		t.Fatalf("cookieless logout: got %d", rec.Code)
	}
	// Wrong method → 405.
	rec = httptest.NewRecorder()
	s.LogoutHandler().ServeHTTP(rec, newTestRequest("GET", "/api/logout"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout: got %d, want 405", rec.Code)
	}
}

func TestSessionHandler(t *testing.T) {
	s := newTestService(t, "sekret-pw")

	// Anonymous: not authenticated, and setup is over (an admin exists).
	rec := doJSON(t, s.SessionHandler(), "GET", "/api/auth/session", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session: got %d", rec.Code)
	}
	var out struct {
		Authenticated bool `json:"authenticated"`
		User          *struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"user"`
		SetupRequired bool `json:"setup_required"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if out.Authenticated || out.User != nil || out.SetupRequired {
		t.Fatalf("anon session shape: %+v", out)
	}

	// After login: authenticated with the user summary.
	token, _ := s.sessions.New("admin")
	req := newTestRequest("GET", "/api/auth/session")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec = httptest.NewRecorder()
	s.SessionHandler().ServeHTTP(rec, req)
	json.NewDecoder(rec.Body).Decode(&out)
	if !out.Authenticated || out.User == nil || out.User.Name != "admin" || !out.User.Admin || out.SetupRequired {
		t.Fatalf("authed session shape: %+v", out)
	}
	// Wrong method → 405.
	rec = doJSON(t, s.SessionHandler(), "POST", "/api/auth/session", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST session: got %d, want 405", rec.Code)
	}
}

func TestSessionHandlerSetupRequired(t *testing.T) {
	// A fresh install reports the pending bootstrap to the UI.
	s := newTestService(t, "")
	rec := doJSON(t, s.SessionHandler(), "GET", "/api/auth/session", nil)
	var out struct {
		SetupRequired bool `json:"setup_required"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if !out.SetupRequired {
		t.Fatal("a zero-user install must report setup_required")
	}
}

// cookiesNamed returns the cookies with the given name.
func cookiesNamed(cookies []*http.Cookie, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range cookies {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}
