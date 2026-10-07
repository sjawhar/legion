package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// steps names each write as the relaunch sequence reads: "create sandbox", "suspend", "run",
// "create secret", "update secret", "delete secret", and "record addresses", the role's address
// record written before its generation starts (recordAddresses).
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
		case w.resource == "sandboxes" && w.verb == "patch" && addressRecordPatch(w.patch):
			out = append(out, "record addresses")
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

// A first launch creates the issue Sandbox Suspended, writes the init-only and every role-private
// Secret, then sets it Running. The returned locator addresses only the requested role process in
// the shared pod.
func TestSpawnCreatesTheIssueSandboxAndRoleLocator(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	name := SandboxName(workerToken)
	expectSteps(t, steps(t, g.writes(), name), "create sandbox", "create secret", "run", "record addresses")
	for _, w := range g.writes() {
		if w.verb == "create" && w.resource == "sandboxes" {
			if mode, _, _ := unstructured.NestedString(w.object.(*unstructured.Unstructured).Object, "spec", "operatingMode"); mode != modeSuspended {
				t.Fatalf("created the sandbox %s, want Suspended", mode)
			}
		}
	}
	pod := g.pod(name)
	if err := loc.Validate(); err != nil || loc.Sandbox.PodUID != string(pod.UID) || loc.Runtime != runtime.RuntimeSandbox ||
		loc.Sandbox.Name != name || loc.Sandbox.Container != string(spec.Role) || loc.Sandbox.Generation != spec.Generation ||
		loc.Incarnation != runtime.SandboxIncarnation(string(pod.UID), spec.Generation) || loc.Claim != workerToken {
		t.Fatalf("locator %+v (%v), want the tester process in issue pod uid %s", loc, err, pod.UID)
	}
	provision := g.secret(secretName(name))
	if string(provision.Data[provisionTokenKey]) != "ghs_provision_sjawhar" {
		t.Fatalf("provision secret data %v", provision.Data)
	}
	secret := g.secret(roleSecretName(name, spec.Role))
	if len(secret.Data) != 1 || len(secret.Data[LauncherTokenFile]) == 0 {
		t.Fatalf("role secret keys %v, want the launcher token alone", slices.Sorted(maps.Keys(secret.Data)))
	}
	var start shimwire.LauncherStart
	for _, frame := range g.sent(workerToken) {
		if s, ok := frame.(shimwire.LauncherStart); ok {
			start = s
		}
	}
	if start.Files["ENVOY_TOKEN"] != "envoy-bearer" || start.Files[bootTokenKey] != spec.BootToken {
		t.Fatalf("the tester's start command carries files %v", slices.Sorted(maps.Keys(start.Files)))
	}
	if owner := secret.OwnerReferences; len(owner) != 1 || owner[0].UID != g.sandbox(name).UID || owner[0].Kind != "Sandbox" {
		t.Fatalf("secret owners %+v, want the issue sandbox", owner)
	}
}

func TestRoleStartsShareOneIssuePodAndDoNotReinitializeIt(t *testing.T) {
	g := newRig(t, nil)
	root := g.spawn(rootSpec(t))
	writes := len(g.writes())
	worker := g.spawn(workerSpec(t))
	if root.Sandbox.Name != worker.Sandbox.Name || root.Sandbox.PodUID != worker.Sandbox.PodUID {
		t.Fatalf("role locators root=%+v worker=%+v, want one issue pod", root.Sandbox, worker.Sandbox)
	}
	if root.Sandbox.Container != string(claim.RoleArchitect) || worker.Sandbox.Container != string(claim.RoleTester) {
		t.Fatalf("containers root=%q worker=%q", root.Sandbox.Container, worker.Sandbox.Container)
	}
	for _, write := range g.writes()[writes:] {
		if (write.resource == "sandboxes" && !addressRecordPatch(write.patch)) || write.name == secretName(root.Sandbox.Name) {
			t.Fatalf("worker start rewrote issue pod initialization: %+v", write)
		}
	}
	rootSecret := g.secret(roleSecretName(root.Sandbox.Name, claim.RoleArchitect))
	workerSecret := g.secret(roleSecretName(worker.Sandbox.Name, claim.RoleTester))
	if string(rootSecret.Data[LauncherTokenFile]) == string(workerSecret.Data[LauncherTokenFile]) {
		t.Fatal("role-private launcher Secrets reuse one token")
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
		expectSteps(t, steps(t, g.writes(), name), "create secret", "run", "record addresses")
		if g.sandbox(name).UID != "uid-sandbox-kept" || loc.Sandbox.PodUID != string(g.pod(name).UID) {
			t.Fatalf("locator %+v over sandbox %s", loc, g.sandbox(name).UID)
		}
	})
	t.Run("running a pod no launcher of this runtime is bound to", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{
			sandboxObject(t, name, "uid-sandbox-kept", modeRunning, labels),
			podObject(name, "uid-pod-old", "uid-sandbox-kept", labels, runningStatus()),
		})
		g.suspendDelay = 50 * time.Millisecond
		loc := g.spawn(workerSpec(t))
		expectSteps(t, steps(t, g.writes(), name), "suspend", "create secret", "run", "record addresses")
		if loc.Sandbox.PodUID == "uid-pod-old" || loc.Sandbox.PodUID != string(g.pod(name).UID) {
			t.Fatalf("returned %s; the old pod was uid-pod-old, the pod now is %s", loc.Sandbox.PodUID, g.pod(name).UID)
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
		expectSteps(t, steps(t, g.writes(), name), "create sandbox", "create secret", "run", "record addresses")
		if s := g.sandbox(name); s.UID == "uid-sandbox-going" || deletedAt.IsZero() {
			t.Fatalf("spawned into sandbox %s, before the old one was deleted", s.UID)
		}
		if loc.Sandbox.PodUID != string(g.pod(name).UID) {
			t.Fatalf("locator %+v", loc)
		}
	})
}

