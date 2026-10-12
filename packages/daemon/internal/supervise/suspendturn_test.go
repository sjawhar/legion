package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A suspension that arrives while the agent is in a turn waits for that turn to end (LEGION-283).
// A worker reports its phase complete from inside a turn, and that report is what asks for the
// suspension: stopping the process at once cuts off the tool call that made it, so Oh My Pi never
// writes the call's result and a session resumed from that transcript holds a report with no
// answer. Until the turn ends the agent keeps its process and its capability, and asking again is
// the same wait.
func TestASuspensionArrivingMidTurnWaitsForTheTurnToEnd(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	loc := h.locator()

	for range 2 {
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 left implementing", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
		}
		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)
		if c := h.claim(); c.Locator == nil || *c.Locator != loc || c.CapabilityHash == nil {
			t.Fatalf("the waiting claim %+v, want its process and its capability kept until the turn ends", c)
		}
	}

	h.must(StreamTurnEnd{Claim: testToken})

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Errorf("suspended %+v, want the process whose turn ended, %+v", suspend.Locator, loc)
	}
	h.wantState(StateSuspended)
	if lines := h.logs.lines(`msg="supervise: suspended"`, `reason="LEGION-209 left implementing"`); len(lines) != 1 {
		t.Errorf("suspend lines %q, want one naming the reason the suspension was asked for", lines)
	}
	if stored := h.store.load(testToken); stored.State != StateSuspended || stored.Pending != nil {
		t.Errorf("stored %+v, want the suspension persisted and the turn's task retired", stored)
	}
	h.wantPrompts(1)
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByDaemon})
	h.wantCalls("Suspend", 1)
}

// An operator's suspension whose turn never ends does not hold for ever: the process is stopped
// once the stop timeout has run from the first request, and a request repeated meanwhile does not
// start the wait over.
func TestAnOperatorsSuspensionWhoseTurnNeverEndsStopsTheProcessAtTheStopTimeout(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}

	h.advance(testStop - time.Second)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the repeated suspend returned %v, want ErrSuspendHeld", err)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)

	h.advance(time.Second)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	if c := h.claim(); c.SuspendedBy != SuspendedByOperator {
		t.Errorf("the claim records %q as who suspended it, want the operator", c.SuspendedBy)
	}

	// The turn's end, when it comes, is a suspended process's, and changes nothing.
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
}

// The daemon's own suspension (its issue left the workflow, its tree lingers) waits for the turn
// however long it takes: an architect's sign-off moves its tree into linger from inside a turn that
// goes on after it, and the linger must not cut that turn off. What bounds the wait is what ends
// the process anyway, the tree's close. An operator's suspend arriving meanwhile takes the held
// suspension over, and its stop timeout runs from then.
func TestTheDaemonsSuspensionWaitsForTheTurnPastTheStopTimeout(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.advance(10 * testStop)
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByDaemon {
		t.Fatalf("the claim reports %+v, want the daemon's suspension held", c)
	}

	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the operator's suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	if c := h.claim(); c.SuspensionHeldBy != SuspendedByOperator || !c.RuleHolds() {
		t.Fatalf("the claim reports %+v, want the operator's suspension held", c)
	}
	h.advance(testStop - time.Second)
	h.wantState(StateWorking)
	h.advance(time.Second)
	h.wantState(StateSuspended)
	if c := h.claim(); c.SuspendedBy != SuspendedByOperator {
		t.Errorf("the claim records %q as who suspended it, want the operator", c.SuspendedBy)
	}
}

// A held suspension whose stop the runtime refuses stays held and is tried again at the probe
// interval, so it lands with nothing else retrying it — an operator's suspend has no outbox row.
// At the turn's end the claim is left idle meanwhile, and is handed nothing while it is stopped.
func TestAHeldSuspensionWhoseStopFailsIsTriedAgainUntilItLands(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *harness)
		held ClaimState
	}{
		{"at the stop timeout", func(h *harness) { h.advance(testStop) }, StateWorking},
		{"at the turn's end", func(h *harness) {
			if err := h.handle(StreamTurnEnd{Claim: testToken}); !errors.Is(err, errBoom) {
				h.t.Fatalf("turn end with a failing stop returned %v, want the runtime's error", err)
			}
		}, StateIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
			}
			h.rt.FailSuspend(errBoom)

			tc.stop(h)
			h.wantState(tc.held)
			if tc.held == StateIdle {
				h.must(RequestDeliver{Claim: testToken, Task: "an operator's task"})
			}
			h.observe(runtime.Alive)
			h.wantPrompts(1)

			h.rt.FailSuspend(nil)
			h.advance(testProbe)
			h.wantState(StateSuspended)
			h.wantPrompts(1)
		})
	}
}

