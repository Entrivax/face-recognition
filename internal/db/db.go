// Package db persists the face database in a single bbolt file (default
// data/faces.db) and mirrors it in memory for lock-protected reads. A JSON
// interchange file (embeddings.json next to the database file) keeps the
// store human-readable: it is imported whenever the store is empty (so
// deleting the bbolt file and restarting re-imports it) and written by
// Export / `recogn export`.
//
// The bbolt file maps people (stable ID + display name) to the set of face
// embeddings computed from their enrolled photos. It is deliberately simple
// so the dataset is easy to expand: add a folder under people/ and
// re-enroll, or upload photos through the API.
package db

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	boltErr "go.etcd.io/bbolt/errors"
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

// DB is the on-disk face database plus its in-memory mirror. The zero value
// is not ready; use Open. The DB must not be used after Close.
type DB struct {
	path string // bbolt file
	mu   sync.RWMutex
	data fileFormat // in-memory mirror, updated only after a committed write
	bdb  *bolt.DB
}

// Sentinel errors returned by mutating methods; test with errors.Is.
var (
	ErrPersonNotFound = errors.New("person not found")
	ErrNameTaken      = errors.New("person name already in use")
	ErrIDTaken        = errors.New("person id already in use")
)

// fileFormat is the JSON interchange document schema (versioned for future
// migrations). Threshold is an additive, optional setting: nil when never set
// (older files and fresh databases), and a pointer so an explicitly stored 0
// survives. The same shape serves as the DB's in-memory mirror.
type fileFormat struct {
	Version   int       `json:"version"`
	People    []*Person `json:"people"`
	Threshold *float64  `json:"threshold,omitempty"`
}

// Bucket and meta-key names inside the bbolt file.
var (
	bktMeta   = []byte("meta")
	bktPeople = []byte("people")
	bktPhotos = []byte("photos")

	metaVersionKey   = []byte("version")
	metaThresholdKey = []byte("threshold")

	dbVersion = uint32(1)

	// interchangeName is the JSON interchange file, looked up next to the
	// bbolt file (data/embeddings.json for a DB at data/faces.db).
	interchangeName = "embeddings.json"

	// openTimeout bounds how long Open waits for the file lock held by
	// another process, so a double start fails fast instead of hanging.
	openTimeout = 3 * time.Second
)

// Open opens (creating if needed) the bbolt database at path, loads its
// contents into memory, and imports the JSON interchange file (embeddings.json
// next to path) if the store is empty. The JSON file is left in place and
// never modified by the import, so deleting the bbolt file and restarting
// re-imports it. Open fails if a JSON file exists but cannot be parsed
// (failing loudly beats silently starting empty).
func Open(path string) (*DB, error) {
	d := &DB{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	bdb, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open db %s (locked by another process?): %w", path, err)
	}
	d.bdb = bdb
	if err := bdb.Update(func(tx *bolt.Tx) error {
		mb, err := tx.CreateBucketIfNotExists(bktMeta)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bktPeople); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bktPhotos); err != nil {
			return err
		}
		if len(mb.Get(metaVersionKey)) == 0 {
			var v [4]byte
			binary.LittleEndian.PutUint32(v[:], dbVersion)
			return mb.Put(metaVersionKey, v[:])
		}
		return nil
	}); err != nil {
		bdb.Close()
		return nil, fmt.Errorf("init db %s: %w", path, err)
	}
	if err := d.load(); err != nil {
		bdb.Close()
		return nil, err
	}
	if err := d.importJSONIfNeeded(); err != nil {
		bdb.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the bbolt file lock. The DB must not be used afterwards.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bdb == nil {
		return nil
	}
	err := d.bdb.Close()
	d.bdb = nil
	return err
}

// Path returns the backing bbolt file path.
func (d *DB) Path() string { return d.path }

// interchangePath returns the JSON interchange file path: embeddings.json in
// the database file's directory.
func (d *DB) interchangePath() string {
	return filepath.Join(filepath.Dir(d.path), interchangeName)
}

