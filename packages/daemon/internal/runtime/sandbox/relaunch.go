package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Spawn starts a fresh agent for spec's claim, over whatever the claim's Sandbox already holds
// (decision 3a): a relaunch before the agent's first registration — the registration deadline, a
// death before registration, a retried launch, boot finishing a launch a crash interrupted — is a
// Spawn over an existing Sandbox. A spec with a ResumeSessionFile is refused: continuing a
// recorded agent is Resume's. spec.Plugins travels with a claim that has registered a session
// (specs.SpawnSpec), which a Spawn's claim never has yet, and is cleared here anyway, so Spawn
// always installs nothing and a fresh pod always loads the image's copies.
func (r *Runtime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a ResumeSessionFile is Resume's to set", spec.Claim)
	}
	spec.Plugins = nil
	return r.relaunch(ctx, nil, spec)
}

// Resume starts the agent spec's claim recorded, again, from spec.ResumeSessionFile. prev is a
// hint: the issue's Sandbox is found by name. A suspended Sandbox waits out its pod before
// restarting; an active issue pod starts only this role. The launcher checks the session file
// before starting the child, so a missing retained session never becomes a fresh agent.
func (r *Runtime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile == "" {
		return runtime.Locator{}, fmt.Errorf("resume %s: no session file to resume from", spec.Claim)
	}
	if err := (runtime.Known{Claim: spec.Claim, Locator: prev}).Validate(); err != nil {
		return runtime.Locator{}, fmt.Errorf("sandbox runtime: resume: %w", err)
	}
	return r.relaunch(ctx, prev, spec)
}

