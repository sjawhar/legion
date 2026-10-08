package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// codegraphEmptyLockGrace is how long an empty `.codegraph/codegraph.lock` still counts as held,
// either side of now. CodeGraph's FileLock takes the lock with one `fs.writeFileSync(lockPath,
// String(process.pid), { flag: 'wx' })`: an exclusive create, then the PID write, two syscalls
// back to back. For a moment the file exists and is empty. Timed on a 32-core host at load average
// 130–145, that whole call took about 11 µs at the median and 107 ms at worst, so five seconds
// is about 50 times the worst stall. An empty lock older than that is what a builder leaves when
// it dies between the two calls, or when its write fails (ENOSPC); one dated further than that
// into the future is no starting build's. Both are stale. Nothing reruns a warm-up that skipped
// its repair for a held lock: the repair waits for the next claim's provisioning of the workspace.
const codegraphEmptyLockGrace = 5 * time.Second

// warmLeaseHeartbeat is how often a warm-up holding `.codegraph/legion-warm.lock` sets the lease's
// mtime to now, and warmLeaseStale is how old that mtime may be before another process reads the
// holder as dead and takes the lease over: six missed heartbeats, so a holder stalled through one
// slow tick or a filesystem hiccup keeps its lease, while a dead one (a shim killed outright
// mid-build, a pod that went away; a SIGTERMed shim's `codegraph` child dies with its process group
// and the deferred release runs while the shim winds its Oh My Pi down) leaves a lease that a
// warm-up within the minute still reads as live and skips, and the first one after that takes over.
// The lease is the one cross-process guard here; everything else is this process's. `warming` is a
// map in this process.
// nextCodegraphStep lets a live CodeGraph lock veto a repair but not an `init`: an uninitialized
// workspace has no `codegraph.lock` to read, so two processes that both find it uninitialized would
// both run `init`. codegraphLockHeldByLiveProcess judges liveness by PID, which another
// container's PID namespace makes meaningless; it stays as it is for the one-process case. The
// contenders are the six role containers of one issue pod, which share the issue's workspace and
// each warm it once their Oh My Pi starts (`legion worker-shim --warm-codegraph`), and a draining
// pod overlapping its replacement. flock would not do: under gVisor a pod's flock never reaches
// another pod (initWaitSeconds, internal/runtime/sandbox/manifest.go), where a file's existence
// and mtime on the shared volume do. So the lease is an exclusive create its holder keeps fresh,
// and a stale one is taken over by a rename, the one step exactly one of two contenders wins.
const (
	warmLeaseHeartbeat = 10 * time.Second
	warmLeaseStale     = 60 * time.Second
)

// warmLeaseName is the lease file a warm-up holds under `.codegraph/`, beside the index itself, so
// every process that shares the index shares the lease; CodeGraph's own `.gitignore` there (`*`)
// keeps it out of the repository.
const warmLeaseName = "legion-warm.lock"

// warming holds the workspace directories with a background warm-up in flight in this process.
var warming sync.Map

// indexed holds the workspace directories where this process has already attempted a
// `codegraph index` repair, so a build that never reaches `"complete"` (a `"partial"` or
// `"failed"` result that recurs, or a status this daemon can't parse) gets re-indexed at most
// once per workspace per process rather than on every later spawn.
var indexed sync.Map

// WarmCodegraphIndexInBackground starts warmCodegraphIndex for dir in its own goroutine and
// returns at once, so no launch ever waits on an index build; a second call for a directory whose
// warm-up is still running in this process does nothing, and one running in another process is
// what the lease settles (warmLeaseHeartbeat). A long-lived process calls it: the daemon's host
// provisioning for tmux panes (internal/daemon/outbox.go) and a role's `legion worker-shim
// --warm-codegraph` once Oh My Pi is spawned (internal/shim's Config.WarmCodegraph). The pod init
// container still builds no index: it runs on the pod's registration path and its goroutines die
// with it.
func WarmCodegraphIndexInBackground(dir string) {
	if _, busy := warming.LoadOrStore(dir, struct{}{}); busy {
		return
	}
	go func() {
		defer warming.Delete(dir)
		warmCodegraphIndex(context.Background(), dir)
	}()
}

