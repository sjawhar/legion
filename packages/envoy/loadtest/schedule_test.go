package main

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseScheduleReadsFactorsAndDurations(t *testing.T) {
	s, err := parseSchedule(" 1x:60s, 5x:1m,0.5x:90s ")
	if err != nil {
		t.Fatal(err)
	}
	want := schedule{{Factor: 1, DurationS: 60}, {Factor: 5, DurationS: 60}, {Factor: 0.5, DurationS: 90}}
	if !slices.Equal(s, want) {
		t.Fatalf("parsed %+v, want %+v", s, want)
	}
	if s.total() != 210*time.Second {
		t.Errorf("total %s, want 3m30s", s.total())
	}
	if got := s.meanFactor(); math.Abs(got-(60+300+45)/210.0) > 1e-12 {
		t.Errorf("mean factor %g, want the time-weighted %g", got, (60+300+45)/210.0)
	}
	if s.maxFactor() != 5 {
		t.Errorf("max factor %g, want 5", s.maxFactor())
	}
	if s.String() != "1x:60s,5x:60s,0.5x:90s" {
		t.Errorf("String() = %q", s.String())
	}
	if none, err := parseSchedule(""); err != nil || none != nil {
		t.Fatalf("an empty -schedule parsed to %+v, %v; want no schedule", none, err)
	}
	if none := schedule(nil); none.meanFactor() != 1 || none.maxFactor() != 1 {
		t.Errorf("no schedule: mean %g, max %g; want 1 and 1", none.meanFactor(), none.maxFactor())
	}
}

func TestParseScheduleRefusesAMalformedItem(t *testing.T) {
	for _, c := range []struct{ text, item string }{
		{"1x60s", "1x60s"},
		{"1:60s", "1:60s"},
		{"1x:60s,", ""},
		{"0x:60s", "0x:60s"},
		{"-2x:60s", "-2x:60s"},
		{"NaNx:60s", "NaNx:60s"},
		{"fivex:60s", "fivex:60s"},
		{"2x:sixty", "2x:sixty"},
		{"2x:0s", "2x:0s"},
		{"2x:-5s", "2x:-5s"},
	} {
		_, err := parseSchedule(c.text)
		var se *scheduleError
		if !errors.As(err, &se) {
			t.Errorf("%q: not refused as a schedule error: %v", c.text, err)
			continue
		}
		if se.Item != c.item {
			t.Errorf("%q: refusal names item %q, want %q", c.text, se.Item, c.item)
		}
	}
}

func TestScheduleWindowAndRecoveryReference(t *testing.T) {
	s := schedule{{Factor: 1, DurationS: 30}, {Factor: 4, DurationS: 30}, {Factor: 1, DurationS: 60}}
	if d, err := scheduleWindow(s, 180*time.Second, false); err != nil || d != 2*time.Minute {
		t.Errorf("no -duration: window %s, %v; want the schedule total 2m0s", d, err)
	}
	if d, err := scheduleWindow(s, 2*time.Minute, true); err != nil || d != 2*time.Minute {
		t.Errorf("-duration equal to the total: window %s, %v; want 2m0s", d, err)
	}
	if _, err := scheduleWindow(s, 180*time.Second, true); err == nil || !strings.Contains(err.Error(), "-duration") {
		t.Errorf("-duration 3m0s with a 2m0s schedule: %v; want a refusal naming -duration", err)
	}
	if d, err := scheduleWindow(nil, 90*time.Second, true); err != nil || d != 90*time.Second {
		t.Errorf("no schedule: window %s, %v; want -duration", d, err)
	}

	if ref, err := recoveryReference(nil, 0, false); err != nil || ref != nil {
		t.Errorf("no -recovery-ref-p99: %v, %v; want nil", ref, err)
	}
	if _, err := recoveryReference(nil, 250, true); err == nil || !strings.Contains(err.Error(), "-schedule") {
		t.Errorf("-recovery-ref-p99 without -schedule: %v; want a refusal naming -schedule", err)
	}
	if _, err := recoveryReference(s, 0, true); err == nil {
		t.Error("-recovery-ref-p99 0 is not refused")
	}
	if ref, err := recoveryReference(s, 250, true); err != nil || ref == nil || *ref != 250 {
		t.Errorf("-recovery-ref-p99 250: %v, %v", ref, err)
	}

	if defaultLiveSockets(60, nil) != 2 || defaultLiveSockets(0, nil) != 1 || defaultLiveSockets(60, s) != 8 {
		t.Errorf("default live sockets: %d at 60 req/s, %d at 0, %d at 60 req/s with a 4x peak; want 2, 1 and 8",
			defaultLiveSockets(60, nil), defaultLiveSockets(0, nil), defaultLiveSockets(60, s))
	}
}