// relaunch ensures the claim's pod once, then starts only this role's worker-shim through its
// authenticated launcher. An existing healthy pod is never suspended or reinitialized when another
// role starts or one role recovers. A pod is not healthy when it cannot run the role as a pod
// created now would: it holds, for one of its roles, something fixed for its life that a pod created
// now is not handed (movedInPod, the rule evaluate reads such a role StaleAddress by): its
// launchers dial a stream other than the one a pod created now is handed, and they never redial, or
// it lacks the agent-secrets volumes a role that enrolls now is started against; or a container of
// it runs an image other than the one this runtime launches every container with (imageDrift) —
// unless the launch is installing a stale-plugin pair into this pod (spec.Plugins set,
// LEGION-164): then drift alone is not a reason to replace it, since the install, not a fresh pod on the
// configured image, is what brings this launch's agent onto the current plugin, and a pod replaced
// for some other reason in the same sweep still is. The first role relaunched after any of these
// replaces it, and its siblings resume into the new pod. Replacing a live pod — its Sandbox
// Running, the pod its own, not ended, its init containers passed — ends the process of every role
// still running in it, wherever its turn stood, so the Suspended patch that replaces it names it
// on the Sandbox in the same write (suspendDisplacing): evaluate then reports each of those roles
// ended by the daemon's replacement (Displaced), which the supervisor resumes uncharged
// (LEGION-729). A pod already ended, or never the Sandbox's, is named nothing. Before each start
// the Sandbox records the addresses the generation is handed (recordAddresses). A new pod is
// provisioned by its kind (podKind.provision), under nothing but the pod's own launch turn: an
// issue pod has its workspace's provisioning token minted, the controller's pod needs nothing. A
// workflow claim passed its tree's lifecycle check before the call (supervise's checkLaunch), so
// the tree's cleanup, which waits for every claim of the tree to retire, lists whatever Sandbox
// this creates. Every Kubernetes request of the launch is sent again while the API server answers
// it overloaded, up to the launch's own deadline for it (launching, resend), so a server under load
// slows the launch rather than failing it.
// The try is bounded on the runtime's own account from the moment it holds the pod's launch turn
// (launchBound, LaunchBound): a try that never answers ends at the bound with an error naming it,
// which the supervisor charges as a launch failure and tries again; one the caller's own context
// ends is not the bound's, and the supervisor charges it nothing. The cleanups of a failed try
// (suspendFailedLaunch, stopIssuedChild) run on the caller's context with bounds of their own,
// since the try's bound may be what ended it.
// On an error the caller owns stopping its previous process (runtime.Runtime's Spawn and Resume);
// relaunch stops only the child it may itself have started: a Start the launcher received but did
// not answer in time (stopIssuedChild). A launch installs its plugin pair (launcherCommand) only
// when spec.Plugins is set and the pod was kept rather than replaced: a pod replaced for any other
// reason — a suspended Sandbox, a lost launcher binding, a failed init, a moved address — comes up
// on the configured image, which the boot gate holds to the daemon's contract, so its copies are
// already right; carrying the install onto a fresh pod would be installing over a pair already
// current. l.spec.Plugins is cleared in that case before launcherCommand builds its argv.
func (r *Runtime) relaunch(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	ctx = launching(ctx)
	l, err := r.prepare(spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	r.forget(spec.Claim)
	release, err := r.lockPod(ctx, l.name)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("launch %s: take its pod's launch turn: %w", spec.Claim, err)
	}
	defer release()
	bounded, cancel := context.WithTimeout(ctx, r.launchBound)
	defer cancel()
	fail := func(step string, err error) (runtime.Locator, error) {
		return runtime.Locator{}, fmt.Errorf("launch %s: %s: %w", spec.Claim, step, boundRanOut(ctx, bounded, r.launchBound, err))
	}
	s, err := r.ensureSandbox(bounded, l)
	if err != nil {
		return fail("ensure its sandbox", err)
	}
	pod := r.storedPod(s.Name)
	_, initExit := failedInit(pod)
	live := s.mode() != modeSuspended && ownedBy(pod, s.UID) && !terminal(pod) && initExit == nil
	_, drifted := imageDrift(pod, r.image)
	installing := l.spec.Plugins != nil
	replace := !live || (drifted && !installing) ||
		slices.ContainsFunc(l.roles, func(role claim.Role) bool { return len(r.movedInPod(pod, role)) > 0 })
	if !replace {
		bound, err := r.launcherBound(bounded, s, pod, &l)
		if err != nil {
			return fail("read its pod's launcher bindings", err)
		}
		replace = !bound
	}
	if installing && replace {
		// Replaced for some other reason: the fresh pod's image is already current, so nothing is
		// installed into it.
		l.spec.Plugins = nil
	}
	if replace {
		switch {
		case live:
			if err := r.suspendDisplacing(bounded, s, pod.UID); err != nil {
				return fail("suspend its sandbox", err)
			}
		case s.mode() != modeSuspended:
			if err := r.setMode(bounded, s, modeSuspended); err != nil {
				return fail("suspend its sandbox", err)
			}
		}
		old, err := r.awaitPodGone(bounded, s)
		if err != nil {
			return fail("wait out its previous pod", err)
		}
		if err := l.kind.provision(bounded, r, &l, s); err != nil {
			return runtime.Locator{}, fmt.Errorf("launch %s: %w", spec.Claim, boundRanOut(ctx, bounded, r.launchBound, err))
		}
		if err := r.writeLauncherSecrets(bounded, s, &l); err != nil {
			return fail("write its launcher secrets", err)
		}
		template := r.podTemplate(l)
		running, err := r.patch(bounded, s,
			jsonPatchOp{Op: "add", Path: "/spec/podTemplate", Value: template},
			jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeRunning},
		)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("set its sandbox running", err)
		}
		pod, err = r.awaitNewPod(bounded, running, old)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("wait for its new pod", err)
		}
		s = running
		if err := r.bindLauncherSecrets(bounded, s, l, string(pod.UID)); err != nil {
			return fail("bind its role launchers to the new pod", err)
		}
	}
	loc := r.locatorFor(spec.Claim, spec.Role, pod.UID, spec.Generation)
	ended, deadline, err := r.awaitRoleLauncher(bounded, loc)
	if err != nil {
		return fail("wait for its role launcher", err)
	}
	starting, cancel := context.WithDeadline(bounded, deadline.Add(r.terminationGrace))
	defer cancel()
	if ended {
		// A real failed init proves no role could start. Return its recorded pod so supervision
		// observes Gone (and WorkspaceLost), rather than charging an unrelated launcher timeout.
		r.join(loc)
		return loc, nil
	}
	// A launcher still running an earlier generation of this role runs a process the supervisor has
	// already given up on: it is stopped before the new generation starts, so two generations of a
	// role never run side by side.
	if state, connected := r.launchers.state(spec.Claim, string(pod.UID)); connected && state.Child != nil && state.Child.Generation < spec.Generation {
		stale := state.Child.Generation
		if err := r.launchers.stop(starting, spec.Claim, string(pod.UID), r.stopFrame(stale)); err != nil {
			return fail(fmt.Sprintf("stop its earlier generation %d", stale), err)
		}
	}
	if err := r.recordAddresses(starting, s, spec.Role, spec.Generation); err != nil {
		return fail("record the addresses its generation is handed", err)
	}
	if err := r.launchers.start(starting, spec.Claim, string(pod.UID), launcherCommand(l, r)); err != nil {
		return fail("start through its authenticated launcher", errors.Join(err, r.stopIssuedChild(ctx, spec.Claim, string(pod.UID), spec.Generation, err)))
	}
	r.join(loc)
	return loc, nil
}

