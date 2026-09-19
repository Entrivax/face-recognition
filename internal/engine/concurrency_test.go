package engine

// Permanent regression tests for SECURITY-REVIEW.md H1: Engine.thresh and
// Engine.known were written and read without synchronization. Over HTTP,
// POST /api/config (SetThreshold) and every mutation handler's reload
// (refreshEngine → SetKnown) race public recognize traffic; a torn slice
// header can SIGSEGV and a torn float64 corrupts matching.
//
// These tests are the restoration of the deleted audit probe. They must be
// run under the race detector (make test-race): every unsynchronized
// read/write pair exercised here is reported as "WARNING: DATA RACE". They
// pass functionally without -race, which is exactly why the old suite missed
// the bug.

import (
	"image"
	"sync"
	"testing"
)

// freshFaceInferencer is like fakeInferencer but hands every caller its own
// copy of the detection results. Recognize populates the returned faces in
// place; sharing one backing array across concurrent Recognize calls would
// race in the test harness itself rather than in the engine (the production
// CGO detector allocates fresh results per call, so the engine never sees
// this). Without the copy, a post-fix green run would still report races.
type freshFaceInferencer struct{ faces []Face }

func (f *freshFaceInferencer) clone() []Face {
	out := make([]Face, len(f.faces))
	copy(out, f.faces)
	return out
}

func (f *freshFaceInferencer) detect([]byte) ([]Face, error) { return f.clone(), nil }
func (f *freshFaceInferencer) detectFromImage(image.Image) ([]Face, error) {
	return f.clone(), nil
}
func (f *freshFaceInferencer) embedImage(*image.NRGBA) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}
func (f *freshFaceInferencer) embedBatch(aligned []*image.NRGBA) ([][]float32, error) {
	out := make([][]float32, len(aligned))
	for i, a := range aligned {
		if a != nil {
			out[i] = []float32{1, 0, 0}
		}
	}
	return out, nil
}
func (f *freshFaceInferencer) ping() error { return nil }
func (f *freshFaceInferencer) close()      {}

// concurrencyStub is a freshFaceInferencer with a single detectable face, so
// Recognize runs its full match path (bestPerPerson + threshold read).
func concurrencyStub() *freshFaceInferencer {
	return &freshFaceInferencer{
		faces: []Face{{BBox: [4]float64{0, 0, 10, 10}, Landmarks: makeLandmarks()}},
	}
}

// identitySets are two identity sets of different sizes: a torn slice header
// (new pointer + old length) would index out of bounds in addition to racing.
func identitySets() (small, large []KnownPerson) {
	small = []KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	}
	large = []KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{0, 1, 0}, {0.7, 0.7, 0}}},
		{ID: "carol", Name: "Carol", Embeddings: [][]float32{{0, 0, 1}}},
	}
	return small, large
}

// TestConcurrentThresholdReadWrite races SetThreshold against every unlocked
// reader of thresh: Threshold, MatchEmbedding and Recognize. Before the fix
// this is a deterministic DATA RACE under -race.
func TestConcurrentThresholdReadWrite(t *testing.T) {
	e := NewWithInferencer(concurrencyStub(), 0.5)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	})
	img := makeTestImage(t, 64, 64)

	const goroutines = 8
	const iters = 300
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				switch g % 4 {
				case 0: // writer (POST /api/config → SetThreshold)
					e.SetThreshold(0.2 + float64(k%80)/100)
				case 1: // direct reader
					_ = e.Threshold()
				case 2: // match readers
					e.MatchEmbedding([]float32{1, 0, 0})
					e.MatchAll([]float32{1, 0, 0})
				case 3: // full-pipeline reader (Recognize reads thresh per face)
					_, _ = e.Recognize(img)
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentKnownReadWrite races SetKnown against the identity-set
// readers: MatchEmbedding/MatchAll (bestPerPerson) and Known. Before the fix
// this is a deterministic DATA RACE under -race.
func TestConcurrentKnownReadWrite(t *testing.T) {
	e := NewWithInferencer(&fakeInferencer{}, 0.5)
	small, large := identitySets()

	const goroutines = 8
	const iters = 300
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				switch g % 4 {
				case 0: // writer (mutation handler's reload → SetKnown)
					if k%2 == 0 {
						e.SetKnown(small)
					} else {
						e.SetKnown(large)
					}
				case 1: // matcher
					e.MatchEmbedding([]float32{1, 0, 0})
				case 2: // matcher, ranked variant
					e.MatchAll([]float32{0, 1, 0})
				case 3: // accessor
					_ = e.Known()
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentConfigWriterVsRecognize mirrors the real traffic pattern from
// the review: one goroutine performs the config-write pair (SetThreshold then
// SetKnown, as POST /api/config + s.reload() do back-to-back) while several
// goroutines serve recognize requests. Under -race, the unfixed engine
// reports DATA RACE on both fields.
func TestConcurrentConfigWriterVsRecognize(t *testing.T) {
	e := NewWithInferencer(concurrencyStub(), 0.5)
	small, large := identitySets()
	img := makeTestImage(t, 64, 64)

	const readers = 6
	const iters = 200
	// Single writer goroutine: the admin path. It loops until the readers
	// finish instead of a fixed count — a bounded burst would complete before
	// the (decode-heavy) readers ever touch the fields, and the race detector
	// would only catch the overlap by luck.
	done := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for k := 0; ; k++ {
			select {
			case <-done:
				return
			default:
			}
			e.SetThreshold(0.3 + float64(k%60)/100)
			if k%2 == 0 {
				e.SetKnown(small)
			} else {
				e.SetKnown(large)
			}
		}
	}()
	// Reader goroutines: the public recognize path.
	var readersWG sync.WaitGroup
	for g := 0; g < readers; g++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for k := 0; k < iters; k++ {
				_, _ = e.Recognize(img)
			}
		}()
	}
	// Join order matters: the writer stops only once the readers are done.
	readersWG.Wait()
	close(done)
	<-writerDone
}

