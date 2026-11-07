package supervise

// The live case behind dispatch://INFRA-714 R7: INFRA-264's tester registered on a fresh claim, an
// Envoy message reached its role topic six seconds later and started its first turn, it ran the
// whole testing round in that turn, and its handoff_complete was refused 409 HANDOFF_NO_RUN —
// "this claim has taken none" — because the daemon's phase task had not been delivered yet.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A completion made in a turn a message started, before the claim's phase task was ever put to the
// agent, is credited to that task's run; once the issue has left the phase the task was queued
// for, the task is retired and never delivered after the work it names is done.
func TestAFreshClaimsFirstTurnIsCreditedTheTaskTheDaemonHasNotSentYet(t *testing.T) {
	h := newBareHarness(t)
	issuePhase := phase.Testing
	generation := uint64(0)
	h.deps.PhaseHolds = func(_ context.Context, _ string, p phase.Phase, run uint64) (bool, error) {
		return p == issuePhase && run == generation, nil
	}
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.reach(StateReady)

	// The phase's task is queued for run 7 while the agent is in no turn, and the connection is
	// held shut, so nothing of it reaches the agent.
	h.conns.Remove(h.token)
	generation = 7
	h.must(RequestDeliver{Claim: testToken, Task: "test the branch", Phase: issuePhase, Generation: 7})
	sent := len(h.prompts())
	if p := h.pending(); p.Prompted {
		t.Fatalf("pending %+v, want a task the daemon has not put to the agent", p)
	}

	// An Envoy message starts the claim's first turn, with no delivery behind it.
	h.must(StreamTurnStart{Claim: testToken})

	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("the claim serves run %d during its first turn, want the run of the task it holds, 7", run)
	}

	// The completion is accepted on that answer, and the issue leaves testing with it. The turn
	// then ends, and the task it did is retired rather than sent.
	issuePhase = phase.Reviewing
	h.conns.Register(h.token, h.conn)
	h.must(StreamTurnEnd{Claim: testToken})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim still holds %+v, want the completed phase's task retired", p)
	}
	if stored := h.store.load(testToken); stored.Pending != nil {
		t.Fatalf("the store still holds %+v, want the task retired durably", stored.Pending)
	}
	if got := h.prompts(); len(got) != sent {
		t.Fatalf("the task was sent after its work was reported: prompts %+v", got[sent:])
	}
}
