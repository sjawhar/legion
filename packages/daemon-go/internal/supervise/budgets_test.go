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

	stop := h.wantCalls("Stop", 1)[0]
	if stop.Locator != retiring || stop.Grace != testGrace {
		t.Errorf("stopped %+v with %s, want the pane that took the prompts", stop.Locator, stop.Grace)
	}
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Locator != retiring || resume.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("relaunched %+v after %+v, want the same session after the retired pane", resume.Spec, resume.Locator)
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
	h.wantCalls("Stop", 2)
	h.wantCalls("Resume", 1)
	if h.pending().Task != "the task" {
		t.Errorf("pending %+v, want the undelivered task kept on the failed claim", h.pending())
	}
}

// A retirement whose stop fails charges nothing; the rotated delivery goes out again at the next
// sweep, and the next failure retries the retirement.
func TestAStopThatFailsAtThePromptLimitChargesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.FailStop(errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)
	h.observe(runtime.Alive)

	h.advance(testRPC)

	h.wantCalls("Stop", 1)
	h.wantBudgets(Budgets{PromptFailures: 2})
	h.wantState(StateReady)
	if lines := h.logs.lines("stop"); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), errBoom.Error()) {
		t.Errorf("logged %v, want the failed stop named", lines)
	}
	h.observe(runtime.Alive)
	h.wantPrompts(4)

	h.rt.FailStop(nil)
	h.advance(testRPC)
	h.wantCalls("Stop", 2)
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
	h.deps.Timeouts = Timeouts{Boot: 7 * testBoot, RegistrationIntervals: 1, RPC: 2 * testRPC, Probe: testProbe, StopGrace: 3 * testGrace}
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
	if stop := h.wantCalls("Stop", 1)[0]; stop.Grace != 3*testGrace {
		t.Errorf("stop grace %s, want the constructed %s", stop.Grace, 3*testGrace)
	}
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
		"StopGrace":             func(d *Deps) { d.Timeouts.StopGrace = 0 },
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
