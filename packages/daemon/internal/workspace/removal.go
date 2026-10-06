package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// unpushedRevset is RemoveFinished's push-safety check: every non-empty ancestor of the
// workspace's own commit that is reachable from neither a remote bookmark nor, when the daemon
// recorded one, an ancestor of mergedHead. empty() is excluded because jj always keeps an empty
// commit at the tip of a workspace's working copy; without the exclusion every workspace would
// show that placeholder as unreachable from anything and never be judged safe.
//
// remote_bookmarks() is the shared clone's own ref state, writable by any role's pane on the tree
// volume (the same legacy `.jj/workspace-config.toml` channel snapshotOverrides guards the
// snapshot against, applied to a bookmark instead of a program): this check is what stops an
// ordinary slip — a candidate's workspace genuinely holding a commit nothing pushed — from being
// deleted, not a defense against a hostile role, which already has filesystem access to every
// sibling workspace on the volume and could delete one directly.
//
// This jj log carries none of snapshotOverrides' flags, unlike the snapshot: a repo-configured
// `revset-aliases."empty()" = "all()"` or `"remote_bookmarks()" = "all()"` planted in the shared
// clone's own legacy config, read here the same way, makes this check pass falsely over a
// workspace that in fact holds an unpushed edit. This sits inside the trust model stated just
// above (any tree agent can already delete a sibling's workspace directly) and is tracked as
// hardening on dispatch://LEGION-583, not fixed here.
func unpushedRevset(workspaceName, mergedHead string) string {
	revset := "::" + workspaceName + "@ ~ empty() ~ ::(remote_bookmarks())"
	if mergedHead != "" {
		revset += " ~ ::" + mergedHead
	}
	return revset
}

// snapshotOverrides are the command-line `--config` flags RemoveFinished's one real snapshot adds
// (every other command in this package passes --ignore-working-copy instead and never needs
// these). They outrank whatever a tree agent wrote into the one legacy `.jj/workspace-config.toml`
// file a workspace's own directory (or the shared clone's) can carry on the tree volume: jj
// migrates such a file into whatever fresh config home it finds — a pod's own, which starts empty
// every launch — so a worker that writes one into a workspace's `.jj` before it is ever parked
// reaches every later snapshot of that same workspace. Each flag below is confirmed against the
// pinned jj (0.45.1-sami):
//
//   - `git.filter.enabled=false` turns off the working-copy filter `onClone`'s own doc comment
//     already names: left on, a `git.filter.drivers.<name>.clean`/`.smudge` program runs over
//     every tracked file its `.gitattributes` names.
//   - `signing.backend=none` turns off every signing backend: left configured (`signing.backend`,
//     `signing.behavior`, a key, and a `signing.backends.<backend>.program`), the program runs to
//     (re-)sign the commit the snapshot creates.
//   - `fsmonitor.backend=none` needs no vulnerability to justify it: this snapshot needs no file
//     watching, so it is forced off rather than ever touch a watchman daemon on the tree volume.
//     fsmonitor's one non-`"none"` backend, `"watchman"`, is a fixed binary name jj resolves on
//     the process's own `PATH` in any case, never a path or command a repo config can choose.
//   - `snapshot.auto-track=all()` restores jj's own default (every new path is tracked): a
//     configured `snapshot.auto-track=none()` would otherwise leave every new path untracked,
//     which would starve RemoveFinished's push-safety check of anything to read at all and keep
//     every candidate forever, the opposite failure from a program running. The one thing that
//     still leaves a path untracked under `all()` is jj's own anti-footgun limit on a new file's
//     size (snapshot.max-new-file-size, 1MiB by default): deliberately left at its default rather
//     than raised or disabled, since snapshotting a worker's own oversized file would write its
//     content into the shared clone's object store this pass exists to shrink. untrackedPaths
//     below is what catches that one case: a size-refused path, read off jj's own "Untracked
//     paths:" section on stdout, keeps the workspace rather than let an unsnapshotted file read as
//     a clean `@`.
//
// `--config` outranks every config layer below the command line (built-in defaults, user, repo,
// workspace) regardless of where in the argv it sits; the flags are grouped here for readability,
// not because their position matters.
func snapshotOverrides() []string {
	return []string{
		"--config", "git.filter.enabled=false",
		"--config", "signing.backend=none",
		"--config", "fsmonitor.backend=none",
		"--config", "snapshot.auto-track=all()",
	}
}

