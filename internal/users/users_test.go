package users

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a Store on a fresh temp path and registers cleanup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("users.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesBucketsAndVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "users.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if s.Count() != 0 {
		t.Fatalf("fresh store: Count = %d, want 0", s.Count())
	}
}

func TestCreateAndGetUser(t *testing.T) {
	s := newTestStore(t)

	if s.Count() != 0 {
		t.Fatalf("Count = %d, want 0", s.Count())
	}
	u, err := s.CreateUser("Alice", "long-enough-pw", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Username != "alice" {
		t.Fatalf("stored username = %q, want lowercased", u.Username)
	}
	if !u.Admin {
		t.Fatal("admin flag not stored")
	}
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
		t.Fatalf("password hash is not PHC argon2id: %q", u.PasswordHash)
	}
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		t.Fatal("timestamps must be set")
	}
	if s.Count() != 1 {
		t.Fatalf("Count = %d, want 1", s.Count())
	}

	// Case-insensitive lookup: any case finds the lowercased record.
	for _, variant := range []string{"alice", "Alice", "ALICE", " alice "} {
		got, ok := s.GetUser(variant)
		if !ok || got.Username != "alice" {
			t.Fatalf("GetUser(%q) = %v, %v; want the stored user", variant, got, ok)
		}
	}
	if _, ok := s.GetUser("nobody"); ok {
		t.Fatal("unknown user must not be found")
	}

	// The record is a deep copy: mutating the handed-out Grants slice must
	// not touch the stored one.
	got, _ := s.GetUser("alice")
	got.Grants = append(got.Grants, Grant{RepoID: "r1", Role: RoleWriter})
	if g := s.Users()[0]; len(g.Grants) != 0 {
		t.Fatalf("mutating a returned record leaked into the store: %+v", g.Grants)
	}
}

func TestCreateUserValidation(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name     string
		username string
		password string
		wantErr  string
	}{
		{"empty username", "", "long-enough-pw", "username is empty"},
		{"single char", "a", "long-enough-pw", "at least 2"},
		{"leading dot", ".alice", "long-enough-pw", "must start with a letter or digit"},
		{"leading underscore", "_alice", "long-enough-pw", "must start with a letter or digit"},
		{"leading dash", "-alice", "long-enough-pw", "must start with a letter or digit"},
		{"bad character", "al!ce", "long-enough-pw", "must start with a letter or digit"},
		{"too long", "a123456789012345678901234567890123", "long-enough-pw", "at most 32"},
		{"empty password", "alice", "", "at least 8 characters"},
		{"short password", "alice", "short", "at least 8 characters"},
	}
	for _, tc := range cases {
		if _, err := s.CreateUser(tc.username, tc.password, false); err == nil {
			t.Errorf("%s: CreateUser(%q, %q) succeeded, want error %q", tc.name, tc.username, tc.password, tc.wantErr)
		} else if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}

	// Whitespace-only usernames are empty after the trim.
	if err := ValidUsername("   "); err == nil {
		t.Error("whitespace-only username must be rejected")
	}

	// The accepted shapes (case is allowed on input; storage lowercases).
	for _, ok := range []string{"ab", "a.b", "a_b-c", "A9", "xXyY", "0abc", "nine12345678901234567890123456789012"[:32]} {
		if err := ValidUsername(ok); err != nil {
			t.Errorf("ValidUsername(%q) = %v, want nil", ok, err)
		}
	}
}

func TestUsernameTakenCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"alice", "Alice", "ALICE"} {
		if _, err := s.CreateUser(variant, "long-enough-pw", false); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("CreateUser(%q): err = %v, want ErrUsernameTaken", variant, err)
		}
	}
	if s.Count() != 1 {
		t.Fatalf("Count = %d, want 1 (failed creates must not add rows)", s.Count())
	}
}

