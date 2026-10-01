package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// codegraphTimeout bounds each codegraph invocation independently. The warm-up runs only in the
// background, so nothing waits on it; a first index of a large repository (tens of thousands of
// files) takes longer than Provision's own command budget.
const codegraphTimeout = 30 * time.Minute

// warming holds the workspace directories with a background warm-up in flight in this process.
var warming sync.Map

// indexed holds the workspace directories where this process has already attempted a
// `codegraph index` repair, so a build that never reaches `"complete"` (a `"partial"` or
// `"failed"` result that recurs, or a status this daemon can't parse) gets re-indexed at most
// once per workspace per process rather than on every later spawn.
var indexed sync.Map

// WarmCodegraphIndexInBackground starts warmCodegraphIndex for dir in its own goroutine and
// returns at once, so no launch ever waits on an index build; a second call for a directory whose
// warm-up is still running does nothing. Only a long-lived process calls it (the Go daemon's host
// provisioning for tmux panes, internal/daemon/outbox.go): the pod init container builds no index,
// since it runs on the pod's registration path and its goroutines die with it.
func WarmCodegraphIndexInBackground(dir string) {
	startCodegraphWarmup(dir, warmCodegraphIndex)
}

// WarmCodegraphIndexInPodBackground is the worker-shim's pod entry point (internal/shim),
// started once a pod's OMP child has actually begun, never before: it is warmCodegraphIndex
// behind the cross-pod flock warmCodegraphIndexAcrossPods takes, since sibling phase-worker pods
// of one issue tree share the tree-volume workspace directory.
func WarmCodegraphIndexInPodBackground(dir string) {
	startCodegraphWarmup(dir, warmCodegraphIndexAcrossPods)
}

// startCodegraphWarmup is WarmCodegraphIndexInBackground's and
// WarmCodegraphIndexInPodBackground's shared goroutine and de-duplication: run starts in its own
// goroutine and returns at once, and a second call for a directory whose warm-up is still in
// flight in this process does nothing.
func startCodegraphWarmup(dir string, run func(context.Context, string)) {
	if _, busy := warming.LoadOrStore(dir, struct{}{}); busy {
		return
	}
	go func() {
		defer warming.Delete(dir)
		run(context.Background(), dir)
	}()
}

// warmCodegraphIndexAcrossPods is warmCodegraphIndex's pod entry point: sibling phase-worker
// pods of one issue tree share the same tree-volume workspace directory but run in separate PID
// namespaces, so codegraphLockHeldByLiveProcess's PID check (kill(pid, 0) against this process's
// own namespace) can never see another pod's builder — a live build in a sibling pod would read
// as dead, and the repair would run `codegraph index` alongside it, corrupting the database the
// same way an unguarded live-build race does on a single host (codegraph_test.go). This takes a
// non-blocking flock on `<dir>+".codegraph.lock"` — a sibling of the workspace directory,
// outside the jj working copy, so it is never auto-tracked, and distinct from workspace-init's
// own per-repository `<clone>.lock` — before running the warm-up at all, and skips entirely,
// logging once, if another pod already holds it. The lock is held for the warm-up's whole
// duration: the same live-holder-holds, dead-holder-holds-nothing contract as workspace-init's
// own flock (no lease, no mtime, no takeover — a crashed pod's lock dies with its last file
// descriptor, and the kernel drops it).
func warmCodegraphIndexAcrossPods(ctx context.Context, dir string) {
	lockPath := dir + ".codegraph.lock"
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s could not open its cross-pod lock %s: %s\n", dir, lockPath, err)
		return
	}
	defer file.Close()
	if err := flockNonBlocking(int(file.Fd())); err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: another pod holds %s\n", dir, lockPath)
		return
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()
	warmCodegraphIndex(ctx, dir)
}

