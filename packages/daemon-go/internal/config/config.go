// Package config loads the daemon's `legion.yaml` and its `LEGION_*` environment.
//
// The file keeps the key names the shipped TypeScript daemon reads, so the deployment overlays
// (deploy/kubernetes/daemon/{base,overlays/kind}/legion.yaml) load at every stage of the Go
// rewrite. Stages 1 and 2 model twenty-one of those keys plus five of the rewrite's own; the rest
// fall into three classes the loader decides once, here: known-later keys a later stage models,
// accepted and ignored with one log line each; tossed keys, refused naming the key and why the
// setting no longer exists; and migration-only keys, refused with the TypeScript loader's own
// message text so an operator searching for those words finds the same answer. Anything else is a
// typo.
//
// Load reads the file and nothing it names: a path key (`instructions`, `envoy_token_file`,
// `operator_token_file`) is resolved against the file's directory and kept as a path. What sits at
// that path is read at boot, by ReadSecretPointer and MaterializeDeploymentInstructions, so the
// overlays load on a machine that has none of the files they mount.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Runtime is the `runtime` block read as its discriminator. Stage 1 needs the name alone; every
// member of `runtime.kubernetes` is accepted and ignored until Stage 4 models the Sandbox
// runtime.
type Runtime struct {
	Name string
}

// Config is the daemon's settled configuration: the file, the environment, and the defaults
// resolved into the values the daemon runs on.
type Config struct {
	Project      string
	Port         int
	Bind         string
	PostgresDSN  string
	StateDir     string
	Runtime      Runtime
	AdmissionCap int

	// DaemonURL is the API address every pane is told (`LEGION_DAEMON_URL`), with no trailing
	// slash; the loopback address on Port unless the file names another.
	DaemonURL string
	// OmpInvocation is the invocation as configured, "" when the file sets none. The OMP launch
	// fragment a pane runs is resolved from it at boot (`LEGION_OMP_PATH`, or `mise x <tool> --
	// omp` with mise's absolute path), because resolving it reads the machine, not the file. There
	// is no Go default: the shipped one is the OMP fork pin, and the pin has one home,
	// packages/daemon/src/daemon/omp-pin.ts — a second copy here would be a second literal to bump.
	OmpInvocation string
	// OmpLaunchPrefix is argv prepended to every OMP invocation inside a pane. It may not run
	// `secrets`: a pane's XDG home is the daemon's isolated one, where the secrets client finds
	// neither its config nor sops's age identity (ProviderKeys is how a provider key reaches OMP).
	OmpLaunchPrefix []string
	// ProviderKeys are the provider credentials every pane's OMP receives in its environment, in
	// file order: the variable OMP reads, and the secretsd key that holds it. Boot resolves each
	// once, as the daemon (MaterializeProviderKeys), into a daemon-held 0600 file; a pane never
	// holds the secret store itself.
	ProviderKeys []ProviderKey
	// InstructionsPath is the operator's deployment instructions, "" when none; boot copies it to
	// `<state_dir>/deployment-instructions.md` (MaterializeDeploymentInstructions).
	InstructionsPath string
	// WorkerStreamPort is the worker stream listener's TCP port under a runtime that dials it by
	// TCP; Port + 1 unless the file names another.
	WorkerStreamPort int

	WorkerBootTimeout time.Duration
	// WorkerBootRegistrationDeadlineIntervals × WorkerBootTimeout is the registration deadline: a
	// pane whose process is alive but whose agent has not registered by then is retired and
	// counted as a launch failure.
	WorkerBootRegistrationDeadlineIntervals int
	WorkerRPCTimeout                        time.Duration
	WorkerStopTimeout                       time.Duration
	TreeStopTimeout                         time.Duration
	SlowCommandTimeout                      time.Duration
	// ProbeInterval is how often the runtime sweeps every recorded process.
	ProbeInterval time.Duration

	// The supervision budgets: launches that failed, prompts acknowledged without a turn, and
	// panes retired for those, each before the claim is failed.
	LaunchFailureLimit int
	PromptFailureLimit int
	PromptRetireLimit  int

	// OperatorTokenFile is the bearer the operator routes compare against, as a path; "" when
	// the file sets none.
	OperatorTokenFile string
	EnvoyURL          string
	NatsURLs          []string
	// EnvoyTokenFile is the Envoy bearer's file, "" when none; every pane receives the token as
	// a 0600 file of its own.
	EnvoyTokenFile string

	// Stage 3's workflow dependencies. DispatchTokenFile remains a pointer here: boot reads the
	// bearer only after every local configuration refusal has passed.
	DispatchURL       string
	DispatchTokenFile string
	Projects          map[string]Project
	Gates             Gates
	GitHubApps        GitHubApps
	// Linger is how long a finished tree keeps its workspace before it closes (`linger_hours`).
	Linger         time.Duration
	ReviewRoundCap int
	MaxFixAttempts int
}

