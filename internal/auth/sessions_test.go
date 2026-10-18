package auth

import (
	"net/http"
	"testing"
	"time"
)

// fakeClock returns a controllable clock for tests: the returned func reads
// the current value set via the returned setter.
func fakeClock(start time.Time) (now func() time.Time, advance func(d time.Duration)) {
	current := start
	return func() time.Time { return current },
		func(d time.Duration) { current = current.Add(d) }
}

func TestSessionsSlideTTL(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(10*time.Minute, now)

	token, expiry := s.New("admin")
	if !tokenExpiryMatches(expiry, now().Add(10*time.Minute)) {
		t.Fatalf("expiry = %v, want now+ttl", expiry)
	}
	if name, ok := s.Valid(token); !ok || name != "admin" {
		t.Fatalf("Valid = (%q, %v), want (\"admin\", true)", name, ok)
	}

	// Idle past the TTL: gone.
	advance(11 * time.Minute)
	if _, ok := s.Valid(token); ok {
		t.Fatal("session should expire after the TTL without activity")
	}

	// Sliding: an active session keeps being refreshed.
	token, _ = s.New("admin")
	for i := 0; i < 5; i++ {
		advance(8 * time.Minute)
		if _, ok := s.Valid(token); !ok {
			t.Fatalf("active session expired at minute %d of sliding use", (i+1)*8)
		}
	}

	// After the last refresh the session survives one more TTL, then dies.
	advance(9 * time.Minute)
	if _, ok := s.Valid(token); !ok {
		t.Fatal("session should survive within the refreshed TTL")
	}
	advance(10 * time.Minute)
	if _, ok := s.Valid(token); ok {
		t.Fatal("session should expire once the sliding TTL runs out")
	}
}

func TestSessionsDeleteAndUnknown(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(time.Hour, now)

	if _, ok := s.Valid(""); ok {
		t.Fatal("empty tokens must not validate")
	}
	if _, ok := s.Valid("no-such-token"); ok {
		t.Fatal("unknown tokens must not validate")
	}
	token, _ := s.New("admin")
	s.Delete(token)
	if _, ok := s.Valid(token); ok {
		t.Fatal("deleted session must not validate")
	}
	s.Delete(token) // deleting again is a no-op, no panic
}

func TestSessionsPerUserLookup(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(time.Hour, now)

	// The stored username is canonical (lowercased) whatever case was used.
	token, _ := s.New("MiXeD-Case_User")
	if name, ok := s.Valid(token); !ok || name != "mixed-case_user" {
		t.Fatalf("Valid = (%q, %v), want the lowercased username", name, ok)
	}
}

func TestSessionsDeleteUserSessions(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(time.Hour, now)

	tokA1, _ := s.New("alice")
	tokA2, _ := s.New("ALICE") // same account, second session
	tokB, _ := s.New("bob")

	s.DeleteUserSessions("Alice") // case-insensitive
	if _, ok := s.Valid(tokA1); ok {
		t.Fatal("alice's first session must be deleted")
	}
	if _, ok := s.Valid(tokA2); ok {
		t.Fatal("alice's second session must be deleted")
	}
	if _, ok := s.Valid(tokB); !ok {
		t.Fatal("bob's session must be untouched")
	}
}

func TestSessionsLazyPurge(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(time.Minute, now)

	tokens := make([]string, 3)
	for i := range tokens {
		tokens[i], _ = s.New("admin")
	}
	advance(2 * time.Minute)
	s.Valid("probe") // touching the store purges the expired ones
	if got := len(s.expiries); got != 0 {
		t.Fatalf("expired sessions not purged: %d entries remain, want 0", got)
	}
	if got := len(s.usernames); got != 0 {
		t.Fatalf("purged sessions left usernames behind: %d, want 0", got)
	}
	for _, tok := range tokens {
		if _, ok := s.Valid(tok); ok {
			t.Fatal("expired session validated")
		}
	}
}

// tokenExpiryMatches compares times with second precision to dodge clock
// truncation.
func tokenExpiryMatches(a, b time.Time) bool { return a.Equal(b) }

func TestSessionCookieAttributes(t *testing.T) {
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s := newSessionStore(time.Hour, now)

	req := newTestRequest("POST", "/api/login")
	c := s.cookie("tok123", 3600, req)
	if c.Name != SessionCookie || c.Value != "tok123" {
		t.Fatalf("unexpected cookie %+v", c)
	}
	if c.Path != "/" || !c.HttpOnly || c.MaxAge != 3600 {
		t.Fatalf("unexpected cookie flags: %+v", c)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Secure {
		t.Fatal("plain-http request must not set Secure")
	}

	// Forwarded HTTPS sets Secure.
	req.Header.Set("X-Forwarded-Proto", "https")
	if !s.cookie("t", 0, req).Secure {
		t.Fatal("X-Forwarded-Proto: https must set the Secure flag")
	}
}
