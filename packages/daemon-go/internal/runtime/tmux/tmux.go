// Package tmux is the runtime that runs every agent as a pane on the daemon's private tmux server:
// `legion worker-shim` in the pane, dialing the daemon's worker stream listener, with Oh My Pi
// under it.
//
// One window per issue: the issue's first process opens a window named for it, and a later one
// splits into it while a pane recorded there still verifies as its recorded process. A pane is
// never known by its id alone — a recreated server reissues ids from %0, and the kernel reissues
// pids — so every locator records the pane's root pid and that process's /proc start ticks at
// launch (its incarnation), and nothing is reported alive, killed, or shared until pid, start
// ticks, and the OMP command line all check out again.
//
// The TypeScript runtime this ports is packages/daemon/src/daemon/{tmux,runtime-tmux,
// environment}.ts; each behaviour kept cites the file and line it keeps. The structure is Go's.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.Runtime = (*Runtime)(nil)

// defaultCommandTimeout bounds one tmux client invocation. A client that has not returned by then
// — a server not responding — is killed, and whatever it was asked is unanswered, never answered
// by a guess (the shipped runner's generic budget).
const defaultCommandTimeout = 30 * time.Second

// pollInterval is how often a stop or a resume re-checks a process it is waiting to see gone.
const pollInterval = 100 * time.Millisecond

// pipeDrainDelay bounds how long a tmux invocation's output is waited for once the client has
// exited or been killed. A tmux client hands its stdout and stderr to the server over the socket,
// so a server that accepted and never answered holds the client's pipes open after the client is
// gone; without this bound the command timeout would kill the client and still wait forever.
const pipeDrainDelay = time.Second

// Options is what a Runtime is built from. Every field without a stated default is required.
type Options struct {
	// Project names the private server: its socket, its session, and the owner marker on every
	// window are all `legion-<Project>` (tmux.ts:8-16).
	Project string
	// StateDir is the daemon's state directory: the secrets live under it, and the panes' XDG base
	// directories under its `home`.
	StateDir string
	// StreamAddress is the worker stream listener's address, `unix://<path>` or `tcp://host:port`,
	// passed to every pane's shim as `--connect`.
	StreamAddress string
	// DaemonURL, EnvoyURL, and NatsURLs are what every pane is told (LEGION_DAEMON_URL, ENVOY_URL,
	// ENVOY_NATS_URL); NatsURLs may be empty, and the pane then carries no ENVOY_NATS_URL.
	DaemonURL string
	EnvoyURL  string
	NatsURLs  []string
	// OmpInvocation is the resolved launch fragment (ResolveOmpInvocation); OmpLaunchPrefix the
	// configured argv prepended to it.
	OmpInvocation   string
	OmpLaunchPrefix []string
	// StopGrace is how long Suspend waits for a process to end itself after its shutdown frame,
	// and how long Resume waits for a previous incarnation to be gone (worker_stop_timeout_seconds).
	StopGrace time.Duration
	// ProbeInterval is the sweep's period (probe_interval_seconds).
	ProbeInterval time.Duration
	// AdoptTimeout bounds a working-copy adoption (slow_command_timeout_seconds).
	AdoptTimeout time.Duration
	// CommandTimeout bounds each tmux invocation; 30 s when zero.
	CommandTimeout time.Duration
	// Conns is the directory of agent connections (the worker stream listener): Stop's graceful
	// half and AdoptWorkingCopy go through it.
	Conns runtime.Conns
	// Environ is the daemon's own environment, in os.Environ() form; os.Environ() when nil. Only
	// the allow-listed names are used (PaneEnvironment).
	Environ []string
	// Executable resolves the `legion` binary a pane runs `worker-shim` from; os.Executable when
	// nil. It is called once, by New.
	Executable func() (string, error)
	// Now stamps observations and ages orphans; time.Now when nil.
	Now func() time.Time
	// Log receives what the runtime decides without being asked; slog.Default() when nil.
	Log *slog.Logger
}

