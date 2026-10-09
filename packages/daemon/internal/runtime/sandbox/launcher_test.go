package sandbox

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
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

// conflictSecretUpdates makes the next updates of the Secret named name fail as a conflict, up to
// times of them, each one after between has rewritten the Secret in the tracker as the write that
// won the race would have; it counts the conflicts served.
func conflictSecretUpdates(g *rig, name string, times int32, between func(*corev1.Secret)) *atomic.Int32 {
	var served atomic.Int32
	g.kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name != name || served.Load() >= times {
			return false, nil, nil
		}
		served.Add(1)
		editSecret(g, name, between)
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), name, errors.New("the object has been modified"))
	})
	return &served
}

// A binding write the API server refuses as a conflict — the gh-credential refresher rewrote the
// role's Secret between the binding's read and its write — is retried once over a fresh read: the
// launch passes, the Secret binds the new pod and keeps the refresher's gh files, and the pod's
// launcher is accepted.
func TestABindingWriteRefusedAsAConflictIsRetriedOnceOverAFreshRead(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	secret := roleSecretName(SandboxName(spec.Claim), claim.RoleTester)
	refreshed := ghconfig.Hosts("ghs_refreshed_lease")
	served := conflictSecretUpdates(g, secret, 1, func(s *corev1.Secret) { s.Data[GitHubHostsKey] = []byte(refreshed) })

	loc := g.spawn(spec)

	if served.Load() != 1 {
		t.Fatalf("the conflict reactor served %d conflicts, want the one", served.Load())
	}
	after := g.secret(secret)
	if after.Annotations[launcherPodUIDAnnotation] != loc.Sandbox.PodUID {
		t.Errorf("the Secret binds pod %q, want the new pod %q", after.Annotations[launcherPodUIDAnnotation], loc.Sandbox.PodUID)
	}
	if string(after.Data[GitHubHostsKey]) != refreshed {
		t.Errorf("the retry wrote over the gh files written between read and write: %q", after.Data[GitHubHostsKey])
	}
	hello := shimwire.LauncherHello{Token: string(after.Data[LauncherTokenFile]), Sandbox: loc.Sandbox.Name, Role: string(claim.RoleTester), PodUID: loc.Sandbox.PodUID, LauncherID: "l-1"}
	if handler, reason := g.r.LauncherResolver()(hello); handler == nil {
		t.Errorf("the bound pod's launcher was refused: %s", reason)
	}
}

// A binding write that conflicts again on its retry fails the launch with the conflict, as any other
// write failure does: the bind is retried once, not until it lands.
func TestABindingWriteConflictedTwiceFailsTheLaunch(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	secret := roleSecretName(SandboxName(spec.Claim), claim.RoleTester)
	served := conflictSecretUpdates(g, secret, 2, func(s *corev1.Secret) { s.Data[GitHubHostsKey] = []byte(ghconfig.Hosts("ghs_refreshed_lease")) })
	g.launcher(spec.Claim)

	err := failedLaunch(t, g, spec)

	if err == nil || !strings.Contains(err.Error(), "bind its role launchers to the new pod") || !apierrors.IsConflict(err) {
		t.Fatalf("Spawn = %v, want the launch failed on the binding's conflict", err)
	}
	if served.Load() != 2 {
		t.Fatalf("the conflict reactor served %d conflicts, want the write and its one retry", served.Load())
	}
}

// A launcher's own hello is accepted even when the runtime's configured project
// (Options.Project, the legion.dev/project label's value) carries characters a claim token
// drops: ProjectToken strips everything outside [a-z0-9] (LEGION-565's claim.ProjectToken), and
// a project naming a claim or a Sandbox must go through it first. A deployment's own project
// name may carry such characters (claim.ProjectToken's doc: "sjawhar/legion" names the same role
// topics under either daemon), and Stage 4a's root-ready configures Options.Project with the
// run's own dashed label while its claim tokens use the stripped form the same way, which is
// this test's scenario (LEGION-462 e2e: scripts/e2e/stage4a-sandbox-runtime.sh's root-ready).
func TestALauncherHelloIsAcceptedWhenTheConfiguredProjectCarriesCharactersAClaimTokenDrops(t *testing.T) {
	dashed := "s4a-dashed-project"
	normalized, err := claim.ProjectToken(dashed)
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t, nil, withOptions(func(o *Options) { o.Project = dashed }))
	token, err := claim.NewToken(normalized, testTree, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t, token, claim.RoleArchitect, testTree)
	spec.Project = normalized
	loc := g.spawn(spec)
	name := loc.Sandbox.Name
	hello := shimwire.LauncherHello{
		Token:      string(g.secret(roleSecretName(name, claim.RoleArchitect)).Data[LauncherTokenFile]),
		Sandbox:    name,
		Role:       string(claim.RoleArchitect),
		PodUID:     loc.Sandbox.PodUID,
		LauncherID: "l-1",
	}
	if handler, reason := g.r.LauncherResolver()(hello); handler == nil {
		t.Fatalf("the architect's own launcher, of a project whose label carries characters its claim token drops, was refused: %s", reason)
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

// Every role's launcher token projection, private credential directory, state directory,
// agent-secrets key directory and gh volume are mounted in that role's container alone: no other
// role, nor an init container, can read another role's credentials, the GitHub App token in its gh
// files included. The issue's checkout, tree and sessions are what roles share. The gh volume
// projects the role Secret's two gh keys as the files gh reads, hosts.yml and config.yml, and the
// launcher volume the launcher token alone.
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
		for _, volume := range []string{
			roleVolume("launcher", role), roleVolume("private", role), roleVolume(stateVolume, role),
			roleVolume(agentSecretsKeyVolume, role), roleVolume("gh", role),
		} {
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
	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	for _, role := range claim.Roles {
		launcher := volumes[roleVolume("launcher", role)].Secret
		if launcher == nil || len(launcher.Items) != 1 || launcher.Items[0].Key != LauncherTokenFile {
			t.Errorf("%s's launcher volume projects %+v, want only the launcher token", role, launcher)
		}
		gh := volumes[roleVolume("gh", role)].Secret
		want := []corev1.KeyToPath{{Key: GitHubHostsKey, Path: ghconfig.HostsFile}, {Key: GitHubConfigKey, Path: ghconfig.ConfigFile}}
		if gh == nil || gh.SecretName != roleSecretName(l.name, role) || !slices.Equal(gh.Items, want) || gh.DefaultMode == nil || *gh.DefaultMode != 0o440 {
			t.Errorf("%s's gh volume projects %+v, want %s's %s as %s and %s as %s, read-only", role, gh, roleSecretName(l.name, role),
				GitHubHostsKey, ghconfig.HostsFile, GitHubConfigKey, ghconfig.ConfigFile)
		}
		mount := mountHolding(containerNamed(t, pod, string(role)), GHConfigDir)
		if mount == nil || mount.Name != roleVolume("gh", role) || mount.MountPath != GHConfigDir || !mount.ReadOnly {
			t.Errorf("%s's container mounts %+v at %s, want its gh volume read-only", role, mount, GHConfigDir)
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
