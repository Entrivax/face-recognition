package db

import (
	"bytes"
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
