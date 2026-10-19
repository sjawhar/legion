package tmux

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// ClaudeLaunch is how a pane runs Claude Code for a role `worker_backends` puts on claude, each an
// absolute path boot resolved: the `claude` executable (LEGION_CLAUDE_PATH, else PATH's), the
// claude-envoy plugin's package root (LEGION_CLAUDE_PLUGIN_DIR), whose `legion/` directory is the
// generated Legion plugin and whose `dist/envoy-channel.js` is the MCP server every Claude pane runs,
// and the `bun` that server runs on.
type ClaudeLaunch struct {
	Binary, PluginDir, Bun string
}

// NewClaudeLauncher is Claude Code's launch, from what boot resolved.
func NewClaudeLauncher(launch ClaudeLaunch) Launcher {
	return claudeLauncher{launch: launch}
}

type claudeLauncher struct{ launch ClaudeLaunch }

// Marker is " claude-bridge -- ", the `legion` subcommand and its separator that the pane's
// command line runs Claude Code under (claudeInnerCommand).
func (claudeLauncher) Marker() string { return " claude-bridge -- " }

// ClaudeMCPServer is the name the pane's one MCP server is configured under, which prefixes each of
// its tools in Claude Code: `mcp__legion__legion`, `mcp__legion__envoy_publish`, …
const ClaudeMCPServer = "legion"

// ClaudeConfigDir is a claim's own Claude Code directory, its CLAUDE_CONFIG_DIR: the session
// transcripts `--resume` reads, the MCP configuration, the bridge's phase-stall state. It is named
// for the claim token, under the state directory but outside its secrets, so it outlives every
// launch of the claim and no secret pruning removes it.
func ClaudeConfigDir(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "claude", string(token))
}

// claudeTranscript is a transcript Claude Code writes: `<CLAUDE_CONFIG_DIR>/projects/<cwd>/<session>.jsonl`.
var claudeTranscript = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// claudeResumeSession is the session id a recorded transcript continues, refusing a recorded session
// that is not a transcript of the claim's own Claude Code directory: an Oh My Pi session, recorded
// before the role's backend changed, which `--resume` could not continue.
func claudeResumeSession(configDir, file string) (string, error) {
	rel, err := filepath.Rel(filepath.Join(configDir, "projects"), file)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 2 || parts[0] == ".." {
		return "", fmt.Errorf("the recorded session %s is not a Claude Code transcript under %s: the role's backend changed since it was recorded", file, configDir)
	}
	match := claudeTranscript.FindStringSubmatch(parts[1])
	if match == nil {
		return "", fmt.Errorf("the recorded session %s is not named for a Claude Code session id", file)
	}
	return match[1], nil
}

// newSessionID is a random (version 4) UUID, the form `--session-id` takes.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeClaudeMCPConfig writes the pane's MCP configuration into configDir, 0600, and returns its
// path: the claude-envoy server under ClaudeMCPServer, on bun, its plugin data under configDir.
// Claude Code runs it with the pane's environment, which names the daemon, Envoy and Dispatch.
func writeClaudeMCPConfig(configDir string, launch ClaudeLaunch) (string, error) {
	config := map[string]any{"mcpServers": map[string]any{ClaudeMCPServer: map[string]any{
		"command": launch.Bun,
		"args":    []string{filepath.Join(launch.PluginDir, "dist", "envoy-channel.js")},
		"env":     map[string]string{"CLAUDE_PLUGIN_DATA": filepath.Join(configDir, "envoy-plugin-data")},
	}}}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	path := filepath.Join(configDir, "mcp.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write the Claude Code MCP configuration: %w", err)
	}
	return path, nil
}

// claudeInnerCommand is the command the shim runs for a Claude Code role: `legion claude-bridge`
// around `claude -p` in stream-json both ways, with every permission granted (a headless worker has
// no one to ask), only the MCP server mcpConfig names (an operator's own servers never load), the
// claude-envoy plugin and the generated Legion plugin for their skills, agents and the `dispatch`
// command, the session — `--session-id` for a fresh one, `--resume` to continue one — and the one
// system prompt word every pane gets.
func claudeInnerCommand(legion string, launch ClaudeLaunch, mcpConfig, session string, resume bool, parts runtime.PromptParts) string {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	return shellprefix.Word(legion) + " claude-bridge -- " + shellprefix.Word(launch.Binary) +
		" -p --input-format stream-json --output-format stream-json --verbose --permission-mode bypassPermissions" +
		" --strict-mcp-config --mcp-config " + shellprefix.Word(mcpConfig) +
		" --plugin-dir " + shellprefix.Word(launch.PluginDir) +
		" --plugin-dir " + shellprefix.Word(filepath.Join(launch.PluginDir, "legion")) +
		" " + flag + " " + session + " " + omplaunch.SystemPromptArgument(parts)
}

// claudeEnvironment are the -e pairs a Claude Code pane carries beside every pane's: its claim's own
// configuration directory, and Claude Code's background tasks and scheduled prompts off, since the
// bridge reports a turn's end when its last user message's result arrives and nothing may run after
// it unseen.
func claudeEnvironment(configDir string) []string {
	return []string{
		"-e", "CLAUDE_CONFIG_DIR=" + configDir,
		"-e", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"-e", "CLAUDE_CODE_DISABLE_CRON=1",
	}
}

// Launch is the inner command and the extra -e pairs of a Claude Code launch: the claim's
// directory made, its MCP configuration written, and the session it starts or continues.
func (l claudeLauncher) Launch(spec runtime.SpawnSpec, host Host) (inner string, pairs []string, err error) {
	configDir := ClaudeConfigDir(host.StateDir, spec.Claim)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("spawn %s: the Claude Code directory: %w", spec.Claim, err)
	}
	session, resume := newSessionID(), false
	if spec.ResumeSessionFile != "" {
		if session, err = claudeResumeSession(configDir, spec.ResumeSessionFile); err != nil {
			return "", nil, fmt.Errorf("spawn %s: %w", spec.Claim, err)
		}
		resume = true
	}
	mcpConfig, err := writeClaudeMCPConfig(configDir, l.launch)
	if err != nil {
		return "", nil, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	return claudeInnerCommand(host.Legion, l.launch, mcpConfig, session, resume, spec.Prompt), claudeEnvironment(configDir), nil
}
