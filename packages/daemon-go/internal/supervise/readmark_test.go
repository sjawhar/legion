package supervise

// A pending task's read mark (DeliveredAt) says a worker may be working it, and it is what lets a
// completion from a turn the daemon did not deliver be attributed to that task (Claim.ServingRun).
// These tests pin when the mark is set, kept and cleared, and the run a claim serves because of it.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A task's delivered mark says a worker may have read it, which is what lets a completion from a
// turn the daemon did not deliver be attributed to that task. The bound keeps it: a turn that has
// not started within the wait may still be this task's, starting late. Both refusals drop it: the
// agent refusing in a turn of its own never saw this prompt, and one refused with no turn running
// did not run at all.
func TestOnlyTheTurnBoundLeavesTheTaskMarkedAsRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(h *harness, delivery string)
		read   bool
	}{
		{
			name:   "no turn within the bound",
			refuse: func(h *harness, _ string) { h.advance(2 * testRPC) },
			read:   true,
		},
		{
			name: "refused in a turn of the agent's own",
			refuse: func(h *harness, delivery string) {
				h.must(StreamLateRefusal{Claim: testToken, DeliveryID: delivery, Error: "Agent is already processing"})
			},
		},
		{
			name: "refused with no turn running",
			refuse: func(h *harness, delivery string) {
				h.must(StreamLateRefusal{Claim: testToken, DeliveryID: delivery, Error: "No API key found for provider anthropic"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			base := len(h.prompts())

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			sent := h.wantPrompts(base + 1)[base]
			if p := h.pending(); p.DeliveredAt.IsZero() {
				t.Fatalf("pending after the acknowledgement = %+v, want it marked delivered", p)
			}

			tc.refuse(h, sent.DeliveryID)

			if p := h.pending(); p.DeliveredAt.IsZero() == tc.read {
				t.Fatalf("pending after %s = %+v, want DeliveredAt set = %t", tc.name, p, tc.read)
			}
		})
	}
}

// A refusal can arrive after the wait for the turn ran out: Oh My Pi's own work before a turn
// begins — a slow key command, a compaction — can outlast the bound, and the failure it then
// reports names the delivery it was prompted with. The bound has already taken the task back
// under a new id, and the claim would drop that refusal as stale, leaving the task marked as one
// the agent may have read though its prompt never ran. A completion from whatever turn follows
// would then be attributed to it.
func TestARefusalAfterTheBoundStillTakesTheTaskBackUnread(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	sent := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)
	queued := h.pending()
	if queued.DeliveredAt.IsZero() || queued.ID == sent.DeliveryID {
		t.Fatalf("pending after the bound = %+v, want it marked read under a new id", queued)
	}

	// The refusal names the id it was prompted with, which the take-back replaced.
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: sent.DeliveryID,
		Error: "No API key found for provider anthropic"})

	after := h.pending()
	if !after.DeliveredAt.IsZero() {
		t.Fatalf("pending after the late refusal = %+v, want the delivered mark gone", after)
	}
	// The bound already charged this prompt and already re-queued the task: the refusal changes
	// what the daemon knows about the task, not its id and not its budget. Charging it again would
	// make one slow-then-refused prompt cost one or two depending only on when the answer landed.
	if after.ID != queued.ID {
		t.Fatalf("pending id = %s after the late refusal, want the id the bound queued, %s", after.ID, queued.ID)
	}
	if budgets := h.claim().Budgets; budgets.PromptFailures != 1 {
		t.Fatalf("prompt failures = %d after the late refusal, want the bound's one", budgets.PromptFailures)
	}
}

