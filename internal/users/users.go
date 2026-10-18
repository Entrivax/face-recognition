// Package users persists recogn's user accounts in a single bbolt file
// (default data/users.db) and mirrors them in memory for lock-protected
// reads. It is the storage half of the multi-user model: usernames with
// argon2id password hashes, per-repository grants ("reader"/"writer") and
// API keys; the auth package builds the HTTP surface on top of it.
//
// Bucket layout:
//
//	users    — key: lowercase username, value: JSON-encoded User
//	api_keys — key: sha256 hex of the full API-key token, value: JSON-encoded APIKey
//	meta     — version marker (uint32 little-endian)
//
// The store follows the same discipline as internal/db: every mutation
// commits a targeted bolt.Update FIRST and only then updates the in-memory
// mirror, so a failed write leaves memory and disk consistent; every read
// hands out deep copies, so a reader can never race a mirror write. The
// bbolt file holds an exclusive lock: a second process on the same file
// fails at Open (tests must Close() before reopening the same path).
package users

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Role constants for Grant.Role.
const (
	RoleReader = "reader"
	RoleWriter = "writer"
)

// Grant authorizes a user on one repository (a recogn person / face-DB
// identity). Role is RoleReader or RoleWriter.
type Grant struct {
	RepoID string `json:"repo_id"`
	Role   string `json:"role"`
}

// User is one account record.
type User struct {
	Username     string    `json:"username"` // canonical (lowercase)
	PasswordHash string    `json:"password_hash"`
	Admin        bool      `json:"admin"`
	Grants       []Grant   `json:"grants,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// APIKey is the persisted half of an API key: everything EXCEPT the token
// itself. The plaintext token is returned once by CreateKey and is never
// stored — only its sha256 (which is the api_keys bucket key) is kept, so a
// leaked database cannot mint working keys.
type APIKey struct {
	ID        string    `json:"id"` // hex(sha256(token))[:12]
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"` // first 12 chars of the token, for UI display
	CreatedAt time.Time `json:"created_at"`
}

// Sentinel errors returned by the mutating methods; test with errors.Is.
var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrUserNotFound  = errors.New("user not found")
	ErrInvalidRole   = errors.New("invalid role")
)

// Bucket and meta-key names inside the bbolt file.
var (
	bktUsers = []byte("users")
	bktKeys  = []byte("api_keys")
	bktMeta  = []byte("meta")

	metaVersionKey = []byte("version")

	storeVersion = uint32(1)

	// openTimeout bounds how long Open waits for the file lock held by
	// another process, so a double start fails fast instead of hanging.
	openTimeout = 3 * time.Second
)

// usernameRE defines the accepted username shape: it must start with a
// letter or digit and then continue with letters, digits, dots, underscores
// or hyphens, up to 32 characters in total.
var usernameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)

// Other limits: usernameMinLen is the shortest username (the regex above
// would also allow a single character); usernameMaxLen matches the regex's
// total-length cap; keyTokenBytes is the entropy of an API-key token;
// keyIDLen/keyPrefixLen are the derived display fields; keyNameMaxLen caps
// the human-readable key name; defaultKeyName is used when a key is created
// with an empty name.
const (
	usernameMinLen = 2
	usernameMaxLen = 32
	keyTokenBytes  = 32
	keyIDLen       = 12
	keyPrefixLen   = 12
	keyNameMaxLen  = 64
	defaultKeyName = "api key"
)

// tokenPrefix marks every API-key token ("rk_" + base64url).
const tokenPrefix = "rk_"

// Store is the on-disk user database plus its in-memory mirror. The zero
// value is not ready; use Open. The store must not be used after Close.
type Store struct {
	path  string
	mu    sync.RWMutex
	bdb   *bolt.DB
	users map[string]User   // key: lowercase username; Username field lowercase too
	keys  map[string]APIKey // key: sha256 hex of the full token
}