func TestUsersSortedByName(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"carol", "alice", "bob"} {
		if _, err := s.CreateUser(n, "long-enough-pw", false); err != nil {
			t.Fatal(err)
		}
	}
	all := s.Users()
	if len(all) != 3 {
		t.Fatalf("Users = %d records, want 3", len(all))
	}
	if all[0].Username != "alice" || all[1].Username != "bob" || all[2].Username != "carol" {
		t.Fatalf("not username-sorted: %v", all)
	}
}

func TestSetPassword(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "first-password", false); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetUser("alice")

	if err := s.SetPassword("ALICE", "second-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	after, _ := s.GetUser("alice")
	if after.PasswordHash == before.PasswordHash {
		t.Fatal("hash must change")
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("UpdatedAt must bump: %v → %v", before.UpdatedAt, after.UpdatedAt)
	}
	if !VerifyPassword(after.PasswordHash, "second-password") {
		t.Fatal("new password must verify")
	}
	if VerifyPassword(after.PasswordHash, "first-password") {
		t.Fatal("old password must stop verifying")
	}

	if err := s.SetPassword("nobody", "long-enough-pw"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user: err = %v, want ErrUserNotFound", err)
	}
	if err := s.SetPassword("alice", "short"); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("short password: err = %v, want length error", err)
	}
	after, _ = s.GetUser("alice")
	if !VerifyPassword(after.PasswordHash, "second-password") {
		t.Fatal("rejected SetPassword must not change the stored hash")
	}
}

func TestSetGrants(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}

	grants := []Grant{{RepoID: "r1", Role: RoleReader}, {RepoID: "r2", Role: RoleWriter}}
	if err := s.SetGrants("Alice", grants); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}
	u, _ := s.GetUser("alice")
	if len(u.Grants) != 2 {
		t.Fatalf("grants = %+v", u.Grants)
	}

	// The stored slice is a clone: mutating the caller's backing array must
	// not change the store.
	grants[0] = Grant{RepoID: "hacked", Role: RoleWriter}
	u, _ = s.GetUser("alice")
	if u.Grants[0].RepoID != "r1" {
		t.Fatalf("grant slice aliases the caller's array: %+v", u.Grants)
	}

	// Replacing removes the old set entirely.
	if err := s.SetGrants("alice", []Grant{{RepoID: "r9", Role: RoleReader}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Role("alice", "r1"); got != "" {
		t.Fatalf("old grant survived replacement: %q", got)
	}
	if got := s.Role("alice", "r9"); got != RoleReader {
		t.Fatalf("new grant missing: %q", got)
	}

	// An empty list clears all grants.
	if err := s.SetGrants("alice", nil); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.GetUser("alice"); len(u.Grants) != 0 {
		t.Fatalf("grants not cleared: %+v", u.Grants)
	}

	// Validation: unknown roles are rejected wholesale.
	for _, bad := range []string{"", "admin", "Reader", "WRITE", "owner"} {
		err := s.SetGrants("alice", []Grant{{RepoID: "r1", Role: bad}})
		if !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("role %q: err = %v, want ErrInvalidRole", bad, err)
		}
	}
	if got := s.Role("alice", "r1"); got != "" {
		t.Fatal("a rejected SetGrants must leave the user untouched")
	}
	if err := s.SetGrants("nobody", nil); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user: err = %v, want ErrUserNotFound", err)
	}
}

