package sandbox

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var controllerToken = claim.ControllerToken(testProject)

// controllerSession is a session the controller saved, as the supervisor records it: under Oh My
// Pi's sessions directory, which the controller's launcher mounts from its volume's sessions
// directory, as every launcher does.
const controllerSession = ompSessionsDir + "/--legion--/2026-10-06T12-00-00-000Z_0001.jsonl"

// controllerSpec is the project controller's launch under `controller: daemon`: its role, its
// token, and no issue, tree, repository or git identity.
func controllerSpec(t *testing.T) runtime.SpawnSpec {
	t.Helper()
	spec := testSpec(t, controllerToken, claim.RoleController, "")
	spec.Tree, spec.Env, spec.Repository = "", map[string]string{}, ghrepo.Repository{}
	return spec
}

// The controller's pod is an issue pod with a one-role list: the controller's launcher alone, on a
// volume of its own the Sandbox owns, so a relaunch resumes its session. It provisions no
// workspace and holds no repository credential: its one init container makes the sessions
// directory and holds a resume to its session, and its labels name the controller and no tree or
// issue, so a tree pod's anti-affinity never counts it and it has no affinity of its own. It is
// never enrolled with the secrets broker, even where every workflow role's launcher is.
func TestTheControllersPodIsOneLauncherOnAVolumeOfItsOwn(t *testing.T) {
	opts := goldenOptions()
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := controllerSpec(t)
	spec.Generation, spec.ResumeSessionFile = 2, controllerSession
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatalf("prepare the controller's launch: %v", err)
	}
	if want := []claim.Role{claim.RoleController}; !slices.Equal(l.roles, want) {
		t.Fatalf("the controller's pod runs the roles %v, want %v", l.roles, want)
	}

	s := r.sandboxManifest(l)
	if s.Name != "legion-legion-controller" {
		t.Errorf("the controller's Sandbox is %s, want legion-legion-controller", s.Name)
	}
	if len(s.Spec.VolumeClaimTemplates) != 1 || s.Spec.VolumeClaimTemplates[0].Metadata.Name != treeVolume {
		t.Fatalf("the controller's Sandbox claims %+v, want its own %s volume", s.Spec.VolumeClaimTemplates, treeVolume)
	}
	for _, labels := range []map[string]string{s.Labels, r.podTemplate(l, false).Metadata.Labels} {
		if labels[labelProject] != testProject || labels[labelRole] != string(claim.RoleController) {
			t.Errorf("labels %v, want the project and the controller role", labels)
		}
		for _, key := range []string{labelTree, labelIssue} {
			if _, ok := labels[key]; ok {
				t.Errorf("labels carry %s (%v): a tree pod's anti-affinity would take the controller for another tree", key, labels)
			}
		}
		if roles, err := podRoles(labels); err != nil || !slices.Equal(roles, l.roles) {
			t.Errorf("labels %v name the roles %v (%v), want the pod's %v", labels, roles, err, l.roles)
		}
	}

	pod := r.podTemplate(l, true).Spec
	if pod.Affinity != nil {
		t.Errorf("affinity %+v, want none: the controller shares no tree volume", pod.Affinity)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != initContainer {
		t.Fatalf("init containers %v, want %s alone", pod.InitContainers, initContainer)
	}
	init := pod.InitContainers[0]
	if want := []string{opts.Tools.Legion, "workspace-init", "controller", "--root", TreeRoot}; !slices.Equal(init.Command, want) {
		t.Errorf("init command %q, want %q", init.Command, want)
	}
	if got := envOf(init)["LEGION_RESUME_SESSION_FILE"]; got != TreeRoot+"/"+SessionsSubPath+"/--legion--/2026-10-06T12-00-00-000Z_0001.jsonl" {
		t.Errorf("init LEGION_RESUME_SESSION_FILE = %q, want the session on the controller's volume", got)
	}
	if mounts := init.VolumeMounts; len(mounts) != 1 || mounts[0].Name != treeVolume || mounts[0].MountPath != TreeRoot {
		t.Errorf("init mounts %+v, want the controller's volume at %s alone", mounts, TreeRoot)
	}

	if len(pod.Containers) != 1 || pod.Containers[0].Name != string(claim.RoleController) {
		t.Fatalf("containers %v, want the controller's launcher alone", pod.Containers)
	}
	launcher := pod.Containers[0]
	if launcher.WorkingDir != TreeRoot {
		t.Errorf("working directory %s, want its volume's root %s", launcher.WorkingDir, TreeRoot)
	}
	if want := []string{
		opts.Tools.Legion, "launcher", connectFlag, opts.StreamURL, "--token-file", LauncherDir + "/" + LauncherTokenFile,
		"--sandbox", s.Name, "--role", string(claim.RoleController),
	}; !slices.Equal(launcher.Command[:len(want)], want) {
		t.Errorf("launcher command %q, want it to begin %q", launcher.Command, want)
	}
	for _, m := range launcher.VolumeMounts {
		if m.Name == agentSecretsTokenVolume || strings.HasPrefix(m.Name, agentSecretsKeyVolume) {
			t.Errorf("the controller's launcher mounts %s: it holds no human-tier key", m.Name)
		}
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	for _, name := range []string{provisionVolume, feedVolume, tempVolume, agentSecretsTokenVolume, roleVolume(agentSecretsKeyVolume, claim.RoleController)} {
		if _, ok := volumes[name]; ok {
			t.Errorf("the controller's pod has the %s volume, which only provisioning a workspace or enrolling a key needs", name)
		}
	}
	for _, role := range claim.Roles {
		for _, prefix := range []string{"launcher", "private", stateVolume} {
			if _, ok := volumes[roleVolume(prefix, role)]; ok {
				t.Errorf("the controller's pod has %s's %s volume", role, prefix)
			}
		}
	}
	for _, prefix := range []string{"launcher", "private", stateVolume} {
		if _, ok := volumes[roleVolume(prefix, claim.RoleController)]; !ok {
			t.Errorf("the controller's pod lacks its launcher's %s volume", prefix)
		}
	}
	if v := volumes[treeVolume]; v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != TreeClaimName(controllerToken) {
		t.Errorf("its volume %+v, want the claim of its own Sandbox, %s", v.VolumeSource, TreeClaimName(controllerToken))
	}
	if secret := volumes[roleVolume("launcher", claim.RoleController)].Secret; secret == nil || secret.SecretName != roleSecretName(s.Name, claim.RoleController) {
		t.Errorf("its launcher's token volume %+v, want the role Secret %s", secret, roleSecretName(s.Name, claim.RoleController))
	}
}

// The controller's agent runs the way every role's does — its launcher starts the shim on the pod
// baseline, and the shim Oh My Pi in RPC mode with its prompt — and is told it is the controller,
// and nothing of a tree, an issue, a workspace, GitHub or the secrets broker.
func TestTheControllersAgentIsToldItIsTheControllerAndNothingOfATree(t *testing.T) {
	opts := goldenOptions()
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := controllerSpec(t)
	spec.Generation, spec.ResumeSessionFile = 2, controllerSession
	worker := workerOf(t, r, spec, false)
	env := envOf(worker)
	for name, want := range map[string]string{
		"LEGION_CONTROLLER": "1", "LEGION_ROLE": string(claim.RoleController), "LEGION_PROJECT": testProject,
		"LEGION_DAEMON_URL": opts.DaemonURL, "LEGION_STATE_DIR": StateDir, "ENVOY_URL": opts.EnvoyURL,
		"ENVOY_NATS_URL": opts.NATSURLs[0], "LEGION_BOOT_TOKEN_FILE": generationDir(2) + "/" + bootTokenKey,
		"ENVOY_TOKEN_FILE": generationDir(2) + "/ENVOY_TOKEN", "LEGION_GRANT_FILE": runtime.GrantFile(StateDir, controllerToken),
		"LEGION_GENERATION": "2",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	for _, name := range []string{
		"LEGION_TREE", "LEGION_ISSUE", "LEGION_WORKSPACE", "LEGION_GH_PATH", "LEGION_GIT_PATH", "LEGION_JJ_PATH",
		"LEGION_CREDENTIAL_HELPER", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR", "UV_LINK_MODE", "AGENT_SECRETS_URL",
		"AGENT_SECRETS_KEY_DIR",
	} {
		if value, ok := env[name]; ok {
			t.Errorf("the controller is told %s=%q, which only a tree agent has", name, value)
		}
	}
	command := strings.Join(worker.Command, " ")
	for _, want := range []string{" worker-shim --connect " + opts.StreamURL, " --pod-safety ", " --mode rpc --append-system-prompt "} {
		if !strings.Contains(command, want) {
			t.Errorf("command %q, want %q", command, want)
		}
	}
	if strings.Contains(command, "--agent-secrets-key-dir") {
		t.Errorf("command %q enrolls the controller with the secrets broker; it holds no human-tier key", command)
	}
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	if start := launcherCommand(l, r); start.ResumeFile != controllerSession || start.Files[bootTokenKey] != spec.BootToken {
		t.Errorf("start command resumes %q with boot token %q, want %q and the launch's own", start.ResumeFile, start.Files[bootTokenKey], controllerSession)
	}
}

// A controller launch takes no tree's launch turn and mints no provisioning token: it has no
// repository, so a GitHub token source that would refuse every mint leaves it running. It writes
// no provisioning Secret, only its one launcher's role Secret, and its boot token reaches its
// launcher in the start command, as every role's does.
func TestAControllerLaunchMintsNoProvisioningToken(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = refusingTokens{} }))
	spec := controllerSpec(t)
	g.spawn(spec)
	name := SandboxName(controllerToken)
	if secret := g.secret(secretName(name)); secret != nil {
		t.Errorf("the controller's launch wrote the provisioning Secret %s: %v", secretName(name), secret.Data)
	}
	if secret := g.secret(roleSecretName(name, claim.RoleController)); secret == nil || len(secret.Data[LauncherTokenFile]) == 0 {
		t.Fatalf("the controller's launch wrote no launcher token in %s: %+v", roleSecretName(name, claim.RoleController), secret)
	}
	for _, role := range claim.Roles {
		if g.secret(roleSecretName(name, role)) != nil {
			t.Errorf("the controller's launch wrote %s's role Secret", role)
		}
	}
	var started *shimwire.LauncherStart
	for _, frame := range g.sent(controllerToken) {
		if start, ok := frame.(shimwire.LauncherStart); ok {
			started = &start
		}
	}
	if started == nil || started.Files[bootTokenKey] != spec.BootToken || started.Files["ENVOY_TOKEN"] != "envoy-bearer" {
		t.Fatalf("the controller's launcher was started with %+v, want its boot token and launch secrets", started)
	}
}

