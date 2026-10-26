package tmux

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A tmux daemon that takes over a project's store from a daemon that ran it under runtime
// kubernetes finds claims whose recorded process is a Sandbox role container: a locator another
// runtime recorded (foreign). Their pods are gone before the tmux daemon boots (docs/deployment.md,
// "Moving a project onto tmux"); this runtime can neither see nor stop them, so each is reported
// Gone and Displaced — relaunched here on its session, charged nothing — and asks tmux nothing.

// sandboxLocator is a valid locator of the sandbox runtime for the claim of role on issue.
func sandboxLocator(t *testing.T, project, issue string, role claim.Role) runtime.Locator {
	t.Helper()
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	loc := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation("pod-uid-1", 3),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-" + strings.ToLower(issue), PodUID: "pod-uid-1", Container: string(role), Generation: 3},
	}
	if err := loc.Validate(); err != nil {
		t.Fatal(err)
	}
	return loc
}

// Probe, Suspend and Release of a foreign locator answer at once and run no tmux: the probe is
// Gone and Displaced, naming the pod, and a stop has nothing of it to stop.
func TestAForeignLocatorIsGoneDisplacedAndRunsNoTmux(t *testing.T) {
	ctx := context.Background()
	f := newFakeServerRuntime(t, staticCredential, nil)
	loc := sandboxLocator(t, "omp", "LEGION-43", claim.RoleTester)

	obs, err := f.Probe(ctx, loc)
	if err != nil {
		t.Fatalf("Probe of a foreign locator: %v", err)
	}
	if obs.Kind != runtime.Gone || !obs.Displaced || obs.Locator != loc || !strings.Contains(obs.Detail, "pod legion-legion-43/tester") {
		t.Errorf("Probe of a foreign locator = %+v, want Gone, Displaced, naming its pod", obs)
	}
	if err := f.Suspend(ctx, loc); err != nil {
		t.Errorf("Suspend of a foreign locator: %v", err)
	}
	if err := f.Release(ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}); err != nil {
		t.Errorf("Release of a foreign locator: %v", err)
	}
	if len(f.argvs) != 0 {
		t.Errorf("a foreign locator ran tmux: %q", f.argvs)
	}
}

// ReconcileOrphans takes the known claims of a store another runtime ran: a foreign locator is not
// refused, protects nothing and joins no watch; the next sweep reports it Gone and Displaced once,
// then never again. The roles of one issue share its pod, and two of them at the same generation
// share an incarnation (`PodUID/Generation`): each claim is still displaced on its own.
func TestReconcileOrphansDisplacesForeignLocatorsAndTheSweepReportsEachOnce(t *testing.T) {
	ctx := context.Background()
	f := newFakeServerRuntime(t, staticCredential, nil)
	tester := sandboxLocator(t, "omp", "LEGION-43", claim.RoleTester)
	planner := sandboxLocator(t, "omp", "LEGION-43", claim.RolePlanner)
	if tester.Incarnation != planner.Incarnation {
		t.Fatalf("the two roles' incarnations %s and %s differ; the test needs one pod and one generation", tester.Incarnation, planner.Incarnation)
	}
	known := []runtime.Known{{Claim: tester.Claim, Locator: &tester}, {Claim: planner.Claim, Locator: &planner}, {Claim: "legion-omp-legion-45-architect"}}

	if err := f.ReconcileOrphans(ctx, known, 0); err != nil {
		t.Fatalf("ReconcileOrphans with foreign locators: %v", err)
	}
	if tracked := f.trackedProcesses(); len(tracked) != 0 {
		t.Errorf("foreign locators joined the watch: %+v", tracked)
	}

	sweep := func() []runtime.Observation {
		out := make(chan runtime.Observation, 8)
		if !f.sweep(ctx, out) {
			t.Fatal("the sweep ended early")
		}
		close(out)
		var seen []runtime.Observation
		for obs := range out {
			seen = append(seen, obs)
		}
		return seen
	}
	first := sweep()
	var displaced []claim.Token
	for _, obs := range first {
		if obs.Kind != runtime.Gone || !obs.Displaced {
			t.Errorf("the first sweep reported %+v, want only Gone and Displaced", obs)
		}
		displaced = append(displaced, obs.Locator.Claim)
	}
	if want := []claim.Token{planner.Claim, tester.Claim}; !slices.Equal(displaced, want) {
		t.Errorf("the first sweep displaced %v, want %v", displaced, want)
	}
	if again := sweep(); len(again) != 0 {
		t.Errorf("the next sweep reported %+v, want nothing: each foreign locator is reported once", again)
	}
}

// A displaced locator a stop let go of before the sweep reported it is not reported: the claim's
// machine already acted on it. Another role of the same pod, at the same incarnation, still is.
func TestAForeignLocatorStoppedBeforeTheSweepIsNotReported(t *testing.T) {
	ctx := context.Background()
	f := newFakeServerRuntime(t, staticCredential, nil)
	tester := sandboxLocator(t, "omp", "LEGION-43", claim.RoleTester)
	planner := sandboxLocator(t, "omp", "LEGION-43", claim.RolePlanner)
	if err := f.ReconcileOrphans(ctx, []runtime.Known{{Claim: tester.Claim, Locator: &tester}, {Claim: planner.Claim, Locator: &planner}}, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Suspend(ctx, tester); err != nil {
		t.Fatal(err)
	}
	out := make(chan runtime.Observation, 4)
	f.sweep(ctx, out)
	close(out)
	var reported []claim.Token
	for obs := range out {
		reported = append(reported, obs.Locator.Claim)
	}
	if !slices.Equal(reported, []claim.Token{planner.Claim}) {
		t.Errorf("the sweep reported %v, want the planner alone: the tester's foreign locator was already stopped", reported)
	}
}

// A resume whose previous incarnation another runtime recorded waits for nothing — this runtime
// cannot see that process — and opens the pane at once, continuing the session from the folder it
// was carried into when its recorded path is not on this host.
func TestAResumeOverAForeignIncarnationOpensThePaneOnTheCarriedSession(t *testing.T) {
	ctx := context.Background()
	f := newFakeServerRuntime(t, staticCredential, nil)
	f.stopGrace = time.Hour
	spec := f.spec(t, claim.RoleTester)
	prev := sandboxLocator(t, spec.Project, spec.Issue, claim.RoleTester)
	sessions := filepath.Join(f.stateDir, "home", ".omp", "agent", "sessions")
	spec.ResumeSessionFile = filepath.Join(sessions, "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	carried := filepath.Join(sessions, "--pod-work--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, carried, time.Now())

	done := make(chan error, 1)
	go func() {
		_, err := f.Resume(ctx, &prev, spec)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume over a foreign incarnation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Resume over a foreign incarnation waited for a process this runtime cannot see")
	}
	var command string
	for _, argv := range f.argvs {
		if verb(argv) == "new-window" {
			command = argv[len(argv)-1]
		}
	}
	if !strings.Contains(command, "--resume="+carried) {
		t.Errorf("the pane's command = %q, want it to resume %s", command, carried)
	}
}