// Runtime is the tmux runtime. Build one with New.
type Runtime struct {
	socket         string
	stateDir       string
	streamAddress  string
	daemonURL      string
	envoyURL       string
	natsURLs       []string
	ompInvocation  string
	ompPrefix      []string
	stopGrace      time.Duration
	probeInterval  time.Duration
	adoptTimeout   time.Duration
	commandTimeout time.Duration
	conns          runtime.Conns
	paneEnv        map[string]string
	environ        []string
	tmuxPath       string
	legion         string
	now            func() time.Time
	log            *slog.Logger

	// run executes one tmux argv. A field so an in-package test can record every argv the
	// runtime hands tmux; New sets it to runTmux.
	run func(ctx context.Context, argv []string) (result, error)
	// readProc reads a /proc file; a field for the same reason.
	readProc func(path string) ([]byte, error)

	// launchMu serializes everything that decides which window a pane lands in and every change
	// to the private server's set of panes the runtime must not race: session creation, window
	// choice, the pane's opening and recording, orphan reaping, and the scrub.
	launchMu sync.Mutex

	trackedMu sync.Mutex
	// tracked is every process the runtime watches, keyed by incarnation: the panes it opened,
	// and those it was told of by ReconcileOrphans. An entry leaves when its process is stopped
	// or a sweep has reported it gone for good.
	tracked map[string]*trackedProcess
}

// trackedProcess is one watched process: its locator, and the issue it works on when the runtime
// opened it (empty for one it adopted, whose window is then not offered to later panes).
type trackedProcess struct {
	locator runtime.Locator
	issue   string
}

// New builds a runtime from opts, resolving the tmux and legion binaries now so a daemon that
// cannot launch anything refuses at boot rather than at its first spawn.
func New(opts Options) (*Runtime, error) {
	switch {
	case opts.Project == "":
		return nil, errors.New("tmux runtime: no project")
	case !filepath.IsAbs(opts.StateDir):
		return nil, fmt.Errorf("tmux runtime: state directory %q is not an absolute path", opts.StateDir)
	case !strings.HasPrefix(opts.StreamAddress, "unix://") && !strings.HasPrefix(opts.StreamAddress, "tcp://"):
		return nil, fmt.Errorf("tmux runtime: stream address %q is neither unix:// nor tcp://", opts.StreamAddress)
	case opts.DaemonURL == "":
		return nil, errors.New("tmux runtime: no daemon URL")
	case opts.EnvoyURL == "":
		return nil, errors.New("tmux runtime: no Envoy URL")
	case opts.OmpInvocation == "":
		return nil, errors.New("tmux runtime: no OMP invocation")
	case opts.StopGrace <= 0 || opts.ProbeInterval <= 0 || opts.AdoptTimeout <= 0:
		return nil, errors.New("tmux runtime: the stop grace, probe interval, and adoption timeout must be positive")
	case opts.Conns == nil:
		return nil, errors.New("tmux runtime: no connection directory")
	}
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	executable := opts.Executable
	if executable == nil {
		executable = os.Executable
	}
	legion, err := executable()
	if err != nil {
		return nil, fmt.Errorf("tmux runtime: resolve the legion binary panes run: %w", err)
	}
	paneEnv := PaneEnvironment(environ, opts.StateDir)
	tmuxPath, err := lookPath("tmux", paneEnv["PATH"])
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		socket:         "legion-" + opts.Project,
		stateDir:       opts.StateDir,
		streamAddress:  opts.StreamAddress,
		daemonURL:      opts.DaemonURL,
		envoyURL:       opts.EnvoyURL,
		natsURLs:       opts.NatsURLs,
		ompInvocation:  opts.OmpInvocation,
		ompPrefix:      opts.OmpLaunchPrefix,
		stopGrace:      opts.StopGrace,
		probeInterval:  opts.ProbeInterval,
		adoptTimeout:   opts.AdoptTimeout,
		commandTimeout: opts.CommandTimeout,
		conns:          opts.Conns,
		paneEnv:        paneEnv,
		tmuxPath:       tmuxPath,
		legion:         legion,
		now:            opts.Now,
		log:            opts.Log,
		readProc:       os.ReadFile,
		tracked:        map[string]*trackedProcess{},
	}
	if r.commandTimeout == 0 {
		r.commandTimeout = defaultCommandTimeout
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	for name, value := range paneEnv {
		r.environ = append(r.environ, name+"="+value)
	}
	sort.Strings(r.environ)
	r.run = r.runTmux
	return r, nil
}

// lookPath finds command on searchPath, the PATH the panes run under.
func lookPath(command, searchPath string) (string, error) {
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, command)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("tmux runtime: %s is not on the daemon's PATH (%s)", command, searchPath)
}

// Socket is the private server's socket and session name, `legion-<project>`.
func (r *Runtime) Socket() string { return r.socket }

// ControllerLaunch is "daemon": under tmux the daemon opens the interactive controller in a pane
// of its own server.
func (r *Runtime) ControllerLaunch() runtime.ControllerLaunch { return runtime.ControllerLaunchDaemon }

// result is one tmux invocation's outcome. timedOut is the budget the client was killed at, zero
// when it returned on its own.
type result struct {
	stdout, stderr string
	exitCode       int
	timedOut       time.Duration
}

