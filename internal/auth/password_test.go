package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const pw = "correct-horse-battery-staple"
	hash, err := HashPassword([]byte(pw))
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("hash is not argon2id PHC: %q", hash)
	}
	if !verifyPassword(pw, hash) {
		t.Fatal("matching password must verify against its hash")
	}
	if verifyPassword("wrong", hash) {
		t.Fatal("wrong password must not verify")
	}
	if verifyPassword(pw+"x", hash) {
		t.Fatal("extended password must not verify")
	}

	// Every hash carries a fresh random salt, so hashing the same password
	// twice must produce different encodings (both still verify).
	other, err := HashPassword([]byte(pw))
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if other == hash {
		t.Fatal("two hashes of one password must differ (random salt)")
	}
	if !verifyPassword(pw, other) {
		t.Fatal("second hash must verify too")
	}

	// The hash must not contain the plaintext in any encoding.
	for _, enc := range []string{pw, base64.StdEncoding.EncodeToString([]byte(pw)), base64.RawURLEncoding.EncodeToString([]byte(pw))} {
		if strings.Contains(hash, enc) {
			t.Fatal("hash must not leak the password (raw or base64)")
		}
	}
}

func TestVerifyPasswordRejectsBadHashes(t *testing.T) {
	good, err := HashPassword([]byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"",                      // unset: never matches (Enabled gate decides)
		"change-me",             // plaintext instead of a hash
		"$argon2i$v=19$m=65536,t=2,p=1$abc$abc", // wrong scheme variant
		"$bcrypt$10$abc",        // unsupported scheme
		"$argon2id$v=16$m=65536,t=2,p=1$" + b64("salt") + "$" + b64("hash"), // wrong version
		"$argon2id$v=19$m=0,t=2,p=1$" + b64("salt") + "$" + b64("hash"),     // zero cost
		"$argon2id$v=19$m=65536,t=2$" + b64("salt") + "$" + b64("hash"),     // missing p
		"$argon2id$v=19$m=65536,t=2,p=1$!!!$" + b64("hash"),                 // undecodable salt
		"$argon2id$v=19$m=65536,t=2,p=1$" + b64("salt") + "$!!",             // undecodable hash
		"not-a-hash",
	}
	for _, b := range bad {
		if verifyPassword("pw", b) {
			t.Fatalf("malformed configured hash must not verify: %q", b)
		}
		if verifyPassword("pw", b+"trailing") {
			t.Fatalf("tampered hash must not verify: %q", b+"trailing")
		}
	}
	// The well-formed hash itself must still verify (guards against the
	// rejection list above being too eager).
	if !verifyPassword("pw", good) {
		t.Fatal("control: well-formed hash must verify")
	}
}

// b64 encodes s the way PHC strings do (unpadded standard base64).
func b64(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }

func TestVerifyPasswordEmptyConfigured(t *testing.T) {
	if verifyPassword("anything", "") {
		t.Fatal("empty configured hash must never verify (password auth disabled)")
	}
}
