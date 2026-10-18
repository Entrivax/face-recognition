package auth

// API-key and password-change HTTP tests, driven through the handlers with
// session-cookie principals (and one API-key principal round trip). The
// plaintext token must appear exactly once — in the create response — and
// never again in any listing.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// io_NopReader wraps an io.Reader as io.ReadCloser for httptest bodies.
func io_NopReader(r io.Reader) io.ReadCloser { return io.NopCloser(r) }

// doJSONAuthed is doJSON with a session cookie for username attached (the
// cookie proves the principal; the handler under test stays unmodified).
func doJSONAuthed(t *testing.T, s *Service, h http.Handler, method, target, username string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, target, strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	token, _ := s.sessions.New(username)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAPIKeysHTTPRoundtrip(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	h := s.APIKeysHandler()

	// Anonymous → 401 (the handler wraps itself in RequireAuth).
	rec := doJSON(t, h, "GET", "/api/auth/keys", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous key list: got %d, want 401", rec.Code)
	}

	// Create: the plaintext token appears exactly once, in this response.
	rec = doJSONAuthed(t, s, h, "POST", "/api/auth/keys", "admin", map[string]string{"name": "ci bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create key: got %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Prefix    string `json:"prefix"`
		Token     string `json:"token"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "ci bot" || created.Prefix != created.Token[:12] || len(created.ID) != 12 || created.CreatedAt == "" {
		t.Fatalf("created key shape: %+v", created)
	}
	if !strings.HasPrefix(created.Token, "rk_") || len(created.Token) != 46 {
		t.Fatalf("token shape: %q", created.Token)
	}
	if _, err := time.Parse(time.RFC3339, created.CreatedAt); err != nil {
		t.Fatalf("created_at not RFC3339: %v", err)
	}

	// List: id/name/prefix/created_at only — the token must NOT be there.
	rec = doJSONAuthed(t, s, h, "GET", "/api/auth/keys", "admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.Token) {
		t.Fatal("key listing leaked the plaintext token")
	}
	var list struct {
		Keys []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 1 || list.Keys[0].ID != created.ID || list.Keys[0].Name != "ci bot" || list.Keys[0].Prefix != created.Prefix {
		t.Fatalf("unexpected key list: %+v", list)
	}

	// The token authenticates as an API-key principal.
	p := s.Principal(bearerReq(t, s, "GET", "/api/people", created.Token))
	if !p.Authed || p.Username != "admin" || !p.ViaKey || !p.Admin {
		t.Fatalf("bearer rk_ principal = %+v", p)
	}

	// Owner-scoped delete by id.
	rec = doJSONAuthed(t, s, h, "DELETE", "/api/auth/keys/"+created.ID, "admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete key: got %d (%s)", rec.Code, rec.Body.String())
	}
	// Gone: the token no longer resolves, the list is empty, re-delete 404s.
	if p := s.Principal(bearerReq(t, s, "GET", "/api/people", created.Token)); p.Authed {
		t.Fatal("deleted token must stop resolving")
	}
	rec = doJSONAuthed(t, s, h, "GET", "/api/auth/keys", "admin", nil)
	list.Keys = nil
	json.NewDecoder(rec.Body).Decode(&list)
	if len(list.Keys) != 0 {
		t.Fatalf("keys not emptied: %+v", list.Keys)
	}
	rec = doJSONAuthed(t, s, h, "DELETE", "/api/auth/keys/"+created.ID, "admin", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: got %d, want 404", rec.Code)
	}
}

func TestAPIKeysOwnerScoped(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	if err := s.CreateUser("carol", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	tokenB, keyB, err := s.users.CreateKey("carol", "carols key")
	if err != nil {
		t.Fatal(err)
	}

	// Admin's session cannot delete carol's key by ID: 404, and the key
	// keeps working.
	rec := doJSONAuthed(t, s, s.APIKeysHandler(), "DELETE", "/api/auth/keys/"+keyB.ID, "admin", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete (admin session): got %d, want 404", rec.Code)
	}
	if p := s.Principal(bearerReq(t, s, "GET", "/api/people", tokenB)); !p.Authed || p.Username != "carol" {
		t.Fatalf("carol's key must keep working: %+v", p)
	}

	// Carol's own session deletes it.
	rec = doJSONAuthed(t, s, s.APIKeysHandler(), "DELETE", "/api/auth/keys/"+keyB.ID, "carol", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	if p := s.Principal(bearerReq(t, s, "GET", "/api/people", tokenB)); p.Authed {
		t.Fatal("deleted key must stop authenticating")
	}
}

func TestChangePassword(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	if err := s.CreateUser("carol", "old-password", false); err != nil {
		t.Fatal(err)
	}
	h := s.ChangePasswordHandler()

	// Wrong current password → 403.
	rec := doJSONAuthed(t, s, h, "POST", "/api/auth/password", "carol", map[string]string{"current": "wrong-pass", "new": "new-password"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current: got %d, want 403", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg != "current password is incorrect" {
		t.Fatalf("error body = %q", msg)
	}

	// Short new password → 400.
	rec = doJSONAuthed(t, s, h, "POST", "/api/auth/password", "carol", map[string]string{"current": "old-password", "new": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short new password: got %d, want 400", rec.Code)
	}

	// Success → 200; old password stops logging in, new one works.
	rec = doJSONAuthed(t, s, h, "POST", "/api/auth/password", "carol", map[string]string{"current": "old-password", "new": "new-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("change: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, s.LoginHandler(), "POST", "/api/login", loginJSON("carol", "old-password"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password after change: got %d, want 401", rec.Code)
	}
	rec = doJSON(t, s.LoginHandler(), "POST", "/api/login", loginJSON("carol", "new-password"))
	if rec.Code != http.StatusOK {
		t.Fatalf("new password after change: got %d, want 200", rec.Code)
	}

	// An API-key principal may change the password too.
	keyToken, _, err := s.users.CreateKey("carol", "k")
	if err != nil {
		t.Fatal(err)
	}
	rec = doJSONAuthed(t, s, h, "POST", "/api/auth/password", "carol", map[string]string{"current": "new-password", "new": "key-path-pw"})
	if rec.Code != http.StatusOK {
		t.Fatalf("session-principal change: got %d (%s)", rec.Code, rec.Body.String())
	}
	b, _ := json.Marshal(map[string]string{"current": "key-path-pw", "new": "final-password"})
	req := bearerReq(t, s, "POST", "/api/auth/password", keyToken)
	req.Body = io_NopReader(strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("api-key principal change: got %d (%s)", rec2.Code, rec2.Body.String())
	}
	rec = doJSON(t, s.LoginHandler(), "POST", "/api/login", loginJSON("carol", "final-password"))
	if rec.Code != http.StatusOK {
		t.Fatalf("password changed via key principal must stick: got %d", rec.Code)
	}

	// Anonymous → 401.
	rec = doJSON(t, h, "POST", "/api/auth/password", map[string]string{"current": "x", "new": "long-enough"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous change: got %d, want 401", rec.Code)
	}
}

func TestDeleteUserKillsSessionsAndKeys(t *testing.T) {
	s := newTestService(t, "sekret-pw")
	if err := s.CreateUser("carol", "long-enough", false); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.users.CreateKey("carol", "k")
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, _ := s.sessions.New("carol")
	// carol's session authenticates before deletion.
	req := newTestRequest("GET", "/api/people")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: sessionToken})
	if p := s.Principal(req); !p.Authed {
		t.Fatal("precondition: carol's session must authenticate")
	}

	deleted, err := s.DeleteUser("admin", "carol")
	if err != nil || !deleted {
		t.Fatalf("DeleteUser = %v, %v; want true, nil", deleted, err)
	}
	// Sessions die...
	req = newTestRequest("GET", "/api/people")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: sessionToken})
	if p := s.Principal(req); p.Authed {
		t.Fatalf("deleted user's session principal = %+v", p)
	}
	// ...keys die (cascade)...
	if _, ok := s.users.LookupKey(token); ok {
		t.Fatal("deleted user's api key must be cascaded away")
	}
	if keys := s.users.ListKeys("carol"); len(keys) != 0 {
		t.Fatalf("cascaded keys remain: %+v", keys)
	}
	// ...and a second delete is a no-op.
	if deleted, err := s.DeleteUser("admin", "carol"); deleted || err != nil {
		t.Fatalf("second delete = %v, %v; want false, nil", deleted, err)
	}
}
