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
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// One launch try is bounded inside the runtime call at LaunchBound, counted from the moment it
// holds its pod's launch turn, Spawn's and Resume's alike (both are relaunch): a try whose launcher
// never answers ends at the bound, not at the boot timeout its launcher wait would have run to,
// with an error naming the bound, and the caller's own context is untouched, so the supervisor
// reads the failure as the runtime's and charges it.
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
			g.r.launchBound = 500 * time.Millisecond
			began := time.Now()
			err := launch(g, workerSpec(t))
			took := time.Since(began)
			want := "wait for its role launcher: context deadline exceeded (the launch try's bound of 500ms ran out)"
			if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s = %v, want %q", name, err, want)
			}
			if took < g.r.launchBound || took >= g.opts.BootTimeout {
				t.Fatalf("the try ended after %s, want about its bound %s and before the boot timeout %s", took, g.r.launchBound, g.opts.BootTimeout)
			}
			if g.ctx.Err() != nil {
				t.Fatalf("the caller's context ended with the try: %v", g.ctx.Err())
			}
		})
	}
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
// three boot timeouts, the launcher backstop, the termination grace, then the calls. At the
// daemon's production defaults — worker_boot_timeout_seconds 120 and worker_stop_timeout_seconds
// 10 (config), and ProvisionBound's 60 minutes (workspace.FetchTimeout + workspace.CloneTimeout) —
// that is 1 h 45 min 41 s with relaunchCalls at 71: well past the 60-minute floor a provisioning pod's rightful wait sets, and
// never under the boot timeout, ProvisionBound and the grace together. The rig's runtime answers
// the same arithmetic over its own options.
func TestLaunchBoundSumsTheTrysWaitsAndCalls(t *testing.T) {
	boot, provision, grace := 120*time.Second, 60*time.Minute, 10*time.Second
	if relaunchCalls != 71 {
		t.Fatalf("relaunchCalls = %d, want 71: %d per try and %d per each of %d roles", relaunchCalls, relaunchPodCalls, relaunchRoleCalls, len(claim.Roles))
	}
	waits := 3*boot + (2*boot + provision + 2*recheckInterval) + grace
	if got, want := launchBound(boot, provision, grace), waits+71*apiTimeout; got != want || want != time.Hour+45*time.Minute+41*time.Second {
		t.Fatalf("launchBound = %s, want %s (1h45m41s)", got, want)
	}
	production := testOptions()
	production.BootTimeout, production.TerminationGrace = boot, grace
	r, err := configure(production)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.LaunchBound(); got != launchBound(boot, provision, grace) || got < boot+r.ProvisionBound()+grace || got <= 60*time.Minute {
		t.Fatalf("LaunchBound at the production defaults = %s, want %s, at least %s and past 60m", got, launchBound(boot, provision, grace), boot+r.ProvisionBound()+grace)
	}
	g := newRig(t, nil)
	if got, want := g.r.LaunchBound(), launchBound(g.opts.BootTimeout, g.r.ProvisionBound(), g.opts.TerminationGrace); got != want {
		t.Fatalf("the rig's LaunchBound = %s, want %s", got, want)
	}
}

// countingTokens counts the provisioning-token mints of staticTokens.
type countingTokens struct{ mints *atomic.Int32 }

func (c countingTokens) Token(ctx context.Context, owner string) (string, error) {
	c.mints.Add(1)
	return staticTokens{}.Token(ctx, owner)
}

// countingStore counts the store reads a launch makes (ensureSandbox, provision) over the rig's
// store.
type countingStore struct {
	Store
	reads *atomic.Int32
}

func (s countingStore) IssueHasSessions(ctx context.Context, project, issue string) (bool, error) {
	s.reads.Add(1)
	return s.Store.IssueHasSessions(ctx, project, issue)
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
		o.GitHubCredential = func(ctx context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
			renders.Add(1)
			return staticCredential(ctx, role, owner)
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
	if frames != 1 || mints.Load() != 1 || renders.Load() != int32(len(claim.Roles)) {
		t.Fatalf("the replacing try sent %d launcher frames, minted %d tokens and rendered %d credentials, want one Start, one mint and one credential per role",
			frames, mints.Load(), renders.Load())
	}
}
