package db

// Tests for schema v2: repositories (bbolt repos bucket + RepoID on person
// records) and the repo-scoped API. The legacy methods keep operating on
// legacy persons (RepoID "") — pinned here too, so a legacy enrollment can
// never touch a repo person's record and vice versa.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// isUUIDv4 reports whether s is a lowercase hyphenated UUID v4 (36 chars).
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
	}
	return s[14] == '4' && (s[19] == '8' || s[19] == '9' || s[19] == 'a' || s[19] == 'b')
}

// rewriteVersion rewrites the meta bucket's version marker of an already
// closed store, to simulate stores from other schema versions.
func rewriteVersion(path string, ver uint32) error {
	b, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return err
	}
	defer b.Close()
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], ver)
	return b.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktMeta).Put(metaVersionKey, buf[:])
	})
}

// storedVersionVia returns the version marker through an open DB handle
// (the store holds the bbolt file lock, so a separate read-only open would
// time out).
func storedVersionVia(t *testing.T, d *DB) uint32 {
	t.Helper()
	var ver uint32
	if err := d.bdb.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bktMeta).Get(metaVersionKey)
		if len(v) != 4 {
			return fmt.Errorf("missing version marker")
		}
		ver = binary.LittleEndian.Uint32(v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ver
}

func TestRepoCRUD(t *testing.T) {
	d := openTemp(t)

	// Empty (or whitespace) names are refused.
	repo, err := d.CreateRepo("   ")
	if err == nil || repo.ID != "" {
		t.Fatalf("empty name must be refused, got %+v err=%v", repo, err)
	}

	family, err := d.CreateRepo("  Family  ")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if !isUUIDv4(family.ID) {
		t.Errorf("repo ID %q is not a lowercase hyphenated UUID v4", family.ID)
	}
	if family.Name != "Family" {
		t.Errorf("name not trimmed: %q", family.Name)
	}
	if family.Shared || family.ShareToken != "" {
		t.Errorf("fresh repo must be unshared: %+v", family)
	}
	if family.CreatedAt.IsZero() || family.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt must be a UTC timestamp, got %+v", family.CreatedAt)
	}

	// Name uniqueness is case-insensitive.
	if _, err := d.CreateRepo("FAMILY"); !errors.Is(err, ErrRepoNameTaken) {
		t.Errorf("expected ErrRepoNameTaken, got %v", err)
	}

	work, err := d.CreateRepo("Work")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if family.ID == work.ID {
		t.Fatalf("two repos must get distinct IDs")
	}

	// Lookups by ID (exact) and name (case-insensitive).
	if r, ok := d.GetRepo(family.ID); !ok || r.Name != "Family" || !r.CreatedAt.Equal(family.CreatedAt) {
		t.Errorf("GetRepo: %+v ok=%v", r, ok)
	}
	if _, ok := d.GetRepo("no-such-repo"); ok {
		t.Errorf("GetRepo must miss unknown IDs")
	}
	if r, ok := d.GetRepoByName("fAMiLY"); !ok || r.ID != family.ID {
		t.Errorf("GetRepoByName (EqualFold): %+v ok=%v", r, ok)
	}
	if _, ok := d.GetRepoByName("nope"); ok {
		t.Errorf("GetRepoByName must miss unknown names")
	}

	// Handouts are copies: mutating one must not corrupt the store.
	h, _ := d.GetRepo(family.ID)
	h.Name = "MUTATED"
	h.CreatedAt = time.Time{}
	if r, _ := d.GetRepo(family.ID); r.Name != "Family" || r.CreatedAt.IsZero() {
		t.Errorf("repo record mutated through a handout: %+v", r)
	}

	// Repos() sorts fold-style (equipe < family < work), regardless of
	// creation order.
	if _, err := d.CreateRepo("équipe"); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, 3)
	for _, r := range d.Repos() {
		got = append(got, r.Name)
	}
	if want := []string{"équipe", "Family", "Work"}; !slices.Equal(got, want) {
		t.Errorf("Repos order = %q, want %q", got, want)
	}

	// Everything survives a reopen.
	path := d.Path()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	r, ok := d2.GetRepo(family.ID)
	if !ok || r.Name != "Family" || !r.CreatedAt.Equal(family.CreatedAt) {
		t.Errorf("repo not persisted: %+v (created %s)", r, family.CreatedAt)
	}
	if n := len(d2.Repos()); n != 3 {
		t.Errorf("Repos after reopen = %d, want 3", n)
	}
}

