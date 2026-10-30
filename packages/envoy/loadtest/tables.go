package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// stepData is one step directory's results.
type stepData struct {
	name string
	sum  summary
	meta *metaFile
}

func runTables(args []string) error {
	fs := flag.NewFlagSet("tables", flag.ExitOnError)
	out := fs.String("out", "", "TABLES.md path (default: <dir>/TABLES.md for one step directory, ./TABLES.md for several)")
	bin := fs.String("bin", filepath.Join(loadtestRoot(), "bin", "envoy-dispatch"), "Dispatch binary the profiles came from (omitted when absent)")
	tops := fs.Bool("tops", true, "write each step directory's pprof tops and goroutines.summary.txt")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen tables [-out TABLES.md] [-bin envoy-dispatch] [-tops=false] <step dir>...")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	dirs := fs.Args()
	if len(dirs) == 0 {
		fs.Usage()
		return fmt.Errorf("name at least one step directory")
	}
	var steps []stepData
	for _, dir := range dirs {
		st, err := readStep(dir)
		if err != nil {
			return err
		}
		steps = append(steps, st)
		if !*tops {
			continue
		}
		pprofTops(dir, *bin)
		if b, err := os.ReadFile(filepath.Join(dir, "goroutines.txt")); err == nil {
			text := goroutineSummary(st.name, string(b))
			if err := os.WriteFile(filepath.Join(dir, "goroutines.summary.txt"), []byte(text), 0o644); err != nil {
				return err
			}
		}
	}
	if *out == "" {
		*out = "TABLES.md"
		if len(dirs) == 1 {
			*out = filepath.Join(dirs[0], "TABLES.md")
		}
	}
	md := renderTables(steps)
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Print(md)
	return nil
}

func readStep(dir string) (stepData, error) {
	st := stepData{name: filepath.Base(filepath.Clean(dir))}
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return st, fmt.Errorf("step %s: %w", dir, err)
	}
	if err := json.Unmarshal(b, &st.sum); err != nil {
		return st, fmt.Errorf("step %s: summary.json: %w", dir, err)
	}
	if st.sum.Config.Step != "" {
		st.name = st.sum.Config.Step
	}
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var m metaFile
		if json.Unmarshal(b, &m) == nil {
			st.meta = &m
		}
	}
	return st, nil
}

// num formats x with d decimals and thousands separators.
func num(x float64, d int) string {
	if !finite(x) {
		return "-"
	}
	s := fmt.Sprintf("%.*f", d, math.Abs(x))
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	if x < 0 && strings.Trim(s, "0.") != "" {
		b.WriteByte('-')
	}
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if frac != "" {
		b.WriteString("." + frac)
	}
	return b.String()
}

// pair renders "a / b" of a stat, or "-" when it holds no values.
func pair(s stat, a, b func(stat) float64, d int) string {
	if s.N == 0 {
		return "-"
	}
	return num(a(s), d) + " / " + num(b(s), d)
}

func p50(s stat) float64  { return s.P50 }
func p99(s stat) float64  { return s.P99 }
func smax(s stat) float64 { return s.Max }

// label is the step's name in a table row, marked "(<names> exited)" when a stack process exited
// during the step.
func (s stepData) label() string {
	if len(s.sum.StackExits) == 0 {
		return s.name
	}
	names := make([]string, len(s.sum.StackExits))
	for i, x := range s.sum.StackExits {
		names[i] = x.Name
	}
	return s.name + " (" + strings.Join(names, ", ") + " exited)"
}

// withFailedReads is label, marked "(1 failed read)" or "(<n> failed reads)" when n reads failed.
func withFailedReads(label string, failedReads int) string {
	switch failedReads {
	case 0:
		return label
	case 1:
		return label + " (1 failed read)"
	}
	return fmt.Sprintf("%s (%d failed reads)", label, failedReads)
}

// commitsUnreadMark marks the step, in a table with cells that depend on the run's commit rows,
// when loadtest.event_commits could not be read (events.commits_error).
const commitsUnreadMark = "(commit rows not read)"

