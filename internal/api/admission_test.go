package api

// SECURITY-REVIEW.md M1: /api/recognize used to have no admission control at
// all — every request paid a full decode + letterbox + crop allocation before
// the (bounded) inference gate, so unauthenticated traffic could pin memory
// for minutes (WriteTimeout 300s). The fix adds two layers, in order:
//
//  1. a per-IP token-bucket rate limit (429 + Retry-After), and
//  2. a non-blocking in-flight gate sized 2× the inference concurrency
//     (503 + Retry-After) around recognize AND compare — requests are never
//     queued, because queueing pins memory, which is the failure mode.
//
// These tests pin both layers and the multipart parse memory reduction.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"recogn/internal/engine"
)

// blockingEngine parks inside Recognize/Detect so a test can hold an
// in-flight slot open deterministically. Both return one face so the
// admitted request completes through the whole handler.
type blockingEngine struct {
	stubEngine
	arrived chan struct{} // closed on the first blocked call
	release chan struct{} // closed to let the blocked call finish
	once    sync.Once
}

var oneFace = []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}

func (b *blockingEngine) Recognize([]byte) ([]engine.Face, error) {
	b.arrivedOnce()
	<-b.release
	return oneFace, nil
}

func (b *blockingEngine) Detect([]byte) ([]engine.Face, error) {
	b.arrivedOnce()
	<-b.release
	return oneFace, nil
}

func (b *blockingEngine) arrivedOnce() {
	b.once.Do(func() { close(b.arrived) })
}

func postRecognize(s *Server, remote, xff string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/recognize", strings.NewReader("img"))
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	var mu sync.Mutex
	cur := start
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance = func(d time.Duration) {
		mu.Lock()
		cur = cur.Add(d)
		mu.Unlock()
	}
	return now, advance
}

// TestRecognizeInFlightCap pins the in-flight gate: with one slot, a second
// request is shed with 503 + Retry-After (and never queued), while the
// admitted one completes normally.
func TestRecognizeInFlightCap(t *testing.T) {
	eng := &blockingEngine{arrived: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestServer(t, eng)
	s.inflight = newInflightGate(1)

	// Request 1 occupies the only slot (the engine blocks mid-recognize).
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postRecognize(s, "192.0.2.1:1111", "")
	}()
	select {
	case <-eng.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the engine")
	}

	// Request 2: the gate is full → 503 + Retry-After, JSON error body.
	rec := postRecognize(s, "192.0.2.1:1111", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated gate: got %d (%s), want 503", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry a Retry-After header")
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["error"] == "" {
		t.Fatalf("503 must carry a JSON error, got %s", rec.Body.String())
	}

	// The admitted request completes normally once the engine unblocks.
	close(eng.release)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("admitted request: got %d (%s)", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted request never completed")
	}
}

// TestCompareAlsoAdmitted pins the gate on /api/compare too: it runs two
// detections and two embeddings per request, so it shares the budget.
func TestCompareAlsoAdmitted(t *testing.T) {
	eng := &blockingEngine{arrived: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestServer(t, eng)
	s.inflight = newInflightGate(1)

	body, ct := compareBody(t, []byte("a"), []byte("b"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/compare", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-eng.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first compare never reached the engine")
	}

	body2, ct2 := compareBody(t, []byte("a"), []byte("b"))
	req := httptest.NewRequest(http.MethodPost, "/api/compare", body2)
	req.Header.Set("Content-Type", ct2)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated gate on compare: got %d (%s), want 503", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("compare 503 must carry Retry-After")
	}

	close(eng.release)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("admitted compare: got %d (%s)", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted compare never completed")
	}
}

// TestRecognizeRateLimitedPerIP pins the token bucket: recognizeBurst quick
// requests pass, then the IP is 429'd with Retry-After; a different IP is
// unaffected; tokens refill with time.
func TestRecognizeRateLimitedPerIP(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newTestServer(t, eng)
	now, advance := fakeClock(time.Unix(1700000000, 0))
	s.recognLimiter.now = now

	const ip = "9.9.9.9:1111"
	for i := 0; i < recognizeBurst; i++ {
		if rec := postRecognize(s, ip, ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d (%s), want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := postRecognize(s, ip, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("burst+1: got %d (%s), want 429", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 must carry Retry-After")
	}

	// A different IP is unaffected.
	if rec := postRecognize(s, "8.8.8.8:2222", ""); rec.Code != http.StatusOK {
		t.Fatalf("other IP: got %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Tokens refill over time.
	advance(2 * time.Second)
	if rec := postRecognize(s, ip, ""); rec.Code != http.StatusOK {
		t.Fatalf("after refill: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestRecognizeRateLimitIgnoresUntrustedXFF pins the anti-spoofing default:
// rotating X-Forwarded-For does not mint fresh buckets without trusted
// proxies configured — the limit still trips on the peer address.
func TestRecognizeRateLimitIgnoresUntrustedXFF(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newTestServer(t, eng)
	now, _ := fakeClock(time.Unix(1700000000, 0))
	s.recognLimiter.now = now

	for i := 0; i < recognizeBurst; i++ {
		xff := fmt.Sprintf("203.0.113.%d", i%10)
		if rec := postRecognize(s, "7.7.7.7:9999", xff); rec.Code != http.StatusOK {
			t.Fatalf("request %d (xff=%s): got %d, want 200", i+1, xff, rec.Code)
		}
	}
	if rec := postRecognize(s, "7.7.7.7:9999", "203.0.113.200"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rotated XFF must not bypass the limit: got %d, want 429", rec.Code)
	}
}

// TestLargeMultipartStillParses pins the multipart parse memory reduction:
// parts larger than the 10 MiB parse memory must spill to OS temp files and
// still be readable end-to-end (33 MiB body cap unchanged).
func TestLargeMultipartStillParses(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	s, _ := newTestServer(t, eng)

	big := make([]byte, multipartMemory+(1<<20)) // 11 MiB file part
	body, ct := multipartBody(t, "image", "big.jpg", big)
	req := httptest.NewRequest(http.MethodPost, "/api/recognize", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("11 MiB part: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("count = %d, want 1", resp.Count)
	}
}