// RemoveFinished removes ws when every non-empty commit it holds is on GitHub by unpushedRevset's
// rule, and otherwise keeps it, naming in one log line the commits that are not — never deleting
// unpushed work. A workspace already gone (removed already, or never provisioned on this volume)
// is left alone without error, unless a prior pass's Remove renamed it aside and was killed
// before finishing the delete (removingSuffix, bookmark.go): that renamed-aside directory, found
// where ws.Dir no longer is, is finished rather than re-judged — it was already found safe to
// remove, and a half-deleted tree is unsafe to snapshot. The daemon's candidate list is computed
// from issue lifecycle alone and may still name a workspace nothing on this volume ever created.
// log receives exactly one line: what was removed, what was kept and why, or that there was
// nothing to do. ws is Location's, which names the shared clone; issue is logged as the daemon
// knows it (ws.Dir's own base name is lowercased for jj's workspace name, per Location).
//
// Before judging anything, the check snapshots ws.Dir's own working copy for real (a plain `jj
// status` there, with snapshotOverrides below, which explains what each override neutralizes and
// why): a child parked between phases can hold an edit its agent never ran a jj command over
// since, and without this snapshot that edit is invisible to unpushedRevset's `::workspaceName@`
// and the workspace would be judged clean and removed out from under it. The one jj command this
// package runs against the shared clone proper without --ignore-working-copy is `jj workspace
// add` (createWorkspace); this is the other, and the only one that runs a real snapshot over a
// workspace a tree agent writes to directly.
//
// The snapshot cannot see everything on disk, though, and jj says nothing useful when it misses:
// a directory holding its own .git or .jj — any filesystem entry of that name, a real nested
// repository's directory, a plain file, or even a dangling symlink, jj's own skip condition does
// not distinguish between them (a second repository an agent cloned to patch a dependency or a
// fork, inside the workspace, is the real case; a bare file or symlink of that name is the same
// hole by construction) — is skipped outright: not tracked, not listed as untracked, no warning
// anywhere. A file whose name is not valid UTF-8 gets only a warning jj writes to stderr, with
// stdout claiming no changes at all. Both leave unpushed work this workspace alone holds invisible
// to unpushedRevset's read of `@`. So two checks run before it, on the snapshot's own result:
// anything at all on stderr keeps the workspace (this also makes the untracked-path case below
// fail closed if a future jj ever changes its stdout wording rather than its stderr one), and so
// does any nested repository (nestedRepositories below, which treats the workspace's own `.git`
// and `.jj` — every workspace has both — as not nested, and nothing else as exempt by type; it
// is also kept, not treated as a failure, when that walk runs out of time against deadline, the
// removal pass's own budget — never the ambient ctx, which on the production path
// (removeFinishedWorkspaces, its own caller) carries no deadline at all; wrapping only the walk,
// not ctx itself, is what keeps an in-flight jj command unaffected: removalBudget's own doc
// comment promises the budget never interrupts one already running).
//
// Past both, the snapshot can still leave an ordinary path untracked rather than commit it (an
// oversized new file, under snapshot.max-new-file-size, which snapshotOverrides deliberately
// leaves at its default): jj's own contract for "something was left out" there is the "Untracked
// paths:" header `jj status` prints on stdout before every such path, one per line as `? <path>`.
// Reading that header is what untrackedPaths below does; its presence means @ does not hold
// everything on disk, so unpushedRevset's read of @ cannot be trusted to prove the workspace
// clean, and the workspace is kept rather than risk removing one that in fact still holds unpushed
// work on disk. Every removed workspace's gitignored content (including `.git/info/exclude`, which
// provisioning writes `.codegraph/` into) is deleted with it: jj never snapshots it, so none of
// the checks above ever see it, but it is also never pushed by definition, so there is nothing of
// it for any of them to protect.
func RemoveFinished(ctx context.Context, run Runner, ws Workspace, issue, mergedHead string, deadline time.Time, log func(string)) error {
	if !located(ws) {
		return fmt.Errorf("workspace to remove (%#v) is not a workspace Location names", ws)
	}
	present, err := pathExists(ws.Dir)
	if err != nil {
		return err
	}
	if !present {
		aside, err := pathExists(removingPath(ws.Dir))
		if err != nil {
			return err
		}
		if !aside {
			log(fmt.Sprintf("%s has no workspace on this volume; nothing to remove", issue))
			return nil
		}
		// A prior pass's Remove renamed the workspace aside and was killed before finishing the
		// delete: it already judged this workspace safe to remove, and the renamed-aside
		// directory may be a half-deleted tree, unsafe to snapshot and meaningless to re-judge.
		// Finish it.
		if err := Remove(ctx, run, ws); err != nil {
			return err
		}
		log(fmt.Sprintf("removed %s's workspace: finished a removal an earlier pass was interrupted mid-delete", issue))
		return nil
	}
	snapshot := append([]string{"jj", "status", "--color=never"}, snapshotOverrides()...)
	snapshotted, err := RunChecked(ctx, run, snapshot, nil, ws.Dir)
	if err != nil {
		return err
	}
	if stderr := strings.TrimSpace(snapshotted.Stderr); stderr != "" {
		log(fmt.Sprintf("kept %s's workspace: the snapshot wrote to stderr (%s)", issue, stderr))
		return nil
	}
	walkCtx, cancelWalk := context.WithDeadline(ctx, deadline)
	nested, err := nestedRepositories(walkCtx, ws.Dir)
	cancelWalk()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log(fmt.Sprintf("kept %s's workspace: ran out of time walking for a nested repository (%v)", issue, err))
			return nil
		}
		return err
	}
	if len(nested) > 0 {
		log(fmt.Sprintf("kept %s's workspace: a nested repository below the workspace root (%s)",
			issue, strings.Join(nested, ", ")))
		return nil
	}
	if untracked := untrackedPaths(snapshotted.Stdout); len(untracked) > 0 {
		log(fmt.Sprintf("kept %s's workspace: the snapshot left %d path(s) untracked (%s)",
			issue, len(untracked), strings.Join(untracked, ", ")))
		return nil
	}
	workspaceName := filepath.Base(ws.Dir)
	unpushed, err := RunChecked(ctx, run, onClone(ws.Clone, "log", "-r", unpushedRevset(workspaceName, mergedHead), "--no-graph", "-T", `commit_id ++ "\n"`), nil, "")
	if err != nil {
		return err
	}
	if commits := nonEmptyLines(unpushed.Stdout); len(commits) > 0 {
		log(fmt.Sprintf("kept %s's workspace: %d commit(s) not reachable from a remote bookmark or the merged head (%s)",
			issue, len(commits), strings.Join(commits, ", ")))
		return nil
	}
	if err := Remove(ctx, run, ws); err != nil {
		return err
	}
	log(fmt.Sprintf("removed %s's workspace: every commit is on GitHub", issue))
	return nil
}

