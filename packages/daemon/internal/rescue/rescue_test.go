package rescue_test

// The push check against real git and jj repositories: a bare remote standing in for GitHub, and a
// pod's disk holding a jj clone of it with the issue's workspace (rescuetest). Each case puts the
// workspace in one state and reads what the check reports and what the remote then holds.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const (
	issue  = "WIDGETS-42"
	prefix = "legion/WIDGETS-42-rescue-9d3f0c7a"
)

// only is the check's one entry, which every case's disk holds one workspace for.
func only(t *testing.T, d *rescuetest.Disk, got []shimwire.RescueWorkspace) shimwire.RescueWorkspace {
	t.Helper()
	if len(got) != 1 || got[0].Dir != d.Workspace {
		t.Fatalf("the check reported %+v, want one entry for %s", got, d.Workspace)
	}
	return got[0]
}

// rescuedTo asserts the entry pushed exactly one branch, prefix-1, and that the remote holds want
// there; it returns that commit.
func rescuedTo(t *testing.T, d *rescuetest.Disk, w shimwire.RescueWorkspace, want string) {
	t.Helper()
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch", w)
	}
	b := w.Branches[0]
	if b.Name != prefix+"-1" || b.Commit != want {
		t.Errorf("the check pushed %s at %s, want %s-1 at %s", b.Name, b.Commit, prefix, want)
	}
	if held := d.RemoteBranch(b.Name); held != want {
		t.Errorf("the remote holds %s at %q, want %s", b.Name, held, want)
	}
}

// opCount is how many operations jj's log holds, which a check that wrote nothing leaves alone.
func opCount(t *testing.T, d *rescuetest.Disk) int {
	t.Helper()
	return len(strings.Fields(d.Run(d.Clone, "jj", "op", "log", "--no-graph", "--ignore-working-copy", "--at-op=@", "-T", `id.short() ++ "\n"`)))
}

// A workspace that holds nothing its remote lacks — jj's empty, undescribed working copy on the
// pushed commit — is clean, and nothing is pushed.
func TestACleanWorkspaceIsCleanAndNothingIsPushed(t *testing.T) {
	d := rescuetest.New(t, issue)
	before := d.RemoteBranches()
	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Clean || len(w.Branches) != 0 || w.Error != "" {
		t.Fatalf("the check reported %+v, want clean", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// A commit made with plain git in the workspace moves the workspace's git HEAD before jj reads it;
// no remote ref holds it, so it is rescued as it stands.
func TestAnUnpushedGitCommitOnTheWorkspaceHeadIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "fix.txt", "a fix made with plain git\n")
	d.Git("add", "fix.txt")
	d.Git("commit", "--quiet", "-m", "a fix made with plain git")
	commit := d.Git("rev-parse", "HEAD")
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// A commit on the issue's own branch that no remote ref holds is rescued, though the workspace's
// HEAD does not name it: the branch is the issue's, whichever worktree last worked on it.
func TestAnUnpushedCommitOnTheIssuesOwnBranchIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "work on the issue's branch")
	d.Run(d.Clone, "git", "branch", "--force", d.Branch, commit)
	if head := d.Git("rev-parse", "HEAD"); head != d.Base {
		t.Fatalf("the workspace's git HEAD is %s, want %s: only the issue's branch may name the commit", head, d.Base)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// A commit a role made in the pod on a local git branch of another name than the issue's is
// rescued, though neither the workspace's HEAD nor jj's own line names it.
func TestAnUnpushedCommitOnALocalGitBranchIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "work on a local branch")
	d.Run(d.Clone, "git", "branch", "topic", commit)
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// Files written in the workspace and in no commit become one commit on top of the workspace's
// head, built in an index of its own: the workspace's files, its index, its HEAD and jj's
// operation log are what they were.
func TestADirtyTreeIsRescuedAsOneCommitWithoutChangingTheWorkspace(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "base.txt", "base, edited\n")
	head := d.Git("rev-parse", "HEAD")
	index := d.Git("rev-parse", "--path-format=absolute", "--git-path", "index")
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	ops := opCount(t, d)

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch", w)
	}
	commit := w.Branches[0].Commit
	if parent := d.Run(d.Remote, "git", "rev-parse", commit+"^"); parent != head {
		t.Errorf("the rescued commit's parent is %s, want the workspace's head %s", parent, head)
	}
	for file, want := range map[string]string{"notes/draft.txt": "an edit in no commit", "base.txt": "base, edited"} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
	if d.RemoteBranch(w.Branches[0].Name) != commit {
		t.Errorf("the remote does not hold %s at %s", w.Branches[0].Name, commit)
	}
	if got := d.Git("rev-parse", "HEAD"); got != head {
		t.Errorf("the workspace's HEAD is %s after the check, want %s", got, head)
	}
	if indexAfter, err := os.ReadFile(index); err != nil || string(indexAfter) != string(indexBefore) {
		t.Errorf("the workspace's index changed under the check (%v)", err)
	}
	if body, err := os.ReadFile(filepath.Join(d.Workspace, "notes", "draft.txt")); err != nil || string(body) != "an edit in no commit\n" {
		t.Errorf("the workspace's file is %q (%v) after the check, want it as written", body, err)
	}
	if got := opCount(t, d); got != ops {
		t.Errorf("jj's operation log holds %d operations after the check, want %d", got, ops)
	}
}

