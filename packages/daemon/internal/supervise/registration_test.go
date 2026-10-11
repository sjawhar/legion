package supervise

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

func TestTheRegistrationDeadlineRetiresALiveUnregisteredProcess(t *testing.T) {
	for _, state := range []ClaimState{StateLaunching, StateShimConnected} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			alive := h.locator()

			h.advance(deadline)

			if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
				t.Errorf("suspended %+v, want the live process", suspends[0].Locator)
			}
			h.wantCalls("Release", 0)
			h.wantBudgets(Budgets{LaunchFailures: 1})
			h.wantCalls("Spawn", 2)
			methods := h.rt.Methods()
			var spawns []int
			for i, method := range methods {
				if method == "Spawn" {
					spawns = append(spawns, i)
				}
			}
			if slices.Index(methods, "Suspend") > spawns[1] {
				t.Errorf("runtime calls %v: the relaunch came before the suspension", methods)
			}
			h.wantState(StateLaunching)
		})
	}
}

// The runtime's ProvisionBound adds onto the base Boot×RegistrationIntervals deadline while a
// claim is still StateLaunching (armRegistration, machine.go): a live, unregistered process is
// left alone through the base deadline and the bound both, but once the whole extended deadline
// passes it is retired exactly as TestTheRegistrationDeadlineRetiresALiveUnregisteredProcess
// expects with no bound at all (the registration deadline still fires — it is only sized wider,
// never disabled).
func TestTheRegistrationDeadlineWaitsOutProvisioningThenStillRetires(t *testing.T) {
	h := newBareHarness(t)
	h.rt.ProvisionGrace = testBoot
	h.start(queuedClaim())
	h.launch()
	alive := h.locator()

	h.advance(deadline)
	h.wantCalls("Suspend", 0)
	h.wantState(StateLaunching)
	if h.locator() != alive {
		t.Fatalf("locator changed to %+v before the bound ran out, want the same process still live", h.locator())
	}

	h.advance(testBoot)
	if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
		t.Errorf("suspended %+v, want the live process", suspends[0].Locator)
	}
	h.wantCalls("Spawn", 2)
	h.wantState(StateLaunching)
}

// The shim's first hello (helloed, table.go) re-arms the registration deadline at the base
// Boot×RegistrationIntervals alone: armRegistration adds the runtime's ProvisionBound only while
// the claim is still StateLaunching, and helloed sets StateShimConnected before it re-arms, so a
// claim that hellos and never registers is retired counted from the hello, not from launch + base
// + the provisioning bound.
func TestTheRegistrationDeadlineReArmsAtTheBaseBoundFromTheFirstHello(t *testing.T) {
	h := newBareHarness(t)
	h.rt.ProvisionGrace = 10 * testBoot
	h.start(queuedClaim())
	h.launch()
	alive := h.locator()

	// Well inside the bound (launch + base + bound is 13×testBoot here): the hello arrives, and
	// the deadline re-arms from it rather than continuing to count toward the wider bound.
	h.advance(2 * testBoot)
	h.connect()
	h.wantState(StateShimConnected)
	h.wantCalls("Suspend", 0)

	// Just short of hello + base: not yet retired — a too-short re-arm (Boot alone, or one Probe
	// interval) would already have fired by here.
	h.advance(deadline - time.Nanosecond)
	h.wantCalls("Suspend", 0)

	// hello + base exactly (2×testBoot + deadline = 5×testBoot total), nowhere near launch + base
	// + bound (13×testBoot): still retired, since the re-arm dropped the bound rather than adding
	// to it.
	h.advance(time.Nanosecond)
	if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
		t.Errorf("suspended %+v, want the live process", suspends[0].Locator)
	}
	h.wantCalls("Spawn", 2)
}

func TestTheRegistrationDeadlineWithADeadProcessCountsOneFailureWithoutASuspension(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive}, fake.ProbeResult{Kind: runtime.Alive}, fake.ProbeResult{Kind: runtime.Gone})

	h.advance(deadline)

	h.wantCalls("Suspend", 0)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Spawn", 2)
}

