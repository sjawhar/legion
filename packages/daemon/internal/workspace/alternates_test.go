package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A clone from the feed keeps its own objects, so replacing a foreign feed cannot remove history
// that its refs still need. The replacement path is the same one workspace-fetch takes before
// workspace-init provisions and fetches the clone.
func TestACloneSurvivesAForeignFeedReplacement(t *testing.T) {
	run := newLocalRunner(t)
	lost := pushRemoteBranch(t, run.remote, "history", "history.txt")
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the feed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(working.Clone, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clone's alternates = %v, want no borrowed history", err)
	}
	runSetup(t, fetch.Feed, "git", "--git-dir="+run.remote, "update-ref", "-d", "refs/heads/history")
	if err := os.WriteFile(filepath.Join(first.Feed, "info", "grafts"), []byte(lost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	replaced, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("Fetch after the foreign feed: %v", err)
	}
	if !replaced.Cloned {
		t.Fatal("Fetch kept a feed with grafts, want it replaced")
	}
	runSetup(t, working.Clone, "git", "-C", working.Clone, "fsck", "--connectivity-only", "--no-dangling")
	if _, err := Provision(context.Background(), run, request); err != nil {
		t.Fatalf("provision after the feed replacement: %v", err)
	}
}
