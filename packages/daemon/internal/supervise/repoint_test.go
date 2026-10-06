package supervise

// A process the runtime reports StaleAddress — alive, but at an address that no longer reaches this
// daemon (LEGION-592: the daemon's own worker-stream address moved, as it does when its pod is
// replaced in the cluster) — is not a death: these tests pin that the claim relaunches its
// recorded session at once, exactly as a suspend-then-resume does, charges nothing, and can never
// fail for it, whatever state it was found in.

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
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

// A process found at a stale address mid-turn cannot finish that turn — it cannot report back to
// this daemon at all — so the task goes back to waiting first (interrupted), but unlike a death
// nothing is charged. A turn in flight is lost; the agent resumes from its last saved turn once
// the relaunch registers.
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

// A held suspension — the operator asked to suspend a working claim, and the machine is waiting
// for its turn to end before it acts — cannot ever resolve for a process at a stale address: that
// process will never report its turn ending. The stale observation drops the held suspension and
// relaunches, rather than waiting on an end that can never come.
func TestAStaleAddressDropsAHeldSuspensionAndRelaunches(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	if err := h.handle(RequestSuspend{Claim: testToken}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	if claim := h.claim(); !claim.SuspensionHeld {
		t.Fatalf("claim %+v after a mid-turn suspend, want a held suspension", claim)
	}

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if claim := h.claim(); claim.SuspensionHeld {
		t.Fatalf("claim %+v after the stale observation, want the held suspension dropped", claim)
	}
	h.relaunched()
	h.wantState(StateReady)
}