func TestASuspendThatFailsAtTheDeadlineIsRetriedAtTheNextProbe(t *testing.T) {
	h := newHarness(t)
	h.launch()
	alive := h.locator()
	h.rt.FailSuspend(errBoom)

	h.advance(deadline)
	h.wantCalls("Suspend", 1)
	h.wantBudgets(Budgets{})
	h.wantState(StateLaunching)
	if h.locator() != alive {
		t.Fatalf("locator %+v, want the process the suspension could not end", h.locator())
	}

	h.rt.FailSuspend(nil)
	h.advance(testProbe)
	h.wantCalls("Suspend", 2)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Spawn", 2)
}

// A daemon that restarts while a claim is booting watches it again from its own start: the
// in-memory timers did not survive, and a process that never registers must still be retired.
func TestABootingClaimIsWatchedAgainAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateShimConnected)
	alive := h.locator()
	h.restart()

	h.advance(deadline)

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
		t.Errorf("suspended %+v, want the process that never registered", suspend.Locator)
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantState(StateLaunching)
}

// A daemon restart must not bring the provisioning bound back for a claim already past its hello:
// NewMachine's restore path arms StateLaunching and StateShimConnected alike through armBoot,
// which derives the bound from the claim's own state (armRegistration) — StateShimConnected never
// gets one, restored or not, so a restart cannot re-grant it to a claim that no longer needs it.
func TestARestartDoesNotReArmTheProvisioningBoundForAClaimAlreadyPastItsHello(t *testing.T) {
	h := newBareHarness(t)
	h.rt.ProvisionGrace = 10 * testBoot
	h.start(queuedClaim())
	h.reach(StateShimConnected)
	alive := h.locator()

	h.restart()

	// Just short of the base deadline alone: not yet retired.
	h.advance(deadline - time.Nanosecond)
	h.wantCalls("Suspend", 0)

	// At the base deadline alone, nowhere near base+bound (11×testBoot here): retired.
	h.advance(time.Nanosecond)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
		t.Errorf("suspended %+v, want the process that never registered", suspend.Locator)
	}
	h.wantState(StateLaunching)
}

// A claim restored in StateLaunching — a daemon restart caught it between the launch and the
// first probe, or its pod never reached its own container at all — keeps the full base+bound
// deadline from the restart, not the base alone: NewMachine's restore path arms StateLaunching
// through armBoot exactly as a fresh launch does, and armRegistration adds the bound only for
// that state. Deliberate, not yet ideal (a daemon that keeps restarting can keep re-granting the
// bound to a claim whose pod never progresses at all; recorded separately, dispatch://LEGION-585)
// — but today nothing in this package catches that StateLaunching case being changed to the base
// alone, so this pins it.
func TestARestartOfAClaimStillLaunchingKeepsTheFullProvisioningBound(t *testing.T) {
	h := newBareHarness(t)
	h.rt.ProvisionGrace = 10 * testBoot
	h.start(queuedClaim())
	h.launch()
	alive := h.locator()

	h.restart()

	// Just short of base+bound: not yet retired.
	h.advance(deadline + 10*testBoot - time.Nanosecond)
	h.wantCalls("Suspend", 0)

	// At base+bound exactly: retired.
	h.advance(time.Nanosecond)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
		t.Errorf("suspended %+v, want the process that never registered", suspend.Locator)
	}
	h.wantState(StateLaunching)
}