func TestRenameRepo(t *testing.T) {
	d := openTemp(t)
	r, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateRepo("Work"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRepoShare(r.ID, true, "tok-plaintext"); err != nil {
		t.Fatal(err)
	}

	if _, err := d.RenameRepo("no-such", "X"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("unknown repo: expected ErrRepoNotFound, got %v", err)
	}
	if _, err := d.RenameRepo(r.ID, "  "); err == nil {
		t.Errorf("empty name must be refused")
	}
	if _, err := d.RenameRepo(r.ID, "work"); !errors.Is(err, ErrRepoNameTaken) {
		t.Errorf("case-insensitive collision: expected ErrRepoNameTaken, got %v", err)
	}

	upd, err := d.RenameRepo(r.ID, "  Famiglia  ")
	if err != nil {
		t.Fatalf("RenameRepo: %v", err)
	}
	if upd.ID != r.ID || upd.Name != "Famiglia" {
		t.Errorf("unexpected rename result: %+v", upd)
	}
	// The rename must keep the share fields and the creation timestamp.
	if !upd.Shared || upd.ShareToken != "tok-plaintext" || !upd.CreatedAt.Equal(r.CreatedAt) {
		t.Errorf("rename dropped fields: %+v", upd)
	}
	if got, _ := d.GetRepoByName("famiglia"); got.ID != r.ID {
		t.Errorf("renamed repo not reachable under the new name: %+v", got)
	}
	if got, _ := d.GetRepoByName("Family"); got.ID != "" || got.Name != "" {
		t.Errorf("old name still resolves: %+v", got)
	}

	// Renaming onto the repo's own name (case variation) is allowed.
	if _, err := d.RenameRepo(r.ID, "FAMIGLIA"); err != nil {
		t.Errorf("self rename: %v", err)
	}
	if got, _ := d.GetRepo(r.ID); got.Name != "FAMIGLIA" {
		t.Errorf("case-only rename not applied: %+v", got)
	}
}

func TestRepoShare(t *testing.T) {
	d := openTemp(t)
	r, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRepoShare("no-such", true, "x"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("unknown repo: expected ErrRepoNotFound, got %v", err)
	}
	// An empty token is refused (the caller passes the plaintext now).
	if err := d.SetRepoShare(r.ID, true, "   "); err == nil {
		t.Errorf("empty share token accepted")
	}
	const token = "tok-plaintext"
	if err := d.SetRepoShare(r.ID, true, token); err != nil {
		t.Fatalf("SetRepoShare: %v", err)
	}
	if got, _ := d.GetRepo(r.ID); !got.Shared || got.ShareToken != token {
		t.Errorf("share not set: %+v", got)
	}
	// Disabling clears the token, whatever was passed.
	if err := d.SetRepoShare(r.ID, false, "stale"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetRepo(r.ID); got.Shared || got.ShareToken != "" {
		t.Errorf("share not cleared: %+v", got)
	}

	// The cleared share must persist.
	path := d.Path()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got, _ := d2.GetRepo(r.ID); got.Shared || got.ShareToken != "" {
		t.Errorf("cleared share not persisted: %+v", got)
	}

	// A set share (the token) persists too.
	if err := d2.SetRepoShare(r.ID, true, token); err != nil {
		t.Fatal(err)
	}
	if err := d2.Close(); err != nil {
		t.Fatal(err)
	}
	d3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d3.Close()
	if got, _ := d3.GetRepo(r.ID); !got.Shared || got.ShareToken != token {
		t.Errorf("set share not persisted: %+v", got)
	}
}

func TestNewPersonID(t *testing.T) {
	repo := "0f4c2a1b-7788-49cc-9edd-a1b2c3d4e5f0"
	if id := d_newPersonID(repo, "Alice Smith"); id != repo+"-alice-smith" {
		t.Errorf("unexpected ID %q", id)
	}
	for _, tc := range []struct{ repo, name string }{
		{strings.ToUpper(repo), "Renée Éció"},     // repo casing folded, diacritics dropped by the slug
		{repo, "ÖTitle/with*weird@chars!"},        // non-slug runes dropped
		{repo, "   "},                             // empty slug falls back to the hash ID
		{repo, strings.Repeat("x", 3) + "ß Drag"}, // ß kept as a distinct letter
	} {
		got := d_newPersonID(tc.repo, tc.name)
		if !strings.HasPrefix(got, strings.ToLower(tc.repo)+"-") {
			t.Errorf("NewPersonID(%q, %q) = %q, missing repo prefix", tc.repo, tc.name, got)
		}
		if got != strings.ToLower(tc.repo)+"-"+newID(tc.name) {
			t.Errorf("NewPersonID(%q, %q) = %q, want repo prefix + newID slug", tc.repo, tc.name, got)
		}
		for _, r := range got {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				t.Errorf("NewPersonID(%q, %q) = %q has rune %q outside [a-z0-9-]", tc.repo, tc.name, got, r)
			}
		}
	}
	// Repo UUID casing must not change the derived ID.
	if d_newPersonID(repo, "Alice") != d_newPersonID(strings.ToUpper(repo), "Alice") {
		t.Errorf("repo UUID casing must not change the ID")
	}
}

// d_newPersonID keeps the test table terse.
func d_newPersonID(repo, name string) string { return (&DB{}).NewPersonID(repo, name) }

func TestAddPhotoHashedForRepo(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	emb := []float32{1, 2, 3}

	// An empty repoID is refused instead of creating an unassigned person.
	if err := d.AddPhotoHashedForRepo("", "Alice", "a/1.jpg", "h1", emb); err == nil {
		t.Errorf("empty repoID must be refused")
	}

	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", emb); err != nil {
		t.Fatalf("AddPhotoHashedForRepo: %v", err)
	}
	p := d.GetInRepo(repo.ID, "ALICE")
	if p == nil {
		t.Fatalf("repo person not created")
	}
	if want := d.NewPersonID(repo.ID, "Alice"); p.ID != want {
		t.Errorf("unexpected ID %q, want %q", p.ID, want)
	}
	if p.RepoID != repo.ID {
		t.Errorf("RepoID not set: %+v", p)
	}
	if len(p.Photos) != 1 || p.Photos[0].Hash != "h1" {
		t.Fatalf("photo not stored: %+v", p.Photos)
	}

	// Appending to the existing person; same path replaces.
	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/2.jpg", "h2", emb); err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1b", emb); err != nil {
		t.Fatal(err)
	}
	p = d.GetInRepo(repo.ID, "alice")
	if len(p.Photos) != 2 {
		t.Fatalf("expected 2 photos after append+replace, got %+v", p.Photos)
	}
	if p.Photos[0].Hash != "h1b" || p.Photos[1].Hash != "h2" {
		t.Errorf("photos not replaced/appended by path: %+v", p.Photos)
	}

	// The same name in the legacy scope is a different person.
	if err := d.AddPhotoHashed("Alice", "a/1.jpg", "legacy", emb); err != nil {
		t.Fatal(err)
	}
	legacy := d.GetByID("alice")
	if legacy == nil || legacy.RepoID != "" || legacy.ID == p.ID || len(legacy.Photos) != 1 {
		t.Fatalf("legacy person broken: %+v", legacy)
	}

	// A second name slugifying to the same repo-scoped ID is refused,
	// and the existing person is untouched.
	if err := d.AddPhotoHashedForRepo(repo.ID, "Bob", "b/1.jpg", "h", emb); err != nil {
		t.Fatal(err)
	}
	err = d.AddPhotoHashedForRepo(repo.ID, "Bob!", "b/2.jpg", "h", emb)
	if !errors.Is(err, ErrIDTaken) {
		t.Errorf("expected ErrIDTaken, got %v", err)
	}
	if p := d.GetInRepo(repo.ID, "Bob"); len(p.Photos) != 1 || p.Photos[0].Hash != "h" {
		t.Errorf("existing repo person must be untouched after an ID collision: %+v", p)
	}

	// Everything persists: person record, RepoID, photos.
	path := d.Path()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if p := d2.GetInRepo(repo.ID, "Alice"); p == nil || p.RepoID != repo.ID || len(p.Photos) != 2 || p.Photos[0].Hash != "h1b" {
		t.Errorf("repo person lost after reopen: %+v", p)
	}
	if _, ok := d2.GetRepo(repo.ID); !ok {
		t.Errorf("repo lost after reopen")
	}
}

