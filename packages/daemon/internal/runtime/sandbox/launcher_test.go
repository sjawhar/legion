package sandbox

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A launcher is accepted only with its own role's current token, from the pod its Secret is bound
// to, and only while that pod is the issue Sandbox's controller-owned pod: another role's token, a
// token of an earlier pod, or a pod the binding does not name controls nothing.
func TestTheLauncherResolverAcceptsOnlyTheBoundRoleOfTheCurrentPod(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name
	token := func(role claim.Role) string {
		return string(g.secret(roleSecretName(name, role)).Data[LauncherTokenFile])
	}
	accept := g.r.LauncherResolver()
	valid := shimwire.LauncherHello{Token: token(claim.RoleTester), Sandbox: name, Role: string(claim.RoleTester), PodUID: loc.Sandbox.PodUID, LauncherID: "l-1"}
	if handler, reason := accept(valid); handler == nil {
		t.Fatalf("the tester's own launcher was refused: %s", reason)
	}
	for label, edit := range map[string]func(*shimwire.LauncherHello){
		"another role's token":            func(h *shimwire.LauncherHello) { h.Token = token(claim.RoleArchitect) },
		"another role with this token":    func(h *shimwire.LauncherHello) { h.Role = string(claim.RoleArchitect) },
		"a pod the binding does not name": func(h *shimwire.LauncherHello) { h.PodUID = "uid-pod-elsewhere" },
		"another issue's Sandbox":         func(h *shimwire.LauncherHello) { h.Sandbox = "legion-legion-legion-209" },
		"a role Legion does not have":     func(h *shimwire.LauncherHello) { h.Role = "operator" },
	} {
		t.Run(label, func(t *testing.T) {
			hello := valid
			edit(&hello)
			if handler, reason := accept(hello); handler != nil || reason == "" {
				t.Fatalf("accepted %+v (reason %q)", hello, reason)
			}
		})
	}
}

// A start of a new generation while the role's launcher still runs an earlier one stops the
// earlier one first: two generations of one role never run side by side in the issue pod.
func TestANewGenerationStopsTheEarlierOneFirst(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(workerSpec(t))
	spec := workerSpec(t)
	spec.Generation, spec.BootToken = 2, "boot-g2"
	g.spawn(spec)
	var got []string
	for _, frame := range g.sent(workerToken) {
		switch command := frame.(type) {
		case shimwire.LauncherStart:
			got = append(got, fmt.Sprintf("start %d", command.Generation))
		case shimwire.LauncherStop:
			got = append(got, fmt.Sprintf("stop %d", command.Generation))
		}
	}
	if strings.Join(got, ", ") != "start 1, stop 1, start 2" {
		t.Fatalf("the tester's launcher was sent %v, want generation 1 stopped before 2 starts", got)
	}
}

// Every role's launcher token projection, private credential directory, state directory and
// agent-secrets key directory are mounted in that role's container alone: no other role, nor an
// init container, can read another role's credentials. The issue's checkout, tree and sessions are
// what roles share.
func TestEveryRolesPrivateVolumesMountInItsContainerAlone(t *testing.T) {
	opts := goldenOptions()
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.prepare(rootSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.podTemplate(l, false).Spec
	containers := append(slices.Clone(pod.InitContainers), pod.Containers...)
	for _, role := range claim.Roles {
		for _, volume := range []string{roleVolume("launcher", role), roleVolume("private", role), roleVolume(stateVolume, role), roleVolume(agentSecretsKeyVolume, role)} {
			var mountedIn []string
			for _, c := range containers {
				for _, m := range c.VolumeMounts {
					if m.Name == volume {
						mountedIn = append(mountedIn, c.Name)
					}
				}
			}
			if len(mountedIn) != 1 || mountedIn[0] != string(role) {
				t.Errorf("%s is mounted in %v, want %s alone", volume, mountedIn, role)
			}
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil && strings.HasSuffix(v.Name, "-"+string(claim.RoleTester)) &&
			(len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != LauncherTokenFile) {
			t.Errorf("role volume %s projects %v, want only the launcher token", v.Name, v.Secret.Items)
		}
	}
}

func TestRejectedLauncherHellosDoNotSpendKubernetesRequests(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	accept := g.r.LauncherResolver()
	g.kube.ClearActions()
	for range 20 {
		for _, name := range []string{loc.Sandbox.Name, "unknown-sandbox"} {
			if handler, reason := accept(shimwire.LauncherHello{
				Token: "wrong-token", Sandbox: name, Role: "tester", PodUID: loc.Sandbox.PodUID, LauncherID: "attacker",
			}); handler != nil || reason == "" {
				t.Fatal("unauthenticated launcher was accepted")
			}
		}
	}
	for _, action := range g.kube.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			t.Fatal("unauthenticated launcher hello issued a Kubernetes Secret GET")
		}
	}
}
