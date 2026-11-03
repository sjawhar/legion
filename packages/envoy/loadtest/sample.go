package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/jackc/pgx/v5"
)

// clkTck is the kernel's USER_HZ, the unit of /proc/<pid>/stat CPU times.
const clkTck = 100.0

type sample struct {
	At         time.Time          `json:"at"`
	CPUTicks   map[string]float64 `json:"cpu_ticks"` // utime+stime ticks; postgres adds its reaped children
	RSSKiB     map[string]float64 `json:"rss_kib"`
	SysBusy    float64            `json:"sys_busy_ticks"`
	SysTotal   float64            `json:"sys_total_ticks"`
	PGConns    map[string]int     `json:"pg_conns"` // state|wait_event_type -> n (datname=dispatch)
	PGTotal    int                `json:"pg_total"`
	Goroutines int                `json:"goroutines"`
	PoolWait   int                `json:"pool_acquire_goroutines"`
	// HeapLiveBytes and HeapLiveObjects are the heap read this sample carries, if any: the sums of
	// /debug/pprof/heap's inuse_space and inuse_objects, the runtime's sampled estimate of the live
	// heap at its last GC, not runtime.MemStats HeapInuse. The read runs on its own
	// (sampler.readHeaps) and lands in the first sample that looks for it once it has ended.
	// HeapAt is when it ended (zero when the sample carries none), and HeapInWindow applies
	// InWindow's rule to HeapAt, so the window holds a read by its own time, not its sample's.
	HeapLiveBytes   float64 `json:"heap_live_bytes"`
	HeapLiveObjects float64 `json:"heap_live_objects"`
	// MemStatsHeapInuse is an older run's heap read (samples.json `heap_inuse`): runtime.MemStats
	// HeapInuse from /debug/pprof/heap?debug=1. A run now never writes it; it is read only to
	// render an older step directory, under its own label.
	MemStatsHeapInuse float64            `json:"heap_inuse,omitzero"`
	HeapAt            time.Time          `json:"heap_at,omitzero"`
	HeapInWindow      bool               `json:"heap_in_window,omitempty"`
	NATS              map[string]float64 `json:"nats"`
	InWindow          bool               `json:"in_window"`
	// FailedReads names the reads of this sample that failed: readPostgres, readGoroutines,
	// readHeap, readNATS. Their values above stay 0 (NATS empty) and enter no statistic.
	FailedReads []string `json:"failed_reads,omitempty"`
}

// The sampler's reads that can fail, as sample.FailedReads names them: pg_stat_activity (a failed
// connect included), Dispatch's goroutine and heap profiles, and NATS /varz.
const (
	readPostgres   = "postgres"
	readGoroutines = "goroutines"
	readHeap       = "heap"
	readNATS       = "nats"
)

// sampler takes a sample every 5 s: CPU and RSS of each stack process and of loadgen from /proc,
// Postgres connections, Dispatch's goroutines and pool waiters, NATS /varz and /jsz, and the
// pull-request counts of the pull spy. Dispatch's heap is read on its own every 15 s (readHeaps), so
// that a slow read of a large heap delays no sample. A failed Postgres, pprof or NATS /varz read is
// marked in its sample (sample.FailedReads); a /jsz read that fails leaves its keys out of the
// sample. At each sample it also checks that every stack process is alive (checkAlive).
type sampler struct {
	pids    map[string]int // by name; "postgres" is the postmaster
	pprof   string
	natsMon string
	pgURL   string
	// pulls counts the pull requests on the stream's consumers; nil when the spy did not start.
	pulls   *pullSpy
	mu      sync.Mutex
	samples []sample
	exits   map[string]stackExit // by name, the stack processes found gone
}

// stackExit is a stack process that exited during the step: the first check that found it gone.
type stackExit struct {
	Name          string    `json:"name"`
	PID           int       `json:"pid"`
	FirstSeenGone time.Time `json:"first_seen_gone"`
}

func newSampler(ep endpoints) *sampler {
	return &sampler{pids: ep.pids, pprof: ep.pprof, natsMon: ep.natsMon, pgURL: ep.pg, exits: map[string]stackExit{}}
}

