package main

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// TestOpenLoopKeepsTheSchedule compares openLoop with the generators' algorithm before it: draw
// the next arrival, stop at the window end, then draw the arrival's plan from the same stream.
// The times are in the past, so openLoop never sleeps.
func TestOpenLoopKeepsTheSchedule(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	winStart, winEnd := start.Add(30*time.Second), start.Add(90*time.Second)
	const seed, stream, rate = 7, 2, 4.0
	type arrival struct {
		in    bool
		pick  float64
		issue int
	}

	var want []arrival
	r := rand.New(rand.NewPCG(seed, stream))
	next := start
	for {
		next = next.Add(time.Duration(r.ExpFloat64() / rate * float64(time.Second)))
		if !next.Before(winEnd) {
			break
		}
		want = append(want, arrival{in: !next.Before(winStart) && next.Before(winEnd), pick: r.Float64(), issue: r.IntN(1000)})
	}

	var got []arrival
	openLoop(context.Background(), seed, stream, rate, start, winStart, winEnd, func(r *rand.Rand, in bool) {
		got = append(got, arrival{in: in, pick: r.Float64(), issue: r.IntN(1000)})
	})
	if !slices.Equal(got, want) {
		t.Fatalf("openLoop drew %d arrivals, the old algorithm %d, or they differ", len(got), len(want))
	}
	in := 0
	for _, a := range got {
		if a.in {
			in++
		}
	}
	if in == 0 || in == len(got) {
		t.Fatalf("%d of %d arrivals in the window, want some in and some in the warm-up", in, len(got))
	}

	fired := false
	openLoop(context.Background(), seed, stream, 0, start, winStart, winEnd, func(*rand.Rand, bool) { fired = true })
	if fired {
		t.Fatal("a rate of 0 fired")
	}
}
