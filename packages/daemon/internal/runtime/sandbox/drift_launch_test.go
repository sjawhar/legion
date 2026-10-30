package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

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
