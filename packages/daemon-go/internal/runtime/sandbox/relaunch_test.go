package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// steps names each write as the relaunch sequence reads: "create sandbox", "suspend", "run",
// "create secret", "update secret", "delete secret".
func steps(t *testing.T, writes []action, sandboxName string) []string {
	t.Helper()
	var out []string
	for _, w := range writes {
		if w.name != sandboxName && w.name != secretName(sandboxName) {
			continue
		}
		switch {
		case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeRunning):
			out = append(out, "run")
		case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeSuspended):
			out = append(out, "suspend")
		case w.resource == "sandboxes":
			out = append(out, w.verb+" sandbox")
		case w.resource == "secrets":
			out = append(out, w.verb+" secret")
		}
	}
	return out
}

func expectSteps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("writes: %s\nwant:   %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

// A first launch creates the Sandbox Suspended, writes its Secret, and only then sets it Running;
// the incarnation it returns is the pod the Sandbox then owns.
func TestSpawnCreatesTheSandboxSuspendedAndRunsItAfterItsSecret(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	name := SandboxName(workerToken)
	expectSteps(t, steps(t, g.writes(), name), "create sandbox", "create secret", "run")
	for _, w := range g.writes() {
		if w.verb == "create" && w.resource == "sandboxes" {
			if mode, _, _ := unstructured.NestedString(w.object.(*unstructured.Unstructured).Object, "spec", "operatingMode"); mode != modeSuspended {
				t.Fatalf("created the sandbox %s, want Suspended", mode)
			}
		}
	}
	pod := g.pod(name)
	if err := loc.Validate(); err != nil || loc.Incarnation != string(pod.UID) || loc.Runtime != runtime.RuntimeSandbox ||
		loc.Sandbox.Name != name || loc.Claim != workerToken {
		t.Fatalf("locator %+v (%v), want the claim's sandbox at pod uid %s", loc, err, pod.UID)
	}
	secret := g.secret(secretName(name))
	if string(secret.Data[bootTokenKey]) != spec.BootToken || string(secret.Data[provisionTokenKey]) != "ghs_provision_sjawhar" ||
		string(secret.Data["ENVOY_TOKEN"]) != "envoy-bearer" {
		t.Fatalf("secret data %v", secret.Data)
	}
	if owner := secret.OwnerReferences; len(owner) != 1 || owner[0].UID != g.sandbox(name).UID || owner[0].Kind != "Sandbox" {
		t.Fatalf("secret owners %+v, want the sandbox", owner)
	}
}

// Every relaunch before an agent registers — the deadline, a death, a retry, boot finishing an
// interrupted launch — is a Spawn over a Sandbox that already exists (B2).
func TestSpawnOverAnExistingSandbox(t *testing.T) {
	name := SandboxName(workerToken)
	labels := claimLabels(claim.RoleTester)
	t.Run("suspended", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-kept", modeSuspended, labels)})
		loc := g.spawn(workerSpec(t))
		expectSteps(t, steps(t, g.writes(), name), "create secret", "run")
		if g.sandbox(name).UID != "uid-sandbox-kept" || loc.Incarnation != string(g.pod(name).UID) {
			t.Fatalf("locator %+v over sandbox %s", loc, g.sandbox(name).UID)
		}
	})
	t.Run("running", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{
			sandboxObject(t, name, "uid-sandbox-kept", modeRunning, labels),
			podObject(name, "uid-pod-old", "uid-sandbox-kept", labels, runningStatus()),
		})
		g.suspendDelay = 50 * time.Millisecond
		loc := g.spawn(workerSpec(t))
		expectSteps(t, steps(t, g.writes(), name), "suspend", "create secret", "run")
		if loc.Incarnation == "uid-pod-old" || loc.Incarnation != string(g.pod(name).UID) {
			t.Fatalf("returned %s; the old pod was uid-pod-old, the pod now is %s", loc.Incarnation, g.pod(name).UID)
		}
	})
	t.Run("being deleted", func(t *testing.T) {
		deleting := sandboxObject(t, name, "uid-sandbox-going", modeRunning, labels)
		now := metav1.NewTime(rigNow)
		deleting.SetDeletionTimestamp(&now)
		deleting.SetFinalizers([]string{"foregroundDeletion"})
		g := newRig(t, []k8sruntime.Object{deleting})
		var deletedAt time.Time
		go func() {
			time.Sleep(150 * time.Millisecond)
			deletedAt = time.Now()
			_ = g.dyn.Tracker().Delete(sandboxGVR, testNamespace, name)
		}()
		loc := g.spawn(workerSpec(t))
		expectSteps(t, steps(t, g.writes(), name), "create sandbox", "create secret", "run")
		if s := g.sandbox(name); s.UID == "uid-sandbox-going" || deletedAt.IsZero() {
			t.Fatalf("spawned into sandbox %s, before the old one was deleted", s.UID)
		}
		if loc.Incarnation != string(g.pod(name).UID) {
			t.Fatalf("locator %+v", loc)
		}
	})
}

