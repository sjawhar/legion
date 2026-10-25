// Package claudelaunch is the one definition of how Claude Code is launched for a Legion phase
// worker: the agent command around `claude -p`, the session id a launch starts or continues, the
// MCP document naming the one claude-envoy server, and the environment Claude Code reads. A tmux
// pane (runtime/tmux/claude.go) and a Kubernetes pod (runtime/sandbox) run the same worker, so
// both build these shapes here and neither holds a second copy of one.
//
// The package holds no path of its own and resolves none: every executable and directory it names
// is an argument, because the two runtimes find them in different places. Under tmux they are
// boot resolved from LEGION_CLAUDE_PATH, LEGION_BUN_PATH and LEGION_CLAUDE_PLUGIN_DIR on the
// daemon's own host (internal/daemon/launchers.go); under Kubernetes they are the worker image's
// constants, which the daemon never sees. Nor does anything here touch a file: the MCP document is
// returned as bytes, and each runtime writes it where that runtime keeps it.
package claudelaunch

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
)

// Paths are the absolute paths a Claude Code agent command names, each resolved by the caller: the
// `legion` executable whose claude-bridge subcommand runs the worker's turns, the `claude`
// executable it runs, and the claude-envoy plugin's package root, whose `legion/` directory is the
// generated Legion plugin and whose `dist/envoy-channel.js` is the MCP server every worker runs
// (MCPConfig).
type Paths struct {
	Legion, Claude, PluginDir string
}

// MCPServer is the name a worker's one MCP server is configured under, which prefixes each of its
// tools in Claude Code: `mcp__legion__legion`, `mcp__legion__envoy_publish`, …
const MCPServer = "legion"

// Argv is the agent command a Legion phase worker's Claude Code runs under: `legion claude-bridge`
// around `claude -p` in stream-json both ways, with every permission granted (a headless worker
// has no one to ask), only the MCP server mcpConfig names (an operator's own servers never load),
// the claude-envoy plugin and the generated Legion plugin for their skills, agents and the
// `dispatch` command, and the session — `--session-id` for a fresh one, `--resume` to continue the
// one ResumeSession read.
//
// It ends there: `--append-system-prompt` and its value are the caller's, since the runtimes pass
// the prompt differently. A pane's shell reads the prompt files itself
// (omplaunch.SystemPromptArgument), where a pod, which cannot see the daemon's disk, carries the
// text inlined as one argv word.
func Argv(paths Paths, mcpConfig, session string, resume bool) []string {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	return []string{
		paths.Legion, "claude-bridge", "--", paths.Claude,
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config", "--mcp-config", mcpConfig,
		"--plugin-dir", paths.PluginDir,
		"--plugin-dir", filepath.Join(paths.PluginDir, "legion"),
		flag, session,
	}
}

// MCPConfig is the worker's MCP document, the JSON `--mcp-config` reads: the claude-envoy server
// under MCPServer, run by bun from pluginDir's `dist/envoy-channel.js`, its plugin data under
// configDir. Claude Code runs the server with the worker's own environment, which names the
// daemon, Envoy and Dispatch. The caller writes the bytes where its runtime keeps them.
func MCPConfig(bun, pluginDir, configDir string) ([]byte, error) {
	config := map[string]any{"mcpServers": map[string]any{MCPServer: map[string]any{
		"command": bun,
		"args":    []string{filepath.Join(pluginDir, "dist", "envoy-channel.js")},
		"env":     map[string]string{"CLAUDE_PLUGIN_DATA": filepath.Join(configDir, "envoy-plugin-data")},
	}}}
	return json.Marshal(config)
}

// Environment is what a Claude Code worker carries beside its runtime's own pairs, each a
// `NAME=VALUE`: its Claude Code configuration directory, and Claude Code's background tasks and
// scheduled prompts off, since the bridge reports a turn's end when its last user message's result
// arrives and nothing may run after it unseen.
func Environment(configDir string) []string {
	return []string{
		"CLAUDE_CONFIG_DIR=" + configDir,
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CLAUDE_CODE_DISABLE_CRON=1",
	}
}

// NewSessionID is a random (version 4) UUID, the form `--session-id` takes.
func NewSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// transcript is the name of a transcript Claude Code writes, `<session>.jsonl`, its one group the
// session id.
var transcript = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// Transcript is the session id of file when file is a transcript Claude Code writes —
// `<CLAUDE_CONFIG_DIR>/projects/<cwd>/<session>.jsonl`, wherever that configuration directory is —
// and ok false for any other path. A pod keeps a role's configuration directory on the issue's
// volume and a tmux pane the claim's under the daemon's state directory, and a reader of a recorded
// session is told neither, so the shape is the whole test: no Oh My Pi session file, named for a
// timestamp and a counter, has it.
func Transcript(file string) (session string, ok bool) {
	if filepath.Base(filepath.Dir(filepath.Dir(file))) != "projects" {
		return "", false
	}
	match := transcript.FindStringSubmatch(filepath.Base(file))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// ResumeSession is the session id a recorded transcript continues, refusing a recorded session
// that is not a transcript (Transcript) of the claim's own Claude Code directory: an Oh My Pi
// session, recorded before the role's backend changed, which `--resume` could not continue.
func ResumeSession(configDir, file string) (string, error) {
	if filepath.Dir(filepath.Dir(file)) != filepath.Join(configDir, "projects") {
		return "", fmt.Errorf("the recorded session %s is not a Claude Code transcript under %s: the role's backend changed since it was recorded", file, configDir)
	}
	session, ok := Transcript(file)
	if !ok {
		return "", fmt.Errorf("the recorded session %s is not named for a Claude Code session id", file)
	}
	return session, nil
}
