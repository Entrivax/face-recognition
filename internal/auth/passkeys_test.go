package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// sampleCredential builds a plausible stored credential.
func sampleCredential(id string) webauthn.Credential {
	return webauthn.Credential{
		ID:              []byte(id),
		PublicKey:       []byte("public-key-bytes-" + id),
		AttestationType: "none",
		Transport:       []protocol.AuthenticatorTransport{protocol.Internal},
		Authenticator:   webauthn.Authenticator{AAGUID: make([]byte, 16), SignCount: 7},
	}
}

func TestPasskeyStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passkeys.json")
	s, err := loadPasskeyStore(path, nil)
	if err != nil {
		t.Fatalf("missing file must be an empty store, got error: %v", err)
	}
	if s.count() != 0 {
		t.Fatal("missing file must yield an empty store")
	}

	cred := sampleCredential("cred-1")
	if err := s.add(cred); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Re-adding the same credential id must not duplicate it.
	if err := s.add(sampleCredential("cred-1")); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("duplicate credential stored, count=%d", s.count())
	}

	// File exists with 0600 and round-trips through a fresh store.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("store file missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v, want 0600", fi.Mode().Perm())
	}
	reloaded, err := loadPasskeyStore(path, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	creds := reloaded.credentials()
	if len(creds) != 1 || string(creds[0].ID) != "cred-1" || creds[0].Authenticator.SignCount != 7 {
		t.Fatalf("round-trip mismatch: %+v", creds)
	}
}

func TestPasskeyStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "passkeys.json")
	s, err := loadPasskeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := s.add(sampleCredential(id)); err != nil {
			t.Fatal(err)
		}
	}
	// No temp files left behind, only the store itself.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "passkeys.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("atomic write left files behind: %v", names)
	}
}

func TestPasskeyStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "passkeys.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPasskeyStore(path, nil); err == nil {
		t.Fatal("a corrupt passkeys.json must fail loudly (mirrors the DB's embeddings.json handling)")
	}
}

func TestPasskeyStoreRemove(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	dir := t.TempDir()
	path := filepath.Join(dir, "passkeys.json")
	s, err := loadPasskeyStore(path, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.add(sampleCredential("keep-me"))
	_ = s.add(sampleCredential("drop-me"))

	// Unknown id → not removed.
	removed, err := s.removeByID(base64URL("nope"))
	if err != nil || removed {
		t.Fatalf("unknown id: removed=%v err=%v, want false/nil", removed, err)
	}
	removed, err = s.removeByID(base64URL("drop-me"))
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	if s.count() != 1 {
		t.Fatalf("count after remove = %d, want 1", s.count())
	}
	reloaded, err := loadPasskeyStore(path, now)
	if err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if reloaded.count() != 1 || string(reloaded.credentials()[0].ID) != "keep-me" {
		t.Fatalf("removal not persisted: %+v", reloaded.credentials())
	}
}

func TestPasskeyStoreUpdateSignCount(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s, err := loadPasskeyStore(filepath.Join(t.TempDir(), "passkeys.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.add(sampleCredential("u1"))
	cred := sampleCredential("u1")
	cred.Authenticator.SignCount = 99
	s.update(cred)
	if got := s.credentials()[0].Authenticator.SignCount; got != 99 {
		t.Fatalf("sign count not updated: %d", got)
	}
	s.update(sampleCredential("unknown")) // no-op
	if s.count() != 1 {
		t.Fatal("update must not add credentials")
	}
}

func TestPasskeyRecordJSONShape(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s, err := loadPasskeyStore(filepath.Join(t.TempDir(), "passkeys.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.add(sampleCredential("shape")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatalf("store is not a JSON array of records: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("unexpected record count %d", len(recs))
	}
	rec := recs[0]
	if _, ok := rec["credential"]; !ok {
		t.Fatalf("record must carry the webauthn credential, keys=%v", rec)
	}
	if _, ok := rec["added_at"]; !ok {
		t.Fatalf("record must carry added_at, keys=%v", rec)
	}
}

func TestPendingCeremoniesTTL(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	p := newPendingCeremonies(now)

	p.put(&webauthn.SessionData{Challenge: "ch-1"})
	if _, ok := p.take("ch-1"); !ok {
		t.Fatal("a fresh ceremony must be retrievable exactly once")
	}
	if _, ok := p.take("ch-1"); ok {
		t.Fatal("a ceremony is single-use")
	}

	p.put(&webauthn.SessionData{Challenge: "ch-2"})
	advance(ceremonyTTL + time.Second)
	if _, ok := p.take("ch-2"); ok {
		t.Fatal("an expired ceremony must not be retrievable")
	}
	if _, ok := p.take("unknown"); ok {
		t.Fatal("unknown challenge must not resolve")
	}
	if len(p.sessions) != 0 {
		t.Fatalf("expired ceremonies not purged: %d remain", len(p.sessions))
	}
}

// TestRPForRequest covers the per-request RPID/origin derivation and the env
// overrides. (Set via t.Setenv in api package tests too; here directly.)
func TestRPForRequest(t *testing.T) {
	cfg := newTestConfig(t)
	s := &Service{cfg: cfg} // rpForRequest only reads cfg + the request

	req := newTestRequest("POST", "/api/auth/passkey/login/begin")
	req.Host = "recogn.local:8443"
	rpid, origin := s.rpForRequest(req)
	if rpid != "recogn.local" {
		t.Fatalf("RPID = %q, want host without port", rpid)
	}
	if origin != "http://recogn.local:8443" {
		t.Fatalf("origin = %q, want scheme from X-Forwarded-Proto + Host", origin)
	}

	req.Header.Set("X-Forwarded-Proto", "https")
	_, origin = s.rpForRequest(req)
	if origin != "https://recogn.local:8443" {
		t.Fatalf("origin behind TLS proxy = %q", origin)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"example.com:8443": "example.com",
		"localhost":        "localhost",
		"localhost:8080":   "localhost",
		"":                 "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