// The death path's Resume over a Failed pod, which the controller keeps under Running: suspend,
// wait the pod out, rewrite the Secret, run (B1). The relaunched pod resumes the recorded session
// and its init container is told where that session is on the tree volume.
func TestResumeAfterAFailedPod(t *testing.T) {
	name := SandboxName(workerToken)
	labels := claimLabels(claim.RoleTester)
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(name), Namespace: testNamespace, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: "uid-sandbox-kept",
		}}},
		Data: map[string][]byte{bootTokenKey: []byte("boot-g1")},
	}
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, name, "uid-sandbox-kept", modeRunning, labels),
		podObject(name, "uid-pod-dead", "uid-sandbox-kept", labels, corev1.PodStatus{
			Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(mainContainer, 137, "Error")},
		}),
		oldSecret,
	})
	g.suspendDelay = 50 * time.Millisecond
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	loc, err := g.r.Resume(g.ctx, sandboxLocator(workerToken, "uid-pod-dead"), spec)
	if err != nil {
		t.Fatal(err)
	}
	expectSteps(t, steps(t, g.writes(), name), "suspend", "update secret", "run")
	if loc.Incarnation == "uid-pod-dead" || loc.Incarnation != string(g.pod(name).UID) {
		t.Fatalf("resumed at %s; the pod now is %s", loc.Incarnation, g.pod(name).UID)
	}
	if token := string(g.secret(secretName(name)).Data[bootTokenKey]); token != "boot-g2" {
		t.Fatalf("the secret holds %q, want the new generation's token", token)
	}
	pod := g.pod(name)
	if !strings.Contains(strings.Join(pod.Spec.Containers[0].Command, " "), "--resume="+resumeSession) {
		t.Fatalf("the relaunched agent does not resume the session: %v", pod.Spec.Containers[0].Command)
	}
	if got := envOf(pod.Spec.InitContainers[0])["LEGION_RESUME_SESSION_FILE"]; got != TreeRoot+"/sessions/"+strings.TrimPrefix(resumeSession, ompSessionsDir+"/") {
		t.Fatalf("the init container checks %q", got)
	}
}

// The Secret holds this launch's tokens at the moment the Sandbox is set Running, so the pod never
// starts on a missing Secret or on the previous generation's token (decision 6).
func TestTheSecretHoldsTheLaunchsTokensWhenRunningIsPatched(t *testing.T) {
	g := newRig(t, nil)
	var mu sync.Mutex
	var atRunning []string
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeRunning) {
			mu.Lock()
			defer mu.Unlock()
			secret := g.secret(secretName(SandboxName(workerToken)))
			if secret == nil {
				atRunning = append(atRunning, "no secret")
			} else {
				atRunning = append(atRunning, string(secret.Data[bootTokenKey]))
			}
		}
		return false, nil, nil
	})
	spec := workerSpec(t)
	firstToken := spec.BootToken
	first := g.spawn(spec)
	if err := g.r.Suspend(g.ctx, first); err != nil {
		t.Fatal(err)
	}
	spec.Generation, spec.BootToken = 2, "boot-g2"
	second := g.spawn(spec)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(atRunning, ", ") != firstToken+", boot-g2" {
		t.Fatalf("the secret held %v as each Running patch was sent", atRunning)
	}
	if first.Incarnation == second.Incarnation {
		t.Fatal("the respawn returned the first incarnation")
	}
}

