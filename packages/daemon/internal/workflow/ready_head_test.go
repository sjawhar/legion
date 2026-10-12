package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The pull request's updated_at at W, the head a lagging record holds; at V, a push between W and
// the READY's head; and at X, the head GitHub showed the route, which the READY names.
var (
	atW = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	atV = atW.Add(time.Minute)
	atX = atW.Add(2 * time.Minute)
)

// seedLaggingRecord is LEGION-208 in merging, its pull request #42 recorded at W, at W's clock,
// with its merger, and a design gate approved or closed at version 4.
func seedLaggingRecord(t *testing.T, pool *pgxpool.Pool, approved bool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}
	if approved {
		gate.ApprovedVersion = new(4)
	}
	seedGate(t, pool, gate)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: "W", HeadUpdatedAt: atW})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
}

// readyAtX is the merger's READY at X, read by the route against the lagging record at W.
var readyAtX = intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
	Summary: readyPacket, Commit: "X", RecordedHead: "W", RecordedHeadAt: atW, HeadAt: atX}

// recordedHead is the head and clock the record holds for LEGION-208's pull request.
func recordedHead(t *testing.T, pool *pgxpool.Pool) (string, time.Time) {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
		return err
	})
	if pr == nil {
		t.Fatal("LEGION-208 has no pull request recorded")
	}
	return pr.HeadSHA, pr.HeadUpdatedAt
}

// A READY the design gate holds was read against a record lagging at W while GitHub showed X. The
// READY advances the record to X at GitHub's clock, so a synchronize for V, a push between W and X
// whose webhook arrives late, is older than the record and changes nothing: it does not void the
// held READY, and the gate's approval advances the merge.
func TestALateSynchronizeOfAnEarlierHeadDoesNotVoidAGateHeldREADY(t *testing.T) {
	pool := migratedPool(t)
	seedLaggingRecord(t, pool, false)
	apply := applyFacts(t, pool, readyEngine(""))
	if result := apply("ready", readyAtX); refusalCode(result) != "DESIGN_GATE_CLOSED" {
		t.Fatalf("READY with the gate closed = %#v, want DESIGN_GATE_CLOSED", result)
	}
	if head, at := recordedHead(t, pool); head != "X" || !at.Equal(atX) {
		t.Errorf("record after the gate-held READY = %s at %s, want X at %s", head, at, atX)
	}

	apply("late-v", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "V", UpdatedAt: atV})
	for _, notice := range architectNotices(t, pool) {
		if strings.HasPrefix(notice.Reason, "READY_HEAD_MOVED:") {
			t.Errorf("a late synchronize of V voided the held READY: %+v", notice)
		}
	}
	apply("approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})
	assertPhase(t, pool, phase.AwaitingMerge)
}

// An accepted READY read against a lagging record advances it to the READY's head at GitHub's
// clock, so a synchronize of W, the head the record lagged at, redelivered after the acceptance
// changes nothing: it does not withdraw the READY into another round.
func TestARedeliveredSynchronizeOfTheLaggingHeadDoesNotWithdrawTheREADY(t *testing.T) {
	pool := migratedPool(t)
	seedLaggingRecord(t, pool, true)
	apply := applyFacts(t, pool, readyEngine(""))
	if result := apply("ready", readyAtX); result.Refusal != nil {
		t.Fatalf("READY against a lagging record = %#v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.AwaitingMerge)
	if head, at := recordedHead(t, pool); head != "X" || !at.Equal(atX) {
		t.Errorf("record after the accepted READY = %s at %s, want X at %s", head, at, atX)
	}

	apply("redelivered-w", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "W", UpdatedAt: atW})
	assertPhase(t, pool, phase.AwaitingMerge)
	if got := autoMergeDisables(t, pool); len(got) != 0 {
		t.Fatalf("auto-merge disables %v after a redelivered synchronize, want none", got)
	}
}

// The route reads the record's head and its clock together, and READY is accepted only while the
// transaction's record is exactly that snapshot, or already at the READY's commit. A record that
// left the head the route read and came back to it - a force push and its undo, C2 to C to C2 -
// holds the same head at a later clock, so it moved, and READY is refused.
func TestAREADYIsRefusedWhenTheRecordLeftAndCameBackToTheHeadTheRouteRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		head    string
		at      time.Time
		refused bool
	}{
		{"the record is the snapshot the route read", "C2", atW, false},
		{"the record came back to that head at a later clock", "C2", atX, true},
		{"the record is already at the commit", "X", atX, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedLaggingRecord(t, pool, true)
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
				HeadSHA: tc.head, HeadUpdatedAt: tc.at})
			apply := applyFacts(t, pool, readyEngine(""))
			ready := readyAtX
			ready.RecordedHead, ready.RecordedHeadAt = "C2", atW
			result := apply("ready", ready)
			if !tc.refused {
				if result.Refusal != nil {
					t.Fatalf("READY = %#v, want it accepted", result.Refusal)
				}
				assertPhase(t, pool, phase.AwaitingMerge)
				return
			}
			if refusalCode(result) != "READY_HEAD_MOVED" {
				t.Fatalf("READY = %#v, want READY_HEAD_MOVED", result)
			}
			assertPhase(t, pool, phase.Merging)
		})
	}
}
