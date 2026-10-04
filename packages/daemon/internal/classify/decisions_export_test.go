package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAdvancePullRequestHeadCountsRedFixAndConsumesHandoffClassification(t *testing.T) {
	prior := record.PullRequest{
		HeadSHA: "old", CheckedHead: "old", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2,
		Pushes: []record.ClassifiedPush{{SHA: "next", Before: "old", HandoffOnly: true}},
	}
	got := AdvancePullRequestHead(prior, "next")
	if got.FixAttempts != 2 || got.HeadCounted != "" {
		t.Fatalf("handoff-only head = %#v, want unchanged count", got)
	}

	got = AdvancePullRequestHead(record.PullRequest{HeadSHA: "old", CheckedHead: "old", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2}, "next")
	if got.FixAttempts != 3 || got.HeadCounted != "next" || HeadVerdict(got) != "" {
		t.Fatalf("real fix head = %#v, want counted next head", got)
	}
}

func TestApplyPushTakesBackOnlyTheCurrentHandoffOnlyCount(t *testing.T) {
	counted := record.PullRequest{HeadSHA: "head", FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "head"}
	got := ApplyPush(counted, record.ClassifiedPush{SHA: "head", HandoffOnly: true})
	if got.FixAttempts != 2 || got.BlockedAttempts != 0 || got.HeadCounted != "" {
		t.Fatalf("handoff-only take-back = %#v, want decremented unblocked head", got)
	}
}

func TestApplySettlementUsesTheExportedSettlementClassifiers(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}}
	got, applied := ApplySettlement(prior, SettlementCandidate{
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2}}, Generation: 1, Snapshot: "snapshot", Failing: []string{"unit"}, Cancelled: []string{"e2e"},
	})
	if !applied || got.Generation != 1 || got.Snapshot != "snapshot" || len(got.Failing) != 1 || len(got.Cancelled) != 1 {
		t.Fatalf("settlement = %#v applied %t, want red candidate applied", got, applied)
	}
}

func TestBlockFixAttemptPublishesOnlyOncePerExhaustedCount(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3}
	got, blocked := BlockFixAttempt(prior, 3)
	if !blocked || got.BlockedAttempts != 3 {
		t.Fatalf("first exhausted count = %#v blocked %t, want publish", got, blocked)
	}
	_, blocked = BlockFixAttempt(got, 3)
	if blocked {
		t.Fatal("same exhausted count published twice")
	}
}