// commitsLabel is the step's label in a table with cells that depend on the run's commit rows,
// marked commitsUnreadMark when they were not read.
func (s stepData) commitsLabel() string {
	if !s.sum.Events.commitsUnread() {
		return s.label()
	}
	return s.label() + " " + commitsUnreadMark
}

// commitCount is text, a cell of counts from the run's commit rows, or "-" when they were not read:
// then each count is 0 because nothing was read, not a measured 0. The latencies and the CPU per
// operation that come from the commit rows need no override: with no commit row they hold no value
// (pair shows "-"), and run.go nulls the CPU per operation.
func (s stepData) commitCount(text string) string {
	if s.sum.Events.commitsUnread() {
		return "-"
	}
	return text
}

// commitsUnreadNote defines commitsUnreadMark under a table, when a row has it: subject names the
// table's cells that depend on the commit rows, and verb is "show" or "shows" to agree with it.
func commitsUnreadNote(md *markdown, marked bool, subject, verb string) {
	if marked {
		md.line("\n%s: loadgen could not read the run's rows of loadtest.event_commits (summary.json `events.commits_error` says why: a database without commit-trigger.sql, or Postgres not reachable), so %s %s `-`, not a measured 0, and the step failed.",
			commitsUnreadMark, subject, verb)
	}
}

// markdown is TABLES.md as it is written.
type markdown struct {
	strings.Builder
}

func (m *markdown) line(format string, args ...any) { fmt.Fprintf(m, format+"\n", args...) }

func (m *markdown) row(cells ...string) { m.line("| %s |", strings.Join(cells, " | ")) }

func (m *markdown) header(cells ...string) {
	m.row(cells...)
	m.line("|%s", strings.Repeat("---|", len(cells)))
}

// emptyRow is a row of label, then "-" in each of the other columns.
func (m *markdown) emptyRow(label string, columns int) {
	cells := []string{label}
	for range columns - 1 {
		cells = append(cells, "-")
	}
	m.row(cells...)
}

func renderTables(steps []stepData) string {
	var md markdown
	md.line("# Load test tables\n")
	md.line("One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.\n")
	eventsTable(&md, steps)
	kindsTable(&md, steps)
	resourcesTable(&md, steps)
	commitOrderLockTable(&md, steps)
	liveTable(&md, steps)
	ungrantedTable(&md, steps)
	xidTable(&md, steps)
	reconnectsTable(&md, steps)
	restartTable(&md, steps)
	mentionsTable(&md, steps)
	routesTable(&md, steps)
	replayTable(&md, steps)
	return md.String()
}

func eventsTable(md *markdown, steps []stepData) {
	md.line("## Events, delivery and requests\n")
	md.header("step", "start (UTC)", "sessions", "events/s target / offered / committed", "write errors non-2xx / transport",
		"delivery ms from arrival p50 / p99", "delivery ms from commit p50 / p99", "issue-topic receipts got / expected",
		"req/s target / sent", "req 2xx", "req ms p50 / p99", "requests dropped at the in-flight cap")
	below, exited, unread := false, false, false
	for _, s := range steps {
		c, e, q := s.sum.Config, s.sum.Events, s.sum.Requests
		start := "-"
		if s.meta != nil && !s.meta.WindowStart.IsZero() {
			start = s.meta.WindowStart.UTC().Format("2006-01-02 15:04:05")
		}
		unread = unread || e.commitsUnread()
		events := "0"
		if c.EventsPerS > 0 {
			events = fmt.Sprintf("%s / %s / %s", num(c.EventsPerS, 2), num(e.OfferedPerS, 2), s.commitCount(num(e.CommittedPerS, 2)))
		}
		receipts := s.commitCount(fmt.Sprintf("%s / %s", num(float64(e.IssueTopicReceipts), 0), num(float64(e.ExpectedIssueTopicReceipts), 0)))
		reqRate := "0"
		share := "-"
		if c.RequestsPerS > 0 {
			reqRate = num(c.RequestsPerS, 1) + " / " + num(q.SentPerS, 1)
			if q.BelowTarget {
				reqRate += " (below 95%)"
				below = true
			}
		}
		if q.Sent > 0 {
			share = num(100*float64(q.OK2xx)/float64(q.Sent), 1) + "%"
		}
		if len(s.sum.StackExits) > 0 {
			exited = true
		}
		md.row(s.commitsLabel(), start, num(float64(c.Sessions), 0), events,
			fmt.Sprintf("%s / %s", num(float64(e.WriteErrors.Non2xx), 0), num(float64(e.WriteErrors.Transport), 0)),
			pair(e.DeliveryMsFromArrival, p50, p99, 0), pair(e.DeliveryMsFromCommit, p50, p99, 0), receipts,
			reqRate, share, pair(q.LatencyMs, p50, p99, 0), num(float64(q.DroppedInflightCap), 0))
	}
	if below {
		md.line("\n(below 95%%): the step sent fewer than 95%% of its target requests per second, so its request columns describe a lighter load than its name.")
	}
	if exited {
		md.line("\n(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.")
	}
	commitsUnreadNote(md, unread, "the committed events/s, the delivery latencies from arrival and from commit, and the issue-topic receipts", "show")
}

