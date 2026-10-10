package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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

// A child of a closed tree re-admitted as a root of its own keeps its key, so its Sandbox's name,
// and finds the Sandbox its old tree suspended, still labelled with that tree. The Sandbox owns the
// issue's volume, which holds its clone, workspace and its roles' sessions, so the orphan root's
// launch keeps it, relabelled for its own tree, and its pod mounts the same claim. One still running
// roles of the old tree is refused and left as it is.
func TestAnOrphanRootKeepsTheSandboxAndVolumeItsOldTreeLeft(t *testing.T) {
	orphan := claim.Token("legion-legion-legion-209-architect")
	name := SandboxName(orphan)
	oldTree := map[string]string{labelProject: testProject, labelTree: labelValue(testTree), labelIssue: labelValue("LEGION-209")}
	orphanSpec := func(t *testing.T) runtime.SpawnSpec {
		spec := testSpec(t, orphan, claim.RoleArchitect, "LEGION-209")
		spec.Tree = "LEGION-209"
		return spec
	}
	t.Run("suspended by its old tree, owning its volume", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeSuspended, oldTree)})
		loc := g.spawn(orphanSpec(t))
		if deleted := slices.ContainsFunc(g.writes(), func(a action) bool { return a.resource == "sandboxes" && a.verb == "delete" }); deleted {
			t.Fatalf("the orphan root's launch deleted a Sandbox: %v", g.writes())
		}
		s := g.sandbox(name)
		if s == nil || s.UID != "uid-sandbox-old-tree" || s.Labels[labelTree] != labelValue("LEGION-209") || s.Labels[labelIssue] != labelValue("LEGION-209") {
			t.Fatalf("orphan root's Sandbox = %+v, want the old tree's Sandbox kept and relabelled for tree LEGION-209", s)
		}
		if len(s.Spec.VolumeClaimTemplates) != 1 || s.Spec.VolumeClaimTemplates[0].Metadata.Name != issueVolume {
			t.Fatalf("orphan root's Sandbox claims %+v, want the issue volume it owned kept", s.Spec.VolumeClaimTemplates)
		}
		if got := s.Spec.PodTemplate.Metadata.Labels[labelTree]; got != labelValue("LEGION-209") {
			t.Fatalf("the relabelled Sandbox's pod template carries tree %q, want LEGION-209", got)
		}
		pod := g.pod(name)
		if pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
			t.Fatalf("pod %+v, locator %+v; want the launch's pod", pod, loc)
		}
		if pod.Labels[labelTree] != labelValue("LEGION-209") {
			t.Fatalf("the new pod carries tree %q, want LEGION-209", pod.Labels[labelTree])
		}
		mounted := slices.IndexFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == issueVolume })
		if mounted < 0 || pod.Spec.Volumes[mounted].PersistentVolumeClaim == nil || pod.Spec.Volumes[mounted].PersistentVolumeClaim.ClaimName != IssueClaimName(orphan) {
			t.Fatalf("the new pod's volumes %+v, want the issue's own claim %s", pod.Spec.Volumes, IssueClaimName(orphan))
		}
	})
	t.Run("still running its old tree's roles", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeRunning, oldTree)})
		g.launcher(orphan)
		if _, err := g.r.Spawn(g.ctx, orphanSpec(t)); err == nil || !strings.Contains(err.Error(), "still runs roles") {
			t.Fatalf("orphan root's launch over a running Sandbox of its old tree = %v, want the refusal", err)
		}
		if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-old-tree" || s.Labels[labelTree] != labelValue(testTree) {
			t.Fatalf("the old tree's running Sandbox = %+v, want it left as it was", s)
		}
	})
}

// The death path's Resume over a Failed issue pod, which the controller keeps under Running:
// suspend, wait the pod out, rewrite the Secrets, run (B1). The role's launcher in the new pod is
// told the new generation's boot token and resumes the recorded session, and the init container is
// told where that session is on the issue's volume.
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

// Each issue pod provisions its own clone on its own volume, so a launch of one issue never waits
// on another pod of its tree: a child issue's pod is set Running while the root's is still in
// workspace-init.
func TestAPodOfATreeLaunchesWhileAnotherIsStillInitializing(t *testing.T) {
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
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the child issue's launch waited on the root's workspace-init")
	}
	if pod := g.pod(root); pod == nil || pod.Status.Phase != corev1.PodPending {
		t.Fatalf("the root's pod is %+v, want it still Pending in workspace-init", pod)
	}
	if got := steps(t, g.writes(), SandboxName(childToken)); !slices.Contains(got, "run") {
		t.Fatalf("the child issue's writes are %v, want its pod set Running", got)
	}
}

