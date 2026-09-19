package auth

// SECURITY-REVIEW.md M5: deleting the only registered passkey while no
// password hash is configured silently flipped the server to open mode —
// Enabled() re-evaluated false, the admin middleware became a pass-through,
// and nothing logged the change. The fix refuses the deletion with 409: the
// deliberate reset path (deleting data/passkeys.json) stays available, but no
// HTTP request can silently turn a secured server public.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPasskeyDeleteLastCredentialRefused pins the refusal: in passkey-only
// mode the only credential cannot be removed over HTTP, the store is
// unchanged, and the refusal is logged per attempt.
func TestPasskeyDeleteLastCredentialRefused(t *testing.T) {
	s := newTestService(t, "") // no password hash
	s.store.add(sampleCredential("only"))
	id := base64URL("only")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	req := newTestRequest("DELETE", "/api/auth/passkeys/"+id)
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete last credential: got %d (%s), want 409", rec.Code, rec.Body.String())
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if !strings.Contains(body["error"], "RECOGN_ADMIN_PASSWORD_HASH") ||
		!strings.Contains(body["error"], "passkeys.json") {
		t.Fatalf("error body must name both escape hatches, got %q", body["error"])
	}
	if s.store.count() != 1 {
		t.Fatal("the last credential must survive a refused deletion")
	}
	if n := strings.Count(buf.String(), "last admin credential"); n != 1 {
		t.Fatalf("refusal warning logged %d times, want once", n)
	}
}

// TestPasskeyDeleteWithTwoCredentialsAllowed: with a second credential
// remaining, deleting one is normal credential management — service stays
// enabled, no warning.
func TestPasskeyDeleteWithTwoCredentialsAllowed(t *testing.T) {
	s := newTestService(t, "")
	s.store.add(sampleCredential("k1"))
	s.store.add(sampleCredential("k2"))

	rec := httptest.NewRecorder()
	req := newTestRequest("DELETE", "/api/auth/passkeys/"+base64URL("k1"))
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete one of two: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if s.store.count() != 1 || !s.Enabled() {
		t.Fatalf("one credential must remain: count=%d enabled=%v", s.store.count(), s.Enabled())
	}
}

// TestPasskeyDeleteLastPasskeyWithPasswordAllowed pins the non-regression:
// with a password hash configured, removing the last passkey is fine —
// password login still gates admin.
func TestPasskeyDeleteLastPasskeyWithPasswordAllowed(t *testing.T) {
	s := newTestService(t, "sekret") // password hash configured
	s.store.add(sampleCredential("k1"))

	rec := httptest.NewRecorder()
	req := newTestRequest("DELETE", "/api/auth/passkeys/"+base64URL("k1"))
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete last passkey with password: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if s.store.count() != 0 || !s.Enabled() {
		t.Fatalf("password must keep the service enabled: count=%d enabled=%v", s.store.count(), s.Enabled())
	}
}

// TestPasskeyDeleteUnknownIDStill404: the 409 guard must not mask the
// not-found case when the store has other credentials.
func TestPasskeyDeleteUnknownIDStill404(t *testing.T) {
	s := newTestService(t, "")
	s.store.add(sampleCredential("k1"))
	s.store.add(sampleCredential("k2"))

	rec := httptest.NewRecorder()
	req := newTestRequest("DELETE", "/api/auth/passkeys/"+base64URL("nope"))
	s.PasskeyListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown id: got %d (%s), want 404", rec.Code, rec.Body.String())
	}
}
