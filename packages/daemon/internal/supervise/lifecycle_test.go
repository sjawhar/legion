package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// epochStore is the in-memory store over one tree lifecycle: AdmitClaim binds the open epoch or
// refuses a reserved cleanup, and CheckLaunch refuses a reserved or stale epoch, as the Postgres
// store does through the shared serializer. It fails the next unanswered launch checks first, as a
// store that does not answer fails them.
type epochStore struct {
	*memStore
	epoch      uint64
	reserved   bool
	unanswered int
}

// errUnanswered is a launch check the store did not answer: no lifecycle refused it.
var errUnanswered = errors.New("the store did not answer the launch check")

func (s *epochStore) AdmitClaim(ctx context.Context, c Claim) (Claim, error) {
	if s.reserved {
		return Claim{}, treelifecycle.ErrCleanupReserved
	}
	c.TreeEpoch = s.epoch
	return c, s.memStore.PutClaim(ctx, c)
}

func (s *epochStore) CheckLaunch(_ context.Context, c Claim) error {
	if s.unanswered > 0 {
		s.unanswered--
		return errUnanswered
	}
	if s.reserved {
		return treelifecycle.ErrCleanupReserved
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
	h.deps.Store = store
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
	if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
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
	if err := h.handle(gone); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("death during a reserved cleanup = %v, want the reservation's wait", err)
	}
	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
}

// reservingSuspend reserves its tree's cleanup while a stop waits out its grace: after a
// retirement's own lifecycle check, and before its relaunch's.
type reservingSuspend struct {
	*fake.Runtime
	store *epochStore
}

func (r *reservingSuspend) Suspend(ctx context.Context, loc runtime.Locator) error {
	r.store.reserved = true
	return r.Runtime.Suspend(ctx, loc)
}

// unansweredSuspend has its store fail the next checks launch checks once a stop lands: after a
// retirement's own lifecycle check, and before its relaunch's.
type unansweredSuspend struct {
	*fake.Runtime
	store  *epochStore
	checks int
}

func (r *unansweredSuspend) Suspend(ctx context.Context, loc runtime.Locator) error {
	r.store.unanswered = r.checks
	return r.Runtime.Suspend(ctx, loc)
}

// refuseLate is the agent answering its acknowledged prompt a failure with no turn of its own
// running, which charges the prompt (lateRefused).
func refuseLate(h *harness) error {
	h.t.Helper()
	return h.handle(StreamLateRefusal{Claim: h.token, DeliveryID: h.pending().ID, Error: "No API key found for anthropic."})
}