// ProvisionBound is the fetch's own clone bound, workspace.FetchTimeout (30m), and nothing more:
// provisioning from the feed onto the issue's own volume waits on no other pod, so no lock wait is
// added to it. Pinned to a literal, since no test can wait the real bound out.
func TestProvisionBoundIsTheFetchTimeoutExactly(t *testing.T) {
	g := newRig(t, nil)
	if got := g.r.ProvisionBound(); got != 30*time.Minute {
		t.Fatalf("ProvisionBound = %s, want %s", got, 30*time.Minute)
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
		Message: "0/3 nodes are available: 3 Insufficient cpu.",
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
	for _, want := range []string{"wait for its new pod", "PodSchedulingBlocked", "0/3 nodes are available: 3 Insufficient cpu."} {
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

// The provisioning token is minted under the pod's launch turn, so its mint is bounded like any
// other API call: a stalled GitHub connection must not hold the pod's turn for good.
func TestTheProvisioningTokenMintIsBounded(t *testing.T) {
	tokens := &deadlineTokens{}
	g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = tokens }))
	g.spawn(workerSpec(t))
	if len(tokens.bounded) != 1 || !tokens.bounded[0] {
		t.Fatalf("mints bounded: %v, want one with a deadline", tokens.bounded)
	}
}

// A step of an issue pod's provisioning that fails — the retained-sessions read, the
// provisioning-token mint (a GitHub outage), the provisioning Secret's write — fails that launch
// with its error and nothing more: the launch returns it, never panics, gives the pod's launch turn
// back, and the pod's next launch, once the failure passes, launches.
func TestAFailedStepOfAnIssuePodsProvisioningReleasesThePodsTurn(t *testing.T) {
	failure := errors.New("the step failed")
	for name, tc := range map[string]struct {
		// fail arms the failure for the next launch only; the spec it returns is that launch's.
		fail func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec)
		want string
	}{
		"the retained-sessions read": {
			fail: func(t *testing.T, failed *atomic.Bool) (rigOption, func(g *rig) runtime.SpawnSpec) {
				store := &sessionsFailingStore{fakeStore: newFakeStore(), failed: failed, err: failure}
				return withOptions(func(o *Options) { o.Store = store }), func(*rig) runtime.SpawnSpec { return rootSpec(t) }
			},
			want: "read its issue's retained sessions",
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
			unlock, err := g.r.lockPod(taking, SandboxName(spec.Claim))
			if err != nil {
				t.Fatalf("the pod's launch turn is still held after the failed launch: %v", err)
			}
			unlock()
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

// sessionsFailingStore is a fakeStore whose next retained-sessions read fails, once.
type sessionsFailingStore struct {
	*fakeStore
	failed *atomic.Bool
	err    error
}

func (s *sessionsFailingStore) IssueHasSessions(ctx context.Context, project, issue string) (bool, error) {
	if s.failed.CompareAndSwap(false, true) {
		return false, s.err
	}
	return s.fakeStore.IssueHasSessions(ctx, project, issue)
}

// outageTokens refuses its next installation token, once, as a GitHub outage would.
type outageTokens struct{ failed *atomic.Bool }

func (o *outageTokens) Token(ctx context.Context, owner string) (string, error) {
	if o.failed.CompareAndSwap(false, true) {
		return "", errors.New("no installation token: GitHub is down")
	}
	return staticTokens{}.Token(ctx, owner)
}

// A role starting into a running issue pod reads every launcher Secret of the pod to check the pod
// is bound to it. A Secret that is gone, is not the Sandbox's, or binds another pod proves the pod
// is not one this runtime bound, and the pod is replaced. A read that fails, such as a server error,
// a throttled request or a timeout, proves nothing: that start fails with the error, and the pod
// and the sibling already running in it are left as they are, so one API-server error during a
// handoff never ends the roles already in the pod. The role's next start goes into the same pod.
func TestOnlyAPodsUnboundLaunchersReplaceItNotAFailedRead(t *testing.T) {
	for name, tc := range map[string]struct {
		// unbind is what happens to the reviewer's launcher Secret before the reviewer starts, with
		// fail armed to fail the next read of it.
		unbind   func(g *rig, secret string, fail *atomic.Bool)
		replaced bool
	}{
		"a read that fails with a server error": {
			unbind:   func(_ *rig, _ string, fail *atomic.Bool) { fail.Store(true) },
			replaced: false,
		},
		"a Secret that is gone": {
			unbind: func(g *rig, secret string, _ *atomic.Bool) {
				if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, secret); err != nil {
					g.t.Fatal(err)
				}
			},
			replaced: true,
		},
		"a Secret that is not the Sandbox's": {
			unbind: func(g *rig, secret string, _ *atomic.Bool) {
				editSecret(g, secret, func(s *corev1.Secret) { s.OwnerReferences = nil })
			},
			replaced: true,
		},
		"a Secret that binds another pod": {
			unbind: func(g *rig, secret string, _ *atomic.Bool) {
				editSecret(g, secret, func(s *corev1.Secret) { s.Annotations[launcherPodUIDAnnotation] = "uid-another-pod" })
			},
			replaced: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			testerLoc := g.spawn(workerSpec(t))
			sandboxName := testerLoc.Sandbox.Name
			reviewerSecret := roleSecretName(sandboxName, claim.RoleReviewer)
			var fail atomic.Bool
			g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				if a.(k8stesting.GetAction).GetName() == reviewerSecret && fail.CompareAndSwap(true, false) {
					return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
				}
				return false, nil, nil
			})
			tc.unbind(g, reviewerSecret, &fail)

			reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
			if !tc.replaced {
				g.launcher(reviewer.Claim)
				err := failedLaunch(t, g, reviewer)
				if err == nil || !strings.Contains(err.Error(), "read its pod's launcher bindings") || !strings.Contains(err.Error(), "etcdserver: request timed out") {
					t.Fatalf("Spawn = %v, want the failed read of %s", err, reviewerSecret)
				}
				if pod := g.pod(sandboxName); pod == nil || string(pod.UID) != testerLoc.Sandbox.PodUID {
					t.Fatalf("a failed read replaced the pod the tester runs in (%s), want it kept", testerLoc.Sandbox.PodUID)
				}
				if mode := g.sandbox(sandboxName).mode(); mode != modeRunning {
					t.Fatalf("a failed read left the Sandbox %s, want it running", mode)
				}
				if obs, err := g.r.Probe(g.ctx, testerLoc); err != nil || obs.Kind != runtime.Alive {
					t.Fatalf("the tester after its sibling's failed start: %s (%v), want alive", obs.Kind, err)
				}
			}
			reviewerLoc := g.spawn(reviewer)
			if replaced := reviewerLoc.Sandbox.PodUID != testerLoc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("the reviewer's start replaced the pod: %t, want %t", replaced, tc.replaced)
			}
		})
	}
}