// The controller's pod is sized by its own entry under runtime.kubernetes.resources, as a workflow
// role's pod is by that role's: both its containers carry it, so an operator who sizes it moves it
// out of the BestEffort class the kubelet evicts first.
func TestTheControllersPodTakesItsOwnResources(t *testing.T) {
	opts := goldenOptions()
	sized := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	opts.Resources = map[claim.Role]corev1.ResourceRequirements{claim.RoleController: sized}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, controllerSpec(t), false)
	if containers := slices.Concat(pod.InitContainers, pod.Containers); len(containers) != 2 {
		t.Fatalf("the controller's pod runs %d containers, want its init container and its launcher", len(containers))
	}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if !reflect.DeepEqual(c.Resources, sized) {
			t.Errorf("container %s carries resources %+v, want the controller's %+v", c.Name, c.Resources, sized)
		}
	}
}

// A controller pod is never enrolled with the secrets broker, so under a runtime that enrolls every
// workflow role it carries no AGENT_SECRETS_URL, and that is exactly what a controller launched
// now is handed: holding none is no address that moved. It probes Alive while the daemon's
// addresses stay put, and a daemon restarted at the same addresses re-adopts it as it is, rather
// than replacing it at every observation.
func TestAControllerPodIsNotStaleForWantOfTheBrokersAddress(t *testing.T) {
	broker := func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	}
	g := newRig(t, nil, withOptions(broker))
	loc := g.spawn(controllerSpec(t))
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the controller right after its launch: %+v, %v; want Alive", obs, err)
	}
	r2 := secondRuntime(t, g, broker)()
	g.connectRunning(controllerToken, loc.Sandbox.Generation)
	g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	if obs := readopt(t, g.ctx, r2, loc)[0]; obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("a daemon restarted at the same addresses re-adopts the controller as %s (%s), want Alive", obs.Kind, obs.Detail)
	}
}

