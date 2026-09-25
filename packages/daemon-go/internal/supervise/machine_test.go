package supervise

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

var liveStates = []ClaimState{StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}

func TestSpawnPersistsTheBootTokenHashBeforeItLaunches(t *testing.T) {
	h := newHarness(t)
	var spawnsAtPut []int
	h.store.onPut = func(Claim) { spawnsAtPut = append(spawnsAtPut, len(h.rt.CallsOf("Spawn"))) }

	h.launch()

	spec := h.wantCalls("Spawn", 1)[0].Spec
	c := h.claim()
	if spec.Claim != testToken || spec.Issue != "LEGION-209" || spec.Role != claim.RoleImplementer {
		t.Errorf("spawned %+v, want the claim's own identity", spec)
	}
	if spec.Generation != 1 || c.Generation != 1 {
		t.Errorf("generation spawned %d, held %d; want 1", spec.Generation, c.Generation)
	}
	if spec.BootToken == "" || !bytes.Equal(HashBootToken(spec.BootToken), c.BootTokenHash) {
		t.Errorf("the claim holds %x, not the hash of the boot token the launch carried", c.BootTokenHash)
	}
	if spec.ResumeSessionFile != "" || spec.Env["LEGION_ISSUE"] != "LEGION-209" {
		t.Errorf("spawned %+v, want a fresh launch with the specs' environment", spec)
	}
	history := h.store.history()
	first := history[1]
	if first.State != StateLaunching || first.Locator != nil || !bytes.Equal(first.BootTokenHash, c.BootTokenHash) || spawnsAtPut[0] != 0 {
		t.Errorf("first write %+v after %d spawns, want the launch's boot token hash before any spawn", first, spawnsAtPut[0])
	}
	if last := history[len(history)-1]; last.Locator == nil || *last.Locator != h.locator() {
		t.Errorf("last write %+v, want the spawned locator", last)
	}
}

func TestEveryLaunchMintsItsOwnBootToken(t *testing.T) {
	h := newHarness(t)
	h.launch()
	first := h.claim().BootTokenHash
	h.observe(runtime.Gone)

	spawns := h.wantCalls("Spawn", 2)
	if spawns[0].Spec.BootToken == spawns[1].Spec.BootToken {
		t.Error("the relaunch reused the first launch's boot token")
	}
	if bytes.Equal(first, h.claim().BootTokenHash) || h.generation() != 2 {
		t.Errorf("after the relaunch: hash %x, generation %d; want a new hash at generation 2", h.claim().BootTokenHash, h.generation())
	}
}

func TestHelloConnectsTheShim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateShimConnected)
}

func TestRegisterRecordsTheAgentBeforeItReturns(t *testing.T) {
	h := newHarness(t)
	h.reach(StateShimConnected)

	h.register()

	h.wantState(StateRegistered)
	stored := h.store.load(testToken)
	if stored.State != StateRegistered || stored.Session != session || stored.SessionFile != sessionFile ||
		string(stored.CapabilityHash) != "capability" {
		t.Errorf("stored %+v when register returned, want the session and capability hash", stored)
	}
}

// Under always-dial the agent can only register after its shim's hello was accepted, but the
// hello reaches the machine through the stream's channel and the registration through HTTP, so
// the registration can arrive first.
func TestARegistrationThatOvertakesTheHelloStillRegisters(t *testing.T) {
	h := newHarness(t)
	h.launch()

	h.register()
	h.wantState(StateRegistered)
	h.connect()
	h.wantState(StateRegistered)
}

func TestARegistrationThatCannotBePersistedFails(t *testing.T) {
	h := newHarness(t)
	h.reach(StateShimConnected)
	h.store.fail("PutClaim", errBoom)

	err := h.handle(RequestRegister{Claim: testToken, Generation: 1, Session: session, SessionFile: sessionFile})
	if !errors.Is(err, errBoom) {
		t.Fatalf("register over a failing store returned %v, want the store's error", err)
	}
}

func TestTheRecordedSessionMayRegisterAgain(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)

			h.must(RequestRegister{Claim: testToken, Generation: 1, Session: session, SessionFile: sessionFile, CapabilityHash: []byte("second")})

			h.wantState(state)
			if got := string(h.store.load(testToken).CapabilityHash); got != "second" {
				t.Errorf("stored capability hash %q, want the re-registration's", got)
			}
		})
	}
}

func TestAnotherSessionIsRefusedTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.observe(runtime.Gone) // relaunch: the same session is expected back

	err := h.handle(RequestRegister{Claim: testToken, Generation: 2, Session: "ses_intruder", SessionFile: "/x.jsonl"})
	if !errors.Is(err, claim.SameAgentRefusal) {
		t.Fatalf("register as another session returned %v, want the same-agent refusal", err)
	}
	if got := h.store.load(testToken).Session; got != session {
		t.Errorf("stored session %q, want %q", got, session)
	}
	h.wantState(StateLaunching)

	h.register()
	if err := h.handle(RequestReady{Claim: testToken, Generation: 2, Session: "ses_intruder"}); !errors.Is(err, claim.SameAgentRefusal) {
		t.Errorf("ready as another session returned %v, want the same-agent refusal", err)
	}
	h.wantState(StateRegistered)
}

func TestAStaleGenerationIsFenced(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)

	for _, ev := range []Event{
		RequestRegister{Claim: testToken, Generation: 1, Session: session, SessionFile: sessionFile},
		RequestReady{Claim: testToken, Generation: 1, Session: session},
		RequestExit{Claim: testToken, Generation: 1, Session: session, Reason: "old"},
	} {
		if err := h.handle(ev); !errors.Is(err, claim.StaleGeneration) {
			t.Errorf("%T at generation 1 of 2 returned %v, want the stale-generation refusal", ev, err)
		}
	}
	h.must(StreamHello{Claim: testToken, Generation: 1})
	h.must(StreamHello{Claim: testToken, Generation: 1})

	h.wantState(StateLaunching)
	if lines := h.logs.lines("dropped a stale event", "generation"); len(lines) != 1 {
		t.Errorf("stale hello logged %d times, want once: %v", len(lines), lines)
	}
}

func TestReadyResetsTheLaunchBudget(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.observe(runtime.Gone)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.connect()
	h.register()
	h.wantBudgets(Budgets{LaunchFailures: 1})

	h.ready()

	h.wantState(StateReady)
	h.wantBudgets(Budgets{})
	if got := h.store.load(testToken).Budgets; got != (Budgets{}) {
		t.Errorf("stored budgets %+v, want the reset persisted", got)
	}
}

// The plugin calls ready again whenever it regains its Envoy role; that is not a transition.
func TestARepeatedReadyIsAcceptedWithoutMovingTheClaim(t *testing.T) {
	for _, state := range []ClaimState{StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			h.ready()
			h.wantState(state)
		})
	}
}

func TestABootIntervalWithALiveProcessChangesNothing(t *testing.T) {
	for _, state := range []ClaimState{StateLaunching, StateShimConnected} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			h.advance(testBoot)
			h.wantCalls("Probe", 1)
			h.advance(testBoot)
			h.wantCalls("Probe", 2)
			h.wantState(state)
			h.wantBudgets(Budgets{})
			h.wantCalls("Suspend", 0)
		})
	}
}

func TestABootIntervalWithADeadProcessCountsOneLaunchFailure(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

	h.advance(testBoot)

	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Spawn", 2)
	h.wantCalls("Suspend", 0)
	h.wantState(StateLaunching)
	if h.generation() != 2 {
		t.Errorf("generation %d, want the relaunch's 2", h.generation())
	}
}

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

// The registration deadline relaunches the claim, so it suspends the unregistered process and never
// releases it: under a sandbox a release deletes the claim's objects, and a root's tree volume with
// them.
func TestTheRegistrationDeadlineRetiresARootThroughSuspendNeverRelease(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.launch()
	alive := h.locator()

	h.advance(deadline)

	if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
		t.Errorf("suspended %+v, want the root's live process", suspends[0].Locator)
	}
	h.wantCalls("Release", 0)
	h.wantCalls("Spawn", 2)
	h.wantState(StateLaunching)
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

func TestRegistrationEndsTheBootWatch(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	if live := h.clock.Live(); live != 0 {
		t.Errorf("%d timers still armed after registration", live)
	}
	h.advance(2 * deadline)
	h.wantCalls("Probe", 0)
	h.wantCalls("Suspend", 0)
	h.wantState(StateRegistered)
}