// A Secret left by an earlier Sandbox of the same name — one released while its garbage was still
// being collected — is replaced, never updated, so the collector cannot delete the new one.
func TestASecretOwnedByAnEarlierSandboxIsReplaced(t *testing.T) {
	name := SandboxName(workerToken)
	g := newRig(t, []k8sruntime.Object{&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(name), Namespace: testNamespace, UID: "uid-secret-old", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: "uid-sandbox-released",
		}}},
	}})
	g.spawn(workerSpec(t))
	expectSteps(t, steps(t, g.writes(), name), "create sandbox", "delete secret", "create secret", "run")
	if secret := g.secret(secretName(name)); secret.UID == "uid-secret-old" || secret.OwnerReferences[0].UID != g.sandbox(name).UID {
		t.Fatalf("secret %s owned by %+v", secret.UID, secret.OwnerReferences)
	}
}

// Under gVisor workspace-init's flock stays inside its own pod, so two pods of a tree could
// provision the shared clone at once. A relaunch sets a tree pod Running only once no other pod of
// its tree is still in workspace-init (#1258 deep review, finding 2).
func TestAPodOfATreeRunsOnlyOnceNoOtherIsInitializing(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.spawn(rootSpec(t))
	root := SandboxName(rootToken)
	done := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, workerSpec(t))
		done <- err
	}()
	worker := SandboxName(workerToken)
	g.eventually("the worker's sandbox", func() bool { return g.sandbox(worker) != nil })
	time.Sleep(200 * time.Millisecond)
	if got := steps(t, g.writes(), worker); slicesContain(got, "run") {
		t.Fatalf("the worker was set Running while the root was still in workspace-init: %v", got)
	}
	g.update(g.pod(root), func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-10-1-40-7", runningStatus() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never launched after the root's workspace-init finished")
	}
}

