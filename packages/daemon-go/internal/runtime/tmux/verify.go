package tmux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// noServerStderr is what tmux says when no server is behind the daemon's own socket: a socket
// left behind by an exited server, or one never created — a first boot, or a reboot that cleared
// TMUX_TMPDIR (tmux.ts:18-22).
var noServerStderr = regexp.MustCompile(`no server running|error connecting to .*\(No such file or directory\)`)

// paneGoneStderr is what a command's stderr says when the pane it targets provably does not exist:
// tmux cannot find it, or there is no server at all — and no server on the daemon's own socket
// means no Legion pane. Every other failure proves nothing about the pane (tmux.ts:386-392).
var paneGoneStderr = regexp.MustCompile(`can't find pane|` + noServerStderr.String())

// targetAbsent is a failure that says nothing is behind a target: no server, or no such session
// (tmux.ts:42-46).
func targetAbsent(res result) bool {
	return noServerStderr.MatchString(res.stderr) || strings.HasPrefix(res.stderr, "no such session") ||
		strings.HasPrefix(res.stderr, "can't find session")
}

type paneLookupStatus int

const (
	panePresent paneLookupStatus = iota
	paneAbsent
	paneLookupFailed
)

// paneLookup is a pane's live root pid, or the proof it is not there, or the lack of either.
type paneLookup struct {
	status paneLookupStatus
	pid    int
	detail string
}

// readPaneLookup reads a `list-panes -t <pane>` result (tmux.ts:394-433). The listing is the
// pane's whole window, so the pane-id column picks the row, exactly: an id missing from a good
// listing is absent, never approximated by a sibling's pid. A failure is absent only on proof
// (paneGoneStderr); a client killed by its timeout, any other stderr, or a row whose pid does not
// parse proves nothing and is failed.
func readPaneLookup(paneID string, res result) paneLookup {
	if res.timedOut > 0 {
		return paneLookup{status: paneLookupFailed, detail: fmt.Sprintf("list-panes -t %s timed out after %s", paneID, res.timedOut)}
	}
	if res.exitCode != 0 {
		stderr := strings.TrimSpace(res.stderr)
		if paneGoneStderr.MatchString(stderr) {
			return paneLookup{status: paneAbsent}
		}
		detail := fmt.Sprintf("list-panes -t %s exited %d", paneID, res.exitCode)
		if stderr != "" {
			detail += ": " + stderr
		}
		return paneLookup{status: paneLookupFailed, detail: detail}
	}
	for _, line := range strings.Split(res.stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != paneID {
			continue
		}
		pid := 0
		if len(fields) > 1 {
			pid, _ = strconv.Atoi(fields[1])
		}
		if pid <= 0 {
			return paneLookup{
				status: paneLookupFailed,
				detail: fmt.Sprintf("list-panes -t %s reported an unparseable pid for %s: %s", paneID, paneID, strings.Join(fields, " ")),
			}
		}
		return paneLookup{status: panePresent, pid: pid}
	}
	return paneLookup{status: paneAbsent}
}

type verdictReason int

const (
	reasonVerified verdictReason = iota
	reasonPaneGone
	// reasonListingFailed is not a verdict about the pane at all: tmux could not be listed, so
	// the pane is neither confirmed nor refuted, and no caller reads it as alive or gone.
	reasonListingFailed
	reasonPidMismatch
	reasonStatUnreadable
	reasonStartMismatch
	reasonNotOmp
)

// verdict is verifyPane's answer: verified, or why not, with what the pane showed instead.
type verdict struct {
	reason        verdictReason
	observedPid   int
	observedTicks uint64
	detail        string
}

func (v verdict) verified() bool { return v.reason == reasonVerified }

// describeVerdict is the sentence an operator reads for a pane that did not verify, the recorded
// identity beside what the pane shows (runtime-tmux.ts:189-211).
func describeVerdict(pane string, recorded incarnation, v verdict) string {
	identity := fmt.Sprintf("recorded pid %d start %d", recorded.pid, recorded.ticks)
	switch v.reason {
	case reasonPaneGone:
		return fmt.Sprintf("pane %s is gone", pane)
	case reasonListingFailed:
		return fmt.Sprintf("cannot verify pane %s: %s", pane, v.detail)
	case reasonPidMismatch:
		return fmt.Sprintf("pane %s now runs pid %d (%s)", pane, v.observedPid, identity)
	case reasonStatUnreadable:
		return fmt.Sprintf("pane %s pid %d has no readable /proc stat (%s)", pane, v.observedPid, identity)
	case reasonStartMismatch:
		return fmt.Sprintf("pane %s runs pid %d started at %d (%s)", pane, v.observedPid, v.observedTicks, identity)
	case reasonNotOmp:
		return fmt.Sprintf("pane %s pid %d is not running OMP (%s)", pane, v.observedPid, identity)
	default:
		return fmt.Sprintf("pane %s runs its recorded process", pane)
	}
}

