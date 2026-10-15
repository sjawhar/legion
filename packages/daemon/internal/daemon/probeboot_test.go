package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// probeBootConfig is a Kubernetes daemon's configuration over the cluster at server, with the whole
// workflow configured, whose Postgres cannot be reached, whose NATS listens nowhere and whose
// Dispatch counts every request it gets: a probe-boot that opened its store, connected to NATS or
// asked Dispatch anything would fail or show it.
func probeBootConfig(t *testing.T, server string) (config.Config, *atomic.Int32) {
	t.Helper()
	t.Setenv("LEGION_TEST_PG_DSN", "postgres://legion:legion@127.0.0.1:1/legion?sslmode=disable&connect_timeout=1")
	cfg := kubernetesConfig(t, server)
	var dispatched atomic.Int32
	dispatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dispatch.Close)
	cfg.DispatchURL = dispatch.URL
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte("dispatch-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.NatsURLs = []string{"nats://127.0.0.1:1"}
	cfg.Projects = map[string]config.Project{cfg.Project: {Repo: ghrepo.MustParse("acme/widgets")}}
	return cfg, &dispatched
}

// agentSandboxCluster is an API server with Agent Sandbox installed and no Sandbox in the
// namespace, recording each request it is sent.
func agentSandboxCluster(t *testing.T, requests *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*requests = append(*requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		switch r.URL.Path {
		case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/sandboxes.agents.x-k8s.io":
			body = map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "sandboxes.agents.x-k8s.io"},
				"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": true}}}}
		case "/apis/apps/v1/namespaces/agent-sandbox-system/deployments/agent-sandbox-controller":
			body = map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "agent-sandbox-controller", "namespace": "agent-sandbox-system"},
				"status":   map[string]any{"availableReplicas": 1}}
		case "/apis/agents.x-k8s.io/v1beta1/namespaces/legion/sandboxes":
			body = map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "SandboxList", "metadata": map[string]any{}, "items": []any{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			body = map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// recordedProber is the image probe probeBoot is handed: it records what it was built from and
// probed with, and answers report.
type recordedProber struct {
	report bootprobe.ImageReport
	probed []sandbox.ImageProbe
}

func (p *recordedProber) ProbeImage(_ context.Context, probe sandbox.ImageProbe) (bootprobe.ImageReport, error) {
	p.probed = append(p.probed, probe)
	return p.report, nil
}

// probe-boot runs the Kubernetes runtime's boot probe and refuses any other runtime by name.
func TestProbeBootRefusesARuntimeWithoutTheSandboxProbe(t *testing.T) {
	cfg, _ := probeBootConfig(t, "https://127.0.0.1:1")
	cfg.Runtime = config.Runtime{Name: "tmux"}
	_, err := ProbeBoot(context.Background(), cfg, lookup(nil), quietLogger())
	if err == nil || !strings.Contains(err.Error(), `runtime "tmux"`) {
		t.Fatalf("ProbeBoot = %v, want a refusal naming runtime \"tmux\"", err)
	}
}

// probe-boot reads the configuration's files as boot does, so a file boot refuses refuses it too.
func TestProbeBootRefusesWhatBootRefusesOfTheFilesItNames(t *testing.T) {
	cfg, _ := probeBootConfig(t, "https://127.0.0.1:1")
	cfg.OperatorTokenFile = filepath.Join(t.TempDir(), "absent")
	_, err := ProbeBoot(context.Background(), cfg, lookup(nil), quietLogger())
	if err == nil || !strings.Contains(err.Error(), "operator_token_file") {
		t.Fatalf("ProbeBoot = %v, want boot's refusal of operator_token_file", err)
	}
}

// A cluster without Agent Sandbox is refused by name, as boot refuses it, before any probe; the
// cluster is only read, and Dispatch is asked nothing.
func TestProbeBootRefusesAClusterWithoutAgentSandboxBeforeAnyProbe(t *testing.T) {
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
	cfg, dispatched := probeBootConfig(t, apiServer.URL)

	_, err := probeBoot(context.Background(), cfg, lookup(nil), quietLogger(),
		func(context.Context, *rest.Config, sandbox.Options) (imageProber, error) {
			t.Error("the probe ran on a cluster without Agent Sandbox")
			return &recordedProber{}, nil
		})

	for _, want := range []string{"sandboxes.agents.x-k8s.io", "agent-sandbox-system/agent-sandbox-controller"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ProbeBoot = %v, want a refusal naming %s", err, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the cluster check sent %s; it may only read", request)
		}
	}
	if n := dispatched.Load(); n != 0 {
		t.Errorf("probe-boot sent Dispatch %d requests, want none", n)
	}
}

// On a cluster boot's check passes, probe-boot runs the cluster check and then the image probe a
// booting daemon runs (sandboxBootOf, one value for both), on a runtime built from the Options boot
// reads and holding no token source, store or connections, and it answers the probe's report. The
// prober is built only once the check's last read, the census's Sandbox list, has been made. It
// opens no store (its Postgres cannot be reached), connects to no NATS and asks Dispatch nothing.
func TestProbeBootRunsTheDaemonsImageProbeAfterTheClusterCheck(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	server := agentSandboxCluster(t, &requested, &mu)
	cfg, dispatched := probeBootConfig(t, server.URL)
	prober := &recordedProber{report: bootprobe.ImageReport{ModelFallback: "off"}}
	var built []sandbox.Options
	var checkedFirst bool
	census := "GET /apis/agents.x-k8s.io/v1beta1/namespaces/" + cfg.Runtime.Kubernetes.Namespace + "/sandboxes"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := probeBoot(ctx, cfg, lookup(nil), quietLogger(),
		func(_ context.Context, _ *rest.Config, opts sandbox.Options) (imageProber, error) {
			mu.Lock()
			checkedFirst = slices.Contains(requested, census)
			mu.Unlock()
			built = append(built, opts)
			return prober, nil
		})

	if err != nil {
		t.Fatalf("ProbeBoot = %v, want a pass", err)
	}
	if report.ModelFallback != "off" {
		t.Errorf("report = %+v, want the probe's", report)
	}
	if !checkedFirst {
		t.Error("the prober was built before the cluster check ran")
	}
	if len(built) != 1 || len(prober.probed) != 1 {
		t.Fatalf("built %d probers and probed %d times, want 1 and 1", len(built), len(prober.probed))
	}
	opts := built[0]
	k := cfg.Runtime.Kubernetes
	if opts.Namespace != k.Namespace || opts.Image != k.Image || opts.DispatchURL != cfg.DispatchURL || opts.DispatchToken != "dispatch-test-token" {
		t.Errorf("built from %+v, want the Options boot reads from the configuration", opts)
	}
	if opts.Tokens != nil || opts.GitHubCredential != nil || opts.Store != nil || opts.Conns != nil || opts.StreamURL != "" {
		t.Errorf("built with supervision's options (%+v); probe-boot has none", opts)
	}
	if n := dispatched.Load(); n != 0 {
		t.Errorf("probe-boot sent Dispatch %d requests, want none", n)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requested {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("probe-boot sent %s before its probe; the cluster check may only read", request)
		}
	}
}
