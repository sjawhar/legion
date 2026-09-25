package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func TestProvisionUsesBookmarkCommitAndRefusesConflictBeforeWorkspaceAdd(t *testing.T) {
	t.Run("one bookmark commit", func(t *testing.T) {
		run := newLocalRunner(t)
		req := provisionRequest(t)
		first, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		commit := runSetup(t, first.Dir, "jj", "log", "-r", "@", "--no-graph", "-T", "commit_id")
		commit = strings.TrimSpace(commit)
		runSetup(t, req.StateDir, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets"))
		if err := os.RemoveAll(first.Dir); err != nil {
			t.Fatalf("remove forgotten workspace directory: %v", err)
		}

		before := len(run.Calls())
		second, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("re-provision from bookmark: %v", err)
		}
		if second != first {
			t.Errorf("workspace = %#v, want %#v", second, first)
		}
		calls := run.Calls()[before:]
		add := findCall(t, calls, "jj", "workspace", "add")
		if got := add.Argv[7]; got != commit {
			t.Errorf("workspace revision = %q, want bookmark commit %q", got, commit)
		}
		for _, call := range calls {
			if commandWith(call.Argv, "jj", "bookmark", "set") {
				t.Errorf("resolving bookmark should not be moved: %#v", call.Argv)
			}
		}
	})

	t.Run("conflicted bookmark", func(t *testing.T) {
		run := newLocalRunner(t)
		req := provisionRequest(t)
		workspace, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
		runSetup(t, workspace.Dir, "jj", "new", "-m", "later work")
		operation := strings.TrimSpace(runSetup(t, clone, "jj", "op", "log", "--no-graph", "-T", "id.short()", "--limit", "1"))
		runSetup(t, workspace.Dir, "jj", "bookmark", "set", workspace.Bookmark, "-r", "@")
		runSetup(t, clone, "jj", "--at-op", operation, "bookmark", "set", workspace.Bookmark, "-r", "main", "--allow-backwards")
		runSetup(t, clone, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", clone)
		if err := os.RemoveAll(workspace.Dir); err != nil {
			t.Fatalf("remove conflict workspace: %v", err)
		}

		before := len(run.Calls())
		_, err = Provision(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "Bookmark legion/WIDGETS-42 is conflicted") {
			t.Fatalf("conflicted provision error = %v", err)
		}
		for _, call := range run.Calls()[before:] {
			if commandWith(call.Argv, "jj", "workspace", "add") {
				t.Errorf("conflicted bookmark reached workspace add: %#v", call.Argv)
			}
		}
		if _, statErr := os.Stat(workspace.Dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("conflicted bookmark created workspace directory: %v", statErr)
		}
	})
}

// commitOf is the commit id rev names in dir's repository, read without a snapshot.
func commitOf(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(runSetup(t, dir, "jj", "log", "-r", rev, "--no-graph", "-T", "commit_id", "--ignore-working-copy"))
}

// feedRequest is req provisioned as a pod provisions it: from a feed a fresh Fetch filled, with no
// credential.
func feedRequest(t *testing.T, run *recordingRunner, req Request) Request {
	t.Helper()
	fetch := fetchRequest(t)
	if _, err := Fetch(context.Background(), run, fetch); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return Request{StateDir: req.StateDir, Repo: req.Repo, Issue: req.Issue, CredentialHelper: req.CredentialHelper, Source: FromFeed(fetch.Feed)}
}

// pushRemoteBranch pushes one commit adding file to branch on the bare remote, from a clone of its
// own: on top of the branch when the remote has it, on main otherwise. It returns the commit's id.
func pushRemoteBranch(t *testing.T, remote, branch, file string) string {
	t.Helper()
	scratch := t.TempDir()
	runSetup(t, scratch, "git", "clone", "--quiet", remote, "other")
	other := filepath.Join(scratch, "other")
	if exec.Command("git", "-C", other, "rev-parse", "--verify", "--quiet", "origin/"+branch).Run() == nil {
		runSetup(t, other, "git", "checkout", "--quiet", branch)
	} else {
		runSetup(t, other, "git", "checkout", "--quiet", "-b", branch)
	}
	if err := os.WriteFile(filepath.Join(other, file), []byte("pushed from another clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, other, "git", "add", file)
	runSetup(t, other, "git", "-c", "user.email=legion-test@example.invalid", "-c", "user.name=Legion test", "commit", "--quiet", "-m", "pushed from another clone")
	runSetup(t, other, "git", "push", "--quiet", "origin", branch)
	return strings.TrimSpace(runSetup(t, other, "git", "rev-parse", "HEAD"))
}

// lostWorkspace is an issue whose workspace pushed its work from the shared clone and was then lost,
// as a tree whose volume went loses it: forgotten, its directory gone, the clone kept.
type lostWorkspace struct {
	run    *recordingRunner
	req    Request
	first  Workspace
	clone  string
	pushed string
}

// pushedThenLost provisions legion/WIDGETS-42 on the host's path, commits file there, pushes the
// bookmark from the shared clone, and loses the workspace.
func pushedThenLost(t *testing.T, file string) lostWorkspace {
	t.Helper()
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
	if err := os.WriteFile(filepath.Join(first.Dir, file), []byte("pushed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, first.Dir, "jj", "describe", "-m", "pushed work")
	pushed := commitOf(t, first.Dir, first.Bookmark)
	runSetup(t, clone, "jj", "git", "push", "--remote", "origin", "--bookmark", first.Bookmark)
	runSetup(t, clone, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", clone)
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}
	return lostWorkspace{run: run, req: req, first: first, clone: clone, pushed: pushed}
}

// refusedBeforeAnything provisions the lost workspace's issue and returns the refusal, failing the
// test when provisioning succeeds or when it added, tracked, created or registered anything first.
func (l lostWorkspace) refusedBeforeAnything(t *testing.T, attempt int) string {
	t.Helper()
	before := len(l.run.Calls())
	_, err := Provision(context.Background(), l.run, l.req)
	if err == nil {
		t.Fatalf("attempt %d provisioned", attempt)
	}
	nothingProvisioned(t, l.run, before, l.first, fmt.Sprintf("attempt %d", attempt), "jj", "bookmark", "track")
	return err.Error()
}

// nothingProvisioned fails the test when a call after the first before ran `jj workspace add` or
// starts with also, or when workspace's directory or registration exists; what names the attempt.
func nothingProvisioned(t *testing.T, run *recordingRunner, before int, workspace Workspace, what string, also ...string) {
	t.Helper()
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "jj", "workspace", "add") || len(also) > 0 && commandWith(call.Argv, also...) {
			t.Errorf("%s ran %q", what, call.Argv)
		}
	}
	if _, statErr := os.Stat(workspace.Dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("%s created the workspace directory: %v", what, statErr)
	}
	if listed := runSetup(t, workspace.Clone, "jj", "workspace", "list", "-R", workspace.Clone); strings.Contains(listed, filepath.Base(workspace.Dir)+":") {
		t.Errorf("%s registered the workspace:\n%s", what, listed)
	}
}

// An issue branch another clone pushed before the issue's first workspace existed — a repository's
// fixture, or the branch a tree pushed before its volume was lost — is where the workspace starts:
// provisioning tracks legion/<KEY>@origin and adds the workspace at it, on the host's path and on
// a pod's feed. The branch's commits are in the workspace, the local bookmark is the remote's, and
// the issue's next push moves that branch.
func TestProvisionAdoptsAnIssueBranchOnlyTheRemoteHas(t *testing.T) {
	for _, path := range []string{"host", "feed"} {
		t.Run(path, func(t *testing.T) {
			run := newLocalRunner(t)
			pushed := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "fixture.txt")
			req := provisionRequest(t)
			if path == "feed" {
				req = feedRequest(t, run, req)
			}
			working, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if _, err := os.Stat(filepath.Join(working.Dir, "fixture.txt")); err != nil {
				t.Errorf("the workspace does not hold the branch the remote already had: %v", err)
			}
			if parent := commitOf(t, working.Dir, "@-"); parent != pushed {
				t.Errorf("the workspace's @- is %s, want the remote branch's %s", parent, pushed)
			}
			if local := commitOf(t, working.Dir, `bookmarks(exact:"legion/WIDGETS-42")`); local != pushed {
				t.Errorf("the local legion/WIDGETS-42 is %q, want the remote's %s", local, pushed)
			}

			if err := os.WriteFile(filepath.Join(working.Dir, "work.txt"), []byte("the implementer's change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runSetup(t, working.Dir, "jj", "describe", "-m", "the implementer's change")
			runSetup(t, working.Dir, "jj", "bookmark", "set", "legion/WIDGETS-42", "-r", "@")
			fromOrigin(t, run, working.Dir, "jj", "git", "push", "--bookmark", "legion/WIDGETS-42")
			parents := strings.TrimSpace(runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "log", "-1", "--format=%P", "legion/WIDGETS-42"))
			if parents != pushed {
				t.Errorf("after the push the remote branch's head has parents %q, want the adopted %s", parents, pushed)
			}
		})
	}
}

// A merged pull request's branch, deleted on GitHub, leaves no remote row: an issue workspace
// provisioned after that — its directory gone, as when a tree's volume is lost — starts from main
// as a brand-new issue's does, and the merged branch is not brought back.
func TestProvisionAfterTheMergedBranchIsDeletedStartsFromMain(t *testing.T) {
	l := pushedThenLost(t, "merged.txt")
	runSetup(t, l.req.StateDir, "git", "--git-dir="+l.run.remote, "branch", "-D", l.first.Bookmark)

	second, err := Provision(context.Background(), l.run, l.req)
	if err != nil {
		t.Fatalf("provision after the merged branch was deleted: %v", err)
	}
	if parent, main := commitOf(t, second.Dir, "@-"), commitOf(t, l.clone, "main@origin"); parent != main {
		t.Errorf("the new workspace's @- is %s, want main's %s", parent, main)
	}
	if _, err := os.Stat(filepath.Join(second.Dir, "merged.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the new workspace holds the merged branch's file: %v", err)
	}
	if local := commitOf(t, second.Dir, `bookmarks(exact:"legion/WIDGETS-42")`); local == l.pushed || local == "" {
		t.Errorf("the local legion/WIDGETS-42 is %q: want it created on the new working copy, never the merged %s", local, l.pushed)
	}
}

// codeSpan is the refusal's backticked command that starts with prefix, failing the test when the
// refusal names none.
func codeSpan(t *testing.T, refusal, prefix string) string {
	t.Helper()
	spans := strings.Split(refusal, "`")
	for i := 1; i < len(spans); i += 2 {
		if strings.HasPrefix(spans[i], prefix) {
			return spans[i]
		}
	}
	t.Fatalf("the refusal names no `%s …`:\n%s", prefix, refusal)
	return ""
}

// githubDelete is how a refusal names deleting an acme/widgets branch on GitHub.
const githubDelete = "gh api -X DELETE repos/acme/widgets/git/refs/heads/"

// runWayOut runs the refusal's backticked command that starts with prefix, exactly as written but
// for a `<commit>` placeholder, which becomes commit, and returns it. A GitHub deletion is GitHub's
// side of it: the ref the command names leaves the remote.
func (l lostWorkspace) runWayOut(t *testing.T, refusal, prefix, commit string) string {
	t.Helper()
	command := strings.ReplaceAll(codeSpan(t, refusal, prefix), "<commit>", commit)
	if ref, ok := strings.CutPrefix(command, githubDelete); ok {
		runSetup(t, l.req.StateDir, "git", "--git-dir="+l.run.remote, "update-ref", "-d", "refs/heads/"+ref)
		return command
	}
	// A pending change in the shared clone's own working copy, which a snapshot would record: the
	// way out, run from an operator's shell, must take none (and so runs no filter the tree
	// planted there).
	planted := filepath.Join(l.clone, "way-out-pending.txt")
	if err := os.WriteFile(planted, []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := cloneSnapshots(t, l.clone)
	runSetup(t, l.clone, strings.Fields(command)...)
	if after := cloneSnapshots(t, l.clone); after != before {
		t.Errorf("%q snapshotted the shared clone's working copy (%d snapshots, then %d)", command, before, after)
	}
	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}
	return command
}

// cloneSnapshots is how many operations in the shared clone's log snapshotted its working copy.
func cloneSnapshots(t *testing.T, clone string) int {
	t.Helper()
	log := runSetup(t, clone, "jj", "op", "log", "--no-graph", "-T", `description ++ "\n"`, "--ignore-working-copy", "--color=never", "-R", clone)
	return strings.Count(log, "snapshot working copy\n")
}

// A local legion/<KEY> deleted in the shared clone (`jj bookmark delete`) and never pushed leaves
// origin's row tracked with no local bookmark. Tracking it changes nothing, and starting at main
// would put a second branch beside the remote's, which the next push would drop. Provisioning
// refuses that state by name, every time, before anything is registered, and each of the three
// ways out, run exactly as the refusal writes it, provisions as it says: restoring the bookmark and
// cancelling the deletion start at origin's branch, and deleting the branch on GitHub starts at
// main.
func TestProvisionRefusesALocalDeletionNeverPushed(t *testing.T) {
	for _, way := range []struct {
		name       string
		command    string
		atPushed   bool
		remoteGone bool
	}{
		{"restore", "jj bookmark set", true, false},
		{"cancel the deletion", "jj bookmark forget", true, false},
		{"delete the branch on GitHub", githubDelete, false, true},
	} {
		t.Run(way.name, func(t *testing.T) {
			l := pushedThenLost(t, "pushed.txt")
			runSetup(t, l.clone, "jj", "bookmark", "delete", l.first.Bookmark)
			var refusal string
			for attempt := 1; attempt <= 2; attempt++ {
				refusal = l.refusedBeforeAnything(t, attempt)
				if !strings.Contains(refusal, "Bookmark legion/WIDGETS-42 was deleted in the shared clone") {
					t.Fatalf("attempt %d: the refusal does not name the local deletion:\n%s", attempt, refusal)
				}
			}

			command := l.runWayOut(t, refusal, way.command, "")
			working, err := Provision(context.Background(), l.run, l.req)
			if err != nil {
				t.Fatalf("provision after `%s`: %v", command, err)
			}
			parent, main := commitOf(t, working.Dir, "@-"), commitOf(t, l.clone, "main@origin")
			switch {
			case way.atPushed && parent != l.pushed:
				t.Errorf("after `%s` the workspace's @- is %s, want origin's branch %s", command, parent, l.pushed)
			case !way.atPushed && parent != main:
				t.Errorf("after `%s` the workspace's @- is %s, want main's %s", command, parent, main)
			}
			remote := strings.TrimSpace(runSetup(t, l.req.StateDir, "git", "--git-dir="+l.run.remote, "branch", "--list", l.first.Bookmark))
			if gone := remote == ""; gone != way.remoteGone {
				t.Errorf("after `%s` origin's %s is %q; want gone: %v", command, l.first.Bookmark, remote, way.remoteGone)
			}
		})
	}
}

// A bookmark whose sides diverged is conflicted after the next fetch: a local deletion never pushed
// and then origin's branch moving (a human's push, GitHub's Update branch), a local move never
// pushed and then the branch deleted on GitHub (a merge), or a local move never pushed while origin
// moved. With a deleted side, `bookmarks(exact:)` resolves it to the other side's commit alone.
// Provisioning refuses the conflict by name, with its sides, every time, before anything is
// registered, and each of its two ways out, every command of it run in order exactly as the refusal
// writes it, provisions as it says: keeping an added commit starts there, and starting from main
// (deleting the branch on GitHub while origin has it, then the local bookmark) starts at main.
func TestProvisionRefusesALocalConflictAndItsWaysOutHold(t *testing.T) {
	moveLocally := func(t *testing.T, l lostWorkspace) string {
		runSetup(t, l.clone, "jj", "new", "--no-edit", l.first.Bookmark, "-m", "never pushed", "--ignore-working-copy")
		runSetup(t, l.clone, "jj", "bookmark", "set", l.first.Bookmark, "-r", l.first.Bookmark+"+", "--ignore-working-copy")
		return commitOf(t, l.clone, l.first.Bookmark)
	}
	for _, diverge := range []struct {
		name string
		// diverges the sides of l's bookmark, returning the commit to keep and the refusal's sides
		sides func(t *testing.T, l lostWorkspace) (string, string)
		// fromMain is the refusal's commands, in order, that start from main in this state
		fromMain []string
	}{
		{"deleted locally, then origin moved", func(t *testing.T, l lostWorkspace) (string, string) {
			runSetup(t, l.clone, "jj", "bookmark", "delete", l.first.Bookmark)
			moved := pushRemoteBranch(t, l.run.remote, l.first.Bookmark, "moved.txt")
			return moved, "(adds " + moved + "; removes " + l.pushed + "), one side a deletion"
		}, []string{githubDelete, "jj bookmark delete"}},
		{"moved locally, then deleted on GitHub", func(t *testing.T, l lostWorkspace) (string, string) {
			local := moveLocally(t, l)
			runSetup(t, l.req.StateDir, "git", "--git-dir="+l.run.remote, "update-ref", "-d", "refs/heads/"+l.first.Bookmark)
			return local, "(adds " + local + "; removes " + l.pushed + "), one side a deletion"
		}, []string{"jj bookmark delete"}},
		{"moved locally while origin moved", func(t *testing.T, l lostWorkspace) (string, string) {
			local := moveLocally(t, l)
			moved := pushRemoteBranch(t, l.run.remote, l.first.Bookmark, "moved.txt")
			return local, "(adds " + local + ", " + moved + "; removes " + l.pushed + ")"
		}, []string{githubDelete, "jj bookmark delete"}},
	} {
		for _, way := range []struct {
			name     string
			commands []string
			atKept   bool
		}{
			{"keep an added commit", []string{"jj bookmark set"}, true},
			{"start from main", diverge.fromMain, false},
		} {
			t.Run(diverge.name+"/"+way.name, func(t *testing.T) {
				l := pushedThenLost(t, "pushed.txt")
				keep, sides := diverge.sides(t, l)
				var refusal string
				for attempt := 1; attempt <= 2; attempt++ {
					refusal = l.refusedBeforeAnything(t, attempt)
					if want := "Bookmark legion/WIDGETS-42 is conflicted " + sides + ";"; !strings.Contains(refusal, want) {
						t.Fatalf("attempt %d: the refusal lacks %q:\n%s", attempt, want, refusal)
					}
				}
				var ran []string
				for _, prefix := range way.commands {
					ran = append(ran, l.runWayOut(t, refusal, prefix, keep))
				}
				working, err := Provision(context.Background(), l.run, l.req)
				if err != nil {
					t.Fatalf("provision after %q: %v", ran, err)
				}
				parent, main := commitOf(t, working.Dir, "@-"), commitOf(t, l.clone, "main@origin")
				switch {
				case way.atKept && parent != keep:
					t.Errorf("after %q the workspace's @- is %s, want the kept %s", ran, parent, keep)
				case !way.atKept && parent != main:
					t.Errorf("after %q the workspace's @- is %s, want main's %s", ran, parent, main)
				}
			})
		}
	}
}