func TestAProcessFoundGoneIsRelaunchedAsTheSameSession(t *testing.T) {
	for _, kind := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess} {
		for _, state := range liveStates {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				h := newHarness(t)
				h.reach(state)
				dead := h.locator()

				h.observe(kind)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{LaunchFailures: 1})
				registered := state != StateLaunching && state != StateShimConnected
				if !registered {
					h.wantCalls("Spawn", 2)
					h.wantCalls("Resume", 0)
					return
				}
				resume := h.wantCalls("Resume", 1)[0]
				if resume.Previous == nil || *resume.Previous != dead || resume.Spec.ResumeSessionFile != sessionFile || resume.Spec.Generation != 2 {
					t.Errorf("resumed %+v from %+v, want the same session after the dead incarnation, at generation 2", resume.Spec, resume.Previous)
				}
				if state == StateWorking {
					if p := h.claim().Pending; p != nil {
						t.Errorf("pending %+v, want the delivery the dead turn had confirmed retired", p)
					}
					if _, ok := h.store.delivery(testToken); ok {
						t.Error("the store still holds the confirmed delivery")
					}
				}
			})
		}
	}
}

func TestAClosedStreamIsJudgedByAProbe(t *testing.T) {
	for _, state := range liveStates {
		t.Run(string(state)+"/gone", func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			closed := h.locator()
			h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

			h.must(StreamClosed{Claim: testToken})

			if probe := h.wantCalls("Probe", 1)[0]; probe.Locator != closed {
				t.Errorf("probed %+v, want the claim's process", probe.Locator)
			}
			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{LaunchFailures: 1})
		})
		t.Run(string(state)+"/alive", func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)

			h.must(StreamClosed{Claim: testToken})

			h.wantState(state)
			h.wantBudgets(Budgets{})
		})
	}
}

func TestUncertainIsNeitherAliveNorGone(t *testing.T) {
	for _, state := range liveStates {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			probesBefore := len(h.calls("Probe"))

			h.observe(runtime.Uncertain)
			h.wantState(state)
			if got := h.claim().UncertainStreak; got != 1 {
				t.Fatalf("uncertain streak %d, want 1", got)
			}
			if got := h.store.load(testToken).UncertainStreak; got != 1 {
				t.Errorf("stored uncertain streak %d, want 1", got)
			}

			h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Uncertain})
			h.advance(testProbe)
			h.wantCalls("Probe", probesBefore+1)
			if got := h.claim().UncertainStreak; got != 2 {
				t.Fatalf("uncertain streak after a second uncertain probe %d, want 2", got)
			}
			if lines := h.logs.lines("uncertain", "streak=2"); len(lines) != 1 {
				t.Errorf("logged %v, want the streak of 2 once", lines)
			}

			h.advance(testProbe)
			if got := h.claim().UncertainStreak; got != 0 {
				t.Errorf("uncertain streak after an alive probe %d, want 0", got)
			}
			h.wantState(state)
			h.wantBudgets(Budgets{})
		})
	}
}

func TestAProbeTheRuntimeCannotAnswerIsRetriedWithoutAVerdict(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.ScriptProbe(fake.ProbeResult{Err: errBoom})
	h.observe(runtime.Uncertain)

	h.advance(testProbe)
	if got := h.claim().UncertainStreak; got != 1 {
		t.Fatalf("uncertain streak after a failed probe %d, want it unchanged at 1", got)
	}
	h.wantState(StateReady)

	h.advance(testProbe)
	h.wantCalls("Probe", 2)
	if got := h.claim().UncertainStreak; got != 0 {
		t.Errorf("uncertain streak after the retried probe %d, want 0", got)
	}
}

func TestSuspendKeepsTheSessionAndDropsTheLocator(t *testing.T) {
	for _, state := range []ClaimState{StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			loc := h.locator()

			h.must(RequestSuspend{Claim: testToken})

			if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
				t.Errorf("suspended %+v, want %+v", suspend.Locator, loc)
			}
			h.wantState(StateSuspended)
			c := h.claim()
			if c.Locator != nil || c.SessionFile != sessionFile || c.Session != session {
				t.Errorf("suspended claim %+v, want no locator and the session kept", c)
			}
			if stored := h.store.load(testToken); stored.State != StateSuspended || stored.Locator != nil {
				t.Errorf("stored %+v, want the suspension persisted", stored)
			}
			if state == StateWorking && c.Pending != nil {
				t.Errorf("pending %+v, want the interrupted turn's delivery retired", c.Pending)
			}
		})
	}
}

func TestASuspendThatFailsChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	loc := h.locator()
	h.rt.FailSuspend(errBoom)

	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("suspend returned %v, want the runtime's error", err)
	}
	h.wantState(StateIdle)
	if h.locator() != loc {
		t.Errorf("locator %+v, want %+v", h.locator(), loc)
	}
}

func TestSuspendingASuspendedClaimIsANoOp(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)
	h.must(RequestSuspend{Claim: testToken})
	h.wantCalls("Suspend", 1)
}

func TestResumeRelaunchesTheSameSessionAfterTheSuspendedIncarnation(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	suspended := h.locator()
	h.must(RequestSuspend{Claim: testToken})

	h.must(RequestResume{Claim: testToken})

	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != suspended || resume.Spec.ResumeSessionFile != sessionFile || resume.Spec.Generation != 2 {
		t.Errorf("resumed %+v waiting out %+v, want the session after the suspended incarnation", resume.Spec, resume.Previous)
	}
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	h.relaunched()
	h.wantState(StateReady)
}

// A restart forgets the suspended incarnation; by the time a claim is suspended its process was
// stopped, so the resume has nothing to wait out and says so with the zero locator.
func TestResumeAfterARestartHasNoIncarnationToWaitOut(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)
	h.restart()

	h.must(RequestResume{Claim: testToken})

	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous != nil || resume.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("resumed %+v waiting out %+v, want the session and no previous incarnation", resume.Spec, resume.Previous)
	}
}

func TestADeliveryToASuspendedClaimResumesIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)

	h.must(RequestDeliver{Claim: testToken, Task: "answer the review"})

	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.relaunched()
	prompts := h.wantPrompts(2)
	if prompts[1].Message != "answer the review" || prompts[1].DeliveryID != h.pending().ID {
		t.Errorf("sent %+v, want the queued task under its delivery id", prompts[1])
	}
}

// Stop ends the claim, so it releases whatever the claim holds — with its locator when a process
// runs, and with none when nothing does: under a sandbox a claim with no process still has objects
// the release deletes by the claim's name.
func TestStopReleasesAndRetiresTheClaim(t *testing.T) {
	for _, state := range liveStates {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			loc := h.locator()

			h.must(RequestStop{Claim: testToken})

			wantReleasedAt(t, h, testToken, loc)
			h.wantState(StateRetired)
			if h.claim().Locator != nil || h.clock.Live() != 0 {
				t.Errorf("retired claim %+v with %d timers armed, want no locator and no timers", h.claim(), h.clock.Live())
			}
		})
	}
	for _, state := range []ClaimState{StateQueued, StateSuspended} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			h.must(RequestStop{Claim: testToken})
			wantReleasedWithNoLocator(t, h, testToken)
			h.wantState(StateRetired)
		})
	}
	t.Run("failed", func(t *testing.T) {
		h := newHarness(t)
		h.specs.err = errBoom
		if err := h.handle(RequestSpawn{Claim: testToken}); !errors.Is(err, errBoom) {
			t.Fatalf("spawn returned %v", err)
		}
		h.wantState(StateFailed)
		h.must(RequestStop{Claim: testToken})
		wantReleasedWithNoLocator(t, h, testToken)
		h.wantState(StateRetired)
	})
	t.Run("retired", func(t *testing.T) {
		h := newHarness(t)
		h.must(RequestStop{Claim: testToken})
		h.must(RequestStop{Claim: testToken})
		h.wantCalls("Release", 1)
		h.wantState(StateRetired)
	})
}

// wantReleasedAt is one Release of token at its process loc.
func wantReleasedAt(t *testing.T, h *harness, token claim.Token, loc runtime.Locator) {
	t.Helper()
	released := h.wantCalls("Release", 1)[0].Released
	if released.Claim != token || released.Locator == nil || *released.Locator != loc {
		t.Errorf("released %+v, want %s at %+v", released, token, loc)
	}
}

