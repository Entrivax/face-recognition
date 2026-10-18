package auth

// Setup flow tests: a zero-user install generates a one-time setup key (in
// memory only) that POST /api/auth/setup exchanges for the first ADMIN
// account. Wrong keys are refused (403) and rate-limited; a second setup is
// refused forever; every restart mints a fresh key while no users exist.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// setupKeyShape: four groups of four RFC4648 base32 characters
// (uppercase letters + digits 2-7, no look-alikes), dash-separated.
var setupKeyShape = regexp.MustCompile(`^[A-Z2-7]{4}(-[A-Z2-7]{4}){3}$`)

// setupPost POSTs one setup attempt and returns the recorder.
func setupPost(t *testing.T, s *Service, key, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"key": key, "username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/auth/setup", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.SetupHandler().ServeHTTP(rec, req)
	return rec
}

func TestSetupKeyGeneratedWhenNoUsers(t *testing.T) {
	s := newTestService(t, "")
	if !s.SetupRequired() || s.Mode() != "setup" {
		t.Fatalf("zero-user install: mode=%q setup_required=%v", s.Mode(), s.SetupRequired())
	}
	key, ok := s.SetupKey()
	if !ok {
		t.Fatal("a zero-user install must have a setup key")
	}
	if !setupKeyShape.MatchString(key) {
		t.Fatalf("setup key %q does not match XXXX-XXXX-XXXX-XXXX in RFC4648 base32", key)
	}
	if len(key) != 19 { // 16 chars + 3 dashes
		t.Fatalf("setup key length = %d, want 19", len(key))
	}
}

