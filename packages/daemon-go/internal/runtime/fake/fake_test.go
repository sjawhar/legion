package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func spec(token claim.Token) runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim:      token,
		Project:    "omp",
		Tree:       "LEGION-208",
		Issue:      "LEGION-208",
		Role:       claim.RoleTester,
		Generation: 3,
		BootToken:  "boot-1",
	}
}

// The reason to record is that most of what the supervisor does to a runtime returns nothing: a
// suspend, a stop, an orphan reconciliation are all "it happened, with these arguments, in this
// order" and nothing else.
func TestRuntimeRecordsEveryCallInOrderWithItsArguments(t *testing.T) {
	ctx := context.Background()
	fake := NewRuntime()

	locator, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := fake.Resume(ctx, locator, spec("legion-omp-LEGION-208-tester")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := fake.Suspend(ctx, locator); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := fake.Stop(ctx, locator, 7*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := fake.Probe(ctx, locator); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := fake.ReconcileOrphans(ctx, []runtime.Locator{locator}, time.Minute); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	identity := runtime.GitIdentity{Name: "Legion", Email: "legion@example.com"}
	if err := fake.AdoptWorkingCopy(ctx, locator, identity); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if launch := fake.ControllerLaunch(); launch != runtime.ControllerLaunchDaemon {
		t.Fatalf("controller launch: got %q, want %q", launch, runtime.ControllerLaunchDaemon)
	}

	wantMethods := []string{
		"Spawn", "Resume", "Suspend", "Stop", "Probe", "ReconcileOrphans", "AdoptWorkingCopy",
		"ControllerLaunch",
	}
	got := fake.Methods()
	if len(got) != len(wantMethods) {
		t.Fatalf("methods: got %v, want %v", got, wantMethods)
	}
	for i, method := range wantMethods {
		if got[i] != method {
			t.Fatalf("methods: got %v, want %v", got, wantMethods)
		}
	}

	calls := fake.Calls()
	if calls[0].Spec.Claim != "legion-omp-LEGION-208-tester" || calls[0].Spec.Role != claim.RoleTester {
		t.Errorf("spawn call recorded %+v", calls[0].Spec)
	}
	if calls[1].Locator != locator {
		t.Errorf("resume recorded locator %+v, want %+v", calls[1].Locator, locator)
	}
	if calls[3].Grace != 7*time.Second {
		t.Errorf("stop recorded grace %s, want 7s", calls[3].Grace)
	}
	if len(calls[5].Known) != 1 || calls[5].Known[0] != locator {
		t.Errorf("reconcile recorded known %+v", calls[5].Known)
	}
	if calls[6].Identity != identity {
		t.Errorf("adopt recorded identity %+v, want %+v", calls[6].Identity, identity)
	}
	if of := fake.CallsOf("Stop"); len(of) != 1 || of[0].Grace != 7*time.Second {
		t.Errorf("CallsOf(Stop): %+v", of)
	}
}

// A locator the fake mints is a locator the daemon could have stored: the supervisor validates
// what it reads back, so a fake that handed out an unvalidatable one would make every test that
// round-trips a claim through the store fail for a reason no production code has.
func TestSpawnAnswersTheScriptAndOtherwiseMintsALocatorThatValidates(t *testing.T) {
	ctx := context.Background()
	fake := NewRuntime()
	scripted := runtime.Locator{
		Runtime:     runtime.RuntimeSandbox,
		Claim:       "legion-omp-LEGION-208-tester",
		Incarnation: "pod-uid",
		Sandbox:     &runtime.SandboxLocator{Namespace: "legion", Name: "sandbox-1"},
	}
	refusal := errors.New("no pane could be opened")
	fake.ScriptSpawn(SpawnResult{Locator: scripted}, SpawnResult{Err: refusal})

	first, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if first != scripted {
		t.Fatalf("spawn: got %+v, want the scripted %+v", first, scripted)
	}
	if _, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester")); !errors.Is(err, refusal) {
		t.Fatalf("spawn: got %v, want %v", err, refusal)
	}

	minted, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-planner"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := minted.Validate(); err != nil {
		t.Fatalf("the minted locator does not validate: %v", err)
	}
	if minted.Claim != "legion-omp-LEGION-208-planner" {
		t.Fatalf("minted locator is on claim %q", minted.Claim)
	}
}

