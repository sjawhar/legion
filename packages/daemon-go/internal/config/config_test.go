package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The smallest file that loads: the three Stage 1 keys and the Stage 3 workflow keys with no
// environment fallback.
const minimalFile = `project: demo
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  DEMO: { repo: acme/widgets }
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
`

// The refusals the shipped loader words itself, quoted here from
// packages/daemon/src/daemon/config.ts (:1316, :1320, :1327, :1330, :1350, :1354, :981) so an
// operator moving from the TypeScript daemon reads the same sentence.
const (
	wantDispatchMcpURLMessage  = "dispatch_mcp_url was replaced by dispatch_url (the service base URL, no /mcp)"
	wantDispatchProjectMessage = "dispatch_project was replaced by projects"
	wantBoardProjectIDsMessage = "board_project_ids was replaced by projects"
	wantReposMessage           = "repos was replaced by projects"
	wantWorkerBudgetMessage    = "worker_budget was replaced by worker_cap"
	wantAppLoginsMessage       = "app_logins is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
	wantGatesMergeMessage      = "gates.merge is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func noEnv(string) string { return "" }

func envMap(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// `state_dir` is resolved against the file that set it, as the shipped loader resolves it
// (config.ts:1407). Taking the string as read would make the daemon's state directory depend on
// the cwd `legion start` was launched from, and a `legion stop` from elsewhere would look
// somewhere else.
func TestRelativeStateDirResolvesAgainstTheConfigsDirectory(t *testing.T) {
	path := writeConfigFile(t, strings.Replace(minimalFile, "state_dir: /var/lib/legion", "state_dir: state", 1))

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(filepath.Dir(path), "state"); cfg.StateDir != want {
		t.Fatalf("StateDir = %q, want %q", cfg.StateDir, want)
	}
}

func TestAbsoluteStateDirIsTakenAsItIs(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir != "/var/lib/legion" {
		t.Fatalf("StateDir = %q, want /var/lib/legion", cfg.StateDir)
	}
}

// captureLog swaps the default logger for a JSON one writing to the returned builder, so a test
// can read the accepted-and-ignored lines Load emits.
func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	out := &strings.Builder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return out
}

func ignoredLine(key string, stage int) string {
	quoted, err := json.Marshal(key)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`"key":%s,"stage":%d`, quoted, stage)
}

// defaultsFor is the Config a file carrying only project, state_dir, and postgres_dsn resolves to
// on port, bind, and runtime: every other key at its default. The Stage 2 defaults are the shipped
// loader's (packages/daemon/src/daemon/config.ts:288-312, `port + 1` at :1797, the loopback
// daemon URL at :1510) plus the four keys Stage 2 adds — except omp_invocation, which has none
// here: the shipped default is the OMP fork pin, whose one home is packages/daemon/src/daemon/
// omp-pin.ts (docs/solutions/daemon/omp-pin-bump-behavioral-proof.md:29-43).
func defaultsFor(port int, bind, runtime string) Config {
	return Config{
		Project:                                 "demo",
		Port:                                    port,
		Bind:                                    bind,
		PostgresDSN:                             "postgres://legion@127.0.0.1:5432/legion",
		StateDir:                                "/var/lib/legion",
		Runtime:                                 Runtime{Name: runtime},
		AdmissionCap:                            4,
		DaemonURL:                               fmt.Sprintf("http://127.0.0.1:%d", port),
		OmpInvocation:                           "",
		WorkerStreamPort:                        port + 1,
		WorkerBootTimeout:                       120 * time.Second,
		WorkerBootRegistrationDeadlineIntervals: 3,
		WorkerRPCTimeout:                        5 * time.Second,
		WorkerStopTimeout:                       10 * time.Second,
		TreeStopTimeout:                         60 * time.Second,
		SlowCommandTimeout:                      300 * time.Second,
		ProbeInterval:                           30 * time.Second,
		LaunchFailureLimit:                      3,
		PromptFailureLimit:                      3,
		PromptRetireLimit:                       2,
		EnvoyURL:                                "http://127.0.0.1:9020",
		DispatchURL:                             "http://127.0.0.1:8080",
		DispatchTokenFile:                       "/var/run/legion/DISPATCH_TOKEN",
		Projects:                                map[string]Project{"DEMO": {Repo: "acme/widgets"}},
		Gates:                                   Gates{Design: DesignGateRootIssues},
		GitHubApps: GitHubApps{
			Implement: GitHubApp{AppID: "1", PrivateKey: "implement-test-key", Installations: map[string]string{}},
			Review:    GitHubApp{AppID: "2", PrivateKey: "review-test-key", Installations: map[string]string{}},
		},
		Linger:         72 * time.Hour,
		ReviewRoundCap: 3,
		MaxFixAttempts: 3,
	}
}

func TestLoadMinimalFileAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := defaultsFor(13370, "127.0.0.1", "tmux"); !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
}

