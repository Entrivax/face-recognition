package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	l := newRateLimiter(now)

	// rateMaxFailures failures are tolerated; the policy denies when MORE
	// than that many failures sit in the trailing window.
	for i := 0; i < rateMaxFailures; i++ {
		if ok, retry := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d denied (retry=%d), want allowed", i+1, retry)
		}
		l.RecordFailure("1.2.3.4")
	}
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("attempt rateMaxFailures+1 should still be allowed")
	}
	l.RecordFailure("1.2.3.4") // 11th failure → next attempt denied
	if ok, retry := l.Allow("1.2.3.4"); ok || retry < 1 || retry > rateMaxRetryAfter {
		t.Fatalf("after 11 failures: allowed=%v retry=%d, want denial with sane Retry-After", ok, retry)
	}

	// The window rolls: once the failures age out, access is granted again.
	advance(rateWindow + time.Second)
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("failures outside the window must not block access")
	}
}

func TestRateLimiterRetryAfterFromWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	l := newRateLimiter(now)

	for i := 0; i < rateMaxFailures+1; i++ {
		l.RecordFailure("9.9.9.9")
	}
	// Fresh burst: the oldest failure leaves the window in ~5 min, but the
	// advertised Retry-After is capped at 60s.
	_, retry := l.Allow("9.9.9.9")
	if retry != rateMaxRetryAfter {
		t.Fatalf("Retry-After = %d, want capped %d", retry, rateMaxRetryAfter)
	}
	// As the oldest failures age out the remaining wait shrinks accordingly.
	advance(4 * time.Minute)
	_, retry = l.Allow("9.9.9.9")
	if retry < 1 || retry > rateMaxRetryAfter {
		t.Fatalf("Retry-After %d out of range", retry)
	}
}

func TestRateLimiterResetAndIsolation(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	l := newRateLimiter(now)

	for i := 0; i < rateMaxFailures+3; i++ {
		l.RecordFailure("1.1.1.1")
	}
	if ok, _ := l.Allow("1.1.1.1"); ok {
		t.Fatal("1.1.1.1 should be denied")
	}
	// Another IP is unaffected.
	if ok, _ := l.Allow("2.2.2.2"); !ok {
		t.Fatal("a different IP must not be blocked by 1.1.1.1's failures")
	}
	// Reset clears the history.
	l.Reset("1.1.1.1")
	if ok, _ := l.Allow("1.1.1.1"); !ok {
		t.Fatal("Reset must re-enable access")
	}

	// Failure timestamps are pruned per IP when they age out.
	for i := 0; i < 5; i++ {
		l.RecordFailure("3.3.3.3")
	}
	advance(rateWindow + time.Minute)
	l.RecordFailure("3.3.3.3")
	if got := len(l.failures["3.3.3.3"]); got != 1 {
		t.Fatalf("stale failures not pruned: %d remain, want 1", got)
	}
}

func TestRateLimiterConstantMemory(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	l := newRateLimiter(now)

	// Way more failures than the per-IP cap: only the newest are kept.
	for i := 0; i < 200; i++ {
		l.RecordFailure("5.5.5.5")
	}
	if got := len(l.failures["5.5.5.5"]); got > maxFailuresPerIP {
		t.Fatalf("per-IP history exceeded the cap: %d > %d", got, maxFailuresPerIP)
	}

	// Stale keys are dropped wholesale on the periodic sweep.
	for _, ip := range []string{"6.6.6.1", "6.6.6.2", "5.5.5.5"} {
		l.RecordFailure(ip)
	}
	advance(rateWindow + time.Minute)
	l.RecordFailure("7.7.7.7") // triggers the sweep
	if len(l.failures) != 1 {
		t.Fatalf("sweep left %d keys, want only the fresh one", len(l.failures))
	}
}

func TestRemoteIP(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/login", nil)
	if got := remoteIP(req); got != "192.0.2.1" {
		t.Fatalf("remoteIP = %q, want the RemoteAddr host", got)
	}
	// A spoofed X-Forwarded-For must not change the key.
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := remoteIP(req); got != "192.0.2.1" {
		t.Fatalf("X-Forwarded-For must be ignored, got %q", got)
	}
}