// stopIssuedChild stops generation of token's launcher in podUID after its Start returned startErr,
// unless the launcher answered that Start with a refusal (startRefusal), which started nothing.
// Any other error leaves the Start's outcome unknown: the launcher may have received it and begun
// the generation while its answer was still on its way. The Stop travels after the Start on the
// same connection, so the launcher handles it once it has handled the Start. It runs with its own
// bound, since the start's own deadline may be what ended it.
func (r *Runtime) stopIssuedChild(ctx context.Context, token claim.Token, podUID string, generation uint64, startErr error) error {
	var refused startRefusal
	if errors.As(startErr, &refused) {
		return nil
	}
	stopping, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.bootTimeout+r.terminationGrace)
	defer cancel()
	if err := r.launchers.stop(stopping, token, podUID, r.stopFrame(generation)); err != nil {
		return fmt.Errorf("stop generation %d, which its unanswered Start may have begun: %w", generation, err)
	}
	return nil
}

// boundRanOut is err with a note that the launch try's bound ran out, when it did: the bounded
// context has ended while the caller's own, parent, is alive, and err is a deadline exceeded. Any
// other err is returned as it is — a step's own deadline (apiTimeout's call, the start frame's
// grace), or the caller's context ending, which the supervisor tells apart by that context's own
// error and charges nothing.
func boundRanOut(parent, bounded context.Context, bound time.Duration, err error) error {
	if bounded.Err() != nil && parent.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (the launch try's bound of %s ran out)", err, bound)
	}
	return err
}

// relaunchCalls is the most bounded calls one replacing launch try makes between its waits —
// client requests, each bounded by apiTimeout once the limiter lets it go (requestConfig), the
// mints bounded by call, and the store reads and launcher frames bounded by their context alone,
// the try's — counted generously, every retry and rare branch included, since the bound they size
// (launchBound) must never be shorter than a legitimate try. Per try (relaunchPodCalls):
// ensureSandbox's Get, its Create or relabel patch and that relabel's store read, and for a Sandbox
// being deleted the wait's last Get, past its deadline, and the Get and Create after it (6); the
// suspension patch (1); awaitPodGone's confirming Get, which may begin just before its deadline
// (1); provision's retained-sessions store read, its token mint and its Secret upsert's Get,
// Delete and Create (5), and its repository cache's ensure (cacheEnsureCalls); the Running patch
// (1); the stop of an earlier generation (1); the address record patch (1); the Start frame (1).
// Per launcher role of the pod (relaunchRoleCalls, claim.Roles for an issue pod, the most any pod
// has): launcherBound's Secret Get (1); writeLauncherSecrets' credential mint and its upsert's Get,
// Delete and Create (4); bindLauncherSecrets' Get and Update, and both again on a conflict (4).
var relaunchCalls = relaunchPodCalls + relaunchRoleCalls*len(claim.Roles)

const (
	relaunchPodCalls  = 17 + cacheEnsureCalls
	relaunchRoleCalls = 9
	// cacheEnsureCalls is one ensure of a repository's cache (ensureRepositoryCache) at its most:
	// ensureCacheSandbox's Get and Create, the Get after a Create that found the name taken, and
	// for a Sandbox being deleted the wait's last Get and the Get and Create after it (6);
	// writeCacheSecret's token mint, its Secret's Get, and its upsert's Get, Delete and Create (5);
	// reconcileCachePod's suspension patch, awaitPodGone's confirming Get and the Running patch (3).
	cacheEnsureCalls = 14
)

// launchBound is the longest one launch try may run once it holds its pod's launch turn, from the
// runtime's boot timeout, ProvisionBound and termination grace (LaunchBound): the sum of a
// replacing try's waits — ensureSandbox's wait for a Sandbox being deleted, awaitPodGone and
// awaitNewPod, the boot timeout each; provision's waits on its repository's cache (cacheWaits);
// awaitRoleLauncher's backstop; the termination grace the start frame has past the launcher
// deadline — and of its relaunchCalls calls at apiTimeout each.
func launchBound(bootTimeout, provisionBound, terminationGrace time.Duration) time.Duration {
	waits := 3*bootTimeout + cacheWaits(bootTimeout) + launcherBackstop(bootTimeout, provisionBound) + terminationGrace
	return waits + time.Duration(relaunchCalls)*apiTimeout
}

