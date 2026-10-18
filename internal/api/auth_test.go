package api

// Tests for the session/account surface: the session probe, the first-run
// setup flow, login/logout, the personal API keys endpoints, the self-
// service password change, the admin users endpoints and the config
// endpoint's role split. Everything runs through the real routes; the auth
// service's internals are covered by internal/auth's own tests.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"recogn/internal/users"
)

// TestSessionProbe pins GET /api/auth/session for both postures: anonymous
// ({"authenticated":false,"user":null,...}) and logged-in (the user block).
func TestSessionProbe(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{}) // zero users: setup mode

	// Anonymous: not authenticated; a fresh server with no users still
	// requires the setup.
	var session struct {
		Authenticated bool           `json:"authenticated"`
		User          map[string]any `json:"user"`
		SetupRequired bool           `json:"setup_required"`
	}
	rec := get(t, s, nil, "/api/auth/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session probe: got %d (%s)", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &session)
	if session.Authenticated || session.User != nil || !session.SetupRequired {
		t.Fatalf("anonymous session shape: %+v", session)
	}

	// After the admin exists: authenticated with the user block.
	cookie := createAdmin(t, s)
	rec = get(t, s, cookie, "/api/auth/session")
	decodeBody(t, rec, &session)
	if !session.Authenticated || session.SetupRequired {
		t.Fatalf("authed session shape: %+v", session)
	}
	if session.User["name"] != adminName || session.User["admin"] != true {
		t.Errorf("user block: %v", session.User)
	}
}

