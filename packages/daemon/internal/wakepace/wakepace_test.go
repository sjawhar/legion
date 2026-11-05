package wakepace

import (
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	// rate is the operator's eight wakes a minute, the pace the plan's worked example uses.
	rate = 8
	// interval is what one wake costs at that rate.
	interval = time.Minute / rate
)

// origin is the one instant the tests reserve from, so that every release reads as an offset from
// it and no test needs a clock.
var origin = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// reserveAll makes n reserves, every one of them at now, and returns their releases in the order
// they were made.
func reserveAll(l *Limiter, now time.Time, n int) []time.Time {
	releases := make([]time.Time, n)
	for i := range releases {
		releases[i] = l.Reserve(now)
	}
	return releases
}

// jitterCycle draws the given parts in turn, which is how a test reads an exact schedule out of a
// jittered limiter. It is for one goroutine only.
func jitterCycle(parts ...time.Duration) func() time.Duration {
	next := 0
	return func() time.Duration {
		part := parts[next%len(parts)]
		next++
		return part
	}
}

// mostInAnyMinute is the largest number of the releases that fall within one minute of each other,
// counting both ends of the window. The releases must be sorted.
func mostInAnyMinute(releases []time.Time) int {
	most := 0
	for i, start := range releases {
		count := 0
		for _, release := range releases[i:] {
			if release.Sub(start) > time.Minute {
				break
			}
			count++
		}
		most = max(most, count)
	}
	return most
}

// The first wake a limiter is ever asked about runs at once: a daemon that has woken nobody has
// spent none of its rate.
func TestFirstReserveReturnsNow(t *testing.T) {
	if release := New(rate, 1, nil).Reserve(origin); !release.Equal(origin) {
		t.Fatalf("the first Reserve(%s) = %s, want %s", origin, release, origin)
	}
}

// A queue saturated at one instant leaves at once and then one interval at a time, and no minute
// of it holds more than rate+burst wakes.
func TestSaturatedQueueLeavesOneIntervalApart(t *testing.T) {
	releases := reserveAll(New(rate, 1, nil), origin, 40)
	for i, release := range releases {
		want := origin.Add(time.Duration(i) * interval)
		if !release.Equal(want) {
			t.Fatalf("release %d = %s, want %s", i, release, want)
		}
	}
	if most := mostInAnyMinute(releases); most > rate+1 {
		t.Errorf("a minute of the run held %d releases, want no more than %d", most, rate+1)
	}
}

// Jitter spreads the releases without loosening the pace: every gap is at least one interval, so
// the rate holds, and less than two, so a queue still drains. A jitter that varies makes gaps that
// vary, which is what keeps two daemons restarted together off the same instants.
func TestJitterKeepsEveryGapBetweenOneIntervalAndTwo(t *testing.T) {
	for name, jitter := range map[string]func() time.Duration{
		"half an interval every time": jitterCycle(interval / 2),
		"a part that varies":          jitterCycle(0, interval/4, interval/2, 3*interval/4, interval-time.Nanosecond),
	} {
		releases := reserveAll(New(rate, 1, jitter), origin, 40)
		gaps := make([]time.Duration, 0, len(releases)-1)
		for i, release := range releases[1:] {
			gap := release.Sub(releases[i])
			if gap < interval || gap > 2*interval {
				t.Fatalf("%s: the gap before release %d was %s, want between %s and %s", name, i+1, gap, interval, 2*interval)
			}
			gaps = append(gaps, gap)
		}
		allEqual := len(slices.Compact(slices.Clone(gaps))) == 1
		if varies := name == "a part that varies"; allEqual == varies {
			t.Errorf("%s: the gaps were all %s? %t, want %t", name, gaps[0], allEqual, !varies)
		}
	}
}

