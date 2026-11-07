package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// One launch try is bounded inside the runtime call at LaunchBound, counted from the moment it
// holds its pod's launch turn, Spawn's and Resume's alike (both are relaunch): a try whose launcher
// never answers ends at the bound, not at the boot timeout its launcher wait would have run to,
// with an error naming the bound and the stage it stood in, and the caller's own context is
// untouched, so the supervisor reads the failure as the runtime's and charges it.
// The bound is armed only once that launcher wait is the one stage left to outlast: the pod this
// try keeps is already made, Running and bound (awaitingItsRoleLauncher), so no earlier wait of a
// replacing try — the wait for the previous pod, for the new pod — can be the one the bound ends
// in. The row reads the stage the error names and the deadline it carries; how long the try took
// is no statement of the runtime's and is read nowhere here, a bound counted from the launch turn
// being no promise about the wall clock of a loaded machine.
func TestALaunchTryEndsAtItsBoundNamingIt(t *testing.T) {
	for name, launch := range map[string]func(g *rig, spec runtime.SpawnSpec) error{
		"Spawn": func(g *rig, spec runtime.SpawnSpec) error {
			_, err := g.r.Spawn(g.ctx, spec)
			return err
		},
		"Resume": func(g *rig, spec runtime.SpawnSpec) error {
			spec.ResumeSessionFile = resumeSession
			_, err := g.r.Resume(g.ctx, nil, spec)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			g.autoStart.Store(false)
			spec := workerSpec(t)
			awaitingItsRoleLauncher(t, g, spec)
			g.r.launchBound = 500 * time.Millisecond

			err := launch(g, spec)

			want := "wait for its role launcher: context deadline exceeded (the launch try's bound of 500ms ran out)"
			if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s = %v, want %q", name, err, want)
			}
			if g.ctx.Err() != nil {
				t.Fatalf("the caller's context ended with the try: %v", g.ctx.Err())
			}
		})
	}
}

// awaitingItsRoleLauncher leaves spec's issue Sandbox Running with the pod a launch made and that
// pod's role launchers bound to it, no launcher answering: the state a launch into it keeps its
// pod in, so the try that follows runs no wait but awaitRoleLauncher's. A first launch, on a
// context of its own, is cut short once the stores hold what awaitNewPod's predicate reads — the
// Sandbox Running and a pod it owns — and the bind that follows has landed, which is what the next
// launch's launcherBound reads of a pod it may keep. The rig starts no pod (autoStart), so the
// launcher of that pod never says hello, here or in the try the caller measures.
func awaitingItsRoleLauncher(t *testing.T, g *rig, spec runtime.SpawnSpec) {
	t.Helper()
	l, err := g.r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	reaching, cutShort := context.WithCancel(g.ctx)
	defer cutShort()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = g.r.Spawn(reaching, spec)
	}()
	name := SandboxName(spec.Claim)
	testwait.Eventually(t, "the Sandbox Running with its new pod, its role launchers bound to it", func() bool {
		stored, err := g.r.storedSandbox(name)
		if err != nil || stored == nil || stored.mode() != modeRunning {
			return false
		}
		pod := g.r.storedPod(name)
		if !ownedBy(pod, stored.UID) {
			return false
		}
		bound, err := g.r.launcherBound(g.ctx, stored, pod, &l)
		return err == nil && bound
	})
	cutShort()
	<-ended
}

