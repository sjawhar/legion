package supervise

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// compactInstructions stands for what the API sends with every compaction: Oh My Pi writes a
// text summary only for a compaction that carries instructions, and Machine.Compact refuses one
// that carries none.
const testCompactInstructions = "Write the summary as text."

// ladderStuck is the claim the provider-errors notice names, idle between the ladder's sends:
// six turns of it in a row ended in a provider error, so the project's controller has been told
// once, the task it owes is unconfirmed on the claim, and the ladder's next step is ten minutes
// away. ladderSends is how many prompts the connection has seen by then. set, when it is given,
// changes the dependencies before the machine is built, as workingClaim's own does.
const ladderSends = providerErrorRunTold

func ladderStuck(t *testing.T, notices *int, set ...func(*Deps)) *harness {
	t.Helper()
	h := workingClaim(t, append([]func(*Deps){func(d *Deps) {
		d.ProviderErrorRunReached = func(Claim, string) { *notices++ }
	}}, set...)...)
	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(turn+1, promptRetryDelay(turn+1))
		}
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Fatalf("the run of provider errors is %d, want the %d the controller is told at", got, providerErrorRunTold)
	}
	if *notices != 1 {
		t.Fatalf("the controller was told %d times, want once", *notices)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle between the ladder's sends", got)
	}
	h.wantPrompts(ladderSends)
	return h
}

// A compaction that ran starts the claim's run of provider-error turns again, so the project's
// controller is told a second time after six more refused turns. Without the reset the
// controller would be told once and never again until a turn of that claim ended with no error
// at all, and the compaction it asked for would buy it no second look.
//
// The owed task goes again at once rather than at the end of the ladder step the compaction
// interrupted: the next step of that ladder is ten minutes away, and putting the task in front
// of the shortened session is the whole point of the operation.
func TestACompactionStartsTheRunOfProviderErrorsAgainSoTheControllerIsToldASecondTime(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.store.load(h.token).ProviderErrorRun; got != 0 {
		t.Fatalf("the stored run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.claim().Session; got != session {
		t.Fatalf("the compacted claim records session %q, want the session it had, %q", got, session)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the compacted claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
	h.must(StreamTurnStart{Claim: h.token})

	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(sent+1+turn, promptRetryMax)
		}
	}

	if notices != 2 {
		t.Fatalf("the controller was told %d times, want a second notice after six more refused turns", notices)
	}
}