// legacyOpenLoop is openLoop as it was before -schedule, at one rate: the reference that a step
// without a schedule must still draw.
func legacyOpenLoop(rngSeed, stream uint64, rate float64, start, winStart, winEnd time.Time, fire func(r *rand.Rand, at time.Time, in bool)) {
	if rate <= 0 {
		return
	}
	r := rand.New(rand.NewPCG(rngSeed, stream))
	for next := start; ; {
		next = next.Add(time.Duration(r.ExpFloat64() / rate * float64(time.Second)))
		if !next.Before(winEnd) {
			return
		}
		fire(r, next, inWindow(next, winStart, winEnd))
	}
}

type arrival struct {
	at   time.Time
	in   bool
	plan uint64
}

func TestOpenLoopWithoutAScheduleDrawsTheSameArrivals(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	winStart, winEnd := start.Add(30*time.Second), start.Add(210*time.Second)
	for _, c := range []struct {
		seed, stream uint64
		rate         float64
	}{{1, 1, 4}, {1, 2, 60}, {7, 1, 40}, {3, 2, 600}, {1, 1, 0}} {
		var want, got []arrival
		legacyOpenLoop(c.seed, c.stream, c.rate, start, winStart, winEnd, func(r *rand.Rand, at time.Time, in bool) {
			want = append(want, arrival{at, in, r.Uint64()})
		})
		openLoop(context.Background(), c.seed, c.stream, ratePlan(c.rate, start, winStart, nil), winStart, winEnd, func(r *rand.Rand, at time.Time, in bool) {
			got = append(got, arrival{at, in, r.Uint64()})
		})
		if len(got) != len(want) {
			t.Fatalf("seed %d stream %d rate %g: %d arrivals, the schedule before -schedule drew %d", c.seed, c.stream, c.rate, len(got), len(want))
		}
		for i := range want {
			if !got[i].at.Equal(want[i].at) || got[i].in != want[i].in || got[i].plan != want[i].plan {
				t.Fatalf("seed %d stream %d rate %g: arrival %d is %+v, the schedule before -schedule drew %+v", c.seed, c.stream, c.rate, i, got[i], want[i])
			}
		}
	}
}

func TestOpenLoopFollowsTheSchedule(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	winStart := start.Add(100 * time.Second)
	s := schedule{{Factor: 1, DurationS: 100}, {Factor: 5, DurationS: 100}, {Factor: 0.5, DurationS: 200}}
	winEnd := winStart.Add(s.total())
	const rate = 20.0
	counts := make([]int, 4) // warm-up, then the three segments
	bounds := []time.Time{winStart, winStart.Add(100 * time.Second), winStart.Add(200 * time.Second), winEnd}
	inCount := 0
	openLoop(context.Background(), 5, 1, ratePlan(rate, start, winStart, s), winStart, winEnd, func(_ *rand.Rand, at time.Time, in bool) {
		k := 0
		for k < len(bounds)-1 && !at.Before(bounds[k]) {
			k++
		}
		if !at.Before(bounds[len(bounds)-1]) {
			t.Fatalf("an arrival at %s, at or after the window end", at.Sub(winStart))
		}
		counts[k]++
		if in {
			inCount++
		}
	})
	wants := []float64{rate * 1 * 100, rate * 1 * 100, rate * 5 * 100, rate * 0.5 * 200}
	for k, want := range wants {
		if math.Abs(float64(counts[k])-want) > 4*math.Sqrt(want) {
			t.Errorf("segment %d (0 = warm-up at 1x): %d arrivals, want about %.0f", k, counts[k], want)
		}
	}
	if inCount != counts[1]+counts[2]+counts[3] {
		t.Errorf("%d arrivals in the window, want the %d of the three segments", inCount, counts[1]+counts[2]+counts[3])
	}
}