func TestSetupSuccessCreatesAdminAndConsumesKey(t *testing.T) {
	s := newTestService(t, "")
	key, _ := s.SetupKey()

	rec := setupPost(t, s, key, "root", "bootstrappw")
	if rec.Code != 200 {
		t.Fatalf("setup: got %d (%s)", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	json.NewDecoder(rec.Body).Decode(&ok)
	if !ok.OK {
		t.Fatalf("setup body = %s", rec.Body.String())
	}

	// The key is consumed.
	if _, still := s.SetupKey(); still {
		t.Fatal("setup key must be consumed by a successful setup")
	}
	// The created account is a real admin and can log in.
	if s.SetupRequired() || s.Mode() != "users" {
		t.Fatalf("after setup: mode=%q setup_required=%v", s.Mode(), s.SetupRequired())
	}
	usersList := s.Users()
	if len(usersList) != 1 || usersList[0].Username != "root" || !usersList[0].Admin {
		t.Fatalf("setup must create an ADMIN account, got %+v", usersList)
	}
	rec = doJSON(t, s.LoginHandler(), "POST", "/api/login", loginJSON("root", "bootstrappw"))
	if rec.Code != 200 {
		t.Fatalf("the setup-created admin must be able to log in: %d (%s)", rec.Code, rec.Body.String())
	}

	// A second setup is refused even with the (now stale) original key.
	rec = setupPost(t, s, key, "intruder", "whatever-pw")
	if rec.Code != 403 {
		t.Fatalf("second setup: got %d, want 403", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg != "setup already completed" {
		t.Fatalf("second-setup error body = %q", msg)
	}
	if len(s.Users()) != 1 {
		t.Fatal("the refused setup must not create anything")
	}
}

func TestSetupWrongKeyRefused(t *testing.T) {
	s := newTestService(t, "")
	key, _ := s.SetupKey()

	rec := setupPost(t, s, "AAAA-BBBB-CCCC-DDDD", "root", "bootstrappw")
	if rec.Code != 403 {
		t.Fatalf("wrong key: got %d, want 403", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg != "invalid setup key" {
		t.Fatalf("wrong-key error body = %q", msg)
	}
	// Nothing created, key not consumed by failures.
	if !s.SetupRequired() {
		t.Fatal("a refused setup must not create the admin")
	}
	if again, ok := s.SetupKey(); !ok || again != key {
		t.Fatalf("failed attempts must not rotate the key: %q (was %q)", again, key)
	}
}

func TestSetupKeyNormalization(t *testing.T) {
	// The compare normalizes trim + strip spaces/dashes + uppercase, so
	// sloppy transcriptions still match.
	normalizers := []struct {
		name string
		fn   func(string) string
	}{
		{"exact", func(k string) string { return k }},
		{"lowercase", strings.ToLower},
		{"spaces for dashes", func(k string) string { return strings.ReplaceAll(k, "-", " ") }},
		{"compact lowercase", func(k string) string { return strings.ReplaceAll(strings.ToLower(k), "-", "") }},
		{"padded", func(k string) string { return "  " + k + "  " }},
	}
	for _, norm := range normalizers {
		t.Run(norm.name, func(t *testing.T) {
			s := newTestService(t, "")
			key, ok := s.SetupKey()
			if !ok {
				t.Fatal("missing setup key")
			}
			rec := setupPost(t, s, norm.fn(key), "root", "bootstrappw")
			if rec.Code != 200 {
				t.Fatalf("variant: got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSetupValidationErrors(t *testing.T) {
	s := newTestService(t, "")
	key, _ := s.SetupKey()

	// Short password (rejected by the users store).
	rec := setupPost(t, s, key, "root", "short")
	if rec.Code != 400 {
		t.Fatalf("short password: got %d, want 400", rec.Code)
	}
	if msg := errBodyOf(t, rec); msg == "" {
		t.Fatal("validation errors must carry a message")
	}
	// Invalid username shape.
	rec = setupPost(t, s, key, ".bad", "bootstrappw")
	if rec.Code != 400 {
		t.Fatalf("bad username: got %d, want 400", rec.Code)
	}
	// Nothing was created and the key still works afterwards.
	if len(s.Users()) != 0 {
		t.Fatal("rejected setups must not create accounts")
	}
	rec = setupPost(t, s, key, "root", "bootstrappw")
	if rec.Code != 200 {
		t.Fatalf("key must survive validation failures: got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSetupBadJSONAndMethod(t *testing.T) {
	s := newTestService(t, "")
	rec := httptest.NewRecorder()
	s.SetupHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/setup", bytes.NewReader([]byte("not json"))))
	if rec.Code != 400 {
		t.Fatalf("bad JSON: got %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.SetupHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/setup", nil))
	if rec.Code != 405 {
		t.Fatalf("GET setup: got %d, want 405", rec.Code)
	}
}

func TestSetupKeyFreshPerService(t *testing.T) {
	cfg := newTestConfig(t)
	s1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key1, ok := s1.SetupKey()
	if !ok {
		t.Fatal("first service must have a setup key")
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	// Same directory, zero users: the new process (here: new Service) mints
	// a NEW key rather than reusing the old one.
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	key2, ok := s2.SetupKey()
	if !ok {
		t.Fatal("second service must have a setup key")
	}
	if key1 == key2 {
		t.Fatalf("setup keys must differ per service: %q == %q", key1, key2)
	}
	if !setupKeyShape.MatchString(key2) {
		t.Fatalf("second key malformed: %q", key2)
	}
}

func TestSetupRateLimited(t *testing.T) {
	s := newTestService(t, "")
	key, _ := s.SetupKey()

	for i := 0; i < 11; i++ {
		rec := setupPost(t, s, "XXXX-XXXX-XXXX-XXXX", "root", "bootstrappw")
		if rec.Code != 403 {
			t.Fatalf("wrong-key attempt %d: got %d, want 403", i+1, rec.Code)
		}
	}
	rec := setupPost(t, s, key, "root", "bootstrappw")
	if rec.Code != 429 {
		t.Fatalf("locked setup: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 must carry Retry-After")
	}

	// A different IP can still complete the bootstrap.
	req := httptest.NewRequest("POST", "/api/auth/setup",
		strings.NewReader(`{"key":"`+key+`","username":"root","password":"bootstrappw"}`))
	req.RemoteAddr = "10.0.0.9:5555"
	rec2 := httptest.NewRecorder()
	s.SetupHandler().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("other-IP setup: got %d (%s)", rec2.Code, rec2.Body.String())
	}
}
