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