// Open opens (creating if needed) the bbolt database at path, creating the
// users/api_keys/meta buckets and loading the mirror.
func Open(path string) (*Store, error) {
	s := &Store{path: path, users: make(map[string]User), keys: make(map[string]APIKey)}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create users dir: %w", err)
	}
	bdb, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open users db %s (locked by another process?): %w", path, err)
	}
	s.bdb = bdb
	if err := bdb.Update(func(tx *bolt.Tx) error {
		mb, err := tx.CreateBucketIfNotExists(bktMeta)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bktUsers); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bktKeys); err != nil {
			return err
		}
		if len(mb.Get(metaVersionKey)) == 0 {
			var v [4]byte
			binary.LittleEndian.PutUint32(v[:], storeVersion)
			return mb.Put(metaVersionKey, v[:])
		}
		return nil
	}); err != nil {
		bdb.Close()
		return nil, fmt.Errorf("init users db %s: %w", path, err)
	}
	if err := s.load(); err != nil {
		bdb.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the bbolt file lock. The store must not be used afterwards.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bdb == nil {
		return nil
	}
	err := s.bdb.Close()
	s.bdb = nil
	return err
}

// load rebuilds the in-memory mirror from the bbolt file. Used only while
// opening, before the store is shared.
func (s *Store) load() error {
	return s.bdb.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if v := mb.Get(metaVersionKey); len(v) == 4 {
			if ver := binary.LittleEndian.Uint32(v); ver != storeVersion {
				return fmt.Errorf("unsupported users db version %d (want %d)", ver, storeVersion)
			}
		}
		ub := tx.Bucket(bktUsers)
		if err := ub.ForEach(func(k, v []byte) error {
			var u User
			if err := json.Unmarshal(v, &u); err != nil {
				return fmt.Errorf("decode user %q: %w", k, err)
			}
			s.users[string(k)] = u
			return nil
		}); err != nil {
			return err
		}
		kb := tx.Bucket(bktKeys)
		return kb.ForEach(func(k, v []byte) error {
			var key APIKey
			if err := json.Unmarshal(v, &key); err != nil {
				return fmt.Errorf("decode api key %q: %w", k, err)
			}
			s.keys[string(k)] = key
			return nil
		})
	})
}

// keyFor normalizes a username to the canonical store key: trimmed and
// lowercased (the accepted charset is ASCII, so no locale folding).
func keyFor(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ValidUsername checks the username shape: trimmed, 2..32 characters,
// starting with a letter or digit, then only letters, digits, '.', '_' or
// '-'. Usernames are stored (and looked up) lowercased, so case differences
// never create distinct accounts.
func ValidUsername(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("username is empty")
	}
	if len(n) < usernameMinLen {
		return fmt.Errorf("username must be at least %d characters", usernameMinLen)
	}
	if len(n) > usernameMaxLen {
		return fmt.Errorf("username must be at most %d characters", usernameMaxLen)
	}
	if !usernameRE.MatchString(n) {
		return errors.New("username must start with a letter or digit and contain only letters, digits, '.', '_' or '-'")
	}
	return nil
}

// Count returns the number of users (the setup gate reads it live).
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// Users returns all users, username-sorted. Every record is a deep copy: the
// Grants slice gets its own backing array, so a reader iterating (or
// mutating) the copy never races the in-place mirror writes.
func (s *Store) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, cloneUser(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// GetUser returns a user by username (case-insensitive; the lookup key is
// the lowercased name), and whether one exists. The returned record is a
// deep copy; mutating it never affects the stored record.
func (s *Store) GetUser(username string) (User, bool) {
	key := keyFor(username)
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[key]
	if !ok {
		return User{}, false
	}
	return cloneUser(u), true
}

// CreateUser validates the username and password, hashes the password and
// stores the new user. Errors: ValidUsername's shape errors, HashPassword's
// length error, ErrUsernameTaken (case-insensitive), or a wrapped
// persistence failure.
func (s *Store) CreateUser(username, password string, admin bool) (User, error) {
	name := strings.TrimSpace(username)
	if err := ValidUsername(name); err != nil {
		return User{}, err
	}
	name = strings.ToLower(name)
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	u := User{Username: name, PasswordHash: hash, Admin: admin, CreatedAt: now, UpdatedAt: now}
	v, err := json.Marshal(u)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[name]; exists {
		return User{}, fmt.Errorf("%w: %q", ErrUsernameTaken, name)
	}
	if err := s.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktUsers).Put([]byte(name), v)
	}); err != nil {
		return User{}, err
	}
	// The disk commit succeeded; now update the in-memory mirror.
	s.users[name] = cloneUser(u)
	return cloneUser(u), nil
}

