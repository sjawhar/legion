package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// One window per issue, named for it; a key too long for a window name keeps a prefix and a hash
// of the whole (runtime-tmux.ts:41-49).
func TestTreeName(t *testing.T) {
	if got := treeName("LEGION-42"); got != "legion-42" {
		t.Errorf("treeName = %q", got)
	}
	long := "PROJ-" + strings.Repeat("X", 300)
	sum := sha256.Sum256([]byte(strings.ToLower(long)))
	want := strings.ToLower(long)[:160-17] + "-" + hex.EncodeToString(sum[:])[:16]
	if got := treeName(long); got != want || len(got) != 160 {
		t.Errorf("treeName(long) = %q (%d), want %q", got, len(got), want)
	}
}

// A `-P -F` report is three tokens from new-window and two from split-window; anything else is a
// launch failure naming the command and tmux's stderr, never the report (tmux.ts:234-260,
// tmux.test.ts:458-494).
func TestReadPaneReport(t *testing.T) {
	report, err := readPaneReport("new-window", result{stdout: "@42 %1 4242\n"}, true)
	if err != nil || report != (paneReport{window: "@42", pane: "%1", pid: 4242}) {
		t.Errorf("new-window report = (%+v, %v)", report, err)
	}
	report, err = readPaneReport("split-window", result{stdout: "%7 5151\n"}, false)
	if err != nil || report != (paneReport{pane: "%7", pid: 5151}) {
		t.Errorf("split-window report = (%+v, %v)", report, err)
	}
	for _, tc := range []struct {
		command string
		result  result
		window  bool
		want    string
	}{
		{"new-window", result{stdout: "GH_TOKEN=leaked-value\n", stderr: "unexpected output"}, true, "tmux new-window did not report a window id: unexpected output"},
		{"new-window", result{stdout: "@42 leaked 1\n"}, true, "tmux new-window did not report a pane id: tmux printed nothing on stderr"},
		{"split-window", result{stdout: "%7 0\n"}, false, "tmux split-window did not report a pane pid: tmux printed nothing on stderr"},
	} {
		_, err := readPaneReport(tc.command, tc.result, tc.window)
		if err == nil || err.Error() != tc.want {
			t.Errorf("readPaneReport(%q) error = %v, want %q", tc.result.stdout, err, tc.want)
		}
	}
}

func testSpec() runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim:      claim.Token("legion-omp-LEGION-43-tester"),
		Project:    "omp",
		Tree:       "LEGION-42",
		Issue:      "LEGION-43",
		Role:       claim.RoleTester,
		Generation: 2,
		BootToken:  "boot-secret",
		Env:        map[string]string{"JJ_USER": "legion-tester", "PATH": "/legion/bin:/usr/bin", "GH_CONFIG_DIR": "/state/gh"},
		Secrets:    map[string]string{"ENVOY_TOKEN": "envoy-secret"},
		Prompt:     runtime.PromptParts{RolePromptPaths: []string{"/roles/tester.md"}},
		Workspace:  "/state/workspaces/LEGION-43",
	}
}

// The pane's -e pairs, in the one order: the pairs every Legion pane carries (the shipped set,
// processes.ts:4562-4579, less what Stage 3 adds, plus LEGION_DAEMON_API=go), the XDG base
// directories under `<state_dir>/home` explicitly, the caller's own variables sorted, then one
// `<NAME>_FILE` pointer per secret — the boot token's first. PATH is never a pair: tmux would
// discard it (LEGION-91); the shell command exports it. No secret value is in any pair.
func TestPanePairs(t *testing.T) {
	spec := testSpec()
	in := paneInputs{
		stateDir:  "/state",
		daemonURL: "http://127.0.0.1:13370",
		envoyURL:  "http://127.0.0.1:9020",
		natsURLs:  []string{"nats://a:4222", "nats://b:4222"},
	}
	files := secretFiles("/state", spec)
	got := panePairs(spec, in, files)
	want := []string{
		"-e", "LEGION_DAEMON_API=go",
		"-e", "LEGION_TREE=LEGION-42",
		"-e", "LEGION_ISSUE=LEGION-43",
		"-e", "LEGION_ROLE=tester",
		"-e", "LEGION_GENERATION=2",
		"-e", "LEGION_PROJECT=omp",
		"-e", "LEGION_DAEMON_URL=http://127.0.0.1:13370",
		"-e", "LEGION_STATE_DIR=/state",
		"-e", "LEGION_WORKSPACE=/state/workspaces/LEGION-43",
		"-e", "ENVOY_NATS_URL=nats://a:4222,nats://b:4222",
		"-e", "ENVOY_URL=http://127.0.0.1:9020",
		"-e", "GIT_TERMINAL_PROMPT=0",
		"-e", "XDG_CONFIG_HOME=/state/home/.config",
		"-e", "XDG_CACHE_HOME=/state/home/.cache",
		"-e", "XDG_DATA_HOME=/state/home/.local/share",
		"-e", "XDG_STATE_HOME=/state/home/.local/state",
		"-e", "GH_CONFIG_DIR=/state/gh",
		"-e", "JJ_USER=legion-tester",
		"-e", "LEGION_BOOT_TOKEN_FILE=/state/secrets/legion-omp-LEGION-43-tester",
		"-e", "ENVOY_TOKEN_FILE=/state/secrets/legion-omp-LEGION-43-tester-envoy_token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("panePairs =\n%q\nwant\n%q", got, want)
	}

	// No NATS configured: no ENVOY_NATS_URL pair rather than an empty one.
	in.natsURLs = nil
	for _, pair := range panePairs(spec, in, files) {
		if strings.HasPrefix(pair, "ENVOY_NATS_URL=") {
			t.Errorf("an unconfigured NATS still produced %q", pair)
		}
	}
}

