package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A root has no ancestors here (Ancestors): every rule written over the one chain walk is a rule
// about one tree, and a root's own Dispatch parent belongs to another. The root of this test ran
// as a tree of its own — it was handed over, so no tree adopts it — while Dispatch still shows it
// under a member of another tree, that member is done and that tree's own root waits for its
// merge.
//
// Walked past the root into that tree, the chain reached an ancestor the daemon has already
// accepted, so the root's own first phase was held where it stood (HeldUnderAccepted) until a tree
// it is no part of closed: the line offered it nothing, `blocked` named neither it nor a reason,
// and its architect heard nothing. The records above the pass are what made the walk reach that
// far (LineFacts.Parents, Store.AncestorsOf), so the whole path is read here as the line reads it.
func TestARootReadsNoAncestorInTheTreeItsDispatchParentBelongsTo(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const (
		root       = "LEGION-910"
		otherRoot  = "LEGION-920"
		doneMember = "LEGION-921"
	)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, chainIssue(root, root, doneMember, phase.Admitted, "in_progress")))
		must(t, records.PutIssue(ctx, tx, chainIssue(otherRoot, otherRoot, "", phase.AwaitingMerge, "needs_review")))
		must(t, records.PutIssue(ctx, tx, chainIssue(doneMember, otherRoot, otherRoot, phase.Done, "done")))
		approved := 1
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree of %s: %v", root, err)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		issues, err := records.LiveIssues(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		facts, err := ReadLineFacts(ctx, tx, records, "LEGION", issues)
		if err != nil {
			t.Fatalf("read the line's facts: %v", err)
		}
		// The control: the pass does hold the records of the other tree, so a walk that reads
		// them reaches the issue waiting for its merge.
		if _, carried := facts.Parents[doneMember]; !carried {
			t.Fatalf("the pass's facts hold no record of %s, the done parent of %s in the other tree: this test proves nothing", doneMember, root)
		}
		chain := chainOf(byKey(issues), facts.Parents)
		if above := Ancestors(recordOf(issues, root), chain); len(above) != 0 {
			t.Fatalf("the chain above %s = %v, want nothing: its Dispatch parent %s belongs to the tree of %s", root, keysOf(above), doneMember, otherRoot)
		}
		if ancestor := AcceptedAncestor(recordOf(issues, root), chain); ancestor != "" {
			t.Fatalf("the accepted ancestor of %s = %q, want none: %s merges a head %s is no part of", root, ancestor, otherRoot, root)
		}
		if !inLine(Waiting(issues, nil, facts), root) {
			t.Fatalf("%s stands out of the waiting line, though its design gate is open and nothing of its own tree holds it: it waits on the close of %s, which is no tree of its own", root, otherRoot)
		}
	})
}