// wantReleasedWithNoLocator is one Release of token that carried no locator.
func wantReleasedWithNoLocator(t *testing.T, h *harness, token claim.Token) {
	t.Helper()
	released := h.wantCalls("Release", 1)[0].Released
	if released.Claim != token || released.Locator != nil {
		t.Errorf("released %+v, want %s with no locator", released, token)
	}
}

func TestAStopThatFailsChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.FailRelease(errBoom)
	if err := h.handle(RequestStop{Claim: testToken}); !errors.Is(err, errBoom) {
		t.Fatalf("stop returned %v, want the runtime's error", err)
	}
	h.wantState(StateReady)
}

// A worker's agent reporting its own end ends the claim: the process is released — under tmux a
// pane still verified as the agent's is stopped, under a sandbox the claim's objects are deleted
// rather than left for the orphan sweep.
func TestAWorkerClaimsExitReleasesItAndRecordsWhy(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			loc := h.locator()

			h.must(RequestExit{Claim: testToken, Generation: 1, Session: session, Reason: "phase complete"})

			wantReleasedAt(t, h, testToken, loc)
			h.wantCalls("Suspend", 0)
			h.wantState(StateRetired)
			if h.claim().Locator != nil {
				t.Error("a retired claim kept its locator")
			}
			if lines := h.logs.lines("exit", "phase complete"); len(lines) != 1 {
				t.Errorf("logged %v, want the exit and its reason once", lines)
			}
		})
	}
}

// The tree's root claim is not retired before its tree closes: its agent's exit suspends it, so
// the claim keeps its session and stays resumable — and known to the orphan sweep, which under a
// sandbox would otherwise delete its Sandbox and the tree volume with it.
func TestARootClaimsExitSuspendsIt(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			loc := h.locator()

			h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

			if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
				t.Errorf("suspended %+v, want %+v", suspend.Locator, loc)
			}
			h.wantCalls("Release", 0)
			h.wantState(StateSuspended)
			c := h.claim()
			if c.Locator != nil || c.Session != session || c.SessionFile != sessionFile || h.clock.Live() != 0 {
				t.Errorf("claim %+v with %d timers armed, want no locator, the session kept, and nothing armed", c, h.clock.Live())
			}
			if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.Locator != nil {
				t.Errorf("stored %+v, want the suspension persisted", stored)
			}

			h.must(RequestResume{Claim: rootToken})
			if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != loc || resume.Spec.ResumeSessionFile != sessionFile {
				t.Errorf("resumed %+v waiting out %+v, want the session after the suspended incarnation", resume.Spec, resume.Previous)
			}
		})
	}
}

// An exit is the agent's own end, so a runtime that cannot suspend its process does not keep a
// finished root live: the error is logged and the claim is suspended anyway, its secret revoked,
// with the process remembered for the resume to wait out.
func TestARootExitWhoseSuspendFailsStillSuspendsIt(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	loc := h.locator()
	h.rt.FailSuspend(errBoom)

	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

	h.wantState(StateSuspended)
	if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.Locator != nil || stored.CapabilityHash != nil {
		t.Errorf("stored %+v, want the root suspended with no process and no capability", stored)
	}
	if lines := h.logs.lines("suspend", errBoom.Error()); len(lines) != 1 {
		t.Errorf("logged %v, want the failed suspension named once", lines)
	}
	h.rt.FailSuspend(nil)
	h.must(RequestResume{Claim: rootToken})
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != loc {
		t.Errorf("resumed waiting out %+v, want the exited process %+v", resume.Previous, loc)
	}
}

// Likewise a worker's exit retires its claim even when the runtime cannot release its process; only
// the operator's stop keeps a claim whose release failed.
func TestAWorkerExitWhoseReleaseFailsStillRetiresIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.rt.FailRelease(errBoom)

	h.must(RequestExit{Claim: testToken, Generation: h.generation(), Session: session, Reason: "phase complete"})

	h.wantState(StateRetired)
	if stored := h.store.load(testToken); stored.State != StateRetired || stored.Locator != nil || stored.CapabilityHash != nil {
		t.Errorf("stored %+v, want the claim retired with no process and no capability", stored)
	}
	if lines := h.logs.lines("release", errBoom.Error()); len(lines) != 1 {
		t.Errorf("logged %v, want the failed release named once", lines)
	}
}