// TestThresholdSetterRoundTrip pins the functional semantics across the
// synchronization change: the setter round-trips through the getter and a
// match flips when the threshold crosses the score.
func TestThresholdSetterRoundTrip(t *testing.T) {
	e := NewWithInferencer(concurrencyStub(), 0.5)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	})
	img := makeTestImage(t, 64, 64)

	if got := e.Threshold(); got != 0.5 {
		t.Fatalf("initial threshold = %v, want 0.5", got)
	}
	// Score ~1.0 clears 0.5 → Alice.
	faces, err := e.Recognize(img)
	if err != nil || len(faces) != 1 || faces[0].Name != "Alice" {
		t.Fatalf("at 0.5: faces=%+v err=%v, want Alice", faces, err)
	}
	// Raise above any achievable score → unknown, no Matches.
	e.SetThreshold(1.1)
	if got := e.Threshold(); got != 1.1 {
		t.Fatalf("threshold after SetThreshold(1.1) = %v, want 1.1", got)
	}
	faces, err = e.Recognize(img)
	if err != nil || len(faces) != 1 {
		t.Fatalf("at 1.1: faces=%+v err=%v", faces, err)
	}
	if faces[0].Name != "unknown" || len(faces[0].Matches) != 0 {
		t.Errorf("at 1.1: name=%q matches=%d, want unknown/0",
			faces[0].Name, len(faces[0].Matches))
	}
	// And back down → Alice again.
	e.SetThreshold(0.5)
	faces, err = e.Recognize(img)
	if err != nil || len(faces) != 1 || faces[0].Name != "Alice" {
		t.Errorf("back at 0.5: faces=%+v err=%v, want Alice", faces, err)
	}
}

// TestKnownReturnsSnapshot pins the copy-on-write contract: Known returns a
// snapshot the caller may freely mutate without affecting the engine, and
// SetKnown replaces the set wholesale (nil included).
func TestKnownReturnsSnapshot(t *testing.T) {
	e := &Engine{thresh: 0.5}
	e.SetKnown([]KnownPerson{
		{ID: "a", Name: "A", Embeddings: [][]float32{{1, 0, 0}}},
	})

	k := e.Known()
	if len(k) != 1 || k[0].Name != "A" {
		t.Fatalf("Known = %+v, want one person A", k)
	}
	// Mutate the returned slice: element edit + append.
	k[0].Name = "mutated"
	k = append(k, KnownPerson{ID: "b", Name: "B"})
	if got := e.Known(); len(got) != 1 || got[0].Name != "A" {
		t.Errorf("engine set changed through the returned slice: %+v", got)
	}
	// Replace the set wholesale, including nil.
	e.SetKnown(nil)
	if got := e.Known(); len(got) != 0 {
		t.Errorf("SetKnown(nil) left %d people", len(got))
	}
	large := []KnownPerson{
		{ID: "a", Name: "A", Embeddings: [][]float32{{1, 0, 0}}},
		{ID: "b", Name: "B", Embeddings: [][]float32{{0, 1, 0}}},
	}
	e.SetKnown(large)
	if got := e.Known(); len(got) != 2 {
		t.Errorf("SetKnown(replace) left %d people, want 2", len(got))
	}
}