func TestMetaSurvivesRepoMutations(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h", []float32{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetMetaInRepo(repo.ID, "Alice", metaFixture()); err != nil {
		t.Fatal(err)
	}
	want := func() *PersonMeta { return d.GetInRepo(repo.ID, "Alice").Meta }

	// Adding / replacing / removing photos keeps the metadata.
	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/2.jpg", "h", []float32{2}); err != nil {
		t.Fatal(err)
	}
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h", []float32{3})
	if ok, err := d.RemovePhotoInRepo(repo.ID, "Alice", "a/2.jpg"); !ok || err != nil {
		t.Fatalf("RemovePhotoInRepo: %v %v", ok, err)
	}
	if got := want(); got == nil || len(got.Aliases) != 2 {
		t.Errorf("repo photo mutations lost metadata: %+v", got)
	}

	// Thumbnail sidecar changes (addressed by the repo-prefixed ID) too.
	p := d.GetInRepo(repo.ID, "Alice")
	if err := d.SetThumbnail(p.ID, []byte("jpg"), "a/1.jpg"); err != nil {
		t.Fatal(err)
	}
	if got := want(); got == nil || len(got.Aliases) != 2 {
		t.Errorf("SetThumbnail lost metadata: %+v", got)
	}
	if err := d.ClearThumbnail(p.ID); err != nil {
		t.Fatal(err)
	}
	if got := want(); got == nil || len(got.Aliases) != 2 {
		t.Errorf("ClearThumbnail lost metadata: %+v", got)
	}

	// A rename keeps the metadata.
	if _, err := d.RenamePersonInRepo(repo.ID, "Alice", "Alicia"); err != nil {
		t.Fatal(err)
	}
	if got := d.GetInRepo(repo.ID, "Alicia").Meta; got == nil || len(got.Aliases) != 2 {
		t.Errorf("RenamePersonInRepo lost metadata: %+v", got)
	}

	// SetMetaInRepo round trip and its not-found error.
	if _, err := d.SetMetaInRepo(repo.ID, "Nobody", metaFixture()); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("unknown person: expected ErrPersonNotFound, got %v", err)
	}
	upd, err := d.SetMetaInRepo(repo.ID, "ALICIA", metaFixture())
	if err != nil || upd == nil || len(upd.Meta.Aliases) != 2 || upd.RepoID != repo.ID {
		t.Fatalf("SetMetaInRepo: err=%v upd=%+v", err, upd)
	}
}

