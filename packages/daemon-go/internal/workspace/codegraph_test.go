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
// from, its `init` writes that marker, and it records every invocation's argv and environment
// (one call per line, `argv\tenv` with `env` entries joined by a space) to callLog so the test can
// see exactly what WarmCodegraphIndex ran and with what environment.
func stubCodegraph(t *testing.T, callLog string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\t%s\n' "$*" "$(env | tr '\n' ' ')" >> '` + callLog + `'
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

func TestWarmCodegraphIndexBuildsAnIndexWhenTheCliIsOnPath(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	stubCodegraph(t, callLog)
	dir := t.TempDir()

	WarmCodegraphIndex(context.Background(), dir)
	if _, err := os.Stat(filepath.Join(dir, ".codegraph-initialized")); err != nil {
		t.Fatalf("workspace left uninitialized after warming: %v", err)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if !strings.Contains(string(calls), "status --json") || !strings.Contains(string(calls), "init") {
		t.Fatalf("codegraph calls = %q, want both status --json and init", calls)
	}

	// Warming the same directory again is a no-op re-index: codegraph runs status again, but
	// never a second init, since the marker already reports initialized.
	before := len(strings.Split(strings.TrimSpace(string(calls)), "\n"))
	WarmCodegraphIndex(context.Background(), dir)
	calls, err = os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log after the second warm-up: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != before+1 || !strings.HasPrefix(lines[len(lines)-1], "status --json\t") {
		t.Fatalf("codegraph calls after a second warm-up = %v, want exactly one more status --json call", lines)
	}
}

// TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing proves the acceptance criterion directly:
// warming logs its skip to stderr (never through Request.Log, which cmd/legion's exact stdout
// assertions own), so this only has to show nothing panics or leaves an index when codegraph is
// absent from PATH entirely — including on this devbox, which has a real one.
func TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing(t *testing.T) {
	dir := t.TempDir()
	// Narrowed to a fresh, empty directory so this test proves the missing-CLI path even on a
	// machine (like this one) that has a real `codegraph` on its ordinary PATH.
	t.Setenv("PATH", t.TempDir())

	WarmCodegraphIndex(context.Background(), dir)
	if _, err := os.Stat(filepath.Join(dir, ".codegraph-initialized")); !os.IsNotExist(err) {
		t.Fatalf("stat .codegraph-initialized = %v, want it absent with no codegraph on PATH", err)
	}
}

// TestWarmCodegraphIndexGivesCodegraphNoSecretLikeVariable proves the correctness/security finding
// directly: a provisioning caller's own process environment may hold a one-shot GitHub token or
// other secret (GH_TOKEN here, standing in for any of them), and codegraph must never see it —
// only the minimal PATH/HOME/DO_NOT_TRACK set codegraphEnvironment names.
func TestWarmCodegraphIndexGivesCodegraphNoSecretLikeVariable(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	stubCodegraph(t, callLog)
	t.Setenv("GH_TOKEN", "ghs_should-never-reach-codegraph")
	t.Setenv("LEGION_GRANT", "grant-should-never-reach-codegraph")
	dir := t.TempDir()

	WarmCodegraphIndex(context.Background(), dir)
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("codegraph was never invoked")
	}
	if strings.Contains(string(calls), "ghs_should-never-reach-codegraph") ||
		strings.Contains(string(calls), "grant-should-never-reach-codegraph") ||
		strings.Contains(string(calls), "GH_TOKEN=") || strings.Contains(string(calls), "LEGION_GRANT=") {
		t.Fatalf("codegraph's environment carried a secret-like variable: %s", calls)
	}
	if !strings.Contains(string(calls), "DO_NOT_TRACK=1") {
		t.Fatalf("codegraph's environment did not carry DO_NOT_TRACK=1: %s", calls)
	}
}
