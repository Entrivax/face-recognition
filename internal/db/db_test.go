package db

import (
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
