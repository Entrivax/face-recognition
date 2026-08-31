// Package db persists the face database as a single human-editable JSON file.
// The database maps people (stable ID + display name) to the set of face
// embeddings computed from their enrolled photos. It is deliberately simple so
// the dataset is easy to expand: add a folder under people/ and re-enroll, or
// upload photos through the API.
package db

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Photo records one enrolled image and the embedding derived from it. Hash is
// the content hash used to skip re-embedding unchanged files on re-enroll.
type Photo struct {
	Path      string    `json:"path"`      // file name, relative to the person's folder under the people dir
	Hash      string    `json:"hash"`      // sha1 of file bytes
	Embedding []float32 `json:"embedding"` // 512-d ArcFace embedding
}

// Person is one enrolled identity.
type Person struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Photos   []Photo `json:"photos"`
	Thumb    string  `json:"thumb,omitempty"`     // face thumbnail sidecar file name in ThumbDir
	ThumbSrc string  `json:"thumb_src,omitempty"` // enrolled photo the thumbnail was generated from
}

// DB is the on-disk face database. The zero value is not ready; use Open.
type DB struct {
	path string
	mu   sync.RWMutex
	data fileFormat
}

// Sentinel errors returned by RenamePerson; test with errors.Is.
var (
	ErrPersonNotFound = errors.New("person not found")
	ErrNameTaken      = errors.New("person name already in use")
)

// fileFormat is the JSON document schema (versioned for future migrations).
type fileFormat struct {
	Version int       `json:"version"`
	People  []*Person `json:"people"`
}

// Open loads the database from path, creating an empty one if it doesn't
// exist. The file is written on the first mutating call.
func Open(path string) (*DB, error) {
	d := &DB{path: path}
	d.data.Version = 1
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return d, nil
		}
		return nil, fmt.Errorf("read db: %w", err)
	}
	if len(b) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(b, &d.data); err != nil {
		return nil, fmt.Errorf("parse db %s: %w", path, err)
	}
	return d, nil
}

// Path returns the backing file path.
func (d *DB) Path() string { return d.path }