// Through the supervisor, a try that runs out of its bound is a launch failure like a refusal: the
// claim is charged one, and the next try begins at the next generation while the claim is still
// launching. Each try runs out the same way here, so the launch budget runs out and the claim
// fails, its last error naming the bound.
func TestALaunchTryThatRunsOutIsChargedAndTheNextTryRuns(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.r.launchBound = 500 * time.Millisecond
	spec := workerSpec(t)
	machine, _, _ := supervisedSandboxMachine(t, g, spec)
	launched := make(chan error, 1)
	go func() { launched <- machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}) }()

	testwait.Eventually(t, "the second try to begin", func() bool { return machine.View().Generation >= 2 })
	if view := machine.View(); view.Generation != 2 || view.State != supervise.StateLaunching || view.Budgets.LaunchFailures != 1 || view.Locator != nil {
		t.Fatalf("as the second try begins the claim is %s at generation %d with %d launch failures and locator %+v, want launching at 2 with one and none",
			view.State, view.Generation, view.Budgets.LaunchFailures, view.Locator)
	}

	err := <-launched
	if err == nil || !strings.Contains(err.Error(), "the launch try's bound of 500ms ran out") {
		t.Fatalf("the launch ended with %v, want the last try's error naming the bound", err)
	}
	if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Generation != 3 {
		t.Fatalf("claim after the launch = %s at generation %d with %d launch failures, want failed at 3 with 3", got.State, got.Generation, got.Budgets.LaunchFailures)
	}
	if g.ctx.Err() != nil {
		t.Fatalf("the caller's context ended with the tries: %v", g.ctx.Err())
	}
}

// The sibling: a try the caller's own context ends — the daemon's stop — is not the bound's, and
// is charged nothing. The claim stays launching at the same generation with no process recorded,
// for the next boot to launch again, and the supervisor says why.
func TestALaunchTryItsContextEndsIsChargedNothing(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	spec := workerSpec(t)
	log := &lockedLog{}
	machine, _, _ := loggedSandboxMachine(t, g, spec, supervise.Claim{
		Token: spec.Claim, Project: spec.Project, Tree: spec.Tree, Issue: spec.Issue, Role: spec.Role, State: supervise.StateQueued,
	}, log)
	ctx, cancel := context.WithCancel(g.ctx)
	launched := make(chan error, 1)
	go func() { launched <- machine.Handle(ctx, supervise.RequestSpawn{Claim: spec.Claim}) }()
	g.pendingPod(SandboxName(spec.Claim), pulling)
	cancel()

	err := <-launched
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "the launch try's bound") {
		t.Fatalf("the launch ended with %v, want the caller's cancellation, naming no bound", err)
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Budgets.LaunchFailures != 0 || got.Generation != 1 || got.Locator != nil {
		t.Fatalf("claim after the cut-short launch = %s at generation %d with %d launch failures and locator %+v, want launching at 1 with none",
			got.State, got.Generation, got.Budgets.LaunchFailures, got.Locator)
	}
	if !strings.Contains(log.String(), "supervise: launch cut short; its context ended") {
		t.Fatalf("the machine logged:\n%s\nwant the launch cut short by its context", log.String())
	}
}

// launchBound is the sum of a replacing try's waits and of its relaunchCalls calls at apiTimeout:
// three boot timeouts, the repository cache's waits (cacheWaits: five boot timeouts and one other
// ensure's calls), the launcher backstop, the termination grace, then the calls. At the daemon's
// production defaults — worker_boot_timeout_seconds 120 and worker_stop_timeout_seconds 10
// (config), and ProvisionBound's 280 minutes (workspace.FetchBudget + workspace.CloneTimeout +
// workspace.CheckoutTimeout) — that is 5 h 49 min 41 s with relaunchCalls at 85: well past the
// 280-minute floor a provisioning pod's rightful wait sets, and never under the boot timeout,
// ProvisionBound and the grace together. The rig's runtime answers the same arithmetic over its
// own options.
func TestLaunchBoundSumsTheTrysWaitsAndCalls(t *testing.T) {
	boot, provision, grace := 120*time.Second, 280*time.Minute, 10*time.Second
	if relaunchCalls != 85 || cacheEnsureCalls != 14 {
		t.Fatalf("relaunchCalls = %d, want 85: %d per try (%d of them the repository cache's ensure) and %d per each of %d roles", relaunchCalls, relaunchPodCalls, cacheEnsureCalls, relaunchRoleCalls, len(claim.Roles))
	}
	cache := 5*boot + 14*apiTimeout
	waits := 3*boot + cache + (2*boot + provision + 2*recheckInterval) + grace
	if got, want := launchBound(boot, provision, grace), waits+85*apiTimeout; got != want || want != 5*time.Hour+49*time.Minute+41*time.Second {
		t.Fatalf("launchBound = %s, want %s (5h49m41s)", got, want)
	}
	production := testOptions()
	production.BootTimeout, production.TerminationGrace = boot, grace
	r, err := configure(production)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.LaunchBound(); got != launchBound(boot, provision, grace) || got < boot+r.ProvisionBound()+grace || got <= 280*time.Minute {
		t.Fatalf("LaunchBound at the production defaults = %s, want %s, at least %s and past 280m", got, launchBound(boot, provision, grace), boot+r.ProvisionBound()+grace)
	}
	g := newRig(t, nil)
	if got, want := g.r.LaunchBound(), launchBound(g.opts.BootTimeout, g.r.ProvisionBound(), g.opts.TerminationGrace); got != want {
		t.Fatalf("the rig's LaunchBound = %s, want %s", got, want)
	}
}