// checkAlive records each stack process that is no longer alive, with the time it was first
// seen gone. Under -allow-foreign there are no pids, so nothing is checked.
func (s *sampler) checkAlive(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, pid := range s.pids {
		if _, seen := s.exits[name]; !seen && !procAlive(pid) {
			s.exits[name] = stackExit{Name: name, PID: pid, FirstSeenGone: now}
		}
	}
}

// stackExits lists the stack processes found gone, in the order they were first seen gone.
func (s *sampler) stackExits() []stackExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Collect(maps.Values(s.exits))
	slices.SortFunc(out, func(a, b stackExit) int {
		if c := a.FirstSeenGone.Compare(b.FirstSeenGone); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// procStatFields are the fields of /proc/<pid>/stat after the command name.
type procStatFields struct {
	state                        string
	ppid                         int
	utime, stime, cutime, cstime float64
}

func readProcStat(pid int) (procStatFields, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStatFields{}, false
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return procStatFields{}, false
	}
	// After ")": state(0) ppid(1) ... utime(11) stime(12) cutime(13) cstime(14).
	f := strings.Fields(s[end+2:])
	if len(f) < 15 {
		return procStatFields{}, false
	}
	var p procStatFields
	p.state = f[0]
	p.ppid, _ = strconv.Atoi(f[1])
	p.utime, _ = strconv.ParseFloat(f[11], 64)
	p.stime, _ = strconv.ParseFloat(f[12], 64)
	p.cutime, _ = strconv.ParseFloat(f[13], 64)
	p.cstime, _ = strconv.ParseFloat(f[14], 64)
	return p, true
}

func procRSS(pid int) float64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			if fields := strings.Fields(v); len(fields) > 0 {
				n, _ := strconv.ParseFloat(fields[0], 64)
				return n
			}
		}
	}
	return 0
}

// procAlive is whether pid is a live process: /proc/<pid> exists and it is not a zombie (Z) or
// dead (X) task.
func procAlive(pid int) bool {
	st, ok := readProcStat(pid)
	return ok && st.state != "Z" && st.state != "X"
}

// childrenOf lists the live processes whose parent is pid.
func childrenOf(pid int) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, ok := readProcStat(child); ok && st.ppid == pid {
			out = append(out, child)
		}
	}
	return out
}

// postgresUsage is the stack's Postgres CPU and RSS: the postmaster, its live children (the
// backends and background workers) and the postmaster's reaped-child time, which holds the CPU
// of backends that have exited. Only the stack's own postmaster counts, never every process
// named postgres on the box.
func postgresUsage(postmaster int) (ticks, rssKiB float64, ok bool) {
	st, ok := readProcStat(postmaster)
	if !ok {
		return 0, 0, false
	}
	ticks = st.utime + st.stime + st.cutime + st.cstime
	rssKiB = procRSS(postmaster)
	for _, child := range childrenOf(postmaster) {
		if c, ok := readProcStat(child); ok {
			ticks += c.utime + c.stime
			rssKiB += procRSS(child)
		}
	}
	return ticks, rssKiB, true
}

// sysCPU reads the box's busy (not idle or I/O wait) and total CPU ticks, all CPUs, from the
// first line of /proc/stat.
func sysCPU() (busy, total float64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0, 0
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseFloat(v, 64)
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total
}

// boxBusy is the box's busy fraction over window, from two /proc/stat reads window apart; false
// when /proc/stat gave no ticks or ctx ended first. Every process on the box counts, also those of
// other PID namespaces that ps does not show.
func boxBusy(ctx context.Context, window time.Duration) (float64, bool) {
	busy0, total0 := sysCPU()
	if !sleepUntil(ctx, time.Now().Add(window)) {
		return 0, false
	}
	busy1, total1 := sysCPU()
	if total1 <= total0 {
		return 0, false
	}
	return (busy1 - busy0) / (total1 - total0), true
}

