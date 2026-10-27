package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A head pushed onto a red counts a fix attempt whether its push is known yet or not, unless the
// push was the review App's (the tester's red tests).
func TestAdvancePullRequestHeadCountsEveryPushOntoRedButTheReviewApps(t *testing.T) {
	red := record.PullRequest{HeadSHA: "old", CheckedHead: "old", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2}
	for _, tc := range []struct {
		name    string
		pushes  []record.ClassifiedPush
		counted bool
	}{
		{"a push not yet arrived", nil, true},
		{"a known push", []record.ClassifiedPush{{SHA: "next", Before: "old"}}, true},
		{"the review App's push", []record.ClassifiedPush{{SHA: "next", Before: "old", ByReviewApp: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := red
			prior.Pushes = tc.pushes
			got := AdvancePullRequestHead(prior, "next")
			wantAttempts, wantCounted := 2, ""
			if tc.counted {
				wantAttempts, wantCounted = 3, "next"
			}
			if got.FixAttempts != wantAttempts || got.HeadCounted != wantCounted || HeadVerdict(got) != "" || len(got.Pushes) != 0 {
				t.Fatalf("got %#v, want fixAttempts=%d headCounted=%q, no verdict and every push spent", got, wantAttempts, wantCounted)
			}
		})
	}
}

// A push that arrives after its head takes back the count that head's arrival made only when it
// was the review App's.
func TestApplyPushTakesBackOnlyTheReviewAppsCount(t *testing.T) {
	counted := record.PullRequest{HeadSHA: "head", FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "head"}
	got := ApplyPush(counted, record.ClassifiedPush{SHA: "head", ByReviewApp: true})
	if got.FixAttempts != 2 || got.BlockedAttempts != 0 || got.HeadCounted != "" || !got.PlannedRed {
		t.Fatalf("the review App's take-back = %#v, want decremented, unblocked and planned", got)
	}
	got = ApplyPush(counted, record.ClassifiedPush{SHA: "head"})
	if got.FixAttempts != 3 || got.BlockedAttempts != 3 || got.HeadCounted != "head" || got.PlannedRed {
		t.Fatalf("another push = %#v, want the count kept", got)
	}
}

// The counted mark names the head the count last counted, and stays across every head pushed on
// top of it until one is pushed onto a green verdict: a push onto a head CI has not judged yet is the
// same attempt, not a new one, so it is neither counted nor clears the mark. A head that was not
// counted takes nothing back when its late push turns out the review App's.
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
	if got := ApplyPush(unjudged, record.ClassifiedPush{SHA: "h5", Before: "h4", ByReviewApp: true}); got.FixAttempts != 3 || got.HeadCounted != "h4" {
		t.Fatalf("h5's late review App push = %#v, want nothing taken back: h5 was not counted", got)
	}
	unjudged.CheckedHead, unjudged.Failing, unjudged.CheckRuns = "h5", []string{}, []record.AttemptRun{{Name: "ci", ID: 1}}
	onGreen := AdvancePullRequestHead(unjudged, "h6")
	if onGreen.FixAttempts != 3 || onGreen.HeadCounted != "" {
		t.Fatalf("a push onto green h5 = %#v, want the count kept and the mark cleared", onGreen)
	}
}

// A push onto a settled head whose base branch requires no check and no workflow leaves the
// counted attempt, as a push onto green does, though that head has no verdict: both markers clear
// and the count stays. A head without a verdict for any other reason - the required set unread, no
// settlement standing for it, a required workflow still running - is the same attempt, so the push
// keeps both markers.
func TestAdvancePullRequestHeadLeavesTheAttemptOnAPushOntoABaseThatRequiresNothing(t *testing.T) {
	marked := record.PullRequest{HeadSHA: "h3", CheckedHead: "h3", Failing: []string{}, Required: []string{},
		FixAttempts: 3, HeadCounted: "h3", HeadCountedBefore: "h2"}
	for _, tc := range []struct {
		name    string
		pr      func(record.PullRequest) record.PullRequest
		cleared bool
	}{
		{"a settled head whose base requires nothing", func(pr record.PullRequest) record.PullRequest { return pr }, true},
		{"a settled head whose required set was never read", func(pr record.PullRequest) record.PullRequest {
			pr.Required = nil
			return pr
		}, false},
		{"a head no settlement stands for, whose base requires nothing", func(pr record.PullRequest) record.PullRequest {
			pr.CheckedHead = "h1"
			return pr
		}, false},
		{"a settled head whose base requires only a workflow still running", func(pr record.PullRequest) record.PullRequest {
			pr.Workflows, pr.WorkflowsHead = []record.RequiredWorkflow{{Path: "review.yml", Result: Pending}}, "h3"
			return pr
		}, false},
		{"a settled head whose base requires only a workflow that succeeded", func(pr record.PullRequest) record.PullRequest {
			pr.Workflows, pr.WorkflowsHead = []record.RequiredWorkflow{{Path: "review.yml", Result: Success}}, "h3"
			return pr
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AdvancePullRequestHead(tc.pr(marked), "h4")
			wantCounted, wantBefore := "h3", "h2"
			if tc.cleared {
				wantCounted, wantBefore = "", ""
			}
			if got.FixAttempts != 3 || got.HeadCounted != wantCounted || got.HeadCountedBefore != wantBefore {
				t.Fatalf("fixAttempts=%d headCounted=%q headCountedBefore=%q, want 3, %q and %q",
					got.FixAttempts, got.HeadCounted, got.HeadCountedBefore, wantCounted, wantBefore)
			}
		})
	}
}

