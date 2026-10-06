package supervise

// A process the runtime reports StaleAddress — alive, but holding an address a process launched now
// is not handed (LEGION-592: an address the daemon hands its processes moved, as the worker stream
// does when the daemon restarts on another host, bind or worker_stream_port) — is not a death: these
// tests pin that the claim is relaunched at once through the launch path a death uses (a Resume of
// its recorded session, or a Spawn when it has none), that the stale observation itself is never
// charged though a relaunch the runtime refuses is, and that a held suspension ends in a suspension.

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A ready claim found at a stale address relaunches the same session at once: a Resume, not a
// Spawn, and no budget moves — unlike a death, which always charges a launch failure.
func TestAStaleAddressRelaunchesTheSameSessionChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("relaunched with %+v, want the recorded session resumed", resumed.Spec)
	}
	h.relaunched()
	h.wantState(StateReady)
}

// A claim mid-launch — relaunching already, its shim not yet connected to this daemon at all —
// found at a stale address is relaunched again, the same session once more: the runtime has no
// notion of registration, so a stale pod is reported whatever state the claim's own machine is
// in, and the machine must not wait for a hello that pod can never send.
func TestAStaleAddressWhileLaunchingRelaunchesAgainChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestSuspend{Claim: testToken})
	h.wantState(StateSuspended)
	h.must(RequestResume{Claim: testToken})
	h.wantState(StateLaunching)

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if resumed := h.wantCalls("Resume", 2)[1]; resumed.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("relaunched with %+v, want the recorded session resumed again", resumed.Spec)
	}
	h.relaunched()
	h.wantState(StateReady)
}

// A claim found at a stale address before its agent registered has no session to resume, so it is
// relaunched the way a death before registration is: a Spawn again (over its existing Sandbox,
// under the Sandbox runtime), charged nothing.
func TestAStaleAddressBeforeRegistrationSpawnsAgainChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateShimConnected)

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if spawned := h.wantCalls("Spawn", 2)[1]; spawned.Spec.ResumeSessionFile != "" {
		t.Fatalf("relaunched with %+v, want a fresh spawn: the claim recorded no session", spawned.Spec)
	}
	h.wantCalls("Resume", 0)
}

// The stale observation is free, but the relaunch it starts is a launch like any other: one the
// runtime refuses is a launch failure, tried again at once (launch). A death charges one more for
// the death itself (TestAResumeTheRuntimeRefusesIsALaunchFailure); this charges only the refusal.
func TestAStaleAddressWhoseRelaunchIsRefusedChargesThatLaunchFailure(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.ScriptResume(fake.SpawnResult{Err: errBoom})

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	h.wantCalls("Resume", 2)
}

// A process found at a stale address mid-turn is replaced mid-turn: that turn used the addresses
// the process holds, and the relaunch ends it. The task goes back to waiting first (interrupted),
// but unlike a death nothing is charged. A turn in flight is lost; the agent resumes from its last
// saved turn once the relaunch registers.
func TestAStaleAddressMidTurnInterruptsTheTaskChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	sent := h.wantPrompts(1)[0].DeliveryID

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if p := h.pending(); !p.Interrupted || p.ID == sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the stale observation = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
	stored := h.store.load(testToken)
	if stored.Pending == nil || stored.ServingGeneration != 0 {
		t.Fatalf("stored claim %+v, want the task kept and no run served", stored)
	}

	h.relaunched()
	if resent := h.wantPrompts(2)[1]; resent.DeliveryID == sent {
		t.Fatalf("re-sent %+v under the id the stale process's turn ran under", resent)
	}
}

// A held suspension — the workflow asked to suspend a working claim, and the machine is waiting
// for its turn to end before it acts — is not waited out for a process at a stale address, which
// is replaced at once whatever it is doing (repoint), and whose turn may never report its end at
// all when the address that moved is the worker stream's. The stale observation ends the hold as a
// death does (endHeld): the claim is suspended at once and charged nothing, and no pod is launched
// only to be suspended by the suspension's retry.
func TestAStaleAddressWithAHeldSuspensionSuspendsTheClaimChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	if claim := h.claim(); !claim.SuspensionHeld {
		t.Fatalf("claim %+v after a mid-turn suspend, want a held suspension", claim)
	}

	h.observe(runtime.StaleAddress)

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{})
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the stale process %+v", suspend.Locator, loc)
	}
	h.wantCalls("Spawn", 1)
	h.wantCalls("Resume", 0)
	if claim := h.claim(); claim.SuspensionHeld {
		t.Fatalf("claim %+v after the stale observation, want the held suspension ended", claim)
	}
}
