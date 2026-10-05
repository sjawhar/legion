package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A finished child whose every commit reached origin — parked after its own push — is removed.
func TestRemoveFinishedRemovesAWorkspaceWithEveryCommitPushed(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains after RemoveFinished: %v", err)
	}
	if want := "removed WIDGETS-42's workspace"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}

// A finished child whose last commit is only the merged pull request's head is removed: GitHub
// deletes a squash merge's branch, so that commit carries no remote bookmark of its own once the
// clone's next fetch prunes it — and the daemon's recorded merged head is what tells that commit
// from one that was never pushed at all.
func TestRemoveFinishedRemovesAWorkspaceWhoseLastCommitIsOnlyTheMergedHead(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "feature.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	mergedHead := strings.TrimSpace(runSetup(t, ws.Dir, "jj", "log", "-r", ws.Bookmark, "--no-graph", "-T", "commit_id"))
	runSetup(t, ws.Clone, "jj", "git", "push", "--remote", "origin", "--bookmark", ws.Bookmark, "--allow-empty-description")
	runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "branch", "-D", ws.Bookmark)
	// The next pod's own provisioning fetch is what prunes the now-deleted branch on the shared
	// clone; RemoveFinished is never the one that fetches.
	runSetup(t, ws.Clone, "jj", "git", "fetch")
	if listed := strings.TrimSpace(runSetup(t, ws.Clone, "jj", "log", "-r", "bookmarks(exact:"+ws.Bookmark+")", "--no-graph", "-T", "commit_id")); listed != "" {
		t.Fatalf("test setup: bookmark remains after branch deletion: %s", listed)
	}

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", mergedHead, func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains after RemoveFinished: %v", err)
	}
	if want := "removed WIDGETS-42's workspace"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}

// A child with a commit that reached neither a remote bookmark nor the merged head — local-only
// work no push ever carried — is kept, and the kept commit is named in the one line RemoveFinished
// logs, never deleted.
func TestRemoveFinishedKeepsAWorkspaceWithAnUnpushedCommit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "unpushed.txt"), []byte("never pushed\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, ws.Dir, "jj", "status")
	commit := strings.TrimSpace(runSetup(t, ws.Dir, "jj", "log", "-r", "@", "--no-graph", "-T", "commit_id"))

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") || !strings.Contains(logged[0], commit) {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept and commit %s", logged, commit)
	}
}

// A pending edit the workspace's own agent never ran a jj command over since is still counted as
// unpushed work: RemoveFinished snapshots ws.Dir itself before judging it, so a file dropped on
// disk and never committed does not slip past the push-safety check (dispatch://LEGION-583's
// correction: a parked child's un-snapshotted edit must keep its workspace).
func TestRemoveFinishedSnapshotsTheWorkspaceBeforeJudgingAnUncommittedEdit(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "pending.txt"), []byte("never snapshotted\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	// No manual `jj status` here, unlike the other tests above: the edit above is on disk only,
	// untracked by any jj operation, until RemoveFinished itself snapshots it.

	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("workspace removed, want it kept: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "kept WIDGETS-42's workspace") {
		t.Errorf("logged %v, want one line naming WIDGETS-42 kept", logged)
	}
}

// A candidate with no workspace on this volume — already removed, or never provisioned here — is
// left alone without error: the daemon's candidate list is computed from issue lifecycle alone and
// may still name one nothing on this volume ever created.
func TestRemoveFinishedOfAWorkspaceAlreadyGoneIsANoop(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	ws, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	var logged []string
	if err := RemoveFinished(context.Background(), run, ws, "WIDGETS-42", "", func(line string) { logged = append(logged, line) }); err != nil {
		t.Fatalf("RemoveFinished: %v", err)
	}
	if want := "has no workspace on this volume"; len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Errorf("logged %v, want one line containing %q", logged, want)
	}
}
