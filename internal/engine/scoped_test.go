package engine

import (
	"fmt"
	"image"
	"math"
	"reflect"
	"sync"
	"testing"
)

// scopedStub serves a fixed set of detections and one fixed embedding per
// detection index, so RecognizeScoped can be exercised without models. The
// face template is copied on every detect call: Recognize writes identity
// fields into the detected faces in place, so sharing the backing array
// across calls (or across concurrent calls) would alias earlier results.
type scopedStub struct {
	faces []Face      // detection template (copied per call)
	embs  [][]float32 // per-index embedding; a nil entry = failed embed
}

func (s *scopedStub) detect([]byte) ([]Face, error) { return s.snapshot(), nil }
func (s *scopedStub) detectFromImage(image.Image) ([]Face, error) {
	return s.snapshot(), nil
}

// snapshot copies the face template; Landmarks backing arrays stay shared but
// are only ever read.
func (s *scopedStub) snapshot() []Face {
	out := make([]Face, len(s.faces))
	copy(out, s.faces)
	return out
}

func (s *scopedStub) embedImage(*image.NRGBA) ([]float32, error) {
	return nil, fmt.Errorf("unexpected embedImage call")
}

func (s *scopedStub) embedBatch(aligned []*image.NRGBA) ([][]float32, error) {
	out := make([][]float32, len(aligned))
	for i, a := range aligned {
		if a == nil {
			continue // failed alignment stays nil
		}
		if i < len(s.embs) {
			out[i] = s.embs[i]
		}
	}
	return out, nil
}

func (s *scopedStub) ping() error { return nil }
func (s *scopedStub) close()      {}

// scopedIdentities is the shared two-person identity set: the direction
// {1,2,0} matches Alice exactly and Bob at 0.8, and {2,1,0} is the mirror.
func scopedIdentities() []KnownPerson {
	return []KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 2, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{2, 1, 0}}},
	}
}

// twoFaceStub detects two faces: face 0 embeds toward Alice (Alice holds the
// globally best score), face 1 toward Bob.
func twoFaceStub() *scopedStub {
	return &scopedStub{
		faces: []Face{
			{BBox: [4]float64{0, 0, 10, 10}, Landmarks: makeLandmarks()},
			{BBox: [4]float64{30, 0, 10, 10}, Landmarks: makeLandmarks()},
		},
		embs: [][]float32{{1, 2, 0}, {2, 1, 0}},
	}
}

