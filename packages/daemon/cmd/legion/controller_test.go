package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

const (
	controllerProject       = "demo"
	controllerOperatorToken = "op-tok-7f3a"
)

// memoryController is the project's controller record held in memory: what the daemon's store
// holds, with every capability hash it was handed kept in order.
type memoryController struct {
	mu     sync.Mutex
	record controller.Record
	minted [][]byte
}

func (m *memoryController) MintController(_ context.Context, _ string, hash []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record = controller.Record{Generation: m.record.Generation + 1, CapabilityHash: hash}
	m.minted = append(m.minted, hash)
	return m.record.Generation, nil
}

func (m *memoryController) Controller(context.Context, string) (controller.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.record, m.record.Generation != 0, nil
}

func (m *memoryController) RegisterController(_ context.Context, _ string, generation uint64, session string, secretHash []byte, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.record.Generation {
		return false, nil
	}
	m.record.Session, m.record.SecretHash, m.record.RegisteredAt = session, secretHash, at
	return true, nil
}

// statusWrites is Dispatch as the issue status route reaches it: every status it was asked to
// set. Any other Dispatch call is a test failure (the embedded nil Client panics).
type statusWrites struct {
	dispatch.Client
	mu     sync.Mutex
	writes []string
}

func (s *statusWrites) SetStatus(_ context.Context, key, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, key+"="+status)
	return nil
}

// controllerDaemon is the daemon's HTTP surface as `legion controller start` and `legion status`
// reach it: the real api.NewServer over an in-memory controller record, grants, and Dispatch, on
// a loopback port, with every request that reached it recorded.
type controllerDaemon struct {
	t          *testing.T
	url        string
	port       int
	controller *memoryController
	dispatch   *statusWrites

	mu   sync.Mutex
	seen []seenRequest
}

func newControllerDaemon(t *testing.T) *controllerDaemon {
	return newControllerDaemonGated(t, config.DesignGateRootIssues)
}

// newControllerDaemonGated is newControllerDaemon for a project whose `gates.design` is gate.
func newControllerDaemonGated(t *testing.T, gate config.DesignGate) *controllerDaemon {
	return newControllerDaemonWithOptions(t, controllerDaemonOptions{gate: gate})
}

// newControllerDaemonWithTokens is newControllerDaemon whose GitHub credential route mints a
// review App token from tokens, for the tests of the controller's GitHub credential fetch and
// refresh loop.
func newControllerDaemonWithTokens(t *testing.T, tokens appauth.Tokens) *controllerDaemon {
	return newControllerDaemonWithOptions(t, controllerDaemonOptions{gate: config.DesignGateRootIssues, tokens: tokens, githubOwner: "acme"})
}

// controllerDaemonOptions configures newControllerDaemonWithOptions: the project's design gate,
// and, for the tests of the controller's GitHub credential route, a Tokens fake and the
// repository owner its mints need.
type controllerDaemonOptions struct {
	gate        config.DesignGate
	tokens      appauth.Tokens
	githubOwner string
}

// newControllerDaemonWithOptions is the daemon's HTTP surface as `legion controller start` and
// `legion status` reach it: the real api.NewServer over an in-memory controller record, grants,
// and Dispatch, on a loopback port, with every request that reached it recorded.
func newControllerDaemonWithOptions(t *testing.T, opts controllerDaemonOptions) *controllerDaemon {
	t.Helper()
	d := &controllerDaemon{t: t, controller: &memoryController{}, dispatch: &statusWrites{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Project: controllerProject, OperatorToken: controllerOperatorToken, Controller: d.controller,
		DesignGate: opts.gate, Dispatch: d.dispatch, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tokens: opts.tokens, GitHubOwner: opts.githubOwner,
	}).Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		d.mu.Lock()
		d.seen = append(d.seen, seenRequest{
			method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body, status: recorder.Code, answer: recorder.Body.Bytes(),
		})
		d.mu.Unlock()
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	d.port = server.Listener.Addr().(*net.TCPAddr).Port
	return d
}

func (d *controllerDaemon) requests() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

// operatorMachine is one operator's machine: a controller.yaml naming every key, the files it
// points at, a HOME of its own, and an Oh My Pi that records how it was launched and exits.
type operatorMachine struct {
	t          *testing.T
	daemon     *controllerDaemon
	dir, home  string
	config     string
	tokenFile  string
	record     string
	defaultDir string
}

type controllerOptions struct {
	// lines replace the default controller.yaml wholesale.
	lines []string
	// tokenMode is the operator token file's mode; tokenContents its contents.
	tokenMode     os.FileMode
	tokenContents string
	omitToken     bool
	exitCode      int
	// waitForFile, when set, has the recording Oh My Pi wait until that file exists before it
	// records argv, env, and cwd and exits with exitCode, so a test of the refresh loop keeps
	// Oh My Pi running exactly until it has observed what it needs, whatever the machine's load,
	// and then releases it by creating the file.
	waitForFile string
}

