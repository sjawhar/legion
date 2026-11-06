package rescue_test

// What the push check takes as the workspace's own. A pod's shared clone keeps every commit it
// ever fetched visible to jj (workspace-init sets git.abandon-unreachable-commits false), so its
// visible heads hold other trees' handoffs and the pre-rebase copies of branches origin rewrote or
// deleted. What the clone held when it was made is none of the pod's work, and none of it is
// rescued; the workspace's own unpushed commit beside it is, as the one rescue branch of the check.
// What a role made in the pod is its own wherever its working copy is now and whatever its dates
// say: a side line it left, the merge it left behind when its working copy moved back onto the
// issue's bookmark, and a commit git made in the workspace that jj then moved away from.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// visibleHeads is every head jj can see in the clone, as the pod's roles see it.
func visibleHeads(t *testing.T, d *rescuetest.Disk) []string {
	t.Helper()
	return strings.Fields(d.Run(d.Clone, "jj", "log", "--no-graph", "--ignore-working-copy", "-r", "visible_heads()", "-T", `commit_id ++ "\n"`))
}

// foreignBranch pushes another tree's branch to origin, one commit on the base that someone made
// an hour before the pod, and returns the side clone the commit was made in, and the commit. Run
// in one of rescuetest.New's before, it is a branch the pod's clone holds from the start.
func foreignBranch(d *rescuetest.Disk, branch string) (string, string) {
	side := filepath.Join(filepath.Dir(d.Remote), "side-"+branch)
	d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
	d.Write(side, branch+".txt", branch+"\n")
	d.Run(side, "git", "add", branch+".txt")
	d.RunBefore(side, "git", "commit", "--quiet", "-m", "another tree's "+branch)
	d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return side, d.Run(side, "git", "rev-parse", "HEAD")
}

// A commit the clone held when it was made is not rescued, in every state a fetch leaves it in
// once origin no longer has it.
func TestACommitTheCloneHeldWhenItWasMadeIsNotRescued(t *testing.T) {
	for _, tc := range []struct {
		name string
		// after is what happens to the fetched branch on origin, and in the clone.
		after func(t *testing.T, d *rescuetest.Disk, side string)
	}{
		{
			name: "deleted on origin and fetched again",
			after: func(_ *testing.T, d *rescuetest.Disk, _ string) {
				d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
				d.JJ("git", "fetch")
			},
		},
		{
			name: "deleted on origin and not fetched since",
			after: func(_ *testing.T, d *rescuetest.Disk, _ string) {
				d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
			},
		},
		{
			name: "tracked as a local bookmark, then deleted on origin and not fetched since",
			after: func(_ *testing.T, d *rescuetest.Disk, _ string) {
				d.JJ("bookmark", "track", "other", "--remote", "origin")
				d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
			},
		},
		{
			name: "rewritten on origin and fetched again",
			after: func(_ *testing.T, d *rescuetest.Disk, side string) {
				d.RunBefore(side, "git", "commit", "--quiet", "--amend", "-m", "another tree's other, rebased")
				d.Run(side, "git", "push", "--quiet", "--force", "origin", "HEAD:refs/heads/other")
				d.JJ("git", "fetch")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var side, foreign string
			d := rescuetest.New(t, issue, func(d *rescuetest.Disk) { side, foreign = foreignBranch(d, "other") })
			d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
			tc.after(t, d, side)
			if !slices.Contains(visibleHeads(t, d), foreign) {
				t.Fatalf("the fetched commit %s is not a visible head of the clone (%v): the case does not stand", foreign, visibleHeads(t, d))
			}
			if held := d.RemoteBranch("other"); held == foreign {
				t.Fatalf("the remote still holds %s on other: the case does not stand", foreign)
			}

			d.Write(d.Workspace, "feature.txt", "the workspace's own work\n")
			d.JJ("commit", "-m", "the workspace's own work")
			own := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")

			w := only(t, d, d.Check(context.Background(), prefix))
			if w.Status != rescue.Rescued || len(w.Branches) != 1 || w.Branches[0].Commit != own {
				t.Fatalf("the check reported %+v, want one rescue branch, holding the workspace's own %s and not the fetched %s", w, own, foreign)
			}
		})
	}
}

