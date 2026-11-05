package daemon

// The push check before a round's replacement: the fake pod runtime answers RescueUnpushed by
// running the real check (internal/rescue) over a pod's disk built from real git and jj
// repositories (rescuetest), so each case puts the issue's workspace in one state and reads what
// the sweep then did — whether the pod was replaced, what the remote holds, what the issue and
// the controller were told.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// podUpdateRescueBranch is the prefix of every rescue branch of the rig's pod.
var podUpdateRescueBranch = "legion/" + podUpdateIssue + "-rescue-" + podUpdatePod[:8]

// rescueOn points the rig's runtime at d: every push check runs the real one over d's disk, as
// the shim of the claim it is asked through would run it in the pod.
func (r *podUpdateRig) rescueOn(d *rescuetest.Disk) {
	r.rt.RescueWith(func(ctx context.Context, _ runtime.Known, request runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		return rescue.Report(rescue.Run(ctx, rescue.Request{
			StateDir: d.StateDir, Branch: request.Branch, Env: d.Env,
			WorkspaceTimeout: request.WorkspaceTimeout, Deadline: time.Now().Add(request.Timeout),
		})), nil
	})
}

// answeredRound opens the round over the rig's pod and has every role of roles answer it.
func (r *podUpdateRig) answeredRound(roles ...claim.Role) {
	r.t.Helper()
	r.drifts(podUpdateDrift)
	r.tick()
	for _, role := range roles {
		r.awaitRound(role, podUpdateRound)
		r.signal(role, "")
	}
}

// awaitRescues sweeps until the runtime has been asked for n push checks.
func (r *podUpdateRig) awaitRescues(n int, what string) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%d push check(s) %s", n, what), func() bool {
		if len(r.rt.CallsOf("RescueUnpushed")) >= n {
			return true
		}
		r.tick()
		return len(r.rt.CallsOf("RescueUnpushed")) >= n
	})
}