// editSecret rewrites the Secret named name in the tracker.
func editSecret(g *rig, name string, edit func(*corev1.Secret)) {
	g.t.Helper()
	secret := g.secret(name)
	if secret == nil {
		g.t.Fatalf("no Secret %s", name)
	}
	next := secret.DeepCopy()
	edit(next)
	if err := g.kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), next, testNamespace); err != nil {
		g.t.Fatal(err)
	}
}

// A transient binding read after a live role's Machine let its previous locator go must not leave
// that previous child outside both the Machine and the runtime: Machine retries the ordinary
// Resume error until its launch-failure budget runs out, then persists the claim Failed. The child
// it started at generation 1 must already be stopped by then; neither a failed claim nor the
// runtime's forgotten watch may leave it acting.
func TestAFailedLauncherBindingReadNeverLeavesASupervisedRoleChildRunning(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	machine, store, _ := supervisedSandboxMachine(t, g, spec)
	first := startSupervisedSandboxRole(t, g, machine, spec, resumeSession)

	secret := roleSecretName(first.Sandbox.Name, claim.RoleTester)
	g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == secret {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})
	err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *first, Kind: runtime.StaleAddress, At: rigNow,
	}})
	if err == nil || !strings.Contains(err.Error(), "read its pod's launcher bindings") {
		t.Fatalf("StaleAddress = %v, want the binding-read failure", err)
	}
	if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Locator != nil {
		t.Fatalf("claim after the persistent failures = %+v, want failed with three launch failures and no locator", got)
	}
	if recorded, found := g.r.recorded(spec.Claim); found {
		t.Errorf("runtime still records %s after the failed claim", recorded.Incarnation)
	}
	state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
	if !connected || state.Child != nil {
		t.Fatalf("the failed claim left launcher connected=%t child=%+v, want no child", connected, state.Child)
	}
	if stored := store.claim; stored.State != supervise.StateFailed || stored.Locator != nil {
		t.Errorf("stored claim after failure = %+v, want failed with no locator", stored)
	}
}