// The tree's close is the one thing that releases its root: a root the daemon already suspended
// has no process, so the release carries no locator.
func TestTreeCloseReleasesASuspendedRootWithNoLocator(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})
	h.wantState(StateSuspended)

	h.must(RequestStop{Claim: rootToken})

	wantReleasedWithNoLocator(t, h, rootToken)
	h.wantState(StateRetired)
}

// The agent of a claim the daemon suspended, stopped, or gave up on reports its own end as it
// goes; that report is the expected echo of the daemon's own act.
func TestAnExitAfterTheDaemonEndedTheProcessIsAccepted(t *testing.T) {
	for _, end := range []struct {
		state ClaimState
		ended func(*harness)
	}{
		{StateSuspended, func(h *harness) { h.must(RequestSuspend{Claim: testToken}) }},
		{StateRetired, func(h *harness) { h.must(RequestStop{Claim: testToken}) }},
		{StateFailed, func(h *harness) {
			for range 3 {
				h.observe(runtime.Gone)
			}
		}},
	} {
		t.Run(string(end.state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			end.ended(h)
			h.wantState(end.state)

			h.must(RequestExit{Claim: testToken, Generation: h.generation(), Session: session, Reason: "shutdown"})

			h.wantState(end.state)
		})
	}
}

// Astra's cases, verbatim from dispatch://LEGION-208/spec, section "Testing": "supervision
// state-machine tests against a fake runtime and clock, including Astra's cases (stale incarnation
// after a generation change; suspension not yet complete; duplicate observations; crash between
// turn start and delivery persistence; a `failed` claim stays failed across a restart)".

func TestAstraStaleIncarnationAfterAGenerationChange(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	old := h.locator()
	h.observe(runtime.Gone)
	current := h.locator()
	if current.Incarnation == old.Incarnation || h.generation() != 2 {
		t.Fatalf("relaunch left incarnation %s at generation %d", current.Incarnation, h.generation())
	}

	for _, kind := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess, runtime.Uncertain, runtime.Alive} {
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: old, Kind: kind, At: h.clock.Now()}})
	}
	h.must(StreamHello{Claim: testToken, Generation: 1})

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Resume", 1)
	if h.claim().UncertainStreak != 0 || h.locator() != current {
		t.Errorf("claim %+v, want the current incarnation untouched", h.claim())
	}
	if lines := h.logs.lines("dropped a stale event", old.Incarnation); len(lines) != 1 {
		t.Errorf("the stale incarnation was logged %d times, want once: %v", len(lines), lines)
	}
}

func TestAstraSuspensionNotYetComplete(t *testing.T) {
	h := newBareHarness(t)
	gated := &gatedRuntime{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Runtime = gated
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateIdle)
	suspending := h.locator()

	suspended := make(chan error, 1)
	go func() { suspended <- h.m.Handle(h.ctx, RequestSuspend{Claim: testToken}) }()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("suspend never reached the runtime")
	}
	delivered := make(chan error, 1)
	go func() { delivered <- h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the steer"}) }()
	time.Sleep(50 * time.Millisecond)
	if resumes := h.calls("Resume"); len(resumes) != 0 {
		t.Fatal("resumed while the suspension was still in the runtime")
	}
	close(gated.release)
	for _, done := range []chan error{suspended, delivered} {
		if err := <-done; err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	h.m.Wait()

	methods := h.rt.Methods()
	if slices.Index(methods, "Resume") < slices.Index(methods, "Suspend") {
		t.Fatalf("runtime calls %v: the resume overlapped the suspension", methods)
	}
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != suspending {
		t.Errorf("resume waits out %+v, want the incarnation being suspended %+v", resume.Previous, suspending)
	}
	h.wantState(StateLaunching)

	// What the suspended process says on its way out changes nothing about the resumed one.
	h.must(StreamTurnEnd{Claim: testToken})
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: suspending, Kind: runtime.Gone, At: h.clock.Now()}})
	h.wantCalls("Resume", 1)
	h.wantCalls("Spawn", 1)
	h.wantBudgets(Budgets{})
	h.wantState(StateLaunching)
}