// A retirement whose relaunch the tree lifecycle refuses — the registration deadline's, or a prompt
// retirement's, whichever of the prompt's outcomes reached the limit: a turn that never started, a
// refusal of the prompt, or a late refusal of it — never leaves a live claim with no process for the
// rows that act on one. A lifecycle that already refuses retires nothing: the claim keeps its process
// and is charged nothing more. One that begins to refuse while the stop waits out its grace refuses
// a relaunch whose process is gone: the claim is left suspended by the daemon, running nothing.
// Either way the operator's suspend, the root's exit, and a stale plugin found once launches are
// allowed again are handled as on any claim in that state.
func TestARetirementWhoseRelaunchTheTreeLifecycleRefusesLeavesNoLiveClaimWithoutAProcess(t *testing.T) {
	retirements := []struct {
		name string
		// reach brings the claim to the moment its retirement is decided, and retire decides it.
		reach, retire func(h *harness)
		// state and kept are the claim a refusal before the retirement leaves, its process kept;
		// retired is the budgets of one whose relaunch was refused after the stop.
		state         ClaimState
		kept, retired Budgets
	}{
		{
			name:   "the registration deadline",
			reach:  func(h *harness) { h.reach(StateRegistered) },
			retire: func(h *harness) { h.advance(deadline) },
			state:  StateRegistered, kept: Budgets{}, retired: Budgets{},
		},
		{
			name: "a prompt retirement",
			reach: func(h *harness) {
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "the task"})
				for range testLimits().PromptFailures - 1 {
					h.advance(testRPC)
					h.observe(runtime.Alive)
				}
			},
			retire: func(h *harness) { h.advance(testRPC) },
			state:  StateReady, kept: Budgets{PromptFailures: 2}, retired: Budgets{PromptRetires: 1},
		},
		{
			name: "a refused prompt's retirement",
			reach: func(h *harness) {
				h.reach(StateReady)
				h.conn.RefusePrompt("the model provider refused the request")
				h.must(RequestDeliver{Claim: h.token, Task: "the task"})
				h.observe(runtime.Alive)
			},
			retire: func(h *harness) { h.observe(runtime.Alive) },
			state:  StateReady, kept: Budgets{PromptFailures: 2}, retired: Budgets{PromptRetires: 1},
		},
		{
			name: "a late refusal's retirement",
			reach: func(h *harness) {
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "the task"})
				for range testLimits().PromptFailures - 1 {
					if err := refuseLate(h); err != nil {
						h.t.Fatalf("refuse the acknowledged prompt: %v", err)
					}
					h.observe(runtime.Alive)
				}
			},
			retire: func(h *harness) {
				if err := refuseLate(h); err != nil && strings.Contains(err.Error(), "outside") {
					h.t.Errorf("the late refusal's retirement: %v", err)
				}
			},
			state: StateReady, kept: Budgets{PromptFailures: 2}, retired: Budgets{PromptRetires: 1},
		},
	}
	refusals := []struct {
		name   string
		refuse func(h *harness, store *epochStore)
		// stopped is whether the retirement stopped the process before its relaunch was refused.
		stopped bool
	}{
		{name: "its tree's cleanup already reserved", refuse: func(_ *harness, store *epochStore) { store.reserved = true }},
		{
			name:    "its tree's cleanup reserved while the stop waits out its grace",
			refuse:  func(h *harness, store *epochStore) { h.m.deps.Runtime = &reservingSuspend{Runtime: h.rt, store: store} },
			stopped: true,
		},
	}
	events := []struct {
		name  string
		event func(h *harness, store *epochStore) Event
	}{
		{"the operator's suspend", func(h *harness, _ *epochStore) Event {
			return RequestSuspend{Claim: h.token, Reason: "drain", By: SuspendedByOperator}
		}},
		{"the root's exit", func(h *harness, _ *epochStore) Event {
			return RequestExit{Claim: h.token, Generation: h.generation(), Session: session, Reason: "done"}
		}},
		{"a stale plugin found once launches are allowed again", func(h *harness, store *epochStore) Event {
			store.reserved = false
			h.m.deps.PluginContract = 25
			return StalePlugin{Claim: h.token, Trigger: "sweep"}
		}},
	}
	for _, retirement := range retirements {
		for _, refusal := range refusals {
			for _, event := range events {
				t.Run(retirement.name+", "+refusal.name+": "+event.name, func(t *testing.T) {
					store := &epochStore{epoch: 1}
					h := lifecycleHarness(t, store, rootClaim())
					retirement.reach(h)
					launched := h.locator()
					refusal.refuse(h, store)
					retirement.retire(h)
					if lines := h.logs.lines("moved", "outside"); len(lines) != 0 {
						t.Errorf("the retirement moved the claim outside its row's states: %v", lines)
					}
					left := h.claim()
					suspends := len(h.calls("Suspend"))

					err := h.handleWithoutPanic(event.event(h, store))

					switch {
					case refusal.stopped:
						if left.State != StateSuspended || left.SuspendedBy != SuspendedByDaemon || left.Locator != nil || suspends != 1 || left.Budgets != retirement.retired {
							t.Errorf("the refusal after the stop left the claim %s by %q with locator %+v, budgets %+v and %d suspensions; want suspended by the daemon with none, %+v and 1",
								left.State, left.SuspendedBy, left.Locator, left.Budgets, suspends, retirement.retired)
						}
					case left.State != retirement.state || left.Locator == nil || *left.Locator != launched || suspends != 0 || left.Budgets != retirement.kept:
						t.Errorf("the refusal before the stop left the claim %s with locator %+v, budgets %+v and %d suspensions; want %s on %+v, %+v and none",
							left.State, left.Locator, left.Budgets, suspends, retirement.state, launched, retirement.kept)
					}
					if err != nil {
						t.Errorf("%s: %v", event.name, err)
					}
					if c := h.claim(); c.Locator == nil && slices.Contains(live, c.State) {
						t.Errorf("after %s the claim is %s with no process", event.name, c.State)
					}
				})
			}
		}
	}
}

