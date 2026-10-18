package auth

import (
	"errors"
	"fmt"
	"strings"

	"recogn/internal/users"
)

// ErrAdminProtected is returned by DeleteUser when the target account is
// the LAST remaining admin: admins are the key to the server and the last
// one cannot be deleted through the API (disable them by rotating the
// password or revoking grants instead). When several admins exist, deleting
// one is allowed as long as another remains.
var ErrAdminProtected = errors.New("the last administrator account cannot be deleted")

// ErrSelfProtected is returned by DeleteUser when the caller tries to
// delete their own account (only reachable for admins, the only callers
// the API layer admits).
var ErrSelfProtected = errors.New("you cannot delete your own account")

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

// DeleteUser removes an account, cascading their API keys (the store does
// it) and their live sessions (here). caller is the canonical username of
// the verified admin principal making the call (the api layer's only
// admitted caller). Two refusals: deleting one's OWN account
// (ErrSelfProtected) and deleting the LAST remaining admin
// (ErrAdminProtected) — with several admins, any of them may delete
// another. Returns whether a user was deleted.
//
// The whole operation runs under s.deleteMu: the administrator count and
// the deletion are two separate store transactions, so concurrent deletes
// must not interleave between the check and the delete (two admins deleting
// each other could otherwise both pass the check and remove every admin).
func (s *Service) DeleteUser(caller, username string) (bool, error) {
	s.deleteMu.Lock()
	defer s.deleteMu.Unlock()
	u, ok := s.users.GetUser(username)
	if !ok {
		return false, nil
	}
	if u.Admin {
		if strings.EqualFold(caller, u.Username) {
			return false, ErrSelfProtected
		}
		admins := 0
		for _, cand := range s.users.Users() {
			if cand.Admin {
				admins++
			}
			if admins > 1 {
				break
			}
		}
		if admins <= 1 {
			return false, fmt.Errorf("%w: %q", ErrAdminProtected, u.Username)
		}
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
// current password first). The user's live sessions are revoked — but only
// after the store accepted the new password: a failed reset leaves the old
// credential (and its sessions) valid. Their personal API keys are
// deliberately left intact: a password reset is not a compromise verdict,
// and keys are shown only once at creation, so revoking them here would
// unrecoverably break the user's scripts (the POST /api/users/{u}/password
// response carries a notice asking the caller to review them instead).
func (s *Service) SetPassword(username, password string) error {
	if err := s.users.SetPassword(username, password); err != nil {
		return err
	}
	s.sessions.DeleteUserSessions(username)
	return nil
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