func TestRoleLiteral(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("boss", "long-enough-pw", true); err != nil { // admin, no grants
		t.Fatal(err)
	}
	if _, err := s.CreateUser("dev", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants("dev", []Grant{{RepoID: "r1", Role: RoleReader}, {RepoID: "r2", Role: RoleWriter}}); err != nil {
		t.Fatal(err)
	}

	// Literal even for the admin: the store does not special-case Admin.
	if got := s.Role("boss", "r1"); got != "" {
		t.Fatalf("admin Role = %q, want \"\" (literal; the auth layer maps admins to writer)", got)
	}
	if got := s.Role("dev", "r1"); got != RoleReader {
		t.Fatalf("Role = %q, want reader", got)
	}
	if got := s.Role("dev", "r2"); got != RoleWriter {
		t.Fatalf("Role = %q, want writer", got)
	}
	if got := s.Role("dev", "r3"); got != "" {
		t.Fatalf("ungranted repo: Role = %q, want \"\"", got)
	}
	if got := s.Role("ghost", "r1"); got != "" {
		t.Fatalf("unknown user: Role = %q, want \"\"", got)
	}
}

// TestDeleteUserCascade pins that deleting a user removes their record AND
// every API key they own, in one consistent step.
func TestDeleteUserCascade(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUser("alice", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("bob", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey("alice", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey("alice", "a2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey("bob", "b1"); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteUser("ALICE")
	if err != nil || !deleted {
		t.Fatalf("DeleteUser = %v, %v; want true, nil", deleted, err)
	}
	if _, ok := s.GetUser("alice"); ok {
		t.Fatal("user must be gone")
	}
	if keys := s.ListKeys("alice"); len(keys) != 0 {
		t.Fatalf("cascade left %d keys behind", len(keys))
	}
	// Bob and his key are untouched.
	if _, ok := s.GetUser("bob"); !ok {
		t.Fatal("other users must survive the cascade")
	}
	if keys := s.ListKeys("bob"); len(keys) != 1 {
		t.Fatalf("bob's keys: got %d, want 1", len(keys))
	}

	// Deleting again is a no-op (false, nil), not an error.
	if deleted, err := s.DeleteUser("alice"); deleted || err != nil {
		t.Fatalf("second DeleteUser = %v, %v; want false, nil", deleted, err)
	}
}

// TestReopenMirrorMatchesDisk is the mirror-vs-disk consistency probe: after
// closing and reopening the store, the mirror rebuilt from the file must
// equal what the live store reported (users, grants, keys, timestamps).
func TestReopenMirrorMatchesDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "users.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("Alice", "long-enough-pw", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants("alice", []Grant{{RepoID: "r1", Role: RoleWriter}, {RepoID: "r2", Role: RoleReader}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("alice", "rotated-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey("alice", "cli key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("Bob", "long-enough-pw", false); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.DeleteUser("bob"); err != nil || !deleted { // must NOT reappear after reopen
		t.Fatal(err)
	}
	want, _ := s.GetUser("alice")
	beforeCount := s.Count()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if s2.Count() != beforeCount {
		t.Fatalf("Count after reopen = %d, want %d", s2.Count(), beforeCount)
	}
	got, ok := s2.GetUser("alice")
	if !ok {
		t.Fatal("user lost across reopen")
	}
	if got.Username != want.Username || got.Admin != want.Admin || got.PasswordHash != want.PasswordHash {
		t.Fatalf("user record changed across reopen:\n got %+v\nwant %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("timestamps changed across reopen:\n got %v/%v\nwant %v/%v", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	if len(got.Grants) != 2 || got.Grants[0].RepoID != "r1" || got.Grants[0].Role != RoleWriter || got.Grants[1].RepoID != "r2" || got.Grants[1].Role != RoleReader {
		t.Fatalf("grants changed across reopen: %+v", got.Grants)
	}
	if !VerifyPassword(got.PasswordHash, "rotated-password") {
		t.Fatal("password hash lost across reopen")
	}
	if keys := s2.ListKeys("alice"); len(keys) != 1 || keys[0].Name != "cli key" || keys[0].Username != "alice" {
		t.Fatalf("keys changed across reopen: %+v", keys)
	}
	if _, ok := s2.GetUser("bob"); ok {
		t.Fatal("deleted user reappeared after reopen")
	}
}

// TestDoubleOpenRefused pins the bbolt exclusive-lock behaviour the tests
// themselves rely on: a second Open on the same path while one is held must
// fail (with the 3s timeout) instead of silently sharing the file.
func TestDoubleOpenRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("second Open on a locked file must fail")
	}
}

func TestUserTimestampsAreUTC(t *testing.T) {
	s := newTestStore(t)
	u, err := s.CreateUser("alice", "long-enough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	if loc := u.CreatedAt.Location(); loc != time.UTC {
		t.Fatalf("CreatedAt location = %v, want UTC", loc)
	}
}