// Stage 2 command paths only need the local daemon record. They keep loading until Task 3.12
// wires the workflow's App and Dispatch clients, which is the first operation that needs them.
func TestLoadAllowsStage2ConfigWithoutWorkflowKeys(t *testing.T) {
	_, err := Load(writeConfigFile(t, "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"), noEnv)

	if err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Every key Stage 2 models, set to a value other than its default, lands in Config as written —
// a relative path resolved against the file's directory, as the shipped loader resolves
// `instructions`, `envoy_token_file`, and `operator_token_file` (config.ts:1300-1313, 1409-1415).
func TestLoadReadsEveryStage2Key(t *testing.T) {
	path := writeConfigFile(t, minimalFile+`port: 14000
daemon_url: http://127.0.0.1:14000/
omp_invocation: mise x github:acme/omp@1 -- omp
omp_launch_prefix: [env, OMP_PROFILE=legion, --, env, OMP_PROFILE=legion, --]
provider_keys: {GEMINI_API_KEY: GEMINI_API_KEY_TESTS, ANTHROPIC_API_KEY: ANTHROPIC_API_KEY}
instructions: rules/instructions.md
worker_stream_port: 14100
worker_boot_timeout_seconds: 90
worker_boot_registration_deadline_intervals: 4
worker_rpc_timeout_seconds: 7
worker_stop_timeout_seconds: 11
tree_stop_timeout_seconds: 61
slow_command_timeout_seconds: 301
probe_interval_seconds: 15
launch_failure_limit: 5
prompt_failure_limit: 6
prompt_retire_limit: 7
operator_token_file: tokens/OPERATOR_TOKEN
envoy_url: http://127.0.0.1:19020
nats_urls: [nats://127.0.0.1:4222, nats://127.0.0.1:4223, nats://127.0.0.1:4222]
envoy_token_file: /run/legion/ENVOY_TOKEN
`)
	dir := filepath.Dir(path)

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := defaultsFor(14000, "127.0.0.1", "tmux")
	want.DaemonURL = "http://127.0.0.1:14000"
	want.OmpInvocation = "mise x github:acme/omp@1 -- omp"
	want.OmpLaunchPrefix = []string{"env", "OMP_PROFILE=legion", "--", "env", "OMP_PROFILE=legion", "--"}
	// In file order: the variable OMP reads, and the secretsd key that holds it.
	want.ProviderKeys = []ProviderKey{
		{Env: "GEMINI_API_KEY", Secret: "GEMINI_API_KEY_TESTS"},
		{Env: "ANTHROPIC_API_KEY", Secret: "ANTHROPIC_API_KEY"},
	}
	want.InstructionsPath = filepath.Join(dir, "rules/instructions.md")
	want.WorkerStreamPort = 14100
	want.WorkerBootTimeout = 90 * time.Second
	want.WorkerBootRegistrationDeadlineIntervals = 4
	want.WorkerRPCTimeout = 7 * time.Second
	want.WorkerStopTimeout = 11 * time.Second
	want.TreeStopTimeout = 61 * time.Second
	want.SlowCommandTimeout = 301 * time.Second
	want.ProbeInterval = 15 * time.Second
	want.LaunchFailureLimit = 5
	want.PromptFailureLimit = 6
	want.PromptRetireLimit = 7
	want.OperatorTokenFile = filepath.Join(dir, "tokens/OPERATOR_TOKEN")
	want.EnvoyURL = "http://127.0.0.1:19020"
	// A set, as the shipped `readStringArray` makes it (config.ts:438-447): the repeat is dropped,
	// first occurrence kept. The launch prefix is argv and keeps its repeats (config.ts:449-461).
	want.NatsURLs = []string{"nats://127.0.0.1:4222", "nats://127.0.0.1:4223"}
	want.EnvoyTokenFile = "/run/legion/ENVOY_TOKEN"
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
}

// Stage 3's keys are settled here so later workflow tasks receive one fully validated daemon
// configuration rather than parsing their own YAML fragments.
func TestLoadReadsEveryStage3Key(t *testing.T) {
	path := writeConfigFile(t, strings.ReplaceAll(minimalFile, "dispatch_url: http://127.0.0.1:8080\n"+
		"dispatch_token_file: /var/run/legion/DISPATCH_TOKEN\n"+
		"projects:\n  DEMO: { repo: acme/widgets }\n"+
		"gates:\n  design: root-issues\n"+
		"github_apps:\n  implement:\n    app_id: \"1\"\n    private_key: implement-test-key\n"+
		"  review:\n    app_id: \"2\"\n    private_key: review-test-key\n", `dispatch_url: https://dispatch.example
dispatch_token_file: tokens/DISPATCH_TOKEN
projects:
  DEMO: { repo: acme/widgets }
  OTHER: { repo: acme/other, merge_queue_role: merge-queue }
gates:
  design: off
github_apps:
  implement:
    app_id: "11"
    private_key: implement-key
    installations: { sjawhar: "101" }
  review:
    app_id: "22"
    private_key: review-key
linger_hours: 96
review_round_cap: 5
max_fix_attempts: 4
`))

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DispatchURL != "https://dispatch.example" {
		t.Errorf("DispatchURL = %q", cfg.DispatchURL)
	}
	if want := filepath.Join(filepath.Dir(path), "tokens/DISPATCH_TOKEN"); cfg.DispatchTokenFile != want {
		t.Errorf("DispatchTokenFile = %q, want %q", cfg.DispatchTokenFile, want)
	}
	if !reflect.DeepEqual(cfg.Projects, map[string]Project{
		"DEMO":  {Repo: "acme/widgets"},
		"OTHER": {Repo: "acme/other", MergeQueueRole: "merge-queue"},
	}) {
		t.Errorf("Projects = %#v", cfg.Projects)
	}
	if cfg.Gates.Design != DesignGateOff {
		t.Errorf("Gates.Design = %q, want %q", cfg.Gates.Design, DesignGateOff)
	}
	if got := cfg.GitHubApps.Implement; got.AppID != "11" || got.PrivateKey != "implement-key" || got.Installations["sjawhar"] != "101" {
		t.Errorf("implement app = %#v", got)
	}
	if cfg.GitHubApps.Review.AppID != "22" || cfg.GitHubApps.Review.PrivateKey != "review-key" {
		t.Errorf("review app = %#v", cfg.GitHubApps.Review)
	}
	if cfg.Linger != 96*time.Hour || cfg.ReviewRoundCap != 5 || cfg.MaxFixAttempts != 4 {
		t.Errorf("Linger=%s ReviewRoundCap=%d MaxFixAttempts=%d, want 96h, 5, 4", cfg.Linger, cfg.ReviewRoundCap, cfg.MaxFixAttempts)
	}
}

func TestLoadReadsPrivateKeyCommand(t *testing.T) {
	body := strings.Replace(minimalFile, "private_key: implement-test-key", "private_key_command: printf command-key", 1)

	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubApps.Implement.PrivateKey != "command-key" {
		t.Errorf("command key = %q, want command-key", cfg.GitHubApps.Implement.PrivateKey)
	}
}

func TestLoadAcceptsPrivateKeyCommandWithUnpaddedBase64Output(t *testing.T) {
	privateKey := "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----"
	encoded := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(privateKey)), "=")
	script := fmt.Sprintf(`import base64,sys; k=%q; sys.stdout.write(base64.b64decode(k + "=" * (-len(k) %% 4)).decode())`, encoded)
	command := fmt.Sprintf("python3 -c %q", script)
	body := strings.Replace(minimalFile, "private_key: implement-test-key", fmt.Sprintf("private_key_command: %q", command), 1)

	cfg, err := Load(writeConfigFile(t, body), noEnv)

	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubApps.Implement.PrivateKey != privateKey {
		t.Errorf("command key = %q, want unpadded fixture", cfg.GitHubApps.Implement.PrivateKey)
	}
}

