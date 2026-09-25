package supervise

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

func TestLaunchFailuresRunOutIntoFailed(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.observe(runtime.Gone)
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 2})

	h.observe(runtime.Gone)

	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: 3})
	h.wantCalls("Spawn", 3)
	if h.claim().Locator != nil || h.clock.Live() != 0 {
		t.Errorf("failed claim %+v with %d timers, want no locator and nothing armed", h.claim(), h.clock.Live())
	}
	if stored := h.store.load(testToken); stored.State != StateFailed {
		t.Errorf("stored state %s, want failed", stored.State)
	}
}

// A claim that fails on a process it still records — found dead, the budget spent — has that
// process suspended, so nothing of it keeps holding a node until its tree closes.
func TestAFailedClaimSuspendsTheProcessItStillRecords(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	var dead runtime.Locator
	for range 3 {
		dead = h.locator()
		h.observe(runtime.Gone)
	}

	h.wantState(StateFailed)
	if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != dead {
		t.Errorf("suspended %+v, want the process the claim failed on %+v", suspends[0].Locator, dead)
	}
	h.wantCalls("Release", 0)
}

// A suspension that fails there is logged and the claim fails anyway: it is never left in the state
// it was in, relaunching nothing and waiting on a process nobody watches.
func TestAFailedClaimWhoseSuspendFailsStillFails(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.FailSuspend(errBoom)
	for range 3 {
		h.observe(runtime.Gone)
	}

	h.wantState(StateFailed)
	if stored := h.store.load(testToken); stored.State != StateFailed || stored.Locator != nil {
		t.Errorf("stored %+v, want the failure persisted with no locator", stored)
	}
	if lines := h.logs.lines("suspend", errBoom.Error()); len(lines) != 1 {
		t.Errorf("logged %v, want the failed suspension named once", lines)
	}
}

func TestASpawnTheRuntimeRefusesIsALaunchFailure(t *testing.T) {
	h := newHarness(t)
	h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom})

	h.must(RequestSpawn{Claim: testToken})

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Spawn", 2)
	if h.generation() != 2 {
		t.Errorf("generation %d, want the retry's 2", h.generation())
	}
}

func TestSpawnsTheRuntimeKeepsRefusingEndInFailed(t *testing.T) {
	h := newHarness(t)
	h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})

	err := h.handle(RequestSpawn{Claim: testToken})

	if !errors.Is(err, errBoom) {
		t.Errorf("spawn returned %v, want the runtime's last refusal", err)
	}
	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: 3})
	h.wantCalls("Spawn", 3)
	h.wantCalls("Suspend", 0)
}

func TestALaunchWhoseSpecCannotBeBuiltIsALaunchFailure(t *testing.T) {
	h := newHarness(t)
	h.specs.err = errBoom

	err := h.handle(RequestSpawn{Claim: testToken})

	if !errors.Is(err, errBoom) {
		t.Errorf("spawn returned %v, want the spec's error", err)
	}
	h.wantState(StateFailed)
	h.wantCalls("Spawn", 0)
}

func TestAResumeTheRuntimeRefusesIsALaunchFailure(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.ScriptResume(fake.SpawnResult{Err: errBoom})

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 2})
	h.wantCalls("Resume", 2)
}

// Three acknowledged prompts that start no turn — each retried at the next sweep — retire the
// pane and relaunch the same session; the second time the budget runs out, the retirement is
// terminal.
func TestPromptFailuresRetireAndRelaunchThenFail(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	retiring := h.locator()
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.wantBudgets(Budgets{PromptFailures: 2})
	h.wantPrompts(3)

	h.advance(testRPC)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != retiring {
		t.Errorf("suspended %+v, want the pane that took the prompts", suspend.Locator)
	}
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != retiring || resume.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("relaunched %+v after %+v, want the same session after the retired pane", resume.Spec, resume.Previous)
	}
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PromptRetires: 1})
	if queued := h.pending(); queued.Task != "the task" || !queued.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the task still queued", queued)
	}

	h.relaunched()
	sent := h.wantPrompts(4)[3]
	if sent.DeliveryID != h.pending().ID {
		t.Errorf("sent %+v after the relaunch, want the queued delivery", sent)
	}
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)

	h.wantState(StateFailed)
	h.wantBudgets(Budgets{PromptFailures: 3, PromptRetires: 2})
	h.wantCalls("Suspend", 2)
	h.wantCalls("Resume", 1)
	if h.pending().Task != "the task" {
		t.Errorf("pending %+v, want the undelivered task kept on the failed claim", h.pending())
	}
}

