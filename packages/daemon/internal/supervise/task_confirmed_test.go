package supervise

import "testing"

// A turn that starts on the task the daemon gave the agent tells the workflow the agent took it
// (Deps.TaskConfirmed): a root whose wait that untaken task held is decided on that fact rather
// than at the next tick. Nothing else is told of: a turn with no delivery to confirm, and a turn
// that confirms a delivery the write failed for, tell no one.
func TestATurnThatTakesItsTaskTellsTheWorkflow(t *testing.T) {
	h := newBareHarness(t)
	var confirmed []Claim
	h.deps.TaskConfirmed = func(c Claim) { confirmed = append(confirmed, c) }
	h.start(queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "read the answer"})
	h.wantPrompts(1)
	if len(confirmed) != 0 {
		t.Fatalf("told of %d claims before any turn, want none", len(confirmed))
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if len(confirmed) != 1 || confirmed[0].Token != testToken {
		t.Fatalf("told of %+v, want the one claim whose task was taken", confirmed)
	}
	if p := confirmed[0].Pending; p == nil || p.ConfirmedAt.IsZero() {
		t.Fatalf("told of pending %+v, want the confirmed delivery", p)
	}

	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if len(confirmed) != 1 {
		t.Fatalf("told of %d claims, want the one confirmation", len(confirmed))
	}
}