// Resuming is the same agent in a new process, so the incarnation has to move: a fake that
// handed back the old one would let a test pass while the fences it is testing are broken.
func TestResumeMintsANewIncarnationForTheSameClaim(t *testing.T) {
	ctx := context.Background()
	fake := NewRuntime()
	before, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	after, err := fake.Resume(ctx, before, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if after.Claim != before.Claim {
		t.Fatalf("resume moved the claim: %q -> %q", before.Claim, after.Claim)
	}
	if after.Incarnation == before.Incarnation {
		t.Fatalf("resume kept the incarnation %q", after.Incarnation)
	}
	if err := after.Validate(); err != nil {
		t.Fatalf("the resumed locator does not validate: %v", err)
	}
}

func TestTheSweepPlaysScriptedObservationsInOrderAndClosesWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := NewRuntime()
	locator, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Scripted before the sweep exists: a supervisor's first observation can land before it has
	// started pumping.
	fake.Emit(runtime.Observation{Locator: locator, Kind: runtime.Alive})

	sweep, err := fake.Observe(ctx)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	// And after: the sweep is a live source, not a replay of a fixed list.
	fake.Emit(
		runtime.Observation{Locator: locator, Kind: runtime.Uncertain, Detail: "listing-failed"},
		runtime.Observation{Locator: locator, Kind: runtime.Gone},
	)

	for _, want := range []runtime.ObservationKind{runtime.Alive, runtime.Uncertain, runtime.Gone} {
		select {
		case observation, open := <-sweep:
			if !open {
				t.Fatalf("the sweep closed before %q", want)
			}
			if observation.Kind != want {
				t.Fatalf("observation: got %q, want %q", observation.Kind, want)
			}
			if observation.Locator != locator {
				t.Fatalf("observation is about %+v, want %+v", observation.Locator, locator)
			}
			if observation.At.IsZero() {
				t.Fatalf("observation %q carries no time", want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the sweep never played %q", want)
		}
	}

	cancel()
	select {
	case _, open := <-sweep:
		if open {
			t.Fatalf("the sweep sent an observation after its context ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the sweep did not close when its context ended")
	}
}

// `Uncertain` is a verdict and an error is the runtime failing to answer at all; a double that
// could not produce both separately could not test the difference.
func TestProbeAnswersTheScriptedVerdictOrTheScriptedFailure(t *testing.T) {
	ctx := context.Background()
	fake := NewRuntime()
	locator, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	broken := errors.New("the tmux server socket is unreadable")
	fake.ScriptProbe(
		ProbeResult{Kind: runtime.Uncertain, Detail: "listing-failed"},
		ProbeResult{Kind: runtime.NotRecordedProcess},
		ProbeResult{Err: broken},
	)

	uncertain, err := fake.Probe(ctx, locator)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if uncertain.Kind != runtime.Uncertain || uncertain.Detail != "listing-failed" {
		t.Fatalf("probe: got %+v", uncertain)
	}
	if uncertain.Locator != locator {
		t.Fatalf("probe answered about %+v, want %+v", uncertain.Locator, locator)
	}
	notRecorded, err := fake.Probe(ctx, locator)
	if err != nil || notRecorded.Kind != runtime.NotRecordedProcess {
		t.Fatalf("probe: got %+v, %v", notRecorded, err)
	}
	if _, err := fake.Probe(ctx, locator); !errors.Is(err, broken) {
		t.Fatalf("probe: got %v, want %v", err, broken)
	}
	// Unscripted, a process the fake spawned is alive: the uninteresting case stays quiet.
	alive, err := fake.Probe(ctx, locator)
	if err != nil || alive.Kind != runtime.Alive {
		t.Fatalf("probe: got %+v, %v", alive, err)
	}
}

func TestAScriptedFailureReachesTheMethodItWasSetOn(t *testing.T) {
	ctx := context.Background()
	fake := NewRuntime()
	locator, err := fake.Spawn(ctx, spec("legion-omp-LEGION-208-tester"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	stuck := errors.New("the pane refused to die")
	fake.FailStop(stuck)
	if err := fake.Stop(ctx, locator, time.Second); !errors.Is(err, stuck) {
		t.Fatalf("stop: got %v, want %v", err, stuck)
	}
	if err := fake.Suspend(ctx, locator); err != nil {
		t.Fatalf("suspend was not the method that was made to fail: %v", err)
	}
}

func TestControllerLaunchAnswersWhatTheRuntimeWasBuiltFor(t *testing.T) {
	operator := NewRuntime()
	operator.Launch = runtime.ControllerLaunchOperator
	if got := operator.ControllerLaunch(); got != runtime.ControllerLaunchOperator {
		t.Fatalf("controller launch: got %q, want %q", got, runtime.ControllerLaunchOperator)
	}
}

func TestConnRecordsWhatTheDaemonAskedOfTheAgent(t *testing.T) {
	ctx := context.Background()
	conn := NewConn()

	if err := conn.Prompt(ctx, "delivery-1", "plan LEGION-208"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := conn.Prompt(ctx, "delivery-1", "plan LEGION-208"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	prompts := conn.Prompts()
	if len(prompts) != 2 {
		t.Fatalf("prompts: got %d, want 2 — a repeated delivery is still a frame the daemon sent", len(prompts))
	}
	if prompts[0].DeliveryID != "delivery-1" || prompts[0].Message != "plan LEGION-208" {
		t.Fatalf("prompt recorded %+v", prompts[0])
	}

	state, err := conn.GetState(ctx)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.IsStreaming {
		t.Fatalf("a fresh connection reported a turn in flight")
	}
	conn.SetStreaming(true)
	if state, err = conn.GetState(ctx); err != nil || !state.IsStreaming {
		t.Fatalf("get state: got %+v, %v", state, err)
	}

	identity := runtime.GitIdentity{Name: "Legion", Email: "legion@example.com"}
	if err := conn.AdoptWorkingCopy(ctx, identity, 30*time.Second); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	adoptions := conn.Adoptions()
	if len(adoptions) != 1 || adoptions[0].Identity != identity || adoptions[0].Timeout != 30*time.Second {
		t.Fatalf("adoptions: %+v", adoptions)
	}

	if err := conn.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if conn.Shutdowns() != 1 {
		t.Fatalf("shutdowns: got %d, want 1", conn.Shutdowns())
	}

	refused := errors.New("the agent is not accepting prompts")
	conn.FailPrompt(refused)
	if err := conn.Prompt(ctx, "delivery-2", "test LEGION-208"); !errors.Is(err, refused) {
		t.Fatalf("prompt: got %v, want %v", err, refused)
	}
	if len(conn.Prompts()) != 3 {
		t.Fatalf("a refused prompt is still a frame the daemon sent: %+v", conn.Prompts())
	}
}

// The directory answers for the claims that have a live connection and says so for the ones that
// do not — the ordinary state of a pane whose agent has not connected yet, or has gone.
func TestConnsAnswersOnlyForClaimsWithALiveConnection(t *testing.T) {
	conns := NewConns()
	conn := NewConn()
	conns.Register("legion-omp-LEGION-208-tester", conn)

	got, ok := conns.Conn("legion-omp-LEGION-208-tester")
	if !ok {
		t.Fatalf("a registered claim has no connection")
	}
	if got != runtime.Conn(conn) {
		t.Fatalf("the directory handed back another connection")
	}
	if _, ok := conns.Conn("legion-omp-LEGION-208-planner"); ok {
		t.Fatalf("a claim that never connected has a connection")
	}
	conns.Remove("legion-omp-LEGION-208-tester")
	if _, ok := conns.Conn("legion-omp-LEGION-208-tester"); ok {
		t.Fatalf("a closed connection is still in the directory")
	}
}

// The point of the fake: code written against the boundary takes it without knowing it is one.
func TestTheFakesAreUsableThroughTheBoundaryInterfaces(t *testing.T) {
	var asRuntime runtime.Runtime = NewRuntime()
	if asRuntime.ControllerLaunch() != runtime.ControllerLaunchDaemon {
		t.Fatalf("controller launch through the interface: %q", asRuntime.ControllerLaunch())
	}
	conns := NewConns()
	conns.Register("legion-omp-LEGION-208-tester", NewConn())
	var directory runtime.Conns = conns
	conn, ok := directory.Conn("legion-omp-LEGION-208-tester")
	if !ok {
		t.Fatalf("the directory answered no connection through the interface")
	}
	if err := conn.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown through the interface: %v", err)
	}
}
