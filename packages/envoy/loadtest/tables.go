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
	"strconv"
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

func renderTables(steps []stepData) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	row := func(cells ...string) { w("| %s |", strings.Join(cells, " | ")) }
	header := func(cells ...string) {
		row(cells...)
		w("|%s", strings.Repeat("---|", len(cells)))
	}

	w("# Load test tables\n")
	w("One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.\n")

	w("## Events, delivery and requests\n")
	header("step", "start (UTC)", "sessions", "events/s target / offered / committed", "write errors non-2xx / transport",
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
		row(s.label(), start, num(float64(c.Sessions), 0), events,
			fmt.Sprintf("%s / %s", num(float64(e.WriteErrors.Non2xx), 0), num(float64(e.WriteErrors.Transport), 0)),
			pair(e.DeliveryMsFromArrival, p50, p99, 0), pair(e.DeliveryMsFromCommit, p50, p99, 0),
			fmt.Sprintf("%s / %s", num(float64(e.IssueTopicReceipts), 0), num(float64(e.ExpectedIssueTopicReceipts), 0)),
			reqRate, share, pair(q.LatencyMs, p50, p99, 0), num(float64(q.DroppedInflightCap), 0))
	}
	if below {
		w("\n(below 95%%): the step sent fewer than 95%% of its target requests per second, so its request columns describe a lighter load than its name.")
	}
	if exited {
		w("\n(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.")
	}

	w("\n## Write latency by kind\n")
	w("Write ms p50 / p99 over 2xx writes, then committed / offered.\n")
	cells := []string{"step"}
	for _, k := range eventKinds {
		cells = append(cells, k+" ms p50 / p99 (committed / offered)")
	}
	header(cells...)
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
		row(cells...)
	}

	w("\n## Resources\n")
	resourceCells := []string{"step", "Dispatch cores mean / peak 5 s", "Postgres cores", "listener cores", "NATS cores", "loadgen cores", "proxy cores",
		"box busy", "box busy before", "Dispatch RSS max MiB", "heap in use MiB p50 / max", "goroutines p50 / max",
		"goroutines waiting for a pool connection p50 / max", "Postgres connections max", "connections waiting on a lock p50",
		"Dispatch CPU-s per operation"}
	header(resourceCells...)
	for _, s := range steps {
		busyBefore := "-"
		if s.meta != nil && s.meta.BoxBusyBefore != nil {
			busyBefore = num(100**s.meta.BoxBusyBefore, 0) + "%"
		}
		r := s.sum.Resources
		if r == nil {
			cells := []string{s.label()}
			for range len(resourceCells) - 1 {
				cells = append(cells, "-")
			}
			// The reading before the step is meta.json's, not the sampler's.
			cells[slices.Index(resourceCells, "box busy before")] = busyBefore
			row(cells...)
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
		row(s.label(), dispatch, cores("postgres", 2), cores("listener", 3), cores("nats", 3), cores("loadgen", 2), cores("proxy", 2),
			num(100*r.SysBusyFrac, 0)+"%", busyBefore, rss, pair(r.HeapInuseMiB, p50, smax, 0), pair(r.Goroutines, p50, smax, 0),
			pair(r.PoolAcquireGoroutines, p50, smax, 0), pgMax, lockWait, perOp)
	}

	w("\n## Commit-order lock\n")
	w("Sampled every 20 ms from 20 s into the window to its end.\n")
	lockCells := []string{"step", "time held", "waiters p50 / max", "hold ms p50 / p90 / p99", "holds seen", "what the holder was doing (share of held samples)"}
	header(lockCells...)
	for _, s := range steps {
		l := s.sum.CommitOrderLock
		if l.Samples == 0 {
			cells := []string{s.label()}
			for range len(lockCells) - 1 {
				cells = append(cells, "-")
			}
			row(cells...)
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
			doing = append(doing, fmt.Sprintf("%s %s%%", strings.ReplaceAll(what, "|", `\|`), num(100*d.Share, 0)))
		}
		row(s.label(), num(100*l.HeldFraction, 0)+"%", pair(l.Waiters, p50, smax, 0), holds, num(float64(l.HoldsObserved), 0), strings.Join(doing, "; "))
	}

	emptyRow := func(label string, columns int) {
		cells := []string{label}
		for range columns - 1 {
			cells = append(cells, "-")
		}
		row(cells...)
	}

	w("\n## Delivery from sending, and SSE frames\n")
	w("NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.\n")
	sseCells := []string{"step", "NATS delivery ms from sent p50 / p99", "SSE frame ms from created_at p50 / p99",
		"SSE frame ms from write response p50 / p99", "SSE frames", "SSE order violations"}
	header(sseCells...)
	for _, s := range steps {
		sse := s.sum.SSE
		if sse == nil {
			cells := []string{s.label(), pair(s.sum.Events.DeliveryMsFromSent, p50, p99, 0)}
			for range len(sseCells) - 2 {
				cells = append(cells, "-")
			}
			row(cells...)
			continue
		}
		row(s.label(), pair(s.sum.Events.DeliveryMsFromSent, p50, p99, 0), pair(sse.FrameMsFromCreatedAt, p50, p99, 0),
			pair(sse.FrameMsFromWriteResponse, p50, p99, 0), num(float64(sse.Frames), 0), num(float64(sse.OrderViolations), 0))
	}

	w("\n## Ungranted locks\n")
	w("Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.\n")
	ungrantedCells := []string{"step", "samples with an ungranted lock", "locks waited for: type target mode, samples with a waiter, waiters p50 / max",
		"top waiter query → first blocker query (count), per lock"}
	header(ungrantedCells...)
	for _, s := range steps {
		u := s.sum.UngrantedLocks
		if u.Samples == 0 {
			emptyRow(s.label(), len(ungrantedCells))
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
		row(s.label(), num(100*u.ShareWithAny, 0)+"%", lockText, pairText)
	}

	w("\n## Transaction ids\n")
	header("step", "transaction ids per s")
	for _, s := range steps {
		if r := s.sum.Resources; r != nil && r.XIDsPerS != nil {
			row(s.label(), num(*r.XIDsPerS, 1))
			continue
		}
		emptyRow(s.label(), 2)
	}

	w("\n## Editors\n")
	w("Browser editors on document websockets. Updates, latencies and markers count the updates scheduled in the window; connects, connect failures and closes count the whole step. Relay: one editor's send to another editor's receipt of that update on the same document. Durable: send to the marker's row in doc_updates, ±10 ms.\n")
	editorCells := []string{"step", "editors on documents (with one editor)", "updates/s target / sent", "updates sent / acknowledged / refused",
		"updates not sent", "relay ms p50 / p99", "durable ms p50 / p99 (found / markers)", "connects / connect failures", "closes by code"}
	header(editorCells...)
	var single, editorNotes []string
	// counted is a count map as "<total> (<key> <n>, ...)", most frequent first, or "0".
	counted := func(m map[string]int) string {
		total := 0
		var parts []string
		for _, kv := range byCount(m) {
			total += kv.v
			parts = append(parts, fmt.Sprintf("%s %s", mdCell(kv.k), num(float64(kv.v), 0)))
		}
		if total == 0 {
			return "0"
		}
		return num(float64(total), 0) + " (" + strings.Join(parts, ", ") + ")"
	}
	for _, s := range steps {
		e := s.sum.Editors
		if e == nil {
			emptyRow(s.label(), len(editorCells))
			continue
		}
		if len(e.SingleEditorDocs) > 0 {
			single = append(single, fmt.Sprintf("%s: %s", s.label(), strings.Join(e.SingleEditorDocs, ", ")))
		}
		durable := "-"
		if e.DurableNote == "" {
			durable = fmt.Sprintf("%s (%s / %s)", pair(e.DurableMs, p50, p99, 0), num(float64(e.MarkersFound), 0), num(float64(e.Markers), 0))
		} else {
			editorNotes = append(editorNotes, fmt.Sprintf("%s: durable latency %s", s.label(), e.DurableNote))
		}
		var closes []string
		for _, kv := range byCount(e.Closes) {
			closes = append(closes, fmt.Sprintf("%s: %s", mdCell(kv.k), num(float64(kv.v), 0)))
		}
		closeText := "0"
		if len(closes) > 0 {
			closeText = strings.Join(closes, "; ")
		}
		row(s.label(), fmt.Sprintf("%s on %s (%s)", num(float64(e.Editors), 0), num(float64(e.Docs), 0), num(float64(len(e.SingleEditorDocs)), 0)),
			num(e.TargetPerS, 1)+" / "+num(e.SentPerS, 1),
			fmt.Sprintf("%s / %s / %s", num(float64(e.Sent), 0), num(float64(e.Acknowledged), 0), num(float64(e.Refused), 0)),
			counted(e.NotSent), pair(e.RelayMs, p50, p99, 0), durable,
			num(float64(e.Connects), 0)+" / "+counted(e.ConnectFailures), closeText)
	}
	if len(single) > 0 {
		w("\nDocuments with one editor, which have no relay latency: %s.", strings.Join(single, "; "))
	}
	for _, n := range editorNotes {
		w("\n%s.", strings.TrimSuffix(n, "."))
	}

	w("\n## Document rooms\n")
	w("Round trips: the latency proxy's count of Dispatch's round trips (each Sync and simple Query) from the window's start to its end; room work names a room table or the document's advisory lock. Connections held by rooms: backends holding or waiting for a document advisory lock, or running a statement on doc_updates, every 100 ms in a step with editors. Batches: dispatch_rooms on /debug/vars. Versions: artifact_versions' growth, and the proxy's timing of its inserts.\n")
	roomCells := []string{"step", "round trips room / all", "room round trips per update", "connections held by rooms p50 / p99 / max",
		"shared-pool share", "append batches / updates / round trips", "batch sizes (updates: batches)", "artifact_versions KiB/s",
		"versions written", "version insert ms p50 / p99 (count, total ms)"}
	header(roomCells...)
	var roomNotes []string
	for _, s := range steps {
		r := s.sum.Rooms
		if r == nil {
			emptyRow(s.label(), len(roomCells))
			continue
		}
		trips, perUpdate := "-", "-"
		if rt := r.RoundTrips; rt != nil {
			trips = num(float64(rt.Room), 0) + " / " + num(float64(rt.All), 0)
			if rt.RoomPerUpdate != nil {
				perUpdate = num(*rt.RoomPerUpdate, 2)
			}
			if rt.UnparsedConnections > 0 {
				roomNotes = append(roomNotes, fmt.Sprintf("%s: the proxy could not follow %s connections, whose round trips are not counted", s.label(), num(float64(rt.UnparsedConnections), 0)))
			}
		} else {
			roomNotes = append(roomNotes, fmt.Sprintf("%s: round trips not measured: %s", s.label(), r.RoundTripsNote))
		}
		held, share := "-", "-"
		if c := r.Connections; c != nil && c.Held.N > 0 {
			held = fmt.Sprintf("%s / %s / %s", num(c.Held.P50, 0), num(c.Held.P99, 0), num(c.Held.Max, 0))
			if c.SharedPoolShare != nil {
				share = num(100**c.SharedPoolShare, 0) + "%"
			}
		}
		batches, sizes := r.BatchesNote, r.BatchesNote
		if b := r.Batches; b != nil {
			batches = fmt.Sprintf("%s / %s / %s", num(float64(b.AppendBatches), 0), num(float64(b.AppendedUpdates), 0), num(float64(b.AppendRoundTrips), 0))
			sizes = batchSizesText(b.BatchSizes)
		}
		v := r.Versions
		kibPerS, written := "-", "-"
		if v.BytesPerS != nil {
			kibPerS = num(*v.BytesPerS/1024, 1)
		}
		if v.Written != nil {
			written = num(float64(*v.Written), 0)
		}
		if v.Note != "" {
			roomNotes = append(roomNotes, fmt.Sprintf("%s: artifact_versions not read: %s", s.label(), v.Note))
		}
		inserts := "-"
		if v.InsertCount != nil && v.InsertTotalMs != nil {
			inserts = fmt.Sprintf("%s (%s, %s)", pair(v.InsertMs, p50, p99, 1), num(float64(*v.InsertCount), 0), num(*v.InsertTotalMs, 0))
		}
		row(s.label(), trips, perUpdate, held, share, mdCell(batches), mdCell(sizes), kibPerS, written, inserts)
	}
	for _, n := range roomNotes {
		w("\n%s.", strings.TrimSuffix(n, "."))
	}

	w("\n## Requests by route\n")
	w("p50 / p99 ms (failures, non-2xx or transport, / count).\n")
	cells = []string{"route"}
	routes := map[string]bool{}
	for _, s := range steps {
		cells = append(cells, s.label())
		for name := range s.sum.Requests.Routes {
			routes[name] = true
		}
	}
	header(cells...)
	names := slices.Sorted(maps.Keys(routes))
	for _, name := range names {
		cells := []string{strings.ReplaceAll(name, "|", `\|`)}
		for _, s := range steps {
			r, ok := s.sum.Requests.Routes[name]
			if !ok {
				cells = append(cells, "-")
				continue
			}
			cells = append(cells, fmt.Sprintf("%s (%s/%s)", pair(r.LatencyMs, p50, p99, 0), num(float64(r.Failures), 0), num(float64(r.Count), 0)))
		}
		row(cells...)
	}
	return b.String()
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
	return strings.ReplaceAll(s, "|", `\|`)
}

// batchSizesText is dispatch_rooms' batch-size histogram as "<updates>: <batches>" for each
// bucket that holds a batch, smallest bucket first; "0" when none does.
func batchSizesText(sizes map[string]uint64) string {
	lower := func(label string) int {
		n, _ := strconv.Atoi(strings.SplitN(label, "-", 2)[0])
		return n
	}
	labels := slices.SortedFunc(maps.Keys(sizes), func(a, b string) int { return lower(a) - lower(b) })
	var parts []string
	for _, label := range labels {
		if n := sizes[label]; n > 0 {
			parts = append(parts, label+": "+num(float64(n), 0))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}
