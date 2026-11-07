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

// The pair holds for every turn, not for the unconfirmed case alone: the end line names what its
// own start line named. turnEnded reads the claim's pending delivery rather than the turn's own
// (Claim.turnsDelivery: a pending delivery that is confirmed is taken for the turn now running),
// and the claim can hold a confirmed delivery while a turn that is not that delivery's runs.
//
// The window is the daemon's own: a turn of the task's ends before Oh My Pi says which turn the
// prompt's was, so settle retires nothing, the claim goes idle holding that task confirmed, and
// the task is still in the worker's hands (turnEnded's resultPending branch). A turn of the
// agent's own then — a background task finishing, an Envoy notice, a person's steer — is no
// task's, so turnStarted's own test (p.ConfirmedAt.IsZero()) leaves it unconfirmed and its start
// line names no delivery, while its end line names the task that is waiting for its result. A
// reader pairing the two reads that task's turn as having run twice.
//
// Measured at e873e1641111: turn 2 starts naming no delivery and ends naming the confirmed one.
func TestTheTurnLogEndLineNamesWhatItsOwnStartLineNamed(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	delivery := h.pending().ID

	// The task's own turn, start to end, with no prompt_result behind it yet.
	h.must(StreamTurnStart{Claim: testToken})
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	p := h.claim().Pending
	if p == nil || p.ConfirmedAt.IsZero() || !h.m.resultPending() {
		t.Fatalf("the task is not still in the worker's hands: pending %+v, state %s", p, h.state())
	}

	// A turn of the agent's own in that window.
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	starts, ends := h.records.of(startedLine), h.records.of(endedLine)
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("the log holds %d starts and %d ends, want two of each", len(starts), len(ends))
	}
	for i := range starts {
		start, started := starts[i].attr("delivery")
		end, ended := ends[i].attr("delivery")
		if started != ended || (started && start.String() != end.String()) {
			t.Fatalf("turn %d started naming delivery %v (present %v) and ended naming %v (present %v)",
				i+1, start, started, end, ended)
		}
	}
	if start, ok := starts[0].attr("delivery"); !ok || start.String() != delivery {
		t.Fatalf("the task's own turn started naming %v (present %v), want %s", start, ok, delivery)
	}
}