// cacheWaits is the most a launch try's provision waits on its repository's cache: the cache's
// turn (lockPod on its Sandbox), which at most one other ensure holds ahead of it in the worst case
// — a launch of another of the repository's issues, or the sweep — with that ensure's two waits and
// its cacheEnsureCalls calls, since an ensure after it finds the cache made; then this try's own
// ensure's two waits, ensureCacheSandbox's for a cache Sandbox being deleted and
// reconcileCachePod's awaitPodGone; then awaitCacheService. Each wait is the boot timeout.
func cacheWaits(bootTimeout time.Duration) time.Duration {
	return 2*bootTimeout + time.Duration(cacheEnsureCalls)*apiTimeout + 2*bootTimeout + bootTimeout
}

// launcherBackstop is await's own budget for a role launcher's wait (awaitRoleLauncher), given the
// boot timeout and ProvisionBound: no launcher deadline lies past twice the boot timeout plus
// ProvisionBound from the wait's start (launcherDeadline), and two recheck intervals more let the
// wait's own check end it first.
func launcherBackstop(boot, bound time.Duration) time.Duration {
	return 2*boot + bound + 2*recheckInterval
}

// awaitRoleLauncher waits until loc's role launcher has connected, reporting false, or loc's pod
// has ended, reporting true, and returns the deadline the wait stood at when it stopped. The
// launcher starts only once both of the pod's init containers have finished, and workspace-fetch's
// clone with its retries, then workspace-init's clone of the feed, may take up to ProvisionBound
// from the fetch's own start. So until the wait first sees the pod provisioning (an init container
// running, provisioning) it gives the pod the boot timeout, the image pull's; from then on, the boot
// timeout from the last moment it saw the pod provisioning, never past the boot timeout plus
// ProvisionBound from the first (launcherDeadline). A pod that never provisions past the boot
// timeout waits it alone.
func (r *Runtime) awaitRoleLauncher(ctx context.Context, loc runtime.Locator) (bool, time.Time, error) {
	what := "role launcher " + string(loc.Claim)
	start := time.Now()
	deadline := start.Add(r.bootTimeout)
	var first time.Time
	ended := false
	// await's own budget is only a backstop (launcherBackstop). The check below runs at every
	// informer event and at least every recheckInterval, and it ends the wait before it extends the
	// deadline, so the pod is first seen provisioning within the boot timeout of the start or never,
	// and no deadline lies past twice the boot timeout plus ProvisionBound from the start.
	backstop := launcherBackstop(r.bootTimeout, r.ProvisionBound())
	err := r.await(ctx, backstop, what, func(context.Context) (bool, error) {
		view, err := r.view(loc.Sandbox.Name)
		if serverBusy(err) && time.Now().Before(deadline) {
			// A store whose list or watch the API server answers overloaded is waited out within the
			// wait's deadline, as the launch's own requests are (resend): the informer lists and
			// watches again by itself.
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_, initExit := failedInit(view.pod)
		if view.pod == nil || string(view.pod.UID) != loc.Sandbox.PodUID || terminal(view.pod) || initExit != nil {
			ended = true
			return true, nil
		}
		if _, ready := r.launchers.state(loc.Claim, loc.Sandbox.PodUID); ready {
			return true, nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false, fmt.Errorf("timed out after %s waiting for %s%s", deadline.Sub(start), what, r.stillFetching(view.pod))
		}
		if provisioning(view.pod) {
			if first.IsZero() {
				first = now
			}
			deadline = launcherDeadline(first, now, r.bootTimeout, r.ProvisionBound())
		}
		return false, nil
	})
	return ended, deadline, err
}

// launcherDeadline is when a wait for a role launcher gives up, having first seen its pod
// provisioning at first and last at last: the boot timeout after last, but no later than the boot
// timeout plus bound after first.
func launcherDeadline(first, last time.Time, boot, bound time.Duration) time.Time {
	deadline := last.Add(boot)
	if limit := first.Add(boot + bound); deadline.After(limit) {
		return limit
	}
	return deadline
}

// stillFetching is what a launch that gives up on pod says of it while its workspace-fetch still
// runs: so, with the state of the repository cache that fetch fills a fresh feed from
// (fetchCacheState), since the fetch's own failure, which names why it ran so long, has not come.
// Nothing for a pod past its fetch.
func (r *Runtime) stillFetching(pod *corev1.Pod) string {
	if status := initContainerStatus(pod, fetchContainer); status == nil || status.State.Running == nil {
		return ""
	}
	return "; its " + fetchContainer + " is still running" + r.fetchCacheState(pod)
}

// provisioning is whether pod is running one of its init containers, workspace-fetch or
// workspace-init: the rule the liveness mapping reads such a pod alive by (evaluate).
func provisioning(pod *corev1.Pod) bool {
	if phaseOf(pod) != corev1.PodPending {
		return false
	}
	return slices.ContainsFunc(pod.Status.InitContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.State.Running != nil
	})
}

