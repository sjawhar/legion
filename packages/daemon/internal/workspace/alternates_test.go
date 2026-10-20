package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared clone whose objects live in a bare feed through objects/info/alternates (git clone
// --shared, then jj git init --colocate) works for everything an issue does: jj reads the history,
// a workspace is added at the default branch, a commit is made and pushed, a second provisioning
// recovers the workspace from the pushed branch after the workspace directory is lost, and a fetch
// into the feed is visible to jj. No pack is copied into the clone's own store. The clone checks
// out the feed's HEAD, so jj's first snapshot of the clone's own workspace records no change: on a
// --no-checkout clone it records every file deleted.
func TestJJWorksOnACloneWhoseObjectsAreAnAlternate(t *testing.T) {
	remote := seedRemote(t, t.TempDir(), "release/v7.1")
	pushRemoteBranch(t, remote, "release/v7.1", "history.txt")
	feed := filepath.Join(t.TempDir(), "feed", "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "clone", "--bare", "--quiet", remote, feed)
	clone := filepath.Join(t.TempDir(), "repos", "github.com", "acme", "widgets")
	runSetup(t, t.TempDir(), "git", "clone", "--shared", "--quiet", feed, clone)
	runSetup(t, clone, "git", "remote", "set-url", "origin", remote)
	runSetup(t, clone, "jj", "git", "init", "--colocate")
	alternates, err := os.ReadFile(filepath.Join(clone, ".git", "objects", "info", "alternates"))
	if err != nil || strings.TrimSpace(string(alternates)) != filepath.Join(feed, "objects") {
		t.Fatalf("alternates = %q (%v), want the feed's objects", alternates, err)
	}
	runSetup(t, clone, "jj", "git", "fetch", "-R", clone)
	if listed := strings.TrimSpace(runSetup(t, clone, "jj", "log", "-r", "release/v7.1@origin", "--no-graph", "-T", "commit_id", "-R", clone)); len(listed) != 40 {
		t.Fatalf("jj cannot read the default branch through the alternate: %q", listed)
	}
	if diff := strings.TrimSpace(runSetup(t, clone, "jj", "diff", "--summary", "-r", "@", "-R", clone)); diff != "" {
		t.Fatalf("jj's first snapshot of the shared clone's own workspace changed it:\n%s", diff)
	}
	working := filepath.Join(t.TempDir(), "workspaces", "widgets-42")
	if err := os.MkdirAll(filepath.Dir(working), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetup(t, clone, "jj", "workspace", "add", working, "--name", "widgets-42", "--revision", "release/v7.1@origin", "-R", clone)
	if err := os.WriteFile(filepath.Join(working, "change.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSetup(t, working, "jj", "commit", "-m", "a change", "-R", working)
	runSetup(t, working, "jj", "git", "push", "--named", "legion/WIDGETS-42=@-", "-R", working)
	pushed := strings.TrimSpace(runSetup(t, working, "git", "--git-dir="+remote, "rev-parse", "refs/heads/legion/WIDGETS-42"))
	if len(pushed) != 40 {
		t.Fatalf("the push did not land: %q", pushed)
	}
	// The clone's own store holds no pack and none of the feed's objects, only what jj wrote
	// itself (the new commit, its working-copy commits): history stayed in the feed.
	assertHistoryOnlyInFeed(t, filepath.Join(clone, ".git"), feed)
	// Lost workspace: the directory goes, the clone forgets it, a second add starts at the pushed branch.
	if err := os.RemoveAll(working); err != nil {
		t.Fatal(err)
	}
	runSetup(t, clone, "jj", "workspace", "forget", "widgets-42", "-R", clone)
	// jj leaves the colocated workspace's git worktree entry, locked; provisioning's re-add removes
	// it first (createWorkspace), so the spike does what provisioning does.
	if err := removeGitWorktree(clone, working); err != nil {
		t.Fatal(err)
	}
	runSetup(t, clone, "jj", "git", "fetch", "-R", clone)
	runSetup(t, clone, "jj", "workspace", "add", working, "--name", "widgets-42", "--revision", "legion/WIDGETS-42@origin", "-R", clone)
	if parent := commitOf(t, working, "@-"); parent != pushed {
		t.Fatalf("the recovered workspace's @- is %s, want the pushed %s", parent, pushed)
	}
	// A fetch into the feed (what the pod's fetch container does on a relaunch) is visible to jj.
	moved := pushRemoteBranch(t, remote, "release/v7.1", "moved.txt")
	runSetup(t, feed, "git", "--git-dir="+feed, "fetch", "--quiet", "origin", "+refs/heads/*:refs/heads/*")
	runSetup(t, clone, "jj", "git", "fetch", "-R", clone)
	if head := commitOf(t, clone, "release/v7.1@origin"); head != moved {
		t.Fatalf("after the feed fetch jj sees %s, want %s", head, moved)
	}
}

// assertHistoryOnlyInFeed fails unless the git directory gitDir holds no pack of its own and none
// of the objects reachable in the bare feed as a loose object: the clone reads its history through
// objects/info/alternates and copied nothing. The empty tree is passed over: jj writes it into
// every store it opens, for its own working-copy commits.
func assertHistoryOnlyInFeed(t *testing.T, gitDir, feed string) {
	t.Helper()
	counts := map[string]string{}
	for _, line := range strings.Split(runSetup(t, gitDir, "git", "--git-dir="+gitDir, "count-objects", "-v"), "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			counts[key] = strings.TrimSpace(value)
		}
	}
	if counts["in-pack"] != "0" || counts["packs"] != "0" {
		t.Fatalf("the clone's own store holds packs=%s in-pack=%s; history should stay in the feed", counts["packs"], counts["in-pack"])
	}
	history := strings.Fields(runSetup(t, feed, "git", "--git-dir="+feed, "rev-list", "--objects", "--no-object-names", "--all"))
	if len(history) == 0 {
		t.Fatal("the feed holds no history to compare the clone's store with")
	}
	for _, object := range history {
		if object == emptyTree {
			continue
		}
		if _, err := os.Stat(filepath.Join(gitDir, "objects", object[:2], object[2:])); err == nil {
			t.Fatalf("the clone's own store holds %s, an object of the feed's history", object)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// emptyTree is git's id of the tree with no entry.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