// load rebuilds the in-memory mirror from the bbolt file. Used only while
// opening, before the DB is shared.
func (d *DB) load() error {
	d.data = fileFormat{Version: 1}
	return d.bdb.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if v := mb.Get(metaVersionKey); len(v) == 4 {
			if ver := binary.LittleEndian.Uint32(v); ver != dbVersion {
				return fmt.Errorf("unsupported database version %d (want %d)", ver, dbVersion)
			}
		}
		if v := mb.Get(metaThresholdKey); len(v) == 8 {
			f := float64FromBytes(v)
			d.data.Threshold = &f
		}
		pb := tx.Bucket(bktPeople)
		phb := tx.Bucket(bktPhotos)
		return pb.ForEach(func(id, v []byte) error {
			var p Person
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("decode person %q: %w", id, err)
			}
			p.Photos = nil
			if sb := phb.Bucket(id); sb != nil {
				if err := sb.ForEach(func(k, val []byte) error {
					hash, emb, err := decodePhoto(val)
					if err != nil {
						return fmt.Errorf("decode photo %q of %q: %w", k, p.Name, err)
					}
					p.Photos = append(p.Photos, Photo{Path: string(k), Hash: hash, Embedding: emb})
					return nil
				}); err != nil {
					return err
				}
			}
			sort.Slice(p.Photos, func(i, j int) bool { return p.Photos[i].Path < p.Photos[j].Path })
			d.data.People = append(d.data.People, &p)
			return nil
		})
	})
}

// importJSONIfNeeded imports the JSON interchange file into the bbolt store
// when the store has no people. The file is read but never renamed or
// rewritten; on parse failure Open fails, so corrupt data is never silently
// dropped.
func (d *DB) importJSONIfNeeded() error {
	if len(d.data.People) != 0 {
		return nil
	}
	jsonPath := d.interchangePath()
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", jsonPath, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil // empty file: nothing to import
	}
	var ff fileFormat
	if err := json.Unmarshal(b, &ff); err != nil {
		return fmt.Errorf("parse %s: %w", jsonPath, err)
	}
	err = d.bdb.Update(func(tx *bolt.Tx) error {
		pb := tx.Bucket(bktPeople)
		phb := tx.Bucket(bktPhotos)
		for _, p := range ff.People {
			if p == nil || strings.TrimSpace(p.Name) == "" {
				continue
			}
			id := p.ID
			if id == "" {
				id = newID(p.Name)
			}
			v, err := json.Marshal(Person{ID: id, Name: p.Name, Thumb: p.Thumb, ThumbSrc: p.ThumbSrc})
			if err != nil {
				return err
			}
			if err := pb.Put([]byte(id), v); err != nil {
				return err
			}
			sb, err := phb.CreateBucketIfNotExists([]byte(id))
			if err != nil {
				return err
			}
			for _, ph := range p.Photos {
				if err := sb.Put([]byte(ph.Path), encodePhoto(ph.Hash, ph.Embedding)); err != nil {
					return err
				}
			}
		}
		if ff.Threshold != nil {
			return tx.Bucket(bktMeta).Put(metaThresholdKey, float64Bytes(*ff.Threshold))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("import %s: %w", jsonPath, err)
	}
	if err := d.load(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "db: imported %d people from %s (file left in place; 'recogn export' refreshes it)\n",
		len(d.data.People), jsonPath)
	return nil
}

// Export writes the JSON interchange file to the default location
// (embeddings.json next to the database) and returns its path.
func (d *DB) Export() (string, error) { return d.ExportTo("") }

// ExportTo writes the JSON interchange file to path (empty means the default
// location) and returns the path used. The write is atomic (temp file +
// rename) and never touches the bbolt store. People are written name-sorted,
// so repeated exports produce byte-stable files that diff cleanly.
func (d *DB) ExportTo(path string) (string, error) {
	if path == "" {
		path = d.interchangePath()
	}
	d.mu.RLock()
	doc := fileFormat{Version: d.data.Version, Threshold: d.data.Threshold, People: make([]*Person, len(d.data.People))}
	for i, p := range d.data.People {
		cp := *p
		doc.People[i] = &cp
	}
	d.mu.RUnlock()
	sort.Slice(doc.People, func(i, j int) bool {
		if doc.People[i].Name != doc.People[j].Name {
			return doc.People[i].Name < doc.People[j].Name
		}
		return doc.People[i].ID < doc.People[j].ID
	})
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// Threshold returns the persisted match threshold, or nil when none was ever
// stored. Callers fall back to the environment/default when nil.
func (d *DB) Threshold() *float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.data.Threshold
}

// SetThreshold persists the match threshold in the database. The engine's
// in-memory threshold is updated separately by the caller.
func (d *DB) SetThreshold(t float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktMeta).Put(metaThresholdKey, float64Bytes(t))
	})
	if err != nil {
		return err
	}
	v := t
	d.data.Threshold = &v
	return nil
}

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
	return d.findByIDLocked(id)
}