// A child of a closed tree re-admitted as a root of its own keeps its key, so its Sandbox's name:
// the Sandbox its old tree suspended mounts the old root's volume and has none of its own. The
// orphan root's launch replaces that Sandbox with one of its own tree, carrying the tree volume.
// One still running roles of the old tree is refused and left as it is.
func TestAnOrphanRootReplacesTheSandboxItsOldTreeLeft(t *testing.T) {
	orphan := claim.Token("legion-legion-legion-209-architect")
	name := SandboxName(orphan)
	oldTree := map[string]string{labelProject: testProject, labelTree: labelValue(testTree), labelIssue: labelValue("LEGION-209")}
	orphanSpec := func(t *testing.T) runtime.SpawnSpec {
		spec := testSpec(t, orphan, claim.RoleArchitect, "LEGION-209")
		spec.Tree = "LEGION-209"
		return spec
	}
	t.Run("suspended by its old tree", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeSuspended, oldTree)})
		g.spawn(orphanSpec(t))
		s := g.sandbox(name)
		if s == nil || s.UID == "uid-sandbox-old-tree" || s.Labels[labelTree] != labelValue("LEGION-209") || len(s.Spec.VolumeClaimTemplates) == 0 {
			t.Fatalf("orphan root's Sandbox = %+v, want a new one of tree LEGION-209 with the tree volume", s)
		}
	})
	t.Run("still running its old tree's roles", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeRunning, oldTree)})
		g.launcher(orphan)
		if _, err := g.r.Spawn(g.ctx, orphanSpec(t)); err == nil || !strings.Contains(err.Error(), "still runs roles") {
			t.Fatalf("orphan root's launch over a running Sandbox of its old tree = %v, want the refusal", err)
		}
		if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-old-tree" {
			t.Fatalf("the old tree's running Sandbox = %+v, want it left as it was", s)
		}
	})
	t.Run("owning its old tree's volume", func(t *testing.T) {
		owner := sandboxObject(t, name, "uid-sandbox-old-root", modeSuspended, oldTree)
		if err := unstructured.SetNestedSlice(owner.Object, []any{map[string]any{"metadata": map[string]any{"name": treeVolume}}}, "spec", "volumeClaimTemplates"); err != nil {
			t.Fatal(err)
		}
		g := newRig(t, []k8sruntime.Object{owner})
		g.launcher(orphan)
		if _, err := g.r.Spawn(g.ctx, orphanSpec(t)); err == nil || !strings.Contains(err.Error(), "only that tree's cleanup deletes it") {
			t.Fatalf("a launch over a Sandbox owning another tree's volume = %v, want the refusal", err)
		}
		if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-old-root" {
			t.Fatalf("the Sandbox owning the old tree's volume = %+v, want it left as it was", s)
		}
	})
}

