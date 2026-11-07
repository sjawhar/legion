package main

import (
	"testing"
	"time"
)

// recoveryFixture builds requests and writes over a step whose window starts at winStart.
type recoveryFixture struct {
	winStart time.Time
	reqs     []reqRec
	writes   []writeRec
	commits  []commitRow
	marker   int64
}

// requests adds n requests started at second at of the window, ok of them 2xx, each taking ms.
func (f *recoveryFixture) requests(at float64, n, ok int, ms float64) {
	for i := range n {
		status := 200
		if i >= ok {
			status = 429
		}
		f.reqs = append(f.reqs, reqRec{route: "r", status: status, at: f.winStart.Add(time.Duration(at * float64(time.Second))).UnixNano(),
			latency: time.Duration(ms * float64(time.Millisecond))})
	}
}

// addWrites adds n comments scheduled at second at of the window, committed of them committed.
func (f *recoveryFixture) addWrites(at float64, n, committed int) {
	for i := range n {
		f.marker++
		f.writes = append(f.writes, writeRec{Marker: f.marker, Kind: kindComment, Status: 201, InWindow: true,
			Scheduled: f.winStart.Add(time.Duration(at * float64(time.Second))).UnixNano()})
		if i < committed {
			f.commits = append(f.commits, commitRow{EventID: f.marker, Type: "comment.created", Marker: f.marker})
		}
	}
}

func TestComputeRecoveryFindsTheFirstWindowFromWhichEveryWindowRecovered(t *testing.T) {
	// 60 s before the drop, then 65 s at 1x: six whole windows and a 5 s remainder.
	s := schedule{{Factor: 1, DurationS: 30}, {Factor: 5, DurationS: 30}, {Factor: 1, DurationS: 65}}
	f := &recoveryFixture{winStart: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
	winEnd := f.winStart.Add(s.total())
	f.requests(45, 100, 0, 5000) // before the drop: not counted
	f.addWrites(45, 10, 0)
	f.requests(61, 100, 50, 40)   // window 0: half the requests refused
	f.requests(72, 100, 100, 500) // window 1: p99 500 ms, above 2 x 100
	f.requests(85, 100, 100, 50)  // window 2: 9 of 10 events committed, under 95%
	f.addWrites(85, 10, 9)
	f.requests(91, 200, 199, 60) // window 3: 99.5% 2xx, 20 of 20 committed
	f.addWrites(95, 20, 20)
	// window 4: nothing to measure
	f.requests(115, 50, 50, 80)   // window 5
	f.requests(121, 10, 0, 30000) // the 5 s remainder: not counted
	f.addWrites(122, 5, 0)

	ref := 100.0
	r := computeRecovery(s, f.winStart, winEnd, f.reqs, f.writes, f.commits, &ref)
	if r == nil {
		t.Fatal("no recovery for a schedule of three segments")
	}
	if r.DropS != 60 || r.FactorBefore != 5 || r.FactorAfter != 1 || r.Schedule != "1x:30s,5x:30s,1x:65s" {
		t.Errorf("drop %g s (%gx -> %gx), schedule %q; want 60 s (5x -> 1x)", r.DropS, r.FactorBefore, r.FactorAfter, r.Schedule)
	}
	if r.RefSource != recoveryRefFlag || r.RefP99Ms == nil || *r.RefP99Ms != 100 {
		t.Errorf("reference %v from %q, want 100 from the flag", r.RefP99Ms, r.RefSource)
	}
	if len(r.Windows) != 6 {
		t.Fatalf("%d windows, want 6 whole ones (the 5 s remainder is not counted)", len(r.Windows))
	}
	wantRecovered := []bool{false, false, false, true, true, true}
	for k, w := range r.Windows {
		if w.Recovered != wantRecovered[k] {
			t.Errorf("window %d (%+v): recovered %t, want %t", k, w, w.Recovered, wantRecovered[k])
		}
		if w.StartS != float64(10*k) {
			t.Errorf("window %d starts %g s after the drop, want %d", k, w.StartS, 10*k)
		}
	}
	if w := r.Windows[2]; w.Offered != 10 || w.Committed != 9 || w.Requests != 100 {
		t.Errorf("window 2: %+v, want 100 requests and 9 of 10 committed", w)
	}
	if w := r.Windows[5]; w.Requests != 50 || w.Offered != 0 {
		t.Errorf("window 5: %+v, want 50 requests and no event: the remainder's are not in it", w)
	}
	if r.RecoverySeconds == nil || *r.RecoverySeconds != 30 {
		t.Errorf("recovery %v s, want 30", r.RecoverySeconds)
	}

	// Without the flag the reference is the p99 of the last three windows' requests (60 and
	// 80 ms): 80. Window 1's 500 ms is above 160, window 2's 50 ms is not.
	r = computeRecovery(s, f.winStart, winEnd, f.reqs, f.writes, f.commits, nil)
	if r.RefSource != recoveryRefLast || r.RefP99Ms == nil || *r.RefP99Ms != 80 {
		t.Errorf("reference %v from %q, want 80 from the last 3 windows", r.RefP99Ms, r.RefSource)
	}
	if r.Windows[1].Recovered || r.RecoverySeconds == nil || *r.RecoverySeconds != 30 {
		t.Errorf("window 1 recovered %t, recovery %v s; want false and 30", r.Windows[1].Recovered, r.RecoverySeconds)
	}

	// A last window that is not recovered: no recovery time.
	f.requests(118, 50, 0, 40)
	r = computeRecovery(s, f.winStart, winEnd, f.reqs, f.writes, f.commits, &ref)
	if r.Windows[5].Recovered || r.RecoverySeconds != nil {
		t.Errorf("last window recovered %t, recovery %v; want false and null", r.Windows[5].Recovered, r.RecoverySeconds)
	}
}

func TestComputeRecoveryNeedsTwoSegments(t *testing.T) {
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	one := schedule{{Factor: 3, DurationS: 60}}
	if r := computeRecovery(one, start, start.Add(time.Minute), nil, nil, nil, nil); r != nil {
		t.Fatalf("a one-segment schedule has a recovery: %+v", r)
	}
	// A last segment shorter than a window: no window, so no recovery time and no reference.
	short := schedule{{Factor: 3, DurationS: 60}, {Factor: 1, DurationS: 5}}
	r := computeRecovery(short, start, start.Add(short.total()), nil, nil, nil, nil)
	if r == nil || len(r.Windows) != 0 || r.RecoverySeconds != nil || r.RefP99Ms != nil {
		t.Fatalf("a 5 s last segment: %+v, want no window, no recovery time, no reference", r)
	}
	// Windows with nothing to measure are met.
	empty := schedule{{Factor: 3, DurationS: 60}, {Factor: 1, DurationS: 30}}
	r = computeRecovery(empty, start, start.Add(empty.total()), nil, nil, nil, nil)
	if len(r.Windows) != 3 || r.RecoverySeconds == nil || *r.RecoverySeconds != 0 {
		t.Fatalf("three empty windows: %+v, want recovered from 0 s", r)
	}
}
