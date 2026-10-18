package db

// Repositories group persons into separately addressable datasets (schema
// v2). A repo owns its persons: everyone created through the repo API
// carries the repo's ID (Person.RepoID) and a NewPersonID-prefixed record
// ID, so the same person name can exist in two repos without colliding.
// Legacy persons (RepoID "") stay reachable only through the pre-repo
// methods; repo-scoped accessors never return or mutate them.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	boltErr "go.etcd.io/bbolt/errors"
)

// Repo is one named repository (a group of persons with its own dataset
// directory). ID is a random UUID v4, lowercase and hyphenated (36
// characters); it is also the repo's dataset directory name under the people
// root (people/<uuid>/<Person>/*.jpg). Shared records whether the repo is
// published through a share link; ShareToken holds that token's plaintext —
// it is what presented tokens are verified against and what lets the admin
// re-display the link. API keys, in contrast, are stored hash-only.
type Repo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	Shared     bool      `json:"shared,omitempty"`
	ShareToken string    `json:"share_token,omitempty"`
}

// Sentinel errors returned by the repo methods; test with errors.Is.
var (
	ErrRepoNotFound  = errors.New("repo not found")
	ErrRepoNameTaken = errors.New("repo name already in use")
)

// normRepoID canonicalizes a repository ID: IDs are lowercase UUIDs as
// minted by CreateRepo, and every ID-taking repo method folds its input so
// a route parameter in another casing still resolves to the same repo.
func normRepoID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// newUUID returns a random RFC 4122 version-4 UUID: lowercase, hyphenated,
// 36 characters. crypto/rand only fails when the system entropy source is
// broken, in which case repo creation must fail rather than mint a
// predictable ID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// NewPersonID derives a repo-scoped person ID from the repo's UUID and the
// person name: the lowercased UUID, a hyphen, then the name slug (the
// legacy newID scheme). The result stays within the [a-z0-9-] charset
// (lowercase hex digits and hyphens plus the slug's alphabet) and is stable
// for a given (repoUUID, name), so dataset paths and thumbnails follow.
func (d *DB) NewPersonID(repoUUID, name string) string {
	return strings.ToLower(repoUUID) + "-" + newID(name)
}

// CreateRepo creates a repository with a fresh UUID. The name is trimmed and
// must be non-empty; it is unique case-insensitively among repos (character
// validation happens in callers). CreatedAt is stored in UTC. 122 random ID
// bits make collisions negligible; crypto/rand failure aborts the call.
func (d *DB) CreateRepo(name string) (Repo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Repo{}, errors.New("repo name is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cand := range d.data.Repos {
		if strings.EqualFold(cand.Name, name) {
			return Repo{}, fmt.Errorf("%w: %q", ErrRepoNameTaken, name)
		}
	}
	id, err := newUUID()
	if err != nil {
		return Repo{}, err
	}
	repo := Repo{ID: id, Name: name, CreatedAt: time.Now().UTC()}
	v, err := json.Marshal(repo)
	if err != nil {
		return Repo{}, err
	}
	if err := d.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktRepos).Put([]byte(repo.ID), v)
	}); err != nil {
		return Repo{}, err
	}
	// Disk commit succeeded; now update the in-memory mirror. Repo holds no
	// reference fields, but a fresh copy keeps the mirror independent of
	// whatever a caller does with the returned value.
	cp := repo
	d.data.Repos = append(d.data.Repos, &cp)
	return repo, nil
}

func (d *DB) findRepoLocked(id string) *Repo {
	for _, r := range d.data.Repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (d *DB) findRepoByNameLocked(name string) *Repo {
	for _, r := range d.data.Repos {
		if strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

// Repos returns all repositories as copies, sorted by name case- and
// diacritics-insensitively like People ("Équipe" orders as "equipe"); names
// whose fold keys tie fall back to raw byte order for determinism.
func (d *DB) Repos() []Repo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	type keyed struct {
		key  string
		repo Repo
	}
	rows := make([]keyed, 0, len(d.data.Repos))
	for _, r := range d.data.Repos {
		rows = append(rows, keyed{key: foldKey(r.Name), repo: *r})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].key != rows[j].key {
			return rows[i].key < rows[j].key
		}
		return rows[i].repo.Name < rows[j].repo.Name
	})
	out := make([]Repo, len(rows))
	for i, r := range rows {
		out[i] = r.repo
	}
	return out
}

// GetRepo returns a repository by ID (case-insensitive; IDs are canonically
// lowercase UUIDs as minted by CreateRepo).
func (d *DB) GetRepo(id string) (Repo, bool) {
	id = normRepoID(id)
	d.mu.RLock()
	defer d.mu.RUnlock()
	r := d.findRepoLocked(id)
	if r == nil {
		return Repo{}, false
	}
	return *r, true
}

// GetRepoByName returns a repository by name (case-insensitive).
func (d *DB) GetRepoByName(name string) (Repo, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	r := d.findRepoByNameLocked(name)
	if r == nil {
		return Repo{}, false
	}
	return *r, true
}

// RenameRepo renames the repository identified by id, keeping its CreatedAt
// and share fields. Errors: ErrRepoNotFound, ErrRepoNameTaken
// (another repo already uses the name, case-insensitively), or a wrapped
// persistence failure.
func (d *DB) RenameRepo(id, newName string) (Repo, error) {
	id = normRepoID(id)
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return Repo{}, errors.New("repo name is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.findRepoLocked(id)
	if r == nil {
		return Repo{}, ErrRepoNotFound
	}
	for _, cand := range d.data.Repos {
		if cand == r {
			continue
		}
		if strings.EqualFold(cand.Name, newName) {
			return Repo{}, fmt.Errorf("%w: %q", ErrRepoNameTaken, newName)
		}
	}
	// Copy the stored record and touch only the name, so CreatedAt and the
	// share fields survive (same record-copy rule as the person mutators).
	rec := *r
	rec.Name = newName
	v, err := json.Marshal(rec)
	if err != nil {
		return Repo{}, err
	}
	if err := d.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktRepos).Put([]byte(id), v)
	}); err != nil {
		return Repo{}, err
	}
	// Disk commit succeeded; now update the in-memory mirror.
	r.Name = newName
	return *r, nil
}

