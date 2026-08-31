package db

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "emb.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return d
}

func TestAddAndListPeople(t *testing.T) {
	d := openTemp(t)
	emb := []float32{0.1, 0.2, 0.3}
	if err := d.AddPhoto("Alice", "alice/1.jpg", []byte("img1"), emb); err != nil {
		t.Fatalf("AddPhoto: %v", err)
	}
	if err := d.AddPhoto("Bob", "bob/1.jpg", []byte("img2"), emb); err != nil {
		t.Fatalf("AddPhoto: %v", err)
	}
	people := d.People()
	if len(people) != 2 {
		t.Fatalf("expected 2 people, got %d", len(people))
	}
	// Sorted by name.
	if people[0].Name != "Alice" || people[1].Name != "Bob" {
		t.Errorf("people not sorted: %v", people)
	}
	if people[0].ID != "alice" {
		t.Errorf("unexpected id %q", people[0].ID)
	}
	if len(people[0].Photos) != 1 || people[0].Photos[0].Embedding[0] != 0.1 {
		t.Errorf("photo/embedding not stored: %+v", people[0].Photos)
	}
}

func TestCaseInsensitiveLookup(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("Katya Sudnikova", "k/1.jpg", []byte("x"), []float32{1})
	if d.Get("katya sudnikova") == nil {
		t.Errorf("case-insensitive Get failed")
	}
	if d.Get("KATYA SUDNIKOVA") == nil {
		t.Errorf("uppercase Get failed")
	}
}

func TestReplaceSamePath(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("A", "a/1.jpg", []byte("v1"), []float32{1})
	_ = d.AddPhoto("A", "a/1.jpg", []byte("v2"), []float32{2})
	p := d.Get("A")
	if len(p.Photos) != 1 {
		t.Fatalf("expected replace, got %d photos", len(p.Photos))
	}
	if p.Photos[0].Embedding[0] != 2 {
		t.Errorf("expected updated embedding, got %v", p.Photos[0].Embedding)
	}
}

func TestPhotoHashSkip(t *testing.T) {
	d := openTemp(t)
	b := []byte("image-bytes")
	_ = d.AddPhoto("A", "a/1.jpg", b, []float32{1})
	if d.PhotoHash("A", "a/1.jpg") != HashBytes(b) {
		t.Errorf("PhotoHash mismatch")
	}
	if d.PhotoHash("A", "a/other.jpg") != "" {
		t.Errorf("expected empty hash for unknown path")
	}
}

func TestRemovePersonAndPhoto(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("A", "a/1.jpg", []byte("1"), []float32{1})
	_ = d.AddPhoto("A", "a/2.jpg", []byte("2"), []float32{2})

	ok, err := d.RemovePhoto("A", "a/1.jpg")
	if !ok || err != nil {
		t.Fatalf("RemovePhoto: ok=%v err=%v", ok, err)
	}
	if len(d.Get("A").Photos) != 1 {
		t.Errorf("expected 1 photo left")
	}

	ok, err = d.RemovePerson("A")
	if !ok || err != nil {
		t.Fatalf("RemovePerson: ok=%v err=%v", ok, err)
	}
	if d.Get("A") != nil {
		t.Errorf("person should be gone")
	}
	// Removing again reports not-found.
	ok, _ = d.RemovePerson("A")
	if ok {
		t.Errorf("expected ok=false removing missing person")
	}
}

func TestRenamePerson(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("img"), []float32{1})
	_ = d.AddPhoto("Bob", "b/1.jpg", []byte("img2"), []float32{2})
	p := d.Get("Alice")
	jpg := []byte("fake-jpeg-bytes")
	if err := d.SetThumbnail(p.ID, jpg, "a/1.jpg"); err != nil {
		t.Fatalf("SetThumbnail: %v", err)
	}
	oldThumb := filepath.Join(d.ThumbDir(), "alice.jpg")

	// Happy path: name, ID and thumbnail sidecar all follow the rename.
	upd, err := d.RenamePerson("alice", "Alicia") // old lookup is case-insensitive
	if err != nil {
		t.Fatalf("RenamePerson: %v", err)
	}
	if upd.ID != "alicia" || upd.Name != "Alicia" {
		t.Errorf("updated person = %q/%q, want Alicia/alicia", upd.Name, upd.ID)
	}
	if d.Get("Alice") != nil || d.Get("ALICIA") == nil {
		t.Errorf("old name should be gone, new name resolvable case-insensitively")
	}
	newThumb := filepath.Join(d.ThumbDir(), "alicia.jpg")
	if got := d.ThumbFile("alicia"); got != newThumb {
		t.Errorf("ThumbFile = %q, want %q", got, newThumb)
	}
	if b, err := os.ReadFile(newThumb); err != nil || !bytes.Equal(b, jpg) {
		t.Errorf("renamed sidecar mismatch: err=%v len=%d", err, len(b))
	}
	if _, err := os.Stat(oldThumb); !os.IsNotExist(err) {
		t.Errorf("old sidecar should be gone, err=%v", err)
	}
	upd = d.Get("Alicia")
	if upd.Thumb != "alicia.jpg" || upd.ThumbSrc != "a/1.jpg" || len(upd.Photos) != 1 {
		t.Errorf("unexpected person after rename: %+v", upd)
	}

	// Renaming onto another person's name is rejected.
	if _, err := d.RenamePerson("Alicia", "BOB"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("expected ErrNameTaken, got %v", err)
	}
	if d.Get("Alicia") == nil || d.Get("Bob") == nil {
		t.Errorf("failed rename must not change anyone")
	}

	// A name whose derived ID collides with another person is rejected too:
	// "Bob!" slugifies to Bob's ID even though the names differ.
	_ = d.AddPhoto("Cara", "c/1.jpg", []byte("img3"), []float32{3})
	if _, err := d.RenamePerson("Cara", "Bob!"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("id collision should be ErrNameTaken, got %v", err)
	}

	// Unknown person.
	if _, err := d.RenamePerson("Nobody", "X"); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("expected ErrPersonNotFound, got %v", err)
	}
	// Empty name.
	if _, err := d.RenamePerson("Alicia", "   "); err == nil {
		t.Errorf("expected error for empty name")
	}

	// Case-only rename: same ID, sidecar untouched, display name updated.
	if _, err := d.RenamePerson("alicia", "ALICIA"); err != nil {
		t.Fatalf("case-only rename: %v", err)
	}
	if p := d.Get("alicia"); p == nil || p.Name != "ALICIA" || p.ID != "alicia" {
		t.Errorf("case-only rename failed: %+v", p)
	}
	if _, err := os.Stat(newThumb); err != nil {
		t.Errorf("sidecar should be untouched by a case-only rename: %v", err)
	}

	// The rename survives a reopen.
	d2, err := Open(d.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if p := d2.Get("ALICIA"); p == nil || p.ID != "alicia" || p.Thumb != "alicia.jpg" {
		t.Errorf("rename not persisted: %+v", p)
	}
}