// A workspace whose own work is all pushed is clean, however many commits the clone held when it
// was made that origin has since deleted.
func TestAWorkspaceBesideStaleCommitsTheCloneHeldIsClean(t *testing.T) {
	d := rescuetest.New(t, issue, func(d *rescuetest.Disk) {
		for _, name := range []string{"old-merge", "abandoned-wip"} {
			foreignBranch(d, name)
		}
	})
	d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
	for _, name := range []string{"old-merge", "abandoned-wip"} {
		d.Run(d.Remote, "git", "branch", "--quiet", "-D", name)
	}
	d.JJ("git", "fetch")
	before := d.RemoteBranches()
	if w := only(t, d, d.Check(context.Background(), prefix)); w.Status != rescue.Clean || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want clean: nothing the pod made is unpushed", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// A side line a role made in the pod and left, its working copy moved back onto the issue's
// bookmark since, is on neither the working copy's line nor the bookmark's, and it is rescued: the
// clone did not hold it when it was made.
func TestASideLineMadeInThePodIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("new", "-m", "an experiment off the base", d.Base)
	d.Write(d.Workspace, "experiment.txt", "an experiment\n")
	side := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
	if line := d.JJ("log", "--no-graph", "-r", side+" & ::@", "-T", "commit_id"); line != "" {
		t.Fatalf("the side line %s is on the working copy's line: the case does not stand", side)
	}
	if head := d.Git("rev-parse", "HEAD"); head == side {
		t.Fatalf("the workspace's git HEAD is the side line %s: the case does not stand", side)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), side)
}

// A commit someone else made, which the pod fetched while it ran and origin deleted since, is
// rescued too, however old it is: the clone did not hold it when it was made, so the check cannot
// tell it from the pod's own, and pushing it is noise, never a loss.
func TestACommitThePodFetchedAndOriginDeletedSinceIsRescued(t *testing.T) {
	for name, dated := range map[string]bool{"made while the pod ran": false, "made an hour before the pod": true} {
		t.Run(name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
			side := filepath.Join(filepath.Dir(d.Remote), "side")
			d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
			d.Write(side, "later.txt", "fetched while the pod ran\n")
			d.Run(side, "git", "add", "later.txt")
			if dated {
				d.RunBefore(side, "git", "commit", "--quiet", "-m", "fetched while the pod ran")
			} else {
				d.Run(side, "git", "commit", "--quiet", "-m", "fetched while the pod ran")
			}
			later := d.Run(side, "git", "rev-parse", "HEAD")
			d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/later")
			d.JJ("git", "fetch")
			d.Run(d.Remote, "git", "branch", "--quiet", "-D", "later")
			d.JJ("git", "fetch")
			rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), later)
		})
	}
}

// A clone with no record of what it held when it was made, as a pod an earlier release provisioned
// has, rescues every head and branch origin lacks, the clone's old fetched commits among them:
// noise, and never a loss.
func TestACloneWithNoRecordRescuesEveryHeadOriginLacks(t *testing.T) {
	var foreign string
	d := rescuetest.New(t, issue, func(d *rescuetest.Disk) { _, foreign = foreignBranch(d, "other") })
	d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
	if err := os.Remove(filepath.Join(d.Clone, ".jj", "legion-held")); err != nil {
		t.Fatal(err)
	}
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
	d.JJ("git", "fetch")
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), foreign)
}

