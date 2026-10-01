package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubCodegraph puts a fake `codegraph` executable first on PATH. Its `status --json` reports
// uninitialized until `init` runs, which creates a `.codegraph/` directory in the directory it is
// run from — real codegraph's own side effect, which nextCodegraphStep's dirExists check reads —
// and reports initialized with a complete index from then on. It records every invocation's argv
// and environment (one call per line, `argv\tenv` with `env` entries joined by a space) to
// callLog so the test can see exactly what warmCodegraphIndex ran and with what environment.
func stubCodegraph(t *testing.T, callLog string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\t%s\n' "$*" "$(env | tr '\n' ' ')" >> '` + callLog + `'
marker=".codegraph/.codegraph-initialized"
case "$1" in
status)
  if [ -f "$marker" ]; then
    echo '{"initialized":true,"index":{"state":"complete"}}'
  else
    echo '{"initialized":false}'
  fi
  ;;
init)
  mkdir -p .codegraph
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

	warmCodegraphIndex(context.Background(), dir)
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", ".codegraph-initialized")); err != nil {
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
	warmCodegraphIndex(context.Background(), dir)
	calls, err = os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log after the second warm-up: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != before+1 || !strings.HasPrefix(lines[len(lines)-1], "status --json\t") {
		t.Fatalf("codegraph calls after a second warm-up = %v, want exactly one more status --json call", lines)
	}
}

// TestWarmCodegraphIndexInBackgroundNeverWaitsAndBuildsOncePerWorkspace pins what a launch needs:
// the call returns while `codegraph init` is still running, and a second call for the same
// workspace during that build starts no second one.
func TestWarmCodegraphIndexInBackgroundNeverWaitsAndBuildsOncePerWorkspace(t *testing.T) {
	scratch := t.TempDir()
	callLog := filepath.Join(scratch, "calls.log")
	release := filepath.Join(scratch, "release")
	bin := filepath.Join(scratch, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":false}' ;;
init) while [ ! -f '` + release + `' ] && [ -d '` + scratch + `' ]; do sleep 0.05; done ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub's held `codegraph init` exits once its scratch directory is removed, so no stub
	// loop is left running past this test regardless of how it ends.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()

	returned := make(chan struct{})
	go func() {
		WarmCodegraphIndexInBackground(dir)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WarmCodegraphIndexInBackground blocked for over 1s; it must return at once")
	}
	waitFor(t, func() bool {
		calls, _ := os.ReadFile(callLog)
		return strings.Contains(string(calls), "init")
	}, "codegraph init to start")
	WarmCodegraphIndexInBackground(dir)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, busy := warming.Load(dir)
		return !busy
	}, "the background warm-up to finish")
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "init"); got != 1 {
		t.Fatalf("codegraph init ran %d times for one workspace, want 1; calls: %q", got, calls)
	}
}

func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing proves the acceptance criterion directly:
// warming logs its skip to stderr, so this only has to show nothing panics or leaves an index
// when codegraph is absent from PATH entirely — including on this devbox, which has a real one.
func TestWarmCodegraphIndexNeverFailsWhenTheCliIsMissing(t *testing.T) {
	dir := t.TempDir()
	// Narrowed to a fresh, empty directory so this test proves the missing-CLI path even on a
	// machine (like this one) that has a real `codegraph` on its ordinary PATH.
	t.Setenv("PATH", t.TempDir())

	warmCodegraphIndex(context.Background(), dir)
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", ".codegraph-initialized")); !os.IsNotExist(err) {
		t.Fatalf("stat .codegraph/.codegraph-initialized = %v, want it absent with no codegraph on PATH", err)
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

	warmCodegraphIndex(context.Background(), dir)
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

// TestWarmCodegraphIndexRepairsAPartialIndex: an index an earlier warm-up left partial —
// `initialized:true` but `index.state` not `"complete"`, as a daemon exit or the 30-minute kill
// leaves it — is repaired with `codegraph index`, never accepted as built and never re-run
// through `codegraph init` (which would exit 0 printing "Already initialized" without touching
// the partial state).
func TestWarmCodegraphIndexRepairsAPartialIndex(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	warmCodegraphIndex(context.Background(), dir)
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status", "index"}) {
		t.Fatalf("codegraph calls = %v, want exactly status then index (never init)", lines)
	}
}

// TestWarmCodegraphIndexInitializesWhenNotInitializedDespiteAnExistingDirectory: a `.codegraph/`
// directory can exist with no database — CodeGraph's own generated `.codegraph/.gitignore` is
// `*` then `!.gitignore`, so that one file stays tracked, and a fresh workspace of a repository
// that committed it starts with the directory present and `status` reporting `initialized:false`.
// That must still run `init`, not `index` (which refuses: "CodeGraph not initialized").
func TestWarmCodegraphIndexInitializesWhenNotInitializedDespiteAnExistingDirectory(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codegraph", ".gitignore"), []byte("*\n!.gitignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":false}' ;;
index) echo 'CodeGraph not initialized' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	warmCodegraphIndex(context.Background(), dir)
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status", "init"}) {
		t.Fatalf("codegraph calls = %v, want exactly status then init (never index, which refuses on an untracked directory)", lines)
	}
}

