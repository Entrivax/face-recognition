package engine

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0.0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1.0},
		{"empty", nil, nil, 0.0},
		{"mismatched length", []float32{1, 2}, []float32{1}, 0.0},
	}
	for _, c := range cases {
		if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: Cosine = %v, want %v", c.name, got, c.want)
		}
	}
	// Normalisation invariance: scaled vectors should give ~1.
	a := []float32{0.3, 0.4, 0.5}
	b := []float32{3, 4, 5}
	if got := Cosine(a, b); math.Abs(got-1.0) > 1e-5 {
		t.Errorf("scaled vectors: Cosine = %v, want ~1", got)
	}
}

func TestMatchEmbeddingThreshold(t *testing.T) {
	e := &Engine{thresh: 0.5}
	e.SetKnown([]KnownPerson{
		{ID: "alice", Name: "Alice", Embeddings: [][]float32{{1, 0, 0}}},
		{ID: "bob", Name: "Bob", Embeddings: [][]float32{{0, 1, 0}}},
	})

	// Query close to Alice.
	m, ok := e.MatchEmbedding([]float32{0.9, 0.1, 0})
	if !ok || m.Name != "Alice" {
		t.Errorf("expected Alice match, got %+v ok=%v", m, ok)
	}
	// Query orthogonal to both enrolled embeddings -> score ~0, below threshold.
	m, ok = e.MatchEmbedding([]float32{0, 0, 1})
	if ok {
		t.Errorf("expected no match, got %+v (score %v)", m, m.Score)
	}
	// Empty identity set never matches.
	e.SetKnown(nil)
	if _, ok := e.MatchEmbedding([]float32{1, 0, 0}); ok {
		t.Errorf("empty known set should never match")
	}
}

func TestUmeyamaIdentity(t *testing.T) {
	// If src landmarks already equal the template, the transform should be
	// ~identity (scale 1, no translation) mapping template -> template.
	src := make([][2]float64, 5)
	for i, p := range arcfaceTemplate {
		src[i] = [2]float64{float64(p[0]), float64(p[1])}
	}
	M := umeyama(src, arcfaceTemplate)
	// a≈1, b≈0, tx≈0, ty≈0
	if math.Abs(M[0][0]-1) > 1e-6 || math.Abs(M[0][1]) > 1e-6 {
		t.Errorf("expected identity rotation/scale, got %v", M)
	}
	if math.Abs(M[0][2]) > 1e-4 || math.Abs(M[1][2]) > 1e-4 {
		t.Errorf("expected ~zero translation, got tx=%v ty=%v", M[0][2], M[1][2])
	}
}

func TestInvertAffineRoundTrip(t *testing.T) {
	m := [2][3]float64{{0.8, -0.2, 10}, {0.2, 0.8, -5}}
	inv := invertAffine(m)
	// Compose m then inv on a point; should return the point.
	px, py := 37.0, 52.0
	// forward
	fx := m[0][0]*px + m[0][1]*py + m[0][2]
	fy := m[1][0]*px + m[1][1]*py + m[1][2]
	// back
	bx := inv[0][0]*fx + inv[0][1]*fy + inv[0][2]
	by := inv[1][0]*fx + inv[1][1]*fy + inv[1][2]
	if math.Abs(bx-px) > 1e-6 || math.Abs(by-py) > 1e-6 {
		t.Errorf("round trip failed: (%v,%v) -> (%v,%v) -> (%v,%v)", px, py, fx, fy, bx, by)
	}
}

func TestCheckModels(t *testing.T) {
	if err := CheckModels("/nonexistent/a.onnx", "/nonexistent/b.onnx"); err == nil {
		t.Errorf("expected error for missing models")
	}
}

func TestLargestFace(t *testing.T) {
	faces := []Face{
		{BBox: [4]float64{0, 0, 10, 10}}, // 100
		{BBox: [4]float64{5, 5, 40, 30}}, // 1200 — largest
		{BBox: [4]float64{1, 1, 20, 20}}, // 400
	}
	best, ok := LargestFace(faces)
	if !ok || best.BBox != faces[1].BBox {
		t.Fatalf("LargestFace = %+v ok=%v, want %+v", best, ok, faces[1])
	}
	// Single face.
	best, ok = LargestFace(faces[:1])
	if !ok || best.BBox != faces[0].BBox {
		t.Fatalf("single face: got %+v ok=%v", best, ok)
	}
	// Empty set.
	if _, ok := LargestFace(nil); ok {
		t.Fatal("empty set should report ok=false")
	}
}
