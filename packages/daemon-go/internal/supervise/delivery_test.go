package supervise

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestATaskQueuedBeforeReadyIsDeliveredOnceAtReady(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	queued := h.pending()
	if stored, ok := h.store.delivery(testToken); !ok || stored.ID != queued.ID || stored.Task != "implement the plan" {
		t.Fatalf("stored delivery %+v, want the queued task", stored)
	}
	h.connect()
	h.register()
	h.wantPrompts(0)

	h.ready()

	sent := h.wantPrompts(1)[0]
	if sent.DeliveryID != queued.ID || sent.Message != "implement the plan" {
		t.Errorf("sent %+v, want the queued task under its delivery id", sent)
	}
	if h.pending().DeliveredAt.IsZero() {
		t.Error("the acknowledged delivery has no DeliveredAt")
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("the turn did not confirm the delivery")
	}
	if stored, _ := h.store.delivery(testToken); stored.ConfirmedAt.IsZero() {
		t.Error("the confirmation was not persisted")
	}

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v after its turn ended, want none", p)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the store still holds the delivery its turn finished")
	}
	h.advance(10 * testRPC)
	h.wantPrompts(1)
	h.wantBudgets(Budgets{})
}

func TestADeliveryToAnIdleClaimIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})
	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Errorf("sent %+v, want the next task", sent)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("the idle claim's turn did not confirm its delivery")
	}
}

func TestATaskGivenBeforeReadyWaitsForReady(t *testing.T) {
	for _, state := range []ClaimState{StateQueued, StateLaunching, StateShimConnected, StateRegistered} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			h.wantPrompts(0)
			h.wantState(state)
			h.reach(StateReady)
			if sent := h.wantPrompts(1)[0]; sent.Message != "the task" || sent.DeliveryID != h.pending().ID {
				t.Errorf("sent %+v at ready, want the task queued before it", sent)
			}
		})
	}
}

// A second delivery is refused for the delivery the claim already holds, whatever its state: the
// refusal is ErrDeliveryPending, and it says so rather than naming the state.
func TestASecondDeliveryWhileOneIsPendingIsRefused(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)

	err := h.handle(RequestDeliver{Claim: testToken, Task: "another"})

	var refused *RefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrDeliveryPending) || err.Error() != "deliver refused: a delivery is already pending" {
		t.Fatalf("a second delivery returned %v, want the pending-delivery refusal", err)
	}
	if h.pending().Task != "implement the plan" {
		t.Errorf("pending %+v, want the first task kept", h.pending())
	}
}

