package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

// cpStack is one synthetic stack, root first as a reader writes it, with its sample's values.
type cpStack struct {
	frames []string
	values []int64
}

// cpProfile builds a profile of the sample types (type, unit pairs) holding each stack as one
// sample: one function and one location per distinct frame name, locations leaf first.
func cpProfile(types [][2]string, stacks ...cpStack) *profile.Profile {
	p := &profile.Profile{DurationNanos: 600e9}
	for _, st := range types {
		p.SampleType = append(p.SampleType, &profile.ValueType{Type: st[0], Unit: st[1]})
	}
	locs := map[string]*profile.Location{}
	for _, s := range stacks {
		sample := &profile.Sample{Value: s.values}
		for i := len(s.frames) - 1; i >= 0; i-- {
			name := s.frames[i]
			loc, ok := locs[name]
			if !ok {
				fn := &profile.Function{ID: uint64(len(p.Function) + 1), Name: name, SystemName: name, Filename: "x.go"}
				p.Function = append(p.Function, fn)
				loc = &profile.Location{ID: uint64(len(p.Location) + 1), Line: []profile.Line{{Function: fn, Line: 1}}}
				p.Location = append(p.Location, loc)
				locs[name] = loc
			}
			sample.Location = append(sample.Location, loc)
		}
		p.Sample = append(p.Sample, sample)
	}
	return p
}

var cpuTypes = [][2]string{{"samples", "count"}, {"cpu", "nanoseconds"}}

// cpuStack is a CPU sample of n samples at 100 Hz.
func cpuStack(n int64, frames ...string) cpStack {
	return cpStack{frames: frames, values: []int64{n, n * 10e6}}
}

// httpRoot is the frames of a request goroutine down to the API middleware, root first.
var httpRoot = []string{
	"runtime.goexit", "net/http.(*conn).serve", "net/http.serverHandler.ServeHTTP", "net/http.(*ServeMux).ServeHTTP",
	"net/http.HandlerFunc.ServeHTTP", dispatchPkg + "accesslog.HandlerFunc.func1", apiPkg + "trackTransactions.func1",
	apiPkg + "(*server).refuseUnstorableParameters.func1",
}

func request(frames ...string) []string { return append(slices.Clone(httpRoot), frames...) }

// cpuFixture is a stack for each entry point the acceptance names, an unknown one, and one holding
// both a handler frame and a fold frame; want is each row's samples.
func cpuFixture() (*profile.Profile, map[string]int64) {
	p := cpProfile(cpuTypes,
		cpuStack(7, request(apiPkg+"(*server).getIssueReferences-fm", apiPkg+"(*server).getIssueReferences",
			apiPkg+"(*server).loadArtifactsByRefKey", "encoding/json.Unmarshal", "runtime.mallocgc")...),
		cpuStack(11, "runtime.goexit", "golang.org/x/sync/singleflight.(*Group).doCall", docsPkg+"(*Service).coldRead.func1",
			docsPkg+"(*Service).foldDocument", dispatchPkg+"pmdoc.WriteTokenJSON", "crypto/sha256.Sum256"),
		cpuStack(5, "runtime.goexit", "runtime.gcBgMarkWorker", "runtime.systemstack", "runtime.gcDrain", "runtime.scanobject"),
		cpuStack(2, "runtime.goexit", "runtime.bgsweep", "runtime.sweepone"),
		cpuStack(1, "runtime.goexit", "runtime.bgscavenge", "runtime.(*scavengerState).run"),
		cpuStack(3, "runtime.goexit", dispatchPkg+"outbox.Run", dispatchPkg+"outbox.publishWindow",
			"github.com/jackc/pgx/v5/pgconn.(*PgConn).Exec"),
		cpuStack(1, "runtime.goexit", dispatchPkg+"outbox.Run", dispatchPkg+"outbox.publishWindow",
			"github.com/nats-io/nats%2ego.(*Conn).publish"),
		cpuStack(2, "runtime.goexit", "net/http.(*conn).serve", "net/http.(*conn).readRequest"),
		cpuStack(4, request(apiPkg+"(*server).streamEvents-fm", apiPkg+"(*server).streamEvents", "net/http.(*response).Write")...),
		cpuStack(6, "runtime.mcall", "runtime.park_m", "runtime.schedule", "runtime.findRunnable", "runtime.netpoll"),
		cpuStack(1, "runtime.goexit", "example.com/mystery.run", "example.com/mystery.spin"),
		cpuStack(9, request(apiPkg+"(*server).getArtifactText", docsPkg+"(*Service).coldRead", docsPkg+"(*Service).foldDocument",
			dispatchPkg+"pmdoc.walkNode", dispatchPkg+"pmdoc.len16")...),
	)
	return p, map[string]int64{
		"HTTP getIssueReferences": 7, entryFold: 11, entryGCMark: 5, entryGCSweep: 3, entryOutbox: 4, entrySSE: 4,
		entryHTTPServer: 2, entryScheduler: 6, entryOther: 1, "HTTP getArtifactText": 9,
	}
}