// A green settlement at an exhausted count is the fix that worked, not a blocked pull request: only
// a red one reports the count, and only a red that stands for the head (HeadVerdict): a red a code
// push left behind reports nothing.
// A red sends the tree back only while it stands for the head: a red a code push left behind, whose
// head no longer carries it, sends nothing back.
func TestRedSendsBackOnlyOnTheRedThatStandsForTheHead(t *testing.T) {
	if !RedSendsBack(record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, nil) {
		t.Fatal("the head's own red did not send the tree back")
	}
	if RedSendsBack(record.PullRequest{HeadSHA: "fix", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, nil) {
		t.Fatal("a red that no longer stands for the head sent the tree back")
	}
}

// A red that only the review workflows the project declares make is the reviewer's round's to
// decide, so it sends nothing back; a red required workflow the project does not declare - a test
// or lint workflow - is a failing check, and sends the tree back as a red required check does,
// alone or beside a declared one. With no review workflow declared, every red required workflow
// sends it back. A required check still pending or with no result is not red, so a review
// workflow's red beside it is still the review workflows' alone.
func TestRedSendsBackLeavesOnlyADeclaredReviewWorkflowsRedToTheReviewer(t *testing.T) {
	const review, tests = ".github/workflows/review.yml", ".github/workflows/tests.yml"
	declared := []string{review}
	code := func(failing []string, cancelled []string, runs []record.AttemptRun, workflows ...record.RequiredWorkflow) record.PullRequest {
		return record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: failing, Cancelled: cancelled, CheckRuns: runs, Required: []string{"ci"},
			Workflows: workflows, WorkflowsHead: "head"}
	}
	green := []record.AttemptRun{{Name: "ci", ID: 1}}
	for _, tc := range []struct {
		name                            string
		pr                              record.PullRequest
		declared                        []string
		onlyWorkflows, onlyReview, back bool
	}{
		{"the review workflow failed beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), declared, true, true, false},
		{"the review workflow timed out beside a check with no result", code([]string{}, nil, nil, record.RequiredWorkflow{Path: review, Result: "timed_out"}), declared, true, true, false},
		{"the review workflow failed beside a cancelled check", code([]string{}, []string{"ci"}, nil, record.RequiredWorkflow{Path: review, Result: "failure"}), declared, true, true, false},
		{"the review workflow failed beside a green undeclared one", code([]string{}, nil, green,
			record.RequiredWorkflow{Path: review, Result: "failure"}, record.RequiredWorkflow{Path: tests, Result: Success}), declared, true, true, false},
		{"an undeclared required workflow failed beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: tests, Result: "failure"}), declared, true, false, true},
		{"the review workflow and an undeclared one failed", code([]string{}, nil, green,
			record.RequiredWorkflow{Path: review, Result: "failure"}, record.RequiredWorkflow{Path: tests, Result: "failure"}), declared, true, false, true},
		{"the review workflow failed with none declared", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), nil, true, false, true},
		{"the required check failed beside a green review workflow", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: Success}), declared, false, false, true},
		{"the required check and the review workflow failed", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), declared, false, false, true},
		{"the review workflow still running beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: Pending}), declared, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedOnlyByWorkflows(tc.pr); got != tc.onlyWorkflows {
				t.Errorf("RedOnlyByWorkflows = %t, want %t", got, tc.onlyWorkflows)
			}
			if got := RedOnlyByReviewWorkflows(tc.pr, tc.declared); got != tc.onlyReview {
				t.Errorf("RedOnlyByReviewWorkflows = %t, want %t", got, tc.onlyReview)
			}
			if got := RedSendsBack(tc.pr, tc.declared); got != tc.back {
				t.Errorf("RedSendsBack = %t, want %t", got, tc.back)
			}
		})
	}
}

