package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "faces.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// writeInterchange writes a JSON interchange file next to dbPath and returns
// its path and content.
func writeInterchange(t *testing.T, dbPath, content string) (string, []byte) {
	t.Helper()
	p := filepath.Join(filepath.Dir(dbPath), "embeddings.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, []byte(content)
}

const importFixture = `{
  "version": 1,
  "people": [
    {
      "id": "alice",
      "name": "Alice",
      "photos": [
        {"path": "a/1.jpg", "hash": "aaa", "embedding": [0.25, -0.5, 1]},
        {"path": "a/2.jpg", "hash": "bbb", "embedding": [0.125]}
      ]
    },
    {"id": "bob", "name": "Bob", "photos": []}
  ],
  "threshold": 0.52
}`

func TestJSONImport(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	jsonPath, want := writeInterchange(t, dbPath, importFixture)

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open with import: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	people := d.People()
	if len(people) != 2 {
		t.Fatalf("expected 2 imported people, got %d", len(people))
	}
	a := d.Get("alice")
	if a == nil || len(a.Photos) != 2 {
		t.Fatalf("alice not imported with photos: %+v", a)
	}
	if a.Photos[0].Path != "a/1.jpg" || a.Photos[0].Hash != "aaa" {
		t.Errorf("photo fields not imported: %+v", a.Photos[0])
	}
	if e := a.Photos[0].Embedding; len(e) != 3 || e[0] != 0.25 || e[1] != -0.5 || e[2] != 1 {
		t.Errorf("embedding not imported: %v", e)
	}
	if d.Threshold() == nil || *d.Threshold() != 0.52 {
		t.Errorf("threshold not imported: %v", d.Threshold())
	}

	// The JSON file must be left untouched by the import.
	got, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("interchange file was modified by the import")
	}

	// The imported data must be persisted in the bbolt store itself
	// (a reopen without the JSON must still show everything).
	if err := os.Remove(jsonPath); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	if p := d2.Get("alice"); p == nil || len(p.Photos) != 2 || p.Photos[0].Embedding[1] != -0.5 {
		t.Errorf("imported data lost after reopen: %+v", p)
	}
	if d2.Threshold() == nil || *d2.Threshold() != 0.52 {
		t.Errorf("imported threshold lost after reopen: %v", d2.Threshold())
	}
}

func TestJSONReimportAfterDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	jsonPath, _ := writeInterchange(t, dbPath, importFixture)

	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.People()) != 2 {
		t.Fatalf("import failed: %d people", len(d.People()))
	}
	_ = d.Close()

	// Delete the bbolt store: the JSON still exists, so the next start
	// must re-import it (the file is never renamed away).
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reimport after delete: %v", err)
	}
	defer d2.Close()
	if len(d2.People()) != 2 || d2.Get("BOB") == nil {
		t.Errorf("expected re-import after store deletion, got %+v", d2.People())
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Errorf("interchange file must survive the import: %v", err)
	}
}

func TestJSONIgnoredWhenStorePopulated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.AddPhoto("Live", "l/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}

	// A stale interchange file must never overwrite populated stores.
	writeInterchange(t, dbPath, `{"version":1,"people":[{"id":"stale","name":"Stale","photos":[]}]}`)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Get("Stale") != nil {
		t.Errorf("stale JSON was imported over a populated store")
	}
	if d2.Get("Live") == nil {
		t.Errorf("store contents lost on reopen")
	}
}

func TestCorruptJSONFailsOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	writeInterchange(t, dbPath, `{"version":1,"people":[ oops`)
	if _, err := Open(dbPath); err == nil {
		t.Fatal("expected Open to fail on an unparseable interchange file")
	}
}