// A spec the runtime cannot honour exactly is refused before anything touches the disk or tmux —
// above all one that would put a variable the runtime owns, or a credential, into a -e value.
func TestValidateSpawnSpecRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*runtime.SpawnSpec)
		want   string
	}{
		{"no claim", func(s *runtime.SpawnSpec) { s.Claim = "" }, "spawn: no claim token"},
		{"a claim that is not one file name", func(s *runtime.SpawnSpec) { s.Claim = "../x" }, `spawn "../x": the claim token names the pane's secret files and must be one file name`},
		{"no tree", func(s *runtime.SpawnSpec) { s.Tree = "" }, "spawn legion-omp-LEGION-43-tester: no tree"},
		{"no boot token", func(s *runtime.SpawnSpec) { s.BootToken = "" }, "spawn legion-omp-LEGION-43-tester: no boot token"},
		{"a relative workspace", func(s *runtime.SpawnSpec) { s.Workspace = "workspaces/x" }, `spawn legion-omp-LEGION-43-tester: workspace "workspaces/x" is not an absolute path`},
		{"no role prompt", func(s *runtime.SpawnSpec) { s.Prompt.RolePromptPaths = nil }, "spawn legion-omp-LEGION-43-tester: no role prompt"},
		{"a variable the runtime sets", func(s *runtime.SpawnSpec) { s.Env["LEGION_TREE"] = "OTHER-1" }, "spawn legion-omp-LEGION-43-tester: Env sets LEGION_TREE, which the runtime sets itself"},
		{"an XDG directory", func(s *runtime.SpawnSpec) { s.Env["XDG_CONFIG_HOME"] = "/home/me/.config" }, "spawn legion-omp-LEGION-43-tester: Env sets XDG_CONFIG_HOME, which the runtime sets itself"},
		{"a credential as a value", func(s *runtime.SpawnSpec) { s.Env["ANTHROPIC_API_KEY"] = "sk-x" }, "spawn legion-omp-LEGION-43-tester: Env carries ANTHROPIC_API_KEY, a credential-shaped name; a secret travels in Secrets, as a file"},
		{"a secret name that is not a variable name", func(s *runtime.SpawnSpec) { s.Secrets["BAD NAME"] = "x" }, `spawn legion-omp-LEGION-43-tester: secret "BAD NAME" is not an environment variable name`},
		{"a secret whose pointer the runtime owns", func(s *runtime.SpawnSpec) { s.Secrets["LEGION_BOOT_TOKEN"] = "x" }, "spawn legion-omp-LEGION-43-tester: secret LEGION_BOOT_TOKEN's pointer LEGION_BOOT_TOKEN_FILE is a variable the runtime sets itself"},
		{"a secret whose pointer Env also sets", func(s *runtime.SpawnSpec) { s.Env["ENVOY_TOKEN_FILE"] = "/elsewhere" }, "spawn legion-omp-LEGION-43-tester: secret ENVOY_TOKEN's pointer ENVOY_TOKEN_FILE is also set in Env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			tc.mutate(&spec)
			err := validateSpawnSpec(spec)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateSpawnSpec = %v, want %q", err, tc.want)
			}
		})
	}
	if err := validateSpawnSpec(testSpec()); err != nil {
		t.Errorf("a well-formed spec was refused: %v", err)
	}
}
