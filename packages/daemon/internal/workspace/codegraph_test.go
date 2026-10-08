package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectory: `.codegraph/` can exist
// without a database — CodeGraph's own generated `.codegraph/.gitignore` is `*` then
// `!.gitignore`, so that one file stays tracked, and a fresh workspace of a repository that
// committed it starts with the directory present and `status` reporting `initialized:false`.
// That must still run `init`, not `index` (which refuses: "CodeGraph not initialized"), and must
// not consume the once-per-workspace repair budget.
func TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectory(t *testing.T) {
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

// TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectoryAgainstTheRealCli proves the
// same case against the real codegraph CLI (not a stub): `.codegraph/` holding only its own
// generated `.gitignore`, with no database.
func TestWarmCodegraphIndexInitializesAnUntrackedCodegraphDirectoryAgainstTheRealCli(t *testing.T) {
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
	// Start returns once execve has closed the child's close-on-exec pipe, before the kernel has
	// laid out the new argv: /proc/<pid>/cmdline reads empty until it has, and again across the
	// script's own `exec -a`, and an empty cmdline reads as not codegraph. A real builder writes its
	// PID to the lock from its own running code, after its exec has finished, so the lock is written
	// only once the stand-in is there too: argv[0] is codegraph.
	waitFor(t, func() bool {
		cmdline, _ := os.ReadFile("/proc/" + strconv.Itoa(builder.Process.Pid) + "/cmdline")
		argv0, _, _ := strings.Cut(string(cmdline), "\x00")
		return argv0 == "codegraph"
	}, "the stand-in builder to exec as codegraph")
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

// TestWarmCodegraphIndexJudgesAnEmptyLockByItsAge: CodeGraph creates `.codegraph/codegraph.lock`
// before it writes the builder's PID into it, so a build that has only just started shows an
// empty lock. A fresh empty lock is held, and warming skips the repair rather than start a second
// writer beside that build. One older than codegraphEmptyLockGrace is what a builder that died
// between the create and the write leaves behind: stale, so the repair runs. So is one dated
// further than the grace into the future, which no starting build wrote.
func TestWarmCodegraphIndexJudgesAnEmptyLockByItsAge(t *testing.T) {
	for _, tc := range []struct {
		name string
		// age is how old the lock's mtime is when warming reads it; negative is in the future.
		// Zero means the stub's own `status` creates the lock, as a build taking it while warming
		// runs would, so the lock is as young as it can be when warming reads it.
		age  time.Duration
		want []string
	}{
		{"fresh empty lock: a build about to write its PID holds it, no repair", 0, []string{"status"}},
		{"empty lock older than the grace: a dead builder's, repaired", codegraphEmptyLockGrace + time.Minute, []string{"status", "index"}},
		{"empty lock dated past the grace in the future: stale, repaired", -codegraphEmptyLockGrace - time.Minute, []string{"status", "index"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callLog := filepath.Join(t.TempDir(), "calls.log")
			dir := t.TempDir()
			lock := filepath.Join(dir, ".codegraph", "codegraph.lock")
			if err := os.Mkdir(filepath.Dir(lock), 0o700); err != nil {
				t.Fatal(err)
			}
			takeLock := ""
			if tc.age == 0 {
				takeLock = " : > .codegraph/codegraph.lock;"
			} else {
				if err := os.WriteFile(lock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := time.Now().Add(-tc.age)
				if err := os.Chtimes(lock, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			binDir := t.TempDir()
			script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status)` + takeLock + ` echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
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
			if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); !slices.Equal(lines, tc.want) {
				t.Fatalf("codegraph calls = %v, want %v", lines, tc.want)
			}
		})
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

// stubCalls is the argv of every invocation stubCodegraph recorded, in order; nil when it was
// never run.
func stubCalls(t *testing.T, callLog string) []string {
	t.Helper()
	calls, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read codegraph call log: %v", err)
	}
	var argv []string
	for line := range strings.Lines(strings.TrimSpace(string(calls))) {
		args, _, _ := strings.Cut(line, "\t")
		argv = append(argv, args)
	}
	return argv
}

// TestWarmCodegraphIndexHoldsACrossProcessLease: the six role shims of one issue pod, and a
// draining pod beside its replacement, each warm the one workspace they share, and only one may
// build. `.codegraph/legion-warm.lock` settles it across processes, where `warming` cannot: a
// fresh lease (its holder's heartbeat within warmLeaseStale) makes a warm-up skip without even
// running `status`, and leaves the file to its holder; one older than warmLeaseStale is a dead
// holder's, taken over and built behind, with nothing of the takeover left in the directory; and a
// warm-up that ran leaves no lease behind for the next to find.
func TestWarmCodegraphIndexHoldsACrossProcessLease(t *testing.T) {
	leaseOf := func(t *testing.T, dir string, age time.Duration) string {
		t.Helper()
		lease := filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lease, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-age)
		if err := os.Chtimes(lease, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	leaseFiles := func(t *testing.T, dir string) []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, ".codegraph"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), warmLeaseName) {
				names = append(names, entry.Name())
			}
		}
		return names
	}

	t.Run("a fresh lease held by another process skips the warm-up", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		lease := leaseOf(t, dir, 0)

		warmCodegraphIndex(context.Background(), dir)
		if calls := stubCalls(t, callLog); calls != nil {
			t.Fatalf("codegraph calls = %v, want none while another process holds the lease", calls)
		}
		if _, err := os.Stat(lease); err != nil {
			t.Fatalf("the holder's lease: %v, want it left in place", err)
		}
	})
	t.Run("a stale lease is taken over and the build runs", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()
		leaseOf(t, dir, warmLeaseStale+time.Minute)

		warmCodegraphIndex(context.Background(), dir)
		if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status --json", "init"}) {
			t.Fatalf("codegraph calls = %v, want status --json then init behind the taken-over lease", calls)
		}
		if left := leaseFiles(t, dir); left != nil {
			t.Fatalf(".codegraph/ holds %v after the warm-up, want neither the lease nor the takeover marker", left)
		}
	})
	t.Run("the lease is released after a run", func(t *testing.T) {
		callLog := filepath.Join(t.TempDir(), "calls.log")
		stubCodegraph(t, callLog)
		dir := t.TempDir()

		warmCodegraphIndex(context.Background(), dir)
		if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status --json", "init"}) {
			t.Fatalf("codegraph calls = %v, want status --json then init", calls)
		}
		if left := leaseFiles(t, dir); left != nil {
			t.Fatalf(".codegraph/ holds %v after the warm-up, want the lease released", left)
		}
	})
}

