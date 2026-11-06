package record

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// One pass walks one chain (chainOf): the hold under an accepted ancestor (Waiting) and the
// children-first cycle (ChildWaits) are two walks of the same records, so the map they walk is
// built in one place. It holds the pass's own records and the records above them that its read
// leaves out (LineFacts.Parents), which is what lets either walk cross an ancestor already done;
// the pass's own records stay a map of their own, since what runs and what an issue ranks against
// is read from those alone.
func TestOnePassWalksOneChain(t *testing.T) {
	root := chainIssue("LEGION-900", "LEGION-900", "", phase.Admitted, "in_progress")
	merging := chainIssue("LEGION-901", "LEGION-900", "LEGION-900", phase.Merging, "needs_review")
	doneParent := chainIssue("LEGION-902", "LEGION-900", "LEGION-901", phase.Done, "done")
	child := chainIssue("LEGION-903", "LEGION-900", "LEGION-902", phase.Admitted, "todo")
	issues := []Issue{root, merging, child}
	above := map[string]Issue{doneParent.Key: doneParent}

	chain := chainOf(byKey(issues), above)
	want := map[string]Issue{root.Key: root, merging.Key: merging, child.Key: child, doneParent.Key: doneParent}
	if !reflect.DeepEqual(chain, want) {
		t.Fatalf("the chain of the pass holds %v, want %v: its own records and the records above them",
			slices.Sorted(maps.Keys(chain)), slices.Sorted(maps.Keys(want)))
	}
	// The one walk both readers make crosses the done parent and names the merging ancestor,
	// which is the whole reason the records above the pass are in the map at all.
	if ancestor := AcceptedAncestor(child, chain); ancestor != merging.Key {
		t.Fatalf("the accepted ancestor of %s over the pass's chain = %q, want %s past the done parent %s", child.Key, ancestor, merging.Key, doneParent.Key)
	}
	if ancestor := AcceptedAncestor(child, byKey(issues)); ancestor != "" {
		t.Fatalf("the accepted ancestor of %s over the pass's own records alone = %q, want none: that walk stops at the parent they leave out", child.Key, ancestor)
	}
}