func TestCodepathsAssignsEverySampleToOneEntryPoint(t *testing.T) {
	p, want := cpuFixture()
	c, err := cpuByEntry(p, new(4.0))
	if err != nil {
		t.Fatal(err)
	}
	var sum, ns int64
	var shares, cores float64
	got := map[string]int64{}
	for _, r := range c.Rows {
		if _, dup := got[r.Row]; dup {
			t.Errorf("row %q twice", r.Row)
		}
		got[r.Row] = r.Samples
		sum += r.Samples
		ns += r.CPUNs
		shares += *r.Share
		cores += *r.Cores
	}
	if c.Samples != 52 || sum != c.Samples || ns != 52*10e6 || math.Abs(c.CPUS-0.52) > 1e-12 {
		t.Errorf("rows sum to %d samples (%d ns) of %d (%v CPU s); want all 52 samples, 0.52 s", sum, ns, c.Samples, c.CPUS)
	}
	if math.Abs(shares-1) > 1e-12 || math.Abs(cores-4) > 1e-12 {
		t.Errorf("shares sum to %v, cores to %v; want 1 and the cgroup's 4", shares, cores)
	}
	for row, n := range want {
		if got[row] != n {
			t.Errorf("row %q: %d samples, want %d", row, got[row], n)
		}
	}
	for row, n := range got {
		if _, ok := want[row]; !ok && n != 0 {
			t.Errorf("row %q: %d samples, want none", row, n)
		}
	}
	// Every fixed row is in the table even with no sample, after the handlers, `other` last.
	var labels []string
	for _, r := range c.Rows {
		labels = append(labels, r.Row)
	}
	wantLabels := append([]string{"HTTP getArtifactText", "HTTP getIssueReferences"}, fixedEntries()...)
	if !slices.Equal(labels, wantLabels) {
		t.Errorf("rows %q, want %q", labels, wantLabels)
	}
	if len(c.OtherByStart) != 1 || c.OtherByStart[0].Row != "example.com/mystery.run" || c.OtherByStart[0].Samples != 1 {
		t.Errorf("other by goroutine start %+v, want example.com/mystery.run with 1 sample", c.OtherByStart)
	}
}

