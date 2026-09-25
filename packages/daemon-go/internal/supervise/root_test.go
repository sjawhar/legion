package supervise

// The tree's root claim — the architect of the issue its tree is named for — ends only when its
// tree closes: its exit and its registration deadline suspend it, and only the tree's close
// releases it.

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

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

// A root's exit ends its phase as a suspension does, so a task still pending unconfirmed — sent and
// acknowledged, no turn started — is retired with it: the next resume starts with its new phase's
// task, never the finished one's. That holds whether or not the runtime could suspend the process.
func TestARootsExitRetiresItsPhasesUnconfirmedTask(t *testing.T) {
	for _, failSuspend := range []bool{false, true} {
		name := "suspend ok"
		if failSuspend {
			name = "suspend fails"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: rootToken, Task: "the task"})
			h.wantPrompts(1)
			if stored, ok := h.store.delivery(rootToken); !ok || stored.DeliveredAt.IsZero() || !stored.ConfirmedAt.IsZero() {
				t.Fatalf("stored delivery %+v (ok=%v), want it acknowledged and unconfirmed", stored, ok)
			}
			if failSuspend {
				h.rt.FailSuspend(errBoom)
			}

			h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

			h.wantState(StateSuspended)
			if p := h.claim().Pending; p != nil {
				t.Errorf("pending %+v, want the unconfirmed task retired with the root's exit", p)
			}
			if d, ok := h.store.delivery(rootToken); ok {
				t.Errorf("stored delivery %+v, want it retired", d)
			}
		})
	}
}

// The compound failure: a root's exit whose Suspend fails leaves a process that may still run, the
// resumes that follow are refused while it does, and the claim fails. The retry the workflow sends
// next resumes the same session after that exited process — never with nothing to wait out, which
// on tmux would start a second agent on the session beside the first.
func TestARetryAfterAFailedRootExitAndRefusedResumesWaitsOutTheExitedProcess(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	exited := h.locator()
	h.rt.FailSuspend(errBoom)
	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})
	h.wantState(StateSuspended)
	still := errors.New("previous incarnation still running")
	h.rt.ScriptResume(fake.SpawnResult{Err: still}, fake.SpawnResult{Err: still}, fake.SpawnResult{Err: still})
	if err := h.handle(RequestResume{Claim: rootToken}); !errors.Is(err, still) {
		t.Fatalf("resume returned %v, want the runtime's refusal", err)
	}
	h.wantState(StateFailed)

	h.must(RequestRetry{Claim: rootToken})

	resumes := h.wantCalls("Resume", 4)
	for i, resume := range resumes {
		if resume.Previous == nil || *resume.Previous != exited || resume.Spec.ResumeSessionFile != sessionFile {
			t.Errorf("resume %d waited out %+v for %q, want the exited process %+v and the recorded session",
				i+1, resume.Previous, resume.Spec.ResumeSessionFile, exited)
		}
	}
	h.wantCalls("Spawn", 1)
}

// The tree's close is the one thing that releases its root: a root the daemon already suspended
// has no process, so the release carries no locator.
func TestTreeCloseReleasesASuspendedRootWithNoLocator(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})
	h.wantState(StateSuspended)

	h.must(RequestStop{Claim: rootToken, TreeClose: true})

	wantReleasedWithNoLocator(t, h, rootToken)
	h.wantState(StateRetired)
}

// Its close releases the root from any state: whatever runs is stopped, and the claim retires.
func TestTreeCloseReleasesALiveRoot(t *testing.T) {
	for _, state := range liveStates {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			loc := h.locator()

			h.must(RequestStop{Claim: rootToken, TreeClose: true})

			if released := h.wantCalls("Release", 1)[0].Released; released.Claim != rootToken || released.Locator == nil || *released.Locator != loc {
				t.Errorf("released %+v, want %s at %+v", released, rootToken, loc)
			}
			h.wantState(StateRetired)
		})
	}
}

// Nothing else retires the tree's root (N-v2-4): a retired root leaves the orphan sweep's known
// set, and under a sandbox the sweep would then delete its Sandbox and the tree volume with it,
// every child's workspace included. A stop that is not the tree's close — the operator's — is
// refused in every state and changes nothing. The refusal is ErrRootStop, whatever the state, and
// says what stops the root's process where one runs: suspend, once its agent has registered.
func TestAStopThatIsNotTheTreesCloseNeverRetiresItsRoot(t *testing.T) {
	const (
		suspendIt = "stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process"
		booting   = "stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process once its agent has registered"
		nothing   = "stop refused: the tree's root claim ends only when its tree closes; no process of this claim is running"
	)
	wantRefused := func(t *testing.T, h *harness, state ClaimState, want string) {
		t.Helper()
		before := h.claim()
		err := h.handle(RequestStop{Claim: rootToken})
		if !errors.Is(err, ErrRootStop) || err.Error() != want {
			t.Fatalf("stop of a root in %s returned %v, want ErrRootStop saying %q", state, err, want)
		}
		h.wantCalls("Release", 0)
		h.wantState(state)
		if after := h.claim(); !reflect.DeepEqual(after.Locator, before.Locator) || after.Generation != before.Generation {
			t.Errorf("claim %+v after the refused stop, want it unchanged from %+v", after, before)
		}
		if stored := h.store.load(rootToken); stored.State != state {
			t.Errorf("stored state %s, want %s", stored.State, state)
		}
	}
	for state, want := range map[ClaimState]string{
		StateQueued: nothing, StateSuspended: nothing,
		StateLaunching: booting, StateShimConnected: booting,
		StateRegistered: suspendIt, StateReady: suspendIt, StateWorking: suspendIt, StateIdle: suspendIt,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			wantRefused(t, h, state, want)
		})
	}
	t.Run(string(StateLaunchUncertain), func(t *testing.T) {
		uncertain := rootClaim()
		uncertain.State = StateLaunchUncertain
		h := newHarnessOf(t, uncertain)
		wantRefused(t, h, StateLaunchUncertain, nothing)
	})
	t.Run(string(StateFailed), func(t *testing.T) {
		h := newHarnessOf(t, rootClaim())
		h.reach(StateReady)
		for range 3 {
			h.observe(runtime.Gone)
		}
		h.wantState(StateFailed)
		wantRefused(t, h, StateFailed, nothing)
	})
}

// Suspend is what stops the root's process short of its tree's close, so it is accepted as soon as
// the agent has registered: a registered agent holds a capability that mints grants and no timer
// runs on it, so a claim that could be neither stopped nor suspended there would keep that
// capability until its process died. The suspension revokes it and keeps the session to resume.
func TestARegisteredRootCanBeSuspendedAndResumed(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateRegistered)
	loc := h.locator()

	h.must(RequestSuspend{Claim: rootToken})

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Errorf("suspended %+v, want the registered process %+v", suspend.Locator, loc)
	}
	h.wantState(StateSuspended)
	if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.CapabilityHash != nil || stored.Session != session {
		t.Errorf("stored %+v, want it suspended with its capability revoked and its session kept", stored)
	}

	h.must(RequestResume{Claim: rootToken})
	if resume := h.wantCalls("Resume", 1)[0]; resume.Spec.ResumeSessionFile != sessionFile || resume.Previous == nil || *resume.Previous != loc {
		t.Errorf("resumed %+v, want the recorded session after the suspended process", resume)
	}
}