// suspendFailedLaunch sets the Sandbox of a launch that failed once its Running patch was sent
// Suspended again. The patch may have been applied even when its answer was an error, and a pod the
// controller made later would run a valid token for a claim the supervisor counts as failed. It is
// not cancelled with the caller, and it is bounded by apiTimeout: it is part of the launch, so it
// waits out an API server answering it overloaded (resend), but no longer than that, so a daemon
// stop never waits a boot timeout on it. A suspension that still fails is logged; the next
// relaunch suspends the Sandbox in any case.
func (r *Runtime) suspendFailedLaunch(ctx context.Context, token claim.Token, s *sandbox) {
	suspending, cancel := call(context.WithoutCancel(ctx))
	defer cancel()
	if err := r.setMode(suspending, s, modeSuspended); err != nil {
		r.log.Error("sandbox runtime: could not suspend the sandbox of a failed launch", "claim", token, "err", err)
	}
}

// ensureSandbox is the claim's Sandbox read from the API, created Suspended when absent, with a
// template the Running patch replaces before any pod exists. One being deleted is waited out
// first, bounded by the boot timeout; one by the claim's name that is not this project's is a
// refusal.
//
// A Sandbox fits a launch when it carries the tree label the launch's pod carries, none for the
// controller's pod. It is made for an issue, whose key, so its Sandbox's name, outlives a move to
// another tree: a child of a closed tree re-admitted as a root of its own finds the Sandbox its old
// tree left, labelled with that tree and owning the volume that holds the issue's clone, workspace
// and its roles' sessions. Such a Sandbox is relabelled for this launch's tree and kept, volume and
// sessions with it, once no stored claim of the issue in another tree may still run a process
// there (Store.IssueRunsElsewhere); the Running patch rewrites its pod template, labels included,
// before any pod runs. One its old tree left Running — the old tree's close suspended every role
// of it, but the issue's suspension never ran, since the issue had moved — is suspended in the
// same patch, so the relaunch waits its pod out. One a claim of the old tree may still run in is a
// refusal, since replacing it would end that claim. Nothing here deletes a Sandbox, and none
// without a volume of its own reaches here: every Sandbox this runtime makes carries the issue
// claim template (sandboxManifest), and the boot census refuses one of the tree-volume layout
// before any launch (treeVolumeLayout).
func (r *Runtime) ensureSandbox(ctx context.Context, l launch) (*sandbox, error) {
	deadline := time.Now().Add(r.bootTimeout)
	// refused is the relabel the API server refused as invalid, and refusedAt the resourceVersion its
	// fence tested: a Sandbox read again still at that resourceVersion has not changed since, so the
	// refusal was the patch's own, and it is the answer.
	var refused error
	var refusedAt string
	tree := r.labels(l)[labelTree]
	for {
		u, err := r.sandboxClient().Get(ctx, l.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			manifest, err := encodeSandbox(r.sandboxManifest(l))
			if err != nil {
				return nil, err
			}
			createdObject, err := r.sandboxClient().Create(ctx, manifest, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) && time.Now().Before(deadline) {
				continue
			}
			if err != nil {
				return nil, err
			}
			return decodeSandbox(createdObject)
		}
		if err != nil {
			return nil, err
		}
		s, err := decodeSandbox(u)
		if err != nil {
			return nil, err
		}
		if s.Labels[labelProject] != r.project {
			return nil, fmt.Errorf("sandbox %s exists but is not project %s's (%s=%q)", l.name, r.project, labelProject, s.Labels[labelProject])
		}
		if s.DeletionTimestamp == nil {
			if s.Labels[labelTree] == tree {
				return s, nil
			}
			if refused != nil && s.ResourceVersion == refusedAt {
				return nil, fmt.Errorf("relabel sandbox %s for tree %s: %w", l.name, tree, refused)
			}
			// Fenced to the resourceVersion read, beside the uid patch tests: a relabel, a mode change,
			// or a delete begun since that read fails the test (422), and this launch reads the Sandbox
			// again within its deadline; a delete already completed answers 404 and fails the try.
			ops := []jsonPatchOp{
				{Op: "test", Path: "/metadata/resourceVersion", Value: s.ResourceVersion},
				{Op: "add", Path: labelPatchPath(labelTree), Value: tree},
			}
			if s.mode() != modeSuspended {
				running, err := r.store.IssueRunsElsewhere(ctx, r.project, l.spec.Issue, l.spec.Tree)
				if err != nil {
					return nil, fmt.Errorf("sandbox %s, made for tree %s: %w", l.name, s.Labels[labelTree], err)
				}
				if running != "" {
					return nil, fmt.Errorf("sandbox %s, made for tree %s, does not fit this launch of tree %s and still runs roles (%s); it is relabelled once they stop",
						l.name, s.Labels[labelTree], tree, running)
				}
				ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeSuspended})
			}
			r.log.Info("sandbox runtime: relabelling a sandbox for its issue's new tree, keeping its volume",
				"sandbox", l.name, "uid", s.UID, "tree", s.Labels[labelTree], "for", tree, "operatingMode", s.mode())
			relabelled, err := r.patch(ctx, s, ops...)
			if apierrors.IsInvalid(err) && time.Now().Before(deadline) {
				// The fence's test failed: the Sandbox changed since the read, or this patch, sent
				// again after an overloaded answer (resend), was already applied. Read it again and
				// decide from what it holds now; one still at the tested resourceVersion refused the
				// patch itself.
				refused, refusedAt = err, s.ResourceVersion
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("relabel sandbox %s for tree %s: %w", l.name, tree, err)
			}
			return relabelled, nil
		}
		r.log.Info("sandbox runtime: waiting out a sandbox being deleted", "sandbox", l.name, "uid", s.UID)
		gone := func(ctx context.Context) (bool, error) {
			current, err := r.sandboxClient().Get(ctx, l.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return err == nil && current.GetUID() != s.UID, err
		}
		if err := r.await(ctx, time.Until(deadline), fmt.Sprintf("sandbox %s (uid %s) to be deleted", l.name, s.UID), gone); err != nil {
			return nil, err
		}
	}
}

