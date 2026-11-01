package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The forced stop takes the confirmed task back before it stops the process, as a death does. When
// that write fails, nothing is stopped and the stop is tried again; the task is never left
// confirmed on a suspended claim, where settle would retire it as a finished turn. A daemon
// restarted after the stop still holds the interrupted task for the operator's resume.
func TestAForcedStopWhoseTaskWriteFailsKeepsTheTaskForTheResume(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue testing.", ID: "outbox:7", Phase: phase.Testing, Generation: 1})
	h.must(StreamTurnStart{Claim: testToken, DeliveryID: "outbox:7"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend = %v, want ErrSuspendHeld", err)
	}
	h.store.fail("PutDelivery", errBoom)
	h.advance(testStop)
	if p := h.claim().Pending; p == nil || p.Task != "Continue testing." {
		t.Fatalf("after the failed task write pending %+v, want the task kept", p)
	}
	h.store.fail("PutDelivery", nil)
	h.advance(testProbe)
	h.wantState(StateSuspended)

	h.restart()
	if stored := h.store.load(testToken); stored.Pending == nil || !stored.Pending.Interrupted || !stored.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("stored after the forced stop %+v, want the interrupted task", stored.Pending)
	}
	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)
	if p := h.pending(); !p.Interrupted || p.Task != "Continue testing." {
		t.Fatalf("after the operator resume pending %+v, want the interrupted phase task", p)
	}
}