func TestRepoScopedLookups(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateRepo("Work")
	if err != nil {
		t.Fatal(err)
	}
	emb := []float32{1}
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", emb)
	_ = d.AddPhotoHashedForRepo(repo.ID, "Bob", "b/1.jpg", "h2", emb)
	_ = d.AddPhotoHashedForRepo(other.ID, "Alice", "a/1.jpg", "h3", emb)
	_ = d.AddPhoto("Legacy", "l/1.jpg", []byte("x"), emb)

	// The same name in both repos resolves to two independent persons.
	a1, a2 := d.GetInRepo(repo.ID, "alice"), d.GetInRepo(other.ID, "ALICE")
	if a1 == nil || a2 == nil || a1.ID == a2.ID || a1.RepoID != repo.ID || a2.RepoID != other.ID {
		t.Fatalf("repo-scoped persons must be independent: %+v vs %+v", a1, a2)
	}

	// PhotoHashInRepo resolves within the repo only.
	if got := d.PhotoHashInRepo(repo.ID, "Alice", "a/1.jpg"); got != "h1" {
		t.Errorf("PhotoHashInRepo = %q, want h1", got)
	}
	if got := d.PhotoHashInRepo(other.ID, "Alice", "a/1.jpg"); got != "h3" {
		t.Errorf("PhotoHashInRepo resolved the wrong repo: %q", got)
	}
	if got := d.PhotoHashInRepo(repo.ID, "Nobody", "a/1.jpg"); got != "" {
		t.Errorf("unknown person must hash empty, got %q", got)
	}
	// Empty repoID never matches anything repo-scoped.
	if got := d.PhotoHashInRepo("", "Alice", "a/1.jpg"); got != "" {
		t.Errorf("empty repoID must hash empty, got %q", got)
	}
	// The legacy hash accessor only sees legacy persons.
	if got := d.PhotoHash("Alice", "a/1.jpg"); got != "" {
		t.Errorf("legacy PhotoHash must not see repo persons, got %q", got)
	}
	if got := d.PhotoHash("Legacy", "l/1.jpg"); got != HashBytes([]byte("x")) {
		t.Errorf("legacy PhotoHash broken: %q", got)
	}

	// PeopleByRepo: only that repo's persons, fold-sorted.
	byRepo := d.PeopleByRepo(repo.ID)
	if len(byRepo) != 2 || byRepo[0].Name != "Alice" || byRepo[1].Name != "Bob" {
		t.Fatalf("PeopleByRepo = %+v", byRepo)
	}
	if n := d.PeopleCount(repo.ID); n != 2 || d.PeopleCount(other.ID) != 1 || d.PeopleCount("") != 0 {
		t.Errorf("PeopleCount broken: %d/%d/%d", d.PeopleCount(repo.ID), d.PeopleCount(other.ID), d.PeopleCount(""))
	}
	if got := d.PeopleByRepo(""); len(got) != 0 {
		t.Errorf(`PeopleByRepo("") must exclude legacy persons, got %+v`, got)
	}
	// Handouts are deep copies.
	byRepo[0].Photos = nil
	if p := d.GetInRepo(repo.ID, "Alice"); len(p.Photos) != 1 {
		t.Errorf("PeopleByRepo handed out aliased records: %+v", p)
	}
	// The legacy global view still lists everyone.
	if n := len(d.People()); n != 4 {
		t.Errorf("People() = %d persons, want 4 (global view)", n)
	}
	if d.Get("Alice") == nil || d.GetByID("legacy") == nil {
		t.Errorf("global accessors broken")
	}
}

func TestRepoScopedPersonMutations(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateRepo("Work")
	if err != nil {
		t.Fatal(err)
	}
	emb := []float32{1}
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", emb)
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/2.jpg", "h1b", emb)
	_ = d.AddPhotoHashedForRepo(other.ID, "Alice", "a/1.jpg", "h3", emb)
	_ = d.AddPhoto("Legacy", "l/1.jpg", []byte("x"), emb)

	// Removing a photo is scoped to the repo.
	if ok, err := d.RemovePhotoInRepo(repo.ID, "Alice", "a/2.jpg"); !ok || err != nil {
		t.Fatalf("RemovePhotoInRepo: %v %v", ok, err)
	}
	if got := d.PhotoHashInRepo(repo.ID, "Alice", "a/2.jpg"); got != "" {
		t.Errorf("photo not removed in repo: %q", got)
	}
	if got := d.PhotoHashInRepo(other.ID, "Alice", "a/1.jpg"); got != "h3" {
		t.Errorf("other repo's photo damaged: %q", got)
	}
	if ok, err := d.RemovePhotoInRepo(repo.ID, "Alice", "not-there.jpg"); ok || err != nil {
		t.Errorf("missing photo: ok=%v err=%v", ok, err)
	}
	if ok, _ := d.RemovePhotoInRepo(repo.ID, "Nobody", "x.jpg"); ok {
		t.Errorf("missing person must report not-found")
	}
	if ok, _ := d.RemovePhotoInRepo("", "Legacy", "l/1.jpg"); ok {
		t.Errorf(`empty repoID must never remove anything`)
	}

	// Removing a person is scoped too, and removes the sidecar.
	a1 := d.GetInRepo(repo.ID, "Alice")
	if err := d.SetThumbnail(a1.ID, []byte("jpg"), "a/1.jpg"); err != nil {
		t.Fatal(err)
	}
	thumbPath := filepath.Join(d.ThumbDir(), a1.ID+".jpg")
	if ok, err := d.RemovePersonInRepo(other.ID, "Alice"); !ok || err != nil {
		t.Fatalf("RemovePersonInRepo(other): %v %v", ok, err)
	}
	if d.GetInRepo(other.ID, "Alice") != nil {
		t.Errorf("other repo's Alice not removed")
	}
	if d.GetInRepo(repo.ID, "Alice") == nil {
		t.Errorf("wrong repo's Alice removed!")
	}
	if d.GetByID("legacy") == nil {
		t.Errorf("legacy person removed by a repo-scoped delete")
	}
	if ok, err := d.RemovePersonInRepo(repo.ID, "Alice"); !ok || err != nil {
		t.Fatalf("RemovePersonInRepo: %v %v", ok, err)
	}
	if _, err := os.Stat(thumbPath); !os.IsNotExist(err) {
		t.Errorf("sidecar should be gone with the person, err=%v", err)
	}
	if ok, _ := d.RemovePersonInRepo(repo.ID, "Alice"); ok {
		t.Errorf("second removal must report not-found")
	}
	if ok, _ := d.RemovePersonInRepo("", "Legacy"); ok {
		t.Errorf(`empty repoID must never remove anything`)
	}
	if d.GetByID("legacy") == nil {
		t.Errorf("legacy person must be untouched by repo-scoped removals")
	}
}

