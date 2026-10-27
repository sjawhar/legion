package main

import (
	"slices"
	"testing"
	"time"
)

func TestHoldSegmentation(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	xact := func(s int) time.Time { return t0.Add(-time.Duration(s) * time.Second) }
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	samples := []lockSample{
		{at: at(0)},
		{at: at(20), pid: 5, xact: xact(1)},
		{at: at(40), pid: 5, xact: xact(1)},
		{at: at(60), pid: 6, xact: xact(2)}, // a new holder ends pid 5's hold: 40 ms
		{at: at(80), pid: 6, xact: xact(2)},
		{at: at(100)},                        // nobody holds it: pid 6's hold ends, 40 ms
		{at: at(120), pid: 7, xact: xact(3)}, // one backend,
		{at: at(140), pid: 7, xact: xact(4)}, // a new transaction: 20 ms
		{at: at(160)},                        // 20 ms
		{at: at(180), pid: 8, xact: xact(5)}, // still open at the end: not counted
	}

	if got, want := holdDurations(samples), []float64{40, 40, 20, 20}; !slices.Equal(got, want) {
		t.Fatalf("holds %v, want %v", got, want)
	}
	sum := summarizeLock(samples)
	if sum.HeldFraction != 0.7 || sum.HoldsObserved != 4 {
		t.Fatalf("held fraction %g holds %d, want 0.7 and 4", sum.HeldFraction, sum.HoldsObserved)
	}
}
