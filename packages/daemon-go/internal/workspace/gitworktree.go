package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// gitWorktreeLockReason is the reason each workspace's git worktree is locked with: `git worktree
// prune` skips a locked entry, and whoever runs one on the shared clone may not see this
// workspace's directory (it is on another tree's volume, or another host's mount of the clone).
const gitWorktreeLockReason = "legion workspace: its directory may be invisible to other processes sharing this clone"

// gitWorktreeEntries is the admin directories of the shared clone's git worktrees registered at
// dir: each <clone>/.git/worktrees/<id> whose gitdir file names dir's .git, dir as given or with its
// symlinks resolved, as jj records it. git names an entry after the directory's base name, with a
// number on a collision, so the id is read, never derived. A clone with no linked worktree, or a
// jj that colocates no workspace (0.44), has none.
func gitWorktreeEntries(cloneDir, dir string) ([]string, error) {
	admin := filepath.Join(cloneDir, ".git", "worktrees")
	entries, err := os.ReadDir(admin)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read git worktrees: %w", err)
	}
	resolved, err := resolvedPath(dir)
	if err != nil {
		return nil, err
	}
	want := []string{filepath.Join(filepath.Clean(dir), ".git"), filepath.Join(resolved, ".git")}
	var own []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entryDir := filepath.Join(admin, entry.Name())
		gitdir, err := os.ReadFile(filepath.Join(entryDir, "gitdir"))
		if errors.Is(err, fs.ErrNotExist) {
			// No worktree git could name: nobody's to claim.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read git worktree %s: %w", entryDir, err)
		}
		target := strings.TrimSuffix(string(gitdir), "\n")
		if !filepath.IsAbs(target) {
			target = filepath.Join(entryDir, target)
		}
		if slices.Contains(want, filepath.Clean(target)) {
			own = append(own, entryDir)
		}
	}
	return own, nil
}

// resolvedPath is path, made absolute, with the symlinks of its longest existing prefix resolved.
func resolvedPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		missing = filepath.Join(filepath.Base(path), missing)
		path = filepath.Dir(path)
	}
}

// removeGitWorktree deletes the shared clone's git worktree entry for dir, whose directory is gone:
// what `git worktree prune` does to that one entry, locked or not, and nothing to any other.
func removeGitWorktree(cloneDir, dir string) error {
	own, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	for _, entry := range own {
		if err := os.RemoveAll(entry); err != nil {
			return fmt.Errorf("remove git worktree %s: %w", entry, err)
		}
	}
	return nil
}

// lockGitWorktree locks the shared clone's git worktree entry for dir, as `git worktree lock` does,
// so a bare `git worktree prune` from a process that cannot see dir skips it. An entry already
// locked keeps its lock and reason.
func lockGitWorktree(cloneDir, dir string) error {
	own, err := gitWorktreeEntries(cloneDir, dir)
	if err != nil {
		return err
	}
	for _, entry := range own {
		lock, err := os.OpenFile(filepath.Join(entry, "locked"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lock git worktree %s: %w", entry, err)
		}
		_, err = lock.WriteString(gitWorktreeLockReason)
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("lock git worktree %s: %w", entry, err)
		}
	}
	return nil
}