func newControllerStart(t *testing.T, d *controllerDaemon, opts controllerOptions) *operatorMachine {
	t.Helper()
	dir, home, record := t.TempDir(), t.TempDir(), t.TempDir()
	c := &operatorMachine{
		t: t, daemon: d, dir: dir, home: home, record: record,
		config:     filepath.Join(dir, "controller.yaml"),
		tokenFile:  filepath.Join(dir, "operator-token"),
		defaultDir: filepath.Join(home, ".local", "state", "legion", controllerProject+"-controller"),
	}
	if !opts.omitToken {
		contents := opts.tokenContents
		if contents == "" {
			contents = controllerOperatorToken + "\n"
		}
		mode := opts.tokenMode
		if mode == 0 {
			mode = 0o600
		}
		c.write("operator-token", contents, mode)
	}
	c.write("instructions.md", "Always be kind.\n", 0o600)
	c.write("envoy-token", "envoy-bearer\n", 0o600)
	c.write("dispatch-token", "dispatch-bearer\n", 0o600)
	c.write("nats-seed", testnats.UserSeed(t)+"\n", 0o600)
	lines := opts.lines
	if lines == nil {
		lines = []string{
			"project: " + controllerProject,
			"daemon_url: " + d.url,
			"operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020",
			"envoy_token_file: envoy-token",
			"nats_urls: [nats://a:4222, nats://b:4222]",
			"nats_nkey_seed_file: ./nats-seed",
			"dispatch_url: https://dispatch.test",
			"dispatch_token_file: ./dispatch-token",
			"instructions: ./instructions.md",
			"omp_launch_prefix: [env, LEGION_TEST_PREFIX_RAN=1]",
		}
	}
	c.write("controller.yaml", strings.Join(lines, "\n")+"\n", 0o600)

	// The Oh My Pi LEGION_OMP_PATH names. As the load probe (`models --extension <probe>`) it
	// records its environment and working directory and reports, as the probe extension does,
	// pi-legion loaded from the file `loaded-from` names, or not loaded when there is none;
	// pi-envoy publishing the interface `envoy-interface` holds (1 when absent; `none` for no
	// pi-envoy) from the file `envoy-loaded-from` names; and the pre-split package loaded from the
	// file `legacy-loaded-from` names, when there is one. As the controller it records its argv,
	// environment, and working directory, and exits with the code the test asked for.
	omp := filepath.Join(record, "omp")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = models ]; then
  env -0 >%[1]s/probe-env
  pwd -P >%[1]s/probe-cwd
  readlink /proc/self/fd/0 >%[1]s/probe-stdin
  if [ -f %[1]s/loaded-from ]; then
    printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=%%s\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n' "$(cat %[1]s/loaded-from)" >&2
  else
    echo LEGION_PLUGIN_LOADED=no >&2
  fi
  interface=$(cat %[1]s/envoy-interface 2>/dev/null || echo 1)
  if [ "$interface" = none ]; then
    echo LEGION_ENVOY_INTERFACE=none >&2
  else
    printf 'LEGION_ENVOY_INTERFACE=%%s\nLEGION_ENVOY_LOADED_FROM=%%s\n' "$interface" "$(cat %[1]s/envoy-loaded-from)" >&2
  fi
  [ ! -f %[1]s/legacy-loaded-from ] || printf 'LEGION_LEGACY_PLUGIN_LOADED_FROM=%%s\n' "$(cat %[1]s/legacy-loaded-from)" >&2
  exit 0
fi
[ -f "$GH_CONFIG_DIR/hosts.yml" ] && cp "$GH_CONFIG_DIR/hosts.yml" %[1]s/hosts-at-start
%[3]s
for a in "$@"; do printf '%%s\0' "$a"; done >%[1]s/argv
readlink /proc/self/fd/0 >%[1]s/stdin
env -0 >%[1]s/env
pwd >%[1]s/cwd
exit %[2]d
`, record, opts.exitCode, waitForFileScript(opts.waitForFile))
	if err := os.WriteFile(omp, []byte(script), 0o700); err != nil {
		t.Fatalf("write the recording omp: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OMP_PROFILE", "")
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LEGION_OMP_PATH", omp)
	c.installPlugin(api.DaemonAPIVersion)
	return c
}

// waitForFileScript is the recording Oh My Pi's wait for file: a poll every tenth of a second,
// bounded at two minutes so a test that never releases it still ends. Empty when file is empty,
// so a test with no loop to wait for records and exits at once.
func waitForFileScript(file string) string {
	if file == "" {
		return ""
	}
	return fmt.Sprintf(`i=0
while [ "$i" -lt 1200 ] && [ ! -e %q ]; do
  i=$((i + 1))
  sleep 0.1
