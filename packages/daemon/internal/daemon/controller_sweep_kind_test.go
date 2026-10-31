package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// The controller orphan sweep's live proof (scripts/e2e/controller-sweep-kind.sh, whose driver is
// controller_sweep_live_test.go, built with -tags e2e) loads its legion.yaml and builds its Sandbox
// runtime with the functions here. They are untagged so `go test` holds them to the loader's and
// the runtime's current contracts on every run: a change that refuses the proof's file or its
// runtime's options fails this package's tests, not the live proof the next time a kind cluster
// runs it.

// controllerSweepKindProject is the proof's project. Dispatch maps it no repository.
const controllerSweepKindProject = "CSWEEP"

// controllerSweepKindStub is the agent the controller's shim wraps: it sleeps, so the pod stays up
// and nothing dials a model, Dispatch or NATS.
var controllerSweepKindStub = []string{"/bin/sh", "-c", "exec sleep infinity", "controller-sweep-stub"}

// writeControllerSweepKindConfig writes the proof's legion.yaml, and the token files it names,
// under dir: `controller: daemon` on runtime.kubernetes, the store at dsn, the worker stream bound
// at streamHost, the worker image, and the cluster as kubeconfig's kubeContext. The hosts no stub
// dials are documentation placeholders.
func writeControllerSweepKindConfig(t *testing.T, dir, dsn, streamHost, image, kubeconfig, kubeContext string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"envoy-token", "operator-token", "dispatch-token"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("placeholder-"+file+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf(`project: %[1]s
controller: daemon
postgres_dsn: %[2]s
state_dir: %[3]s
bind: %[4]s
daemon_url: http://%[4]s:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
worker_boot_timeout_seconds: 300
projects:
  %[1]s: {}
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: %[5]s
    storage_class: standard
    kubeconfig: %[6]s
    context: %[7]s
    session_store: postgres
    session_dsn_secret: SESSION_DSN
`, controllerSweepKindProject, dsn, filepath.Join(dir, "state"), streamHost, image, kubeconfig, kubeContext)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// controllerSweepKindRuntime builds the Sandbox runtime from cfg as boot does: its cluster client
// (kubeClient), sandboxOptions over the loaded configuration, then boot's own factory
// (sandboxRuntime) on listener and st, which also registers the runtime's launcher acceptor. What
// the proof stands in for: the agent is a sleep stub, the App tokens are placeholders
// (outboxTokens), and no repository is mapped, so the controller's gh files name no owner.
func controllerSweepKindRuntime(ctx context.Context, cfg config.Config, listener *stream.Listener, st *store.Store, log *slog.Logger) (runtime.Runtime, error) {
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		return nil, err
	}
	rc, err := kubeClient(k)
	if err != nil {
		return nil, err
	}
	opts, err := sandboxOptions(cfg, k, project, "placeholder-dispatch-token", lookup(nil), log)
	if err != nil {
		return nil, fmt.Errorf("sandboxOptions: %w", err)
	}
	opts.Agent = controllerSweepKindStub
	owners := func() []string { return nil }
	return sandboxRuntime(rc, opts, owners)(ctx, listener, listener.Addr(), outboxTokens{}, nil, st, noRepositories)
}

// controllerSweepKindSpec is the project controller's launch as the proof makes it: the
// controller's token and role, no issue, tree or repository, the launch secret the file's
// envoy_token_file names, and prompt as its role prompt.
func controllerSweepKindSpec(project, bootToken, prompt string) runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim: claim.ControllerToken(project), Project: project, Role: claim.RoleController, Generation: 1, BootToken: bootToken,
		Env: map[string]string{}, Secrets: map[string]string{"ENVOY_TOKEN": "placeholder-envoy-token"},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
	}
}

// The live proof's legion.yaml passes the daemon's loader as a `controller: daemon` deployment, the
// runtime it configures builds through boot's factory and sandbox.New, against an API server
// standing in for the kind cluster, and its controller launch passes the spawn-spec checks. Every
// step is the one the driver takes before it reaches the cluster.
func TestTheControllerSweepKindProofBuildsItsRuntimeFromItsConfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(newIssueSandboxAPI())
	t.Cleanup(func() { cancel(); server.Close() })
	dir := t.TempDir()
	path := writeControllerSweepKindConfig(t, dir, testDSN(t), "192.0.2.10",
		"ghcr.io/sjawhar/legion-worker@sha256:"+strings.Repeat("a", 64), writeKubeconfig(t, server.URL, ""), "test")
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if cfg.ControllerLaunch != config.ControllerLaunchDaemon {
		t.Fatalf("the proof's file launches the controller as %q, want %q", cfg.ControllerLaunch, config.ControllerLaunchDaemon)
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	refuse := func(string) (claim.Token, uint64, bool, bool, error) { return "", 0, false, false, nil }
	listener, err := stream.Listen(ctx, "tcp://127.0.0.1:0", refuse, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerSweepKindRuntime(ctx, cfg, listener, st, quietLogger()); err != nil {
		t.Fatalf("the proof's runtime: %v", err)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateSpawnSpec(controllerSweepKindSpec(project, "boot-token", filepath.Join(dir, "role.md")), nil); err != nil {
		t.Fatalf("the proof's controller launch: %v", err)
	}
}
