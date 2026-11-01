package supervise

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// testIdleSuspend is the idle suspend a test deployment gives its workers
// (worker_idle_suspend_seconds).
const testIdleSuspend = 30 * time.Minute

// idleHarness is a harness whose implementer claim is suspended once it has been idle
// testIdleSuspend. The shipped testTimeouts leaves the idle suspend off, so every other test of
// this package is untouched by the wait.
func idleHarness(t *testing.T) *harness {
	t.Helper()
	return idleHarnessOf(t, queuedClaim(), nil)
}

// idleHarnessInPhase is idleHarness whose issue is in a delivered task's phase while holds says so
// and has left it when it does not: such a task is dropped where it is (sendPending), so the claim
// is idle again with no turn of that task ever ending to start its wait.
func idleHarnessInPhase(t *testing.T, holds *bool) *harness {
	t.Helper()
	return idleHarnessOf(t, queuedClaim(), holds)
}

// idleHarnessOf is the idle suspend's harness over the queued claim c, whichever role it is on.
func idleHarnessOf(t *testing.T, c Claim, holds *bool) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.Timeouts.IdleSuspend = testIdleSuspend
	if holds != nil {
		h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return *holds, nil }
	}
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// A worker whose turn ended and that was given nothing since is suspended once its idle suspend
// is spent: the daemon takes the process back and keeps the session, so the next task resumes the
// same agent, and the suspension is the daemon's own by rule.
func TestAnIdleWorkerIsSuspendedWithItsSessionKept(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend)
	h.wantState(StateSuspended)
	c := h.claim()
	if c.SuspendedBy != SuspendedByIdle || !c.SuspendedBy.ByRule() {
		t.Fatalf("suspended by %q (by rule %t), want idle by rule", c.SuspendedBy, c.SuspendedBy.ByRule())
	}
	if c.Session != session || c.SessionFile != sessionFile || c.Locator != nil {
		t.Fatalf("the idle suspend left %+v, want the session and its file kept with no locator", c)
	}
	if stored := h.store.load(h.token); stored.State != StateSuspended || stored.SuspendedBy != SuspendedByIdle || stored.SessionFile != sessionFile {
		t.Fatalf("stored %+v, want the idle suspension and its session persisted", stored)
	}
	h.wantCalls("Suspend", 1)
}

// A task that reaches the claim before the wait is spent ends it: the claim keeps its process
// through the deadline that wait had, and the next wait begins whole at the task's turn's end.
func TestATaskBeforeTheIdleSuspendKeepsTheProcess(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend / 2)
	h.must(RequestDeliver{Claim: h.token, Task: "review the head"})
	h.must(StreamTurnStart{Claim: h.token})
	h.advance(testIdleSuspend / 6)
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)

	// The wait the task ended would be spent here; the one its turn began is not.
	h.advance(testIdleSuspend / 2)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)

	h.advance(testIdleSuspend / 2)
	h.wantState(StateSuspended)
}

// A task the issue's phase has left is dropped where it is, without ever becoming a turn
// (sendPending): the worker is idle again with nothing waiting, and the wait begins again from the
// drop. Nothing else would ever arm it — no turn of that task will end — so the worker would
// otherwise hold its process for as long as its issue ran.
func TestATaskDroppedWithoutATurnStartsTheWaitAgain(t *testing.T) {
	holds := true
	h := idleHarnessInPhase(t, &holds)
	h.reach(StateIdle)
	h.advance(testIdleSuspend / 2)

	holds = false // the issue left the task's phase while the worker sat idle
	h.must(RequestDeliver{Claim: h.token, Task: "test the head", Phase: phase.Testing})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim still holds %+v after the issue left the task's phase", p)
	}

	// The wait the task ended does not fire at that wait's old deadline.
	h.advance(testIdleSuspend / 2)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)

	h.advance(testIdleSuspend / 2)
	h.wantState(StateSuspended)
	if by := h.claim().SuspendedBy; by != SuspendedByIdle {
		t.Fatalf("suspended by %q, want idle", by)
	}
}

// A turn the agent started itself — an Envoy notice's, a human's steer — is the agent at work, so
// it ends the wait as a task does, and the next wait begins whole at that turn's end rather than
// at the deadline the claim had before it.
func TestATurnOfTheAgentsOwnStartsTheWaitAgainAtItsEnd(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend / 2)

	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	h.advance(testIdleSuspend / 6)
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)

	h.advance(testIdleSuspend / 2)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)

	h.advance(testIdleSuspend / 2)
	h.wantState(StateSuspended)
}

// What else reaches an idle claim is not the worker's own work and does not postpone its
// suspension: the shim reconnecting hands the agent nothing, so the wait it finds running keeps
// the deadline it had.
func TestAShimReconnectDoesNotPostponeTheIdleSuspend(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend / 2)
	h.connect()
	h.wantState(StateIdle)

	h.advance(testIdleSuspend / 2)
	h.wantState(StateSuspended)
}

// A claim in a turn is never suspended by the wait: nothing arms it while the agent works, and the
// turn's own end starts the next one.
func TestAWorkingClaimIsNeverIdleSuspended(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateWorking)
	h.advance(2 * testIdleSuspend)
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
}

