package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// movedControllerReservation is the controller pod's reservation with another CPU request, the
// kind's own and nothing the daemon derives (movedReservation is the issue pod's).
func movedControllerReservation(o *Options) {
	resources := reservations()
	resources.Controller.Requests[corev1.ResourceCPU] = resource.MustParse("5")
	resources.Controller.Limits[corev1.ResourceCPU] = resource.MustParse("5")
	o.Resources = resources
}

// A pod no round covers (roundCovers) is replaced by its next launch for every kind of drift, as
// every launch did before the round existed. Its three facets reach that launch as moved addresses
// (movedInPod, TestTheControllersPodIsNotLeftBehindByAConfigurationItCannotCarry); the kind of
// drift that does not is the shape its template was built from — the image, the kind's own
// reservation, an operator volume or mount, and the disk size, each key of podShapeRecord — which
// the launch reads through the same one computation the round's reasons are named from
// (shapeDrift). Nothing else would: PodDrift answers nothing for such a pod, so no round is ever
// opened over it, and a launch that kept it would leave the project controller running for ever in
// a pod the deployment has moved away from, on the image, reservation, volumes, mounts and disk it
// was created with.
//
// A launch that installs a stale-plugin pair into such a pod keeps it, as it always did
// (spec.Plugins, LEGION-164): the install, not a fresh pod on the configured image, is what brings
// that agent onto the current plugin. And a daemon restarted on the configuration its pod was
// built from replaces nothing — the failure that would matter most here, since it would rebuild
// the controller's pod at every launch of it.
func TestALaunchReplacesAPodNoRoundCoversWhoseShapeHasDrifted(t *testing.T) {
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	for name, tc := range map[string]struct {
		now      func(*Options)
		plugins  *runtime.PluginInstall
		replaced bool
	}{
		"the same configuration":                           {func(*Options) {}, nil, false},
		"another image":                                    {func(o *Options) { o.Image = nextImage }, nil, true},
		"another reservation":                              {movedControllerReservation, nil, true},
		"an operator volume added":                         {withOperatorVolume, nil, true},
		"an operator mount added":                          {withOperatorMount, nil, true},
		"another disk size":                                {func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, nil, true},
		"an installing launch into a pod on another image": {func(o *Options) { o.Image = nextImage }, plugins, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, tc.now)()
			g.connectRunning(controllerToken, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
				t.Fatalf("PodDrift = %q of a pod no round covers, want nothing: its launch is what replaces it", drifted)
			}
			shape := func() string {
				return g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
			}
			built := shape()
			spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-controller-g2", controllerSession, tc.plugins
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the controller: %v", err)
			}
			if replaced := next.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the controller's pod: %t, want %t (pod %s, then %s)",
					replaced, tc.replaced, loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
			switch want := r2.podShapes[controllerPod{}]; {
			case tc.replaced && shape() != want:
				t.Errorf("the controller's new pod is built from %s, want the configuration as it now reads, %s", shape(), want)
			case !tc.replaced && shape() != built:
				t.Errorf("the kept pod's template records %s, want the shape that pod was built from, %s", shape(), built)
			}
		})
	}
}

// The same five keys over a pod a round does cover: an issue pod whose shape has moved is kept by
// every launch into it, whatever moved, and the round PodDrift names the reasons of is what
// replaces it. This is the direction that must never break — replacing an issue pod where it
// stands ends every role's turn in it and deletes the disk they work on, with everything unpushed
// (INFRA-484) — and the four keys beside the image reach a launch through nothing but shapeDrift,
// which a pod a round covers is never read by (relaunch, roundCovers).
func TestALaunchKeepsAnIssuePodWhateverOfItsShapeMoved(t *testing.T) {
	for name, now := range map[string]func(*Options){
		"another image":            func(o *Options) { o.Image = nextImage },
		"another reservation":      movedReservation,
		"an operator volume added": withOperatorVolume,
		"an operator mount added":  withOperatorMount,
		"another disk size":        func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := workerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, now)()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
				t.Fatalf("PodDrift of the issue pod is empty, so this case proves nothing: its round is what replaces it")
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the tester: %v", err)
			}
			if next.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Errorf("Resume replaced the issue pod %s with %s, want it kept for its update round",
					loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
		})
	}
}