// DeleteUser deletes the user (case-insensitive) and CASCADES: every API key
// owned by the user is deleted in the same transaction, so the store never
// keeps ownerless keys. Returns whether a user was deleted.
func (s *Store) DeleteUser(username string) (bool, error) {
	key := keyFor(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[key]
	if !ok {
		return false, nil
	}
	owned := make([]string, 0)
	for h, k := range s.keys {
		if k.Username == u.Username {
			owned = append(owned, h)
		}
	}
	err := s.bdb.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bktUsers).Delete([]byte(key)); err != nil {
			return err
		}
		kb := tx.Bucket(bktKeys)
		for _, h := range owned {
			if err := kb.Delete([]byte(h)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	// The disk commit succeeded; now update the in-memory mirror.
	delete(s.users, key)
	for _, h := range owned {
		delete(s.keys, h)
	}
	return true, nil
}

// SetPassword re-hashes and stores the user's password and bumps UpdatedAt.
// Errors: HashPassword's length error, ErrUserNotFound, or a wrapped
// persistence failure.
func (s *Store) SetPassword(username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	key := keyFor(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[key]
	if !ok {
		return ErrUserNotFound
	}
	rec := cloneUser(u)
	rec.PasswordHash = hash
	rec.UpdatedAt = time.Now().UTC()
	if err := s.updateUserLocked(key, rec); err != nil {
		return err
	}
	return nil
}

// SetGrants replaces the user's whole grant list and bumps UpdatedAt. Every
// role must be RoleReader or RoleWriter (else ErrInvalidRole) — validation
// happens before anything is written, so a rejected call leaves the user
// untouched. The slice is cloned, so the caller may reuse its backing array
// afterwards. Errors: ErrInvalidRole, ErrUserNotFound, or a wrapped
// persistence failure.
func (s *Store) SetGrants(username string, grants []Grant) error {
	for _, g := range grants {
		if g.Role != RoleReader && g.Role != RoleWriter {
			return fmt.Errorf("%w: %q (want %q or %q)", ErrInvalidRole, g.Role, RoleReader, RoleWriter)
		}
	}
	key := keyFor(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[key]
	if !ok {
		return ErrUserNotFound
	}
	rec := cloneUser(u)
	rec.Grants = append([]Grant(nil), grants...)
	rec.UpdatedAt = time.Now().UTC()
	return s.updateUserLocked(key, rec)
}

// updateUserLocked persists rec and updates the mirror. Caller holds s.mu.
func (s *Store) updateUserLocked(key string, rec User) error {
	v, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := s.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktUsers).Put([]byte(key), v)
	}); err != nil {
		return err
	}
	s.users[key] = rec
	return nil
}

// Role returns the user's stored role on repoID: "writer", "reader", or ""
// when the user is unknown or has no grant for the repository. The method is
// deliberately literal about Admin — admins have no grants of their own; the
// auth layer's RoleFor treats an admin as a writer on every repo.
func (s *Store) Role(username, repoID string) string {
	u, ok := s.GetUser(username)
	if !ok {
		return ""
	}
	for _, g := range u.Grants {
		if g.RepoID == repoID {
			return g.Role
		}
	}
	return ""
}

// CreateKey mints an API key for the user: the token is "rk_" + base64url
// of 32 random bytes, and ONLY its sha256 (the api_keys bucket key), ID
// (first 12 hex chars of that sha256) and display prefix (first 12 chars of
// the token) are stored. The plaintext token is returned exactly once.
//
// keyName is trimmed; an empty name becomes "api key"; names longer than 64
// characters are truncated (the name is a label, not a credential).
// Errors: ErrUserNotFound, or a wrapped random/persistence failure.
func (s *Store) CreateKey(username, keyName string) (string, APIKey, error) {
	name := keyFor(username)
	keyName = strings.TrimSpace(keyName)
	if keyName == "" {
		keyName = defaultKeyName
	}
	if r := []rune(keyName); len(r) > keyNameMaxLen {
		keyName = string(r[:keyNameMaxLen])
	}
	raw := make([]byte, keyTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", APIKey{}, fmt.Errorf("generate api key: %w", err)
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	key := APIKey{
		ID:        hash[:keyIDLen],
		Username:  name,
		Name:      keyName,
		Prefix:    token[:keyPrefixLen],
		CreatedAt: time.Now().UTC(),
	}
	v, err := json.Marshal(key)
	if err != nil {
		return "", APIKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[name]; !ok {
		return "", APIKey{}, ErrUserNotFound
	}
	if _, exists := s.keys[hash]; exists {
		// Astronomically unlikely (same sha256 twice) but refusing beats
		// silently overwriting a live credential.
		return "", APIKey{}, errors.New("api key collision; try again")
	}
	if err := s.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktKeys).Put([]byte(hash), v)
	}); err != nil {
		return "", APIKey{}, err
	}
	// The disk commit succeeded; now update the in-memory mirror.
	s.keys[hash] = key
	return token, cloneKey(key), nil
}

