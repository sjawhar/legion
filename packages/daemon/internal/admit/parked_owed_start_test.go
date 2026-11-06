package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A start owed to a worker whose tree is then parked is finished by the repair pass, not carried
// across the park.
//
// The repair reads the row's tree through the project's live records, which leave out a
// lingering root: its phase is done and its status is not todo, and it holds no slot under this
// release, so nothing of it is in that read. Reading the linger off the zero record answered
// false, and every other fence passed — the members of a parked tree keep their phase and their
// generation — so the row survived the park. At the tree's re-admission its issue stood in the
// line's first part with the old row's created_at, ahead of its own root in the second part, and
// took the first free slot before the tree was opened or its architect started.
func TestAStartOwedBeforeAParkIsFinishedRatherThanCarriedAcrossIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const root, member = "LEGION-400", "LEGION-401"
	parent := root
	until := fixedNow.Add(time.Hour)

	// The tree as its park left it: the root lingering at done, the member keeping the phase and
	// the generation it was working, and the start its worker is owed still pending.
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a parked root", Phase: phase.Done, Generation: 1,
		Status: "in_progress", Rank: "A", HandedOver: true, LingerUntil: &until})
	putIssue(t, pool, record.Issue{Key: member, Tree: root, Parent: &parent, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a member mid-phase", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "A1"})
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(member, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleImplementer,
			Generation: 1, Phase: phase.Implementing, Task: "Implement the change.", NeedsSlot: true}, fixedNow)
		if err != nil {
			t.Fatalf("build the start %s is owed: %v", member, err)
		}
		if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
			t.Fatalf("enqueue the start %s is owed: %v", member, err)
		}
	})
	if owed := startsOf(t, pool, member); len(owed) != 1 {
		t.Fatalf("the starts %s owes = %+v, want the one the park left behind: the fixture's own premise", member, owed)
	}

	tick(t, pool, admission, "tick")

	if owed := startsOf(t, pool, member); len(owed) != 0 {
		t.Fatalf("the starts %s owes after the pass = %+v, want none: a row whose tree has left the workflow would come back at the tree's re-admission, in the line's first part, ahead of its own root", member, owed)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		// The line is empty with it: nothing of a parked tree waits for a slot.
		issues, err := record.NewStore().LiveIssues(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		facts, err := record.ReadLineFacts(context.Background(), tx, record.NewStore(), testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line's facts: %v", err)
		}
		for _, entry := range record.Waiting(issues, nil, facts) {
			if entry.Issue.Key == member {
				t.Fatalf("%s stands in the waiting line though its tree lingers", member)
			}
		}
	})
}
