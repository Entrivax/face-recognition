package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPasskeyRegisterRefusedInOpenMode pins the H2 fix (SECURITY-REVIEW.md):
// in open mode — no password hash configured and zero registered passkeys —
// the admin middleware passes through, which made passkey registration
// first-come-first-served: the first network peer to complete a ceremony
// owned admin permanently. Registration must be refused instead.
func TestPasskeyRegisterRefusedInOpenMode(t *testing.T) {
	s := newTestService(t, "") // open mode: Enabled() == false

	// Begin must be refused with actionable guidance, not handed a ceremony.
	rec := httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/begin"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("open-mode register begin: got %d (%s), want 403", rec.Code, rec.Body.String())
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if !strings.Contains(body["error"], "RECOGN_ADMIN_PASSWORD_HASH") {
		t.Fatalf("error body must name the env var, got %q", body["error"])
	}

	// Finish is refused before its body is even parsed (garbage body must
	// yield 403, not the parse 400).
	rec = httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/finish"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("open-mode register finish: got %d (%s), want 403", rec.Code, rec.Body.String())
	}

	// Wrong method is still a 405: the method check precedes the refusal.
	rec = httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("GET", "/api/auth/passkey/register/begin"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET register begin: got %d, want 405", rec.Code)
	}

	// Nothing was created: no credential, no pending ceremony.
	if s.store.count() != 0 {
		t.Fatal("open-mode register attempt must not create a credential")
	}
	if len(s.pending.sessions) != 0 {
		t.Fatal("open-mode register attempt must not create a ceremony")
	}
}

// TestPasskeyRegisterWarnsPerRefusedAttempt asserts the review's "per-request
// log" requirement: every refused open-mode registration attempt leaves a
// warning in the log.
func TestPasskeyRegisterWarnsPerRefusedAttempt(t *testing.T) {
	s := newTestService(t, "")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/begin"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: got %d, want 403", i+1, rec.Code)
		}
	}
	if n := strings.Count(buf.String(), "passkey registration refused in open mode"); n != 2 {
		t.Fatalf("warning logged %d times, want once per refused request (2)", n)
	}
}

// TestPasskeyRegisterAllowedAfterFirstCredential pins the non-regression half
// of H2: once any admin credential exists (here a registered passkey, i.e.
// passkey-only mode), registration is reachable again behind the admin
// middleware — the bootstrap gap is closed, not passkey management.
func TestPasskeyRegisterAllowedAfterFirstCredential(t *testing.T) {
	s := newTestService(t, "") // no password hash configured
	s.store.add(sampleCredential("k1"))
	if !s.Enabled() {
		t.Fatal("a registered passkey must enable the service")
	}
	rec := httptest.NewRecorder()
	s.PasskeyRegisterHandler().ServeHTTP(rec, newTestRequest("POST", "/api/auth/passkey/register/begin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("passkey-only register begin: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
}