// A commit only jj names — the working copy described with the work in it, so the workspace's git
// HEAD is still the pushed commit under it — is rescued as jj holds it, on the workspace's own line.
func TestAJJOnlyCommitIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work jj alone holds\n")
	d.JJ("describe", "-m", "work jj alone holds")
	commit := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	if head := d.Git("rev-parse", "HEAD"); head != d.Base {
		t.Fatalf("the workspace's git HEAD is %s, want %s: only jj may name the commit", head, d.Base)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// A merge whose bookmark was never moved onto it: the worker's push of the bookmark says "Nothing
// changed", because the bookmark still names the commit the remote has, and the merge is on the
// disk alone. It is empty — both of its parents are pushed and it resolves nothing — but it is
// described, and it is rescued, whatever the bookmarks say: with the working copy still on top of
// it, and with the working copy moved back onto the bookmark since, where it is on neither line and
// is the pod's own because the pod made it.
func TestAMergeWhoseBookmarkWasNotMovedOntoItIsRescued(t *testing.T) {
	for name, moveOn := range map[string]bool{"the working copy on top of it": false, "the working copy moved back onto the bookmark": true} {
		t.Run(name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			side := filepath.Join(filepath.Dir(d.Remote), "side")
			d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
			d.Write(side, "side.txt", "side\n")
			d.Run(side, "git", "add", "side.txt")
			d.Run(side, "git", "commit", "--quiet", "-m", "side")
			d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/side")
			d.JJ("git", "fetch")
			d.JJ("new", "-m", "merge side into the issue", `bookmarks(exact:"`+d.Branch+`")`, `remote_bookmarks(exact:"side")`)
			merge := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
			d.JJ("new")
			if pushed := d.JJ("git", "push", "--bookmark", d.Branch); !strings.Contains(pushed, "Nothing changed") {
				t.Fatalf("the worker's push said %q, want the \"Nothing changed\" a bookmark left behind its merge gives", pushed)
			}
			if held := d.RemoteBranch(d.Branch); held != d.Base {
				t.Fatalf("the remote's %s is %s, want it still at %s", d.Branch, held, d.Base)
			}
			if moveOn {
				d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
				if head := d.Git("rev-parse", "HEAD"); head != d.Base {
					t.Fatalf("the workspace's git HEAD is %s, want %s: only jj may name the merge", head, d.Base)
				}
			}
			rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), merge)
		})
	}
}

// A commit the remote already holds, under a ref the clone never fetched (GitHub keeps a pull
// request's head after its branch is deleted), is no lost work: the check reads the remote itself.
func TestACommitTheRemoteHoldsUnderARefTheCloneNeverFetchedIsClean(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "merged.txt", "squash-merged work\n")
	d.JJ("commit", "-m", "squash-merged work")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.Run(d.Clone, "git", "push", "--quiet", "origin", commit+":refs/pull/7/head")
	if w := only(t, d, d.Check(context.Background(), prefix)); w.Status != rescue.Clean {
		t.Fatalf("the check reported %+v, want clean: the remote holds %s under refs/pull/7/head", w, commit)
	}
}