// warmCodegraphIndex builds a workspace's codegraph index before any worker needs it: the tester's
// `affected` and the reviewer's `impact`/`callers` queries need an index already built, not one
// built on first use. Callers run it through WarmCodegraphIndexInBackground, outside any
// provisioning serialization: indexing reads only the issue's own workspace directory. codegraph
// is deliberately not one of the tools Runner requires — the daemon must still boot where it is
// absent (the tmux runtime's host provisioning never installs it) — so this resolves it from PATH
// on its own, directly with os/exec, and never fails provisioning: a missing CLI, a non-zero exit,
// or a timeout is logged loudly to stderr. Warming simply does not happen when the CLI is missing
// or fails; the worker falls back to grep, per its role prompt. An index an earlier warm-up left
// partial (`initialized` true but `index.state` not `"complete"`) is repaired with `codegraph
// index` rather than accepted as built — `codegraph init` on an already-initialized directory only
// prints "Already initialized" and exits 0, so it cannot do this repair itself — unless the build
// that left it that way is still running: nextCodegraphStep skips the repair entirely while a
// build holds `.codegraph/codegraph.lock`. A second `codegraph index` started against a live build
// damages the shared SQLite database even when CodeGraph's own lock refuses it the write
// (observed: the second process exits on "Could not acquire file lock", and the live build still
// fails with "database disk image is malformed") — so this never lets a second process even
// attempt it, and never relies on CodeGraph's own mtime-based (2-minute) staleness check, which
// can hand the lock to a second writer regardless. The whole warm-up, `status` included, runs
// under the workspace's cross-process lease (acquireWarmLease, warmLeaseHeartbeat): a second
// process that finds the lease held leaves the build to its holder.
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
	// dirExists is read before the lease's directory is made: nextCodegraphStep's fallback (a
	// status that failed or didn't parse) reads an existing `.codegraph/` as an index to repair,
	// and a workspace never initialized must still read as directory-less there. `codegraph init`
	// tolerates the directory — a committed `.codegraph/.gitignore` leaves it present anyway.
	codegraphDir := filepath.Join(dir, ".codegraph")
	dirExists, err := pathExists(codegraphDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	if !dirExists {
		if err := os.Mkdir(codegraphDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
			return
		}
	}
	release, held := acquireWarmLease(dir, filepath.Join(codegraphDir, warmLeaseName))
	if !held {
		return
	}
	defer release()
	status, err := runCodegraph(ctx, codegraphPath, dir, "status", "--json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	// The lock read and the kill(pid, 0) syscall below are worth paying for only when a repair
	// might actually run: a complete index already needs neither, and logging a "skipped" line
	// for a lock some unrelated codegraph run holds at that moment, on an index that needed no
	// repair, would be misleading.
	complete := status.exitCode == 0 && json.Valid([]byte(status.stdout))
	if complete {
		initialized, indexComplete := codegraphStatus(status.stdout)
		complete = initialized && indexComplete
	}
	lockLive := dirExists && !complete && codegraphLockHeldByLiveProcess(dir)
	step := nextCodegraphStep(dirExists, lockLive, status.exitCode, status.stdout)
	if step == codegraphStepNone {
		if lockLive {
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: a build in progress holds the index lock\n", dir)
		}
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

// acquireWarmLease takes dir's warm-up lease, the file at path, or reports that another process
// holds it. An exclusive create either wins the lease or finds a holder's file. One whose mtime is
// within warmLeaseStale is a live warm-up elsewhere, and the caller leaves the build to it. An
// older one is a dead holder's: it is taken over by renaming it aside — the one step two
// contenders cannot both win, the loser's rename failing with ENOENT — and the create is tried
// once more, an EEXIST then being a contender that got there first. A file gone between the
// failed create and its stat was just released, and gets the same one retry.
func acquireWarmLease(dir, path string) (release func(), held bool) {
	skipped := func(what string) (func(), bool) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: another process %s %s\n", dir, what, path)
		return nil, false
	}
	failed := func(err error) (func(), bool) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return nil, false
	}
	for attempt := 0; ; attempt++ {
		lease, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			created, err := lease.Stat()
			_ = lease.Close()
			if err != nil {
				_ = os.Remove(path)
				return failed(err)
			}
			return holdWarmLease(path, created), true
		}
		if !errors.Is(err, os.ErrExist) {
			return failed(err)
		}
		if attempt > 0 {
			return skipped("holds")
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return failed(err)
		}
		if time.Since(info.ModTime()) < warmLeaseStale {
			return skipped("holds")
		}
		stale := path + ".stale-" + rand.Text()
		if err := os.Rename(path, stale); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return skipped("took over")
			}
			return failed(err)
		}
		_ = os.Remove(stale)
	}
}

