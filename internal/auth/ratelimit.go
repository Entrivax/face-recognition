package auth

import (
	"fmt"
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

// parseTrustedProxies turns a comma-separated CIDR list (RECOGN_TRUSTED_PROXY_CIDR)
// into networks the login limiter may trust for client-IP extraction. An
// empty spec disables the feature entirely (the safe default).
func parseTrustedProxies(spec string) ([]*net.IPNet, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", part, err)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

func containsIP(cidrs []*net.IPNet, ip net.IP) bool {
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the address rate limiting keys on. Default (no trusted
// proxies configured): the network peer, with X-Forwarded-For ignored.
//
// When the direct peer is inside a configured trusted CIDR, the rightmost
// non-trusted entry of X-Forwarded-For is used instead — the client IP the
// nearest trusted proxy observed (SECURITY-REVIEW.md M4). Walking right to
// left and stopping at the first non-trusted, parseable address means spoofed
// entries to the LEFT of the real client hop cannot steer the key, and a
// chain of trusted proxies is handled too. Anything missing or unparsable
// falls back to the peer (fail closed).
func (s *Service) clientIP(r *http.Request) string {
	peer := remoteIP(r)
	if len(s.trusted) == 0 {
		return peer
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !containsIP(s.trusted, peerIP) {
		return peer
	}
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return peer // a corrupted hop poisons the chain: fail closed
		}
		if containsIP(s.trusted, ip) {
			continue // another trusted proxy hop
		}
		return ip.String()
	}
	return peer
}

// ClientIP is the exported proxy-aware client key, shared with the API
// package's own admission limiter so both rate limits agree on who the
// client is (SECURITY-REVIEW.md M1/M4).
func (s *Service) ClientIP(r *http.Request) string { return s.clientIP(r) }