done
`, file)
}

// awaitFile polls until file exists, failing the test after timeout.
func awaitFile(t *testing.T, file string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(file); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written", file)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitFileContaining polls until file holds want, failing the test after timeout with what it
// held last.
func awaitFileContaining(t *testing.T, file, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		body, err := os.ReadFile(file)
		if err == nil && strings.Contains(string(body), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never held %q; last read %q (%v)", file, want, body, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// installPlugin installs, in the operator's default Oh My Pi profile, a pi-legion manifest
// declaring contract and a pi-envoy manifest beside it, and has the recording Oh My Pi load both.
func (c *operatorMachine) installPlugin(contract int) {
	c.t.Helper()
	c.loads(c.installPluginAt(filepath.Join(c.home, ".omp"), contract))
}

// loads has the recording Oh My Pi report pi-legion loaded from the package directory dir, and
// pi-envoy from the pi-envoy package beside it, as the probe extension renders each: a file URL of
// its dist/legion.js or dist/envoy.js with a cache-busting query.
func (c *operatorMachine) loads(dir string) {
	c.t.Helper()
	c.tells("loaded-from", "file://"+filepath.Join(dir, "dist", "legion.js")+"?mtime=1")
	c.tells("envoy-loaded-from", "file://"+filepath.Join(filepath.Dir(dir), "pi-envoy", "dist", "envoy.js")+"?mtime=1")
}

// loadsNothing has the recording Oh My Pi report pi-legion not loaded.
func (c *operatorMachine) loadsNothing() {
	c.t.Helper()
	if err := os.Remove(filepath.Join(c.record, "loaded-from")); err != nil && !os.IsNotExist(err) {
		c.t.Fatal(err)
	}
}

// loadsEnvoyAt has the recording Oh My Pi report pi-envoy publishing plugin interface version;
// "none" is no pi-envoy loaded.
func (c *operatorMachine) loadsEnvoyAt(version string) {
	c.t.Helper()
	c.tells("envoy-interface", version)
}

// loadsLegacyFrom has the recording Oh My Pi report the pre-split package loaded from from.
func (c *operatorMachine) loadsLegacyFrom(from string) {
	c.t.Helper()
	c.tells("legacy-loaded-from", from)
}

// tells writes what the recording Oh My Pi reads under name.
func (c *operatorMachine) tells(name, value string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.record, name), []byte(value), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// probeEnv is the environment the load probe's Oh My Pi ran under.
func (c *operatorMachine) probeEnv() map[string]string {
	c.t.Helper()
	return readEnv(c.t, filepath.Join(c.record, "probe-env"))
}

// installPluginAt installs a pi-legion manifest declaring contract, and a pi-envoy manifest, under
// the Oh My Pi data root root, and answers pi-legion's package directory.
func (c *operatorMachine) installPluginAt(root string, contract int) string {
	c.t.Helper()
	plugins := filepath.Join(root, "plugins", "node_modules", "@sjawhar")
	for name, manifest := range map[string]string{
		"pi-legion": fmt.Sprintf(`{"name":"@sjawhar/pi-legion","version":"9.9.9","legion":{"daemonApiVersion":%d}}`, contract),
		"pi-envoy":  `{"name":"@sjawhar/pi-envoy","version":"9.9.9"}`,
	} {
		if err := os.MkdirAll(filepath.Join(plugins, name), 0o700); err != nil {
			c.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugins, name, "package.json"), []byte(manifest), 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
	return filepath.Join(plugins, "pi-legion")
}

func (c *operatorMachine) write(name, contents string, mode os.FileMode) {
	c.t.Helper()
	path := filepath.Join(c.dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		c.t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		c.t.Fatalf("chmod %s: %v", path, err)
	}
}

// run is `legion controller start --config <file> [extra…]`; the operator token is in neither
// stream, whatever happened.
func (c *operatorMachine) run(extra ...string) (int, string, string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "controller", "start", "--config", c.config}, extra...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, controllerOperatorToken) {
			c.t.Fatalf("legion controller start printed the operator token on %s: %q", name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// refused runs the command and requires it to fail with want on stderr.
func (c *operatorMachine) refused(want string, extra ...string) {
	c.t.Helper()
	code, _, errb := c.run(extra...)
	if code != 1 || !strings.Contains(errb, want) {
		c.t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q", code, errb, want)
	}
}

// launched says whether the recording Oh My Pi ran.
func (c *operatorMachine) launched() bool {
	_, err := os.Stat(filepath.Join(c.record, "argv"))
	return err == nil
}

func (c *operatorMachine) argv() []string {
	c.t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.record, "argv"))
	if err != nil {
		c.t.Fatalf("Oh My Pi was not launched: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func (c *operatorMachine) env() map[string]string {
	c.t.Helper()
	return readEnv(c.t, filepath.Join(c.record, "env"))
}

// readEnv reads an `env -0` recording.
func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Oh My Pi was not launched: %v", err)
	}
	env := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return env
}

func (c *operatorMachine) wantNoSecretRequest() {
	c.t.Helper()
	if requests := c.daemon.requests(); len(requests) != 0 {
		c.t.Fatalf("%d requests reached the daemon, want none: first %s %s", len(requests), requests[0].method, requests[0].path)
	}
}

func (c *operatorMachine) wantNothingLaunchedOrWritten(stateDir string) {
	c.t.Helper()
	if c.launched() {
		c.t.Fatal("Oh My Pi was launched")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		c.t.Fatalf("the state directory %s exists (%v); want nothing written", stateDir, err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// The one daemon call: POST /legion/v1/controller/secret, the operator token as a bearer, and the
// contract the probe held the plugin to as the body.
func TestControllerStartFetchesTheSecretWithTheOperatorBearer(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	requests := d.requests()
	if len(requests) != 2 {
		t.Fatalf("%d requests reached the daemon, want the secret request and the github-credential fetch", len(requests))
	}
	got := requests[0]
	if got.method != http.MethodPost || got.path != "/legion/v1/controller/secret" ||
		got.authorization != "Bearer "+controllerOperatorToken || got.contentType != "application/json" ||
		string(got.body) != fmt.Sprintf(`{"pluginContract":%d}`, api.DaemonAPIVersion) || got.status != http.StatusOK {
		t.Fatalf("secret request = %s %s auth %q type %q body %q → %d", got.method, got.path, got.authorization, got.contentType, got.body, got.status)
	}
}

// Under the default state directory: the minted capability 0600 in a 0700 secrets directory, the
// gh shim, the legion launcher, the deployment instructions, and the controller's working
// directory.
func TestControllerStartWritesTheSecretAndTheControllersFilesUnderTheStateDirectory(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	secretFile := filepath.Join(c.defaultDir, "secrets", "legion-demo-controller")
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	if sum := sha256.Sum256(secret); len(d.controller.minted) != 1 || !bytes.Equal(d.controller.minted[0], sum[:]) {
		t.Fatalf("the secret file does not hold the capability the daemon minted")
	}
	if got := modeOf(t, secretFile); got != 0o600 {
		t.Errorf("secret file mode %o, want 0600", got)
	}
	if got := modeOf(t, filepath.Join(c.defaultDir, "secrets")); got != 0o700 {
		t.Errorf("secrets directory mode %o, want 0700", got)
	}
	if got := modeOf(t, filepath.Join(c.defaultDir, "bin", "legion")); got&0o111 == 0 {
		t.Errorf("legion launcher mode %o, want executable", got)
	}
	instructions, err := os.ReadFile(filepath.Join(c.defaultDir, "deployment-instructions.md"))
	if err != nil || !strings.HasPrefix(string(instructions), "# Deployment instructions (demo)\n\nAlways be kind.") {
		t.Errorf("deployment instructions = %q, %v", instructions, err)
	}
	cwd, err := os.ReadFile(filepath.Join(c.record, "cwd"))
	if err != nil || strings.TrimSpace(string(cwd)) != filepath.Join(c.defaultDir, "controller") {
		t.Errorf("Oh My Pi ran in %q (%v), want %s", cwd, err, filepath.Join(c.defaultDir, "controller"))
	}
}

// Oh My Pi runs through the launch prefix, interactive (no --mode rpc, no --resume), with one
// --append-system-prompt holding the controller prompt, the daemon's design gate policy, and the
// deployment instructions; under the operator's own environment plus exactly the shared
// controller environment — the secrets as file pointers, never values — including
// LEGION_CONTROLLER_START_MESSAGE, which the extension sends as the session's first turn.
func TestControllerStartLaunchesOhMyPiWithTheSharedControllerEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	baseline := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		baseline[name] = value
	}
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}

	// The controller's role part, then the daemon's part for a controller the operator started.
	var controllerPrompt []byte
	for _, part := range []string{filepath.Join("shared", "controller-root.md"), filepath.Join("go", "controller-interactive.md")} {
		body, err := os.ReadFile(filepath.Join(c.defaultDir, "prompts", part))
		if err != nil {
			t.Fatalf("read the controller prompt snapshot: %v", err)
		}
		controllerPrompt = append(controllerPrompt, body...)
	}
	instructions, err := os.ReadFile(filepath.Join(c.defaultDir, "deployment-instructions.md"))
	if err != nil {
		t.Fatalf("read the deployment instructions: %v", err)
	}
	// `$(cat …)` drops each file's trailing newlines, as a shell does. The start message travels
	// as LEGION_CONTROLLER_START_MESSAGE, not a CLI word: argv carries only the system prompt.
	wantArgv := []string{"--append-system-prompt",
		strings.TrimRight(string(controllerPrompt), "\n") + "\n\nDesign gate policy: `gates.design: root-issues`.\n\n" +
			strings.TrimRight(string(instructions), "\n")}
	if got := c.argv(); !slices.Equal(got, wantArgv) {
		t.Fatalf("Oh My Pi's argv = %q\nwant %q", got, wantArgv)
	}

	env := c.env()
	added := map[string]string{}
	for name, value := range env {
		if old, ok := baseline[name]; !ok || old != value {
			added[name] = value
		}
	}
	// What sh itself sets is not the command's.
	for _, shells := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(added, shells)
	}
	secrets := filepath.Join(c.defaultDir, "secrets")
	bin := filepath.Join(c.defaultDir, "bin")
	want := map[string]string{
		"LEGION_TEST_PREFIX_RAN":          "1",
		"LEGION_CONTROLLER":               "1",
		"LEGION_ROLE":                     "controller",
		"LEGION_DAEMON_URL":               d.url,
		"LEGION_PROJECT":                  "demo",
		"LEGION_STATE_DIR":                c.defaultDir,
		"ENVOY_NATS_URL":                  "nats://a:4222,nats://b:4222",
		"ENVOY_URL":                       "http://envoy.test:9020",
		"PATH":                            bin + ":/usr/bin:/bin",
		"PI_SHELL_PREFIX":                 shellprefix.For(bin),
		"GH_CONFIG_DIR":                   filepath.Join(c.defaultDir, "gh"),
		"GH_TOKEN":                        "",
		"GITHUB_TOKEN":                    "",
		"GH_HOST":                         "",
		"LEGION_CONTROLLER_START_MESSAGE": daemon.ControllerStartMessage,
		"DISPATCH_URL":                    "https://dispatch.test",
		"DISPATCH_TOKEN_FILE":             filepath.Join(c.dir, "dispatch-token"),
		"LEGION_CONTROLLER_SECRET_FILE":   filepath.Join(secrets, "legion-demo-controller"),
		"ENVOY_TOKEN_FILE":                filepath.Join(c.dir, "envoy-token"),
		"NATS_NKEY_SEED_FILE":             filepath.Join(c.dir, "nats-seed"),
	}
	for name := range want {
		if old, ok := baseline[name]; ok && old == want[name] {
			// Set before the run to the value the command sets: not observable as added.
			delete(want, name)
		}
	}
	if len(added) != len(want) {
		t.Errorf("added environment has %d keys, want %d:\n got %v\nwant %v", len(added), len(want), added, want)
	}
	for name, value := range want {
		if added[name] != value {
			t.Errorf("%s = %q, want %q", name, added[name], value)
		}
	}
	secret, err := os.ReadFile(filepath.Join(secrets, "legion-demo-controller"))
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	for name, value := range env {
		if strings.Contains(value, string(secret)) {
			t.Errorf("%s carries the controller secret's value", name)
		}
	}
	for _, name := range []string{"LEGION_CONTROLLER_SECRET", "ENVOY_TOKEN", "DISPATCH_TOKEN", "NATS_NKEY_SEED"} {
		if _, set := env[name]; set {
			t.Errorf("%s is set; only its _FILE pointer may be", name)
		}
	}
	// Three lines: the probe, named before it runs so a prompt or a wait can be tied to it; the
	// no-App line, since this default daemon has no Tokens; then the launch.
	wantProbe := fmt.Sprintf("[legion] checking the controller's Oh My Pi (env 'LEGION_TEST_PREFIX_RAN=1' %s) in %s before the daemon mints a capability\n",
		filepath.Join(c.record, "omp"), filepath.Join(c.defaultDir, "controller"))
	wantNoApp := "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n"
	wantLog := fmt.Sprintf("[legion] starting the controller for demo against %s; state in %s\n", d.url, c.defaultDir)
	if errb != wantProbe+wantNoApp+wantLog {
		t.Errorf("stderr = %q, want %q", errb, wantProbe+wantNoApp+wantLog)
	}
}

// The controller is told the daemon's real design gate policy, so under `gates.design: off` its take
// comment promises no design approval that cannot happen (skill://legion-controller).
func TestControllerStartTellsTheControllerTheDaemonsDesignGatePolicy(t *testing.T) {
	d := newControllerDaemonGated(t, config.DesignGateOff)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	argv := c.argv()
	if len(argv) != 2 || !strings.Contains(argv[1], "\n\nDesign gate policy: `gates.design: off`.\n\n") {
		t.Fatalf("Oh My Pi's argv = %q; want the system prompt to carry the off policy line", argv)
	}
}

// A daemon that answers the secret without a policy it knows is refused, never guessed at: the
// controller would otherwise promise, or withhold, a design approval on an assumption.
func TestControllerSecretWithoutADesignGatePolicyIsRefused(t *testing.T) {
	for _, body := range []string{`{"secret":"s"}`, `{"secret":"s","designGate":"sometimes"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		_, _, err := fetchControllerSecret(context.Background(), server.URL, controllerOperatorToken, api.DaemonAPIVersion)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "not 'root-issues' or 'off'; upgrade the daemon") {
			t.Fatalf("fetchControllerSecret(%s) error = %v; want the policy refusal", body, err)
		}
	}
}