func kindsTable(md *markdown, steps []stepData) {
	md.line("\n## Write latency by kind\n")
	md.line("Write ms p50 / p99 over 2xx writes, then committed / offered.\n")
	cells := []string{"step"}
	for _, k := range eventKinds {
		cells = append(cells, k+" ms p50 / p99 (committed / offered)")
	}
	md.header(cells...)
	unread := false
	for _, s := range steps {
		unread = unread || s.sum.Events.commitsUnread()
		cells := []string{s.commitsLabel()}
		for _, k := range eventKinds {
			ks, ok := s.sum.Events.ByKind[k]
			if !ok || ks.Offered == 0 {
				cells = append(cells, "-")
				continue
			}
			committed := s.commitCount(num(float64(ks.Committed), 0))
			cells = append(cells, fmt.Sprintf("%s (%s / %s)", pair(ks.WriteMs, p50, p99, 0), committed, num(float64(ks.Offered), 0)))
		}
		md.row(cells...)
	}
	commitsUnreadNote(md, unread, "the committed counts of each kind", "show")
}

func resourcesTable(md *markdown, steps []stepData) {
	md.line("\n## Resources\n")
	md.header("step", "Dispatch cores mean / peak 5 s", "Postgres cores", "listener cores", "NATS cores", "loadgen cores", "proxy cores",
		"box busy", "box busy before", "Dispatch RSS max MiB", "heap in use MiB p50 / max", "goroutines p50 / max",
		"goroutines waiting for a pool connection p50 / max", "Postgres connections max", "connections waiting on a lock p50",
		"Dispatch CPU-s per operation")
	marked, unread := false, false
	for _, s := range steps {
		unread = unread || s.sum.Events.commitsUnread()
		busyBefore := "-"
		if s.meta != nil && s.meta.BoxBusyBefore != nil {
			busyBefore = num(100**s.meta.BoxBusyBefore, 0) + "%"
		}
		r := s.sum.Resources
		if r == nil {
			// No sampler window: every cell is "-" but box busy before, which is meta.json's
			// reading, not the sampler's.
			md.row(s.commitsLabel(), "-", "-", "-", "-", "-", "-", "-", busyBefore, "-", "-", "-", "-", "-", "-", "-")
			continue
		}
		marked = marked || r.failedReads() > 0
		cores := func(name string, d int) string {
			v, ok := r.CPUCoresMean[name]
			if !ok {
				return "-"
			}
			return num(v, d)
		}
		dispatch := "-"
		if v, ok := r.CPUCoresMean["dispatch"]; ok {
			dispatch = num(v, 2) + " / " + num(r.CPUCoresPeak5s["dispatch"], 2)
		}
		rss := "-"
		if v, ok := r.RSSMiBMax["dispatch"]; ok {
			rss = num(v, 0)
		}
		perOp := "-"
		if r.DispatchCPUSPerOp != nil {
			perOp = num(*r.DispatchCPUSPerOp, 4)
		}
		lockWait := "-"
		if st, ok := r.PGConnsByState["active|Lock"]; ok {
			lockWait = num(st.P50, 0)
		} else if r.PGConnsTotal.N > 0 {
			lockWait = "0"
		}
		pgMax := "-"
		if r.PGConnsTotal.N > 0 {
			pgMax = num(r.PGConnsTotal.Max, 0)
		}
		md.row(withFailedReads(s.commitsLabel(), r.failedReads()), dispatch, cores("postgres", 2), cores("listener", 3), cores("nats", 3), cores("loadgen", 2), cores("proxy", 2),
			num(100*r.SysBusyFrac, 0)+"%", busyBefore, rss, pair(r.HeapInuseMiB, p50, smax, 0), pair(r.Goroutines, p50, smax, 0),
			pair(r.PoolAcquireGoroutines, p50, smax, 0), pgMax, lockWait, perOp)
	}
	failedReadsNote(md, marked, "the 5 s sampler's reads of pg_stat_activity (a failed connect included) and of Dispatch's goroutine and heap profiles",
		"`resources.pg_read_errors` and `resources.pprof_read_errors`")
	commitsUnreadNote(md, unread, "the Dispatch CPU-s per operation (its operations count the committed writes)", "shows")
}