// A retirement whose suspension fails charges nothing; the rotated delivery goes out again at the
// next sweep, and the next failure retries the retirement.
func TestASuspendThatFailsAtThePromptLimitChargesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.FailSuspend(errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)
	h.observe(runtime.Alive)

	h.advance(testRPC)

	h.wantCalls("Suspend", 1)
	h.wantBudgets(Budgets{PromptFailures: 2})
	h.wantState(StateReady)
	if lines := h.logs.lines("suspend"); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), errBoom.Error()) {
		t.Errorf("logged %v, want the failed suspension named", lines)
	}
	h.observe(runtime.Alive)
	h.wantPrompts(4)

	h.rt.FailSuspend(nil)
	h.advance(testRPC)
	h.wantCalls("Suspend", 2)
	h.wantBudgets(Budgets{PromptRetires: 1})
	h.wantState(StateLaunching)
}

func TestAStartedTurnResetsBothPromptBudgets(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	for range 2 {
		h.advance(testRPC)
		h.observe(runtime.Alive)
	}
	h.advance(testRPC)
	h.relaunched()
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.wantBudgets(Budgets{PromptFailures: 1, PromptRetires: 1})

	h.must(StreamTurnStart{Claim: testToken})

	h.wantBudgets(Budgets{})
	if got := h.store.load(testToken).Budgets; got != (Budgets{}) {
		t.Errorf("stored budgets %+v, want the reset persisted", got)
	}
}

func TestEveryLimitAndTimeoutIsAConstructorParameter(t *testing.T) {
	h := newBareHarness(t)
	h.deps.Limits = Limits{LaunchFailures: 1, PromptFailures: 1, PromptRetires: 1}
	h.deps.Timeouts = Timeouts{Boot: 7 * testBoot, RegistrationIntervals: 1, RPC: 2 * testRPC, Probe: testProbe}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})

	h.advance(testRPC)
	h.wantBudgets(Budgets{})
	h.advance(testRPC)

	h.wantState(StateFailed)
	h.wantBudgets(Budgets{PromptFailures: 1, PromptRetires: 1})
	h.wantCalls("Suspend", 1)
}

func TestAMachineRefusesLimitsOrTimeoutsThatCannotWork(t *testing.T) {
	for name, mutate := range map[string]func(*Deps){
		"LaunchFailures":        func(d *Deps) { d.Limits.LaunchFailures = 0 },
		"PromptFailures":        func(d *Deps) { d.Limits.PromptFailures = 0 },
		"PromptRetires":         func(d *Deps) { d.Limits.PromptRetires = 0 },
		"Boot":                  func(d *Deps) { d.Timeouts.Boot = 0 },
		"RegistrationIntervals": func(d *Deps) { d.Timeouts.RegistrationIntervals = 0 },
		"RPC":                   func(d *Deps) { d.Timeouts.RPC = 0 },
		"Probe":                 func(d *Deps) { d.Timeouts.Probe = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newBareHarness(t)
			mutate(&h.deps)
			if _, err := NewMachine(h.ctx, h.deps, queuedClaim()); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("new machine with %s zero returned %v, want a refusal naming it", name, err)
			}
		})
	}
}

// A failed claim stays failed until someone decides otherwise; the architect's retry of a held
// phase is that decision. It relaunches the same session with fresh budgets.
func TestRetryRelaunchesAFailedClaimOnItsSessionWithFreshBudgets(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	for range 3 {
		h.observe(runtime.Gone)
	}
	h.wantState(StateFailed)
	resumes := len(h.calls("Resume"))

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	calls := h.wantCalls("Resume", resumes+1)
	if got := calls[len(calls)-1].Spec.ResumeSessionFile; got != sessionFile {
		t.Errorf("retry resumed session file %q, want the claim's %q", got, sessionFile)
	}
	h.relaunched()
	h.wantState(StateReady)
}

// A retry relaunches the same session, so it hands the runtime the process the claim last ran to
// wait out — the one its own exit released, or the one it failed on — rather than open a second
// agent on the session while the first may still be going.
func TestRetryWaitsOutTheProcessTheClaimLastRan(t *testing.T) {
	wantRetryWaitsOut := func(t *testing.T, h *harness, last runtime.Locator) {
		t.Helper()
		resumes := len(h.calls("Resume"))
		h.must(RequestRetry{Claim: testToken})
		calls := h.wantCalls("Resume", resumes+1)
		if prev := calls[len(calls)-1].Previous; prev == nil || *prev != last {
			t.Errorf("retry resumed waiting out %+v, want the process the claim last ran %+v", prev, last)
		}
	}
	t.Run("retired by its exit", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		last := h.locator()
		h.must(RequestExit{Claim: testToken, Generation: h.generation(), Session: session, Reason: "phase complete"})
		h.wantState(StateRetired)
		wantRetryWaitsOut(t, h, last)
	})
	t.Run("failed with its suspension refused", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.rt.FailSuspend(errBoom)
		h.observe(runtime.Gone)
		h.observe(runtime.Gone)
		last := h.locator()
		h.observe(runtime.Gone)
		h.wantState(StateFailed)
		wantRetryWaitsOut(t, h, last)
	})
}