func TestASendWithNoConnectionRequeuesWithoutACharge(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conns.Remove(testToken)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			h.wantPrompts(base)
			h.wantBudgets(Budgets{})
			if !h.pending().DeliveredAt.IsZero() {
				t.Error("a delivery with no connection reads as delivered")
			}
			if lines := h.logs.lines("no connection"); len(lines) != 1 {
				t.Errorf("logged %v, want the missing connection once", lines)
			}

			h.connect()

			if sent := h.wantPrompts(base + 1)[base]; sent.DeliveryID != h.pending().ID {
				t.Errorf("sent %+v, want the queued delivery", sent)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A prompt lost to the transport was never the agent's answer: it waits for the shim's next
// hello, uncharged, under the same id.
func TestATransportErrorRequeuesWithoutACharge(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conn.FailPrompt(errBoom)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			first := h.wantPrompts(base + 1)[base]
			h.wantBudgets(Budgets{})
			if !h.pending().DeliveredAt.IsZero() {
				t.Error("a lost delivery reads as delivered")
			}
			h.advance(10 * testRPC)
			h.wantPrompts(base + 1)

			h.conn.FailPrompt(nil)
			h.connect()

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != first.DeliveryID {
				t.Errorf("re-sent under %s, want the same delivery id %s", second.DeliveryID, first.DeliveryID)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A prompt the agent refuses is charged like an acknowledged prompt that started no turn
// (LEGION-60), and retried at the next sweep rather than on the spot: a merely busy agent is not
// re-prompted into another refusal.
func TestARefusedPromptIsChargedAndRetriedAtTheNextSweep(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conn.RefusePrompt("agent busy")

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			first := h.wantPrompts(base + 1)[base]
			h.wantBudgets(Budgets{PromptFailures: 1})
			if p := h.pending(); p.ID != first.DeliveryID || !p.DeliveredAt.IsZero() {
				t.Errorf("pending %+v, want the refused delivery queued under its own id", p)
			}
			h.advance(10 * testRPC)
			h.wantPrompts(base + 1)
			h.wantState(state)

			h.conn.FailPrompt(nil)
			h.observe(runtime.Alive)

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != first.DeliveryID {
				t.Errorf("re-sent under %s, want %s", second.DeliveryID, first.DeliveryID)
			}
			h.wantBudgets(Budgets{PromptFailures: 1})
		})
	}
}

// An agent that refuses every prompt is not retried forever: its refusals spend the prompt
// budget, and the retirement that follows is the same as for prompts that start no turn.
func TestAnAgentThatRefusesEveryPromptRunsOutOfItsBudget(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	refusing := h.locator()
	h.conn.RefusePrompt("agent busy")
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.observe(runtime.Alive)
	h.wantBudgets(Budgets{PromptFailures: 2})

	h.observe(runtime.Alive)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != refusing {
		t.Errorf("suspended %+v, want the refusing process", suspend.Locator)
	}
	h.wantCalls("Resume", 1)
	h.wantBudgets(Budgets{PromptRetires: 1})
	h.wantState(StateLaunching)
}

// The sweep that finds a ready or idle claim's process alive is what retries a delivery waiting
// for its connection when nothing else will: no hello, no turn end, no request.
func TestAnAliveSweepSendsAWaitingDelivery(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.conns.Remove(testToken)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			h.observe(runtime.Alive)
			h.wantPrompts(base)

			h.conns.Register(testToken, h.conn)
			h.observe(runtime.Alive)

			if sent := h.wantPrompts(base + 1)[base]; sent.DeliveryID != h.pending().ID {
				t.Errorf("sent %+v, want the waiting delivery", sent)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

func TestAnAckWithNoTurnChargesOneFailureAndRotatesTheDelivery(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			first := h.wantPrompts(base + 1)[base]

			h.advance(testRPC)

			h.wantBudgets(Budgets{PromptFailures: 1})
			rotated := h.pending()
			if rotated.ID == first.DeliveryID {
				t.Fatal("the delivery id was not rotated after an acknowledged prompt started no turn")
			}
			if stored, _ := h.store.delivery(testToken); stored.ID != rotated.ID {
				t.Errorf("stored delivery id %s, want the rotated %s", stored.ID, rotated.ID)
			}
			h.wantPrompts(base + 1) // not re-sent on the spot: a merely slow agent is not re-prompted at once
			h.wantState(state)

			h.observe(runtime.Alive)

			if second := h.wantPrompts(base + 2)[base+1]; second.DeliveryID != rotated.ID || second.Message != "the task" {
				t.Errorf("re-sent %+v at the sweep, want the task under the rotated id", second)
			}
		})
	}
}

// B1: the agent's own agent_start carries no delivery id and confirms the delivery in flight —
// here before the machine has even heard the send's acknowledgement.
func TestARealTurnStartConfirmsTheDeliveryInFlight(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)

	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	inFlight := waitFor(t, "the send", gated.entered)
	// Handle, not must: must waits for the send, which the gate is holding.
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	h.wantState(StateWorking)
	if p := h.pending(); p.ID != inFlight || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the in-flight delivery %s confirmed", p, inFlight)
	}
	gated.release <- struct{}{}
	h.m.Wait()

	if live := h.clock.Live(); live != 0 {
		t.Errorf("%d timers armed after the late acknowledgement of a confirmed delivery", live)
	}
	h.advance(10 * testRPC)
	h.wantBudgets(Budgets{})
	h.wantState(StateWorking)
}

// B1: only the shim's replayed agent_start carries a delivery id; one carrying an id the claim
// has moved past is dropped and logged once, and one carrying the current id confirms it.
func TestAReplayedTurnStartIsFencedOnItsDeliveryID(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	stale := h.wantPrompts(1)[0].DeliveryID
	h.advance(testRPC)
	h.observe(runtime.Alive)
	current := h.wantPrompts(2)[1].DeliveryID

	h.must(StreamTurnStart{Claim: testToken, DeliveryID: stale})
	h.must(StreamTurnStart{Claim: testToken, DeliveryID: stale})

	h.wantState(StateReady)
	if !h.pending().ConfirmedAt.IsZero() {
		t.Fatal("a replay carrying a stale delivery id confirmed the current delivery")
	}
	if lines := h.logs.lines("dropped a stale event", stale); len(lines) != 1 {
		t.Errorf("the stale replay was logged %d times, want once: %v", len(lines), lines)
	}

	h.must(StreamTurnStart{Claim: testToken, DeliveryID: current})
	h.wantState(StateWorking)
	if h.pending().ConfirmedAt.IsZero() {
		t.Error("a replay carrying the current delivery id did not confirm it")
	}
}