// A Resume whose next spec prepare rejects must stop the old role child too. Machine has already
// let its locator go, so the malformed spec must not leave the child either watched or running as
// the Machine retries to Failed. Each is a different prepare refusal that happens before relaunch
// otherwise forgets the role: the recorded session path, a prompt file and one argv string.
func TestAPrepareRefusalOnResumeNeverLeavesASupervisedRoleChildRunning(t *testing.T) {
	for name, tc := range map[string]struct {
		sessionFile string
		mutate      func(*runtime.SpawnSpec)
		want        string
	}{
		"an invalid stored session path": {
			sessionFile: "/sessions/not-under-the-pod.jsonl",
			want:        "is not under",
		},
		"a missing prompt file": {
			sessionFile: resumeSession,
			mutate: func(spec *runtime.SpawnSpec) {
				spec.Prompt.RolePromptPaths = []string{"/missing/role-prompt.md"}
			},
			want: "prompt file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := workerSpec(t)
			machine, store, specs := supervisedSandboxMachine(t, g, spec)
			first := startSupervisedSandboxRole(t, g, machine, spec, tc.sessionFile)
			next := spec
			if tc.mutate != nil {
				tc.mutate(&next)
			}
			specs.next = &next
			err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
				Locator: *first, Kind: runtime.StaleAddress, At: rigNow,
			}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StaleAddress = %v, want the prepare refusal %q", err, tc.want)
			}
			if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Locator != nil {
				t.Fatalf("claim after the persistent prepare refusal = %+v, want failed with three launch failures and no locator", got)
			}
			if recorded, found := g.r.recorded(spec.Claim); found {
				t.Errorf("runtime still records %s after the failed claim", recorded.Incarnation)
			}
			state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
			if !connected || state.Child != nil {
				t.Fatalf("the failed claim left launcher connected=%t child=%+v, want no child", connected, state.Child)
			}
			if stored := store.claim; stored.State != supervise.StateFailed || stored.Locator != nil {
				t.Errorf("stored claim after failure = %+v, want failed with no locator", stored)
			}
		})
	}
}

// An error while Machine asks its Specs for a later Resume is before Runtime.Resume and therefore
// before relaunch's cleanup. Machine itself must stop the previous role process before it reports
// the launch failure; otherwise the same cleared locator leaves that generation watched and running.
func TestASpecBuilderRefusalOnResumeNeverLeavesASupervisedRoleChildRunning(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	machine, store, specs := supervisedSandboxMachine(t, g, spec)
	first := startSupervisedSandboxRole(t, g, machine, spec, resumeSession)
	specs.err = errors.New("the deployment instructions could not be read")
	err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *first, Kind: runtime.StaleAddress, At: rigNow,
	}})
	if err == nil || !strings.Contains(err.Error(), "build the launch") {
		t.Fatalf("StaleAddress = %v, want the Spec-builder refusal", err)
	}
	if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Locator != nil {
		t.Fatalf("claim after the persistent Spec refusal = %+v, want failed with three launch failures and no locator", got)
	}
	if recorded, found := g.r.recorded(spec.Claim); found {
		t.Errorf("runtime still records %s after the failed claim", recorded.Incarnation)
	}
	state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
	if !connected || state.Child != nil {
		t.Fatalf("the failed claim left launcher connected=%t child=%+v, want no child", connected, state.Child)
	}
	if stored := store.claim; stored.State != supervise.StateFailed || stored.Locator != nil {
		t.Errorf("stored claim after failure = %+v, want failed with no locator", stored)
	}
}