func TestControllerStartOmitsTheOptionalPointersTheFileDoesNotSet(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	for _, name := range []string{"DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE", "NATS_NKEY_SEED_FILE"} {
		os.Unsetenv(name)
	}
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	env := c.env()
	for _, name := range []string{"DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE", "NATS_NKEY_SEED_FILE"} {
		if _, set := env[name]; set {
			t.Errorf("%s is set with no key naming it", name)
		}
	}
	if env["LEGION_CONTROLLER"] != "1" {
		t.Errorf("LEGION_CONTROLLER = %q, want 1", env["LEGION_CONTROLLER"])
	}
}

func TestControllerStartExitsWithOhMyPisCode(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{exitCode: 3})
	if code, _, _ := c.run(); code != 3 {
		t.Fatalf("legion controller start = %d, want Oh My Pi's 3", code)
	}
}

// --daemon-url replaces the file's daemon_url for the secret request and for LEGION_DAEMON_URL.
func TestControllerStartDaemonURLOverridesTheFile(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: http://127.0.0.1:1", "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	if code, _, errb := c.run("--daemon-url", d.url+"/"); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	if n := len(d.requests()); n != 2 {
		t.Fatalf("%d requests reached the overriding daemon, want the secret request and the github-credential fetch", n)
	}
	if got := c.env()["LEGION_DAEMON_URL"]; got != d.url {
		t.Fatalf("LEGION_DAEMON_URL = %q, want %q", got, d.url)
	}
}