const (
	defaultPort         = 13370
	defaultBind         = "127.0.0.1"
	defaultAdmissionCap = 4
	defaultRuntimeName  = "tmux"
	defaultEnvoyURL     = "http://127.0.0.1:9020"

	// maxTimerSeconds is the shipped bound on every duration key (config.ts:303-308): the
	// largest whole number of seconds whose milliseconds fit a signed 32-bit timer. Go's timers
	// are not so bounded; the bound is kept so a file one daemon accepts, the other accepts too.
	maxTimerSeconds = 2_147_483
)

// durationKeys are the positive-second keys and their defaults (config.ts:297-312, and the new
// probe interval). Every one of them is held to the timer bound.
var durationKeys = []struct {
	key          string
	defaultValue int
	field        func(*Config) *time.Duration
}{
	{"worker_boot_timeout_seconds", 120, func(c *Config) *time.Duration { return &c.WorkerBootTimeout }},
	{"worker_rpc_timeout_seconds", 5, func(c *Config) *time.Duration { return &c.WorkerRPCTimeout }},
	{"worker_stop_timeout_seconds", 10, func(c *Config) *time.Duration { return &c.WorkerStopTimeout }},
	{"tree_stop_timeout_seconds", 60, func(c *Config) *time.Duration { return &c.TreeStopTimeout }},
	{"slow_command_timeout_seconds", 300, func(c *Config) *time.Duration { return &c.SlowCommandTimeout }},
	{"probe_interval_seconds", 30, func(c *Config) *time.Duration { return &c.ProbeInterval }},
}

// countKeys are the positive-integer keys with no unit, and their defaults: the registration
// deadline's interval count (config.ts:300) and the supervision budgets.
var countKeys = []struct {
	key          string
	defaultValue int
	field        func(*Config) *int
}{
	{"worker_boot_registration_deadline_intervals", 3, func(c *Config) *int { return &c.WorkerBootRegistrationDeadlineIntervals }},
	{"launch_failure_limit", 3, func(c *Config) *int { return &c.LaunchFailureLimit }},
	{"prompt_failure_limit", 3, func(c *Config) *int { return &c.PromptFailureLimit }},
	{"prompt_retire_limit", 2, func(c *Config) *int { return &c.PromptRetireLimit }},
}

// knownLaterKeys is every top-level key the shipped loader accepts, that no stage so far models
// and a later stage does, mapped to that stage. A file carrying one loads; the key is logged and
// dropped. The stages are the plan's: 3 the workflow on the devbox, 4 the Sandbox runtime.
var knownLaterKeys = map[string]int{
	"max_recursion_depth": 3,
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
	Project           *string
	Port              *int
	Bind              *string
	PostgresDSN       *string
	StateDir          *string
	AdmissionCap      *int
	Runtime           *string
	DaemonURL         *string
	OmpInvocation     *string
	OmpLaunchPrefix   []string
	ProviderKeys      []ProviderKey
	Instructions      *string
	WorkerStreamPort  *int
	OperatorTokenFile *string
	EnvoyURL          *string
	NatsURLs          []string
	EnvoyTokenFile    *string
	DispatchURL       *string
	DispatchTokenFile *string
	Projects          map[string]Project
	Gates             *Gates
	GitHubApps        *GitHubApps
	Linger            *time.Duration
	ReviewRoundCap    *int
	MaxFixAttempts    *int
	Durations         map[string]int
	Counts            map[string]int
}

// Load reads the daemon's complete runtime configuration. App private-key commands and secrets
// resolve only after the YAML's non-secret validation has passed.
func Load(path string, env func(string) string) (Config, error) {
	return load(path, env, true)
}

// LoadForValidation checks a configuration without executing its App private-key commands or
// secrets reads. It is the Go equivalent of the shipped daemon's --check-config path.
func LoadForValidation(path string, env func(string) string) (Config, error) {
	return load(path, env, false)
}