// A claim that never registered is relaunched through Spawn, which hands the runtime no previous
// locator. When that Spawn fails before its Start (here every read of the role's launcher Secret
// fails), the Machine must stop the generation it let go, since the runtime cannot know it: retried
// to Failed, no child of the role is left running and nothing is watched.
func TestASpawnRefusalForAnUnregisteredClaimNeverLeavesItsChildRunning(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	machine, store, _ := supervisedSandboxMachine(t, g, spec)
	g.launcher(spec.Claim)
	if err := machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	first := machine.Claim().Locator
	if first == nil {
		t.Fatal("initial Spawn left the Machine without a locator")
	}
	if err := machine.Handle(g.ctx, supervise.StreamHello{Claim: spec.Claim, Generation: 1}); err != nil {
		t.Fatalf("hello for generation 1: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateShimConnected {
		t.Fatalf("role state = %s, want shim_connected and unregistered", got)
	}
	secret := roleSecretName(first.Sandbox.Name, claim.RoleTester)
	g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == secret {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})
	err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *first, Kind: runtime.StaleAddress, At: rigNow,
	}})
	if err == nil || !strings.Contains(err.Error(), "read its pod's launcher bindings") {
		t.Fatalf("StaleAddress = %v, want the binding-read failure", err)
	}
	if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Locator != nil {
		t.Fatalf("claim after the persistent Spawn refusal = %+v, want failed with three launch failures and no locator", got)
	}
	if recorded, found := g.r.recorded(spec.Claim); found {
		t.Errorf("runtime still records %s after the failed claim", recorded.Incarnation)
	}
	state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
	if !connected || state.Child != nil {
		t.Fatalf("the failed claim left launcher connected=%t child=%+v, want no child", connected, state.Child)
	}
	if stored := store.claim; stored.State != supervise.StateFailed || stored.Locator != nil {
		t.Errorf("stored claim after failure = %+v, want failed with no locator", stored)
	}
}

// startSupervisedSandboxRole walks a real Sandbox runtime's role through the Machine to Ready at
// generation 1, returning the recorded locator a later Resume must stop on error.
func startSupervisedSandboxRole(
	t *testing.T, g *rig, machine *supervise.Machine, spec runtime.SpawnSpec, sessionFile string,
) *runtime.Locator {
	t.Helper()

	g.launcher(spec.Claim)
	if err := machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	first := machine.Claim().Locator
	if first == nil {
		t.Fatal("initial Spawn left the Machine without a locator")
	}
	for _, event := range []supervise.Event{
		supervise.StreamHello{Claim: spec.Claim, Generation: 1},
		supervise.RequestRegister{Claim: spec.Claim, Generation: 1, Session: "ses-tester", SessionFile: sessionFile},
		supervise.RequestReady{Claim: spec.Claim, Generation: 1, Session: "ses-tester"},
	} {
		if err := machine.Handle(g.ctx, event); err != nil {
			t.Fatalf("%T for generation 1: %v", event, err)
		}
	}
	if got := machine.Claim().State; got != supervise.StateReady {
		t.Fatalf("initial role state = %s, want ready", got)
	}
	return first
}

// A launcher can receive a Start while its answer is delayed past the runtime's start deadline.
// That start may have begun the new generation even though start returns an error, so relaunch,
// which owns the child it started, sends a Stop for the issued generation before returning. This
// holds both where Resume has no previous locator and where its previous locator names the same
// pod: the Stop names the issued generation, never the previous one, which is the caller's.
func TestAStartWhoseAnswerIsDelayedPastItsDeadlineIsStoppedBeforeResumeReturns(t *testing.T) {
	for name, previous := range map[string]bool{
		"without a previous locator":              false,
		"on the same pod as its previous locator": true,
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(func(o *Options) {
				o.BootTimeout, o.TerminationGrace = time.Second, 50*time.Millisecond
			}))
			spec := workerSpec(t)
			first := g.spawn(spec)
			delayed := g.scriptedStartLauncher(spec.Claim, 1, func(shimwire.LauncherStart) *shimwire.LauncherStartResult { return nil })
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
			var prev *runtime.Locator
			if previous {
				prev = &first
			}
			if _, err := g.r.Resume(g.ctx, prev, spec); err == nil || !strings.Contains(err.Error(), "start through its authenticated launcher") {
				t.Fatalf("Resume = %v, want the delayed Start's deadline", err)
			}
			if start := delayed.start(t, 2); start.Generation != 2 {
				t.Fatalf("Start = generation %d, want generation 2", start.Generation)
			}
			if stop := delayed.stop(t, 2); stop.Generation != 2 {
				t.Fatalf("Stop = generation %d, want the Start's generation 2", stop.Generation)
			}
			if recorded, found := g.r.recorded(spec.Claim); found {
				t.Errorf("runtime still records %s after the failed Resume", recorded.Incarnation)
			}
			// The launcher reports generation 2's exit in the state frame it writes after its Stop
			// result, which the runtime reads asynchronously; wait for it, then hold it exactly.
			g.eventually("the launcher to report generation 2 ended", func() bool {
				state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
				return connected && state.LastExit != nil && state.LastExit.Generation == 2
			})
			state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
			if !connected || state.Child != nil || state.LastExit == nil || state.LastExit.Generation != 2 {
				t.Fatalf("launcher after the delayed Start = connected=%t state=%+v, want generation 2 ended", connected, state)
			}
		})
	}
}