// setOf builds an allowed set; with no ids it returns an empty non-nil map.
func setOf(ids ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// TestRecognizeScopedNilAllowedEqualsRecognize pins the delegation contract:
// a nil allowed set must reproduce Recognize's result exactly on the same
// stub.
func TestRecognizeScopedNilAllowedEqualsRecognize(t *testing.T) {
	e := NewWithInferencer(twoFaceStub(), 0.5)
	e.SetKnown(scopedIdentities())
	img := makeTestImage(t, 64, 64)

	direct, err := e.Recognize(img)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := e.RecognizeScoped(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity-check the baseline so the comparison is not vacuous.
	if len(direct) != 2 || direct[0].Name != "Alice" || direct[1].Name != "Bob" {
		t.Fatalf("unscoped baseline = %+v, want Alice then Bob", direct)
	}
	if !reflect.DeepEqual(direct, scoped) {
		t.Errorf("RecognizeScoped(nil) differs from Recognize:\ndirect = %+v\nscoped = %+v",
			direct, scoped)
	}
}

// TestRecognizeScopedRestrictedIdentitySet checks that a non-nil allowed set
// drops every other identity BEFORE threshold filtering and the best pick:
// the disallowed person can never surface in Name/PersonID/Matches, even when
// it holds the globally best score, and the allowed person wins when its own
// score clears the threshold.
func TestRecognizeScopedRestrictedIdentitySet(t *testing.T) {
	e := NewWithInferencer(twoFaceStub(), 0.5)
	e.SetKnown(scopedIdentities())
	img := makeTestImage(t, 64, 64)

	// Baseline (unscoped): face 0 matches Alice 1.0 with Bob as a 0.8
	// runner-up; face 1 is the mirror image.
	base, err := e.Recognize(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 2 || base[0].Name != "Alice" {
		t.Fatalf("baseline = %+v, want face 0 = Alice", base)
	}
	if len(base[0].Matches) != 2 || base[0].Matches[1].PersonID != "bob" {
		t.Fatalf("baseline face 0 matches = %+v, want Alice then Bob", base[0].Matches)
	}

	// Scoped to Bob: Alice — the globally best-scoring identity on face 0 —
	// must be dropped entirely; Bob's 0.8 clears the 0.5 threshold, so the
	// allowed person wins.
	scoped, err := e.RecognizeScoped(img, setOf("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Fatalf("expected 2 faces, got %d", len(scoped))
	}
	for i, f := range scoped {
		if f.Name == "Alice" || f.PersonID == "alice" {
			t.Errorf("face %d leaked disallowed Alice: name=%q id=%q", i, f.Name, f.PersonID)
		}
		for _, m := range f.Matches {
			if m.PersonID == "alice" || m.Name == "Alice" {
				t.Errorf("face %d leaked disallowed Alice in Matches: %+v", i, m)
			}
		}
	}
	wantBob := Cosine([]float32{1, 2, 0}, []float32{2, 1, 0}) // 0.8
	if f0 := scoped[0]; f0.Name != "Bob" || f0.PersonID != "bob" {
		t.Errorf("face 0 = %s/%s, want Bob/bob", f0.Name, f0.PersonID)
	} else if math.Abs(f0.Confidence-wantBob) > 1e-6 {
		t.Errorf("face 0 confidence = %v, want Bob's %v", f0.Confidence, wantBob)
	} else if len(f0.Matches) != 1 || f0.Matches[0].PersonID != "bob" {
		t.Errorf("face 0 matches = %+v, want only Bob", f0.Matches)
	}
	if f1 := scoped[1]; f1.Name != "Bob" || f1.PersonID != "bob" || f1.Confidence < 0.999 {
		t.Errorf("face 1 = %+v, want Bob ~1.0", f1)
	}

	// Mirror scope: Alice only — Bob must never surface on either face.
	aliceScoped, err := e.RecognizeScoped(img, setOf("alice"))
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range aliceScoped {
		if f.Name != "Alice" || f.PersonID != "alice" {
			t.Errorf("alice scope face %d = %s/%s, want Alice/alice", i, f.Name, f.PersonID)
		}
		for _, m := range f.Matches {
			if m.PersonID == "bob" || m.Name == "Bob" {
				t.Errorf("alice scope face %d leaked Bob in Matches: %+v", i, m)
			}
		}
	}
}

// TestRecognizeScopedEmptyAllowed checks the empty non-nil set: every face is
// unknown, Matches empty, and the confidence hint 0.
func TestRecognizeScopedEmptyAllowed(t *testing.T) {
	e := NewWithInferencer(twoFaceStub(), 0.5)
	e.SetKnown(scopedIdentities())
	faces, err := e.RecognizeScoped(makeTestImage(t, 64, 64), setOf())
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 2 {
		t.Fatalf("expected 2 faces, got %d", len(faces))
	}
	for i, f := range faces {
		if f.Name != "unknown" {
			t.Errorf("face %d name = %q, want unknown", i, f.Name)
		}
		if f.PersonID != "" {
			t.Errorf("face %d person id = %q, want empty", i, f.PersonID)
		}
		if len(f.Matches) != 0 {
			t.Errorf("face %d matches = %+v, want none", i, f.Matches)
		}
		if f.Confidence != 0 {
			t.Errorf("face %d confidence = %v, want 0", i, f.Confidence)
		}
	}
}

// TestRecognizeScopedNearMissHint checks that when no allowed identity clears
// the threshold, the face stays unknown and the confidence hint is the best
// score among ALLOWED identities only — a disallowed near-tie holding the
// globally best score must not leak into it.
func TestRecognizeScopedNearMissHint(t *testing.T) {
	// Both scores sit below the 0.9 threshold and near-tie: Alice ~0.83
	// beats Bob ~0.80 on this query.
	q := []float32{1, 1, 0}
	aliceDir := []float32{1, 0.2, 0}
	bobDir := []float32{0.15, 1, 0}
	e := NewWithInferencer(&scopedStub{
		faces: []Face{{BBox: [4]float64{0, 0, 10, 10}, Landmarks: makeLandmarks()}},
		embs:  [][]float32{q},
	}, 0.9)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{aliceDir}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{bobDir}},
	})
	aliceScore := Cosine(q, aliceDir)
	bobScore := Cosine(q, bobDir)
	if aliceScore >= 0.9 || bobScore >= 0.9 {
		t.Fatalf("test setup: scores not below threshold (%v, %v)", aliceScore, bobScore)
	}
	if aliceScore <= bobScore {
		t.Fatalf("test setup: disallowed Alice does not hold the globally best score")
	}

	// Scoped to Bob: Alice's better score must not surface anywhere.
	faces, err := e.RecognizeScoped(makeTestImage(t, 64, 64), setOf("bob"))
	if err != nil {
		t.Fatal(err)
	}
	f := faces[0]
	if f.Name != "unknown" || f.PersonID != "" || len(f.Matches) != 0 {
		t.Fatalf("face = %+v, want unknown with no matches", f)
	}
	if math.Abs(f.Confidence-bobScore) > 1e-6 {
		t.Errorf("confidence = %v, want allowed Bob's %v", f.Confidence, bobScore)
	}
	if f.Confidence >= aliceScore {
		t.Errorf("confidence %v reflects the disallowed best score %v", f.Confidence, aliceScore)
	}

	// Unscoped contrast: the hint is Alice's globally best score.
	base, err := e.Recognize(makeTestImage(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(base[0].Confidence-aliceScore) > 1e-6 {
		t.Errorf("unscoped confidence = %v, want Alice's %v", base[0].Confidence, aliceScore)
	}
}

// TestRecognizeScopedConcurrent runs RecognizeScoped from two goroutines with
// different allowed sets against one engine; each call must only ever see its
// own scope. The engine's existing RWMutex design (snapshot reads under
// RLock, copy-on-write SetKnown) makes the concurrent reads safe.
func TestRecognizeScopedConcurrent(t *testing.T) {
	e := NewWithInferencer(twoFaceStub(), 0.5)
	e.SetKnown(scopedIdentities())
	img := makeTestImage(t, 64, 64)

	const iters = 25
	run := func(allowed map[string]struct{}, want string) error {
		for i := 0; i < iters; i++ {
			faces, err := e.RecognizeScoped(img, allowed)
			if err != nil {
				return err
			}
			if len(faces) != 2 {
				return fmt.Errorf("expected 2 faces, got %d", len(faces))
			}
			for _, f := range faces {
				if f.Name != want {
					return fmt.Errorf("leaked cross-scope identity: name=%q want %q", f.Name, want)
				}
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = run(setOf("alice"), "Alice") }()
	go func() { defer wg.Done(); errs[1] = run(setOf("bob"), "Bob") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}