// TestTwoContendersThatJudgedOneLeaseStaleCannotBothHoldIt drives the interleavings a scheduler
// rarely produces, through the warmLeaseStep seam: two warm-ups (two role containers relaunching
// into one workspace after a SIGKILLed pod) both read the same stale lease. Whichever order they
// then act in, exactly one holds the lease afterwards and the other skips, so one build runs:
// (1) B judges the lease stale, then A takes it over, holds, and starts building before B acts —
// B's takeover must not touch A's fresh lease, which a rename of "whatever is at the path" did;
// (2) B is inside its takeover, the marker held, when A judges the same lease stale — A finds the
// marker and leaves the takeover to B. A dead contender's marker, older than warmLeaseStale, is
// removed and the takeover proceeds; a lease released between the stat and the takeover gives the
// contender its retry rather than a skip.
func TestTwoContendersThatJudgedOneLeaseStaleCannotBothHoldIt(t *testing.T) {
	staleLease := func(t *testing.T) (dir, lease string) {
		t.Helper()
		dir = t.TempDir()
		lease = filepath.Join(dir, ".codegraph", warmLeaseName)
		if err := os.Mkdir(filepath.Dir(lease), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lease, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-warmLeaseStale - time.Minute)
		if err := os.Chtimes(lease, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return dir, lease
	}
	// at runs f once, when acquireWarmLease reaches step. The seam is cleared before f runs, so the
	// contender f itself runs — which reaches the same steps — is not caught by it again.
	at := func(t *testing.T, step string, f func()) {
		t.Helper()
		warmLeaseStep = func(s string) {
			if s == step {
				warmLeaseStep = nil
				f()
			}
		}
		t.Cleanup(func() { warmLeaseStep = nil })
	}
	holdsAlone := func(t *testing.T, lease string, releaseA, releaseB func(), heldA, heldB bool) {
		t.Helper()
		if heldA == heldB {
			t.Fatalf("A holds = %v, B holds = %v; want exactly one holder", heldA, heldB)
		}
		if _, err := os.Stat(lease); err != nil {
			t.Fatalf("the lease: %v, want the holder's lease in place", err)
		}
		if _, err := os.Stat(lease + ".takeover"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the takeover marker: %v, want it gone once the takeover ended", err)
		}
		for _, release := range []func(){releaseA, releaseB} {
			if release != nil {
				release()
			}
		}
		if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the lease after the holder's release: %v, want it gone", err)
		}
	}

	t.Run("the first to act holds; the second, judging the same stale lease, finds a fresh one", func(t *testing.T) {
		dir, lease := staleLease(t)
		var releaseA func()
		var heldA bool
		at(t, "judged stale", func() { releaseA, heldA = acquireWarmLease(dir, lease) })
		releaseB, heldB := acquireWarmLease(dir, lease)
		if !heldA || heldB {
			t.Fatalf("A holds = %v, B holds = %v; want A, which acted first, to hold and B to skip", heldA, heldB)
		}
		holdsAlone(t, lease, releaseA, releaseB, heldA, heldB)
	})
	t.Run("a contender inside its takeover holds the marker; the other leaves the takeover to it", func(t *testing.T) {
		dir, lease := staleLease(t)
		var releaseA func()
		var heldA bool
		at(t, "marker held", func() { releaseA, heldA = acquireWarmLease(dir, lease) })
		releaseB, heldB := acquireWarmLease(dir, lease)
		if heldA || !heldB {
			t.Fatalf("A holds = %v, B holds = %v; want B, whose takeover was under way, to hold and A to skip", heldA, heldB)
		}
		holdsAlone(t, lease, releaseA, releaseB, heldA, heldB)
	})
	t.Run("a dead contender's marker is removed and the takeover proceeds", func(t *testing.T) {
		dir, lease := staleLease(t)
		marker := lease + ".takeover"
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-warmLeaseStale - time.Minute)
		if err := os.Chtimes(marker, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		release, held := acquireWarmLease(dir, lease)
		if !held {
			t.Fatal("the contender skipped behind a dead contender's marker, want it to take the lease over")
		}
		holdsAlone(t, lease, release, nil, held, false)
	})
	t.Run("a live contender's marker makes the other skip", func(t *testing.T) {
		dir, lease := staleLease(t)
		if err := os.WriteFile(lease+".takeover", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if release, held := acquireWarmLease(dir, lease); held {
			release()
			t.Fatal("the contender took the lease over behind a live contender's marker")
		}
	})
	t.Run("a lease released between the stat and the takeover is retried, not skipped", func(t *testing.T) {
		dir, lease := staleLease(t)
		at(t, "judged stale", func() {
			if err := os.Remove(lease); err != nil {
				t.Fatal(err)
			}
		})
		release, held := acquireWarmLease(dir, lease)
		if !held {
			t.Fatal("the contender skipped after the holder released, want it to take the free lease")
		}
		holdsAlone(t, lease, release, nil, held, false)
	})
}

// TestWarmCodegraphIndexStillInitializesAFreshWorkspaceWhenStatusFails: the lease lives under
// `.codegraph/`, so the warm-up makes that directory before `status` runs, and nextCodegraphStep's
// fallback for a `status` that failed reads an existing directory as an index to repair. A
// workspace that had no `.codegraph/` before the warm-up must still be judged directory-less
// there: `init`, never `index`, which refuses on an uninitialized workspace.
func TestWarmCodegraphIndexStillInitializesAFreshWorkspaceWhenStatusFails(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$1" >> '` + callLog + `'
case "$1" in
status) echo 'database disk image is malformed' >&2; exit 1 ;;
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
		t.Fatalf("codegraph calls = %v, want exactly status then init (the lease's directory must not read as an index to repair)", lines)
	}
}