func TestRenamePersonInRepo(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateRepo("Work")
	if err != nil {
		t.Fatal(err)
	}
	emb := []float32{1}
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", emb)
	_ = d.AddPhotoHashedForRepo(repo.ID, "Cara", "c/1.jpg", "h2", emb)
	_ = d.AddPhotoHashedForRepo(other.ID, "Alicia", "a/1.jpg", "h3", emb)
	_ = d.AddPhoto("Alicia", "l/1.jpg", []byte("x"), emb) // legacy Alicia

	p := d.GetInRepo(repo.ID, "Alice")
	if err := d.SetThumbnail(p.ID, []byte("jpg"), "a/1.jpg"); err != nil {
		t.Fatal(err)
	}
	oldThumb := filepath.Join(d.ThumbDir(), p.ID+".jpg")

	// Names held by OTHER scopes (another repo, legacy) do not block a
	// repo-scoped rename: uniqueness is scoped to the repo.
	upd, err := d.RenamePersonInRepo(repo.ID, "Alice", "ALICIA")
	if err != nil {
		t.Fatalf("scoped rename: %v", err)
	}
	wantID := d.NewPersonID(repo.ID, "ALICIA")
	if upd.ID != wantID || upd.Name != "ALICIA" || upd.RepoID != repo.ID {
		t.Fatalf("unexpected rename result: %+v", upd)
	}
	if d.GetInRepo(repo.ID, "alice") != nil || d.GetInRepo(repo.ID, "Alicia") == nil {
		t.Errorf("rename not applied in scope")
	}
	if got := d.GetInRepo(other.ID, "Alicia"); got == nil || got.ID == wantID {
		t.Errorf("other repo's person must be untouched: %+v", got)
	}
	if d.GetByID("alicia") == nil {
		t.Errorf("legacy person must be untouched by the repo rename")
	}
	// The handout is a deep copy: caller mutations stay local.
	upd.Photos = nil
	if q := d.GetInRepo(repo.ID, "Alicia"); len(q.Photos) != 1 || q.ID != wantID {
		t.Errorf("returned person aliased the store: %+v", q)
	}
	// The sidecar followed the ID change.
	newThumb := filepath.Join(d.ThumbDir(), wantID+".jpg")
	if b, err := os.ReadFile(newThumb); err != nil || !bytes.Equal(b, []byte("jpg")) {
		t.Errorf("sidecar not renamed: %v", err)
	}
	if _, err := os.Stat(oldThumb); !os.IsNotExist(err) {
		t.Errorf("old sidecar should be gone, err=%v", err)
	}

	// A name already used inside the repo is rejected (case-insensitive)...
	if _, err := d.RenamePersonInRepo(repo.ID, "Cara", "ALICIA"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("expected ErrNameTaken, got %v", err)
	}
	// ...and so is a name whose derived ID collides inside the repo
	// ("alicia?" slugifies to Alicia's ID although the names differ).
	if _, err := d.RenamePersonInRepo(repo.ID, "Cara", "alicia?"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("expected ErrNameTaken for ID collision, got %v", err)
	}
	if got := d.GetInRepo(repo.ID, "Cara"); got == nil || len(got.Photos) != 1 {
		t.Errorf("failed renames must not change anyone: %+v", got)
	}

	// Unknown person, unknown repo, empty name.
	if _, err := d.RenamePersonInRepo(repo.ID, "Nobody", "X"); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("expected ErrPersonNotFound, got %v", err)
	}
	if _, err := d.RenamePersonInRepo("", "Cara", "X"); err == nil {
		t.Errorf("empty repoID must be refused")
	}
	if _, err := d.RenamePersonInRepo(repo.ID, "Cara", "   "); err == nil {
		t.Errorf("empty name must be refused")
	}
}