func TestCodepathsPrecedence(t *testing.T) {
	for _, c := range []struct {
		name  string
		stack []string // root first
		want  string
	}{
		{"a handler frame above a fold frame", request(apiPkg+"(*server).getArtifactText", docsPkg+"(*Service).coldRead", docsPkg+"(*Service).foldDocument"), "HTTP getArtifactText"},
		{"a handler loading a room", request(apiPkg+"(*server).uploadArtifact", docsPkg+"(*Service).publishLiveWrite", ygoWebsocketPkg+"(*Server).loadRoom", docsPkg+"(*renderedReplica).observe"), "HTTP uploadArtifact"},
		{"the document websocket", request(apiPkg+"(*server).refuseUnstorableActor.func1", docsPkg+"(*Service).ServeHTTP", ygoWebsocketPkg+"(*Server).ServeHTTP", docsPkg+"(*renderedReplica).observe"), "HTTP " + docWebsocketHandler},
		{"an API function outside a request", []string{"runtime.goexit", apiPkg + "(*deliveryHub).run", "encoding/json.Marshal"}, entryOther},
		{"the room observer on a persistence worker", []string{"runtime.goexit", ygoWebsocketPkg + "(*Server).startPersistenceWorker.func1", docsPkg + "(*renderedReplica).observe"}, entryObserver},
		{"the settle timer", []string{"runtime.goexit", "time.goFunc", docsPkg + "(*Service).scheduleSettleAfterLocked.func1", docsPkg + "(*Service).settleRoom", docsPkg + "(*PgVersioned).Compact"}, entrySettlement},
		{"a persistence worker compacting", []string{"runtime.goexit", ygoWebsocketPkg + "(*Server).startPersistenceWorker.func1", docsPkg + "(*servicePersistenceAdapter).Compact", docsPkg + "(*PgVersioned).Compact"}, entryCompaction},
		{"the idle sweep", []string{"runtime.goexit", ygoWebsocketPkg + "(*Server).idleSweepLoop"}, entryRoomsOther},
		{"the outbox compacting documents", []string{"runtime.goexit", dispatchPkg + "outbox.compactDaily", dispatchPkg + "outbox.compactDocuments", docsPkg + "(*Service).CompactAll"}, entryCompaction},
		{"the event stream relay", []string{"runtime.goexit", apiPkg + "(*eventStreamRelay).start.func2", apiPkg + "(*eventStreamRelay).run"}, entrySSE},
		{"the HTTP server reading a request", []string{"runtime.goexit", "net/http.(*conn).serve", "net/http.(*conn).readRequest"}, entryHTTPServer},
		{"the pprof endpoint", []string{"runtime.goexit", "net/http.(*conn).serve", "net/http.HandlerFunc.ServeHTTP", "net/http/pprof.Index", "runtime/pprof.writeHeap"}, entryProfiling},
		{"the CPU profiler", []string{"runtime.goexit", "runtime/pprof.profileWriter", "runtime/pprof.(*profileBuilder).addCPUData"}, entryProfiling},
		{"an HTTP client is not the server", []string{"runtime.goexit", "net/http.(*persistConn).readLoop"}, entryOther},
		{"a GC mark worker without a traceback", []string{"runtime._GC"}, entryGCMark},
		{"runtime frames only", []string{"runtime._System"}, entryScheduler},
	} {
		stack := slices.Clone(c.stack)
		slices.Reverse(stack)
		if got := entryPoint(stack); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCodepathsSubsystemsCountEachSampleOnceInEach(t *testing.T) {
	p, _ := cpuFixture()
	subs, err := subsystems(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]cpSubsystem{}
	for _, s := range subs {
		got[s.Row] = s
		if s.Cores != nil {
			t.Errorf("%s: cores %v without a cgroup, want null", s.Row, *s.Cores)
		}
	}
	for name, want := range map[string]int64{
		"dispatch/docs (fold, read cache, rooms)": 20, "pmdoc (document tree, render, block tokens)": 20,
		"API handlers (dispatch/api)": 20, "encoding/json (v1 and v2)": 7, "GC mark (background workers and assists)": 5,
		"crypto (sha256 of block tokens; TLS)": 11, "pgx / pgconn (database driver)": 3, "runtime.mallocgc (allocation)": 7,
		"pmdoc.walkNode": 9, "len16": 9, "refreshAnchors": 0, "NATS client": 1,
	} {
		if got[name].Samples != want {
			t.Errorf("%s: %d samples, want %d", name, got[name].Samples, want)
		}
	}
	if s := got["refreshAnchors"]; s.InProfile || s.Share == nil || *s.Share != 0 || !s.WritePath {
		t.Errorf("refreshAnchors: in profile %v, share %v, write path %v; want absent, 0, a write-path row", s.InProfile, s.Share, s.WritePath)
	}
	if s := got["len16"]; !s.InProfile || math.Abs(*s.Share-9.0/52) > 1e-12 {
		t.Errorf("len16: in profile %v, share %v; want present at 9/52", s.InProfile, *s.Share)
	}
}

var heapTypes = [][2]string{{"alloc_objects", "count"}, {"alloc_space", "bytes"}, {"inuse_objects", "count"}, {"inuse_space", "bytes"}}

func heapStack(alloc, inuse int64, frames ...string) cpStack {
	return cpStack{frames: frames, values: []int64{1, alloc, 1, inuse}}
}

const mib1 = 1 << 20

func heapFixtures() (start, end *profile.Profile) {
	handler := request(apiPkg+"(*server).getIssueReferences", "encoding/json.Unmarshal")
	fold := []string{"runtime.goexit", docsPkg + "(*Service).coldRead.func1", docsPkg + "(*Service).foldDocument", dispatchPkg + "pmdoc.(*blockPathWalker).record"}
	mystery := []string{"runtime.goexit", "example.com/mystery.run"}
	start = cpProfile(heapTypes, heapStack(100*mib1, 0, handler...), heapStack(1000*mib1, 0, fold...), heapStack(5*mib1, 0, mystery...))
	end = cpProfile(heapTypes, heapStack(400*mib1, 20*mib1, handler...), heapStack(9000*mib1, 300*mib1, fold...), heapStack(5*mib1, 80*mib1, mystery...))
	return start, end
}

// TestCodepathsMemoryRowsSumToTheWorkingSet: the rows sum to the working set with and without
// HeapReleased. With it, heap idle retained and non-Go memory leave the released pages out, so a
// large HeapReleased under a working set that covers the resident memory (Sys - HeapReleased)
// gives a non-negative non-Go row where working set - Sys would be negative. Without it, the old
// formula holds and a note says so.
func TestCodepathsMemoryRowsSumToTheWorkingSet(t *testing.T) {
	start, end := heapFixtures()
	const (
		retainedRow = "heap idle retained (HeapSys - HeapInuse - HeapReleased)"
		nonGoRow    = "non-Go memory (working set - (Sys - HeapReleased))"
		oldIdleRow  = "heap idle (HeapSys - HeapInuse)"
		oldNonGoRow = "non-Go memory (working set - Sys)"
	)
	old := &memStats{HeapInuse: 600 * mib1, HeapSys: 800 * mib1, StackSys: 16 * mib1, Sys: 1000 * mib1}
	released := &memStats{HeapInuse: 600 * mib1, HeapSys: 1400 * mib1, HeapReleased: new(700.0 * mib1), StackSys: 16 * mib1, Sys: 1600 * mib1}
	for _, tc := range []struct {
		ms                *memStats
		ws                float64
		idleRow, nonGoRow string
		idle, nonGo       float64
	}{
		{old, 1200 * mib1, oldIdleRow, oldNonGoRow, 200 * mib1, 200 * mib1},
		{old, 900 * mib1, oldIdleRow, oldNonGoRow, 200 * mib1, -100 * mib1},
		// Resident 1600 - 700 = 900 MiB: a 1,000 MiB working set leaves 100 MiB non-Go, where
		// working set - Sys would be -600 MiB.
		{released, 1000 * mib1, retainedRow, nonGoRow, 100 * mib1, 100 * mib1},
		{released, 850 * mib1, retainedRow, nonGoRow, 100 * mib1, -50 * mib1},
	} {
		m, err := memoryTable(memoryInputs{heapEnd: end, heapStart: start, ms: tc.ms, ws: new(tc.ws), wsSource: "test"})
		if err != nil {
			t.Fatal(err)
		}
		var sum, shares float64
		rows := map[string]cpMemoryRow{}
		for _, r := range m.Rows {
			if r.Bytes == nil || r.ShareOfWorkingSet == nil {
				t.Fatalf("working set %v: row %q has no bytes or share", tc.ws, r.Row)
			}
			sum += *r.Bytes
			shares += *r.ShareOfWorkingSet
			rows[r.Row] = r
		}
		if math.Abs(sum-tc.ws) > 1e-3 || math.Abs(shares-1) > 1e-12 {
			t.Errorf("working set %v: rows sum to %v bytes, shares to %v", tc.ws, sum, shares)
		}
		fold := rows["Go heap in use: "+entryFold]
		if math.Abs(*fold.Bytes-300.0/400*600*mib1) > 1e-3 || math.Abs(*fold.WindowAllocBytes-8000*mib1) > 1e-3 {
			t.Errorf("fold row: %v bytes, %v allocated; want 450 MiB and 8,000 MiB", *fold.Bytes, *fold.WindowAllocBytes)
		}
		if r := rows["Go heap in use: HTTP getIssueReferences"]; math.Abs(*r.WindowAllocBytes-300*mib1) > 1e-3 {
			t.Errorf("getIssueReferences allocated %v, want 300 MiB", *r.WindowAllocBytes)
		}
		if r, ok := rows[tc.idleRow]; !ok || *r.Bytes != tc.idle {
			t.Errorf("working set %v: %q %+v (present %v), want %v", tc.ws, tc.idleRow, r, ok, tc.idle)
		}
		r, ok := rows[tc.nonGoRow]
		if !ok {
			t.Fatalf("working set %v: no row %q in %v", tc.ws, tc.nonGoRow, slices.Collect(maps.Keys(rows)))
		}
		negativeNote := slices.ContainsFunc(m.Notes, func(n string) bool { return strings.HasPrefix(n, "Non-Go memory is negative") })
		if *r.Bytes != tc.nonGo || negativeNote != (tc.nonGo < 0) {
			t.Errorf("working set %v: non-Go %v, negative note %v; want %v", tc.ws, *r.Bytes, negativeNote, tc.nonGo)
		}
		noReleasedNote := slices.ContainsFunc(m.Notes, func(n string) bool { return strings.Contains(n, "has no HeapReleased") })
		if noReleasedNote != (tc.ms.HeapReleased == nil) {
			t.Errorf("working set %v, HeapReleased %v: note for a block without HeapReleased %v", tc.ws, tc.ms.HeapReleased, noReleasedNote)
		}
		if (m.HeapReleased == nil) != (tc.ms.HeapReleased == nil) || m.HeapReleased != nil && *m.HeapReleased != *tc.ms.HeapReleased {
			t.Errorf("working set %v: heap_released_bytes %v, want %v", tc.ws, m.HeapReleased, tc.ms.HeapReleased)
		}
	}
}

func TestCodepathsMemoryWithoutInputsShowsNoNumber(t *testing.T) {
	m, err := memoryTable(memoryInputs{heapEndWhy: "no heap-end.pprof", heapStartWhy: "no heap-start.pprof", msWhy: "no heap.txt", wsSource: "no cgroup.json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Rows {
		if r.Bytes != nil || r.ShareOfWorkingSet != nil || r.WindowAllocBytes != nil {
			t.Errorf("row %q has a number with no input: %+v", r.Row, r)
		}
	}
	if len(m.Notes) != 4 {
		t.Errorf("notes %q, want one per missing input", m.Notes)
	}
}

// outageCgroup is a dispatch role with 40 minutes, minute i at i cores and i GiB, minute 7 null.
func outageCgroup() *cgroupFile {
	r := cgroupRole{CoresMean: new(12.0), WorkingSetBytesMax: new(int64(40 << 30))}
	for i := range 40 {
		r.Minutes = append(r.Minutes, &cgroupMinute{Cores: float64(i), WorkingSetBytesMean: float64(i) * (1 << 30)})
	}
	r.Minutes[7] = nil
	return &cgroupFile{Roles: map[string]cgroupRole{"dispatch": r}}
}

func TestCodepathsOutageBuckets(t *testing.T) {
	buckets, err := parseOutageBuckets(defaultOutageBucketsName, defaultOutageBucketsText)
	if err != nil {
		t.Fatal(err)
	}
	want := []outageBucket{
		{"23:00", 5, 12.46, 15.63, 9.53, 10.59}, {"23:10", 15, 11.80, 14.37, 10.12, 11.27},
		{"23:20", 25, 13.82, 16.18, 11.55, 12.11}, {"23:30", 35, 13.79, 17.03, 13.30, 13.84},
	}
	if !slices.Equal(buckets, want) {
		t.Fatalf("committed buckets %+v, want the production profile's §5 rows %+v", buckets, want)
	}
	cf := outageCgroup()
	ws, wsSource := workingSetAtEnd(cf)
	if ws == nil || *ws != 39<<30 || !strings.Contains(wsSource, "last minute") {
		t.Fatalf("working set at the end %v (%s), want the last minute's 39 GiB", ws, wsSource)
	}
	cores, coresWhy := dispatchCores(cf)
	outage := func(windowOffsetMin int) cpOutage {
		return outageTable(cpOutage{BucketsSource: "test", WindowOffsetMin: windowOffsetMin, OutageCoresLow: 11.1, OutageCoresHigh: 13.8,
			OutageMemoryGiB: 13.8, ReplayMemorySource: wsSource}, buckets, cf, cores, coresWhy, ws)
	}
	o := outage(0)
	// 23:00 is minutes 5-14 without the null minute 7: mean (5+6+8+...+14)/9, max 14.
	b := o.Buckets[0]
	if mean := (95.0 - 7) / 9; b.ReplayMinutes != 9 || !nearPtr(b.ReplayCoresMean, mean) || !nearPtr(b.ReplayCoresMax, 14) || !nearPtr(b.ReplayWSGiBMean, mean) || !nearPtr(b.ReplayWSGiBMax, 14) {
		t.Errorf("23:00: %+v; want 9 minutes, mean %v, max 14", b, mean)
	}
	// 23:30 is minutes 35-44, of which 35-39 exist.
	b = o.Buckets[3]
	if b.ReplayMinutes != 5 || !nearPtr(b.ReplayCoresMean, 37) || !nearPtr(b.ReplayCoresMax, 39) || b.OutageCoresMean != 13.79 {
		t.Errorf("23:30: %+v; want 5 minutes, mean 37, max 39 beside 13.79", b)
	}
	if !nearPtr(o.ReplayCores, 12) || !nearPtr(o.ResidualCoresLow, -0.9) || !nearPtr(o.ResidualCoresHigh, 1.8) || !nearPtr(o.ReplayMemoryGiB, 39) || !nearPtr(o.ResidualMemoryGiB, 13.8-39) {
		t.Errorf("totals: cores %v, residual %v to %v, memory %v, residual %v", deref(o.ReplayCores), deref(o.ResidualCoresLow), deref(o.ResidualCoresHigh), deref(o.ReplayMemoryGiB), deref(o.ResidualMemoryGiB))
	}
	// A window starting at 23:00 puts the 23:00 bucket at its minutes 0-9, and 23:30 past its end.
	o = outage(5)
	if b := o.Buckets[0]; b.ReplayMinutes != 9 || !nearPtr(b.ReplayCoresMean, (45.0-7)/9) {
		t.Errorf("23:00 at window offset 5: %+v", b)
	}
	if b := o.Buckets[3]; b.ReplayMinutes != 10 || !nearPtr(b.ReplayCoresMean, 34.5) {
		t.Errorf("23:30 at window offset 5: %+v, want minutes 30-39", b)
	}
	o = outage(40)
	if b := o.Buckets[0]; b.ReplayMinutes != 0 || b.ReplayCoresMean != nil || b.ReplayMissing == "" {
		t.Errorf("a bucket before the window: %+v, want no number and why", b)
	}
	if _, err := parseOutageBuckets("x", "label,offset_min,cores_mean,cores_max,mem_gib_mean,mem_gib_max\n23:00,5,a,1,1,1\n"); err == nil {
		t.Error("a bucket with a cell that is not a number was taken")
	}
}

// cpWriteProfile writes p to path as pprof writes it (gzipped protobuf).
func cpWriteProfile(t *testing.T, path string, p *profile.Profile) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

const heapTxtFixture = `heap profile: 3: 400 [3: 9405000] @ heap/524288
1: 100 [1: 100] @ 0x1 0x2
#	0x1	main.f+0x1	x.go:1

# runtime.MemStats
# Alloc = 600000000
# TotalAlloc = 9000000000
# Sys = 1048576000
# Lookups = 0
# Mallocs = 10
# Frees = 5
# HeapAlloc = 600000000
# HeapSys = 838860800
# HeapIdle = 209715200
# HeapInuse = 629145600
# HeapReleased = 104857600
# HeapObjects = 5
# Stack = 8388608 / 16777216
# MSpan = 1 / 2
# GCSys = 3
# NumGC = 9
`

func TestCodepathsParsesMemStats(t *testing.T) {
	ms, err := parseMemStats([]byte(heapTxtFixture))
	if err != nil {
		t.Fatal(err)
	}
	if ms.HeapReleased == nil || *ms.HeapReleased != 104857600 {
		t.Errorf("HeapReleased %v, want 104857600", ms.HeapReleased)
	}
	ms.HeapReleased = nil
	if want := (memStats{HeapSys: 838860800, HeapInuse: 629145600, StackSys: 16777216, Sys: 1048576000}); ms != want {
		t.Errorf("%+v, want %+v", ms, want)
	}
	without := strings.Replace(heapTxtFixture, "# HeapReleased = 104857600\n", "", 1)
	if ms, err := parseMemStats([]byte(without)); err != nil || ms.HeapReleased != nil {
		t.Errorf("a block without HeapReleased gave %+v, %v; want no HeapReleased and no error", ms, err)
	}
	if _, err := parseMemStats([]byte("heap profile: 1: 2\n")); !errors.Is(err, errNoMemStats) {
		t.Errorf("a heap.txt with no runtime.MemStats gave %v, want errNoMemStats", err)
	}
}

// TestCodepathsCommandTakesACutHeapTxt: a heap.txt cut before its runtime.MemStats block, as a
// fetch that ran out of time leaves it, still gives CODEPATHS.md: the CPU tables, and the Go heap
// rows by entry point with heap-end's inuse_space shares, while every figure from runtime.MemStats
// shows `-` with a note naming why. A runtime.MemStats block that lacks a figure still fails.
func TestCodepathsCommandTakesACutHeapTxt(t *testing.T) {
	step, out := filepath.Join(t.TempDir(), "r16"), t.TempDir()
	if err := os.MkdirAll(step, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := cpuFixture()
	cpWriteProfile(t, filepath.Join(step, "cpu.pprof"), p)
	start, end := heapFixtures()
	cpWriteProfile(t, filepath.Join(step, "heap-start.pprof"), start)
	cpWriteProfile(t, filepath.Join(step, "heap-end.pprof"), end)
	cut, _, _ := strings.Cut(heapTxtFixture, "# runtime.MemStats")
	if err := os.WriteFile(filepath.Join(step, "heap.txt"), []byte(cut+"1: 100 [1: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cg, err := json.Marshal(outageCgroup())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(step, "cgroup.json"), cg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCodepaths([]string{"-step", step, "-out", out}); err != nil {
		t.Fatalf("a cut heap.txt failed the command: %v", err)
	}
	var rep codepathsReport
	b, err := os.ReadFile(filepath.Join(out, "codepaths.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.CPU == nil || rep.CPU.Samples != 52 || len(rep.Subsystems) == 0 {
		t.Fatalf("cpu %+v, %d subsystems; want the CPU tables", rep.CPU, len(rep.Subsystems))
	}
	if m := rep.Memory; m.HeapInuse != nil || m.HeapSys != nil || m.HeapReleased != nil || m.StackSys != nil || m.Sys != nil || m.WorkingSetBytes == nil {
		t.Errorf("memory %+v: want no runtime.MemStats figure, and the working set", m)
	}
	rows := map[string]cpMemoryRow{}
	for _, r := range rep.Memory.Rows {
		rows[r.Row] = r
		if r.Bytes != nil {
			t.Errorf("row %q has %v bytes with no runtime.MemStats, want null", r.Row, *r.Bytes)
		}
	}
	if fold, ok := rows["Go heap in use: "+entryFold]; !ok || fold.InuseShare == nil || *fold.InuseShare != 0.75 || fold.WindowAllocBytes == nil {
		t.Errorf("fold row %+v (present %v): want heap-end's inuse_space share 0.75 and the window's allocation", fold, ok)
	}
	md := readText(t, filepath.Join(out, "CODEPATHS.md"))
	for _, want := range []string{
		"| HTTP getIssueReferences | 7 | 13.46% | 0.07 | 1.62 |",
		"| Go heap in use: " + entryFold + " | - | - | - | 75.00% | 7.812 |",
		"| heap idle retained (HeapSys - HeapInuse - HeapReleased) | - | - | - | - | - |",
		"| stacks (StackSys) | - | - | - | - | - |",
		"| other runtime memory (Sys - HeapSys - StackSys) | - | - | - | - | - |",
		"| non-Go memory (working set - (Sys - HeapReleased)) | - | - | - | - | - |",
		fmt.Sprintf("heap.txt: no `# runtime.MemStats` block in its %d bytes (the block ends the profile, so a fetch cut short has none), so every row from runtime.MemStats shows `-`.", len(cut)+len("1: 100 [1: 1")),
	} {
		if !strings.Contains(md, want) {
			t.Errorf("CODEPATHS.md has no %q:\n%s", want, md)
		}
	}

	if err := os.WriteFile(filepath.Join(step, "heap.txt"), []byte(cut+"# runtime.MemStats\n# Sys = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCodepaths([]string{"-step", step, "-out", out}); err == nil {
		t.Error("a runtime.MemStats block with no HeapSys was taken")
	}
}

func TestCodepathsCommand(t *testing.T) {
	step, out := filepath.Join(t.TempDir(), "r16"), t.TempDir()
	if err := os.MkdirAll(step, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := cpuFixture()
	cpWriteProfile(t, filepath.Join(step, "cpu.pprof"), p)

	// With only the CPU profile, the cores and every memory and replay figure are `-`, never 0.
	if err := runCodepaths([]string{"-step", step, "-out", out}); err != nil {
		t.Fatal(err)
	}
	var rep codepathsReport
	b, err := os.ReadFile(filepath.Join(out, "codepaths.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.CPU == nil || rep.CPU.CgroupCoresMean != nil || rep.CPU.Samples != 52 {
		t.Fatalf("cpu %+v, want 52 samples and no cgroup cores", rep.CPU)
	}
	for _, r := range rep.CPU.Rows {
		if r.Cores != nil {
			t.Errorf("row %q: cores %v with no cgroup.json, want null", r.Row, *r.Cores)
		}
	}
	md := readText(t, filepath.Join(out, "CODEPATHS.md"))
	for _, want := range []string{
		"Cores show `-`: cgroup.json: no cgroup.json in the step directory.",
		"| HTTP getIssueReferences | 7 | 13.46% | 0.07 | - |",
		"| **total** | 52 | 100.00% | 0.52 | - |",
		"| heap idle retained (HeapSys - HeapInuse - HeapReleased) | - | - | - | - | - |",
		"| 23:00 | 5 | 12.46 / 15.63 | - / - | 9.53 / 10.59 | - / - | 0 of 10 |",
		"| replay | - | - |",
		"| **residual** (outage - replay) | - | - |",
		"heap-end: no heap-end.pprof or heap.pprof in the step directory",
		"## Not reproduced: none given (no -residual-notes)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("CODEPATHS.md has no %q:\n%s", want, md)
		}
	}

	// With every input: heap-end falls back to heap.pprof, and the notes' list lines are copied.
	start, end := heapFixtures()
	cpWriteProfile(t, filepath.Join(step, "heap-start.pprof"), start)
	cpWriteProfile(t, filepath.Join(step, "heap.pprof"), end)
	if err := os.WriteFile(filepath.Join(step, "heap.txt"), []byte(heapTxtFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cg, err := json.Marshal(outageCgroup())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(step, "cgroup.json"), cg, 0o644); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("# Differences\n\n- Reads assumed at ρ = 1.\n  continuation, not a list line\n* Embeddings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCodepaths([]string{"-step", step, "-out", out, "-residual-notes", notes}); err != nil {
		t.Fatal(err)
	}
	if b, err = os.ReadFile(filepath.Join(out, "codepaths.json")); err != nil {
		t.Fatal(err)
	}
	rep = codepathsReport{}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Inputs[2].Path != filepath.Join(step, "heap.pprof") {
		t.Errorf("heap-end input %+v, want the heap.pprof fallback", rep.Inputs[2])
	}
	var cores, ws float64
	for _, r := range rep.CPU.Rows {
		cores += *r.Cores
	}
	for _, r := range rep.Memory.Rows {
		ws += *r.Bytes
	}
	if math.Abs(cores-12) > 1e-9 || math.Abs(ws-39<<30) > 1e-3 {
		t.Errorf("cores rows sum to %v, memory rows to %v; want 12 and 39 GiB", cores, ws)
	}
	if m := rep.Memory; m.HeapReleased == nil || *m.HeapReleased != 104857600 {
		t.Errorf("heap_released_bytes %v, want heap.txt's 104857600", m.HeapReleased)
	}
	if !slices.Equal(rep.NotReproduced, []string{"- Reads assumed at ρ = 1.", "* Embeddings."}) {
		t.Errorf("not reproduced %q", rep.NotReproduced)
	}
	md = readText(t, filepath.Join(out, "CODEPATHS.md"))
	for _, want := range []string{
		"| HTTP getIssueReferences | 7 | 13.46% | 0.07 | 1.62 |",
		"| replay | 12.00 | 39.00 |",
		"| **residual** (outage - replay) | -0.90 to 1.80 | -25.20 |",
		"| heap idle retained (HeapSys - HeapInuse - HeapReleased) | 100 | 0.098 |",
		"\nHeapReleased, 100 MiB (0.098 GiB): heap pages the runtime has returned to the OS.",
		"\n## Not reproduced\n\n- Reads assumed at ρ = 1.\n* Embeddings.\n",
		"| refreshAnchors *(write path, overlapping)* | 0 | 0.00% (not in profile) | 0.00 | 0.00 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("CODEPATHS.md has no %q:\n%s", want, md)
		}
	}
	if err := runCodepaths([]string{"-step", step, "-out", out, "-cgroup", filepath.Join(step, "absent.json")}); err == nil {
		t.Error("a named input that does not exist was taken as missing")
	}
}