func TestAstraDuplicateObservations(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	dead := h.locator()
	gone := RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}}

	h.must(gone)
	h.must(gone)

	h.wantCalls("Resume", 1)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	if lines := h.logs.lines("dropped a stale event", dead.Incarnation); len(lines) != 1 {
		t.Errorf("the duplicate was logged %d times, want once: %v", len(lines), lines)
	}

	h.relaunched()
	h.observe(runtime.Alive)
	h.observe(runtime.Alive)
	h.wantState(StateReady)
	h.wantCalls("Resume", 1)
}

// The daemon sent the task, the agent acknowledged it and started the turn, and the daemon went
// down before it recorded the turn. After the restart the delivery reads as sent and unconfirmed;
// the agent reporting a turn in flight confirms it, and nothing is sent twice.
func TestAstraCrashBetweenTurnStartAndDeliveryPersistence(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	sent := h.wantPrompts(1)[0]
	if stored, ok := h.store.delivery(testToken); !ok || stored.DeliveredAt.IsZero() || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("stored delivery %+v, want it acknowledged and unconfirmed", stored)
	}

	h.restart()
	h.conn.SetStreaming(true)
	h.connect()

	h.wantState(StateWorking)
	h.wantPrompts(1)
	if p := h.pending(); p.ID != sent.DeliveryID || p.ConfirmedAt.IsZero() {
		t.Errorf("pending %+v, want the sent delivery confirmed", p)
	}
	if stored, _ := h.store.delivery(testToken); stored.ConfirmedAt.IsZero() {
		t.Error("the confirmation was not persisted")
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the delivery outlived its turn")
	}
}

// The same crash with an agent that is not streaming: the delivery is sent again under the same
// id, which the shim answers without a second turn if it already had it.
func TestAfterARestartAnIdleAgentIsSentTheSameDelivery(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	first := h.wantPrompts(1)[0]

	h.restart()
	h.connect()

	second := h.wantPrompts(2)[1]
	if second.DeliveryID != first.DeliveryID {
		t.Errorf("re-sent under %s, want the same delivery id %s", second.DeliveryID, first.DeliveryID)
	}
	h.wantState(StateReady)
}

func TestAstraAFailedClaimStaysFailedAcrossARestart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	last := h.locator()
	for range 3 {
		last = h.locator()
		h.observe(runtime.Gone)
	}
	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: 3})

	h.restart()
	calls := len(h.rt.Calls())
	for _, ev := range []Event{
		StreamHello{Claim: testToken, Generation: h.generation()},
		StreamClosed{Claim: testToken},
		StreamTurnStart{Claim: testToken},
		RuntimeObservation{Observation: runtime.Observation{Locator: last, Kind: runtime.Gone, At: h.clock.Now()}},
	} {
		h.must(ev)
	}
	for _, ev := range []Event{
		RequestSpawn{Claim: testToken},
		RequestResume{Claim: testToken},
		RequestDeliver{Claim: testToken, Task: "try again"},
		RequestRegister{Claim: testToken, Generation: h.generation(), Session: session, SessionFile: sessionFile},
	} {
		var refused *RefusedError
		if err := h.handle(ev); !errors.As(err, &refused) {
			t.Errorf("%T on a failed claim returned %v, want a refusal", ev, err)
		}
	}
	h.advance(10 * deadline)

	h.wantState(StateFailed)
	if got := len(h.rt.Calls()); got != calls {
		t.Errorf("the runtime was asked %v after the restart", h.rt.Methods()[calls:])
	}
}

func TestTerminalCallbackRunsOnlyAfterReadyAndFailurePersist(t *testing.T) {
	h := newHarness(t)
	var observed []ClaimState
	h.m.OnTerminal(func(c Claim, state ClaimState) {
		if persisted := h.store.load(c.Token); persisted.State != state {
			t.Fatalf("callback state %s ran before persisted claim state %s", state, persisted.State)
		}
		observed = append(observed, state)
	})

	h.reach(StateReady)
	for range 3 {
		h.observe(runtime.Gone)
	}

	if !slices.Equal(observed, []ClaimState{StateReady, StateFailed}) {
		t.Fatalf("terminal callbacks = %v, want ready then failed", observed)
	}
}