// failedReadsNote defines the failed-reads mark under a table, when a row has it: what names the
// reads that failed, saying what a failed read covers, and keys the summary.json keys that count
// the failures.
func failedReadsNote(md *markdown, marked bool, what, keys string) {
	if marked {
		md.line("\n(<n> failed reads), or (1 failed read): %s failed n times (summary.json %s); those reads enter no statistic, and a cell with no successful read shows `-`, not 0.", what, keys)
	}
}

func commitOrderLockTable(md *markdown, steps []stepData) {
	md.line("\n## Commit-order lock\n")
	md.line("Sampled every 20 ms from 20 s into the window to its end.\n")
	columns := []string{"step", "time held", "waiters p50 / max", "hold ms p50 / p90 / p99", "holds seen", "what the holder was doing (share of held samples)"}
	md.header(columns...)
	marked := false
	for _, s := range steps {
		l := s.sum.CommitOrderLock
		marked = marked || l.Errors > 0
		if l.Samples == 0 {
			md.emptyRow(withFailedReads(s.label(), l.Errors), len(columns))
			continue
		}
		holds := "-"
		if l.HoldMs.N > 0 {
			holds = fmt.Sprintf("%s / %s / %s", num(l.HoldMs.P50, 0), num(l.HoldMs.P90, 0), num(l.HoldMs.P99, 0))
		}
		var doing []string
		for i, d := range l.HolderDoing {
			if i == 4 {
				break
			}
			what := d.What
			if len(what) > 70 {
				what = what[:70]
			}
			doing = append(doing, fmt.Sprintf("%s %s%%", mdEscape(what), num(100*d.Share, 0)))
		}
		md.row(withFailedReads(s.label(), l.Errors), num(100*l.HeldFraction, 0)+"%", pair(l.Waiters, p50, smax, 0), holds, num(float64(l.HoldsObserved), 0), strings.Join(doing, "; "))
	}
	failedReadsNote(md, marked, "the probe's connect or read of pg_locks", "`commit_order_lock.errors`")
}

func liveTable(md *markdown, steps []stepData) {
	md.line("\n## Delivery from sending, and live socket frames\n")
	md.line("NATS: every issue-topic receipt of a primary event. Live: the event frames of the dashboard live sockets that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.\n")
	md.header("step", "NATS delivery ms from sent p50 / p99", "live frame ms from created_at p50 / p99",
		"live frame ms from write response p50 / p99", "live frames", "live order violations")
	unread := false
	for _, s := range steps {
		live, e := s.sum.Live, s.sum.Events
		unread = unread || e.commitsUnread()
		md.row(s.commitsLabel(), pair(e.DeliveryMsFromSent, p50, p99, 0), pair(live.FrameMsFromCreatedAt, p50, p99, 0),
			pair(live.FrameMsFromWriteResponse, p50, p99, 0), num(float64(live.Frames), 0), num(float64(live.OrderViolations), 0))
	}
	commitsUnreadNote(md, unread, "the NATS delivery latencies from sent and the live frame latencies from write response (both take only committed writes' primary events)", "show")
}

