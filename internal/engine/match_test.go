package engine

import (
	"fmt"
	"image"
	"testing"
)

func TestMatchAll(t *testing.T) {
	e := &Engine{thresh: 0.5}
	e.SetKnown([]KnownPerson{
		// Alice has two embeddings: the per-person best must win, and she
		// must appear exactly once.
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}, {0.99, 0.1, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{0, 1, 0}}},
		{ID: "carol", Name: "Carol", Embeddings: [][]float32{{0.8, 0.6, 0}}}, // 0.8 to the query
	})

	got := e.MatchAll([]float32{1, 0, 0})
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates (Alice, Carol), got %+v", got)
	}
	if got[0].Name != "Alice" || got[0].Score < 0.999 {
		t.Errorf("top candidate = %+v, want Alice ~1.0", got[0])
	}
	if got[1].Name != "Carol" || got[1].Score < 0.799 {
		t.Errorf("second candidate = %+v, want Carol ~0.8", got[1])
	}
	for _, m := range got {
		if m.PersonID == "" {
			t.Errorf("candidate missing PersonID: %+v", m)
		}
	}

	// Query orthogonal to everyone → nothing above the threshold.
	if got := e.MatchAll([]float32{0, 0, 1}); len(got) != 0 {
		t.Errorf("expected no candidates, got %+v", got)
	}

	// Equal scores → deterministic tie-break by name.
	tie := &Engine{thresh: 0.5}
	tie.SetKnown([]KnownPerson{
		{ID: "b", Name: "Bob", Embeddings: [][]float32{{1, 0, 0}}},
		{ID: "a", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	})
	got = tie.MatchAll([]float32{1, 0, 0})
	if len(got) != 2 || got[0].Name != "Alice" || got[1].Name != "Bob" {
		t.Errorf("tie-break: got %+v, want Alice then Bob", got)
	}

	// Empty identity set → no candidates.
	e.SetKnown(nil)
	if got := e.MatchAll([]float32{1, 0, 0}); len(got) != 0 {
		t.Errorf("empty known set should give no candidates, got %+v", got)
	}
}

// fakeInferencer serves pre-set detections and a fixed embedding, so
// Recognize can be exercised without models.
type fakeInferencer struct {
	faces []Face
}

func (f *fakeInferencer) detect([]byte) ([]Face, error) { return f.faces, nil }
func (f *fakeInferencer) embedImage(*image.NRGBA) ([]float32, error) {
	return []float32{1, 0, 0}, nil // matches Alice in the test identity set
}
func (f *fakeInferencer) embedBatch(aligned []*image.NRGBA) ([][]float32, error) {
	out := make([][]float32, len(aligned))
	for i, a := range aligned {
		if a == nil {
			continue
		}
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
func (f *fakeInferencer) ping() error { return nil }
func (f *fakeInferencer) close()      {}

// errEmbedInferencer detects fine but always fails the embedding step.
type errEmbedInferencer struct {
	faces []Face
}

func (f *errEmbedInferencer) detect([]byte) ([]Face, error) { return f.faces, nil }
func (f *errEmbedInferencer) embedImage(*image.NRGBA) ([]float32, error) {
	return nil, fmt.Errorf("embed boom")
}
func (f *errEmbedInferencer) embedBatch([]*image.NRGBA) ([][]float32, error) {
	return nil, fmt.Errorf("embed boom")
}
func (f *errEmbedInferencer) ping() error { return nil }
func (f *errEmbedInferencer) close()      {}

func TestRecognizePopulatesMatches(t *testing.T) {
	e := NewWithInferencer(&fakeInferencer{
		faces: []Face{{BBox: [4]float64{0, 0, 10, 10}}},
	}, 0.5)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{0, 1, 0}}},
	})

	faces, err := e.Recognize(makeTestImage(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 1 {
		t.Fatalf("expected 1 face, got %d", len(faces))
	}
	f := faces[0]
	if len(f.Matches) != 1 {
		t.Fatalf("expected only Alice above threshold, got %+v", f.Matches)
	}
	if f.Name != "Alice" || f.PersonID != "alice" {
		t.Errorf("identity = %s/%s, want Alice/alice", f.Name, f.PersonID)
	}
	if f.Confidence < 0.999 {
		t.Errorf("confidence = %v, want ~1", f.Confidence)
	}
	if f.Matches[0].Name != "Alice" || f.Matches[0].Score < 0.999 {
		t.Errorf("top match = %+v, want Alice ~1.0", f.Matches[0])
	}
}

func TestRecognizeEmbedFailureLeavesUnknown(t *testing.T) {
	e := NewWithInferencer(&errEmbedInferencer{
		faces: []Face{{BBox: [4]float64{0, 0, 10, 10}}},
	}, 0.5)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	})

	faces, err := e.Recognize(makeTestImage(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if faces[0].Name != "unknown" {
		t.Errorf("name = %q, want unknown", faces[0].Name)
	}
	if len(faces[0].Matches) != 0 {
		t.Errorf("expected no matches after embed failure, got %+v", faces[0].Matches)
	}
}

// batchFallbackInferencer fails every embedBatch call but embeds fine
// per-face, exercising Recognize's whole-batch fallback path.
type batchFallbackInferencer struct {
	faces []Face
}

func (f *batchFallbackInferencer) detect([]byte) ([]Face, error) { return f.faces, nil }
func (f *batchFallbackInferencer) embedImage(*image.NRGBA) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}
func (f *batchFallbackInferencer) embedBatch([]*image.NRGBA) ([][]float32, error) {
	return nil, fmt.Errorf("batch boom")
}
func (f *batchFallbackInferencer) ping() error { return nil }
func (f *batchFallbackInferencer) close()      {}