// TestNextCodegraphStep table-tests the pure decision warmCodegraphIndex runs on: a parsed
// status decides outright (its `initialized:false` always means init, even when `.codegraph/`
// exists); dirExists is consulted only when status failed or didn't parse; and a live lock
// always wins over an index repair.
func TestNextCodegraphStep(t *testing.T) {
	for _, tc := range []struct {
		name                string
		dirExists, lockLive bool
		statusExitCode      int
		statusStdout        string
		want                codegraphStep
	}{
		{"status valid, not initialized, no directory: init", false, false, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, not initialized, directory exists: init (untracked .codegraph)", true, false, 0, `{"initialized":false}`, codegraphStepInit},
		{"status valid, complete: none", true, false, 0, `{"initialized":true,"index":{"state":"complete"}}`, codegraphStepNone},
		{"status valid, complete, lock live: none (complete wins, lock irrelevant)", true, true, 0, `{"initialized":true,"index":{"state":"complete"}}`, codegraphStepNone},
		{"status valid, partial, no live lock: index", true, false, 0, `{"initialized":true,"index":{"state":"indexing"}}`, codegraphStepIndex},
		{"status valid, partial, live lock: none", true, true, 0, `{"initialized":true,"index":{"state":"indexing"}}`, codegraphStepNone},
		{"status failed, directory exists, no live lock: index", true, false, 1, ``, codegraphStepIndex},
		{"status failed, directory exists, live lock: none", true, true, 1, ``, codegraphStepNone},
		{"status failed, no directory: init", false, false, 1, ``, codegraphStepInit},
		{"status unparseable, directory exists: index", true, false, 0, `not json`, codegraphStepIndex},
		{"status unparseable, no directory: init", false, false, 0, `not json`, codegraphStepInit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextCodegraphStep(tc.dirExists, tc.lockLive, tc.statusExitCode, tc.statusStdout); got != tc.want {
				t.Fatalf("nextCodegraphStep(%v, %v, %d, %q) = %v, want %v", tc.dirExists, tc.lockLive, tc.statusExitCode, tc.statusStdout, got, tc.want)
			}
		})
	}
}
