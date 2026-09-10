package models

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// buildTestZip creates a zip archive in memory with the given entry names.
func buildTestZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestEnsureBothPresent(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"det.onnx", "emb.onnx"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Ensure(dir, "det.onnx", "emb.onnx", Options{Auto: true}); err != nil {
		t.Fatalf("Ensure with both models present should succeed: %v", err)
	}
}

func TestEnsureAutoDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, "det.onnx", "emb.onnx", Options{Auto: false}); err != nil {
		t.Fatalf("Ensure with Auto=false should return nil: %v", err)
	}
}

func TestEnsureDownloadsMissingModels(t *testing.T) {
	zipBytes := buildTestZip(t, map[string]string{
		detEntry: "detector-bytes",
		embEntry: "embedder-bytes",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := Ensure(dir, "det.onnx", "emb.onnx", Options{Auto: true, URL: srv.URL}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, name := range []string{"det.onnx", "emb.onnx"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		want := "detector-bytes"
		if name == "emb.onnx" {
			want = "embedder-bytes"
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
	// Temp download file must be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, ".pack-download.tmp")); !os.IsNotExist(err) {
		t.Errorf("temp download file should be removed")
	}
}

func TestEnsureNeverClobbersExisting(t *testing.T) {
	zipBytes := buildTestZip(t, map[string]string{
		detEntry: "new-bytes",
		embEntry: "embedder-bytes",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "det.onnx"), []byte("old-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir, "det.onnx", "emb.onnx", Options{Auto: true, URL: srv.URL}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "det.onnx"))
	if string(got) != "old-bytes" {
		t.Errorf("existing model should not be clobbered, got %q", got)
	}
}

func TestEnsurePackMissingEntry(t *testing.T) {
	zipBytes := buildTestZip(t, map[string]string{detEntry: "detector-bytes"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := Ensure(dir, "det.onnx", "emb.onnx", Options{Auto: true, URL: srv.URL})
	if err == nil {
		t.Fatal("expected error when pack lacks an entry")
	}
	// The successfully extractable entry may still land; the key point is a
	// clear error. No temp files should remain.
	if _, err := os.Stat(filepath.Join(dir, ".pack-download.tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file should be removed after failure")
	}
}
