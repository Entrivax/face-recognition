package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters used by HashPassword when creating NEW hashes. The
// full parameter set travels inside the PHC string, so hashes created with
// different (e.g. stronger) parameters keep verifying transparently. The
// defaults sit well above the OWASP minimum (m=19 MiB, t=2, p=1) while
// staying cheap enough for a rate-limited login endpoint on a small server.
const (
	argon2Time    = 2
	argon2Memory  = 64 * 1024 // KiB (64 MiB)
	argon2Threads = 1
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// HashPassword derives an argon2id PHC-encoded hash of the password using a
// fresh random salt. The returned string contains no secret, only the
// parameters, salt and digest — it is what RECOGN_ADMIN_PASSWORD_HASH stores,
// so a leaked environment variable does not leak the password itself.
func HashPassword(password []byte) (string, error) {
	if len(password) == 0 {
		return "", errors.New("password is empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey(password, salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword checks a provided password against the configured argon2id
// PHC hash. The candidate is re-derived with the hash's own parameters and
// compared in constant time. An empty or malformed configured hash never
// matches: only Enabled/PasswordEnabled may decide that password auth is off,
// and auth.New validates the configured hash at startup so a typo cannot
// silently disable logins.
func verifyPassword(provided, configured string) bool {
	params, salt, want, err := parsePasswordHash(configured)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(provided), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// argon2Params is the cost set parsed out of a PHC string.
type argon2Params struct {
	time    uint32
	memory  uint32
	threads uint8 // argon2.IDKey takes lanes as uint8
}

// parsePasswordHash decodes a PHC-format argon2id hash string of the shape
//
//	$argon2id$v=19$m=65536,t=2,p=1$<base64 salt>$<base64 hash>
//
// (unpadded standard base64, the PHC convention). Anything else — other
// schemes, missing fields, undecodable base64, zero costs — is an error.
func parsePasswordHash(encoded string) (argon2Params, []byte, []byte, error) {
	if encoded == "" {
		return argon2Params{}, nil, nil, errors.New("empty password hash")
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return argon2Params{}, nil, nil, errors.New("not a PHC-format hash")
	}
	if parts[1] != "argon2id" {
		return argon2Params{}, nil, nil, fmt.Errorf("unsupported scheme %q (want argon2id)", parts[1])
	}
	if !strings.HasPrefix(parts[2], "v=") {
		return argon2Params{}, nil, nil, errors.New("missing argon2 version")
	}
	version, err := strconv.Atoi(parts[2][2:])
	if err != nil || version != argon2.Version {
		return argon2Params{}, nil, nil, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	costs := strings.Split(parts[3], ",")
	params, err := parseCosts(costs)
	if err != nil {
		return argon2Params{}, nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argon2Params{}, nil, nil, errors.New("malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argon2Params{}, nil, nil, errors.New("malformed hash")
	}
	if len(salt) == 0 || len(want) == 0 {
		return argon2Params{}, nil, nil, errors.New("empty salt or hash")
	}
	return params, salt, want, nil
}

// parseCosts parses the "m=65536,t=2,p=1" PHC cost segment (already split on
// commas), rejecting wrong field counts and zero or unparsable values. The
// guards mirror argon2.IDKey's panic conditions (time/threads ≥ 1), so
// verifyPassword can never trip a library panic on a crafted hash. m and t
// are 32-bit, p (the lane count argon2 takes as uint8) is 8-bit.
func parseCosts(segments []string) (argon2Params, error) {
	if len(segments) != 3 {
		return argon2Params{}, errors.New("malformed cost parameters (want m=...,t=...,p=...)")
	}
	fields := []string{"m", "t", "p"}
	bits := []int{32, 32, 8}
	var params argon2Params
	for i, seg := range segments {
		if i >= len(fields) || !strings.HasPrefix(seg, fields[i]+"=") {
			return params, fmt.Errorf("malformed cost parameter %q", seg)
		}
		n, err := strconv.ParseUint(seg[len(fields[i])+1:], 10, bits[i])
		if err != nil || n == 0 {
			return params, fmt.Errorf("malformed cost parameter %q", seg)
		}
		switch fields[i] {
		case "m":
			params.memory = uint32(n)
		case "t":
			params.time = uint32(n)
		case "p":
			params.threads = uint8(n)
		}
	}
	return params, nil
}