// runTmux runs one argv against the private server under the pane environment, bounded by the
// command timeout. A client that ran and failed is a result; one that could not run at all, or
// whose caller's context ended, is an error.
func (r *Runtime) runTmux(ctx context.Context, argv []string) (result, error) {
	bounded, cancel := context.WithTimeout(ctx, r.commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, r.tmuxPath, argv[1:]...)
	cmd.Env = r.environ
	cmd.WaitDelay = pipeDrainDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return result{}, ctx.Err()
	}
	if bounded.Err() != nil {
		res.exitCode, res.timedOut = -1, r.commandTimeout
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.exitCode = exit.ExitCode()
		return res, nil
	}
	return result{}, fmt.Errorf("run tmux %s: %w", verb(argv), err)
}

// verb is the tmux command an argv runs: the word after `-L <socket>`.
func verb(argv []string) string {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "-L" {
			return argv[i+2]
		}
	}
	return strings.Join(argv, " ")
}

// failure is the detail every error message about a tmux command carries: its stderr, trimmed —
// never stdout, which for show-environment is the value dump — and said out loud when empty
// (tmux.ts:48-56).
func failure(res result) string {
	if res.timedOut > 0 {
		return fmt.Sprintf(": timed out after %s", res.timedOut)
	}
	if detail := strings.TrimSpace(res.stderr); detail != "" {
		return ": " + detail
	}
	return ": tmux printed nothing on stderr"
}

// incarnation is a pane's process identity: its root pid and that process's start ticks.
type incarnation struct {
	pid   int
	ticks uint64
}

func (i incarnation) String() string {
	return strconv.Itoa(i.pid) + ":" + strconv.FormatUint(i.ticks, 10)
}

func parseIncarnation(s string) (incarnation, error) {
	pidText, ticksText, ok := strings.Cut(s, ":")
	pid, pidErr := strconv.Atoi(pidText)
	ticks, ticksErr := strconv.ParseUint(ticksText, 10, 64)
	if !ok || pidErr != nil || ticksErr != nil || pid <= 0 {
		return incarnation{}, fmt.Errorf("incarnation %q is not <pane pid>:<start ticks>", s)
	}
	return incarnation{pid: pid, ticks: ticks}, nil
}

// paneOf is a locator's pane and incarnation, or the reason nothing can be done with it.
func paneOf(loc runtime.Locator) (string, incarnation, error) {
	if err := loc.Validate(); err != nil {
		return "", incarnation{}, err
	}
	if loc.Runtime != runtime.RuntimeTmux {
		return "", incarnation{}, fmt.Errorf("tmux runtime cannot act on a %q locator (%s)", loc.Runtime, loc.Claim)
	}
	inc, err := parseIncarnation(loc.Incarnation)
	if err != nil {
		return "", incarnation{}, fmt.Errorf("locator %s: %w", loc.Claim, err)
	}
	return loc.Tmux.Pane, inc, nil
}

func (r *Runtime) track(loc runtime.Locator, issue string) {
	r.trackedMu.Lock()
	defer r.trackedMu.Unlock()
	if existing, ok := r.tracked[loc.Incarnation]; ok && existing.issue != "" {
		issue = existing.issue
	}
	r.tracked[loc.Incarnation] = &trackedProcess{locator: loc, issue: issue}
}

func (r *Runtime) untrack(loc runtime.Locator) {
	r.trackedMu.Lock()
	defer r.trackedMu.Unlock()
	delete(r.tracked, loc.Incarnation)
}

// trackedProcesses is every watched process, ordered by claim then incarnation, so a sweep and a
// window choice visit them the same way every time.
func (r *Runtime) trackedProcesses() []trackedProcess {
	r.trackedMu.Lock()
	defer r.trackedMu.Unlock()
	out := make([]trackedProcess, 0, len(r.tracked))
	for _, entry := range r.tracked {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].locator.Claim != out[j].locator.Claim {
			return out[i].locator.Claim < out[j].locator.Claim
		}
		return out[i].locator.Incarnation < out[j].locator.Incarnation
	})
	return out
}

// Suspend stops the process within the configured stop grace; the claim's session file is the
// caller's to keep, and a later Resume continues from it.
func (r *Runtime) Suspend(ctx context.Context, loc runtime.Locator) error {
	return r.Stop(ctx, loc, r.stopGrace)
}

