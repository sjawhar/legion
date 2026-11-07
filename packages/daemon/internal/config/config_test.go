package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The smallest file that loads: the three Stage 1 keys and the Stage 3 workflow keys with no
// environment fallback.
const minimalFile = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  DEMO: {}
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
`

// The five `gates.design` shapes the configuration check drives `legion start --check-config`
// against: a list, the empty list, no `gates` key at all, a word the key does not take, and a
// list naming one step twice. Each is a whole legion.yaml but for the one thing it tests, so the
// check reads the OK line or the refusal and nothing else can be what failed. Their paths are
// relative, which the loader resolves against the file: the check writes the two bearer files
// beside the file it writes, wherever it writes it.
const gatesListFile = `project: DEMO
state_dir: ./state
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: ./dispatch-token
operator_token_file: ./operator-token
projects:
  DEMO: {}
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
gates:
  design: [decisions, criteria]
`

const gatesEmptyListFile = `project: DEMO
state_dir: ./state
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: ./dispatch-token
operator_token_file: ./operator-token
projects:
  DEMO: {}
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
gates:
  design: []
`

const gatesAbsentFile = `project: DEMO
state_dir: ./state
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: ./dispatch-token
operator_token_file: ./operator-token
projects:
  DEMO: {}
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
`

const gatesUnknownWordFile = `project: DEMO
state_dir: ./state
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: ./dispatch-token
operator_token_file: ./operator-token
projects:
  DEMO: {}
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
gates:
  design: sometimes
`

const gatesRepeatedStepFile = `project: DEMO
state_dir: ./state
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: ./dispatch-token
operator_token_file: ./operator-token
projects:
  DEMO: {}
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
gates:
  design: [spec, criteria, spec]
`

// The refusals of the replaced keys, of app_logins, and of gates.merge, quoted here so a change to
// any of them fails a test.
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
// (loadConfigFromFile, config.ts). Taking the string as read would make the daemon's state
// directory depend on the cwd `legion start` was launched from, and a `legion stop` from elsewhere
// would look somewhere else.
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

// defaultsFor is the Config minimalFile resolves to on port: every key it leaves out at its default,
// on the loopback bind and the tmux runtime, with the API on port, the worker stream on `port + 1`
// and a loopback daemon URL — except omp_invocation, which has none: the OMP fork pin's one home
// is the repository's .omp-pin.
func defaultsFor(port int) Config {
	return Config{
		Project:                                 "DEMO",
		Port:                                    port,
		Bind:                                    "127.0.0.1",
		PostgresDSN:                             "postgres://legion@127.0.0.1:5432/legion",
		StateDir:                                "/var/lib/legion",
		Runtime:                                 Runtime{Name: "tmux"},
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
		ControllerWakeInterval:                  time.Hour,
		LaunchFailureLimit:                      3,
		PromptFailureLimit:                      3,
		PromptRetireLimit:                       2,
		WorkerWakesPerMinute:                    8,
		WorkerWakeBurst:                         1,
		EnvoyURL:                                "http://127.0.0.1:9020",
		NatsURLs:                                []string{"nats://127.0.0.1:4222"},
		DispatchURL:                             "http://127.0.0.1:8080",
		DispatchTokenFile:                       "/var/run/legion/DISPATCH_TOKEN",
		Projects:                                map[string]Project{"DEMO": {ProductionCheck: claim.RoleTester}},
		Gates:                                   Gates{Design: DesignGateRootIssues.Steps()},
		GitHubApps: GitHubApps{
			Implement: GitHubApp{AppID: "1", PrivateKey: "implement-test-key", Installations: map[string]string{}},
			Review:    GitHubApp{AppID: "2", PrivateKey: "review-test-key", Installations: map[string]string{}},
		},
		Linger:               72 * time.Hour,
		ProductionCheckBound: 336 * time.Hour,
		PodUpdateRetask:      time.Hour,
		PodUpdateNotice:      4 * time.Hour,
		ReviewRoundCap:       3,
		MaxFixAttempts:       3,
		ControllerLaunch:     ControllerLaunchOperator,
		// The worker stall-watch numbers this daemon's own environment sets, which noEnv sets
		// none of: the plugin's own defaults, which the backstop's floor is computed from.
		StallLimits: StallLimits{CallGrace: StallCallGrace, Step: StallStep},
	}
}

func TestLoadMinimalFileAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := defaultsFor(13370); !reflect.DeepEqual(cfg, want) {
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
// `instructions`, `envoy_token_file`, `nats_nkey_seed_file`, and `operator_token_file`
// (loadConfigFromFile, config.ts).
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
controller_wake_interval_seconds: 900
launch_failure_limit: 5
prompt_failure_limit: 6
prompt_retire_limit: 7
worker_wakes_per_minute: 12
worker_wake_burst: 3
operator_token_file: tokens/OPERATOR_TOKEN
envoy_url: http://127.0.0.1:19020
nats_urls: [nats://127.0.0.1:4222, nats://127.0.0.1:4223, nats://127.0.0.1:4222]
envoy_token_file: /run/legion/ENVOY_TOKEN
nats_nkey_seed_file: secrets/legion-pane.nk
`)
	dir := filepath.Dir(path)

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := defaultsFor(14000)
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
	want.ControllerWakeInterval = 15 * time.Minute
	want.LaunchFailureLimit = 5
	want.PromptFailureLimit = 6
	want.PromptRetireLimit = 7
	want.WorkerWakesPerMinute = 12
	want.WorkerWakeBurst = 3
	want.OperatorTokenFile = filepath.Join(dir, "tokens/OPERATOR_TOKEN")
	want.EnvoyURL = "http://127.0.0.1:19020"
	// A set, as the shipped `readStringArray` makes it (config.ts): the repeat is dropped, first
	// occurrence kept. The launch prefix is argv and keeps its repeats (readArgv, config.ts).
	want.NatsURLs = []string{"nats://127.0.0.1:4222", "nats://127.0.0.1:4223"}
	want.EnvoyTokenFile = "/run/legion/ENVOY_TOKEN"
	want.NatsNkeySeedFile = filepath.Join(dir, "secrets/legion-pane.nk")
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
}

