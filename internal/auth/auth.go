// Package auth implements multi-user authentication for recogn's API.
//
// User accounts — a username with an argon2id password hash, an admin flag,
// per-repository grants ("reader"/"writer") and API keys — persist in a
// bbolt store at <DataDir>/users.db (package recogn/internal/users). The
// in-memory pieces stay here: session tokens (cookie recogn_session or a
// Bearer session token), the login and setup rate limiters, and the setup
// key.
//
// A fresh install (zero users) runs in "setup" mode: New generates a
// one-time setup key, kept in memory ONLY, that the operator presents to
// POST /api/auth/setup to create the first ADMIN account. Until then admin
// surface access is impossible — there is no open mode.
//
// After setup, logins are username + password (rate-limited per client IP);
// requests authenticate with the session cookie or a Bearer token — either
// a session token or an API key ("rk_..."-prefixed, resolved through the
// users store, whose grants decide per-repo roles). Principal() resolves
// every request to a fresh user record, so admin/grant changes apply to
// live sessions and keys immediately.
//
// The package is self-contained on purpose: it must not import internal/api
// (which imports it), so it carries its own tiny writeJSON/writeError pair
// matching the api package's {"error": msg} style. RECOGN_TRUSTED_PROXY_CIDR
// still controls the client-IP keying of the rate limiters (M4); the old
// RECOGN_ADMIN_PASSWORD_HASH variable is ignored (a later step removes the
// config field).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"recogn/internal/config"
	"recogn/internal/users"
)

// defaultNow is the wall clock; tests swap an injectable one in.
func defaultNow() time.Time { return time.Now() }

// maxBody caps JSON request bodies (login, setup, password change, key
// creation) at 1 MiB, matching the api package's JSON body cap.
const maxBody = 1 << 20

// usersDBName is the bbolt file holding the user accounts, next to the
// face DB in DataDir.
const usersDBName = "users.db"

// setupKeyBytes is the entropy of a setup key: 10 bytes → exactly 16
// RFC4648 base32 characters, formatted XXXX-XXXX-XXXX-XXXX. The RFC4648
// uppercase alphabet (A–Z, 2–7) contains no look-alike pairs (no 0/O, no
// 1/I), so the key survives sloppy transcription.
const (
	setupKeyBytes = 10
	setupGroupLen = 4
)

// Service owns every authentication mechanism: the users store, the session
// store, the login and setup rate limiters and the setup key. (Each
// sub-store keeps its own injectable clock; tests drive them directly.)
type Service struct {
	cfg      config.Config
	users    *users.Store
	sessions *SessionStore
	limiter  *RateLimiter // failed logins, keyed per client IP
	setupLim *RateLimiter // failed setup attempts, keyed per client IP

	setupMu  sync.Mutex
	setupKey string // in memory only; "" once consumed or unavailable

	trusted []*net.IPNet // trusted reverse-proxy CIDRs for client-IP extraction (M4)
}

// New builds the auth service from cfg: it opens the users store (failing
// loudly on a locked or unreadable users.db), parses the trusted-proxy
// CIDRs and — when no users exist yet — mints the setup key for the
// first-admin bootstrap. Note the old RECOGN_ADMIN_PASSWORD_HASH variable
// is deliberately ignored: accounts now live in users.db.
func New(cfg config.Config) (*Service, error) {
	ustore, err := users.Open(filepath.Join(cfg.DataDir, usersDBName))
	if err != nil {
		return nil, err
	}
	trusted, err := parseTrustedProxies(cfg.TrustedProxyCIDRs)
	if err != nil {
		ustore.Close()
		return nil, fmt.Errorf("invalid RECOGN_TRUSTED_PROXY_CIDR: %w", err)
	}
	s := &Service{
		cfg:      cfg,
		users:    ustore,
		sessions: newSessionStore(cfg.SessionTTL, defaultNow),
		limiter:  newRateLimiter(defaultNow),
		setupLim: newRateLimiter(defaultNow),
		trusted:  trusted,
	}
	if s.users.Count() == 0 {
		s.setupKey = generateSetupKey()
	}
	return s, nil
}

// Close releases the users store's bbolt file lock.
func (s *Service) Close() error { return s.users.Close() }

// generateSetupKey returns the human-transcribable setup key in the
// XXXX-XXXX-XXXX-XXXX form. Panics on crypto/rand failure — handing out a
// predictable bootstrap key is worse than refusing to start.
func generateSetupKey() string {
	b := make([]byte, setupKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic("auth: setup key generation failed: " + err.Error())
	}
	// 10 bytes are exactly 80 bits: base32 (5 bits/char) needs no padding
	// and yields exactly 16 characters.
	enc := base32.StdEncoding.EncodeToString(b)
	if len(enc) != 16 {
		panic("auth: setup key encoding produced " + strconv.Itoa(len(enc)) + " chars")
	}
	return enc[0:setupGroupLen] + "-" + enc[setupGroupLen:2*setupGroupLen] + "-" +
		enc[2*setupGroupLen:3*setupGroupLen] + "-" + enc[3*setupGroupLen:4*setupGroupLen]
}

// normalizeSetupKey prepares a presented key for comparison: trim, strip
// the group dashes (and any stray spaces), uppercase.
func normalizeSetupKey(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "-", "")
	return strings.ReplaceAll(v, " ", "")
}

// checkSetupKey reports whether the presented (normalized) setup key matches
// the live one, in constant time. An unconsumed key of the wrong length
// simply fails the comparison.
func (s *Service) checkSetupKey(presented string) bool {
	if s.setupKey == "" {
		return false
	}
	got := normalizeSetupKey(presented)
	want := normalizeSetupKey(s.setupKey)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// SetupRequired reports whether the first-admin bootstrap is still pending
// (a live user count check, not a cached flag).
func (s *Service) SetupRequired() bool { return s.users.Count() == 0 }

// SetupKey returns the current setup key and whether one exists. The key is
// held in memory only: it survives until the first admin is created (or the
// process restarts, which mints a fresh one).
func (s *Service) SetupKey() (string, bool) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupKey == "" {
		return "", false
	}
	return s.setupKey, true
}

// Mode names the effective auth posture: "setup" while no users exist, else
// "users". (Replaces the old open/password/passkey mode names.)
func (s *Service) Mode() string {
	if s.SetupRequired() {
		return "setup"
	}
	return "users"
}
