package config

import "testing"

// TestTrustedProxyCIDRsFromEnv covers the M4 config plumbing: the opt-in
// trusted-proxy CIDR list travels verbatim (comma-separated) and defaults to
// empty (peer-keyed rate limiting, XFF untrusted).
func TestTrustedProxyCIDRsFromEnv(t *testing.T) {
	t.Setenv("RECOGN_TRUSTED_PROXY_CIDR", "10.0.0.0/8, 192.168.0.0/16")
	if got := Default().TrustedProxyCIDRs; got != "10.0.0.0/8, 192.168.0.0/16" {
		t.Fatalf("TrustedProxyCIDRs = %q", got)
	}
	t.Setenv("RECOGN_TRUSTED_PROXY_CIDR", "")
	if got := Default().TrustedProxyCIDRs; got != "" {
		t.Fatalf("unset env must yield empty TrustedProxyCIDRs, got %q", got)
	}
}