// verifyPane asks whether pane still runs the recorded process: its current pid equals the
// recorded pid, that pid's start ticks equal the recorded ones, and its command line names OMP —
// the three tiers, in that order (runtime-tmux.ts:729-770). It is the one chokepoint Probe, a stop's
// kill gate, and a spawn's window choice all go through. The error is the runtime's own failure
// (tmux could not be run, /proc could not be read for a reason other than the process being
// gone); a listing that failed is a verdict, reasonListingFailed.
func (r *Runtime) verifyPane(ctx context.Context, pane string, recorded incarnation) (verdict, error) {
	res, err := r.run(ctx, listPaneArgv(r.socket, pane))
	if err != nil {
		return verdict{}, err
	}
	lookup := readPaneLookup(pane, res)
	switch lookup.status {
	case paneLookupFailed:
		return verdict{reason: reasonListingFailed, detail: lookup.detail}, nil
	case paneAbsent:
		return verdict{reason: reasonPaneGone}, nil
	}
	if lookup.pid != recorded.pid {
		return verdict{reason: reasonPidMismatch, observedPid: lookup.pid}, nil
	}
	ticks, alive, err := r.startTicks(lookup.pid)
	if err != nil {
		return verdict{}, err
	}
	if !alive {
		return verdict{reason: reasonStatUnreadable, observedPid: lookup.pid}, nil
	}
	if ticks != recorded.ticks {
		return verdict{reason: reasonStartMismatch, observedPid: lookup.pid, observedTicks: ticks}, nil
	}
	omp, err := r.runsOmp(lookup.pid)
	if err != nil {
		return verdict{}, err
	}
	if !omp {
		return verdict{reason: reasonNotOmp, observedPid: lookup.pid}, nil
	}
	return verdict{reason: reasonVerified}, nil
}

// processGone is a /proc read that failed because the process is gone: ENOENT once its directory
// is reaped, ESRCH while the entry lingers. Anything else is a fault of this host, never evidence
// about the process (runtime-tmux.ts:213-219).
func processGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// startTicks is pid's start ticks, or alive=false when the process is gone. A malformed stat line
// is an error: a guessed value could let a stale pane verify as ours (runtime-tmux.ts:693-709).
func (r *Runtime) startTicks(pid int) (ticks uint64, alive bool, err error) {
	stat, err := r.readProc(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if processGone(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	ticks, err = parseProcStatStartTicks(string(stat))
	if err != nil {
		return 0, false, err
	}
	return ticks, true, nil
}

// runsOmp is whether pid's command line names OMP. It is asked only just after pid's stat was
// read, so a command line that has vanished since means the process exited in between — not OMP
// any more (runtime-tmux.ts:859-873).
func (r *Runtime) runsOmp(pid int) (bool, error) {
	cmdline, err := r.readProc(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		if processGone(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(cmdline), "omp"), nil
}

// parseProcStatStartTicks is field 22 (starttime) of a /proc/<pid>/stat line, counted only after
// the last `)`, since field 2 is the executable name in parentheses and may itself hold spaces and
// parentheses (proc-stat.ts:1-28).
func parseProcStatStartTicks(stat string) (uint64, error) {
	commEnd := strings.LastIndexByte(stat, ')')
	if commEnd < 0 {
		return 0, fmt.Errorf("malformed /proc/<pid>/stat line (no comm terminator): %s", strings.TrimSpace(stat))
	}
	fields := strings.Fields(stat[commEnd+1:])
	const starttime = 22 - 3
	if len(fields) <= starttime {
		return 0, fmt.Errorf("malformed /proc/<pid>/stat line (no starttime field): %s", strings.TrimSpace(stat))
	}
	ticks, err := strconv.ParseUint(fields[starttime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed /proc/<pid>/stat line (no starttime field): %s", strings.TrimSpace(stat))
	}
	return ticks, nil
}

// Probe is one observation of loc's process, now (runtime-tmux.ts:836-857): Alive when the pane
// verifies as the recorded process, Gone when the pane is provably not there, NotRecordedProcess
// when something else holds it, and Uncertain when tmux could not be listed — never read as either
// of the first two. An error is the runtime itself failing, or a locator nothing can be done with.
func (r *Runtime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	pane, inc, err := paneOf(loc)
	if err != nil {
		return runtime.Observation{}, err
	}
	v, err := r.verifyPane(ctx, pane, inc)
	if err != nil {
		return runtime.Observation{}, fmt.Errorf("probe %s: %w", loc.Claim, err)
	}
	obs := runtime.Observation{Locator: loc, At: r.now()}
	switch v.reason {
	case reasonVerified:
		obs.Kind = runtime.Alive
	case reasonPaneGone:
		obs.Kind = runtime.Gone
		obs.Detail = describeVerdict(pane, inc, v)
	case reasonListingFailed:
		obs.Kind = runtime.Uncertain
		obs.Detail = describeVerdict(pane, inc, v)
	default:
		obs.Kind = runtime.NotRecordedProcess
		obs.Detail = describeVerdict(pane, inc, v)
	}
	return obs, nil
}