// A 403 names the URL and the one cause the route has: every Go daemon serves it, since none boots
// without operator_token_file. Nothing is written and nothing launched.
func TestControllerStartRefusedByTheDaemonWritesNothing(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{tokenContents: "not-the-operator-token\n"})
	code, _, errb := c.run()
	want := d.url + "/legion/v1/controller/secret: the daemon answered 403 Forbidden: Invalid operator token — the operator token does not match the daemon's operator_token_file\n"
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// A daemon from before contract 12 decodes the secret request strictly into an empty body, so it
// refuses the contract field with a 400 naming it as unknown. That refusal says which side to
// upgrade instead of quoting the daemon, and nothing is launched or written.
func TestControllerStartAgainstADaemonBeforeContract12NamesTheSideToUpgrade(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The contract-11 daemon's controllerSecret: readBody into struct{}, unknown fields refused.
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var empty struct{}
		if err := decoder.Decode(&empty); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body: " + err.Error()})
			return
		}
		t.Errorf("the contract-11 stand-in accepted %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(old.Close)
	c := newControllerStart(t, newControllerDaemon(t), controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + old.URL, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	code, _, errb := c.run()
	want := fmt.Sprintf("%s/legion/v1/controller/secret: the daemon speaks a daemon API contract older than 12, and this legion speaks %d; upgrade the daemon to this legion's release, or start the controller with the legion built with that daemon\n", old.URL, api.DaemonAPIVersion)
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// An unreachable daemon is named, and no other address is tried.
func TestControllerStartUnreachableDaemonIsNamedAndNoOtherAddressTried(t *testing.T) {
	d := newControllerDaemon(t)
	unreachable := "http://127.0.0.1:1"
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + unreachable, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	c.refused("could not reach the Legion daemon at " + unreachable + ": ")
	c.refused("; is the port-forward running? (never falls back to another address)")
	c.wantNoSecretRequest()
	c.wantNothingLaunchedOrWritten(c.defaultDir)
}

// Every refusal the operator's machine can find on its own comes before the one request: the
// daemon revokes the incumbent controller's capability on every mint, so a local failure must
// never cut it off for nothing.
func TestControllerStartRefusesLocallyBeforeTheRequest(t *testing.T) {
	t.Run("a group-readable operator token file, naming the path and mode", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{tokenMode: 0o640})
		c.refused(fmt.Sprintf("operator_token_file %s is readable by its group or others (mode 0640); chmod 0600 it", c.tokenFile))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a missing operator token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{omitToken: true})
		c.refused(fmt.Sprintf("operator_token_file names %s, which could not be read: ", c.tokenFile))
		c.wantNoSecretRequest()
	})
	t.Run("a blank operator token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{tokenContents: " \n"})
		c.refused(fmt.Sprintf("operator_token_file names %s, which is empty", c.tokenFile))
		c.wantNoSecretRequest()
	})
	t.Run("a blank Envoy token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("envoy-token", "\n", 0o600)
		c.refused(fmt.Sprintf("envoy_token_file names %s, which is empty", filepath.Join(c.dir, "envoy-token")))
		c.wantNoSecretRequest()
	})
	t.Run("a blank NATS nkey seed file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("nats-seed", " \n", 0o600)
		c.refused(fmt.Sprintf("nats_nkey_seed_file names %s, which is empty", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a missing NATS nkey seed file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "nats-seed"))
		c.refused(fmt.Sprintf("nats_nkey_seed_file names %s, which could not be read: ", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
	})
	t.Run("a NATS nkey seed file holding no user seed", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		seed := testnats.Account(t)
		c.write("nats-seed", seed+"\n", 0o600)
		want := fmt.Sprintf("nats_nkey_seed_file (%s) holds an nkey seed that is not a user's", filepath.Join(c.dir, "nats-seed"))
		if code, _, errb := c.run(); code != 1 || !strings.Contains(errb, want) || strings.Contains(errb, seed) {
			t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q, without the seed", code, errb, want)
		}
		c.wantNoSecretRequest()
	})
	t.Run("a NATS nkey seed file its group can read", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		if err := os.Chmod(filepath.Join(c.dir, "nats-seed"), 0o640); err != nil {
			t.Fatal(err)
		}
		c.refused(fmt.Sprintf("nats_nkey_seed_file %s is readable by its group or others (mode 0640); chmod 0600 it", filepath.Join(c.dir, "nats-seed")))
		c.wantNoSecretRequest()
	})
	t.Run("a missing Dispatch token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "dispatch-token"))
		c.refused(fmt.Sprintf("dispatch_token_file names %s, which could not be read: ", filepath.Join(c.dir, "dispatch-token")))
		c.wantNoSecretRequest()
	})
	t.Run("a missing instructions file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "instructions.md"))
		c.refused("instructions file " + filepath.Join(c.dir, "instructions.md") + " could not be read: ")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("a blank instructions file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.write("instructions.md", " \n", 0o600)
		c.refused("instructions file " + filepath.Join(c.dir, "instructions.md") + " is empty")
		c.wantNoSecretRequest()
	})
	t.Run("an Oh My Pi invocation it cannot resolve", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		t.Setenv("LEGION_OMP_PATH", "")
		c.refused("omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	// The mint revokes the incumbent controller, so an Oh My Pi whose plugin would refuse the
	// controller at session start is found before it: a second start from a profile nobody
	// updated leaves the working controller alone.
	t.Run("a plugin in the operator's Oh My Pi profile that speaks another contract, naming both", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.installPlugin(api.DaemonAPIVersion - 1)
		manifest := filepath.Join(c.home, ".omp", "plugins", "node_modules", "@sjawhar", "pi-legion", "package.json")
		c.refused(fmt.Sprintf("pi-legion at %s (package 9.9.9) speaks daemon API contract %d; this daemon requires %d.",
			manifest, api.DaemonAPIVersion-1, api.DaemonAPIVersion))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	// Each refusal of the load probe says what Oh My Pi did, launched as the controller launches
	// it, and the remedy under the controller's environment; none names a profile, which only Oh
	// My Pi resolves.
	for _, testCase := range []struct {
		name  string
		loads func(c *operatorMachine)
		want  []string
	}{
		{"an Oh My Pi that does not load the plugin", (*operatorMachine).loadsNothing,
			[]string{"did not load pi-legion (not installed, disabled, or unregistered). Install the @sjawhar/pi-legion release built from this daemon's commit into the Oh My Pi the controller runs", "plugin list` under the controller's environment"}},
		{"an Oh My Pi that loads the plugin but no pi-envoy", func(c *operatorMachine) { c.loadsEnvoyAt("none") },
			[]string{"loaded pi-legion but no pi-envoy. Install the @sjawhar/pi-envoy release built from this daemon's commit into the Oh My Pi the controller runs, and check it with `cd ", "plugin list`"}},
		{"an Oh My Pi whose pi-envoy publishes another interface", func(c *operatorMachine) { c.loadsEnvoyAt("2") },
			[]string{"loaded pi-envoy from file://" + "@HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js?mtime=1, which publishes plugin interface 2, and a pi-legion that speaks 1. Install both releases built from this daemon's commit into the Oh My Pi the controller runs, and check them with `cd ", "plugin list`"}},
		{"an Oh My Pi that still loads the pre-split package", func(c *operatorMachine) { c.loadsLegacyFrom("file:///plugins/legacy/dist/legion.js") },
			[]string{"loaded @sjawhar/pi-legion-envoy from file:///plugins/legacy/dist/legion.js beside pi-legion. Run `cd ", "plugin uninstall @sjawhar/pi-legion-envoy` under the controller's environment"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{})
			testCase.loads(c)
			code, _, errb := c.run()
			for _, want := range testCase.want {
				want = strings.ReplaceAll(want, "@HOME", c.home)
				if code != 1 || !strings.Contains(errb, want) {
					t.Fatalf("legion controller start = %d, stderr %q; want 1 and %q", code, errb, want)
				}
			}
			if strings.Contains(errb, "OMP profile") {
				t.Errorf("legion controller start's refusal names a profile, which only Oh My Pi resolves: %q", errb)
			}
			c.wantNoSecretRequest()
			c.wantNothingLaunchedOrWritten(c.defaultDir)
		})
	}
	t.Run("an unknown key, naming it and the example", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{lines: []string{
			"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]", "runtime: kubernetes",
		}})
		c.refused(`unknown key "runtime" in the controller configuration`)
		c.refused("deploy/kubernetes/daemon/controller.yaml.example")
		c.wantNoSecretRequest()
	})
}

