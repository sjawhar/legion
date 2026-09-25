package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

const controllerUsage = "usage: legion controller start --config <controller.yaml> [--daemon-url <url>]"

// controllerSecretVariable names the controller capability's secret: its file is written under it,
// and the session finds that file through the variable with "_FILE" appended, as every pane finds
// a secret file.
const controllerSecretVariable = "LEGION_CONTROLLER_SECRET"

func runController(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "start" {
		fmt.Fprintln(stderr, controllerUsage)
		return 2
	}
	flags := newFlags("controller start", stderr)
	configPath := flags.String("config", "", "path to the operator-side controller.yaml (required)")
	daemonURL := flags.String("daemon-url", "", "the daemon's API address, overriding the file's daemon_url")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *configPath == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, controllerUsage)
		return 2
	}
	code, err := controllerStart(ctx, *configPath, *daemonURL, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "legion controller start: %v\n", err)
		return 1
	}
	return code
}

// controllerStart is `legion controller start`, the operator's side of a controller the daemon
// cannot launch itself (LEGION-206 Requirement 11; the shipped cmdControllerStart,
// packages/daemon/src/cli/controller-start.ts:262-384). In order, and nothing is kept, and nothing
// but the probe is launched, until the daemon has answered: read the strict operator-side file;
// refuse an operator token file others can read, and a blank or unreadable Envoy or Dispatch token
// file, a role-prompt bundle missing a file, an instructions file that is missing or blank, and an
// Oh My Pi invocation that does not resolve; then probe that Oh My Pi as the controller will run it
// — the launch prefix, the invocation, the controller's whole environment, in
// `<state_dir>/controller`, created for it, at the operator's terminal — and refuse a
// pi-legion-envoy it does not load, or loads speaking another Go daemon API contract than this
// binary's, which would refuse the controller at session start (daemon.ProbeController). The probe
// runs `omp models`, which starts no session, so it neither registers, takes the controller role,
// nor reads a controller secret. Then fetch the controller secret with the operator token as a
// bearer (the daemon mints a fresh capability and revokes the previous controller's); write it
// 0600 under the local state directory beside the gh shim, the `legion` launcher, and the
// deployment instructions; then run Oh My Pi interactive — the launch prefix and the resolved invocation, one joined
// `--append-system-prompt`, no `--resume`, no `--mode rpc` — in the foreground with the same
// environment, and answer its exit code. A refusal before the secret is written removes the
// directories made for the probe, so the state directory is as it was.
func controllerStart(ctx context.Context, configPath, daemonURL string, stderr io.Writer) (int, error) {
	absolute, err := filepath.EvalSymlinks(configPath)
	if err == nil {
		absolute, err = filepath.Abs(absolute)
	}
	if err != nil {
		return 0, fmt.Errorf("controller configuration %s could not be read: %w", configPath, err)
	}
	cfg, err := config.LoadController(absolute, daemonURL)
	if err != nil {
		return 0, err
	}
	operatorToken, err := config.ReadOperatorTokenFile("operator_token_file", cfg.OperatorTokenFile)
	if err != nil {
		return 0, err
	}
	// Every check this machine can make on its own comes before the fetch, and none of them writes
	// under the state directory but the probe's own directory, removed again on a refusal: the
	// daemon mints a fresh capability on every request and revokes the incumbent controller's, so
	// a failure found here never cuts the running controller off for nothing.
	if cfg.EnvoyTokenFile != "" {
		if _, err := config.ReadSecretPointer("envoy_token_file", cfg.EnvoyTokenFile); err != nil {
			return 0, err
		}
	}
	if cfg.DispatchTokenFile != "" {
		if _, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return 0, err
		}
	}
	rolesDir, err := prompts.ResolveRolePromptsDir(os.LookupEnv)
	if err != nil {
		return 0, err
	}
	if cfg.InstructionsPath != "" {
		if _, err := config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return 0, err
		}
	}
	invocation, err := omplaunch.ResolveInvocation(cfg.OmpInvocation, os.Getenv)
	if err != nil {
		return 0, err
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, fmt.Errorf("find the home directory the controller's state lives under: %w", err)
		}
		stateDir = filepath.Join(filepath.Dir(registry.Path(nil, home)), cfg.Project+"-controller")
	}
	controllerDir := filepath.Join(stateDir, "controller")
	token := string(legionclaim.ControllerToken(cfg.Project))
	// One environment, probed and then launched: the operator's own with the controller's set on
	// top, later pairs replacing inherited values of the same name.
	env := processEnvironment()
	for _, pair := range controllerEnvironment(cfg, stateDir, token, runtime.SecretFilePath(stateDir, token)) {
		env[pair[0]] = pair[1]
	}
	created, err := makeDirs(controllerDir)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(stderr, "[legion] checking the controller's Oh My Pi (%s) in %s before the daemon mints a capability\n",
		omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation), controllerDir)
	secret, err := probeAndMint(ctx, cfg.DaemonURL, daemon.ControllerProbe{
		Omp: invocation, Prefix: cfg.OmpLaunchPrefix, Env: env, WorkDir: controllerDir, Stdin: os.Stdin, Stderr: stderr,
		Contract: api.GoDaemonAPIVersion, Log: slog.New(slog.NewTextHandler(stderr, nil)),
	}, operatorToken)
	if err != nil {
		removeDirs(created)
		return 0, err
	}

	if _, err := runtime.WriteSecretFile(stateDir, token, controllerSecretVariable, secret); err != nil {
		return 0, fmt.Errorf("write the controller secret: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve the legion executable the controller's launcher runs: %w", err)
	}
	if err := workerbin.Install(stateDir, executable); err != nil {
		return 0, err
	}
	instructionsFile := ""
	if cfg.InstructionsPath != "" {
		if instructionsFile, err = config.MaterializeDeploymentInstructions(cfg.InstructionsPath, stateDir, cfg.Project); err != nil {
			return 0, err
		}
	}
	command := omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation) + " " + omplaunch.SystemPromptArgument(runtime.PromptParts{
		RolePromptPaths:            []string{filepath.Join(rolesDir, "controller-root.md")},
		DeploymentInstructionsPath: instructionsFile,
	})
	fmt.Fprintf(stderr, "[legion] starting the controller for %s against %s; state in %s\n", cfg.Project, cfg.DaemonURL, stateDir)
	// Interactive and in the foreground: the operator's terminal is Oh My Pi's. The child is not
	// bound to ctx — a Ctrl-C reaches Oh My Pi through the terminal's process group and is its to
	// handle, while this process, whose signals main has already caught, waits for its exit.
	omp := exec.Command("sh", "-c", command)
	omp.Dir = controllerDir
	for _, name := range slices.Sorted(maps.Keys(env)) {
		omp.Env = append(omp.Env, name+"="+env[name])
	}
	omp.Stdin, omp.Stdout, omp.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := omp.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if code := exit.ExitCode(); code > 0 {
				return code, nil
			}
			return 1, nil // killed by a signal
		}
		return 0, fmt.Errorf("run Oh My Pi: %w", err)
	}
	return 0, nil
}

