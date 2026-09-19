package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// Passkeys live in a single JSON file next to the face DB (data/passkeys.json).
// It is a plain array of credential records; a missing file is an empty store
// (nothing registered yet), while a corrupt one fails startup loudly — a
// silently ignored store would let an admin believe a passkey is still
// registered (mirrors how the DB treats a bad embeddings.json).
const passkeysFileName = "passkeys.json"

// passkeyRecord is one registered credential on disk: the full WebAuthn
// credential (public key, sign counter, flags, …) plus bookkeeping.
type passkeyRecord struct {
	Credential webauthn.Credential `json:"credential"`
	AddedAt    time.Time           `json:"added_at"`
}

// passkeyStore is the on-disk credential store. All mutations rewrite the
// file atomically (temp file + rename, mode 0600 — the file holds public
// keys only, but there is no reason to make it world-readable).
type passkeyStore struct {
	mu    sync.RWMutex
	path  string
	creds []passkeyRecord
	now   func() time.Time // injectable clock for tests
}

// loadPasskeyStore reads the credential file. A missing file is an empty
// store (no passkeys registered yet), not an error; a corrupt one is an
// error so the caller can refuse to start rather than drop admin access
// silently.
func loadPasskeyStore(path string, now func() time.Time) (*passkeyStore, error) {
	if now == nil {
		now = defaultNow
	}
	s := &passkeyStore{path: path, now: now}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // first run: no passkeys registered yet
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil // empty file: nothing registered
	}
	var creds []passkeyRecord
	if err := json.Unmarshal(b, &creds); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.creds = creds
	return s, nil
}

// save rewrites the store file atomically: write to a temp file in the same
// directory (so the rename stays on one filesystem), fsync-free but with
// mode 0600, then rename over the target.
func (s *passkeyStore) save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.creds, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, passkeysFileName+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

// credentials returns a copy of all stored credentials.
func (s *passkeyStore) credentials() []webauthn.Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]webauthn.Credential, 0, len(s.creds))
	for i := range s.creds {
		out = append(out, s.creds[i].Credential)
	}
	return out
}

// records returns a copy of all stored records (credential + added_at).
func (s *passkeyStore) records() []passkeyRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]passkeyRecord(nil), s.creds...)
}

// count reports the number of registered credentials.
func (s *passkeyStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.creds)
}

// add persists a newly registered credential; duplicate IDs are ignored.
func (s *passkeyStore) add(c webauthn.Credential) error {
	s.mu.Lock()
	for i := range s.creds {
		if string(s.creds[i].Credential.ID) == string(c.ID) {
			s.mu.Unlock()
			return nil // already registered
		}
	}
	s.creds = append(s.creds, passkeyRecord{Credential: c, AddedAt: s.now()})
	s.mu.Unlock()
	return s.save()
}