func load(path string, env func(string) string, resolveAppKeys bool) (Config, error) {
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
	cfg, err := resolve(file, env, filepath.Dir(path))
	if err != nil || !resolveAppKeys || file.GitHubApps == nil {
		return cfg, err
	}
	apps, err := ResolveGitHubApps(cfg.GitHubApps)
	if err != nil {
		return Config{}, err
	}
	cfg.GitHubApps = apps
	return cfg, nil
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
	file := fileConfig{Durations: map[string]int{}, Counts: map[string]int{}}
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
		case "daemon_url":
			file.DaemonURL, err = readString(value, key)
		case "omp_invocation":
			file.OmpInvocation, err = readNonEmptyString(value, key)
		case "omp_launch_prefix":
			file.OmpLaunchPrefix, err = readLaunchPrefix(value, key)
		case "provider_keys":
			file.ProviderKeys, err = readProviderKeys(value, key)
		case "instructions":
			file.Instructions, err = readNonEmptyString(value, key)
		case "worker_stream_port":
			file.WorkerStreamPort, err = readInt(value, key)
		case "operator_token_file":
			file.OperatorTokenFile, err = readNonEmptyString(value, key)
		case "envoy_url":
			file.EnvoyURL, err = readString(value, key)
		case "nats_urls":
			file.NatsURLs, err = readStrings(value, key)
		case "envoy_token_file":
			file.EnvoyTokenFile, err = readNonEmptyString(value, key)
		case "dispatch_url":
			file.DispatchURL, err = readString(value, key)
		case "dispatch_token_file":
			file.DispatchTokenFile, err = readNonEmptyString(value, key)
		case "projects":
			file.Projects, err = readProjects(value, key)
		case "gates":
			file.Gates, err = readGates(value, key)
		case "github_apps":
			file.GitHubApps, err = readGitHubApps(value, key)
		case "linger_hours":
			file.Linger, err = readLingerHours(value, key)
		case "review_round_cap":
			file.ReviewRoundCap, err = readPositiveInteger(value, key, 0)
		case "max_fix_attempts":
			file.MaxFixAttempts, err = readPositiveInteger(value, key, 0)
		default:
			if isDurationKey(key) || isCountKey(key) {
				err = readPositive(value, key, file)
			} else {
				err = classify(key, value)
			}
		}
		if err != nil {
			return fileConfig{}, err
		}
	}
	return file, nil
}

func isDurationKey(key string) bool {
	for _, d := range durationKeys {
		if d.key == key {
			return true
		}
	}
	return false
}

func isCountKey(key string) bool {
	for _, c := range countKeys {
		if c.key == key {
			return true
		}
	}
	return false
}

// readPositive reads a duration or count key into the file's map: a positive integer and, for a
// duration, at most the timer bound (config.ts:480-487, 1367-1382).
func readPositive(value *yaml.Node, key string, file fileConfig) error {
	read, err := readInt(value, key)
	if err != nil || read == nil {
		return err
	}
	if *read <= 0 {
		return fmt.Errorf("%s must be a positive integer", key)
	}
	if isDurationKey(key) {
		if *read > maxTimerSeconds {
			return fmt.Errorf("%s must be at most %d", key, maxTimerSeconds)
		}
		file.Durations[key] = *read
	} else {
		file.Counts[key] = *read
	}
	return nil
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

// readNonEmptyString is a string key whose blank value is a refusal rather than an unset key
// (the shipped `requireNonEmpty`, config.ts:614-617).
func readNonEmptyString(value *yaml.Node, key string) (*string, error) {
	read, err := readString(value, key)
	if err != nil || read == nil {
		return read, err
	}
	if strings.TrimSpace(*read) == "" {
		return nil, fmt.Errorf("%s must not be empty", key)
	}
	return read, nil
}

// readStrings reads a sequence of non-empty strings, in order and with repeats — the shipped
// `readArgv` (config.ts:452-461); a caller that wants a set dedupes it.
func readStrings(value *yaml.Node, key string) ([]string, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	refusal := fmt.Errorf("%s must be an array of non-empty strings", key)
	if value.Kind != yaml.SequenceNode {
		return nil, refusal
	}
	read := make([]string, 0, len(value.Content))
	for _, entry := range value.Content {
		var s string
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" || entry.Decode(&s) != nil || s == "" {
			return nil, refusal
		}
		read = append(read, s)
	}
	return read, nil
}

