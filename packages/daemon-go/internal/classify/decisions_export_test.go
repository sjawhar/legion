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
	got := ApplyPush(counted, "head", PushClassification{HandoffOnly: true})
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
	prior := record.PullRequest{FixAttempts: 3}
	got, blocked := BlockFixAttempt(prior, 3)
	if !blocked || got.BlockedAttempts != 3 {
		t.Fatalf("first exhausted count = %#v blocked %t, want publish", got, blocked)
	}
	_, blocked = BlockFixAttempt(got, 3)
	if blocked {
		t.Fatal("same exhausted count published twice")
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