// countingTokens counts the provisioning-token mints of staticTokens.
type countingTokens struct{ mints *atomic.Int32 }

func (c countingTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	c.mints.Add(1)
	return staticTokens{}.Token(ctx, repo)
}

// countingStore counts the store reads a launch makes (ensureSandbox, provision) over the rig's
// store.
type countingStore struct {
	Store
	reads *atomic.Int32
}

func (s countingStore) IssueRunsElsewhere(ctx context.Context, project, issue, tree string) (string, error) {
	s.reads.Add(1)
	return s.Store.IssueRunsElsewhere(ctx, project, issue, tree)
}

// relaunchCalls never undercounts a replacing try: one that reads every launcher binding of a live
// pod, finds it unbound and replaces it — the longest path a try takes — makes no more bounded
// calls than relaunchCalls counts, every request through the fakes (the informers' lists and
// watches aside), every mint, every store read and every launcher frame together.
func TestAReplacingLaunchMakesNoMoreCallsThanTheBoundCounts(t *testing.T) {
	var mints, renders, reads atomic.Int32
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Tokens = countingTokens{&mints}
		o.GitHubCredential = func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			renders.Add(1)
			return staticCredential(ctx, role, repo)
		}
		o.Store = countingStore{Store: o.Store, reads: &reads}
	}))
	tester := g.spawn(workerSpec(t))
	// The merger's Secret, the last of the pod's six a start reads (launcherBound), binds another
	// pod: the reviewer's start reads every binding, finds the pod unbound, and replaces it.
	editSecret(g, roleSecretName(tester.Sandbox.Name, claim.RoleMerger), func(s *corev1.Secret) {
		s.Annotations[launcherPodUIDAnnotation] = "uid-another-pod"
	})
	g.clearActions()
	mints.Store(0)
	renders.Store(0)
	reads.Store(0)

	reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
	loc := g.spawn(reviewer)
	if loc.Sandbox.PodUID == tester.Sandbox.PodUID {
		t.Fatal("the reviewer's start kept the pod, want it replaced")
	}
	requests := g.requests()
	frames := len(g.sent(reviewer.Claim))
	calls := len(requests) + int(mints.Load()) + int(renders.Load()) + int(reads.Load()) + frames
	t.Logf("the replacing try made %d calls: %d requests, %d mints, %d credential renders, %d store reads, %d launcher frames; relaunchCalls is %d",
		calls, len(requests), mints.Load(), renders.Load(), reads.Load(), frames, relaunchCalls)
	if calls > relaunchCalls {
		var listed []string
		for _, a := range requests {
			listed = append(listed, a.String())
		}
		t.Fatalf("the replacing try made %d bounded calls, more than the %d relaunchCalls counts; its requests:\n%s", calls, relaunchCalls, strings.Join(listed, "\n"))
	}
	if frames != 1 || mints.Load() != 2 || renders.Load() != int32(len(claim.Roles)) {
		t.Fatalf("the replacing try sent %d launcher frames, minted %d tokens and rendered %d credentials, want one Start, two mints (the provisioning token and the repository cache's) and one credential per role",
			frames, mints.Load(), renders.Load())
	}
}
