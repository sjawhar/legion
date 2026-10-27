package supervise

import (
	"testing"
	"time"
)

// testIdleSuspend is the idle suspend a test deployment gives its workers
// (worker_idle_suspend_seconds).
const testIdleSuspend = 30 * time.Minute

// idleHarness is a harness whose claim is suspended once it has been idle testIdleSuspend. The
// shipped testTimeouts leaves the idle suspend off, so every other test of this package is
// untouched by the wait.
func idleHarness(t *testing.T) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.Timeouts.IdleSuspend = testIdleSuspend
	c := queuedClaim()
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

// A task that reaches the claim before the wait is spent ends it: the claim keeps its process, and
// the next wait begins when that task's turn ends.
func TestATaskBeforeTheIdleSuspendKeepsTheProcess(t *testing.T) {
	h := idleHarness(t)
	h.reach(StateIdle)
	h.advance(testIdleSuspend / 2)
	h.must(RequestDeliver{Claim: h.token, Task: "review the head"})
	h.advance(testIdleSuspend)
	if got := h.state(); got == StateSuspended {
		t.Fatalf("state = %s, want a claim with a task to keep its process", got)
	}
	h.wantCalls("Suspend", 0)
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