// The controller's Sandbox is its claim's alone, so its Release deletes it, with its Secrets and
// volume through their owner references. Releasing a workflow claim still leaves its issue pod to
// the tree's cleanup (TestReleaseNeverDeletesTheIssueSandbox).
func TestReleasingTheControllerDeletesItsSandbox(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(controllerSpec(t))
	issue := g.spawn(workerSpec(t))
	if err := g.r.Release(g.ctx, runtime.Known{Claim: controllerToken, Locator: &loc}); err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(loc.Sandbox.Name); s != nil {
		t.Fatalf("the released controller's Sandbox %s is still there (%s)", s.Name, s.mode())
	}
	if g.sandbox(issue.Sandbox.Name) == nil {
		t.Fatal("releasing the controller deleted an issue's Sandbox")
	}
	if err := g.r.Release(g.ctx, runtime.Known{Claim: controllerToken}); err != nil {
		t.Fatalf("releasing the controller again: %v", err)
	}
}

// A launcher on the controller's pod is accepted only as the controller, with its pod's current
// token: a workflow role is no role of that pod, and the controller is no role of an issue pod.
func TestTheLauncherResolverAcceptsTheControllersLauncherOnItsOwnPodOnly(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(controllerSpec(t))
	issue := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name
	token := string(g.secret(roleSecretName(name, claim.RoleController)).Data[LauncherTokenFile])
	accept := g.r.LauncherResolver()
	valid := shimwire.LauncherHello{Token: token, Sandbox: name, Role: string(claim.RoleController), PodUID: loc.Sandbox.PodUID, LauncherID: "l-1"}
	if handler, reason := accept(valid); handler == nil {
		t.Fatalf("the controller's own launcher was refused: %s", reason)
	}
	for label, edit := range map[string]func(*shimwire.LauncherHello){
		"a workflow role on the controller's pod": func(h *shimwire.LauncherHello) { h.Role = string(claim.RoleArchitect) },
		"a pod the binding does not name":         func(h *shimwire.LauncherHello) { h.PodUID = "uid-pod-elsewhere" },
		"a token of no launcher":                  func(h *shimwire.LauncherHello) { h.Token = "not-the-token" },
		"the controller on an issue's pod": func(h *shimwire.LauncherHello) {
			h.Sandbox, h.PodUID = issue.Sandbox.Name, issue.Sandbox.PodUID
		},
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

// The daemon tells the orphan sweep of every claim it has not retired (internal/daemon's
// knownClaims), a suspended one with no locator. The controller's one-role Sandbox holds its saved
// session, so a known controller claim keeps it, running or suspended; once the claim is retired,
// and so unknown, the sweep deletes it. A known claim of another Sandbox or role keeps nothing of
// the controller's.
func TestTheOrphanSweepKeepsAKnownControllersSandboxAndDeletesAnUnknownOnes(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(controllerSpec(t))
	worker := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name

	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: controllerToken, Locator: &loc}, {Claim: workerToken, Locator: &worker}}, 0); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(name) == nil {
		t.Fatal("the sweep deleted the Sandbox of a running controller")
	}
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: controllerToken}, {Claim: workerToken, Locator: &worker}}, 0); err != nil {
		t.Fatal(err)
	}
	if g.sandbox(name) == nil {
		t.Fatal("the sweep deleted the Sandbox of a suspended controller, known with no locator: its saved session went with it")
	}

	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &worker}}, 0); err != nil {
		t.Fatal(err)
	}
	g.eventually("the retired controller's Sandbox to be deleted", func() bool { return g.sandbox(name) == nil })
	if g.sandbox(worker.Sandbox.Name) == nil {
		t.Fatal("the sweep deleted the live tree's issue Sandbox")
	}
}

// refusingTokens refuses every installation token, as a GitHub outage would.
type refusingTokens struct{}

func (refusingTokens) Token(context.Context, string) (string, error) {
	return "", errors.New("no installation token: GitHub is down")
}
