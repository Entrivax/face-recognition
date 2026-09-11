package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Login rate-limit policy: a rolling window of recent failed logins is kept
// per client IP. More than rateMaxFailures failures within the trailing
// rateWindow deny further attempts for a while (429 + Retry-After).
const (
	rateWindow        = 5 * time.Minute
	rateMaxFailures   = 10
	rateMaxRetryAfter = 60 // seconds; advertised upper bound in Retry-After
	// maxFailuresPerIP caps how many timestamps are kept per IP: failures
	// beyond it cannot change the verdict (already denied), so dropping the
	// oldest keeps memory constant even under a flood.
	maxFailuresPerIP = 32
)

// RateLimiter tracks failed login attempts per client IP in a rolling
// window. IPs are keyed on the request's RemoteAddr host part — proxy headers
// like X-Forwarded-For are client-controlled and never trusted. Memory stays
// bounded: at most maxFailuresPerIP timestamps per IP, and keys with no
// recent failures are dropped whenever the map is touched after the sweep
// interval.
type RateLimiter struct {
	mu        sync.Mutex
	now       func() time.Time // injectable clock for tests
	failures  map[string][]time.Time
	lastSweep time.Time
}

// newRateLimiter builds a limiter with the given clock.
func newRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{now: now, failures: make(map[string][]time.Time), lastSweep: now()}
}

// Allow reports whether a login attempt from ip may proceed. When denied it
// also returns the Retry-After value in seconds: the time until enough
// failures have fallen out of the window to matter, clamped to a sane
// maximum so a burst never advertises a long lockout.
func (l *RateLimiter) Allow(ip string) (allowed bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	fails := l.pruneLocked(ip, now)
	if len(fails) <= rateMaxFailures {
		return true, 0
	}
	// Denied: the oldest failure must age out of the window before the
	// count can drop back to the limit.
	retry := int((rateWindow - now.Sub(fails[0])) / time.Second)
	if retry < 1 {
		retry = 1
	}
	if retry > rateMaxRetryAfter {
		retry = rateMaxRetryAfter
	}
	return false, retry
}

// RecordFailure records a failed login from ip.
func (l *RateLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	fails := append(l.pruneLocked(ip, now), now)
	if len(fails) > maxFailuresPerIP {
		// Keep only the most recent maxFailuresPerIP timestamps.
		fails = append([]time.Time(nil), fails[len(fails)-maxFailuresPerIP:]...)
	}
	l.failures[ip] = fails
}

// Reset clears the failure history for ip (successful login).
func (l *RateLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// pruneLocked drops ip's timestamps older than the window and returns the
// remaining ones. Caller must hold l.mu.
func (l *RateLimiter) pruneLocked(ip string, now time.Time) []time.Time {
	fails := l.failures[ip]
	kept := fails[:0]
	for _, t := range fails {
		if now.Sub(t) < rateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = kept
	}
	return kept
}

// sweepInterval is how often stale per-IP keys are dropped wholesale.
const sweepInterval = time.Minute

// sweepLocked removes IPs with no recent failures, bounding the map. Caller
// must hold l.mu.
func (l *RateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for ip, fails := range l.failures {
		if len(fails) == 0 || now.Sub(fails[len(fails)-1]) >= rateWindow {
			delete(l.failures, ip)
		}
	}
}

// remoteIP extracts the client IP for rate limiting from the request's
// network peer address. X-Forwarded-For is deliberately ignored: it is set
// by the client, so trusting it would let an attacker sidestep the limit.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr) // no port (e.g. unix socket)
	}
	return host
}