// TestRecognizeBatchFailureFallsBackToPerFace checks that a whole-batch
// failure degrades to per-face embedding without changing the result.
func TestRecognizeBatchFailureFallsBackToPerFace(t *testing.T) {
	e := NewWithInferencer(&batchFallbackInferencer{
		faces: []Face{{BBox: [4]float64{0, 0, 10, 10}}},
	}, 0.5)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
	})
	faces, err := e.Recognize(makeTestImage(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 1 || faces[0].Name != "Alice" {
		t.Fatalf("identity = %+v, want Alice", faces[0])
	}
}

// nilAwareBatchInferencer returns a directionally distinct embedding per
// input index (face 0 → Alice's direction, face 1 → Bob's) and nil for nil
// inputs, letting Recognize's face→embedding mapping be verified without
// models. (Embeddings must be non-collinear: cosine ignores magnitude.)
type nilAwareBatchInferencer struct {
	faces []Face
}

func (f *nilAwareBatchInferencer) detect([]byte) ([]Face, error) { return f.faces, nil }
func (f *nilAwareBatchInferencer) embedImage(*image.NRGBA) ([]float32, error) {
	return []float32{1, 2, 0}, nil
}
func (f *nilAwareBatchInferencer) embedBatch(aligned []*image.NRGBA) ([][]float32, error) {
	out := make([][]float32, len(aligned))
	for i, a := range aligned {
		if a == nil {
			continue // failed alignment stays nil
		}
		if i%2 == 0 {
			out[i] = []float32{1, 2, 0} // Alice's direction
		} else {
			out[i] = []float32{2, 1, 0} // Bob's direction
		}
	}
	return out, nil
}
func (f *nilAwareBatchInferencer) ping() error { return nil }
func (f *nilAwareBatchInferencer) close()      {}

// TestRecognizeMultiFaceBatchedMapping feeds two detectable faces and
// directionally distinct per-face embeddings; each face must receive its own
// identity, and the face→embedding mapping must be by detection index (no
// cross-wiring). A high threshold keeps Matches to the single best identity.
func TestRecognizeMultiFaceBatchedMapping(t *testing.T) {
	e := NewWithInferencer(&nilAwareBatchInferencer{
		faces: []Face{
			{BBox: [4]float64{0, 0, 10, 10}, Landmarks: makeLandmarks()},
			{BBox: [4]float64{30, 0, 10, 10}, Landmarks: makeLandmarks()},
		},
	}, 0.9)
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 2, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{2, 1, 0}}},
	})
	faces, err := e.Recognize(makeTestImage(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 2 {
		t.Fatalf("expected 2 faces, got %d", len(faces))
	}
	if faces[0].Name != "Alice" || faces[0].PersonID != "alice" {
		t.Errorf("face 0 = %+v, want Alice", faces[0])
	}
	if faces[1].Name != "Bob" || faces[1].PersonID != "bob" {
		t.Errorf("face 1 = %+v, want Bob", faces[1])
	}
	// Embeddings map back per index (directions, not swapped).
	if faces[0].Embedding == nil || faces[1].Embedding == nil {
		t.Fatal("embeddings must be populated")
	}
	if c := Cosine(faces[0].Embedding, []float32{1, 2, 0}); c < 0.9999 {
		t.Errorf("face 0 embedding direction wrong (cosine %.4f to Alice's vector)", c)
	}
	if c := Cosine(faces[1].Embedding, []float32{2, 1, 0}); c < 0.9999 {
		t.Errorf("face 1 embedding direction wrong (cosine %.4f to Bob's vector)", c)
	}
}

// makeLandmarks returns 5 template-matching landmarks so alignment succeeds.
func makeLandmarks() [][2]float64 {
	out := make([][2]float64, 5)
	for i, p := range arcfaceTemplate {
		out[i] = [2]float64{float64(p[0]), float64(p[1])}
	}
	return out
}
