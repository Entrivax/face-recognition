package users

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// phcShape matches the argon2id PHC string HashPassword emits: version 19,
// the module's default costs, a 16-byte salt (22 unpadded base64 chars) and
// a 32-byte digest (43 chars), both unpadded standard base64.
var phcShape = regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)

func TestHashPasswordRoundtrip(t *testing.T) {
	const pw = "correct-horse-battery"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !phcShape.MatchString(hash) {
		t.Fatalf("hash is not the expected argon2id PHC format: %q", hash)
	}
	if !VerifyPassword(hash, pw) {
		t.Fatal("matching password must verify against its hash")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("wrong password must not verify")
	}
	if VerifyPassword(hash, pw+"x") {
		t.Fatal("extended password must not verify")
	}

	// Every hash carries a fresh random salt, so hashing the same password
	// twice must produce different encodings (both still verify).
	other, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("second HashPassword: %v", err)
	}
	if other == hash {
		t.Fatal("two hashes of one password must differ (random salt)")
	}
	if !VerifyPassword(other, pw) {
		t.Fatal("second hash must verify too")
	}

	// The hash must not contain the plaintext in any encoding.
	for _, enc := range []string{pw, base64.StdEncoding.EncodeToString([]byte(pw)), base64.RawURLEncoding.EncodeToString([]byte(pw))} {
		if strings.Contains(hash, enc) {
			t.Fatal("hash must not leak the password (raw or base64)")
		}
	}
}

func TestHashPasswordLengthRejection(t *testing.T) {
	for _, pw := range []string{"", "x", "1234567", strings.Repeat("a", 7)} {
		if _, err := HashPassword(pw); err == nil || !strings.Contains(err.Error(), "at least 8 characters") {
			t.Fatalf("HashPassword(%q) = %v, want the minimum-length error", pw, err)
		}
	}
	// 8 characters is accepted (boundary).
	if _, err := HashPassword("12345678"); err != nil {
		t.Fatalf("8-character password rejected: %v", err)
	}
}

func TestVerifyPasswordRejectsBadHashes(t *testing.T) {
	good, err := HashPassword("long-enough")
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"",                                      // unset/lost hash
		"change-me",                             // plaintext instead of a hash
		"$argon2i$v=19$m=65536,t=2,p=1$abc$abc", // wrong scheme variant
		"$bcrypt$10$abc",                        // unsupported scheme
		"$argon2id$v=16$m=65536,t=2,p=1$" + b64("salt") + "$" + b64("hash"), // wrong version
		"$argon2id$v=19$m=0,t=2,p=1$" + b64("salt") + "$" + b64("hash"),     // zero cost
		"$argon2id$v=19$m=65536,t=2$" + b64("salt") + "$" + b64("hash"),     // missing p
		"$argon2id$v=19$m=65536,t=2,p=1$!!!$" + b64("hash"),                 // undecodable salt
		"$argon2id$v=19$m=65536,t=2,p=1$" + b64("salt") + "$!!",             // undecodable hash
		"not-a-hash",
	}
	for _, b := range bad {
		if VerifyPassword("long-enough", b) {
			t.Fatalf("malformed stored hash must not verify: %q", b)
		}
		if VerifyPassword("long-enough", b+"trailing") {
			t.Fatalf("tampered hash must not verify: %q", b+"trailing")
		}
	}
	// The well-formed hash itself must still verify (guards against the
	// rejection list above being too eager).
	if !VerifyPassword(good, "long-enough") {
		t.Fatal("control: well-formed hash must verify")
	}
}

// b64 encodes s the way PHC strings do (unpadded standard base64).
func b64(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }
