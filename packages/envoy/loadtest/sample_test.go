package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProcAliveSeesAZombieAndAReapedProcessAsGone(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("this test's own process reads as gone")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Until Wait reaps it, the exited child stays in /proc as a zombie.
	deadline := time.Now().Add(10 * time.Second)
	for procAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("exited, unreaped child %d still reads as alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	if procAlive(pid) {
		t.Fatalf("reaped child %d reads as alive", pid)
	}
}

func TestSamplerRecordsAStackExitOnceAndFailsTheStep(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	gone := cmd.Process.Pid
	s := newSampler(endpoints{pids: map[string]int{"dispatch": gone, "postgres": os.Getpid()}})
	first := time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)
	s.checkAlive(first)
	s.checkAlive(first.Add(5 * time.Second))
	exits := s.stackExits()
	if len(exits) != 1 || exits[0].Name != "dispatch" || exits[0].PID != gone || !exits[0].FirstSeenGone.Equal(first) {
		t.Fatalf("stack exits %+v, want dispatch once, first seen gone at %s", exits, first)
	}
	err := errStackExits(exits)
	if err == nil {
		t.Fatal("a step in which dispatch exited does not fail")
	}
	if msg := err.Error(); !strings.Contains(msg, "dispatch") || !strings.Contains(msg, "pid "+strconv.Itoa(gone)) {
		t.Fatalf("error %q does not name dispatch and pid %d", msg, gone)
	}
	if errStackExits(nil) != nil {
		t.Fatal("a step with no stack exit fails")
	}
}

func TestSamplerChecksNothingWithoutPIDs(t *testing.T) {
	s := newSampler(endpoints{pids: map[string]int{}})
	s.checkAlive(time.Now())
	if exits := s.stackExits(); len(exits) != 0 {
		t.Fatalf("stack exits %+v under -allow-foreign, want none", exits)
	}
}

// fakeSample is an in-window sample at second at, with pgTotal connections ("idle|" state),
// goroutines and heap in MiB (0: not read), and the reads named in failed marked as failed. A heap
// read, one that worked or failed, ended at the sample's time.
func fakeSample(at, pgTotal, goroutines int, heapMiB float64, failed ...string) sample {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	s := sample{
		At: t0.Add(time.Duration(at) * time.Second), InWindow: true,
		CPUTicks: map[string]float64{"dispatch": float64(at) * 50}, RSSKiB: map[string]float64{"dispatch": 1024},
		PGConns: map[string]int{"idle|": pgTotal}, PGTotal: pgTotal, Goroutines: goroutines, PoolWait: goroutines / 10,
		HeapLiveBytes: heapMiB * 1048576, FailedReads: failed,
	}
	if heapMiB > 0 || slices.Contains(failed, readHeap) {
		s.HeapAt, s.HeapInWindow = s.At, true
	}
	return s
}

func TestSummarizeResourcesLeavesFailedReadsOutOfTheStats(t *testing.T) {
	// Every Postgres and goroutine read failed, and the one heap read.
	none := summarizeResources([]sample{
		fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap),
		fakeSample(5, 0, 0, 0, readPostgres, readGoroutines),
		fakeSample(10, 0, 0, 0, readPostgres, readGoroutines),
	}, 0, 10)
	if none.PGConnsTotal.N != 0 || len(none.PGConnsByState) != 0 || none.Goroutines.N != 0 || none.PoolAcquireGoroutines.N != 0 || none.HeapLiveMiB.N != 0 {
		t.Fatalf("failed reads entered the stats: %+v", none)
	}
	if none.PGReadErrors != 3 || none.PprofReadErrors != 4 {
		t.Fatalf("pg_read_errors %d, pprof_read_errors %d; want 3 and 4", none.PGReadErrors, none.PprofReadErrors)
	}

	// Some reads failed: their zeros would pull each p50 down to 0.
	some := summarizeResources([]sample{
		fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap),
		fakeSample(5, 0, 100, 0, readPostgres),
		fakeSample(10, 10, 120, 0),
		fakeSample(15, 12, 140, 30),
	}, 0, 15)
	if some.PGConnsTotal.N != 2 || some.PGConnsTotal.P50 != 10 || some.PGConnsByState["idle|"].N != 2 {
		t.Fatalf("Postgres connections %+v by state %+v: want the 2 reads that succeeded, p50 10", some.PGConnsTotal, some.PGConnsByState)
	}
	if some.Goroutines.N != 3 || some.Goroutines.P50 != 120 || some.PoolAcquireGoroutines.N != 3 {
		t.Fatalf("goroutines %+v, pool waiters %+v: want the 3 reads that succeeded, p50 120", some.Goroutines, some.PoolAcquireGoroutines)
	}
	if some.HeapLiveMiB.N != 1 || some.HeapLiveMiB.Max != 30 {
		t.Fatalf("heap %+v, want the one read that succeeded", some.HeapLiveMiB)
	}
	if some.PGReadErrors != 2 || some.PprofReadErrors != 2 {
		t.Fatalf("pg_read_errors %d, pprof_read_errors %d; want 2 and 2", some.PGReadErrors, some.PprofReadErrors)
	}
}

// TestSummarizeResourcesTakesHeapReadsByWhenTheyEnded: a heap read enters the window by when it
// ended, not by the sample that carries it: a read that ended before the window, carried by the
// window's first sample, is left out; one that ended in the window, carried by the first sample
// past it, counts.
func TestSummarizeResourcesTakesHeapReadsByWhenTheyEnded(t *testing.T) {
	early := fakeSample(0, 1, 1, 0)
	early.HeapLiveBytes, early.HeapAt, early.HeapInWindow = 50*1048576, early.At.Add(-time.Second), false
	late := fakeSample(15, 1, 1, 0)
	late.InWindow = false
	late.HeapLiveBytes, late.HeapAt, late.HeapInWindow = 70*1048576, late.At.Add(-2*time.Second), true
	failedLate := fakeSample(20, 1, 1, 0, readHeap)
	failedLate.InWindow = false
	res := summarizeResources([]sample{early, fakeSample(5, 1, 1, 0), fakeSample(10, 1, 1, 0), late, failedLate}, 0, 10)
	if res.HeapLiveMiB.N != 1 || res.HeapLiveMiB.Max != 70 || res.PprofReadErrors != 1 {
		t.Fatalf("heap %+v, pprof_read_errors %d; want the one read that worked and ended in the window (70 MiB), and the failed one counted",
			res.HeapLiveMiB, res.PprofReadErrors)
	}
}

func TestGoroutineCountsFailsWithoutAProfile(t *testing.T) {
	const profile = "goroutine profile: total 7\n" +
		"3 @ 0x1 0x2\n#\t0x1\tgithub.com/jackc/puddle/v2.(*Pool[...]).Acquire+0x1\tpool.go:1\n\n" +
		"4 @ 0x3\n#\t0x3\truntime.gopark+0x1\tproc.go:1\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ok/"):
			fmt.Fprint(w, profile)
		case strings.HasPrefix(r.URL.Path, "/empty/"):
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	if total, pool, err := goroutineCounts(srv.URL + "/ok"); err != nil || total != 7 || pool != 3 {
		t.Fatalf("goroutineCounts = %d, %d, %v; want 7 goroutines, 3 in the pool's Acquire", total, pool, err)
	}
	for _, base := range []string{srv.URL + "/down", srv.URL + "/empty", "http://127.0.0.1:1"} {
		if _, _, err := goroutineCounts(base); err == nil {
			t.Errorf("goroutineCounts(%s) read no profile and no error", base)
		}
		if _, _, err := heapLive(context.Background(), base); err == nil {
			t.Errorf("heapLive(%s) read no profile and no error", base)
		}
	}
}

