// Per-user state for the dispatch dashboard.
//
// One file per user under ~/.local/share/dispatch/users/<login>.json holds
// their refreshable OAuth tokens and their list of watched <owner>/<repo>
// pairs. Keying by GitHub login (which is unique and stable) avoids the
// concurrent-write races of a single shared store, and makes "log out user X"
// a trivial `os.Remove`.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// User is the on-disk record for one authenticated dashboard user.
type User struct {
	Login        string   `json:"login"`
	Tokens       Tokens   `json:"tokens"`
	WatchedRepos []string `json:"watchedRepos"`
	// Addressed maps "<owner>/<repo>#<number>" → the thread.updatedAt at the
	// moment the user marked it addressed. When the thread's updatedAt
	// advances past this timestamp (i.e. new activity), the sidebar resurfaces
	// the row automatically. Unmarking removes the key.
	Addressed map[string]string `json:"addressed,omitempty"`
}

// DefaultUsersDir returns ~/.local/share/dispatch/users.
func DefaultUsersDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "dispatch", "users"), nil
}

// loginShape restricts what we'll accept as a path-component-safe form of a
// GitHub login. GitHub logins are already a strict subset (alphanumerics +
// hyphens, no leading/trailing hyphen), but we re-validate to keep `..` and
// other path traversals impossible.
var loginShape = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)

// userPath returns the full path for a given login. Returns an error rather
// than silently sanitizing — a login that doesn't match GitHub's published
// shape is a bug we want to surface, not paper over.
func userPath(dir, login string) (string, error) {
	if !loginShape.MatchString(login) {
		return "", fmt.Errorf("invalid login %q", login)
	}
	return filepath.Join(dir, login+".json"), nil
}

// ReadUser returns (nil, nil) when no file exists for this login — caller
// treats that as "user has never logged in" or "user logged out".
func ReadUser(dir, login string) (*User, error) {
	path, err := userPath(dir, login)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var u User
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if u.Login == "" {
		u.Login = login
	}
	// Normalize watched list: lowercase, deduplicate, drop empties.
	u.WatchedRepos = normalizeRepoList(u.WatchedRepos)
	return &u, nil
}

// WriteUser persists with mode 0600. Caller is responsible for ensuring the
// User's Tokens field has been freshly refreshed if needed.
func WriteUser(dir string, u *User) error {
	if u == nil {
		return errors.New("WriteUser: nil user")
	}
	path, err := userPath(dir, u.Login)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Normalize before write so the on-disk shape is canonical.
	u.WatchedRepos = normalizeRepoList(u.WatchedRepos)
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// RemoveUser deletes the file for a given login. Missing file is not an
// error.
func RemoveUser(dir, login string) error {
	path, err := userPath(dir, login)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// repoShape enforces the GitHub-style "<owner>/<repo>" form. Owner and repo
// names follow the same character set; we don't enforce length here because
// GitHub's own limits change over time.
var repoShape = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ValidateRepoSlug returns nil iff s is a well-formed <owner>/<repo>. Used by
// the views endpoint to reject malformed input before persistence.
func ValidateRepoSlug(s string) error {
	if !repoShape.MatchString(s) {
		return fmt.Errorf("invalid repo slug %q (expected <owner>/<repo>)", s)
	}
	return nil
}

func normalizeRepoList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