// A READY stands until the head's own CI turns red: a red the head's own settlement and run bring
// withdraws it, a head a .legion/-only push reached included, while a red carried to it from the
// head before it, which READY found green on GitHub since, does not; nor does any red on a pull
// request that is no longer open.
func TestRedWithdrawsReadyOnlyOnTheHeadsOwnRed(t *testing.T) {
	handoff := []record.ClassifiedPush{{SHA: "ready", Before: "code", HandoffOnly: true}}
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		want bool
	}{
		{"the READY head's own red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Pushes: handoff, Failing: []string{"ci"}, Required: []string{"ci"}}, true},
		{"its own required workflow run red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Pushes: handoff, Required: []string{},
			Workflows: []record.RequiredWorkflow{{Path: "review.yml", Result: "failure"}}, WorkflowsHead: "ready"}, true},
		{"a red carried from the code head", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "code", Pushes: handoff, Failing: []string{"ci"}, Required: []string{"ci"}}, false},
		{"a closed pull request's red", record.PullRequest{State: record.PullRequestClosed, HeadSHA: "ready", CheckedHead: "ready", Failing: []string{"ci"}, Required: []string{"ci"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedWithdrawsReady(tc.pr); got != tc.want {
				t.Fatalf("RedWithdrawsReady = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestBlockFixAttemptPublishesOnlyOnARedSettlement(t *testing.T) {
	stale := record.PullRequest{HeadSHA: "fix", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3}
	if got, blocked := BlockFixAttempt(stale, 3); blocked || got.BlockedAttempts != 0 {
		t.Fatalf("a red that no longer stands for the head = %#v blocked %t, want no publish", got, blocked)
	}
	exhausted := record.PullRequest{HeadSHA: "head", CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}, FixAttempts: 3}
	if got, blocked := BlockFixAttempt(exhausted, 3); blocked || got.BlockedAttempts != 0 {
		t.Fatalf("green settlement at an exhausted count = %#v blocked %t, want no publish", got, blocked)
	}
	exhausted.Failing = []string{"ci"}
	if got, blocked := BlockFixAttempt(exhausted, 3); !blocked || got.BlockedAttempts != 3 {
		t.Fatalf("red settlement at an exhausted count = %#v blocked %t, want publish", got, blocked)
	}
}

func TestApplyDesignGateEventTracksCurrentVersionApproval(t *testing.T) {
	gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 2}
	opened := ApplyDesignGateEvent(gate, DesignGateApproved, 2)
	if !DesignGateOpen(opened) {
		t.Fatalf("approved gate = %#v, want open", opened)
	}
	closed := ApplyDesignGateEvent(opened, DesignGateVersion, 3)
	if DesignGateOpen(closed) || closed.LatestVersion != 3 {
		t.Fatalf("new version gate = %#v, want closed at v3", closed)
	}
}

// arrive applies one head's push webhook and its synchronize in the given order, as the engine
// does: the push classifies the head's changed paths and names whether its pusher was the review
// App, and the synchronize advances the pull request to the head.
func arrive(pr record.PullRequest, sha, changedPaths string, byReviewApp, pushFirst bool) record.PullRequest {
	truncated := "false"
	classification := ClassifyPush(PushPayload{ChangedPaths: &changedPaths, ChangedPathsTruncated: &truncated})
	push := record.ClassifiedPush{SHA: sha, Before: pr.HeadSHA, HandoffOnly: classification.HandoffOnly,
		Unknown: classification.Unknown, ByReviewApp: byReviewApp}
	if pushFirst {
		return AdvancePullRequestHead(ApplyPush(pr, push), sha)
	}
	return ApplyPush(AdvancePullRequestHead(pr, sha), push)
}

// One tester round, in both webhook orders, each step reading the pull request the previous one
// handed over: the review App's red tests set the planned mark, its handoff-only head carries it,
// the implementer's fix clears it without counting the planned red, and a later fix whose push
// webhook is lost counts against the unplanned red and carries the cleared mark.
func TestPlannedRedIsSetCarriedAndClearedAcrossATesterRound(t *testing.T) {
	for _, pushFirst := range []bool{true, false} {
		name := "synchronize first"
		if pushFirst {
			name = "push first"
		}
		t.Run(name, func(t *testing.T) {
			pr := record.PullRequest{HeadSHA: "impl", CheckedHead: "impl", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}}
			check := func(step, head string, plannedRed bool, fixAttempts int, headCounted string) {
				t.Helper()
				if pr.HeadSHA != head || pr.PlannedRed != plannedRed || pr.FixAttempts != fixAttempts || pr.HeadCounted != headCounted {
					t.Errorf("after %s: head %q plannedRed=%v fixAttempts=%d headCounted=%q, want head %q plannedRed=%v fixAttempts=%d headCounted=%q",
						step, pr.HeadSHA, pr.PlannedRed, pr.FixAttempts, pr.HeadCounted, head, plannedRed, fixAttempts, headCounted)
				}
			}

			pr = arrive(pr, "red-tests", "src/widget_test.go", true, pushFirst)
			check("the review App's red tests", "red-tests", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "tester-handoff", ".legion/test.json", true, pushFirst)
			check("the tester's handoff-only head", "tester-handoff", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "fix", "src/widget.go", false, pushFirst)
			check("the implementer's fix", "fix", false, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = AdvancePullRequestHead(pr, "fix-2")
			check("a later fix whose push webhook is lost", "fix-2", false, 1, "fix-2")
		})
	}
}
