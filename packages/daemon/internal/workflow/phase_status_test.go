package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The status of every phase, written out here rather than read from the mapping, because the
// mapping is the only place these values live: a phase given another phase's status is a wrong
// status on the board for as long as the issue stands there, and nothing else would catch it. A
// phase added to the mapping and not to this list fails too, so the value a new phase shows is
// decided here as well.
func TestEachPhaseShowsItsOwnDispatchStatus(t *testing.T) {
	want := map[phase.Phase]string{
		phase.Admitted:       "",
		phase.AwaitingPerson: "",
		phase.Planning:       "in_progress",
		phase.Implementing:   "in_progress",
		phase.Testing:        "testing",
		phase.Reviewing:      "needs_review",
		phase.Accepting:      "needs_review",
		phase.Retro:          "retro",
		phase.Merging:        "retro",
		phase.AwaitingMerge:  "",
		phase.Done:           "done",
		phase.Held:           "",
	}
	for at, status := range want {
		if got := PhaseStatus(at); got != status {
			t.Errorf("PhaseStatus(%s) = %q, want %q", at, got, status)
		}
	}
	for at := range phaseStatus {
		if _, named := want[at]; !named {
			t.Errorf("the mapping holds %s, which this test does not name; add it here with the status Dispatch should show for it", at)
		}
	}
}

// Every phase the daemon has stands in the mapping. The list is phase.All, never one written here:
// a phase added to the daemon and left out of the mapping is what this catches. A phase with no
// entry reads as the empty status, which is no write at all, so the two-value lookup is what tells
// it from the four phases that mean the empty status.
func TestEveryPhaseHasAStatusEntry(t *testing.T) {
	for _, at := range phase.All {
		if _, ok := phaseStatus[at]; !ok {
			t.Errorf("phase %s has no entry in phaseStatus; an issue standing in it would keep whatever status it arrived with", at)
		}
	}
}

// A child's own first phase writes its status: the move into planning queues the in_progress that
// tells the person reading Dispatch that Legion works the child, in the transaction that records
// the phase. No status repair runs here — the write comes from the transition itself. The move on
// into implementing writes nothing more, because in_progress is the status the issue already
// shows, and it keeps it.
func TestATransitionWritesTheStatusOfThePhaseItEnters(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := testEngine(config.DesignGateRootIssues, nil)

	// The tree's gate is open, so recording the child moves it from admitted into planning in the
	// one fact.
	applyRefusingNothing(t, pool, engine, intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 1, Type: "issue.updated",
		Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V"})
	assertChildPhase(t, pool, phase.Planning)
	var status, observed string
	if err := pool.QueryRow(ctx, "select payload->>'status', payload->>'observedStatus' from outbox where kind = 'dispatch_status' and issue = 'LEGION-209'").
		Scan(&status, &observed); err != nil {
		t.Fatalf("read the child's status write: %v", err)
	}
	if status != "in_progress" || observed != "todo" {
		t.Fatalf("the move into planning wrote %q over %q, want in_progress over the todo Dispatch showed", status, observed)
	}
	assertOutboxCount(t, pool, "dispatch_status", 1)

	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "planner-claim"})
	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "planner-claim",
		Summary: "the plan", HandoffRecord: "plan", HandoffVersion: 1})
	assertChildPhase(t, pool, phase.Implementing)
	var carried string
	if err := pool.QueryRow(ctx, "select status from issues where key = 'LEGION-209'").Scan(&carried); err != nil {
		t.Fatalf("read the child's status: %v", err)
	}
	if carried != "in_progress" {
		t.Fatalf("the child in implementing carries status %q, want in_progress", carried)
	}
	assertOutboxCount(t, pool, "dispatch_status", 1)
}

// assertChildPhase requires LEGION-209, the child of this file's tree, to stand in want.
func assertChildPhase(t *testing.T, pool *pgxpool.Pool, want phase.Phase) {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = 'LEGION-209'").Scan(&got); err != nil {
		t.Fatalf("read the child's phase: %v", err)
	}
	if got != string(want) {
		t.Fatalf("the child is in phase %q, want %q", got, want)
	}
}