func ungrantedTable(md *markdown, steps []stepData) {
	md.line("\n## Ungranted locks\n")
	md.line("Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.\n")
	columns := []string{"step", "samples with an ungranted lock", "locks waited for: type target mode, samples with a waiter, waiters p50 / max",
		"top waiter query → first blocker query (count), per lock"}
	md.header(columns...)
	marked := false
	for _, s := range steps {
		u := s.sum.UngrantedLocks
		marked = marked || u.Errors > 0
		if u.Samples == 0 {
			md.emptyRow(withFailedReads(s.label(), u.Errors), len(columns))
			continue
		}
		var locks, pairs []string
		for i, g := range u.Groups {
			if i == 4 {
				break
			}
			locks = append(locks, fmt.Sprintf("%s %s %s %s%%, %s / %s", mdCell(g.LockType), mdCell(g.Target), mdCell(g.Mode),
				num(100*g.ShareWithWaiter, 0), num(g.WaitersP50, 0), num(g.WaitersMax, 0)))
			top := "-"
			if len(g.TopPairs) > 0 {
				p := g.TopPairs[0]
				top = fmt.Sprintf("%s → %s (%s)", mdCell(p.WaiterQuery), mdCell(p.BlockerQuery), num(float64(p.Count), 0))
			}
			pairs = append(pairs, top)
		}
		lockText, pairText := "-", "-"
		if len(locks) > 0 {
			lockText, pairText = strings.Join(locks, "; "), strings.Join(pairs, "; ")
		}
		md.row(withFailedReads(s.label(), u.Errors), num(100*u.ShareWithAny, 0)+"%", lockText, pairText)
	}
	failedReadsNote(md, marked, "the sampler's connect or read of pg_locks", "`ungranted_locks.errors`")
}

func xidTable(md *markdown, steps []stepData) {
	md.line("\n## Transaction ids\n")
	md.header("step", "transaction ids per s")
	marked := false
	for _, s := range steps {
		r := s.sum.Resources
		switch {
		case r != nil && r.XIDsPerS != nil:
			md.row(s.label(), num(*r.XIDsPerS, 1))
		case r != nil && r.XIDReadError != "":
			// The probe makes one rate from its connect and two reads: a failure of any is one
			// failed read, which leaves no rate.
			marked = true
			md.emptyRow(withFailedReads(s.label(), 1), 2)
		default:
			md.emptyRow(s.label(), 2)
		}
	}
	if marked {
		md.line("\n(1 failed read): the probe's connect or one of its two reads failed; summary.json `resources.xid_read_error` holds the error.")
	}
}

func routesTable(md *markdown, steps []stepData) {
	md.line("\n## Requests by route\n")
	md.line("p50 / p99 ms (failures, non-2xx or transport, / count).\n")
	cells := []string{"route"}
	routes := map[string]bool{}
	for _, s := range steps {
		cells = append(cells, s.label())
		for name := range s.sum.Requests.Routes {
			routes[name] = true
		}
	}
	md.header(cells...)
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		cells := []string{mdEscape(name)}
		for _, s := range steps {
			r, ok := s.sum.Requests.Routes[name]
			if !ok {
				cells = append(cells, "-")
				continue
			}
			cells = append(cells, fmt.Sprintf("%s (%s/%s)", pair(r.LatencyMs, p50, p99, 0), num(float64(r.Failures), 0), num(float64(r.Count), 0)))
		}
		md.row(cells...)
	}
}

// kib renders bytes as KiB.
func kib(bytes float64) string { return num(bytes/1024, 0) }

// scaled is st with every value divided by d, for a column in a larger unit.
func scaled(st stat, d float64) stat {
	return stat{N: st.N, P50: st.P50 / d, P90: st.P90 / d, P99: st.P99 / d, Max: st.Max / d}
}

