package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
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

// relaunch ensures the issue pod once, then starts only this role's worker-shim through its
// authenticated launcher. An existing healthy issue pod is never suspended or reinitialized when
// another role starts or one role recovers.
func (r *Runtime) relaunch(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	l, err := r.prepare(spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	fail := func(step string, err error) (runtime.Locator, error) {
		return runtime.Locator{}, fmt.Errorf("launch %s: %s: %w", spec.Claim, step, err)
	}
	r.forget(spec.Claim)
	r.own(spec.Claim)
	r.mu.Lock()
	resources, testOnly := r.resourceStore, r.withoutResourceStoreTest
	r.mu.Unlock()
	if resources == nil {
		if !testOnly {
			return fail("load the durable issue resources capability", errors.New("no issue resource store"))
		}
	} else if err := resources.EnsureIssueResources(ctx, r.project, spec.Issue, spec.Tree, l.name, spec.TreeEpoch); err != nil {
		// A reservation of the tree's cleanup refuses at once: no cleanup can finish while this
		// claim is unretired, so waiting would only hold the machine its close needs to stop.
		return fail("record its durable issue resources", err)
	}
	release, err := r.lockIssue(ctx, spec.Issue)
	if err != nil {
		return fail("take its issue pod launch turn", err)
	}
	defer release()
	s, err := r.ensureSandbox(ctx, l)
	if err != nil {
		return fail("ensure its sandbox", err)
	}
	pod := r.storedPod(s.Name)
	if s.mode() == modeSuspended || !ownedBy(pod, s.UID) || terminal(pod) || !r.launcherBound(ctx, s, pod) {
		if s.mode() != modeSuspended {
			if err := r.setMode(ctx, s, modeSuspended); err != nil {
				return fail("suspend its sandbox", err)
			}
		}
		old, err := r.awaitPodGone(ctx, s)
		if err != nil {
			return fail("wait out its previous pod", err)
		}
		treeRelease, err := r.lockTree(ctx, l.spec.Tree)
		if err != nil {
			return fail("take its tree's launch turn", err)
		}
		defer treeRelease()
		if err := r.awaitTreeInitialized(ctx, l); err != nil {
			return fail("wait for its tree's other pods to finish initializing", err)
		}
		minting, cancel := call(ctx)
		provisionToken, err := r.tokens.Token(minting, l.spec.Repository.Owner())
		cancel()
		if err != nil {
			return fail("mint the provisioning token for "+l.spec.Repository.Owner(), err)
		}
		if err := r.writeSecret(ctx, s, l, provisionToken); err != nil {
			return fail("write its secret", err)
		}
		template := r.podTemplate(l, r.treePodScheduled(l))
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
	loc := r.locatorFor(spec.Claim, pod.UID, spec.Generation)
	starting, cancel := context.WithTimeout(ctx, r.bootTimeout+r.terminationGrace)
	defer cancel()
	// A launcher still running an earlier generation of this role runs a process the supervisor has
	// already given up on: it is stopped before the new generation starts, so two generations of a
	// role never run side by side.
	if state, connected := r.launchers.state(spec.Claim); connected && state.Child != nil && state.Child.Generation < spec.Generation {
		stale := state.Child.Generation
		if err := r.launchers.stop(starting, spec.Claim, shimwire.LauncherStop{
			ID: "stop-" + strconv.FormatUint(stale, 10), Generation: stale, GraceMs: int(math.Ceil(r.terminationGrace.Seconds() * 1000)),
		}); err != nil {
			return fail(fmt.Sprintf("stop its earlier generation %d", stale), err)
		}
	}
	if err := r.launchers.start(starting, spec.Claim, launcherCommand(l, r)); err != nil {
		return fail("start through its authenticated launcher", err)
	}
	r.join(loc)
	return loc, nil
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
func (r *Runtime) ensureSandbox(ctx context.Context, l launch) (*sandbox, error) {
	deadline := time.Now().Add(r.bootTimeout)
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
			return s, nil
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

// writeSecret makes the issue's init-only provisioning Secret and, for each of the six fixed
// launchers, a role-private Secret holding a fresh launcher token. It runs only before a new issue
// pod starts, so every pod's launchers authenticate with tokens no earlier pod held; a role's
// launch credentials travel in its launcher's start command instead (launcherCommand).
func (r *Runtime) writeSecret(ctx context.Context, s *sandbox, l launch, provisionToken string) error {
	if err := r.upsertSecret(ctx, corev1.Secret{
		ObjectMeta: r.secretMeta(s, l, secretName(s.Name)),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{provisionTokenKey: []byte(provisionToken)},
	}); err != nil {
		return err
	}
	for _, role := range claim.Roles {
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

func (r *Runtime) secretMeta(s *sandbox, l launch, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: r.namespace, Labels: r.labels(l.spec), Annotations: map[string]string{annotationIssue: l.spec.Issue},
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

// treePodScheduled is whether another pod of l's tree is scheduled now: placed on a node, not
// finished, and not being deleted. A scheduled pod holds the tree volume's attachment from the
// moment it is placed, before any container runs, so readiness would be too late a signal.
func (r *Runtime) treePodScheduled(l launch) bool {
	for _, pod := range r.treePods(l) {
		if pod.Spec.NodeName != "" && !terminal(pod) && pod.DeletionTimestamp == nil {
			return true
		}
	}
	return false
}

// lockTree takes the tree's launch turn: one relaunch of a tree's pods at a time holds it, from
// the check that no other pod of the tree is initializing until its own new pod is in the store,
// where the next relaunch's check sees it.
func (r *Runtime) lockTree(ctx context.Context, tree string) (func(), error) {
	r.mu.Lock()
	turn, ok := r.trees[tree]
	if !ok {
		turn = make(chan struct{}, 1)
		r.trees[tree] = turn
	}
	r.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lockIssue serializes first creation and replacement of one issue pod. Role starts in a healthy
// pod share this lock only while they select a launcher command; child issue pods remain distinct.
func (r *Runtime) lockIssue(ctx context.Context, issue string) (func(), error) {
	r.mu.Lock()
	turn, ok := r.issues[issue]
	if !ok {
		turn = make(chan struct{}, 1)
		r.issues[issue] = turn
	}
	r.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// awaitTreeInitialized waits until no other pod of l's tree is initializing: every tree pod's
// workspace-init container provisions against the one shared clone on the tree volume, and under
// gVisor its flock does not reach past its own pod (each sandbox keeps gofer file locks to
// itself), so the runtime, through which every launch goes, is what keeps two of them from
// provisioning at once. A pod counts as initializing from its start until workspace-init ends, its
// workspace-fetch included. The wait is bounded as workspace-init's own lock wait is.
func (r *Runtime) awaitTreeInitialized(ctx context.Context, l launch) error {
	return r.await(ctx, time.Duration(r.initWaitSeconds())*time.Second, "the other pods of tree "+l.spec.Tree+" to finish workspace-init", func() (bool, error) {
		for _, pod := range r.treePods(l) {
			if initializing(pod) {
				return false, nil
			}
		}
		return true, nil
	})
}

// initializing is whether pod's workspace-init has yet to finish: the pod is not done, and its
// workspace-init container has not terminated.
func initializing(pod *corev1.Pod) bool {
	if terminal(pod) {
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

// patch applies ops to the Sandbox read as s, as one JSON patch whose first operation tests s's
// uid, so a Sandbox deleted and recreated in between is never written. A JSON patch, not a merge
// patch: `add` replaces the pod template whole, where a merge patch would keep every key of the
// previous template the new one leaves out — an affinity among them.
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
