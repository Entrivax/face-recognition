package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPasswordHash caches the argon2id hash of "sekret" so tests do not pay
// the KDF cost on every service construction.
var testPasswordHash = mustHashForTest("sekret")

// mustHashForTest hashes pw for test configs, memoizing by password.
func mustHashForTest(pw string) string {
	if pw == "" {
		return "" // empty means unset: open/passkey-only mode
	}
	if v, ok := testHashCache.Load(pw); ok {
		return v.(string)
	}
	h, err := HashPassword([]byte(pw))
	if err != nil {
		panic(err)
	}
	testHashCache.Store(pw, h)
	return h
}

var testHashCache sync.Map

// newTestService builds a Service with a fixed clock and password auth
// enabled, plus the recorder plumbing to drive its handlers.
func newTestService(t *testing.T, password string) *Service {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.AdminPasswordHash = mustHashForTest(password)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return s
}

// doJSON POSTs a JSON body to a handler and returns the recorder.
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

func TestServiceModeAndEnabled(t *testing.T) {
	s := newTestService(t, "")
	if s.Enabled() || s.Mode() != "open" {
		t.Fatalf("no credentials: mode=%q enabled=%v", s.Mode(), s.Enabled())
	}
	cfg := newTestConfig(t)
	cfg.AdminPasswordHash = mustHashForTest("pw")
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Mode() != "password" || !s2.PasswordEnabled() || s2.PasskeyEnabled() {
		t.Fatalf("password only: mode=%q", s2.Mode())
	}
	// A stored credential alone flips to passkey mode.
	s2.store.add(sampleCredential("k1"))
	if s2.Mode() != "password+passkey" {
		t.Fatalf("both methods: mode=%q", s2.Mode())
	}
	s2.cfg.AdminPasswordHash = ""
	if s2.Mode() != "passkey" || !s2.Enabled() {
		t.Fatalf("passkey only: mode=%q", s2.Mode())
	}
}

// TestNewRejectsMalformedPasswordHash pins the startup validation: a typo in
// RECOGN_ADMIN_PASSWORD_HASH must fail startup loudly instead of silently
// disabling password logins.
func TestNewRejectsMalformedPasswordHash(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.AdminPasswordHash = "change-me" // plaintext, not a PHC hash
	if _, err := New(cfg); err == nil {
		t.Fatal("a malformed password hash must fail startup")
	}
	// A well-formed hash is accepted and enables password mode.
	cfg.AdminPasswordHash = testPasswordHash
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("valid hash rejected: %v", err)
	}
	if !s.PasswordEnabled() {
		t.Fatal("configured hash must enable password auth")
	}
}

func TestMiddlewareOpenModePassthrough(t *testing.T) {
	s := newTestService(t, "") // open mode
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := newTestRequest("POST", "/api/enroll")
	rec := httptest.NewRecorder()
	s.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open mode middleware must pass through, got %d", rec.Code)
	}
}

func TestMiddlewareGating(t *testing.T) {
	s := newTestService(t, "sekret")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(next)

	// Anonymous → 401 JSON error.
	req := newTestRequest("GET", "/api/config")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon: got %d, want 401", rec.Code)
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "authentication required" {
		t.Fatalf("error body = %q", body["error"])
	}

	// The password as a Bearer credential must NOT authenticate (not even
	// the correct one): passwords are only checked by POST /api/login.
	req = newTestRequest("GET", "/api/config")
	req.Header.Set("Authorization", "Bearer sekret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer password: got %d, want 401", rec.Code)
	}

	// A Bearer credential carrying a live session token → through.
	sessionToken, _ := s.sessions.New()
	req = newTestRequest("GET", "/api/config")
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer session token: got %d, want 200", rec.Code)
	}

	// Valid session cookie → through.
	token, _ := s.sessions.New()
	req = newTestRequest("GET", "/api/config")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session cookie: got %d, want 200", rec.Code)
	}
}

func TestLoginHandler(t *testing.T) {
	s := newTestService(t, "sekret")
	h := s.LoginHandler()

	// Wrong method.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newTestRequest("GET", "/api/login"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login: got %d, want 405", rec.Code)
	}

	// Success: sets the session cookie and returns {"ok":true}.
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"sekret"}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", rec.Code, rec.Body.String())
	}
	var ok map[string]bool
	json.NewDecoder(rec.Body).Decode(&ok)
	if !ok["ok"] {
		t.Fatalf("body = %s", rec.Body.String())
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
	// The minted session actually authorizes requests.
	req = newTestRequest("GET", "/api/config")
	req.AddCookie(c)
	if !s.Authorized(req) {
		t.Fatal("login cookie must authorize admin requests")
	}

	// Wrong password → 401 and a recorded failure.
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"nope"}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", rec.Code)
	}
	var errBody map[string]string
	json.NewDecoder(rec.Body).Decode(&errBody)
	if errBody["error"] != "invalid password" {
		t.Fatalf("error body = %q", errBody["error"])
	}
	if got := len(s.limiter.failures["192.0.2.1"]); got != 1 {
		t.Fatalf("failure not recorded (%d)", got)
	}
}

