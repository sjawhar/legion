package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A late refusal is Oh My Pi refusing a prompt after it acknowledged it, or a refusal of an earlier
// connection's prompt that the shim replays from its backlog. These tests cover what it clears,
// takes back and charges, in every claim state and across reconnects and restarts.

// A refusal is judged against the prompt that set the mark in every claim state. The process can
// die after the acknowledgement, so the given-up prompt's refusal lands while the claim is
// relaunching - and the relaunch's first send can then be lost. If the refusal were ignored for
// the state it landed in, the mark would stand with nothing left to clear it, and a claim serving
// no run would be credited with a task nobody read. The third expiry of the turn's wait retires
// and relaunches in one handler, and lands the refusal in the same state.
func TestARefusalLandingWhileTheClaimRelaunchesClearsTheMark(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires int
		end     func(h *harness)
	}{
		{name: "the process dies after the wait runs out", expires: 1, end: func(h *harness) { h.observe(runtime.Gone) }},
		{name: "the third expiry retires and relaunches", expires: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			var marking fake.Prompt
			for i := 0; i < tc.expires; i++ {
				base := len(h.prompts())
				if i == 0 {
					h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
				} else {
					h.observe(runtime.Alive)
				}
				marking = h.wantPrompts(base + 1)[base]
				h.advance(2 * testRPC)
			}
			if tc.end != nil {
				tc.end(h)
			}
			if state := h.state(); state != StateLaunching {
				t.Fatalf("after the process ended the claim is %s, want launching", state)
			}
			if p := h.pending(); p.DeliveredAt.IsZero() {
				t.Fatalf("pending before the refusal = %+v, want the given-up prompt's mark standing", p)
			}

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: marking.DeliveryID, Error: "the model provider refused the request"})
			h.conn.FailPrompt(errBoom)
			h.reach(StateReady)

			if p := h.pending(); !p.DeliveredAt.IsZero() {
				t.Fatalf("pending after the relaunch's lost send = %+v, want the given-up prompt's mark gone", p)
			}
			if run := h.claim().ServingRun(); run != 0 {
				t.Fatalf("the claim serves run %d, want none: nobody read the task", run)
			}
		})
	}
}

// The prompt that set the mark is recorded with the task, not in the daemon's memory: a refusal
// OMP gave while the daemon was down reaches the restarted daemon, and it must still be judged
// against the prompt that set the mark.
func TestAfterARestartTheMarkingPromptsRefusalStillClearsTheMark(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)
	if p := h.pending(); p.DeliveredAt.IsZero() || p.ID == first.DeliveryID {
		t.Fatalf("pending after the wait ran out = %+v, want it marked under a new id", p)
	}

	h.restart()
	replacement := fake.NewConn()
	h.conns.Register(testToken, replacement)
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request", Replayed: true,
		ConnSequence: replacement.Sequence()})

	if p := h.pending(); !p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the refusal on the restarted daemon = %+v, want its read mark gone", p)
	}
	if run := h.claim().ServingRun(); run != 0 {
		t.Fatalf("the claim serves run %d, want none: nobody read the task", run)
	}
}

// A refusal replayed from the shim's backlog answers a prompt an earlier connection sent - after a
// restart, possibly of the very delivery the restarted daemon is re-sending under the same id. It
// may clear the mark that prompt set and nothing more: the re-send in flight is a new prompt, so
// the task is prompted once, its acknowledgement marks the task again, and the turn it starts
// serves the task's run.
func TestAReplayedRefusalOnlyClearsTheMarkItsPromptSet(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0]

	h.restart()
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, StreamHello{Claim: testToken, Generation: h.generation()}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the re-send", gated.entered)
	// The backlog is read before the re-send is answered.
	if err := h.m.Handle(h.ctx, StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID,
		Error: "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion.", Replayed: true,
		ConnSequence: gated.Sequence()}); err != nil {
		t.Fatal(err)
	}
	gated.release <- struct{}{}
	h.m.Wait()

	if sent := gated.Prompts(); len(sent) != 1 || sent[0].DeliveryID != first.DeliveryID {
		t.Fatalf("prompts after the restart = %+v, want the one re-send of %s", sent, first.DeliveryID)
	}
	if p := h.pending(); p.ID != first.DeliveryID || p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the re-send was acknowledged = %+v, want it marked by the re-send", p)
	}
	h.must(StreamTurnStart{Claim: testToken})
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("the claim serves run %d, want the task's 7", run)
	}
}

