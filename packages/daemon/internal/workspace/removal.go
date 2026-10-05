package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// RemovalCandidate is one sibling workspace of a tree the daemon has judged safe to remove by
// lifecycle alone: every role's claim is gone (suspended, failed, retired, or never claimed), with
// no live pod. MergedHead is the merged pull request's head commit the daemon recorded for a
// candidate that merged, empty for one that never did. GitHub deletes a squash merge's branch, so
// that commit carries no remote bookmark of its own once the clone fetches the deletion;
// RemoveFinished's push-safety check below needs the recorded head to tell that commit from one
// that was never pushed at all (dispatch://LEGION-583).
type RemovalCandidate struct {
	Issue      string `json:"issue"`
	MergedHead string `json:"mergedHead,omitempty"`
}

// unpushedRevset is RemoveFinished's push-safety check: every non-empty ancestor of the
// workspace's own commit that is reachable from neither a remote bookmark nor, when the daemon
// recorded one, an ancestor of mergedHead. empty() is excluded because jj always keeps an empty
// commit at the tip of a workspace's working copy; without the exclusion every workspace would
// show that placeholder as unreachable from anything and never be judged safe.
func unpushedRevset(workspaceName, mergedHead string) string {
	revset := "::" + workspaceName + "@ ~ empty() ~ ::(remote_bookmarks())"
	if mergedHead != "" {
		revset += " ~ ::" + mergedHead
	}
	return revset
}

// RemoveFinished removes ws when every non-empty commit it holds is on GitHub by unpushedRevset's
// rule, and otherwise keeps it, naming in one log line the commits that are not — never deleting
// unpushed work. A workspace already gone (removed already, or never provisioned on this volume)
// is left alone without error, unless a prior pass's Remove renamed it aside and was killed
// before finishing the delete (removingSuffix, bookmark.go): that renamed-aside directory, found
// where ws.Dir no longer is, is finished rather than re-judged — it was already found safe to
// remove, and a half-deleted tree is unsafe to snapshot. The daemon's candidate list is computed
// from issue lifecycle alone and may still name a workspace nothing on this volume ever created.
// log receives exactly one
// line: what was removed, what was kept and why, or that there was nothing to do. ws is Location's,
// which names the shared clone; issue is logged as the daemon knows it (ws.Dir's own base name is
// lowercased for jj's workspace name, per Location).
//
// Before judging anything, the check snapshots ws.Dir's own working copy (a plain `jj status`
// there, not onClone's --ignore-working-copy, and never the shared clone's): a child parked
// between phases can hold an edit its agent never ran a jj command over since, and without this
// snapshot that edit is invisible to unpushedRevset's `::workspaceName@` and the workspace would
// be judged clean and removed out from under it. The one jj command this package runs against the
// shared clone proper without --ignore-working-copy is `jj workspace add` (createWorkspace); this
// is the other, and the only one that runs a real snapshot over a workspace a tree agent writes
// to directly, so it carries snapshotOverrides (config.go): without them a tree agent's own
// `jj config set --repo` could run a hostile working-copy filter or signing program on this
// snapshot, since that command writes the one repo config every workspace of the tree reads.
//
// The snapshot can still leave a path untracked rather than commit it: jj's own anti-footgun
// limit on a new file's size (snapshot.max-new-file-size, which snapshotOverrides disables for
// this command, so this is belt and suspenders) and any other reason jj's auto-track
// (snapshot.auto-track = "all()" by default, so every other new path is committed) might still
// skip a path both exit 0 and print no error — jj's own contract for "something was left out" is
// the "Untracked paths:" header `jj status` prints on stdout (never stderr's prose, which is only
// the human explanation and is not guaranteed stable across reasons or releases) before every
// such path, one per line as `? <path>`. Reading that header is what untrackedPaths below does;
// its presence means @ does not hold everything on disk, so unpushedRevset's read of @ cannot be
// trusted to prove the workspace clean, and the workspace is kept rather than risk removing one
// that in fact still holds unpushed work on disk.
func RemoveFinished(ctx context.Context, run Runner, ws Workspace, issue, mergedHead string, log func(string)) error {
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