// However the jitter falls, no minute of the run holds more than rate+burst releases: that is the
// cap the limiter exists to keep.
func TestNoMinuteHoldsMoreThanRatePlusBurst(t *testing.T) {
	jitter := jitterCycle(0, interval/3, interval-time.Nanosecond, interval/8, 2*interval/3)
	releases := reserveAll(New(rate, 1, jitter), origin, 40)
	if most := mostInAnyMinute(releases); most > rate+1 {
		t.Errorf("a minute of the run held %d releases, want no more than %d", most, rate+1)
	}
}

// Wakes leave in the order they were reserved, so the queue a caller built is the queue that runs.
func TestReleasesComeOutInTheOrderReserved(t *testing.T) {
	jitter := jitterCycle(interval-time.Nanosecond, 0, interval/2, interval/5)
	releases := reserveAll(New(rate, 1, jitter), origin, 40)
	if !slices.IsSortedFunc(releases, time.Time.Compare) {
		t.Errorf("the releases came out %v, want them non-decreasing in the order reserved", releases)
	}
}

// A lone wake after an idle minute runs at once: the bucket refilled while nothing woke.
func TestAWakeAfterAnIdleMinuteRunsAtOnce(t *testing.T) {
	l := New(rate, 1, nil)
	releases := reserveAll(l, origin, rate)
	idle := releases[len(releases)-1].Add(time.Minute)
	if release := l.Reserve(idle); !release.Equal(idle) {
		t.Fatalf("Reserve(%s) a minute after the run = %s, want %s", idle, release, idle)
	}
}

// A rate of zero is the pacing off: every wake runs the moment its caller asks.
func TestRateZeroPacesNothing(t *testing.T) {
	for i, release := range reserveAll(New(0, 1, nil), origin, 40) {
		if !release.Equal(origin) {
			t.Fatalf("release %d with the pacing off = %s, want %s", i, release, origin)
		}
	}
}

// A burst of two lets two wakes go at once; from the third on the spacing promise holds, one
// interval between one paced wake and the next.
func TestBurstLetsItsFirstWakesGoAtOnce(t *testing.T) {
	const burst = 2
	for i, release := range reserveAll(New(rate, burst, nil), origin, 40) {
		want := origin.Add(time.Duration(max(i-burst+1, 0)) * interval)
		if !release.Equal(want) {
			t.Fatalf("release %d of a burst of %d = %s, want %s", i, burst, release, want)
		}
	}
}

// A burst below one is a burst of one, not a panic and not a pace that runs backwards: the
// configuration layer refuses such a burst by name, and the limiter still has to hold the rate if
// one reaches it.
func TestBurstBelowOneActsAsOne(t *testing.T) {
	for _, burst := range []int{0, -1} {
		for i, release := range reserveAll(New(rate, burst, nil), origin, 40) {
			want := origin.Add(time.Duration(i) * interval)
			if !release.Equal(want) {
				t.Fatalf("release %d of a burst of %d = %s, want %s, the schedule of a burst of 1", i, burst, release, want)
			}
		}
	}
}

// Several goroutines reserving at once get a schedule as orderly as one goroutine's: every wake
// its own moment, and the rate kept across all of them.
func TestConcurrentReservesShareOneSchedule(t *testing.T) {
	const goroutines, each = 10, 10
	l := New(rate, 1, nil)
	releases := make([]time.Time, goroutines*each)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				releases[g*each+i] = l.Reserve(origin)
			}
		}()
	}
	wg.Wait()

	slices.SortFunc(releases, time.Time.Compare)
	if distinct := slices.CompactFunc(slices.Clone(releases), time.Time.Equal); len(distinct) != len(releases) {
		t.Fatalf("%d reserves gave %d distinct releases, want %d", len(releases), len(distinct), len(releases))
	}
	if !releases[0].Equal(origin) {
		t.Errorf("the earliest release = %s, want %s", releases[0], origin)
	}
	for i, release := range releases[1:] {
		if gap := release.Sub(releases[i]); gap < interval {
			t.Fatalf("the gap before release %d was %s, want at least %s", i+1, gap, interval)
		}
	}
}
