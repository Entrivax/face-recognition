package api

// Admission control for the inference-heavy endpoints (SECURITY-REVIEW.md
// M1): /api/recognize is public, and every request pays a full image decode +
// letterbox tensor + face crops in memory before the (bounded) inference
// gate. Nothing used to bound how many such requests could be in flight at
// once, so unauthenticated traffic could pin memory for minutes — the
// server's WriteTimeout is 300 s. Two cheap layers close that, in order:
//
//  1. ipRateLimiter — a per-IP token bucket on /api/recognize that absorbs
//     request floods with 429 + Retry-After.
//  2. inflightGate — a non-blocking in-flight semaphore (2× the inference
//     concurrency) around recognize and compare. When full, new requests are
//     shed with 503 + Retry-After. Requests are deliberately never queued:
//     a queue would pin admitted-but-waiting memory, which is exactly the
//     failure mode being prevented.
//
// Both bound memory per request: the 32 MiB body cap bounds compressed
// bytes, the pixel gate (engine.decodeLimited) bounds decoded pixels, and
// these two bound how many such requests may be live simultaneously.

import (
	"sync"
	"time"
)

const (
	// admitPerWorker bounds concurrent recognize/compare requests per
	// inference-concurrency unit (RECOGN_CONCURRENCY, default
	// min(NumCPU, 4)). At the default 4 that is 8 concurrent decodes —
	// comfortably above what the inference gate itself admits.
	admitPerWorker = 2
	// minAdmitSlots keeps tiny hosts (RECOGN_CONCURRENCY=1) usable.
	minAdmitSlots = 2
	// multipartMemory is the memory ParseMultipartForm may spend on buffered
	// parts; larger file parts spill to OS temp files instead of sitting
	// RAM-resident for the whole request (the request body cap maxUpload
	// still bounds the total bytes).
	multipartMemory = 10 << 20 // 10 MiB

	// Recognize rate-limit policy: recognizeBurst requests may arrive at
	// once per client, refilled at recognizeRefillPerSec per second — far
	// above interactive/UI use, tight enough to blunt floods.
	recognizeBurst        = 30
	recognizeRefillPerSec = 1.0
	recognizeRetryMax     = 60 // seconds; advertised upper bound in Retry-After

	// admitRetryAfter is advertised on 503 when the in-flight gate is full;
	// inference takes well under a second, so this is generous.
	admitRetryAfter = 2
)

// inflightGate is a non-blocking counting semaphore: tryAcquire either takes
// one of n slots or reports failure immediately (no queueing).
type inflightGate struct {
	slots chan struct{}
}

func newInflightGate(n int) *inflightGate {
	if n < 1 {
		n = 1
	}
	return &inflightGate{slots: make(chan struct{}, n)}
}

func (g *inflightGate) tryAcquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *inflightGate) release() { <-g.slots }

// ipRateLimiter is a per-client token bucket. Clients refill at perSec
// tokens per second up to burst; each request consumes one. Memory stays
// bounded: idle client entries are dropped on a periodic sweep, mirroring
// auth.RateLimiter. now is injectable for tests.
type ipRateLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	perSec    float64
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(now func() time.Time, perSec, burst float64) *ipRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &ipRateLimiter{
		now:       now,
		perSec:    perSec,
		burst:     burst,
		buckets:   make(map[string]*bucket),
		lastSweep: now(),
	}
}

// allow consumes one token for ip, reporting whether the request may proceed
// and — when denied — the Retry-After value in seconds.
func (l *ipRateLimiter) allow(ip string) (allowed bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens += d.Seconds() * l.perSec
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	retry := int((1 - b.tokens) / l.perSec)
	if retry < 1 {
		retry = 1
	}
	if retry > recognizeRetryMax {
		retry = recognizeRetryMax
	}
	return false, retry
}

// sweepInterval bounds how often idle client entries are dropped wholesale.
const limiterSweepInterval = time.Minute

// sweepLocked drops buckets that cannot refill to a usable level before
// touching them again, bounding the map. Caller must hold l.mu.
func (l *ipRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < limiterSweepInterval {
		return
	}
	l.lastSweep = now
	// An untouched bucket is at most l.burst behind; it refills to full in
	// burst/perSec seconds. Anything idle longer than twice that is dead.
	idleAfter := time.Duration(2 * l.burst / l.perSec * float64(time.Second))
	for ip, b := range l.buckets {
		if now.Sub(b.last) > idleAfter {
			delete(l.buckets, ip)
		}
	}
}