// The plugin held to the contract is the one Oh My Pi reports loading, launched as the controller
// launches it: no resolution of its own decides which copy that is. Each row installs a copy of
// this binary's contract and one of another at two roots, and has Oh My Pi load one of them.
func TestControllerStartHoldsTheCopyOhMyPiLoadsToTheContract(t *testing.T) {
	t.Run("the loaded copy speaks the contract; the profile's own does not", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.installPluginAt(filepath.Join(c.home, ".omp"), api.DaemonAPIVersion-1)
		c.loads(c.installPluginAt(filepath.Join(c.home, "project", ".omp"), api.DaemonAPIVersion))
		if code, _, errb := c.run(); code != 0 || !c.launched() {
			t.Fatalf("legion controller start = %d (launched %t), stderr %q; want the controller started", code, c.launched(), errb)
		}
	})
	t.Run("the loaded copy speaks another contract; the profile's own speaks this one", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		loaded := c.installPluginAt(filepath.Join(c.home, "project", ".omp"), api.DaemonAPIVersion-1)
		c.loads(loaded)
		c.refused(fmt.Sprintf("pi-legion at %s (package 9.9.9) speaks daemon API contract %d; this daemon requires %d.",
			filepath.Join(loaded, "package.json"), api.DaemonAPIVersion-1, api.DaemonAPIVersion))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
}

// The load probe's Oh My Pi reads the operator's stdin, as the controller does: a launch prefix
// such as `secrets` scopes a human-tier grant by the terminal it is run from, and refuses on a
// stdin that is not one.
func TestControllerStartProbesOnTheOperatorsStdin(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	terminal, err := os.Create(filepath.Join(t.TempDir(), "operator-terminal"))
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	stdin := os.Stdin
	os.Stdin = terminal
	defer func() { os.Stdin = stdin }()

	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	probe, err := os.ReadFile(filepath.Join(c.record, "probe-stdin"))
	if err != nil {
		t.Fatal(err)
	}
	launch, err := os.ReadFile(filepath.Join(c.record, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(probe)); got != terminal.Name() || strings.TrimSpace(string(launch)) != terminal.Name() {
		t.Errorf("the load probe read %q and the controller %q, want both on the operator's %q", got, strings.TrimSpace(string(launch)), terminal.Name())
	}
}

// The load probe's Oh My Pi runs in the controller's own directory, which exists for it, under the
// environment the controller is then launched with.
func TestControllerStartProbesUnderTheControllersEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	cwd, err := os.ReadFile(filepath.Join(c.record, "probe-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(c.defaultDir, "controller")); strings.TrimSpace(string(cwd)) != want {
		t.Errorf("the load probe ran in %q, want the controller's directory %q", strings.TrimSpace(string(cwd)), want)
	}
	probe, launch := c.probeEnv(), c.env()
	for _, shell := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(probe, shell)
		delete(launch, shell)
	}
	if !maps.Equal(probe, launch) {
		t.Errorf("the load probe ran under %v, but the controller was launched under %v", probe, launch)
	}
	if probe["LEGION_CONTROLLER"] != "1" || probe["LEGION_CONTROLLER_SECRET_FILE"] == "" {
		t.Errorf("the load probe's environment lacks the controller's set: LEGION_CONTROLLER=%q LEGION_CONTROLLER_SECRET_FILE=%q",
			probe["LEGION_CONTROLLER"], probe["LEGION_CONTROLLER_SECRET_FILE"])
	}
}

// The controller is pane-side: the daemon's own NATS seed, by value or by pointer, exported in the
// operator's shell, reaches neither its load probe nor its Oh My Pi, while its pane seed pointer does.
func TestControllerStartDropsTheDaemonSeedFromTheControllersEnvironment(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	const seed = "SUAMDAEMONSEEDVALUEFORTHECONTROLLERTEST"
	t.Setenv("NATS_DAEMON_NKEY_SEED", seed)
	t.Setenv("NATS_DAEMON_NKEY_SEED_FILE", filepath.Join(c.dir, "daemon-seed"))
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		for name, value := range env {
			if strings.HasPrefix(name, "NATS_DAEMON_NKEY_SEED") || strings.Contains(value, seed) {
				t.Errorf("the %s's environment carries the daemon seed: %s", kind, name)
			}
		}
		if env["NATS_NKEY_SEED_FILE"] != filepath.Join(c.dir, "nats-seed") {
			t.Errorf("the %s's NATS_NKEY_SEED_FILE = %q, want the pane seed's file", kind, env["NATS_NKEY_SEED_FILE"])
		}
	}
}