// The death path's Resume over a Failed issue pod, which the controller keeps under Running:
// suspend, wait the pod out, rewrite the Secrets, run (B1). The role's launcher in the new pod is
// told the new generation's boot token and resumes the recorded session, and the init container is
// told where that session is on the tree volume.
func TestResumeAfterAFailedPod(t *testing.T) {
	name := SandboxName(workerToken)
	labels := claimLabels(claim.RoleTester)
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(name), Namespace: testNamespace, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: "uid-sandbox-kept",
		}}},
		Data: map[string][]byte{provisionTokenKey: []byte("ghs_old")},
	}
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, name, "uid-sandbox-kept", modeRunning, labels),
		podObject(name, "uid-pod-dead", "uid-sandbox-kept", labels, corev1.PodStatus{
			Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 137, "Error")},
		}),
		oldSecret,
	})
	g.suspendDelay = 50 * time.Millisecond
	g.launcher(workerToken)
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	dead := sandboxLocator(workerToken, "uid-pod-dead")
	loc, err := g.r.Resume(g.ctx, &dead, spec)
	if err != nil {
		t.Fatal(err)
	}
	expectSteps(t, steps(t, g.writes(), name), "suspend", "update secret", "run", "record addresses")
	if loc.Sandbox.PodUID == "uid-pod-dead" || loc.Sandbox.PodUID != string(g.pod(name).UID) || loc.Sandbox.Generation != 2 {
		t.Fatalf("resumed at %+v; the pod now is %s", loc.Sandbox, g.pod(name).UID)
	}
	var start shimwire.LauncherStart
	for _, frame := range g.sent(workerToken) {
		if s, ok := frame.(shimwire.LauncherStart); ok {
			start = s
		}
	}
	if start.Generation != 2 || start.Files[bootTokenKey] != "boot-g2" || start.ResumeFile != resumeSession ||
		!slices.Contains(start.Argv, "--resume="+resumeSession) {
		t.Fatalf("the relaunched role's start is generation %d resuming %q with argv %q, want generation 2 resuming the session", start.Generation, start.ResumeFile, start.Argv)
	}
}

// Stage 4b's finished-idle-planner-death: `kill 1` in a role container ends that role's launcher,
// the kubelet restarts the container under restartPolicy Always, and the new launcher connects
// before the pod status drops the killed instance's terminated state. The replacement generation
// is judged by its own launcher, not by that stale status: reading it as dead reported every
// relaunch Gone the moment it started, until the claim's launch budget ran out and the resident
// role never came back.
func TestAReplacementGenerationOutlivesTheKilledInstancesTerminatedStatus(t *testing.T) {
	g := newRig(t, nil)
	name := SandboxName(workerToken)
	killed := g.spawn(workerSpec(t))

	// The kill: the launcher's connection ends with its container, and the kubelet records the exit.
	g.r.launchers.mu.Lock()
	session := g.r.launchers.sessions[workerToken]
	g.r.launchers.mu.Unlock()
	session.close()
	g.update(g.pod(name), func(p *corev1.Pod) {
		for i := range p.Status.ContainerStatuses {
			if p.Status.ContainerStatuses[i].Name == workerContainer {
				p.Status.ContainerStatuses[i] = terminated(workerContainer, 0, "Completed")
			}
		}
	})
	g.eventually("the store to see the killed role container", func() bool {
		status := containerStatus(g.r.storedPod(name), workerContainer)
		return status != nil && status.State.Terminated != nil
	})
	if obs, err := g.r.Probe(g.ctx, killed); err != nil || obs.Kind != runtime.Gone {
		t.Fatalf("the killed generation is %s, want gone: %v", obs.Kind, err)
	}

	// The restarted container's launcher connects while the pod still carries that terminated state.
	g.connect(workerToken)
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	fresh, err := g.r.Resume(g.ctx, &killed, spec)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Sandbox.PodUID != killed.Sandbox.PodUID || fresh.Sandbox.Generation != 2 {
		t.Fatalf("the replacement is %+v, want generation 2 in the issue pod %s", fresh.Sandbox, killed.Sandbox.PodUID)
	}
	obs, err := g.r.Probe(g.ctx, fresh)
	if err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("the replacement generation is %s (%s), want alive: %v", obs.Kind, obs.Detail, err)
	}
}