// A held suspension whose stop lands at the turn's end but whose write fails leaves the claim
// suspended in memory, its process let go (suspended), not idle: an idle claim with no locator is
// one the next connection close would probe through a nil locator, crashing the daemon.
func TestAHeldSuspensionWhoseStopLandsButWhoseWriteFailsStaysSuspended(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.store.fail("PutClaim", errBoom)

	if err := h.handle(StreamTurnEnd{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("turn end with a failing write returned %v, want the store's error", err)
	}
	h.store.fail("PutClaim", nil)
	h.must(StreamClosed{Claim: testToken})

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	h.wantCalls("Probe", 0)
}

// A start run against the claim while the daemon's suspension is held hands it work again: the
// suspension it supersedes never runs at the turn's end, as the outbox finishes a suspend older than
// the newest start without acting (workflow.StopActs).
func TestAStartRunAgainstTheClaimDropsTheSuspensionWaitingForItsTurn(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}

	if err := h.m.StartedBy(h.ctx, 42); err != nil {
		t.Fatalf("record the start: %v", err)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.advance(testStop)

	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
}

// An operator's suspension holds until the operator resumes the claim: a start meeting it, held for
// the turn or run, records nothing and waits (ErrHeldByOperator), and the suspension runs at the
// turn's end.
func TestAStartWaitsOutTheOperatorsSuspension(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}

	if err := h.m.StartedBy(h.ctx, 42); !errors.Is(err, ErrHeldByOperator) {
		t.Fatalf("a start against the held operator's suspension returned %v, want ErrHeldByOperator", err)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateSuspended)
	if err := h.m.StartedBy(h.ctx, 42); !errors.Is(err, ErrHeldByOperator) {
		t.Fatalf("a start against the operator's suspension returned %v, want ErrHeldByOperator", err)
	}
	if got := h.claim().LastStartRow; got != 0 {
		t.Fatalf("the claim recorded start row %d, want none while the operator's suspension holds", got)
	}
	h.must(RequestResume{Claim: testToken})
	if err := h.m.StartedBy(h.ctx, 42); err != nil {
		t.Fatalf("the start after the operator's resume: %v", err)
	}
}

// A process that dies while its suspension is held has nothing left to wait for: the claim is
// suspended, not relaunched, and the death is charged nowhere. The turn's task goes back to waiting
// as a death leaves it, so a task of no phase goes on the resume.
func TestAProcessThatDiesWhileItsSuspensionWaitsIsSuspendedNotRelaunched(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}

	h.observe(runtime.Gone)

	h.wantState(StateSuspended)
	h.wantCalls("Spawn", 1)
	h.wantCalls("Resume", 0)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Errorf("suspended %+v, want the dead process's %+v", suspend.Locator, loc)
	}
	h.wantBudgets(Budgets{})
	if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.Interrupted {
		t.Errorf("pending %+v, want the dead turn's task back unconfirmed and marked interrupted", p)
	}
	h.advance(testStop)
	h.wantCalls("Suspend", 1)
}

// A prompt retirement while a suspension is held is that suspension's stop: the process is alive,
// so a stop the runtime refuses leaves the claim working and still holding the suspension, retried
// at the probe interval, rather than marked suspended over a process that still runs. Nothing is
// charged — here the retirement would be the claim's last, which would fail it. The prompt was on
// its way when a turn began, and the agent refuses it at the prompt-failure limit.
func TestAPromptRetirementWhileASuspensionIsHeldRunsTheHeldStop(t *testing.T) {
	h := newBareHarness(t)
	h.deps.Limits = Limits{LaunchFailures: 3, PromptFailures: 1, PromptRetires: 1}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateReady)
	gated := newGatedConn()
	gated.RefusePrompt("the model provider refused the request")
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Handle(h.ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.rt.FailSuspend(errBoom)

	gated.release <- struct{}{}
	h.m.Wait()

	h.wantState(StateWorking)
	if c := h.claim(); !c.SuspensionHeld || c.Locator == nil {
		t.Fatalf("after the refused stop the claim is %+v, want its process kept and the suspension still held", c)
	}
	h.wantCalls("Suspend", 1)
	h.rt.FailSuspend(nil)
	h.advance(testProbe)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 2)
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
}

// Only a held suspension lets a row end in suspended without naming that state. A row that
// suspends a claim holding none — idle with a phase's task waiting, whose suspension would retire
// that task, or ready — is reported, not let through silently; the same row ending a held
// suspension passes.
func TestOnlyAHeldSuspensionLetsARowEndSuspendedWithoutNamingIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   ClaimState
		prepare func(h *harness)
		refused bool
	}{
		{"an idle claim holding none", StateIdle, func(h *harness) {
			h.must(RequestDeliver{Claim: testToken, Task: "the next task", Phase: phase.Testing, Generation: 7})
		}, true},
		{"a ready claim", StateReady, func(*harness) {}, true},
		{"a working claim holding one", StateWorking, func(h *harness) {
			if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
				h.t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(tc.state)
			tc.prepare(h)
			k := key{tc.state, onObservation}
			saved := table[k]
			t.Cleanup(func() { table[k] = saved })
			table[k] = rule{name: "a rogue row", to: saved.to, act: func(m *Machine, ctx context.Context, _ Event) error {
				return m.suspendNow(ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator})
			}}

			err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})

			if refused := err != nil && strings.Contains(err.Error(), "a rogue row"); refused != tc.refused {
				t.Fatalf("the rogue suspension returned %v, want refused = %v", err, tc.refused)
			}
			h.wantState(StateSuspended)
		})
	}
}

// A daemon restart forgets the wait, and the request asked again (the outbox's row is retried until
// the claim answers nil) waits again. A turn that ended while the daemon was down is found over by
// the reconnected shim's first hello, which runs the suspension.
func TestASuspensionAskedAgainAfterARestartRunsWhenTheShimFindsTheTurnOver(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.restart()
	h.wantState(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend after the restart returned %v, want ErrSuspendHeld", err)
	}

	h.conn.SetStreaming(false)
	h.must(StreamHello{Claim: testToken, Generation: h.generation()})

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
}