// recreate creates the issue's workspace again in the live pod, in the order provisioning does
// (workspace.Remove, then workspace.createWorkspace): the workspace forgotten and its directory
// gone; then, with setAside, the issue's bookmark deleted; the workspace added again at revision,
// with its creation recorded anew; and, with setAside, the bookmark set on its working copy. The
// record is dated two seconds on, so everything made before the re-creation was made before the
// record at any clock's resolution.
func recreate(t *testing.T, d *rescuetest.Disk, revision string, setAside bool) {
	t.Helper()
	name := strings.ToLower(issue)
	d.Run(d.Clone, "jj", "workspace", "forget", name)
	if err := os.RemoveAll(d.Workspace); err != nil {
		t.Fatal(err)
	}
	if setAside {
		d.Run(d.Clone, "jj", "bookmark", "delete", `exact:"`+d.Branch+`"`)
	}
	d.Run(d.Clone, "jj", "workspace", "add", "--name", name, "--revision", revision, d.Workspace)
	if err := workspace.RecordCreated(d.Workspace, time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if setAside {
		d.JJ("bookmark", "set", d.Branch, "-r", "@")
	}
}

// offBothLines fails the test when commit is on the workspace's working-copy line or the issue's
// bookmark's: the case stands only where the commit is made in the pod and on neither.
func offBothLines(t *testing.T, d *rescuetest.Disk, commit string) {
	t.Helper()
	if on := d.JJ("log", "--no-graph", "-r", commit+` & ::(@ | bookmarks(exact:"`+d.Branch+`"))`, "-T", "commit_id"); on != "" {
		t.Fatalf("%s is on the workspace's own line: the case does not stand", commit)
	}
	if !slices.Contains(visibleHeads(t, d), commit) {
		t.Fatalf("%s is not a visible head of the clone (%v): the case does not stand", commit, visibleHeads(t, d))
	}
}

// Work a role made in the pod before its workspace was created again is still the pod's: the
// re-creation writes a new creation record in the workspace's .jj, which the launcher's resume
// check reads, and leaves the clone's record of what it held when it was made, which the push
// check reads, as it was.
// Plain: an unbookmarked commit, the working copy moved onto the issue's bookmark, then the
// workspace created again there. Set aside: the issue's bookmark moved onto a commit nobody
// described after its last push and GitHub deleted the branch, so the re-creation deletes the
// bookmark and starts at main, logging that the commit stays visible in the shared clone,
// recoverable by id; it is rescued, as that line promises.
func TestWorkMadeBeforeTheWorkspaceWasCreatedAgainIsRescued(t *testing.T) {
	t.Run("created again on the issue's bookmark", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		d.JJ("new", "-m", "work before the re-creation", d.Base)
		d.Write(d.Workspace, "early.txt", "work before the re-creation\n")
		early := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
		d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
		recreate(t, d, `bookmarks(exact:"`+d.Branch+`")`, false)
		offBothLines(t, d, early)
		rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), early)
	})
	t.Run("created again at main, the issue's moved bookmark set aside", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
		d.Write(d.Workspace, "pushed.txt", "pushed work\n")
		d.JJ("commit", "-m", "pushed work")
		d.JJ("bookmark", "set", d.Branch, "-r", "@-")
		d.JJ("git", "push", "--bookmark", d.Branch)
		d.Write(d.Workspace, "undescribed.txt", "work nobody described\n")
		d.JJ("bookmark", "set", d.Branch, "-r", "@")
		moved := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
		d.JJ("new", "main")
		d.Run(d.Remote, "git", "branch", "--quiet", "-D", d.Branch)
		d.JJ("git", "fetch")
		if listed := d.JJ("bookmark", "list", "--conflicted", "-T", `name ++ "\n"`); !strings.Contains(listed, d.Branch) {
			t.Fatalf("the conflicted bookmarks are %q, want %s with one side a deletion: the case does not stand", listed, d.Branch)
		}
		recreate(t, d, "main", true)
		offBothLines(t, d, moved)
		rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), moved)
	})
}

// A workspace provisioning started at the default branch has the issue's bookmark on its working
// copy, an empty commit with no description, which jj exports to the issue's git branch. That
// commit holds no work, on git's side as on jj's, and the workspace is clean.
func TestTheIssuesBranchOnAnEmptyWorkingCopyIsClean(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("new", "main")
	d.JJ("bookmark", "set", d.Branch, "-r", "@")
	if at, branch := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id"), d.Run(d.Clone, "git", "rev-parse", "refs/heads/"+d.Branch); at != branch {
		t.Fatalf("the issue's git branch is %s, want the working copy %s: the case does not stand", branch, at)
	}
	if w := only(t, d, d.Check(context.Background(), prefix)); w.Status != rescue.Clean || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want clean: an empty working copy holds no work", w)
	}
}

// scratchWorktree is a git worktree of the clone at the base, detached, which the test removes
// again before the check: the branch git writes in it is then named by no worktree's HEAD.
func scratchWorktree(t *testing.T, d *rescuetest.Disk) string {
	t.Helper()
	scratch := filepath.Join(filepath.Dir(d.Remote), "scratch")
	d.Run(d.Clone, "git", "worktree", "add", "--quiet", "--detach", scratch, d.Base)
	return scratch
}

