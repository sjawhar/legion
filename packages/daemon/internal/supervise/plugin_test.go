package supervise

// A claim whose recorded plugin contract differs from the daemon's — re-adopted at boot with an
// earlier daemon's contract (or none at all), or registering again still stale after its first
// relaunch — is relaunched onto the current plugin the way one holding a moved address is
// (repoint): these tests pin that it is relaunched at once through the launch path a death uses,
// charged nothing the first time, charged as any launch failure the next time the relaunch was
// already made for this daemon's contract and still did not land on it, ignored while a claim is
// still launching (its own registration decides it), and that a current registration clears the
// mark the relaunch left.

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A claim found re-adopted, registered or later, on a plugin contract the daemon does not speak is
// relaunched at once, charged nothing: the process did nothing wrong, it simply predates this
// daemon's plugin. The relaunch resumes the recorded session, since every one of these states has
// already registered one, at the next generation.
func TestAStalePluginRelaunchesTheClaimChargingNothing(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			stale := h.locator()
			h.m.deps.PluginContract = 25

			if err := h.handle(StalePlugin{Claim: h.token}); err != nil {
				t.Fatalf("StalePlugin while %s = %v, want nil", state, err)
			}

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{})
			if claim := h.claim(); claim.PluginRelaunchFor != 25 {
				t.Fatalf("PluginRelaunchFor after the relaunch = %d, want 25", claim.PluginRelaunchFor)
			}
			resume := h.wantCalls("Resume", 1)[0]
			if resume.Previous == nil || *resume.Previous != stale || resume.Spec.ResumeSessionFile != sessionFile || resume.Spec.Generation != 2 {
				t.Fatalf("resumed %+v from %+v, want the recorded session after the stale incarnation, at generation 2", resume.Spec, resume.Previous)
			}
		})
	}
}

// A claim found stale while the daemon's own comparison has already been settled by a registration
// that landed between the boot's post and this decision is left alone: the recheck inside
// refreshPlugin is exactly what makes that safe.
func TestAStalePluginAlreadyCurrentDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	// m.deps.PluginContract stays at the harness default, which register() above already recorded
	// for the claim: it is current from the start.

	if err := h.handle(StalePlugin{Claim: h.token}); err != nil {
		t.Fatalf("StalePlugin while current = %v, want nil", err)
	}

	h.wantState(StateReady)
	h.wantBudgets(Budgets{})
	h.wantCalls("Resume", 0)
	h.wantCalls("Spawn", 1)
}

// A process found stale mid-turn is replaced mid-turn: that turn used whatever plugin it was
// running, and the relaunch ends it. The task goes back to waiting first (interrupted), charged
// nothing, and the agent resumes from its last saved turn once the relaunch registers.
func TestAStalePluginMidTurnInterruptsTheTaskChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	sent := h.wantPrompts(1)[0].DeliveryID
	h.m.deps.PluginContract = 25

	if err := h.handle(StalePlugin{Claim: h.token}); err != nil {
		t.Fatalf("StalePlugin mid-turn = %v, want nil", err)
	}

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if p := h.pending(); !p.Interrupted || p.ID == sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the stale-plugin relaunch = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
}

// A held suspension — the workflow asked to suspend a working claim, and the machine is waiting for
// its turn to end before it acts — is not waited out for a process found on a stale plugin: it ends
// the hold as a death or a stale address does (endHeld), suspended at once and charged nothing. Its
// next resume launches on the current plugin.
func TestAStalePluginWithAHeldSuspensionSuspendsTheClaimChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	h.m.deps.PluginContract = 25

	if err := h.handle(StalePlugin{Claim: h.token}); err != nil {
		t.Fatalf("StalePlugin while held = %v, want nil", err)
	}

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{})
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the stale process %+v", suspend.Locator, loc)
	}
	h.wantCalls("Resume", 0)
	if claim := h.claim(); claim.SuspensionHeld {
		t.Fatalf("claim %+v after the stale-plugin event, want the held suspension ended", claim)
	}
}

