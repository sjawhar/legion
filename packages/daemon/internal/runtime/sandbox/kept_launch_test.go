package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A role is started in the pod it will actually run in, not in the pod the configuration now
// describes. A launch into a live drifted pod keeps that pod, since the update round, not the
// launch, is what ends the roles already working in it (INFRA-484), so the start command is built
// from what that pod was built with (keptPod): no agent-secrets flag the pod carries no volumes
// for, no PI_CONFIG_FILES entry the pod does not hold.
//
// The tester's own TestARoleStartedInAKeptDriftedPodIsStartedAsItsPodWasBuilt holds the kept pod
// that carries neither facet. These three hold the rest of the rule: a launch that replaces the pod
// still builds the start from the configuration as it now reads, since the pod it makes carries it;
// a kept pod that does carry both is started with both; and a stored template this daemon cannot
// read makes the launch replace the pod rather than guess what it carries.

// keptRolesVolume is the operator's volume of per-role settings overlays and keptRolesMount the
// mount every pod built with it holds, keptOverlayFile the tester's own file under that mount: the
// three a deployment adds when it starts giving one role settings of its own, which no running pod
// can be changed to carry.
var (
	keptRolesVolume = corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	keptRolesMount = corev1.VolumeMount{Name: keptRolesVolume.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true}
)

const keptOverlayFile = "/etc/legion-operator-roles/fallback.yml"

// withBrokerAndOverlay enrols the runtime's pods with the secrets broker and gives the tester an
// overlay of its own: the two drift facets a role's own start command names, through the shim's
// agent-secrets flags (launcherCommand) and PI_CONFIG_FILES (roleOperatorEnv).
func withBrokerAndOverlay(o *Options) {
	o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	o.Pod.Volumes = append(o.Pod.Volumes, keptRolesVolume)
	o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, keptRolesMount)
	o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {keptOverlayFile}}
}

// restartedOver is a daemon restarted over g's running tester, its configuration changed by move,
// with that tester's launcher connected to it and reporting its child, as a real launcher does once
// it has redialled the restarted daemon.
func restartedOver(t *testing.T, g *rig, loc runtime.Locator, move func(*Options)) *Runtime {
	t.Helper()
	r2 := secondRuntime(t, g, move)()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	return r2
}

// resumeTester is the launch under test: the tester's claim resumed at generation 2, on the session
// its running generation recorded, which is how the supervisor starts a role again in a pod of its
// own issue.
func resumeTester(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) runtime.Locator {
	t.Helper()
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	next, err := r.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	return next
}

// startSent is the start command the launch under test sent the tester's launcher: an earlier
// generation's as the last one received means that launch started nothing, and nothing it names can
// be read.
func startSent(t *testing.T, g *rig, generation uint64) shimwire.LauncherStart {
	t.Helper()
	start := lastStart(t, g, workerToken)
	if start.Generation != generation {
		t.Fatalf("the tester's launcher last received generation %d's start command, want generation %d's", start.Generation, generation)
	}
	return start
}

// agentSecretsFlags is whether start names the shim's three agent-secrets flags, in the order
// launcherCommand appends them: the key directory and token file it stats before it starts anything
// (shim.ReadAgentSecretsFlags), and the client binary it enrols with.
func agentSecretsFlags(start shimwire.LauncherStart, r *Runtime) bool {
	want := []string{
		"--agent-secrets-key-dir", AgentSecretsKeyDir,
		"--pod-token-file", AgentSecretsTokenDir + "/" + AgentSecretsTokenFile,
		"--agent-secrets-bin", r.tools.AgentSecrets,
	}
	return strings.Contains(strings.Join(start.Argv, "\x00"), strings.Join(want, "\x00"))
}

