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
	Path      string    `json:"path"`      // path relative to the people dir
	Hash      string    `json:"hash"`      // sha1 of file bytes
	Embedding []float32 `json:"embedding"` // 512-d ArcFace embedding
}

// Person is one enrolled identity.
type Person struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Photos []Photo `json:"photos"`
}

// DB is the on-disk face database. The zero value is not ready; use Open.
type DB struct {
	path string
	mu   sync.RWMutex
	data fileFormat
}

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
// one existed.
func (d *DB) RemovePerson(name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, p := range d.data.People {
		if strings.EqualFold(p.Name, name) {
			d.data.People = append(d.data.People[:i], d.data.People[i+1:]...)
			return true, d.saveLocked()
		}
	}
	return false, nil
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