// refusableCheckLaunchStore wraps memStore's CheckLaunch so a test can make the launch path's
// lifecycle recheck refuse on demand, the same way a reserved tree cleanup refuses a live launch.
type refusableCheckLaunchStore struct {
	*memStore
	refuse bool
}

func (s *refusableCheckLaunchStore) CheckLaunch(ctx context.Context, c Claim) error {
	if s.refuse {
		return treelifecycle.ErrCleanupReserved
	}
	return s.memStore.CheckLaunch(ctx, c)
}

// A relaunch the launch path refuses (a reserved tree cleanup, the same refusal
// relaunchAfterFailure already pays for before its charge) leaves a stale claim exactly as it
// found it: refreshPlugin's own checkLaunch runs before the Warn, the PluginRelaunchFor mark, and
// the turn's interruption, so the refused boot attempt changes nothing — the claim stays working
// its turn on the stale contract, its confirmed pending delivery untouched. Once the refusal
// clears, the sweep's re-post (refreshStalePlugins in internal/daemon) relaunches it for real.
func TestAStalePluginRefusedByTheLaunchPathChangesNothing(t *testing.T) {
	h := newBareHarness(t)
	c := fixture(StateWorking)
	c.PluginContract = 1
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, c.Token, *c.Pending); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	store := &refusableCheckLaunchStore{memStore: h.store, refuse: true}
	h.m.deps.Store = store
	h.m.deps.PluginContract = 24

	if err := h.handle(StalePlugin{Claim: h.token, Trigger: "boot"}); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("StalePlugin refused by the launch path = %v, want the reservation's wait", err)
	}
	h.wantState(StateWorking)
	if claim := h.claim(); claim.PluginRelaunchFor != 0 {
		t.Fatalf("PluginRelaunchFor after the refusal = %d, want 0", claim.PluginRelaunchFor)
	}
	if p := h.pending(); p.Interrupted || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the refusal = %+v, want the confirmed delivery untouched", p)
	}
	h.wantCalls("Resume", 0)
	if lines := h.logs.lines("relaunching it onto the current plugin"); len(lines) != 0 {
		t.Fatalf("logged %v, want no relaunching line while the refusal lasts", lines)
	}

	store.refuse = false
	if err := h.handle(StalePlugin{Claim: h.token, Trigger: "sweep"}); err != nil {
		t.Fatalf("StalePlugin once the refusal clears = %v, want nil", err)
	}
	h.wantState(StateLaunching)
	if claim := h.claim(); claim.PluginRelaunchFor != 24 || claim.Generation != 4 {
		t.Fatalf("claim after the sweep relaunch = %+v, want PluginRelaunchFor 24 at generation 4", claim)
	}
	h.wantCalls("Resume", 1)
	if p := h.pending(); !p.Interrupted || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the sweep relaunch = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
	if lines := h.logs.lines("relaunching it onto the current plugin"); len(lines) != 1 {
		t.Fatalf("logged %v, want exactly one relaunching line once the refusal clears", lines)
	}
}

// A claim still launching, or otherwise with no process registered to compare, is left to its own
// registration: the boot never posts it this event (the daemon package's own test covers that), and
// the table row ignores it outright for every other state besides the four live states above.
func TestAStalePluginIsIgnoredWhileLaunchingOrGone(t *testing.T) {
	for _, state := range []ClaimState{
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected,
		StateSuspended, StateFailed, StateRetired,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, fixture(state))
			h.m.deps.PluginContract = 24
			before := h.claim()

			if err := h.handle(StalePlugin{Claim: h.token}); err != nil {
				t.Fatalf("StalePlugin while %s = %v, want nil (ignored)", state, err)
			}

			after := h.claim()
			if after.State != before.State || after.Budgets != before.Budgets {
				t.Fatalf("claim moved: %+v -> %+v", before, after)
			}
			if calls := h.rt.Methods(); len(calls) > 0 {
				t.Fatalf("asked the runtime %v", calls)
			}
		})
	}
}

