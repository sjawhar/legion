// Package config loads the daemon's `legion.yaml` and its `LEGION_*` environment.
//
// The file keeps the key names the shipped TypeScript daemon reads, so the deployment overlays
// (deploy/kubernetes/daemon/{base,overlays/kind}/legion.yaml) load at every stage of the Go
// rewrite. Stage 1 models seven of those keys; the rest fall into three classes the loader
// decides once, here: known-later keys a later stage models, accepted and ignored with one log
// line each; tossed keys, refused naming the key and why the setting no longer exists; and
// migration-only keys, refused with the TypeScript loader's own message text so an operator
// searching for those words finds the same answer. Anything else is a typo.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Runtime is the `runtime` block read as its discriminator. Stage 1 needs the name alone; every
// member of `runtime.kubernetes` is accepted and ignored until Stage 4 models the Sandbox
// runtime.
type Runtime struct {
	Name string
}

// Config is the daemon's settled configuration: the file, the environment, and the defaults
// resolved into the values Stage 1 runs on.
type Config struct {
	Project      string
	Port         int
	Bind         string
	PostgresDSN  string
	StateDir     string
	Runtime      Runtime
	AdmissionCap int
}

const (
	defaultPort         = 13370
	defaultBind         = "127.0.0.1"
	defaultAdmissionCap = 4
	defaultRuntimeName  = "tmux"
)

// knownLaterKeys is every top-level key the shipped loader accepts, that Stage 1 does not model
// and a later stage does, mapped to that stage. A file carrying one loads; the key is logged and
// dropped. The stages are the plan's: 2 supervision on tmux, 3 the workflow on the devbox,
// 4 the Sandbox runtime.
var knownLaterKeys = map[string]int{
	"daemon_url":                  2,
	"instructions":                2,
	"omp_invocation":              2,
	"omp_launch_prefix":           2,
	"worker_stream_port":          2,
	"worker_boot_timeout_seconds": 2,
	"worker_boot_registration_deadline_intervals": 2,
	"worker_rpc_timeout_seconds":                  2,
	"worker_stop_timeout_seconds":                 2,
	"tree_stop_timeout_seconds":                   2,
	"slow_command_timeout_seconds":                2,
	"envoy_url":                                   3,
	"envoy_token_file":                            3,
	"nats_urls":                                   3,
	"dispatch_url":                                3,
	"projects":                                    3,
	"gates":                                       3,
	"github_apps":                                 3,
	"max_recursion_depth":                         3,
	"linger_hours":                                3,
	"max_fix_attempts":                            3,
	"operator_token_file":                         4,
}

// tossedKeys are the settings the rewrite removed, mapped to why. A file carrying one is refused
// rather than silently ignored: each was load-bearing for the TypeScript daemon, so leaving it in
// place would misstate what the daemon does.
var tossedKeys = map[string]string{
	"worker_cap":                 "the running-worker cap no longer exists (LEGION-208 Requirement 8)",
	"worker_idle_retire_seconds": `a worker is suspended when its phase ends, never after an idle window (LEGION-208 Design, "Process supervision")`,
	"resync_interval_seconds":    `the mirror of Dispatch and GitHub as truth, and resync's drift healing, no longer exist (LEGION-208 Design, "Ported, and tossed")`,
}

// migrationKeys are the keys the TypeScript loader already refuses with a migration message,
// mapped to that message verbatim (packages/daemon/src/daemon/config.ts:1316, 1320, 1327, 1330,
// 1350, 1354). The text is kept exactly, including `worker_budget`'s pointer at `worker_cap`,
// which this loader tosses in turn: an operator who hits the message searches for the same words
// in either daemon, and the second refusal names the cap's own fate.
var migrationKeys = map[string]string{
	"dispatch_mcp_url":  "dispatch_mcp_url was replaced by dispatch_url (the service base URL, no /mcp)",
	"dispatch_project":  "dispatch_project was replaced by projects",
	"board_project_ids": "board_project_ids was replaced by projects",
	"repos":             "repos was replaced by projects",
	"app_logins":        "app_logins is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes",
	"worker_budget":     "worker_budget was replaced by worker_cap",
}

// gatesMergeMessage is `parseGates`'s own refusal (config.ts:981).
const gatesMergeMessage = "gates.merge is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"

// fileConfig holds the modelled keys as they were read: a pointer per key, so a key the file
// leaves out is distinguishable from one it sets to a zero value.
type fileConfig struct {
	Project      *string
	Port         *int
	Bind         *string
	PostgresDSN  *string
	StateDir     *string
	AdmissionCap *int
	Runtime      *string
}

// Load reads the file at path and resolves it against env — the environment lookup, injected so a
// test can supply one; nil reads the process environment. The first refusal wins, in the file's
// own key order, so an operator fixing a file works down it.
func Load(path string, env func(string) string) (Config, error) {
	if env == nil {
		env = os.Getenv
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	root, err := rootMapping(&document)
	if err != nil {
		return Config{}, err
	}
	file, err := readKeys(root)
	if err != nil {
		return Config{}, err
	}
	return resolve(file, env, filepath.Dir(path))
}

// rootMapping returns the document's root mapping, or nil for an empty file.
func rootMapping(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind == 0 || len(document.Content) == 0 {
		return nil, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config file root must be a mapping")
	}
	return root, nil
}

// readKeys walks the top-level keys in file order, reading the modelled ones and classifying the
// rest.
func readKeys(root *yaml.Node) (fileConfig, error) {
	var file fileConfig
	if root == nil {
		return file, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i].Value, root.Content[i+1]
		var err error
		switch key {
		case "project":
			file.Project, err = readString(value, key)
		case "bind":
			file.Bind, err = readString(value, key)
		case "postgres_dsn":
			file.PostgresDSN, err = readString(value, key)
		case "state_dir":
			file.StateDir, err = readString(value, key)
		case "port":
			file.Port, err = readInt(value, key)
		case "admission_cap":
			file.AdmissionCap, err = readInt(value, key)
		case "runtime":
			file.Runtime, err = readRuntime(value)
		default:
			err = classify(key, value)
		}
		if err != nil {
			return fileConfig{}, err
		}
	}
	return file, nil
}