// A registration deadline whose relaunch the tree lifecycle refuses is tried again at the probe
// interval, as one whose stop fails is: once launches are allowed again, the next try retires the
// process and relaunches the same session.
func TestARegistrationDeadlineTheTreeLifecycleRefusesIsTriedAgainAtTheProbeInterval(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateRegistered)
	store.reserved = true
	h.advance(deadline)
	h.wantCalls("Suspend", 0)
	h.wantState(StateRegistered)

	store.reserved = false
	h.advance(testProbe)

	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
}

// A retirement whose relaunch the store does not check once the stop has landed — its Postgres did
// not answer — keeps the claim's task and relaunches the same session once the store answers: no
// tree is closing, so nothing else would retire or resume the claim. The claim is left launching with
// no process, its phase's task kept, and the relaunch is tried again at each probe interval until the
// check passes; a registration deadline's relaunch is charged its launch failure then, a prompt
// retirement's is not. The relaunched agent is handed the task once it is ready.
func TestARetirementWhoseRelaunchTheStoreDoesNotCheckKeepsItsTaskAndRelaunchesAtTheProbeInterval(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reach, retire func(h *harness)
		// left is the budgets the unchecked relaunch leaves, relaunched the budgets once it runs.
		left, relaunched Budgets
	}{
		{
			name: "the registration deadline",
			reach: func(h *harness) {
				h.reach(StateRegistered)
				h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
			},
			retire: func(h *harness) { h.advance(deadline) },
			left:   Budgets{}, relaunched: Budgets{LaunchFailures: 1},
		},
		{
			name: "a prompt retirement",
			reach: func(h *harness) {
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
				for range testLimits().PromptFailures - 1 {
					h.advance(testRPC)
					h.observe(runtime.Alive)
				}
			},
			retire: func(h *harness) { h.advance(testRPC) },
			left:   Budgets{PromptRetires: 1}, relaunched: Budgets{PromptRetires: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &epochStore{epoch: 1}
			h := lifecycleHarness(t, store, queuedClaim())
			tc.reach(h)
			generation := h.generation()
			// The store answers neither the relaunch's own check nor its first retry.
			h.m.deps.Runtime = &unansweredSuspend{Runtime: h.rt, store: store, checks: 2}

			tc.retire(h)

			h.wantCalls("Suspend", 1)
			h.wantCalls("Resume", 0)
			left := h.claim()
			if left.State != StateLaunching || left.Locator != nil || left.Generation != generation || left.Budgets != tc.left {
				t.Fatalf("the unchecked relaunch left the claim %s at generation %d with locator %+v and budgets %+v; want launching at %d with none and %+v",
					left.State, left.Generation, left.Locator, left.Budgets, generation, tc.left)
			}
			if p := left.Pending; p == nil || p.Task != "the task" || p.Phase != phase.Implementing {
				t.Fatalf("the unchecked relaunch left the pending task %+v, want the phase's task kept", p)
			}
			if stored := h.store.load(h.token); stored.State != StateLaunching || stored.Locator != nil || stored.Pending == nil || stored.Pending.Phase != phase.Implementing {
				t.Fatalf("stored %s with locator %+v and pending %+v, want launching with no process and the phase's task", stored.State, stored.Locator, stored.Pending)
			}

			h.advance(testProbe)

			h.wantCalls("Resume", 0)
			if c := h.claim(); c.State != StateLaunching || c.Locator != nil || c.Budgets != tc.left {
				t.Fatalf("after a retry the store did not answer the claim is %s with locator %+v and budgets %+v; want launching with none and %+v",
					c.State, c.Locator, c.Budgets, tc.left)
			}
			// The retired process's connection closes meanwhile: there is nothing to probe, and the
			// relaunch still waits for its timer.
			h.must(StreamClosed{Claim: h.token})
			if lines := h.logs.lines("probed with no process to probe"); len(lines) != 1 {
				t.Errorf("logged %v, want the close's probe to find no process once", lines)
			}

			h.advance(testProbe)

			resumed := h.wantCalls("Resume", 1)[0]
			c := h.claim()
			if c.State != StateLaunching || c.Locator == nil || c.Generation != generation+1 || c.Budgets != tc.relaunched || resumed.Spec.ResumeSessionFile != sessionFile {
				t.Fatalf("after the checked retry the claim is %s at generation %d with locator %+v and budgets %+v, resuming %q; want the same session launching at %d with a process and %+v",
					c.State, c.Generation, c.Locator, c.Budgets, resumed.Spec.ResumeSessionFile, generation+1, tc.relaunched)
			}
			if lines := h.logs.lines("trying it again at the probe interval"); len(lines) != 2 {
				t.Errorf("logged %v, want each unchecked relaunch named once", lines)
			}
			h.relaunched()
			if prompts := h.prompts(); len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1].Message, "the task") {
				t.Errorf("the relaunched agent was prompted %+v, want the phase's task", prompts)
			}
		})
	}
}