// A branch git rewrote in the pod keeping its commits' author dates as their committer dates
// (`git rebase` and `git am` with --committer-date-is-author-date) holds a commit made in the pod
// whose committer time is older than the pod. The clone did not hold it when it was made, and the
// branch is rescued.
func TestABranchGitRewroteInThePodKeepingOldDatesIsRescued(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write makes the branch in the scratch worktree and returns its name.
		write func(t *testing.T, d *rescuetest.Disk, scratch, authored string) string
	}{
		{
			name: "git rebase --committer-date-is-author-date",
			write: func(_ *testing.T, d *rescuetest.Disk, scratch, authored string) string {
				d.Run(scratch, "git", "switch", "--quiet", "-c", "topic")
				d.Write(scratch, "topic.txt", "topic work\n")
				d.Run(scratch, "git", "add", "topic.txt")
				d.Run(scratch, "git", "commit", "--quiet", "--date="+authored, "-m", "topic work")
				d.Run(scratch, "git", "rebase", "--quiet", "--force-rebase", "--committer-date-is-author-date", d.Base)
				return "topic"
			},
		},
		{
			name: "git am --committer-date-is-author-date",
			write: func(t *testing.T, d *rescuetest.Disk, scratch, authored string) string {
				d.Write(scratch, "patched.txt", "a patch's work\n")
				d.Run(scratch, "git", "add", "patched.txt")
				d.Run(scratch, "git", "commit", "--quiet", "--date="+authored, "-m", "a patch's work")
				patch := filepath.Join(t.TempDir(), "work.patch")
				d.Write(filepath.Dir(patch), filepath.Base(patch), d.Run(scratch, "git", "format-patch", "--stdout", "-1", "HEAD")+"\n")
				d.Run(scratch, "git", "switch", "--quiet", "--detach", d.Base)
				d.Run(scratch, "git", "switch", "--quiet", "-c", "applied")
				d.Run(scratch, "git", "am", "--quiet", "--committer-date-is-author-date", patch)
				return "applied"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			scratch := scratchWorktree(t, d)
			authored := d.Provisioned.Add(-time.Hour).Format(time.RFC3339)
			branch := tc.write(t, d, scratch, authored)
			d.Run(d.Clone, "git", "worktree", "remove", "--force", scratch)
			tip := d.Run(d.Clone, "git", "rev-parse", "refs/heads/"+branch)
			committed, err := strconv.ParseInt(d.Run(d.Clone, "git", "log", "-1", "--format=%ct", tip), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if committed >= d.Provisioned.Add(-time.Minute).Unix() {
				t.Fatalf("%s's tip %s was committed at %d, want an hour before the pod: the case does not stand", branch, tip, committed)
			}
			rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), tip)
		})
	}
}

