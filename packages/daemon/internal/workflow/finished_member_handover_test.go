package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The expiry reads the member's record status (record.Issue.Status) and not its phase, pinned
// here: a member whose work finished — its phase is done — while its status is one the workflow
// runs in, which is what a person's move of a finished child back to in_progress during the
// linger leaves. record.InWorkflow answers false for it, because it asks the phase as well, and
// the expiry asks the status alone.
//
// What the rule means for it: the tree has closed, so nothing of that tree will ever run it, and
// the status a person set says it is meant to be worked. It is handed to Legion as a root of its
// own, and its own re-admission decides what that means for a record whose phase is done
// (admit's TestAHandedOverChildThatFinishedIsAdmittedFresh). The member a person left out of the
// workflow is the one the tree keeps.
func TestAFinishedMemberAPersonPutBackToWorkIsHandedOverWhenTheTreeCloses(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, revived, finished := "LEGION-208", "LEGION-210", "LEGION-211"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	// The member a person put back to work after it finished, and the member that finished and
	// was left alone.
	for _, member := range []struct{ key, status string }{
		{key: revived, status: "in_progress"},
		{key: finished, status: "done"},
	} {
		seedIssue(t, pool, record.Issue{Key: member.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: &rootKey, Project: "LEGION", Title: member.key, Phase: phase.Done, Generation: 7,
			Status: member.status, DispatchStatus: member.status, Rank: "V", LastDispatchSeq: 1})
	}

	apply := applyFacts(t, pool, engine)
	apply("closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
		Status: "done", Title: "root", Rank: "U"})
	recordTheStatusAMoveShows(t, pool, root, "done")

	apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

	assertHandedOver(t, pool, revived, root, 7)
	if got := handoverFences(t, pool); len(got) != 1 || got[0] != revived {
		t.Fatalf("hand-over fences = %v, want %s's alone: a member Dispatch shows done stays with the tree that closed", got, revived)
	}
}
