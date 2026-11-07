package supervise

import "testing"

// R6 of dispatch://INFRA-1132 binds both turn lines alike: each carries the claim, its generation
// and, where the turn is a task's, the delivery. turnStarted holds to that — a turn that is no
// task's names no delivery, and its own comment says so — and turnEnded does not: it names
// whatever the claim holds pending, whether or not that task's turn is the one ending.
//
// The path below is one a worker meets. A task is delivered; Oh My Pi acknowledges the prompt and
// then refuses it with no turn of its own (lateRefused), which sends the task back unread and
// leaves it pending with nothing in flight. The agent then starts a turn of its own — an Envoy
// notice, a person's steer — which is no task's, so the start line names no delivery. When that
// turn ends, the end line names the task that is still waiting to be read, and a reader pairing
// the two finds an end for a turn that never started and reads a task as run when it is not.
//
// Measured at 25dcbfd9ec2c: the start line carries no delivery and the end line carries the
// pending task's id.
func TestTheTurnLogEndLineNamesNoDeliveryForATurnThatIsNoTasks(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	delivery := h.pending().ID

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: delivery, Error: "the provider rejected the prompt"})
	p := h.claim().Pending
	if p == nil {
		t.Fatalf("the refused task is not pending; state = %s", h.state())
	}
	if !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task sent back unread reads confirmed at %s; the turn below would be its own", p.ConfirmedAt)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	wantTurnLine(t, h, startedLine, h.generation(), "")

	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	wantTurnLine(t, h, endedLine, h.generation(), "")
}
