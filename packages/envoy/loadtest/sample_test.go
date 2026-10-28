package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
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
// goroutines and heap in MiB (0: not read), and the reads named in failed marked as failed.
func fakeSample(at, pgTotal, goroutines int, heapMiB float64, failed ...string) sample {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	return sample{
		At: t0.Add(time.Duration(at) * time.Second), InWindow: true,
		CPUTicks: map[string]float64{"dispatch": float64(at) * 50}, RSSKiB: map[string]float64{"dispatch": 1024},
		PGConns: map[string]int{"idle|": pgTotal}, PGTotal: pgTotal, Goroutines: goroutines, PoolWait: goroutines / 10,
		HeapInuse: heapMiB * 1048576, FailedReads: failed,
	}
}

func TestSummarizeResourcesLeavesFailedReadsOutOfTheStats(t *testing.T) {
	// Every Postgres and goroutine read failed, and the one heap read.
	none := summarizeResources([]sample{
		fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap),
		fakeSample(5, 0, 0, 0, readPostgres, readGoroutines),
		fakeSample(10, 0, 0, 0, readPostgres, readGoroutines),
	}, 0, 10)
	if none.PGConnsTotal.N != 0 || len(none.PGConnsByState) != 0 || none.Goroutines.N != 0 || none.PoolAcquireGoroutines.N != 0 || none.HeapInuseMiB.N != 0 {
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
	if some.HeapInuseMiB.N != 1 || some.HeapInuseMiB.Max != 30 {
		t.Fatalf("heap %+v, want the one read that succeeded", some.HeapInuseMiB)
	}
	if some.PGReadErrors != 2 || some.PprofReadErrors != 2 {
		t.Fatalf("pg_read_errors %d, pprof_read_errors %d; want 2 and 2", some.PGReadErrors, some.PprofReadErrors)
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
		if _, _, _, err := heapStats(base); err == nil {
			t.Errorf("heapStats(%s) read no profile and no error", base)
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
// failed reads are neither counted nor logged, as poll does.
func TestSamplerMarksNoReadThatFailedOnceTheStepEnded(t *testing.T) {
	down := endpoints{pprof: "http://127.0.0.1:1", natsMon: "http://127.0.0.1:1", pg: unreachablePG, pids: map[string]int{}}
	now := time.Now()

	var logged bytes.Buffer
	running := newSampler(down)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	running.loop(ctx, now, now.Add(time.Minute), log.New(&logged, "", 0))
	ss := running.all()
	if want := []string{readPostgres, readGoroutines, readHeap, readNATS}; len(ss) == 0 || !slices.Equal(ss[0].FailedReads, want) {
		t.Fatalf("samples %+v: want the first to mark %v", ss, want)
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