func TestExportRoundTrip(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	dbPath := filepath.Join(dir1, "faces.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	emb := []float32{0.25, -0.5, 1.5, 0}
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("img1"), emb); err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhoto("Bob", "b/1.jpg", []byte("img2"), []float32{-1}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThreshold(0.61); err != nil {
		t.Fatal(err)
	}

	// Export into a second directory and import it as a fresh store.
	out, err := d.ExportTo(filepath.Join(dir2, "embeddings.json"))
	if err != nil {
		t.Fatalf("ExportTo: %v", err)
	}
	if out != filepath.Join(dir2, "embeddings.json") {
		t.Errorf("unexpected export path %q", out)
	}
	d2, err := Open(filepath.Join(dir2, "faces.db"))
	if err != nil {
		t.Fatalf("import exported JSON: %v", err)
	}
	defer d2.Close()

	pa, pb := d.Get("Alice"), d2.Get("Alice")
	if pb == nil || len(pb.Photos) != 1 {
		t.Fatalf("Alice not round-tripped: %+v", pb)
	}
	if pb.ID != pa.ID || pb.Name != pa.Name {
		t.Errorf("identity mismatch: %q/%q vs %q/%q", pb.ID, pb.Name, pa.ID, pa.Name)
	}
	if pb.Photos[0].Path != pa.Photos[0].Path || pb.Photos[0].Hash != pa.Photos[0].Hash {
		t.Errorf("photo fields mismatch: %+v vs %+v", pb.Photos[0], pa.Photos[0])
	}
	if e1, e2 := pa.Photos[0].Embedding, pb.Photos[0].Embedding; len(e1) != len(e2) || e1[1] != e2[1] || e1[2] != e2[2] {
		t.Errorf("embedding mismatch: %v vs %v", e1, e2)
	}
	if d2.Threshold() == nil || *d2.Threshold() != 0.61 {
		t.Errorf("threshold not round-tripped: %v", d2.Threshold())
	}
	if d2.Get("Bob") == nil {
		t.Errorf("Bob not round-tripped")
	}
}

func TestExportDefaultPath(t *testing.T) {
	d := openTemp(t)
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if p != filepath.Join(filepath.Dir(d.Path()), "embeddings.json") {
		t.Errorf("default export path = %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("export file missing: %v", err)
	}
	// The exported document must be valid JSON with the interchange schema.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var ff fileFormat
	if err := json.Unmarshal(b, &ff); err != nil {
		t.Fatalf("exported file is not valid interchange JSON: %v", err)
	}
	if ff.Version != 1 || len(ff.People) != 1 {
		t.Errorf("unexpected exported document: %+v", ff)
	}
	// Repeated exports must be byte-stable (people name-sorted).
	p2, err := d.Export()
	if err != nil {
		t.Fatalf("second Export: %v", err)
	}
	b2, _ := os.ReadFile(p2)
	if !bytes.Equal(b, b2) {
		t.Errorf("exports are not byte-stable")
	}
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

// TestGetReturnsCopy checks that Get/GetByID hand out copies: mutating the
// returned pointer must not touch the stored record.
func TestGetReturnsCopy(t *testing.T) {
	d := openTemp(t)
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	p := d.Get("Alice")
	p.Name = "MUTATED"
	p.Photos = nil
	byID := d.GetByID(p.ID)
	byID.Photos = nil
	byID.Name = "ALSO-MUTATED"
	fresh := d.Get("Alice")
	if fresh == nil || fresh.Name != "Alice" || len(fresh.Photos) != 1 {
		t.Fatalf("stored record was mutated through the returned pointer: %+v", fresh)
	}
}

func TestAddPhotoIDCollision(t *testing.T) {
	d := openTemp(t)
	if err := d.AddPhoto("Bob", "b/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	// "bob!" slugifies to the same ID as "Bob" although the names differ:
	// in the bbolt layout they would share one photo bucket, so it must be
	// rejected instead of silently mixing two people's photos.
	err := d.AddPhoto("bob!", "b/1.jpg", []byte("y"), []float32{2})
	if !errors.Is(err, ErrIDTaken) {
		t.Fatalf("expected ErrIDTaken, got %v", err)
	}
	if p := d.Get("Bob"); len(p.Photos) != 1 || p.Photos[0].Embedding[0] != 1 {
		t.Errorf("existing person must be untouched after an ID collision: %+v", p)
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

	// The rename — including the moved photo bucket — survives a reopen.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(d.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	if p := d2.Get("ALICIA"); p == nil || p.ID != "alicia" || p.Thumb != "alicia.jpg" {
		t.Errorf("rename not persisted: %+v", p)
	}
	if p := d2.Get("ALICIA"); len(p.Photos) != 1 || p.Photos[0].Embedding[0] != 1 {
		t.Errorf("photos not moved with the rename: %+v", p)
	}
	if p := d2.Get("Bob"); p == nil || len(p.Photos) != 1 {
		t.Errorf("other person's photos must be untouched by the rename: %+v", p)
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
	path := filepath.Join(dir, "faces.db")
	d, _ := Open(path)
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{0.5, 0.6})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	p := d2.Get("Alice")
	if p == nil || len(p.Photos) != 1 || p.Photos[0].Embedding[1] != 0.6 {
		t.Fatalf("round trip failed: %+v", p)
	}
}

func TestConcurrentWrites(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "faces.db")
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
	if err := d.Close(); err != nil {
		t.Fatal(err)
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
	if err := d2.Close(); err != nil {
		t.Fatal(err)
	}
	d3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer d3.Close()
	if d3.Threshold() == nil || *d3.Threshold() != 0 {
		t.Fatalf("stored zero threshold lost: %v", d3.Threshold())
	}
}

func TestEncodeDecodePhoto(t *testing.T) {
	hash := HashBytes([]byte("image-bytes"))
	emb := []float32{0.25, -0.5, 1.5, 0, -3.75}
	enc := encodePhoto(hash, emb)
	gotHash, gotEmb, err := decodePhoto(enc)
	if err != nil {
		t.Fatalf("decodePhoto: %v", err)
	}
	if gotHash != hash {
		t.Errorf("hash mismatch: %q vs %q", gotHash, hash)
	}
	if len(gotEmb) != len(emb) {
		t.Fatalf("embedding length mismatch: %d vs %d", len(gotEmb), len(emb))
	}
	for i := range emb {
		if gotEmb[i] != emb[i] {
			t.Errorf("embedding[%d] = %v, want %v", i, gotEmb[i], emb[i])
		}
	}
	// A 512-d embedding encodes to a compact record:
	// 1-byte hash length + 40-byte hash + 2-byte dim + 2048 bytes.
	if n := len(encodePhoto(hash, make([]float32, 512))); n != 1+40+2+512*4 {
		t.Errorf("512-d record size = %d, want %d", n, 1+40+2+512*4)
	}

	// Corrupt records must be rejected, not decoded into garbage.
	corrupt := map[string]func([]byte) []byte{
		"truncated floats": func(b []byte) []byte { return b[:len(b)-3] },
		"trailing junk":    func(b []byte) []byte { return append(b, 0, 0) },
		"truncated hash":   func(b []byte) []byte { return b[:3] },
		"implausible dim":  func(b []byte) []byte { b[41] = 0x7f; return b },
	}
	for name, mutate := range corrupt {
		if _, _, err := decodePhoto(mutate(append([]byte(nil), enc...))); err == nil {
			t.Errorf("%s: expected decode error", name)
		}
	}
}