// Oh My Pi answers a delivery's prompt once, and the shim fans that answer out to every request
// of the delivery: the re-send this connection is waiting on, and the earlier request whose send
// was lost in transit, which the shim replays. The re-send's refusal charges the prompt; the
// replayed copy of the same answer charges nothing more.
func TestAReplayedCopyOfAnAnsweredRefusalChargesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.conn.FailPrompt(errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0]
	h.conn.RefusePrompt("the model provider refused the request")
	h.observe(runtime.Alive)
	if second := h.wantPrompts(2)[1]; second.DeliveryID != first.DeliveryID {
		t.Fatalf("the re-send went under %s, want the lost send's %s", second.DeliveryID, first.DeliveryID)
	}
	charged := h.claim().Budgets

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request", Replayed: true,
		ConnSequence: h.conn.Sequence()})

	if got := h.claim().Budgets; got != charged {
		t.Fatalf("budgets after the replayed copy = %+v, want %+v: one refusal is one charge", got, charged)
	}
}

// A replayed refusal is judged only against the prompt that set the mark, never against the
// pending id: the wait for a turn re-queued the task under a new id, keeping the mark the first
// prompt's acknowledgement set, and the new id's first send was lost in transit. A refusal of that
// lost send, replayed on the next connection, says nothing about the first prompt, so the mark
// stands.
func TestAReplayedRefusalOfTheReSentDeliveryLeavesTheEarlierMark(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0]
	h.advance(2 * testRPC)
	h.conn.FailPrompt(errBoom)
	h.observe(runtime.Alive)
	resent := h.wantPrompts(2)[1]
	if p := h.pending(); p.ID != resent.DeliveryID || p.MarkedBy != first.DeliveryID {
		t.Fatalf("pending after the lost re-send = %+v, want %s pending under %s's mark", p, resent.DeliveryID, first.DeliveryID)
	}

	next := fake.NewConn()
	h.conns.Register(testToken, next)
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: resent.DeliveryID, Error: "Agent is busy", Replayed: true,
		ConnSequence: next.Sequence()})

	if p := h.pending(); p.DeliveredAt.IsZero() || p.MarkedBy != first.DeliveryID {
		t.Fatalf("pending after the replayed refusal = %+v, want the first prompt's mark standing", p)
	}
}

// A turn that is not the task's can confirm it: an agent_start the stream reports before the
// prompt's acknowledgement, a turn already running when the prompt lands, or a restart whose ask
// finds a turn running. When the replayed refusal of that prompt then arrives, it says the prompt
// never ran, so the confirmation was the other turn's: the task goes back unread, charged nothing,
// and is sent again when that turn ends - never retired as served by it. A confirmed delivery is
// never being re-sent, so this is not the re-send the replayed-refusal fence protects.
func TestAReplayedRefusalTakesBackATaskAForeignTurnConfirmed(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	for _, tc := range []struct {
		name string
		// confirm delivers the task, confirms it by a turn that is not its own, and returns the
		// delivery id the refused prompt carried and the replacement connection the refusal replays
		// on, which prompts go to from then on.
		confirm func(t *testing.T, h *harness) (string, *fake.Conn)
	}{
		{name: "acknowledged, then a foreign turn starts", confirm: func(t *testing.T, h *harness) (string, *fake.Conn) {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			id := h.wantPrompts(1)[0].DeliveryID
			h.must(StreamTurnStart{Claim: testToken})
			return id, reconnect(h, false)
		}},
		{name: "acknowledged, a foreign turn starts, and the daemon restarts in it", confirm: func(t *testing.T, h *harness) (string, *fake.Conn) {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			id := h.wantPrompts(1)[0].DeliveryID
			h.must(StreamTurnStart{Claim: testToken})
			h.restart()
			return id, reconnect(h, true)
		}},
		{name: "a foreign turn starts before the acknowledgement", confirm: func(t *testing.T, h *harness) (string, *fake.Conn) {
			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
				t.Fatal(err)
			}
			id := waitFor(t, "the send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
				t.Fatal(err)
			}
			gated.release <- struct{}{}
			h.m.Wait()
			// Every later prompt goes straight through.
			go func() {
				for range gated.entered {
					gated.release <- struct{}{}
				}
			}()
			return id, reconnect(h, true)
		}},
		{name: "a restart's ask finds a foreign turn running", confirm: func(t *testing.T, h *harness) (string, *fake.Conn) {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			id := h.wantPrompts(1)[0].DeliveryID
			h.restart()
			return id, reconnect(h, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			id, conn := tc.confirm(t, h)
			if p := h.pending(); p.ConfirmedAt.IsZero() {
				t.Fatalf("pending before the refusal = %+v, want it confirmed by the foreign turn", p)
			}
			charged := h.claim().Budgets
			sent := len(conn.Prompts())

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: id, Error: busy, Replayed: true, ConnSequence: conn.Sequence()})
			if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending after the replayed refusal = %+v, want it taken back unread", p)
			}
			conn.SetStreaming(false)
			h.must(StreamTurnEnd{Claim: testToken})

			if c := h.claim(); c.Pending == nil || c.ServingGeneration != 0 || c.Budgets != charged {
				t.Fatalf("after the foreign turn: pending %+v serving %d budgets %+v, want the task kept, no run served, nothing charged",
					c.Pending, c.ServingGeneration, c.Budgets)
			}
			testwait.Eventually(t, "the task to be sent again", func() bool { return len(conn.Prompts()) > sent })
		})
	}
}

