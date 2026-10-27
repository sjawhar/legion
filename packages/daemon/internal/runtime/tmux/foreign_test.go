package tmux

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A tmux daemon that takes over a project's store from a daemon that ran it under runtime
// kubernetes finds claims whose recorded process is a Sandbox role container: a locator another
// runtime recorded. Under `runtime.tmux.take_over: kubernetes` (foreign) their pods are gone before
// the tmux daemon boots (docs/deployment.md, "Moving a project onto tmux"); this runtime can neither
// see nor stop them, so each is reported Gone and Displaced — relaunched here on its session,
// charged nothing — and asks tmux nothing. Without the key, every such locator is refused, naming
// the claim and the key.

// sandboxLocator is a valid locator of the sandbox runtime for the claim of role on issue: every
// role of an issue pod runs in its one container (runtime.IssuePodContainer).
func sandboxLocator(t *testing.T, project, issue string, role claim.Role) runtime.Locator {
	t.Helper()
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	loc := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation("pod-uid-1", 3),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-" + strings.ToLower(issue), PodUID: "pod-uid-1", Container: runtime.IssuePodContainer, Generation: 3},
	}
	if err := loc.Validate(); err != nil {
		t.Fatal(err)
	}
	return loc
}

// takingOver is a fake server runtime under `runtime.tmux.take_over: kubernetes`, its panes' HOME
// under its state directory, so the pane's Oh My Pi sessions directory is
// `<state_dir>/home/.omp/agent/sessions`.
func takingOver(t *testing.T) *fakeServerRuntime {
	t.Helper()
	return newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
		o.TakeOver = TakeOverKubernetes
		o.Environ = append(o.Environ, "HOME="+filepath.Join(o.StateDir, "home"))
	})
}

// paneSessions is the Oh My Pi sessions directory of a takingOver runtime's panes.
func paneSessions(f *fakeServerRuntime) string {
	return filepath.Join(f.stateDir, "home", ".omp", "agent", "sessions")
}

// opened is whether any recorded argv opened a pane.
func opened(argvs [][]string) bool {
	return slices.ContainsFunc(argvs, func(argv []string) bool { v := verb(argv); return v == "new-window" || v == "split-window" })
}

// takeOverRefusal is the refusal of a Sandbox locator by a runtime without runtime.tmux.take_over.
func takeOverRefusal(loc runtime.Locator) string {
	return fmt.Sprintf(`tmux runtime cannot act on a "sandbox" locator (%s): a daemon that ran the project under runtime kubernetes recorded it; once every pod of that daemon is stopped, runtime.tmux.take_over: kubernetes lets this daemon take the claim over`, loc.Claim)
}

