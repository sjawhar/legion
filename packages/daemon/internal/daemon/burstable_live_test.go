//go:build e2e

// The Burstable roles live proof's driver (scripts/e2e/burstable-kind.sh, which builds it with
// `-tags e2e`, owns the throwaway kind cluster it runs on, runs `legion start --check-config` on
// the files this writes, and reads the pods' QoS class back with kubectl). Two legion.yaml files,
// one giving the implementer a memory request below its limit and one with no `resources` block,
// are each read by the daemon's own loader and translated by the daemon's own sandboxOptions, and
// the Agent Sandbox runtime they configure launches one issue's root on the cluster, through Agent
// Sandbox's controller and the gvisor runtime class. What the proof stands in for lies outside the
// reservation path: the agent is a sleep stub (no model, no Dispatch, no NATS), the store answers
// as the daemon's does for an issue's first launch, the repository is a public one GitHub clones
// without a credential, and the roles' gh files hold a placeholder token.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// burstableLiveRepository is a public repository, so the pod's workspace-fetch clones it without
// GitHub asking for the placeholder provisioning token; its default branch is master.
var burstableLiveRepository = ghrepo.MustParse("octocat/Hello-World")

// burstableLiveStub is the agent every launched shim wraps: it sleeps, so the pod stays up for
// kubectl to read and nothing dials a model, Dispatch or NATS.
var burstableLiveStub = []string{"/bin/sh", "-c", "exec sleep infinity", "burstable-stub"}

// burstableLiveCases are the two deployments the proof launches: the implementer requesting 8Gi
// of memory under a 32Gi limit, whose issue pod must be Burstable, and a file with no `resources`
// block, whose issue pod must stay Guaranteed at the daemon's defaults.
var burstableLiveCases = []struct {
	name, project, resources string
	qos                      corev1.PodQOSClass
	implementer              corev1.ResourceRequirements
}{
	{
		name: "burstable", project: "BURST", qos: corev1.PodQOSBurstable,
		resources: "    resources:\n      implementer: {memory: 32Gi, memory_request: 8Gi}\n",
		implementer: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("20Gi")},
		},
	},
	{
		name: "guaranteed", project: "GUARD", qos: corev1.PodQOSGuaranteed,
		implementer: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("6Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("6Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("20Gi")},
		},
	},
}

