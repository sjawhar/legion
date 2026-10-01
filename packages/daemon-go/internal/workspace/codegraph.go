package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// codegraphTimeout bounds each codegraph invocation independently, the same budget Provision's own
// commands get from the Runner.
const codegraphTimeout = CommandTimeout

// WarmCodegraphIndex mirrors the TypeScript daemon's ensureCodegraphIndex (research report
// AGENTC-1305 §7): the tester's `affected` and the reviewer's `impact`/`callers` queries need an
// index already built, not one built on first use. Each caller (the pod init container,
// cmd/legion/workspace_init.go, after its repository flock is released; the Go daemon's host
// provisioning for tmux panes, internal/daemon/outbox.go, after Provision returns) runs this
// outside its own provisioning serialization — indexing reads only the issue's own workspace
// directory, so holding a repository-wide lock across it would queue every other issue's pod or
// pane behind one potentially slow index build for no correctness reason; Provision itself never
// calls it. codegraph is deliberately not one of the tools Runner requires — the daemon and the
// pod init container must still boot where it is absent (the tmux runtime's host provisioning
// never installs it) — so this resolves it from PATH on its own, directly with os/exec, and never
// fails provisioning: a missing CLI, a non-zero exit, or a timeout is logged loudly to stderr,
// exactly as the TypeScript daemon's console.error does, and never through Request.Log (reserved
// for the one structured provisioning message createWorkspace writes; cmd/legion/
// workspace_init.go and tests assert its stdout exactly). Warming simply does not happen when the
// CLI is missing or fails; the worker falls back to grep, per its role prompt. Every codegraph
// invocation gets a minimal, explicit environment — PATH, HOME, TMPDIR when set, and
// DO_NOT_TRACK=1 — never the process's full environment: a provisioning caller may hold a
// one-shot GitHub token or other secrets in its own environment, and codegraph gets none of them.
// DO_NOT_TRACK=1 disables both CodeGraph's telemetry and its update check (its bundled docs rank
// DO_NOT_TRACK above CODEGRAPH_TELEMETRY above stored config above default-on), so an automatic,
// non-opt-in warm-up never phones home.
func WarmCodegraphIndex(ctx context.Context, dir string) {
	codegraphPath, err := exec.LookPath("codegraph")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up skipped for %s: %s\n", dir, err)
		return
	}
	status, err := runCodegraph(ctx, codegraphPath, dir, "status", "--json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	if status.exitCode == 0 && codegraphInitialized(status.stdout) {
		return
	}
	result, err := runCodegraph(ctx, codegraphPath, dir, "init")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	if result.exitCode != 0 {
		fmt.Fprintf(os.Stderr, "[legion] codegraph init failed for %s (exit %d): %s\n", dir, result.exitCode, strings.TrimSpace(result.stderr))
	}
}

// codegraphInitialized reads `codegraph status --json`'s `initialized` field exactly as the
// TypeScript daemon's isCodegraphInitialized does: `codegraph status` exits 0 whether or not the
// project has ever been indexed, so only the parsed body tells the two apart, and anything that
// fails to parse as that shape is treated as not initialized.
func codegraphInitialized(stdout string) bool {
	var parsed struct {
		Initialized bool `json:"initialized"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return false
	}
	return parsed.Initialized
}

type codegraphResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// codegraphEnvironment is the minimal environment every codegraph invocation gets: its own PATH
// lookup (so a self-exec or any PATH-relative helper it spawns still resolves), HOME (codegraph
// reads its config/cache under it), TMPDIR when the caller's own environment sets one, and the
// telemetry/update-check opt-out. Nothing else — never GH_*, LEGION_*, provider keys, or any
// other token-shaped variable a provisioning caller's own process environment may hold.
func codegraphEnvironment() []string {
	env := []string{"DO_NOT_TRACK=1"}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	if tmpdir, ok := os.LookupEnv("TMPDIR"); ok {
		env = append(env, "TMPDIR="+tmpdir)
	}
	return env
}

// runCodegraph runs one codegraph subcommand in dir, bounded by codegraphTimeout, with
// codegraphEnvironment's minimal, explicit environment — never the caller's full environment.
func runCodegraph(ctx context.Context, codegraphPath, dir string, args ...string) (codegraphResult, error) {
	bounded, cancel := context.WithTimeout(ctx, codegraphTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, codegraphPath, args...)
	cmd.Dir = dir
	cmd.Env = codegraphEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := codegraphResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.exitCode = exited.ExitCode()
		return result, nil
	}
	return result, err
}