// An agent that registers and never says it is ready never became a working agent. The deadline
// runs again from the registration, and at it the live process is retired, one launch failure is
// charged, and the same session is resumed one generation later. Whether its shim said hello first
// changes nothing. The relaunched agent that says it is ready is sent the task the claim held.
func TestARegisteredAgentThatNeverSaysReadyIsRelaunchedAtTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{"registered over the API with no stream", false},
		{"registered after its shim said hello", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.launch()
			h.advance(testBoot)
			if tc.stream {
				h.connect()
				h.advance(testBoot)
			}
			h.register()
			h.wantState(StateRegistered)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			alive := h.locator()

			// Just short of the registration plus the deadline, and past any deadline armed before
			// the registration: left alone.
			h.advance(deadline - time.Nanosecond)
			h.wantCalls("Suspend", 0)
			h.wantState(StateRegistered)

			h.advance(time.Nanosecond)
			if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
				t.Errorf("suspended %+v, want the live process whose agent never said ready", suspends[0].Locator)
			}
			h.wantCalls("Release", 0)
			if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != sessionFile || resumed.Spec.Generation != 2 {
				t.Errorf("resumed %+v, want the registered session at generation 2", resumed.Spec)
			}
			h.wantBudgets(Budgets{LaunchFailures: 1})
			h.wantState(StateLaunching)
			h.wantPrompts(0)
			if lines := h.logs.lines("the agent registered and never said it was ready"); len(lines) != 1 {
				t.Errorf("logged %q, want one line naming the agent that never said ready", lines)
			}

			h.relaunched()
			if prompts := h.wantPrompts(1); prompts[0].Message != "implement the plan" {
				t.Errorf("the relaunched agent was prompted %q, want the task the claim held", prompts[0].Message)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A daemon restart does not end the wait: a claim restored registered is watched again from the
// restart, as a booting claim is, and relaunched at the deadline.
func TestARegisteredClaimIsWatchedAgainAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	alive := h.locator()
	h.restart()

	h.advance(deadline - time.Nanosecond)
	h.wantCalls("Suspend", 0)
	h.advance(time.Nanosecond)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != alive {
		t.Errorf("suspended %+v, want the process whose agent never said ready", suspend.Locator)
	}
	if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("resumed %+v, want the registered session", resumed.Spec)
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantState(StateLaunching)
}

// The same session registering again is issued a new secret and leaves the deadline where its first
// registration set it: an agent that keeps registering and never says ready is still relaunched.
func TestARepeatedRegistrationDoesNotPutTheDeadlineOff(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	h.advance(deadline / 2)
	h.register()
	h.advance(deadline - deadline/2)
	h.wantCalls("Suspend", 1)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantState(StateLaunching)
}

// A registered agent whose process is already gone at the deadline is counted once and relaunched,
// with nothing to suspend.
func TestARegisteredAgentWhoseProcessIsGoneAtTheDeadlineCountsOneFailureWithoutASuspension(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

	h.advance(deadline)

	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 1)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantState(StateLaunching)
}

// Relaunches whose agents keep registering and never say ready spend the launch failure budget: the
// claim fails at launch_failure_limit instead of relaunching for ever.
func TestAgentsThatNeverSayReadyFailTheClaimAtTheLaunchFailureLimit(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	for range testLimits().LaunchFailures - 1 {
		h.advance(deadline)
		h.wantState(StateLaunching)
		h.connect()
		h.register()
	}
	h.advance(deadline)
	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: testLimits().LaunchFailures})
	h.wantCalls("Suspend", testLimits().LaunchFailures)
}

// Ready ends the boot watch: from it the claim is prompted, and no deadline remains.
func TestReadyEndsTheBootWatch(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	if live := h.clock.Live(); live != 0 {
		t.Errorf("%d timers still armed once the agent is ready", live)
	}
	h.advance(2 * deadline)
	h.wantCalls("Probe", 0)
	h.wantCalls("Suspend", 0)
	h.wantState(StateReady)
}

// A ready whose first claim write fails stays ready in memory. Its retry must persist that ready
// state before it returns, so a restart restores ready rather than registered and never charges a
// launch failure for an agent that was ready.
func TestARetriedReadyAfterAClaimWriteFailureStaysReadyAcrossARestart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	h.store.fail("PutClaim", errBoom)

	err := h.handle(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ready over a failed claim write returned %v, want %v", err, errBoom)
	}
	h.wantState(StateReady)
	if live := h.clock.Live(); live != 0 {
		t.Fatalf("%d timers armed after failed ready, want none", live)
	}

	h.store.fail("PutClaim", nil)
	h.ready()
	h.restart()
	h.wantState(StateReady)
	h.advance(2 * deadline)
	h.wantCalls("Suspend", 0)
	h.wantBudgets(Budgets{})
}
