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
)

var controllerToken = claim.ControllerToken(testProject)

// controllerSpec is the project controller's launch under `controller: daemon`: its role, its
// token, and no issue, tree, repository or git identity.
func controllerSpec(t *testing.T) runtime.SpawnSpec {
	t.Helper()
	spec := testSpec(t, controllerToken, claim.RoleController, "")
	spec.Tree, spec.Env, spec.Repository = "", map[string]string{}, ghrepo.Repository{}
	return spec
}

// The controller's pod runs the agent the way every pod does — the shim, the pod baseline, Oh My
// Pi in RPC mode with its prompt — on a volume of its own the Sandbox owns, so a relaunch resumes
// its session. It provisions no workspace and holds no repository credential: its one init
// container makes the sessions directory and holds a resume to its session, and its agent is told
// it is the controller, and nothing of a tree, an issue, a workspace or GitHub.
func TestTheControllersPodRunsOnAVolumeOfItsOwnWithNoWorkspace(t *testing.T) {
	opts := goldenOptions()
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := controllerSpec(t)
	spec.Generation, spec.ResumeSessionFile = 2, ompSessionsDir+"/--legion--/2026-10-06T12-00-00-000Z_0001.jsonl"
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatalf("prepare the controller's launch: %v", err)
	}

	s := r.sandboxManifest(l)
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
	for _, v := range pod.Volumes {
		switch v.Name {
		case provisionVolume, feedVolume, tempVolume:
			t.Errorf("the controller's pod has the %s volume, which only provisioning a workspace needs", v.Name)
		case treeVolume:
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != TreeClaimName(controllerToken) {
				t.Errorf("its volume %+v, want the claim of its own Sandbox, %s", v.VolumeSource, TreeClaimName(controllerToken))
			}
		}
	}

	main := containerNamed(t, pod, mainContainer)
	if main.WorkingDir != TreeRoot {
		t.Errorf("working directory %s, want its volume's root %s", main.WorkingDir, TreeRoot)
	}
	env := envOf(main)
	for name, want := range map[string]string{
		"LEGION_CONTROLLER": "1", "LEGION_ROLE": string(claim.RoleController), "LEGION_PROJECT": testProject,
		"LEGION_DAEMON_URL": opts.DaemonURL, "LEGION_STATE_DIR": StateDir, "ENVOY_URL": opts.EnvoyURL,
		"ENVOY_NATS_URL": opts.NATSURLs[0], "LEGION_BOOT_TOKEN_FILE": BootDir + "/" + bootTokenKey,
		"ENVOY_TOKEN_FILE": BootDir + "/ENVOY_TOKEN", "LEGION_GRANT_FILE": runtime.GrantFile(StateDir, controllerToken),
		"LEGION_GENERATION": "2",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	for _, name := range []string{
		"LEGION_TREE", "LEGION_ISSUE", "LEGION_WORKSPACE", "LEGION_GH_PATH", "LEGION_GIT_PATH", "LEGION_JJ_PATH",
		"LEGION_CREDENTIAL_HELPER", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR", "AGENT_SECRETS_URL",
	} {
		if value, ok := env[name]; ok {
			t.Errorf("the controller is told %s=%q, which only a tree agent has", name, value)
		}
	}
	command := strings.Join(main.Command, " ")
	for _, want := range []string{" worker-shim --connect " + opts.StreamURL, " --pod-safety ", " --mode rpc --append-system-prompt "} {
		if !strings.Contains(command, want) {
			t.Errorf("command %q, want %q", command, want)
		}
	}
	if strings.Contains(command, "--agent-secrets-key-dir") {
		t.Errorf("command %q enrolls the controller with the secrets broker; it holds no human-tier key", command)
	}
}

// A controller launch takes no tree's launch turn and mints no provisioning token: it has no
// repository, so a GitHub token source that would refuse every mint leaves it running, and its
// Secret holds its boot token and launch secrets alone.
func TestAControllerLaunchMintsNoProvisioningToken(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens = refusingTokens{} }))
	g.spawn(controllerSpec(t))
	name := SandboxName(controllerToken)
	secret := g.secret(secretName(name))
	if secret == nil {
		t.Fatal("the controller's launch wrote no Secret")
	}
	if _, ok := secret.Data[provisionTokenKey]; ok {
		t.Errorf("the controller's Secret holds %s", provisionTokenKey)
	}
	for _, key := range []string{bootTokenKey, "ENVOY_TOKEN"} {
		if len(secret.Data[key]) == 0 {
			t.Errorf("the controller's Secret lacks %s", key)
		}
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
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if !reflect.DeepEqual(c.Resources, sized) {
			t.Errorf("container %s carries resources %+v, want the controller's %+v", c.Name, c.Resources, sized)
		}
	}
}

// A controller pod is never enrolled with the secrets broker, so under a runtime that enrolls every
// worker pod it carries no AGENT_SECRETS_URL, and that is exactly what a controller pod launched
// now is handed: holding none is no address that moved. Its pod probes Alive while the daemon's
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
	if obs := readopt(t, g.ctx, secondRuntime(t, g, broker), loc); obs.Kind != runtime.Alive {
		t.Fatalf("a daemon restarted at the same addresses re-adopts the controller as %s (%s), want Alive", obs.Kind, obs.Detail)
	}
}

// refusingTokens refuses every installation token, as a GitHub outage would.
type refusingTokens struct{}

func (refusingTokens) Token(context.Context, string) (string, error) {
	return "", errors.New("no installation token: GitHub is down")
}
