package supervise

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

// A worker that dies with work outstanding — its task's turn running, or the task pending, re-sent
// after an earlier death — is relaunched with that task, and the relaunch reaches ready again
// whatever killed it, so ready cannot bound those deaths. An agent that dies each time before it
// completes a turn — in the task's turn, or ready with the re-sent task not yet begun — is failed at
// the launch failure limit, where the workflow holds its issue, instead of relaunched for ever. Its
// task stays pending, for the retry to send.
func TestAnAgentThatDiesWithWorkOutstandingIsFailedAtTheLimit(t *testing.T) {
	for name, beforeDeath := range map[string]func(*harness){
		"dies in each turn of its task": func(h *harness) {
			h.must(StreamTurnStart{Claim: testToken})
			h.wantState(StateWorking)
		},
		"dies ready, before the re-sent task's turn": func(h *harness) { h.wantState(StateReady) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			h.must(StreamTurnStart{Claim: testToken})
			limit := testLimits().LaunchFailures
			for death := 1; death < limit; death++ {
				h.observe(runtime.Gone)
				h.relaunched()
				beforeDeath(h)
			}
			launches := len(h.calls("Resume"))

			h.observe(runtime.Gone)

			h.wantState(StateFailed)
			h.wantCalls("Resume", launches)
			h.wantBudgets(Budgets{Deaths: limit})
			stored := h.store.load(testToken)
			if stored.State != StateFailed || stored.Budgets.Deaths != limit || stored.Pending == nil {
				t.Errorf("stored claim %s with budgets %+v and pending %+v, want it failed at %d deaths with its task kept",
					stored.State, stored.Budgets, stored.Pending, limit)
			}
		})
	}
}

// A process that dies before its agent is ready is a launch failure, however much work the claim
// holds: Deaths counts only deaths after ready, so a boot death after earlier deaths with work is
// charged once, to LaunchFailures, and does not bring the claim nearer the deaths limit.
func TestADeathBeforeReadyIsALaunchFailureNotADeath(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)

	h.observe(runtime.Gone)

	h.wantBudgets(Budgets{LaunchFailures: 2, Deaths: 1})
}

// Deaths are charged against the task the claim holds, so they are cleared when that task ends and
// not by a turn that never ran it: a notice's turn colliding with the re-send is refused busy and
// its end leaves the interrupted task still waiting, and its deaths with it.
func TestATurnOfTheAgentsOwnDoesNotClearTheDeathsOfATaskItNeverRan(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)
	h.relaunched()
	resent := h.wantPrompts(2)[1].DeliveryID

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamLateRefusal{Claim: testToken, DeliveryID: resent, Error: "Agent is already processing a request"})
	h.must(StreamTurnEnd{Claim: testToken})

	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v, want the task still waiting", p)
	}
	h.wantBudgets(Budgets{Deaths: 1})
}

// A task that ends some other way than its turn's end — the daemon's suspension a transition or a
// close sends, held for the turn and run when the process dies first — takes its deaths with it:
// the claim's next round starts its count afresh. (An operator's forced stop does not end the task:
// it goes back interrupted for the operator's resume.)
func TestATaskThatEndsTakesItsDeathsWithIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "round one", Phase: "implementing", Generation: 5})
	h.must(StreamTurnStart{Claim: testToken})
	for range testLimits().LaunchFailures - 1 {
		h.observe(runtime.Gone)
		h.relaunched()
		h.must(StreamTurnStart{Claim: testToken})
	}
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.observe(runtime.Gone)
	h.wantState(StateSuspended)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the daemon's suspension, want the finished phase's task retired", p)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "round two", Phase: "implementing", Generation: 6})
	h.relaunched()
	h.must(StreamTurnStart{Claim: testToken})
	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1})
}