// holdWarmLease keeps the lease at path fresh, its mtime set to now every warmLeaseHeartbeat so a
// live holder's lease is never read as stale, until release. release stops the heartbeat and
// removes the file, but only while it is still the one created (os.SameFile): the lease of a
// holder stalled past warmLeaseStale and taken over belongs to its successor by then, and the
// stalled holder must not remove it from under the successor's build.
func holdWarmLease(path string, created os.FileInfo) (release func()) {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(warmLeaseHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				_ = os.Chtimes(path, now, now)
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
		if current, err := os.Stat(path); err == nil && os.SameFile(created, current) {
			_ = os.Remove(path)
		}
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
// initialized" and exits 0, repairing nothing. lockLive — whether a build holds
// `.codegraph/codegraph.lock` (codegraphLockHeldByLiveProcess) — always wins over an index
// repair: a build that is still running also reports `index.state: "indexing"`,
// indistinguishable from one left partial by a dead process, so this never relies on CodeGraph's
// own mtime-based staleness check to tell the two apart.
func nextCodegraphStep(dirExists, lockLive bool, statusExitCode int, statusStdout string) codegraphStep {
	if statusExitCode == 0 && json.Valid([]byte(statusStdout)) {
		initialized, complete := codegraphStatus(statusStdout)
		switch {
		case !initialized:
			return codegraphStepInit
		case complete:
			return codegraphStepNone
		case lockLive:
			return codegraphStepNone
		default:
			return codegraphStepIndex
		}
	}
	if !dirExists {
		return codegraphStepInit
	}
	if lockLive {
		return codegraphStepNone
	}
	return codegraphStepIndex
}

// codegraphStatus reads `codegraph status --json`'s `initialized` and `index.state` fields.
// `codegraph status` exits 0 whether or not the project has ever been indexed, so only the parsed
// body tells the cases apart, and anything that fails to parse as that shape is treated as not
// initialized. `index.state` is `"complete"` only once a build has finished; a build still
// running, or one an earlier warm-up left partial, reports it `"indexing"` with `initialized`
// already true.
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

// codegraphLockHeldByLiveProcess reports whether dir's .codegraph/codegraph.lock is held by a
// build: it names a process that is still alive, or it is empty and its mtime is within
// codegraphEmptyLockGrace of now. CodeGraph 1.5.0's FileLock (its installed dist's utils.js)
// writes the lock's entire content as the builder's decimal PID with no other metadata, but it
// creates the file before it writes that PID, so an empty lock is a build that has just taken it
// until the grace runs out. The content and the mtime come from one open file, so they describe
// the same lock. CodeGraph's own staleness check compares the lock file's mtime against a fixed
// 2-minute timeout rather than checking the PID, so a lock can still name a live,
// actively-writing process past that window. Any other lock this cannot read, or whose content
// isn't a PID, is treated as not live: this never blocks a repair on a lock it cannot make sense
// of. Liveness alone is not enough on a host where PIDs recycle: a dead builder's PID reused by an
// unrelated process would read as live forever, so where `/proc/<pid>/cmdline` exists, the
// process also has to look like codegraph — a dead builder whose PID is unused, or whose slot now
// holds something else, is correctly stale. `/proc` absent (non-Linux) falls back to the liveness
// check alone.
func codegraphLockHeldByLiveProcess(dir string) bool {
	lock, err := os.Open(filepath.Join(dir, ".codegraph", "codegraph.lock"))
	if err != nil {
		return false
	}
	defer lock.Close()
	content, err := io.ReadAll(lock)
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		info, err := lock.Stat()
		if err != nil {
			return false
		}
		return time.Since(info.ModTime()).Abs() < codegraphEmptyLockGrace
	}
	pid, err := strconv.Atoi(text)
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