// controllerEnvironment is what the controller's Oh My Pi is told on top of the operator's own
// environment: the shared controller set a tmux controller pane carries
// (packages/daemon/src/daemon/controller-environment.ts), plus LEGION_DAEMON_API=go, which picks
// the plugin's Go client, and PI_SHELL_PREFIX, which keeps this state directory's gh shim and
// legion launcher first in the agent's bash tool as on every Go pane. Secrets travel as
// `<NAME>_FILE` pointers only. Later pairs replace any inherited value of the same name.
func controllerEnvironment(cfg config.ControllerConfig, stateDir, token, secretFile string) [][2]string {
	workerBin, bin := workerbin.Dir(stateDir), workerbin.LauncherDir(stateDir)
	separator := string(filepath.ListSeparator)
	env := [][2]string{
		{"LEGION_CONTROLLER", "1"},
		{"LEGION_ROLE", "controller"},
		{"LEGION_DAEMON_API", "go"},
		{"LEGION_DAEMON_URL", cfg.DaemonURL},
		{"LEGION_PROJECT", cfg.Project},
		{"LEGION_STATE_DIR", stateDir},
		{"ENVOY_NATS_URL", strings.Join(cfg.NatsURLs, ",")},
		{"ENVOY_URL", cfg.EnvoyURL},
		// Every inherited worker-bin is dropped (the shipped pathWithoutWorkerBin), so a start
		// from inside a Legion pane never puts that pane's shim behind this one.
		{"PATH", workerBin + separator + bin + separator + workerBinFreePath(os.Getenv("PATH"))},
		{"PI_SHELL_PREFIX", shellprefix.For(workerBin, bin)},
		{"GH_CONFIG_DIR", filepath.Join(stateDir, "gh")},
		{"GH_TOKEN", ""},
		{"GITHUB_TOKEN", ""},
		{"GH_HOST", ""},
		{"LEGION_GRANT_FILE", runtime.GrantFile(stateDir, legionclaim.Token(token))},
	}
	if cfg.DispatchURL != "" {
		env = append(env, [2]string{"DISPATCH_URL", cfg.DispatchURL}, [2]string{"DISPATCH_TOKEN_FILE", cfg.DispatchTokenFile})
	}
	env = append(env, [2]string{controllerSecretVariable + "_FILE", secretFile})
	if cfg.EnvoyTokenFile != "" {
		env = append(env, [2]string{"ENVOY_TOKEN_FILE", cfg.EnvoyTokenFile})
	}
	return env
}

// probeAndMint is the controller probe and then the one daemon call, which mints the capability:
// the probe's refusal comes before the mint, never after it.
func probeAndMint(ctx context.Context, daemonURL string, probe daemon.ControllerProbe, operatorToken string) (string, error) {
	if err := daemon.ProbeController(ctx, probe); err != nil {
		return "", err
	}
	return fetchControllerSecret(ctx, daemonURL, operatorToken)
}

// makeDirs creates dir and its missing parents, 0700, and answers the ones it created, deepest
// last.
func makeDirs(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || filepath.Dir(d) == d {
			break
		}
		missing = append([]string{d}, missing...)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		removeDirs(missing)
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return missing, nil
}

// removeDirs removes the directories makeDirs created, deepest first, each only while empty.
func removeDirs(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = os.Remove(created[i])
	}
}

// fetchControllerSecret is `POST /legion/v1/controller/secret` with the operator token as a
// bearer. A failed request names the daemon URL and never tries another address; a refusal
// quotes the daemon's `error` (packages/daemon/src/cli/controller-start.ts:214-260).
func fetchControllerSecret(ctx context.Context, daemonURL, operatorToken string) (string, error) {
	const route = "/legion/v1/controller/secret"
	status, body, err := operator{base: daemonURL, bearer: operatorToken}.do(ctx, http.MethodPost, route, struct{}{})
	if err != nil {
		return "", fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if status/100 != 2 {
		hint := ""
		if status == http.StatusForbidden {
			hint = " — the operator token does not match the daemon's operator_token_file"
		}
		return "", fmt.Errorf("%s%s: %s%s", daemonURL, route, refusal(status, body), hint)
	}
	var answer api.ControllerSecretResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%s%s answered with a body that is not JSON", daemonURL, route)
	}
	if answer.Secret == "" {
		return "", fmt.Errorf("%s%s answered with no secret", daemonURL, route)
	}
	return answer.Secret, nil
}
