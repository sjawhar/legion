package duty

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fixJitter makes every jittered wait draw what draw answers for its bound, until the test ends.
func fixJitter(t *testing.T, draw func(n time.Duration) time.Duration) {
	t.Helper()
	saved := jitterN
	jitterN = draw
	t.Cleanup(func() { jitterN = saved })
}

// passClock records the fake-clock time each pass started at.
type passClock struct {
	mu sync.Mutex
	at []time.Time
}

func (c *passClock) record() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = append(c.at, time.Now())
}

func (c *passClock) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.at...)
}

// Every wait after a pass is the interval plus up to a tenth of it again, never less: at the
// smallest draw the gap is exactly the interval, at the largest it is just under 1.1 intervals, so
// no tick lands inside a period duty_passes already records as served and two processes' ticks do
// not stay in step. Each pass is handed the moment its timer fired as its due time, the moment a
// leased pass buckets its period by (Exclusive).
func TestRunJitteredWaitsBetweenOneAndOnePointOneIntervalsAfterEveryPass(t *testing.T) {
	const interval = 5 * time.Minute
	for _, tc := range []struct {
		name string
		draw func(n time.Duration) time.Duration
		gap  time.Duration
	}{
		{name: "smallest draw", draw: func(time.Duration) time.Duration { return 0 }, gap: interval},
		{name: "largest draw", draw: func(n time.Duration) time.Duration { return n - 1 }, gap: interval + interval/10 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixJitter(t, tc.draw)
			synctest.Test(t, func(t *testing.T) {
				var passes passClock
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				booted := time.Now()
				go func() {
					defer close(done)
					RunJittered(ctx, interval, func() bool { return false }, func(_ context.Context, due time.Time) {
						if now := time.Now(); !due.Equal(now) {
							t.Errorf("a pass at %v was handed due %v, want the moment its timer fired", now.Sub(booted), due.Sub(booted))
						}
						passes.record()
					})
				}()
				time.Sleep(6 * interval)
				cancel()
				<-done

				at := passes.times()
				if len(at) < 4 {
					t.Fatalf("passes in six intervals = %d, want at least 4", len(at))
				}
				if start := at[0].Sub(booted); start < 0 || start >= interval {
					t.Errorf("first pass %v after boot, want within [0, %v)", start, interval)
				}
				for i := 1; i < len(at); i++ {
					gap := at[i].Sub(at[i-1])
					if gap < interval || gap >= interval+interval/10 {
						t.Errorf("gap before pass %d = %v, want within [%v, %v)", i+1, gap, interval, interval+interval/10)
					}
					if gap != tc.gap {
						t.Errorf("gap before pass %d = %v, want %v for this draw", i+1, gap, tc.gap)
					}
				}
			})
		})
	}
}

// A caller with nothing to do never starts the loop.
func TestRunJitteredDoesNotStartWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := false
		RunJittered(context.Background(), time.Minute, func() bool { return true }, func(context.Context, time.Time) { ran = true })
		if ran {
			t.Error("an idle loop ran a pass")
		}
	})
}

// runLeasedDeadline bounds each wait for a pass below: far longer than any tick here takes, so
// reaching it is a pass that never came, not a slow one.
const runLeasedDeadline = 30 * time.Second

// A loop that speeds up while work remains - the block-index reconciliation asks for a two-second
// step after a full batch and the full interval otherwise - must make its first pass at start and
// then every pass at the step it asked for, with the step as each tick's period. A loop that held
// its ticks to the interval, or judged a step's tick by the interval's period, would serve the
// step's ticks an interval apart: one batch per interval while thousands of blocks wait to be
// indexed. And no tick may come sooner than its wait after the pass before it ended, which is what
// keeps two of a lone loop's ticks out of one period.
func TestRunLeasedAfterEachPassTicksAtStartAndThenAtTheWaitEachPassAsksFor(t *testing.T) {
	leases := leasesOf(t, storetest.Open(t))
	const interval, step = time.Hour, 100 * time.Millisecond
	const passes = 4

	type passTimes struct{ began, ended time.Time }
	ran := make(chan passTimes)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		RunLeasedAfterEachPass(ctx, leases, "block-index", interval, func(ctx context.Context) time.Duration {
			began := time.Now()
			// A pass's own time, so a loop that ticks on a fixed cadence instead of after the
			// pass shows here as a gap shorter than the step.
			time.Sleep(step / 2)
			select {
			case ran <- passTimes{began: began, ended: time.Now()}:
			case <-ctx.Done():
			}
			return step
		})
	}()
	defer func() { stop(); <-done }()

	var got []passTimes
	for len(got) < passes {
		select {
		case p := <-ran:
			got = append(got, p)
		case <-time.After(runLeasedDeadline):
			t.Fatalf("the loop made %d passes in %v, the step %v and the interval %v; want %d: a tick was held to the interval or refused by its own previous pass",
				len(got), runLeasedDeadline, step, interval, passes)
		}
	}
	if first := got[0].began.Sub(started); first >= interval {
		t.Errorf("the first pass came %v after start; want it at start", first)
	}
	for i := 1; i < len(got); i++ {
		if gap := got[i].began.Sub(got[i-1].ended); gap < step {
			t.Errorf("pass %d began %v after pass %d ended; want at least the %v it asked for", i+1, gap, i, step)
		}
	}
}

// A loop whose first tick is one interval in - the issue-ask repair (the boot makes its own pass
// first) - must not pass at start: the issue-ask repair's boot pass would run twice on each
// deploy.
func TestRunLeasedWaitsAnIntervalBeforeTheFirstTickWhenAsked(t *testing.T) {
	leases := leasesOf(t, storetest.Open(t))
	const interval = 300 * time.Millisecond

	ran := make(chan time.Time, 1)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		RunLeased(ctx, leases, "issue-ask-repair", interval, TickAfterInterval, func(context.Context) {
			select {
			case ran <- time.Now():
			default:
			}
		})
	}()
	defer func() { stop(); <-done }()

	select {
	case began := <-ran:
		if first := began.Sub(started); first < interval {
			t.Errorf("the first pass came %v after start; want one interval, %v, in", first, interval)
		}
	case <-time.After(runLeasedDeadline):
		t.Fatalf("no pass in %v with an interval of %v", runLeasedDeadline, interval)
	}
}