// readLaunchPrefix is `omp_launch_prefix`, argv, refused when it runs `secrets`. A pane's XDG home
// is the daemon's isolated one: the secrets client finds no secretsd config there, and sops no age
// identity — and handing a pane that identity would hand every pane every agent-tier secret on the
// box. A provider key is named in provider_keys instead, and reaches OMP as a daemon-held file.
func readLaunchPrefix(value *yaml.Node, key string) ([]string, error) {
	prefix, err := readStrings(value, key)
	if err != nil || len(prefix) == 0 {
		return prefix, err
	}
	if filepath.Base(prefix[0]) == "secrets" {
		return nil, errors.New(`omp_launch_prefix runs "secrets", which cannot decrypt inside a Go pane (its XDG home is isolated); name the keys in provider_keys instead`)
	}
	return prefix, nil
}

// envVarName is a name a shell accepts as a variable: what a provider key becomes in OMP's
// environment, and a single argv word for `secrets get`, never a command line.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProviderKey is one `provider_keys` entry: Env, the variable Oh My Pi reads (GEMINI_API_KEY), and
// Secret, the secretsd key that holds its value in this deployment (GEMINI_API_KEY_TESTS) — named
// apart because a deployment names its secrets per environment while the provider's variable is
// fixed.
type ProviderKey struct {
	Env    string
	Secret string
}

// providerKeysShape is the refusal for anything but the mapping.
const providerKeysShape = "provider_keys must be a mapping of the variable OMP reads to the secretsd key that holds it, e.g. {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}"

// readProviderKeys is `provider_keys`: a mapping of the variable OMP reads to its secretsd key
// name, both sides environment variable names, in file order. A variable named twice is refused
// rather than one entry silently winning.
func readProviderKeys(value *yaml.Node, key string) ([]ProviderKey, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, errors.New(providerKeysShape)
	}
	const nameRule = "must be an environment variable name (letters, digits, and underscores, not starting with a digit)"
	var keys []ProviderKey
	seen := map[string]bool{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		envNode, secretNode := value.Content[i], value.Content[i+1]
		env := envNode.Value
		if envNode.Kind != yaml.ScalarNode || !envVarName.MatchString(env) {
			return nil, fmt.Errorf("%s key %q (the variable OMP reads) %s", key, env, nameRule)
		}
		if seen[env] {
			return nil, fmt.Errorf("%s names %s twice", key, env)
		}
		seen[env] = true
		var secret string
		if secretNode.Kind != yaml.ScalarNode || secretNode.Decode(&secret) != nil {
			return nil, fmt.Errorf("%s value for %s (the secretsd key name) must be a string", key, env)
		}
		if !envVarName.MatchString(secret) {
			return nil, fmt.Errorf("%s value %q for %s (the secretsd key name) %s", key, secret, env, nameRule)
		}
		keys = append(keys, ProviderKey{Env: env, Secret: secret})
	}
	return keys, nil
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

// readLingerHours is `linger_hours`: a positive number of hours, a decimal among them (0.3 is 18
// minutes), bounded as the shipped key is, by the timer bound in whole hours (config.ts
// MAX_TIMER_HOURS). A value too small to be a nanosecond is refused, never read as zero, which the
// engine would take for its 72-hour default.
func readLingerHours(value *yaml.Node, key string) (*time.Duration, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	var hours float64
	if (value.Tag != "!!int" && value.Tag != "!!float") || value.Decode(&hours) != nil {
		return nil, fmt.Errorf("%s must be a number", key)
	}
	if !(hours > 0) {
		return nil, fmt.Errorf("%s must be a positive number", key)
	}
	if hours > maxTimerSeconds/3600 {
		return nil, fmt.Errorf("%s must be at most %d", key, maxTimerSeconds/3600)
	}
	linger := time.Duration(math.Round(hours * float64(time.Hour)))
	if linger <= 0 {
		return nil, fmt.Errorf("%s must be a positive number", key)
	}
	return &linger, nil
}

// validURL is the shipped `validateUrl` (config.ts:627-634) for the URLs this daemon dials or
// hands out: a scheme and a host, or the key is refused.
func validURL(value, key string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be a valid URL", key)
	}
	return parsed, nil
}

