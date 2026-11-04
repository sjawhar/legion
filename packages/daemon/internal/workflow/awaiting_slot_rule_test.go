package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The spec's rule, restored: a root enters awaiting_person only while it holds a slot, so only a
// root whose design gate has never opened, whose architect is writing the spec and holds the
// whole of the tree's work (dispatch://INFRA-980, "Which issue needs a slot", line 65).
//
// A root whose gate has opened waits for its children instead, and holds no slot there. Sending
// such a root into awaiting_person takes every first-phase member of its tree out of the waiting
// line (record.Waiting drops the members of a root waiting on a person), so the children it is
// waiting for can never start; the controller is told its slot is released though it held none;
// and the next pass answers the wait as an approval, because the gate is open, which clears what
// the architect handed to the person. The rule is read as record.AtWork, the one definition the
// release of a slot and the line both read.
func TestARootWhoseGateHasOpenedEntersNoWaitOnAPerson(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, DeclaredSeq: 9}
	approved := 1
	opened := &record.DesignGate{Issue: root.Key, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}

	decision := decideAwait(awaitView{Root: root, Gate: opened, Armed: true})
	if decision.Act != actStay {
		t.Fatalf("the decision for a root whose gate has opened = %+v, want it left where it stands: it holds no slot to hand back, and its children are waiting for theirs", decision)
	}

	// The control: the same root before its gate ever opened, where the architect is writing the
	// spec and does hold the slot, is not stopped by this rule — whatever the rest of the rules
	// then decide, rule 0 is not what answers it.
	writing := *opened
	writing.ApprovedVersion, writing.EverOpened = nil, false
	if decision := decideAwait(awaitView{Root: root, Gate: &writing, Armed: true}); decision.Why == "its design gate has opened, so it holds no slot to hand back" {
		t.Fatalf("a root whose gate has never opened is stopped by the slot rule: that root is the one the wait is for")
	}
}