// The Secrets hold a launch's credentials when the issue Sandbox is set Running, so no launcher
// starts on a missing Secret, and the one-generation boot token travels only in the launcher's
// start command, never in a Secret. A later generation of the role in the same pod sets nothing
// Running again (decision 6).
func TestTheSecretsHoldTheLaunchsCredentialsWhenRunningIsPatched(t *testing.T) {
	g := newRig(t, nil)
	var mu sync.Mutex
	var atRunning []string
	name := SandboxName(workerToken)
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeRunning) {
			mu.Lock()
			defer mu.Unlock()
			provision, role := g.secret(secretName(name)), g.secret(roleSecretName(name, claim.RoleTester))
			switch {
			case provision == nil || role == nil:
				atRunning = append(atRunning, "missing secret")
			case len(provision.Data[provisionTokenKey]) == 0 || len(role.Data[LauncherTokenFile]) == 0:
				atRunning = append(atRunning, "incomplete secrets")
			default:
				atRunning = append(atRunning, "ok")
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
	if strings.Join(atRunning, ", ") != "ok" {
		t.Fatalf("the secrets at each Running patch: %v, want one complete patch", atRunning)
	}
	if first.Sandbox.PodUID != second.Sandbox.PodUID || first.Incarnation == second.Incarnation {
		t.Fatalf("the second generation ran at %s after %s, want a new generation in the same pod", second.Incarnation, first.Incarnation)
	}
	var tokens []string
	for _, frame := range g.sent(workerToken) {
		if s, ok := frame.(shimwire.LauncherStart); ok {
			tokens = append(tokens, s.Files[bootTokenKey])
		}
	}
	if strings.Join(tokens, ", ") != firstToken+", boot-g2" {
		t.Fatalf("the launcher was started with %v", tokens)
	}
	for _, secret := range []string{secretName(name), roleSecretName(name, claim.RoleTester)} {
		for key, value := range g.secret(secret).Data {
			if string(value) == firstToken || string(value) == "boot-g2" {
				t.Fatalf("Secret %s carries a boot token as %s", secret, key)
			}
		}
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
	expectSteps(t, steps(t, g.writes(), name), "create sandbox", "delete secret", "create secret", "run", "record addresses")
	if secret := g.secret(secretName(name)); secret.UID == "uid-secret-old" || secret.OwnerReferences[0].UID != g.sandbox(name).UID {
		t.Fatalf("secret %s owned by %+v", secret.UID, secret.OwnerReferences)
	}
}

// Under gVisor workspace-init's flock stays inside its own pod, so two issue pods of a tree could
// provision the shared clone at once. A relaunch sets an issue pod Running only once no other pod
// of its tree is still in workspace-init (#1258 deep review, finding 2).
func TestAPodOfATreeRunsOnlyOnceNoOtherIsInitializing(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.spawn(rootSpec(t))
	root := SandboxName(rootToken)
	g.launcher(childToken)
	done := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, childSpec(t))
		done <- err
	}()
	child := SandboxName(childToken)
	g.eventually("the child issue's sandbox", func() bool { return g.sandbox(child) != nil })
	time.Sleep(200 * time.Millisecond)
	if got := steps(t, g.writes(), child); slices.Contains(got, "run") {
		t.Fatalf("the child issue's pod was set Running while the root's was still in workspace-init: %v", got)
	}
	g.update(g.pod(root), func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-192-0-2-7", runningStatus() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the child issue never launched after the root's workspace-init finished")
	}
}

// The daemon computes removable-workspace candidates last, under the tree's launch turn, after
// every other pod of the tree has finished initializing (dispatch://LEGION-583): a sibling that
// becomes live while this launch waits out another pod's workspace-init is read as live by the
// time the list is actually built, not as whatever it was before the wait, and its workspace is
// kept off the list the child issue pod's own init container carries.
func TestRemovableWorkspacesAreReadAfterATreesOtherPodsFinishInitializing(t *testing.T) {
	var stillRemovable atomic.Bool
	stillRemovable.Store(true)
	removable := func(context.Context, string, string) ([]runtime.RemovableWorkspace, error) {
		if stillRemovable.Load() {
			return []runtime.RemovableWorkspace{{Issue: "LEGION-999"}}, nil
		}
		return nil, nil
	}
	g := newRig(t, nil, withOptions(func(o *Options) { o.Removable = removable }))
	g.autoStart.Store(false)
	g.spawn(rootSpec(t))
	root := SandboxName(rootToken)
	g.launcher(childToken)

	done := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, childSpec(t))
		done <- err
	}()
	worker := SandboxName(childToken)
	g.eventually("the child issue's sandbox", func() bool { return g.sandbox(worker) != nil })
	time.Sleep(200 * time.Millisecond)
	if got := steps(t, g.writes(), worker); slices.Contains(got, "run") {
		t.Fatalf("the child issue's pod was set Running while the root's was still in workspace-init: %v", got)
	}
	// The sibling becomes live (no longer a candidate) while the worker's launch is still waiting
	// out the root's workspace-init — exactly the window a stale, SpawnSpec-time list would have
	// missed.
	stillRemovable.Store(false)
	g.update(g.pod(root), func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-192-0-2-7", runningStatus() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never launched after the root's workspace-init finished")
	}
	pod := g.pod(worker)
	if pod == nil {
		t.Fatal("the worker's pod does not exist")
	}
	got, set := envOf(containerNamed(t, pod.Spec, initContainer))["LEGION_REMOVABLE_WORKSPACES"]
	if set {
		t.Errorf("the worker's init container carries LEGION_REMOVABLE_WORKSPACES=%q, want none: the sibling became live before the list was built", got)
	}
}