// People returns a copy of all people, sorted by name.
func (d *DB) People() []Person {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Person, 0, len(d.data.People))
	for _, p := range d.data.People {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a person by name (case-insensitive), or nil.
func (d *DB) Get(name string) *Person {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.findLocked(name)
}

func (d *DB) findLocked(name string) *Person {
	for _, p := range d.data.People {
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	return nil
}

// GetByID returns a person by ID, or nil.
func (d *DB) GetByID(id string) *Person {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, p := range d.data.People {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// AddPhoto appends (or replaces, if the same path exists) a photo embedding
// for the named person, creating the person if needed, and persists the DB.
func (d *DB) AddPhoto(name, relPath string, imgBytes []byte, embedding []float32) error {
	return d.addPhoto(name, relPath, hashBytes(imgBytes), embedding)
}

// AddPhotoHashed is AddPhoto when the caller already knows the content hash.
func (d *DB) AddPhotoHashed(name, relPath, hash string, embedding []float32) error {
	return d.addPhoto(name, relPath, hash, embedding)
}

func (d *DB) addPhoto(name, relPath, hash string, embedding []float32) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("person name is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.findLocked(name)
	if p == nil {
		p = &Person{ID: newID(name), Name: name}
		d.data.People = append(d.data.People, p)
	}
	// Replace an existing entry for the same path, else append.
	replaced := false
	for i := range p.Photos {
		if p.Photos[i].Path == relPath {
			p.Photos[i] = Photo{Path: relPath, Hash: hash, Embedding: embedding}
			replaced = true
			break
		}
	}
	if !replaced {
		p.Photos = append(p.Photos, Photo{Path: relPath, Hash: hash, Embedding: embedding})
	}
	sort.Slice(p.Photos, func(i, j int) bool { return p.Photos[i].Path < p.Photos[j].Path })
	return d.saveLocked()
}

// PhotoHash returns the stored content hash for a person's photo path, or "".
func (d *DB) PhotoHash(name, relPath string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p := d.findLocked(name)
	if p == nil {
		return ""
	}
	for _, ph := range p.Photos {
		if ph.Path == relPath {
			return ph.Hash
		}
	}
	return ""
}

// RemovePerson deletes a person (by name, case-insensitive). Returns whether
// one existed. The person's face thumbnail sidecar, if any, is deleted too.
func (d *DB) RemovePerson(name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, p := range d.data.People {
		if strings.EqualFold(p.Name, name) {
			thumb := p.Thumb
			d.data.People = append(d.data.People[:i], d.data.People[i+1:]...)
			if thumb != "" {
				_ = os.Remove(filepath.Join(d.ThumbDir(), thumb)) // best-effort
			}
			return true, d.saveLocked()
		}
	}
	return false, nil
}

// ThumbDir returns the sidecar directory holding face thumbnails, located
// next to the database file (data/thumbs for a DB at data/embeddings.json).
func (d *DB) ThumbDir() string {
	return filepath.Join(filepath.Dir(d.path), "thumbs")
}

// RenamePerson renames a person (oldName matched case-insensitively) and
// re-derives their ID from the new name, keeping every derived artifact
// consistent: the thumbnail sidecar file is renamed <newID>.jpg and the
// Thumb field updated. Photo paths and ThumbSrc are folder-relative
// basenames, so they are unaffected by a rename. Returns the updated person.
//
// Errors: ErrPersonNotFound, ErrNameTaken (another person already uses the
// new name, or the derived ID), or a wrapped filesystem/persistence failure.
func (d *DB) RenamePerson(oldName, newName string) (*Person, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, errors.New("person name is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.findLocked(oldName)
	if p == nil {
		return nil, ErrPersonNotFound
	}
	id := newID(newName)
	for _, cand := range d.data.People {
		if cand == p {
			continue
		}
		if strings.EqualFold(cand.Name, newName) || cand.ID == id {
			return nil, fmt.Errorf("%w: %q", ErrNameTaken, newName)
		}
	}
	// Move the thumbnail sidecar with the ID change (a case-only rename
	// keeps the ID, so nothing to do there).
	if id != p.ID && p.Thumb != "" {
		oldThumb := filepath.Join(d.ThumbDir(), p.Thumb)
		if _, err := os.Stat(oldThumb); err == nil {
			if err := os.MkdirAll(d.ThumbDir(), 0o755); err != nil {
				return nil, fmt.Errorf("create thumbnail dir: %w", err)
			}
			if err := os.Rename(oldThumb, filepath.Join(d.ThumbDir(), id+".jpg")); err != nil {
				return nil, fmt.Errorf("rename thumbnail: %w", err)
			}
			p.Thumb = id + ".jpg"
		} else {
			// Sidecar already gone: clear the stale record so the next
			// rescan backfills a thumbnail under the new ID.
			p.Thumb = ""
			p.ThumbSrc = ""
		}
	}
	p.ID = id
	p.Name = newName
	updated := *p
	if err := d.saveLocked(); err != nil {
		return nil, err
	}
	return &updated, nil
}

// SetThumbnail stores jpg as the person's face thumbnail sidecar
// (<personID>.jpg under ThumbDir) and records the file name on the person
// along with the enrolled photo path it was generated from (srcPhotoPath).
// Any previous thumbnail is replaced; enrollment callers pass a photo only
// when the person has none (first-wins), while the API's thumbnail-chooser
// calls it explicitly to re-select.
func (d *DB) SetThumbnail(personID string, jpg []byte, srcPhotoPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var p *Person
	for _, cand := range d.data.People {
		if cand.ID == personID {
			p = cand
			break
		}
	}
	if p == nil {
		return fmt.Errorf("person %q not found", personID)
	}
	dir := d.ThumbDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create thumbnail dir: %w", err)
	}
	fileName := personID + ".jpg"
	if err := os.WriteFile(filepath.Join(dir, fileName), jpg, 0o644); err != nil {
		return fmt.Errorf("write thumbnail: %w", err)
	}
	p.Thumb = fileName
	p.ThumbSrc = srcPhotoPath
	return d.saveLocked()
}

// ThumbFile returns the full path of the person's recorded thumbnail, or ""
// when the person is unknown or has no thumbnail.
func (d *DB) ThumbFile(personID string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, p := range d.data.People {
		if p.ID == personID && p.Thumb != "" {
			return filepath.Join(d.ThumbDir(), p.Thumb)
		}
	}
	return ""
}

// ClearThumbnail removes the person's face thumbnail sidecar and clears the
// recorded Thumb/ThumbSrc fields. The sidecar file removal is best-effort.
// Used when the thumbnail's source photo is deleted from the person and no
// other photo can regenerate it.
func (d *DB) ClearThumbnail(personID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.data.People {
		if p.ID == personID {
			if p.Thumb != "" {
				_ = os.Remove(filepath.Join(d.ThumbDir(), p.Thumb)) // best-effort
			}
			p.Thumb = ""
			p.ThumbSrc = ""
			return d.saveLocked()
		}
	}
	return fmt.Errorf("person id %q not found", personID)
}

// RemovePhoto deletes a single photo from a person.
func (d *DB) RemovePhoto(name, relPath string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.findLocked(name)
	if p == nil {
		return false, nil
	}
	for i, ph := range p.Photos {
		if ph.Path == relPath {
			p.Photos = append(p.Photos[:i], p.Photos[i+1:]...)
			return true, d.saveLocked()
		}
	}
	return false, nil
}

// saveLocked writes the DB atomically (temp file + rename). Callers hold mu.
func (d *DB) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

// hashBytes returns the sha1 hex of b.
func hashBytes(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// HashBytes is exported for enrollment code that needs the same hash scheme.
func HashBytes(b []byte) string { return hashBytes(b) }

// newID derives a stable, filesystem-safe ID from a name.
func newID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			sb.WriteRune('-')
		}
	}
	id := sb.String()
	id = strings.Trim(id, "-")
	if id == "" {
		sum := sha1.Sum([]byte(name))
		id = "p-" + hex.EncodeToString(sum[:])[:8]
	}
	return id
}
