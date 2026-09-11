package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookie is the name of the admin session cookie.
const SessionCookie = "recogn_session"

// sessionTokenBytes is the entropy of a session token: 32 random bytes,
// base64url-encoded, give a ~256-bit unguessable opaque token.
const sessionTokenBytes = 32

// SessionStore keeps the in-memory set of live admin sessions: opaque token →
// expiry. Nothing is persisted — a restart logs every admin out, which is the
// right default for a single-user tool. The TTL is sliding: every successful
// validation pushes the expiry out again, so an actively used session is not
// logged off mid-work while abandoned ones die quietly.
type SessionStore struct {
	mu       sync.RWMutex
	ttl      time.Duration
	now      func() time.Time // injectable clock for tests
	expiries map[string]time.Time
}

// newSessionStore builds a store with the given sliding TTL and clock.
func newSessionStore(ttl time.Duration, now func() time.Time) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{ttl: ttl, now: now, expiries: make(map[string]time.Time)}
}

// New mints a fresh session token and returns it together with its expiry.
func (s *SessionStore) New() (token string, expiry time.Time) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable; rather than hand out a
		// predictable token, panic — same posture as the standard library.
		panic("auth: session token generation failed: " + err.Error())
	}
	token = base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	expiry = s.now().Add(s.ttl)
	s.expiries[token] = expiry
	return token, expiry
}

// Valid reports whether the token belongs to a live session. Validating a
// live session refreshes (slides) its expiry; touching the map also lazily
// purges expired entries.
func (s *SessionStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	exp, ok := s.expiries[token]
	if !ok || !now.Before(exp) {
		return false // unknown or expired
	}
	s.expiries[token] = now.Add(s.ttl) // sliding TTL
	return true
}

// Delete drops a session (logout); deleting an unknown token is a no-op.
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expiries, token)
}

// purgeLocked removes expired entries. Caller must hold s.mu (write).
func (s *SessionStore) purgeLocked(now time.Time) {
	for token, exp := range s.expiries {
		if !now.Before(exp) {
			delete(s.expiries, token)
		}
	}
}

// cookie builds the session cookie for the request. The Secure attribute is
// set when the connection is TLS or a proxy forwarded the request over
// HTTPS, so the cookie is never sent over plain HTTP by accident while still
// working on plain-http localhost.
func (s *SessionStore) cookie(token string, maxAge int, r *http.Request) *http.Cookie {
	// Secure when the connection is TLS or a proxy forwarded the request
	// over HTTPS, so the cookie is never set over plain HTTP by accident
	// while still working on plain-http localhost.
	secure := r.TLS != nil ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
}