// A registration whose plugin still differs from the daemon's is relaunched at once, uncharged the
// first time (the claim's last relaunch was made for no contract, or an earlier one), and charged
// as any launch failure the next time it is still the daemon's own contract the claim's last
// relaunch was made for — a plugin or an image the relaunch put the process on is itself behind
// this daemon — until the launch budget ends it in failed. Every one is answered with the refusal
// naming both contracts.
func TestAStaleRegistrationRelaunchesOnceUnchargedThenChargesUntilFailed(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.m.deps.PluginContract = 24
	h.m.deps.PluginRelaunchable = true
	h.m.deps.Limits.LaunchFailures = 2

	register := func() error {
		return h.handle(RequestRegister{
			Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: 1,
		})
	}
	wantStale := func(err error) {
		t.Helper()
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(1, 24) {
			t.Fatalf("register = %v, want the stale-plugin refusal naming 1 and 24", err)
		}
	}

	wantStale(register())
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if claim := h.claim(); claim.PluginRelaunchFor != 24 {
		t.Fatalf("PluginRelaunchFor after the first stale registration = %d, want 24", claim.PluginRelaunchFor)
	}

	wantStale(register())
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})

	wantStale(register())
	h.wantState(StateFailed)
}

// A registration whose plugin declares no contract at all is the same refusal, worded for it.
func TestAStaleRegistrationWithNoDeclaredContractNamesIt(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.m.deps.PluginContract = 24
	h.m.deps.PluginRelaunchable = true

	err := h.handle(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: 0,
	})
	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(0, 24) {
		t.Fatalf("register with no declared contract = %v, want the refusal naming 0 and 24", err)
	}
}

// A registration that records the daemon's own contract clears whatever PluginRelaunchFor an
// earlier stale relaunch left: the claim remembers only the contract its last relaunch was made
// for, and a current registration means that relaunch landed.
func TestACurrentRegistrationClearsPluginRelaunchFor(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.m.deps.PluginContract = 24
	h.m.deps.PluginRelaunchable = true

	if err := h.handle(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: 1,
	}); err == nil {
		t.Fatal("the stale registration was not refused")
	}
	if claim := h.claim(); claim.PluginRelaunchFor != 24 {
		t.Fatalf("PluginRelaunchFor = %d, want 24", claim.PluginRelaunchFor)
	}

	h.must(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: 24,
	})
	if claim := h.claim(); claim.PluginRelaunchFor != 0 {
		t.Fatalf("PluginRelaunchFor after a current registration = %d, want 0", claim.PluginRelaunchFor)
	}
}

// A daemon that knows no plugin source to relaunch a stale process onto (PluginRelaunchable false:
// a Kubernetes daemon with no runtime.kubernetes.plugins and no linked pair) records the
// registration and accepts it, relaunching and refusing nothing — the degraded mode LEGION-164's
// boot would otherwise burn every live claim's launch budget on at the first boot on the release.
func TestANoSourceDaemonRecordsAndAcceptsAStaleRegistrationWithoutRelaunching(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.m.deps.PluginContract = 24
	// PluginRelaunchable is left at its zero value, false: the daemon knows no plugin source.

	if err := h.handle(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: 1,
	}); err != nil {
		t.Fatalf("register with no plugin source = %v, want it accepted", err)
	}

	h.wantState(StateRegistered)
	h.wantBudgets(Budgets{})
	if claim := h.claim(); claim.PluginContract != 1 || claim.PluginRelaunchFor != 0 {
		t.Fatalf("claim = %+v, want pluginContract 1 recorded and no relaunch made for it", claim)
	}
	h.wantCalls("Resume", 0)
	h.wantCalls("Spawn", 1)
}
