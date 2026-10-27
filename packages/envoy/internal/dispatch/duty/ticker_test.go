package duty

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
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
// not stay in step.
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
					RunJittered(ctx, interval, func() bool { return false }, func(context.Context) { passes.record() })
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
		RunJittered(context.Background(), time.Minute, func() bool { return true }, func(context.Context) { ran = true })
		if ran {
			t.Error("an idle loop ran a pass")
		}
	})
}