// reconnectsTable is the live reconnects table: one row per step and reconnect kind.
func reconnectsTable(md *markdown, steps []stepData) {
	md.line("\n## Live reconnects\n")
	md.line("The reconnects of the resuming dashboard live sockets (-live-resume) whose dial started in the window, by the resume they subscribed with: all, last_id (the last event id received), stale (max(id) - %d, at the restart), no_id (none). Replayed: the frames at or below the event log's max(id) read just before the dial. A reconnect whose events_start says `replay_complete: false` refetches the dashboard's routes once.\n", staleBacklog)
	cells := []string{"step", "kind", "reconnects (complete)", "after restart", "failed attempts", "gap s p50 / max",
		"events_start ms p50 / max", "replayed frames per reconnect p50 / max", "replayed KiB per reconnect p50 / max",
		"replayed KiB total", "replay ms p50 / max", "refetches", "refetch requests (failures)", "refetch KiB", "refetch ms p50 / p99"}
	md.header(cells...)
	for _, s := range steps {
		r := s.sum.LiveReconnects
		if r == nil {
			md.emptyRow(s.label(), len(cells))
			continue
		}
		for _, kind := range append([]string{"all"}, reconnectKinds...) {
			g, ok := r.Groups[kind]
			if !ok && kind != "all" {
				continue
			}
			md.row(s.label(), kind, fmt.Sprintf("%s (%s)", num(float64(g.Reconnects), 0), num(float64(g.Complete), 0)),
				num(float64(g.AfterRestart), 0), num(float64(g.FailedAttempts), 0), pair(scaled(g.GapMs, 1000), p50, smax, 1),
				pair(g.StartMs, p50, smax, 0), pair(g.ReplayFrames, p50, smax, 0), pair(scaled(g.ReplayBytes, 1024), p50, smax, 0),
				kib(float64(g.ReplayBytesTotal)), pair(g.ReplayMs, p50, smax, 0), num(float64(g.Refetches), 0),
				fmt.Sprintf("%s (%s)", num(float64(g.RefetchRequests), 0), num(float64(g.RefetchFailures), 0)),
				kib(float64(g.RefetchBytes)), pair(g.RefetchMs, p50, p99, 0))
		}
	}
}

// restartTable is the restart table: one row per step.
func restartTable(md *markdown, steps []stepData) {
	md.line("\n## Restart\n")
	md.line("Dispatch restarted by stack.sh restart-dispatch at -restart-at into the window. CPU-seconds from the restart's start (Dispatch stopping) to 60 s after it, Dispatch's across its old and new pid; per 5 s from the restart's start.\n")
	cells := []string{"step", "restart at s", "restart s (stop to /healthz)", "pid old → new", "Dispatch CPU-s in 60 s",
		"Dispatch CPU-s per 5 s", "Postgres CPU-s in 60 s", "Postgres CPU-s per 5 s", "error"}
	md.header(cells...)
	for _, s := range steps {
		r := s.sum.Restart
		if r == nil {
			md.emptyRow(s.label(), len(cells))
			continue
		}
		opt := func(v *float64) string {
			if v == nil {
				return "-"
			}
			return num(*v, 2)
		}
		list := func(vs []float64) string {
			if len(vs) == 0 {
				return "-"
			}
			out := make([]string, len(vs))
			for i, v := range vs {
				out[i] = num(v, 2)
			}
			return strings.Join(out, " ")
		}
		errText := "-"
		if r.Error != "" {
			errText = mdCell(r.Error)
		}
		md.row(s.label(), num(r.AtS, 0), num(r.DurationS, 1), fmt.Sprintf("%d → %d", r.OldPID, r.NewPID),
			opt(r.DispatchCPUS), list(r.DispatchCPUSPer5s), opt(r.PostgresCPUS), list(r.PostgresCPUSPer5s), errText)
	}
}