// A launcher that answers a Start OK but names another generation than the Start's has said it
// started something, and not what it was asked. That is no refusal, the one start error after which
// nothing can be running, so relaunch reads the Start's outcome as unknown and stops the issued
// generation, as for an unanswered Start: the launcher here did begin it.
func TestAnOKStartAnswerNamingAnotherGenerationIsStoppedNotTakenAsARefusal(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	first := g.spawn(spec)
	misanswering := g.scriptedStartLauncher(spec.Claim, 1, func(start shimwire.LauncherStart) *shimwire.LauncherStartResult {
		return &shimwire.LauncherStartResult{ID: start.ID, OK: true, RunningGeneration: start.Generation - 1}
	})
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	_, err := g.r.Resume(g.ctx, &first, spec)
	if start := misanswering.start(t, 2); start.Generation != 2 {
		t.Fatalf("Start = generation %d, want generation 2", start.Generation)
	}
	if stop := misanswering.stop(t, 2); stop.Generation != 2 {
		t.Fatalf("Stop = generation %d, want the Start's generation 2", stop.Generation)
	}
	var refused startRefusal
	if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), "answered OK naming generation 1") {
		t.Fatalf("Resume = %v, want the mismatched answer as an unknown outcome, not a refusal", err)
	}
	if recorded, found := g.r.recorded(spec.Claim); found {
		t.Errorf("runtime still records %s after the failed Resume", recorded.Incarnation)
	}
	g.eventually("the launcher to report generation 2 ended", func() bool {
		state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID)
		return connected && state.LastExit != nil && state.LastExit.Generation == 2
	})
	if state, connected := g.r.launchers.state(spec.Claim, first.Sandbox.PodUID); !connected || state.Child != nil {
		t.Fatalf("launcher after the mismatched answer = connected=%t state=%+v, want no child", connected, state)
	}
}

// scriptedStartLauncher is a launcher that starts its child at each Start it receives and answers it
// as answer says: nil withholds the Start result and state until the runtime's cleanup sends Stop.
// The Stop runs after the Start on the same connection, so the child it reports ended is exactly
// the Start's generation.
type scriptedStartLauncher struct {
	starts chan shimwire.LauncherStart
	stops  chan shimwire.LauncherStop
}

func (g *rig) scriptedStartLauncher(
	token claim.Token, running uint64, answer func(shimwire.LauncherStart) *shimwire.LauncherStartResult,
) *scriptedStartLauncher {
	g.t.Helper()
	g.r.launchers.mu.Lock()
	old := g.r.launchers.sessions[token]
	g.r.launchers.mu.Unlock()
	if old != nil {
		old.close()
	}
	server, client := net.Pipe()
	pod := g.pod(SandboxName(token))
	session := g.r.launchers.accept(token, shimwire.LauncherHello{LauncherID: "scripted-" + string(token), PodUID: string(pod.UID)})
	g.t.Cleanup(func() { _ = client.Close() })
	scripted := &scriptedStartLauncher{
		starts: make(chan shimwire.LauncherStart, 2),
		stops:  make(chan shimwire.LauncherStop, 4),
	}
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	go func() {
		writer := shimwire.NewWriter(client)
		_ = writer.WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: running, PID: 42}})
		reader := shimwire.NewReader(bufio.NewReader(client))
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				return
			}
			switch command := frame.(type) {
			case shimwire.LauncherStart:
				running = command.Generation
				scripted.starts <- command
				if result := answer(command); result != nil {
					_ = writer.WriteFrame(*result)
				}
			case shimwire.LauncherStop:
				scripted.stops <- command
				if running != command.Generation {
					_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", running, command.Generation)})
					continue
				}
				running = 0
				_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, OK: true})
				_ = writer.WriteFrame(shimwire.LauncherState{LastExit: &shimwire.LauncherExit{Generation: command.Generation, Code: 143, Signal: "terminated"}})
			}
		}
	}()
	return scripted
}