func TestDeleteRepo(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateRepo("Work")
	if err != nil {
		t.Fatal(err)
	}
	emb := []float32{1}
	_ = d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", emb)
	_ = d.AddPhotoHashedForRepo(repo.ID, "Bob", "b/1.jpg", "h2", emb)
	_ = d.AddPhotoHashedForRepo(other.ID, "Cara", "c/1.jpg", "h3", emb)
	_ = d.AddPhoto("Legacy", "l/1.jpg", []byte("x"), emb)

	for _, name := range []string{"Alice", "Bob"} {
		p := d.GetInRepo(repo.ID, name)
		if err := d.SetThumbnail(p.ID, []byte("jpg"), "x.jpg"); err != nil {
			t.Fatal(err)
		}
	}
	aliceID := d.GetInRepo(repo.ID, "Alice").ID
	bobID := d.GetInRepo(repo.ID, "Bob").ID
	caraID := d.GetInRepo(other.ID, "Cara").ID

	// Unknown repo: (false, nil).
	if ok, err := d.DeleteRepo("no-such-repo"); ok || err != nil {
		t.Fatalf("unknown repo delete: ok=%v err=%v", ok, err)
	}

	if ok, err := d.DeleteRepo(repo.ID); !ok || err != nil {
		t.Fatalf("DeleteRepo: ok=%v err=%v", ok, err)
	}
	if _, ok := d.GetRepo(repo.ID); ok {
		t.Errorf("repo record not deleted")
	}
	if got := d.PeopleByRepo(repo.ID); len(got) != 0 {
		t.Errorf("repo persons not cascaded: %+v", got)
	}
	if d.PeopleCount(repo.ID) != 0 || d.PeopleCount(other.ID) != 1 {
		t.Errorf("people counts wrong after cascade: %d/%d", d.PeopleCount(repo.ID), d.PeopleCount(other.ID))
	}
	// Persons of other repos and legacy persons survive.
	if d.GetInRepo(other.ID, "Cara") == nil || d.GetByID("legacy") == nil {
		t.Errorf("unrelated persons were deleted")
	}
	for _, id := range []string{aliceID, bobID} {
		if _, err := os.Stat(filepath.Join(d.ThumbDir(), id+".jpg")); !os.IsNotExist(err) {
			t.Errorf("sidecar of deleted person still present, err=%v", err)
		}
	}

	// On disk: person records and their photo sub-buckets are gone; the
	// unrelated person's bucket survives.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := bolt.Open(d.Path(), 0o644, &bolt.Options{Timeout: openTimeout, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.View(func(tx *bolt.Tx) error {
		phb := tx.Bucket(bktPhotos)
		if phb.Bucket([]byte(aliceID)) != nil || phb.Bucket([]byte(bobID)) != nil {
			return errors.New("photo buckets of deleted persons still exist")
		}
		if phb.Bucket([]byte(caraID)) == nil || phb.Bucket([]byte("legacy")) == nil {
			return errors.New("unrelated person's photo bucket must survive")
		}
		if tx.Bucket(bktRepos).Get([]byte(repo.ID)) != nil {
			return errors.New("repo record still on disk")
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.GetByID(aliceID) != nil || d2.GetByID(bobID) != nil {
		t.Errorf("deleted persons still on disk after reopen")
	}
	if d2.GetInRepo(other.ID, "Cara") == nil || d2.GetByID("legacy") == nil {
		t.Errorf("unrelated persons lost after reopen")
	}
	if n := len(d2.Repos()); n != 1 {
		t.Errorf("Repos after reopen = %d, want 1", n)
	}
	// Re-deleting is a no-op.
	if ok, err := d2.DeleteRepo(repo.ID); ok || err != nil {
		t.Errorf("double delete: ok=%v err=%v", ok, err)
	}
}

// TestLegacyMethodsScopedToLegacy pins the scoping rule of the pre-repo
// methods: they operate on legacy persons (RepoID "") only, so a legacy
// enrollment never writes into a repo person's record and a legacy removal
// can never cascade into a repo.
func TestLegacyMethodsScopedToLegacy(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhotoHashedForRepo(repo.ID, "Alice", "a/1.jpg", "h1", []float32{1}); err != nil {
		t.Fatal(err)
	}

	// With only a repo person named Alice, a legacy lookup must refuse.
	if _, err := d.SetMeta("Alice", metaFixture()); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("legacy SetMeta must not see repo persons, got %v", err)
	}

	// Legacy enrollment of the same name creates a separate legacy person.
	if err := d.AddPhotoHashed("Alice", "l/1.jpg", "h2", []float32{2}); err != nil {
		t.Fatal(err)
	}
	repoP, legacy := d.GetInRepo(repo.ID, "Alice"), d.GetByID("alice")
	if repoP == nil || legacy == nil || repoP.ID == legacy.ID {
		t.Fatalf("expected separate repo and legacy persons: %+v / %+v", repoP, legacy)
	}
	if len(repoP.Photos) != 1 || repoP.Photos[0].Hash != "h1" {
		t.Errorf("repo person was modified by the legacy enrollment: %+v", repoP.Photos)
	}
	if len(legacy.Photos) != 1 || legacy.Photos[0].Hash != "h2" || legacy.RepoID != "" {
		t.Errorf("legacy person not created properly: %+v", legacy)
	}

	// Legacy metadata lands on the legacy person only.
	if _, err := d.SetMeta("Alice", metaFixture()); err != nil {
		t.Fatalf("SetMeta on the legacy person: %v", err)
	}
	if p := d.GetByID("alice"); p == nil || p.Meta == nil {
		t.Errorf("metadata did not land on the legacy person: %+v", p)
	}
	if p := d.GetInRepo(repo.ID, "Alice"); p == nil || p.Meta != nil {
		t.Errorf("metadata leaked into the repo person: %+v", p)
	}
	if ok, err := d.RemovePerson("Alice"); !ok || err != nil {
		t.Fatalf("RemovePerson: %v %v", ok, err)
	}
	if d.GetInRepo(repo.ID, "Alice") == nil {
		t.Errorf("repo person was removed by the legacy RemovePerson")
	}
	if d.GetByID("alice") != nil {
		t.Errorf("legacy person should be gone")
	}
}

