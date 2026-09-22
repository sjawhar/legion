package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The smallest file that loads: the three keys with no default.
const minimalFile = `project: demo
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
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
	path := writeConfigFile(t, "project: demo\nstate_dir: state\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n")

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

func TestLoadMinimalFileAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Project:      "demo",
		Port:         13370,
		Bind:         "127.0.0.1",
		PostgresDSN:  "postgres://legion@127.0.0.1:5432/legion",
		StateDir:     "/var/lib/legion",
		Runtime:      Runtime{Name: "tmux"},
		AdmissionCap: 4,
	}
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
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

	fromEnv, err := Load(writeConfigFile(t, "project: demo\nstate_dir: /var/lib/legion\n"), env)
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
	captureLog(t)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "absent", body: minimalFile, want: "tmux"},
		{name: "the tmux scalar", body: minimalFile + "runtime: tmux\n", want: "tmux"},
		{name: "the kubernetes scalar", body: minimalFile + "runtime: kubernetes\n", want: "kubernetes"},
		{
			name: "the nested kubernetes block",
			body: minimalFile + "runtime:\n  kubernetes:\n    namespace: legion\n    image: ghcr.io/sjawhar/legion-worker@sha256:0\n",
			want: "kubernetes",
		},
		{
			name: "a nested block Stage 4 has not modelled yet",
			body: minimalFile + "runtime:\n  kubernetes:\n    namespace: legion\n    scheduling: {priority_class: legion}\n",
			want: "kubernetes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, tc.body), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Runtime.Name != tc.want {
				t.Errorf("Runtime.Name = %q, want %q", cfg.Runtime.Name, tc.want)
			}
		})
	}
}

func TestLoadIgnoresEveryMemberOfTheKubernetesBlock(t *testing.T) {
	out := captureLog(t)
	body := minimalFile + "runtime:\n  kubernetes:\n    namespace: legion\n    image: ghcr.io/sjawhar/legion-worker@sha256:0\n"

	if _, err := Load(writeConfigFile(t, body), noEnv); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !strings.Contains(out.String(), ignoredLine("runtime.kubernetes", 4)) {
		t.Errorf("log %q does not name runtime.kubernetes at stage 4", out.String())
	}
	for _, member := range []string{"runtime.kubernetes.namespace", "runtime.kubernetes.image"} {
		if strings.Contains(out.String(), member) {
			t.Errorf("log names %s: the block is ignored whole at Stage 1, never walked", member)
		}
	}
}

// The overlays the in-cluster daemon ships with must load unchanged: they are the reason the Go
// loader keeps the shipped key names.
func TestLoadReadsTheShippedOverlays(t *testing.T) {
	for _, overlay := range []string{
		"../../../../deploy/kubernetes/daemon/base/legion.yaml",
		"../../../../deploy/kubernetes/daemon/overlays/kind/legion.yaml",
	} {
		t.Run(filepath.Base(filepath.Dir(overlay)), func(t *testing.T) {
			out := captureLog(t)
			env := envMap(map[string]string{"LEGION_POSTGRES_DSN": "postgres://legion@db:5432/legion"})

			cfg, err := Load(overlay, env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			want := Config{
				Project:      "demo",
				Port:         13370,
				Bind:         "0.0.0.0",
				PostgresDSN:  "postgres://legion@db:5432/legion",
				StateDir:     "/var/lib/legion",
				Runtime:      Runtime{Name: "kubernetes"},
				AdmissionCap: 4,
			}
			if cfg != want {
				t.Errorf("Load = %+v, want %+v", cfg, want)
			}

			// Every key the overlay carries that a later stage models, with that stage.
			for key, stage := range map[string]int{
				"daemon_url":          2,
				"instructions":        2,
				"worker_stream_port":  2,
				"envoy_url":           3,
				"envoy_token_file":    3,
				"nats_urls":           3,
				"dispatch_url":        3,
				"projects":            3,
				"gates":               3,
				"github_apps":         3,
				"operator_token_file": 4,
				"runtime.kubernetes":  4,
			} {
				if !strings.Contains(out.String(), ignoredLine(key, stage)) {
					t.Errorf("log does not name %s at stage %d; log was:\n%s", key, stage, out.String())
				}
			}
		})
	}
}

// Every top-level key of the shipped schema (CONFIG_SCHEMA, packages/daemon/src/daemon/config.ts
// :319-415, 37 keys) plus the new postgres_dsn, and the class Stage 1 puts it in. No shipped key
// may fall through to the typo refusal.
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

		{key: "daemon_url", line: "daemon_url: http://127.0.0.1:13370", class: knownLater, stage: 2},
		{key: "instructions", line: "instructions: /etc/legion/instructions.md", class: knownLater, stage: 2},
		{key: "omp_invocation", line: "omp_invocation: omp", class: knownLater, stage: 2},
		{key: "omp_launch_prefix", line: "omp_launch_prefix: mise x --", class: knownLater, stage: 2},
		{key: "worker_stream_port", line: "worker_stream_port: 13371", class: knownLater, stage: 2},
		{key: "worker_boot_timeout_seconds", line: "worker_boot_timeout_seconds: 120", class: knownLater, stage: 2},
		{key: "worker_boot_registration_deadline_intervals", line: "worker_boot_registration_deadline_intervals: 3", class: knownLater, stage: 2},
		{key: "worker_rpc_timeout_seconds", line: "worker_rpc_timeout_seconds: 5", class: knownLater, stage: 2},
		{key: "worker_stop_timeout_seconds", line: "worker_stop_timeout_seconds: 10", class: knownLater, stage: 2},
		{key: "tree_stop_timeout_seconds", line: "tree_stop_timeout_seconds: 60", class: knownLater, stage: 2},
		{key: "slow_command_timeout_seconds", line: "slow_command_timeout_seconds: 300", class: knownLater, stage: 2},
		{key: "envoy_url", line: "envoy_url: http://127.0.0.1:9020", class: knownLater, stage: 3},
		{key: "envoy_token_file", line: "envoy_token_file: /var/run/legion/ENVOY_TOKEN", class: knownLater, stage: 3},
		{key: "nats_urls", line: "nats_urls: [nats://127.0.0.1:4222]", class: knownLater, stage: 3},
		{key: "dispatch_url", line: "dispatch_url: https://dispatch.example", class: knownLater, stage: 3},
		{key: "projects", line: "projects: {DEMO: {repo: acme/widgets}}", class: knownLater, stage: 3},
		{key: "gates", line: "gates: {design: off}", class: knownLater, stage: 3},
		{key: "github_apps", line: "github_apps: {implement: {app_id: \"1\"}}", class: knownLater, stage: 3},
		{key: "max_recursion_depth", line: "max_recursion_depth: 8", class: knownLater, stage: 3},
		{key: "linger_hours", line: "linger_hours: 72", class: knownLater, stage: 3},
		{key: "max_fix_attempts", line: "max_fix_attempts: 3", class: knownLater, stage: 3},
		{key: "operator_token_file", line: "operator_token_file: /var/run/legion/OPERATOR_TOKEN", class: knownLater, stage: 4},

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