// A compaction Oh My Pi refused or skipped resets nothing: the session is as it was, so the run
// of refused turns it carried still stands and the controller is not told again for it. The
// owed task goes again all the same.
func TestACompactionThatWasRefusedKeepsTheRunOfProviderErrors(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	h.conn.RefuseCompact("Nothing to compact (session too small)")

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if reason := runtimeRefusalReason(err); reason != "Nothing to compact (session too small)" {
		t.Fatalf("compact answered %v, want Oh My Pi's own refusal", err)
	}
	h.m.Wait()
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Errorf("the run of provider errors is %d, want the %d it carried: nothing was compacted", got, providerErrorRunTold)
	}
	if got := h.state(); got != StateIdle {
		t.Errorf("the claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
}

// runtimeRefusalReason is the words the far side refused with, as runtime.RefusalReason reads
// them; it is spelled here so this file needs no import for one call.
func runtimeRefusalReason(err error) string {
	var refusal interface{ RefusalReason() string }
	if errors.As(err, &refusal) {
		return refusal.RefusalReason()
	}
	return ""
}

// A claim being compacted is busy exactly as a claim in a turn is: it is working, a newer task
// waits rather than being sent into the compaction, a second compaction is refused, and the end
// of the compaction is what releases the task.
func TestAClaimBeingCompactedIsBusyUntilTheCompactionEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(RequestDeliver{Claim: h.token, Task: "A newer task.", ID: "newer"})
	second := h.m.Handle(h.ctx, RequestCompact{Claim: h.token})

	var refused *RefusedError
	if !errors.As(second, &refused) {
		t.Fatalf("a second compaction answered %v, want a refusal", second)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after the compaction, want idle", got)
	}
	prompts := h.wantPrompts(sent + 1)
	if prompts[sent].DeliveryID != "newer" {
		t.Fatalf("the send after the compaction carried %q, want the newer task", prompts[sent].DeliveryID)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted.
func waitForCompacting(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := h.m.View().Claim; c.Compacting && c.State == StateWorking {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the claim never reported itself being compacted")
}

// A turn that ends while the session is being compacted leaves the claim exactly where it is: a
// compaction aborts whatever turn its session was running, so a background turn's end can
// arrive under one, and taking it for the compaction's end would hand the claim its owed task
// while Oh My Pi is still writing the summary.
func TestATurnThatEndsDuringACompactionLeavesTheClaimWorking(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.state(); got != StateWorking {
		t.Fatalf("the claim is %s after a turn ended under the compaction, want it still working", got)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.wantPrompts(sent + 1)
}

// A compaction asked for with no instructions is refused before anything is sent: Oh My Pi
// archives the history onto snapcompact's images for a compaction that carries none, and a
// worker resumed on images needs a vision model to read its own past.
func TestACompactionWithNoInstructionsIsRefusedBeforeAnythingIsSent(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)

	err := h.m.Compact(h.ctx, "  ")

	if !errors.Is(err, ErrCompactInstructions) {
		t.Fatalf("compact with no instructions answered %v, want ErrCompactInstructions", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it left idle", got)
	}
}

// The ladder has just sent the owed task and the prompt is in flight — acknowledged, with the
// wait for its turn armed — when the compaction is taken. Oh My Pi runs one thing at a time, so
// the compaction aborts the turn that prompt started, and the result of an aborted turn carries
// no error of its own: read as a turn that worked, it would confirm the task and the end of the
// compaction would retire it unsent. The task is taken back instead, and the end of the
// compaction sends it again.
func TestACompactionTakenWhileTheOwedPromptIsInFlightSendsTheTaskAgainAfterIt(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.advance(promptRetryMax) // the ladder's next step sends the owed task
	sent := h.wantPrompts(ladderSends + 1)[ladderSends].DeliveryID
	if got := h.state(); got != StateIdle {
		t.Fatalf("after the ladder's send the claim is %s, want it idle waiting for the turn", got)
	}
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: sent, AgentInvoked: true})

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	owed := h.claim().Pending
	if owed == nil {
		t.Fatal("the compaction retired the task whose turn it aborted: the claim owes nothing, and nothing sends it again")
	}
	if owed.ID == sent {
		t.Fatalf("the task kept the id %q its aborted prompt carried; a task taken back goes under a new one, so that prompt's result names nothing", sent)
	}
	prompts := h.wantPrompts(ladderSends + 2)
	if prompts[ladderSends+1].DeliveryID != owed.ID {
		t.Fatalf("the send after the compaction carried %q, want the task the claim still owes, %q",
			prompts[ladderSends+1].DeliveryID, owed.ID)
	}
}

// A turn Oh My Pi starts as the compaction ends — a follow-up an Envoy delivery queued while the
// session was being compacted — leaves the claim working for that turn. Idling it there would
// send the owed task into a running agent, and that turn's own end would then arrive at an idle
// claim, where it means nothing.
func TestATurnThatStartsDuringACompactionKeepsTheClaimWorkingUntilItEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateWorking {
		t.Fatalf("the claim is %s while its agent runs a turn that started under the compaction, want it working", got)
	}
	h.wantPrompts(ladderSends)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after that turn ended, want idle", got)
	}
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the send after the turn carried %q, want the owed task %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A claim that was ready is ready again when the compaction ends: no turn of it has begun or
// ended, so idling it would say one had. The rollback of a take the store would not write
// restores the same state.
func TestACompactionGivesBackAReadyClaimReady(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.store.fail("PutClaim", errors.New("the store is down"))

	refused := h.m.Compact(h.ctx, testCompactInstructions)

	if refused == nil {
		t.Fatal("a compaction the store would not write the take of answered no error")
	}
	if got := h.state(); got != StateReady {
		t.Fatalf("the claim is %s after the take was rolled back, want the ready it was", got)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none: the claim was never taken", got)
	}
	h.store.fail("PutClaim", nil)

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}

	h.m.Wait()
	if got := h.state(); got != StateReady {
		t.Fatalf("the compacted claim is %s, want the ready it was: no turn of it has ended", got)
	}
	if got := h.store.load(h.token).State; got != StateReady {
		t.Fatalf("the stored claim is %s, want ready", got)
	}
}

// A compaction whose take the store would not write asked Oh My Pi nothing and sent the claim
// nothing, so it is told apart from a compaction that ran and was refused: its caller answers
// a failure rather than report a compaction that did not run, which carries the claim's task
// with it (api's compact).
func TestACompactionTheStoreWouldNotTakeSaysNothingWasAsked(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.store.fail("PutClaim", errors.New("the store is down"))

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction the store would not take answered %v, want ErrCompactNotTaken", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	h.wantPrompts(ladderSends)
}

