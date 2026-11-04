package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A5's rule that a parked child holds nothing, read over a child some other tree of the daemon
// recorded. Whether a child is open is a question about the child — does a record of it exist,
// and has that record left the workflow — and not about which tree holds that record: a child
// parked in another tree is as parked as one parked in this one, and no tree will do its work,
// so reading it as open would hold its Dispatch parent for ever.
//
// A child no tree records at all is the opposite case and stays open in backlog or triage: it is
// work the architect filed and has not released yet, which is exactly what the parent must wait
// for.
func TestAChildParkedInAnotherTreeHoldsNothing(t *testing.T) {
	const parent, parkedElsewhere, unrecorded, otherTree = "LEGION-500", "LEGION-501", "LEGION-502", "LEGION-600"
	parentKey := parent
	issues := []Issue{{Key: parent, Tree: parent, Project: "LEGION", Phase: phase.Admitted, Status: "in_progress"}}
	children := map[string][]DispatchIssueRow{parent: {
		{Key: parkedElsewhere, Project: "LEGION", Status: "backlog", Parent: parent},
		{Key: unrecorded, Project: "LEGION", Status: "backlog", Parent: parent},
	}}
	// The parked child's own record is another tree's: a person moved it under this parent on
	// Dispatch after that tree parked it, and workflow's leave recorded it at phase done there.
	records := map[string]Issue{parkedElsewhere: {Key: parkedElsewhere, Tree: otherTree, Parent: &parentKey,
		Project: "LEGION", Phase: phase.Done, Status: "backlog"}}

	waits := ChildWaits(issues, children, records)
	wait, waiting := waits[parent]
	if !waiting {
		t.Fatalf("the judgement of %s = %+v, want it waiting on the child no tree records", parent, waits)
	}
	if len(wait.Open) != 1 || wait.Open[0].Key != unrecorded {
		t.Fatalf("the open children of %s = %+v, want %s alone: %s is parked, and the tree that parked it is not the question",
			parent, wait.Open, unrecorded, parkedElsewhere)
	}
}