// classify decides an unmodelled key's class, refusing or logging it.
func classify(key string, value *yaml.Node) error {
	if reason, ok := tossedKeys[key]; ok {
		return fmt.Errorf("unknown key %s: %s", key, reason)
	}
	if message, ok := migrationKeys[key]; ok {
		return errors.New(message)
	}
	stage, ok := knownLaterKeys[key]
	if !ok {
		return fmt.Errorf("unknown key %s", key)
	}
	// `gates` is the one known-later block walked a level, as the shipped loader walks it
	// (config.ts:396-399): `merge` is a refusal, not a key a later stage models.
	if key == "gates" {
		if err := checkGates(value); err != nil {
			return err
		}
	}
	logIgnored(key, stage)
	return nil
}

// checkGates walks `gates` one level: `merge` carries the shipped refusal, `design` is accepted
// and ignored under the `gates` line until Stage 3 models the design gate.
func checkGates(value *yaml.Node) error {
	if value.Tag == "!!null" {
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return errors.New("gates must be a mapping")
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch key := value.Content[i].Value; key {
		case "merge":
			return errors.New(gatesMergeMessage)
		case "design":
		default:
			return fmt.Errorf("unknown key gates.%s", key)
		}
	}
	return nil
}

// readRuntime reads `runtime` as its discriminator: the `tmux` or `kubernetes` scalar, or the
// shipped one-key `{kubernetes: {...}}` mapping whose members Stage 4 models and Stage 1 ignores
// whole — nothing under it is walked.
func readRuntime(value *yaml.Node) (*string, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind == yaml.ScalarNode {
		name := value.Value
		if name != "tmux" && name != "kubernetes" {
			return nil, errors.New("runtime must be 'tmux' or 'kubernetes'")
		}
		return &name, nil
	}
	if value.Kind != yaml.MappingNode ||
		len(value.Content) != 2 ||
		value.Content[0].Value != "kubernetes" {
		return nil, errors.New("runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes")
	}
	logIgnored("runtime.kubernetes", 4)
	name := "kubernetes"
	return &name, nil
}

func logIgnored(key string, stage int) {
	slog.Info("legion.yaml key accepted and ignored", "key", key, "stage", stage)
}

func readString(value *yaml.Node, key string) (*string, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	var read string
	if value.Kind != yaml.ScalarNode || value.Decode(&read) != nil {
		return nil, fmt.Errorf("%s must be a string", key)
	}
	return &read, nil
}

func readInt(value *yaml.Node, key string) (*int, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	var read int
	if value.Tag != "!!int" || value.Decode(&read) != nil {
		return nil, fmt.Errorf("%s must be an integer", key)
	}
	return &read, nil
}

// resolve applies the environment and the defaults, and validates what the daemon cannot start
// without. Where a key has both a file and an environment source, the file wins, as it does in
// the shipped loader. configDir is the directory of the file the keys came from: a relative
// path in it is relative to the file, never to the cwd a command was launched from.
func resolve(file fileConfig, env func(string) string, configDir string) (Config, error) {
	cfg := Config{
		Port:         defaultPort,
		Bind:         defaultBind,
		Runtime:      Runtime{Name: defaultRuntimeName},
		AdmissionCap: defaultAdmissionCap,
	}

	if file.Project == nil || strings.TrimSpace(*file.Project) == "" {
		return Config{}, errors.New("project is required")
	}
	cfg.Project = *file.Project

	if file.StateDir == nil || strings.TrimSpace(*file.StateDir) == "" {
		return Config{}, errors.New("state_dir is required")
	}
	cfg.StateDir = *file.StateDir
	if !filepath.IsAbs(cfg.StateDir) {
		cfg.StateDir = filepath.Join(configDir, cfg.StateDir)
	}

	cfg.PostgresDSN = env("LEGION_POSTGRES_DSN")
	if file.PostgresDSN != nil {
		cfg.PostgresDSN = *file.PostgresDSN
	}
	if strings.TrimSpace(cfg.PostgresDSN) == "" {
		return Config{}, errors.New("postgres_dsn is required (or set LEGION_POSTGRES_DSN)")
	}

	if file.Port != nil {
		cfg.Port = *file.Port
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, errors.New("port must be a valid TCP port (1-65535)")
	}

	if file.Bind != nil {
		cfg.Bind = *file.Bind
	}
	if strings.TrimSpace(cfg.Bind) == "" {
		return Config{}, errors.New("bind must not be empty")
	}

	if file.Runtime != nil {
		cfg.Runtime = Runtime{Name: *file.Runtime}
	}

	switch {
	case file.AdmissionCap != nil:
		cfg.AdmissionCap = *file.AdmissionCap
	case env("LEGION_ADMISSION_CAP") != "":
		parsed, err := strconv.Atoi(env("LEGION_ADMISSION_CAP"))
		if err != nil || parsed < 1 {
			return Config{}, errors.New("LEGION_ADMISSION_CAP must be a positive integer")
		}
		cfg.AdmissionCap = parsed
	}
	if cfg.AdmissionCap < 1 {
		return Config{}, errors.New("admission_cap must be a positive integer")
	}

	return cfg, nil
}