func TestAForeignTurnDoesNotConfirmADeliveryNeverSent(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.conns.Remove(testToken)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})

	h.must(StreamTurnStart{Claim: testToken})

	h.wantState(StateWorking)
	if !h.pending().ConfirmedAt.IsZero() {
		t.Fatal("a turn confirmed a delivery that was never sent")
	}
	h.conns.Register(testToken, h.conn)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if sent := h.wantPrompts(1)[0]; sent.DeliveryID != h.pending().ID {
		t.Errorf("sent %+v at the turn's end, want the queued delivery", sent)
	}
}

func TestADeliveryDuringAForeignTurnIsSentWhenTheTurnEnds(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.must(RequestDeliver{Claim: testToken, Task: "after this turn"})
	h.wantPrompts(0)

	h.must(StreamTurnEnd{Claim: testToken})
	if sent := h.wantPrompts(1)[0]; sent.Message != "after this turn" {
		t.Errorf("sent %+v, want the task queued during the turn", sent)
	}
}

// A late refusal under a turn that confirmed the delivery: the turn may be foreign and really
// running, so the claim stays working; only the delivery is taken back, rotated, and charged,
// and it is sent again once that turn ends.
func TestALateRefusalUnderAConfirmingTurnReversesOnlyTheDelivery(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	refused := h.wantPrompts(1)[0].DeliveryID
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: "agent busy"})

	h.wantState(StateWorking)
	h.wantBudgets(Budgets{PromptFailures: 1})
	p := h.pending()
	if p.ID == refused || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the delivery unconfirmed under a rotated id", p)
	}
	if stored, _ := h.store.delivery(testToken); stored.ID != p.ID || !stored.ConfirmedAt.IsZero() {
		t.Errorf("stored delivery %+v, want the reversal persisted", stored)
	}
	h.wantPrompts(1)

	h.must(StreamTurnEnd{Claim: testToken})

	h.wantState(StateIdle)
	if sent := h.wantPrompts(2)[1]; sent.DeliveryID != p.ID || sent.Message != "the task" {
		t.Errorf("sent %+v after the turn, want the task under the rotated id", sent)
	}
}

// A late refusal while the acknowledged prompt is still waiting for its turn: the no-turn timer
// for the refused id is cancelled, so the one failure is charged once, and the rotated delivery
// goes at the next sweep.
func TestALateRefusalWhileAwaitingTheTurnCancelsTheTimerAndChargesOnce(t *testing.T) {
	for _, state := range prompted {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			base := len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			refused := h.wantPrompts(base + 1)[base].DeliveryID
			h.advance(time.Second)

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: refused, Error: "agent busy"})

			h.wantState(state)
			h.wantBudgets(Budgets{PromptFailures: 1})
			rotated := h.pending().ID
			if rotated == refused {
				t.Fatal("the delivery id was not rotated after the late refusal")
			}
			h.advance(testRPC - time.Second) // the refused prompt's timer would have fired here
			h.wantBudgets(Budgets{PromptFailures: 1})
			h.wantPrompts(base + 1)

			h.observe(runtime.Alive)
			if resent := h.wantPrompts(base + 2)[base+1]; resent.DeliveryID != rotated {
				t.Fatalf("re-sent %+v at the sweep, want the task under the rotated id %s", resent, rotated)
			}
			h.advance(testRPC) // the re-sent prompt's own timer
			h.wantBudgets(Budgets{PromptFailures: 2})
		})
	}
}

func TestALateRefusalForAnotherDeliveryIsDroppedAndLoggedOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	held := h.pending().ID

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: "delivery-elsewhere", Error: "busy"})
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: "delivery-elsewhere", Error: "busy"})

	h.wantBudgets(Budgets{})
	if h.pending().ID != held {
		t.Errorf("pending %s, want %s untouched", h.pending().ID, held)
	}
	if lines := h.logs.lines("dropped a stale event", "delivery-elsewhere"); len(lines) != 1 {
		t.Errorf("logged %d times, want once: %v", len(lines), lines)
	}
}