// TestWarmCodegraphIndexInitializesWhenNotInitializedDespiteAnExistingDirectoryAgainstTheRealCli
// proves the same case against the real codegraph CLI (not a stub): `.codegraph/` holding only
// its own generated `.gitignore`, with no database. Devbox-only: the `daemon-go` CI job installs
// no `codegraph`, so this skips there and a green CI run is never evidence for this case; run it
// locally on a machine with the real CLI on PATH.
func TestWarmCodegraphIndexInitializesWhenNotInitializedDespiteAnExistingDirectoryAgainstTheRealCli(t *testing.T) {
	if _, err := exec.LookPath("codegraph"); err != nil {
		t.Skip("no codegraph CLI on PATH")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codegraph", ".gitignore"), []byte("*\n!.gitignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("console.log(1);\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	warmCodegraphIndex(context.Background(), dir)

	codegraphPath, err := exec.LookPath("codegraph")
	if err != nil {
		t.Fatal(err)
	}
	status, err := runCodegraph(context.Background(), codegraphPath, dir, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	initialized, complete := codegraphStatus(status.stdout)
	if !initialized || !complete {
		t.Fatalf("status after warming = %q, want initialized and complete", status.stdout)
	}
}

// TestWarmCodegraphIndexSkipsRepairWhileABuildIsLive: a status of `"indexing"` is also what a
// build still in progress reports, and `.codegraph/codegraph.lock` naming a live process whose
// `/proc/<pid>/cmdline` names codegraph must skip the repair entirely rather than run `codegraph
// index` alongside it.
func TestWarmCodegraphIndexSkipsRepairWhileABuildIsLive(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A process actually named "codegraph" in its own argv, so codegraphLockHeldByLiveProcess's
	// /proc/<pid>/cmdline check — the PID-reuse hardening — sees what a real builder's PID would.
	builderScript := filepath.Join(binDir, "codegraph-builder")
	if err := os.WriteFile(builderScript, []byte("#!/bin/bash\nexec -a codegraph sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	builder := exec.Command(builderScript)
	if err := builder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Process.Kill() })
	lock := filepath.Join(dir, ".codegraph", "codegraph.lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(builder.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	warmCodegraphIndex(context.Background(), dir)
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if !slices.Equal(lines, []string{"status"}) {
		t.Fatalf("codegraph calls = %v, want exactly status alone (no index or init while the lock names a live process)", lines)
	}
}

// TestWarmCodegraphIndexRepairsOncePerWorkspace: a workspace whose index never reaches
// `"complete"` gets re-indexed at most once per process, not on every call.
func TestWarmCodegraphIndexRepairsOncePerWorkspace(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	warmCodegraphIndex(context.Background(), dir)
	warmCodegraphIndex(context.Background(), dir)
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	if got := strings.Count(string(calls), "index"); got != 1 {
		t.Fatalf("codegraph index ran %d times across two warm-ups of the same workspace, want 1; calls: %q", got, calls)
	}
}

// TestNextCodegraphStep table-tests the pure decision warmCodegraphIndex runs on: a parsed
// status decides outright (its `initialized:false` always means init, even when `.codegraph/`
// exists), and dirExists is consulted only when status failed or didn't parse. Whether
// `.codegraph/codegraph.lock` names a live process is checked separately by the caller, not by
// this function (TestWarmCodegraphIndexSkipsRepairWhileABuildIsLive covers that).
func TestNextCodegraphStep(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dirExists      bool
		statusExitCode int
		statusStdout   string
		want           codegraphStep
	}{
		{"status valid, not initialized, no directory: init", false, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, not initialized, directory exists: init (untracked .codegraph)", true, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, complete: none", true, 0, `{"initialized":true,"index":{"state":"complete"}}`, codegraphStepNone},
		{"status valid, partial: index", true, 0, `{"initialized":true,"index":{"state":"indexing"}}`, codegraphStepIndex},
		{"status failed, directory exists: index", true, 1, ``, codegraphStepIndex},
		{"status failed, no directory: init", false, 1, ``, codegraphStepInit},
		{"status unparseable, directory exists: index", true, 0, `not json`, codegraphStepIndex},
		{"status unparseable, no directory: init", false, 0, `not json`, codegraphStepInit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextCodegraphStep(tc.dirExists, tc.statusExitCode, tc.statusStdout); got != tc.want {
				t.Fatalf("nextCodegraphStep(%v, %d, %q) = %v, want %v", tc.dirExists, tc.statusExitCode, tc.statusStdout, got, tc.want)
			}
		})
	}
}

// TestWarmCodegraphIndexAcrossPodsRunsExactlyOneBuildWhenTwoPodsRace proves the cross-pod
// exclusion the flock exists for: two concurrent calls to warmCodegraphIndexAcrossPods for the
// same workspace directory — the inner function, bypassing WarmCodegraphIndexInPodBackground's
// own in-process `warming` map entirely, exactly as two sibling pods of one issue tree would
// (each its own process, each with its own empty dedup map) — run `codegraph init` exactly
// once; the second's flock attempt fails while the first still holds it, and it returns without
// calling codegraph at all.
func TestWarmCodegraphIndexAcrossPodsRunsExactlyOneBuildWhenTwoPodsRace(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	binDir := t.TempDir()
	release := filepath.Join(binDir, "release")
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo '{"initialized":false}' ;;
init) while [ ! -f '` + release + `' ]; do sleep 0.05; done ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			warmCodegraphIndexAcrossPods(context.Background(), dir)
		}()
	}
	// Lets the one that wins the flock reach its blocking `init` before releasing it: the
	// loser's whole path (open, fail the non-blocking flock, log, return) is near-instant next
	// to that, so by the time status has been logged once, the loser has already tried and
	// failed.
	waitFor(t, func() bool {
		calls, _ := os.ReadFile(callLog)
		return strings.Contains(string(calls), "status")
	}, "the winning pod's codegraph status to run")
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if got := strings.Count(string(calls), "init\n"); got != 1 {
		t.Fatalf("codegraph init ran %d times across two racing pods, want exactly 1; calls: %q", got, lines)
	}
	if got := strings.Count(string(calls), "status\n"); got != 1 {
		t.Fatalf("codegraph status ran %d times across two racing pods, want exactly 1 (the loser's flock attempt must fail before it ever calls codegraph); calls: %q", got, lines)
	}
}