// Stage 3's keys are settled here so later workflow tasks receive one fully validated daemon
// configuration rather than parsing their own YAML fragments.
func TestLoadReadsEveryStage3Key(t *testing.T) {
	path := writeConfigFile(t, strings.ReplaceAll(minimalFile, "dispatch_url: http://127.0.0.1:8080\n"+
		"dispatch_token_file: /var/run/legion/DISPATCH_TOKEN\n"+
		"projects:\n  DEMO: {}\n"+
		"gates:\n  design: root-issues\n"+
		"github_apps:\n  implement:\n    app_id: \"1\"\n    private_key: implement-test-key\n"+
		"  review:\n    app_id: \"2\"\n    private_key: review-test-key\n", `dispatch_url: https://dispatch.example
dispatch_token_file: tokens/DISPATCH_TOKEN
projects:
  DEMO: {}
  OTHER: { merge_queue_role: merge-queue }
  EMPTY: {}
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
production_check_hours: 48
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
		"DEMO":  {ProductionCheck: claim.RoleTester},
		"OTHER": {MergeQueueRole: "merge-queue", ProductionCheck: claim.RoleTester},
		"EMPTY": {ProductionCheck: claim.RoleTester},
	}) {
		t.Errorf("Projects = %#v", cfg.Projects)
	}
	if len(cfg.Gates.Design) != 0 {
		t.Errorf("Gates.Design = %v, want no step for 'off'", cfg.Gates.Design)
	}
	if got := cfg.GitHubApps.Implement; got.AppID != "11" || got.PrivateKey != "implement-key" || got.Installations["sjawhar"] != "101" {
		t.Errorf("implement app = %#v", got)
	}
	if cfg.GitHubApps.Review.AppID != "22" || cfg.GitHubApps.Review.PrivateKey != "review-key" {
		t.Errorf("review app = %#v", cfg.GitHubApps.Review)
	}
	if cfg.Linger != 96*time.Hour || cfg.ProductionCheckBound != 48*time.Hour || cfg.ReviewRoundCap != 5 || cfg.MaxFixAttempts != 4 {
		t.Errorf("Linger=%s ProductionCheckBound=%s ReviewRoundCap=%d MaxFixAttempts=%d, want 96h, 48h, 5, 4", cfg.Linger, cfg.ProductionCheckBound, cfg.ReviewRoundCap, cfg.MaxFixAttempts)
	}
}

// `gates.design` takes the steps a deployment gates, in the order it names them, and the two
// words it took before the list as the lists they mean. A key that names nothing, written as
// `design:` or as the empty string, is the unset key the loader read before the list, the single
// step spec. A value it does not take, and a list naming one step twice, are refused by a
// message naming what the key takes.
func TestLoadReadsTheDesignGateSteps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		want    []record.DesignStep
		refusal string
	}{
		{name: "a list", body: gatesListFile, want: []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria}},
		{name: "the empty list", body: gatesEmptyListFile, want: []record.DesignStep{}},
		{name: "no gates key", body: gatesAbsentFile, want: []record.DesignStep{record.DesignStepSpec}},
		{name: "the word root-issues", body: minimalFile, want: []record.DesignStep{record.DesignStepSpec}},
		{name: "the word off", body: strings.Replace(minimalFile, "design: root-issues", "design: off", 1), want: []record.DesignStep{}},
		{name: "a key that names nothing", body: strings.Replace(minimalFile, "design: root-issues", "design:", 1), want: []record.DesignStep{record.DesignStepSpec}},
		{name: "the empty word", body: strings.Replace(minimalFile, "design: root-issues", `design: ""`, 1), want: []record.DesignStep{record.DesignStepSpec}},
		{
			name:    "a word the key does not take",
			body:    gatesUnknownWordFile,
			refusal: "gates.design must be a list of 'decisions', 'criteria' or 'spec', or 'root-issues' for the single step 'spec', or 'off' for no step",
		},
		{name: "a step named twice", body: gatesRepeatedStepFile, refusal: "gates.design names 'spec' twice"},
		{name: "a step the key does not take", body: strings.Replace(minimalFile, "design: root-issues", "design: [spec, later]", 1),
			refusal: "gates.design[1] must be 'decisions', 'criteria' or 'spec'"},
		{name: "a value that is neither a string nor a list", body: strings.Replace(minimalFile, "design: root-issues", "design: {step: spec}", 1),
			refusal: "gates.design must be a list of 'decisions', 'criteria' or 'spec', or 'root-issues' for the single step 'spec', or 'off' for no step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, tc.body), noEnv)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("Load = %v, want the refusal %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.Gates.Steps(), tc.want) {
				t.Fatalf("Gates.Steps() = %v, want %v", cfg.Gates.Steps(), tc.want)
			}
		})
	}
}

// A Config nobody gave a Gates field gates the spec. The zero value is what every rig that
// builds a Config literal holds, and a gate that waves a root's spec through because a field
// was left out is a gate nobody notices is gone; the value a deployment writes for `off` is a
// list that is empty rather than absent, so the two are told apart by the field being nil.
// This asserts the zero value itself, so a field added to Gates later breaks a test here rather
// than the gate of a deployment.
func TestTheZeroValueOfGatesGatesTheSpec(t *testing.T) {
	var cfg Config
	if want := []record.DesignStep{record.DesignStepSpec}; !reflect.DeepEqual(cfg.Gates.Steps(), want) {
		t.Fatalf("the zero Config's Gates.Steps() = %v, want %v: an unset field is the gate the daemon held before a deployment could name a list",
			cfg.Gates.Steps(), want)
	}
	off := Config{Gates: Gates{Design: DesignGateOff.Steps()}}
	if steps := off.Gates.Steps(); len(steps) != 0 || steps == nil {
		t.Fatalf("Gates.Steps() under 'off' = %#v, want the non-nil empty list: 'off' gates no step and is not the unset field", steps)
	}
}

func TestLoadReadsProjectMergeRules(t *testing.T) {
	body := strings.Replace(minimalFile, "DEMO: {}",
		"DEMO: { merge: { by_hand: ['^migrations/versions/'] } }", 1)
	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load project merge rules: %v", err)
	}
	rules := cfg.Projects["DEMO"].Merge
	if !rules.MatchByHand([]string{"migrations/versions/0001.py"}) {
		t.Error("by_hand rule does not match its migration path")
	}
}

// A project's slack_channels are the Slack channel ids whose @-mentions reach its controller, kept
// in the order the file writes them, on a deployment whose daemon launches that controller
// enrolled with the secrets broker (slackChannelsFile).
func TestLoadReadsProjectSlackChannels(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, slackChannelsFile("[C00000000, G00000001]", true, true)), noEnv)
	if err != nil {
		t.Fatalf("Load project slack channels: %v", err)
	}
	if got, want := cfg.Projects["LEGSMOKE"].SlackChannels, []string{"C00000000", "G00000001"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SlackChannels = %q, want %q", got, want)
	}
}

// slackChannelsFile is kubernetesFile with the project's slack_channels set to channels, under
// `controller: daemon` when daemon is true and with an agent_secrets block when enrolled is.
func slackChannelsFile(channels string, daemon, enrolled bool) string {
	body := kubernetesWith("  LEGSMOKE: {}\n",
		"  LEGSMOKE: { slack_channels: "+channels+" }\n")
	if daemon {
		body = "controller: daemon\n" + body
	}
	if enrolled {
		body += "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile
	}
	return body
}

// Only the controller the daemon launches, enrolled with the secrets broker, is granted the Slack
// bot token (LEGION_SLACK_BOT_TOKEN, owned by the Legion daemon's service): the broker grants a
// service's secret to that service's own pods and no one else. So a project names Slack channels
// only where both hold, and anywhere else the loader refuses them, naming why, rather than start a
// controller that hears mentions it can never answer. An empty list names no channel.
func TestSlackChannelsNeedTheDaemonsControllerEnrolledWithTheBroker(t *testing.T) {
	const why = "needs controller: daemon and runtime.kubernetes.agent_secrets: Slack replies need the daemon-launched controller enrolled with the secrets broker, which grants the Slack bot token (LEGION_SLACK_BOT_TOKEN) only to the Legion daemon service's own pods"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"under tmux", strings.Replace(minimalFile, "DEMO: {}", "DEMO: { slack_channels: [C00000000] }", 1), "projects.DEMO.slack_channels " + why},
		{"the operator's controller", slackChannelsFile("[C00000000]", false, true), "projects.LEGSMOKE.slack_channels " + why},
		{"no broker", slackChannelsFile("[C00000000]", true, false), "projects.LEGSMOKE.slack_channels " + why},
		{"neither", slackChannelsFile("[C00000000]", false, false), "projects.LEGSMOKE.slack_channels " + why},
		{"an empty list", slackChannelsFile("[]", false, false), ""},
		{"both", slackChannelsFile("[C00000000]", true, true), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadForValidation: %v, want it to load", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}

// A channel belongs to one project: the loader refuses a channel named twice, in one project or in
// two, and an id that is not a Slack channel's, which would make a subject no mention is published
// under.
func TestLoadRefusesProjectSlackChannels(t *testing.T) {
	for _, tc := range []struct{ name, projects, want string }{
		{
			name:     "not a list",
			projects: "projects:\n  DEMO: { slack_channels: C00000000 }\n",
			want:     "projects.DEMO.slack_channels must be an array of non-empty strings",
		},
		{
			name:     "not a channel id",
			projects: "projects:\n  DEMO: { slack_channels: [general] }\n",
			want:     `projects.DEMO.slack_channels entry "general" is not a Slack channel id (C or G, then capital letters and digits, such as C0123ABCD)`,
		},
		{
			name:     "a direct message",
			projects: "projects:\n  DEMO: { slack_channels: [D00000000] }\n",
			want:     `projects.DEMO.slack_channels entry "D00000000" is not a Slack channel id (C or G, then capital letters and digits, such as C0123ABCD)`,
		},
		{
			name:     "a subject wildcard",
			projects: "projects:\n  DEMO: { slack_channels: ['C*'] }\n",
			want:     `projects.DEMO.slack_channels entry "C*" is not a Slack channel id (C or G, then capital letters and digits, such as C0123ABCD)`,
		},
		{
			name:     "twice in one project",
			projects: "projects:\n  DEMO: { slack_channels: [C00000000, C00000000] }\n",
			want:     `projects.DEMO.slack_channels names "C00000000" twice`,
		},
		{
			name: "in two projects",
			projects: "projects:\n  DEMO: { slack_channels: [C00000000] }\n" +
				"  OTHER: { slack_channels: [C00000001, C00000000] }\n",
			want: `projects.OTHER.slack_channels names "C00000000", which projects.DEMO.slack_channels already names: a channel's mentions reach one project's controller`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", tc.projects, 1)
			if body == minimalFile {
				t.Fatal("the fixture's projects block was not replaced")
			}
			_, err := Load(writeConfigFile(t, body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// `deploys` is still a known key of a project and nothing is read from it: a file that sets it
// loads, and every project that names it raises one warning, in file order, naming the key,
// saying the daemon no longer wakes a production check from a deploy run and asking for the
// key's removal. INFRA's own deployment file still writes it, and a reader that refused it
// would make this release that deployment's outage, on a schedule this repository does not
// control (docs/solutions/envoy/a-setting-another-repositorys-deployment-sets-is-removed-in-two-releases-pr2674.md).
func TestLoadWarnsOncePerProjectThatStillSetsDeploys(t *testing.T) {
	body := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n"+
		"  DEMO:\n    deploys:\n"+
		"      - {repo: acme/infra, workflow: .github/workflows/envoy.yaml, step: Apply}\n"+
		"  OTHER:\n    deploys: []\n"+
		"  QUIET: {}\n", 1)
	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load a file that still sets deploys: %v", err)
	}
	want := []string{
		"projects.DEMO.deploys is set and ignored: the daemon no longer wakes a production check from a deploy run, so the workflows this key names configure nothing; remove it from the deployment's legion.yaml",
		"projects.OTHER.deploys is set and ignored: the daemon no longer wakes a production check from a deploy run, so the workflows this key names configure nothing; remove it from the deployment's legion.yaml",
	}
	if !reflect.DeepEqual(cfg.Warnings, want) {
		t.Errorf("Warnings = %#v, want one line for each of the two projects that set the key: %#v", cfg.Warnings, want)
	}
	if cfg, err := Load(writeConfigFile(t, minimalFile), noEnv); err != nil || cfg.Warnings != nil {
		t.Errorf("Load the file that sets no deploys = %#v, %v; want no warning", cfg.Warnings, err)
	}
}

// Nothing is read from `deploys`, so no value of it is refused any more: what the deleted reader
// took for a list of entries, and what it refused as a workflow no run could name, each load and
// warn alike.
func TestLoadReadsNothingFromProjectDeploys(t *testing.T) {
	for _, tc := range []struct{ name, deploys string }{
		{"one mapping where the reader wanted a list", "{repo: sjawhar/legion, workflow: .github/workflows/envoy.yaml, step: Apply}"},
		{"an entry with no step and a workflow no run names", "[{repo: sjawhar/legion, workflow: envoy.yaml}]"},
		{"an entry that is no mapping", "[sjawhar/legion]"},
		{"the key with nothing under it", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n  DEMO:\n    deploys: "+tc.deploys+"\n", 1)
			cfg, err := Load(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Warnings) != 1 {
				t.Errorf("Warnings = %#v, want the one line DEMO's key raises", cfg.Warnings)
			}
		})
	}
}

// A project's commit_issue_trailer_repos are the repositories whose commits a pane stamps with
// the issue key, each a parsed repository, kept in the file's order. A project that names none,
// or names an empty list, has none. The key only selects among the project's repositories, so
// two projects may each name one side of a configured rename, which is two projects' own
// selections and not one list naming one repository twice.
func TestLoadReadsProjectCommitIssueTrailerRepos(t *testing.T) {
	body := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n"+
		"  DEMO:\n    commit_issue_trailer_repos: [acme/widgets, acme/infra]\n"+
		"  ONE: { commit_issue_trailer_repos: [acme/gadgets] }\n"+
		"  OTHER: {}\n"+
		"  EMPTY: { commit_issue_trailer_repos: [] }\n", 1)
	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load project commit_issue_trailer_repos: %v", err)
	}
	want := []ghrepo.Repository{ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("acme/infra")}
	if got := cfg.Projects["DEMO"].CommitIssueTrailerRepos; !reflect.DeepEqual(got, want) {
		t.Errorf("DEMO commit_issue_trailer_repos = %#v, want %#v", got, want)
	}
	if got := cfg.Projects["ONE"].CommitIssueTrailerRepos; !reflect.DeepEqual(got, []ghrepo.Repository{ghrepo.MustParse("acme/gadgets")}) {
		t.Errorf("ONE commit_issue_trailer_repos = %#v, want acme/gadgets alone", got)
	}
	for _, key := range []string{"OTHER", "EMPTY"} {
		if got := cfg.Projects[key].CommitIssueTrailerRepos; got != nil {
			t.Errorf("%s commit_issue_trailer_repos = %#v, want nil", key, got)
		}
	}

	pair := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n",
		"projects:\n  DEMO: { commit_issue_trailer_repos: [acme/widgets] }\n"+
			"  OTHER: { commit_issue_trailer_repos: [acme/gadgets] }\n", 1) +
		"repository_renames: [{from: acme/widgets, to: acme/gadgets}]\n"
	if _, err := Load(writeConfigFile(t, pair), noEnv); err != nil {
		t.Errorf("Load the pair's two names, one in each project: %v", err)
	}
}

// The loader refuses a commit_issue_trailer_repos entry it could stamp no commit for, or that
// names one repository the list already names: a value that is no owner and name, one repository
// twice, a repeat differing only in case — GitHub compares a name without case — and a repeat a
// configured repository_renames pair makes one repository, which readCommitIssueTrailerRepos
// cannot see because a project is read before the renames settle (checkCommitIssueTrailerRenames).
func TestLoadRefusesProjectCommitIssueTrailerRepos(t *testing.T) {
	for _, tc := range []struct{ name, repos, renames, want string }{
		{
			name:  "a value that is no owner and name",
			repos: "[widgets]",
			want:  `projects.DEMO.commit_issue_trailer_repos[0] must be "owner/name" (got "widgets")`,
		},
		{
			name:  "one repository twice",
			repos: "[acme/widgets, acme/widgets]",
			want:  "projects.DEMO.commit_issue_trailer_repos names acme/widgets twice",
		},
		{
			name:  "one repository twice in two cases",
			repos: "[acme/widgets, Acme/Widgets]",
			want:  "projects.DEMO.commit_issue_trailer_repos names Acme/Widgets twice",
		},
		{
			name:    "the two names of a configured rename",
			repos:   "[acme/widgets, Acme/Gadgets]",
			renames: "repository_renames: [{from: acme/widgets, to: acme/gadgets}]\n",
			want: "projects.DEMO.commit_issue_trailer_repos[0] (acme/widgets) and projects.DEMO.commit_issue_trailer_repos[1] (Acme/Gadgets) name one repository: " +
				"repository_renames makes acme/widgets and Acme/Gadgets one repository, so the list names it twice; name it once",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n  DEMO:\n    commit_issue_trailer_repos: "+tc.repos+"\n", 1)
			if body == minimalFile {
				t.Fatal("the fixture's projects block was not replaced")
			}
			_, err := Load(writeConfigFile(t, body+tc.renames), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
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
			name: "dispatch_url has a scheme that is not http or https",
			body: strings.Replace(minimalFile, "dispatch_url: http://127.0.0.1:8080\n", "dispatch_url: htp://127.0.0.1:8080\n", 1),
			want: `dispatch_url must be http or https, not "htp"`,
		},
		{
			name: "dispatch_url is the /mcp endpoint",
			body: strings.Replace(minimalFile, "dispatch_url: http://127.0.0.1:8080\n", "dispatch_url: http://127.0.0.1:8080/mcp/\n", 1),
			want: "dispatch_url must be the dispatch service base URL, not the /mcp endpoint",
		},
		// Boot needs each of these for a workflow, so the loader refuses the file, in the words of
		// its own refusal: `legion start --check-config` never passes a file `legion start` refuses.
		{
			name: "dispatch_url without dispatch_token_file",
			body: strings.Replace(minimalFile, "dispatch_token_file: /var/run/legion/DISPATCH_TOKEN\n", "", 1),
			want: "dispatch_token_file is required when dispatch_url is configured",
		},
		{
			name: "dispatch_url without nats_urls",
			body: strings.Replace(minimalFile, "nats_urls: [nats://127.0.0.1:4222]\n", "", 1),
			want: "nats_urls is required when dispatch_url is configured: the workflow's intake reads Envoy's notification stream",
		},
		{
			name: "projects without the daemon's own project",
			body: strings.Replace(minimalFile, "project: DEMO\n", "project: OTHER\n", 1),
			want: "projects must configure OTHER, the daemon's own project",
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
			body: minimalFile + "projects: {demo: {}}\n",
			want: `projects key "demo" must match ^[A-Z][A-Z0-9]*$`,
		},
		{
			// The project's repositories are Dispatch's mapping, so a file that still names one is
			// refused, naming where they are set.
			name: "a project names its repository",
			body: strings.Replace(minimalFile, "DEMO: {}", "DEMO: { repo: acme/widgets }", 1),
			want: `Unknown key "projects.DEMO.repo": the project's repositories are Dispatch's mapping (Settings → Repositories → Projects)`,
		},
		{
			name: "review_workflows is no longer a key",
			body: strings.Replace(minimalFile, "DEMO: {}", "DEMO: { review_workflows: [.github/workflows/review.yml] }", 1),
			want: `Unknown key "projects.DEMO.review_workflows"`,
		},
		{
			name: "review_workflows is no longer a key, even empty",
			body: strings.Replace(minimalFile, "DEMO: {}", "DEMO: { review_workflows: [] }", 1),
			want: `Unknown key "projects.DEMO.review_workflows"`,
		},
		{
			name: "gates is not a mapping",
			body: minimalFile + "gates: root-issues\n",
			want: "gates must be a mapping",
		},
		{
			name: "gates design is invalid",
			body: minimalFile + "gates: {design: later}\n",
			want: "gates.design must be a list of 'decisions', 'criteria' or 'spec', or 'root-issues' for the single step 'spec', or 'off' for no step",
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
			body: "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nprojects: {DEMO: {}}\n",
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
			want: "github_apps.implement requires exactly one of private_key, private_key_command, private_key_secret, or token_command",
		},
		{
			name: "github app has two private key sources",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\", private_key: key, private_key_command: \"printf key\"}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement requires exactly one of private_key, private_key_command, private_key_secret, or token_command",
		},
		{
			name: "github app secret is not a single key name",
			body: minimalFile + "github_apps: {implement: {app_id: \"1\", private_key_secret: \"secrets get KEY\"}, review: {app_id: \"2\", private_key: key}}\n",
			want: "github_apps.implement.private_key_secret must be a single secretsd key name (no whitespace)",
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

// `worker_stream_port` defaults to one past `port` (resolveDaemonConfig, config.ts), which is why
// it has to move with a file that moves `port`.
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

func TestEndpointURLKeepsAPath(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"envoy_url", "https://envoy.example/tenant"},
		{"nats_urls", "nats://nats.example:4222/tenant"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			got, err := endpointURL(tc.value, tc.key)
			if err != nil {
				t.Fatalf("endpointURL: %v", err)
			}
			if got.Path != "/tenant" {
				t.Errorf("path = %q, want /tenant", got.Path)
			}
		})
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
			name: "nats_urls entry with a query string",
			body: strings.Replace(minimalFile, "nats_urls: [nats://127.0.0.1:4222]\n",
				`nats_urls: ["nats://127.0.0.1:4222?token=token"]`+"\n", 1),
			want: `nats_urls entry "nats://127.0.0.1:4222?token=token" must not include a query string or fragment; use URL userinfo or a secret for credentials`,
		},
		{
			name: "nats_urls entry with a fragment",
			body: strings.Replace(minimalFile, "nats_urls: [nats://127.0.0.1:4222]\n",
				`nats_urls: ["nats://127.0.0.1:4222#fragment"]`+"\n", 1),
			want: `nats_urls entry "nats://127.0.0.1:4222#fragment" must not include a query string or fragment; use URL userinfo or a secret for credentials`,
		},
		{
			name: "envoy_url with a query string",
			body: minimalFile + "envoy_url: https://envoy.example?access_token=token\n",
			want: "envoy_url must not include a query string or fragment; use URL userinfo or a secret for credentials",
		},
		{
			name: "envoy_url with a fragment",
			body: minimalFile + "envoy_url: https://envoy.example#fragment\n",
			want: "envoy_url must not include a query string or fragment; use URL userinfo or a secret for credentials",
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
			name: "controller_wake_interval_seconds zero",
			body: minimalFile + "controller_wake_interval_seconds: 0\n",
			want: "controller_wake_interval_seconds must be a positive integer",
		},
		{
			name: "controller_wake_interval_seconds past the timer bound",
			body: minimalFile + "controller_wake_interval_seconds: 2147484\n",
			want: "controller_wake_interval_seconds must be at most 2147483",
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
			name: "nats_nkey_seed_file blank",
			body: minimalFile + "nats_nkey_seed_file: \"\"\n",
			want: "nats_nkey_seed_file must not be empty",
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
			want: "provider_keys must be a mapping of the variable OMP reads to the key that holds it (a secretsd key, or file:/abs/path under runtime tmux; under runtime kubernetes, a key of the providers Secret), e.g. {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}",
		},
		{
			name: "provider_keys a scalar",
			body: minimalFile + "provider_keys: GEMINI_API_KEY_TESTS\n",
			want: "provider_keys must be a mapping of the variable OMP reads to the key that holds it (a secretsd key, or file:/abs/path under runtime tmux; under runtime kubernetes, a key of the providers Secret), e.g. {GEMINI_API_KEY: GEMINI_API_KEY_TESTS}",
		},
		{
			name: "provider_keys with a variable that is not a variable name",
			body: minimalFile + "provider_keys: {1PASSWORD: ONEPASSWORD_TESTS}\n",
			want: `provider_keys key "1PASSWORD" (the variable OMP reads) must be an environment variable name (letters, digits, and underscores, not starting with a digit)`,
		},
		{
			name: "provider_keys with a command where the secretsd key goes",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: \"secrets get GEMINI_API_KEY_TESTS\"}\n",
			want: `provider_keys value "secrets get GEMINI_API_KEY_TESTS" for GEMINI_API_KEY (the key that holds it) must be an environment variable name (letters, digits, and underscores, not starting with a digit), or file: and an absolute path`,
		},
		{
			name: "provider_keys naming a file by a relative path",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: \"file:keys/gemini\"}\n",
			want: `provider_keys value "file:keys/gemini" for GEMINI_API_KEY names a file by a path that is not absolute; write file:/abs/path`,
		},
		{
			name: "provider_keys with a secretsd key that is not a string",
			body: minimalFile + "provider_keys: {GEMINI_API_KEY: [GEMINI_API_KEY_TESTS]}\n",
			want: `provider_keys value for GEMINI_API_KEY (the key that holds it) must be a string`,
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
			want: "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes or tmux",
		},
		{
			name: "a runtime mapping with another single key",
			body: minimalFile + "runtime: {podman: {}}\n",
			want: "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes or tmux",
		},
		{
			name: "a runtime.tmux block that is not a mapping",
			body: minimalFile + "runtime: {tmux: [pane_env]}\n",
			want: "runtime.tmux must be a mapping",
		},
		{
			name: "an unknown member of runtime.tmux",
			body: minimalFile + "runtime: {tmux: {session_store: pvc}}\n",
			want: "unknown key runtime.tmux.session_store",
		},
		{
			name: "runtime.tmux.pane_env a list",
			body: minimalFile + "runtime: {tmux: {pane_env: [ELAN_HOME]}}\n",
			want: "runtime.tmux.pane_env must be a mapping of variable name to value",
		},
		{
			name: "runtime.tmux.pane_env naming something that is not a variable",
			body: minimalFile + "runtime: {tmux: {pane_env: {1ELAN: /opt/elan}}}\n",
			want: `runtime.tmux.pane_env key "1ELAN" must be an environment variable name (letters, digits, and underscores, not starting with a digit)`,
		},
		{
			// The runtime composes every pane's PATH: the daemon's own behind its launcher directory.
			name: "runtime.tmux.pane_env naming PATH",
			body: minimalFile + "runtime: {tmux: {pane_env: {PATH: /usr/bin}}}\n",
			want: "runtime.tmux.pane_env names PATH, which the runtime composes for every pane (the daemon's own PATH behind its launcher directory)",
		},
		{
			// A value on tmux's command line is visible to every process on the host; a secret
			// reaches a pane as a file.
			name: "runtime.tmux.pane_env naming a credential",
			body: minimalFile + "runtime: {tmux: {pane_env: {LEDGER_TOKEN: hunter2}}}\n",
			want: "runtime.tmux.pane_env names LEDGER_TOKEN, a credential-shaped name: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line",
		},
		{
			name: "runtime.tmux.pane_env naming one variable twice",
			body: minimalFile + "runtime: {tmux: {pane_env: {ELAN_HOME: /opt/elan, ELAN_HOME: /srv/elan}}}\n",
			want: "runtime.tmux.pane_env names ELAN_HOME twice",
		},
		{
			name: "runtime.tmux.pane_env with a value that is not a string",
			body: minimalFile + "runtime: {tmux: {pane_env: {ELAN_HOME: [/opt/elan]}}}\n",
			want: "runtime.tmux.pane_env.ELAN_HOME must be a string",
		},
		{
			name: "runtime.tmux.pane_env with a null value",
			body: minimalFile + "runtime: {tmux: {pane_env: {ELAN_HOME: null}}}\n",
			want: "runtime.tmux.pane_env.ELAN_HOME must be a string",
		},
		{
			// PI_CONFIG_FILES separates its overlays with `:`, so it could not name this path.
			name: "runtime.tmux.pi_config_files naming a path holding the separator",
			body: minimalFile + "runtime: {tmux: {pi_config_files: [/etc/legion:operator/overlay.yml]}}\n",
			want: "runtime.tmux.pi_config_files entry \"/etc/legion:operator/overlay.yml\" holds `:`, which separates the overlays PI_CONFIG_FILES names",
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

// A rate of 0 turns the pacing off, so it is the one wake pacing value the loader takes at zero:
// the burst is how many wakes may leave at once, and a burst of none would release nothing.
func TestLoadAcceptsAWakeRateOfZero(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"worker_wakes_per_minute: 0\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerWakesPerMinute != 0 {
		t.Errorf("WorkerWakesPerMinute = %d, want 0: the file turns the pacing off", cfg.WorkerWakesPerMinute)
	}
}

// Every refused wake pacing value names its own key, which is what an operator reading
// `legion start --check-config` needs: which of the two the daemon would not take. The assertion
// is on the key's name rather than the sentence, so rewording a refusal is no test change.
func TestLoadRefusesAWakePacingValueNamingItsKey(t *testing.T) {
	for _, tc := range []struct{ name, line, key string }{
		{"a rate below zero", "worker_wakes_per_minute: -1", "worker_wakes_per_minute"},
		{"a rate that is not a whole number", "worker_wakes_per_minute: 2.5", "worker_wakes_per_minute"},
		{"a burst of zero", "worker_wake_burst: 0", "worker_wake_burst"},
		{"a burst below zero", "worker_wake_burst: -1", "worker_wake_burst"},
		{"a burst that is not a whole number", "worker_wake_burst: 2.5", "worker_wake_burst"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.line+"\n"), noEnv)
			if err == nil {
				t.Fatalf("Load succeeded, want a refusal naming %s", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load error = %q, want it to name %s", err.Error(), tc.key)
			}
		})
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

// Every top-level key of the earlier schema (38 keys, nats_daemon_nkey_seed_file among them since
// #1494) plus the new postgres_dsn and the wake pacing's two keys, and the class it is in. No
// shipped key may fall through to the typo refusal, and none is accepted without being modelled.
func TestLoadClassifiesEveryShippedKey(t *testing.T) {
	const (
		modelled  = "modelled"
		tossed    = "tossed"
		migration = "migration-only"
		// kubernetesOnly is a modelled key the tmux file this test loads refuses by name.
		kubernetesOnly = "kubernetes-only"
	)
	for _, tc := range []struct {
		key   string
		line  string
		class string
		want  string
		// wantPrefix, where set, replaces want: the refusal must start with it, and the reason that
		// follows is the message's own wording.
		wantPrefix string
	}{
		{key: "project", class: modelled},
		{key: "state_dir", class: modelled},
		{key: "postgres_dsn", class: modelled},
		{key: "port", line: "port: 13370", class: modelled},
		{key: "bind", line: "bind: 127.0.0.1", class: modelled},
		{key: "advertise_host", line: "advertise_host: legion-daemon.legion.svc", class: kubernetesOnly, want: "advertise_host is not used when runtime is tmux: every pane dials the daemon's own unix socket; remove advertise_host"},
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
		{key: "nats_nkey_seed_file", line: "nats_nkey_seed_file: /var/run/legion/NATS_NKEY_SEED", class: modelled},
		{key: "nats_daemon_nkey_seed_file", line: "nats_daemon_nkey_seed_file: /var/run/legion/NATS_DAEMON_NKEY_SEED", class: modelled},
		{key: "nats_urls", line: "nats_urls: [nats://127.0.0.1:4222]", class: modelled},
		{key: "operator_token_file", line: "operator_token_file: /var/run/legion/OPERATOR_TOKEN", class: modelled},
		{key: "worker_wakes_per_minute", line: "worker_wakes_per_minute: 8", class: modelled},
		{key: "worker_wake_burst", line: "worker_wake_burst: 1", class: modelled},

		{key: "dispatch_url", line: "dispatch_url: https://dispatch.example", class: modelled},
		{key: "dispatch_token_file", line: "dispatch_token_file: /var/run/legion/DISPATCH_TOKEN", class: modelled},
		{key: "projects", line: "projects: {DEMO: {}}", class: modelled},
		{key: "gates", line: "gates: {design: off}", class: modelled},
		{key: "github_apps", line: "github_apps: {implement: {app_id: \"1\", private_key: key}, review: {app_id: \"2\", private_key: key}}", class: modelled},
		{key: "linger_hours", line: "linger_hours: 72", class: modelled},
		{key: "production_check_hours", line: "production_check_hours: 336", class: modelled},
		{key: "review_round_cap", line: "review_round_cap: 3", class: modelled},
		{key: "max_fix_attempts", line: "max_fix_attempts: 3", class: modelled},
		{key: "capabilities", line: `capabilities: {decided: {secrets: "dispatch://LEGION-205 enrolls pods later"}}`, class: modelled},

		{
			key: "worker_cap", line: "worker_cap: 10", class: tossed,
			want: "unknown key worker_cap: the running-worker cap no longer exists (LEGION-208 Requirement 8)",
		},
		{
			key: "worker_idle_retire_seconds", line: "worker_idle_retire_seconds: 600", class: tossed,
			want: "unknown key worker_idle_retire_seconds: an idle window retires no worker: it releases the worker's issue pod, and the key for it is runtime.kubernetes.idle_release (idle_minutes, backstop_minutes), which suspends every claim of the released pod with the source released and brings them back on the next wake (INFRA-945)",
		},
		{
			key: "resync_interval_seconds", line: "resync_interval_seconds: 600", class: tossed,
			want: "unknown key resync_interval_seconds: the mirror of Dispatch and GitHub as truth, and resync's drift healing, no longer exist (LEGION-208 Design, \"Ported, and tossed\")",
		},
		{
			key: "max_recursion_depth", line: "max_recursion_depth: 8", class: tossed,
			want: "unknown key max_recursion_depth: an architect spawns no worker and no sub-architect (the daemon starts every phase, and only the operator starts any other claim), so there is no spawn recursion to bound",
		},

		{key: "dispatch_mcp_url", line: "dispatch_mcp_url: https://dispatch.example/mcp", class: migration, want: wantDispatchMcpURLMessage},
		{key: "dispatch_project", line: "dispatch_project: DEMO", class: migration, want: wantDispatchProjectMessage},
		{key: "board_project_ids", line: "board_project_ids: [1]", class: migration, want: wantBoardProjectIDsMessage},
		{key: "repos", line: "repos: {DEMO: acme/widgets}", class: migration, want: wantReposMessage},
		{key: "app_logins", line: "app_logins: [legion-implementer]", class: migration, want: wantAppLoginsMessage},
		{key: "worker_budget", line: "worker_budget: 6", class: migration, want: wantWorkerBudgetMessage},
	} {
		t.Run(tc.key, func(t *testing.T) {
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
			default:
				if err == nil {
					t.Fatalf("Load succeeded, want error %q", tc.want+tc.wantPrefix)
				}
				if tc.wantPrefix != "" {
					if !strings.HasPrefix(err.Error(), tc.wantPrefix) {
						t.Errorf("Load error = %q, want a refusal starting %q", err.Error(), tc.wantPrefix)
					}
				} else if err.Error() != tc.want {
					t.Errorf("Load error = %q, want %q", err.Error(), tc.want)
				}
			}
		})
	}
}

// advertise_host is the host of every pod's `--connect tcp://<host>:<port>`, which the shim reads as
// a URL (shim.ParseAddress, internal/shim/config.go) and refuses with anything beyond a host and a
// port, and the daemon adds the port itself (shimAddress, internal/daemon/daemon.go). So the loader
// refuses, by its exact message, every value that is not an IP address or a DNS name: under
// runtime: kubernetes, the one runtime that reads advertise_host, so no other refusal can stand in
// for this one, rather than `legion start --check-config` passing a file no pod boots on.
func TestAdvertiseHostMustBeABareHost(t *testing.T) {
	const refused = "advertise_host must be an IP address or a DNS name, with no scheme, port, path or brackets: the daemon adds worker_stream_port itself"
	for _, tc := range []struct{ name, yaml, want string }{
		{"a scheme", "http://legion-daemon.legion.svc", refused + " (http://legion-daemon.legion.svc)"},
		{"a port", "legion-daemon.legion.svc:13371", refused + " (legion-daemon.legion.svc:13371)"},
		{"brackets", `"[::1]"`, refused + " ([::1])"},
		{"an underscore", "legion_daemon.legion.svc", refused + " (legion_daemon.legion.svc)"},
		{"a path", "legion-daemon.legion.svc/x", refused + " (legion-daemon.legion.svc/x)"},
		{"a trailing slash", "legion-daemon.legion.svc/", refused + " (legion-daemon.legion.svc/)"},
		{"a user", "legion@legion-daemon.legion.svc", refused + " (legion@legion-daemon.legion.svc)"},
		{"a query", `"legion-daemon.legion.svc?x"`, refused + " (legion-daemon.legion.svc?x)"},
		{"a fragment", `"legion-daemon.legion.svc#x"`, refused + " (legion-daemon.legion.svc#x)"},
		{"a space", `"legion daemon"`, refused + " (legion daemon)"},
		{"a space inside a DNS name", `"legion daemon.legion.svc"`, refused + " (legion daemon.legion.svc)"},
		{"a zoned IPv6 address", `"fe80::1%eth0"`, refused + " (fe80::1%eth0)"},
		{"a dotted capital I", "nas.\u0130nternal.example", `advertise_host must be an IP address or an ASCII DNS name; got "nas.\u0130nternal.example"`},
		{"a Kelvin sign", "\u212Aube.example", `advertise_host must be an IP address or an ASCII DNS name; got "\u212aube.example"`},
		{"blank", `""`, "advertise_host must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"advertise_host: "+tc.yaml+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A bare host name and a bare IP address, IPv6 included, both pass under runtime: kubernetes, the
// one runtime that reads advertise_host.
func TestAdvertiseHostAcceptsABareHostOrIPAddress(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a DNS name", "legion-daemon.legion.svc"},
		{"a mixed-case DNS name", "Legion-Daemon.Legion.SVC"},
		{"an IPv4 address", "192.0.2.10"},
		{"a bare IPv6 address", "2001:db8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"advertise_host: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AdvertiseHost != tc.value {
				t.Errorf("AdvertiseHost = %q, want %q", cfg.AdvertiseHost, tc.value)
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

// `capabilities.decided` records the operator's decision on a deployment capability, by name, with
// the reason the daemon's report then shows in the gap's place (capabilities.Deployment). A name
// the daemon does not measure from its deployment is refused naming the ones it does, and a blank
// reason is refused: it would record nothing. Nothing is refused for being decided while present —
// the report calls such a decision moot.
func TestLoadReadsCapabilityDecisions(t *testing.T) {
	t.Run("every decidable name, with its reason", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile+`capabilities:
  decided:
    secrets: "dispatch://LEGION-205 enrolls pods later"
    model-fallback: one model route, nothing to fall back to
    resource-limits: "one tree per node; the pool's floor sizes it"
    pool-capacity: "the pool is sized for the trees; the probe waits for room"
`), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := map[capabilities.Name]string{
			capabilities.Secrets:        "dispatch://LEGION-205 enrolls pods later",
			capabilities.ModelFallback:  "one model route, nothing to fall back to",
			capabilities.ResourceLimits: "one tree per node; the pool's floor sizes it",
			capabilities.PoolCapacity:   "the pool is sized for the trees; the probe waits for room",
		}
		if !reflect.DeepEqual(cfg.Capabilities.Decided, want) {
			t.Errorf("Capabilities.Decided = %#v, want %#v", cfg.Capabilities.Decided, want)
		}
	})
	t.Run("no block, and an empty one", func(t *testing.T) {
		for _, body := range []string{minimalFile, minimalFile + "capabilities:\n", minimalFile + "capabilities: {}\n", minimalFile + "capabilities: {decided: {}}\n"} {
			cfg, err := Load(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Capabilities.Decided) != 0 {
				t.Errorf("Capabilities.Decided = %#v, want none", cfg.Capabilities.Decided)
			}
		}
	})
	for _, tc := range []struct{ name, line, want string }{
		{"a name that is no deployment capability", `capabilities: {decided: {nonsense: "because"}}`, "unknown key capabilities.decided.nonsense: a decision may name secrets, model-fallback, resource-limits or pool-capacity"},
		{"an image row, which no decision covers", `capabilities: {decided: {browser: "no Chromium"}}`, "unknown key capabilities.decided.browser: a decision may name secrets, model-fallback, resource-limits or pool-capacity"},
		{"a blank reason", `capabilities: {decided: {secrets: "  "}}`, "capabilities.decided.secrets must not be empty"},
		{"a null reason", `capabilities: {decided: {secrets: }}`, "capabilities.decided.secrets must not be empty"},
		{"a reason that is not a string", `capabilities: {decided: {secrets: [a, b]}}`, "capabilities.decided.secrets must be a string"},
		{"a name twice", "capabilities:\n  decided:\n    secrets: a\n    secrets: b", "capabilities.decided names secrets twice"},
		{"decided not a mapping", `capabilities: {decided: secrets}`, "capabilities.decided must be a mapping of capability to reason"},
		{"a member beside decided", `capabilities: {present: {secrets: yes}}`, "unknown key capabilities.present"},
		{"the block not a mapping", `capabilities: [secrets]`, "capabilities must be a mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.line+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