// A rescue branch takes the least number the remote has no branch for, so a second check of the
// same pod's work never overwrites the first one's branch, and the branch the first one pushed is
// reported again: the round whose replacement did not happen then must still name it when the pod
// goes.
func TestARescueBranchSkipsANumberTheRemoteHoldsAndReportsIt(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Run(d.Clone, "git", "push", "--quiet", "origin", d.Base+":refs/heads/"+prefix+"-1")
	d.Write(d.Workspace, "feature.txt", "more work\n")
	d.JJ("commit", "-m", "more work")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	w := only(t, d, d.Check(context.Background(), prefix))
	want := []shimwire.RescueBranch{{Name: prefix + "-1", Commit: d.Base}, {Name: prefix + "-2", Commit: commit}}
	if w.Status != rescue.Rescued || !slices.Equal(w.Branches, want) {
		t.Fatalf("the check reported %+v, want %+v", w, want)
	}
	if held := d.RemoteBranch(prefix + "-1"); held != d.Base {
		t.Errorf("the remote's %s-1 is %s, want it left at %s", prefix, held, d.Base)
	}
	again := only(t, d, d.Check(context.Background(), prefix))
	if again.Status != rescue.Rescued || !slices.Equal(again.Branches, want) {
		t.Fatalf("a second check reported %+v, want the two branches again and nothing new pushed: %+v", again, want)
	}
}

// A commit of the issue's own branch that only a stale remote-tracking ref names — the branch
// deleted on the remote since the pod last fetched — is a commit the remote may no longer hold,
// and it is rescued.
func TestAnOwnCommitOnlyAStaleRemoteTrackingRefNamesIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "pushed once, then deleted on the remote")
	d.Run(d.Clone, "git", "branch", "--force", d.Branch, commit)
	d.Run(d.Clone, "git", "push", "--quiet", "origin", d.Branch)
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", d.Branch)
	if tracked := d.Run(d.Clone, "git", "rev-parse", "refs/remotes/origin/"+d.Branch); tracked != commit {
		t.Fatalf("the clone's origin/%s is %s, want the stale %s", d.Branch, tracked, commit)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// Work a role set aside in git's stash, with the workspace's files put back as they were, is
// rescued: the stash names a commit no branch, no HEAD and no jj head does.
func TestStashedWorkIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "base.txt", "an edit set aside\n")
	d.Git("stash", "push", "--quiet", "-m", "set aside")
	stash := d.Git("rev-parse", "refs/stash")
	if body, err := os.ReadFile(filepath.Join(d.Workspace, "base.txt")); err != nil || string(body) != "base\n" {
		t.Fatalf("the workspace's base.txt is %q (%v) after the stash, want it put back", body, err)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), stash)
}

// A commit only a local tag names is rescued; a tag the remote holds is not work.
func TestACommitOnlyALocalTagNamesIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "tagged and never pushed")
	d.Run(d.Clone, "git", "tag", "-a", "-m", "a tag of the role's", "draft", commit)
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// Another git worktree of the clone — one a role added beside its workspace, on a detached HEAD —
// is checked with the workspace: its commit and its files in no commit are rescued, as one commit
// on top of its HEAD.
func TestAnotherWorktreeOfTheCloneIsChecked(t *testing.T) {
	d := rescuetest.New(t, issue)
	other := filepath.Join(d.StateDir, "scratch", "review")
	d.Run(d.Clone, "git", "worktree", "add", "--quiet", "--detach", other, d.Base)
	d.Write(other, "review.txt", "a review in progress\n")
	d.Run(other, "git", "add", "review.txt")
	d.Run(other, "git", "commit", "--quiet", "-m", "a review in progress")
	commit := d.Run(other, "git", "rev-parse", "HEAD")
	d.Write(other, "notes.txt", "notes in no commit\n")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch", w)
	}
	rescued := w.Branches[0].Commit
	if parent := d.Run(d.Remote, "git", "rev-parse", rescued+"^"); parent != commit {
		t.Errorf("the rescued commit's parent is %s, want the other worktree's commit %s", parent, commit)
	}
	if body := d.Run(d.Remote, "git", "show", rescued+":notes.txt"); body != "notes in no commit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the other worktree's file", body)
	}
}