// heapProfileBody is a gzipped binary heap profile, as /debug/pprof/heap serves it, of two stacks:
// in use, 3 MiB in 5 objects and 1 MiB in 2 objects, so 4 MiB in 7 objects; allocated, far more.
func heapProfileBody(t *testing.T) []byte {
	t.Helper()
	p := cpProfile(heapTypes,
		cpStack{frames: []string{"main.main", "main.a"}, values: []int64{50, 300 * mib1, 5, 3 * mib1}},
		cpStack{frames: []string{"main.main", "main.b"}, values: []int64{20, 100 * mib1, 2, 1 * mib1}})
	var b bytes.Buffer
	if err := p.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestHeapLiveSumsTheBinaryProfilesInuse: the heap read takes /debug/pprof/heap, the binary
// profile, never its ?debug=1 text, and records the sums of its inuse_space and inuse_objects; a
// profile with no inuse_space is a failed read, not 0.
func TestHeapLiveSumsTheBinaryProfilesInuse(t *testing.T) {
	body := heapProfileBody(t)
	noInuse := cpProfile(cpuTypes, cpuStack(10, "main.main"))
	var cpu bytes.Buffer
	if err := noInuse.Write(&cpu); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/ok/debug/pprof/heap":
			w.Write(body)
		case "/cpu/debug/pprof/heap":
			w.Write(cpu.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b, o, err := heapLive(context.Background(), srv.URL+"/ok")
	if err != nil || b != 4*mib1 || o != 7 {
		t.Fatalf("heapLive = %v bytes, %v objects, %v; want the profile's 4 MiB in 7 objects in use", b, o, err)
	}
	if _, _, err := heapLive(context.Background(), srv.URL+"/cpu"); err == nil || !strings.Contains(err.Error(), "inuse_space") {
		t.Errorf("a profile with no inuse_space read as %v, want an error naming inuse_space", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if q != "" {
			t.Errorf("the heap read asked with query %q, want the binary profile, no ?debug", q)
		}
	}
}

// natsSample is fakeSample at second at with the NATS /varz counts in and out, or, when ok is
// false, with its NATS read marked as failed.
func natsSample(at int, in, out float64, ok bool) sample {
	s := fakeSample(at, 1, 1, 0)
	s.NATS = map[string]float64{}
	if !ok {
		s.FailedReads = append(s.FailedReads, readNATS)
		return s
	}
	s.NATS["in_msgs"], s.NATS["out_msgs"], s.NATS["slow_consumers"] = in, out, 2
	return s
}

// TestSummarizeResourcesTakesNATSRatesFromTheReadsThatWorked: a failed /varz read is counted and
// enters no rate: the NATS rates come from the first and last reads that worked, and are null when
// fewer than two worked.
func TestSummarizeResourcesTakesNATSRatesFromTheReadsThatWorked(t *testing.T) {
	// The first and the last reads failed: the rates are over the reads at 5 s and 15 s.
	some := summarizeResources([]sample{
		natsSample(0, 0, 0, false), natsSample(5, 1000, 3000, true), natsSample(10, 1500, 4500, true),
		natsSample(15, 2000, 6000, true), natsSample(20, 0, 0, false),
	}, 0, 20)
	if some.NATSInMsgsPerS == nil || some.NATSOutMsgsPerS == nil || *some.NATSInMsgsPerS != 100 || *some.NATSOutMsgsPerS != 300 {
		t.Fatalf("NATS in %v, out %v per s; want 100 and 300, from the reads that worked", some.NATSInMsgsPerS, some.NATSOutMsgsPerS)
	}
	if some.NATSSlowConsumers == nil || *some.NATSSlowConsumers != 2 || some.NATSReadErrors != 2 {
		t.Fatalf("NATS slow consumers %v, nats_read_errors %d; want 2 and 2", some.NATSSlowConsumers, some.NATSReadErrors)
	}
	one := summarizeResources([]sample{natsSample(0, 0, 0, false), natsSample(5, 1000, 3000, true), natsSample(10, 0, 0, false)}, 0, 10)
	if one.NATSInMsgsPerS != nil || one.NATSOutMsgsPerS != nil || one.NATSSlowConsumers == nil || one.NATSReadErrors != 2 {
		t.Fatalf("one read worked: NATS in %v, out %v, slow consumers %v, nats_read_errors %d; want no rate, the read's slow consumers, 2 errors",
			one.NATSInMsgsPerS, one.NATSOutMsgsPerS, one.NATSSlowConsumers, one.NATSReadErrors)
	}
	none := summarizeResources([]sample{natsSample(0, 0, 0, false), natsSample(5, 0, 0, false)}, 0, 5)
	if none.NATSInMsgsPerS != nil || none.NATSOutMsgsPerS != nil || none.NATSSlowConsumers != nil || none.NATSReadErrors != 2 {
		t.Fatalf("no read worked: NATS in %v, out %v, slow consumers %v, nats_read_errors %d; want none and 2 errors",
			none.NATSInMsgsPerS, none.NATSOutMsgsPerS, none.NATSSlowConsumers, none.NATSReadErrors)
	}
}

// TestSamplerMarksNoReadThatFailedOnceTheStepEnded: a read that fails while the step runs is
// marked in its sample and logged; a sample taken once ctx has ended is not kept at all, and its
// failed reads are neither counted nor logged, as poll does. The heap read runs on its own, so its
// failure lands in the first or the second sample.
func TestSamplerMarksNoReadThatFailedOnceTheStepEnded(t *testing.T) {
	down := endpoints{pprof: "http://127.0.0.1:1", natsMon: "http://127.0.0.1:1", pg: unreachablePG, pids: map[string]int{}}
	now := time.Now()

	var logged bytes.Buffer
	running := newSampler(down)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	running.loop(ctx, now, now.Add(time.Minute), log.New(&logged, "", 0))
	ss := running.all()
	if len(ss) != 2 {
		t.Fatalf("%d samples in 7 s, want 2", len(ss))
	}
	heaps := 0
	for _, s := range ss {
		others := slices.DeleteFunc(slices.Clone(s.FailedReads), func(r string) bool { return r == readHeap })
		heaps += len(s.FailedReads) - len(others)
		if !slices.Equal(others, []string{readPostgres, readGoroutines, readNATS}) {
			t.Errorf("sample at %s marks %v, want postgres, goroutines and nats", s.At, s.FailedReads)
		}
	}
	if heaps != 1 {
		t.Errorf("samples mark %v and %v, want the one heap read marked once", ss[0].FailedReads, ss[1].FailedReads)
	}
	for _, name := range []string{"sampler: postgres", "sampler: pprof", "sampler: nats"} {
		if !strings.Contains(logged.String(), name) {
			t.Errorf("run.log does not log the failed %s read:\n%s", name, logged.String())
		}
	}

	logged.Reset()
	ended := newSampler(down)
	endedCtx, end := context.WithCancel(context.Background())
	end()
	ended.loop(endedCtx, now, now.Add(time.Minute), log.New(&logged, "", 0))
	if ss := ended.all(); len(ss) != 0 || logged.Len() != 0 {
		t.Fatalf("reads that failed once ctx had ended: samples %+v, log %q; want no sample and no log line", ss, logged.String())
	}
}

// TestSamplerHeapReadDelaysNoSample: Dispatch's heap endpoint answers 7 s after it is asked, past
// the 5 s sample interval, and the goroutine reads keep their 5 s cadence meanwhile. The heap read
// lands in the first sample after it ended, with its own end time, and no second heap read starts
// before the next 15 s slot.
func TestSamplerHeapReadDelaysNoSample(t *testing.T) {
	const heapDelay = 7 * time.Second
	heapBody := heapProfileBody(t)
	var mu sync.Mutex
	var heapAsked []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/debug/pprof/goroutine":
			fmt.Fprint(w, "goroutine profile: total 7\n")
		case "/debug/pprof/heap":
			mu.Lock()
			heapAsked = append(heapAsked, time.Now())
			mu.Unlock()
			if r.URL.RawQuery != "" {
				http.Error(w, "the sampler reads the binary profile only", http.StatusBadRequest)
				return
			}
			select {
			case <-time.After(heapDelay):
				w.Write(heapBody)
			case <-r.Context().Done():
			}
		case "/varz":
			fmt.Fprint(w, `{"in_msgs": 1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	start := time.Now()
	s := newSampler(endpoints{pprof: srv.URL, natsMon: srv.URL, pg: unreachablePG, pids: map[string]int{}})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s.loop(ctx, start, start.Add(time.Minute), log.New(io.Discard, "", 0))

	ss := s.all()
	if len(ss) != 3 {
		t.Fatalf("%d samples in 12 s, want 3: one each 5 s while the heap read is in flight", len(ss))
	}
	for i, x := range ss {
		if late := x.At.Sub(start) - time.Duration(i)*5*time.Second; late < 0 || late > time.Second {
			t.Errorf("sample %d at %s past the start, want %d s: the heap read in flight delayed it", i, x.At.Sub(start), i*5)
		}
		if x.Goroutines != 7 || slices.Contains(x.FailedReads, readGoroutines) || slices.Contains(x.FailedReads, readHeap) {
			t.Errorf("sample %d: goroutines %d, failed reads %v; want 7 goroutines, no failed goroutine or heap read", i, x.Goroutines, x.FailedReads)
		}
	}
	for i, x := range ss[:2] {
		if !x.HeapAt.IsZero() || x.HeapLiveBytes != 0 {
			t.Errorf("sample %d carries a heap read (ended %s, %v bytes) before the read could end", i, x.HeapAt, x.HeapLiveBytes)
		}
	}
	got := ss[2]
	if got.HeapLiveBytes != 4*mib1 || got.HeapLiveObjects != 7 || !got.HeapInWindow {
		t.Errorf("sample 2 heap live %v bytes, %v objects, in window %v; want the profile's 4 MiB in 7 objects, in the window",
			got.HeapLiveBytes, got.HeapLiveObjects, got.HeapInWindow)
	}
	if ended := got.HeapAt.Sub(start); ended < heapDelay || got.HeapAt.After(got.At.Add(time.Second)) || !got.HeapAt.After(ss[1].At) {
		t.Errorf("sample 2 (at %s) carries a heap read that ended %s past the start, want its own end, after %s and after sample 1",
			got.At.Sub(start), ended, heapDelay)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heapAsked) != 1 {
		t.Errorf("heap asked at %v, want once: the next 15 s slot is past the 12 s run", heapAsked)
	}
}

// TestXIDRateReturnsAFailedRead: given a Postgres it cannot reach, the transaction-id probe
// returns no rate and the error, which run.log holds.
func TestXIDRateReturnsAFailedRead(t *testing.T) {
	var logged bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()
	rate, err := xidRate(ctx, unreachablePG, now, now, log.New(&logged, "", 0))
	if rate != nil || err == nil {
		t.Fatalf("xidRate on an unreachable Postgres = %v, %v; want no rate and an error", rate, err)
	}
	if !strings.Contains(logged.String(), err.Error()) {
		t.Fatalf("run.log does not hold the error %q:\n%s", err, logged.String())
	}
}