// A suspension that reaches a retirement's relaunch while it waits for the store is held, as one of
// any launching claim is, and the retry ends the wait in that suspension rather than a relaunch,
// with no process left to stop. An operator's suspension keeps the phase's task for its resume.
func TestASuspensionOfARelaunchWaitingForTheStoreSuspendsTheClaimAtItsRetry(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateRegistered)
	h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
	h.m.deps.Runtime = &unansweredSuspend{Runtime: h.rt, store: store, checks: 1}
	h.advance(deadline)
	h.wantState(StateLaunching)

	if err := h.handleWithoutPanic(RequestSuspend{Claim: h.token, Reason: "drain", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the operator's suspend of the waiting relaunch = %v, want it held", err)
	}
	h.advance(testProbe)

	c := h.claim()
	if c.State != StateSuspended || c.SuspendedBy != SuspendedByOperator || c.Locator != nil {
		t.Fatalf("after the retry the claim is %s by %q with locator %+v, want suspended by the operator with none", c.State, c.SuspendedBy, c.Locator)
	}
	if p := c.Pending; p == nil || p.Phase != phase.Implementing {
		t.Errorf("the operator's suspension left the pending task %+v, want the phase's task kept", p)
	}
	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 0)
}

// A daemon's suspension that reaches a retirement's relaunch while it waits for the store ends the
// wait in that suspension at the retry, as the operator's does, and ends the claim's phase with
// it: the phase's task is retired, in memory as in the store, so the next start hands the session
// its new phase's task. Nothing is launched.
func TestADaemonSuspensionOfARelaunchWaitingForTheStoreRetiresThePhasesTask(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateRegistered)
	h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
	h.m.deps.Runtime = &unansweredSuspend{Runtime: h.rt, store: store, checks: 1}
	h.advance(deadline)
	h.wantState(StateLaunching)

	if err := h.handleWithoutPanic(RequestSuspend{Claim: h.token, Reason: "the issue left the workflow", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the daemon's suspend of the waiting relaunch = %v, want it held", err)
	}
	h.advance(testProbe)

	c := h.claim()
	if c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon || c.Locator != nil {
		t.Fatalf("after the retry the claim is %s by %q with locator %+v, want suspended by the daemon with none", c.State, c.SuspendedBy, c.Locator)
	}
	if c.Pending != nil {
		t.Errorf("the daemon's suspension left the pending task %+v, want the phase's task retired", c.Pending)
	}
	if stored := h.store.load(h.token); stored.State != StateSuspended || stored.SuspendedBy != SuspendedByDaemon || stored.Pending != nil {
		t.Errorf("stored %s by %q with pending %+v, want suspended by the daemon with the phase's task retired", stored.State, stored.SuspendedBy, stored.Pending)
	}
	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 0)
}

