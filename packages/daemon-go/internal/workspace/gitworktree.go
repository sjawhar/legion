package workspace

import (
	"context"
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
// workspace's directory (an isolated session on the same host that mounts only its own checkout),
// or may run one without asking (stock jj 0.45.1's `jj workspace forget` prunes the whole clone).
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
	// git writes a relative gitdir (jj asks for relative worktree paths; git 2.48 and later honour
	// it) between real paths, so it is resolved from the entry's real directory.
	if admin, err = filepath.EvalSymlinks(admin); err != nil {
		return nil, fmt.Errorf("resolve git worktrees: %w", err)
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

// restoreGitWorktree re-creates the shared clone's git worktree entry for the workspace at dir when
// the entry its .git names is gone, as a bare `git worktree prune` from a process that could not see
// dir leaves it: git fails there while jj keeps working, and `git worktree repair` cannot rebuild a
// missing entry. It writes what `git worktree add` would, at the path the pointer names: gitdir,
// commondir, and HEAD at the working-copy commit's first parent, as jj keeps it; then the index from
// HEAD with `git read-tree`, which writes no working-tree file. A workspace with no .git (jj 0.44),
// or whose entry exists, is left alone; a pointer outside the clone's git worktrees is refused.
func restoreGitWorktree(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	pointer, err := os.ReadFile(filepath.Join(workspace.Dir, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the workspace's git pointer: %w", err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(pointer)), "gitdir: ")
	if !ok {
		return fmt.Errorf("%s/.git names no git worktree: %q", workspace.Dir, pointer)
	}
	dir, err := filepath.EvalSymlinks(workspace.Dir)
	if err != nil {
		return fmt.Errorf("resolve the workspace: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if target, err = resolvedPath(target); err != nil {
		return err
	}
	present, err := pathExists(target)
	if err != nil || present {
		return err
	}
	worktrees, err := resolvedPath(filepath.Join(workspace.Clone, ".git", "worktrees"))
	if err != nil {
		return fmt.Errorf("resolve the shared clone's git worktrees: %w", err)
	}
	if name := filepath.Base(target); filepath.Dir(target) != worktrees || name == "." || name == ".." {
		return fmt.Errorf("workspace %s names git worktree %s, outside the shared clone's %s; its git side was not restored", workspace.Dir, target, worktrees)
	}
	parents, err := RunChecked(ctx, run, []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `parents.map(|c| c.commit_id()).join("\n")`}, nil, workspace.Dir)
	if err != nil {
		return err
	}
	head, _, _ := strings.Cut(strings.TrimSpace(parents.Stdout), "\n")
	if !commitID.MatchString(head) {
		return fmt.Errorf("workspace %s: jj printed no parent commit for its working copy: %q", workspace.Dir, parents.Stdout)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	for name, content := range map[string]string{
		"gitdir":    filepath.Join(dir, ".git"),
		"commondir": filepath.Join("..", ".."),
		"HEAD":      head,
	} {
		if err := os.WriteFile(filepath.Join(target, name), []byte(content+"\n"), 0o644); err != nil {
			return fmt.Errorf("restore git worktree %s: %w", target, err)
		}
	}
	if _, err := RunChecked(ctx, run, []string{"git", "read-tree", "HEAD"}, nil, workspace.Dir); err != nil {
		return err
	}
	log(fmt.Sprintf("Workspace %s had lost its git worktree entry %s (a git worktree prune that could not see the workspace deletes it): restored it at %s, the working copy untouched", workspace.Dir, target, head))
	return nil
}