// A launch into a live drifted pod keeps the pod (relaunch) and starts the role in it. Two of the
// drift facets that reach a round are configuration the role's own start command is built from,
// not only configuration the pod carries: the secrets broker's enrollment, which puts the shim's
// three agent-secrets flags in the start (launcher.go, launcherCommand), and a named role's
// overlay, which puts the file in the generation's PI_CONFIG_FILES (roleOperatorEnv). A pod
// created before either moved carries neither the token volume and key directory those flags name
// nor the mount that overlay file is under, so the role is started against a pod it is not in:
// `legion worker-shim` stats --agent-secrets-key-dir and --pod-token-file and exits 1 when either
// is absent (shim.ReadAgentSecretsFlags), and Oh My Pi refuses to start on a PI_CONFIG_FILES entry
// that is not there.
//
// That process never registers, so its claim never answers the round it was started for, and a
// round waits for every claim of its pod with a session (daemon's blockingClaims). The round that
// would have replaced the pod is therefore blocked by the pod it is replacing — the update task
// itself revives a suspended claim into that pod (supervise's askForUpdate) — and only an operator
// drain ends it, which is the outage this issue exists to end.
//
// The launch must therefore not start a role against configuration its pod cannot carry: either it
// starts the role as the pod it is in was built (no new flag, no new overlay), or it does not
// start that role in that pod at all.
func TestARoleStartedInAKeptDriftedPodIsStartedAsItsPodWasBuilt(t *testing.T) {
	const overlay = "/etc/legion-operator-roles/fallback.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	for name, tc := range map[string]struct {
		now func(*Options)
		// check reports what the start names that the pod the role runs in cannot carry.
		check func(t *testing.T, start map[string]string, argv []string, pod *corev1.Pod)
	}{
		"the secrets broker's enrollment": {
			now: func(o *Options) {
				o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
			},
			check: func(t *testing.T, _ map[string]string, argv []string, pod *corev1.Pod) {
				t.Helper()
				if !slices.Contains(argv, "--agent-secrets-key-dir") {
					return
				}
				held := func(name string) bool {
					return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
				}
				if !held(agentSecretsTokenVolume) || !held(roleVolume(agentSecretsKeyVolume, claim.RoleTester)) {
					t.Errorf("the start names the shim's agent-secrets flags %q, which stat their paths, while the pod it runs in carries neither %s nor %s",
						argv[slices.Index(argv, "--agent-secrets-key-dir"):slices.Index(argv, "--agent-secrets-bin")+2], agentSecretsTokenVolume, roleVolume(agentSecretsKeyVolume, claim.RoleTester))
				}
			},
		},
		"a named role's overlay": {
			now: func(o *Options) {
				o.Pod.Volumes = append(o.Pod.Volumes, roles)
				o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true})
				o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {overlay}}
			},
			check: func(t *testing.T, env map[string]string, _ []string, pod *corev1.Pod) {
				t.Helper()
				if !strings.Contains(env["PI_CONFIG_FILES"], overlay) {
					return
				}
				if !slices.ContainsFunc(containerNamed(t, pod.Spec, workerContainer).VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == roles.Name }) {
					t.Errorf("the start's PI_CONFIG_FILES is %q, on which Oh My Pi refuses to start, while the pod it runs in does not mount %s",
						env["PI_CONFIG_FILES"], roles.Name)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := workerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, tc.now)()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
				t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			kept, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the drifted pod; this case is about the pod a launch keeps")
			}
			start := lastStart(t, g, workerToken)
			tc.check(t, startEnv(start), start.Argv, g.pod(loc.Sandbox.Name))
		})
	}
}