// A start from inside a Legion pane inherits that pane's boot token, by value or by pointer. The
// plugin takes a controller session carrying one for a controller the daemon launched (`controller:
// daemon`), which registers with it, so neither reaches the operator's controller or its load probe.
func TestControllerStartDropsAnInheritedBootToken(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	t.Setenv("LEGION_BOOT_TOKEN", "a-pane-boot-token")
	t.Setenv("LEGION_BOOT_TOKEN_FILE", filepath.Join(c.dir, "boot-token"))
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
		for _, name := range []string{"LEGION_BOOT_TOKEN", "LEGION_BOOT_TOKEN_FILE"} {
			if value, set := env[name]; set {
				t.Errorf("the %s's environment carries %s=%q", kind, name, value)
			}
		}
	}
}

// `project: sjawhar/Legion`, copied from the daemon's legion.yaml, names the daemon's own token in
// the state directory, the secret file and LEGION_PROJECT.
func TestControllerStartSanitizesTheProjectAsTheDaemonDoes(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: sjawhar/Legion", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	stateDir := filepath.Join(c.home, ".local", "state", "legion", "sjawharlegion-controller")
	env := c.env()
	for name, want := range map[string]string{
		"LEGION_PROJECT":                "sjawharlegion",
		"LEGION_STATE_DIR":              stateDir,
		"LEGION_CONTROLLER_SECRET_FILE": filepath.Join(stateDir, "secrets", "legion-sjawharlegion-controller"),
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

// state_dir in the file replaces the default, resolved against the file's directory; so does an
// absolute XDG_STATE_HOME the default.
func TestControllerStartStateDirectory(t *testing.T) {
	t.Run("from the file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{lines: []string{
			"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]", "state_dir: ./state",
		}})
		if code, _, errb := c.run(); code != 0 {
			t.Fatalf("legion controller start = %d, stderr %q", code, errb)
		}
		if got := c.env()["LEGION_STATE_DIR"]; got != filepath.Join(c.dir, "state") {
			t.Fatalf("LEGION_STATE_DIR = %q, want %s", got, filepath.Join(c.dir, "state"))
		}
		if _, err := os.Stat(filepath.Join(c.dir, "state", "secrets", "legion-demo-controller")); err != nil {
			t.Fatalf("the secret file is not under state_dir: %v", err)
		}
	})
	t.Run("under XDG_STATE_HOME", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		stateHome := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)
		if code, _, errb := c.run(); code != 0 {
			t.Fatalf("legion controller start = %d, stderr %q", code, errb)
		}
		if got, want := c.env()["LEGION_STATE_DIR"], filepath.Join(stateHome, "legion", "demo-controller"); got != want {
			t.Fatalf("LEGION_STATE_DIR = %q, want %s", got, want)
		}
	})
}

func TestControllerStartUsage(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"legion", "controller"}, subcommandUsage("controller", controllerCommands)},
		{[]string{"legion", "controller", "stop"}, `legion controller: unknown subcommand "stop"`},
		{[]string{"legion", "controller", "start"}, "usage: legion controller start --config <controller.yaml> [--daemon-url <url>]"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), tc.argv, &out, &errb); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%v = %d, stderr %q; want exit 2 and %q", tc.argv, code, errb.String(), tc.want)
		}
	}
}

// controllerCredentialTokens is a Tokens fake for the controller's GitHub credential route: its
// token is read and set under a lock, so a test can flip it between the refresh loop's ticks, or
// have every mint fail, for the 502 case.
type controllerCredentialTokens struct {
	mu    sync.Mutex
	token string
	err   error
}

func newControllerCredentialTokens(token string) *controllerCredentialTokens {
	return &controllerCredentialTokens{token: token}
}

func (c *controllerCredentialTokens) set(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

func (c *controllerCredentialTokens) failWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *controllerCredentialTokens) Token(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return appauth.Lease{}, c.err
	}
	return appauth.Lease{Token: c.token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// requestsTo answers how many of d's recorded requests were to path.
func requestsTo(d *controllerDaemon, path string) int {
	n := 0
	for _, r := range d.requests() {
		if r.path == path {
			n++
		}
	}
	return n
}

// The controller's gh directory holds the review App's token as the daemon's credential route
// rendered it, written before Oh My Pi starts, under GH_CONFIG_DIR, with one request made before
// the loop.
func TestControllerStartWritesTheControllersGitHubCredentialUnderGh(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	ghDir := filepath.Join(c.defaultDir, "gh")
	hosts, err := os.ReadFile(filepath.Join(ghDir, "hosts.yml"))
	if err != nil || string(hosts) != ghconfig.Hosts("ghs_first") {
		t.Fatalf("hosts.yml = %q, %v, want %q", hosts, err, ghconfig.Hosts("ghs_first"))
	}
	cfg, err := os.ReadFile(filepath.Join(ghDir, "config.yml"))
	if err != nil || string(cfg) != ghconfig.Config {
		t.Fatalf("config.yml = %q, %v, want %q", cfg, err, ghconfig.Config)
	}
	if got := c.env()["GH_CONFIG_DIR"]; got != ghDir {
		t.Errorf("GH_CONFIG_DIR = %q, want %s", got, ghDir)
	}
	startHosts, err := os.ReadFile(filepath.Join(c.record, "hosts-at-start"))
	if err != nil || string(startHosts) != ghconfig.Hosts("ghs_first") {
		t.Fatalf("hosts.yml at Oh My Pi's start = %q, %v; want it to already hold the token", startHosts, err)
	}
	if n := requestsTo(d, "/legion/v1/controller/github-credential"); n != 1 {
		t.Fatalf("%d github-credential requests, want 1", n)
	}
	secret, err := os.ReadFile(filepath.Join(c.defaultDir, "secrets", "legion-demo-controller"))
	if err != nil {
		t.Fatalf("read the secret file: %v", err)
	}
	wantBody, err := json.Marshal(map[string]string{"secret": string(secret)})
	if err != nil {
		t.Fatal(err)
	}
	var credentialRequest seenRequest
	for _, r := range d.requests() {
		if r.path == "/legion/v1/controller/github-credential" {
			credentialRequest = r
		}
	}
	if string(credentialRequest.body) != string(wantBody) {
		t.Errorf("github-credential request body = %s, want %s", credentialRequest.body, wantBody)
	}
	if !strings.Contains(errb, "[legion] the controller's gh acts as the review App from "+ghDir) {
		t.Errorf("stderr = %q; want it to name the review App and the gh directory", errb)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "github-credential.log")); err != nil {
		t.Errorf("github-credential.log does not exist: %v", err)
	}
}

