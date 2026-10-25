package supervise

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// watchdogLimit is the role limit every test here runs under: an implementer's default.
const watchdogLimit = 6 * time.Hour

// watched is a harness whose claim's role the turn watchdog limits, with every report its ladder
// made. The fake clock fires timers on the test's own goroutine, so the reports need no lock.
type watched struct {
	*harness
	reports []WedgedTurn
}

// newWatched is a claim walked to working under the watchdog, its agent answering as a healthy
// one does: the turn is running and nothing has gone wrong yet.
func newWatched(t *testing.T) *watched {
	t.Helper()
	w := &watched{}
	h := newBareHarness(t)
	h.deps.TurnLimit = func(claim.Role) time.Duration { return watchdogLimit }
	h.deps.Wedged = func(report WedgedTurn) { w.reports = append(w.reports, report) }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	w.harness = h
	w.conn.SetStreaming(true)
	w.reach(StateWorking)
	w.m.Wait()
	return w
}

// speak is the agent writing a frame of its own now, as its connection records one (Conn.LastEvent).
func (w *watched) speak() { w.conn.SetLastEvent(w.clock.Now()) }

// wantStep is the rung the claim's watchdog record last ran, "" for a claim it holds none on.
func (w *watched) wantStep(want WatchdogStep) {
	w.t.Helper()
	if got := w.m.Claim().Watchdog; (want == "" && got != nil) || (want != "" && (got == nil || got.Step != want)) {
		w.t.Fatalf("watchdog record = %+v, want step %q", got, want)
	}
}

// launches is how many processes the runtime started for the claim: its first launch and every
// relaunch after it.
func (w *watched) launches() int {
	return len(w.calls("Spawn")) + len(w.calls("Resume"))
}

// A turn that keeps producing events is working, however long it runs: a subagent that takes two
// hours, a test suite that takes six. The watchdog asks the agent nothing, interrupts nothing and
// relaunches nothing while its events arrive.
func TestALongTurnThatKeepsProducingEventsIsNeverTouched(t *testing.T) {
	w := newWatched(t)
	for range 8 {
		w.advance(watchdogLimit - time.Hour)
		w.speak()
	}

	w.wantState(StateWorking)
	w.wantStep("")
	if asks, aborts := w.conn.StateAsks(), w.conn.Aborts(); asks != 0 || aborts != 0 {
		t.Fatalf("after %s of a live turn the agent was asked for its state %d times and aborted %d; want neither",
			8*(watchdogLimit-time.Hour), asks, aborts)
	}
	if launches := w.launches(); launches != 1 {
		t.Fatalf("launches = %d, want the first launch alone", launches)
	}
}

// The first rung is the one that costs the agent nothing: the existing get_state, which starts no
// turn. Its answer proves the process is up and says nothing about the turn, so it never clears
// the clock — the interrupt follows one probe interval later, and only a turn event stops it.
func TestTheFirstRungAsksTheProcessAndTheAnswerDoesNotClearTheClock(t *testing.T) {
	w := newWatched(t)
	w.advance(watchdogLimit)

	w.wantState(StateWorking)
	w.wantStep(WatchdogProbed)
	if asks, aborts := w.conn.StateAsks(), w.conn.Aborts(); asks != 1 || aborts != 0 {
		t.Fatalf("state asks = %d, aborts = %d; want one ask and no abort", asks, aborts)
	}
	if steps := w.m.Claim().Watchdog.Steps; len(steps) != 1 || !strings.Contains(steps[0], "answered get_state") {
		t.Fatalf("steps = %q, want the probe's answer", steps)
	}

	// The turn speaks before the interrupt is due: the clock starts over and the ladder stops.
	w.speak()
	w.advance(w.deps.Timeouts.Probe)
	if aborts := w.conn.Aborts(); aborts != 0 {
		t.Fatalf("aborts = %d after the turn spoke again, want none", aborts)
	}
}

// An agent that answers but whose turn has still produced nothing is interrupted — the existing
// abort, which keeps its process and session — and relaunched only if that interrupt does not end
// the turn.
func TestTheInterruptComesBeforeTheRelaunchAndTheTurnsEndStopsTheLadder(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		endsTurn bool
	}{
		{name: "the interrupted turn ends", endsTurn: true},
		{name: "the interrupted turn does not end", endsTurn: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			w := newWatched(t)
			w.advance(watchdogLimit)
			w.wantStep(WatchdogProbed)

			w.advance(w.deps.Timeouts.Probe)

			w.wantState(StateWorking)
			w.wantStep(WatchdogInterrupted)
			if aborts := w.conn.Aborts(); aborts != 1 {
				t.Fatalf("aborts = %d, want one", aborts)
			}
			if launches := w.launches(); launches != 1 {
				t.Fatalf("launches = %d after the interrupt, want no relaunch yet", launches)
			}

			if testCase.endsTurn {
				w.must(StreamTurnEnd{Claim: w.token})
				w.wantState(StateIdle)
				w.wantStep("")
				w.advance(watchdogLimit)
				if launches := w.launches(); launches != 1 {
					t.Fatalf("launches = %d after the turn ended, want no relaunch", launches)
				}
				return
			}
			w.advance(w.deps.Timeouts.Stop)
			w.wantState(StateLaunching)
			w.wantStep(WatchdogRelaunched)
			if launches := w.launches(); launches != 2 {
				t.Fatalf("launches = %d, want the relaunch", launches)
			}
		})
	}
}

