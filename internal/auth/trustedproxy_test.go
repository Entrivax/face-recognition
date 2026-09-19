package auth

// SECURITY-REVIEW.md M4: the login rate limiter keys on the network peer, so
// behind the documented TLS reverse proxy every client shares one bucket and
// any client can keep the admin 429'd (lockout DoS). The fix is an opt-in
// trusted-proxy mode: when RECOGN_TRUSTED_PROXY_CIDR covers the request's
// direct peer, the limiter keys on the rightmost non-trusted entry of
// X-Forwarded-For — the client IP the nearest trusted proxy observed. Without
// the variable nothing changes: XFF stays untrusted (TestRemoteIP).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testTrustedCIDR covers the httptest default RemoteAddr (192.0.2.1), so a
// test request simulates "connection arrived from inside the trusted proxy".
const testTrustedCIDR = "192.0.2.0/24"

// newProxyTestService builds a password-mode Service with the given
// RECOGN_TRUSTED_PROXY_CIDR value.
func newProxyTestService(t *testing.T, cidrs string) *Service {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.AdminPasswordHash = testPasswordHash
	cfg.TrustedProxyCIDRs = cidrs
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return s
}

// proxiedLogin POSTs one login attempt from peer (RemoteAddr) with an
// X-Forwarded-For header and returns the response code.
func proxiedLogin(t *testing.T, s *Service, peer, xff, password string) int {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	s.LoginHandler().ServeHTTP(rec, req)
	return rec.Code
}

func TestClientIPUnconfiguredIgnoresXFF(t *testing.T) {
	s := newProxyTestService(t, "") // no trusted proxies: today's behaviour
	req := newTestRequest("POST", "/api/login")
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := s.ClientIP(req); got != "192.0.2.1" {
		t.Fatalf("without trusted CIDRs XFF must be ignored, got %q", got)
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	s := newProxyTestService(t, testTrustedCIDR)
	const trustedPeer = "192.0.2.1:1234" // httptest default, inside the CIDR

	cases := []struct {
		name string
		xff  string
		want string
	}{
		{"single client hop", "8.8.8.8", "8.8.8.8"},
		{"rightmost non-trusted entry wins", "8.8.8.8, 9.9.9.9", "9.9.9.9"},
		{"spoofed entries left of the real hop are ignored", "8.8.8.8, 203.0.113.7", "203.0.113.7"},
		{"all entries trusted falls back to peer", "192.0.2.9", "192.0.2.1"},
		{"no XFF falls back to peer", "", "192.0.2.1"},
		{"unparsable XFF fails closed to peer", "not-an-ip", "192.0.2.1"},
		{"unparsable rightmost hop fails closed", "8.8.8.8, also-bad", "192.0.2.1"},
		{"blank XFF fails closed to peer", "  ", "192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newTestRequest("POST", "/api/login")
			req.RemoteAddr = trustedPeer
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := s.ClientIP(req); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPMultipleCIDRs(t *testing.T) {
	s := newProxyTestService(t, " 192.0.2.0/24 , 10.0.0.0/8 ")
	req := newTestRequest("POST", "/api/login")
	req.RemoteAddr = "10.1.2.3:4444" // second trusted CIDR
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := s.ClientIP(req); got != "8.8.8.8" {
		t.Fatalf("proxy in second CIDR: ClientIP = %q, want 8.8.8.8", got)
	}
}

func TestClientIPUntrustedPeerSpoofIgnored(t *testing.T) {
	s := newProxyTestService(t, testTrustedCIDR)
	req := newTestRequest("POST", "/api/login")
	req.RemoteAddr = "10.9.9.9:5555" // not inside any trusted CIDR
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := s.ClientIP(req); got != "10.9.9.9" {
		t.Fatalf("an untrusted peer must not steer the key via XFF, got %q", got)
	}
}

func TestNewRejectsBadTrustedProxyCIDR(t *testing.T) {
	for _, bad := range []string{"banana", "192.0.2.0/24,nope", "192.0.2.1"} {
		cfg := newTestConfig(t)
		cfg.AdminPasswordHash = testPasswordHash
		cfg.TrustedProxyCIDRs = bad
		if _, err := New(cfg); err == nil {
			t.Errorf("RECOGN_TRUSTED_PROXY_CIDR=%q must fail startup loudly", bad)
		}
	}
}

// TestLoginLimiterTrustedProxyIndependence pins the M4 fix end-to-end: two
// clients behind the same trusted proxy get independent lockout buckets, and
// an untrusted direct peer cannot ride or poison an XFF key.
func TestLoginLimiterTrustedProxyIndependence(t *testing.T) {
	s := newProxyTestService(t, testTrustedCIDR)
	const proxyPeer = "192.0.2.1:1234"

	// 11 wrong passwords from client A behind the trusted proxy...
	for i := 0; i < 11; i++ {
		if code := proxiedLogin(t, s, proxyPeer, "203.0.113.10", "bad"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, code)
		}
	}
	// ...lock A out (even with the right password)...
	if code := proxiedLogin(t, s, proxyPeer, "203.0.113.10", "bad"); code != http.StatusTooManyRequests {
		t.Fatalf("locked client: got %d, want 429", code)
	}
	if code := proxiedLogin(t, s, proxyPeer, "203.0.113.10", "sekret"); code != http.StatusTooManyRequests {
		t.Fatalf("locked client with right password: got %d, want 429", code)
	}
	// ...while client B behind the SAME proxy is unaffected.
	if code := proxiedLogin(t, s, proxyPeer, "203.0.113.11", "sekret"); code != http.StatusOK {
		t.Fatalf("other proxied client: got %d, want 200", code)
	}
	// An untrusted peer with a spoofed X-Forwarded-For is keyed on its own
	// address: it can neither ride A's bucket nor poison it.
	if code := proxiedLogin(t, s, "10.9.9.9:5555", "203.0.113.10", "bad"); code != http.StatusUnauthorized {
		t.Fatalf("spoofed XFF from untrusted peer: got %d, want 401 (XFF ignored)", code)
	}
	if code := proxiedLogin(t, s, "10.9.9.9:5555", "203.0.113.10", "sekret"); code != http.StatusOK {
		t.Fatalf("untrusted peer must keep its own bucket: got %d, want 200", code)
	}
	// A remains locked out.
	if code := proxiedLogin(t, s, proxyPeer, "203.0.113.10", "sekret"); code != http.StatusTooManyRequests {
		t.Fatalf("A must still be locked: got %d, want 429", code)
	}
}

func TestLoginLimiterDefaultGroupsByPeer(t *testing.T) {
	// No trusted CIDRs: clients behind the same peer share one bucket — the
	// documented tradeoff M4's opt-in exists for.
	s := newProxyTestService(t, "")
	for i := 0; i < 11; i++ {
		xff := fmt.Sprintf("203.0.113.%d", i) // rotating XFF changes nothing
		if code := proxiedLogin(t, s, "192.0.2.1:1234", xff, "bad"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, code)
		}
	}
	if code := proxiedLogin(t, s, "192.0.2.1:1234", "203.0.113.200", "sekret"); code != http.StatusTooManyRequests {
		t.Fatalf("shared bucket behind untrusted proxy: got %d, want 429", code)
	}
}