// The clone's own working tree is left out of the walk. An issue pod's clone holds no working
// tree at all (workspace.feedClone), while its git HEAD and its index still name every file of
// the default branch, so a check that took the clone's directory would read the whole tree as
// deleted and push that deletion as a rescue branch. The clone is no agent's workspace, and
// nothing of it is recorded or pushed.
func TestTheClonesOwnDirectoryIsNotChecked(t *testing.T) {
	d := rescuetest.NewWithoutCloneTree(t, issue)
	if files := d.Run(d.Clone, "git", "ls-files"); files != "base.txt" {
		t.Fatalf("the clone's index names %q, want base.txt: the case stands on a clone whose HEAD and index name a file it holds no copy of", files)
	}
	before := d.RemoteBranches()
	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Clean || len(w.Branches) != 0 || w.Error != "" {
		for _, b := range w.Branches {
			t.Errorf("the check pushed %s at %s, whose tree is %s, against the clone's HEAD tree %s",
				b.Name, b.Commit, d.Run(d.Remote, "git", "rev-parse", b.Commit+"^{tree}"),
				d.Run(d.Clone, "git", "rev-parse", "HEAD^{tree}"))
		}
		t.Fatalf("the check reported %+v, want clean: the clone's absent working tree is no agent's work", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// Leaving the clone out takes no work with it: a workspace on such a disk that holds a commit the
// remote lacks is rescued, as it is on a disk whose clone has a working tree.
func TestAWorkspaceOfACloneWithNoWorkingTreeIsStillRescued(t *testing.T) {
	d := rescuetest.NewWithoutCloneTree(t, issue)
	d.Write(d.Workspace, "fix.txt", "a fix the remote lacks\n")
	d.JJ("commit", "-m", "a fix the remote lacks")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
	if body := d.Run(d.Remote, "git", "show", commit+":fix.txt"); body != "a fix the remote lacks" {
		t.Errorf("the rescued commit's fix.txt holds %q, want the workspace's file", body)
	}
}

// A directory under workspaces/ that is no git worktree is an error, never a pass: whatever it
// holds, nothing can push it.
func TestADirectoryThatIsNoGitWorktreeIsAnError(t *testing.T) {
	d := rescuetest.New(t, issue)
	stray := filepath.Join(filepath.Dir(d.Workspace), "stray")
	d.Write(stray, "file.txt", "a file in no repository\n")
	got := d.Check(context.Background(), prefix)
	if len(got) != 2 || got[0].Dir != stray || got[1].Dir != d.Workspace {
		t.Fatalf("the check reported %+v, want the stray directory, then the workspace", got)
	}
	if got[0].Status != rescue.Failed || !strings.Contains(got[0].Error, "is not a git worktree") {
		t.Errorf("the stray directory's entry is %+v, want an error saying it is no git worktree", got[0])
	}
	if got[1].Status != rescue.Clean {
		t.Errorf("the workspace's entry is %+v, want clean", got[1])
	}
}

// A push the remote refuses is an error, never a pass, and leaves no branch behind.
func TestAPushTheRemoteRefusesIsAnError(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work the remote will not take\n")
	d.JJ("commit", "-m", "work the remote will not take")
	d.RefuseWrites()
	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || !strings.Contains(w.Error, "the remote refuses this push") || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want an error naming the remote's refusal", w)
	}
	if held := d.RemoteBranch(prefix + "-1"); held != "" {
		t.Errorf("the remote holds %s-1 at %s, want no such branch", prefix, held)
	}
}

// The tests' environment lets git use no transport but local files, whatever the repository's own
// configuration allows: a clone whose origin is a loopback HTTP server, and whose configuration
// allows http itself, is an error before the server sees one request.
func TestTheTestsEnvironmentReachesNoNetworkRemote(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work that must not leave the test\n")
	d.JJ("commit", "-m", "work that must not leave the test")
	d.Run(d.Clone, "git", "remote", "set-url", "origin", server.URL+"/owner/repo.git")
	d.Run(d.Clone, "git", "config", "protocol.http.allow", "always")
	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || !strings.Contains(w.Error, "transport 'http' not allowed") {
		t.Errorf("the check reported %+v, want git's refusal of the http transport", w)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the loopback server received %d requests, want none", n)
	}
}