// baseURL is the shipped `normalizeBaseUrl` (config.ts:768-774): a URL a path is appended to, so
// a query or fragment is refused and trailing slashes are dropped.
func baseURL(value, key string) (string, error) {
	parsed, err := validURL(value, key)
	if err != nil {
		return "", err
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not include a query string or fragment", key)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// underConfig resolves a path key against the directory of the file that set it.
func underConfig(path, configDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(configDir, path)
}

// resolve applies the environment and the defaults, and validates what the daemon cannot start
// without. Where a key has both a file and an environment source, the file wins, as it does in
// the shipped loader. configDir is the directory of the file the keys came from: a relative
// path in it is relative to the file, never to the cwd a command was launched from.
func resolve(file fileConfig, env func(string) string, configDir string) (Config, error) {
	cfg := Config{
		Port:           defaultPort,
		Bind:           defaultBind,
		Runtime:        Runtime{Name: defaultRuntimeName},
		AdmissionCap:   defaultAdmissionCap,
		EnvoyURL:       defaultEnvoyURL,
		Gates:          Gates{Design: DesignGateRootIssues},
		Linger:         72 * time.Hour,
		ReviewRoundCap: 3,
		MaxFixAttempts: 3,
	}
	if file.Project == nil || strings.TrimSpace(*file.Project) == "" {
		return Config{}, errors.New("project is required")
	}
	cfg.Project = *file.Project

	if file.StateDir == nil || strings.TrimSpace(*file.StateDir) == "" {
		return Config{}, errors.New("state_dir is required")
	}
	cfg.StateDir = underConfig(*file.StateDir, configDir)

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

	if err := resolveStage2(file, configDir, &cfg); err != nil {
		return Config{}, err
	}
	if err := resolveStage3(file, configDir, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// resolveStage2 settles the keys Stage 2 models. Each is read from the file alone: the shipped
// loader's environment twins (`LEGION_DAEMON_URL`, `ENVOY_NATS_URL`, `LEGION_WORKER_*_SECONDS`, …)
// are not read, because a daemon started from inside a Legion pane inherits that pane's values
// for exactly those names (config.ts:1497-1503 is the shipped loader working around it).
func resolveStage2(file fileConfig, configDir string, cfg *Config) error {
	cfg.DaemonURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	if file.DaemonURL != nil {
		normalized, err := baseURL(*file.DaemonURL, "daemon_url")
		if err != nil {
			return err
		}
		cfg.DaemonURL = normalized
	}

	if file.OmpInvocation != nil {
		cfg.OmpInvocation = *file.OmpInvocation
	}
	cfg.OmpLaunchPrefix = file.OmpLaunchPrefix
	cfg.ProviderKeys = file.ProviderKeys
	if file.Instructions != nil {
		cfg.InstructionsPath = underConfig(*file.Instructions, configDir)
	}

	switch {
	case file.WorkerStreamPort != nil:
		port := *file.WorkerStreamPort
		if port <= 0 {
			return errors.New("worker_stream_port must be a positive integer")
		}
		if port > 65535 {
			return errors.New("worker_stream_port must be at most 65535")
		}
		cfg.WorkerStreamPort = port
	case cfg.Port+1 > 65535:
		return fmt.Errorf("worker_stream_port defaults to port + 1 (%d), which is not a valid TCP port; set worker_stream_port", cfg.Port+1)
	default:
		cfg.WorkerStreamPort = cfg.Port + 1
	}
	if cfg.WorkerStreamPort == cfg.Port {
		return fmt.Errorf("worker_stream_port must differ from port (both %d)", cfg.Port)
	}

	for _, d := range durationKeys {
		seconds, ok := file.Durations[d.key]
		if !ok {
			seconds = d.defaultValue
		}
		*d.field(cfg) = time.Duration(seconds) * time.Second
	}
	for _, c := range countKeys {
		count, ok := file.Counts[c.key]
		if !ok {
			count = c.defaultValue
		}
		*c.field(cfg) = count
	}
	// The registration deadline is one timer for the product, so each factor fitting is not
	// enough (config.ts:1854-1860).
	if int64(cfg.WorkerBootTimeout/time.Second)*int64(cfg.WorkerBootRegistrationDeadlineIntervals) > maxTimerSeconds {
		return fmt.Errorf("worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals must be at most %d", maxTimerSeconds)
	}

	if file.OperatorTokenFile != nil {
		cfg.OperatorTokenFile = underConfig(*file.OperatorTokenFile, configDir)
	}
	if file.EnvoyURL != nil {
		if _, err := validURL(*file.EnvoyURL, "envoy_url"); err != nil {
			return err
		}
		cfg.EnvoyURL = *file.EnvoyURL
	}
	seen := map[string]bool{}
	for _, raw := range file.NatsURLs {
		if _, err := validURL(raw, "nats_urls"); err != nil {
			return fmt.Errorf("nats_urls entry %q must be a valid URL", raw)
		}
		if !seen[raw] {
			seen[raw] = true
			cfg.NatsURLs = append(cfg.NatsURLs, raw)
		}
	}
	if file.EnvoyTokenFile != nil {
		cfg.EnvoyTokenFile = underConfig(*file.EnvoyTokenFile, configDir)
	}
	return nil
}