// A retirement whose relaunch is checked once the stop has landed, but whose launch the store does
// not write, has started nothing: the process it let go is stopped, and the claim records none. It
// keeps the claim's task and tries the same launch again at the probe interval, as an unchecked
// relaunch does, until the write lands: nothing else would launch a claim that records no process
// before a restart. The relaunched agent is handed the task once it is ready.
func TestARetirementWhoseRelaunchTheStoreDoesNotWriteKeepsItsTaskAndRelaunchesAtTheProbeInterval(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)
	h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
	retired := h.locator()
	// The stop lands, and from then on the store takes no claim write.
	h.m.deps.Runtime = &failingWritesSuspend{Runtime: h.rt, store: h.store}

	h.advance(deadline)

	c := h.claim()
	if c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("the unwritten launch left the claim %s with locator %+v, want launching with no process", c.State, c.Locator)
	}
	if p := c.Pending; p == nil || p.Task != "the task" || p.Phase != phase.Implementing {
		t.Fatalf("the unwritten launch left the pending task %+v, want the phase's task kept", p)
	}
	if _, ok := h.m.timers[TimerRelaunch]; !ok {
		t.Fatalf("the unwritten launch armed %v, want the relaunch's retry", h.m.timers)
	}
	if stops := h.calls("Suspend"); len(stops) != 2 || stops[0].Locator != retired || stops[1].Locator != retired {
		t.Errorf("stopped %+v, want the retired process stopped by the retirement and again by the launch that let it go", stops)
	}
	if lines := h.logs.lines("the launch could not be written; trying it again at the probe interval"); len(lines) != 1 {
		t.Errorf("logged %v, want the unwritten launch named once", lines)
	}
	h.wantCalls("Resume", 0)

	h.store.fail("PutClaim", nil)
	h.advance(testProbe)

	resumed := h.wantCalls("Resume", 1)[0]
	if c := h.claim(); c.State != StateLaunching || c.Locator == nil || resumed.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("after the retry the claim is %s with locator %+v, resuming %q; want the same session launching with a process", c.State, c.Locator, resumed.Spec.ResumeSessionFile)
	}
	h.wantBudgets(Budgets{LaunchFailures: 1})
	if stored := h.store.load(h.token); stored.State != StateLaunching || stored.Locator == nil || stored.Pending == nil || stored.Pending.Phase != phase.Implementing {
		t.Fatalf("stored %s with locator %+v and pending %+v, want the relaunch's process and the phase's task", stored.State, stored.Locator, stored.Pending)
	}
	h.relaunched()
	if prompts := h.prompts(); len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1].Message, "the task") {
		t.Errorf("the relaunched agent was prompted %+v, want the phase's task", prompts)
	}
}

// failingWritesSuspend is a stop that lands, after which the store refuses every claim write.
type failingWritesSuspend struct {
	*fake.Runtime
	store *memStore
}

func (r *failingWritesSuspend) Suspend(ctx context.Context, loc runtime.Locator) error {
	r.store.fail("PutClaim", errBoom)
	return r.Runtime.Suspend(ctx, loc)
}

// A waiting relaunch lives exactly as long as its timer: whatever lets the claim's process go
// disarms the retry and drops the relaunch with it, so nothing can run it afterwards.
func TestWhatLetsAWaitingRelaunchsClaimGoDropsTheRelaunchWithItsTimer(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateRegistered)
	h.m.deps.Runtime = &unansweredSuspend{Runtime: h.rt, store: store, checks: 1}
	h.advance(deadline)
	if _, ok := h.m.timers[TimerRelaunch]; !ok || h.m.relaunch == nil {
		t.Fatalf("the unchecked relaunch armed %v with relaunch %t, want its retry waiting", h.m.timers, h.m.relaunch != nil)
	}

	h.must(RequestStop{Claim: h.token})

	h.wantState(StateRetired)
	if _, ok := h.m.timers[TimerRelaunch]; ok || h.m.relaunch != nil {
		t.Errorf("after the stop the timers are %v with relaunch %t, want the retry and its relaunch both gone", h.m.timers, h.m.relaunch != nil)
	}
}

// A prompt retirement whose relaunch the tree lifecycle refuses keeps the process and leaves its
// prompt count one short of the limit (chargePrompt), so once launches are allowed again the next
// prompt failure retires the process and relaunches the same session.
func TestAPromptRetirementTheTreeLifecycleRefusesIsTriedAgainAtTheNextPromptFailure(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "the task", Phase: phase.Implementing})
	for range testLimits().PromptFailures - 1 {
		h.advance(testRPC)
		h.observe(runtime.Alive)
	}
	store.reserved = true
	h.advance(testRPC)
	h.wantCalls("Suspend", 0)
	h.wantState(StateReady)
	h.wantBudgets(Budgets{PromptFailures: testLimits().PromptFailures - 1})

	store.reserved = false
	h.observe(runtime.Alive) // the sweep sends the task again
	h.advance(testRPC)       // and its prompt starts no turn either

	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PromptRetires: 1})
	if p := h.claim().Pending; p == nil || p.Phase != phase.Implementing {
		t.Errorf("after the retirement the pending task is %+v, want the phase's task kept", p)
	}
}
