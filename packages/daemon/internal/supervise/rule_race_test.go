package supervise

import (
	"errors"
	"testing"
)

// The operator's suspend can arrive after a phase start's RequestRuleResume changed the claim from
// suspended to launching. It is retained through that launch and takes the claim over as soon as
// the agent is ready, rather than being refused in launching and losing the operator's hold.
func TestAnOperatorSuspendRacingARuleResumeIsHeldAcrossTheLaunch(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	h.wantState(StateLaunching)

	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	if claim := h.claim(); !claim.SuspensionHeld || claim.SuspensionHeldBy != SuspendedByOperator {
		t.Fatalf("the launching claim reports %+v, want the operator's suspension held", claim)
	}
	h.connect()
	h.register()
	h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
	h.wantState(StateSuspended)
	if claim := h.claim(); claim.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the claim records %q, want the operator", claim.SuspendedBy)
	}
	h.wantCalls("Resume", 1)
	h.wantCalls("Suspend", 2)
}