func TestLoadRefusesFailedPrivateKeyCommandEvenWhenItPrintsPEM(t *testing.T) {
	command := "printf '%s\\n' '-----BEGIN PRIVATE KEY-----' fixture '-----END PRIVATE KEY-----'; exit 1"
	body := strings.Replace(minimalFile, "private_key: implement-test-key", fmt.Sprintf("private_key_command: %q", command), 1)

	_, err := Load(writeConfigFile(t, body), noEnv)

	if want := "github_apps.implement.private_key_command failed (exit 1)"; err == nil || err.Error() != want {
		t.Errorf("Load error = %v, want %q", err, want)
	}
}

func TestLoadRefusesEveryStage3Key(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "dispatch_url is not a URL",
			body: minimalFile + "dispatch_url: dispatch\n",
			want: "dispatch_url must be a valid URL",
		},
		{
			name: "dispatch_token_file is blank",
			body: minimalFile + "dispatch_token_file: \"\"\n",
			want: "dispatch_token_file must not be empty",
		},
		{
			name: "projects is not a mapping",
			body: minimalFile + "projects: [DEMO]\n",
			want: "projects must be a mapping",
		},
		{
			name: "projects is empty",
			body: minimalFile + "projects: {}\n",
			want: "projects must declare at least one project",
		},
		{
			name: "projects key is invalid",
			body: minimalFile + "projects: {demo: {repo: acme/widgets}}\n",
			want: `projects key "demo" must match ^[A-Z][A-Z0-9]*$`,
		},
		{
			name: "projects entry has no repo",
			body: minimalFile + "projects: {DEMO: {}}\n",
			want: `projects.DEMO.repo must be "owner/name" (got "undefined")`,
		},
		{
			name: "gates is not a mapping",
			body: minimalFile + "gates: root-issues\n",
			want: "gates must be a mapping",
		},
		{
			name: "gates design is invalid",
			body: minimalFile + "gates: {design: later}\n",
			want: "gates.design must be 'root-issues' or 'off'",
		},
		{
			name: "gates has an unknown setting",
			body: minimalFile + "gates: {unknown: true}\n",
			want: "unknown key gates.unknown",
		},
		{
			name: "github_apps is not a mapping",
			body: minimalFile + "github_apps: [implement]\n",
			want: "github_apps must be a mapping",
		},
		{
			name: "github_apps is required for workflow configuration",
			body: "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nprojects: {DEMO: {repo: acme/widgets}}\n",
			want: "github_apps is required",
		},
		{
			name: "github_apps review is absent",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\", private_key: key}}\n",
			want: "github_apps.review is required",
		},
		{
			name: "github app misses an identifier",
			body: minimalFile + "github_apps: {implement: {private_key: key}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement is missing required fields: app_id",
		},
		{
			name: "github app has no private key source",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\"}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement requires exactly one of private_key, private_key_command, or private_key_secret",
		},
		{
			name: "github app has two private key sources",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\", private_key: key, private_key_command: \"printf key\"}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement requires exactly one of private_key, private_key_command, or private_key_secret",
		},
		{
			name: "github app secret is not a single key name",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\", private_key_secret: \"secrets get KEY\"}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement.private_key_secret must be a single secretsd key name (no whitespace)",
		},
		{
			name: "linger_hours is not positive",
			body: minimalFile + "linger_hours: 0\n",
			want: "linger_hours must be a positive number",
		},
		{
			name: "linger_hours exceeds the timer bound",
			body: minimalFile + "linger_hours: 597\n",
			want: "linger_hours must be at most 596",
		},
		{
			name: "review_round_cap is not positive",
			body: minimalFile + "review_round_cap: 0\n",
			want: "review_round_cap must be a positive integer",
		},
		{
			name: "review_round_cap is not an integer",
			body: minimalFile + "review_round_cap: three\n",
			want: "review_round_cap must be an integer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, tc.body), noEnv)
			if err == nil {
				t.Fatalf("Load succeeded, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Load error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadResolvesPrivateKeySecretWithDaemonEnvironment(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	status := `{"key":"TEST_APP_KEY","tier":"human"}`
	privateKey := "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----"
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s %%s %%s %%s\n' "$1" "$2" "$3" "${SECRETSD_SESSION_TOKEN_FILE:-unset}" >> %q
case "$3" in
  --no-request) printf '%%s' %q ;;
  --value) printf '%%s' %q ;;
esac
`, calls, status, strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(privateKey)), "="))
	secretsPath := filepath.Join(dir, "secrets")
	if err := os.WriteFile(secretsPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake secrets: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SECRETSD_SESSION_TOKEN_FILE", "/agent/session/token")
	body := strings.Replace(minimalFile, "private_key: implement-test-key", "private_key_secret: TEST_APP_KEY", 1)

	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubApps.Implement.PrivateKey != privateKey {
		t.Errorf("secret private key = %q, want decoded fixture", cfg.GitHubApps.Implement.PrivateKey)
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	if want := "get TEST_APP_KEY --no-request unset\nget TEST_APP_KEY --value unset\n"; string(got) != want {
		t.Errorf("secrets calls = %q, want %q", got, want)
	}
}

func TestResolveGitHubAppsRefusesAgentTierPrivateKey(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '{\"key\":\"AGENT_APP_KEY\",\"tier\":\"agent\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake secrets: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	apps := GitHubApps{
		Implement: GitHubApp{AppID: "1", PrivateKeySecret: "AGENT_APP_KEY"},
		Review:    GitHubApp{AppID: "2", PrivateKey: "review-key"},
	}

	_, err := ResolveGitHubApps(apps)

	if want := "App private key AGENT_APP_KEY is readable by agent-tier callers; move it to a daemon-only store"; err == nil || err.Error() != want {
		t.Errorf("ResolveGitHubApps error = %v, want %q", err, want)
	}
}

// `worker_stream_port` defaults to one past `port` (config.ts:1793-1798), which is why it has to
// move with a file that moves `port`.
func TestWorkerStreamPortDefaultsToOnePastPort(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"port: 20000\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerStreamPort != 20001 {
		t.Errorf("WorkerStreamPort = %d, want 20001", cfg.WorkerStreamPort)
	}
	if cfg.DaemonURL != "http://127.0.0.1:20000" {
		t.Errorf("DaemonURL = %q, want the loopback URL on the file's port", cfg.DaemonURL)
	}
}

func TestLoadRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		env  func(string) string
		want string
	}{
		{
			name: "postgres_dsn absent from the file and the environment",
			body: "project: demo\nstate_dir: /var/lib/legion\n",
			want: "postgres_dsn is required (or set LEGION_POSTGRES_DSN)",
		},
		{
			name: "postgres_dsn present but blank",
			body: "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: \"  \"\n",
			want: "postgres_dsn is required (or set LEGION_POSTGRES_DSN)",
		},
		{
			name: "project absent",
			body: "state_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n",
			want: "project is required",
		},
		{
			name: "state_dir absent",
			body: "project: demo\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n",
			want: "state_dir is required",
		},
		{
			name: "admission_cap zero",
			body: minimalFile + "admission_cap: 0\n",
			want: "admission_cap must be a positive integer",
		},
		{
			name: "admission_cap negative",
			body: minimalFile + "admission_cap: -1\n",
			want: "admission_cap must be a positive integer",
		},
		{
			name: "omp_invocation not a string",
			body: minimalFile + "omp_invocation: [mise]\n",
			want: "omp_invocation must be a string",
		},
		{
			name: "omp_invocation blank",
			body: minimalFile + "omp_invocation: \"  \"\n",
			want: "omp_invocation must not be empty",
		},
		{
			name: "omp_launch_prefix a string rather than argv",
			body: minimalFile + "omp_launch_prefix: env OMP_PROFILE=legion\n",
			want: "omp_launch_prefix must be an array of non-empty strings",
		},
		{
			name: "omp_launch_prefix with an empty argument",
			body: minimalFile + "omp_launch_prefix: [env, \"\"]\n",
			want: "omp_launch_prefix must be an array of non-empty strings",
		},
		{
			name: "instructions blank",
			body: minimalFile + "instructions: \"\"\n",
			want: "instructions must not be empty",
		},
		{
			name: "daemon_url not a URL",
			body: minimalFile + "daemon_url: 127.0.0.1:13370\n",
			want: "daemon_url must be a valid URL",
		},
		{
			name: "daemon_url with a query string",
			body: minimalFile + "daemon_url: http://127.0.0.1:13370/?x=1\n",
			want: "daemon_url must not include a query string or fragment",
		},
		{
			name: "worker_stream_port zero",
			body: minimalFile + "worker_stream_port: 0\n",
			want: "worker_stream_port must be a positive integer",
		},
		{
			name: "worker_stream_port past the TCP range",
			body: minimalFile + "worker_stream_port: 65536\n",
			want: "worker_stream_port must be at most 65535",
		},
		{
			name: "worker_stream_port equal to port",
			body: minimalFile + "worker_stream_port: 13370\n",
			want: "worker_stream_port must differ from port (both 13370)",
		},
		{
			name: "worker_stream_port defaulting past the TCP range",
			body: minimalFile + "port: 65535\n",
			want: "worker_stream_port defaults to port + 1 (65536), which is not a valid TCP port; set worker_stream_port",
		},
		{
			name: "worker_boot_timeout_seconds zero",
			body: minimalFile + "worker_boot_timeout_seconds: 0\n",
			want: "worker_boot_timeout_seconds must be a positive integer",
		},
		{
			name: "worker_boot_timeout_seconds past the timer bound",
			body: minimalFile + "worker_boot_timeout_seconds: 2147484\n",
			want: "worker_boot_timeout_seconds must be at most 2147483",
		},
		{
			name: "worker_boot_timeout_seconds not an integer",
			body: minimalFile + "worker_boot_timeout_seconds: 1.5\n",
			want: "worker_boot_timeout_seconds must be an integer",
		},
		{
			name: "worker_boot_registration_deadline_intervals negative",
			body: minimalFile + "worker_boot_registration_deadline_intervals: -3\n",
			want: "worker_boot_registration_deadline_intervals must be a positive integer",
		},
		{
			name: "the registration deadline past the timer bound",
			body: minimalFile + "worker_boot_timeout_seconds: 2147483\nworker_boot_registration_deadline_intervals: 2\n",
			want: "worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals must be at most 2147483",
		},
		{
			name: "worker_rpc_timeout_seconds zero",
			body: minimalFile + "worker_rpc_timeout_seconds: 0\n",
			want: "worker_rpc_timeout_seconds must be a positive integer",
		},
		{
			name: "worker_stop_timeout_seconds past the timer bound",
			body: minimalFile + "worker_stop_timeout_seconds: 9999999\n",
			want: "worker_stop_timeout_seconds must be at most 2147483",
		},
		{
			name: "tree_stop_timeout_seconds zero",
			body: minimalFile + "tree_stop_timeout_seconds: 0\n",
			want: "tree_stop_timeout_seconds must be a positive integer",
		},
		{
			name: "slow_command_timeout_seconds zero",
			body: minimalFile + "slow_command_timeout_seconds: 0\n",
			want: "slow_command_timeout_seconds must be a positive integer",
		},
		{
			name: "probe_interval_seconds zero",
			body: minimalFile + "probe_interval_seconds: 0\n",
			want: "probe_interval_seconds must be a positive integer",
		},
		{
			name: "probe_interval_seconds past the timer bound",
			body: minimalFile + "probe_interval_seconds: 2147484\n",
			want: "probe_interval_seconds must be at most 2147483",
		},
		{
			name: "launch_failure_limit zero",
			body: minimalFile + "launch_failure_limit: 0\n",
			want: "launch_failure_limit must be a positive integer",
		},
		{
			name: "prompt_failure_limit not an integer",
			body: minimalFile + "prompt_failure_limit: three\n",
			want: "prompt_failure_limit must be an integer",
		},
		{
			name: "prompt_retire_limit negative",
			body: minimalFile + "prompt_retire_limit: -2\n",
			want: "prompt_retire_limit must be a positive integer",
		},
		{
			name: "operator_token_file blank",
			body: minimalFile + "operator_token_file: \" \"\n",
			want: "operator_token_file must not be empty",
		},
		{
			name: "envoy_url not a URL",
			body: minimalFile + "envoy_url: envoy-listener\n",
			want: "envoy_url must be a valid URL",
		},
		{
			name: "nats_urls a scalar",
			body: minimalFile + "nats_urls: nats://127.0.0.1:4222\n",
			want: "nats_urls must be an array of non-empty strings",
		},
		{
			name: "nats_urls holding something that is not a URL",
			body: minimalFile + "nats_urls: [nats://127.0.0.1:4222, nats-host]\n",
			want: `nats_urls entry "nats-host" must be a valid URL`,
		},
		{
			name: "envoy_token_file blank",
			body: minimalFile + "envoy_token_file: \"\"\n",
			want: "envoy_token_file must not be empty",
		},
		{
			// `secrets` in a pane reads its config and sops's age identity from the pane's own XDG
			// home, which is the daemon's isolated one: it cannot decrypt there, and handing it the
			// age identity would hand every pane every agent-tier secret on the box.
			name: "omp_launch_prefix running secrets",
			body: minimalFile + "omp_launch_prefix: [secrets, ANTHROPIC_API_KEY, --]\n",
			want: `omp_launch_prefix runs "secrets", which cannot decrypt inside a Go pane (its XDG home is isolated); name the keys in provider_keys instead`,
		},
		{
			name: "omp_launch_prefix running secrets by its path",
			body: minimalFile + "omp_launch_prefix: [/home/legion/.local/bin/secrets, ANTHROPIC_API_KEY, --]\n",
			want: `omp_launch_prefix runs "secrets", which cannot decrypt inside a Go pane (its XDG home is isolated); name the keys in provider_keys instead`,
		},
		{
			// A deployment names each secret per environment (GEMINI_API_KEY_TESTS) while OMP reads
			// the provider's own variable (GEMINI_API_KEY), so the one shape says both.
			name: "provider_keys as a list",
			body: minimalFile + "provider_keys: [GEMINI_API_KEY_TESTS]\n",
			want: "provider_keys must be a mapping of the variable OMP reads to the secretsd key that holds it, e.g. {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}",
		},
		{
			name: "provider_keys a scalar",
			body: minimalFile + "provider_keys: GEMINI_API_KEY_TESTS\n",
			want: "provider_keys must be a mapping of the variable OMP reads to the secretsd key that holds it, e.g. {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}",
		},
		{
			name: "provider_keys with a variable that is not a variable name",
			body: minimalFile + "provider_keys: {1PASSWORD: ONEPASSWORD_TESTS}\n",
			want: `provider_keys key "1PASSWORD" (the variable OMP reads) must be an environment variable name (letters, digits, and underscores, not starting with a digit)`,
		},
		{
			name: "provider_keys with a command where the secretsd key goes",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: \"secrets get GEMINI_API_KEY_TESTS\"}\n",
			want: `provider_keys value "secrets get GEMINI_API_KEY_TESTS" for GEMINI_API_KEY (the secretsd key name) must be an environment variable name (letters, digits, and underscores, not starting with a digit)`,
		},
		{
			name: "provider_keys with a secretsd key that is not a string",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: [GEMINI_API_KEY_TESTS]}\n",
			want: `provider_keys value for GEMINI_API_KEY (the secretsd key name) must be a string`,
		},
		{
			name: "provider_keys naming one variable twice",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: GEMINI_API_KEY_TESTS, GEMINI_API_KEY: GEMINI_API_KEY_PROD}\n",
			want: "provider_keys names GEMINI_API_KEY twice",
		},
		{
			name: "admission_cap not an integer",
			body: minimalFile + "admission_cap: four\n",
			want: "admission_cap must be an integer",
		},
		{
			name: "LEGION_ADMISSION_CAP not a positive integer",
			body: minimalFile,
			env:  envMap(map[string]string{"LEGION_ADMISSION_CAP": "zero"}),
			want: "LEGION_ADMISSION_CAP must be a positive integer",
		},
		{
			name: "port outside the TCP range",
			body: minimalFile + "port: 70000\n",
			want: "port must be a valid TCP port (1-65535)",
		},
		{
			name: "tossed worker_cap",
			body: minimalFile + "worker_cap: 3\n",
			want: "unknown key worker_cap: the running-worker cap no longer exists (LEGION-208 Requirement 8)",
		},
		{
			name: "tossed worker_idle_retire_seconds",
			body: minimalFile + "worker_idle_retire_seconds: 600\n",
			want: "unknown key worker_idle_retire_seconds: a worker is suspended when its phase ends, never after an idle window (LEGION-208 Design, \"Process supervision\")",
		},
		{
			name: "tossed resync_interval_seconds",
			body: minimalFile + "resync_interval_seconds: 600\n",
			want: "unknown key resync_interval_seconds: the mirror of Dispatch and GitHub as truth, and resync's drift healing, no longer exist (LEGION-208 Design, \"Ported, and tossed\")",
		},
		{
			name: "migration-only dispatch_mcp_url",
			body: minimalFile + "dispatch_mcp_url: https://dispatch.example/mcp\n",
			want: wantDispatchMcpURLMessage,
		},
		{
			name: "migration-only dispatch_project",
			body: minimalFile + "dispatch_project: DEMO\n",
			want: wantDispatchProjectMessage,
		},
		{
			name: "migration-only board_project_ids",
			body: minimalFile + "board_project_ids: [1]\n",
			want: wantBoardProjectIDsMessage,
		},
		{
			name: "migration-only repos",
			body: minimalFile + "repos: {DEMO: acme/widgets}\n",
			want: wantReposMessage,
		},
		{
			name: "migration-only app_logins",
			body: minimalFile + "app_logins: [legion-implementer]\n",
			want: wantAppLoginsMessage,
		},
		{
			name: "migration-only worker_budget",
			body: minimalFile + "worker_budget: 6\n",
			want: wantWorkerBudgetMessage,
		},
		{
			name: "gates.merge",
			body: minimalFile + "gates:\n  design: root-issues\n  merge: code-owners\n",
			want: wantGatesMergeMessage,
		},
		{
			name: "unknown member of gates",
			body: minimalFile + "gates: {frobnicate: 1}\n",
			want: "unknown key gates.frobnicate",
		},
		{
			name: "gates is not a mapping",
			body: minimalFile + "gates: root-issues\n",
			want: "gates must be a mapping",
		},
		{
			name: "a typo",
			body: minimalFile + "prot: 1\n",
			want: "unknown key prot",
		},
		{
			name: "an unknown runtime name",
			body: minimalFile + "runtime: docker\n",
			want: "runtime must be 'tmux' or 'kubernetes'",
		},
		{
			name: "a runtime mapping with two keys",
			body: minimalFile + "runtime: {kubernetes: {namespace: legion}, tmux: {}}\n",
			want: "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes",
		},
		{
			name: "a runtime mapping with another single key",
			body: minimalFile + "runtime: {podman: {}}\n",
			want: "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes",
		},
		{
			name: "a root that is not a mapping",
			body: "- project: demo\n",
			want: "config file root must be a mapping",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			if env == nil {
				env = noEnv
			}
			_, err := Load(writeConfigFile(t, tc.body), env)
			if err == nil {
				t.Fatalf("Load succeeded, want error %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Load error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadReadsPostgresDSNFromTheEnvironment(t *testing.T) {
	env := envMap(map[string]string{"LEGION_POSTGRES_DSN": "postgres://legion@db:5432/legion"})

	fromEnv, err := Load(writeConfigFile(t, strings.Replace(minimalFile, "postgres_dsn: postgres://legion@127.0.0.1:5432/legion\n", "", 1)), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fromEnv.PostgresDSN != "postgres://legion@db:5432/legion" {
		t.Errorf("PostgresDSN = %q, want the environment's value", fromEnv.PostgresDSN)
	}

	fileWins, err := Load(writeConfigFile(t, minimalFile), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fileWins.PostgresDSN != "postgres://legion@127.0.0.1:5432/legion" {
		t.Errorf("PostgresDSN = %q, want the file's value", fileWins.PostgresDSN)
	}
}

func TestLoadReadsAdmissionCapFromTheEnvironment(t *testing.T) {
	env := envMap(map[string]string{"LEGION_ADMISSION_CAP": "2"})

	fromEnv, err := Load(writeConfigFile(t, minimalFile), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fromEnv.AdmissionCap != 2 {
		t.Errorf("AdmissionCap = %d, want 2 from the environment", fromEnv.AdmissionCap)
	}

	fileWins, err := Load(writeConfigFile(t, minimalFile+"admission_cap: 7\n"), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fileWins.AdmissionCap != 7 {
		t.Errorf("AdmissionCap = %d, want 7 from the file", fileWins.AdmissionCap)
	}
}

func TestLoadReadsRuntimeAsItsDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "absent", body: minimalFile, want: "tmux"},
		{name: "the tmux scalar", body: minimalFile + "runtime: tmux\n", want: "tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, tc.body), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Runtime.Name != tc.want || cfg.Runtime.Kubernetes != nil {
				t.Errorf("Runtime = %+v, want %q and no kubernetes block", cfg.Runtime, tc.want)
			}
		})
	}
}

// Every top-level key of the shipped schema (CONFIG_SCHEMA, packages/daemon/src/daemon/config.ts
// :319-415, 37 keys) plus the new postgres_dsn, and the class it is in at Stage 2. No shipped key
// may fall through to the typo refusal. Stage 2 moved fifteen keys from known-later to modelled.
func TestLoadClassifiesEveryShippedKey(t *testing.T) {
	const (
		modelled   = "modelled"
		knownLater = "known-later"
		tossed     = "tossed"
		migration  = "migration-only"
	)
	for _, tc := range []struct {
		key   string
		line  string
		class string
		stage int
		want  string
	}{
		{key: "project", class: modelled},
		{key: "state_dir", class: modelled},
		{key: "postgres_dsn", class: modelled},
		{key: "port", line: "port: 13370", class: modelled},
		{key: "bind", line: "bind: 127.0.0.1", class: modelled},
		{key: "runtime", line: "runtime: tmux", class: modelled},
		{key: "admission_cap", line: "admission_cap: 4", class: modelled},

		{key: "daemon_url", line: "daemon_url: http://127.0.0.1:13370", class: modelled},
		{key: "instructions", line: "instructions: /etc/legion/instructions.md", class: modelled},
		{key: "omp_invocation", line: "omp_invocation: mise x github:acme/omp@1 -- omp", class: modelled},
		{key: "omp_launch_prefix", line: "omp_launch_prefix: [env, OMP_PROFILE=legion]", class: modelled},
		{key: "worker_stream_port", line: "worker_stream_port: 13371", class: modelled},
		{key: "worker_boot_timeout_seconds", line: "worker_boot_timeout_seconds: 120", class: modelled},
		{key: "worker_boot_registration_deadline_intervals", line: "worker_boot_registration_deadline_intervals: 3", class: modelled},
		{key: "worker_rpc_timeout_seconds", line: "worker_rpc_timeout_seconds: 5", class: modelled},
		{key: "worker_stop_timeout_seconds", line: "worker_stop_timeout_seconds: 10", class: modelled},
		{key: "tree_stop_timeout_seconds", line: "tree_stop_timeout_seconds: 60", class: modelled},
		{key: "slow_command_timeout_seconds", line: "slow_command_timeout_seconds: 300", class: modelled},
		{key: "envoy_url", line: "envoy_url: http://127.0.0.1:9020", class: modelled},
		{key: "envoy_token_file", line: "envoy_token_file: /var/run/legion/ENVOY_TOKEN", class: modelled},
		{key: "nats_urls", line: "nats_urls: [nats://127.0.0.1:4222]", class: modelled},
		{key: "operator_token_file", line: "operator_token_file: /var/run/legion/OPERATOR_TOKEN", class: modelled},

		{key: "dispatch_url", line: "dispatch_url: https://dispatch.example", class: modelled},
		{key: "dispatch_token_file", line: "dispatch_token_file: /var/run/legion/DISPATCH_TOKEN", class: modelled},
		{key: "projects", line: "projects: {DEMO: {repo: acme/widgets}}", class: modelled},
		{key: "gates", line: "gates: {design: off}", class: modelled},
		{key: "github_apps", line: "github_apps: {implement: {app_id: \"1\", private_key: key}, review: {app_id: \"2\", private_key: key}}", class: modelled},
		{key: "max_recursion_depth", line: "max_recursion_depth: 8", class: knownLater, stage: 3},
		{key: "linger_hours", line: "linger_hours: 72", class: modelled},
		{key: "review_round_cap", line: "review_round_cap: 3", class: modelled},
		{key: "max_fix_attempts", line: "max_fix_attempts: 3", class: modelled},

		{
			key: "worker_cap", line: "worker_cap: 10", class: tossed,
			want: "unknown key worker_cap: the running-worker cap no longer exists (LEGION-208 Requirement 8)",
		},
		{
			key: "worker_idle_retire_seconds", line: "worker_idle_retire_seconds: 600", class: tossed,
			want: "unknown key worker_idle_retire_seconds: a worker is suspended when its phase ends, never after an idle window (LEGION-208 Design, \"Process supervision\")",
		},
		{
			key: "resync_interval_seconds", line: "resync_interval_seconds: 600", class: tossed,
			want: "unknown key resync_interval_seconds: the mirror of Dispatch and GitHub as truth, and resync's drift healing, no longer exist (LEGION-208 Design, \"Ported, and tossed\")",
		},

		{key: "dispatch_mcp_url", line: "dispatch_mcp_url: https://dispatch.example/mcp", class: migration, want: wantDispatchMcpURLMessage},
		{key: "dispatch_project", line: "dispatch_project: DEMO", class: migration, want: wantDispatchProjectMessage},
		{key: "board_project_ids", line: "board_project_ids: [1]", class: migration, want: wantBoardProjectIDsMessage},
		{key: "repos", line: "repos: {DEMO: acme/widgets}", class: migration, want: wantReposMessage},
		{key: "app_logins", line: "app_logins: [legion-implementer]", class: migration, want: wantAppLoginsMessage},
		{key: "worker_budget", line: "worker_budget: 6", class: migration, want: wantWorkerBudgetMessage},
	} {
		t.Run(tc.key, func(t *testing.T) {
			out := captureLog(t)
			body := minimalFile
			if tc.line != "" {
				body += tc.line + "\n"
			}

			_, err := Load(writeConfigFile(t, body), noEnv)

			switch tc.class {
			case modelled:
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if strings.Contains(out.String(), fmt.Sprintf(`"key":%q`, tc.key)) {
					t.Errorf("a modelled key was logged as accepted and ignored: %s", out.String())
				}
			case knownLater:
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if !strings.Contains(out.String(), ignoredLine(tc.key, tc.stage)) {
					t.Errorf("log does not name %s at stage %d; log was:\n%s", tc.key, tc.stage, out.String())
				}
			default:
				if err == nil {
					t.Fatalf("Load succeeded, want error %q", tc.want)
				}
				if err.Error() != tc.want {
					t.Errorf("Load error = %q, want %q", err.Error(), tc.want)
				}
			}
		})
	}
}

func TestLoadNamesAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := Load(path, noEnv)

	if err == nil {
		t.Fatal("Load succeeded on an absent file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load error = %q, want it to name %s", err.Error(), path)
	}
}
