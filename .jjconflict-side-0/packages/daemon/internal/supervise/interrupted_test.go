package supervise

// A worker whose process dies in a turn of its task is relaunched as the same session, and Oh My Pi
// does not continue an interrupted turn on resume, so the task goes back to waiting for the
// relaunched agent (interrupted). These tests pin what that take-back keeps, what it changes, and
// what a write that fails leaves behind.

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A worker whose process dies mid-turn is relaunched as the same session, and Oh My Pi does not
// continue an interrupted turn on resume. So the task goes back to waiting: unconfirmed, under a
// new id the new process's shim has no record of, and re-sent when the relaunched agent is ready,
// behind the one sentence that says its turn was interrupted — once, however often that happens,
// and never written into the task text, which stays the workflow's. The task is not retired and
// its run is not marked served until a turn of it ends.
func TestATaskInterruptedByADeathIsResentToTheRelaunchedAgent(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	sent := h.wantPrompts(1)[0].DeliveryID

	for death := 1; death <= 2; death++ {
		h.observe(runtime.Gone)
		h.relaunched()

		prompts := h.wantPrompts(1 + death)
		resent := prompts[death]
		if resent.DeliveryID == sent {
			t.Fatalf("death %d: the task was re-sent as %s, the id the dead process's turn ran under", death, sent)
		}
		if want := interruptedTask + "implement the plan"; resent.Message != want {
			t.Fatalf("death %d: re-sent %q, want %q", death, resent.Message, want)
		}
		if p := h.pending(); !p.ConfirmedAt.IsZero() || p.ID != resent.DeliveryID {
			t.Fatalf("death %d: pending %+v, want the re-sent task, unconfirmed until its turn starts", death, p)
		}
		stored := h.store.load(testToken)
		if stored.ServingGeneration != 0 || stored.Pending == nil {
			t.Fatalf("death %d: stored claim serving run %d with pending %+v, want no run served and the task kept",
				death, stored.ServingGeneration, stored.Pending)
		}
		if p := stored.Pending; p.Task != "implement the plan" || !p.Interrupted {
			t.Fatalf("death %d: stored task %q interrupted=%v, want the workflow's text marked interrupted", death, p.Task, p.Interrupted)
		}
		sent = resent.DeliveryID
		h.must(StreamTurnStart{Claim: testToken})
		h.wantState(StateWorking)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	if stored := h.store.load(testToken); stored.ServingGeneration != 5 || stored.Pending != nil {
		t.Fatalf("stored claim serving run %d with pending %+v, want run 5 served once the re-sent turn ended",
			stored.ServingGeneration, stored.Pending)
	}
}

// The interrupted turn ran, so the agent read its task, and the task keeps that read mark while it
// waits for the relaunch: for a claim that has served no run, it is what attributes a completion
// reported before the re-send's acknowledgement to the task's run (ServingRun), where a task that
// lost it would have that completion refused.
func TestAnInterruptedTaskKeepsItsReadMarkUntilTheResend(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	if p := h.pending(); p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the death = %+v, want it unconfirmed with its read mark kept", p)
	}
	if run := h.claim().ServingRun(); run != 5 {
		t.Fatalf("ServingRun() = %d after the death, want 5", run)
	}
}

// A task acknowledged and not yet begun when the process died was never run, so the relaunched
// agent is sent it as it was: nothing was interrupted.
func TestATaskNotYetBegunWhenTheProcessDiedIsResentAsItWas(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.wantPrompts(1)

	h.observe(runtime.Gone)
	h.relaunched()

	if resent := h.wantPrompts(2)[1]; resent.Message != "implement the plan" {
		t.Fatalf("re-sent %q, want the task as it was", resent.Message)
	}
}

// The re-sent task keeps the read mark the interrupted turn earned, and the relaunched agent's
// acknowledgement takes that mark over: a refusal of the re-sent prompt is judged against it, so
// the task goes back unread and the prompt is charged, as for any prompt its agent refused.
func TestARefusalOfTheResentTaskIsJudgedAgainstItsOwnAcknowledgement(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	h.relaunched()
	resent := h.wantPrompts(2)[1].DeliveryID

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: resent, Error: "the model provider refused the request"})

	h.wantBudgets(Budgets{Deaths: 1, PromptFailures: 1})
	if p := h.pending(); p.ID == resent || !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the refusal = %+v, want the task kept unread and unconfirmed under a new id", p)
	}
}

// Taking an interrupted task back is one delivery write, and a write that fails leaves the task as
// it was, confirmed. Were it left taken back in memory only, the next death would find nothing to
// take back and relaunch with the store still holding the confirmed delivery, and a restart before
// the re-send's acknowledgement would retire it as served: the stranded phase this exists to stop.
func TestAnInterruptedTaskWhoseWriteFailedIsTakenBackAtTheNextDeath(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})

	h.store.fail("PutDelivery", errBoom)
	gone := RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}
	if err := h.handle(gone); err == nil {
		t.Fatal("the death reported success with the delivery write failing")
	}
	if p := h.pending(); p.ConfirmedAt.IsZero() || p.Interrupted {
		t.Fatalf("pending after the failed write = %+v, want it as it was: confirmed, not taken back", p)
	}
	h.store.fail("PutDelivery", nil)
	h.observe(runtime.Gone)
	h.restart()

	stored := h.store.load(testToken)
	if p := stored.Pending; p == nil || !p.ConfirmedAt.IsZero() || !p.Interrupted || stored.ServingGeneration != 0 {
		t.Fatalf("after the restart: pending %+v serving run %d, want the task kept, unconfirmed and interrupted, no run served",
			stored.Pending, stored.ServingGeneration)
	}
}
