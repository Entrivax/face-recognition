package enroll

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
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

	rel, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "photo.PNG", img)
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
	if _, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "other.jpg", img); err != nil {
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

	if _, err := EnrollBytes(eng, database, "", "", "Bob", "x.jpg", []byte("img")); err == nil {
		t.Error("expected error when people dir is not configured")
	}
	for _, name := range []string{"", "..", ".", "a/b", `a\b`, ".hidden", "a\nb"} {
		if _, err := EnrollBytes(eng, database, peopleDir, "", name, "x.jpg", []byte("img")); err == nil {
			t.Errorf("expected rejection for name %q", name)
		}
	}
	// A no-face upload must not write anything.
	if _, err := EnrollBytes(&stubEngine{}, database, peopleDir, "", "Bob", "x.jpg", []byte("img")); err == nil {
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

	if _, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "photo.png", img); err != nil {
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
	if _, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "photo.png", img); err != nil {
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
	if _, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "a.png", b1); err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollBytes(eng, database, peopleDir, "", "Bob", "b.png", b2); err != nil {
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
		Progress: func(person, file string, idx, total int) {
			mu.Lock()
			calls = append(calls, call{person, file, idx, total})
			mu.Unlock()
		},
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

// ---- repository-scoped enrollment ----

// TestScanRepoScoped checks that a repo-scoped scan walks only the
// repository's dataset folder (PeopleDir/<RepoID>) and writes through the
// repo-scoped mutators: the repo's persons get NewPersonID-prefixed IDs,
// while legacy flat folders and other repositories stay untouched.
func TestScanRepoScoped(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}

	repo, err := database.CreateRepo("Team")
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.CreateRepo("Other")
	if err != nil {
		t.Fatal(err)
	}

	write := func(dir, person, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, person), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, person, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(peopleDir, repo.ID), "Alice", "a.jpg", "alice-a")
	write(filepath.Join(peopleDir, repo.ID), "Bob", "b.jpg", "bob-b")
	write(filepath.Join(peopleDir, other.ID), "Carol", "c.jpg", "carol-c")
	write(peopleDir, "Flat", "f.jpg", "flat-f") // legacy folder next to the repos

	res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repo.ID})
	if err != nil {
		t.Fatalf("repo scan: %v", err)
	}
	if res.PeopleSeen != 2 || res.PhotosAdded != 2 {
		t.Fatalf("repo scan saw %+v, want PeopleSeen=2 Added=2", res)
	}

	// The repo's persons are repo-scoped with NewPersonID-derived IDs.
	team := database.PeopleByRepo(repo.ID)
	if len(team) != 2 || findPerson(team, "Alice") == nil || findPerson(team, "Bob") == nil {
		t.Fatalf("repo persons: %+v", team)
	}
	for _, p := range team {
		if p.RepoID != repo.ID {
			t.Errorf("person %q RepoID = %q, want %q", p.Name, p.RepoID, repo.ID)
		}
		if want := database.NewPersonID(repo.ID, p.Name); p.ID != want {
			t.Errorf("person %q ID = %q, want %q", p.Name, p.ID, want)
		}
	}
	// Other repositories and the legacy set were not touched.
	if ps := database.PeopleByRepo(other.ID); len(ps) != 0 {
		t.Errorf("other repo got persons from the scan: %+v", ps)
	}
	if database.PeopleCount(other.ID) != 0 {
		t.Errorf("other repo should have no persons after the repo scan")
	}
	if p := database.Get("Flat"); p != nil {
		t.Errorf("legacy folder must not be enrolled by a repo scan, got %+v", p)
	}
	if database.PhotoHash("Alice", "a.jpg") != "" {
		t.Errorf("legacy PhotoHash must not see a repo person")
	}
	if got := database.PhotoHashInRepo(repo.ID, "Alice", "a.jpg"); got != db.HashBytes([]byte("alice-a")) {
		t.Errorf("repo PhotoHash mismatch: %q", got)
	}

	// A legacy scan of the same root enrols only the flat folder (and
	// leaves the repo persons alone). The repo folders themselves are
	// walked too — they are directories — but hold no image files at their
	// root, so they contribute nothing.
	res, err = Scan(eng, database, Options{PeopleDir: peopleDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 1 || findPerson(database.PeopleByRepo(repo.ID), "Bob") == nil {
		t.Fatalf("legacy scan results: %+v", res)
	}
	if p := database.Get("Flat"); p == nil || p.RepoID != "" {
		t.Errorf("legacy scan should enrol the flat folder as a legacy person, got %+v", p)
	}
	if len(database.PeopleByRepo(repo.ID)) != 2 {
		t.Errorf("legacy scan must not add to or remove from the repo")
	}
}

// TestLegacyScanIgnoresRepoPersons checks the read side of the legacy
// scoping rule: a legacy scan resolving a name that also exists as a repo
// person must resolve the LEGACY record — a thumbnail generated for the
// flat folder's photo goes to the legacy person, never to the repo person.
func TestLegacyScanIgnoresRepoPersons(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}
	repo, err := database.CreateRepo("Team")
	if err != nil {
		t.Fatal(err)
	}
	// A repo-scoped DB record for "Alice" with no thumbnail yet (the way a
	// first enrollment whose thumbnail failed looks) — and no files on
	// disk for the repo.
	img := pngBytes(t, 40, 40)
	if err := database.AddPhotoHashedForRepo(repo.ID, "Alice", "a.png", db.HashBytes(img), []float32{0.5}); err != nil {
		t.Fatal(err)
	}
	// A legacy flat folder with the SAME name next to the repo folder.
	if err := os.MkdirAll(filepath.Join(peopleDir, "Alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peopleDir, "Alice", "flat.png"), img, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(eng, database, Options{PeopleDir: peopleDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 1 {
		t.Fatalf("legacy scan: %+v, want 1 added", res)
	}

	// The flat photo belongs to a NEW legacy person, with its thumbnail.
	legacy := database.GetLegacy("Alice")
	if legacy == nil || legacy.RepoID != "" || len(legacy.Photos) != 1 {
		t.Fatalf("legacy Alice: %+v, want one legacy person with 1 photo", legacy)
	}
	if legacy.Thumb == "" || legacy.ThumbSrc != "flat.png" {
		t.Fatalf("legacy Alice thumb = %q src %q, want a generated sidecar from flat.png", legacy.Thumb, legacy.ThumbSrc)
	}
	// The repo person was not touched — old global-lookup behavior would
	// have attributed the legacy photo's thumbnail to them.
	repoAlice := database.GetInRepo(repo.ID, "Alice")
	if repoAlice == nil || len(repoAlice.Photos) != 1 || repoAlice.Photos[0].Path != "a.png" {
		t.Fatalf("repo Alice photos: %+v, want unchanged (1 photo, a.png)", repoAlice)
	}
	if repoAlice.Thumb != "" || repoAlice.ThumbSrc != "" {
		t.Errorf("repo Alice must not gain a thumbnail from the legacy scan: %+v", repoAlice)
	}
}

// TestScanRepoScopedIncrementalByHash checks the incremental contract inside
// a repository: unchanged files keep, a changed file re-enrols.
func TestScanRepoScopedIncrementalByHash(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepo("Team")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(peopleDir, repo.ID, "Alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.jpg", "image-a")
	write("b.jpg", "image-b")
	eng := &stubEngine{faces: testFace()}

	if res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	} else if res.PhotosAdded != 2 {
		t.Fatalf("first scan: expected added=2, got %+v", res)
	}
	if res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	} else if res.PhotosAdded != 0 || res.PhotosKept != 2 {
		t.Fatalf("second scan: expected added=0 kept=2, got %+v", res)
	}
	write("a.jpg", "image-a-v2")
	if res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	} else if res.PhotosAdded != 1 || res.PhotosKept != 1 {
		t.Fatalf("changed-file scan: expected added=1 kept=1, got %+v", res)
	}
}