// removeByID drops the credential with the given base64url id and reports
// whether it existed; the store is persisted when it changed.
func (s *passkeyStore) removeByID(idB64 string) (bool, error) {
	s.mu.Lock()
	idx := -1
	for i := range s.creds {
		if base64URL(string(s.creds[i].Credential.ID)) == idB64 {
			idx = i
			break
		}
	}
	if idx == -1 {
		s.mu.Unlock()
		return false, nil
	}
	removed := s.creds[idx]
	s.creds = append(s.creds[:idx], s.creds[idx+1:]...)
	s.mu.Unlock()
	if err := s.save(); err != nil {
		// Roll the in-memory change back so memory and disk agree.
		s.mu.Lock()
		s.creds = append(s.creds, removed)
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

// update replaces a stored credential (e.g. a bumped sign counter after a
// successful login) and persists the change. The save is best-effort: the
// fresh counter stays in memory and a persistence failure is only logged —
// a lost counter update just weakens cloned-authenticator detection until
// the next login; it must never fail the login itself. Unknown credentials
// are ignored.
func (s *passkeyStore) update(c webauthn.Credential) {
	s.mu.Lock()
	found := false
	for i := range s.creds {
		if string(s.creds[i].Credential.ID) == string(c.ID) {
			s.creds[i].Credential = c
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return
	}
	if err := s.save(); err != nil {
		slog.Warn("persist passkey sign counter", "err", err)
	}
}

// base64URL encodes raw bytes as unpadded base64url (the WebAuthn JSON id form).
func base64URL(b string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(b))
}

// adminUser is the single synthetic WebAuthn user: there is exactly one
// admin account, and every registered passkey belongs to it.
type adminUser struct {
	rpName string
	creds  []webauthn.Credential
}

func (u *adminUser) WebAuthnID() []byte                         { return []byte("admin") }
func (u *adminUser) WebAuthnName() string                       { return "admin" }
func (u *adminUser) WebAuthnDisplayName() string                { return u.rpName }
func (u *adminUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// ceremonyTTL bounds how long a begin ceremony (registration or login)
// stays valid before the client must restart it.
const ceremonyTTL = 5 * time.Minute

// maxPendingCeremonies caps the pending map (SECURITY-REVIEW.md M3): the
// login begin endpoint is public, and without a cap a flood of begins pinned
// one ceremony each in memory for ceremonyTTL. Past the cap new begins are
// refused with 429 until the purge frees slots.
const maxPendingCeremonies = 256

// pendingCeremonies holds the in-flight WebAuthn ceremonies, keyed by the
// random challenge generated in the begin step (the client echoes it back in
// the finish body, which is the lookup key). Entries expire after
// ceremonyTTL; access lazily purges them.
type pendingCeremonies struct {
	mu       sync.Mutex
	now      func() time.Time
	sessions map[string]webauthn.SessionData
}

func newPendingCeremonies(now func() time.Time) *pendingCeremonies {
	if now == nil {
		now = defaultNow
	}
	return &pendingCeremonies{now: now, sessions: make(map[string]webauthn.SessionData)}
}

// put records a begun ceremony under its challenge. It reports whether the
// ceremony was accepted: once maxPendingCeremonies unexpired ceremonies are
// pending, further puts are refused (ok=false) with a Retry-After hint in
// seconds — the caller turns that into a 429.
func (p *pendingCeremonies) put(session *webauthn.SessionData) (accepted bool, retryAfter int) {
	if session == nil || session.Challenge == "" {
		return true, 0 // nothing to track; nothing to bound
	}
	now := p.now()
	session.Expires = now.Add(ceremonyTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purgeLocked(now)
	if len(p.sessions) >= maxPendingCeremonies {
		// Refuse. The next slot frees when the oldest pending ceremony
		// expires; advertise its remaining lifetime, clamped like the login
		// limiter's Retry-After.
		var oldest time.Time
		for _, s := range p.sessions {
			if oldest.IsZero() || s.Expires.Before(oldest) {
				oldest = s.Expires
			}
		}
		seconds := int(oldest.Sub(now) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		if seconds > rateMaxRetryAfter {
			seconds = rateMaxRetryAfter
		}
		return false, seconds
	}
	p.sessions[session.Challenge] = *session
	return true, 0
}

// take pops a pending ceremony by challenge. Unknown or expired challenges
// yield ok=false, so a stale finish attempt cannot replay an old ceremony.
func (p *pendingCeremonies) take(challenge string) (session webauthn.SessionData, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.purgeLocked(now)
	session, ok = p.sessions[challenge]
	if !ok {
		return webauthn.SessionData{}, false
	}
	delete(p.sessions, challenge)
	if !session.Expires.IsZero() && session.Expires.Before(now) {
		return webauthn.SessionData{}, false
	}
	return session, true
}

// purgeLocked drops expired ceremonies. Caller must hold p.mu.
func (p *pendingCeremonies) purgeLocked(now time.Time) {
	for k, s := range p.sessions {
		if !s.Expires.IsZero() && s.Expires.Before(now) {
			delete(p.sessions, k)
		}
	}
}

// rpForRequest returns the Relying Party ID and acceptable origin for a
// ceremony. Values pinned via RECOGN_WEBAUTHN_RPID / RECOGN_WEBAUTHN_ORIGIN
// win; otherwise they are derived from the request so the zero-config default
// works on localhost and behind a reverse proxy: RPID = Host minus port,
// origin = scheme (X-Forwarded-Proto, else http) + Host.
func (s *Service) rpForRequest(r *http.Request) (rpid, origin string) {
	rpid = s.cfg.WebAuthnRPID
	if rpid == "" {
		rpid = hostOnly(r.Host)
	}
	origin = s.cfg.WebAuthnOrigin
	if origin == "" {
		scheme := r.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "http"
		}
		origin = scheme + "://" + r.Host
	}
	return rpid, origin
}

// webAuthnFor builds the per-request WebAuthn instance. go-webauthn validates
// its config on New, so a derived RPID/origin that the request host makes
// invalid surfaces as a 500 here instead of a corrupted ceremony.
func (s *Service) webAuthnFor(r *http.Request) (*webauthn.WebAuthn, error) {
	rpid, origin := s.rpForRequest(r)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpid,
		RPDisplayName: s.cfg.WebAuthnRPName,
		RPOrigins:     []string{origin},
	})
	if err != nil && s.cfg.WebAuthnRPID == "" && net.ParseIP(hostOnly(r.Host)) != nil {
		// The WebAuthn spec forbids IP addresses as RPID, so a server
		// reached via a bare IP (e.g. 127.0.0.1) cannot run ceremonies.
		// Translate the library's validation error into an actionable hint.
		return nil, errors.New("WebAuthn needs a hostname: open the UI via localhost (or a domain), or set RECOGN_WEBAUTHN_RPID")
	}
	return wa, err
}

// hostOnly strips the port from a Host header value ("example.com:8443" →
// "example.com"). Values without a port are returned unchanged.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