// A launch that replaces the pod builds its role's start from the configuration as it now reads:
// the pod it makes carries the broker's enrollment and the operator's overlay mount, so the start
// names the shim's agent-secrets flags and the overlay. The degraded start is the kept pod's alone
// (keptPod) and must never outlive the pod it was built for — a role of the fresh pod that went
// without the broker would stay without it until something else replaced that pod.
func TestALaunchThatReplacesTheDriftedPodStartsItsRoleOnTheConfigurationNow(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	// The worker stream moved as well as the configuration: the pod's launchers dial an address
	// this daemon does not serve and never redial, so no worker in it can be asked to push its work
	// first (movedInPod), and the launch replaces the pod where it stands.
	r2 := restartedOver(t, g, loc, func(o *Options) {
		withBrokerAndOverlay(o)
		moveStream(o)
	})
	fresh := resumeTester(t, g, r2, loc)
	if fresh.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, whose launchers dial a stream this daemon does not serve", loc.Sandbox.PodUID)
	}
	pod := g.pod(loc.Sandbox.Name)
	for _, volume := range []string{agentSecretsTokenVolume, roleVolume(agentSecretsKeyVolume, claim.RoleTester)} {
		if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == volume }) {
			t.Errorf("the replaced pod carries no %s, which the configuration enrols every pod it builds with", volume)
		}
	}
	if !slices.ContainsFunc(containerNamed(t, pod.Spec, workerContainer).VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == keptRolesVolume.Name }) {
		t.Errorf("the replaced pod does not mount %s, which holds the tester's overlay", keptRolesVolume.Name)
	}
	start := startSent(t, g, 2)
	if !agentSecretsFlags(start, r2) {
		t.Errorf("the start command is %q, want the shim's agent-secrets flags: the pod it runs in carries the broker's enrollment", start.Argv)
	}
	if files := startEnv(start)["PI_CONFIG_FILES"]; !strings.Contains(files, keptOverlayFile) {
		t.Errorf("the start command's PI_CONFIG_FILES is %q, want %s among it: the pod it runs in mounts that overlay", files, keptOverlayFile)
	}
}

// A kept pod that does carry the broker's enrollment and the role's overlay is started with both:
// the start names what the pod holds, which here is everything the configuration names. A pod drifts
// for reasons that touch no start command — its reservation here — and a role of it must not be
// started degraded for one of those.
func TestARoleStartedInAKeptPodThatCarriesBothIsStartedWithBoth(t *testing.T) {
	g := newRig(t, nil, withOptions(withBrokerAndOverlay))
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(o *Options) {
		withBrokerAndOverlay(o)
		movedReservation(o)
	})
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
		t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
	}
	kept := resumeTester(t, g, r2, loc)
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the drifted pod; this case is about the pod a launch keeps")
	}
	start := startSent(t, g, 2)
	if !agentSecretsFlags(start, r2) {
		t.Errorf("the start command is %q, want the shim's agent-secrets flags: its kept pod carries the broker's enrollment", start.Argv)
	}
	if files := startEnv(start)["PI_CONFIG_FILES"]; !strings.Contains(files, keptOverlayFile) {
		t.Errorf("the start command's PI_CONFIG_FILES is %q, want %s among it: its kept pod mounts that overlay", files, keptOverlayFile)
	}
}

// A launch that cannot tell what the pod carries replaces it where it stands, which is what every
// launch into a drifted pod did before the update round. The daemon reads what a kept pod was built
// with from its Sandbox's stored pod template (keptBy); a template holding no container for the role
// says nothing about the pod, and a start built on a guess is the outage this issue exists to end.
// The same launch over a readable template keeps the pod
// (TestARoleStartedInAKeptDriftedPodIsStartedAsItsPodWasBuilt).
func TestALaunchReplacesTheDriftedPodWhenItsStoredTemplateCannotBeRead(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, withBrokerAndOverlay)
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
		t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
	}
	forgetPodContainers(t, g, r2, loc.Sandbox.Name)
	fresh := resumeTester(t, g, r2, loc)
	if fresh.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, whose stored template says nothing of what it was built with", loc.Sandbox.PodUID)
	}
}

// forgetPodContainers leaves the stored Sandbox's pod template with no container at all — a
// template this daemon cannot read what its pod was built with from — and waits for the store the
// runtime reads to hold it. The pod itself is untouched: it is the record of the pod's build that
// is gone, not the pod.
func forgetPodContainers(t *testing.T, g *rig, r *Runtime, name string) {
	t.Helper()
	s := g.sandbox(name)
	if s == nil {
		t.Fatalf("sandbox %s is absent", name)
	}
	s.Spec.PodTemplate.Spec.Containers = nil
	u, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.eventually("the sandbox store to see "+name+" with no container in its pod template", func() bool {
		stored, err := r.storedSandbox(name)
		return err == nil && stored != nil && len(stored.Spec.PodTemplate.Spec.Containers) == 0
	})
}
