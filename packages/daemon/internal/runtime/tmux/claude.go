package tmux

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
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
const ClaudeMCPServer = claudelaunch.MCPServer

// ClaudeConfigDir is a claim's own Claude Code directory, its CLAUDE_CONFIG_DIR: the session
// transcripts `--resume` reads, the MCP configuration, and the phase-stall record the pane's
// Legion MCP server keeps (packages/claude-envoy/src/legion-phase-hook.ts). It is named
// for the claim token, under the state directory but outside its secrets, so it outlives every
// launch of the claim and no secret pruning removes it.
func ClaudeConfigDir(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "claude", string(token))
}

// claudeResumeSession is the session id the pane's recorded transcript continues
// (claudelaunch.ResumeSession), refusing a session that is not a transcript of the claim's own
// Claude Code directory.
func claudeResumeSession(configDir, file string) (string, error) {
	return claudelaunch.ResumeSession(configDir, file)
}

// newSessionID is the session id a fresh pane starts (claudelaunch.NewSessionID).
func newSessionID() string { return claudelaunch.NewSessionID() }

// writeClaudeMCPConfig writes the pane's MCP configuration (claudelaunch.MCPConfig) into configDir,
// 0600, and returns its path. The pane's Claude Code reads it off the daemon's own disk, which is
// why the writing stays here; a pod's launcher writes the same document into its own directory.
func writeClaudeMCPConfig(configDir string, launch ClaudeLaunch) (string, error) {
	raw, err := claudelaunch.MCPConfig(launch.Bun, launch.PluginDir, configDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(configDir, "mcp.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write the Claude Code MCP configuration: %w", err)
	}
	return path, nil
}

// claudeInnerCommand is the command the shim runs for a Claude Code role: the agent command
// claudelaunch.Argv builds from the pane's boot-resolved paths, every word quoted for the pane's
// shell, and the one system prompt word every pane gets, whose `$(cat …)` that shell reads.
func claudeInnerCommand(legion string, launch ClaudeLaunch, mcpConfig, session string, resume bool, parts runtime.PromptParts) string {
	paths := claudelaunch.Paths{Legion: legion, Claude: launch.Binary, PluginDir: launch.PluginDir}
	return shellprefix.Command(claudelaunch.Argv(paths, mcpConfig, session, resume)) + " " +
		omplaunch.SystemPromptArgument(parts)
}

// claudeEnvironment are the -e pairs a Claude Code pane carries beside every pane's: tmux's form of
// the environment claudelaunch.Environment names, the claim's own configuration directory included.
func claudeEnvironment(configDir string) []string {
	assignments := claudelaunch.Environment(configDir)
	pairs := make([]string, 0, 2*len(assignments))
	for _, assignment := range assignments {
		pairs = append(pairs, "-e", assignment)
	}
	return pairs
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