// LookupKey resolves a presented API-key token to its owner's FRESH user
// record. The token must carry the "rk_" prefix; it is hashed and looked up
// in the api_keys bucket, then the owning user record is loaded. Unknown
// tokens, foreign prefixes and keys whose owner vanished all return false.
func (s *Store) LookupKey(token string) (User, bool) {
	t := strings.TrimSpace(token)
	if !strings.HasPrefix(t, tokenPrefix) {
		return User{}, false
	}
	sum := sha256.Sum256([]byte(t))
	hash := hex.EncodeToString(sum[:])
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[hash]
	if !ok {
		return User{}, false
	}
	u, ok := s.users[key.Username]
	if !ok {
		return User{}, false
	}
	return cloneUser(u), true
}

// ListKeys returns the user's API keys (case-insensitive owner match),
// creation-sorted. The list never contains the token itself. Unknown users
// simply have no keys.
func (s *Store) ListKeys(username string) []APIKey {
	name := keyFor(username)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]APIKey, 0)
	for _, k := range s.keys {
		if k.Username == name {
			out = append(out, cloneKey(k))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DeleteKey removes an API key by ID — but only when it belongs to the
// named user (owner-scoped; a foreign or unknown ID returns false, never an
// error, so callers cannot probe for other users' key IDs).
func (s *Store) DeleteKey(username, id string) (bool, error) {
	name := keyFor(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	var hash string
	for h, k := range s.keys {
		if k.ID == id && k.Username == name {
			hash = h
			break
		}
	}
	if hash == "" {
		return false, nil
	}
	if err := s.bdb.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktKeys).Delete([]byte(hash))
	}); err != nil {
		return false, err
	}
	// The disk commit succeeded; now update the in-memory mirror.
	delete(s.keys, hash)
	return true, nil
}

// cloneUser returns a deep copy of u for handing to callers: the Grants
// slice gets its own backing array, so a reader iterating (or mutating) the
// copy never races the in-place mirror writes (same rule as db's
// copyPersonLocked).
func cloneUser(u User) User {
	u.Grants = append([]Grant(nil), u.Grants...)
	return u
}

// cloneKey duplicates an APIKey value (all scalar fields today).
func cloneKey(k APIKey) APIKey { return k }