func (d *scriptedStartLauncher) start(t *testing.T, generation uint64) shimwire.LauncherStart {
	t.Helper()
	for {
		select {
		case command := <-d.starts:
			if command.Generation == generation {
				return command
			}
		case <-time.After(time.Second):
			t.Fatalf("no generation-%d Start", generation)
		}
	}
}

func (d *scriptedStartLauncher) stop(t *testing.T, generation uint64) shimwire.LauncherStop {
	t.Helper()
	for {
		select {
		case command := <-d.stops:
			if command.Generation == generation {
				return command
			}
		case <-time.After(time.Second):
			t.Fatalf("no generation-%d Stop", generation)
		}
	}
}

// supervisedSandboxMachine is a Machine over the rig's real Sandbox runtime, rather than the
// runtime fake most Machine tests use. Its in-memory Store and no-timer Clock are enough for this
// lifecycle: no task is sent and no deadline fires while the test drives the Machine directly.
func supervisedSandboxMachine(
	t *testing.T, g *rig, spec runtime.SpawnSpec,
) (*supervise.Machine, *supervisedSandboxStore, *sandboxMachineSpecs) {
	t.Helper()
	c := supervise.Claim{
		Token: spec.Claim, Project: spec.Project, Tree: spec.Tree, Issue: spec.Issue, Role: spec.Role, State: supervise.StateQueued,
	}
	store := &supervisedSandboxStore{claim: c}
	specs := &sandboxMachineSpecs{spec: spec}
	machine, err := supervise.NewMachine(g.ctx, supervise.Deps{
		Runtime: g.r, Conns: g.conns, Store: store, Specs: specs, Clock: sandboxMachineClock{},
		Log:    slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 1, PromptRetires: 1},
		Timeouts: supervise.Timeouts{
			Boot: time.Hour, RegistrationIntervals: 1, RPC: time.Hour, Probe: time.Hour, Stop: time.Hour,
		},
	}, c)
	if err != nil {
		t.Fatalf("new Machine: %v", err)
	}
	return machine, store, specs
}

type sandboxMachineSpecs struct {
	spec runtime.SpawnSpec
	next *runtime.SpawnSpec
	err  error
}

func (s *sandboxMachineSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	if s.err != nil {
		return runtime.SpawnSpec{}, s.err
	}
	if s.next != nil {
		return *s.next, nil
	}
	return s.spec, nil
}

type sandboxMachineClock struct{}

func (sandboxMachineClock) Now() time.Time { return rigNow }

func (sandboxMachineClock) AfterFunc(time.Duration, func()) supervise.Cancel {
	return sandboxMachineCancel{}
}

type sandboxMachineCancel struct{}

func (sandboxMachineCancel) Stop() bool { return true }

type supervisedSandboxStore struct{ claim supervise.Claim }

func (s *supervisedSandboxStore) AdmitClaim(_ context.Context, c supervise.Claim) (supervise.Claim, error) {
	s.claim = c
	return c, nil
}

func (*supervisedSandboxStore) CheckLaunch(context.Context, supervise.Claim) error { return nil }

func (s *supervisedSandboxStore) PutClaim(_ context.Context, c supervise.Claim) error {
	s.claim = c
	return nil
}

func (*supervisedSandboxStore) PutDelivery(context.Context, claim.Token, supervise.Delivery) error {
	return nil
}

func (s *supervisedSandboxStore) PutClaimAndDelivery(_ context.Context, c supervise.Claim, _ supervise.Delivery) error {
	s.claim = c
	return nil
}

func (s *supervisedSandboxStore) RetireDelivery(_ context.Context, c supervise.Claim, _ string) error {
	s.claim = c
	return nil
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
