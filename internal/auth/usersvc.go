package auth

import (
	"errors"
	"fmt"

	"recogn/internal/users"
)

// ErrAdminProtected is returned by DeleteUser when the target account is an
// admin: admins are the last key to the server and cannot be deleted
// through the API (disable them by rotating the password or revoking
// grants instead).
var ErrAdminProtected = errors.New("admin accounts cannot be deleted")

// Users returns every account (username-sorted, deep copies) for the admin
// user-management routes.
func (s *Service) Users() []users.User { return s.users.Users() }

// CreateUser adds an account (username validated and lowercased, password
// hashed) and, being a change of the user set, re-evaluates the setup key
// invariant: once a user exists the pending setup key is consumed.
func (s *Service) CreateUser(username, password string, admin bool) error {
	_, err := s.users.CreateUser(username, password, admin)
	if err != nil {
		return err
	}
	s.setupMu.Lock()
	s.setupKey = ""
	s.setupMu.Unlock()
	return nil
}

// DeleteUser removes a non-admin account, cascading their API keys (the
// store does it) and their live sessions (here). Admin accounts are
// refused with ErrAdminProtected — the api layer checks Users()[].Admin
// first for its 4xx mapping, this is the defensive backstop. Returns
// whether a user was deleted.
func (s *Service) DeleteUser(username string) (bool, error) {
	u, ok := s.users.GetUser(username)
	if !ok {
		return false, nil
	}
	if u.Admin {
		return false, fmt.Errorf("%w: %q", ErrAdminProtected, u.Username)
	}
	deleted, err := s.users.DeleteUser(username)
	if err != nil || !deleted {
		return deleted, err
	}
	s.sessions.DeleteUserSessions(u.Username)
	// If the last account is gone the server is back in setup mode; a
	// fresh setup key must exist for the next bootstrap.
	s.setupMu.Lock()
	if s.users.Count() == 0 && s.setupKey == "" {
		s.setupKey = generateSetupKey()
	}
	s.setupMu.Unlock()
	return deleted, nil
}

// SetPassword resets a user's password (the admin reset path; self-service
// password changes go through ChangePasswordHandler, which verifies the
// current password first).
func (s *Service) SetPassword(username, password string) error {
	return s.users.SetPassword(username, password)
}

// SetUserGrants replaces a user's grant list. Roles are validated here
// (ErrInvalidRole); the api layer validates that the repo IDs exist before
// calling.
func (s *Service) SetUserGrants(username string, grants []users.Grant) error {
	return s.users.SetGrants(username, grants)
}

// RemoveRepo strips every grant referencing repoID from all users (called
// when a repository is deleted). Only users whose grant list actually
// changes are rewritten; each rewrite is one atomic store transaction, so a
// mid-loop failure leaves every untouched user intact.
func (s *Service) RemoveRepo(repoID string) error {
	for _, u := range s.users.Users() {
		if len(u.Grants) == 0 {
			continue
		}
		kept := u.Grants[:0]
		changed := false
		for _, g := range u.Grants {
			if g.RepoID == repoID {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if !changed {
			continue
		}
		// u.Grants is this loop's private deep copy (Users clones), so the
		// in-place filter is safe.
		if err := s.users.SetGrants(u.Username, kept); err != nil {
			return err
		}
	}
	return nil
}
