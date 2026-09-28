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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	corev1 "k8s.io/api/core/v1"
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
	err := run(ctx, cfg, quietLogger(), overrides{clock: stillClock{}, listen: heldListen})
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

// Under kubernetes the worker stream is TCP on the daemon's bind, at a port the listener resolves
// itself, the one address the runtime hands every pod's shim, and the runtime is given the
// workflow's App tokens. The host's own agent machinery never runs: no pane launcher is
// installed, no Dispatch token file is written, and the workflow's boot has no worker-bin stage.
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
	prefix := "tcp://" + cfg.Bind + ":"
	if !strings.HasPrefix(address, prefix) || strings.HasSuffix(address, ":0") {
		t.Fatalf("the runtime was told to have shims dial %q, want %s<bound port>", address, prefix)
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "tcp://"), time.Second); err != nil {
		t.Errorf("the worker stream does not accept on %s: %v", address, err)
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

// The address every pod's shim dials derives from runtime.kubernetes's own worker_stream_port
// (readSandbox, kubernetes.go), not a value prepare hardcodes or leaves at the zero a test's own
// config happens to carry: a daemon configured with a distinctive port builds its plan's stream
// address from exactly that port, before anything binds. The test above proves the bound address
// is real and dialable; it cannot catch worker_stream_port being ignored, since testConfig's own
// port is always 0 — this does, by configuring a nonzero one and checking prepare()'s plan
// directly, with no listener bound.
func TestPrepareDerivesTheWorkerStreamAddressFromWorkerStreamPort(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.NatsNkeySeedFile = testnats.SeedFile(t, testnats.UserSeed(t))
	cfg.WorkerStreamPort = 47381

	p, err := prepare(cfg, quietLogger(), overrides{environ: []string{}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if want := "tcp://" + net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.WorkerStreamPort)); p.stream != want {
		t.Errorf("prepare's worker stream address = %q, want %q (derived from worker_stream_port)", p.stream, want)
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
// or pod variable a launch secret's pointer names (the Envoy bearer's, the NATS nkey seed's) — so
// no pod is ever built with it.
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
			want: "provider_keys names ENVOY_TOKEN, the launch secret every launch carries behind its ENVOY_TOKEN_FILE pointer: a provider key may not name a launch secret",
		},
		{
			name: "a provider key the NATS nkey seed's pointer names",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "NATS_NKEY_SEED", Secret: "seed"}}
			},
			want: "provider_keys names NATS_NKEY_SEED, the launch secret every launch carries behind its NATS_NKEY_SEED_FILE pointer: a provider key may not name a launch secret",
		},
		{
			name: "a pod variable that is the NATS nkey seed's pointer",
			change: func(cfg *config.Config) {
				cfg.Runtime.Kubernetes.Pod = config.PodConfig{Env: map[string]string{"NATS_NKEY_SEED_FILE": "/etc/operator/seed"}}
			},
			want: "runtime.kubernetes.pod.env sets NATS_NKEY_SEED_FILE, which every launch sets (the pointer to the launch secret NATS_NKEY_SEED)",
		},
		{
			name: "a provider key reading the NATS nkey seed's providers Secret key",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "FOO", Secret: "NATS_NKEY_SEED"}}
			},
			want: "provider_keys names FOO from the providers Secret's key NATS_NKEY_SEED, which the pod mounts as the launch secret NATS_NKEY_SEED: the shim would export that secret into Oh My Pi's environment as FOO",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
			if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.NatsNkeySeedFile = testnats.SeedFile(t, testnats.UserSeed(t))
			tc.change(&cfg)
			if _, err := prepare(cfg, quietLogger(), overrides{environ: []string{}}); err == nil || err.Error() != tc.want {
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
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(nil), quietLogger())
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

// The NATS nkey seed reaches every pod from the providers Secret, never from a copy in a claim's
// Secret: whenever the daemon has one — from nats_nkey_seed_file, NATS_NKEY_SEED_FILE, or
// NATS_NKEY_SEED — it is a launch secret the runtime reads from the providers mount, and with none
// it is neither.
func TestTheNatsSeedReachesTheSandboxRuntimeAsTheProvidersSecrets(t *testing.T) {
	seedFile := testnats.SeedFile(t, testnats.UserSeed(t))
	for _, tc := range []struct {
		name string
		key  string
		env  map[string]string
		want []string
	}{
		{"nats_nkey_seed_file", seedFile, nil, []string{"NATS_NKEY_SEED"}},
		{"NATS_NKEY_SEED_FILE", "", map[string]string{"NATS_NKEY_SEED_FILE": seedFile}, []string{"NATS_NKEY_SEED"}},
		{"NATS_NKEY_SEED", "", map[string]string{"NATS_NKEY_SEED": "SU…"}, []string{"NATS_NKEY_SEED"}},
		{"no seed", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.NatsNkeySeedFile = tc.key
			opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(tc.env), quietLogger())
			if err != nil {
				t.Fatalf("sandboxOptions: %v", err)
			}
			if !reflect.DeepEqual(opts.ProvidersSecrets, tc.want) {
				t.Errorf("ProvidersSecrets = %v, want %v", opts.ProvidersSecrets, tc.want)
			}
			if got := slices.Contains(opts.LaunchSecrets, "NATS_NKEY_SEED"); got != (tc.want != nil) {
				t.Errorf("LaunchSecrets = %v: carries NATS_NKEY_SEED %t, want %t", opts.LaunchSecrets, got, tc.want != nil)
			}
		})
	}
}

