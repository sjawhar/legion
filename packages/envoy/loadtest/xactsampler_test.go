package main

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

// xactTimeline is one transaction across the samples, one character per 10 ms sample: '.' not
// seen, 'a' active and 'i' idle in transaction without a transaction id, 'X' active and 'I' idle
// in transaction with one.
type xactTimeline struct {
	pid     int32
	startMs int // xact_start, ms from the first sample
	ticks   string
}

// xactSamples renders timelines as samples every 10 ms from t0. A row's statement names its pid and
// sample, with the whitespace that pg_stat_activity keeps.
func xactSamples(t0 time.Time, tls []xactTimeline) []xactSample {
	n := 0
	for _, tl := range tls {
		n = max(n, len(tl.ticks))
	}
	samples := make([]xactSample, n)
	for i := range samples {
		samples[i].at = t0.Add(time.Duration(10*i) * time.Millisecond)
		for _, tl := range tls {
			if i >= len(tl.ticks) || tl.ticks[i] == '.' {
				continue
			}
			c := tl.ticks[i]
			state := "active"
			if c == 'i' || c == 'I' {
				state = "idle in transaction"
			}
			samples[i].rows = append(samples[i].rows, xactRow{pid: tl.pid, xact: t0.Add(time.Duration(tl.startMs) * time.Millisecond),
				hasXID: c == 'X' || c == 'I', state: state, query: fmt.Sprintf("update t%d\n   set x = %d", tl.pid, i)})
		}
	}
	return samples
}

func summarizeTimelines(tls []xactTimeline, committed int) xactSummary {
	tr := newXactTracker(10 * time.Millisecond)
	for _, s := range xactSamples(time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC), tls) {
		tr.add(s)
	}
	return tr.summary(0, committed)
}

func TestXactHoldsFromSamples(t *testing.T) {
	sum := summarizeTimelines([]xactTimeline{
		{1, -5, "aXIX......"},   // hold 20 + 10 = 30 ms, 35 ms old at its last sample, idle 10 ms with an xid
		{2, -1, "iiX......."},   // one sample with an xid: hold 10 ms; idle 20 ms without; 21 ms old
		{3, -50, "aaaaa....."},  // no transaction id: not counted
		{4, -100, ".XXXXXX..."}, // hold 60 ms, 160 ms old
		{5, 25, "...IIII..."},   // hold 40 ms, idle 40 ms with an xid, 35 ms old
		{6, 35, "....XI...."},   // hold 20 ms, idle 10 ms with an xid, 15 ms old
		{1, 40, "....XXXXX."},   // pid 1 again, a new transaction: hold 50 ms, 40 ms old
		{7, 55, "......XIIX"},   // still open at the last sample: no known end, not counted
	}, 12)

	if sum.Samples != 10 || sum.XIDHolders != 6 || sum.CommittedEvents != 12 || sum.IntervalMs != 10 {
		t.Fatalf("samples %d holders %d committed %d interval %g, want 10, 6, 12, 10", sum.Samples, sum.XIDHolders, sum.CommittedEvents, sum.IntervalMs)
	}
	if math.Abs(sum.SamplesPerS-100) > 1e-9 {
		t.Fatalf("samples per s %g, want 100", sum.SamplesPerS)
	}
	// Lower nearest rank over six values: p50 is the third, p99 the fifth.
	for _, c := range []struct {
		name          string
		got           stat
		p50, p99, max float64
	}{
		{"xid hold", sum.XIDHoldMs, 30, 50, 60},               // 10 20 30 40 50 60
		{"transaction", sum.XactMs, 35, 40, 160},              // 15 21 35 35 40 160
		{"idle with xid", sum.IdleInXactWithXIDMs, 0, 10, 40}, // 0 0 0 10 10 40
		{"idle without xid", sum.IdleInXactNoXIDMs, 0, 0, 20}, // 0 0 0 0 0 20
	} {
		if c.got.N != 6 || c.got.P50 != c.p50 || c.got.P99 != c.p99 || c.got.Max != c.max {
			t.Errorf("%s ms %+v, want n 6, p50 %g, p99 %g, max %g", c.name, c.got, c.p50, c.p99, c.max)
		}
	}
	want := []xidHolder{
		{60, 160, "update t4 set x = 6"},
		{50, 40, "update t1 set x = 8"},
		{40, 35, "update t5 set x = 6"},
		{30, 35, "update t1 set x = 3"},
		{20, 15, "update t6 set x = 5"},
	}
	if !slices.Equal(sum.LongestXIDHolders, want) {
		t.Fatalf("longest holders %+v, want %+v", sum.LongestXIDHolders, want)
	}
}

func TestXactHoldOfOneSampleIsOneInterval(t *testing.T) {
	sum := summarizeTimelines([]xactTimeline{{9, 0, ".X."}}, 1)
	if sum.XIDHolders != 1 || sum.XIDHoldMs.Max != 10 || sum.XactMs.Max != 10 {
		t.Fatalf("holders %d hold %+v transaction %+v, want one hold of 10 ms, 10 ms old", sum.XIDHolders, sum.XIDHoldMs, sum.XactMs)
	}
	if want := []xidHolder{{10, 10, "update t9 set x = 1"}}; !slices.Equal(sum.LongestXIDHolders, want) {
		t.Fatalf("longest holders %+v, want %+v", sum.LongestXIDHolders, want)
	}
}

func TestXactSamplerOff(t *testing.T) {
	sum := newXactTracker(0).summary(0, 40)
	if sum.IntervalMs != 0 || sum.Samples != 0 || sum.XIDHolders != 0 || sum.LongestXIDHolders == nil {
		t.Fatalf("summary %+v, want interval 0, no samples, no holders and an empty holder list", sum)
	}
}