// The run a claim's worker is on, which is what a completion is attributed to. The order matters
// in both directions: a task whose turn is running answers over the run the claim was left
// serving, and a task the agent may have read answers only for a claim that has served no run —
// a worker holding an unread next run's task while still finishing the last run's work reports
// the last run's, which is the sequence the run fencing exists for.
func TestTheRunAClaimServes(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		claim   Claim
		serving uint64
	}{
		{name: "no task, no run", claim: Claim{}},
		{name: "a run served, no task", claim: Claim{ServingGeneration: 4}, serving: 4},
		{
			name: "the task whose turn is running",
			claim: Claim{ServingGeneration: 4, Pending: &Delivery{
				Generation: 5, DeliveredAt: at, ConfirmedAt: at}},
			serving: 5,
		},
		{
			name:    "a first task the agent may have read",
			claim:   Claim{Pending: &Delivery{Generation: 5, DeliveredAt: at}},
			serving: 5,
		},
		{
			name: "an unread task on a claim already serving a run",
			claim: Claim{ServingGeneration: 4, Pending: &Delivery{
				Generation: 5, DeliveredAt: at}},
			serving: 4,
		},
		{
			name:    "a task the agent refused, which it never read",
			claim:   Claim{Pending: &Delivery{Generation: 5}},
			serving: 0,
		},
		{
			name: "an operator's task, which belongs to no run",
			claim: Claim{ServingGeneration: 4, Pending: &Delivery{
				DeliveredAt: at, ConfirmedAt: at}},
			serving: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claim.ServingRun(); got != tc.serving {
				t.Fatalf("ServingRun() = %d, want %d", got, tc.serving)
			}
		})
	}
}

// A running turn of the task's own is attributed by its confirmation, not by the read mark. When
// the re-send's turn starts before that send is acknowledged — the send is still in flight — and
// only then does the given-up prompt's refusal arrive, the refusal is judged against the prompt
// it names: that prompt never ran, so the mark it set goes. The running turn's confirmation, and
// the run it is working, stand.
func TestAStaleRefusalLeavesARunningTurnsRun(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)

	// The re-send is held at the door; its turn starts while it is still in flight.
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the re-send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the re-send's turn started = %+v, want it confirmed", p)
	}

	if err := h.m.Handle(h.ctx, StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request"}); err != nil {
		t.Fatal(err)
	}

	if p := h.pending(); p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the stale refusal = %+v, want the running turn's confirmation kept and the given-up prompt's mark gone", p)
	}
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("the claim serves run %d, want the running turn's 7", run)
	}

	// The re-send is then refused: the turn was not its, and the refusal takes the confirmation
	// back. Nothing read the task, since the given-up prompt's mark went with that prompt's refusal,
	// so no run is attributed to it. A memory cleared at confirm would have dropped that refusal and
	// left the mark standing.
	gated.RefusePrompt("Agent is already processing")
	gated.release <- struct{}{}
	h.m.Wait()
	if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the re-send was refused = %+v, want it neither confirmed nor marked", p)
	}
	if run := h.claim().ServingRun(); run != 0 {
		t.Fatalf("the claim serves run %d after the re-send was refused, want none", run)
	}
}

// The answer to a prompt this machine gave up on can arrive after the task has been sent again: a
// send verifies the workspace and adopts it before it writes its frame, so a failure answering
// the earlier prompt has a real window to land in. That answer is about the prompt it named, not
// about the send that has replaced it — acting on it would rotate the id under that send, whose
// own acknowledgement would then read as stale, the task would be prompted a third time, and the
// turn that ran it would have nothing to retire it.
func TestAnAnswerToAGivenUpPromptNeitherMovesNorChargesTheTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)

	// The sweep sends the task again under its new id.
	h.observe(runtime.Alive)
	resent := h.pending()
	if prompts := h.prompts(); len(prompts) != base+2 || prompts[base+1].DeliveryID != resent.ID {
		t.Fatalf("prompts = %+v, want the task sent again under %s", prompts, resent.ID)
	}
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request"})

	if p := h.pending(); p.ID != resent.ID {
		t.Fatalf("pending id = %s after the old prompt's refusal, want the send in flight's %s", p.ID, resent.ID)
	}
	if budgets := h.claim().Budgets; budgets.PromptFailures != 1 {
		t.Fatalf("prompt failures = %d, want the one the bound charged", budgets.PromptFailures)
	}
}

// A refusal of the prompt the bound gave up on is emitted before the next send reaches the
// connection, but it reaches the machine by a longer road — the listener, the daemon's pump, the
// claim's inbox — than the next send's acknowledgement, which the send posts itself. Handled
// after that acknowledgement, it must not clear the mark the acknowledgement set: a first task
// whose turn then starts late would have its completion attributed to no run.
func TestAnOldRefusalHandledAfterTheNextSendsAcknowledgementLeavesItsMark(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)

	// The next send goes out and is acknowledged.
	h.observe(runtime.Alive)
	acknowledged := h.pending()
	if acknowledged.DeliveredAt.IsZero() || acknowledged.ID == first.DeliveryID {
		t.Fatalf("pending after the next send = %+v, want it acknowledged under a new id", acknowledged)
	}

	// The refusal of the first prompt is handled only now.
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request"})

	if p := h.pending(); p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the old refusal = %+v, want the acknowledged send's mark kept", p)
	}
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("the claim serves run %d, want the acknowledged task's 7", run)
	}
}

