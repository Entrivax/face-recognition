package users

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCreateKeyShape(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}

	token, key, err := s.CreateKey("Alice", "cli key")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// Token shape: "rk_" + base64url of 32 bytes (43 chars) = 46 chars,
	// unpadded base64url alphabet only.
	if !strings.HasPrefix(token, "rk_") {
		t.Fatalf("token %q lacks the rk_ prefix", token)
	}
	if len(token) != len("rk_")+43 {
		t.Fatalf("token length = %d, want 46", len(token))
	}
	if strings.ContainsAny(token, "+/=") {
		t.Fatalf("token must be raw-url base64: %q", token)
	}
	if key.Username != "alice" {
		t.Fatalf("key owner = %q, want lowercase username", key.Username)
	}
	if key.Name != "cli key" {
		t.Fatalf("key name = %q", key.Name)
	}
	if key.Prefix != token[:12] {
		t.Fatalf("prefix = %q, want the token's first 12 chars", key.Prefix)
	}
	if len(key.ID) != 12 {
		t.Fatalf("key ID length = %d, want 12 hex chars", len(key.ID))
	}
	if strings.Contains(strings.ToLower(key.ID), "g") {
		t.Fatalf("key ID is not hex: %q", key.ID)
	}
	if key.CreatedAt.IsZero() {
		t.Fatal("CreatedAt must be set")
	}

	// The stored record must not contain the plaintext token anywhere.
	for _, k := range s.ListKeys("alice") {
		if k.Prefix != token[:12] {
			t.Fatalf("stored prefix = %q", k.Prefix)
		}
	}
}

func TestCreateKeyNameDefaultsAndCap(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.CreateKey("alice", "   ")
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != defaultKeyName {
		t.Fatalf("empty name = %q, want default %q", key.Name, defaultKeyName)
	}
	long := strings.Repeat("n", 100)
	_, key, err = s.CreateKey("alice", long)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(key.Name)) != keyNameMaxLen {
		t.Fatalf("name not capped to %d chars: %d", keyNameMaxLen, len([]rune(key.Name)))
	}
}

func TestCreateKeyRequiresUser(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.CreateKey("ghost", "k"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestLookupKeyRoundtripAndRejections(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("Alice", "long-enough-pw", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants("alice", []Grant{{RepoID: "r1", Role: RoleReader}}); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateKey("alice", "k")
	if err != nil {
		t.Fatal(err)
	}

	// Exact token resolves to the fresh user record (lowercased, grants
	// included, admin flag included).
	u, ok := s.LookupKey(token)
	if !ok {
		t.Fatal("the just-created token must resolve")
	}
	if u.Username != "alice" || !u.Admin || len(u.Grants) != 1 {
		t.Fatalf("LookupKey = %+v", u)
	}

	// Whitespace around the token is tolerated.
	if _, ok := s.LookupKey("  " + token + " "); !ok {
		t.Fatal("trimmed token must resolve")
	}

	// Rejections: unknown token, missing rk_ prefix, empty, a session-shaped
	// random string.
	for _, bad := range []string{"", "rk_nope", "rk_", "no-prefix-at-all", token + "x"} {
		if _, ok := s.LookupKey(bad); ok {
			t.Fatalf("LookupKey(%q) must not resolve", bad)
		}
	}
}

func TestListKeysSorted(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"second", "first", "third"} {
		if _, _, err := s.CreateKey("alice", name); err != nil {
			t.Fatal(err)
		}
	}
	keys := s.ListKeys("ALICE")
	if len(keys) != 3 {
		t.Fatalf("ListKeys = %d, want 3", len(keys))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1].CreatedAt.After(keys[i].CreatedAt) {
			t.Fatalf("keys not creation-sorted: %+v", keys)
		}
	}
	// None of the listed keys can be a valid token.
	for _, k := range keys {
		if _, ok := s.LookupKey(k.Prefix); ok {
			t.Fatal("the display prefix must never resolve as a token")
		}
	}
	// No token material in the structs (prefix is 12 chars by design).
	if got := len(keys[0].Prefix); got != keyPrefixLen {
		t.Fatalf("prefix length = %d, want %d", got, keyPrefixLen)
	}
	if s.ListKeys("nobody") == nil || len(s.ListKeys("nobody")) != 0 {
		t.Fatal("unknown user must yield an empty (non-nil) list")
	}
}

func TestDeleteKeyOwnerScoped(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"alice", "bob"} {
		if _, err := s.CreateUser(n, "long-enough-pw", false); err != nil {
			t.Fatal(err)
		}
	}
	tokenA, keyA, err := s.CreateKey("alice", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey("bob", "b"); err != nil {
		t.Fatal(err)
	}

	// Bob (or anyone else) cannot delete alice's key by ID.
	removed, err := s.DeleteKey("bob", keyA.ID)
	if err != nil || removed {
		t.Fatalf("foreign delete = %v, %v; want false, nil", removed, err)
	}
	if _, ok := s.LookupKey(tokenA); !ok {
		t.Fatal("alice's key must survive a foreign delete attempt")
	}

	// The owner deletes their own key.
	removed, err = s.DeleteKey("ALICE", keyA.ID)
	if err != nil || !removed {
		t.Fatalf("owner delete = %v, %v; want true, nil", removed, err)
	}
	if s.ListKeys("alice") == nil || len(s.ListKeys("alice")) != 0 {
		t.Fatal("key must be gone after the owner deletes it")
	}
	if _, ok := s.LookupKey(tokenA); ok {
		t.Fatal("deleted token must stop resolving")
	}
	if removed, err := s.DeleteKey("alice", keyA.ID); removed || err != nil {
		t.Fatalf("re-delete = %v, %v; want false, nil", removed, err)
	}
}

// TestKeysSurviveReopenAndLookup pins that the api_keys bucket round-trips
// through the file: after reopen the SAME token still resolves.
func TestKeysSurviveReopenAndLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "users.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	token, key, err := s.CreateKey("alice", "reopen")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	u, ok := s2.LookupKey(token)
	if !ok {
		t.Fatal("token must still resolve after reopen")
	}
	if u.Username != "alice" {
		t.Fatalf("owner = %q", u.Username)
	}
	keys := s2.ListKeys("alice")
	if len(keys) != 1 || keys[0].ID != key.ID || keys[0].Prefix != key.Prefix || keys[0].Name != key.Name {
		t.Fatalf("key record changed across reopen: %+v vs %+v", keys[0], key)
	}
}

// TestConcurrentReadsAndMutations is the -race probe: parallel readers
// (Count/GetUser/Role/ListKeys/LookupKey/Users) against a mutating
// goroutine (SetPassword/CreateKey/DeleteKey). Run with -race to pin the
// mirror's lock discipline.
func TestConcurrentReadsAndMutations(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("admin", "long-enough-pw", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("carol", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Count()
				_, _ = s.GetUser("admin")
				_ = s.Role("carol", "r1")
				_ = s.ListKeys("carol")
				_, _ = s.LookupKey("rk_nothing")
				_ = s.Users()
			}
		}()
	}
	// Mutations against the readers.
	for i := 0; i < 5; i++ {
		if err := s.SetPassword("carol", strings.Repeat("p", 8+i)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateKey("carol", "k"); err != nil {
			t.Fatal(err)
		}
		keys := s.ListKeys("carol")
		if len(keys) > 0 {
			if _, err := s.DeleteKey("carol", keys[0].ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetGrants("carol", []Grant{{RepoID: "r1", Role: RoleReader}}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
