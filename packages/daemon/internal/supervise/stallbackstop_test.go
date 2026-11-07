package supervise

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// backstopBound is the bound the daemon's sweep would hand this claim's role: four hours for an
// implementer, the role the harness drives (config.StallCallBoundFor), plus the grace and the
// two step gaps. Nothing here reads the configuration — the sweep resolves the bound and the
// request carries it — so the number is written out.
const backstopBound = 4*time.Hour + 5*time.Minute + 2*2*time.Minute

// backstop is the request the daemon's sweep makes for this claim: the role's bound, and the
// deadline it computed from the record it read, which is already past when it posts.
func (h *harness) backstop(last time.Time) StallBackstop {
	return StallBackstop{Claim: h.token, Bound: backstopBound, Deadline: last.Add(backstopBound)}
}

// workingOnATask walks the claim to a turn of a delivered task and reports the id of the prompt
// that turn is running, which is the task the claim is serving.
func (h *harness) workingOnATask(task string) string {
	h.t.Helper()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	return h.wantPrompts(1)[0].DeliveryID
}

// A claim in a turn whose recorded last step is older than its role's bound has that turn ended
// by the daemon: the process is replaced, the session is resumed on the new one, and the task it
// was serving is still owed to it — taken back unconfirmed and marked interrupted, kept in the
// store, and sent again under a new id once the replacement is ready. Nothing is charged: no
// launch failure, and no death with work outstanding, which is what would fail the claim at the
// launch budget if a stall repeated.
func TestTheBackstopEndsAStalledTurnAndLeavesTheTaskOwed(t *testing.T) {
	h := newHarness(t)
	sent := h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if p := h.pending(); !p.Interrupted || p.ID == sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the backstop = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
	stored := h.store.load(h.token)
	if stored.Pending == nil || stored.ServingGeneration != 0 {
		t.Fatalf("stored claim = %+v, want the task kept and no run served", stored)
	}
	if resumes := h.wantCalls("Resume", 1); resumes[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %q, want the claim's own session %q", resumes[0].Spec.ResumeSessionFile, sessionFile)
	}

	h.relaunched()
	if resent := h.wantPrompts(2)[1]; resent.DeliveryID == sent {
		t.Fatalf("re-sent %+v under the id the stalled turn ran under", resent)
	}
	if h.store.load(h.token).Session != session {
		t.Fatalf("session after the backstop = %q, want the same session brought back", h.store.load(h.token).Session)
	}
}

// A claim whose agent has sent no tool frame at all since its turn began is judged from the
// turn's own start, which is the only thing the record holds for it. A claim whose record holds
// neither — no step and no turn start, as a claim whose machine has read nothing has — is judged
// from nothing and left alone: this path guesses at no time of its own.
func TestTheBackstopJudgesAClaimThatHasNotSteppedFromItsTurnsStart(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	started := h.clock.Now()
	if step := h.store.step(h.token); !step.TurnStartedAt.Equal(started) || !step.At.IsZero() {
		t.Fatalf("the record of a claim that has stepped none = %+v, want the turn's start alone", step)
	}
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(started))

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})

	// The same claim with nothing recorded at all: its machine knows of no step and its claim
	// carries none, so the backstop reads no moment to measure and the turn stands.
	bare := newHarness(t)
	bare.workingOnATask("implement the plan")
	bare.m.steppedAt = time.Time{}
	bare.m.claim.Step = StepRecord{}
	bare.advance(backstopBound + time.Minute)

	bare.must(bare.backstop(bare.clock.Now().Add(-backstopBound - time.Minute)))

	bare.wantState(StateWorking)
	bare.wantCalls("Resume", 0)
}

// The negative control: an agent that is sending tool frames is never touched, however long its
// turn runs. The sweep reads the claims before the machine decides, so the fence is the
// machine's own: a frame that arrived after that read is in the machine's inbox ahead of the
// backstop's event, and the turn it belongs to is left exactly as it stands.
func TestTheBackstopLeavesAClaimWhoseRecordMovedInsideItsBound(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	last := h.clock.Now()
	for range 4 {
		h.advance(backstopBound - time.Minute)
		h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	}
	h.advance(time.Minute)

	// The sweep's own read was stale by one frame: the agent stepped again between that read and
	// this decision.
	h.must(h.backstop(last))

	h.wantState(StateWorking)
	h.wantCalls("Resume", 0)
	h.wantCalls("Suspend", 0)
	h.wantBudgets(Budgets{})
	if p := h.pending(); p.Interrupted {
		t.Fatalf("pending after a record that moved = %+v, want the turn untouched", p)
	}
}

// The backstop acts on its own deadline rather than waiting for the turn to end, which is what
// every other mid-turn stop of the daemon's does: a suspension that arrives mid-turn is held for
// that turn (holdSuspension, whose deadline is what bounds the wait), and the turn this exists
// for is one that never ends. The same claim, in the same state, answers the two differently.
func TestTheBackstopDoesNotWaitForATurnThatNeverEnds(t *testing.T) {
	held := newHarness(t)
	held.workingOnATask("implement the plan")
	held.advance(backstopBound + time.Minute)
	if err := held.handle(RequestSuspend{Claim: held.token, By: SuspendedByDaemon, Reason: "the issue closed"}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("a suspension mid-turn = %v, want it held for the turn", err)
	}
	held.wantState(StateWorking)

	h := newHarness(t)
	started := h.clock.Now()
	h.workingOnATask("implement the plan")
	h.advance(backstopBound + time.Minute)
	request := h.backstop(started)
	if !request.Deadline.Before(h.clock.Now()) {
		t.Fatalf("the request's deadline is %s, want a moment already past at %s", request.Deadline, h.clock.Now())
	}

	h.must(request)

	// No turn ended, and no timer of this machine's was waited on: the stop was made where the
	// request arrived.
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
}

// A stall that repeats leaves the claim relaunchable rather than failed. The worker's own ladder
// ends its process at most twice (the plugin's stall recovery), and each of those is a death
// with work outstanding, which is charged; the daemon's backstop is the third stop, and it
// charges nothing, so the claim comes back instead of spending the last of a launch budget whose
// default is three.
func TestTheBackstopChargesNothingAfterTheWorkersOwnTwoExits(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")

	// The worker's two exits, as the daemon reads them: a process gone with the task still
	// outstanding, each charged a death and relaunched.
	for exit := range 2 {
		h.observe(runtime.Gone)
		h.wantState(StateLaunching)
		if got := h.budgets().Deaths; got != exit+1 {
			t.Fatalf("deaths after exit %d = %d, want %d", exit+1, got, exit+1)
		}
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
	}
	before := h.budgets()
	last := h.clock.Now()
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	if got := h.budgets(); got != before {
		t.Fatalf("budgets after the backstop = %+v, want them unchanged at %+v: the stop charges nothing", got, before)
	}
	if got := h.budgets().Deaths; got != 2 {
		t.Fatalf("deaths after the backstop = %d, want the worker's own two", got)
	}
	h.relaunched()
	h.wantState(StateReady)
}

// A suspension already held for the stalled turn ends in a suspension rather than a relaunch, as
// it does for every other replacement the daemon makes (replaceProcess, endHeld): the process the
// backstop would have replaced is stopped, nothing is launched in its place, and the suspension
// decides what follows.
func TestTheBackstopWithAHeldSuspensionSuspendsTheClaim(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	last := h.clock.Now()
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{})
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the stalled process %+v", suspend.Locator, loc)
	}
	h.wantCalls("Resume", 0)
}