// TestScanRepoScopedMissingDir checks that a repo-scoped scan on a missing
// dataset folder fails with the ReadDir error (wrapped, so callers can test
// with errors.Is(err, os.ErrNotExist)) instead of silently doing nothing.
func TestScanRepoScopedMissingDir(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepo("Team")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Scan(&stubEngine{faces: testFace()}, database, Options{PeopleDir: peopleDir, RepoID: repo.ID})
	if err == nil {
		t.Fatal("expected an error for a missing repo folder")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error should wrap the ReadDir failure, got %v", err)
	}
	if !strings.Contains(err.Error(), repo.ID) {
		t.Fatalf("error should name the dataset folder, got %v", err)
	}
}

// TestScanRepoScopedPrune checks that prune inside a repository drops only
// that repo's stale entries: a vanished person folder of the repo loses its
// photos, other repositories and legacy persons are untouched.
func TestScanRepoScopedPrune(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: testFace()}
	repoA, err := database.CreateRepo("A")
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := database.CreateRepo("B")
	if err != nil {
		t.Fatal(err)
	}
	b1, b2 := pngBytes(t, 40, 40), pngBytes(t, 41, 41)
	name1, name2 := db.HashBytes(b1)[:12]+".png", db.HashBytes(b2)[:12]+".png"
	if _, err := EnrollBytes(eng, database, peopleDir, repoA.ID, "Bob", "a.png", b1); err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollBytes(eng, database, peopleDir, repoA.ID, "Bob", "b.png", b2); err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollBytes(eng, database, peopleDir, repoB.ID, "Bob", "x.png", b1); err != nil {
		t.Fatal(err)
	}
	if err := database.AddPhoto("LegacyBob", "l.png", b1, []float32{1}); err != nil {
		t.Fatal(err)
	}

	// Without Prune the vanished file stays in the DB.
	if err := os.Remove(filepath.Join(peopleDir, repoA.ID, "Bob", name2)); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repoA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 0 {
		t.Fatalf("prune without opt-in: pruned %d", res.PhotosPruned)
	}

	// With Prune the stale entry of repo A's Bob is dropped.
	res, err = Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repoA.ID, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 1 {
		t.Fatalf("expected 1 pruned, got %+v", res)
	}
	if p := database.GetInRepo(repoA.ID, "Bob"); p == nil || len(p.Photos) != 1 || p.Photos[0].Path != name1 {
		t.Fatalf("expected only %q to remain in repo A's Bob, got %+v", name1, p)
	}
	// The other repo's Bob and the legacy Bob are untouched.
	if p := database.GetInRepo(repoB.ID, "Bob"); p == nil || len(p.Photos) != 1 {
		t.Errorf("repo B's Bob was touched by repo A's prune: %+v", p)
	}
	if p := database.Get("LegacyBob"); p == nil || len(p.Photos) != 1 {
		t.Errorf("legacy Bob was touched by a repo prune: %+v", p)
	}

	// The whole person folder vanishes: repo A's Bob loses all entries but
	// stays enrolled; repo B's Bob survives untouched.
	if err := os.RemoveAll(filepath.Join(peopleDir, repoA.ID, "Bob")); err != nil {
		t.Fatal(err)
	}
	res, err = Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repoA.ID, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosPruned != 1 {
		t.Fatalf("expected 1 pruned for the missing folder, got %+v", res)
	}
	if p := database.GetInRepo(repoA.ID, "Bob"); p == nil || len(p.Photos) != 0 {
		t.Fatalf("repo A's Bob should remain enrolled with 0 photos, got %+v", p)
	}
	if p := database.GetInRepo(repoB.ID, "Bob"); p == nil || len(p.Photos) != 1 {
		t.Errorf("repo B's Bob was touched by the missing-folder prune: %+v", p)
	}
}

