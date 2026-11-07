package workflow

import (
	"testing"
	"time"

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

// A transition writes the status of the phase it enters, in the transaction that records the
// phase: the move into implementing queues nothing more, because in_progress is the status the
// issue already shows and it keeps it, and the move on into testing queues that phase's own.
// No status repair runs here — each write comes from the transition itself.
//
// Recording a child under a tree whose gate is open is no longer one of those transitions. The
// cap counts the issues at work, so the child's first phase takes a slot of its own: the fact
// records the child at admitted, in the waiting line, and admission's promotion is what moves it
// into planning and writes its in_progress (dispatch://INFRA-980, acceptance A3 and A4). Before
// this release the one fact did both, which is what this test asserted when main wrote it.
func TestATransitionWritesTheStatusOfThePhaseItEnters(t *testing.T) {
	pool := migratedPool(t)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := testEngine(config.DesignGateRootIssues, nil)

	// The child is recorded where the line finds it, and nothing of its own status is written
	// until the slot comes.
	applyRefusingNothing(t, pool, engine, intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 1, Type: "issue.updated",
		Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V"})
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s once the gate's tree recorded it, want admitted: its first phase waits for a slot of its own", got)
	}
	assertQueuedStatusWrites(t, pool)

	// The promotion the slot brings, as admission makes it: the child stands in planning with
	// in_progress written. From here the transitions are the workflow's own again.
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "V"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "planner-claim"})
	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "planner-claim",
		Summary: "the plan", HandoffRecord: "plan", HandoffVersion: 1})
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Implementing {
		t.Fatalf("the child = %s once its planner completed, want implementing", got)
	}
	if got := issueRecord(t, pool, "LEGION-209"); got.Status != "in_progress" {
		t.Fatalf("the child in implementing carries status %q, want in_progress", got.Status)
	}
	assertQueuedStatusWrites(t, pool)
}