func TestRenamePersonMissingSidecarClearsRecord(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("img"), []float32{1})
	p := d.Get("Alice")
	if err := d.SetThumbnail(p.ID, []byte("jpg"), "a/1.jpg"); err != nil {
		t.Fatal(err)
	}
	// The sidecar file disappears (outside interference); rename must still
	// succeed and leave the record clean so a rescan backfills a new thumb.
	if err := os.Remove(filepath.Join(d.ThumbDir(), "alice.jpg")); err != nil {
		t.Fatal(err)
	}
	upd, err := d.RenamePerson("Alice", "Alicia")
	if err != nil {
		t.Fatalf("RenamePerson: %v", err)
	}
	if upd.Thumb != "" || upd.ThumbSrc != "" {
		t.Errorf("stale sidecar record should be cleared, got thumb=%q src=%q", upd.Thumb, upd.ThumbSrc)
	}
}

func TestThumbnailSidecar(t *testing.T) {
	d := openTemp(t)
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("img"), []float32{1})
	p := d.Get("Alice")

	if d.ThumbFile(p.ID) != "" {
		t.Errorf("expected no thumbnail before SetThumbnail")
	}
	jpg := []byte("fake-jpeg-bytes")
	if err := d.SetThumbnail(p.ID, jpg, "a/1.jpg"); err != nil {
		t.Fatalf("SetThumbnail: %v", err)
	}
	want := filepath.Join(d.ThumbDir(), p.ID+".jpg")
	if got := d.ThumbFile(p.ID); got != want {
		t.Errorf("ThumbFile = %q, want %q", got, want)
	}
	b, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(b, jpg) {
		t.Errorf("sidecar file mismatch: err=%v len=%d", err, len(b))
	}
	p = d.Get("Alice")
	if p.Thumb != p.ID+".jpg" {
		t.Errorf("Thumb not recorded on person: %q", p.Thumb)
	}
	if p.ThumbSrc != "a/1.jpg" {
		t.Errorf("ThumbSrc not recorded: %q", p.ThumbSrc)
	}

	// Setting again replaces the sidecar and updates the source.
	jpg2 := []byte("other-jpeg-bytes")
	if err := d.SetThumbnail(p.ID, jpg2, "a/2.jpg"); err != nil {
		t.Fatalf("second SetThumbnail: %v", err)
	}
	if b, _ := os.ReadFile(want); !bytes.Equal(b, jpg2) {
		t.Errorf("thumbnail should be overwritten")
	}
	if p := d.Get("Alice"); p.ThumbSrc != "a/2.jpg" {
		t.Errorf("ThumbSrc should be updated, got %q", p.ThumbSrc)
	}
	if err := d.SetThumbnail("nobody", jpg, "x.jpg"); err == nil {
		t.Errorf("expected error for unknown person")
	}

	// Removing the person removes the sidecar.
	if ok, err := d.RemovePerson("Alice"); !ok || err != nil {
		t.Fatalf("RemovePerson: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("sidecar should be deleted, err=%v", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "emb.json")
	d, _ := Open(path)
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{0.5, 0.6})

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	p := d2.Get("Alice")
	if p == nil || len(p.Photos) != 1 || p.Photos[0].Embedding[1] != 0.6 {
		t.Fatalf("round trip failed: %+v", p)
	}
}

func TestConcurrentSave(t *testing.T) {
	d := openTemp(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('A' + i%5))
			_ = d.AddPhoto(name, "p.jpg", []byte{byte(i)}, []float32{float32(i)})
		}(i)
	}
	wg.Wait()
	if len(d.People()) == 0 {
		t.Errorf("expected some people after concurrent adds")
	}
}

func TestThresholdPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "emb.json")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Threshold() != nil {
		t.Fatalf("fresh db should have no stored threshold, got %v", *d.Threshold())
	}
	if err := d.SetThreshold(0.62); err != nil {
		t.Fatalf("SetThreshold: %v", err)
	}
	// Reopen: the value must survive.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if d2.Threshold() == nil || *d2.Threshold() != 0.62 {
		t.Fatalf("stored threshold lost: %v", d2.Threshold())
	}
	// An explicitly stored 0 must survive too (pointer, not zero sentinel).
	if err := d2.SetThreshold(0); err != nil {
		t.Fatalf("SetThreshold(0): %v", err)
	}
	d3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	if d3.Threshold() == nil || *d3.Threshold() != 0 {
		t.Fatalf("stored zero threshold lost: %v", d3.Threshold())
	}
}