func (d *DB) findByIDLocked(id string) *Person {
	for _, p := range d.data.People {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// AddPhoto appends (or replaces, if the same path exists) a photo embedding
// for the named person, creating the person if needed, and persists the
// change.
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
	if relPath == "" {
		return errors.New("photo path is empty")
	}
	val := encodePhoto(hash, embedding)
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.findLocked(name)
	var person Person
	isNew := p == nil
	if isNew {
		person = Person{ID: newID(name), Name: name}
		// Two different names can slugify to the same ID; in the bbolt
		// layout they would share one photo bucket, so refuse instead of
		// silently letting them corrupt each other's photos.
		if other := d.findByIDLocked(person.ID); other != nil {
			return fmt.Errorf("%w: %q (in use by %q)", ErrIDTaken, person.ID, other.Name)
		}
	} else {
		person = Person{ID: p.ID, Name: p.Name, Thumb: p.Thumb, ThumbSrc: p.ThumbSrc}
	}
	rec, err := json.Marshal(person)
	if err != nil {
		return err
	}
	id := []byte(person.ID)
	err = d.bdb.Update(func(tx *bolt.Tx) error {
		if isNew {
			if err := tx.Bucket(bktPeople).Put(id, rec); err != nil {
				return err
			}
		}
		sb, err := tx.Bucket(bktPhotos).CreateBucketIfNotExists(id)
		if err != nil {
			return err
		}
		return sb.Put([]byte(relPath), val)
	})
	if err != nil {
		return err
	}
	// The disk commit succeeded; now update the in-memory mirror.
	if isNew {
		p = &Person{ID: person.ID, Name: person.Name}
		d.data.People = append(d.data.People, p)
	}
	photo := Photo{Path: relPath, Hash: hash, Embedding: embedding}
	replaced := false
	for i := range p.Photos {
		if p.Photos[i].Path == relPath {
			p.Photos[i] = photo
			replaced = true
			break
		}
	}
	if !replaced {
		p.Photos = append(p.Photos, photo)
	}
	sort.Slice(p.Photos, func(i, j int) bool { return p.Photos[i].Path < p.Photos[j].Path })
	return nil
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
	idx := -1
	var p *Person
	for i, cand := range d.data.People {
		if strings.EqualFold(cand.Name, name) {
			idx, p = i, cand
			break
		}
	}
	if p == nil {
		return false, nil
	}
	id := []byte(p.ID)
	err := d.bdb.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bktPeople).Delete(id); err != nil {
			return err
		}
		if err := tx.Bucket(bktPhotos).DeleteBucket(id); err != nil && !errors.Is(err, boltErr.ErrBucketNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	thumb := p.Thumb
	d.data.People = append(d.data.People[:idx], d.data.People[idx+1:]...)
	if thumb != "" {
		_ = os.Remove(filepath.Join(d.ThumbDir(), thumb)) // best-effort
	}
	return true, nil
}

// ThumbDir returns the sidecar directory holding face thumbnails, located
// next to the database file (data/thumbs for a DB at data/faces.db).
func (d *DB) ThumbDir() string {
	return filepath.Join(filepath.Dir(d.path), "thumbs")
}

// RenamePerson renames a person (oldName matched case-insensitively) and
// re-derives their ID from the new name, keeping every derived artifact
// consistent: the thumbnail sidecar file is renamed <newID>.jpg and the
// person record (with its whole photo set) moves to the new ID inside one
// bbolt transaction. Photo paths and ThumbSrc are folder-relative basenames,
// so they are unaffected by a rename. Returns the updated person.
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
	thumb, thumbSrc := p.Thumb, p.ThumbSrc
	if id != p.ID && thumb != "" {
		oldThumb := filepath.Join(d.ThumbDir(), thumb)
		if _, err := os.Stat(oldThumb); err == nil {
			if err := os.MkdirAll(d.ThumbDir(), 0o755); err != nil {
				return nil, fmt.Errorf("create thumbnail dir: %w", err)
			}
			if err := os.Rename(oldThumb, filepath.Join(d.ThumbDir(), id+".jpg")); err != nil {
				return nil, fmt.Errorf("rename thumbnail: %w", err)
			}
			thumb = id + ".jpg"
		} else {
			// Sidecar already gone: clear the stale record so the next
			// rescan backfills a thumbnail under the new ID.
			thumb, thumbSrc = "", ""
		}
	}
	rec, err := json.Marshal(Person{ID: id, Name: newName, Thumb: thumb, ThumbSrc: thumbSrc})
	if err != nil {
		return nil, err
	}
	newIDb, oldIDb := []byte(id), []byte(p.ID)
	err = d.bdb.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bktPeople).Put(newIDb, rec); err != nil {
			return err
		}
		phb := tx.Bucket(bktPhotos)
		if id != p.ID {
			// Move the photo bucket to the new ID (bbolt has no bucket
			// rename; copy within the same transaction, then drop the old).
			if old := phb.Bucket(oldIDb); old != nil {
				sb, err := phb.CreateBucketIfNotExists(newIDb)
				if err != nil {
					return err
				}
				c := old.Cursor()
				for k, v := c.First(); k != nil; k, v = c.Next() {
					if err := sb.Put(k, v); err != nil {
						return err
					}
				}
				if err := phb.DeleteBucket(oldIDb); err != nil {
					return err
				}
			}
			if err := tx.Bucket(bktPeople).Delete(oldIDb); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.ID, p.Name, p.Thumb, p.ThumbSrc = id, newName, thumb, thumbSrc
	updated := *p
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
	p := d.findByIDLocked(personID)
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
	rec := Person{ID: p.ID, Name: p.Name, Thumb: fileName, ThumbSrc: srcPhotoPath}
	if err := d.putPersonLocked(rec); err != nil {
		return err
	}
	p.Thumb = fileName
	p.ThumbSrc = srcPhotoPath
	return nil
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
	p := d.findByIDLocked(personID)
	if p == nil {
		return fmt.Errorf("person id %q not found", personID)
	}
	if p.Thumb != "" {
		_ = os.Remove(filepath.Join(d.ThumbDir(), p.Thumb)) // best-effort
	}
	if err := d.putPersonLocked(Person{ID: p.ID, Name: p.Name}); err != nil {
		return err
	}
	p.Thumb, p.ThumbSrc = "", ""
	return nil
}

