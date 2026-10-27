package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	HeapInuse  float64            `json:"heap_inuse"`
	HeapAlloc  float64            `json:"heap_alloc"`
	HeapSys    float64            `json:"heap_sys"`
	NATS       map[string]float64 `json:"nats"`
	InWindow   bool               `json:"in_window"`
}

// sampler takes a sample every 5 s: CPU and RSS of each stack process and of loadgen from /proc,
// Postgres connections, Dispatch's goroutines and pool waiters, its heap every 15 s, and NATS
// /varz.
type sampler struct {
	pids    map[string]int // by name; "postgres" is the postmaster
	pprof   string
	natsMon string
	pgURL   string
	mu      sync.Mutex
	samples []sample
}

func newSampler(ep endpoints) *sampler {
	return &sampler{pids: ep.pids, pprof: ep.pprof, natsMon: ep.natsMon, pgURL: ep.pg}
}

// procStatFields are the fields of /proc/<pid>/stat after the command name.
type procStatFields struct {
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

func (s *sampler) loop(ctx context.Context, winStart, winEnd time.Time) {
	var pg *pgx.Conn
	defer func() {
		if pg != nil {
			pg.Close(context.Background())
		}
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	self := os.Getpid()
	for n := 0; ; n++ {
		sm := sample{At: time.Now(), CPUTicks: map[string]float64{}, RSSKiB: map[string]float64{}, PGConns: map[string]int{}, NATS: map[string]float64{}}
		sm.InWindow = !sm.At.Before(winStart) && !sm.At.After(winEnd.Add(5*time.Second))
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
		if pg == nil {
			if c, err := pgx.Connect(ctx, s.pgURL); err == nil {
				pg = c
			}
		}
		if pg != nil {
			rows, err := pg.Query(ctx, `select coalesce(state,'') || '|' || coalesce(wait_event_type,''), count(*)::int from pg_stat_activity where datname='dispatch' group by 1`)
			if err == nil {
				for rows.Next() {
					var k string
					var v int
					if rows.Scan(&k, &v) == nil {
						sm.PGConns[k] = v
						sm.PGTotal += v
					}
				}
				rows.Close()
			} else {
				pg.Close(context.Background())
				pg = nil
			}
		}
		sm.Goroutines, sm.PoolWait = goroutineCounts(s.pprof)
		if n%3 == 0 {
			sm.HeapInuse, sm.HeapAlloc, sm.HeapSys = heapStats(s.pprof)
		}
		if v := natsVarz(s.natsMon); v != nil {
			sm.NATS = v
		}
		s.mu.Lock()
		s.samples = append(s.samples, sm)
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *sampler) all() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sample(nil), s.samples...)
}

var shortClient = &http.Client{Timeout: 10 * time.Second}

// goroutineCounts reads Dispatch's goroutine total and how many wait in pgx's pool Acquire
// (puddle frames) from /debug/pprof/goroutine?debug=1.
func goroutineCounts(pprof string) (total, poolWait int) {
	resp, err := shortClient.Get(pprof + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	block := 0
	inPool := false
	flush := func() {
		if inPool {
			poolWait += block
		}
		block, inPool = 0, false
	}
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "goroutine profile: total "); ok {
			total, _ = strconv.Atoi(v)
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
	return total, poolWait
}

func heapStats(pprof string) (inuse, alloc, sys float64) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(pprof + "/debug/pprof/heap?debug=1")
	if err != nil {
		return 0, 0, 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "# HeapInuse = "); ok {
			inuse, _ = strconv.ParseFloat(v, 64)
		} else if v, ok := strings.CutPrefix(line, "# HeapAlloc = "); ok {
			alloc, _ = strconv.ParseFloat(v, 64)
		} else if v, ok := strings.CutPrefix(line, "# HeapSys = "); ok {
			sys, _ = strconv.ParseFloat(v, 64)
		}
	}
	return inuse, alloc, sys
}

func natsVarz(mon string) map[string]float64 {
	resp, err := shortClient.Get(mon + "/varz")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var v struct {
		CPU         float64 `json:"cpu"`
		Mem         float64 `json:"mem"`
		Connections float64 `json:"connections"`
		InMsgs      float64 `json:"in_msgs"`
		OutMsgs     float64 `json:"out_msgs"`
		SlowCons    float64 `json:"slow_consumers"`
		Subs        float64 `json:"subscriptions"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return map[string]float64{"cpu_pct": v.CPU, "mem": v.Mem, "connections": v.Connections, "in_msgs": v.InMsgs, "out_msgs": v.OutMsgs, "slow_consumers": v.SlowCons, "subscriptions": v.Subs}
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
	HeapInuseMiB          stat               `json:"heap_inuse_mib"`
	NATSOutMsgsPerS       float64            `json:"nats_out_msgs_per_s"`
	NATSInMsgsPerS        float64            `json:"nats_in_msgs_per_s"`
	NATSSlowConsumers     float64            `json:"nats_slow_consumers"`
	// DispatchCPUSPerOp is Dispatch's CPU-seconds per committed event or sent request; null
	// with no operations or no Dispatch pid.
	DispatchCPUSPerOp *float64 `json:"dispatch_cpu_s_per_op"`
}

// summarizeResources reduces the in-window samples; ops is committed events plus requests sent
// in the window, over durationS.
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
	pgState := map[string][]float64{}
	for _, s := range win {
		for k, v := range s.RSSKiB {
			res.RSSMiBMax[k] = max(res.RSSMiBMax[k], v/1024)
		}
		gor = append(gor, float64(s.Goroutines))
		pw = append(pw, float64(s.PoolWait))
		pgTot = append(pgTot, float64(s.PGTotal))
		if s.HeapInuse > 0 {
			heap = append(heap, s.HeapInuse/1048576)
		}
		for k, v := range s.PGConns {
			pgState[k] = append(pgState[k], float64(v))
		}
	}
	for k, v := range pgState {
		res.PGConnsByState[k] = mkStat(v)
	}
	res.Goroutines, res.PoolAcquireGoroutines, res.PGConnsTotal, res.HeapInuseMiB = mkStat(gor), mkStat(pw), mkStat(pgTot), mkStat(heap)
	if total := b.SysTotal - a.SysTotal; total > 0 {
		res.SysBusyFrac = (b.SysBusy - a.SysBusy) / total
	}
	res.NATSOutMsgsPerS = (b.NATS["out_msgs"] - a.NATS["out_msgs"]) / dt
	res.NATSInMsgsPerS = (b.NATS["in_msgs"] - a.NATS["in_msgs"]) / dt
	res.NATSSlowConsumers = b.NATS["slow_consumers"]
	if dc, ok := res.CPUCoresMean["dispatch"]; ok && ops > 0 && durationS > 0 {
		v := dc / (float64(ops) / durationS)
		res.DispatchCPUSPerOp = &v
	}
	return res
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