// The refresh loop keeps the controller's gh directory fresh while Oh My Pi runs: a token the
// daemon mints after Oh My Pi starts reaches hosts.yml before Oh My Pi exits, and the log records
// the refresh. The recording Oh My Pi runs until the test releases it, once it has seen the second
// token land, so the proof does not depend on the machine's speed.
func TestControllerStartRefreshesTheGitHubCredentialWhileOhMyPiRuns(t *testing.T) {
	old := controllerGitHubRefreshInterval
	controllerGitHubRefreshInterval = 50 * time.Millisecond
	t.Cleanup(func() { controllerGitHubRefreshInterval = old })

	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	release := filepath.Join(t.TempDir(), "release")
	c := newControllerStart(t, d, controllerOptions{waitForFile: release, exitCode: 7})

	type result struct {
		code int
		errb string
	}
	done := make(chan result, 1)
	go func() {
		code, _, errb := c.run()
		done <- result{code, errb}
	}()
	hostsFile := filepath.Join(c.defaultDir, "gh", "hosts.yml")
	// The first write holds the first token; only then does the daemon's lease change, so the
	// loop, not the first fetch, is what carries the second token into the file.
	awaitFileContaining(t, hostsFile, "ghs_first", 30*time.Second)
	tokens.set("ghs_second")
	awaitFileContaining(t, hostsFile, "ghs_second", 30*time.Second)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("legion controller start did not return")
	}
	if r.code != 7 {
		t.Fatalf("legion controller start = %d, want 7 (Oh My Pi's exit code); stderr %q", r.code, r.errb)
	}
	hosts, err := os.ReadFile(hostsFile)
	if err != nil || string(hosts) != ghconfig.Hosts("ghs_second") {
		t.Fatalf("hosts.yml = %q, %v; want the second token", hosts, err)
	}
	logBody, err := os.ReadFile(filepath.Join(c.defaultDir, "github-credential.log"))
	if err != nil || !strings.Contains(string(logBody), "github credential refreshed") {
		t.Fatalf("github-credential.log = %q, %v; want a refreshed line", logBody, err)
	}
}

// A superseded capability (a later legion controller start) ends the refresh loop: the gh
// directory keeps the first token, the log records the stop, and no further request reaches the
// daemon. The recording Oh My Pi runs until the test releases it, once it has seen the stop logged
// and the requests settle.
func TestControllerStartStopsTheRefreshLoopWhenTheCapabilityIsSuperseded(t *testing.T) {
	old := controllerGitHubRefreshInterval
	controllerGitHubRefreshInterval = 50 * time.Millisecond
	t.Cleanup(func() { controllerGitHubRefreshInterval = old })

	tokens := newControllerCredentialTokens("ghs_first")
	d := newControllerDaemonWithTokens(t, tokens)
	release := filepath.Join(t.TempDir(), "release")
	c := newControllerStart(t, d, controllerOptions{waitForFile: release})

	done := make(chan struct{})
	go func() {
		c.run()
		close(done)
	}()
	hostsFile := filepath.Join(c.defaultDir, "gh", "hosts.yml")
	awaitFile(t, hostsFile, 30*time.Second)
	// Mint a second capability, which supersedes the one this controller holds.
	status, _, err := (operator{base: d.url, bearer: controllerOperatorToken}).do(context.Background(), http.MethodPost,
		"/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion})
	if err != nil || status != http.StatusOK {
		t.Fatalf("mint a second capability: status %d, err %v", status, err)
	}
	logFile := filepath.Join(c.defaultDir, "github-credential.log")
	awaitFileContaining(t, logFile, "github credential refresh stopped", 30*time.Second)
	before := requestsTo(d, "/legion/v1/controller/github-credential")
	time.Sleep(10 * controllerGitHubRefreshInterval)
	if after := requestsTo(d, "/legion/v1/controller/github-credential"); after != before {
		t.Fatalf("%d further github-credential requests reached the daemon after the stop, want 0 more", after-before)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("legion controller start did not return")
	}
	hosts, err := os.ReadFile(hostsFile)
	if err != nil || string(hosts) != ghconfig.Hosts("ghs_first") {
		t.Fatalf("hosts.yml = %q, %v; want it to keep the first token", hosts, err)
	}
}

// A daemon with no GitHub App for the project writes an empty gh directory and runs no loop,
// stating so, and Oh My Pi still starts.
func TestControllerStartWithNoGitHubAppWritesAnEmptyGhDirectory(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	ghDir := filepath.Join(c.defaultDir, "gh")
	if got := modeOf(t, ghDir); got != 0o700 {
		t.Errorf("gh directory mode = %o, want 0700", got)
	}
	if _, err := os.Stat(filepath.Join(ghDir, "hosts.yml")); !os.IsNotExist(err) {
		t.Errorf("hosts.yml exists (%v); want none, since the daemon has no GitHub App", err)
	}
	if !strings.Contains(errb, "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody\n") {
		t.Errorf("stderr = %q; want the no-App line", errb)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "github-credential.log")); !os.IsNotExist(err) {
		t.Errorf("github-credential.log exists (%v); want none, since no loop ran", err)
	}
	if n := requestsTo(d, "/legion/v1/controller/github-credential"); n != 1 {
		t.Fatalf("%d github-credential requests, want 1", n)
	}
}

// A failed GitHub credential fetch after the mint exits 1, naming the route and the daemon's
// sentence; Oh My Pi never ran, and the secret file stays written.
func TestControllerStartExitsWhenTheGitHubCredentialFetchFails(t *testing.T) {
	tokens := newControllerCredentialTokens("ghs_first")
	tokens.failWith(errors.New("github is down"))
	d := newControllerDaemonWithTokens(t, tokens)
	c := newControllerStart(t, d, controllerOptions{})
	code, _, errb := c.run()
	want := d.url + "/legion/v1/controller/github-credential: GitHub token exchange failed\n"
	if code != 1 || !strings.HasSuffix(errb, want) {
		t.Fatalf("legion controller start = %d, stderr %q; want 1 and a refusal ending %q", code, errb, want)
	}
	if c.launched() {
		t.Fatal("Oh My Pi was launched")
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "secrets", "legion-demo-controller")); err != nil {
		t.Errorf("the secret file does not exist (%v); want it left as controllerStart's other post-mint failures leave it", err)
	}
}