// A local branch with no reflog — none kept, as with core.logAllRefUpdates off — at a commit dated
// before the pod is rescued: the clone did not hold the commit when it was made.
func TestABranchWithNoReflogIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.RunBefore(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "work on a branch with no reflog")
	d.Run(d.Clone, "git", "-c", "core.logAllRefUpdates=false", "branch", "unlogged", commit)
	if _, err := os.Stat(filepath.Join(d.Clone, ".git", "logs", "refs", "heads", "unlogged")); !os.IsNotExist(err) {
		t.Fatalf("the branch has a reflog (%v): the case does not stand", err)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// A commit git made in the workspace itself, whose git HEAD is detached so no branch names it, with
// a committer time older than the pod — `git am` or `git rebase` with
// --committer-date-is-author-date, or GIT_COMMITTER_DATE — and jj's own commit under JJ_TIMESTAMP,
// is a visible head and nothing more once jj moves the working copy onto the issue's bookmark or
// onto main. The clone did not hold it when it was made, and it is rescued whatever its dates say.
func TestAnOldDatedCommitMadeInTheWorkspaceIsRescuedOnceTheWorkingCopyMoves(t *testing.T) {
	for _, made := range []struct {
		name string
		// write makes the commit in the workspace and returns it.
		write func(t *testing.T, d *rescuetest.Disk, authored string) string
	}{
		{
			name: "git am --committer-date-is-author-date",
			write: func(t *testing.T, d *rescuetest.Disk, authored string) string {
				side := filepath.Join(filepath.Dir(d.Remote), "patch-source")
				d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
				d.Write(side, "patched.txt", "a patch's work\n")
				d.Run(side, "git", "add", "patched.txt")
				d.Run(side, "git", "commit", "--quiet", "--date="+authored, "-m", "a patch's work")
				patch := filepath.Join(t.TempDir(), "work.patch")
				d.Write(filepath.Dir(patch), filepath.Base(patch), d.Run(side, "git", "format-patch", "--stdout", "-1", "HEAD")+"\n")
				d.Git("am", "--quiet", "--committer-date-is-author-date", patch)
				return d.Git("rev-parse", "HEAD")
			},
		},
		{
			name: "git rebase --force-rebase --committer-date-is-author-date",
			write: func(_ *testing.T, d *rescuetest.Disk, authored string) string {
				d.Write(d.Workspace, "rebased.txt", "rebased work\n")
				d.Git("add", "rebased.txt")
				d.Git("commit", "--quiet", "--date="+authored, "-m", "rebased work")
				d.Git("rebase", "--quiet", "--force-rebase", "--committer-date-is-author-date", d.Base)
				return d.Git("rev-parse", "HEAD")
			},
		},
		{
			name: "git commit under GIT_COMMITTER_DATE",
			write: func(_ *testing.T, d *rescuetest.Disk, _ string) string {
				d.Write(d.Workspace, "backdated.txt", "backdated work\n")
				d.Git("add", "backdated.txt")
				d.RunBefore(d.Workspace, "git", "commit", "--quiet", "-m", "backdated work")
				return d.Git("rev-parse", "HEAD")
			},
		},
		{
			name: "jj commit under JJ_TIMESTAMP",
			write: func(_ *testing.T, d *rescuetest.Disk, _ string) string {
				d.Write(d.Workspace, "stamped.txt", "work under an old timestamp\n")
				d.RunBefore(d.Workspace, "jj", "commit", "-m", "work under an old timestamp")
				return d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
			},
		},
	} {
		for _, moved := range []struct{ name, onto string }{
			{"then jj new on the issue's bookmark", `bookmarks(exact:"` + "legion/" + issue + `")`},
			{"then jj new on main", "main"},
		} {
			t.Run(made.name+", "+moved.name, func(t *testing.T) {
				d := rescuetest.New(t, issue)
				authored := d.Provisioned.Add(-time.Hour).Format(time.RFC3339)
				commit := made.write(t, d, authored)
				d.JJ("new", moved.onto)
				committed, err := strconv.ParseInt(d.Run(d.Clone, "git", "log", "-1", "--format=%ct", commit), 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if committed >= d.Provisioned.Add(-time.Minute).Unix() {
					t.Fatalf("%s was committed at %d, want an hour before the pod: the case does not stand", commit, committed)
				}
				if branches := d.Run(d.Clone, "git", "for-each-ref", "--contains", commit, "--format=%(refname)", "refs/heads"); branches != "" {
					t.Fatalf("%s is on the local branches %q: the case does not stand", commit, branches)
				}
				offBothLines(t, d, commit)
				rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
			})
		}
	}
}

// A branch jj exports, which git then moved onto a commit dated two months back, keeps only jj's
// export entry once a `git reflog expire` (which `git gc` runs) drops git's own, older entry. The
// clone did not hold the commit when it was made, and the branch is rescued.
func TestABranchWhoseReflogExpiredPastGitsWriteIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("bookmark", "create", "topic", "-r", d.Base)
	old := d.Provisioned.AddDate(0, -2, 0).Format(time.RFC3339)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "env", "GIT_AUTHOR_DATE="+old, "GIT_COMMITTER_DATE="+old, "git", "commit-tree", tree, "-p", d.Base, "-m", "work two months old")
	d.Run(d.Clone, "env", "GIT_COMMITTER_DATE="+old, "git", "update-ref", "-m", "commit: work two months old", "refs/heads/topic", commit)
	d.Run(d.Clone, "git", "reflog", "expire", "--expire=30.days.ago", "--expire-unreachable=30.days.ago", "refs/heads/topic")
	d.JJ("log", "--no-graph", "-r", "topic", "-T", "commit_id")
	if entries := d.Run(d.Clone, "git", "log", "--walk-reflogs", "--format=%gs", "refs/heads/topic", "--"); entries != "export from jj" {
		t.Fatalf("topic's reflog holds %q, want jj's export alone: the case does not stand", entries)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), commit)
}

// A graft (`git replace --graft F W`) makes a commit F that is not the pod's look as though the
// pod's work W were its parent: git, reading the replacement, takes W as held wherever F is. The
// check reads no replacement object, and W is rescued, whether F is on origin now or only in the
// clone's record of what it held when it was made, origin having deleted it since.
func TestWorkAGraftMakesLookHeldIsRescued(t *testing.T) {
	for name, deleted := range map[string]bool{"F on origin": false, "F only in the clone's record": true} {
		t.Run(name, func(t *testing.T) {
			var foreign string
			d := rescuetest.New(t, issue, func(d *rescuetest.Disk) { _, foreign = foreignBranch(d, "other") })
			d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
			if deleted {
				d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
				d.JJ("git", "fetch")
			}
			d.JJ("new", "-m", "work a graft hides", d.Base)
			d.Write(d.Workspace, "grafted.txt", "work a graft hides\n")
			work := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
			d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
			offBothLines(t, d, work)
			d.Run(d.Clone, "git", "replace", "--graft", foreign, work)
			if parent := d.Run(d.Clone, "git", "rev-parse", foreign+"^"); parent != work {
				t.Fatalf("git reads %s's parent as %s, want the grafted %s: the case does not stand", foreign, parent, work)
			}
			if held := d.RemoteBranch("other"); (held == foreign) == deleted {
				t.Fatalf("the remote holds other at %q: the case does not stand", held)
			}
			rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix)), work)
		})
	}
}
