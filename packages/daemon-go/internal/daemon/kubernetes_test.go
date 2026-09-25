package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// kubernetesConfig is testConfig under runtime: kubernetes, its client a kubeconfig whose current
// context points at server.
func kubernetesConfig(t *testing.T, server string) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/sjawhar/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", TreeVolume: "20Gi", Kubeconfig: writeKubeconfig(t, server, "test"),
	}}
	return cfg
}

// writeKubeconfig is a kubeconfig of one cluster at server and one context, its current context
// when current names it.
func writeKubeconfig(t *testing.T, server, current string) string {
	t.Helper()
	body := "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: test\n  cluster:\n    server: " + server + "\n" +
		"users:\n- name: test\n  user:\n    token: kube-test-token\n" +
		"contexts:\n- name: test\n  context:\n    cluster: test\n    user: test\n"
	if current != "" {
		body += "current-context: " + current + "\n"
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}
	return path
}

// A cluster without Agent Sandbox is refused by name (LEGION-206 Requirement 10) before the boot
// is recorded: the CRD and the controller's Deployment are each named, and nothing is launched.
func TestAKubernetesDaemonRefusesAClusterWithoutAgentSandboxBeforeItsBoot(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
	}))
	defer apiServer.Close()
	cfg := kubernetesConfig(t, apiServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, cfg, quietLogger(), overrides{clock: stillClock{}})
	if err == nil {
		t.Fatal("the daemon booted on a cluster without Agent Sandbox")
	}
	for _, want := range []string{"sandboxes.agents.x-k8s.io", "agent-sandbox-system/agent-sandbox-controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot refusal = %q, want it to name %s", err, want)
		}
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Errorf("a refused boot was recorded %d times", count)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requested) == 0 {
		t.Error("the daemon asked the cluster nothing")
	}
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the install check sent %s; it may only read", request)
		}
	}
}

// The worker image is proven after the runtime is built and before the boot is recorded; a refusal
// refuses the boot.
func TestAKubernetesDaemonRefusesTheBootItsWorkerImageProbeRefuses(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	var probed runtime.Runtime
	o.probe = func(_ context.Context, built runtime.Runtime) error {
		probed = built
		return errors.New("worker image ghcr.io/sjawhar/legion-worker@sha256:aaaa refused: the plugin declares contract 3")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, cfg, quietLogger(), o)
	if err == nil || !strings.Contains(err.Error(), "the plugin declares contract 3") {
		t.Fatalf("boot = %v, want the image probe's refusal", err)
	}
	if probed != rt {
		t.Errorf("the probe was handed %v, want the runtime the daemon built", probed)
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Errorf("a boot whose image was refused was recorded %d times", count)
	}
}

// Under kubernetes the worker stream is TCP on the daemon's bind and worker_stream_port, the one
// address the runtime hands every pod's shim, and the runtime is given the workflow's App tokens.
// The host's own agent machinery never runs: no pane launcher is installed, no Dispatch token file
// is written, and the workflow's boot has no worker-bin stage.
func TestAKubernetesDaemonServesItsWorkerStreamOnTCPAndRunsNoHostPaneMachinery(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	tokens := &workflowTokenRecorder{}
	record := &built{}
	o := fakeRuntime(fake.NewRuntime(), record)
	o.workflowTokens = tokens
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger, o) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	awaitHealthz(t, cfg, done)

	record.mu.Lock()
	address, apps := record.address, record.apps
	record.mu.Unlock()
	stream := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.WorkerStreamPort))
	if address != "tcp://"+stream {
		t.Errorf("the runtime was told to have shims dial %q, want tcp://%s", address, stream)
	}
	if conn, err := net.DialTimeout("tcp", stream, time.Second); err != nil {
		t.Errorf("the worker stream does not accept on %s: %v", stream, err)
	} else {
		conn.Close()
	}
	if apps != tokens {
		t.Errorf("the runtime was handed App tokens %v, want the workflow's", apps)
	}
	for _, path := range []string{filepath.Join(cfg.StateDir, "bin"), filepath.Join(cfg.StateDir, "secrets", "dispatch-token")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists (%v): the host's pane machinery ran under kubernetes", path, err)
		}
	}
	if strings.Contains(logged.String(), `"stage":"worker-bin"`) {
		t.Errorf("the boot installed the pane launcher:\n%s", logged.String())
	}
}

