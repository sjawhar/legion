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

// rootCommitID is jj's sentinel commit id for the root commit: every workspace's history ends
// there, and it has no git tree because it isn't a real git commit (`git cat-file` reports no such
// object). A workspace whose working copy has no real parent yet -- its `@-` is the root -- prints
// this as restoreGitWorktree's head, and `git read-tree` on it fails ("failed to unpack tree object
// HEAD"); jj's own colocated `workspace add` writes HEAD as the unborn ref below and an empty
// index for exactly this case, which restoreGitWorktree matches instead of failing to restore a
// workspace that has never had a git-visible commit.
const rootCommitID = "0000000000000000000000000000000000000000"

// rootHeadRef is the unborn ref jj's own colocated `workspace add` writes to HEAD when a
// workspace's working copy has no real parent yet (verified against jj 0.45.1-sami).
const rootHeadRef = "ref: refs/jj/root"

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
// missing entry. It writes what `git worktree add` would -- gitdir, commondir, and HEAD at the
// working-copy commit's first parent, as jj keeps it, then the index from HEAD with `git read-tree`,
// which writes no working-tree file -- into a temporary sibling directory, and renames it onto
// target only once every step succeeds, so target is either the complete entry or still absent even
// across a kill (a SIGKILL, an OOM, a pod eviction, a daemon restart) that runs no Go defer. A
// workspace with no .git (jj 0.44), or whose entry exists, is left alone; a pointer outside the
// clone's git worktrees is refused, since a tree agent can write the workspace's .git.
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
	// target is resolved through every symlink on its path (resolvedPath, above), so a symlinked
	// worktrees component inside it is already followed there. worktrees, in contrast, is resolved
	// only as far as .git: "worktrees" is joined on as a literal, unresolved path segment. A tree
	// agent that can write the shared clone's .git can replace .git/worktrees with a symlink to any
	// directory, and resolving that symlink here too would make both sides of this comparison agree
	// wherever it points -- git never creates .git/worktrees as a symlink, so resolving one is never
	// a legitimate case, only ever that replacement.
	gitDir, err := filepath.EvalSymlinks(filepath.Join(workspace.Clone, ".git"))
	if err != nil {
		return fmt.Errorf("resolve the shared clone's git directory: %w", err)
	}
	worktrees := filepath.Join(gitDir, "worktrees")
	if filepath.Dir(target) != worktrees {
		return fmt.Errorf("workspace %s's .git, which a tree agent can write, names %s outside the shared clone's %s; provisioning refuses to create or write it. Remove the workspace so the next provisioning adds it again", workspace.Dir, target, worktrees)
	}
	parents, err := RunChecked(ctx, run, []string{"jj", "log", "-r", "@", "--no-graph", "--ignore-working-copy", "-T", `parents.map(|c| c.commit_id()).join("\n")`}, nil, workspace.Dir)
	if err != nil {
		return err
	}
	head, _, _ := strings.Cut(strings.TrimSpace(parents.Stdout), "\n")
	if !commitID.MatchString(head) {
		return fmt.Errorf("workspace %s: jj printed no parent commit for its working copy: %q", workspace.Dir, parents.Stdout)
	}
	// A random suffix under a leading dot: git assigns a worktree id from its directory's own base
	// name, and never assigns one beginning with a dot (verified: a directory named ".x" gets the id
	// "-x"), so no real worktree can ever collide with this temporary one. The random suffix also
	// keeps two concurrent restores of the same workspace from writing into, and renaming, the same
	// temporary directory.
	tmp, err := os.MkdirTemp(worktrees, "."+filepath.Base(target)+".restore-")
	if err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	headContent, readTreeArg, where := head, "HEAD", "at "+head
	if head == rootCommitID {
		headContent, readTreeArg, where = rootHeadRef, "--empty", "fresh, with no real commit yet"
	}
	readTree := []string{"git", "--git-dir=" + tmp, "--work-tree=" + dir, "read-tree", readTreeArg}
	write := func(name, content string) error {
		return os.WriteFile(filepath.Join(tmp, name), []byte(content+"\n"), 0o644)
	}
	if err := write("gitdir", filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if err := write("commondir", filepath.Join("..", "..")); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if err := write("HEAD", headContent); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	if _, err := RunChecked(ctx, run, readTree, nil, workspace.Clone); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("restore git worktree %s: %w", target, err)
	}
	log(fmt.Sprintf("Workspace %s had lost its git worktree entry %s (a git worktree prune that could not see the workspace deletes it): restored it %s, the working copy untouched", workspace.Dir, target, where))
	return nil
}
