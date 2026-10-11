package supervise

import (
	"errors"
	"testing"
)

// A daemon suspend (linger or an issue leaving the workflow) that reaches a rule-resuming launch
// waits through that launch and runs at ready. It is not silently finished while the claim keeps
// running.
func TestADaemonSuspendDuringARuleResumeLaunchIsHeldUntilReady(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("daemon suspend during launch = %v, want ErrSuspendHeld", err)
	}
	if claim := h.claim(); !claim.SuspensionHeld || claim.SuspensionHeldBy != SuspendedByDaemon {
		t.Fatalf("the launch reports %+v, want the daemon suspension held", claim)
	}
	h.connect()
	h.register()
	h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByDaemon {
		t.Fatalf("the claim records %q, want daemon", got)
	}
}