// Deaths are counted until the agent next completes a turn, so an agent that finishes each
// re-sent task after the death that interrupted it is relaunched every time: a pod lost now and
// then to its node is not a broken agent.
func TestACompletedTurnAfterEachDeathKeepsTheClaimRelaunching(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	for range 2 * testLimits().LaunchFailures {
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.must(StreamTurnStart{Claim: testToken})
		h.observe(runtime.Gone)
		h.relaunched()
		h.must(StreamTurnStart{Claim: testToken})
		h.must(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)
	}
	h.wantBudgets(Budgets{})
}

// A parked agent — idle, with nothing pending — that the environment kills now and then (a node
// drained, a pod evicted) is relaunched each time: with no work outstanding its deaths strand
// nothing, so they are not counted against it.
func TestAParkedAgentWithNothingPendingIsRelaunchedHoweverOftenItDies(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	for range 2 * testLimits().LaunchFailures {
		h.observe(runtime.Gone)
		h.relaunched()
	}
	h.wantState(StateReady)
	h.wantBudgets(Budgets{})
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

// testLaunchBackoff is the first wait before a launch tried again after a charged launch failure
// in every machine the harness builds (Timeouts.LaunchBackoff). The harness's clock records each
// wait rather than waiting it, so it costs the suite nothing.
const testLaunchBackoff = 2 * time.Second

// Each try that follows a charged launch failure waits first, doubling, so a fault that lasts a
// moment cannot spend a claim's whole launch budget in that moment: the second try waits between
// half the first step of the backoff and the whole of it, the third between the whole of it and
// twice it. That holds for a launch the runtime refuses, which the launch tries again itself, and
// for a process that dies before its agent is ready, which relaunches after its charge.
func TestChargedLaunchFailuresAreSpacedByTheBackoff(t *testing.T) {
	for name, fail := range map[string]func(*harness){
		"a launch the runtime refuses": func(h *harness) {
			h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})
			if err := h.handle(RequestSpawn{Claim: testToken}); !errors.Is(err, errBoom) {
				t.Fatalf("spawn returned %v, want the runtime's last refusal", err)
			}
		},
		"a process that dies before its agent is ready": func(h *harness) {
			h.launch()
			h.observe(runtime.Gone)
			h.observe(runtime.Gone)
			h.observe(runtime.Gone)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)

			fail(h)

			h.wantState(StateFailed)
			h.wantBudgets(Budgets{LaunchFailures: 3})
			h.clock.mu.Lock()
			slept := slices.Clone(h.clock.slept)
			h.clock.mu.Unlock()
			if len(slept) != 2 {
				t.Fatalf("the machine waited %v before its tries, want two waits, before the second try and the third", slept)
			}
			for i, step := range []time.Duration{testLaunchBackoff, 2 * testLaunchBackoff} {
				if slept[i] < step/2 || slept[i] > step {
					t.Errorf("wait %d before try %d was %s, want between %s and %s", i+1, i+2, slept[i], step/2, step)
				}
			}
		})
	}
}

// A charged launch failure is written before the backoff waits, so the daemon's stop during the
// wait forgives nothing: the claim is stored launching, with the charge, for the next boot to
// launch again.
func TestAChargeOutlivesADaemonStopDuringTheBackoff(t *testing.T) {
	h := newHarness(t)
	h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom})
	stopping, stop := context.WithCancel(h.ctx)
	defer stop()
	// The daemon stops once the charged try is written, so its backoff is cut short.
	h.store.onPut = func(c Claim) {
		if c.Budgets.LaunchFailures == 1 {
			stop()
		}
	}

	if err := h.m.Handle(stopping, RequestSpawn{Claim: testToken}); !errors.Is(err, context.Canceled) {
		t.Fatalf("spawn whose backoff the stop cut short = %v, want the stop's context error", err)
	}

	h.wantCalls("Spawn", 1)
	if stored := h.store.load(testToken); stored.State != StateLaunching || stored.Budgets.LaunchFailures != 1 || stored.Locator != nil {
		t.Fatalf("stored claim after a stop in the backoff = %s with budgets %+v and locator %v, want launching with the charge and no process",
			stored.State, stored.Budgets, stored.Locator)
	}
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
	h.deps.Timeouts = Timeouts{Boot: 7 * testBoot, RegistrationIntervals: 1, RPC: 2 * testRPC, Probe: testProbe, Stop: testStop, LaunchBackoff: testLaunchBackoff}
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
		"Stop":                  func(d *Deps) { d.Timeouts.Stop = 0 },
		"LaunchBackoff":         func(d *Deps) { d.Timeouts.LaunchBackoff = 0 },
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

