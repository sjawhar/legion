package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A row the template prints for a commit jj cannot load carries an error value where the commit id
// goes, and a `jj workspace add` given it as a revision registers the workspace before it fails.
// createWorkspace refuses such a row by its shape before anything is added. Provision's fetch fails
// on the same clone first, so createWorkspace is called directly.
func TestCreateWorkspaceRefusesARowItCannotRead(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	runSetup(t, first.Clone, "jj", "new", "--no-edit", first.Bookmark, "-m", "its object goes", "--ignore-working-copy")
	runSetup(t, first.Clone, "jj", "bookmark", "set", first.Bookmark, "-r", first.Bookmark+"+", "--ignore-working-copy")
	gone := commitOf(t, first.Clone, first.Bookmark)
	runSetup(t, first.Clone, "jj", "workspace", "forget", filepath.Base(first.Dir), "-R", first.Clone)
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(first.Clone, ".git", "objects", gone[:2], gone[2:])); err != nil {
		t.Fatalf("remove the bookmark's commit object: %v", err)
	}

	before := len(run.Calls())
	err = createWorkspace(context.Background(), run, first)
	if err == nil || !strings.Contains(err.Error(), "Bookmark legion/WIDGETS-42's row \"local|1|0|0|<Error:") || !strings.Contains(err.Error(), "is not the shape") {
		t.Fatalf("createWorkspace on an unreadable bookmark: %v", err)
	}
	nothingProvisioned(t, run, before, first, "the refusal", "jj", "bookmark", "track")
}

// Two fetches that race on an untracked origin row, one seeing the branch move and one seeing it
// deleted, leave the row conflicted with one side a deletion, which `untracked_remote_bookmarks()`
// resolves to the moved commit alone. createWorkspace refuses it before tracking or adding anything,
// and the way out it names holds: the next provisioning's fetch sets the row to origin's branch as
// it is then, here none, so that provisioning starts at main. Provision's own fetch settles the row
// first, so createWorkspace is called directly.
func TestCreateWorkspaceRefusesAnOriginRowConcurrentFetchesConflicted(t *testing.T) {
	run := newLocalRunner(t)
	listed := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "fixture.txt")
	req := provisionRequest(t)
	other := req
	other.Issue = "WIDGETS-7"
	if _, err := Provision(context.Background(), run, other); err != nil {
		t.Fatalf("provision the shared clone: %v", err)
	}
	workspace, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	clone := workspace.Clone
	fromOrigin(t, run, clone, "jj", "git", "fetch", "-R", clone)
	operation := strings.TrimSpace(runSetup(t, clone, "jj", "op", "log", "--no-graph", "-T", "id.short()", "--limit", "1", "--ignore-working-copy", "-R", clone))
	moved := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "moved.txt")
	fromOrigin(t, run, clone, "jj", "--at-op", operation, "git", "fetch", "-R", clone)
	runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "update-ref", "-d", "refs/heads/legion/WIDGETS-42")
	fromOrigin(t, run, clone, "jj", "--at-op", operation, "git", "fetch", "-R", clone)

	before := len(run.Calls())
	err = createWorkspace(context.Background(), run, workspace)
	want := "Remote bookmark legion/WIDGETS-42@origin is conflicted (adds " + moved + "; removes " + listed + "), one side a deletion, which concurrent fetches leave"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("createWorkspace on a conflicted origin row: %v\nwant it to contain %q", err, want)
	}
	nothingProvisioned(t, run, before, workspace, "the refusal", "jj", "bookmark", "track")

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if parent, main := commitOf(t, working.Dir, "@-"), commitOf(t, clone, "main@origin"); parent != main {
		t.Errorf("after provisioning again the workspace's @- is %s, want main's %s", parent, main)
	}
}

// trackRace runs a function once, just before the first `jj bookmark track` it is asked to run.
type trackRace struct {
	*recordingRunner
	before func()
}

func (r *trackRace) Run(ctx context.Context, command Command) (Result, error) {
	if before := r.before; before != nil && commandWith(command.Argv, "jj", "bookmark", "track") {
		r.before = nil
		before()
	}
	return r.recordingRunner.Run(ctx, command)
}

// A fetch between the read of an untracked origin row and its track moves the row, so the track
// leaves the local bookmark on the newer commit. A workspace at the listed commit could never move
// that bookmark: jj refuses the implementer's `jj bookmark set` as sideways. That provisioning is
// refused before anything is added, and the next one starts at the bookmark.
func TestProvisionRefusesABranchThatMovedWhileItWasTracked(t *testing.T) {
	run := newLocalRunner(t)
	listed := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "fixture.txt")
	req := provisionRequest(t)
	workspace, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	var moved string
	race := &trackRace{recordingRunner: run, before: func() {
		moved = pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "moved.txt")
		fromOrigin(t, run, workspace.Clone, "jj", "git", "fetch", "-R", workspace.Clone)
	}}

	before := len(run.Calls())
	_, err = Provision(context.Background(), race, req)
	if moved == "" {
		t.Fatalf("provisioning never tracked the origin row: %v", err)
	}
	want := "Bookmark legion/WIDGETS-42@origin moved from " + listed + " to " + moved + " while it was being tracked"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("provision across a moved origin row: %v\nwant it to contain %q", err, want)
	}
	nothingProvisioned(t, run, before, workspace, "the refusal")

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if parent := commitOf(t, working.Dir, "@-"); parent != moved {
		t.Errorf("after provisioning again the workspace's @- is %s, want the bookmark's %s", parent, moved)
	}
}

// A local bookmark tracked on origin before its first push (`jj bookmark track legion/<KEY>@origin`,
// which jj's hint names when a push refuses a new remote bookmark, or auto-tracking) sits beside an
// origin row that is tracked and has no target. It is a healthy bookmark: the workspace starts at
// its commit.
func TestProvisionStartsAtALocalBookmarkTrackedBeforeItsFirstPush(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	local := commitOf(t, first.Dir, first.Bookmark)
	runSetup(t, first.Clone, "jj", "bookmark", "track", first.Bookmark+"@origin", "-R", first.Clone)
	rows := runSetup(t, first.Clone, "jj", "bookmark", "list", "--all-remotes", "exact:"+first.Bookmark, "-T", bookmarkRowTemplate, "--ignore-working-copy", "-R", first.Clone)
	if !strings.Contains(rows, "local|1|0|0|"+local+"|\n") || !strings.Contains(rows, "origin|0|0|1||\n") {
		t.Fatalf("the rows are not a local bookmark tracked before its first push:\n%s", rows)
	}
	runSetup(t, first.Clone, "jj", "workspace", "forget", filepath.Base(first.Dir), "-R", first.Clone)
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}

	second, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision a bookmark tracked before its first push: %v", err)
	}
	if parent := commitOf(t, second.Dir, "@-"); parent != local {
		t.Errorf("the workspace's @- is %s, want the local bookmark's %s", parent, local)
	}
}
