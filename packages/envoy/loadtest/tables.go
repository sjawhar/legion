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

// probeLabel is the step's label in a table of a probe, marked "(<n> failed reads)" when the probe
// had failed reads.
func (s stepData) probeLabel(failedReads int) string {
	if failedReads == 0 {
		return s.label()
	}
	return fmt.Sprintf("%s (%d failed reads)", s.label(), failedReads)
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
	sseTable(&md, steps)
	ungrantedTable(&md, steps)
	xidTable(&md, steps)
	routesTable(&md, steps)
	return md.String()
}

func eventsTable(md *markdown, steps []stepData) {
	md.line("## Events, delivery and requests\n")
	md.header("step", "start (UTC)", "sessions", "events/s target / offered / committed", "write errors non-2xx / transport",
		"delivery ms from arrival p50 / p99", "delivery ms from commit p50 / p99", "issue-topic receipts got / expected",
		"req/s target / sent", "req 2xx", "req ms p50 / p99", "requests dropped at the in-flight cap")
	below, exited := false, false
	for _, s := range steps {
		c, e, q := s.sum.Config, s.sum.Events, s.sum.Requests
		start := "-"
		if s.meta != nil && !s.meta.WindowStart.IsZero() {
			start = s.meta.WindowStart.UTC().Format("2006-01-02 15:04:05")
		}
		events := "0"
		if c.EventsPerS > 0 {
			events = fmt.Sprintf("%s / %s / %s", num(c.EventsPerS, 2), num(e.OfferedPerS, 2), num(e.CommittedPerS, 2))
		}
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
		md.row(s.label(), start, num(float64(c.Sessions), 0), events,
			fmt.Sprintf("%s / %s", num(float64(e.WriteErrors.Non2xx), 0), num(float64(e.WriteErrors.Transport), 0)),
			pair(e.DeliveryMsFromArrival, p50, p99, 0), pair(e.DeliveryMsFromCommit, p50, p99, 0),
			fmt.Sprintf("%s / %s", num(float64(e.IssueTopicReceipts), 0), num(float64(e.ExpectedIssueTopicReceipts), 0)),
			reqRate, share, pair(q.LatencyMs, p50, p99, 0), num(float64(q.DroppedInflightCap), 0))
	}
	if below {
		md.line("\n(below 95%%): the step sent fewer than 95%% of its target requests per second, so its request columns describe a lighter load than its name.")
	}
	if exited {
		md.line("\n(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.")
	}
}

func kindsTable(md *markdown, steps []stepData) {
	md.line("\n## Write latency by kind\n")
	md.line("Write ms p50 / p99 over 2xx writes, then committed / offered.\n")
	cells := []string{"step"}
	for _, k := range eventKinds {
		cells = append(cells, k+" ms p50 / p99 (committed / offered)")
	}
	md.header(cells...)
	for _, s := range steps {
		cells := []string{s.label()}
		for _, k := range eventKinds {
			ks, ok := s.sum.Events.ByKind[k]
			if !ok || ks.Offered == 0 {
				cells = append(cells, "-")
				continue
			}
			cells = append(cells, fmt.Sprintf("%s (%s / %s)", pair(ks.WriteMs, p50, p99, 0), num(float64(ks.Committed), 0), num(float64(ks.Offered), 0)))
		}
		md.row(cells...)
	}
}

func resourcesTable(md *markdown, steps []stepData) {
	md.line("\n## Resources\n")
	md.header("step", "Dispatch cores mean / peak 5 s", "Postgres cores", "listener cores", "NATS cores", "loadgen cores", "proxy cores",
		"box busy", "box busy before", "Dispatch RSS max MiB", "heap in use MiB p50 / max", "goroutines p50 / max",
		"goroutines waiting for a pool connection p50 / max", "Postgres connections max", "connections waiting on a lock p50",
		"Dispatch CPU-s per operation")
	for _, s := range steps {
		busyBefore := "-"
		if s.meta != nil && s.meta.BoxBusyBefore != nil {
			busyBefore = num(100**s.meta.BoxBusyBefore, 0) + "%"
		}
		r := s.sum.Resources
		if r == nil {
			// No sampler window: every cell is "-" but box busy before, which is meta.json's
			// reading, not the sampler's.
			md.row(s.label(), "-", "-", "-", "-", "-", "-", "-", busyBefore, "-", "-", "-", "-", "-", "-", "-")
			continue
		}
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
		md.row(s.label(), dispatch, cores("postgres", 2), cores("listener", 3), cores("nats", 3), cores("loadgen", 2), cores("proxy", 2),
			num(100*r.SysBusyFrac, 0)+"%", busyBefore, rss, pair(r.HeapInuseMiB, p50, smax, 0), pair(r.Goroutines, p50, smax, 0),
			pair(r.PoolAcquireGoroutines, p50, smax, 0), pgMax, lockWait, perOp)
	}
}

// failedReadsNote defines the "(<n> failed reads)" mark under a probe's table, when a row has it.
func failedReadsNote(md *markdown, marked bool, key string) {
	if marked {
		md.line("\n(<n> failed reads): the probe's read of pg_locks failed n times, a failed connect included (summary.json `%s.errors`), so its samples miss those reads; a row with no sample reads `-`, not 0.", key)
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
			md.emptyRow(s.probeLabel(l.Errors), len(columns))
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
		md.row(s.probeLabel(l.Errors), num(100*l.HeldFraction, 0)+"%", pair(l.Waiters, p50, smax, 0), holds, num(float64(l.HoldsObserved), 0), strings.Join(doing, "; "))
	}
	failedReadsNote(md, marked, "commit_order_lock")
}

func sseTable(md *markdown, steps []stepData) {
	md.line("\n## Delivery from sending, and SSE frames\n")
	md.line("NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.\n")
	md.header("step", "NATS delivery ms from sent p50 / p99", "SSE frame ms from created_at p50 / p99",
		"SSE frame ms from write response p50 / p99", "SSE frames", "SSE order violations")
	for _, s := range steps {
		sse := s.sum.SSE
		md.row(s.label(), pair(s.sum.Events.DeliveryMsFromSent, p50, p99, 0), pair(sse.FrameMsFromCreatedAt, p50, p99, 0),
			pair(sse.FrameMsFromWriteResponse, p50, p99, 0), num(float64(sse.Frames), 0), num(float64(sse.OrderViolations), 0))
	}
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
			md.emptyRow(s.probeLabel(u.Errors), len(columns))
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
		md.row(s.probeLabel(u.Errors), num(100*u.ShareWithAny, 0)+"%", lockText, pairText)
	}
	failedReadsNote(md, marked, "ungranted_locks")
}

func xidTable(md *markdown, steps []stepData) {
	md.line("\n## Transaction ids\n")
	md.header("step", "transaction ids per s")
	for _, s := range steps {
		if r := s.sum.Resources; r != nil && r.XIDsPerS != nil {
			md.row(s.label(), num(*r.XIDsPerS, 1))
			continue
		}
		md.emptyRow(s.label(), 2)
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