// A refusal can land after settle retired the task it names: the suspension's retire, or the
// turn end's, failed and left the task held, so the fence let the refusal through, and the settle
// before the row retired it. The row then finds no pending task, and it must have nothing to do
// rather than end the daemon.
func TestARefusalOfATaskRetiredUnderItHasNothingToDo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replayed bool
		hold     func(h *harness) string
	}{
		{name: "suspended", hold: func(h *harness) string {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing, Generation: 7})
			id := h.pending().ID
			h.store.fail("RetireDelivery", errBoom)
			_ = h.handle(RequestSuspend{Claim: testToken})
			return id
		}},
		{name: "suspended, replayed", replayed: true, hold: func(h *harness) string {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing, Generation: 7})
			id := h.pending().ID
			h.store.fail("RetireDelivery", errBoom)
			_ = h.handle(RequestSuspend{Claim: testToken})
			return id
		}},
		{name: "idle", hold: func(h *harness) string {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			id := h.pending().ID
			h.must(StreamTurnStart{Claim: testToken})
			h.store.fail("RetireDelivery", errBoom)
			_ = h.handle(StreamTurnEnd{Claim: testToken})
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			id := tc.hold(h)
			if h.claim().Pending == nil {
				t.Fatal("the failed retire left no task held")
			}
			h.store.fail("RetireDelivery", nil)

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: id, Error: "the model provider refused the request", Replayed: tc.replayed})

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending after the refusal = %+v, want the task retired", p)
			}
		})
	}
}

