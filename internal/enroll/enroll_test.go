package enroll

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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

// pngBytes renders a deterministic w x h gradient as PNG bytes (real,
// decodable image content, needed for thumbnail generation).
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
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

// TestEnrollBytesGeneratesThumbnail checks that the first successful upload
// also stores a face-crop JPEG sidecar next to the DB file, and that later
// uploads never overwrite it.
func TestEnrollBytesGeneratesThumbnail(t *testing.T) {
	peopleDir := t.TempDir()
	dbDir := t.TempDir()
	database, err := db.Open(filepath.Join(dbDir, "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{30, 20, 40, 50}}}}
	img := pngBytes(t, 120, 120)

	if _, err := EnrollBytes(eng, database, peopleDir, "Bob", "photo.png", img); err != nil {
		t.Fatal(err)
	}
	p := database.Get("Bob")
	if p == nil || p.Thumb == "" {
		t.Fatalf("expected thumbnail sidecar recorded, got %+v", p)
	}
	thumbPath := filepath.Join(dbDir, "thumbs", p.Thumb)
	raw, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("sidecar is not a decodable JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != thumbSize || b.Dy() != thumbSize {
		t.Fatalf("thumbnail %dx%d, want %dx%d", b.Dx(), b.Dy(), thumbSize, thumbSize)
	}

	// A second upload must not overwrite the existing thumbnail.
	before := append([]byte(nil), raw...)
	if _, err := EnrollBytes(eng, database, peopleDir, "Bob", "photo.png", img); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(thumbPath)
	if !bytes.Equal(before, after) {
		t.Error("thumbnail was overwritten on re-enroll")
	}
}

// TestScanBackfillsThumbnail checks that a rescan generates the missing
// thumbnail for an already-enrolled person without re-adding their photos.
func TestScanBackfillsThumbnail(t *testing.T) {
	dir := t.TempDir()
	dbDir := t.TempDir()
	database, err := db.Open(filepath.Join(dbDir, "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	img := pngBytes(t, 100, 100)
	if err := os.MkdirAll(filepath.Join(dir, "Carol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Carol", "c.jpg"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-enroll without a thumbnail, the way an older DB looks.
	if err := database.AddPhoto("Carol", "c.jpg", img, []float32{0.3}); err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{5, 5, 20, 20}}}}

	res, err := Scan(eng, database, Options{PeopleDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 0 || res.PhotosKept != 1 {
		t.Fatalf("backfill scan should keep the photo unchanged, got %+v", res)
	}
	p := database.Get("Carol")
	if p == nil || p.Thumb == "" {
		t.Fatalf("thumbnail not backfilled, got %+v", p)
	}
	if _, err := os.Stat(filepath.Join(dbDir, "thumbs", p.Thumb)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
}