// The same holds across a relaunch: a refusal the old process emitted can still be in the claim's
// inbox when the new process is sent the task and acknowledges it. The new process's
// acknowledgement takes the mark over, so the old prompt's refusal names a prompt that no longer
// holds it; a relaunch itself leaves the mark alone.
func TestAnOldProcessesRefusalLeavesTheNewSendsMark(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	base := len(h.prompts())
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(base + 1)[base]
	h.advance(2 * testRPC)

	h.must(RequestSuspend{Claim: testToken})
	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)
	h.observe(runtime.Alive)
	if p := h.pending(); p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the relaunched send = %+v, want it acknowledged", p)
	}

	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request"})

	if p := h.pending(); p.DeliveredAt.IsZero() {
		t.Fatalf("pending after the old process's refusal = %+v, want the new send's mark kept", p)
	}
}

// A re-send can start and never be acknowledged: its adopt step fails, the transport loses it, or
// the agent refuses it on arrival — also after its turn confirmed it while it was in flight, and
// also as the first send of a relaunched process. None of those marks the task, so the mark still
// standing is the one the given-up prompt's acknowledgement set — and that prompt's refusal,
// whenever it lands, says the prompt never ran. It must clear the mark, or a completion from the
// next turn of a claim serving no run is credited to a task nobody read. Nothing on the way —
// a confirmation, a relaunch — may drop the record of which prompt set the mark.
func TestAnUnacknowledgedResendLeavesTheGivenUpPromptsRefusalToClearTheMark(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity bool
		resend   func(h *harness)
	}{
		{name: "the re-send's adopt step fails", identity: true, resend: func(h *harness) {
			h.rt.FailAdoptWorkingCopy(errBoom)
			h.observe(runtime.Alive)
		}},
		{name: "the transport loses the re-send", resend: func(h *harness) {
			h.conn.FailPrompt(errBoom)
			h.observe(runtime.Alive)
		}},
		{name: "the agent refuses the re-send on arrival", resend: func(h *harness) {
			h.conn.RefusePrompt("agent busy")
			h.observe(runtime.Alive)
		}},
		{name: "the re-send's turn confirms it in flight, then the send is lost", resend: func(h *harness) {
			gated := newGatedConn()
			gated.Conn.FailPrompt(errBoom)
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}}); err != nil {
				h.t.Fatal(err)
			}
			waitFor(h.t, "the re-send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
				h.t.Fatal(err)
			}
			gated.release <- struct{}{}
			h.m.Wait()
		}},
		{name: "a relaunch, then the new process's send is lost", resend: func(h *harness) {
			h.conn.FailPrompt(errBoom)
			h.must(RequestSuspend{Claim: testToken})
			h.must(RequestResume{Claim: testToken})
			h.reach(StateReady)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.identity {
				// A send adopts the role's identity before it writes its frame; the first send's
				// adopt succeeds and the re-send's fails.
				h.deps.Identity = func(context.Context, claim.Role) (runtime.GitIdentity, error) {
					return runtime.GitIdentity{Name: "legion-tester[bot]", Email: "t@example.invalid"}, nil
				}
				h.restart()
			}
			h.reach(StateReady)
			base := len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			first := h.wantPrompts(base + 1)[base]
			h.advance(2 * testRPC)

			tc.resend(h)
			if p := h.pending(); p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending after the lost re-send = %+v, want it unconfirmed under the given-up prompt's mark", p)
			}

			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: "the model provider refused the request"})

			if p := h.pending(); !p.DeliveredAt.IsZero() {
				t.Fatalf("pending after the given-up prompt's refusal = %+v, want its read mark gone", p)
			}
			if run := h.claim().ServingRun(); run != 0 {
				t.Fatalf("the claim serves run %d, want none: nobody read the task", run)
			}
		})
	}
}
