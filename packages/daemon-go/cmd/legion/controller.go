package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

const controllerUsage = "usage: legion controller start --config <controller.yaml> [--daemon-url <url>]"

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
// packages/daemon/src/cli/controller-start.ts:262-384). In order, and nothing is written or
// launched until the daemon has answered: read the strict operator-side file; refuse an operator
// token file others can read, and a blank or unreadable Envoy or Dispatch token file, a role-prompt
// bundle missing a file, an instructions file that is missing or blank, an Oh My Pi invocation
// that does not resolve, and a pi-legion-envoy manifest, at the plugin root this process's
// environment names, that does not speak this binary's Go daemon API contract (it would refuse the
// controller at session start; a dotenv file Oh My Pi reads itself or the launch prefix can move
// that root, which this check does not see: daemon.VerifyPluginContract);
// fetch the controller secret with the operator token as a bearer (the daemon mints a fresh
// capability and revokes the previous controller's); write it 0600 under the local state
// directory beside the gh shim, the `legion` launcher, and the deployment instructions;
// then run Oh My Pi interactive — the launch prefix and the resolved invocation, one joined
// `--append-system-prompt`, no `--resume`, no `--mode rpc` — in the foreground with the shared
// controller environment, and answer its exit code.
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
	// Everything up to the fetch reads and writes nothing under the state directory: the daemon
	// mints a fresh capability on every request and revokes the incumbent controller's, so a
	// failure this machine can find on its own is found first — never after the running
	// controller has been cut off for nothing.
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
	if err := prompts.CheckRolePrompts(rolesDir); err != nil {
		return 0, err
	}
	if cfg.InstructionsPath != "" {
		if _, err := config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return 0, err
		}
	}
	invocation, err := tmux.ResolveOmpInvocation(cfg.OmpInvocation, os.Getenv)
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
	if _, err := daemon.VerifyPluginContract(processEnvironment(), controllerDir, api.GoDaemonAPIVersion); err != nil {
		return 0, err
	}

	secret, err := fetchControllerSecret(ctx, cfg.DaemonURL, operatorToken)
	if err != nil {
		return 0, err
	}

	token := string(legionclaim.ControllerToken(cfg.Project))
	secretFile, err := tmux.WriteSecretFile(stateDir, token, secret)
	if err != nil {
		return 0, fmt.Errorf("write the controller secret: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve the legion executable the controller's launcher runs: %w", err)
	}
	if err := tmux.InstallWorkerBin(stateDir, executable); err != nil {
		return 0, err
	}
	instructionsFile := ""
	if cfg.InstructionsPath != "" {
		if instructionsFile, err = config.MaterializeDeploymentInstructions(cfg.InstructionsPath, stateDir, cfg.Project); err != nil {
			return 0, err
		}
	}
	if err := os.MkdirAll(controllerDir, 0o700); err != nil {
		return 0, fmt.Errorf("create %s: %w", controllerDir, err)
	}

	env := os.Environ()
	for _, pair := range controllerEnvironment(cfg, stateDir, token, secretFile) {
		env = append(env, pair[0]+"="+pair[1])
	}
	command := tmux.WithOmpLaunchPrefix(cfg.OmpLaunchPrefix, invocation) + " " + tmux.SystemPromptArgument(runtime.PromptParts{
		RolePromptPaths:            []string{filepath.Join(rolesDir, "controller-root.md")},
		DeploymentInstructionsPath: instructionsFile,
	})
	fmt.Fprintf(stderr, "[legion] starting the controller for %s against %s; state in %s\n", cfg.Project, cfg.DaemonURL, stateDir)
	// Interactive and in the foreground: the operator's terminal is Oh My Pi's. The child is not
	// bound to ctx — a Ctrl-C reaches Oh My Pi through the terminal's process group and is its to
	// handle, while this process, whose signals main has already caught, waits for its exit.
	omp := exec.Command("sh", "-c", command)
	omp.Dir, omp.Env = controllerDir, env
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
	workerBin, bin := tmux.WorkerBinDir(stateDir), tmux.LegionBinDir(stateDir)
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
		{"LEGION_GRANT_FILE", filepath.Join(stateDir, "secrets", token+"-grant")},
	}
	if cfg.DispatchURL != "" {
		env = append(env, [2]string{"DISPATCH_URL", cfg.DispatchURL}, [2]string{"DISPATCH_TOKEN_FILE", cfg.DispatchTokenFile})
	}
	env = append(env, [2]string{"LEGION_CONTROLLER_SECRET_FILE", secretFile})
	if cfg.EnvoyTokenFile != "" {
		env = append(env, [2]string{"ENVOY_TOKEN_FILE", cfg.EnvoyTokenFile})
	}
	return env
}

// fetchControllerSecret is `POST /legion/v1/controller/secret` with the operator token as a
// bearer. A failed request names the daemon URL and never tries another address; a refusal
// quotes the daemon's `error` (packages/daemon/src/cli/controller-start.ts:214-260).
func fetchControllerSecret(ctx context.Context, daemonURL, operatorToken string) (string, error) {
	url := daemonURL + "/legion/v1/controller/secret"
	status, body, err := operator{base: daemonURL, bearer: operatorToken}.do(ctx, http.MethodPost, "/legion/v1/controller/secret", struct{}{})
	if err != nil {
		return "", fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if status/100 != 2 {
		detail := strings.TrimSpace(string(body))
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &refusal) == nil && refusal.Error != "" {
			detail = refusal.Error
		}
		hint := ""
		if status == http.StatusForbidden {
			hint = " — the operator token does not match the daemon's operator_token_file"
		}
		return "", fmt.Errorf("%s answered %d: %s%s", url, status, detail, hint)
	}
	var answer api.ControllerSecretResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%s answered with a body that is not JSON", url)
	}
	if answer.Secret == "" {
		return "", fmt.Errorf("%s answered with no secret", url)
	}
	return answer.Secret, nil
}