const v2ImportFixture = `{
  "version": 2,
  "repos": [
    {"id": "0f4c2a1b-7788-49cc-9edd-a1b2c3d4e5f0", "name": "Family", "created_at": "2024-01-02T03:04:05Z", "shared": true, "share_token": "tok"}
  ],
  "people": [
    {"id": "0f4c2a1b-7788-49cc-9edd-a1b2c3d4e5f0-alice", "name": "Alice", "repo_id": "0f4c2a1b-7788-49cc-9edd-a1b2c3d4e5f0", "photos": [{"path": "a/1.jpg", "hash": "aaa", "embedding": [1]}]},
    {"id": "legacy", "name": "Bob", "photos": []}
  ],
  "threshold": 0.5
}`

func TestJSONImportV2Repos(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	writeInterchange(t, dbPath, v2ImportFixture)

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open with v2 import: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const repoID = "0f4c2a1b-7788-49cc-9edd-a1b2c3d4e5f0"
	repo, ok := d.GetRepo(repoID)
	if !ok || repo.Name != "Family" || !repo.Shared || repo.ShareToken != "tok" {
		t.Fatalf("repo not imported: %+v ok=%v", repo, ok)
	}
	if !repo.CreatedAt.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("created_at not imported: %s", repo.CreatedAt)
	}
	if p := d.GetInRepo(repoID, "Alice"); p == nil || p.RepoID != repoID || len(p.Photos) != 1 {
		t.Fatalf("repo person not imported: %+v", p)
	}
	if p := d.Get("Bob"); p == nil || p.RepoID != "" {
		t.Fatalf("legacy person not imported as legacy: %+v", p)
	}
	if n := d.PeopleCount(repoID); n != 1 {
		t.Errorf("PeopleCount = %d, want 1", n)
	}
	if len(d.Repos()) != 1 || d.PeopleByRepo(repoID)[0].ID != d.NewPersonID(repoID, "Alice") {
		t.Errorf("repo listing/person ID wrong")
	}
}

// TestMigrationV1Interchange covers the wave-1 migration path: a v1-shaped
// interchange file (no repos key) written next to a fresh store, then the
// bbolt file deleted, re-imports as legacy persons.
func TestMigrationV1Interchange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	writeInterchange(t, dbPath, importFixture)
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	defer d2.Close()
	a := d2.Get("alice")
	if a == nil || a.RepoID != "" || len(a.Photos) != 2 {
		t.Fatalf("v1 interchange not imported as legacy persons: %+v", a)
	}
	if d2.GetByID("bob") == nil {
		t.Errorf("second legacy person missing")
	}
	if len(d2.Repos()) != 0 {
		t.Errorf("v1 interchange must not create repos, got %+v", d2.Repos())
	}
	if got := storedVersionVia(t, d2); got != dbVersion {
		t.Errorf("migrated store version = %d, want %d", got, dbVersion)
	}
}