// Each state of the issue's workspace, from a pod with nothing to lose to one whose only copy of
// a commit is jj's view: the pod is replaced once the check has carried what it found off the
// disk, the remote holds each rescue branch, and the issue is told each branch it must bring in.
// The check is asked through the implementer, the role that acts as the implement App, though the
// architect's claim comes first in the group.
func TestPodUpdatePushesWhatTheDiskHoldsBeforeItReplacesThePod(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before, when set, pushes to the remote before the pod's clone is made, so the clone holds
		// what it pushed from the start (rescuetest.New).
		before func(d *rescuetest.Disk)
		// leave puts the workspace in its state and returns the commit the rescue branch must
		// hold, "" when nothing is to be rescued, and a check of that commit's shape.
		leave func(t *testing.T, d *rescuetest.Disk) (string, func(commit string))
	}{
		{
			name: "clean",
			leave: func(*testing.T, *rescuetest.Disk) (string, func(string)) {
				return "", nil
			},
		},
		{
			name: "an unpushed git commit",
			leave: func(_ *testing.T, d *rescuetest.Disk) (string, func(string)) {
				d.Write(d.Workspace, "fix.txt", "a fix made with plain git\n")
				d.Git("add", "fix.txt")
				d.Git("commit", "--quiet", "-m", "a fix made with plain git")
				return d.Git("rev-parse", "HEAD"), nil
			},
		},
		{
			name: "a dirty tree",
			leave: func(t *testing.T, d *rescuetest.Disk) (string, func(string)) {
				d.Write(d.Workspace, "draft.txt", "an edit in no commit\n")
				head := d.Git("rev-parse", "HEAD")
				return "a new commit", func(commit string) {
					if parent := d.Run(d.Remote, "git", "rev-parse", commit+"^"); parent != head {
						t.Errorf("the rescued commit's parent is %s, want the workspace's head %s", parent, head)
					}
					if body := d.Run(d.Remote, "git", "show", commit+":draft.txt"); body != "an edit in no commit" {
						t.Errorf("the rescued commit's draft.txt holds %q, want the edit", body)
					}
				}
			},
		},
		{
			name: "a jj-only commit",
			leave: func(_ *testing.T, d *rescuetest.Disk) (string, func(string)) {
				d.Write(d.Workspace, "feature.txt", "work jj alone holds\n")
				d.JJ("describe", "-m", "work jj alone holds")
				return d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id"), nil
			},
		},
		{
			name: "a merge whose bookmark was not moved onto it",
			leave: func(t *testing.T, d *rescuetest.Disk) (string, func(string)) {
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
					t.Fatalf("the worker's push said %q, want \"Nothing changed\"", pushed)
				}
				return merge, nil
			},
		},
		{
			name: "an unpushed commit beside commits the clone held when it was made",
			before: func(d *rescuetest.Disk) {
				side := filepath.Join(filepath.Dir(d.Remote), "side")
				d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
				for _, name := range []string{"old-merge", "abandoned-wip"} {
					d.Write(side, name+".txt", name+"\n")
					d.Run(side, "git", "add", name+".txt")
					d.RunBefore(side, "git", "commit", "--quiet", "-m", name)
					d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+name)
				}
			},
			leave: func(_ *testing.T, d *rescuetest.Disk) (string, func(string)) {
				d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
				for _, name := range []string{"old-merge", "abandoned-wip"} {
					d.Run(d.Remote, "git", "branch", "--quiet", "-D", name)
				}
				d.JJ("git", "fetch")
				d.Write(d.Workspace, "feature.txt", "the workspace's own work\n")
				d.JJ("commit", "-m", "the workspace's own work")
				return d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id"), nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []func(*rescuetest.Disk)
			if tc.before != nil {
				before = append(before, tc.before)
			}
			d := rescuetest.New(t, podUpdateIssue, before...)
			want, shape := tc.leave(t, d)
			rig := newPodUpdateRig(t, readyWorker(claim.RoleArchitect), readyWorker(claim.RoleImplementer))
			rig.rescueOn(d)
			rig.answeredRound(claim.RoleArchitect, claim.RoleImplementer)

			rig.awaitReplacement(1, "once every worker answered and the push check passed")
			checks := rig.rt.CallsOf("RescueUnpushed")
			if len(checks) != 1 {
				t.Fatalf("the runtime was asked for %d push checks, want one for the one replacement", len(checks))
			}
			if got := checks[0]; got.Pod.Claim != rig.tokens[claim.RoleImplementer] || got.Rescue.Branch != podUpdateRescueBranch ||
				got.Rescue.WorkspaceTimeout != rescueWorkspaceTimeout || got.Rescue.Timeout != rescueTimeout {
				t.Errorf("the push check was asked through %s for %+v, want the implementer's claim and branch %s with the check's bounds",
					got.Pod.Claim, got.Rescue, podUpdateRescueBranch)
			}
			branch := podUpdateRescueBranch + "-1"
			held := d.RemoteBranch(branch)
			messages := rig.messages()
			if want == "" {
				if held != "" || len(messages) != 0 {
					t.Fatalf("the remote holds %s at %q and the issue was told %q, want neither for a clean disk", branch, held, messages)
				}
				return
			}
			if held == "" || (want != "a new commit" && held != want) {
				t.Fatalf("the remote holds %s at %q, want %s", branch, held, want)
			}
			if shape != nil {
				shape(held)
			}
			if extra := d.RemoteBranch(podUpdateRescueBranch + "-2"); extra != "" {
				t.Fatalf("the remote holds %s-2 at %s, want one rescue branch: the workspace's own work is one head", podUpdateRescueBranch, extra)
			}
			if len(messages) != 1 || !strings.Contains(messages[0], "`"+branch+"` (commit "+held+")") || !strings.Contains(messages[0], d.Workspace) {
				t.Fatalf("the issue was told %q, want one message naming %s at %s from %s", messages, branch, held, d.Workspace)
			}
			if logs := rig.logs.String(); !strings.Contains(logs, branch+"@"+held) || !strings.Contains(logs, "push check of a workspace before its pod is replaced: rescued") {
				t.Errorf("the sweep's log does not name %s@%s on its workspace line and its replacement line:\n%s", branch, held, logs)
			}
		})
	}
}

// A failed claim is not waited for (askedClaims), so nothing it promised says its work is pushed.
// The check reads the disk itself, so what it committed there and never pushed reaches the remote
// before the pod goes.
func TestPodUpdateRescuesWhatAFailedClaimLeftOnTheDisk(t *testing.T) {
	d := rescuetest.New(t, podUpdateIssue)
	d.Write(d.Workspace, "harness.txt", "the planner's harness, committed and never pushed\n")
	d.JJ("commit", "-m", "the planner's harness")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true, contract: readyWorker(claim.RolePlanner).contract})
	rig.rescueOn(d)
	rig.answeredRound(claim.RoleImplementer)

	rig.awaitReplacement(1, "once the implementer answered, the failed planner asked nothing")
	if held := d.RemoteBranch(podUpdateRescueBranch + "-1"); held != commit {
		t.Fatalf("the remote holds %s-1 at %q, want the failed planner's commit %s", podUpdateRescueBranch, held, commit)
	}
}

