package netutil

import (
	"net"
	"strings"
	"testing"
)

func TestNormalizeListenAddr(t *testing.T) {
	cases := map[string]string{
		"8080":         ":8080",        // bare port → wildcard
		":8080":        ":8080",        // wildcard, unchanged
		"127.0.0.1:80": "127.0.0.1:80", // specific host
		"localhost:80": "localhost:80", // hostname
		"[::1]:8080":   "[::1]:8080",   // IPv6 literal
		"":             "",             // empty → all interfaces, auto port
		"no-a-port":    "no-a-port",    // not a port → pass through (bind fails)
	}
	for in, want := range cases {
		if got := normalizeListenAddr(in); got != want {
			t.Errorf("normalizeListenAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListenBarePort(t *testing.T) {
	ln, err := Listen("0")
	if err != nil {
		t.Fatalf("Listen(\"0\"): %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().String(); !strings.HasSuffix(got, ":0") || got == ":0" {
		// An auto-assigned port must materialize in the bound address.
		if _, port, err := net.SplitHostPort(got); err != nil || port == "0" || port == "" {
			t.Errorf("Listen(\"0\") bound %q, want an assigned port", got)
		}
	}
}

func TestURLsSpecificHost(t *testing.T) {
	for _, in := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		urls := URLs(in)
		if len(urls) != 1 {
			t.Errorf("URLs(%q) = %v, want exactly 1 URL", in, urls)
		}
	}
	want := "http://127.0.0.1:8080"
	if got := URLs("127.0.0.1:8080")[0]; got != want {
		t.Errorf("URLs host case = %q, want %q", got, want)
	}
	want = "http://[::1]:8080"
	if got := URLs("[::1]:8080")[0]; got != want {
		t.Errorf("URLs IPv6 case = %q, want %q", got, want)
	}
}

func TestURLsWildcard(t *testing.T) {
	urls := URLs(":8080")
	if len(urls) < 1 || urls[0] != "http://localhost:8080" {
		t.Fatalf("URLs(:8080) = %v, want localhost first", urls)
	}
	for _, u := range urls[1:] {
		host, _, err := hostOf(u)
		if err != nil {
			t.Fatalf("bad URL %q: %v", u, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			t.Errorf("non-IP host %q in wildcard list", host)
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Errorf("unusable address %q listed for wildcard listener", host)
		}
	}
}

func TestURLsWildcardV4Only(t *testing.T) {
	urls := URLs("0.0.0.0:9000")
	if len(urls) < 1 || urls[0] != "http://localhost:9000" {
		t.Fatalf("URLs(0.0.0.0:9000) = %v, want localhost first", urls)
	}
	for _, u := range urls[1:] {
		host, _, err := hostOf(u)
		if err != nil {
			t.Fatalf("bad URL %q: %v", u, err)
		}
		if strings.Contains(host, ":") {
			t.Errorf("IPv6 address %q listed for 0.0.0.0 listener", host)
		}
	}
}

// hostOf strips the http:// prefix and any IPv6 brackets, returning the host.
func hostOf(url string) (host string, port string, err error) {
	s := strings.TrimPrefix(url, "http://")
	return net.SplitHostPort(s)
}

func TestURLFor(t *testing.T) {
	cases := map[[2]string]string{
		{"127.0.0.1", "8080"}: "http://127.0.0.1:8080",
		{"localhost", "80"}:   "http://localhost:80",
		{"::1", "8080"}:       "http://[::1]:8080",
		{"fe80::1", "9000"}:   "http://[fe80::1]:9000",
	}
	for in, want := range cases {
		if got := urlFor(in[0], in[1]); got != want {
			t.Errorf("urlFor(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
