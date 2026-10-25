package claudelaunch

import (
	"strings"
	"testing"
)

var testPaths = Paths{Legion: "/opt/legion/bin/legion", Claude: "/usr/local/bin/claude", PluginDir: "/opt/legion/claude-envoy"}

// The agent command as words, which is what a pod's argv carries and what a pane's shell renders:
// the bridge around `claude -p` in stream-json both ways (--verbose, which stream-json output
// requires), every permission granted, only the configured MCP server, both plugin directories and
// the session. It stops before --append-system-prompt, which each runtime adds its own way.
func TestArgv(t *testing.T) {
	session := "11111111-2222-4333-8444-555555555555"
	got := Argv(testPaths, "/state/claude-mcp.json", session, false)
	want := []string{
		"/opt/legion/bin/legion", "claude-bridge", "--", "/usr/local/bin/claude",
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config", "--mcp-config", "/state/claude-mcp.json",
		"--plugin-dir", "/opt/legion/claude-envoy",
		"--plugin-dir", "/opt/legion/claude-envoy/legion",
		"--session-id", session,
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("fresh:\n%q\nwant\n%q", got, want)
	}
	resumed := Argv(testPaths, "/state/claude-mcp.json", session, true)
	if last := resumed[len(resumed)-2:]; last[0] != "--resume" || last[1] != session {
		t.Errorf("resumed ends %q", last)
	}
}

// A Claude Code worker's own environment, in order: its configuration directory, and Claude Code's
// background tasks and scheduled prompts off.
func TestEnvironment(t *testing.T) {
	got := strings.Join(Environment("/state/claude/c"), " ")
	if got != "CLAUDE_CONFIG_DIR=/state/claude/c CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 CLAUDE_CODE_DISABLE_CRON=1" {
		t.Errorf("Environment = %s", got)
	}
}