// nestedRepositories walks ws.Dir (filepath.WalkDir, which reads each entry's type the way
// os.Lstat does — never following a symlink into its target, so a symlinked directory reports
// IsDir false and WalkDir never recurses into one) for any entry named .git or .jj below the
// root, of any type — directory, plain file, symlink, even a dangling one — excluding only the
// workspace's own top-level .git and .jj (every workspace has both: a colocated workspace's own
// .git is a worktree *file*, its own .jj a real directory; neither is nested). jj's own skip rule
// (lib/src/local_working_copy.rs, the pinned fork) is `disk_dir.join(name).symlink_metadata().is_ok()`
// for both names — existence alone, no type check at all — so a plain file or a (possibly
// dangling) symlink named .git or .jj hides the whole directory from jj's snapshot exactly as a
// real nested repository does: not tracked, not listed as untracked, no warning anywhere.
// Confirmed against the pinned jj: a plain file and a dangling symlink named .git, planted beside
// an unpushed file, each made the snapshot report the directory as having no changes, with empty
// stderr. Found, the search stops descending into a nested directory (a file or a symlink has
// nothing to descend into); finding any one of them is enough to keep the whole workspace. The
// walk also checks ctx on every entry: a workspace holding a large gitignored tree jj's own
// snapshot never descends into (the walk is never gitignore-aware) can otherwise run well past
// the removal pass's own budget. ctx here is deadline (RemoveFinished's own parameter, the
// removal pass's budget deadline), wrapped around this walk alone — never the wider ctx a jj
// command still in flight runs under — so ctx.Err() bounds only this walk, and a timed-out walk
// keeps the workspace rather than fail the whole pass.
func nestedRepositories(ctx context.Context, root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.Name() != ".git" && d.Name() != ".jj" {
			return nil
		}
		if path == filepath.Join(root, ".git") || path == filepath.Join(root, ".jj") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		found = append(found, path)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("walk %s for a nested repository: %w", root, err)
	}
	return found, nil
}

// untrackedPaths reads jj status's own "Untracked paths:" section off stdout (never stderr's
// prose): every line "? <path>" immediately after that header, until a line that is not one.
// Absent the header, there is nothing to read, and the slice is empty.
func untrackedPaths(stdout string) []string {
	lines := strings.Split(stdout, "\n")
	var paths []string
	inSection := false
	for _, line := range lines {
		if line == "Untracked paths:" {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		path, ok := strings.CutPrefix(line, "? ")
		if !ok {
			break
		}
		paths = append(paths, path)
	}
	return paths
}
