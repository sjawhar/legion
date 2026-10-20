// Package claudebridge is `legion claude-bridge`: the process `legion worker-shim` runs in Oh My
// Pi's place for a phase worker whose role runs on Claude Code (`worker_backends`). It speaks Oh My
// Pi's RPC frames to the shim on its stdin and stdout and Claude Code's stream-json to the
// `claude -p` it runs, so the daemon's worker stream, the shim and supervision stay as they are:
//
//   - a `prompt` becomes a user message and starts a turn (`agent_start`); a prompt that arrives
//     during a turn is acknowledged and then refused the way Oh My Pi refuses it ("Agent is already
//     processing"), which the daemon takes back uncharged and sends again at the turn's end;
//   - `abort` becomes an interrupt control request, answered once Claude Code acknowledges it;
//   - `get_state` and `negotiate_protocol` are the bridge's own to answer;
//   - each user message's `result` ends one step of the turn, and the turn ends (`agent_end`) once
//     no Envoy delivery waits and the phase-stall check owes no follow-up (stall.go).
//
// A fresh session (`--session-id`) is seeded with a local `/rename` before anything else: Claude
// Code writes no transcript until its first input, and the Legion claim (packages/claude-envoy)
// registers that transcript's path, which `--resume` takes on a relaunch. Envoy deliveries arrive
// as user turns over the inbox socket the bridge opens and names to Claude Code, and so to its MCP
// servers, as LEGION_CLAUDE_INBOX (inbox.go), since Claude Code's print mode delivers no channel
// notification.
package claudebridge

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Config is one bridge's resolved inputs. Run spawns Argv once and bridges until it exits.
type Config struct {
	// Argv is the `claude` invocation, which names its session with exactly one of --session-id
	// (a fresh session) and --resume.
	Argv []string
	// Env is the environment Claude Code runs under (the bridge's own); Run adds InboxVariable.
	Env []string
	// SessionID is the session Argv names; Seed is whether it is fresh and so seeded with Title.
	SessionID string
	Seed      bool
	Title     string
	// StallFile holds this session's phase-stall state across relaunches (stall.go).
	StallFile string
	// Stdin carries the daemon's frames from the shim, Stdout the frames back to it. Log receives
	// the bridge's own lines and Claude Code's stderr: what the pane shows.
	Stdin  io.Reader
	Stdout io.Writer
	Log    io.Writer
}

// Configure reads a bridge's configuration from the claude argv and the environment the runtime
// gave it: the session from argv, the seed title from LEGION_ROLE and LEGION_ISSUE, and the
// phase-stall file under CLAUDE_CONFIG_DIR (the claim's own Claude Code directory, which must be
// absolute). Every refusal names what is missing.
func Configure(argv, env []string) (Config, error) {
	if len(argv) == 0 {
		return Config{}, errors.New("no claude command to run after --")
	}
	session, fresh, err := sessionOf(argv[1:])
	if err != nil {
		return Config{}, err
	}
	configDir := envValue(env, "CLAUDE_CONFIG_DIR")
	if !filepath.IsAbs(configDir) {
		return Config{}, fmt.Errorf("CLAUDE_CONFIG_DIR must be an absolute path (got %q): the claim's Claude Code directory holds its phase-stall state", configDir)
	}
	cfg := Config{
		Argv:      argv,
		Env:       env,
		SessionID: session,
		Seed:      fresh,
		StallFile: filepath.Join(configDir, "legion-phase-stall-"+session+".json"),
	}
	if fresh {
		role, issue := envValue(env, "LEGION_ROLE"), envValue(env, "LEGION_ISSUE")
		if role == "" || issue == "" {
			return Config{}, errors.New("LEGION_ROLE and LEGION_ISSUE are required to title a fresh session")
		}
		cfg.Title = "Legion " + role + " · " + issue
	}
	return cfg, nil
}

// sessionOf is the session args name: --session-id's (fresh) or --resume's, each as `--flag value`
// or `--flag=value`. Naming neither, both, or one twice is refused: the bridge must know whether to
// seed, and which session's state it keeps.
func sessionOf(args []string) (id string, fresh bool, err error) {
	found := 0
	for i := 0; i < len(args); i++ {
		for _, flag := range []string{"--session-id", "--resume"} {
			value, ok := "", false
			if args[i] == flag && i+1 < len(args) {
				value, ok = args[i+1], true
				i++
			} else if v, cut := strings.CutPrefix(args[i], flag+"="); cut {
				value, ok = v, true
			}
			if !ok {
				continue
			}
			if value == "" {
				return "", false, fmt.Errorf("%s names no session", flag)
			}
			found++
			id, fresh = value, flag == "--session-id"
			break
		}
	}
	if found != 1 {
		return "", false, errors.New("the claude command must name its session with exactly one --session-id or --resume")
	}
	return id, fresh, nil
}

// envValue is name's value in env, the last assignment winning; "" when env assigns none.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}