// The idle suspend is for a phase worker only. An architect idling is its tree waiting for its own
// events, and the project controller's life is the keeper's, not a wait's; nothing resumes either
// by rule, so a wait that suspended one would strand it and every notice its tree published. A
// claim on either role arms nothing however long it idles, whatever worker_idle_suspend_seconds
// is.
func TestAnArchitectAndTheControllerAreNeverIdleSuspended(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim Claim
	}{
		{"architect", rootClaim()},
		{"controller", Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: StateQueued}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := idleHarnessOf(t, tc.claim, nil)
			h.reach(StateIdle)
			if _, armed := h.m.timers[TimerIdle]; armed {
				t.Fatalf("the idle suspend is armed for %s, want it armed for phase workers only", tc.name)
			}
			h.advance(100 * testIdleSuspend)
			h.wantState(StateIdle)
			h.wantCalls("Suspend", 0)
		})
	}
}

// Which roles the idle suspend is for, in one place: the five phase workers, and nothing else.
// The daemon reads the same answer when it decides whether a message that found no holder may
// resume the worker it names (internal/daemon's wakeIdleSuspendedRole), so a role moved into or
// out of the list moves both.
func TestTheIdleSuspendIsForEveryPhaseWorkerAndNoOtherRole(t *testing.T) {
	want := map[claim.Role]bool{
		claim.RolePlanner:     true,
		claim.RoleImplementer: true,
		claim.RoleTester:      true,
		claim.RoleReviewer:    true,
		claim.RoleMerger:      true,
		claim.RoleArchitect:   false,
		claim.RoleController:  false,
	}
	for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
		if got := IdleSuspendable(role); got != want[role] {
			t.Errorf("IdleSuspendable(%s) = %t, want %t", role, got, want[role])
		}
	}
	if len(want) != len(claim.Roles)+1 {
		t.Fatalf("%d roles are judged here, want every one of claim.Roles and the controller", len(want))
	}
}

// A worker that survives a daemon restart is idle-suspended like any other: the restart's machine
// starts the wait from the claim it read back, where only a turn's end ever armed it before and an
// idle worker re-adopted after a restart kept its process for as long as its issue ran.
func TestAnIdleWorkerRestoredAtBootIsSuspendedWhenItsWaitIsSpent(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.restart()
	h.wantState(StateIdle)

	h.advance(testIdleSuspend)
	h.wantState(StateSuspended)
	if by := h.claim().SuspendedBy; by != SuspendedByIdle {
		t.Fatalf("suspended by %q, want idle", by)
	}
	h.wantCalls("Suspend", 1)
}

// A worker read back still holding a task is given no wait at all: the wait is for a worker with
// nothing to do, and this one's task reaches it as soon as its shim says hello. The turn that task
// becomes starts the first wait at its end.
func TestAnIdleWorkerRestoredHoldingATaskIsGivenNoWait(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.conns.Remove(h.token) // no connection: the task waits for the shim's hello
	h.must(RequestDeliver{Claim: h.token, ID: "d-restored", Task: "test the head"})
	h.restart()

	if live := h.clock.Live(); live != 0 {
		t.Fatalf("%d timers armed for a claim read back holding its task, want none", live)
	}
	h.advance(2 * testIdleSuspend)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
}

// The next task resumes the same agent: the resume carries the session file the idle suspend kept,
// and no fresh agent is spawned in its place.
func TestAWakeAfterTheIdleSuspendResumesTheSameSession(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend)
	h.wantState(StateSuspended)
	h.must(RequestDeliver{Claim: h.token, ID: "d-after-idle", Task: "the next phase's task"})
	resumes := h.calls("Resume")
	if len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumes = %+v, want one resume of %s", resumes, sessionFile)
	}
	h.wantCalls("Spawn", 1)
}

// worker_idle_suspend_seconds 0 arms nothing: an idle worker keeps its process for as long as the
// deployment leaves it there.
func TestWithTheIdleSuspendOffAnIdleWorkerKeepsItsProcess(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.advance(100 * testIdleSuspend)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
}

// A workflow suspension of a claim the idle suspend holds takes the suspension over: the issue
// left the workflow — its child was parked, its tree lingers — and only the workflow may put that
// worker back, while a claim left marked `idle` would be resumed by the next message to its role
// (internal/daemon's wakeIdleSuspendedRole). The reverse never happens: the idle suspend's timer
// is armed for no suspended claim.
func TestAWorkflowSuspensionTakesOverTheIdleSuspends(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend)
	h.wantState(StateSuspended)
	h.must(RequestSuspend{Claim: h.token, Reason: "its child was parked", By: SuspendedByDaemon})
	if c := h.claim(); c.SuspendedBy != SuspendedByDaemon {
		t.Fatalf("suspended by %q, want daemon", c.SuspendedBy)
	}
	if stored := h.store.load(h.token); stored.SuspendedBy != SuspendedByDaemon {
		t.Fatalf("stored suspended by %q, want daemon", stored.SuspendedBy)
	}
}
