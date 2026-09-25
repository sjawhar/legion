package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/prompts"
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
	t.Helper()
	d := &controllerDaemon{t: t, controller: &memoryController{}, dispatch: &statusWrites{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Project: controllerProject, OperatorToken: controllerOperatorToken, Controller: d.controller,
		Dispatch: d.dispatch, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	lines := opts.lines
	if lines == nil {
		lines = []string{
			"project: " + controllerProject,
			"daemon_url: " + d.url,
			"operator_token_file: ./operator-token",
			"envoy_url: http://envoy.test:9020",
			"envoy_token_file: envoy-token",
			"nats_urls: [nats://a:4222, nats://b:4222]",
			"dispatch_url: https://dispatch.test",
			"dispatch_token_file: ./dispatch-token",
			"instructions: ./instructions.md",
			"omp_launch_prefix: [env, LEGION_TEST_PREFIX_RAN=1]",
		}
	}
	c.write("controller.yaml", strings.Join(lines, "\n")+"\n", 0o600)

	// The Oh My Pi LEGION_OMP_PATH names: it records its argv, environment, and working
	// directory, and exits with the code the test asked for.
	omp := filepath.Join(record, "omp")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\0' \"$a\"; done >%[1]s/argv\nenv -0 >%[1]s/env\npwd >%[1]s/cwd\nexit %[2]d\n",
		record, opts.exitCode)
	if err := os.WriteFile(omp, []byte(script), 0o700); err != nil {
		t.Fatalf("write the recording omp: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OMP_PROFILE", "")
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PATH", "/usr/bin:/bin:/opt/x/worker-bin")
	t.Setenv("LEGION_OMP_PATH", omp)
	t.Setenv("LEGION_ROLE_PROMPTS_DIR", prompts.SourceRolePromptsDir())
	c.installPlugin(api.GoDaemonAPIVersion)
	return c
}

// installPlugin installs, in the operator's default Oh My Pi profile, a pi-legion-envoy manifest
// declaring contract.
func (c *operatorMachine) installPlugin(contract int) {
	c.t.Helper()
	c.installPluginAt(filepath.Join(c.home, ".omp"), contract)
}

// installPluginAt installs a pi-legion-envoy manifest declaring contract under the Oh My Pi data
// root root.
func (c *operatorMachine) installPluginAt(root string, contract int) {
	c.t.Helper()
	dir := filepath.Join(root, "plugins", "node_modules", "@sjawhar", "pi-legion-envoy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"name":"@sjawhar/pi-legion-envoy","version":"9.9.9","legion":{"goDaemonApiVersion":%d}}`, contract)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o600); err != nil {
		c.t.Fatal(err)
	}
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
	raw, err := os.ReadFile(filepath.Join(c.record, "env"))
	if err != nil {
		c.t.Fatalf("Oh My Pi was not launched: %v", err)
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

// The one daemon call: POST /legion/v1/controller/secret, the operator token as a bearer, an
// empty JSON object as the body.
func TestControllerStartFetchesTheSecretWithTheOperatorBearer(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	requests := d.requests()
	if len(requests) != 1 {
		t.Fatalf("%d requests reached the daemon, want exactly the secret request", len(requests))
	}
	got := requests[0]
	if got.method != http.MethodPost || got.path != "/legion/v1/controller/secret" ||
		got.authorization != "Bearer "+controllerOperatorToken || got.contentType != "application/json" ||
		string(got.body) != "{}" || got.status != http.StatusOK {
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
	if got := modeOf(t, filepath.Join(c.defaultDir, "worker-bin", "gh")); got != 0o700 {
		t.Errorf("gh shim mode %o, want 0700", got)
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
// --append-system-prompt holding the controller prompt and the deployment instructions, under
// the operator's own environment plus exactly the shared controller environment — the secrets as
// file pointers, never values.
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

	controllerPrompt, err := os.ReadFile(filepath.Join(prompts.SourceRolePromptsDir(), "controller-root.md"))
	if err != nil {
		t.Fatalf("read controller-root.md: %v", err)
	}
	instructions, err := os.ReadFile(filepath.Join(c.defaultDir, "deployment-instructions.md"))
	if err != nil {
		t.Fatalf("read the deployment instructions: %v", err)
	}
	// `$(cat …)` drops each file's trailing newlines, as a shell does.
	wantArgv := []string{"--append-system-prompt",
		strings.TrimRight(string(controllerPrompt), "\n") + "\n\n" + strings.TrimRight(string(instructions), "\n")}
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
	workerBin, bin := filepath.Join(c.defaultDir, "worker-bin"), filepath.Join(c.defaultDir, "bin")
	want := map[string]string{
		"LEGION_TEST_PREFIX_RAN":        "1",
		"LEGION_CONTROLLER":             "1",
		"LEGION_ROLE":                   "controller",
		"LEGION_DAEMON_API":             "go",
		"LEGION_DAEMON_URL":             d.url,
		"LEGION_PROJECT":                "demo",
		"LEGION_STATE_DIR":              c.defaultDir,
		"ENVOY_NATS_URL":                "nats://a:4222,nats://b:4222",
		"ENVOY_URL":                     "http://envoy.test:9020",
		"PATH":                          workerBin + ":" + bin + ":/usr/bin:/bin",
		"PI_SHELL_PREFIX":               "PATH='" + workerBin + ":" + bin + ":'${PATH#'" + workerBin + ":" + bin + ":'} &&",
		"GH_CONFIG_DIR":                 filepath.Join(c.defaultDir, "gh"),
		"GH_TOKEN":                      "",
		"GITHUB_TOKEN":                  "",
		"GH_HOST":                       "",
		"LEGION_GRANT_FILE":             filepath.Join(secrets, "legion-demo-controller-grant"),
		"DISPATCH_URL":                  "https://dispatch.test",
		"DISPATCH_TOKEN_FILE":           filepath.Join(c.dir, "dispatch-token"),
		"LEGION_CONTROLLER_SECRET_FILE": filepath.Join(secrets, "legion-demo-controller"),
		"ENVOY_TOKEN_FILE":              filepath.Join(c.dir, "envoy-token"),
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
	for _, name := range []string{"LEGION_CONTROLLER_SECRET", "ENVOY_TOKEN", "DISPATCH_TOKEN"} {
		if _, set := env[name]; set {
			t.Errorf("%s is set; only its _FILE pointer may be", name)
		}
	}
	wantLog := fmt.Sprintf("[legion] starting the controller for demo against %s; state in %s\n", d.url, c.defaultDir)
	if errb != wantLog {
		t.Errorf("stderr = %q, want %q", errb, wantLog)
	}
}

func TestControllerStartOmitsTheOptionalPointersTheFileDoesNotSet(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: demo", "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
	}})
	for _, name := range []string{"DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE"} {
		os.Unsetenv(name)
	}
	if code, _, errb := c.run(); code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	env := c.env()
	for _, name := range []string{"DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE"} {
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
	if n := len(d.requests()); n != 1 {
		t.Fatalf("%d requests reached the overriding daemon, want 1", n)
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
	want := d.url + "/legion/v1/controller/secret answered 403: Invalid operator token — the operator token does not match the daemon's operator_token_file\n"
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
	t.Run("a missing Dispatch token file", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		os.Remove(filepath.Join(c.dir, "dispatch-token"))
		c.refused(fmt.Sprintf("dispatch_token_file names %s, which could not be read: ", filepath.Join(c.dir, "dispatch-token")))
		c.wantNoSecretRequest()
	})
	t.Run("an unusable role-prompt directory, naming it and controller-root.md", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		empty := t.TempDir()
		t.Setenv("LEGION_ROLE_PROMPTS_DIR", empty)
		c.refused("Role prompts directory " + empty + " is missing")
		c.refused("controller-root.md")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
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
	// The mint revokes the incumbent controller, so an Oh My Pi whose plugin would refuse the Go
	// controller at session start is found before it: a second start from a profile nobody
	// updated leaves the working controller alone.
	t.Run("a plugin in the operator's Oh My Pi profile that speaks another contract, naming both", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		c.installPlugin(api.GoDaemonAPIVersion - 1)
		manifest := filepath.Join(c.home, ".omp", "plugins", "node_modules", "@sjawhar", "pi-legion-envoy", "package.json")
		c.refused(fmt.Sprintf("pi-legion-envoy at %s (package 9.9.9) speaks Go daemon API contract %d; this daemon requires %d.",
			manifest, api.GoDaemonAPIVersion-1, api.GoDaemonAPIVersion))
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
	t.Run("no plugin in the operator's Oh My Pi profile", func(t *testing.T) {
		d := newControllerDaemon(t)
		c := newControllerStart(t, d, controllerOptions{})
		if err := os.RemoveAll(filepath.Join(c.home, ".omp")); err != nil {
			t.Fatal(err)
		}
		c.refused("pi-legion-envoy manifest at " + filepath.Join(c.home, ".omp", "plugins", "node_modules", "@sjawhar", "pi-legion-envoy", "package.json") + " could not be read")
		c.wantNoSecretRequest()
		c.wantNothingLaunchedOrWritten(c.defaultDir)
	})
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

// The contract check reads the manifest Oh My Pi loads under the operator's environment, which the
// controller's Oh My Pi inherits whole (@oh-my-pi/pi-utils dirs.ts: getBaseConfigRoot,
// DirResolver's constructor, resolveActiveAgentDirOverride, getPluginsDir). Each row installs the
// release Oh My Pi loads where it loads it, and a plugin speaking another contract at the root a
// wrong resolution would read: the controller starts only when the check read the first. An
// honoured PI_CODING_AGENT_DIR moves the plugins only by turning the XDG data root off, so with
// none it changes nothing; PI_CONFIG_DIR moves the config root, and an XDG data root still wins.
// A relative agent directory is resolved where the controller's Oh My Pi runs, the controller's
// state directory's `controller`. An Oh My Pi running under a profile hands its children
// PI_CODING_AGENT_DIR set to that profile's agent directory, which Oh My Pi ignores, so an operator
// starting from inside one is not refused.
func TestControllerStartChecksThePluginOhMyPiLoads(t *testing.T) {
	for _, tc := range []struct {
		name string
		// env names variables beyond HOME; "<home>" in a value is the operator's home directory.
		env map[string]string
		// loaded is the root Oh My Pi loads the plugin from, stale the one a wrong resolution reads,
		// both relative to the home directory; "" installs nothing there.
		loaded, stale string
	}{
		{name: "PI_CODING_AGENT_DIR honoured with no XDG data root",
			env: map[string]string{"PI_CODING_AGENT_DIR": "<home>/elsewhere"}, loaded: ".omp"},
		{name: "PI_CODING_AGENT_DIR honoured with an XDG data root",
			env: map[string]string{"PI_CODING_AGENT_DIR": "<home>/elsewhere", "XDG_DATA_HOME": "<home>/xdg"}, loaded: ".omp", stale: "xdg/omp"},
		{name: "PI_CONFIG_DIR with no XDG data root",
			env: map[string]string{"PI_CONFIG_DIR": ".omp-alt"}, loaded: ".omp-alt", stale: ".omp"},
		{name: "PI_CONFIG_DIR with an XDG data root",
			env: map[string]string{"PI_CONFIG_DIR": ".omp-alt", "XDG_DATA_HOME": "<home>/xdg"}, loaded: "xdg/omp", stale: ".omp"},
		{name: "a named profile, which ignores PI_CODING_AGENT_DIR",
			env: map[string]string{"OMP_PROFILE": "work", "PI_CODING_AGENT_DIR": "<home>/.omp/profiles/work/agent"}, loaded: ".omp/profiles/work", stale: ".omp"},
		{name: "PI_PROFILE's agent directory under the default profile",
			env: map[string]string{"PI_PROFILE": "work", "PI_CODING_AGENT_DIR": "<home>/.omp/profiles/work/agent", "XDG_DATA_HOME": "<home>/xdg"}, loaded: "xdg/omp", stale: ".omp"},
		{name: "a relative PI_CODING_AGENT_DIR, resolved in the controller's directory",
			env: map[string]string{"PI_CODING_AGENT_DIR": "agent", "XDG_DATA_HOME": "<home>/xdg"}, loaded: ".omp", stale: "xdg/omp"},
		{name: "a relative PI_CODING_AGENT_DIR that names the config root's own agent directory from the controller's",
			env: map[string]string{"PI_CODING_AGENT_DIR": "../../../../../.omp/agent", "XDG_DATA_HOME": "<home>/xdg"}, loaded: "xdg/omp", stale: ".omp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{})
			if err := os.RemoveAll(filepath.Join(c.home, ".omp")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PI_PROFILE", "")
			for name, value := range tc.env {
				t.Setenv(name, strings.ReplaceAll(value, "<home>", c.home))
			}
			c.installPluginAt(filepath.Join(c.home, tc.loaded), api.GoDaemonAPIVersion)
			if tc.stale != "" {
				c.installPluginAt(filepath.Join(c.home, tc.stale), api.GoDaemonAPIVersion-1)
			}
			if code, _, errb := c.run(); code != 0 || !c.launched() {
				t.Fatalf("legion controller start = %d (launched %t), stderr %q; want the controller started", code, c.launched(), errb)
			}
		})
	}
}

// `project: sjawhar/Legion`, copied from the daemon's legion.yaml, names the daemon's own token in
// the state directory, the secret file, the grant file, and LEGION_PROJECT.
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
		"LEGION_GRANT_FILE":             filepath.Join(stateDir, "secrets", "legion-sjawharlegion-controller-grant"),
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
	for _, argv := range [][]string{
		{"legion", "controller"},
		{"legion", "controller", "stop"},
		{"legion", "controller", "start"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), argv, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: legion controller start --config <controller.yaml> [--daemon-url <url>]") {
			t.Errorf("%v = %d, stderr %q; want the usage and exit 2", argv, code, errb.String())
		}
	}
}
