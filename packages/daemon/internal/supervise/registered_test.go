package supervise

import (
	"context"
	"testing"
)

// Every registration the claim persists is told to Deps.Registered with the claim as written — the
// first one and the recorded session registering again, which may have moved its file — and
// nothing else is: a suspension and the issue's close, which forgets the claim's session, tell it
// nothing, so the daemon's record of the conversation outlives both.
func TestEveryRegistrationAndNothingElseIsToldToRegistered(t *testing.T) {
	h := newBareHarness(t)
	var told []Claim
	h.deps.Registered = func(_ context.Context, c Claim) { told = append(told, c) }
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())

	h.reach(StateRegistered)
	if len(told) != 1 || told[0].Session != session || told[0].SessionFile != sessionFile || told[0].State != StateRegistered || told[0].Token != testToken {
		t.Fatalf("told after the registration = %+v, want the registered claim once", told)
	}
	h.ready()
	h.must(RequestRegister{Claim: testToken, Generation: h.generation(), Session: session, SessionFile: "/state/sessions/moved.jsonl",
		CapabilityHash: []byte("capability-2"), PluginContract: h.deps.PluginContract})
	if len(told) != 2 || told[1].Session != session || told[1].SessionFile != "/state/sessions/moved.jsonl" {
		t.Fatalf("told after the re-registration = %+v, want it told with the moved file", told)
	}

	h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})
	h.wantState(StateSuspended)
	h.must(RequestIssueClose{Claim: testToken})
	h.wantState(StateRetired)
	if len(told) != 2 {
		t.Errorf("told after the suspension and the close = %+v, want nothing more", told[2:])
	}
}