// TestEnrollBytesRepoScoped checks a repo-scoped upload: the file lands in
// the repository's dataset folder, the DB write goes through the repo
// mutator, the thumbnail is scoped by person ID, and the same person name
// in two repositories stays two independent people.
func TestEnrollBytesRepoScoped(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := database.CreateRepo("A")
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := database.CreateRepo("B")
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}
	img := pngBytes(t, 80, 80) // real image bytes: the thumbnail crop needs one

	rel, err := EnrollBytes(eng, database, peopleDir, repoA.ID, "Bob", "photo.PNG", img)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repoA.ID, "Bob", db.HashBytes(img)[:12]+".png") // real PNG bytes → sniffed extension
	if rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	if _, err := os.Stat(filepath.Join(peopleDir, rel)); err != nil {
		t.Fatalf("image not written: %v", err)
	}
	p := database.GetInRepo(repoA.ID, "Bob")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("repo person not enrolled: %+v", p)
	}
	if p.RepoID != repoA.ID {
		t.Errorf("RepoID = %q, want %q", p.RepoID, repoA.ID)
	}
	if got := database.PhotoHashInRepo(repoA.ID, "Bob", db.HashBytes(img)[:12]+".png"); got != db.HashBytes(img) {
		t.Fatalf("DB hash mismatch for the repo-enrolled photo")
	}
	if database.PhotoHash("Bob", db.HashBytes(img)[:12]+".png") != "" {
		t.Fatalf("legacy PhotoHash must not see a repo person")
	}
	if p.Thumb == "" {
		t.Fatalf("expected a thumbnail sidecar for the repo person")
	}
	if _, err := os.Stat(filepath.Join(database.ThumbDir(), p.Thumb)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}

	// The same name in another repo is an independent person with its own
	// photo and thumbnail.
	relB, err := EnrollBytes(eng, database, peopleDir, repoB.ID, "Bob", "photo.PNG", img)
	if err != nil {
		t.Fatal(err)
	}
	if relB == rel {
		t.Fatalf("the two repos must save to different folders, both %q", relB)
	}
	pB := database.GetInRepo(repoB.ID, "Bob")
	if pB == nil || pB.ID == p.ID || len(pB.Photos) != 1 {
		t.Fatalf("repo B's Bob: %+v", pB)
	}
	if pB.Thumb == "" || pB.Thumb == p.Thumb {
		t.Errorf("thumbnails must be keyed per person: %q vs %q", pB.Thumb, p.Thumb)
	}

	// A rescan of repo A keeps the uploaded file (already enrolled).
	res, err := Scan(eng, database, Options{PeopleDir: peopleDir, RepoID: repoA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.PhotosAdded != 0 || res.PhotosKept != 1 {
		t.Fatalf("rescan after repo upload: expected added=0 kept=1, got %+v", res)
	}

	// Re-uploading the same bytes into repo B stays idempotent.
	if _, err := EnrollBytes(eng, database, peopleDir, repoB.ID, "Bob", "other.jpg", img); err != nil {
		t.Fatal(err)
	}
	if pB = database.GetInRepo(repoB.ID, "Bob"); pB == nil || len(pB.Photos) != 1 {
		t.Fatalf("expected exactly 1 photo after duplicate repo upload, got %+v", pB)
	}
}

// TestEnrollBytesRepoScopedCanonicalFolder checks that a repo-scoped
// enrollment of "alice" when "Alice" already exists in the repo lands in the
// existing canonical folder instead of creating a case-duplicate one.
func TestEnrollBytesRepoScopedCanonicalFolder(t *testing.T) {
	peopleDir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepo("Team")
	if err != nil {
		t.Fatal(err)
	}
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}

	if _, err := EnrollBytes(eng, database, peopleDir, repo.ID, "Alice", "a.png", []byte("first")); err != nil {
		t.Fatal(err)
	}
	rel, err := EnrollBytes(eng, database, peopleDir, repo.ID, "alice", "b.png", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(rel), "/Alice/") {
		t.Fatalf("rel = %q, want it under the canonical folder", rel)
	}
	p := database.GetInRepo(repo.ID, "ALICE")
	if p == nil || p.Name != "Alice" || len(p.Photos) != 2 {
		t.Fatalf("unexpected person after case-variant enrollment: %+v", p)
	}
}