// A push the remote refuses leaves the pod where it is: nothing is replaced, the round stays
// open, and once the notice bound has passed since the last answer the controller is told once,
// naming the claim the check ran through, the workspace and the error. A later sweep checks again,
// and once the remote takes the push the pod is replaced.
func TestPodUpdateHoldsTheRoundWhenThePushCheckFails(t *testing.T) {
	d := rescuetest.New(t, podUpdateIssue)
	d.Write(d.Workspace, "feature.txt", "work the remote will not take\n")
	d.JJ("commit", "-m", "work the remote will not take")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.RefuseWrites()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rescueOn(d)
	rig.answeredRound(claim.RoleImplementer)

	rig.awaitRescues(1, "once the implementer answered")
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times after a push check that failed, want none", len(calls))
	}
	if refused := rig.rt.CallsOf("ReplacePodNotDue"); len(refused) == 0 {
		t.Fatal("the runtime recorded no refused replacement: the failed check must refuse it inside ReplacePod")
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the round = %q after a failed push check, want it still open", c.UpdateRound)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times before the notice bound", len(notices))
	}

	rig.clock.advance(podUpdateNotice)
	rig.awaitRescues(len(rig.rt.CallsOf("RescueUnpushed"))+1, "past the notice bound")
	rig.tick()
	rig.tick()
	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a push check that keeps failing, want once", len(notices))
	}
	for _, want := range []string{string(rig.tokens[claim.RoleImplementer]), d.Workspace, "the remote refuses this push", "check for unpushed work"} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, want it to name %q", notices[0].Reason, want)
		}
	}
	wantAnswered(t, notices[0], claim.RoleImplementer)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the push keeps failing, want none", len(calls))
	}
	if logs := rig.logs.String(); !strings.Contains(logs, "push check of a workspace before its pod is replaced: error") {
		t.Errorf("the sweep's log has no error line for the workspace:\n%s", logs)
	}

	if err := os.Remove(filepath.Join(d.Remote, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
	rig.awaitReplacement(1, "once the remote takes the push")
	if held := d.RemoteBranch(podUpdateRescueBranch + "-1"); held != commit {
		t.Fatalf("the remote holds %s-1 at %q, want %s", podUpdateRescueBranch, held, commit)
	}
	if notices := rig.notices(); len(notices) != 1 {
		t.Errorf("the controller was told %d times in all, want the one notice", len(notices))
	}
}

// A pod whose shim predates the check cannot be checked, and is replaced all the same, as every
// pod was before the check: holding every pod started from an older image until its tree ends is
// not the trade. The replacement's line and the issue say that nothing checked the disk.
func TestPodUpdateReplacesAPodWhoseShimPredatesTheCheckAndSaysSo(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rt.RescueWith(func(context.Context, runtime.Known, runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		return nil, fmt.Errorf("worker-stream: %w (its agent answered: Unknown command: rescue-unpushed)", runtime.ErrRescueUnsupported)
	})
	rig.answeredRound(claim.RoleImplementer)

	rig.awaitReplacement(1, "though its shim predates the push check")
	messages := rig.messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "did not check the pod's workspaces") || !strings.Contains(messages[0], "predates the push check") {
		t.Fatalf("the issue was told %q, want one message that nothing checked the disk", messages)
	}
	if logs := rig.logs.String(); !strings.Contains(logs, `"unchecked":"the pod's shim predates the push check`) {
		t.Errorf("the replacement's line does not say the pod went unchecked:\n%s", logs)
	}
}

// A check the shim could not run at all — a transport failure, a shim with no workspace to read —
// is no pass: the next process of the pod is asked, and with none left the round holds.
func TestPodUpdateHoldsTheRoundWhenNoProcessCanRunThePushCheck(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.RescueWith(func(_ context.Context, k runtime.Known, _ runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		return nil, errors.New("the claim has no connection to ask over: " + string(k.Claim))
	})
	rig.answeredRound(claim.RoleImplementer, claim.RoleTester)

	rig.awaitRescues(2, "through both processes of the pod")
	checks := rig.rt.CallsOf("RescueUnpushed")
	if checks[0].Pod.Claim != rig.tokens[claim.RoleImplementer] || checks[1].Pod.Claim != rig.tokens[claim.RoleTester] {
		t.Errorf("the push check was asked through %s then %s, want the implementer then the tester", checks[0].Pod.Claim, checks[1].Pod.Claim)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times with no check run, want none", len(calls))
	}
}
