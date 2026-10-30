package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Oh My Pi's agent_start can land before the answer to the prompt that caused it, so the
// acknowledgement arrives in a turn that has already confirmed the delivery. The prompt is still
// outstanding then (ackedInTurn), and the turns that follow are still only its own once its
// result says so: without that, the ordering alone would decide whether a role's background
// result could take its task.
func TestAnAcknowledgementThatLandsInsideItsOwnTurnStillAwaitsItsResult(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send", gated.entered)

	// The turn's start beats the prompt's answer, and confirms the delivery; the answer follows.
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	gated.release <- struct{}{}
	h.m.Wait()
	p := h.pending()
	if p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the turn that started first to have confirmed it", p)
	}

	// A turn ends — the role's own background result's, for all the daemon knows — and the task
	// is not retired on it.
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	if h.claim().Pending == nil {
		t.Fatal("an agent_end its prompt's result had not followed retired the task")
	}
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: p.ID, AgentInvoked: true})
	if got := h.claim().Pending; got != nil {
		t.Fatalf("pending %+v after the prompt's result, want it retired", *got)
	}
}

// A7. Background work is on for every role, so Oh My Pi wakes a settled session with its own
// background result and starts a turn the daemon never prompted. That turn's agent_start carries
// no delivery id — Oh My Pi's own never does — and its agent_end names no prompt either, so both
// look exactly like the turn the daemon's task is waiting for. Only the prompt's own result says
// which turn was the prompt's (omp://rpc.md: agent_end "carries no prompt identity, so correlate
// prompts through `prompt_result`"), and it arrives on the heels of that turn's agent_end.
//
// The two orders the daemon can see this in are the two cases here, and they are not the same
// question. Before the send, the turn is simply the claim working: the task waits for the turn's
// end, which sends it. After the prompt was acknowledged, the daemon is waiting for *its* task's
// turn, and the background result's is the one that starts; the task's own turn is the one after
// it, because Oh My Pi's follow-up queue dequeues one delivery group per drain (omp://rpc.md,
// "Mode semantics"). Either way the role is prompted exactly once, works the task in a turn that
// serves its run and its assignment, and the task is retired only when its prompt's work is over.
func TestABackgroundResultsTurnIsNotTheTasksTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// start brings the claim to the moment the background result's turn begins, and answers
		// how many prompts the role has had by then.
		start func(h *harness) int
	}{
		{name: "before the daemon sends the task", start: func(h *harness) int {
			h.conns.Remove(h.token)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Assignment: "A1"})
			h.wantPrompts(0)
			h.conns.Register(h.token, h.conn)
			return 0
		}},
		{name: "after the task's prompt was acknowledged", start: func(h *harness) int {
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Assignment: "A1"})
			h.wantPrompts(1)
			if p := h.pending(); p.DeliveredAt.IsZero() {
				t.Fatalf("pending %+v, want the prompt acknowledged", p)
			}
			return 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			prompted := tc.start(h)

			// The background result's turn: an agent_start the daemon did not prompt, and an
			// agent_end with no prompt_result behind it (mustQuiet).
			h.mustQuiet(StreamTurnStart{Claim: testToken})
			h.wantState(StateWorking)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			h.wantState(StateIdle)

			if h.claim().Pending == nil {
				t.Fatal("the background result's turn took the task: the claim holds none, and the role is never prompted for it again")
			}
			// Whichever order it came in, the task has now been prompted exactly once: the send
			// before the turn, or the one the turn's end made.
			sent := h.wantPrompts(1)[0]
			if sent.Message != "implement the plan" {
				t.Fatalf("the prompt is %+v, want the task", sent)
			}
			if prompted == 0 && h.pending().DeliveredAt.IsZero() {
				t.Fatal("the turn's end sent the task but the claim does not hold it as delivered")
			}

			// The task's own turn, the one after the background result's.
			h.must(StreamTurnStart{Claim: testToken})
			if got := h.claim().ServingRun(); got != 7 {
				t.Fatalf("during the task's turn the claim serves run %d, want the task's 7", got)
			}
			if got := h.claim().ServingAssignment(); got != "A1" {
				t.Fatalf("during the task's turn the claim serves assignment %q, want the task's A1", got)
			}
			delivery := h.pending().ID
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			if p := h.claim().Pending; p == nil {
				t.Fatal("the task retired at an agent_end its prompt's result had not followed")
			}

			// Oh My Pi says the prompt's work is over: that turn was its own, and the task's life
			// ends with it.
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: delivery, AgentInvoked: true})

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the task's prompt ended, want it retired", *p)
			}
			if stored := h.store.load(testToken); stored.ServingGeneration != 7 || stored.ServingAssignmentID != "A1" {
				t.Fatalf("the stored claim serves run %d assignment %q, want 7 and A1",
					stored.ServingGeneration, stored.ServingAssignmentID)
			}
			if _, ok := h.store.delivery(testToken); ok {
				t.Error("the store still holds the delivery whose prompt ended")
			}
			// The role was prompted for this task exactly once, whichever order it came in.
			h.wantPrompts(1)
		})
	}
}

// The ordinary turn, with no background work in it: the task's own turn ends and its result
// follows it, as Oh My Pi writes the two (the harness's own turn end carries the result,
// resultAfterTurn), and the delivery retires once. The result is what retires it, so a result
// that beats its turn's end — the two are milliseconds apart on one stream — retires it just the
// same, from the end that follows.
func TestATasksTurnRetiresItOnceWhicheverOfItsEndAndItsResultComesFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		// finish ends the task's turn and its prompt, in this case's order.
		finish func(h *harness, delivery string)
	}{
		{name: "the turn ends, then its result", finish: func(h *harness, _ string) {
			h.must(StreamTurnEnd{Claim: testToken})
		}},
		{name: "the result comes first", finish: func(h *harness, delivery string) {
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: delivery, AgentInvoked: true})
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Assignment: "A1"})
			h.must(StreamTurnStart{Claim: testToken})
			if h.pending().ConfirmedAt.IsZero() {
				t.Fatal("the task's own turn did not confirm it")
			}

			tc.finish(h, h.pending().ID)

			h.wantState(StateIdle)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v, want the task retired", *p)
			}
			if stored := h.store.load(testToken); stored.ServingGeneration != 7 {
				t.Fatalf("the stored claim serves run %d, want 7", stored.ServingGeneration)
			}
			h.wantPrompts(1)
			h.wantBudgets(Budgets{})
		})
	}
}

// A prompt Oh My Pi took and ran no turn for at all is the definitive form of what the wait for
// the turn otherwise only times out on: the task goes back to waiting, keeping its read mark,
// and the prompt is charged like any other that came to nothing.
func TestAPromptThatRanNoTurnIsChargedWhenItsResultSaysSo(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 7})
	first := h.wantPrompts(1)[0].DeliveryID

	h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: false, Error: "the command ran locally"})

	h.wantBudgets(Budgets{PromptFailures: 1})
	p := h.pending()
	if p.ID == first {
		t.Fatal("the task kept the id of the prompt that ran no turn; a retry under it is an echo the shim answers")
	}
	if p.DeliveredAt.IsZero() {
		t.Error("the task lost its read mark: the agent took the prompt, so it may have read it")
	}
	if _, awaiting := h.m.timers[TimerTurn]; awaiting {
		t.Error("the wait for a turn that will never start is still armed")
	}
}
