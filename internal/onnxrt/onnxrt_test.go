package onnxrt

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests need the real ONNX models; they skip if models/ is absent.

func modelsDir(t *testing.T) string {
	t.Helper()
	dir := "../../models"
	if _, err := os.Stat(filepath.Join(dir, "w600k_r50.onnx")); err != nil {
		t.Skip("models not present; run `make models`")
	}
	return dir
}

func TestOpenRunCloseEmbedder(t *testing.T) {
	dir := modelsDir(t)
	s, err := Open(filepath.Join(dir, "w600k_r50.onnx"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	in := make([]float32, 1*3*112*112)
	out, err := s.Run(in, []int64{1, 3, 112, 112})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 output, got %d", len(out))
	}
	if len(out[0].Data) != 512 {
		t.Errorf("embedding length = %d, want 512", len(out[0].Data))
	}
	if len(out[0].Dims) != 2 || out[0].Dims[1] != 512 {
		t.Errorf("embedding dims = %v", out[0].Dims)
	}
}

func TestDetectorOutputShapes(t *testing.T) {
	dir := modelsDir(t)
	s, err := Open(filepath.Join(dir, "det_10g.onnx"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	in := make([]float32, 1*3*640*640)
	out, err := s.Run(in, []int64{1, 3, 640, 640})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 9 {
		t.Fatalf("expected 9 outputs, got %d", len(out))
	}
	// First three (scores) should be [N,1] with N = 12800/3200/800.
	wantN := []int64{12800, 3200, 800}
	for i := 0; i < 3; i++ {
		if out[i].Dims[0] != wantN[i] {
			t.Errorf("score output %d dims = %v, want N=%d", i, out[i].Dims, wantN[i])
		}
	}
}

func TestRunShapeValidation(t *testing.T) {
	dir := modelsDir(t)
	s, err := Open(filepath.Join(dir, "w600k_r50.onnx"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	// Mismatched element count should error, not crash.
	if _, err := s.Run([]float32{1, 2, 3}, []int64{1, 3, 112, 112}); err == nil {
		t.Errorf("expected error for mismatched shape, got nil")
	}
	// Empty shape.
	if _, err := s.Run(nil, nil); err == nil {
		t.Errorf("expected error for empty shape, got nil")
	}
}

// TestRepeatedRunNoLeak runs the embedder many times; a gross leak in the C
// shim would show up as runaway allocations. We just check it completes and
// produces consistent output length (RSS inspection is environment-dependent).
func TestRepeatedRunNoLeak(t *testing.T) {
	dir := modelsDir(t)
	s, err := Open(filepath.Join(dir, "w600k_r50.onnx"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	in := make([]float32, 1*3*112*112)
	for i := 0; i < 50; i++ {
		out, err := s.Run(in, []int64{1, 3, 112, 112})
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if len(out[0].Data) != 512 {
			t.Fatalf("Run %d: bad length %d", i, len(out[0].Data))
		}
	}
}

func TestOpenMissingModel(t *testing.T) {
	if _, err := Open("/nonexistent/model.onnx"); err == nil {
		t.Errorf("expected error opening missing model")
	}
}
