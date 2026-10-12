package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A phase task already queued on a claim survives an operator suspension. The operator resumes the
// same phase owner, so retiring that task at suspension loses the start row's only delivery and
// leaves the resumed owner idle forever.
func TestAnOperatorSuspensionKeepsThePendingPhaseTaskForItsResume(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue testing.", ID: "outbox:7", Phase: phase.Testing, Generation: 1})
	h.must(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator})
	h.wantState(StateSuspended)
	if pending := h.claim().Pending; pending == nil || pending.Task != "Continue testing." || pending.Phase != phase.Testing {
		t.Fatalf("after the operator suspension pending %+v, want the phase task kept", pending)
	}
	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)
	if pending := h.claim().Pending; pending == nil || pending.Task != "Continue testing." {
		t.Fatalf("after the operator resume pending %+v, want the original phase task", pending)
	}
	h.wantPrompts(2)
}
