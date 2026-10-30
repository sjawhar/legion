package sandbox

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	ompsessions "github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Three facets moved out of movedInPod into driftedInPod for this issue — the secrets broker's
// enrollment, a named role's overlays and the session store — so a launch no longer replaces a
// live pod that holds one, and an update round replaces it instead. PodDrift answers nothing for
// the project controller's pod, by design: that pod has no issue workspace and nothing to push,
// and drift.go says it "is replaced by its next launch as it always was".
//
// It is not. The launch replaces a live pod only for movedInPod, which these three left, so the
// controller's pod is replaced by neither: not by its launch, which keeps it, and not by a round,
// which is never opened over it. The controller then runs for ever in a pod built before the
// deployment changed — with no broker, so the token its `slack_post` is granted through
// agent-secrets never reaches it; with none of the overlays `role_config_files.controller` names;
// and writing its session to the store the old pod projects. Its observation is Alive and PodDrift
// is empty, so nothing anywhere says so.
//
// On main each of these is movedInPod, read StaleAddress, and the controller's next launch
// replaces its pod. A controller left behind by a configuration change must still be brought onto
// it: either its launch replaces the pod, as before, or PodDrift names the drift so something
// replaces it.
func TestTheControllersPodIsNotLeftBehindByAConfigurationItCannotCarry(t *testing.T) {
	const overlay = "/etc/legion-operator-roles/controller.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	for name, now := range map[string]func(*Options){
		"the secrets broker's enrollment": func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
		},
		"the controller's own overlay": func(o *Options) {
			o.Pod.Volumes = append(o.Pod.Volumes, roles)
			o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true})
			o.RoleConfigFiles = map[claim.Role][]string{claim.RoleController: {overlay}}
		},
		"the session store": func(o *Options) {
			o.SessionStore = ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "legion/sessions/other"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, now)()
			g.connectRunning(controllerToken, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			held := r2.driftedInPod(g.pod(loc.Sandbox.Name), claim.RoleController)
			if len(held) == 0 {
				t.Fatalf("the controller's pod holds nothing of this change, so this case proves nothing")
			}
			obs := readopt(t, g.ctx, r2, loc)[0]
			drifted := driftOf(t, g.ctx, r2, loc)
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the controller: %v", err)
			}
			replaced := next.Sandbox.PodUID != loc.Sandbox.PodUID
			if replaced || len(drifted) > 0 {
				return
			}
			t.Errorf("the controller's pod holds %+v, which a pod created now is not handed, and nothing will replace it: its launch kept pod %s, PodDrift is empty and its observation is %s (%s)",
				held, loc.Sandbox.PodUID, obs.Kind, obs.Detail)
			if obs.Kind != runtime.StaleAddress {
				t.Logf("on main this same change reads StaleAddress and the launch replaces the pod")
			}
		})
	}
}
