package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A child that was already planning when the daemon that writes its phase's status was deployed
// shows `todo` in Dispatch, and no transition is coming to correct it: the boot writes it
// (dispatch://INFRA-714/spec R4). The write is the phase's own status, conditional on the status
// the record held, and the record takes that status with it.
func TestTheBootWritesTheStatusOfAPhaseAnIssueAlreadyStoodIn(t *testing.T) {
	pool := migratedPool(t)
	seedBootTree(t, pool)
	// The child the older daemon left: planning, recorded todo, which is what Dispatch shows.
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a child the old daemon started", Parent: new("LEGION-208"),
		Phase: phase.Planning, Generation: 4, Status: "todo", DispatchStatus: "todo", Rank: "V"})
	// A child whose status is already its phase's is left alone, and so is one out of the workflow.
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a child under test", Parent: new("LEGION-208"),
		Phase: phase.Testing, Generation: 4, Status: "testing", DispatchStatus: "testing", Rank: "W"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-211", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a parked child", Parent: new("LEGION-208"),
		Phase: phase.Done, Generation: 4, Status: "backlog", DispatchStatus: "backlog", Rank: "X"})

	applyMirrored(t, pool, testEngine(config.DesignGateRootIssues, nil), "boot", intake.DaemonBooted{})

	want := []record.StatusWrite{{Status: "in_progress", ObservedStatus: "todo"}}
	if got := statusWrites(t, pool, "LEGION-209"); len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("the status writes the boot queued for the planning child = %+v, want %+v", got, want)
	}
	for _, key := range []string{"LEGION-208", "LEGION-210", "LEGION-211"} {
		if got := statusWrites(t, pool, key); len(got) != 0 {
			t.Fatalf("the boot queued %+v for %s, want nothing: its status is already its phase's or it is out of the workflow", got, key)
		}
	}
	if got := recordedStatus(t, pool, "LEGION-209"); got != "in_progress" {
		t.Fatalf("the record of the planning child = %q, want the status the boot wrote, in_progress", got)
	}
}

// seedBootTree records the live tree the boot's issues belong to: its root planning in its slot,
// its gate open.
func seedBootTree(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// recordedStatus is the status the record holds for key.
func recordedStatus(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(t.Context(), "select status from issues where key = $1", key).Scan(&status); err != nil {
		t.Fatalf("read the status of %s: %v", key, err)
	}
	return status
}