// TestBurstableRoleRendersABurstablePodLive launches an issue pod from each case's configuration
// and holds it to the case's reservation and QoS class, as the API server recorded them. It writes
// `<case> <namespace>/<pod> <legion.yaml>` per case to LEGION_E2E_RESULT for the script.
func TestBurstableRoleRendersABurstablePodLive(t *testing.T) {
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST", "LEGION_E2E_WORK", "LEGION_E2E_RESULT"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/burstable-kind.sh sets it", name)
		}
	}
	// The proof creates pods, so it runs only on a kind cluster, the one its script made, and
	// never on whatever the operator's current context is.
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var results []string
	for _, tc := range burstableLiveCases {
		started := time.Now()
		path := writeBurstableLiveConfig(t, env, tc.name, tc.project, tc.resources)
		cfg, err := config.LoadForValidation(path, nil)
		if err != nil {
			t.Fatalf("%s: load %s: %v", tc.name, path, err)
		}
		deployment := Deployment(cfg)
		fmt.Printf("== %s: %s\n", tc.name, path)
		fmt.Printf("   resource-limits row: %s\n", states(deployment.Report())["resource-limits"].Detail)
		pod := launchBurstableLiveRoot(ctx, t, cfg, log)
		implementer := liveContainer(t, pod, string(claim.RoleImplementer))
		fmt.Printf("   pod %s/%s: qosClass %s, runtimeClassName %s, node %s, PodScheduled %s\n", pod.Namespace, pod.Name,
			pod.Status.QOSClass, liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName, liveCondition(pod, corev1.PodScheduled))
		fmt.Printf("   implementer container: requests cpu %s memory %s; limits cpu %s memory %s\n",
			implementer.Resources.Requests.Cpu(), implementer.Resources.Requests.Memory(), implementer.Resources.Limits.Cpu(), implementer.Resources.Limits.Memory())
		if pod.Status.QOSClass != tc.qos {
			t.Errorf("%s: pod %s is %q, want %s", tc.name, pod.Name, pod.Status.QOSClass, tc.qos)
		}
		for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			if got, want := implementer.Resources.Requests[kind], tc.implementer.Requests[kind]; got.Cmp(want) != 0 {
				t.Errorf("%s: the implementer requests %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
			if got, want := implementer.Resources.Limits[kind], tc.implementer.Limits[kind]; got.Cmp(want) != 0 {
				t.Errorf("%s: the implementer is limited to %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
		}
		if scheduled := liveCondition(pod, corev1.PodScheduled); scheduled != corev1.ConditionTrue {
			t.Errorf("%s: pod %s PodScheduled is %q, want True", tc.name, pod.Name, scheduled)
		}
		fmt.Printf("ok %s in %s\n", tc.name, time.Since(started).Round(time.Second))
		results = append(results, fmt.Sprintf("%s %s/%s %s", tc.name, pod.Namespace, pod.Name, path))
	}
	if err := os.WriteFile(env["LEGION_E2E_RESULT"], []byte(strings.Join(results, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeBurstableLiveConfig writes the case's legion.yaml, and the token files it names, under
// LEGION_E2E_WORK/<name>, with resources appended to its runtime.kubernetes block. The hosts no
// stub dials are documentation placeholders; the worker stream binds LEGION_E2E_STREAM_HOST.
func writeBurstableLiveConfig(t *testing.T, env map[string]string, name, project, resources string) string {
	t.Helper()
	dir := filepath.Join(env["LEGION_E2E_WORK"], name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"envoy-token", "operator-token", "dispatch-token"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("placeholder-"+file+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := env["LEGION_E2E_STREAM_HOST"]
	body := fmt.Sprintf(`project: %s
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %s
bind: %s
daemon_url: http://%s:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
worker_boot_timeout_seconds: 300
projects:
  %s: { repo: %s }
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: %s
    storage_class: standard
    kubeconfig: %s
    context: %s
%s`, project, filepath.Join(dir, "state"), host, host, project, burstableLiveRepository, env["LEGION_E2E_IMAGE"],
		env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"], resources)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// launchBurstableLiveRoot builds the Agent Sandbox runtime from cfg as boot does — the cluster
// check, then sandboxOptions over the loaded configuration — on a worker stream at cfg's bind,
// launches the root architect of an issue of cfg's project through the runtime's Spawn, and returns
// the issue pod as the API server holds it once the launch returned.
func launchBurstableLiveRoot(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger) *corev1.Pod {
	t.Helper()
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := kubeClient(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.CheckInstalled(ctx, rc, agentSandbox); err != nil {
		t.Fatalf("the cluster check: %v", err)
	}
	opts, err := sandboxOptions(cfg, k, project, "placeholder-dispatch-token", lookup(nil), log)
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	issue := cfg.Project + "-1"
	token, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	bootToken := "boot-" + string(token)
	resolve := func(presented string) (claim.Token, uint64, bool, bool, error) {
		if presented != bootToken {
			return "", 0, false, false, nil
		}
		return token, 1, false, true, nil
	}
	listener, err := stream.Listen(ctx, "tcp://"+cfg.Bind+":0", resolve, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		t.Fatalf("the worker stream: %v", err)
	}
	go func() {
		for range listener.Events() {
		}
	}()
	opts.Conns, opts.StreamURL, opts.Store = listener, listener.Addr(), firstLaunchStore{}
	opts.Tokens = publicCloneTokens{}
	opts.GitHubCredential = func(_ context.Context, role claim.Role, owner string) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_placeholder", string(appauth.AppRoleFor(role)), time.Now().Add(time.Hour))
		return ghconfig.Bundle{Default: owner, Owners: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	opts.Agent = burstableLiveStub
	rt, err := sandbox.New(ctx, rc, opts)
	if err != nil {
		t.Fatalf("sandbox.New: %v", err)
	}
	listener.SetLauncherResolver(rt.LauncherResolver())

	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the architect.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := rt.Spawn(ctx, runtime.SpawnSpec{
		Claim: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleArchitect, Generation: 1, BootToken: bootToken,
		Secrets: map[string]string{"ENVOY_TOKEN": "placeholder-envoy-token"},
		Prompt:  runtime.PromptParts{RolePromptPaths: []string{prompt}}, Repository: burstableLiveRepository, DefaultBranch: "master",
	})
	if err != nil {
		t.Fatalf("Spawn %s: %v", token, err)
	}
	clients, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	pods, err := clients.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if string(pods.Items[i].UID) == loc.Sandbox.PodUID {
			return &pods.Items[i]
		}
	}
	t.Fatalf("no pod in %s has the launched uid %s", k.Namespace, loc.Sandbox.PodUID)
	return nil
}

// firstLaunchStore answers the runtime as the daemon's store does for an issue's first launch:
// no session recorded, its tree live, no other tree running it, and its Sandbox its own tree's.
type firstLaunchStore struct{}

func (firstLaunchStore) IssueHasSessions(context.Context, string, string) (bool, error) {
	return false, nil
}

func (firstLaunchStore) TreeLive(context.Context, string, string) (bool, error) { return true, nil }

func (firstLaunchStore) IssueRunsElsewhere(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (firstLaunchStore) IssueSandboxHome(_ context.Context, _, issue string) (string, error) {
	return issue, nil
}

// publicCloneTokens is the provisioning token for a public repository, which GitHub never asks
// git for.
type publicCloneTokens struct{}

func (publicCloneTokens) Token(context.Context, string) (string, error) {
	return "public-repository-needs-no-token", nil
}

func liveContainer(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("pod %s has no container %s", pod.Name, name)
	return corev1.Container{}
}

func liveCondition(pod *corev1.Pod, kind corev1.PodConditionType) corev1.ConditionStatus {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == kind {
			return condition.Status
		}
	}
	return ""
}

func liveString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