// awaitPodGone waits, bounded by the boot timeout, until no pod the Sandbox owns holds its name —
// the store first, then one GET to confirm, since a store that has not yet seen a pod proves
// nothing about it (decision 2). It returns every uid it saw holding the name, none of which is
// the incarnation the relaunch will return.
func (r *Runtime) awaitPodGone(ctx context.Context, s *sandbox) (map[types.UID]bool, error) {
	old := map[types.UID]bool{}
	gone := func(ctx context.Context) (bool, error) {
		if pod := r.storedPod(s.Name); ownedBy(pod, s.UID) {
			old[pod.UID] = true
			return false, nil
		}
		pod, err := r.kube.CoreV1().Pods(r.namespace).Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if ownedBy(pod, s.UID) {
			old[pod.UID] = true
			return false, nil
		}
		return true, nil
	}
	err := r.await(ctx, r.bootTimeout, fmt.Sprintf("the pod of sandbox %s to be gone", s.Name), gone)
	return old, err
}

// awaitNewPod waits, bounded by the boot timeout, for a pod the Sandbox owns that is none of old
// and is not being deleted: the pod the Running patch made. s is the Sandbox as that patch left
// it, and the wait also holds until the Sandbox store has reached its generation (the API server
// bumps it on every spec write) and shows it Running, so nothing that reads the stores right
// after the launch — a Probe, a Suspend — sees its pod and not its Sandbox, or any copy from before
// the patch: the Suspended one it was created as, or, for a relaunch over a Running Sandbox, the
// Running one from before the relaunch.
func (r *Runtime) awaitNewPod(ctx context.Context, s *sandbox, old map[types.UID]bool) (*corev1.Pod, error) {
	var found *corev1.Pod
	what := fmt.Sprintf("a new pod of sandbox %s and its Running patch in the Sandbox store", s.Name)
	err := r.await(ctx, r.bootTimeout, what, func(context.Context) (bool, error) {
		pod := r.storedPod(s.Name)
		if !ownedBy(pod, s.UID) || old[pod.UID] || pod.DeletionTimestamp != nil {
			return false, nil
		}
		stored, err := r.storedSandbox(s.Name)
		if err != nil || stored == nil || stored.UID != s.UID {
			return false, err
		}
		if stored.Generation < s.Generation || stored.mode() != modeRunning {
			return false, nil
		}
		found = pod
		return true, nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w; %s", err, r.waitedOut(s))
	}
	return found, err
}

// waitedOut is what the daemon's own stores say about a launch that ran out of time: the Sandbox's
// Ready condition, where the controller reports why it made no pod, and the pod's phase where one
// exists and the wait was for something else. A timeout that says only how long it waited sends an
// operator to the cluster for a condition the daemon was already holding.
func (r *Runtime) waitedOut(s *sandbox) string {
	stored, err := r.storedSandbox(s.Name)
	switch {
	case err != nil:
		return fmt.Sprintf("the Sandbox store could not be read for %s: %v", s.Name, err)
	case stored == nil:
		return fmt.Sprintf("the Sandbox store holds no %s", s.Name)
	}
	detail := fmt.Sprintf("sandbox %s is %s at generation %d", s.Name, stored.mode(), stored.Generation)
	if ready := stored.condition(conditionReady); ready != nil {
		detail += fmt.Sprintf("; Ready=%s %s: %s", ready.Status, ready.Reason, ready.Message)
	}
	if pod := r.storedPod(s.Name); ownedBy(pod, s.UID) {
		return detail + fmt.Sprintf("; its pod %s is %s", pod.UID, phaseOf(pod))
	}
	return detail + "; it has no pod"
}

// writeLauncherSecrets makes, for each launcher role of the pod (l.roles), a role-private Secret
// holding a fresh launcher token. It runs only before a new pod starts, so every pod's launchers
// authenticate with tokens no earlier pod held; a role's launch credentials travel in its
// launcher's start command instead (launcherCommand). The Secret also carries the role's GitHub
// credential, its gh bundle (gitHubBundles): each owner's hosts.yml and config.yml (GitHubHostsKey,
// GitHubConfigKey) and the default owner (GitHubDefaultKey), which its container projects at
// GHConfigDir.
func (r *Runtime) writeLauncherSecrets(ctx context.Context, s *sandbox, l *launch) error {
	bundles, err := r.gitHubBundles(ctx, l)
	if err != nil {
		return err
	}
	for _, role := range l.roles {
		token, err := launcherToken()
		if err != nil {
			return err
		}
		bundle := bundles[role]
		data := map[string][]byte{LauncherTokenFile: []byte(token), GitHubDefaultKey: []byte(bundle.Default)}
		for _, owner := range l.gitHubOwners {
			rendered := bundle.Owners[owner]
			data[GitHubHostsKey(owner)], data[GitHubConfigKey(owner)] = []byte(rendered.Hosts), []byte(rendered.Config)
		}
		if err := r.upsertSecret(ctx, corev1.Secret{
			ObjectMeta: r.secretMeta(s, *l, roleSecretName(s.Name, role)),
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}); err != nil {
			return err
		}
		if bundle.Default == "" {
			r.log.Info("sandbox runtime: no GitHub owner to act for; the role's gh acts as nobody", "sandbox", s.Name, "role", role)
			continue
		}
		written := bundle.Owners[bundle.Default]
		r.log.Info("sandbox runtime: github credential written", "sandbox", s.Name, "role", role, "app", written.App,
			"expiresAt", written.ExpiresAt, "owners", l.gitHubOwners)
	}
	return nil
}

// gitHubBundles mints each launcher role's gh bundle for the issue's repository's owner
// (Options.GitHubCredential) and records the bundle's owners and default on l, which the pod's gh
// volumes and every agent's GH_CONFIG_DIR are built from. Every role's bundle holds the same owners
// and default, and one that does not fails the launch, as a mint that fails or a bundle Validate
// refuses does, naming the role, since a pod whose gh holds no token would start every role unable
// to reach GitHub. The one exception is the controller of a project with no GitHub owner to mint
// for (runtime.ErrNoGitHubOwner, every mapped repository without an App): it runs with gh acting as
// nobody, as `legion controller start` does there, its bundle empty — no owner, no default — and
// its gh directory an empty one (volumes).
func (r *Runtime) gitHubBundles(ctx context.Context, l *launch) (map[claim.Role]ghconfig.Bundle, error) {
	l.gitHubOwners, l.gitHubDefault = nil, ""
	bundles := make(map[claim.Role]ghconfig.Bundle, len(l.roles))
	for i, role := range l.roles {
		minting, cancel := call(ctx)
		bundle, err := r.gitHubCredential(minting, role, l.spec.Repository.Owner())
		cancel()
		switch {
		case role == claim.RoleController && errors.Is(err, runtime.ErrNoGitHubOwner):
			bundle, err = ghconfig.Bundle{}, nil
		case err == nil:
			err = bundle.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("write the github credential for %s: %w", role, err)
		}
		owners := bundle.OwnerNames()
		if i == 0 {
			l.gitHubOwners, l.gitHubDefault = owners, bundle.Default
		} else if !slices.Equal(owners, l.gitHubOwners) || bundle.Default != l.gitHubDefault {
			return nil, fmt.Errorf("write the github credential for %s: its owners %v (default %s) are not the pod's %v (default %s)",
				role, owners, bundle.Default, l.gitHubOwners, l.gitHubDefault)
		}
		bundles[role] = bundle
	}
	return bundles, nil
}

// secretMeta is a pod's Secret's metadata: the pod's labels, the Sandbox as owner, and the
// annotations its kind gives every Secret of the pod (podKind.secretAnnotations).
func (r *Runtime) secretMeta(s *sandbox, l launch, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: r.namespace, Labels: r.labels(l), Annotations: l.kind.secretAnnotations(l),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: s.Name, UID: s.UID,
		}},
	}
}

