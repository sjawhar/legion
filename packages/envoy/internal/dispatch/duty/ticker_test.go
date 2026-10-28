package duty

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// runLeasedDeadline bounds each wait for a pass below: far longer than any tick here takes, so
// reaching it is a pass that never came, not a slow one.
const runLeasedDeadline = 30 * time.Second

// A loop that speeds up while work remains - the reference-excerpt and block-index
// reconciliations ask for a two-second step after a full batch and the full interval otherwise -
// must make its first pass at start and then every pass at the step it asked for, with the step as
// each tick's period. A loop that held its ticks to the interval, or judged a step's tick by the
// interval's period, would serve the step's ticks five minutes apart: one batch per interval while
// thousands of mentions wait for an excerpt. And no tick may come sooner than its wait after the
// pass before it ended, which is what keeps two of a lone loop's ticks out of one period.
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
		RunLeasedAfterEachPass(ctx, leases, "reference-excerpts", interval, func(ctx context.Context) time.Duration {
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

// A loop whose first tick is one interval in - the suggestion-outcome sweep, the issue-ask repair
// (the boot makes its own pass first) and the daily compaction - must not pass at start: a
// compaction that ran at every boot would walk every document on each deploy.
func TestRunLeasedWaitsAnIntervalBeforeTheFirstTickWhenAsked(t *testing.T) {
	leases := leasesOf(t, storetest.Open(t))
	const interval = 300 * time.Millisecond

	ran := make(chan time.Time, 1)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		RunLeased(ctx, leases, "document-compaction", interval, TickAfterInterval, func(context.Context) time.Duration {
			select {
			case ran <- time.Now():
			default:
			}
			return interval
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

// A pass of a fixed-cadence loop that overruns its wait - a compaction slowed by a large database,
// a redelivery sweep paging a slow GitHub - must be followed at once by the tick a ticker would
// deliver: due at the latest scheduled moment not after now, the moments it overran dropped. A
// loop that kept due plus one wait would run each overrun moment late, one after another, each due
// long before it runs; one that took now as the due time, or waited for the scheduled moment after
// now, would put its due times off the whole waits apart that keep a lone loop's ticks in
// consecutive periods, and the lag would build up.
func TestRunLeasedTicksAtOnceAtTheLatestMomentDueWhenAPassOverruns(t *testing.T) {
	due := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	const wait = time.Second
	for _, c := range []struct {
		name      string
		now, want time.Duration
	}{
		{"a pass shorter than its wait", 400 * time.Millisecond, wait},
		{"a pass that ends at the next moment", wait, wait},
		{"a pass that overruns into the next wait", 1500 * time.Millisecond, wait},
		{"a pass that overruns several waits", 3500 * time.Millisecond, 3 * wait},
		{"a pass that ends at a later moment", 3 * wait, 3 * wait},
	} {
		if next := nextDue(due, wait, due.Add(c.now)); next.Sub(due) != c.want {
			t.Errorf("%s: a tick due at %s whose wait is %v, at %v in, was followed by one due %v in; want %v",
				c.name, due.Format("15:04:05.000"), wait, c.now, next.Sub(due), c.want)
		}
	}
}