// While a send is in flight, a turn that starts may be that send's own, and a replayed refusal
// naming the same delivery answers an earlier connection's prompt of it. It must not take the task
// from under the turn working it, and it clears only a mark its own prompt set.
func TestAReplayedRefusalWhileTheSendIsInFlightLeavesTheConfirmedTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		// start delivers the task and returns the gated connection the next send is held on and the
		// prompt whose acknowledgement will stand as the mark when the refusal lands.
		start func(t *testing.T, h *harness) (*gatedConn, string)
	}{
		{name: "the restart's re-send of the prompt that set the mark", start: func(t *testing.T, h *harness) (*gatedConn, string) {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			first := h.wantPrompts(1)[0].DeliveryID
			h.restart()
			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, StreamHello{Claim: testToken, Generation: h.generation()}); err != nil {
				t.Fatal(err)
			}
			return gated, first
		}},
		{name: "a re-send under a new id, the first prompt's mark kept", start: func(t *testing.T, h *harness) (*gatedConn, string) {
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			first := h.wantPrompts(1)[0].DeliveryID
			h.advance(2 * testRPC)
			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
				t.Fatal(err)
			}
			return gated, first
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			gated, marking := tc.start(t, h)
			resent := waitFor(t, "the re-send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
				t.Fatal(err)
			}
			if p := h.pending(); p.ID != resent || p.ConfirmedAt.IsZero() || p.MarkedBy != marking {
				t.Fatalf("pending once the turn started = %+v, want %s confirmed under %s's mark", p, resent, marking)
			}

			if err := h.m.Handle(h.ctx, StreamLateRefusal{Claim: testToken, DeliveryID: resent, Error: "Agent is busy", Replayed: true,
				ConnSequence: gated.Sequence()}); err != nil {
				t.Fatal(err)
			}

			p := h.pending()
			if p.ID != resent || p.ConfirmedAt.IsZero() {
				t.Fatalf("pending after the replayed refusal = %+v, want %s still confirmed by the turn working it", p, resent)
			}
			if named := resent == marking; !named && p.MarkedBy != marking {
				t.Fatalf("pending after the replayed refusal = %+v, want %s's mark kept: the refusal names another prompt", p, marking)
			}
			gated.release <- struct{}{}
			h.m.Wait()
		})
	}
}

// A send lost with its connection keeps its id, and the replacement connection's hello re-sends
// it. The shim's backlog can then replay a turn of the agent's own that started while no daemon
// was connected - which confirms the task, since the re-send is in flight - and Oh My Pi's busy
// refusal of the lost send. The re-send's acknowledgement is posted by its own goroutine, not
// through the stream, so it can be handled before, between or after the two replayed events. In
// every order the re-send is this connection's own prompt of the task, so the refusal of the lost
// one takes nothing back: the task is prompted once more, never again, and its own turn serves it.
// So too when yet another connection's hello comes while the re-send is out: a send in flight may
// be the prompt whose turn confirmed the task, whichever connection carried it.
func TestAReplayedRefusalLeavesATaskThisConnectionReSent(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	for _, ack := range []string{
		"before the turn starts", "between the start and the refusal", "after the refusal",
		"after another connection's hello and the refusal",
	} {
		t.Run("the re-send acknowledged "+ack, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.conn.FailPrompt(errBoom)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			d := h.pending().ID
			h.conn.FailPrompt(nil)
			charged := h.claim().Budgets

			gated := newGatedConn()
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, StreamHello{Claim: testToken, Generation: h.generation()}); err != nil {
				t.Fatal(err)
			}
			if got := waitFor(t, "the re-send", gated.entered); got != d {
				t.Fatalf("re-sent %s, want %s", got, d)
			}
			acknowledge := func() {
				gated.release <- struct{}{}
				h.m.Wait()
			}
			// Handled without waiting, since the re-send's goroutine is held until it is acknowledged.
			replay := func(ev Event) {
				if err := h.m.Handle(h.ctx, ev); err != nil {
					t.Fatalf("%T: %v", ev, err)
				}
			}
			if ack == "before the turn starts" {
				acknowledge()
			}
			replay(StreamTurnStart{Claim: testToken}) // the agent's own turn
			if ack == "between the start and the refusal" {
				acknowledge()
			}
			if ack == "after another connection's hello and the refusal" {
				replay(StreamHello{Claim: testToken, Generation: h.generation()})
			}
			replay(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: gated.Sequence()})
			if strings.HasPrefix(ack, "after") {
				acknowledge()
			}

			// From here on Oh My Pi is in the task's own turn, so any further prompt is refused busy.
			gated.Conn.RefusePrompt(busy)
			go func() {
				for range gated.entered {
					gated.release <- struct{}{}
				}
			}()
			h.must(StreamTurnEnd{Claim: testToken})   // the agent's own turn ends
			h.must(StreamTurnStart{Claim: testToken}) // the task's own turn, from the re-send
			if run := h.claim().ServingRun(); run != 7 {
				t.Fatalf("while the task's own turn runs, the claim serves run %d, want 7", run)
			}
			h.must(StreamTurnEnd{Claim: testToken})

			if prompts := gated.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != d {
				t.Fatalf("prompts after the reconnect = %+v, want only the re-send of %s", prompts, d)
			}
			if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 || c.Budgets != charged {
				t.Fatalf("after the task's turn: pending %+v serving %d budgets %+v, want run 7 served and nothing charged",
					c.Pending, c.ServingGeneration, c.Budgets)
			}
		})
	}
}