// A failed claim stays failed until someone decides otherwise: the architect's retry of a held
// phase decides it for a worker, and the operator's retry for an architect, which nothing else
// relaunches. Each relaunches the same session with fresh budgets.
func TestRetryRelaunchesAFailedClaimOnItsSessionWithFreshBudgets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim Claim
		retry Event
	}{
		{"the architect's retry of a worker", queuedClaim(), RequestRetry{Claim: testToken}},
		{"the operator's retry of an architect", rootClaim(), RequestOperatorRetry{Claim: rootToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, tc.claim)
			h.reach(StateReady)
			for range 3 {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			resumes := len(h.calls("Resume"))

			h.must(tc.retry)

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{})
			calls := h.wantCalls("Resume", resumes+1)
			if got := calls[len(calls)-1].Spec.ResumeSessionFile; got != sessionFile {
				t.Errorf("retry resumed session file %q, want the claim's %q", got, sessionFile)
			}
			h.relaunched()
			h.wantState(StateReady)
		})
	}
}

// The operator's retry is an architect's alone. A failed phase worker is its architect's to retry,
// and the daemon's controller its keeper's, so the operator's retry of either is refused, naming
// that retry, and changes nothing.
func TestTheOperatorsRetryOfAFailedClaimThatIsNotAnArchitectsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, names string
		claim       Claim
	}{
		{"a phase worker", "its architect's retry_or_escalate while its issue is held, and by the next start of its role otherwise", queuedClaim()},
		{"the controller", "the daemon's controller keeper retries the controller", controllerClaim()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, tc.claim)
			h.reach(StateReady)
			for range 3 {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			before, calls, puts := h.claim(), len(h.rt.Calls()), len(h.store.history())

			err := h.handle(RequestOperatorRetry{Claim: tc.claim.Token})

			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, ErrRetryArchitectOnly) || refused.State != StateFailed ||
				!strings.Contains(err.Error(), tc.names) {
				t.Fatalf("the operator's retry of %s returned %v, want the architect-only refusal naming %q", tc.name, err, tc.names)
			}
			if after := h.claim(); after.State != before.State || after.Budgets != before.Budgets || after.Generation != before.Generation {
				t.Errorf("the refused retry moved the claim: %+v -> %+v", before, after)
			}
			if got := h.rt.Calls()[calls:]; len(got) != 0 {
				t.Errorf("the refused retry asked the runtime %+v", got)
			}
			if got := len(h.store.history()); got != puts {
				t.Errorf("the refused retry wrote the claim %d times", got-puts)
			}
		})
	}
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
	t.Run("failed when prompt retirements ran out", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		for range 2 {
			h.advance(testRPC)
			h.observe(runtime.Alive)
		}
		h.advance(testRPC)
		h.relaunched()
		last := h.locator()
		for range 2 {
			h.advance(testRPC)
			h.observe(runtime.Alive)
		}
		h.advance(testRPC)
		h.wantState(StateFailed)
		wantRetryWaitsOut(t, h, last)
	})
	t.Run("failed when a registration deadline spent the launches", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.observe(runtime.Gone)
		h.advance(deadline)
		last := h.locator()
		h.advance(deadline)
		h.wantState(StateFailed)
		wantRetryWaitsOut(t, h, last)
	})
}
