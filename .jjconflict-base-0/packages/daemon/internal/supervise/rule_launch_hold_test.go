package supervise

import (
	"errors"
	"testing"
)

// A runtime suspend that fails while a rule-resuming agent becomes ready leaves the operator's
// launch-time hold intact and retries it at the probe interval. The claim must not remain ready
// with an in-memory-only operator hold.
func TestAReadyTimeOperatorSuspendFailureRetriesItsHold(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.connect()
	h.register()
	h.rt.FailSuspend(errors.New("the runtime refused the stop"))
	if err := h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session}); err == nil {
		t.Fatal("ready with a failed operator suspend succeeded, want the runtime failure")
	}
	if got := h.claim(); got.State != StateReady || !got.SuspensionHeld || got.SuspensionHeldBy != SuspendedByOperator {
		t.Fatalf("after the failed ready-time stop %+v, want ready with the operator hold intact", got)
	}
	h.rt.FailSuspend(nil)
	h.advance(testProbe)
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the retry suspended by %q, want operator", got)
	}
}

// If a rule-resuming agent never becomes ready, its registration deadline completes the retained
// operator suspend instead of dropping it and relaunching. The operator's hold is durable and the
// launch budget is unchanged.
func TestRegistrationDeadlineCompletesTheHeldOperatorSuspend(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.advance(deadline)
	h.wantState(StateSuspended)
	if got := h.claim(); got.SuspendedBy != SuspendedByOperator || got.Budgets.LaunchFailures != 0 {
		t.Fatalf("after the deadline %+v, want the operator suspension and no launch failure", got)
	}
	h.wantCalls("Resume", 1)
	h.wantCalls("Suspend", 2)
}