// DeleteRepo removes the repository identified by id together with every
// person that belongs to it: one bbolt transaction deletes the repo record,
// the person records and their photo sub-buckets; after the commit each
// deleted person's thumbnail sidecar is removed best-effort. The repo's
// dataset directory (people/<id>/) is NOT touched — the caller owns the
// dataset tree, since the DB does not know the people root. Returns false
// when the repository did not exist.
func (d *DB) DeleteRepo(id string) (bool, error) {
	id = normRepoID(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	ri := -1
	for i, r := range d.data.Repos {
		if r.ID == id {
			ri = i
			break
		}
	}
	if ri < 0 {
		return false, nil
	}
	// The victims are collected from the mirror (the same source RemovePerson
	// uses) so one transaction can delete the repo record, the person records
	// and their photo buckets together.
	type victim struct {
		id    string
		thumb string
	}
	victims := make([]victim, 0)
	for _, p := range d.data.People {
		if p.RepoID == id {
			victims = append(victims, victim{id: p.ID, thumb: p.Thumb})
		}
	}
	err := d.bdb.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bktRepos).Delete([]byte(id)); err != nil {
			return err
		}
		pb, phb := tx.Bucket(bktPeople), tx.Bucket(bktPhotos)
		for _, v := range victims {
			idb := []byte(v.id)
			if err := pb.Delete(idb); err != nil {
				return err
			}
			if err := phb.DeleteBucket(idb); err != nil && !errors.Is(err, boltErr.ErrBucketNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	// Disk commit succeeded; now update the in-memory mirror.
	d.data.Repos = append(d.data.Repos[:ri], d.data.Repos[ri+1:]...)
	kept := d.data.People[:0]
	for _, p := range d.data.People {
		if p.RepoID != id {
			kept = append(kept, p)
		}
	}
	d.data.People = kept
	for _, v := range victims {
		if v.thumb != "" {
			_ = os.Remove(filepath.Join(d.ThumbDir(), v.thumb)) // best-effort
		}
	}
	return true, nil
}

// SetRepoShare marks the repository shared or unshared, storing the share
// token itself. When enabled, token is the plaintext token — it is stored
// verbatim so the link can be re-displayed and presented tokens verified
// against it. When disabled, the token is cleared and the argument is
// ignored. Errors: ErrRepoNotFound or a wrapped persistence failure.
func (d *DB) SetRepoShare(id string, enabled bool, token string) error {
	id = normRepoID(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.findRepoLocked(id)
	if r == nil {
		return ErrRepoNotFound
	}
	rec := *r
	rec.Shared = enabled
	rec.ShareToken = ""
	if enabled {
		if strings.TrimSpace(token) == "" {
			return errors.New("share token is empty")
		}
		rec.ShareToken = token
	}
	v, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := d.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktRepos).Put([]byte(id), v)
	}); err != nil {
		return err
	}
	// Disk commit succeeded; now update the in-memory mirror.
	r.Shared, r.ShareToken = rec.Shared, rec.ShareToken
	return nil
}

// PeopleByRepo returns the persons belonging to repoID as deep copies,
// sorted like People. Legacy persons (RepoID "") are never returned — not
// even for an empty repoID: they are only reachable through the legacy
// accessors.
func (d *DB) PeopleByRepo(repoID string) []Person {
	repoID = normRepoID(repoID)
	d.mu.RLock()
	defer d.mu.RUnlock()
	type keyed struct {
		key    string
		person Person
	}
	rows := make([]keyed, 0, len(d.data.People))
	for _, p := range d.data.People {
		if repoID == "" || p.RepoID != repoID {
			continue
		}
		rows = append(rows, keyed{key: foldKey(p.Name), person: *copyPersonLocked(p)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].key != rows[j].key {
			return rows[i].key < rows[j].key
		}
		return rows[i].person.Name < rows[j].person.Name
	})
	out := make([]Person, len(rows))
	for i, r := range rows {
		out[i] = r.person
	}
	return out
}

// PeopleCount returns how many persons belong to repoID. An empty repoID
// counts 0: legacy persons are not repo-scoped.
func (d *DB) PeopleCount(repoID string) int {
	repoID = normRepoID(repoID)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if repoID == "" {
		return 0
	}
	n := 0
	for _, p := range d.data.People {
		if p.RepoID == repoID {
			n++
		}
	}
	return n
}

// GetInRepo returns a person of repository repoID by name
// (case-insensitive), or nil. Legacy persons never match — the returned
// pointer refers to a deep copy.
func (d *DB) GetInRepo(repoID, name string) *Person {
	repoID = normRepoID(repoID)
	d.mu.RLock()
	defer d.mu.RUnlock()
	p := d.findInRepoLocked(repoID, name)
	if p == nil {
		return nil
	}
	return copyPersonLocked(p)
}

func (d *DB) findInRepoLocked(repoID, name string) *Person {
	if repoID == "" {
		return nil // legacy persons are not repo-scoped
	}
	for _, p := range d.data.People {
		if p.RepoID == repoID && strings.EqualFold(p.Name, name) {
			return p
		}
	}
	return nil
}