// upsertSecret writes want, creating it, updating the Sandbox's own Secret by its name, or
// replacing one that is not the Sandbox's (writeSecret). A create refused as already done, or an
// update refused as a conflict, is read and written once more: a launch's write the server applied
// and answered overloaded is sent again (resend) and refused that way, so what it left is read
// before the launch goes on from it, as bindLauncherSecrets reads again after a conflict.
func (r *Runtime) upsertSecret(ctx context.Context, want corev1.Secret) error {
	err := r.writeSecret(ctx, want)
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		err = r.writeSecret(ctx, want)
	}
	return err
}

func (r *Runtime) writeSecret(ctx context.Context, want corev1.Secret) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	existing, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &want, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	case ownedBySandbox(existing.OwnerReferences, want.OwnerReferences[0].UID):
		existing.Labels, existing.OwnerReferences, existing.Data, existing.StringData = want.Labels, want.OwnerReferences, want.Data, nil
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for key, value := range want.Annotations {
			existing.Annotations[key] = value
		}
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	default:
		uid := existing.UID
		if err := secrets.Delete(ctx, want.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil &&
			!apierrors.IsNotFound(err) {
			return err
		}
		_, err = secrets.Create(ctx, &want, metav1.CreateOptions{})
		return err
	}
}

func launcherToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("mint launcher token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func ownedBySandbox(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// lockPod takes a pod's launch turn, keyed by its Sandbox's name, an issue pod's and the project
// controller's alike: one relaunch, issue suspension, release or orphan deletion of a pod at a time
// holds it. Role starts in a healthy pod share it only while they select a launcher command; the
// pods of one tree's issues take turns of their own, never one another's.
func (r *Runtime) lockPod(ctx context.Context, sandbox string) (func(), error) {
	return r.takeTurn(ctx, r.podTurns, sandbox)
}

// takeTurn waits for key's turn in turns, a map r.mu guards, until ctx ends; the returned func
// gives the turn back.
func (r *Runtime) takeTurn(ctx context.Context, turns map[string]chan struct{}, key string) (func(), error) {
	r.mu.Lock()
	turn, ok := turns[key]
	if !ok {
		turn = make(chan struct{}, 1)
		turns[key] = turn
	}
	r.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// jsonPatchOp is one RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// labelPatchEscape escapes a label key as an RFC 6901 JSON pointer token: `~` as `~0`, `/` as `~1`.
var labelPatchEscape = strings.NewReplacer("~", "~0", "/", "~1")

// labelPatchPath is the JSON pointer of one of a Sandbox's labels, its key escaped as RFC 6901
// requires (`~` as `~0`, then `/` as `~1`): an `add` there sets the label, replacing its value.
func labelPatchPath(key string) string {
	return "/metadata/labels/" + labelPatchEscape.Replace(key)
}

// displacedAnnotation names, on a Sandbox, the uid of the last live pod of it the daemon replaced
// (relaunch). A process recorded in that pod ended by the daemon's own act once the pod is going
// (evaluate's displaced). It lives with the Sandbox, so a restarted daemon reads it as well.
const displacedAnnotation = "legion.dev/displaced-pod"

// suspendDisplacing sets the Sandbox read as s Suspended and names uid on it as the live pod that
// suspension replaces (displacedAnnotation), in one JSON merge patch: the API server applies both
// or neither, so a refused suspension names no pod, and a merge patch sets the one annotation key
// whatever other annotations the Sandbox carries, or none. It carries s's uid, which the API server
// refuses to change on a live object, so a Sandbox deleted and recreated in between is never
// written: the fence recordAddresses keeps.
func (r *Runtime) suspendDisplacing(ctx context.Context, s *sandbox, uid types.UID) error {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": string(s.UID), "annotations": map[string]string{displacedAnnotation: string(uid)}},
		"spec":     map[string]any{"operatingMode": modeSuspended},
	})
	if err != nil {
		return err
	}
	_, err = r.sandboxClient().Patch(ctx, s.Name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// patch applies ops to the Sandbox read as s, as one JSON patch whose first operation tests s's
// uid, so a Sandbox deleted and recreated in between is never written. A JSON patch, not a merge
// patch: `add` replaces the pod template whole, where a merge patch would keep every key of the
// previous template the new one leaves out.
func (r *Runtime) patch(ctx context.Context, s *sandbox, ops ...jsonPatchOp) (*sandbox, error) {
	body, err := json.Marshal(append([]jsonPatchOp{{Op: "test", Path: "/metadata/uid", Value: string(s.UID)}}, ops...))
	if err != nil {
		return nil, err
	}
	u, err := r.sandboxClient().Patch(ctx, s.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}
