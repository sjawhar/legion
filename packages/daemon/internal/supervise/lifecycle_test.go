package supervise

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// lifecycleRuntime is the fake runtime with the durable tree-lifecycle capability, so the machine
// binds and checks epochs as it does over the Sandbox runtime. The cleanup methods are not reached.
type lifecycleRuntime struct{ *fake.Runtime }

func (lifecycleRuntime) ReserveWorkflowTreeCleanup(context.Context, string, string, uint64) (uint64, bool, error) {
	return 0, false, errors.New("not reached")
}

func (lifecycleRuntime) ReserveOperatorTreeCleanup(context.Context, string, string) (uint64, bool, error) {
	return 0, false, errors.New("not reached")
}

func (lifecycleRuntime) CleanupTree(context.Context, string, string, uint64) error {
	return errors.New("not reached")
}

var errReserved = errors.New("tree cleanup is reserved")

// epochStore is the in-memory store over one tree lifecycle: AdmitClaim binds the open epoch or
// refuses a reserved cleanup, and CheckLaunch refuses a reserved or stale epoch, as the Postgres
// store does through the shared serializer.
type epochStore struct {
	*memStore
	epoch    uint64
	reserved bool
}

func (s *epochStore) AdmitClaim(ctx context.Context, c Claim) (Claim, error) {
	if s.reserved {
		return Claim{}, errReserved
	}
	c.TreeEpoch = s.epoch
	return c, s.memStore.PutClaim(ctx, c)
}

func (s *epochStore) CheckLaunch(_ context.Context, c Claim) error {
	if s.reserved {
		return errReserved
	}
	if c.TreeEpoch != s.epoch {
		return fmt.Errorf("bound epoch %d is stale; current is %d", c.TreeEpoch, s.epoch)
	}
	return nil
}

func lifecycleHarness(t *testing.T, store *epochStore, c Claim) *harness {
	t.Helper()
	h := newBareHarness(t)
	store.memStore = h.store
	h.deps.Runtime, h.deps.Store = lifecycleRuntime{h.rt}, store
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

func retiredOfEpoch(epoch uint64) Claim {
	c := queuedClaim()
	c.State, c.TreeEpoch, c.Session, c.SessionFile, c.Generation = StateRetired, epoch, "ses_kept", "/sessions/kept.jsonl", 3
	c.Budgets.LaunchFailures = 2
	return c
}

// A retired claim brought back after its tree's cleanup confirmed and a fresh root admission
// opened the next epoch binds that epoch before it launches; a machine that kept the retired
// claim's epoch would be refused as stale and the re-admitted tree's role would never start.
func TestARevivedClaimBindsTheTreesOpenEpochBeforeItLaunches(t *testing.T) {
	store := &epochStore{epoch: 2}
	h := lifecycleHarness(t, store, retiredOfEpoch(1))
	h.must(RequestRetry{Claim: h.token})
	if got := h.m.Claim(); got.TreeEpoch != 2 || got.State != StateLaunching {
		t.Fatalf("revived claim = epoch %d in %s, want epoch 2 launching", got.TreeEpoch, got.State)
	}
	if calls := len(h.rt.CallsOf("Resume")) + len(h.rt.CallsOf("Spawn")); calls != 1 {
		t.Fatalf("runtime starts = %d, want 1", calls)
	}
}

// While the tree's cleanup is reserved, a revival is the named wait: nothing launches, the claim
// keeps its state, and its stored launch-failure budget is not charged, so the start retries.
func TestARevivalDuringAReservedCleanupLaunchesNothingAndChargesNoBudget(t *testing.T) {
	store := &epochStore{epoch: 1, reserved: true}
	h := lifecycleHarness(t, store, retiredOfEpoch(1))
	if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, errReserved) {
		t.Fatalf("retry during a reserved cleanup = %v, want the reservation's wait", err)
	}
	if calls := len(h.rt.CallsOf("Resume")) + len(h.rt.CallsOf("Spawn")); calls != 0 {
		t.Fatalf("runtime starts during a reserved cleanup = %d, want 0", calls)
	}
	stored := h.store.load(h.token)
	if stored.State != StateRetired || stored.Budgets.LaunchFailures != 2 || stored.TreeEpoch != 1 {
		t.Fatalf("stored claim after the refused retry = %s, launch failures %d, epoch %d; want retired, 2, epoch 1",
			stored.State, stored.Budgets.LaunchFailures, stored.TreeEpoch)
	}
}

// A process that dies while its tree's cleanup is reserved is not relaunched, and the refused
// relaunch charges no launch failure: the close retires the claim, and a budget spent here would
// follow the claim into its tree's next run.
func TestADeathDuringAReservedCleanupRelaunchesNothingAndChargesNoBudget(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateReady)
	store.reserved = true
	gone := RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}
	if err := h.handle(gone); !errors.Is(err, errReserved) {
		t.Fatalf("death during a reserved cleanup = %v, want the reservation's wait", err)
	}
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
}

// The reservation can commit after the machine's own check passed and before the runtime's
// resource recheck. That refusal from the runtime is the same uncharged wait: one start attempt,
// no launch failure, no retry loop spending the budget until the claim fails, and a claim brought
// back from rest rests again.
func TestARuntimeRecheckMeetingAReservationIsAnUnchargedWait(t *testing.T) {
	store := &epochStore{epoch: 2}
	h := lifecycleHarness(t, store, retiredOfEpoch(1))
	refusal := fmt.Errorf("launch: record its durable issue resources: %w", treelifecycle.ErrCleanupReserved)
	h.rt.ScriptResume(fake.SpawnResult{Err: refusal})
	h.rt.ScriptSpawn(fake.SpawnResult{Err: refusal})
	if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("retry refused by the runtime's recheck = %v, want the reservation's wait", err)
	}
	if calls := len(h.rt.CallsOf("Resume")) + len(h.rt.CallsOf("Spawn")); calls != 1 {
		t.Fatalf("runtime starts = %d, want exactly the one refused attempt", calls)
	}
	stored := h.store.load(h.token)
	if stored.Budgets.LaunchFailures != 0 || stored.State != StateRetired {
		t.Fatalf("stored claim = %s with %d launch failures, want retired with none", stored.State, stored.Budgets.LaunchFailures)
	}
}
