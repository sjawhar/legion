package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// A clone made by the old shared-clone path can still exist when the feed is replaced. Provisioning
// must identify the refs that lost their borrowed objects before Jujutsu reports a bare bad object.
func TestProvisionNamesRefsThatLostBorrowedHistory(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Clone), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetup(t, request.StateDir, "git", "clone", "--shared", "--quiet", first.Feed, workspace.Clone)
	runSetup(t, workspace.Clone, "git", "remote", "set-url", "origin", GitHubURL(request.Repo))
	runSetup(t, workspace.Clone, "jj", "git", "init", "--colocate")
	lost := commitOnlyInFeed(t, first.Feed)
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "refs/jj/keep/lost-history", lost)
	replaceAlternate(t, workspace.Clone, run.remote)
	failed := exec.Command("git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "fsck", "--connectivity-only", "--no-dangling")
	if output, err := failed.CombinedOutput(); err == nil {
		t.Fatalf("test setup left the shared clone connected: %s", output)
	}

	_, err = Provision(context.Background(), failedFetchRunner{Runner: run}, request)
	if err == nil {
		t.Fatal("Provision succeeded with refs that lost borrowed history")
	}
	if !strings.Contains(err.Error(), "lost borrowed history") || !strings.Contains(err.Error(), "refs/jj/keep/lost-history") {
		t.Fatalf("Provision = %v, want the lost ref and borrowed history", err)
	}
	if strings.Contains(err.Error(), "bad object") {
		t.Fatalf("Provision = %v, want the named lost ref instead of git's bad object", err)
	}
}

// A clone created before local feed clones replaced alternates is dissociated from the quarantined
// feed by the uncredentialed provision step before that feed is removed.
func TestProvisionDissociatesAnOldSharedCloneAfterFeedQuarantine(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Clone), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetup(t, request.StateDir, "git", "clone", "--shared", "--quiet", first.Feed, workspace.Clone)
	runSetup(t, workspace.Clone, "git", "remote", "set-url", "origin", GitHubURL(request.Repo))
	runSetup(t, workspace.Clone, "jj", "git", "init", "--colocate")
	lost := commitOnlyInFeed(t, first.Feed)
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "refs/jj/keep/lost-history", lost)
	if err := os.WriteFile(filepath.Join(first.Feed, "info", "grafts"), []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), run, fetch); err != nil {
		t.Fatalf("Fetch after the foreign feed: %v", err)
	}

	if _, err := Provision(context.Background(), run, request); err != nil {
		t.Fatalf("Provision after the feed quarantine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Clone, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone alternates after dissociation = %v, want absent", err)
	}
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "fsck", "--connectivity-only", "--no-dangling")
	if _, err := os.Stat(first.Feed + ".legacy"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantined feed after provision = %v, want removed", err)
	}
}

// Moving a symlinked feed owner aside must preserve the legacy alternate path until the
// uncredentialed provisioner repacks the clone.
func TestProvisionDissociatesAnOldSharedCloneAfterSymlinkedFeedOwnerQuarantine(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Clone), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetup(t, request.StateDir, "git", "clone", "--shared", "--quiet", first.Feed, workspace.Clone)
	runSetup(t, workspace.Clone, "git", "remote", "set-url", "origin", GitHubURL(request.Repo))
	runSetup(t, workspace.Clone, "jj", "git", "init", "--colocate")
	lost := commitOnlyInFeed(t, first.Feed)
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "refs/jj/keep/lost-history", lost)
	owner := filepath.Dir(first.Feed)
	target := filepath.Join(t.TempDir(), "owner")
	if err := os.Rename(owner, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), run, fetch); err != nil {
		t.Fatalf("Fetch after the symlinked owner: %v", err)
	}

	if _, err := Provision(context.Background(), run, request); err != nil {
		t.Fatalf("Provision after the symlinked owner quarantine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Clone, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone alternates after dissociation = %v, want absent", err)
	}
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "fsck", "--connectivity-only", "--no-dangling")
	if _, err := os.Lstat(owner + ".legacy"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantined owner after provision = %v, want removed", err)
	}
}

// A previous Jujutsu import can retain a feed-only commit in its operation view after Git drops the
// ref that first introduced it. Provisioning must name that lost operation history too.
func TestProvisionNamesJujutsuOperationHistoryThatLostBorrowedObjects(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Clone), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetup(t, request.StateDir, "git", "clone", "--shared", "--quiet", first.Feed, workspace.Clone)
	runSetup(t, workspace.Clone, "git", "remote", "set-url", "origin", GitHubURL(request.Repo))
	runSetup(t, workspace.Clone, "jj", "git", "init", "--colocate")
	lost := commitOnlyInFeed(t, first.Feed)
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "refs/heads/lost-history", lost)
	runSetup(t, workspace.Clone, "jj", "git", "import", "-R", workspace.Clone)
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "-d", "refs/heads/lost-history")
	runSetup(t, workspace.Clone, "git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "update-ref", "-d", "refs/jj/keep/"+lost)
	if err := os.Remove(filepath.Join(workspace.Clone, ".git", "logs", "refs", "heads", "lost-history")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	replaceAlternate(t, workspace.Clone, run.remote)
	if output, err := exec.Command("git", "--git-dir="+filepath.Join(workspace.Clone, ".git"), "fsck", "--connectivity-only", "--no-dangling").CombinedOutput(); err != nil {
		t.Fatalf("Git reports missing history that only Jujutsu should name: %v\n%s", err, output)
	}
	view := exec.Command("jj", "log", "-r", "visible_heads()", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never", "-R", workspace.Clone)
	if output, err := view.CombinedOutput(); err == nil {
		t.Fatalf("test setup left Jujutsu's operation view readable: %s", output)
	}

	_, err = Provision(context.Background(), failedFetchRunner{Runner: run}, request)
	if err == nil {
		t.Fatal("Provision succeeded with Jujutsu operation history that lost borrowed objects")
	}
	if !strings.Contains(err.Error(), "Jujutsu's operation view") || !strings.Contains(err.Error(), lost) {
		t.Fatalf("Provision = %v, want the lost operation object %s", err, lost)
	}
	if strings.Contains(err.Error(), "bad object") {
		t.Fatalf("Provision = %v, want the named lost operation object instead of git's bad object", err)
	}
}

// failedFetchRunner enters Provision's failed-fetch diagnosis path without changing the damaged
// Jujutsu operation view the test constructed from a real shared clone.
type failedFetchRunner struct{ Runner }

func (r failedFetchRunner) Run(ctx context.Context, command Command) (Result, error) {
	if commandWith(command.Argv, "jj", "git", "fetch") {
		return Result{ExitCode: 1, Stderr: "the provision fetch failed"}, nil
	}
	return r.Runner.Run(ctx, command)
}

func replaceAlternate(t *testing.T, clone, remote string) {
	t.Helper()
	fresh := filepath.Join(t.TempDir(), "replacement.git")
	runSetup(t, t.TempDir(), "git", "clone", "--bare", "--quiet", remote, fresh)
	if err := os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(filepath.Join(fresh, "objects")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitOnlyInFeed(t *testing.T, feed string) string {
	t.Helper()
	tree := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main^{tree}"))
	command := exec.Command("git", "--git-dir="+feed, "commit-tree", tree)
	command.Stdin = strings.NewReader("lost borrowed history\n")
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Legion test",
		"GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test",
		"GIT_COMMITTER_EMAIL=legion-test@example.invalid",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create feed-only commit: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}
