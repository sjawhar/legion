package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
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
// hint: the claim's Sandbox is found by the claim's name, so the relaunch waits out whatever pod
// holds it even when prev is nil, a claim suspended across a daemon restart. The init container
// refuses to start when the session file is missing from the tree volume — a fresh agent on a
// claim that had one is never started.
func (r *Runtime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile == "" {
		return runtime.Locator{}, fmt.Errorf("resume %s: no session file to resume from", spec.Claim)
	}
	if err := (runtime.Known{Claim: spec.Claim, Locator: prev}).Validate(); err != nil {
		return runtime.Locator{}, fmt.Errorf("sandbox runtime: resume: %w", err)
	}
	return r.relaunch(ctx, prev, spec)
}

// relaunch is Spawn's and Resume's one sequence (decision 3a):
//  1. ensure the Sandbox exists, created Suspended when absent, after any deletion in progress;
//  2. stop any current pod: a Sandbox not Suspended is patched Suspended;
//  3. wait out the old pod, until no pod the Sandbox owns holds the name;
//  4. write the claim's Secret, owned by the Sandbox, with a provisioning token minted for it;
//  5. patch the new pod template and `operatingMode: Running` in one write;
//  6. wait for the new pod, and return its uid as the incarnation.
//
// Steps 4 to 6 take the tree's launch turn and wait until no other pod of the tree is in
// workspace-init (awaitTreeInitialized).
func (r *Runtime) relaunch(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	l, err := r.prepare(spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	fail := func(step string, err error) (runtime.Locator, error) {
		return runtime.Locator{}, fmt.Errorf("launch %s: %s: %w", spec.Claim, step, err)
	}
	// This launch replaces whatever process the claim ran, so nothing more is reported about it.
	r.forget(spec.Claim)
	if prev != nil {
		r.log.Info("sandbox runtime: relaunching", "claim", spec.Claim, "previous", prev.Incarnation, "resume", l.resumeFile != "")
	}
	s, created, err := r.ensureSandbox(ctx, l)
	if err != nil {
		return fail("ensure its sandbox", err)
	}
	if !created && s.mode() != modeSuspended {
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fail("suspend its sandbox", err)
		}
	}
	old, err := r.awaitPodGone(ctx, s)
	if err != nil {
		return fail("wait out its previous pod", err)
	}
	release, err := r.lockTree(ctx, l.spec.Tree)
	if err != nil {
		return fail("take its tree's launch turn", err)
	}
	defer release()
	if err := r.awaitTreeInitialized(ctx, l); err != nil {
		return fail("wait for its tree's other pods to finish initializing", err)
	}
	// Minted now, not before the waits: an installation token can be handed out with minutes left.
	// Bounded like an API call, since the tree's launch turn is held while it runs.
	owner, _, _ := strings.Cut(l.repo, "/")
	minting, cancel := call(ctx)
	provisionToken, err := r.tokens.Token(minting, owner)
	cancel()
	if err != nil {
		return fail("mint the provisioning token for "+owner, err)
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
	pod, err := r.awaitNewPod(ctx, running, old)
	if err != nil {
		r.suspendFailedLaunch(ctx, spec.Claim, s)
		return fail("wait for its new pod", err)
	}
	loc := r.locatorFor(spec.Claim, pod.UID)
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

// ensureSandbox is the claim's Sandbox read from the API, created Suspended when absent. One being
// deleted is waited out first, bounded by the boot timeout; one by the claim's name that is not
// this project's is a refusal.
func (r *Runtime) ensureSandbox(ctx context.Context, l launch) (*sandbox, bool, error) {
	deadline := time.Now().Add(r.bootTimeout)
	for {
		getting, cancel := call(ctx)
		u, err := r.sandboxClient().Get(getting, l.name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			manifest, err := encodeSandbox(r.sandboxManifest(l, r.treePodScheduled(l)))
			if err != nil {
				return nil, false, err
			}
			creating, cancel := call(ctx)
			createdObject, err := r.sandboxClient().Create(creating, manifest, metav1.CreateOptions{})
			cancel()
			if apierrors.IsAlreadyExists(err) && time.Now().Before(deadline) {
				continue
			}
			if err != nil {
				return nil, false, err
			}
			created, err := decodeSandbox(createdObject)
			return created, true, err
		}
		if err != nil {
			return nil, false, err
		}
		s, err := decodeSandbox(u)
		if err != nil {
			return nil, false, err
		}
		if s.Labels[labelProject] != r.project {
			return nil, false, fmt.Errorf("sandbox %s exists but is not project %s's (%s=%q)", l.name, r.project, labelProject, s.Labels[labelProject])
		}
		if s.DeletionTimestamp == nil {
			return s, false, nil
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
			return nil, false, err
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
	return found, err
}

// writeSecret makes the claim's Secret hold this launch's boot token, provisioning token, and the
// spec's secrets, owned by the Sandbox so garbage collection deletes it with the Sandbox (decision
// 6). It is written while the Sandbox is Suspended, so no pod ever waits on a missing Secret or
// starts on the previous generation's token. A Secret left owned by an earlier Sandbox of the same
// name is replaced, not updated: the collector may already be deleting it.
func (r *Runtime) writeSecret(ctx context.Context, s *sandbox, l launch, provisionToken string) error {
	ctx, cancel := call(ctx)
	defer cancel()
	data := map[string][]byte{bootTokenKey: []byte(l.spec.BootToken), provisionTokenKey: []byte(provisionToken)}
	for name, value := range l.spec.Secrets {
		data[name] = []byte(value)
	}
	want := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName(s.Name), Namespace: r.namespace, Labels: r.labels(l.spec),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: s.Name, UID: s.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	existing, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case ownedBySandbox(existing.OwnerReferences, s.UID):
		existing.Labels, existing.OwnerReferences, existing.Data, existing.StringData = want.Labels, want.OwnerReferences, want.Data, nil
		_, err := secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	default:
		uid := existing.UID
		if err := secrets.Delete(ctx, want.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil &&
			!apierrors.IsNotFound(err) {
			return err
		}
	}
	_, err = secrets.Create(ctx, &want, metav1.CreateOptions{})
	return err
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

// awaitTreeInitialized waits until no other pod of l's tree is initializing: every init container
// of a tree runs workspace-init against the one shared clone on the tree volume, and under gVisor
// its flock does not reach past its own pod (each sandbox keeps gofer file locks to itself), so
// the runtime, through which every launch goes, is what keeps two of them from provisioning at
// once. The wait is bounded as workspace-init's own lock wait is.
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
// init container has not terminated.
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