// A send's outcome can arrive after settle retired its task: the turn end's retire failed and
// left the task held, and the next event's settle retires it. The acknowledgement then has no
// task to mark, and it must record nothing rather than end the daemon.
func TestALateAcknowledgementOfARetiredTaskRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	id := waitFor(t, "the send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	h.store.fail("RetireDelivery", errBoom)
	if err := h.m.Handle(h.ctx, StreamTurnEnd{Claim: testToken}); err == nil {
		t.Fatal("the turn end's retire succeeded, want it to fail and leave the task held")
	}
	h.store.fail("RetireDelivery", nil)
	if h.claim().Pending == nil {
		t.Fatal("the task was retired, want it held after the failed retire")
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the late acknowledgement panicked: %v", r)
			}
		}()
		if err := h.m.Handle(h.ctx, PromptAcked{Claim: testToken, Generation: h.generation(), DeliveryID: id}); err != nil {
			t.Fatalf("the late acknowledgement: %v", err)
		}
	}()
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the late acknowledgement: pending %+v serving %d, want run 7 served", c.Pending, c.ServingGeneration)
	}
	gated.release <- struct{}{}
	h.m.Wait()
}

// A replayed refusal of a prompt no turn confirmed takes nothing back, even from a connection that
// has not prompted the task: the prompt was acknowledged, so its turn wait is armed and keeps the
// hello from re-sending, and the refusal clears the mark it set. The task keeps its id and its
// wait, and the prompt is charged when that wait runs out, as any acknowledged prompt that started
// no turn is.
func TestAReplayedRefusalOfAnUnconfirmedTaskKeepsItsWait(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	d := h.wantPrompts(1)[0].DeliveryID
	charged := h.claim().Budgets
	replacement := reconnect(h, false)
	if prompts := replacement.Prompts(); len(prompts) != 0 {
		t.Fatalf("the hello re-sent %+v, want the armed turn wait to hold it", prompts)
	}

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: replacement.Sequence()})
	if p := h.pending(); p.ID != d || !p.DeliveredAt.IsZero() || h.claim().Budgets != charged {
		t.Fatalf("after the replayed refusal: pending %+v budgets %+v, want %s kept, its mark cleared, nothing charged",
			p, h.claim().Budgets, d)
	}
	h.advance(2 * testRPC)
	if got := h.claim().Budgets.PromptFailures; got != charged.PromptFailures+1 {
		t.Fatalf("after the turn wait: %d prompt failures, want %d", got, charged.PromptFailures+1)
	}
}

// A replayed refusal takes back a task a turn not its own confirmed only when it names that task's
// prompt: one naming any other delivery says nothing about the prompt that confirmed it.
func TestAReplayedRefusalOfAnotherDeliveryLeavesAConfirmedTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	h.wantPrompts(1)
	h.restart()
	replacement := reconnect(h, true)
	before := h.pending()
	if before.ConfirmedAt.IsZero() {
		t.Fatalf("pending before the refusal = %+v, want it confirmed by the running turn", before)
	}

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: "delivery-elsewhere", Error: "refused", Replayed: true, ConnSequence: replacement.Sequence()})
	if p := h.pending(); p.ID != before.ID || !p.ConfirmedAt.Equal(before.ConfirmedAt) || !p.DeliveredAt.Equal(before.DeliveredAt) {
		t.Fatalf("pending after the refusal = %+v, want it untouched: %+v", p, before)
	}
}