// An agent that stops answering entirely is recovered without a person: the probe finds no answer,
// so the ladder goes straight to the relaunch, which resumes the claim's own session, hands the
// outstanding task back to the agent it brings up, and clears the ladder once that agent is ready.
func TestAnAgentThatStopsAnsweringIsRelaunchedOnItsOwnSession(t *testing.T) {
	w := newWatched(t)
	task := w.pending()
	w.conn.FailGetState(errBoom)

	w.advance(watchdogLimit)

	w.wantState(StateLaunching)
	w.wantStep(WatchdogRelaunched)
	if aborts := w.conn.Aborts(); aborts != 0 {
		t.Fatalf("aborts = %d, want none: a process that does not answer takes no abort", aborts)
	}
	resume := w.wantCalls("Resume", 1)[0]
	if resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("the relaunch resumed %q, want the claim's own session file %q", resume.Spec.ResumeSessionFile, sessionFile)
	}
	if got := w.claim().Session; got != session {
		t.Fatalf("session = %q, want the claim's own %q", got, session)
	}
	// The task whose turn wedged goes back for the relaunched agent, as a death in a turn takes it
	// back: a new id, unconfirmed, and marked interrupted.
	if p := w.pending(); !p.ConfirmedAt.IsZero() || !p.Interrupted || p.ID == task.ID || p.Task != task.Task {
		t.Fatalf("pending = %+v, want %q taken back interrupted under a new id", p, task.Task)
	}

	w.relaunched()
	w.wantStep("")
	if len(w.reports) != 0 {
		t.Fatalf("reports = %+v, want none: the relaunch produced a ready agent", w.reports)
	}
	if prompts := w.prompts(); len(prompts) != 2 || !strings.HasPrefix(prompts[1].Message, interruptedTask) {
		t.Fatalf("prompts = %+v, want the task sent again behind the interrupted-turn sentence", prompts)
	}
}

// A report reaches a human only after the relaunch has failed to produce a live, registered agent:
// nothing is published while the ladder is probing, interrupting or waiting out the relaunch's own
// boot, and the one that is published names the claim, how long it was wedged and every rung.
func TestAReportReachesAHumanOnlyAfterTheRelaunchFails(t *testing.T) {
	w := newWatched(t)
	w.conn.FailGetState(errBoom)

	w.advance(watchdogLimit)
	w.wantStep(WatchdogRelaunched)
	if len(w.reports) != 0 {
		t.Fatalf("reports = %+v, want none while the relaunch boots", w.reports)
	}

	// The relaunched process never says hello, so its registration deadline runs out: that is the
	// relaunch failing to produce a registered agent.
	w.advance(deadline)

	w.wantStep(WatchdogReported)
	if len(w.reports) != 1 {
		t.Fatalf("reports = %+v, want exactly one", w.reports)
	}
	report := w.reports[0]
	if report.Claim.Token != w.token || report.Silence < watchdogLimit {
		t.Fatalf("report = %+v, want %s wedged for at least %s", report, w.token, watchdogLimit)
	}
	if reason := report.Reason(); !strings.Contains(reason, string(w.token)) || !strings.Contains(reason, "get_state") {
		t.Fatalf("reason = %q, want the claim and the rungs that were tried", reason)
	}

	// The report is made once: the next deadline of the same claim publishes nothing more.
	w.advance(deadline)
	if len(w.reports) != 1 {
		t.Fatalf("reports = %+v, want the one report", w.reports)
	}
}

// The ladder's position is on the claim, so a daemon restart takes it up where it left it rather
// than starting over: a claim restored mid-ladder interrupts the turn it had already probed.
func TestARestartTakesTheLadderUpWhereItLeftIt(t *testing.T) {
	w := newWatched(t)
	w.advance(watchdogLimit)
	w.wantStep(WatchdogProbed)

	w.restart()
	w.conns.Register(w.token, w.conn)

	if step := w.m.Claim().Watchdog; step == nil || step.Step != WatchdogProbed {
		t.Fatalf("the restored claim's watchdog = %+v, want the probe it had already made", step)
	}
	// The restored machine measures the silence from the restart: the agent said nothing before
	// it that this daemon can read, so the turn gets its role's whole limit again, and the rung
	// that follows is the interrupt rather than a second probe.
	w.advance(watchdogLimit)

	w.wantStep(WatchdogInterrupted)
	if aborts, asks := w.conn.Aborts(), w.conn.StateAsks(); aborts != 1 || asks != 1 {
		t.Fatalf("aborts = %d, state asks = %d; want the interrupt alone after the restart", aborts, asks)
	}
}

// A role the deployment switched the watchdog off for (`turn_watchdog_seconds: {implementer: 0}`)
// is never watched: no clock is armed, and a silent turn is left alone.
func TestARoleTheWatchdogIsOffForIsNeverTouched(t *testing.T) {
	h := newBareHarness(t)
	h.deps.TurnLimit = func(claim.Role) time.Duration { return 0 }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateWorking)
	h.m.Wait()

	h.advance(24 * time.Hour)

	h.wantState(StateWorking)
	if asks, aborts := h.conn.StateAsks(), h.conn.Aborts(); asks != 0 || aborts != 0 {
		t.Fatalf("state asks = %d, aborts = %d; want neither for a role the watchdog is off for", asks, aborts)
	}
	if c := h.claim(); c.Watchdog != nil {
		t.Fatalf("watchdog record = %+v, want none", c.Watchdog)
	}
}

// A machine with no limits at all — a daemon built before the key, and every test that is about
// something else — watches nothing.
func TestAMachineWithNoLimitsWatchesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.m.Wait()

	h.advance(24 * time.Hour)

	h.wantState(StateWorking)
	if aborts := h.conn.Aborts(); aborts != 0 {
		t.Fatalf("aborts = %d, want none", aborts)
	}
}
