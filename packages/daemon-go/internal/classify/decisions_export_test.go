package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAdvancePullRequestHeadCountsRedFixAndConsumesHandoffClassification(t *testing.T) {
	prior := record.PullRequest{
		HeadSHA: "old", Verdict: "red", FixAttempts: 2,
		PendingPush: &record.PendingPush{SHA: "next", HandoffOnly: true},
	}
	got := AdvancePullRequestHead(prior, "next")
	if got.FixAttempts != 2 || got.HeadCounted != "" || got.PendingPush != nil {
		t.Fatalf("handoff-only head = %#v, want unchanged count and consumed classification", got)
	}

	got = AdvancePullRequestHead(record.PullRequest{HeadSHA: "old", Verdict: "red", FixAttempts: 2}, "next")
	if got.FixAttempts != 3 || got.HeadCounted != "next" || got.Verdict != "" {
		t.Fatalf("real fix head = %#v, want counted next head", got)
	}
}

func TestApplyPushTakesBackOnlyTheCurrentHandoffOnlyCount(t *testing.T) {
	counted := record.PullRequest{HeadSHA: "head", Verdict: "", FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "head"}
	got := ApplyPush(counted, "", "head", PushClassification{HandoffOnly: true}, false)
	if got.FixAttempts != 2 || got.BlockedAttempts != 0 || got.HeadCounted != "" {
		t.Fatalf("handoff-only take-back = %#v, want decremented unblocked head", got)
	}
}

func TestApplySettlementUsesTheExportedSettlementClassifiers(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}}
	got, applied := ApplySettlement(prior, SettlementCandidate{
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2}}, Generation: 1, Snapshot: "snapshot", Verdict: "red", Failing: []string{"unit"},
	})
	if !applied || got.Verdict != "red" || got.Generation != 1 || got.Snapshot != "snapshot" || len(got.Failing) != 1 {
		t.Fatalf("settlement = %#v applied %t, want red candidate applied", got, applied)
	}
}

func TestApplyReviewRecordsChangesRequestedAndOnlyCurrentHeadApproval(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head"}
	if got := ApplyReview(prior, "approved", "old"); got.ReviewDecision != "" {
		t.Fatalf("stale approval = %#v, want no decision", got)
	}
	if got := ApplyReview(prior, "changes_requested", "old"); got.ReviewDecision != "changes_requested" {
		t.Fatalf("changes requested = %#v, want recorded decision", got)
	}
}

func TestBlockFixAttemptPublishesOnlyOncePerExhaustedCount(t *testing.T) {
	prior := record.PullRequest{Verdict: "red", FixAttempts: 3}
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
// a red one reports the count.
func TestBlockFixAttemptPublishesOnlyOnARedSettlement(t *testing.T) {
	exhausted := record.PullRequest{Verdict: "green", FixAttempts: 3}
	if got, blocked := BlockFixAttempt(exhausted, 3); blocked || got.BlockedAttempts != 0 {
		t.Fatalf("green settlement at an exhausted count = %#v blocked %t, want no publish", got, blocked)
	}
	exhausted.Verdict = "red"
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
	if pushFirst {
		return AdvancePullRequestHead(ApplyPush(pr, pr.HeadSHA, sha, classification, byReviewApp), sha)
	}
	return ApplyPush(AdvancePullRequestHead(pr, sha), pr.HeadSHA, sha, classification, byReviewApp)
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
			pr := record.PullRequest{HeadSHA: "impl", Verdict: "green"}
			check := func(step, head string, plannedRed bool, fixAttempts int, headCounted string) {
				t.Helper()
				if pr.HeadSHA != head || pr.PlannedRed != plannedRed || pr.FixAttempts != fixAttempts || pr.HeadCounted != headCounted || pr.PendingPush != nil {
					t.Errorf("after %s: head %q plannedRed=%v fixAttempts=%d headCounted=%q pendingPush=%v, want head %q plannedRed=%v fixAttempts=%d headCounted=%q and no pending push",
						step, pr.HeadSHA, pr.PlannedRed, pr.FixAttempts, pr.HeadCounted, pr.PendingPush, head, plannedRed, fixAttempts, headCounted)
				}
			}

			pr = arrive(pr, "red-tests", "src/widget_test.go", true, pushFirst)
			check("the review App's red tests", "red-tests", true, 0, "")
			pr.Verdict = "red"

			pr = arrive(pr, "tester-handoff", ".legion/test.json", true, pushFirst)
			check("the tester's handoff-only head", "tester-handoff", true, 0, "")
			pr.Verdict = "red"

			pr = arrive(pr, "fix", "src/widget.go", false, pushFirst)
			check("the implementer's fix", "fix", false, 0, "")
			pr.Verdict = "red"

			pr = AdvancePullRequestHead(pr, "fix-2")
			check("a later fix whose push webhook is lost", "fix-2", false, 1, "fix-2")
		})
	}
}
