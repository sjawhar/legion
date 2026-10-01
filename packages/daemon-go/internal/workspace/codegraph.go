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

// warmCodegraphIndex mirrors the TypeScript daemon's ensureCodegraphIndex (research report
// AGENTC-1305 §7): the tester's `affected` and the reviewer's `impact`/`callers` queries need an
// index already built, not one built on first use. codegraph is deliberately not one of the tools
// Runner requires — the daemon and the pod init container must still boot where it is absent (the
// tmux runtime's host provisioning never installs it) — so this resolves it from PATH on its own,
// directly with os/exec, and never fails provisioning: a missing CLI, a non-zero exit, or a
// timeout is logged loudly to stderr, exactly as the TypeScript daemon's console.error does, and
// never through Request.Log (reserved for the one structured provisioning message createWorkspace
// writes; cmd/legion/workspace_init.go and tests assert its stdout exactly). Warming simply does
// not happen when the CLI is missing or fails; the worker falls back to grep, per its role
// prompt. DO_NOT_TRACK=1 disables both CodeGraph's telemetry and its update check (its bundled
// docs rank DO_NOT_TRACK above CODEGRAPH_TELEMETRY above stored config above default-on), so an
// automatic, non-opt-in warm-up never phones home.
func warmCodegraphIndex(ctx context.Context, dir string) {
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

// runCodegraph runs one codegraph subcommand in dir, bounded by codegraphTimeout, with
// DO_NOT_TRACK=1 added to the process environment.
func runCodegraph(ctx context.Context, codegraphPath, dir string, args ...string) (codegraphResult, error) {
	bounded, cancel := context.WithTimeout(ctx, codegraphTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, codegraphPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DO_NOT_TRACK=1")
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
