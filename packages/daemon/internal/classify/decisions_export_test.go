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

// The counted mark names the head the count last counted, and stays across every head pushed on
// top of it until one is pushed onto a green verdict: a push onto a head CI has not judged yet is the
// same attempt, not a new one, so it is neither counted nor clears the mark. A head that was not
// counted takes nothing back when its late push turns out handoff-only.
func TestTheCountedMarkStaysUntilAHeadIsPushedOntoGreen(t *testing.T) {
	counted := AdvancePullRequestHead(record.PullRequest{HeadSHA: "h3", CheckedHead: "h3", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2}, "h4")
	if counted.FixAttempts != 3 || counted.HeadCounted != "h4" {
		t.Fatalf("a push onto red = %#v, want counted h4", counted)
	}
	unjudged := AdvancePullRequestHead(counted, "h5")
	if unjudged.FixAttempts != 3 || unjudged.HeadCounted != "h4" {
		t.Fatalf("a push onto unjudged h4 = %#v, want the count and the mark on h4 kept", unjudged)
	}
	if !FixAttemptCapHolds(record.PullRequest{HeadSHA: "h5", CheckedHead: "h5", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3, HeadCounted: "h4"}, 3) {
		t.Fatal("a red at h5, pushed onto unjudged h4, was not held at the exhausted count")
	}
	if got := ApplyPush(unjudged, record.ClassifiedPush{SHA: "h5", Before: "h4", HandoffOnly: true}); got.FixAttempts != 3 || got.HeadCounted != "h4" {
		t.Fatalf("h5's late handoff-only push = %#v, want nothing taken back: h5 was not counted", got)
	}
	unjudged.CheckedHead, unjudged.Failing, unjudged.CheckRuns = "h5", []string{}, []record.AttemptRun{{Name: "ci", ID: 1}}
	onGreen := AdvancePullRequestHead(unjudged, "h6")
	if onGreen.FixAttempts != 3 || onGreen.HeadCounted != "" {
		t.Fatalf("a push onto green h5 = %#v, want the count kept and the mark cleared", onGreen)
	}
}

// An uncounted push onto a red inside the counted attempt - the review App's red tests, or a
// handoff-only push - leaves the attempt's mark where it stood, whichever of the push webhook and
// the synchronize arrives first: synchronize first counts the head until its push is classified,
// and the push's take-back restores the mark that count replaced.
func TestAnUncountedPushOntoAMarkedRedKeepsTheMarkInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name, paths string
		byReviewApp bool
	}{
		{"the review App's red tests", "src/widget_test.go", true},
		{"a handoff-only push", ".legion/LEGION-208/test.json", false},
	} {
		for _, pushFirst := range []bool{true, false} {
			marked := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "h2"}
			got := arrive(marked, "h3", tc.paths, tc.byReviewApp, pushFirst)
			if got.FixAttempts != 3 || got.BlockedAttempts != 3 || got.HeadCounted != "h2" || got.HeadCountedBefore != "" {
				t.Errorf("%s, push first %v: fixAttempts=%d blockedAttempts=%d headCounted=%q headCountedBefore=%q, want 3, 3, \"h2\" and \"\"",
					tc.name, pushFirst, got.FixAttempts, got.BlockedAttempts, got.HeadCounted, got.HeadCountedBefore)
			}
		}
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

// A red sends the tree back only while it stands for the head (HeadVerdict): the head's own red, or
// a code head's carried to the handoff-only head above it, whose code is the same; a red a code push
// left behind, whose head no longer carries it, sends nothing back. The tester's planned red (its
// own failing tests) is the one exception, and a head with no verdict - no settlement, or a required
// set never read - sends nothing.
func TestRedSendsBackOnlyOnTheRedThatStandsForTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		back bool
	}{
		{"the head's own red", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, true},
		{"the code head's red carried to the handoff-only head above it", record.PullRequest{HeadSHA: "head", CheckedHead: "code", Failing: []string{"ci"}, Required: []string{"ci"},
			Pushes: []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}}, true},
		{"a red the tester planned", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, PlannedRed: true}, false},
		{"a green head", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{}, CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}}, false},
		{"a red that no longer stands for the head", record.PullRequest{HeadSHA: "fix", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, false},
		{"a head with no settlement", record.PullRequest{HeadSHA: "head", Required: []string{"ci"}}, false},
		{"a red with the required set unread", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedSendsBack(tc.pr); got != tc.back {
				t.Fatalf("RedSendsBack = %t, want %t", got, tc.back)
			}
		})
	}
}

// A red only required workflows make is one no required check makes: every required check passed,
// is pending or has no result, and at least one required workflow's run ended red. A red required
// check beside a red workflow is not that, and a workflow still running is not red.
func TestRedOnlyByWorkflowsNamesARedNoRequiredCheckMakes(t *testing.T) {
	const review, tests = ".github/workflows/review.yml", ".github/workflows/tests.yml"
	code := func(failing []string, cancelled []string, runs []record.AttemptRun, workflows ...record.RequiredWorkflow) record.PullRequest {
		return record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: failing, Cancelled: cancelled, CheckRuns: runs, Required: []string{"ci"},
			Workflows: workflows, WorkflowsHead: "head"}
	}
	green := []record.AttemptRun{{Name: "ci", ID: 1}}
	for _, tc := range []struct {
		name          string
		pr            record.PullRequest
		onlyWorkflows bool
	}{
		{"a workflow failed beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), true},
		{"a workflow timed out beside a check with no result", code([]string{}, nil, nil, record.RequiredWorkflow{Path: review, Result: "timed_out"}), true},
		{"a workflow failed beside a cancelled check", code([]string{}, []string{"ci"}, nil, record.RequiredWorkflow{Path: review, Result: "failure"}), true},
		{"one workflow failed beside a green one", code([]string{}, nil, green,
			record.RequiredWorkflow{Path: review, Result: "failure"}, record.RequiredWorkflow{Path: tests, Result: Success}), true},
		{"the required check failed beside a green workflow", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: Success}), false},
		{"the required check and a workflow failed", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), false},
		{"a workflow still running beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: Pending}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedOnlyByWorkflows(tc.pr); got != tc.onlyWorkflows {
				t.Fatalf("RedOnlyByWorkflows = %t, want %t", got, tc.onlyWorkflows)
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