// A sweep can send the task through the replacement connection before the machine handles that
// connection's hello: the listener registers a connection before it queues the hello, and
// observations reach the machine on their own path. The refusal the backlog then replays, of the
// lost first send, arrives on the connection that sent the re-send, so it takes nothing back
// whichever came first, on a live daemon or a restarted one.
func TestAReplayedRefusalLeavesATaskItsConnectionSentBeforeItsHello(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	for _, restart := range []bool{false, true} {
		name := "a live daemon"
		if restart {
			name = "a restarted daemon"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			if restart {
				h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			} else {
				h.conn.FailPrompt(errBoom)
				h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
				h.conn.FailPrompt(nil)
			}
			d := h.pending().ID
			if restart {
				h.restart()
			}
			charged := h.claim().Budgets

			replacement := newGatedConn()
			h.conns.Register(testToken, replacement)
			handle := func(ev Event) {
				if err := h.m.Handle(h.ctx, ev); err != nil {
					t.Fatalf("%T: %v", ev, err)
				}
			}
			handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
			if got := waitFor(t, "the sweep's send", replacement.entered); got != d {
				t.Fatalf("the sweep sent %s, want %s", got, d)
			}
			handle(StreamHello{Claim: testToken, Generation: h.generation()})
			handle(StreamTurnStart{Claim: testToken}) // the agent's own turn, replayed
			replacement.release <- struct{}{}
			h.m.Wait()
			handle(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: replacement.Sequence()})

			replacement.Conn.RefusePrompt(busy)
			go func() {
				for range replacement.entered {
					replacement.release <- struct{}{}
				}
			}()
			h.must(StreamTurnEnd{Claim: testToken})   // the agent's own turn ends
			h.must(StreamTurnStart{Claim: testToken}) // the task's own turn, from the sweep's send
			if run := h.claim().ServingRun(); run != 7 {
				t.Fatalf("while the task's own turn runs, the claim serves run %d, want 7", run)
			}
			h.must(StreamTurnEnd{Claim: testToken})
			if prompts := replacement.Prompts(); len(prompts) != 1 {
				t.Fatalf("prompts through the replacement = %+v, want only the sweep's send", prompts)
			}
			if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 || c.Budgets != charged {
				t.Fatalf("after the task's turn: pending %+v serving %d budgets %+v, want run 7 served and nothing charged",
					c.Pending, c.ServingGeneration, c.Budgets)
			}
		})
	}
}

// reconnect is the shim's replacement connection: registered, then its hello handled, as the
// listener does. streaming is whether the agent is in a turn when it connects.
func reconnect(h *harness, streaming bool) *fake.Conn {
	h.t.Helper()
	replacement := fake.NewConn()
	replacement.SetStreaming(streaming)
	h.conns.Register(testToken, replacement)
	h.must(StreamHello{Claim: testToken, Generation: h.generation()})
	return replacement
}

// What a refusal established holds in memory even when its write fails, and the write is made
// again before the next decision, so once the store takes writes again a restart reads the task as
// the refusal left it: a task taken back is not retired as served by the turn that confirmed it,
// and a mark that was cleared does not come back.
func TestARefusalWhoseWriteFailedIsWrittenBeforeARestartReadsIt(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	t.Run("a task taken back from a foreign turn", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
		d := h.wantPrompts(1)[0].DeliveryID
		h.must(StreamTurnStart{Claim: testToken})
		replacement := reconnect(h, true)
		h.store.fail("PutDelivery", errBoom)
		_ = h.handle(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: replacement.Sequence()})
		h.store.fail("PutDelivery", nil)
		h.observe(runtime.Alive)

		h.restart()
		reconnect(h, false)
		if c := h.claim(); c.Pending == nil || c.ServingGeneration == 7 {
			t.Fatalf("after the restart: pending %+v serving %d, want the task still waiting, not served", c.Pending, c.ServingGeneration)
		}
	})
	t.Run("a mark cleared", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
		d := h.wantPrompts(1)[0].DeliveryID
		replacement := reconnect(h, false)
		h.store.fail("PutDelivery", errBoom)
		_ = h.handle(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: replacement.Sequence()})
		h.store.fail("PutDelivery", nil)
		h.observe(runtime.Alive)

		h.restart()
		if p := h.pending(); !p.DeliveredAt.IsZero() || p.MarkedBy != "" {
			t.Fatalf("after the restart: pending %+v, want the refused prompt's mark still cleared", p)
		}
	})
}

