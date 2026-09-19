package db

// Regression tests for SECURITY-REVIEW.md M6: the DB's read accessors used to
// hand out Person copies whose Photos slice still aliased the in-memory
// mirror's backing array, so a reader iterating the slice raced an enrolment
// or delete performing in-place writes (element replace, sort, append-shift)
// on the live array — torn Photo struct reads, the same bug family as H1.
//
// The fix deep-copies the Photos slice under the read lock. Photo.Embedding
// backing arrays are deliberately shared: the store never mutates an
// embedding after the Photo is created (AddPhoto replaces the whole struct),
// so there is no write to race on.

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReturnedPersonDoesNotAliasStore pins the deterministic half of M6:
// mutating a returned person's Photos elements must never write through to
// the stored record. Before the fix this corrupted the in-memory mirror (and
// could then poison the next bbolt rewrite).
func TestReturnedPersonDoesNotAliasStore(t *testing.T) {
	d := openTemp(t)
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	// corrupt mutates every field the deep copy is expected to protect:
	// the Photo struct's scalar fields and the slice length itself.
	// (Embedding elements are intentionally left alone — see the comment
	// above.)
	corrupt := func(p *Person) {
		for i := range p.Photos {
			p.Photos[i].Path = "MUTATED"
			p.Photos[i].Hash = "MUTATED"
		}
		p.Photos = append(p.Photos, Photo{Path: "EXTRA"})
	}

	t.Run("Get", func(t *testing.T) {
		p := d.Get("Alice")
		if p == nil {
			t.Fatal("Get returned nil")
		}
		corrupt(p)
		fresh := d.Get("Alice")
		if len(fresh.Photos) != 1 || fresh.Photos[0].Path != "a/1.jpg" || fresh.Photos[0].Hash == "MUTATED" {
			t.Fatalf("Get: stored record corrupted through the returned pointer: %+v", fresh.Photos)
		}
	})

	t.Run("GetByID", func(t *testing.T) {
		p := d.GetByID("alice")
		if p == nil {
			t.Fatal("GetByID returned nil")
		}
		corrupt(p)
		fresh := d.GetByID("alice")
		if len(fresh.Photos) != 1 || fresh.Photos[0].Path != "a/1.jpg" || fresh.Photos[0].Hash == "MUTATED" {
			t.Fatalf("GetByID: stored record corrupted through the returned pointer: %+v", fresh.Photos)
		}
	})

	t.Run("People", func(t *testing.T) {
		ps := d.People()
		if len(ps) != 1 {
			t.Fatalf("People returned %d people, want 1", len(ps))
		}
		corrupt(&ps[0])
		fresh := d.Get("Alice")
		if len(fresh.Photos) != 1 || fresh.Photos[0].Path != "a/1.jpg" || fresh.Photos[0].Hash == "MUTATED" {
			t.Fatalf("People: stored record corrupted through the returned slice: %+v", fresh.Photos)
		}
	})

	// The stored hash lookup still resolves the original path: the aliasing
	// mutation above must not have renamed the store's photo.
	if d.PhotoHash("Alice", "a/1.jpg") != HashBytes([]byte("x")) {
		t.Fatal("store photo path/hash damaged by mutation of a returned copy")
	}
}

// TestConcurrentPhotoReadersVsWriters is the -race probe for M6 (the H1
// pattern): writers exercise the in-place mirror writes (element replace,
// sort.Slice, append-shift) while readers iterate the slice handed out by the
// accessors and marshal an export. Without the deep copy this trips DATA RACE
// under -race; with it, every reader works on its own snapshot.
func TestConcurrentPhotoReadersVsWriters(t *testing.T) {
	d := openTemp(t)
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(t.TempDir(), "export.json")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: in-place replace (same path), append (new path), remove, and
	// sort — every mutator the review lists.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			path := "w.jpg"
			if i%3 == 2 {
				path = "a/1.jpg" // replace-in-place branch of addPhoto
			}
			emb := []float32{float32(i), float32(i + 1)}
			if err := d.AddPhoto("Alice", path, []byte("w"), emb); err != nil {
				t.Errorf("writer AddPhoto: %v", err)
				return
			}
			if i%5 == 0 {
				if _, err := d.RemovePhoto("Alice", "w.jpg"); err != nil {
					t.Errorf("writer RemovePhoto: %v", err)
					return
				}
			}
		}
	}()

	// Readers: consume the accessors from their own goroutines. The
	// iteration bodies copy fields into locals so -race sees every read.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch r {
				case 0:
					if p := d.Get("Alice"); p != nil {
						_ = readPhotos(p)
					}
				case 1:
					if p := d.GetByID("alice"); p != nil {
						_ = readPhotos(p)
					}
				case 2:
					for _, p := range d.People() {
						_ = readPhotos(&p)
					}
					if _, err := d.ExportTo(exportPath); err != nil {
						t.Errorf("ExportTo: %v", err)
						return
					}
				}
			}
		}(r)
	}

	// Run long enough for the race detector to observe interleavings, then
	// stop everything.
	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// readPhotos iterates a person's photos, copying the fields into locals so
// the race detector observes every read of the aliased array.
func readPhotos(p *Person) int {
	n := 0
	for _, ph := range p.Photos {
		_ = ph.Path
		_ = ph.Hash
		n += len(ph.Embedding)
	}
	return n
}
