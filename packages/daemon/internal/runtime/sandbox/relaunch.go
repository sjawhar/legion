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
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Spawn starts a fresh agent for spec's claim, over whatever the claim's Sandbox already holds
// (decision 3a): a relaunch before the agent's first registration — the registration deadline, a
// death before registration, a retried launch, boot finishing a launch a crash interrupted — is a
// Spawn over an existing Sandbox. A spec with a ResumeSessionFile is refused: continuing a
// recorded agent is Resume's.
func (r *Runtime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a ResumeSessionFile is Resume's to set", spec.Claim)
	}
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
// it lacks the agent-secrets volumes a role that enrolls now is started against. The first role
// relaunched after either moved replaces it, and its siblings resume into the new pod. Before each
// start the Sandbox records the addresses the generation is handed (recordAddresses). A new pod is
// readied by its kind (podKind.readyNewPod): an issue pod waits its turn among its tree's pods and
// has its workspace's provisioning token minted, the controller's pod needs nothing. A workflow
// claim passed its tree's lifecycle check before the call (supervise's checkLaunch), so the tree's
// cleanup, which waits for every claim of the tree to retire, lists whatever Sandbox this creates.
func (r *Runtime) relaunch(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (loc runtime.Locator, retErr error) {
	l, err := r.prepare(spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	fail := func(step string, err error) (runtime.Locator, error) {
		return runtime.Locator{}, fmt.Errorf("launch %s: %s: %w", spec.Claim, step, err)
	}
	r.forget(spec.Claim)
	// Machine.launch has moved prev out of its claim before calling Resume, and forget above has
	// removed it from this runtime's watch. An error from here must therefore leave no child of this
	// role running unrecorded: clean up its child, or the child an ambiguous start may have begun,
	// on every error. The defer uses retErr only as the error it must return; it never calls a
	// function through a named result or a mutable release function. stopUnrecordedRoleChild discovers
	// the child at return time from the launcher state.
	var (
		pod         *corev1.Pod
		startIssued bool
	)
	defer func() {
		if retErr == nil {
			return
		}
		if stopErr := r.stopUnrecordedRoleChild(context.WithoutCancel(ctx), spec.Claim, prev, pod, startIssued, spec.Generation); stopErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("stop its unrecorded role child: %w", stopErr))
		}
	}()
	release, err := r.lockPod(ctx, l.name)
	if err != nil {
		return fail("take its pod's launch turn", err)
	}
	defer release()
	s, err := r.ensureSandbox(ctx, l)
	if err != nil {
		return fail("ensure its sandbox", err)
	}
	pod = r.storedPod(s.Name)
	_, initExit := failedInit(pod)
	replace := s.mode() == modeSuspended || !ownedBy(pod, s.UID) || terminal(pod) || initExit != nil ||
		slices.ContainsFunc(l.roles, func(role claim.Role) bool { return len(r.movedInPod(pod, role)) > 0 })
	if !replace {
		bound, err := r.launcherBound(ctx, s, pod, l.roles)
		if err != nil {
			return fail("read its pod's launcher bindings", err)
		}
		replace = !bound
	}
	if replace {
		if s.mode() != modeSuspended {
			if err := r.setMode(ctx, s, modeSuspended); err != nil {
				return fail("suspend its sandbox", err)
			}
		}
		old, err := r.awaitPodGone(ctx, s)
		if err != nil {
			return fail("wait out its previous pod", err)
		}
		releaseNewPod, err := l.kind.readyNewPod(ctx, r, &l, s)
		if err != nil {
			return runtime.Locator{}, fmt.Errorf("launch %s: %w", spec.Claim, err)
		}
		defer releaseNewPod()
		if err := r.writeLauncherSecrets(ctx, s, l); err != nil {
			return fail("write its launcher secrets", err)
		}
		template := r.podTemplate(l)
		running, err := r.patch(ctx, s,
			jsonPatchOp{Op: "add", Path: "/spec/podTemplate", Value: template},
			jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeRunning},
		)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("set its sandbox running", err)
		}
		pod, err = r.awaitNewPod(ctx, running, old)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("wait for its new pod", err)
		}
		s = running
		if err := r.bindLauncherSecrets(ctx, s, l, string(pod.UID)); err != nil {
			return fail("bind its role launchers to the new pod", err)
		}
	}
	loc = r.locatorFor(spec.Claim, spec.Role, pod.UID, spec.Generation)
	starting, cancel := context.WithTimeout(ctx, r.bootTimeout+r.terminationGrace)
	defer cancel()
	ended, err := r.awaitRoleLauncher(starting, loc)
	if err != nil {
		return fail("wait for its role launcher", err)
	}
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
	startIssued = true
	if err := r.launchers.start(starting, spec.Claim, string(pod.UID), launcherCommand(l, r)); err != nil {
		return fail("start through its authenticated launcher", err)
	}
	r.join(loc)
	return loc, nil
}