// Probe, Suspend and Release of a foreign locator answer at once and run no tmux: the probe is
// Gone and Displaced, naming the pod, and a stop has nothing of it to stop.
func TestAForeignLocatorIsGoneDisplacedAndRunsNoTmux(t *testing.T) {
	ctx := context.Background()
	f := takingOver(t)
	loc := sandboxLocator(t, "omp", "LEGION-43", claim.RoleTester)

	obs, err := f.Probe(ctx, loc)
	if err != nil {
		t.Fatalf("Probe of a foreign locator: %v", err)
	}
	if obs.Kind != runtime.Gone || !obs.Displaced || obs.Locator != loc || !strings.Contains(obs.Detail, "pod legion-legion-43/roles") {
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

// Without runtime.tmux.take_over nothing is foreign: a Sandbox locator is refused by Probe, by a
// stop, by a resume over it, and by boot's ReconcileOrphans, which reaps nothing then — each
// refusal naming the claim and the key that would let this daemon take it over — and none runs
// tmux or opens a pane.
func TestWithoutTakeOverASandboxLocatorIsRefusedNamingTheKey(t *testing.T) {
	ctx := context.Background()
	f := newFakeServerRuntime(t, staticCredential, nil)
	loc := sandboxLocator(t, "omp", "LEGION-43", claim.RoleTester)
	want := takeOverRefusal(loc)

	if _, err := f.Probe(ctx, loc); err == nil || err.Error() != want {
		t.Errorf("Probe = %v, want %q", err, want)
	}
	if err := f.Suspend(ctx, loc); err == nil || err.Error() != want {
		t.Errorf("Suspend = %v, want %q", err, want)
	}
	known := []runtime.Known{{Claim: loc.Claim, Locator: &loc}, {Claim: "legion-omp-legion-45-architect"}}
	if err := f.ReconcileOrphans(ctx, known, 0); err == nil || err.Error() != "reconcile orphans: "+want {
		t.Errorf("ReconcileOrphans = %v, want %q", err, "reconcile orphans: "+want)
	}
	spec := f.spec(t, claim.RoleTester)
	spec.ResumeSessionFile = filepath.Join(t.TempDir(), "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, spec.ResumeSessionFile, time.Now())
	if _, err := f.Resume(ctx, &loc, spec); err == nil || err.Error() != "resume "+string(loc.Claim)+": "+want {
		t.Errorf("Resume over a Sandbox locator = %v, want %q", err, "resume "+string(loc.Claim)+": "+want)
	}
	if len(f.argvs) != 0 {
		t.Errorf("a refused Sandbox locator ran tmux: %q", f.argvs)
	}
}

// New takes over kubernetes alone.
func TestNewRefusesATakeOverOfAnotherRuntime(t *testing.T) {
	_, err := New(Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=/usr/bin:/bin"}, TakeOver: "sandbox",
		Executable: func() (string, error) { return "/opt/legion", nil },
	})
	if err == nil || err.Error() != `tmux runtime: take_over "sandbox" is not a runtime it takes claims over from (kubernetes)` {
		t.Errorf("New with take_over sandbox = %v", err)
	}
}

// ReconcileOrphans takes the known claims of a store another runtime ran: a foreign locator is not
// refused, protects nothing and joins no watch; the next sweep reports it Gone and Displaced once,
// then never again. The roles of one issue share its pod, and two of them at the same generation
// share an incarnation (`PodUID/Generation`): each claim is still displaced on its own.
func TestReconcileOrphansDisplacesForeignLocatorsAndTheSweepReportsEachOnce(t *testing.T) {
	ctx := context.Background()
	f := takingOver(t)
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
	f := takingOver(t)
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
// cannot see that process — and opens the pane at once on the session the pod recorded under the
// image's home (`/home/legion/.omp/profiles/legion/…`), which is not this host's: the session was
// carried into the pane's own sessions directory, and is found there by its name, in the pod's
// folder.
func TestAResumeOverAForeignIncarnationOpensThePaneOnTheCarriedSession(t *testing.T) {
	ctx := context.Background()
	f := takingOver(t)
	f.stopGrace = time.Hour
	spec := f.spec(t, claim.RoleTester)
	prev := sandboxLocator(t, spec.Project, spec.Issue, claim.RoleTester)
	spec.ResumeSessionFile = podSession
	carried := filepath.Join(paneSessions(f), filepath.Base(filepath.Dir(podSession)), filepath.Base(podSession))
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

// A resume over a foreign incarnation whose session is found nowhere on this host — neither in its
// recorded sessions directory nor in the pane's — starts nothing and is runtime.ErrSessionFileMissing,
// which the claim's machine answers with a fresh agent, charged nothing: the operator chose the
// takeover, and a session kept in the other runtime's database is not carried.
func TestAResumeOverAForeignIncarnationWithItsSessionNowhereIsTheMissingSession(t *testing.T) {
	ctx := context.Background()
	f := takingOver(t)
	spec := f.spec(t, claim.RoleTester)
	prev := sandboxLocator(t, spec.Project, spec.Issue, claim.RoleTester)
	spec.ResumeSessionFile = podSession

	_, err := f.Resume(ctx, &prev, spec)
	want := "resume " + string(spec.Claim) + ": the recorded session file is missing: no OMP session at " + podSession +
		", and no other folder of " + filepath.Dir(filepath.Dir(podSession)) + " or of the pane's sessions directory " + paneSessions(f) + " holds that session"
	if !errors.Is(err, runtime.ErrSessionFileMissing) || err.Error() != want {
		t.Fatalf("Resume = %v, want runtime.ErrSessionFileMissing: %q", err, want)
	}
	if opened(f.argvs) {
		t.Errorf("a resume of a missing session opened a pane: %q", f.argvs)
	}
}

// While take_over is set, a session found nowhere starts a fresh agent whatever the claim's
// previous incarnation is and wherever the session was recorded: over a pane of this runtime, with
// no incarnation at all, at a pod's path or at one of this host's own. Neither the path nor the
// incarnation tells the other runtime's sessions from this host's — a host built from the worker
// image runs under the same home and profile a pod did, and a claim the other runtime suspended
// has no incarnation left — so every one is runtime.ErrSessionFileMissing, which the claim's
// machine answers with a fresh agent, charged nothing, rather than a refusal no retry gets past.
func TestUnderTakeOverEverySessionFoundNowhereStartsFresh(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prev    func(spec runtime.SpawnSpec) *runtime.Locator
		session func(f *fakeServerRuntime) string
	}{
		"a pod's session with no previous incarnation": {
			prev:    func(runtime.SpawnSpec) *runtime.Locator { return nil },
			session: func(*fakeServerRuntime) string { return podSession },
		},
		"this host's own session over a previous pane": {
			prev: func(spec runtime.SpawnSpec) *runtime.Locator {
				return &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: spec.Claim, Incarnation: "4242:77", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
			},
			session: func(f *fakeServerRuntime) string {
				return filepath.Join(paneSessions(f), "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-gone.jsonl")
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := takingOver(t)
			spec := f.spec(t, claim.RoleTester)
			spec.ResumeSessionFile = tc.session(f)
			_, err := f.Resume(ctx, tc.prev(spec), spec)
			if !errors.Is(err, runtime.ErrSessionFileMissing) || !strings.HasPrefix(err.Error(), "resume "+string(spec.Claim)+": the recorded session file is missing: no OMP session at "+spec.ResumeSessionFile) {
				t.Fatalf("Resume = %v, want runtime.ErrSessionFileMissing naming %s", err, spec.ResumeSessionFile)
			}
			if opened(f.argvs) {
				t.Errorf("a resume of a missing session opened a pane: %q", f.argvs)
			}
		})
	}
}

// With take_over off, the same resumes are the refusal to start fresh, never
// runtime.ErrSessionFileMissing: the claim resumes the agent it recorded or none, and the refusal
// is charged by the claim's machine, which keeps the recorded session.
func TestWithoutTakeOverAMissingSessionIsRefused(t *testing.T) {
	ctx := context.Background()
	for name, session := range map[string]func(f *fakeServerRuntime) string{
		"a pod's session": func(*fakeServerRuntime) string { return podSession },
		"this host's own session": func(f *fakeServerRuntime) string {
			return filepath.Join(paneSessions(f), "--legion-workspace--", "2026-10-20T10-00-00-000Z_ses-gone.jsonl")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := takingOver(t)
			f.takeOver = ""
			spec := f.spec(t, claim.RoleTester)
			spec.ResumeSessionFile = session(f)
			_, err := f.Resume(ctx, nil, spec)
			if err == nil || errors.Is(err, runtime.ErrSessionFileMissing) || !strings.HasPrefix(err.Error(), "Refusing to start LEGION-43 fresh while respawning: recorded OMP session file is missing: "+spec.ResumeSessionFile) {
				t.Fatalf("Resume without take_over = %v, want the refusal to start fresh", err)
			}
			if opened(f.argvs) {
				t.Errorf("a refused resume opened a pane: %q", f.argvs)
			}
		})
	}
}
