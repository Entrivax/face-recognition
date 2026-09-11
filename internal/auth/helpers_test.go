package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"recogn/internal/config"
)

// testEpoch is a fixed instant all fake clocks start from.
var testEpoch = time.Unix(1700000000, 0)

// newTestRequest builds a bare request the way httptest would (RemoteAddr
// defaults to 192.0.2.1:1234, so rate limiting keys on that IP).
func newTestRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

// newTestConfig returns a Config pointed at temp directories. Callers set
// cfg.AdminPasswordHash etc. themselves.
func newTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Addr = ":0"
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.PeopleDir = filepath.Join(t.TempDir(), "people")
	return cfg
}
