package enroll

import (
	"os"
	"path/filepath"
	"testing"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// stubEngine implements FaceEngine without any real inference.
type stubEngine struct {
	faces []engine.Face
}

func (s *stubEngine) Detect([]byte) ([]engine.Face, error) { return s.faces, nil }
func (s *stubEngine) EmbedFace([]byte, engine.Face) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

func testFace() []engine.Face {
	return []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}
}

// TestScanIncrementalByHash checks that a rescan skips unchanged files by
// content hash and re-enrolls only changed ones.
func TestScanIncrementalByHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "Alice", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("a.jpg", "image-a")
	writeFile("b.jpg", "image-b")

	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}

	res, err := Scan(eng, database, Options{PeopleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 2 || res.PhotosKept != 0 {
		t.Fatalf("first scan: expected added=2 kept=0, got %+v", res)
	}

	// Unchanged rescan: everything skipped by hash.
	res, err = Scan(eng, database, Options{PeopleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 0 || res.PhotosKept != 2 {
		t.Fatalf("second scan: expected added=0 kept=2, got %+v", res)
	}

	// One file changes: exactly that one is re-enrolled.
	writeFile("a.jpg", "image-a-v2")
	res, err = Scan(eng, database, Options{PeopleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 1 || res.PhotosKept != 1 {
		t.Fatalf("third scan: expected added=1 kept=1, got %+v", res)
	}
}

// TestEnrollBytesWritesImage checks that a successful upload lands in the
// people folder with a content-derived name, the DB points at the same
// basename, and a following Scan treats the file as already enrolled.
func TestEnrollBytesWritesImage(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}
	img := []byte("uploaded-image-bytes")

	rel, err := EnrollBytes(eng, database, peopleDir, "Bob", "photo.PNG", img)
	if err != nil {
		t.Fatal(err)
	}
	// The saved name is content-derived: hash prefix + sniffed extension
	// (fake bytes cannot be sniffed, so the fallback .jpg applies).
	want := filepath.Join("Bob", db.HashBytes(img)[:12]+".jpg")
	if rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	if _, err := os.Stat(filepath.Join(peopleDir, rel)); err != nil {
		t.Fatalf("image not written: %v", err)
	}
	if got := database.PhotoHash("Bob", db.HashBytes(img)[:12]+".jpg"); got != db.HashBytes(img) {
		t.Fatalf("DB hash mismatch for enrolled photo")
	}

	// A folder rescan must recognize the written file (kept, not re-added).
	res, err := Scan(eng, database, Options{PeopleDir: peopleDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 0 || res.PhotosKept != 1 {
		t.Fatalf("rescan after upload: expected added=0 kept=1, got %+v", res)
	}

	// Re-uploading the same bytes must stay idempotent.
	if _, err := EnrollBytes(eng, database, peopleDir, "Bob", "other.jpg", img); err != nil {
		t.Fatal(err)
	}
	p := database.Get("Bob")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("expected exactly 1 photo after duplicate upload, got %+v", p)
	}
}

func TestEnrollBytesRejections(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}

	if _, err := EnrollBytes(eng, database, "", "Bob", "x.jpg", []byte("img")); err == nil {
		t.Error("expected error when people dir is not configured")
	}
	for _, name := range []string{"", "..", ".", "a/b", `a\b`, ".hidden", "a\nb"} {
		if _, err := EnrollBytes(eng, database, peopleDir, name, "x.jpg", []byte("img")); err == nil {
			t.Errorf("expected rejection for name %q", name)
		}
	}
	// A no-face upload must not write anything.
	if _, err := EnrollBytes(&stubEngine{}, database, peopleDir, "Bob", "x.jpg", []byte("img")); err == nil {
		t.Error("expected error when no face is detected")
	}
	if _, err := os.Stat(filepath.Join(peopleDir, "Bob")); !os.IsNotExist(err) {
		t.Errorf("failed enrolls must not create a people folder, err=%v", err)
	}
}

func TestCheckName(t *testing.T) {
	for _, name := range []string{"Alice", "Alice Smith", "Ünicode", "O'Brien", "a-b_c"} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "  ", ".", "..", "a/b", `a\b`, ".hidden", "a\x00b", "a\nb"} {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) = nil, want error", name)
		}
	}
}