// mentionsTable is the comment mentions table: one row per step.
func mentionsTable(md *markdown, steps []stepData) {
	md.line("\n## Comment mentions\n")
	md.line("Comment writes (comment, anchored, reply) with -mention-frac: write ms p50 / p99 over the 2xx ones, then 2xx / offered. Window: the writes scheduled in the window. After restart: the writes sent from the restart's start to 60 s after it.\n")
	cells := []string{"step", "mention share / sessions per comment", "window with mentions", "window without mentions",
		"after restart with mentions", "after restart without mentions"}
	md.header(cells...)
	group := func(g mentionGroup) string {
		if g.Offered == 0 {
			return "-"
		}
		return fmt.Sprintf("%s (%s / %s)", pair(g.WriteMs, p50, p99, 0), num(float64(g.OK2xx), 0), num(float64(g.Offered), 0))
	}
	for _, s := range steps {
		m := s.sum.CommentMentions
		if m == nil {
			md.emptyRow(s.label(), len(cells))
			continue
		}
		after := []string{"-", "-"}
		if m.AfterRestart != nil {
			after = []string{group(m.AfterRestart.With), group(m.AfterRestart.Without)}
		}
		md.row(s.label(), fmt.Sprintf("%s%% / %d", num(100*m.Frac, 0), m.PerComment), group(m.Window.With), group(m.Window.Without), after[0], after[1])
	}
}

// replayTable is one row per replay step (summary.json `replay`); absent when no step replayed.
func replayTable(md *markdown, steps []stepData) {
	var rows []stepData
	for _, s := range steps {
		if s.sum.Replay != nil {
			rows = append(rows, s)
		}
	}
	if len(rows) == 0 {
		return
	}
	md.line("\n## Replay\n")
	md.line("F is the rate factor, rho the read actions per write, U the share of diff versions re-sent as uploads. Cores at full rate are Dispatch's CPU-seconds over the window divided by the window and by F; idle-corrected takes Dispatch's cores over the 30 s before the window off first. Substitutes are anchored quotes / version contents the client could not take from the log.\n")
	md.header("step", "F", "rho", "U", "window s", "Dispatch CPU s", "cores at full rate", "idle-corrected", "saturated",
		"writes sent / planned", "failures", "substitutes quote / edit")
	opt := func(p *float64, d int) string {
		if p == nil {
			return "-"
		}
		return num(*p, d)
	}
	for _, s := range rows {
		r := s.sum.Replay
		sent, planned, failed := 0, 0, 0
		for _, w := range r.Writes {
			sent, planned, failed = sent+w.Sent, planned+w.Planned, failed+w.Failed
		}
		saturated := "no"
		if r.Saturated {
			saturated = "yes (" + strings.Join(r.SaturatedReasons, ", ") + ")"
		}
		md.row(s.label(), num(r.Inputs.RateFactor, 3), num(r.Inputs.ReadsPerWrite, 2), num(r.Inputs.UploadShare, 2), num(r.WindowS, 0),
			opt(r.DispatchCPUS, 1), opt(r.CoresFullRate, 2), opt(r.CoresFullRateIdleCorrected, 2), saturated,
			fmt.Sprintf("%s / %s", num(float64(sent), 0), num(float64(planned), 0)), num(float64(failed), 0),
			fmt.Sprintf("%s / %s", num(float64(r.Substitutes.Quote), 0), num(float64(r.Substitutes.Edit), 0)))
	}
}

// pprofTops writes cpu.top.txt, cpu.cum.txt and heap.top.txt beside each profile with go tool
// pprof. Go profiles carry their symbols, so the binary is passed only when it exists.
func pprofTops(dir, bin string) {
	runs := []struct {
		profile, out string
		args         []string
	}{
		{"cpu.pprof", "cpu.top.txt", []string{"-top", "-nodecount=40"}},
		{"cpu.pprof", "cpu.cum.txt", []string{"-top", "-cum", "-nodecount=80"}},
		{"heap.pprof", "heap.top.txt", []string{"-top", "-nodecount=40", "-sample_index=inuse_space"}},
	}
	for _, r := range runs {
		profile := filepath.Join(dir, r.profile)
		if fi, err := os.Stat(profile); err != nil || fi.Size() == 0 {
			continue
		}
		args := append([]string{"tool", "pprof"}, r.args...)
		if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
			args = append(args, bin)
		}
		args = append(args, profile)
		top, err := exec.Command("go", args...).Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen tables: go %s: %v\n", strings.Join(args, " "), err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, r.out), top, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "loadgen tables: %v\n", err)
		}
	}
}

