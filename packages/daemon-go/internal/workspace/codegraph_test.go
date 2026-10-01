package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubCodegraph puts a fake `codegraph` executable first on PATH. Its `status --json` reports
// uninitialized until a marker file `.codegraph-initialized` exists in the directory it is run
// from, its `init` writes that marker, and it records every invocation's argv (one per line) to
// callLog so the test can see exactly what Provision ran.
func stubCodegraph(t *testing.T, callLog string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo "$@" >> '` + callLog + `'
marker=".codegraph-initialized"
case "$1" in
status)
  if [ -f "$marker" ]; then
    echo '{"initialized":true}'
  else
    echo '{"initialized":false}'
  fi
  ;;
init)
  touch "$marker"
  ;;
esac
`
	path := filepath.Join(dir, "codegraph")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProvisionWarmsTheCodegraphIndexWhenTheCliIsOnPath(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	stubCodegraph(t, callLog)
	run := newLocalRunner(t)
	req := provisionRequest(t)

	workspace, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Dir, ".codegraph-initialized")); err != nil {
		t.Fatalf("workspace left uninitialized after provisioning: %v", err)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if !strings.Contains(string(calls), "status --json") || !strings.Contains(string(calls), "init") {
		t.Fatalf("codegraph calls = %q, want both status --json and init", calls)
	}

	// Re-provisioning the same workspace is a no-op re-index: codegraph runs status again, but
	// never a second init, since the marker already reports initialized.
	before := len(strings.Split(strings.TrimSpace(string(calls)), "\n"))
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	calls, err = os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log after second provision: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != before+1 || lines[len(lines)-1] != "status --json" {
		t.Fatalf("codegraph calls after a second provision = %v, want exactly one more status --json call", lines)
	}
}

// TestProvisionNeverFailsWhenTheCodegraphCliIsMissing proves the acceptance criterion directly:
// warmCodegraphIndex logs its skip to stderr (never through Request.Log, which cmd/legion's exact
// stdout assertions own), so this only has to show Provision still succeeds, and leaves no index,
// when codegraph is absent from PATH entirely — including on this devbox, which has a real one.
func TestProvisionNeverFailsWhenTheCodegraphCliIsMissing(t *testing.T) {
	// git and jj resolve to absolute paths before PATH is narrowed below, exactly as
	// resolveTools does at daemon boot: the Runner never looks PATH up again per command.
	run := newLocalRunner(t)
	req := provisionRequest(t)
	// Narrowed to a fresh, empty directory so this test proves the missing-CLI path even on a
	// machine (like this one) that has a real `codegraph` on its ordinary PATH.
	t.Setenv("PATH", t.TempDir())

	workspace, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision with no codegraph on PATH: %v", err)
	}
	if _, err := os.Stat(workspace.Dir); err != nil {
		t.Fatalf("workspace missing after provisioning with no codegraph: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Dir, ".codegraph-initialized")); !os.IsNotExist(err) {
		t.Fatalf("stat .codegraph-initialized = %v, want it absent with no codegraph on PATH", err)
	}
}
