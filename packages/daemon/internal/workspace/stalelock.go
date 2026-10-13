package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoveStaleGitLocks removes every git lock file in workspace's clone that git or jj could take:
// the `<file>.lock` git's lockfile API and jj's git backend create beside a file they are about to
// replace and rename over it once it is written. A process killed or interrupted mid-write leaves
// its lock, and every later write of that file fails on it until someone deletes it. It walks the
// clone's git directory, apart from its worktree entries, and the clone's worktree entries for
// workspace (gitWorktreeEntries, which refuses a symlinked .git or .git/worktrees before anything
// is removed); another workspace's entry keeps its locks. Each removal is logged with the lock's
// path and age, and a lock it cannot remove is an error naming it.
//
// A lock some process still holds must never be removed: two writers would then replace one file.
// So the caller is one that knows no process can hold any: a Kubernetes issue pod's
// `workspace-init provision`, which runs before any role container of the pod starts, on the
// issue's own volume, which no other pod mounts. The tmux runtime's provisioning never calls it,
// since every pane of the repository shares the clone there. jj's own locks need nothing: they are
// flock(2) locks the kernel releases with their holder, so a lock file one leaves blocks no jj.
func RemoveStaleGitLocks(workspace Workspace, now time.Time, log func(string)) error {
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return err
	}
	own, err := gitWorktreeEntries(workspace.Clone, workspace.Dir)
	if err != nil {
		return err
	}
	gitDir := filepath.Join(workspace.Clone, ".git")
	worktrees := filepath.Join(gitDir, "worktrees")
	for _, dir := range append([]string{gitDir}, own...) {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			switch {
			case errors.Is(err, fs.ErrNotExist) && path == dir:
				return nil
			case err != nil:
				return fmt.Errorf("look for stale git locks in %s: %w", dir, err)
			case entry.IsDir() && path == worktrees:
				return fs.SkipDir
			case entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock"):
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("read the stale git lock %s: %w", path, err)
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove the stale git lock %s: %w", path, err)
			}
			log(fmt.Sprintf("removed the stale git lock %s, last written %s ago: no container of the issue's pod has started, and no other pod mounts its volume", path, now.Sub(info.ModTime()).Round(time.Second)))
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