var frameArgs = regexp.MustCompile(`\([^()]*\)$`)

// goroutineSummary summarizes a goroutine dump (/debug/pprof/goroutine?debug=2): the
// goroutines waiting for a shared-pool connection (pgx pool Acquire), those in
// events.(*Broker).Append (waiting for or holding the commit-order lock), other request and
// document goroutines, and the top states.
func goroutineSummary(step, dump string) string {
	const pkg = "github.com/sjawhar/envoy/internal/dispatch/"
	acq, lock, other, states := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	total := 0
	for block := range strings.SplitSeq(dump, "\n\n") {
		lines := strings.Split(strings.TrimLeft(block, "\n"), "\n")
		hdr := lines[0]
		if !strings.HasPrefix(hdr, "goroutine ") {
			continue
		}
		total++
		state := "?"
		if _, rest, ok := strings.Cut(hdr, "["); ok {
			state, _, _ = strings.Cut(rest, ",")
			state = strings.TrimRight(state, "]:")
		}
		states[state]++
		var frames, mine []string
		for _, l := range lines[1:] {
			if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "\t") {
				continue
			}
			f := frameArgs.ReplaceAllString(strings.TrimSpace(l), "")
			frames = append(frames, f)
			if strings.Contains(f, "sjawhar") {
				mine = append(mine, strings.ReplaceAll(f, pkg, ""))
			}
		}
		head := func(xs []string, from, to int) string {
			from, to = min(from, len(xs)), min(to, len(xs))
			return strings.Join(xs[from:to], " <- ")
		}
		joined := strings.Join(mine, " ")
		switch {
		case strings.Contains(block, "Pool[...]).Acquire"):
			acq[head(mine, 0, 4)]++
		case strings.Contains(joined, "events.(*Broker).Append"):
			lock["["+state+"] "+head(mine, 1, 4)]++
		case strings.Contains(joined, "api.") || strings.Contains(joined, "docs."):
			first := ""
			if len(frames) > 0 {
				first = frames[0]
				if len(first) > 45 {
					first = first[:45]
				}
			}
			other["["+state+"] "+first+" | "+head(mine, 0, 4)]++
		}
	}
	var b strings.Builder
	var top []string
	for i, kv := range byCount(states) {
		if i == 8 {
			break
		}
		top = append(top, fmt.Sprintf("%s %d", kv.k, kv.v))
	}
	fmt.Fprintf(&b, "# %s: %d goroutines; states: %s\n", step, total, strings.Join(top, ", "))
	section := func(title string, m map[string]int, limit int) {
		sum := 0
		for _, v := range m {
			sum += v
		}
		if title != "" {
			fmt.Fprintf(&b, "\n## %s: %d\n", title, sum)
		}
		for i, kv := range byCount(m) {
			if i == limit {
				break
			}
			fmt.Fprintf(&b, "%6d  %s\n", kv.v, kv.k)
		}
	}
	section("waiting for a shared-pool connection (puddle Acquire)", acq, 15)
	section("in events.(*Broker).Append (waiting for or holding the commit-order lock)", lock, 8)
	b.WriteString("\n## other request/document goroutines (top)\n")
	section("", other, 15)
	return b.String()
}

type countKV struct {
	k string
	v int
}

func byCount(m map[string]int) []countKV {
	out := make([]countKV, 0, len(m))
	for k, v := range m {
		out = append(out, countKV{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v || (out[i].v == out[j].v && out[i].k < out[j].k) })
	return out
}

// mdCell is s as text in a markdown table cell: "|" escaped, "(none)" when empty.
func mdCell(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "(none)"
	}
	return mdEscape(s)
}

// mdEscape escapes "|" in s, so s stays inside one markdown table cell.
func mdEscape(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