// A compaction of a claim whose shim is not connected asks nothing and takes nothing: the daemon
// has no connection to ask through, which is no refusal of the claim's state — the state takes a
// compaction — so it answers the take that did not happen, and its caller reports the one
// failure it is rather than reading the claim's state for a cause that state does not hold
// (api's compact).
func TestACompactionWhoseShimIsNotConnectedSaysNothingWasAsked(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.conns.Remove(h.token)

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction with no shim connected answered %v, want ErrCompactNotTaken", err)
	}
	var refused *RefusedError
	if errors.As(err, &refused) {
		t.Fatalf("the answer is the refusal %+v; the state takes a compaction, so no refusal of it says what went wrong", refused)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want the idle it was", got)
	}
	h.wantPrompts(ladderSends)
}

// A compaction whose end the store would not write is told apart from one that was refused: Oh
// My Pi compacted the session, the write that gives the claim back failed, and the task the
// claim owes is unsent. Answered as a compaction that did not run, it would tell its caller the
// claim was back and its task had gone again (api's compact).
func TestACompactionWhoseEndTheStoreWouldNotWriteSaysTheClaimWasNotGivenBack(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.store.fail("PutClaim", errors.New("the store is down"))
	release()

	err := <-compacted
	if !errors.Is(err, ErrCompactNotEnded) {
		t.Fatalf("a compaction whose end the store would not write answered %v, want ErrCompactNotEnded", err)
	}
	if got := len(h.conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want the one that ran", got)
	}
	h.m.Wait()
	h.wantPrompts(ladderSends)
	if got := h.store.load(h.token).State; got != StateWorking {
		t.Fatalf("the stored claim is %s, want the working the take wrote: the end was never written", got)
	}
}

// A claim that loses its process while its session is being compacted gets nothing from the end
// of that compaction: the end reaches a claim the relaunch has moved past, so it decides nothing
// and sends nothing. The task the claim owes goes at the relaunch, once its agent is ready
// again, and the answer says so rather than reporting a compaction that gave the claim back.
func TestACompactionWhoseClaimLosesItsProcessLeavesTheOwedTaskToTheRelaunch(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.observe(runtime.Gone)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim lost its process answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantPrompts(ladderSends)

	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the relaunched claim was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A death the wake pacing parks the relaunch of leaves the claim exactly as the park found it:
// working, over the dead process it still records, at the generation the compaction was taken
// at. The one mark of the death is the launch waiting its turn to replace that process
// (launchWaits), so the end of the compaction has that one thing to read. Taken for a
// compaction that ran, the end would give the claim back idle and prompt a process that is
// gone, and the answer would promise its caller both.
func TestACompactionWhoseRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	owed := h.pending().ID
	dead := h.locator()
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	// A claim of the wave takes the turn the limiter had saved, so the relaunch this death
	// charges waits for the next one rather than running where it is raised.
	queueAhead(h, 1)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim's relaunch waits its turn answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateWorking)
	if got := h.claim().Locator; got == nil || *got != dead {
		t.Fatalf("the claim records %+v, want the dead process the park kept, %+v", got, dead)
	}
	h.wantWakeDueAt(waiting)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))
	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the claim the wake's turn relaunched was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A death that spends the claim's last launch leaves it failed, and a failed claim relaunches of
// no accord of its own: an operator's retry is what starts it again. The end of the compaction
// finds a claim with no compaction recorded, which it must give nothing back — taken for a
// compaction that ran it would idle the failed claim and prompt the process the death ended.
func TestACompactionWhoseClaimItsDeathFailedGivesItBackNothing(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, func(d *Deps) { d.Limits.LaunchFailures = 1 })
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.observe(runtime.Gone)
	h.wantState(StateFailed)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim its death failed answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateFailed)
	if got := h.store.load(h.token).State; got != StateFailed {
		t.Fatalf("the stored claim is %s, want the failed its death left", got)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
}

// An operator's suspension that arrives under the compaction is held for its end, and its stop
// timeout is what ends the wait when the compaction outlasts it: the process is stopped and the
// claim suspended, which only the operator's resume starts again. The end of the compaction
// finds a claim with no compaction recorded, which it must give nothing back — taken for a
// compaction that ran it would idle the suspended claim and prompt the process the stop ended.
func TestACompactionAHeldSuspensionStoppedGivesTheClaimBackNothing(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	held := RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
	if err := h.handle(held); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension answered %v, want it held for the compaction's end", err)
	}
	h.advance(testStop)
	h.wantState(StateSuspended)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction a held suspension's deadline stopped answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the claim is suspended by %q, want the operator whose stop ended it", got)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
}
