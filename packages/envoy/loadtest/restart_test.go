package main

import (
	"slices"
	"testing"
	"time"
)

func TestCPUStitchSumsEachPidUpToItsLastReading(t *testing.T) {
	var c cpuStitch
	if _, ok := c.total(); ok {
		t.Fatal("a stitch with no reading has a total")
	}
	steps := []struct {
		pid   int
		ticks float64
		want  float64
	}{
		{100, 50, 50},
		{100, 80, 80}, // the old pid's last reading before it exits
		{100, 70, 80}, // a reading below the last one keeps the last one
		{200, 5, 85},  // the new pid starts from its own zero
		{100, 90, 85}, // a pid the stitch has left is never taken up again
		{200, 12, 92},
	}
	for i, s := range steps {
		if got := c.observe(s.pid, s.ticks); got != s.want {
			t.Fatalf("step %d: observe(%d, %g) = %g, want %g", i, s.pid, s.ticks, got, s.want)
		}
	}
	if got, ok := c.total(); !ok || got != 92 {
		t.Fatalf("total %g %t, want 92", got, ok)
	}
}

func TestCPUAfterTheRestartInterpolatesBetweenSamples(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// Dispatch's stitched ticks: 100 ticks/s (1 core) before the restart at t0+10s, nothing while
	// it is down until t0+15s, 200 ticks/s after.
	ss := []sample{
		{At: t0, CPUTicks: map[string]float64{"dispatch": 0}},
		{At: t0.Add(10 * time.Second), CPUTicks: map[string]float64{"dispatch": 1000}},
		{At: t0.Add(15 * time.Second), CPUTicks: map[string]float64{"dispatch": 1000}},
		{At: t0.Add(45 * time.Second), CPUTicks: map[string]float64{"dispatch": 7000}},
		{At: t0.Add(75 * time.Second), CPUTicks: map[string]float64{"dispatch": 13000}},
	}
	total, per := cpuAfter(ss, "dispatch", t0.Add(10*time.Second))
	if total == nil || *total != 110 {
		t.Fatalf("60 s after the restart: %v CPU-s, want 110 (0 for 5 s while down, then 2 cores for 55 s)", total)
	}
	want := []float64{0, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10}
	if !slices.Equal(per, want) {
		t.Fatalf("per 5 s %v, want %v", per, want)
	}

	short, perShort := cpuAfter(ss, "dispatch", t0.Add(30*time.Second))
	if short != nil {
		t.Fatalf("a minute the samples do not cover has a total %v", *short)
	}
	if len(perShort) != 9 {
		t.Fatalf("per 5 s %v, want the 9 buckets the samples cover", perShort)
	}
	if _, perNone := cpuAfter(ss, "postgres", t0.Add(10*time.Second)); len(perNone) != 0 {
		t.Fatalf("a process with no ticks has buckets %v", perNone)
	}
}