func slicesContain(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Every pod of a tree, the root included, gets the tree's pod affinity exactly when another pod of
// the tree is scheduled at its launch: the tree volume attaches to one node (P2, R1).
func TestTheTreeAffinityFollowsTheTreesScheduledPods(t *testing.T) {
	g := newRig(t, nil)
	hasAffinity := func(token claim.Token) bool {
		pod := g.pod(SandboxName(token))
		if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil {
			return false
		}
		term := pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		return len(term) == 1 && term[0].TopologyKey == corev1.LabelHostname && term[0].LabelSelector.MatchLabels[labelTree] == testTree
	}
	root := g.spawn(rootSpec(t))
	if hasAffinity(rootToken) {
		t.Fatal("the first pod of a tree carries an affinity")
	}
	worker := g.spawn(workerSpec(t))
	if !hasAffinity(workerToken) {
		t.Fatal("a worker spawned beside the scheduled root carries no affinity")
	}
	for _, loc := range []runtime.Locator{worker, root} {
		if err := g.r.Suspend(g.ctx, loc); err != nil {
			t.Fatal(err)
		}
		g.eventually("the pod to go", func() bool { return g.pod(loc.Sandbox.Name) == nil })
	}
	g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if hasAffinity(otherToken) {
		t.Fatal("a pod spawned with no other tree pod scheduled carries an affinity")
	}
	g.spawn(rootSpec(t))
	if !hasAffinity(rootToken) {
		t.Fatal("the root relaunched beside a scheduled worker carries no affinity")
	}
}

// A launch whose Sandbox was replaced between reading it and writing it is refused rather than
// written: every Sandbox patch tests the uid it was read at.
func TestAPatchIsFencedToTheSandboxItRead(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(workerSpec(t))
	name := SandboxName(workerToken)
	read := g.sandbox(name)
	replacement := *read
	replacement.UID = types.UID("uid-sandbox-replacement")
	u, err := encodeSandbox(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
	if err := g.r.setMode(g.ctx, read, modeSuspended); err == nil {
		t.Fatal("a patch fenced to another uid was applied")
	}
	if g.sandbox(name).mode() != modeRunning {
		t.Fatal("the replaced sandbox was written")
	}
}

// A launch returns only once the Sandbox store holds the Sandbox it launched into: with a lagging
// Sandbox informer, a first Spawn would otherwise read its own Sandbox as absent — a Probe Gone,
// a Suspend that finds nothing to suspend.
func TestALaunchReturnsOnlyOnceTheSandboxStoreHoldsItsSandbox(t *testing.T) {
	g := newRig(t, nil, withLaggingSandboxInformer(300*time.Millisecond))
	loc := g.spawn(workerSpec(t))
	obs, err := g.r.Probe(g.ctx, loc)
	if err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe right after Spawn: %s %q, %v; want Alive", obs.Kind, obs.Detail, err)
	}
}

// A launch that fails after setting its Sandbox Running sets it Suspended again before returning:
// the claim is a launch failure now, and a pod the controller created later would run a valid
// token for a claim nothing supervises.
func TestALaunchThatFailsAfterRunningLeavesItsSandboxSuspended(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = 300 * time.Millisecond }))
	g.hold.Store(true)
	if _, err := g.r.Spawn(g.ctx, workerSpec(t)); err == nil || !strings.Contains(err.Error(), "wait for its new pod") {
		t.Fatalf("Spawn: %v, want the new pod's timeout", err)
	}
	name := SandboxName(workerToken)
	expectSteps(t, steps(t, g.writes(), name), "create sandbox", "create secret", "run", "suspend")
	if mode := g.sandbox(name).mode(); mode != modeSuspended {
		t.Fatalf("the failed launch left its sandbox %s", mode)
	}
}

// deadlineTokens records whether each mint's context carries a deadline.
type deadlineTokens struct{ bounded []bool }

func (d *deadlineTokens) Token(ctx context.Context, owner string) (string, error) {
	_, ok := ctx.Deadline()
	d.bounded = append(d.bounded, ok)
	return "ghs_provision_" + owner, nil
}

// The provisioning token is minted inside the tree's launch turn, so its mint is bounded like any
// other API call: a stalled GitHub connection must not hold every launch of the tree.
func TestTheProvisioningTokenMintIsBounded(t *testing.T) {
	tokens := &deadlineTokens{}
	g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = tokens }))
	g.spawn(workerSpec(t))
	if len(tokens.bounded) != 1 || !tokens.bounded[0] {
		t.Fatalf("mints bounded: %v, want one with a deadline", tokens.bounded)
	}
}

// A Running patch the server applied but whose answer never arrived (a client timeout) is as
// ambiguous as a pod that never came: the launch sets its Sandbox Suspended before returning.
func TestALaunchWhoseRunningPatchTimesOutAfterApplyingLeavesItsSandboxSuspended(t *testing.T) {
	g := newRig(t, nil)
	g.hold.Store(true)
	applyThenTimeOut := k8stesting.ObjectReaction(g.dyn.Tracker())
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if !modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeRunning) {
			return false, nil, nil
		}
		if _, _, err := applyThenTimeOut(a); err != nil {
			t.Errorf("apply the Running patch: %v", err)
		}
		return true, nil, fmt.Errorf("the client gave up waiting for the answer: %w", context.DeadlineExceeded)
	})
	if _, err := g.r.Spawn(g.ctx, workerSpec(t)); err == nil || !strings.Contains(err.Error(), "set its sandbox running") {
		t.Fatalf("Spawn: %v, want the Running patch's timeout", err)
	}
	if mode := g.sandbox(SandboxName(workerToken)).mode(); mode != modeSuspended {
		t.Fatalf("the launch left its sandbox %s after an ambiguous Running patch", mode)
	}
}
