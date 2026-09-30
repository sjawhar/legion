package supervise

import (
	"errors"
	"testing"
	"time"

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
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 left implementing"}); !errors.Is(err, ErrSuspendWaits) {
			t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
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
	h.must(RequestSuspend{Claim: testToken})
	h.wantCalls("Suspend", 1)
}

// A turn that never ends does not hold the suspension for ever: the process is stopped once the
// stop timeout has run from the first request, and a request repeated meanwhile does not start the
// wait over.
func TestASuspensionWhoseTurnNeverEndsStopsTheProcessAtTheStopTimeout(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
	}

	h.advance(testStop - time.Second)
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("the repeated suspend returned %v, want ErrSuspendWaits", err)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)

	h.advance(time.Second)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)

	// The turn's end, when it comes, is a suspended process's, and changes nothing.
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
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
			if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
				t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
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

// A start run against the claim while its suspension waits hands it work again: the suspension it
// supersedes neither runs at the turn's end nor at the stop timeout, as the outbox finishes a
// suspend older than the newest start without acting (workflow.StopActs).
func TestAStartRunAgainstTheClaimDropsTheSuspensionWaitingForItsTurn(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
	}

	if err := h.m.StartedBy(h.ctx, 42); err != nil {
		t.Fatalf("record the start: %v", err)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.advance(testStop)

	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
}

// A process that dies while its suspension waits has nothing left to wait for: the claim is
// suspended, not relaunched, and the death is charged nowhere. The turn's task goes back to waiting
// as a death leaves it, so a task of no phase goes on the resume.
func TestAProcessThatDiesWhileItsSuspensionWaitsIsSuspendedNotRelaunched(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
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

// A prompt retirement while a suspension waits for the agent's turn has already stopped the process
// the suspension waited to stop: the claim stays suspended rather than relaunched. The prompt here
// was on its way when a turn began, and the agent refuses it at the prompt-failure limit.
func TestAPromptRetirementWhileASuspensionWaitsLeavesTheClaimSuspended(t *testing.T) {
	h := newBareHarness(t)
	h.deps.Limits = Limits{LaunchFailures: 3, PromptFailures: 1, PromptRetires: 2}
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
	if err := h.m.Handle(h.ctx, RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
	}

	gated.release <- struct{}{}
	h.m.Wait()

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{PromptRetires: 1})
}

// A daemon restart forgets the wait, and the request asked again (the outbox's row is retried until
// the claim answers nil) waits again. A turn that ended while the daemon was down is found over by
// the reconnected shim's first hello, which runs the suspension.
func TestASuspensionAskedAgainAfterARestartRunsWhenTheShimFindsTheTurnOver(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendWaits", err)
	}
	h.restart()
	h.wantState(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendWaits) {
		t.Fatalf("suspend after the restart returned %v, want ErrSuspendWaits", err)
	}

	h.conn.SetStreaming(false)
	h.must(StreamHello{Claim: testToken, Generation: h.generation()})

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
}
