package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The command's refusals come before anything starts, each naming what is missing, as a usage error.
func TestClaudeBridgeRefusesWhatItCannotRun(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_ISSUE", "ACME-7")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "no claude command to run after --"},
		{[]string{"--", "claude", "-p"}, "exactly one --session-id or --resume"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "claude-bridge"}, tc.args...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), claudeBridgeUsage) {
			t.Errorf("claude-bridge %q = %d %q, want 2 naming %q and the usage", tc.args, code, stderr.String(), tc.want)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "relative")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"legion", "claude-bridge", "--", "claude", "--resume", "s"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "CLAUDE_CONFIG_DIR must be an absolute path") {
		t.Errorf("a relative CLAUDE_CONFIG_DIR = %d %q", code, stderr.String())
	}
}

// A claude that cannot be started is the bridge's failure, exit 1, naming it.
func TestClaudeBridgeNamesAClaudeItCannotStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"legion", "claude-bridge", "--", "/nonexistent/claude", "--resume", "s"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "start /nonexistent/claude") {
		t.Errorf("claude-bridge with no claude = %d %q", code, stderr.String())
	}
}