// The shim redials at once, so the connection prompts go through can be newer than the one whose
// queued events the machine is handling: a lost first send, then C2's backlog replaying a turn of
// the agent's own and the lost send's busy refusal, then C3 registered before C2's hello is
// handled, so that hello - or a sweep queued ahead of C2's frames - re-sends the task through C3.
// The refusal arrived on C2, but the task was sent through a newer connection since, so whichever
// sent it and whenever C3's acknowledgement is handled, the refusal takes nothing back.
func TestAReplayedRefusalLeavesATaskANewerConnectionReSent(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	for _, tc := range []struct{ send, ack string }{
		{"C2's hello", "before the turn starts"}, {"C2's hello", "between the start and the refusal"}, {"C2's hello", "after the refusal"},
		{"a sweep", "before the turn starts"}, {"a sweep", "between the start and the refusal"}, {"a sweep", "after the refusal"},
	} {
		ack := tc.ack
		t.Run(tc.send+" re-sends, acknowledged "+ack, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.conn.FailPrompt(errBoom)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			d := h.pending().ID
			h.conn.FailPrompt(nil)
			charged := h.claim().Budgets

			backlog := fake.NewConn() // C2, whose events the machine is about to handle
			h.conns.Register(testToken, backlog)
			newest := newGatedConn() // C3, registered before C2's hello is handled
			h.conns.Register(testToken, newest)
			handle := func(ev Event) {
				if err := h.m.Handle(h.ctx, ev); err != nil {
					t.Fatalf("%T: %v", ev, err)
				}
			}
			if tc.send == "a sweep" {
				handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
			} else {
				handle(StreamHello{Claim: testToken, Generation: h.generation()})
			}
			if got := waitFor(t, "the re-send", newest.entered); got != d {
				t.Fatalf("re-sent %s, want %s", got, d)
			}
			if tc.send == "a sweep" {
				handle(StreamHello{Claim: testToken, Generation: h.generation()}) // C2's, queued behind the sweep
			}
			acknowledge := func() {
				newest.release <- struct{}{}
				h.m.Wait()
			}
			if ack == "before the turn starts" {
				acknowledge()
			}
			handle(StreamTurnStart{Claim: testToken})
			if ack == "between the start and the refusal" {
				acknowledge()
			}
			handle(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: backlog.Sequence()})
			if ack == "after the refusal" {
				acknowledge()
			}

			newest.Conn.RefusePrompt(busy)
			go func() {
				for range newest.entered {
					newest.release <- struct{}{}
				}
			}()
			h.must(StreamTurnEnd{Claim: testToken})
			h.must(StreamTurnStart{Claim: testToken})
			if run := h.claim().ServingRun(); run != 7 {
				t.Fatalf("while the task's own turn runs, the claim serves run %d, want 7", run)
			}
			h.must(StreamTurnEnd{Claim: testToken})
			if prompts := newest.Prompts(); len(prompts) != 1 {
				t.Fatalf("prompts through the newest connection = %+v, want only the re-send", prompts)
			}
			if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 || c.Budgets != charged {
				t.Fatalf("after the task's turn: pending %+v serving %d budgets %+v, want run 7 served and nothing charged",
					c.Pending, c.ServingGeneration, c.Budgets)
			}
		})
	}
}

// confirm writes the claim and the delivery together, and its retry does too: a delivery written
// confirmed beside a claim still ready is a turn a restart finds over, and it would retire the task
// as served while the turn still runs - the task's own, or one whose refusal then finds no task.
func TestAFailedConfirmIsWrittenAgainWholeBeforeARestartReadsIt(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	for _, foreign := range []bool{false, true} {
		name := "the task's own turn"
		if foreign {
			name = "a foreign turn"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			d := h.wantPrompts(1)[0].DeliveryID
			h.store.fail("PutClaim", errBoom)
			_ = h.handle(StreamTurnStart{Claim: testToken})
			h.store.fail("PutClaim", nil)
			h.observe(runtime.Alive)

			h.restart()
			replacement := reconnect(h, true)
			if c := h.claim(); c.Pending == nil || c.ServingGeneration == 7 {
				t.Fatalf("after the restart: pending %+v serving %d, want the task held while its turn runs", c.Pending, c.ServingGeneration)
			}
			if !foreign {
				return
			}
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: d, Error: busy, Replayed: true, ConnSequence: replacement.Sequence()})
			if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending after the replayed refusal = %+v, want it taken back unread", p)
			}
		})
	}
}
