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
	why := "no container of the issue's pod has started, and no other pod mounts its volume"
	for _, dir := range append([]string{gitDir}, own...) {
		if err := removeLocksUnder(dir, filepath.Join(gitDir, "worktrees"), now, log, why); err != nil {
			return err
		}
	}
	return nil
}

// RemoveStaleFeedLocks removes every git lock file in the bare repository gitDir, a feed: the
// `<file>.lock` a fetch killed mid-update leaves beside a ref, packed-refs, HEAD or the shallow
// file, which blocks every later update of that file. It also removes the `tmp_pack_*` and
// `tmp_idx_*` files a transfer killed mid-pack leaves under objects/pack: git writes a received
// pack under such a name and renames it once whole, so one left there is a pack that never
// arrived, which nothing reads and which a feed a retried fetch finds would otherwise carry,
// and hard-link into the issue's clone. No other object is touched. Each removal is
// logged with the file's path and age, and one it cannot remove is an error naming it. A feed's
// only writer is Fetch, called by one process at a time: a pod's workspace-fetch, its first init
// container and the one container that mounts the feed writable, or the repository cache's process
// under its write lock, so no lock or transfer there is live when Fetch starts.
func RemoveStaleFeedLocks(gitDir string, now time.Time, log func(string)) error {
	why := "a feed's only writer is the fetch now starting"
	if err := removeLocksUnder(gitDir, "", now, log, why); err != nil {
		return err
	}
	// The partial packs are looked for only where git writes them, and only when neither objects
	// nor objects/pack is a link: a glob follows one, and would remove files outside the feed.
	// A linked or missing directory is left for foreignFeed, which refuses the feed after this.
	pack := filepath.Join(gitDir, "objects", "pack")
	for _, dir := range []string{filepath.Dir(pack), pack} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return nil
		}
	}
	partial, err := filepath.Glob(filepath.Join(pack, "tmp_*"))
	if err != nil {
		return err
	}
	for _, path := range partial {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("read the partial pack %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the partial pack %s: %w", path, err)
		}
		log(fmt.Sprintf("removed the partial pack %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
	}
	return nil
}

// removeLocksUnder removes every `*.lock` file under dir but those under skip, logging each with
// its age and why no process can hold it.
func removeLocksUnder(dir, skip string, now time.Time, log func(string), why string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist) && path == dir:
			return nil
		case err != nil:
			return fmt.Errorf("look for stale git locks in %s: %w", dir, err)
		case entry.IsDir() && skip != "" && path == skip:
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
		log(fmt.Sprintf("removed the stale git lock %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
		return nil
	})
}