// awaitHealthz returns once the daemon answers /healthz, and fails if it exits first.
func awaitHealthz(t *testing.T, cfg config.Config, done chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
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

	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(nil), quietLogger())
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

// sandboxOptions carries runtime.kubernetes.agent_secrets straight into the runtime's Options,
// nil when the configuration sets no block, and the worker image's agent-secrets binary is
// always the tools path (Task 11 mounts it there whether or not the deployment enrolls).
func TestSandboxOptionsCarryTheAgentSecretsBlock(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1") // the file's fixture (`:32-41`): testConfig under runtime: kubernetes
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: "https://secrets.dev1.internal.trajectorylabs.com", Operator: "sjawhar", Audience: "agent-secrets", TokenExpirySeconds: 1800,
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	want := &sandbox.AgentSecrets{URL: "https://secrets.dev1.internal.trajectorylabs.com", Audience: "agent-secrets", TokenExpiry: 30 * time.Minute}
	if opts.AgentSecrets == nil || *opts.AgentSecrets != *want {
		t.Fatalf("AgentSecrets = %+v, want %+v", opts.AgentSecrets, want)
	}
	if opts.Tools.AgentSecrets != "/opt/legion/go/bin/agent-secrets" {
		t.Fatalf("Tools.AgentSecrets = %q", opts.Tools.AgentSecrets)
	}
	cfg.Runtime.Kubernetes.AgentSecrets = nil
	opts, _ = sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(nil), quietLogger())
	if opts.AgentSecrets != nil {
		t.Fatalf("AgentSecrets = %+v without the block", opts.AgentSecrets)
	}
}

// No credential material — no key, no launcher credential, no bearer — ever reaches a pod or a
// launch secret: the daemon's machine login lives only in the *agentsecrets.Client's process
// memory (Task 1's cred atomic.Pointer[credential]), which the configuration and
// sandbox.Options carry no field for at all. newSecretsLogin hands the runtime only the broker
// URL, audience and token lifetime, and no launch's secrets ever name an AGENT_SECRETS_*
// variable.
func TestNoCredentialMaterialReachesAPod(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: "https://s", Operator: "sjawhar", Audience: "agent-secrets", TokenExpirySeconds: 3600,
	}
	for _, secret := range launchSecrets(cfg, lookup(nil)) {
		if strings.HasPrefix(secret.name, "AGENT_SECRETS") {
			t.Fatalf("launch secret %s: no daemon credential material may reach a pod", secret.name)
		}
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if opts.AgentSecrets == nil || opts.AgentSecrets.URL != "https://s" || opts.AgentSecrets.Audience != "agent-secrets" || opts.AgentSecrets.TokenExpiry != time.Hour {
		t.Fatalf("AgentSecrets = %+v, want only the URL, audience and token lifetime the pod's own client needs", opts.AgentSecrets)
	}
	enroller, client := newSecretsLogin(cfg, quietLogger())
	if enroller == nil || client == nil {
		t.Fatalf("enroller %v, client %v; want both", enroller, client)
	}
	if client.URL != "https://s" || client.Operator != "sjawhar" {
		t.Fatalf("client = %+v, want only the configured URL and operator, never a key or a bearer", client)
	}
	cfg.Runtime.Kubernetes.AgentSecrets = nil
	if enroller, client := newSecretsLogin(cfg, quietLogger()); enroller != nil || client != nil {
		t.Fatalf("without the block: %v, %v; want nil, nil", enroller, client)
	}
}

// newSecretsLogin logs the contract's exact line, once, naming the code the broker issued and
// the configured operator; a background poll goroutine keeps running against the (later closed)
// broker after the login is issued, which is fine — it is the same unbounded, backed-off retry
// production leaves running for as long as the daemon is up.
func TestNewSecretsLoginLogsTheConfirmationCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/launcher-credentials" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"pending_id": "pending-1", "code": "ABCD-1234"})
	}))
	defer server.Close()

	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Runtime.Kubernetes.AgentSecrets = &config.AgentSecretsConfig{
		URL: server.URL, Operator: "sjawhar", Audience: "agent-secrets", TokenExpirySeconds: 3600,
	}
	var logged bytes.Buffer
	enroller, client := newSecretsLogin(cfg, slog.New(slog.NewJSONHandler(&logged, nil)))
	if enroller == nil || client == nil {
		t.Fatal("want an enroller and a client")
	}
	want := "agent-secrets machine login: enter code ABCD-1234 on the Dispatch credential page (approver: sjawhar); pod enrollment is held until approved"
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logged.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log = %s, want it to contain %q", logged.String(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A hello carrying an agent-secrets identity maps to the machine's StreamHello with it; a hello
// with none maps to one with no identity.
func TestAHelloWithAnIdentityMapsToTheMachinesEvent(t *testing.T) {
	ev, err := superviseEvent(stream.Hello{Claim: "legion-LEGION-209-implementer", Generation: 2,
		AgentSecrets: &stream.AgentSecretsIdentity{Thumbprint: "tp", PodToken: "a.b.c"}})
	if err != nil {
		t.Fatal(err)
	}
	hello := ev.(supervise.StreamHello)
	if hello.Generation != 2 || hello.AgentSecrets == nil || hello.AgentSecrets.Thumbprint != "tp" || hello.AgentSecrets.PodToken != "a.b.c" {
		t.Fatalf("mapped %+v", hello)
	}
	ev, _ = superviseEvent(stream.Hello{Claim: "legion-LEGION-209-implementer", Generation: 2})
	if ev.(supervise.StreamHello).AgentSecrets != nil {
		t.Fatal("a hello with no identity mapped to one")
	}
}
