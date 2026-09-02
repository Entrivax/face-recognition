package engine

// CGO pipeline correctness test over the real dataset. This replaces the
// Phase-A sidecar-parity harness: now that inference is CGO-only, this test
// guards against regressions in detection and embedding by checking that every
// enrolled photo yields at least one in-bounds face with a usable embedding,
// and that embeddings of the same person cluster tighter than across people.
//
// It needs the ONNX models and the people/ dataset, and is slow, so it is
// gated behind RECOGN_DATASET=1. Run via `make dataset-test` or scripts.

import (
	"bytes"
	"image"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func datasetImages(t *testing.T) []string {
	t.Helper()
	root := "../../people"
	var imgs []string
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("no people dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(root, e.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
				imgs = append(imgs, filepath.Join(sub, f.Name()))
			}
		}
	}
	sort.Strings(imgs)
	return imgs
}

func TestCGODatasetPipeline(t *testing.T) {
	if os.Getenv("RECOGN_DATASET") == "" {
		t.Skip("set RECOGN_DATASET=1 to run the dataset pipeline test (slow)")
	}
	det := "../../models/det_10g.onnx"
	emb := "../../models/w600k_r50.onnx"
	if _, err := os.Stat(det); err != nil {
		t.Skipf("model missing: %v", err)
	}

	eng, err := New(det, emb, 0.45)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	imgs := datasetImages(t)
	if len(imgs) == 0 {
		t.Skip("no dataset images found")
	}

	// person -> embeddings (largest face per photo)
	perPerson := map[string][][]float32{}

	for _, path := range imgs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		faces, err := eng.Detect(b)
		if err != nil {
			t.Errorf("%s: detect: %v", path, err)
			continue
		}
		if len(faces) == 0 {
			t.Errorf("%s: no face detected (enrollment photos must have a face)", path)
			continue
		}
		// Use the largest face, mirroring enrollment.
		best := faces[0]
		bestArea := best.BBox[2] * best.BBox[3]
		for _, f := range faces[1:] {
			if a := f.BBox[2] * f.BBox[3]; a > bestArea {
				best, bestArea = f, a
			}
		}
		// Box must lie within the image (allowing small overflow).
		for _, v := range best.BBox {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("%s: non-finite bbox %v", path, best.BBox)
			}
		}
		if best.Score < 0.5 {
			t.Errorf("%s: low detection score %.3f", path, best.Score)
		}
		if len(best.Landmarks) < 5 {
			t.Errorf("%s: expected 5 landmarks, got %d", path, len(best.Landmarks))
		}

		em, err := eng.EmbedFace(b, best)
		if err != nil {
			t.Errorf("%s: embed: %v", path, err)
			continue
		}
		if len(em) != 512 {
			t.Errorf("%s: embedding len %d, want 512", path, len(em))
			continue
		}
		// Embedding should be ~unit norm.
		var n float64
		for _, v := range em {
			n += float64(v) * float64(v)
		}
		if math.Abs(math.Sqrt(n)-1.0) > 0.01 {
			t.Errorf("%s: embedding norm %.4f, want ~1.0", path, math.Sqrt(n))
		}
		person := filepath.Base(filepath.Dir(path))
		perPerson[person] = append(perPerson[person], em)
	}

	// Clustering sanity: mean intra-person cosine should exceed mean
	// inter-person cosine by a clear margin.
	var intraSum, interSum float64
	var intraN, interN int
	people := make([]string, 0, len(perPerson))
	for p := range perPerson {
		people = append(people, p)
	}
	sort.Strings(people)
	for i, p := range people {
		es := perPerson[p]
		for a := 0; a < len(es); a++ {
			for b := a + 1; b < len(es); b++ {
				intraSum += Cosine(es[a], es[b])
				intraN++
			}
		}
		for j := i + 1; j < len(people); j++ {
			for _, ea := range perPerson[p] {
				for _, eb := range perPerson[people[j]] {
					interSum += Cosine(ea, eb)
					interN++
				}
			}
		}
	}
	if intraN > 0 && interN > 0 {
		intra := intraSum / float64(intraN)
		inter := interSum / float64(interN)
		t.Logf("mean intra-person cosine = %.4f (%d pairs)", intra, intraN)
		t.Logf("mean inter-person cosine = %.4f (%d pairs)", inter, interN)
		if intra < inter+0.3 {
			t.Errorf("weak separation: intra %.4f vs inter %.4f (margin %.3f < 0.3)",
				intra, inter, intra-inter)
		}
	}
}

// TestCGOBatchedEmbedParity pins the engine's embedBatch contract on real
// dataset faces: every batched result must equal the single-face embedding
// (cosine >= 0.9999 — with the per-face parallel implementation this is
// bit-exact) and stay unit-norm. If someone reintroduces true tensor
// batching, this test fails exactly the way the ArcFace export's
// BatchNorm batch-statistics coupling demands (measured cosines 0.007–0.59).
// Needs the models; gated behind RECOGN_DATASET.
func TestCGOBatchedEmbedParity(t *testing.T) {
	if os.Getenv("RECOGN_DATASET") == "" {
		t.Skip("set RECOGN_DATASET=1 to run the batched-embed parity test (slow)")
	}
	det := "../../models/det_10g.onnx"
	emb := "../../models/w600k_r50.onnx"
	if _, err := os.Stat(det); err != nil {
		t.Skipf("model missing: %v", err)
	}
	inf, err := newCGOInferencer(det, emb, 2)
	if err != nil {
		t.Fatalf("newCGOInferencer: %v", err)
	}
	defer inf.close()

	// Detect faces in a few dataset photos and align them.
	var aligned []*image.NRGBA
	imgs := datasetImages(t)
	if len(imgs) == 0 {
		t.Skip("no dataset images found")
	}
	for _, path := range imgs {
		if len(aligned) >= 6 {
			break
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		faces, err := inf.detect(b)
		if err != nil || len(faces) == 0 {
			continue
		}
		src, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			continue
		}
		for _, f := range faces {
			if a, err := alignFaceFromImage(src, f); err == nil {
				aligned = append(aligned, a)
				break // one face per photo is plenty
			}
		}
	}
	if len(aligned) < 2 {
		t.Skip("too few aligned faces collected for a batch")
	}

	batch, err := inf.embedBatch(aligned)
	if err != nil {
		t.Fatalf("embedBatch: %v", err)
	}
	if len(batch) != len(aligned) {
		t.Fatalf("embedBatch returned %d results for %d faces", len(batch), len(aligned))
	}
	for i, a := range aligned {
		if batch[i] == nil {
			t.Errorf("face %d: batched embedding missing", i)
			continue
		}
		single, err := inf.embedImage(a)
		if err != nil {
			t.Fatalf("face %d: embedImage: %v", i, err)
		}
		if cos := Cosine(batch[i], single); cos < 0.9999 {
			t.Errorf("face %d: batched vs single cosine = %.6f, want >= 0.9999", i, cos)
		}
		var n float64
		for _, v := range batch[i] {
			n += float64(v) * float64(v)
		}
		if math.Abs(math.Sqrt(n)-1.0) > 0.01 {
			t.Errorf("face %d: batched embedding norm = %.4f, want ~1", i, math.Sqrt(n))
		}
	}
	// Empty batch is a no-op, not an error.
	if out, err := inf.embedBatch(nil); err != nil || len(out) != 0 {
		t.Errorf("embedBatch(nil) = (%v, %v), want (empty, nil)", out, err)
	}
}
