package auth

// Principal resolution and role mapping tests: cookie sessions, Bearer
// session tokens, "rk_" API keys, anonymous requests — and how RequireAuth
// / RequireAdmin / RoleFor treat each of them. The user record behind every
// Principal is loaded fresh, so admin/grant edits apply to live
// credentials without a re-login.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"recogn/internal/users"
)

// sessionCookieReq builds a request carrying a fresh session cookie for
// username.
func sessionCookieReq(t *testing.T, s *Service, method, target, username string) *http.Request {
	t.Helper()
	token, _ := s.sessions.New(username)
	req := newTestRequest(method, target)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	return req
}

// bearerReq builds a request with an Authorization: Bearer header.
func bearerReq(t *testing.T, s *Service, method, target, token string) *http.Request {
	t.Helper()
	req := newTestRequest(method, target)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// okHandler is the terminal handler for middleware tests.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestPrincipalViaCookie(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	p := s.Principal(sessionCookieReq(t, s, "GET", "/api/config", "admin"))
	if !p.Authed || p.Username != "admin" || !p.Admin || p.ViaKey {
		t.Fatalf("cookie principal = %+v", p)
	}
}

func TestPrincipalViaBearerSession(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	token, _ := s.sessions.New("admin")
	p := s.Principal(bearerReq(t, s, "GET", "/api/config", token))
	if !p.Authed || p.Username != "admin" || !p.Admin || p.ViaKey {
		t.Fatalf("bearer-session principal = %+v", p)
	}
}

