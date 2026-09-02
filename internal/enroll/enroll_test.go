package enroll

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	// Provenance: the thumbnail came from the saved upload (content-derived name).
	if p.ThumbSrc != db.HashBytes(img)[:12]+".png" {
		t.Fatalf("ThumbSrc = %q, want the saved upload basename", p.ThumbSrc)
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
	if b := decoded.Bounds(); b.Dx() != ThumbSize || b.Dy() != ThumbSize {
		t.Fatalf("thumbnail %dx%d, want %dx%d", b.Dx(), b.Dy(), ThumbSize, ThumbSize)
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
	if p.ThumbSrc != "c.jpg" {
		t.Fatalf("ThumbSrc = %q, want c.jpg", p.ThumbSrc)
	}
	if _, err := os.Stat(filepath.Join(dbDir, "thumbs", p.Thumb)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
}

// TestScanPrunesMissingPhotos checks that Prune drops DB photo entries whose
// files vanished (one by one, or with the whole folder) and that a scan
// without Prune leaves them untouched.
func TestScanPrunesMissingPhotos(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}
	b1 := pngBytes(t, 40, 40)
	b2 := pngBytes(t, 41, 41)
	name1 := db.HashBytes(b1)[:12] + ".png"
	name2 := db.HashBytes(b2)[:12] + ".png"
	if _, err := EnrollBytes(eng, database, peopleDir, "Bob", "a.png", b1); err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollBytes(eng, database, peopleDir, "Bob", "b.png", b2); err != nil {
		t.Fatal(err)
	}

	// Without Prune, a deleted file is left alone in the DB.
	if err := os.Remove(filepath.Join(peopleDir, "Bob", name2)); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(eng, database, Options{PeopleDir: peopleDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 0 {
		t.Fatalf("prune without opt-in: pruned %d", res.PhotosPruned)
	}
	if p := database.Get("Bob"); p == nil || len(p.Photos) != 2 {
		t.Fatalf("expected 2 photos without prune, got %+v", p)
	}

	// With Prune, the stale entry is dropped and the live one kept.
	res, err = Scan(eng, database, Options{PeopleDir: peopleDir, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 1 {
		t.Fatalf("expected 1 pruned, got %+v", res)
	}
	p := database.Get("Bob")
	if p == nil || len(p.Photos) != 1 || p.Photos[0].Path != name1 {
		t.Fatalf("expected only %q to remain, got %+v", name1, p)
	}

	// A person whose folder vanished entirely loses all photo entries but
	// stays enrolled (with zero photos).
	if err := os.RemoveAll(filepath.Join(peopleDir, "Bob")); err != nil {
		t.Fatal(err)
	}
	res, err = Scan(eng, database, Options{PeopleDir: peopleDir, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 1 {
		t.Fatalf("expected 1 pruned for the missing folder, got %+v", res)
	}
	if p := database.Get("Bob"); p == nil || len(p.Photos) != 0 {
		t.Fatalf("person should remain enrolled with 0 photos, got %+v", p)
	}
}

// slowStubEngine wraps stubEngine with a per-file sleep so parallel scans
// genuinely overlap (and the race detector sees concurrent Detect/EmbedFace).
type slowStubEngine struct {
	faces []engine.Face
	delay time.Duration
}

func (s *slowStubEngine) Detect([]byte) ([]engine.Face, error) {
	time.Sleep(s.delay)
	return s.faces, nil
}
func (s *slowStubEngine) EmbedFace([]byte, engine.Face) ([]float32, error) {
	time.Sleep(s.delay / 2)
	return []float32{0.1, 0.2}, nil
}

// TestScanParallelDeterministic checks that a Workers>1 scan produces exactly
// the serial result: identical counts, identical Skipped ordering, identical
// DB photo sets, and exactly one sequential Progress call per file. Run with
// -race (make test-race) to catch data races in the worker pool.
func TestScanParallelDeterministic(t *testing.T) {
	buildDataset := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, person := range []string{"Alice", "Bob", "Carol"} {
			if err := os.MkdirAll(filepath.Join(dir, person), 0o755); err != nil {
				t.Fatal(err)
			}
			for i, name := range []string{"a.png", "b.png", "c.png"} {
				img := pngBytes(t, 40+i, 40+i)
				if err := os.WriteFile(filepath.Join(dir, person, name), img, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		// One extra real image for Bob: after the first scan it is unchanged
		// (kept), and deleting it later exercises prune/scan paths equally.
		if err := os.WriteFile(filepath.Join(dir, "Bob", "d.png"), pngBytes(t, 60, 60), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	run := func(t *testing.T, workers int) (Result, []string, []db.Person, int) {
		t.Helper()
		dir := buildDataset(t)
		database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		eng := &slowStubEngine{faces: testFace(), delay: 2 * time.Millisecond}
		var progress []string
		var progressMu sync.Mutex
		res, err := Scan(eng, database, Options{
			PeopleDir: dir,
			Workers:   workers,
			Progress: func(person, file string, idx, total int) {
				progressMu.Lock()
				progress = append(progress, person+"/"+file)
				progressMu.Unlock()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var skipped []string
		for _, s := range res.Skipped {
			skipped = append(skipped, s.Person+"/"+filepath.Base(s.Path)+": "+s.Reason)
		}
		people := database.People() // copy out before Close (deferred)
		return res, skipped, people, len(progress)
	}

	// Serial reference (10 files: 3 people x 3 images + 1 extra for Bob).
	wantRes, wantSkipped, wantPeople, wantProgress := run(t, 1)
	if wantProgress != 10 {
		t.Fatalf("serial scan: %d progress calls, want 10", wantProgress)
	}

	for _, workers := range []int{2, 4, 8} {
		gotRes, gotSkipped, gotPeople, gotProgress := run(t, workers)

		if gotRes.PeopleSeen != wantRes.PeopleSeen ||
			gotRes.PhotosAdded != wantRes.PhotosAdded ||
			gotRes.PhotosKept != wantRes.PhotosKept ||
			gotRes.PhotosFailed != wantRes.PhotosFailed ||
			gotRes.PhotosPruned != wantRes.PhotosPruned {
			t.Errorf("workers=%d: result counts differ: %+v vs serial %+v", workers, gotRes, wantRes)
		}
		if len(gotSkipped) != len(wantSkipped) {
			t.Fatalf("workers=%d: skipped length %d, want %d (%v)", workers, len(gotSkipped), len(wantSkipped), gotSkipped)
		}
		for i := range gotSkipped {
			if gotSkipped[i] != wantSkipped[i] {
				t.Errorf("workers=%d: skipped[%d] = %q, want %q", workers, i, gotSkipped[i], wantSkipped[i])
			}
		}
		// DB contents: same people with the same photo sets.
		if len(gotPeople) != len(wantPeople) {
			t.Errorf("workers=%d: people count = %d, want %d", workers, len(gotPeople), len(wantPeople))
		}
		for _, p := range gotPeople {
			wp := findPerson(wantPeople, p.Name)
			if wp == nil || len(wp.Photos) != len(p.Photos) {
				t.Errorf("workers=%d: photos for %q differ from serial run", workers, p.Name)
				continue
			}
			for i := range p.Photos {
				if p.Photos[i].Path != wp.Photos[i].Path || p.Photos[i].Hash != wp.Photos[i].Hash {
					t.Errorf("workers=%d: photo %d of %q differs: %+v vs %+v", workers, i, p.Name, p.Photos[i], wp.Photos[i])
				}
			}
		}
		if gotProgress != wantProgress {
			t.Errorf("workers=%d: %d progress calls, want %d", workers, gotProgress, wantProgress)
		}
	}
}

// findPerson locates a person by name in a snapshot (nil when absent).
func findPerson(people []db.Person, name string) *db.Person {
	for i := range people {
		if people[i].Name == name {
			return &people[i]
		}
	}
	return nil
}

// TestScanParallelProgressIndexes checks the Progress contract under
// parallelism: called once per file, sequentially, with idx counting
// completed files per person up to that person's total.
func TestScanParallelProgressIndexes(t *testing.T) {
	dir := t.TempDir()
	for _, person := range []string{"Alice", "Bob"} {
		if err := os.MkdirAll(filepath.Join(dir, person), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			img := pngBytes(t, 40+i, 40+i)
			name := string(rune('a'+i)) + ".png"
			if err := os.WriteFile(filepath.Join(dir, person, name), img, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	eng := &slowStubEngine{faces: testFace(), delay: time.Millisecond}

	type call struct {
		person, file string
		idx, total   int
	}
	var calls []call
	var mu sync.Mutex
	res, err := Scan(eng, database, Options{
		PeopleDir: dir,
		Workers:   4,
		Progress:  func(person, file string, idx, total int) { mu.Lock(); calls = append(calls, call{person, file, idx, total}); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 10 {
		t.Fatalf("added = %d, want 10", res.PhotosAdded)
	}
	if len(calls) != 10 {
		t.Fatalf("%d progress calls, want 10", len(calls))
	}
	seen := map[string]map[string]bool{}
	idxReached := map[string]int{}
	for _, c := range calls {
		if c.idx < 1 || c.idx > c.total {
			t.Errorf("progress %s/%s: idx %d outside 1..%d", c.person, c.file, c.idx, c.total)
		}
		if seen[c.person] == nil {
			seen[c.person] = map[string]bool{}
		}
		if seen[c.person][c.file] {
			t.Errorf("progress: duplicate call for %s/%s", c.person, c.file)
		}
		seen[c.person][c.file] = true
		idxReached[c.person] = c.idx
	}
	for _, person := range []string{"Alice", "Bob"} {
		if idxReached[person] != 5 {
			t.Errorf("person %s: last progress idx = %d, want 5", person, idxReached[person])
		}
	}
}