// relaunch stamps notAfter, in LEGION_REMOVABLE_WORKSPACES' own combined JSON payload, at r.now()
// plus the init-wait window (initWaitSeconds), reaching the init container alongside the list
// itself: a pod the Sandbox controller recreates on its own (eviction, node drain, a hand
// deletion) runs workspace-init from this same pod template without the daemon ever taking the
// tree's launch turn again, so this is what bounds how long such a pod may still trust a list
// that could by then be hours old (dispatch://LEGION-583).
func TestRemovableWorkspacesCarryANotAfterTime(t *testing.T) {
	removable := func(context.Context, string, string) ([]runtime.RemovableWorkspace, error) {
		return []runtime.RemovableWorkspace{{Issue: "LEGION-999"}}, nil
	}
	fixed := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	g := newRig(t, nil, withOptions(func(o *Options) { o.Removable = removable; o.Now = func() time.Time { return fixed } }))
	g.spawn(rootSpec(t))

	pod := g.pod(SandboxName(rootToken))
	if pod == nil {
		t.Fatal("the root's pod does not exist")
	}
	env := envOf(containerNamed(t, pod.Spec, initContainer))
	got, set := env["LEGION_REMOVABLE_WORKSPACES"]
	if !set {
		t.Fatal("the init container carries no LEGION_REMOVABLE_WORKSPACES, want the list and its notAfter")
	}
	var payload struct {
		NotAfter time.Time `json:"notAfter"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("LEGION_REMOVABLE_WORKSPACES = %q is not valid JSON: %v", got, err)
	}
	want := fixed.Add(time.Duration(g.r.initWaitSeconds()) * time.Second)
	if !payload.NotAfter.Equal(want) {
		t.Errorf("notAfter = %s, want exactly the fixed launch time plus the init-wait window: %s", payload.NotAfter, want)
	}
}

// A candidate whose issue still has a live, non-terminal pod of this tree is dropped before it is
// ever written to a pod template, regardless of what its claim record says: fail can persist
// StateFailed even when suspendProcess itself errored (dispatch://LEGION-583), and GoneStates()
// cannot tell that case from a claim whose process is in fact gone.
func TestRemovableWorkspacesDropsACandidateWithALiveTreePod(t *testing.T) {
	removable := func(context.Context, string, string) ([]runtime.RemovableWorkspace, error) {
		return []runtime.RemovableWorkspace{{Issue: "LEGION-999"}}, nil
	}
	g := newRig(t, nil, withOptions(func(o *Options) { o.Removable = removable }))
	// A live, non-terminal pod of the tree whose own issue is the candidate's own.
	g.spawn(testSpec(t, claim.Token("legion-legion-legion-999-reviewer"), claim.RoleReviewer, "LEGION-999"))
	live := SandboxName(claim.Token("legion-legion-legion-999-reviewer"))
	g.update(g.pod(live), func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-192-0-2-7", runningStatus() })

	g.spawn(workerSpec(t))
	worker := SandboxName(workerToken)
	pod := g.pod(worker)
	if pod == nil {
		t.Fatal("the worker's pod does not exist")
	}
	got, set := envOf(containerNamed(t, pod.Spec, initContainer))["LEGION_REMOVABLE_WORKSPACES"]
	if set {
		t.Errorf("the worker's init container carries LEGION_REMOVABLE_WORKSPACES=%q, want none: LEGION-999 has a live tree pod", got)
	}
}

// A tree's launch wait (awaitTreeInitialized) is bounded by treeWaitBound, wider than the
// lock-wait budget alone: a sibling issue pod whose workspace-fetch is still cloning outlasts the
// lock-wait budget but finishes inside treeWaitBound, so the waiting launch is not given up on.
// Small BootTimeout/BootIntervals (via withOptions) keep the lock-wait budget well under the 3 s
// this sleeps, so the test stays fast.
func TestATreesLaunchWaitDoesNotGiveUpWhileASiblingsWorkspaceFetchIsStillCloning(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = 300 * time.Millisecond; o.BootIntervals = 1 }))
	g.autoStart.Store(false)
	g.spawn(rootSpec(t))
	root := SandboxName(rootToken)
	g.launcher(childToken)
	done := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, childSpec(t))
		done <- err
	}()
	child := SandboxName(childToken)
	g.eventually("the child issue's sandbox", func() bool { return g.sandbox(child) != nil })

	// Past the old lock-wait-alone bound (2 s here), with the root's workspace-init still
	// running: the child issue's launch must not have given up early.
	time.Sleep(3 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("the child issue's launch finished (%v) before the root's workspace-init did, past the old lock-wait-alone bound — it gave up early", err)
	default:
	}

	g.update(g.pod(root), func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-192-0-2-7", runningStatus() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the child issue never launched after the root's workspace-init finished")
	}
}

// The real-time test above proves the wait outlasts the lock-wait-alone bound; it cannot wait
// out the real ~30-minute bound to prove ProvisionBound's own value is exact. Pin it against a
// literal instead, for this rig's own testOptions (BootTimeout 2 s, BootIntervals 3): the lock
// wait is ceil(2s)×(3+1) = 8s, plus workspace.FetchTimeout (30m).
func TestProvisionBoundIsFetchTimeoutPlusTheLockWaitBudgetExactly(t *testing.T) {
	g := newRig(t, nil)
	want := 30*time.Minute + 8*time.Second
	if got := g.r.ProvisionBound(); got != want {
		t.Fatalf("ProvisionBound = %s, want %s", got, want)
	}
}

// treeWaitBound's own exact value, pinned to a literal: base (2s×3=6s) + ProvisionBound (30m8s,
// TestProvisionBoundIsFetchTimeoutPlusTheLockWaitBudgetExactly) + one more boot interval (2s) =
// 30m16s. runtime.RegistrationDeadline is the one place armRegistration (machine.go) and
// treeWaitBound compute the sibling's own pre-hello deadline, so nothing here needs to compare
// the two independently.
func TestTheTreeWaitBoundIsTheRegistrationDeadlinePlusOneIntervalExactly(t *testing.T) {
	g := newRig(t, nil)
	if want := 30*time.Minute + 16*time.Second; g.r.treeWaitBound() != want {
		t.Fatalf("treeWaitBound = %s, want %s", g.r.treeWaitBound(), want)
	}
}

// Every issue pod of a tree, the root's included, gets the tree's pod affinity exactly when
// another pod of the tree is scheduled at its launch: the tree volume attaches to one node (P2, R1).
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
	g.spawn(rootSpec(t))
	if hasAffinity(rootToken) {
		t.Fatal("the first pod of a tree carries an affinity")
	}
	g.spawn(childSpec(t))
	if !hasAffinity(childToken) {
		t.Fatal("a child issue's pod launched beside the scheduled root carries no affinity")
	}
	// Neither pod scheduled any longer: both issue Sandboxes suspended and their pods gone.
	for _, token := range []claim.Token{rootToken, childToken} {
		s, err := g.r.storedSandbox(SandboxName(token))
		if err != nil || s == nil {
			t.Fatalf("%s's sandbox: %v", token, err)
		}
		if err := g.r.setMode(g.ctx, s, modeSuspended); err != nil {
			t.Fatal(err)
		}
		g.eventually("the pod to leave the store treePodScheduled reads", func() bool { return g.r.storedPod(SandboxName(token)) == nil })
	}
	third := claim.Token("legion-legion-legion-210-reviewer")
	g.spawn(testSpec(t, third, claim.RoleReviewer, "LEGION-210"))
	if hasAffinity(third) {
		t.Fatal("a pod spawned with no other tree pod scheduled carries an affinity")
	}
	spec := rootSpec(t)
	spec.Generation = 2
	g.spawn(spec)
	if !hasAffinity(rootToken) {
		t.Fatal("the root relaunched beside a scheduled issue pod carries no affinity")
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

// A launch returns only once the runtime's Sandbox store holds the Sandbox as its Running patch
// left it: the same generation the API server holds, and Running. With a lagging Sandbox informer
// the new pod reaches its store first, and anything that reads the stores right after the launch
// would otherwise see no Sandbox (a Probe answering Gone), the Suspended copy a first launch creates,
// or, for a relaunch over a Sandbox already Running (a Resume after a death, the
// registration-deadline relaunch, a retry), the Running copy from before the relaunch, which the
// store then replaces with the relaunch's Suspended patch — and a Suspend that found the pod gone
// would read that as already Suspended and write nothing.
func TestALaunchReturnsOnlyOnceTheSandboxStoreHoldsItsRunningPatch(t *testing.T) {
	for _, launches := range []int{1, 2} {
		t.Run(fmt.Sprintf("launch %d", launches), func(t *testing.T) {
			g := newRig(t, nil, withLaggingSandboxInformer(300*time.Millisecond))
			name := SandboxName(workerToken)
			var loc runtime.Locator
			for launch := range launches {
				spec := workerSpec(t)
				if launch > 0 {
					// A relaunch over a Sandbox already Running: its pod died.
					g.update(g.pod(name), func(p *corev1.Pod) {
						p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 137, "Error")}}
					})
					g.eventually("the store to see the pod fail", func() bool { return terminal(g.r.storedPod(name)) })
					spec.Generation = uint64(launch + 1)
				}
				loc = g.spawn(spec)
			}
			stored, err := g.r.storedSandbox(name)
			if err != nil || stored == nil {
				t.Fatalf("the Sandbox store right after Spawn holds no Sandbox (%v)", err)
			}
			api := g.sandbox(name)
			if api.Generation == 0 {
				t.Fatal("the rig's Sandbox patches bumped no generation, so this table cannot tell the Running patch from an earlier copy")
			}
			if stored.Generation != api.Generation || stored.mode() != modeRunning {
				t.Fatalf("the Sandbox store right after Spawn shows it %s at generation %d; the API holds generation %d, Running",
					stored.mode(), stored.Generation, api.Generation)
			}
			if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
				t.Fatalf("Probe right after Spawn: %s %q, %v; want Alive", obs.Kind, obs.Detail, err)
			}
		})
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

// A launch whose pod never comes said only that it timed out, which tells an operator nothing
// about why: the Sandbox controller records that in the Sandbox's own Ready condition, and the
// probe path already reads it. The timeout carries it too, so the operator reads one line instead
// of going to the cluster for the condition the daemon had in its store all along.
func TestALaunchThatWaitsOutItsPodSaysWhatTheSandboxReports(t *testing.T) {
	blocked := metav1.Condition{
		Type: conditionReady, Status: metav1.ConditionFalse, Reason: "PodSchedulingBlocked",
		Message: "no node satisfies the tree's required anti-affinity",
	}
	g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = 300 * time.Millisecond }))
	g.hold.Store(true)
	// The controller reports on the generation it observed, and a condition of an earlier one is
	// not read as this launch's, so the rig writes it as the controller would: after the Running
	// patch, against the generation that patch produced.
	apply := k8stesting.ObjectReaction(g.dyn.Tracker())
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		handled, object, err := apply(a)
		if !handled || err != nil || !modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeRunning) {
			return handled, object, err
		}
		patched := object.(*unstructured.Unstructured)
		s, err := decodeSandbox(patched)
		if err != nil {
			return true, object, err
		}
		blocked.ObservedGeneration = s.Generation
		status := map[string]any{"conditions": []any{map[string]any{
			"type": blocked.Type, "status": string(blocked.Status), "reason": blocked.Reason,
			"message": blocked.Message, "observedGeneration": blocked.ObservedGeneration,
			"lastTransitionTime": metav1.NewTime(rigNow).Format(time.RFC3339),
		}}}
		if err := unstructured.SetNestedMap(patched.Object, status, "status"); err != nil {
			return true, object, err
		}
		return true, patched, g.dyn.Tracker().Update(sandboxGVR, patched, testNamespace)
	})

	_, err := g.r.Spawn(g.ctx, workerSpec(t))

	if err == nil {
		t.Fatal("Spawn returned no error though its pod never came")
	}
	for _, want := range []string{"wait for its new pod", "PodSchedulingBlocked", "no node satisfies the tree's required anti-affinity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Spawn error = %q, want it to name %q", err, want)
		}
	}
}

// A runtime whose API server refuses connections cannot see its Sandboxes, so boot refuses and
// says which store and which request failed. The refusal comes from the informers' own list and
// watch calls, recorded per feed, not from client-go's watch-error handler: the reflector retries
// a refused watch itself without ever calling that handler, so a runtime that trusted it could
// boot with two stores nothing was feeding.
func TestBootRefusesWhenTheAPIServerRefusesTheConnection(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := dead.URL
	dead.Close()
	boot, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := New(boot, &rest.Config{Host: address}, testOptions())

	if err == nil {
		t.Fatal("New returned a runtime though every list its informers made was refused")
	}
	for _, want := range []string{"sandbox runtime", "store may be stale", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New = %q, want it to name %q", err, want)
		}
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

// A step of an issue pod's preparation that fails — the wait for the tree's other pods to finish
// initializing, the retained-sessions read, the removable-workspaces read, the provisioning-token
// mint (a GitHub outage), the provisioning Secret's write — fails that launch with its error and
// nothing more: the launch returns it, never panics, gives the tree's launch turn back, and the
// tree's next launch, once the failure passes, launches.
func TestAFailedStepOfAnIssuePodsPreparationReleasesTheTreesTurn(t *testing.T) {
	failure := errors.New("the step failed")
	for name, tc := range map[string]struct {
		// fail arms the failure for the next launch only; the spec it returns is that launch's.
		fail func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec)
		want string
	}{
		"the wait for the tree's other pods": {
			fail: func(t *testing.T, _ *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				return withOptions(func(*Options) {}), func(g *rig) runtime.SpawnSpec {
					// The root's pod stays in workspace-init, so the child's launch waits for it,
					// until the launch's own deadline (failedLaunch's) ends the wait.
					g.autoStart.Store(false)
					g.spawn(rootSpec(t))
					return childSpec(t)
				}
			},
			want: "wait for its tree's other pods to finish initializing",
		},
		"the retained-sessions read": {
			fail: func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				store := &sessionsFailingStore{treeStore: newTreeStore(), failed: failed, err: failure}
				return withOptions(func(o *Options) { o.Store = store }), func(*rig) runtime.SpawnSpec { return rootSpec(t) }
			},
			want: "read its tree's retained sessions",
		},
		"the removable-workspaces read": {
			fail: func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				removable := func(context.Context, string, string) ([]runtime.RemovableWorkspace, error) {
					if failed.CompareAndSwap(false, true) {
						return nil, failure
					}
					return nil, nil
				}
				return withOptions(func(o *Options) { o.Removable = removable }), func(*rig) runtime.SpawnSpec { return rootSpec(t) }
			},
			want: "compute its tree's removable workspaces",
		},
		"the provisioning-token mint": {
			fail: func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				tokens := &outageTokens{failed: failed}
				return withOptions(func(o *Options) { o.Tokens = tokens }), func(*rig) runtime.SpawnSpec { return rootSpec(t) }
			},
			want: "mint the provisioning token for sjawhar",
		},
		"the provisioning Secret's write": {
			fail: func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				return withOptions(func(*Options) {}), func(g *rig) runtime.SpawnSpec {
					provisioning := secretName(SandboxName(rootToken))
					g.kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
						if a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Name == provisioning && failed.CompareAndSwap(false, true) {
							return true, nil, failure
						}
						return false, nil, nil
					})
					return rootSpec(t)
				}
			},
			want: "write its provisioning secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var failed atomic.Bool
			option, arm := tc.fail(t, &failed)
			g := newRig(t, nil, option)
			spec := arm(g)
			g.launcher(spec.Claim)
			if err := failedLaunch(t, g, spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Spawn = %v, want the failure of %s", err, name)
			}
			taking, cancel := context.WithTimeout(g.ctx, time.Second)
			defer cancel()
			unlock, err := g.r.lockTree(taking, testTree)
			if err != nil {
				t.Fatalf("the tree's launch turn is still held after the failed launch: %v", err)
			}
			unlock()
			if root := g.pod(SandboxName(rootToken)); root != nil && spec.Claim != rootToken {
				g.update(root, func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "ip-192-0-2-7", runningStatus() })
			}
			g.autoStart.Store(true)
			g.spawn(spec)
		})
	}
}

// failedLaunch is a launch of spec expected to fail within a second, its panic turned into the
// test's failure, so a launch that panics fails its own subtest by name rather than the binary.
func failedLaunch(t *testing.T, g *rig, spec runtime.SpawnSpec) error {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Spawn panicked: %v", p)
		}
	}()
	launching, cancel := context.WithTimeout(g.ctx, time.Second)
	defer cancel()
	_, err := g.r.Spawn(launching, spec)
	return err
}

// sessionsFailingStore is a treeStore whose next retained-sessions read fails, once.
type sessionsFailingStore struct {
	*treeStore
	failed *atomic.Bool
	err    error
}

func (s *sessionsFailingStore) TreeHasSessions(ctx context.Context, project, tree string) (bool, error) {
	if s.failed.CompareAndSwap(false, true) {
		return false, s.err
	}
	return s.treeStore.TreeHasSessions(ctx, project, tree)
}

// outageTokens refuses its next installation token, once, as a GitHub outage would.
type outageTokens struct{ failed *atomic.Bool }

func (o *outageTokens) Token(ctx context.Context, owner string) (string, error) {
	if o.failed.CompareAndSwap(false, true) {
		return "", errors.New("no installation token: GitHub is down")
	}
	return staticTokens{}.Token(ctx, owner)
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