// prepare refuses what the cluster would refuse only later: a kubeconfig with no current context
// when runtime.kubernetes.context names none, and a role's request above its limit.
func TestAKubernetesDaemonRefusesAConfigurationTheClusterWouldRefuseLater(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Kubernetes, *testing.T)
		want   string
	}{
		{
			name: "no current context",
			change: func(k *config.Kubernetes, t *testing.T) {
				k.Kubeconfig = writeKubeconfig(t, "https://127.0.0.1:1", "")
			},
			want: "runtime.kubernetes.context is required: the kubeconfig",
		},
		{
			name: "a context the kubeconfig does not have",
			change: func(k *config.Kubernetes, t *testing.T) {
				k.Kubeconfig, k.Context = writeKubeconfig(t, "https://127.0.0.1:1", ""), "production"
			},
			want: `context "production" does not exist`,
		},
		{
			name: "a request above its limit",
			change: func(k *config.Kubernetes, _ *testing.T) {
				k.Resources = map[claim.Role]config.RoleResources{claim.RoleImplementer: {
					Requests: config.Quantities{CPU: "2", Memory: "4Gi"}, Limits: config.Quantities{CPU: "1", Memory: "8Gi"},
				}}
			},
			want: "runtime.kubernetes.resources.implementer: the cpu request 2 is above its limit 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			tc.change(cfg.Runtime.Kubernetes, t)
			if _, err := prepare(cfg, quietLogger(), overrides{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("prepare = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// A daemon whose LEGION_ROLE_PROMPTS_DIR names no directory is refused before its boot, naming the
// variable and the directory, rather than failing later on the first read of it.
func TestADaemonRefusesARolePromptsDirectoryThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-roles")
	t.Setenv("LEGION_ROLE_PROMPTS_DIR", missing)
	_, err := prepare(kubernetesConfig(t, "https://127.0.0.1:1"), quietLogger(), overrides{})
	if err == nil || !strings.Contains(err.Error(), "LEGION_ROLE_PROMPTS_DIR") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("prepare = %v, want a refusal naming LEGION_ROLE_PROMPTS_DIR and %s", err, missing)
	}
}

// A Kubernetes daemon refuses, before its boot, an operator pod that collides with Legion's own —
// a mount at Legion's boot projection (the LEGION-270 plan's negative control), and a provider key
// the pointer to the daemon's Envoy bearer names — so no pod is ever built with it.
func TestAKubernetesDaemonRefusesAnOperatorPodCollidingWithLegionsBeforeItsBoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{
			name: "a mount at Legion's boot projection",
			change: func(cfg *config.Config) {
				cfg.Runtime.Kubernetes.Pod = config.PodConfig{
					Volumes:      []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}}},
					VolumeMounts: []corev1.VolumeMount{{Name: "creds", MountPath: "/var/run/legion/boot", ReadOnly: true}},
				}
			},
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/boot overlaps /var/run/legion/boot, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's",
		},
		{
			name: "a provider key the Envoy bearer's pointer names",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "ENVOY_TOKEN", Secret: "envoy"}}
			},
			want: "provider_keys names ENVOY_TOKEN, whose pointer ENVOY_TOKEN_FILE every launch sets (the pointer to the launch secret ENVOY_TOKEN): the shim skips a key whose pointer the pod sets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
			if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.change(&cfg)
			if _, err := prepare(cfg, quietLogger(), overrides{}); err == nil || err.Error() != tc.want {
				t.Fatalf("prepare = %v, want %q", err, tc.want)
			}
		})
	}
}

// runtime.kubernetes.pod and provider_keys reach the runtime's Options, each piece of the pod
// as configured and each provider key as its variable's Secret key: the manifests the runtime
// builds from them are the sandbox package's to test, and this is the one place the daemon hands
// them over.
func TestTheOperatorsPodReachesTheSandboxRuntime(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.ProviderKeys = []config.ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "anthropic"}}
	pod := config.PodConfig{
		Env:            map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		ServiceAccount: "legion-worker",
		Volumes:        []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}}},
		VolumeMounts:   []corev1.VolumeMount{{Name: "creds", MountPath: "/etc/legion-operator/creds", ReadOnly: true}},
	}
	cfg.Runtime.Kubernetes.Pod = pod
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want := sandbox.Pod{Env: pod.Env, Volumes: pod.Volumes, VolumeMounts: pod.VolumeMounts, ServiceAccount: pod.ServiceAccount}
	if !reflect.DeepEqual(opts.Pod, want) {
		t.Errorf("the runtime's Pod is %+v, want %+v", opts.Pod, want)
	}
	if keys := map[string]string{"ANTHROPIC_API_KEY": "anthropic"}; !reflect.DeepEqual(opts.ProviderKeys, keys) {
		t.Errorf("the runtime's ProviderKeys are %v, want %v", opts.ProviderKeys, keys)
	}
}

// awaitHealthz returns once the daemon answers /healthz, and fails if it exits first.
func awaitHealthz(t *testing.T, cfg config.Config, done chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("the daemon exited before it answered /healthz: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never answered /healthz: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Each duration key reaches the runtime option that takes it, under either runtime: every value
// here is distinct, so feeding an option from its neighbouring key fails a row.
// worker_stop_timeout_seconds in particular has no other test: it is tmux's stop grace and the
// sandbox's termination grace.
func TestEveryDurationKeyReachesTheRuntimeOptionThatTakesIt(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.WorkerStopTimeout, cfg.WorkerBootTimeout, cfg.ProbeInterval, cfg.SlowCommandTimeout = 11*time.Second, 22*time.Second, 33*time.Second, 44*time.Second
	cfg.WorkerBootRegistrationDeadlineIntervals = 5

	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	panes := tmuxOptions(cfg, "test", "omp", "", "", nil, quietLogger())
	for _, row := range []struct {
		option    string
		got, want time.Duration
	}{
		{"sandbox TerminationGrace", opts.TerminationGrace, cfg.WorkerStopTimeout},
		{"sandbox BootTimeout", opts.BootTimeout, cfg.WorkerBootTimeout},
		{"sandbox ProbeInterval", opts.ProbeInterval, cfg.ProbeInterval},
		{"sandbox AdoptTimeout", opts.AdoptTimeout, cfg.SlowCommandTimeout},
		{"tmux StopGrace", panes.StopGrace, cfg.WorkerStopTimeout},
		{"tmux ProbeInterval", panes.ProbeInterval, cfg.ProbeInterval},
		{"tmux AdoptTimeout", panes.AdoptTimeout, cfg.SlowCommandTimeout},
	} {
		if row.got != row.want {
			t.Errorf("%s = %s, want %s", row.option, row.got, row.want)
		}
	}
	if opts.BootIntervals != cfg.WorkerBootRegistrationDeadlineIntervals {
		t.Errorf("sandbox BootIntervals = %d, want %d", opts.BootIntervals, cfg.WorkerBootRegistrationDeadlineIntervals)
	}
}