// flockNonBlocking is flock(2) LOCK_EX|LOCK_NB, retried only across EINTR — the same pattern as
// workspace-init's own non-blocking flock attempt (cmd/legion/workspace_init.go's flock).
func flockNonBlocking(fd int) error {
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// warmCodegraphIndex mirrors the TypeScript daemon's ensureCodegraphIndex (research report
// AGENTC-1305 §7): the tester's `affected` and the reviewer's `impact`/`callers` queries need an
// index already built, not one built on first use. Callers run it through
// WarmCodegraphIndexInBackground, outside any provisioning serialization: indexing reads only the
// issue's own workspace directory. codegraph is deliberately not one of the tools Runner
// requires — the daemon must still boot where it is absent (the tmux runtime's host provisioning
// never installs it) — so this resolves it from PATH on its own, directly with os/exec, and never
// fails provisioning: a missing CLI, a non-zero exit, or a timeout is logged loudly to stderr,
// exactly as the TypeScript daemon's console.error does. Warming simply does not happen when the
// CLI is missing or fails; the worker falls back to grep, per its role prompt. An index an
// earlier warm-up left partial (`initialized` true but `index.state` not `"complete"`) is
// repaired with `codegraph index` rather than accepted as built — `codegraph init` on an
// already-initialized directory only prints "Already initialized" and exits 0, so it cannot do
// this repair itself — unless the build that left it that way is still running: nextCodegraphStep
// skips the repair entirely while `.codegraph/codegraph.lock` names a live process. A second
// `codegraph index` started against a live build damages the shared SQLite database even when
// CodeGraph's own lock refuses it the write (observed: the second process exits on "Could not
// acquire file lock", and the live build still fails with "database disk image is malformed") —
// so this never lets a second process even attempt it, and never relies on CodeGraph's own
// mtime-based (2-minute) staleness check, which can hand the lock to a second writer regardless.
// Every codegraph invocation gets a minimal, explicit environment — PATH, HOME, TMPDIR when set,
// and DO_NOT_TRACK=1 — never the process's full environment: a provisioning caller may hold a
// one-shot GitHub token or other secrets in its own environment, and codegraph gets none of them.
// DO_NOT_TRACK=1 disables both CodeGraph's telemetry and its update check (its bundled docs rank
// DO_NOT_TRACK above CODEGRAPH_TELEMETRY above stored config above default-on), so an automatic,
// non-opt-in warm-up never phones home.
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
	dirExists, err := pathExists(filepath.Join(dir, ".codegraph"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	step := nextCodegraphStep(dirExists, status.exitCode, status.stdout)
	if step == codegraphStepNone {
		return
	}
	// The lock read and the kill(pid, 0) syscall are worth paying for only once a write step
	// (init or index) is actually about to run: a complete index already returned above, so
	// this can never log a misleading "skipped" line on an index that needed no repair.
	if codegraphLockHeldByLiveProcess(dir) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: the index lock still names a live process\n", dir)
		return
	}
	subcommand := "init"
	if step == codegraphStepIndex {
		if _, already := indexed.LoadOrStore(dir, struct{}{}); already {
			return
		}
		subcommand = "index"
	}
	result, err := runCodegraph(ctx, codegraphPath, dir, subcommand)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	if result.exitCode != 0 {
		fmt.Fprintf(os.Stderr, "[legion] codegraph %s failed for %s (exit %d): %s\n", subcommand, dir, result.exitCode, strings.TrimSpace(result.stderr))
	}
}

// codegraphStep is the single next codegraph subcommand nextCodegraphStep decides, or none.
type codegraphStep int

const (
	codegraphStepNone codegraphStep = iota
	codegraphStepInit
	codegraphStepIndex
)

// nextCodegraphStep decides warmCodegraphIndex's one next subcommand, pure and table-tested
// (codegraph_test.go). When `status` exits 0 and parses as JSON, its own `initialized` and
// `index.state` fields decide outright: `initialized: false` always means `init`, even when
// `.codegraph/` already exists — CodeGraph's own `.gitignore` template (`*` then `!.gitignore`)
// leaves one tracked file behind in every `.codegraph/` a repository commits, so a fresh
// workspace of such a repository starts with the directory present and no database, and `index`
// on that refuses ("CodeGraph not initialized"). dirExists — whether `<workspace>/.codegraph`
// exists on disk — is consulted only as a fallback, when `status` failed to run or returned
// something that is not even JSON (a corrupted database mid-build, an old CLI): there, an
// existing directory still means `index` over `init`, since `init` on one only prints "Already
// initialized" and exits 0, repairing nothing. Whether `.codegraph/codegraph.lock` names a live
// process is the caller's concern, not this function's: it decides only which write step the
// status and the directory call for, and the caller skips that step entirely, for either
// `init` or `index`, while the lock names a live process.
func nextCodegraphStep(dirExists bool, statusExitCode int, statusStdout string) codegraphStep {
	if statusExitCode == 0 && json.Valid([]byte(statusStdout)) {
		initialized, complete := codegraphStatus(statusStdout)
		switch {
		case !initialized:
			return codegraphStepInit
		case complete:
			return codegraphStepNone
		default:
			return codegraphStepIndex
		}
	}
	if !dirExists {
		return codegraphStepInit
	}
	return codegraphStepIndex
}

// codegraphStatus reads `codegraph status --json`'s `initialized` and `index.state` fields
// exactly as the TypeScript daemon's parseCodegraphStatus does: `codegraph status` exits 0
// whether or not the project has ever been indexed, so only the parsed body tells the cases
// apart, and anything that fails to parse as that shape is treated as not initialized.
// `index.state` is `"complete"` only once a build has finished; a build still running, or one an
// earlier warm-up left partial, reports it `"indexing"` with `initialized` already true.
func codegraphStatus(stdout string) (initialized, complete bool) {
	var parsed struct {
		Initialized bool `json:"initialized"`
		Index       struct {
			State string `json:"state"`
		} `json:"index"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return false, false
	}
	return parsed.Initialized, parsed.Index.State == "complete"
}

// codegraphLockHeldByLiveProcess reports whether dir's .codegraph/codegraph.lock names a process
// that is still alive. CodeGraph 1.5.0's FileLock (its installed dist's utils.js) writes the
// lock's entire content as the builder's decimal PID with no other metadata, and its own
// staleness check compares the lock file's mtime against a fixed 2-minute timeout rather than
// checking the PID, so a lock can still name a live, actively-writing process past that window.
// A lock this cannot read, or whose content isn't a PID, is treated as not live — this never
// blocks a repair on a lock it cannot make sense of. Liveness alone is not enough on a host where
// PIDs recycle: a dead builder's PID reused by an unrelated process would read as live forever,
// so where `/proc/<pid>/cmdline` exists, the process also has to look like codegraph — a dead
// builder whose PID is unused, or whose slot now holds something else, is correctly stale.
// `/proc` absent (non-Linux) falls back to the liveness check alone.
func codegraphLockHeldByLiveProcess(dir string) bool {
	content, err := os.ReadFile(filepath.Join(dir, ".codegraph", "codegraph.lock"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil || pid <= 0 {
		return false
	}
	// Signal 0 sends nothing; a nil error means the process exists and is signalable, and EPERM
	// means it exists but belongs to another user — both are live. Any other error (typically
	// ESRCH) means it is gone.
	sigErr := syscall.Kill(pid, 0)
	if sigErr != nil && !errors.Is(sigErr, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// No /proc entry at all (process gone between the signal and this read, or a non-Linux
		// host where /proc never exists): the signal result is all there is to go on.
		return true
	}
	return strings.Contains(string(cmdline), "codegraph")
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