// putPersonLocked persists one person's record (not its photos) to the
// people bucket. Callers hold mu.
func (d *DB) putPersonLocked(p Person) error {
	v, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return d.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktPeople).Put([]byte(p.ID), v)
	})
}

// RemovePhoto deletes a single photo from a person.
func (d *DB) RemovePhoto(name, relPath string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.findLocked(name)
	if p == nil {
		return false, nil
	}
	idx := -1
	for i, ph := range p.Photos {
		if ph.Path == relPath {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	err := d.bdb.Update(func(tx *bolt.Tx) error {
		if sb := tx.Bucket(bktPhotos).Bucket([]byte(p.ID)); sb != nil {
			return sb.Delete([]byte(relPath))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	p.Photos = append(p.Photos[:idx], p.Photos[idx+1:]...)
	return true, nil
}

// encodePhoto packs a photo record as uvarint(hashLen) + hash bytes +
// uvarint(dim) + dim little-endian float32 values (2 KB for a 512-d
// embedding, versus ~22 KB as JSON text).
func encodePhoto(hash string, emb []float32) []byte {
	buf := make([]byte, 0, 2*binary.MaxVarintLen32+len(hash)+4*len(emb))
	buf = binary.AppendUvarint(buf, uint64(len(hash)))
	buf = append(buf, hash...)
	buf = binary.AppendUvarint(buf, uint64(len(emb)))
	var scratch [4]byte
	for _, f := range emb {
		binary.LittleEndian.PutUint32(scratch[:], math.Float32bits(f))
		buf = append(buf, scratch[:]...)
	}
	return buf
}

// decodePhoto is the inverse of encodePhoto. It validates that the declared
// embedding length matches the record, so a truncated or corrupt value fails
// loudly instead of yielding garbage embeddings.
func decodePhoto(b []byte) (hash string, emb []float32, err error) {
	hl, n := binary.Uvarint(b)
	if n <= 0 {
		return "", nil, fmt.Errorf("bad hash length prefix")
	}
	b = b[n:]
	if uint64(len(b)) < hl {
		return "", nil, fmt.Errorf("hash truncated")
	}
	hash = string(b[:hl])
	b = b[hl:]
	dim, n := binary.Uvarint(b)
	if n <= 0 {
		return "", nil, fmt.Errorf("bad embedding length prefix")
	}
	b = b[n:]
	if uint64(len(b)) != dim*4 {
		return "", nil, fmt.Errorf("embedding size mismatch: header says %d floats, %d bytes left", dim, len(b))
	}
	emb = make([]float32, dim)
	for i := range emb {
		emb[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return hash, emb, nil
}

func float64Bytes(f float64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, math.Float64bits(f))
	return b
}

func float64FromBytes(b []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
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
