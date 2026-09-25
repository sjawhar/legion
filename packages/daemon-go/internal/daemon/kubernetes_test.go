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
		Gateway: config.Gateway{URL: "https://gateway.example.test", Audience: "middleman-legion", ServiceAccount: "legion-worker", TokenExpiry: 10 * time.Minute},
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
					Volumes:      []config.PodVolume{{Name: "creds", Secret: &config.ObjectSource{Name: "legion-creds"}}},
					VolumeMounts: []config.PodMount{{Volume: "creds", MountPath: "/var/run/legion/boot", ReadOnly: true}},
				}
			},
			want: "runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/boot overlaps /var/run/legion/boot, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's",
		},
		{
			name: "a provider key the Envoy bearer's pointer names",
			change: func(cfg *config.Config) {
				cfg.ProviderKeys = []config.ProviderKey{{Env: "ENVOY_TOKEN", Secret: "envoy"}}
			},
			want: "provider_keys names ENVOY_TOKEN, which every launch sets itself (the pointer to the launch secret ENVOY_TOKEN): the shim would not export the key",
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

// CheckOperatorPod refuses every operator piece that collides with what Legion puts in a pod,
// naming both: a variable, volume, or mount path of the runtime's own, a path the worker image
// owns, a variable every launch's spec sets, and a provider key the shim could not export — and
// passes an operator pod beside Legion's, the LEGION-270 harness's shape.
func TestCheckOperatorPodRefusesWhatCollidesWithLegionsOwn(t *testing.T) {
	mountAt := func(path string) config.PodConfig {
		return config.PodConfig{
			Volumes:      []config.PodVolume{{Name: "creds", Secret: &config.ObjectSource{Name: "legion-creds"}}},
			VolumeMounts: []config.PodMount{{Volume: "creds", MountPath: path, ReadOnly: true}},
		}
	}
	overlaps := func(path, owned, owner string) string {
		if owner == "the image's" {
			return "runtime.kubernetes.pod.volume_mounts[0].mount_path " + path + " overlaps " + owned + ", which the worker image owns: a mount may be neither at, under, nor above one of the image's"
		}
		return "runtime.kubernetes.pod.volume_mounts[0].mount_path " + path + " overlaps " + owned + ", which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's"
	}
	secrets := map[string]secretPointer{"ENVOY_TOKEN": {"envoy_token_file", "/var/run/legion/ENVOY_TOKEN"}}
	for _, tc := range []struct {
		name string
		pod  config.PodConfig
		keys []config.ProviderKey
		want string
	}{
		{name: "a variable Legion sets", pod: config.PodConfig{Env: map[string]string{"LEGION_TREE": "LEGSMOKE-1"}},
			want: "runtime.kubernetes.pod.env sets LEGION_TREE, which Legion sets in every pod itself"},
		{name: "the pod's PATH", pod: config.PodConfig{Env: map[string]string{"PATH": "/usr/bin"}},
			want: "runtime.kubernetes.pod.env sets PATH, which Legion sets in every pod itself"},
		{name: "a variable of the App's git identity", pod: config.PodConfig{Env: map[string]string{"JJ_USER": "operator"}},
			want: "runtime.kubernetes.pod.env sets JJ_USER, which every launch sets itself (the git identity the role's GitHub App commits as)"},
		{name: "a variable pointing at a launch secret", pod: config.PodConfig{Env: map[string]string{"ENVOY_TOKEN_FILE": "/etc/envoy"}},
			want: "runtime.kubernetes.pod.env sets ENVOY_TOKEN_FILE, which every launch sets itself (the pointer to the launch secret ENVOY_TOKEN)"},
		{name: "a volume Legion names", pod: config.PodConfig{Volumes: []config.PodVolume{
			{Name: "creds", Secret: &config.ObjectSource{Name: "legion-creds"}}, {Name: "boot", Secret: &config.ObjectSource{Name: "a"}},
		}}, want: "runtime.kubernetes.pod.volumes[1].name boot is a volume Legion puts in every pod"},
		{name: "a mount at a path Legion mounts", pod: mountAt("/var/run/legion/boot"),
			want: overlaps("/var/run/legion/boot", "/var/run/legion/boot", "Legion's")},
		{name: "a mount under a path Legion mounts", pod: mountAt("/legion/operator"),
			want: overlaps("/legion/operator", "/legion", "Legion's")},
		{name: "a mount above a path Legion mounts", pod: mountAt("/var/run/legion"),
			want: overlaps("/var/run/legion", "/var/run/legion/boot", "Legion's")},
		{name: "a mount above the sessions Legion mounts", pod: mountAt("/home/legion/.omp/profiles/legion/agent"),
			want: overlaps("/home/legion/.omp/profiles/legion/agent", "/home/legion/.omp/profiles/legion/agent/sessions", "Legion's")},
		{name: "a mount at the root", pod: mountAt("/"),
			want: overlaps("/", "/home/legion/.config", "Legion's")},
		{name: "a mount under a path the image owns", pod: mountAt("/opt/omp/bin"),
			want: overlaps("/opt/omp/bin", "/opt/omp", "the image's")},
		{name: "a mount over a database the image owns", pod: mountAt("/home/legion/.omp/profiles/legion/agent/agent.db"),
			want: overlaps("/home/legion/.omp/profiles/legion/agent/agent.db", "/home/legion/.omp/profiles/legion/agent/agent.db", "the image's")},
		{name: "a provider key Legion sets", keys: []config.ProviderKey{{Env: "PATH", Secret: "search_path"}},
			want: "provider_keys names PATH, which Legion sets in every pod itself: the shim refuses to export a key its own environment names"},
		{name: "a provider key whose pointer Legion sets", keys: []config.ProviderKey{{Env: "DISPATCH_TOKEN", Secret: "dispatch"}},
			want: "provider_keys names DISPATCH_TOKEN, whose pointer DISPATCH_TOKEN_FILE Legion sets in every pod: the shim would skip the key"},
		{name: "a provider key for the settings overlays", keys: []config.ProviderKey{{Env: "PI_CONFIG_FILES", Secret: "overlays"}},
			want: "provider_keys names PI_CONFIG_FILES, the settings overlays Legion composes in every pod: the shim adds a provider key after the pod's own variables, so it would replace them"},
		{name: "a provider key of the App's git identity", keys: []config.ProviderKey{{Env: "GIT_AUTHOR_NAME", Secret: "author"}},
			want: "provider_keys names GIT_AUTHOR_NAME, which every launch sets itself (the git identity the role's GitHub App commits as): the shim would not export the key"},
		{name: "a provider key a launch secret's pointer names", keys: []config.ProviderKey{{Env: "ENVOY_TOKEN", Secret: "envoy"}},
			want: "provider_keys names ENVOY_TOKEN, which every launch sets itself (the pointer to the launch secret ENVOY_TOKEN): the shim would not export the key"},
		{name: "an operator pod beside Legion's", pod: config.PodConfig{
			Env:            map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml", "CLAUDE_CODE_USE_FOUNDRY": "0"},
			ServiceAccount: "legion-worker",
			Volumes: []config.PodVolume{
				{Name: "operator-config", ConfigMap: &config.ObjectSource{Name: "legion-operator"}},
				{Name: "operator-token", Projected: &config.ProjectedSource{Sources: []config.Projection{
					{ServiceAccountToken: &config.TokenProjection{Audience: "middleman-legion", ExpirationSeconds: 3600, Path: "token"}},
				}}},
			},
			VolumeMounts: []config.PodMount{
				{Volume: "operator-config", MountPath: "/home/legion/.omp/profiles/legion/agent/models.yml", SubPath: "models.yml", ReadOnly: true},
				{Volume: "operator-config", MountPath: "/etc/legion-operator/overlay.yml", SubPath: "overlay.yml", ReadOnly: true},
				{Volume: "operator-token", MountPath: "/var/run/operator", ReadOnly: true},
			},
		}, keys: []config.ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "anthropic"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOperatorPod(tc.pod, tc.keys, secrets)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("checkOperatorPod = %v, want no refusal", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("checkOperatorPod = %v, want %q", err, tc.want)
			}
		})
	}
}