func TestLoginRateLimited(t *testing.T) {
	s := newTestService(t, "sekret")
	h := s.LoginHandler()

	for i := 0; i < 11; i++ { // 11 failures trip the limiter
		req := httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"bad"}`)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"bad"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 11 failures: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	// Even the CORRECT password is denied while locked out.
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"sekret"}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password while locked: got %d, want 429", rec.Code)
	}

	// A different IP is unaffected.
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"password":"sekret"}`)))
	req.RemoteAddr = "10.0.0.9:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", rec.Code)
	}
}

func TestLoginNotConfigured(t *testing.T) {
	s := newTestService(t, "") // open mode
	rec := httptest.NewRecorder()
	s.LoginHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"x"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("open-mode login: got %d, want 401", rec.Code)
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "admin authentication is not configured" {
		t.Fatalf("error body = %q", body["error"])
	}
	// And it must not burn rate-limit budget.
	if len(s.limiter.failures) != 0 {
		t.Fatal("unconfigured login must not record a failure")
	}
}

func TestLoginInvalidJSON(t *testing.T) {
	s := newTestService(t, "sekret")
	rec := httptest.NewRecorder()
	s.LoginHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/login", strings.NewReader("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: got %d, want 400", rec.Code)
	}
}

func TestLogoutHandler(t *testing.T) {
	s := newTestService(t, "sekret")
	token, _ := s.sessions.New()

	req := newTestRequest("POST", "/api/logout")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	s.LogoutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: got %d", rec.Code)
	}
	if s.sessions.Valid(token) {
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
	s := newTestService(t, "sekret")

	// Anonymous: not authenticated, password method offered.
	rec := httptest.NewRecorder()
	s.SessionHandler().ServeHTTP(rec, newTestRequest("GET", "/api/auth/session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("session: got %d", rec.Code)
	}
	var out struct {
		Authenticated bool `json:"authenticated"`
		Methods       struct {
			Password bool `json:"password"`
			Passkey  bool `json:"passkey"`
		} `json:"methods"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if out.Authenticated || !out.Methods.Password || out.Methods.Passkey {
		t.Fatalf("anon session shape: %+v", out)
	}

	// After login: authenticated.
	token, _ := s.sessions.New()
	req := newTestRequest("GET", "/api/auth/session")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec = httptest.NewRecorder()
	s.SessionHandler().ServeHTTP(rec, req)
	out = struct {
		Authenticated bool `json:"authenticated"`
		Methods       struct {
			Password bool `json:"password"`
			Passkey  bool `json:"passkey"`
		} `json:"methods"`
	}{}
	json.NewDecoder(rec.Body).Decode(&out)
	if !out.Authenticated {
		t.Fatal("session cookie must authenticate")
	}
	// Wrong method → 405.
	rec = httptest.NewRecorder()
	s.SessionHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/session"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST session: got %d, want 405", rec.Code)
	}
}

func TestSessionHandlerOpenMode(t *testing.T) {
	s := newTestService(t, "") // open mode: everything is authenticated
	rec := httptest.NewRecorder()
	s.SessionHandler().ServeHTTP(rec, newTestRequest("GET", "/api/auth/session"))
	var out struct {
		Authenticated bool `json:"authenticated"`
		Methods       struct {
			Password bool `json:"password"`
			Passkey  bool `json:"passkey"`
		} `json:"methods"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if !out.Authenticated || out.Methods.Password || out.Methods.Passkey {
		t.Fatalf("open mode session shape: %+v", out)
	}
}

func TestPasskeyHandlersGatingAndShape(t *testing.T) {
	s := newTestService(t, "sekret")

	// Login begin without any registered passkey → 401.
	rec := httptest.NewRecorder()
	s.PasskeyLoginHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/login/begin"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login begin without passkeys: got %d, want 401", rec.Code)
	}
	var errBody map[string]string
	json.NewDecoder(rec.Body).Decode(&errBody)
	if !strings.Contains(errBody["error"], "not configured") {
		t.Fatalf("error body = %q", errBody["error"])
	}

	// Register a (fake) credential so the login ceremony can begin.
	s.store.add(sampleCredential("k1"))

	// Register begin is admin-only at the handler level too (the route
	// wraps it in middleware; here we prove the raw handler works when
	// called by an admin).
	rec = httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/begin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("register begin: got %d (%s)", rec.Code, rec.Body.String())
	}
	var creation struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"rp"`
			User struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&creation); err != nil {
		t.Fatalf("register begin body is not the CredentialCreation JSON: %v", err)
	}
	if creation.PublicKey.Challenge == "" {
		t.Fatal("creation options must carry a challenge")
	}
	if creation.PublicKey.RP.ID != "example.com" {
		t.Fatalf("derived RPID = %q (httptest Host is example.com)", creation.PublicKey.RP.ID)
	}
	if creation.PublicKey.RP.Name != "recogn" {
		t.Fatalf("rp name = %q", creation.PublicKey.RP.Name)
	}
	if creation.PublicKey.User.Name != "admin" {
		t.Fatalf("user name = %q", creation.PublicKey.User.Name)
	}

	// Login begin now returns the assertion options JSON.
	rec = httptest.NewRecorder()
	s.PasskeyLoginHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/login/begin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login begin: got %d (%s)", rec.Code, rec.Body.String())
	}
	var assertion struct {
		PublicKey struct {
			Challenge      string `json:"challenge"`
			RelyingPartyID string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&assertion); err != nil {
		t.Fatalf("login begin body is not the CredentialAssertion JSON: %v", err)
	}
	if assertion.PublicKey.Challenge == "" || assertion.PublicKey.RelyingPartyID != "example.com" {
		t.Fatalf("unexpected assertion options: %+v", assertion)
	}

	// Finish with garbage → 400, and an unknown challenge → error.
	rec = httptest.NewRecorder()
	s.PasskeyLoginHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/login/finish"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("login finish garbage: got %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/finish"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register finish garbage: got %d, want 400", rec.Code)
	}

	// Wrong methods → 405.
	rec = httptest.NewRecorder()
	s.PasskeyLoginHandler().ServeHTTP(rec, newTestRequest("GET", "/api/auth/passkey/login/begin"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login begin: got %d, want 405", rec.Code)
	}

	// List & delete endpoints.
	rec = httptest.NewRecorder()
	s.PasskeyListHandler().ServeHTTP(rec, newTestRequest("GET", "/api/auth/passkeys"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d", rec.Code)
	}
	var list struct {
		Passkeys []struct {
			ID      string `json:"id"`
			AddedAt string `json:"added_at"`
		} `json:"passkeys"`
	}
	json.NewDecoder(rec.Body).Decode(&list)
	if len(list.Passkeys) != 1 || list.Passkeys[0].ID == "" || list.Passkeys[0].AddedAt == "" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if _, err := time.Parse(time.RFC3339, list.Passkeys[0].AddedAt); err != nil {
		t.Fatalf("added_at not RFC3339: %v", err)
	}

	// DELETE unknown id → 404; known id → removed.
	rec = httptest.NewRecorder()
	req := newTestRequest("DELETE", "/api/auth/passkeys/"+assertionB64("nope"))
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: got %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = newTestRequest("DELETE", "/api/auth/passkeys/"+list.Passkeys[0].ID)
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete known: got %d (%s)", rec.Code, rec.Body.String())
	}
	if s.PasskeyEnabled() {
		t.Fatal("store should be empty after deleting the only passkey")
	}
}

func TestPasskeyRegisterFinishRejectsBadChallenge(t *testing.T) {
	s := newTestService(t, "sekret")
	s.store.add(sampleCredential("k1"))

	// A syntactically valid assertion body whose challenge was never begun.
	body := `{"id":"a2Fm","rawId":"a2Fm","type":"public-key","response":{"authenticatorData":"eyJjaGFsbGVuZ2Ui","clientDataJSON":"eyJ0eXBlIjoid2ViYXV0aG4uZ2V0IiwiY2hhbGxlbmdlIjoic25vdXplIiwib3JpZ2luIjoiaHR0cDovL2V4YW1wbGUuY29tIn0","signature":"c2ln"}}`
	req := httptest.NewRequest("POST", "/api/auth/passkey/login/finish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.PasskeyLoginHandler().ServeHTTP(rec, req)
	// Either the parse rejects it (400) or the ceremony lookup does (401) —
	// it must never succeed.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale/unknown challenge must not log in: got %d", rec.Code)
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

// assertionB64 encodes a fake credential id the way the API lists them.
func assertionB64(id string) string { return base64URL(id) }
