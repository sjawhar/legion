package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A4's own clause: "A handed-over root that carries accepting resumes at once while the cap is
// full." Its promotion starts no worker — the root arrives in accepting, where record.AtWork is
// false — so it takes no slot, and holding it back bounds nothing while leaving its tree closed.
//
// The fixture is the state the clause is about: the cap's one slot is held by a tree already at
// work, a released child of that tree stands at admitted waiting for its own first phase, and
// the handed-over root ranks behind that child. The child needs a slot and the cap has none, so
// the promotion of the child is right to wait; the carried root's is not, and it is behind it in
// the line.
func TestAHandedOverRootCarryingAcceptingResumesWhileTheCapIsFull(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)

	// The live tree that holds the cap's one slot, its design gate open, with one released
	// child whose first phase has still to start.
	seedSlotted(t, pool, "LEGION-100", "A")
	approved := 1
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: "LEGION-100",
			ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("open the gate of LEGION-100: %v", err)
		}
	})
	putIssue(t, pool, record.Issue{Key: "LEGION-101", Tree: "LEGION-100", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a released child", Phase: phase.Admitted,
		Generation: 1, Status: "todo", Rank: "A1", HandedOver: true})

	// The handed-over root, ranked behind that child: a tree of its own still to be opened,
	// carrying the phase it ran in the tree it left.
	putIssue(t, pool, record.Issue{Key: "LEGION-200", Tree: "LEGION-200", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root", Phase: phase.Accepting,
		Generation: 2, Status: "todo", Rank: "B", HandedOver: true, HandedOverFrom: "LEGION-700"})
	seedMirror(t, pool, "LEGION-200", "todo")

	// Both candidates are in the line, the child first by its tree root's rank: what the pass
	// does with the second one is the whole of this test.
	assertWaiting(t, pool, []string{"LEGION-101", "LEGION-200"})

	tick(t, pool, admission, "tick")

	if holders := slots(t, pool); len(holders) != 1 || holders[0].Issue != "LEGION-100" {
		t.Fatalf("slots = %#v, want the cap's one slot still held by LEGION-100: the carried root takes none", holders)
	}
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	var open bool
	inTx(t, pool, func(tx pgx.Tx) {
		lifecycles, err := record.NewStore().TreeLifecycles(context.Background(), tx, project, []string{"LEGION-200"})
		if err != nil {
			t.Fatalf("read the tree lifecycles: %v", err)
		}
		life, found := lifecycles["LEGION-200"]
		open = found && life.Authority == treelifecycle.AuthorityWorkflow
	})
	if !open {
		t.Fatalf("the tree of the handed-over root LEGION-200 is not open after the pass: a candidate whose promotion starts no worker resumes at once, whatever the cap")
	}
	if carried := issue(t, pool, "LEGION-200"); carried.Status != "in_progress" {
		t.Fatalf("the handed-over root's status = %q, want in_progress: its promotion writes the status of a tree that runs", carried.Status)
	}
}
