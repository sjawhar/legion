package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A forced operator drain ends a long task turn without StreamTurnEnd. Its confirmed phase task is
// interrupted, not retired as finished, so the operator's resume re-sends it to the same owner.
func TestAnOperatorStopTimeoutTakesBackTheConfirmedPhaseTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue testing.", ID: "outbox:7", Phase: phase.Testing, Generation: 1})
	h.must(StreamTurnStart{Claim: testToken, DeliveryID: "outbox:7"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend = %v, want ErrSuspendHeld", err)
	}
	h.advance(testStop)
	h.wantState(StateSuspended)
	pending := h.pending()
	if !pending.Interrupted || !pending.ConfirmedAt.IsZero() || pending.ID == "outbox:7" {
		t.Fatalf("after the forced stop pending %+v, want an interrupted, unconfirmed replacement", pending)
	}
	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)
	if pending = h.pending(); !pending.Interrupted || pending.Task != "Continue testing." {
		t.Fatalf("after the operator resume pending %+v, want the interrupted phase task", pending)
	}
	h.wantPrompts(2)
}