func TestPrincipalViaAPIKey(t *testing.T) {
	s := newTestService(t, "")
	if err := s.CreateUser("dev", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("dev", []users.Grant{{RepoID: "r1", Role: users.RoleReader}}); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.users.CreateKey("dev", "ci")
	if err != nil {
		t.Fatal(err)
	}

	p := s.Principal(bearerReq(t, s, "GET", "/api/people", token))
	if !p.Authed || p.Username != "dev" || p.Admin || !p.ViaKey {
		t.Fatalf("api-key principal = %+v", p)
	}
	if got := s.RoleFor(p, "r1"); got != users.RoleReader {
		t.Fatalf("api-key RoleFor = %q, want reader", got)
	}

	// Grants are live: promoting the user applies to the existing key.
	if err := s.SetUserGrants("dev", []users.Grant{{RepoID: "r1", Role: users.RoleWriter}}); err != nil {
		t.Fatal(err)
	}
	p = s.Principal(bearerReq(t, s, "GET", "/api/people", token))
	if got := s.RoleFor(p, "r1"); got != users.RoleWriter {
		t.Fatalf("after promotion RoleFor = %q, want writer (fresh record)", got)
	}

	// Unknown and foreign-prefixed tokens authenticate nobody.
	if p := s.Principal(bearerReq(t, s, "GET", "/api/people", "rk_unknown")); p.Authed {
		t.Fatalf("unknown api key principal = %+v", p)
	}
	if p := s.Principal(bearerReq(t, s, "GET", "/api/people", "not-a-token")); p.Authed {
		t.Fatalf("garbage bearer principal = %+v", p)
	}
}

func TestPrincipalAnonymous(t *testing.T) {
	s := newTestService(t, "sekret-pw")

	for name, req := range map[string]*http.Request{
		"no credentials":      newTestRequest("GET", "/api/config"),
		"expired session":     bearerReq(t, s, "GET", "/api/config", "stale-token"),
		"bearer of plaintext": bearerReq(t, s, "GET", "/api/config", "sekret-pw"),
	} {
		if p := s.Principal(req); p.Authed {
			t.Fatalf("%s: principal = %+v", name, p)
		}
	}
}

func TestPrincipalOfDeletedUser(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	if err := s.CreateUser("temp", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.users.CreateKey("temp", "k")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := s.sessions.New("temp")

	if _, err := s.DeleteUser("temp"); err != nil {
		t.Fatal(err)
	}
	// Both the key and the session of the deleted user stop authenticating.
	if p := s.Principal(bearerReq(t, s, "GET", "/api/config", token)); p.Authed {
		t.Fatalf("deleted user's api key principal = %+v", p)
	}
	req := newTestRequest("GET", "/api/config")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	if p := s.Principal(req); p.Authed {
		t.Fatalf("deleted user's session principal = %+v", p)
	}
}

func TestRoleForMatrix(t *testing.T) {
	s := newTestService(t, "sekret-pw") // admin user "admin"
	if err := s.CreateUser("reader", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("writer", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("reader", []users.Grant{{RepoID: "r1", Role: users.RoleReader}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("writer", []users.Grant{{RepoID: "r1", Role: users.RoleWriter}}); err != nil {
		t.Fatal(err)
	}
	adminP := s.Principal(sessionCookieReq(t, s, "GET", "/", "admin"))
	readerP := s.Principal(sessionCookieReq(t, s, "GET", "/", "reader"))
	writerP := s.Principal(sessionCookieReq(t, s, "GET", "/", "writer"))
	anon := Principal{}

	cases := []struct {
		name   string
		p      Principal
		repoID string
		want   string
	}{
		{"admin is writer everywhere", adminP, "anything", users.RoleWriter},
		{"admin is writer on unknown repos", adminP, "nonexistent", users.RoleWriter},
		{"reader grant", readerP, "r1", users.RoleReader},
		{"reader without grant", readerP, "r2", ""},
		{"writer grant", writerP, "r1", users.RoleWriter},
		{"unauthenticated", anon, "r1", ""},
	}
	for _, tc := range cases {
		if got := s.RoleFor(tc.p, tc.repoID); got != tc.want {
			t.Errorf("%s: RoleFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRequireAuthAndRequireAdmin(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	if err := s.CreateUser("reader", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("reader", []users.Grant{{RepoID: "r1", Role: users.RoleReader}}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		req   *http.Request
		auth  int
		admin int
	}{
		{"anonymous", newTestRequest("GET", "/api/people"), 401, 401},
		{"reader", sessionCookieReq(t, s, "GET", "/api/people", "reader"), 200, 403},
		{"admin", sessionCookieReq(t, s, "GET", "/api/people", "admin"), 200, 200},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.RequireAuth(okHandler).ServeHTTP(rec, tc.req)
		if rec.Code != tc.auth {
			t.Errorf("%s: RequireAuth = %d, want %d", tc.name, rec.Code, tc.auth)
		}
		rec = httptest.NewRecorder()
		s.RequireAdmin(okHandler).ServeHTTP(rec, tc.req)
		if rec.Code != tc.admin {
			t.Errorf("%s: RequireAdmin = %d, want %d", tc.name, rec.Code, tc.admin)
		}
	}

	// Exact error bodies.
	rec := httptest.NewRecorder()
	s.RequireAuth(okHandler).ServeHTTP(rec, newTestRequest("GET", "/api/people"))
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "authentication required" {
		t.Fatalf("401 body = %q", body["error"])
	}
	rec = httptest.NewRecorder()
	s.RequireAdmin(okHandler).ServeHTTP(rec, sessionCookieReq(t, s, "GET", "/api/people", "reader"))
	body = map[string]string{}
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] != "administrator access required" {
		t.Fatalf("403 body = %q", body["error"])
	}
}

func TestRequireAdminViaAPIKey(t *testing.T) {
	// An API key of an ADMIN user is an admin credential; an API key of a
	// non-admin is not, even with grants.
	s := newTestService(t, "")
	if err := s.CreateUser("boss", "long-enough", true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("peon", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := s.users.CreateKey("boss", "a")
	if err != nil {
		t.Fatal(err)
	}
	peonKey, _, err := s.users.CreateKey("peon", "p")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.RequireAdmin(okHandler).ServeHTTP(rec, bearerReq(t, s, "GET", "/api/users", adminKey))
	if rec.Code != 200 {
		t.Fatalf("admin api key: got %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.RequireAdmin(okHandler).ServeHTTP(rec, bearerReq(t, s, "GET", "/api/users", peonKey))
	if rec.Code != 403 {
		t.Fatalf("non-admin api key: got %d, want 403", rec.Code)
	}
}

func TestServiceDeleteUserRefusesAdmin(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	deleted, err := s.DeleteUser("admin")
	if deleted || !errors.Is(err, ErrAdminProtected) {
		t.Fatalf("deleting an admin = %v, %v; want (false, ErrAdminProtected)", deleted, err)
	}
	if _, ok := s.users.GetUser("admin"); !ok {
		t.Fatal("the admin must survive the refused delete")
	}

	// Users() exposes the Admin flag so the api layer can pre-check.
	var sawAdmin bool
	for _, u := range s.Users() {
		if u.Username == "admin" {
			sawAdmin = u.Admin
		}
	}
	if !sawAdmin {
		t.Fatal("Users() must expose the Admin flag")
	}
}

func TestServiceRemoveRepoStripsGrants(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	for _, n := range []string{"u1", "u2", "u3"} {
		if err := s.CreateUser(n, "long-enough", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserGrants("u1", []users.Grant{
		{RepoID: "r1", Role: users.RoleReader},
		{RepoID: "r2", Role: users.RoleWriter},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("u2", []users.Grant{{RepoID: "r2", Role: users.RoleReader}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserGrants("u3", []users.Grant{{RepoID: "r9", Role: users.RoleReader}}); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveRepo("r2"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	byName := map[string]users.User{}
	for _, u := range s.Users() {
		byName[u.Username] = u
	}
	if g := byName["u1"].Grants; len(g) != 1 || g[0].RepoID != "r1" {
		t.Fatalf("u1 grants = %+v, want only the r1 reader grant", g)
	}
	if g := byName["u2"].Grants; len(g) != 0 {
		t.Fatalf("u2 grants = %+v, want empty", g)
	}
	if g := byName["u3"].Grants; len(g) != 1 || g[0].RepoID != "r9" {
		t.Fatalf("u3 grants must be untouched: %+v", g)
	}
}