// TestSetupFlow covers the first-run bootstrap: the in-memory setup key,
// POST /api/setup creating the admin, and the endpoint refusing once an
// admin exists (or on a wrong key).
func TestSetupFlow(t *testing.T) {
	s, _ := newTestServer(t, &stubEngine{}) // zero users: setup mode

	key, ok := s.auth.SetupKey()
	if !ok || key == "" {
		t.Fatal("a fresh server must hold an in-memory setup key")
	}
	if s.auth.Mode() != "setup" {
		t.Fatalf("mode = %q, want setup", s.auth.Mode())
	}

	// Wrong key → 403, no user created.
	rec := sendJSON(t, s, nil, "/api/setup", map[string]string{"key": "AAAA-BBBB-CCCC-DDDD", "username": "admin", "password": adminPass})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong key: got %d (%s), want 403", rec.Code, rec.Body.String())
	}
	if s.auth.SetupRequired() != true {
		t.Fatalf("failed setup must not create an account")
	}

	// The right key → 200 {"ok":true}; the account can log in immediately.
	rec = sendJSON(t, s, nil, "/api/setup", map[string]string{"key": key, "username": "admin", "password": adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if body["ok"] != true {
		t.Errorf("setup body = %v", body)
	}
	if s.auth.SetupRequired() {
		t.Fatalf("setup should be complete")
	}

	// Once completed the endpoint refuses — even with the right key.
	rec = sendJSON(t, s, nil, "/api/setup", map[string]string{"key": key, "username": "second", "password": adminPass})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup after completion: got %d (%s), want 403", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &body)
	if !strings.Contains(body["error"].(string), "already completed") {
		t.Errorf("error body = %q", body["error"])
	}

	// GET /api/setup is not a route (the handler requires POST).
	if rec := get(t, s, nil, "/api/setup"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET setup: got %d, want 405", rec.Code)
	}
}

// TestLoginLogoutFlow covers the session lifecycle through the real routes.
func TestLoginLogoutFlow(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "", false, false)
	_ = createAdmin(t, e.s)

	// Wrong password → 401 with the uniform message (never "which field").
	rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": adminName, "password": "wrong-password"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] != "invalid username or password" {
		t.Fatalf("error body = %q", body["error"])
	}

	// Right password → 200, cookie with sane attributes.
	rec = sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": adminName, "password": adminPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", rec.Code, rec.Body.String())
	}
	var login struct {
		OK   bool `json:"ok"`
		User struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"user"`
	}
	decodeBody(t, rec, &login)
	if !login.OK || login.User.Name != adminName || !login.User.Admin {
		t.Errorf("login body: %+v", login)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("unexpected cookie %+v", c)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", c.SameSite)
	}

	// The cookie authorizes an any-auth route.
	if rec := get(t, e.s, c, "/api/config"); rec.Code != http.StatusOK {
		t.Fatalf("cookie on /api/config: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Logout clears the session server-side.
	rec = sendJSON(t, e.s, c, "/api/logout", map[string]any{})
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
	if rec := get(t, e.s, c, "/api/config"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale session after logout: got %d, want 401", rec.Code)
	}
}

// TestAPIKeysEndpoints covers the owner's API keys: create (token shown
// exactly once), list, use (a rk_ Bearer acts as the user), delete, and the
// refusals.
func TestAPIKeysEndpoints(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)

	// Anonymous → 401.
	if rec := get(t, e.s, nil, "/api/auth/keys"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon keys list: got %d, want 401", rec.Code)
	}

	// Create: the plaintext token appears exactly once.
	rec := sendJSON(t, e.s, e.writer, "/api/auth/keys", map[string]string{"name": "ci script"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create key: got %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		Token   string `json:"token"`
		Prefix  string `json:"prefix"`
		Name    string `json:"name"`
		Created string `json:"created_at"`
	}
	decodeBody(t, rec, &created)
	if created.Token == "" || !strings.HasPrefix(created.Token, "rk_") || created.ID == "" {
		t.Fatalf("unexpected key response: %+v", created)
	}
	if created.Name != "ci script" || !strings.Contains(created.Prefix, "rk_") {
		t.Errorf("key metadata: %+v", created)
	}
	if _, err := time.Parse(time.RFC3339, created.Created); err != nil {
		t.Errorf("created_at %q is not RFC3339", created.Created)
	}

	// List: the key is there, the token is not.
	var list struct {
		Keys []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
			Token  string `json:"token"`
		} `json:"keys"`
	}
	decodeBody(t, get(t, e.s, e.writer, "/api/auth/keys"), &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != created.ID {
		t.Fatalf("keys list: %+v", list)
	}
	if list.Keys[0].Token != "" {
		t.Errorf("the list must never carry the token")
	}

	// The key acts as its user: authorized for the user's repos, not admin.
	req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec2 := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("rk_ bearer on /api/repos: got %d (%s)", rec2.Code, rec2.Body.String())
	}
	var repos struct {
		Repos []struct {
			Role string `json:"role"`
		} `json:"repos"`
	}
	decodeBody(t, rec2, &repos)
	if len(repos.Repos) != 1 || repos.Repos[0].Role != "writer" {
		t.Errorf("key acts as its user: %+v", repos.Repos)
	}
	// ...but not an admin: /api/users → 403.
	req = httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec2 = httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("rk_ bearer on /api/users: got %d, want 403", rec2.Code)
	}

	// Delete: own key works, unknown id → 404.
	if rec := del(t, e.s, e.writer, "/api/auth/keys/"+created.ID); rec.Code != http.StatusOK {
		t.Fatalf("delete key: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := del(t, e.s, e.writer, "/api/auth/keys/"+created.ID); rec.Code != http.StatusNotFound {
		t.Errorf("re-delete: got %d, want 404", rec.Code)
	}
	// The dead key stops authenticating.
	req = httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec2 = httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("deleted key must stop authenticating: got %d, want 401", rec2.Code)
	}
}

// TestChangePasswordFlow covers POST /api/auth/password: wrong current
// password → 403, success → 200, the new password logs in, the old one
// does not.
func TestChangePasswordFlow(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "", false, false)
	_ = createAdmin(t, e.s)

	// Wrong current password → 403.
	rec := sendJSON(t, e.s, e.admin, "/api/auth/password", map[string]string{"current": "not-it", "new": "new-password-9"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current: got %d (%s), want 403", rec.Code, rec.Body.String())
	}

	// Anonymous → 401.
	if rec := sendJSON(t, e.s, nil, "/api/auth/password", map[string]string{"current": "x", "new": "y"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon change: got %d, want 401", rec.Code)
	}

	// Success: 200.
	if rec := sendJSON(t, e.s, e.admin, "/api/auth/password", map[string]string{"current": adminPass, "new": "new-password-9"}); rec.Code != http.StatusOK {
		t.Fatalf("change: got %d (%s)", rec.Code, rec.Body.String())
	}

	// The old password stops working; the new one logs in.
	if rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": adminName, "password": adminPass}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: got %d, want 401", rec.Code)
	}
	if rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": adminName, "password": "new-password-9"}); rec.Code != http.StatusOK {
		t.Errorf("new password rejected: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Too-short new password → 400 with the users-store text.
	rec = sendJSON(t, e.s, e.admin, "/api/auth/password", map[string]string{"current": "new-password-9", "new": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: got %d (%s), want 400", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if !strings.Contains(body["error"], "password must be at least") {
		t.Errorf("error body = %q", body["error"])
	}
}

// TestUsersEndpointsAdminOnly pins the admin gating of the users endpoints.
func TestUsersEndpointsAdminOnly(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)

	for _, c := range []struct {
		name   string
		method string
		path   string
	}{
		{"list", http.MethodGet, "/api/users"},
		{"create", http.MethodPost, "/api/users"},
		{"delete", http.MethodDelete, "/api/users/rita"},
		{"password", http.MethodPost, "/api/users/rita/password"},
		{"grants", http.MethodPost, "/api/users/rita/repos"},
	} {
		// Anonymous → 401.
		rec := do(t, e.s, c.method, c.path, nil, "", nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anon %s: got %d, want 401", c.name, rec.Code)
		}
		// Authenticated non-admin → 403.
		rec = do(t, e.s, c.method, c.path, nil, "", e.writer, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("writer %s: got %d, want 403", c.name, rec.Code)
		}
	}
}

// TestUsersCRUD covers account creation, listing, validation and deletion
// (with the admin undeletable).
func TestUsersCRUD(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", false, false)

	// Create a plain user.
	rec := sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "Bob", "password": "bob-password-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create user: got %d (%s)", rec.Code, rec.Body.String())
	}

	// List: the admin + bob, with grants and RFC3339 timestamps.
	var list struct {
		Users []struct {
			Username  string        `json:"username"`
			Admin     bool          `json:"admin"`
			Grants    []users.Grant `json:"grants"`
			CreatedAt string        `json:"created_at"`
		} `json:"users"`
		Count int `json:"count"`
	}
	decodeBody(t, get(t, e.s, e.admin, "/api/users"), &list)
	if list.Count != 2 || len(list.Users) != 2 {
		t.Fatalf("users list: %+v", list)
	}
	if list.Users[0].Username != "admin" || !list.Users[0].Admin {
		t.Errorf("admin entry: %+v", list.Users[0])
	}
	if list.Users[1].Username != "bob" || list.Users[1].Admin {
		t.Errorf("bob entry: %+v", list.Users[1]) // stored lowercase
	}
	if _, err := time.Parse(time.RFC3339, list.Users[1].CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC3339", list.Users[1].CreatedAt)
	}

	// Validation errors carry the users store's text.
	if rec := sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "Bob", "password": "bob-password-1"}); rec.Code != http.StatusConflict {
		t.Errorf("taken username: got %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "x", "password": "long-enough-pass"}); rec.Code != http.StatusBadRequest {
		t.Errorf("short username: got %d, want 400", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "bob 2", "password": "long-enough-pass"}); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid username: got %d, want 400", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "carol", "password": "short"}); rec.Code != http.StatusBadRequest {
		t.Errorf("short password: got %d, want 400", rec.Code)
	}
	var errBody map[string]string
	decodeBody(t, sendJSON(t, e.s, e.admin, "/api/users", map[string]any{"username": "carol", "password": "short"}), &errBody)
	if !strings.Contains(errBody["error"], "password must be at least") {
		t.Errorf("error body = %q", errBody["error"])
	}

	// Delete: the admin account is protected.
	rec = del(t, e.s, e.admin, "/api/users/admin")
	if rec.Code != http.StatusForbidden {
		t.Errorf("delete admin: got %d (%s), want 403", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] != "the administrator account cannot be deleted" {
		t.Errorf("admin-delete error = %q", body["error"])
	}
	// Unknown user → 404; the deleted user's login stops working.
	if rec := del(t, e.s, e.admin, "/api/users/nobody"); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown: got %d, want 404", rec.Code)
	}
	if rec := del(t, e.s, e.admin, "/api/users/bob"); rec.Code != http.StatusOK {
		t.Fatalf("delete bob: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": "bob", "password": "bob-password-1"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("deleted user still logs in: got %d", rec.Code)
	}
	// Wrong method → 405.
	if rec := do(t, e.s, http.MethodPost, "/api/users/bob", bytes.NewReader([]byte(`{}`)), "application/json", e.admin, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/users/{u}: got %d, want 405", rec.Code)
	}
}

// TestUserGrants covers POST /api/users/{u}/repos: full replacement,
// repository validation (ID or name) and role validation.
func TestUserGrants(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)
	other := seedRepo(t, e.s, e.admin, "Other")

	// Full replace: replace the writer's grants with reader-on-Other.
	rec := sendJSON(t, e.s, e.admin, "/api/users/walter/repos", map[string]any{
		"grants": []map[string]string{{"repo": other, "role": "reader"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set grants: got %d (%s)", rec.Code, rec.Body.String())
	}
	// The old grant is gone: walter no longer sees Team.
	var list struct {
		Users []struct {
			Username string        `json:"username"`
			Grants   []users.Grant `json:"grants"`
		} `json:"users"`
	}
	decodeBody(t, get(t, e.s, e.admin, "/api/users"), &list)
	for _, u := range list.Users {
		if u.Username != "walter" {
			continue
		}
		if len(u.Grants) != 1 || u.Grants[0].RepoID != other || u.Grants[0].Role != "reader" {
			t.Fatalf("walter grants after replace: %+v", u.Grants)
		}
	}
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", e.writer, nil); rec.Code != http.StatusForbidden {
		t.Errorf("old grant should be gone: got %d, want 403", rec.Code)
	}
	// ...and the new one applies.
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo="+other, nil, "", e.writer, nil); rec.Code != http.StatusOK {
		t.Errorf("new grant should apply: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Grants by repo NAME work too (canonicalized to the ID).
	if rec := sendJSON(t, e.s, e.admin, "/api/users/walter/repos", map[string]any{
		"grants": []map[string]string{{"repo": "Team", "role": "writer"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("grants by name: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", e.writer, nil); rec.Code != http.StatusOK {
		t.Errorf("grant by name should apply: got %d (%s)", rec.Code, rec.Body.String())
	}

	// Validation: unknown repo → 400, bad role → 400, unknown user → 404.
	if rec := sendJSON(t, e.s, e.admin, "/api/users/walter/repos", map[string]any{
		"grants": []map[string]string{{"repo": "nosuchrepo", "role": "reader"}},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown repo: got %d, want 400", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/users/walter/repos", map[string]any{
		"grants": []map[string]string{{"repo": e.repoID, "role": "owner"}},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: got %d, want 400", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/users/nobody/repos", map[string]any{
		"grants": []map[string]string{{"repo": e.repoID, "role": "reader"}},
	}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: got %d, want 404", rec.Code)
	}
}

// TestUserPasswordReset covers POST /api/users/{u}/password: the admin
// resets another user's password; validation errors carry the store text.
func TestUserPasswordReset(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)

	rec := sendJSON(t, e.s, e.admin, "/api/users/rita/password", map[string]string{"password": "rita-password-9"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: got %d (%s)", rec.Code, rec.Body.String())
	}
	// The new password works, the old one doesn't.
	if rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": "rita", "password": "rita-password-1"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: got %d", rec.Code)
	}
	if rec := sendJSON(t, e.s, nil, "/api/login", map[string]string{"username": "rita", "password": "rita-password-9"}); rec.Code != http.StatusOK {
		t.Errorf("new password rejected: got %d (%s)", rec.Code, rec.Body.String())
	}
	// Too short → 400 with the store text.
	rec = sendJSON(t, e.s, e.admin, "/api/users/rita/password", map[string]string{"password": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short: got %d (%s), want 400", rec.Code, rec.Body.String())
	}
	// Unknown user → 404.
	if rec := sendJSON(t, e.s, e.admin, "/api/users/nobody/password", map[string]string{"password": "whatever-long"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: got %d, want 404", rec.Code)
	}
}

// TestConfigRoles pins the config endpoint's split: GET for any
// authenticated principal, POST admin-only.
func TestConfigRoles(t *testing.T) {
	eng := &stubEngine{threshold: 0.45}
	e := newEnv(t, eng, "Team", true, false)

	// Anonymous GET → 401.
	if rec := get(t, e.s, nil, "/api/config"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon GET config: got %d, want 401", rec.Code)
	}
	// Reader GET → 200.
	var cfg struct {
		Threshold float64 `json:"threshold"`
	}
	if rec := get(t, e.s, e.reader, "/api/config"); rec.Code != http.StatusOK {
		t.Fatalf("reader GET config: got %d (%s)", rec.Code, rec.Body.String())
	}
	decodeBody(t, get(t, e.s, e.reader, "/api/config"), &cfg)
	if cfg.Threshold != 0.45 {
		t.Errorf("threshold = %v", cfg.Threshold)
	}
	// Reader POST → 403.
	if rec := sendJSON(t, e.s, e.reader, "/api/config", map[string]float64{"threshold": 0.6}); rec.Code != http.StatusForbidden {
		t.Errorf("reader POST config: got %d, want 403", rec.Code)
	}
	// Admin POST → 200, engine + store updated.
	if rec := sendJSON(t, e.s, e.admin, "/api/config", map[string]float64{"threshold": 0.6}); rec.Code != http.StatusOK {
		t.Fatalf("admin POST config: got %d (%s)", rec.Code, rec.Body.String())
	}
	if eng.threshold != 0.6 {
		t.Errorf("threshold not applied to the engine: %v", eng.threshold)
	}
	if e.db.Threshold() == nil || *e.db.Threshold() != 0.6 {
		t.Errorf("threshold not persisted")
	}
}