// loop samples every 5 s until ctx ends. A failed read is marked in its sample and logged as
// readErrors does, one count each for the Postgres, the pprof and the NATS reads. A read that
// failed once ctx had ended is neither marked nor counted, as poll does. The heap reads run in
// readHeaps; each sample takes the one that has ended since the last sample, if any, in place of
// reading the heap itself. A heap read that ends once the last sample is taken is lost.
func (s *sampler) loop(ctx context.Context, winStart, winEnd time.Time, lg *log.Logger) {
	var pg *pgx.Conn
	pgErrs := readErrors{name: "sampler: postgres", lg: lg}
	pprofErrs := readErrors{name: "sampler: pprof", lg: lg}
	natsErrs := readErrors{name: "sampler: nats", lg: lg}
	heaps, heapsDone := make(chan heapRead, 1), make(chan struct{})
	go func() {
		defer close(heapsDone)
		s.readHeaps(ctx, heaps)
	}()
	defer func() {
		if pg != nil {
			pg.Close(context.Background())
		}
		pgErrs.done()
		pprofErrs.done()
		natsErrs.done()
		<-heapsDone // loop returns only once ctx has ended, which ends readHeaps
	}()
	inWindow := func(at time.Time) bool { return !at.Before(winStart) && !at.After(winEnd.Add(5*time.Second)) }
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	self := os.Getpid()
	for {
		sm := sample{At: time.Now(), CPUTicks: map[string]float64{}, RSSKiB: map[string]float64{}, PGConns: map[string]int{}, NATS: map[string]float64{}}
		sm.InWindow = inWindow(sm.At)
		// failed marks and counts a failed read, unless it failed because ctx ended.
		failed := func(read string, errs *readErrors, err error) {
			if ctx.Err() != nil {
				return
			}
			sm.FailedReads = append(sm.FailedReads, read)
			errs.add(err)
		}
		s.checkAlive(sm.At)
		for name, pid := range s.pids {
			if name == "postgres" {
				if tk, rss, ok := postgresUsage(pid); ok {
					sm.CPUTicks[name], sm.RSSKiB[name] = tk, rss
				}
				continue
			}
			if st, ok := readProcStat(pid); ok {
				sm.CPUTicks[name] = st.utime + st.stime
				sm.RSSKiB[name] = procRSS(pid)
			}
		}
		if st, ok := readProcStat(self); ok {
			sm.CPUTicks["loadgen"] = st.utime + st.stime
			sm.RSSKiB["loadgen"] = procRSS(self)
		}
		sm.SysBusy, sm.SysTotal = sysCPU()
		var err error
		if pg == nil {
			pg, err = pgx.Connect(ctx, s.pgURL)
		}
		if err == nil {
			var conns map[string]int
			if conns, sm.PGTotal, err = pgActivity(ctx, pg); err == nil {
				sm.PGConns = conns
			} else {
				pg.Close(context.Background())
				pg = nil
			}
		}
		if err != nil {
			failed(readPostgres, &pgErrs, err)
		}
		if sm.Goroutines, sm.PoolWait, err = goroutineCounts(s.pprof); err != nil {
			failed(readGoroutines, &pprofErrs, err)
		}
		select {
		case h := <-heaps:
			sm.HeapAt, sm.HeapInWindow = h.at, inWindow(h.at)
			if h.err != nil {
				failed(readHeap, &pprofErrs, h.err)
			} else {
				sm.HeapLiveBytes, sm.HeapLiveObjects = h.bytes, h.objects
			}
		default:
		}
		if v, err := natsVarz(s.natsMon); err != nil {
			failed(readNATS, &natsErrs, err)
		} else {
			sm.NATS = v
		}
		maps.Copy(sm.NATS, natsJsz(s.natsMon))
		if s.pulls != nil {
			sm.NATS["pull_requests"] = float64(s.pulls.total.Load())
			sm.NATS["session_pull_requests"] = float64(s.pulls.sessions.Load())
		}
		s.mu.Lock()
		// A sample whose reads ran once the step had ended keeps none of them, as poll keeps no
		// read then: its failed reads would enter the statistics as zeros (a 0 connection count,
		// 0 goroutines, a NATS rate below zero).
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		s.samples = append(s.samples, sm)
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// heapReadEvery is how often readHeaps starts a heap read; heapReadTimeout bounds one read. The
// runtime walks every heap record to build the profile, so a read takes longer as the heap grows.
const (
	heapReadEvery   = 15 * time.Second
	heapReadTimeout = 2 * time.Minute
)

// heapRead is one ended read of Dispatch's heap: when it ended, and its values or why it failed.
type heapRead struct {
	at             time.Time
	bytes, objects float64
	err            error
}

// readHeaps reads Dispatch's heap at once, then at each heapReadEvery slot after, one read at a
// time: a slot that falls while a read is in flight, or waits for a sample to take it, is skipped.
// It hands each ended read to out, and returns once ctx has ended.
func (s *sampler) readHeaps(ctx context.Context, out chan<- heapRead) {
	for next := time.Now(); sleepUntil(ctx, next); {
		var h heapRead
		h.bytes, h.objects, h.err = heapLive(ctx, s.pprof)
		h.at = time.Now()
		select {
		case out <- h:
		case <-ctx.Done():
			return
		}
		next = next.Add(heapReadEvery)
		if behind := time.Since(next); behind > 0 {
			next = next.Add(behind.Truncate(heapReadEvery) + heapReadEvery)
		}
	}
}

// pgActivity counts the connections to database dispatch in pg_stat_activity, by
// "state|wait_event_type", and in all.
func pgActivity(ctx context.Context, conn *pgx.Conn) (map[string]int, int, error) {
	rows, err := conn.Query(ctx, `select coalesce(state,'') || '|' || coalesce(wait_event_type,''), count(*)::int from pg_stat_activity where datname='dispatch' group by 1`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	conns, total := map[string]int{}, 0
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, 0, err
		}
		conns[k] = v
		total += v
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return conns, total, nil
}

func (s *sampler) all() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sample(nil), s.samples...)
}