// A prompt refused after a foreign turn already confirmed the send (B1's matching) was never taken:
// the confirmation is taken back and the refusal charged once. A prompt lost to the transport
// takes the confirmation back too, at no charge — the shim answers the same id again without a
// second turn if the turn was in fact the delivery's.
func TestAFailedSendAfterAConfirmingTurnTakesTheConfirmationBack(t *testing.T) {
	for _, failure := range []struct {
		name    string
		script  func(*gatedConn)
		charged Budgets
	}{
		{"refused", func(g *gatedConn) { g.RefusePrompt("agent busy") }, Budgets{PromptFailures: 1}},
		{"transport", func(g *gatedConn) { g.FailPrompt(errBoom) }, Budgets{}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			gated := newGatedConn()
			failure.script(gated)
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
				t.Fatal(err)
			}
			id := waitFor(t, "the send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
				t.Fatal(err)
			}
			h.wantState(StateWorking)

			gated.release <- struct{}{}
			h.m.Wait()

			h.wantState(StateWorking)
			h.wantBudgets(failure.charged)
			if p := h.pending(); p.ID != id || !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending %+v, want %s unconfirmed", p, id)
			}
			gated.FailPrompt(nil)
			gated.release <- struct{}{}
			h.must(StreamTurnEnd{Claim: testToken})
			if prompts := gated.Prompts(); len(prompts) != 2 || prompts[1].DeliveryID != id {
				t.Errorf("prompts %+v, want the delivery sent again under its own id after the turn", prompts)
			}
			h.wantBudgets(failure.charged)
		})
	}
}

func TestPromptOutcomesAndTimersForAStaleDeliveryAreDropped(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	held := h.pending()

	for _, ev := range []Event{
		PromptAcked{Claim: testToken, Generation: 1, DeliveryID: "delivery-elsewhere"},
		PromptRefused{Claim: testToken, Generation: 1, DeliveryID: "delivery-elsewhere", Err: errBoom},
		PromptAcked{Claim: testToken, Generation: 0, DeliveryID: held.ID},
		Timer{Claim: testToken, Kind: TimerTurn, Generation: 1, Seq: 9999, DeliveryID: held.ID},
	} {
		h.must(ev)
	}

	h.wantBudgets(Budgets{})
	h.wantState(StateReady)
	if p := h.pending(); p.ID != held.ID || !p.DeliveredAt.Equal(held.DeliveredAt) {
		t.Errorf("pending %+v, want %+v untouched", p, held)
	}
	if lines := h.logs.lines("dropped a stale event"); len(lines) != 4 {
		t.Errorf("logged %d stale events, want 4: %v", len(lines), lines)
	}
}

// The turn ended and the claim was written idle, but the delivery's retirement never landed. The
// confirmed delivery a restart reads back is finished, and does not stand in the way of the next.
func TestAConfirmedDeliveryLeftBehindByACrashIsRetiredBeforeTheNextTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.store.fail("RetireDelivery", errBoom)
	if err := h.handle(StreamTurnEnd{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("turn end over a failing retirement returned %v, want the store's error", err)
	}
	if stored := h.store.load(testToken); stored.State != StateIdle || stored.Pending == nil || stored.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("stored %+v, want an idle claim still holding its confirmed delivery", stored)
	}
	h.store.fail("RetireDelivery", nil)
	h.restart()

	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})

	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Errorf("sent %+v, want the next task", sent)
	}
}

// A connection drops under a send and the shim reconnects before the send's failure is handled:
// the hello that triggers a re-send came while the send was still out, and still triggers it.
func TestAHelloDuringAFailingSendTriggersTheResend(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	gated.FailPrompt(errBoom)
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamHello{Claim: testToken, Generation: h.generation()}); err != nil {
		t.Fatal(err)
	}

	gated.release <- struct{}{} // the first send fails
	resent := waitFor(t, "the re-send", gated.entered)
	gated.FailPrompt(nil)
	gated.release <- struct{}{}
	h.m.Wait()

	if resent != id || len(gated.Prompts()) != 2 {
		t.Fatalf("re-sent %s after prompts %+v, want the delivery sent again under its own id %s", resent, gated.Prompts(), id)
	}
	h.wantBudgets(Budgets{})
}

// The daemon saw a turn start and went down before it saw the turn end. After the restart the
// claim reads as working; the shim's reconnect is when the machine asks, and a turn that is over
// is ended — the claim does not wait in working for an agent_end it already missed.
func TestAfterARestartATurnThatEndedMeanwhileIsEnded(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.restart()

	h.connect()

	h.wantState(StateIdle)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v, want the finished turn's delivery retired", p)
	}
}

func TestAfterARestartATurnStillRunningStaysWorking(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.restart()
	h.conn.SetStreaming(true)

	h.connect()

	h.wantState(StateWorking)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
}

func TestRepeatOutboxDeliveryDoesNotQueueTheTaskTwice(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:42"})
	first := h.pending()
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:42"})

	if pending := h.pending(); pending.ID != "outbox:42" || pending.ID != first.ID {
		t.Fatalf("pending delivery = %#v, want the original outbox delivery", pending)
	}
	if prompts := h.wantPrompts(1); prompts[0].DeliveryID != "outbox:42" {
		t.Fatalf("prompt = %#v, want outbox delivery id", prompts[0])
	}
}