// An uncounted push onto a red inside the counted attempt - the review App's red tests - leaves
// the attempt's mark where it stood, whichever of the push webhook and the synchronize arrives
// first: synchronize first counts the head until its push arrives, and the push's take-back
// restores the mark that count replaced.
func TestAnUncountedPushOntoAMarkedRedKeepsTheMarkInEitherOrder(t *testing.T) {
	for _, pushFirst := range []bool{true, false} {
		marked := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "h2"}
		got := arrive(marked, "h3", true, pushFirst)
		if got.FixAttempts != 3 || got.BlockedAttempts != 3 || got.HeadCounted != "h2" || got.HeadCountedBefore != "" {
			t.Errorf("push first %v: fixAttempts=%d blockedAttempts=%d headCounted=%q headCountedBefore=%q, want 3, 3, \"h2\" and \"\"",
				pushFirst, got.FixAttempts, got.BlockedAttempts, got.HeadCounted, got.HeadCountedBefore)
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

// A red sends the tree back only while it stands for the head (HeadVerdict): the head's own red; a
// red an earlier head left behind sends nothing back. The tester's planned red (its own failing
// tests) is the one exception, and a head with no verdict - no settlement, or a required set never
// read - sends nothing.
func TestRedSendsBackOnlyOnTheRedThatStandsForTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		back bool
	}{
		{"the head's own red", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, true},
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
// withdraws it, while a red of the head before it, which stood before the READY and says nothing
// new about the submitted head, does not; nor does any red on a pull request that is no longer open.
func TestRedWithdrawsReadyOnlyOnTheHeadsOwnRed(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		want bool
	}{
		{"the READY head's own red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Failing: []string{"ci"}, Required: []string{"ci"}}, true},
		{"its own required workflow run red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Required: []string{},
			Workflows: []record.RequiredWorkflow{{Path: "review.yml", Result: "failure"}}, WorkflowsHead: "ready"}, true},
		{"the head before it's red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "code", Failing: []string{"ci"}, Required: []string{"ci"}}, false},
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
// does: the push names whether its pusher was the review App, and the synchronize advances the
// pull request to the head.
func arrive(pr record.PullRequest, sha string, byReviewApp, pushFirst bool) record.PullRequest {
	push := record.ClassifiedPush{SHA: sha, Before: pr.HeadSHA, ByReviewApp: byReviewApp}
	if pushFirst {
		return AdvancePullRequestHead(ApplyPush(pr, push), sha)
	}
	return ApplyPush(AdvancePullRequestHead(pr, sha), push)
}

// One tester round, in both webhook orders, each step reading the pull request the previous one
// handed over: the review App's red tests set the planned mark, its second push of red tests onto
// the planned red keeps it without counting, the implementer's fix clears it without counting the
// planned red, and a later fix whose push webhook is lost counts against the unplanned red and
// carries the cleared mark.
func TestPlannedRedIsSetAndClearedAcrossATesterRound(t *testing.T) {
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

			pr = arrive(pr, "red-tests", true, pushFirst)
			check("the review App's red tests", "red-tests", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "more-red-tests", true, pushFirst)
			check("the review App's second push of red tests", "more-red-tests", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "fix", false, pushFirst)
			check("the implementer's fix", "fix", false, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = AdvancePullRequestHead(pr, "fix-2")
			check("a later fix whose push webhook is lost", "fix-2", false, 1, "fix-2")
		})
	}
}