// stopUnrecordedRoleChild stops the child of token's launcher in either pod its failing relaunch
// could have left it: prev's pod, which Machine had recorded before Resume, and pod, the pod this
// relaunch reached or made. Every such child belongs to this role, never a sibling, and is
// unrecorded once relaunch has failed. A stop is bounded even when the launch caller's context has
// expired. When the prior child, or a child a start may have begun, has disconnected first, its stop
// is still attempted at its generation so the returned launch error says the runtime could not end
// it, rather than silently leaving it a zombie.
func (r *Runtime) stopUnrecordedRoleChild(
	ctx context.Context, token claim.Token, prev *runtime.Locator, pod *corev1.Pod, startIssued bool, generation uint64,
) error {
	candidates := map[string]uint64{}
	if prev != nil && prev.Sandbox != nil && (pod == nil || string(pod.UID) == prev.Sandbox.PodUID) {
		candidates[prev.Sandbox.PodUID] = prev.Sandbox.Generation
	}
	if pod != nil && startIssued {
		if _, already := candidates[string(pod.UID)]; !already {
			candidates[string(pod.UID)] = generation
		}
	}
	var errs []error
	for uid, expected := range candidates {
		state, connected := r.launchers.state(token, uid)
		switch {
		case connected && state.Child == nil:
			continue
		case connected && state.Child != nil:
			expected = state.Child.Generation
		}
		stopping, cancel := context.WithTimeout(ctx, r.bootTimeout+r.terminationGrace)
		err := r.launchers.stop(stopping, token, uid, r.stopFrame(expected))
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s generation %d: %w", uid, expected, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runtime) awaitRoleLauncher(ctx context.Context, loc runtime.Locator) (bool, error) {
	ended := false
	err := r.await(ctx, r.bootTimeout, "role launcher "+string(loc.Claim), func() (bool, error) {
		view, err := r.view(loc.Sandbox.Name)
		if err != nil {
			return false, err
		}
		_, initExit := failedInit(view.pod)
		if view.pod == nil || string(view.pod.UID) != loc.Sandbox.PodUID || terminal(view.pod) || initExit != nil {
			ended = true
			return true, nil
		}
		_, ready := r.launchers.state(loc.Claim, loc.Sandbox.PodUID)
		return ready, nil
	})
	return ended, err
}

// suspendFailedLaunch sets the Sandbox of a launch that failed once its Running patch was sent
// Suspended again. The patch may have been applied even when its answer was an error, and a pod the
// controller made later would run a valid token for a claim the supervisor counts as failed. Best
// effort, and not cancelled with the caller: the next relaunch suspends the Sandbox in any case.
func (r *Runtime) suspendFailedLaunch(ctx context.Context, token claim.Token, s *sandbox) {
	if err := r.setMode(context.WithoutCancel(ctx), s, modeSuspended); err != nil {
		r.log.Error("sandbox runtime: could not suspend the sandbox of a failed launch", "claim", token, "err", err)
	}
}

// ensureSandbox is the claim's Sandbox read from the API, created Suspended when absent, with a
// template the Running patch replaces before any pod exists. One being deleted is waited out
// first, bounded by the boot timeout; one by the claim's name that is not this project's is a
// refusal.
//
// A Sandbox fits a launch when it owns its volume (the claim template every Sandbox made now
// carries) and carries the tree label the launch's pod carries, none for the controller's pod. It is
// made for an issue, whose key, so its Sandbox's name, outlives a move to another tree: a child of a
// closed tree re-admitted as a root of its own finds the Sandbox its old tree suspended, labelled
// with that tree and owning the volume that holds the issue's clone, workspace and its roles'
// sessions. Such a Sandbox, once Suspended, is relabelled for this launch's tree and kept, volume
// and sessions with it; the Running patch rewrites its pod template, labels included, before any
// pod runs. One that still runs roles of the old tree is a refusal, since replacing it would end
// them. A Suspended Sandbox that owns no volume — the tree-volume layout, which mounted its tree
// root's — fits no launch and is deleted and made again.
func (r *Runtime) ensureSandbox(ctx context.Context, l launch) (*sandbox, error) {
	deadline := time.Now().Add(r.bootTimeout)
	tree := r.labels(l)[labelTree]
	for {
		getting, cancel := call(ctx)
		u, err := r.sandboxClient().Get(getting, l.name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			manifest, err := encodeSandbox(r.sandboxManifest(l))
			if err != nil {
				return nil, err
			}
			creating, cancel := call(ctx)
			createdObject, err := r.sandboxClient().Create(creating, manifest, metav1.CreateOptions{})
			cancel()
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
			ownsVolume := len(s.Spec.VolumeClaimTemplates) > 0
			if ownsVolume && s.Labels[labelTree] == tree {
				return s, nil
			}
			if s.mode() != modeSuspended {
				volume := ""
				if !ownsVolume {
					volume = " with no volume of its own"
				}
				return nil, fmt.Errorf("sandbox %s, made for tree %s%s, does not fit this launch of tree %s and still runs roles; it is replaced once they stop",
					l.name, s.Labels[labelTree], volume, tree)
			}
			if ownsVolume {
				r.log.Info("sandbox runtime: relabelling a suspended sandbox for its issue's new tree, keeping its volume",
					"sandbox", l.name, "uid", s.UID, "tree", s.Labels[labelTree], "for", tree)
				relabelled, err := r.patch(ctx, s, jsonPatchOp{Op: "add", Path: labelPatchPath(labelTree), Value: tree})
				if err != nil {
					return nil, fmt.Errorf("relabel sandbox %s for tree %s: %w", l.name, tree, err)
				}
				return relabelled, nil
			}
			r.log.Info("sandbox runtime: replacing a sandbox that owns no volume", "sandbox", l.name, "uid", s.UID,
				"tree", s.Labels[labelTree], "for", tree)
			if err := r.deleteSandbox(ctx, u, false); err != nil && !apierrors.IsConflict(err) {
				return nil, err
			}
			continue
		}
		r.log.Info("sandbox runtime: waiting out a sandbox being deleted", "sandbox", l.name, "uid", s.UID)
		gone := func() (bool, error) {
			getting, cancel := call(ctx)
			defer cancel()
			current, err := r.sandboxClient().Get(getting, l.name, metav1.GetOptions{})
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
	gone := func() (bool, error) {
		if pod := r.storedPod(s.Name); ownedBy(pod, s.UID) {
			old[pod.UID] = true
			return false, nil
		}
		getting, cancel := call(ctx)
		defer cancel()
		pod, err := r.kube.CoreV1().Pods(r.namespace).Get(getting, s.Name, metav1.GetOptions{})
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
	err := r.await(ctx, r.bootTimeout, what, func() (bool, error) {
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
// launcher's start command instead (launcherCommand).
func (r *Runtime) writeLauncherSecrets(ctx context.Context, s *sandbox, l launch) error {
	for _, role := range l.roles {
		token, err := launcherToken()
		if err != nil {
			return err
		}
		if err := r.upsertSecret(ctx, corev1.Secret{
			ObjectMeta: r.secretMeta(s, l, roleSecretName(s.Name, role)),
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{LauncherTokenFile: []byte(token)},
		}); err != nil {
			return err
		}
	}
	return nil
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

func (r *Runtime) upsertSecret(ctx context.Context, want corev1.Secret) error {
	ctx, cancel := call(ctx)
	defer cancel()
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

// treePods are the other pods of l's tree in the store.
func (r *Runtime) treePods(l launch) []*corev1.Pod {
	tree := labelValue(l.spec.Tree)
	var pods []*corev1.Pod
	for _, obj := range r.pods.GetStore().List() {
		pod := obj.(*corev1.Pod)
		if pod.Name != l.name && pod.Labels[labelTree] == tree {
			pods = append(pods, pod)
		}
	}
	return pods
}

// withoutLiveTreePods drops any candidate that still has a live, non-terminal pod of l's tree (by
// its legion.dev/issue label): a claim whose fail persisted StateFailed despite its own
// suspendProcess erroring is indistinguishable, in the daemon's own claim store, from one truly
// gone, so the pod itself, not that record, is checked here — a second guarantee on different
// evidence than removableWorkspaces' own candidate rule.
func (r *Runtime) withoutLiveTreePods(l launch, candidates []runtime.RemovableWorkspace) []runtime.RemovableWorkspace {
	live := make(map[string]bool)
	for _, pod := range r.treePods(l) {
		if !terminal(pod) {
			if issueLabel := pod.Labels[labelIssue]; issueLabel != "" {
				live[issueLabel] = true
			}
		}
	}
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if !live[labelValue(candidate.Issue)] {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

// lockTree takes the tree's launch turn: one relaunch of a tree's pods at a time holds it, from
// the check that no other pod of the tree is initializing until its own new pod is in the store,
// where the next relaunch's check sees it.
func (r *Runtime) lockTree(ctx context.Context, tree string) (func(), error) {
	return r.takeTurn(ctx, r.trees, tree)
}

// lockPod takes a pod's launch turn, keyed by its Sandbox's name, an issue pod's and the project
// controller's alike: one relaunch, issue suspension, release or orphan deletion of a pod at a time
// holds it. Role starts in a healthy pod share it only while they select a launcher command; child
// issue pods remain distinct.
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

// treeWaitBound is awaitTreeInitialized's budget: runtime.RegistrationDeadline, called with
// ProvisionBound (the sibling's own pre-hello registration deadline), plus one more boot interval
// of headroom — the same ceil(boot)×(intervals+1)-against-boot×intervals relationship
// workspace-init's own lock wait holds to the registration deadline alone. A named function so a
// test can assert its exact value without waiting it out.
func (r *Runtime) treeWaitBound() time.Duration {
	return runtime.RegistrationDeadline(r.bootTimeout, r.bootIntervals, r.ProvisionBound()) + r.bootTimeout
}

// awaitTreeInitialized waits until no other pod of l's tree is initializing: every tree pod's
// workspace-init container provisions against the one shared clone on the tree volume, and under
// gVisor its flock does not reach past its own pod (each sandbox keeps gofer file locks to
// itself), so the runtime, through which every launch goes, is what keeps two of them from
// provisioning at once. A pod counts as initializing from its start until workspace-init ends, its
// workspace-fetch included. The wait is bounded by treeWaitBound.
func (r *Runtime) awaitTreeInitialized(ctx context.Context, l launch) error {
	return r.await(ctx, r.treeWaitBound(), "the other pods of tree "+l.spec.Tree+" to finish workspace-init", func() (bool, error) {
		for _, pod := range r.treePods(l) {
			if initializing(pod) {
				return false, nil
			}
		}
		return true, nil
	})
}

// initializing excludes failed attempts that Always restarts: the owning claim's recovery
// replaces those pods, and a failed sibling must not hold the tree's provisioning lock forever.
func initializing(pod *corev1.Pod) bool {
	_, initExit := failedInit(pod)
	if terminal(pod) || initExit != nil {
		return false
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == initContainer {
			return status.State.Terminated == nil
		}
	}
	return true
}

// jsonPatchOp is one RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// labelPatchPath is the JSON pointer of one of a Sandbox's labels, its key escaped as RFC 6901
// requires (`~` as `~0`, then `/` as `~1`): an `add` there sets the label, replacing its value.
func labelPatchPath(key string) string {
	return "/metadata/labels/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
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
	patching, cancel := call(ctx)
	defer cancel()
	u, err := r.sandboxClient().Patch(patching, s.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}
