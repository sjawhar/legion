package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var testClaude = ClaudeLaunch{Binary: "/opt/claude/bin/claude", PluginDir: "/opt/legion/claude-envoy", Bun: "/opt/bun/bin/bun"}

// The inner command of a Claude Code role: the bridge around `claude -p` in stream-json both ways
// (--verbose, which stream-json output requires), every permission granted, only the configured MCP
// server, both plugin directories, the session, and the one system prompt word every pane gets.
func TestClaudeInnerCommand(t *testing.T) {
	parts := runtime.PromptParts{RolePromptPaths: []string{"/roles/implementer.md"}, Addressing: "Legion addressing: x"}
	got := claudeInnerCommand("/opt/legion/legion", testClaude, "/state/claude/c/mcp.json", "11111111-2222-4333-8444-555555555555", false, parts)
	want := "/opt/legion/legion claude-bridge -- /opt/claude/bin/claude -p --input-format stream-json --output-format stream-json --verbose" +
		" --permission-mode bypassPermissions --strict-mcp-config --mcp-config /state/claude/c/mcp.json" +
		" --plugin-dir /opt/legion/claude-envoy --plugin-dir /opt/legion/claude-envoy/legion" +
		" --session-id 11111111-2222-4333-8444-555555555555 " +
		`--append-system-prompt "$(cat /roles/implementer.md)` + "\n\nLegion addressing: x" + `"`
	if got != want {
		t.Errorf("fresh:\n%s\nwant\n%s", got, want)
	}
	resumed := claudeInnerCommand("/opt/legion/legion", testClaude, "/m.json", "11111111-2222-4333-8444-555555555555", true, parts)
	if !strings.Contains(resumed, " --resume 11111111-2222-4333-8444-555555555555 ") || strings.Contains(resumed, "--session-id") {
		t.Errorf("resumed: %s", resumed)
	}
}

// A resumed claim continues the session its recorded transcript names, and only a transcript of the
// claim's own Claude Code directory: a recorded Oh My Pi session is refused by name.
func TestClaudeResumeSession(t *testing.T) {
	dir := "/state/claude/legion-acme-acme-7-implementer"
	id := "11111111-2222-4333-8444-555555555555"
	got, err := claudeResumeSession(dir, filepath.Join(dir, "projects", "-state-workspaces-acme-7", id+".jsonl"))
	if err != nil || got != id {
		t.Errorf("claudeResumeSession = %q, %v", got, err)
	}
	for file, want := range map[string]string{
		"/state/trees/x/.omp/2026-10-18T00-00-00Z_" + id + ".jsonl":   "is not a Claude Code transcript",
		filepath.Join(dir, "projects", "p", "sub", id+".jsonl"):       "is not a Claude Code transcript",
		filepath.Join(dir, "projects", "p", "not-a-session.jsonl"):    "is not named for a Claude Code session id",
		filepath.Join(dir, "projects", "p", id+".jsonl.bak"):          "is not named for a Claude Code session id",
		filepath.Join(dir, "projects", "..", "projects", id+".jsonl"): "is not a Claude Code transcript",
	} {
		if _, err := claudeResumeSession(dir, file); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("claudeResumeSession(%s) = %v, want %q", file, err, want)
		}
	}
}

func TestNewSessionIDIsAVersion4UUID(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := newSessionID()
		if !uuid.MatchString(id) || seen[id] {
			t.Fatalf("newSessionID = %q", id)
		}
		seen[id] = true
	}
}

// The MCP configuration names the one server, on bun, with its plugin data in the claim's directory.
func TestWriteClaudeMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := writeClaudeMCPConfig(dir, testClaude)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the configuration's mode: %v, %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	var config struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.MCPServers[ClaudeMCPServer]
	if len(config.MCPServers) != 1 || !ok || server.Command != testClaude.Bun ||
		len(server.Args) != 1 || server.Args[0] != "/opt/legion/claude-envoy/dist/envoy-channel.js" ||
		server.Env["CLAUDE_PLUGIN_DATA"] != filepath.Join(dir, "envoy-plugin-data") {
		t.Errorf("the MCP configuration is %s", raw)
	}
}

// A Claude Code pane carries its claim's directory and Claude Code's background work off, beside the
// pairs every pane carries.
func TestClaudeEnvironment(t *testing.T) {
	got := strings.Join(claudeEnvironment("/state/claude/c"), " ")
	if got != "-e CLAUDE_CONFIG_DIR=/state/claude/c -e CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 -e CLAUDE_CODE_DISABLE_CRON=1" {
		t.Errorf("claudeEnvironment = %s", got)
	}
}

// A Claude Code launch on a runtime that resolved no Claude Code is refused, and one resuming a
// recorded Oh My Pi session is refused before anything is written.
func TestClaudeLaunchRefusals(t *testing.T) {
	spec := testSpec()
	spec.Backend = runtime.BackendClaude
	r := &Runtime{stateDir: t.TempDir(), legion: "/opt/legion/legion"}
	if _, _, err := r.claudeLaunch(spec); err == nil || !strings.Contains(err.Error(), "resolved no Claude Code") {
		t.Errorf("no Claude Code: %v", err)
	}
	r.claude = &testClaude
	spec.ResumeSessionFile = "/state/trees/x/.omp/session.jsonl"
	if _, _, err := r.claudeLaunch(spec); err == nil || !strings.Contains(err.Error(), "the role's backend changed") {
		t.Errorf("an Oh My Pi session: %v", err)
	}
	spec.ResumeSessionFile = ""
	inner, pairs, err := r.claudeLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	configDir := ClaudeConfigDir(r.stateDir, spec.Claim)
	if !strings.Contains(inner, " claude-bridge -- ") || !strings.Contains(inner, "--mcp-config "+filepath.Join(configDir, "mcp.json")) ||
		!strings.Contains(strings.Join(pairs, " "), "CLAUDE_CONFIG_DIR="+configDir) {
		t.Errorf("claudeLaunch = %s, %q", inner, pairs)
	}
}

// verifyPane's command-line tier accepts the bridge as it accepts OMP, and nothing else.
func TestRunsAgent(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"/opt/omp/bin/omp\x00--mode\x00rpc\x00":                        true,
		"/opt/legion/legion\x00claude-bridge\x00--\x00/opt/claude\x00": true,
		"/bin/sh\x00-c\x00sleep 1\x00":                                 false,
	} {
		r := &Runtime{readProc: func(string) ([]byte, error) { return []byte(cmdline), nil }}
		if got, err := r.runsAgent(1); err != nil || got != want {
			t.Errorf("runsAgent(%q) = %v, %v", cmdline, got, err)
		}
	}
}