// runtime.kubernetes.pod and provider_keys reach the runtime as the pod spec's own pieces: each
// volume with its one source and items, a projected token's unset lifetime left to the API server
// (a zero one is refused at pod creation), each mount's read-only flag as configured, and each
// provider key's variable to its Secret key.
func TestTheOperatorsPodReachesTheSandboxRuntimeAsThePodSpecsPieces(t *testing.T) {
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.ProviderKeys = []config.ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "anthropic"}}
	cfg.Runtime.Kubernetes.Pod = config.PodConfig{
		Env:            map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		ServiceAccount: "legion-worker",
		Volumes: []config.PodVolume{
			{Name: "creds", Secret: &config.ObjectSource{Name: "legion-creds", Items: []config.KeyPath{{Key: "a", Path: "b"}}}},
			{Name: "settings", ConfigMap: &config.ObjectSource{Name: "legion-operator"}},
			{Name: "token", Projected: &config.ProjectedSource{Sources: []config.Projection{
				{ServiceAccountToken: &config.TokenProjection{Audience: "operator", ExpirationSeconds: 3600, Path: "token"}},
				{ServiceAccountToken: &config.TokenProjection{Path: "default-token"}},
				{Secret: &config.ObjectSource{Name: "legion-ca", Items: []config.KeyPath{{Key: "ca.crt", Path: "ca.crt"}}}},
				{ConfigMap: &config.ObjectSource{Name: "legion-routes"}},
			}}},
		},
		VolumeMounts: []config.PodMount{
			{Volume: "settings", MountPath: "/etc/legion-operator/overlay.yml", SubPath: "overlay.yml", ReadOnly: true},
			{Volume: "creds", MountPath: "/etc/legion-operator/creds"},
		},
	}
	opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "test", "tcp://10.0.0.5:13371", "", quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	hour := int64(3600)
	want := sandbox.Pod{
		Env:            map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		ServiceAccount: "legion-worker",
		Volumes: []corev1.Volume{
			{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: "legion-creds", Items: []corev1.KeyToPath{{Key: "a", Path: "b"}},
			}}},
			{Name: "settings", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator"},
			}}},
			{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "operator", ExpirationSeconds: &hour, Path: "token"}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "default-token"}},
				{Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "legion-ca"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "legion-routes"}}},
			}}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "settings", MountPath: "/etc/legion-operator/overlay.yml", SubPath: "overlay.yml", ReadOnly: true},
			{Name: "creds", MountPath: "/etc/legion-operator/creds"},
		},
	}
	if !reflect.DeepEqual(opts.Pod, want) {
		t.Errorf("the runtime's Pod is\n%+v\nwant\n%+v", opts.Pod, want)
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