// Stop ends the locator's process (runtime-tmux.ts:884-951). When the recorded process still
// verifies and the claim has a connection, the connection carries a shutdown frame — the shim
// ends OMP itself — and Stop waits up to grace for the process to go. Whatever is left then is
// killed with kill-pane, and only a pane that verifies as the recorded process is ever killed.
//
// The connection is used only while the recorded process verifies: a claim's connection belongs
// to its live incarnation, and a stale locator must not shut down the claim's newer process. A
// pane that is gone, or now holds some other process, is already stopped as far as this locator
// is concerned; that is logged, not killed. A pane that cannot be verified either way — tmux
// could not be listed — is an error: a process nobody can confirm stopped is never reported
// stopped.
func (r *Runtime) Stop(ctx context.Context, loc runtime.Locator, grace time.Duration) error {
	pane, inc, err := paneOf(loc)
	if err != nil {
		return err
	}
	v, err := r.verifyPane(ctx, pane, inc)
	if err != nil {
		return fmt.Errorf("stop %s: %w", loc.Claim, err)
	}
	if !v.verified() {
		return r.settleUnverified(loc, pane, inc, v)
	}
	if conn, ok := r.conns.Conn(loc.Claim); ok && grace > 0 {
		if err := conn.Shutdown(ctx); err != nil {
			r.log.Warn("tmux runtime: shutdown frame not sent; killing the pane", "claim", loc.Claim, "pane", pane, "err", err)
		} else {
			// The wait's last verdict is the kill gate: taken after the grace, not before it.
			if v, err = r.awaitNotRunning(ctx, pane, inc, grace); err != nil {
				return fmt.Errorf("stop %s: %w", loc.Claim, err)
			}
			if !v.verified() {
				return r.settleUnverified(loc, pane, inc, v)
			}
		}
	}
	res, err := r.run(ctx, killPaneArgv(r.socket, pane))
	if err != nil {
		return fmt.Errorf("stop %s: %w", loc.Claim, err)
	}
	if res.exitCode != 0 && !paneGoneStderr.MatchString(res.stderr) {
		return fmt.Errorf("stop %s: kill-pane %s exited %d%s", loc.Claim, pane, res.exitCode, failure(res))
	}
	r.untrack(loc)
	return nil
}

// settleUnverified finishes a stop whose recorded process is not in its pane: gone, or replaced,
// is stopped; unverifiable is an error.
func (r *Runtime) settleUnverified(loc runtime.Locator, pane string, inc incarnation, v verdict) error {
	switch v.reason {
	case reasonListingFailed:
		return fmt.Errorf("stop %s: %s", loc.Claim, describeVerdict(pane, inc, v))
	case reasonPaneGone:
	default:
		r.log.Warn("tmux runtime: not killing a pane that is not the recorded process; treating it as gone",
			"claim", loc.Claim, "detail", describeVerdict(pane, inc, v))
	}
	r.untrack(loc)
	return nil
}

// awaitNotRunning re-verifies the pane until the recorded process no longer verifies, or grace
// is spent, and returns the last verdict. A verdict that cannot be reached — tmux not answering
// — keeps the wait going; it proves nothing either way.
func (r *Runtime) awaitNotRunning(ctx context.Context, pane string, inc incarnation, grace time.Duration) (verdict, error) {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return verdict{}, ctx.Err()
		case <-deadline.C:
			return r.verifyPane(ctx, pane, inc)
		case <-tick.C:
			v, err := r.verifyPane(ctx, pane, inc)
			if err != nil {
				return verdict{}, err
			}
			if !v.verified() && v.reason != reasonListingFailed {
				return v, nil
			}
		}
	}
}

// AdoptWorkingCopy has the agent's shim set the author of its working copy — the shared
// `jj metaedit --update-author` run in the pane's own workspace — over the claim's connection,
// bounded by the adoption timeout. The recorded process must still verify: the connection is its.
func (r *Runtime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	pane, inc, err := paneOf(loc)
	if err != nil {
		return err
	}
	v, err := r.verifyPane(ctx, pane, inc)
	if err != nil {
		return fmt.Errorf("adopt %s's working copy: %w", loc.Claim, err)
	}
	if !v.verified() {
		return fmt.Errorf("adopt %s's working copy: %s", loc.Claim, describeVerdict(pane, inc, v))
	}
	conn, ok := r.conns.Conn(loc.Claim)
	if !ok {
		return fmt.Errorf("adopt %s's working copy: the claim has no connection to ask over", loc.Claim)
	}
	if err := conn.AdoptWorkingCopy(ctx, id, r.adoptTimeout); err != nil {
		return fmt.Errorf("adopt %s's working copy for %s <%s>: %w", loc.Claim, id.Name, id.Email, err)
	}
	return nil
}