func TestExportV2ReposRoundTrip(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	d, err := Open(filepath.Join(dir1, "faces.db"))
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := d.CreateRepo("Zeta") // created first, must sort second
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := d.CreateRepo("Alpha")
	if err != nil {
		t.Fatal(err)
	}
	const shareToken = "round-trip-token"
	if err := d.SetRepoShare(repoA.ID, true, shareToken); err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhotoHashedForRepo(repoB.ID, "Alice", "a/1.jpg", "h", []float32{1}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhoto("Legacy", "l/1.jpg", []byte("x"), []float32{2}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetMeta("Legacy", metaFixture()); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThreshold(0.55); err != nil {
		t.Fatal(err)
	}

	// Repeated exports are byte-stable (repos and people name-sorted).
	p1, err := d.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := d.Export()
	if err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(p2); !bytes.Equal(b1, b2) {
		t.Errorf("exports with repos are not byte-stable")
	}

	var ff fileFormat
	if err := json.Unmarshal(b1, &ff); err != nil {
		t.Fatal(err)
	}
	if ff.Version != 2 {
		t.Errorf("exported version = %d, want 2", ff.Version)
	}
	if len(ff.Repos) != 2 || ff.Repos[0].Name != "Alpha" || ff.Repos[1].Name != "Zeta" {
		t.Errorf("repos not name-sorted: %+v", ff.Repos)
	}
	// Legacy people omit repo_id; repo people carry it.
	var doc struct {
		People []map[string]any `json:"people"`
	}
	if err := json.Unmarshal(b1, &doc); err != nil {
		t.Fatal(err)
	}
	for _, pm := range doc.People {
		name, _ := pm["name"].(string)
		_, hasRepo := pm["repo_id"]
		if name == "Legacy" && hasRepo {
			t.Errorf("legacy person must omit repo_id")
		}
		if name == "Alice" && !hasRepo {
			t.Errorf("repo person must carry repo_id")
		}
	}

	// The v2 document round-trips: repos, repo_id, share fields, meta.
	if _, err := d.ExportTo(filepath.Join(dir2, "embeddings.json")); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(filepath.Join(dir2, "faces.db"))
	if err != nil {
		t.Fatalf("import v2 export: %v", err)
	}
	defer d2.Close()
	r2, ok := d2.GetRepo(repoB.ID)
	if !ok || r2.Name != "Zeta" || r2.Shared || r2.ShareToken != "" {
		t.Errorf("repo not round-tripped: %+v ok=%v", r2, ok)
	}
	// The share token round-trips (a restored interchange keeps the link
	// valid AND re-displayable).
	if ra, _ := d2.GetRepo(repoA.ID); !ra.Shared || ra.ShareToken != shareToken {
		t.Errorf("repo share fields not round-tripped: %+v", ra)
	}
	if p := d2.GetInRepo(repoB.ID, "Alice"); p == nil || p.RepoID != repoB.ID || p.ID != d.NewPersonID(repoB.ID, "Alice") || len(p.Photos) != 1 {
		t.Errorf("repo person not round-tripped: %+v", p)
	}
	if p := d2.Get("Legacy"); p == nil || p.RepoID != "" || p.Meta == nil || len(p.Meta.Aliases) != 2 {
		t.Errorf("legacy person not round-tripped: %+v", p)
	}
	if d2.Threshold() == nil || *d2.Threshold() != 0.55 {
		t.Errorf("threshold not round-tripped: %v", d2.Threshold())
	}
}

// TestRepoIDCaseNormalized pins the ID canonicalization: repo IDs are
// lowercase UUIDs and every ID-taking method resolves a differently-cased
// parameter to the same repo — including when creating repo persons, so a
// person can never be stored under a casing that later lookups would miss.
func TestRepoIDCaseNormalized(t *testing.T) {
	d := openTemp(t)
	repo, err := d.CreateRepo("Family")
	if err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(repo.ID)
	if r, ok := d.GetRepo(upper); !ok || r.ID != repo.ID {
		t.Errorf("GetRepo must fold the ID casing: %+v ok=%v", r, ok)
	}
	emb := []float32{1}
	if err := d.AddPhotoHashedForRepo(upper, "Alice", "a/1.jpg", "h1", emb); err != nil {
		t.Fatalf("AddPhotoHashedForRepo with uppercase ID: %v", err)
	}
	p := d.GetInRepo(upper, "Alice")
	if p == nil || p.RepoID != repo.ID || p.ID != d.NewPersonID(upper, "Alice") {
		t.Fatalf("repo person not stored under the canonical ID: %+v", p)
	}
	if n := d.PeopleCount(upper); n != 1 || len(d.PeopleByRepo(upper)) != 1 {
		t.Errorf("repo-scoped reads must fold the ID casing")
	}
	if _, err := d.RenameRepo(upper, "Famiglia"); err != nil {
		t.Errorf("RenameRepo with folded ID: %v", err)
	}
	if err := d.SetRepoShare(upper, true, "tok"); err != nil {
		t.Errorf("SetRepoShare with folded ID: %v", err)
	}
	if ok, err := d.DeleteRepo(upper); !ok || err != nil {
		t.Errorf("DeleteRepo with folded ID: ok=%v err=%v", ok, err)
	}
	if _, ok := d.GetRepo(repo.ID); ok {
		t.Errorf("repo should be gone after the folded-ID delete")
	}
}

// TestInterchangeNewerVersionFails keeps Open loud about interchange
// documents it cannot interpret.
func TestInterchangeNewerVersionFails(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "faces.db")
	writeInterchange(t, dbPath, `{"version":3,"repos":[],"people":[]}`)
	if _, err := Open(dbPath); err == nil {
		t.Fatal("expected Open to fail on a newer interchange version")
	}
}

// TestOpenMigratesV1Store pins the bbolt-side migration: a store whose
// version marker says 1 opens cleanly, keeps every person (RepoID "") and
// has its marker rewritten to v2.
func TestOpenMigratesV1Store(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faces.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddPhoto("Alice", "a/1.jpg", []byte("x"), []float32{1}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rewriteVersion(path, 1); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("v1 store must open (migrated): %v", err)
	}
	defer d2.Close()
	if p := d2.Get("Alice"); p == nil || p.RepoID != "" || len(p.Photos) != 1 {
		t.Fatalf("persons lost across the v1→v2 migration: %+v", p)
	}
	if got := storedVersionVia(t, d2); got != dbVersion {
		t.Errorf("stored version = %d, want %d", got, dbVersion)
	}
}

// TestOpenFailsOnNewerVersion keeps the loud failure for a stored version
// the binary cannot interpret.
func TestOpenFailsOnNewerVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faces.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rewriteVersion(path, dbVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to fail on a newer database version")
	}
}
