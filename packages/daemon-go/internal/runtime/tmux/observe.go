package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Observe is the periodic sweep: every watched process probed at once, then again every probe
// interval, each verdict sent on the returned channel, which closes when ctx ends. A sweep blocks
// on its consumer rather than dropping a fact.
//
// Gone and NotRecordedProcess are final for an incarnation — a pid and its start ticks never come
// back — so once one is delivered the process leaves the watch. Alive and Uncertain stay. A probe
// the runtime itself cannot answer (a /proc read failing for a reason other than the process being
// gone) is reported Uncertain with the failure as its detail, and logged: the process's state is
// exactly what nobody knows.
func (r *Runtime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	out := make(chan runtime.Observation)
	go func() {
		defer close(out)
		ticker := time.NewTicker(r.probeInterval)
		defer ticker.Stop()
		for r.sweep(ctx, out) {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}

// sweep probes every watched process once, reporting false once ctx has ended.
func (r *Runtime) sweep(ctx context.Context, out chan<- runtime.Observation) bool {
	for _, entry := range r.trackedProcesses() {
		obs, err := r.Probe(ctx, entry.locator)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			r.log.Error("tmux runtime: probe failed", "claim", entry.locator.Claim, "err", err)
			obs = runtime.Observation{
				Locator: entry.locator, Kind: runtime.Uncertain, At: r.now(), Detail: "probe failed: " + err.Error(),
			}
		}
		select {
		case <-ctx.Done():
			return false
		case out <- obs:
		}
		if obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess {
			r.untrack(entry.locator)
		}
	}
	return ctx.Err() == nil
}

// ReconcileOrphans ends the Legion processes on the private server that neither known nor the
// runtime's own watch accounts for, once idle past grace — what a crash between opening a pane and
// persisting its locator leaves behind (runtime-tmux.ts:963-1011). Two passes: every window marked
// as this daemon's that holds no known pane is killed; then every unknown pane whose start command
// runs `legion worker-shim`, in a window marked as this daemon's, is killed — the pane a crash left
// in a window a live process still shares. Boot calls this with grace 0.
//
// A known claim with no locator holds no pane, so it protects nothing here. The located ones also
// join the watch: they are the processes the daemon recorded before it restarted, and the sweep
// reports on them from now on.
//
// A listing with nothing behind it (no server, no session) reaps nothing; a listing that failed
// otherwise is an error, as is a kill that failed for a reason other than its target being gone,
// after every other candidate has been tried.
func (r *Runtime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	var located []runtime.Locator
	for _, entry := range known {
		if entry.Locator == nil {
			continue
		}
		if _, _, err := paneOf(*entry.Locator); err != nil {
			return fmt.Errorf("reconcile orphans: %w", err)
		}
		located = append(located, *entry.Locator)
	}
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	for _, loc := range located {
		r.track(loc, "")
	}
	knownWindows, knownPanes := map[string]bool{}, map[string]bool{}
	for _, entry := range r.trackedProcesses() {
		knownWindows[entry.locator.Tmux.Window] = true
		knownPanes[entry.locator.Tmux.Pane] = true
	}
	now := r.now()
	var failures []error

	windows, err := r.run(ctx, listOwnedWindowsArgv(r.socket, r.socket))
	if err != nil {
		return err
	}
	if windows.exitCode != 0 && !targetAbsent(windows) {
		return fmt.Errorf("reconcile orphans: list-windows exited %d%s", windows.exitCode, failure(windows))
	}
	if windows.exitCode == 0 {
		for _, window := range readUnknownWindows(windows.stdout, r.socket, knownWindows) {
			if now.Sub(window.activity) < grace {
				continue
			}
			r.log.Warn("tmux runtime: killing an orphaned window", "window", window.id)
			if err := r.kill(ctx, killWindowArgv(r.socket, window.id), window.id); err != nil {
				failures = append(failures, err)
			}
		}
	}

	panes, err := r.run(ctx, listAllPanesArgv(r.socket))
	if err != nil {
		return err
	}
	if panes.exitCode != 0 && !targetAbsent(panes) {
		return errors.Join(append(failures, fmt.Errorf("reconcile orphans: list-panes exited %d%s", panes.exitCode, failure(panes)))...)
	}
	if panes.exitCode == 0 {
		for _, pane := range readUnknownPanes(panes.stdout, r.socket, knownPanes) {
			if now.Sub(pane.activity) < grace {
				continue
			}
			r.log.Warn("tmux runtime: killing an orphaned pane", "pane", pane.id, "window", pane.window)
			if err := r.kill(ctx, killPaneArgv(r.socket, pane.id), pane.id); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// kill runs a kill-window or kill-pane, treating a target already gone as killed.
func (r *Runtime) kill(ctx context.Context, argv []string, target string) error {
	res, err := r.run(ctx, argv)
	if err != nil {
		return err
	}
	if res.exitCode != 0 && !paneGoneStderr.MatchString(res.stderr) && !strings.Contains(res.stderr, "can't find window") {
		return fmt.Errorf("reconcile orphans: %s %s exited %d%s", verb(argv), target, res.exitCode, failure(res))
	}
	return nil
}

// ownedWindow is a window marked as this daemon's, with its last activity.
type ownedWindow struct {
	id       string
	activity time.Time
}

// readUnknownWindows is every window in a list-windows listing marked with owner and not known
// (tmux.ts:455-489). A row that cannot be timed is never a candidate.
func readUnknownWindows(stdout, owner string, known map[string]bool) []ownedWindow {
	var out []ownedWindow
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !windowID.MatchString(fields[0]) || known[fields[0]] || fields[1] != owner {
			continue
		}
		activity, ok := unixSeconds(fields[2])
		if !ok {
			continue
		}
		out = append(out, ownedWindow{id: fields[0], activity: activity})
	}
	return out
}

// ownedPane is an unknown worker-shim pane in a window marked as this daemon's.
type ownedPane struct {
	id, window string
	activity   time.Time
}

// readUnknownPanes is every pane in a list-panes -a listing whose window is marked with owner,
// whose id is not known, and whose start command names `worker-shim` — every Legion process runs
// inside one (tmux.ts:491-545). The start command is the one free-text column, so it is whatever
// lies between the owner and the last column. A row that cannot be timed is never a candidate.
func readUnknownPanes(stdout, owner string, known map[string]bool) []ownedPane {
	var out []ownedPane
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 || !paneID.MatchString(fields[0]) || fields[1] == "" || known[fields[0]] || fields[2] != owner {
			continue
		}
		if !strings.Contains(strings.Join(fields[3:len(fields)-1], "\t"), "worker-shim") {
			continue
		}
		activity, ok := unixSeconds(fields[len(fields)-1])
		if !ok {
			continue
		}
		out = append(out, ownedPane{id: fields[0], window: fields[1], activity: activity})
	}
	return out
}

func unixSeconds(field string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}
