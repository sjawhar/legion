package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// ControllerConfigExample is the example every refusal of the operator-side file points at.
const ControllerConfigExample = "deploy/kubernetes/daemon/controller.yaml.example"

// controllerKeys is every key the operator-side file may carry: the same names as legion.yaml, only
// the ones the controller needs (packages/daemon/src/cli/controller-start.ts:34-47).
var controllerKeys = []string{
	"project", "daemon_url", "operator_token_file", "envoy_url", "envoy_token_file", "nats_urls",
	"dispatch_url", "dispatch_token_file", "instructions", "omp_invocation", "omp_launch_prefix",
	"state_dir",
}

// ControllerConfig is `legion controller start`'s operator-side file, settled. Every path is
// absolute; an optional key the file leaves out is "" (or nil).
type ControllerConfig struct {
	// Project is the daemon's project token (claim.ProjectToken of the value as written).
	Project string
	// DaemonURL is the daemon's API base, with no trailing slash: `--daemon-url` when given,
	// else the file's daemon_url.
	DaemonURL         string
	OperatorTokenFile string
	EnvoyURL          string
	EnvoyTokenFile    string
	NatsURLs          []string
	DispatchURL       string
	DispatchTokenFile string
	InstructionsPath  string
	// OmpInvocation is the invocation as configured, "" when the file sets none; the command
	// resolves it as the daemon resolves its own (omplaunch.ResolveInvocation), with no pinned
	// default in Go.
	OmpInvocation   string
	OmpLaunchPrefix []string
	// StateDir is "" when the file sets none; the command then picks the default under
	// `$XDG_STATE_HOME/legion`.
	StateDir string
}

// LoadController reads the operator-side file strictly: a YAML mapping whose every key is one of
// controllerKeys, so a typo fails on the operator's machine rather than in the controller session.
// It is never the daemon's loader, which runs both GitHub Apps' private_key_command and demands
// keys the controller never uses. Relative paths resolve against the file's directory; `~` is not
// expanded. daemonURL, when not "", is `--daemon-url`, which replaces the file's daemon_url and is
// validated the same way (packages/daemon/src/cli/controller-start.ts:75-175, 291-300).
func LoadController(path, daemonURL string) (ControllerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ControllerConfig{}, fmt.Errorf("controller configuration %s could not be read: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return ControllerConfig{}, fmt.Errorf("Invalid controller configuration: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return ControllerConfig{}, errors.New("controller.yaml must be a mapping")
	}
	root := document.Content[0]
	values := map[string]*yaml.Node{}
	var unknown []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if !slices.Contains(controllerKeys, key) {
			unknown = append(unknown, fmt.Sprintf("unknown key %q in the controller configuration; legion controller start reads only %s — see %s",
				key, strings.Join(controllerKeys, ", "), ControllerConfigExample))
			continue
		}
		values[key] = root.Content[i+1]
	}
	if len(unknown) > 0 {
		return ControllerConfig{}, errors.New(strings.Join(unknown, "; "))
	}

	configDir := filepath.Dir(path)
	const inController = " in the controller configuration"
	required := func(key string) (string, error) { return requiredString(values[key], key, inController) }
	optionalPath := func(key string) (string, error) {
		value, err := optionalString(values[key], key)
		if err != nil || value == "" {
			return "", err
		}
		return underConfig(value, configDir), nil
	}

	var cfg ControllerConfig
	project, err := required("project")
	if err != nil {
		return ControllerConfig{}, err
	}
	if cfg.Project, err = claim.ProjectToken(project); err != nil {
		return ControllerConfig{}, err
	}
	fileDaemonURL, err := required("daemon_url")
	if err != nil {
		return ControllerConfig{}, err
	}
	if cfg.DaemonURL, err = daemonBase(fileDaemonURL, "daemon_url"); err != nil {
		return ControllerConfig{}, err
	}
	operatorTokenFile, err := required("operator_token_file")
	if err != nil {
		return ControllerConfig{}, err
	}
	cfg.OperatorTokenFile = underConfig(operatorTokenFile, configDir)
	if cfg.EnvoyURL, err = required("envoy_url"); err != nil {
		return ControllerConfig{}, err
	}
	if _, err := validURL(cfg.EnvoyURL, "envoy_url"); err != nil {
		return ControllerConfig{}, err
	}
	if node, set := values["nats_urls"]; set {
		if cfg.NatsURLs, err = readNatsURLs(node, "nats_urls"); err != nil {
			return ControllerConfig{}, err
		}
	}
	if len(cfg.NatsURLs) == 0 {
		return ControllerConfig{}, errors.New("nats_urls is required in the controller configuration")
	}
	if cfg.EnvoyTokenFile, err = optionalPath("envoy_token_file"); err != nil {
		return ControllerConfig{}, err
	}
	dispatchURL, err := optionalString(values["dispatch_url"], "dispatch_url")
	if err != nil {
		return ControllerConfig{}, err
	}
	if cfg.DispatchTokenFile, err = optionalPath("dispatch_token_file"); err != nil {
		return ControllerConfig{}, err
	}
	switch {
	case dispatchURL != "" && cfg.DispatchTokenFile == "":
		return ControllerConfig{}, errors.New(missingDispatchTokenFile)
	case dispatchURL == "" && cfg.DispatchTokenFile != "":
		return ControllerConfig{}, errors.New("dispatch_token_file is set but dispatch_url is not")
	case dispatchURL != "":
		if cfg.DispatchURL, err = dispatchBase(dispatchURL, "dispatch_url"); err != nil {
			return ControllerConfig{}, err
		}
	}
	if cfg.InstructionsPath, err = optionalPath("instructions"); err != nil {
		return ControllerConfig{}, err
	}
	if cfg.OmpInvocation, err = optionalString(values["omp_invocation"], "omp_invocation"); err != nil {
		return ControllerConfig{}, err
	}
	if node, set := values["omp_launch_prefix"]; set {
		// The controller runs under the operator's own environment, not a pane's isolated XDG
		// home, so a `secrets` prefix is theirs to use (readLaunchPrefix refuses it for panes).
		if cfg.OmpLaunchPrefix, err = readStrings(node, "omp_launch_prefix"); err != nil {
			return ControllerConfig{}, err
		}
	}
	if cfg.StateDir, err = optionalPath("state_dir"); err != nil {
		return ControllerConfig{}, err
	}
	if daemonURL != "" {
		if cfg.DaemonURL, err = daemonBase(daemonURL, "--daemon-url"); err != nil {
			return ControllerConfig{}, err
		}
	}
	return cfg, nil
}

// daemonBase is a daemon URL the command appends a route to: valid, without trailing slashes.
func daemonBase(value, key string) (string, error) {
	if _, err := validURL(value, key); err != nil {
		return "", err
	}
	return strings.TrimRight(value, "/"), nil
}