var shortClient = &http.Client{Timeout: 10 * time.Second}

// getOK GETs url with client, until ctx ends; an answer other than 200 is an error.
func getOK(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s answered %d %s", redactURL(url), resp.StatusCode, trunc(b))
	}
	return resp.Body, nil
}

// goroutineCounts reads Dispatch's goroutine total and how many wait in pgx's pool Acquire
// (puddle frames) from /debug/pprof/goroutine?debug=1; an error when the read fails or the
// profile names no total.
func goroutineCounts(pprof string) (total, poolWait int, err error) {
	url := pprof + "/debug/pprof/goroutine?debug=1"
	body, err := getOK(context.Background(), shortClient, url)
	if err != nil {
		return 0, 0, err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	block := 0
	inPool, haveTotal := false, false
	flush := func() {
		if inPool {
			poolWait += block
		}
		block, inPool = 0, false
	}
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "goroutine profile: total "); ok {
			total, err = strconv.Atoi(v)
			haveTotal = err == nil
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if !strings.HasPrefix(line, "#") {
			flush()
			if f := strings.Fields(line); len(f) > 0 {
				block, _ = strconv.Atoi(f[0])
			}
			continue
		}
		if strings.Contains(line, "puddle/v2.(*Pool[...]).Acquire") || strings.Contains(line, "puddle/v2.(*Pool).Acquire") {
			inPool = true
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	if !haveTotal {
		return 0, 0, fmt.Errorf("GET %s: no goroutine profile total", redactURL(url))
	}
	return total, poolWait, nil
}

// heapLive reads Dispatch's live heap from /debug/pprof/heap, the binary profile, within
// heapReadTimeout and until ctx ends: the sums of its inuse_space and inuse_objects samples, which
// the runtime has already scaled from its sampled records to estimates of the whole heap. It never
// reads ?debug=1: that text formats every heap record, grew past 500 MB, and its reads took 29% of
// Dispatch's CPU in the r16 replay. An error when the read fails, the profile does not parse, or it
// has no inuse_space or inuse_objects.
func heapLive(ctx context.Context, pprof string) (bytes, objects float64, err error) {
	url := pprof + "/debug/pprof/heap"
	body, err := getOK(ctx, &http.Client{Timeout: heapReadTimeout}, url)
	if err != nil {
		return 0, 0, err
	}
	defer body.Close()
	p, err := profile.Parse(body)
	if err != nil {
		return 0, 0, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	bi, err := valueIndex(p, "inuse_space")
	if err != nil {
		return 0, 0, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	oi, err := valueIndex(p, "inuse_objects")
	if err != nil {
		return 0, 0, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	var b, o int64
	for _, s := range p.Sample {
		b += s.Value[bi]
		o += s.Value[oi]
	}
	return float64(b), float64(o), nil
}

// natsVarz reads NATS's CPU, memory, connections, message counts, slow consumers and subscriptions
// from /varz; an error when the GET fails, answers other than 200 or holds no JSON.
func natsVarz(mon string) (map[string]float64, error) {
	url := mon + "/varz"
	body, err := getOK(context.Background(), shortClient, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var v struct {
		CPU         float64 `json:"cpu"`
		Mem         float64 `json:"mem"`
		Connections float64 `json:"connections"`
		InMsgs      float64 `json:"in_msgs"`
		OutMsgs     float64 `json:"out_msgs"`
		SlowCons    float64 `json:"slow_consumers"`
		Subs        float64 `json:"subscriptions"`
	}
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	return map[string]float64{"cpu_pct": v.CPU, "mem": v.Mem, "connections": v.Connections, "in_msgs": v.InMsgs, "out_msgs": v.OutMsgs, "slow_consumers": v.SlowCons, "subscriptions": v.Subs}, nil
}

// natsJsz reads NATS /jsz: the JetStream consumers, streams and messages, and the JetStream API
// requests and errors; nil when the read fails.
func natsJsz(mon string) map[string]float64 {
	resp, err := shortClient.Get(mon + "/jsz")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var v struct {
		Streams   float64 `json:"streams"`
		Consumers float64 `json:"consumers"`
		Messages  float64 `json:"messages"`
		API       struct {
			Total  float64 `json:"total"`
			Errors float64 `json:"errors"`
		} `json:"api"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return map[string]float64{"js_streams": v.Streams, "js_consumers": v.Consumers, "js_messages": v.Messages, "js_api_total": v.API.Total, "js_api_errors": v.API.Errors}
}

// xidRate reads the next transaction id to be assigned at from and at to, over one direct
// Postgres connection, and returns the transaction ids assigned per second between the two reads.
// When the connect or either read fails, or ctx ends first, it returns no rate and the error,
// logged. pg_current_snapshot assigns no transaction id, so the probe does not count itself.
func xidRate(ctx context.Context, url string, from, to time.Time, lg *log.Logger) (*float64, error) {
	r, err := readXIDRate(ctx, url, from, to)
	if err != nil {
		err = fmt.Errorf("xid probe: %w", err)
		lg.Print(err)
		return nil, err
	}
	return &r, nil
}

func readXIDRate(ctx context.Context, url string, from, to time.Time) (float64, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	read := func(at time.Time) (xid int64, readAt time.Time, err error) {
		if !sleepUntil(ctx, at) {
			return 0, time.Time{}, fmt.Errorf("the step ended before the read at %s: %w", at.UTC().Format(time.RFC3339), context.Cause(ctx))
		}
		if err := conn.QueryRow(ctx, `select pg_snapshot_xmax(pg_current_snapshot())::text::bigint`).Scan(&xid); err != nil {
			return 0, time.Time{}, err
		}
		return xid, time.Now(), nil
	}
	x0, t0, err := read(from)
	if err != nil {
		return 0, err
	}
	x1, t1, err := read(to)
	if err != nil {
		return 0, err
	}
	if !t1.After(t0) {
		return 0, fmt.Errorf("the read at the window end (%s) is not after the read at its start (%s)", t1.UTC().Format(time.RFC3339Nano), t0.UTC().Format(time.RFC3339Nano))
	}
	return float64(x1-x0) / t1.Sub(t0).Seconds(), nil
}

// --- statistics -------------------------------------------------------------------------------

type stat struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// pct is the p-th percentile of sorted xs (the lower nearest rank).
func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(p/100*float64(len(sorted)-1))]
}

func mkStat(xs []float64) stat {
	if len(xs) == 0 {
		return stat{}
	}
	ys := append([]float64(nil), xs...)
	sort.Float64s(ys)
	return stat{N: len(ys), P50: pct(ys, 50), P90: pct(ys, 90), P99: pct(ys, 99), Max: ys[len(ys)-1]}
}

// resourcesSummary is the window's resource use. Cores are CPU-seconds per second.
type resourcesSummary struct {
	WindowS               float64            `json:"window_s"`
	CPUCoresMean          map[string]float64 `json:"cpu_cores_mean"`
	CPUCoresPeak5s        map[string]float64 `json:"cpu_cores_peak_5s"`
	RSSMiBMax             map[string]float64 `json:"rss_mib_max"`
	SysBusyFrac           float64            `json:"sys_busy_frac"`
	Goroutines            stat               `json:"goroutines"`
	PoolAcquireGoroutines stat               `json:"pool_acquire_goroutines"`
	PGConnsTotal          stat               `json:"pg_conns_total"`
	PGConnsByState        map[string]stat    `json:"pg_conns_by_state"`
	// HeapLiveMiB is the in-window heap reads' heap_live_bytes in MiB: the heap profile's in-use
	// bytes, a sampled estimate of the live heap.
	HeapLiveMiB stat `json:"heap_live_mib"`
	// MemStatsHeapInuseMiB is an older run's summary.json `heap_inuse_mib`, runtime.MemStats
	// HeapInuse, a different metric from HeapLiveMiB. A run now never writes it; its presence marks
	// an older summary, whose heap the tables and curves show under its own label.
	MemStatsHeapInuseMiB *stat `json:"heap_inuse_mib,omitempty"`
	// NATSOutMsgsPerS and NATSInMsgsPerS are NATS's messages out and in per second, from the
	// first to the last in-window /varz read that worked; null when fewer than two worked.
	// NATSSlowConsumers is the last such read's count; null when none worked.
	NATSOutMsgsPerS   *float64 `json:"nats_out_msgs_per_s"`
	NATSInMsgsPerS    *float64 `json:"nats_in_msgs_per_s"`
	NATSSlowConsumers *float64 `json:"nats_slow_consumers"`
	// JSConsumersMax is the most JetStream consumers in a sample (/jsz consumers), JSConsumersLast
	// the count at the window's last sample; JSAPIPerS the JetStream API requests per second. Null
	// when no sample read /jsz.
	JSConsumersMax  *float64 `json:"js_consumers_max"`
	JSConsumersLast *float64 `json:"js_consumers_last"`
	JSAPIPerS       *float64 `json:"js_api_per_s"`
	// PullRequestsPerS is the pull requests per second on the stream's consumers (the pull spy),
	// SessionPullRequestsPerS those on session consumers (s-<session id>). Null when the spy did
	// not run.
	PullRequestsPerS        *float64 `json:"pull_requests_per_s"`
	SessionPullRequestsPerS *float64 `json:"session_pull_requests_per_s"`
	// Trends is, per process, its CPU and RSS in the first and the last third of the window and its
	// RSS slope: what shows an upward trend over the window.
	Trends map[string]trend `json:"trends"`
	// DispatchCPUSPerOp is Dispatch's CPU-seconds per committed event or sent request; null
	// with no operations, no Dispatch pid, or when the commit rows were not read
	// (events.commits_error), which leaves the committed events unknown.
	DispatchCPUSPerOp *float64 `json:"dispatch_cpu_s_per_op"`
	// XIDsPerS is the transaction ids Postgres assigned per second from the window start to its
	// end (xidRate); null when a read failed, and then XIDReadError is why.
	XIDsPerS     *float64 `json:"xids_per_s"`
	XIDReadError string   `json:"xid_read_error,omitempty"`
	// PGReadErrors counts the in-window samples whose pg_stat_activity read failed, a failed
	// connect included; PprofReadErrors the in-window reads of Dispatch's goroutine and heap
	// profiles that failed (a heap read by when it ended, sample.HeapInWindow); NATSReadErrors the
	// in-window /varz reads that failed. A failed read enters no statistic above.
	PGReadErrors    int `json:"pg_read_errors"`
	PprofReadErrors int `json:"pprof_read_errors"`
	NATSReadErrors  int `json:"nats_read_errors"`
}

// failedReads is the sampler's failed reads in the window that feed a table, Postgres and pprof;
// no table shows the NATS reads.
func (r *resourcesSummary) failedReads() int { return r.PGReadErrors + r.PprofReadErrors }

// summarizeResources reduces the in-window samples, and the heap reads that ended in the window
// whatever sample carries them; ops is committed events plus requests sent in the window, over
// durationS.
func summarizeResources(ss []sample, ops int, durationS float64) *resourcesSummary {
	var win []sample
	for _, s := range ss {
		if s.InWindow {
			win = append(win, s)
		}
	}
	if len(win) < 2 {
		return nil
	}
	a, b := win[0], win[len(win)-1]
	dt := b.At.Sub(a.At).Seconds()
	if dt <= 0 {
		return nil
	}
	res := &resourcesSummary{WindowS: dt, CPUCoresMean: map[string]float64{}, CPUCoresPeak5s: map[string]float64{}, RSSMiBMax: map[string]float64{}, PGConnsByState: map[string]stat{}}
	for name := range b.CPUTicks {
		if _, ok := a.CPUTicks[name]; !ok {
			continue
		}
		res.CPUCoresMean[name] = (b.CPUTicks[name] - a.CPUTicks[name]) / clkTck / dt
		for i := 1; i < len(win); i++ {
			d := win[i].At.Sub(win[i-1].At).Seconds()
			if d > 0 {
				res.CPUCoresPeak5s[name] = max(res.CPUCoresPeak5s[name], (win[i].CPUTicks[name]-win[i-1].CPUTicks[name])/clkTck/d)
			}
		}
	}
	var gor, pw, pgTot, heap []float64
	var natsOK []sample
	pgState := map[string][]float64{}
	for _, s := range ss {
		if !s.HeapInWindow {
			continue
		}
		if slices.Contains(s.FailedReads, readHeap) {
			res.PprofReadErrors++
		} else if s.HeapLiveBytes > 0 {
			heap = append(heap, s.HeapLiveBytes/1048576)
		}
	}
	for _, s := range win {
		for k, v := range s.RSSKiB {
			res.RSSMiBMax[k] = max(res.RSSMiBMax[k], v/1024)
		}
		if slices.Contains(s.FailedReads, readGoroutines) {
			res.PprofReadErrors++
		} else {
			gor = append(gor, float64(s.Goroutines))
			pw = append(pw, float64(s.PoolWait))
		}
		if slices.Contains(s.FailedReads, readNATS) {
			res.NATSReadErrors++
		} else {
			natsOK = append(natsOK, s)
		}
		if slices.Contains(s.FailedReads, readPostgres) {
			res.PGReadErrors++
			continue
		}
		pgTot = append(pgTot, float64(s.PGTotal))
		for k, v := range s.PGConns {
			pgState[k] = append(pgState[k], float64(v))
		}
	}
	for k, v := range pgState {
		res.PGConnsByState[k] = mkStat(v)
	}
	res.Goroutines, res.PoolAcquireGoroutines, res.PGConnsTotal, res.HeapLiveMiB = mkStat(gor), mkStat(pw), mkStat(pgTot), mkStat(heap)
	if total := b.SysTotal - a.SysTotal; total > 0 {
		res.SysBusyFrac = (b.SysBusy - a.SysBusy) / total
	}
	if len(natsOK) > 0 {
		last := natsOK[len(natsOK)-1]
		slow := last.NATS["slow_consumers"]
		res.NATSSlowConsumers = &slow
		if d := last.At.Sub(natsOK[0].At).Seconds(); len(natsOK) >= 2 && d > 0 {
			out := (last.NATS["out_msgs"] - natsOK[0].NATS["out_msgs"]) / d
			in := (last.NATS["in_msgs"] - natsOK[0].NATS["in_msgs"]) / d
			res.NATSOutMsgsPerS, res.NATSInMsgsPerS = &out, &in
		}
	}
	rate := func(key string) *float64 {
		x, okA := a.NATS[key]
		y, okB := b.NATS[key]
		if !okA || !okB {
			return nil
		}
		v := (y - x) / dt
		return &v
	}
	res.JSAPIPerS, res.PullRequestsPerS, res.SessionPullRequestsPerS = rate("js_api_total"), rate("pull_requests"), rate("session_pull_requests")
	for _, s := range win {
		if c, ok := s.NATS["js_consumers"]; ok {
			if res.JSConsumersMax == nil || c > *res.JSConsumersMax {
				res.JSConsumersMax = &c
			}
		}
	}
	if c, ok := b.NATS["js_consumers"]; ok {
		res.JSConsumersLast = &c
	}
	res.Trends = trends(win)
	if dc, ok := res.CPUCoresMean["dispatch"]; ok && ops > 0 && durationS > 0 {
		v := dc / (float64(ops) / durationS)
		res.DispatchCPUSPerOp = &v
	}
	return res
}

// trend is one process's CPU and memory early and late in the window: its cores over the first
// and the last third of the window's samples, its mean RSS in each, and the least-squares slope of
// its RSS over the whole window.
type trend struct {
	CoresFirstThird    float64 `json:"cores_first_third"`
	CoresLastThird     float64 `json:"cores_last_third"`
	RSSMiBFirstThird   float64 `json:"rss_mib_first_third"`
	RSSMiBLastThird    float64 `json:"rss_mib_last_third"`
	RSSSlopeMiBPerMin  float64 `json:"rss_slope_mib_per_min"`
	SamplesInEachThird int     `json:"samples_in_each_third"`
}

// trends is the trend of each process every in-window sample measured; empty with fewer than six
// samples, which give no thirds of two samples each.
func trends(win []sample) map[string]trend {
	out := map[string]trend{}
	k := len(win) / 3
	if k < 2 {
		return out
	}
	first, last := win[:k], win[len(win)-k:]
	cores := func(part []sample, name string) (float64, bool) {
		a, b := part[0], part[len(part)-1]
		ta, okA := a.CPUTicks[name]
		tb, okB := b.CPUTicks[name]
		dt := b.At.Sub(a.At).Seconds()
		if !okA || !okB || dt <= 0 {
			return 0, false
		}
		return (tb - ta) / clkTck / dt, true
	}
	meanRSS := func(part []sample, name string) float64 {
		var sum float64
		for _, s := range part {
			sum += s.RSSKiB[name] / 1024
		}
		return sum / float64(len(part))
	}
	for name := range win[len(win)-1].CPUTicks {
		c1, ok1 := cores(first, name)
		c2, ok2 := cores(last, name)
		if !ok1 || !ok2 {
			continue
		}
		// The least-squares slope of RSS (MiB) over time (minutes from the first sample).
		var n, sx, sy, sxx, sxy float64
		for _, s := range win {
			if _, ok := s.RSSKiB[name]; !ok {
				continue
			}
			x, y := s.At.Sub(win[0].At).Minutes(), s.RSSKiB[name]/1024
			n, sx, sy, sxx, sxy = n+1, sx+x, sy+y, sxx+x*x, sxy+x*y
		}
		var slope float64
		if d := n*sxx - sx*sx; d > 0 {
			slope = (n*sxy - sx*sy) / d
		}
		out[name] = trend{CoresFirstThird: c1, CoresLastThird: c2, RSSMiBFirstThird: meanRSS(first, name), RSSMiBLastThird: meanRSS(last, name),
			RSSSlopeMiBPerMin: slope, SamplesInEachThird: k}
	}
	return out
}

func logSamplerStart(lg *log.Logger, ep endpoints) {
	names := make([]string, 0, len(ep.pids))
	for name, pid := range ep.pids {
		names = append(names, fmt.Sprintf("%s=%d", name, pid))
	}
	sort.Strings(names)
	if len(names) == 0 {
		lg.Printf("sampler: no stack pids (-allow-foreign): /proc CPU and RSS columns stay empty except loadgen's")
		return
	}
	lg.Printf("sampler: pids %s", strings.Join(names, " "))
}
