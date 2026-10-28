package duty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A lone loop of a fixed cadence - the webhook redelivery, suggestion-outcome, compaction,
// issue-ask repair and prune loops, each on duty.RunLeased's ticker - must serve every period its
// run spans: each pass records the period after the one the pass before it recorded.
// Here every pass takes 40% of the interval, as a sweep that pages GitHub or a compaction that
// walks every document takes a noticeable share of its own. A loop whose next tick is armed only
// once the pass returns is due one interval plus that pass's time after the tick before, so its
// due times drift across the period boundaries and, about every two and a half passes here, jump
// over a whole period that no pass serves: the period a lone process loses. A loop that ticks on a
// ticker, its due times one interval apart while each pass takes less than the interval, serves
// every one.
func TestRunLeasedServesEveryPeriodOfALoneLoopWhosePassesTakeTime(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	const name = "suggestion-outcomes"
	const interval, passTime = time.Second, 400 * time.Millisecond
	const passes = 7

	// Each pass reads the period the pass before it recorded: Exclusive writes a pass's record once
	// the pass has returned, so the record a pass reads at its start is its predecessor's.
	recorded := make(chan time.Time, passes)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLeased(ctx, leases, name, interval, TickAtStart, func(ctx context.Context) {
			var boundary time.Time
			err := database.Pool.QueryRow(ctx, `select boundary from duty_passes where lease = $1`, name).Scan(&boundary)
			if err == nil {
				recorded <- boundary
			} else if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
				t.Errorf("read the loop's last recorded period: %v", err)
			}
			time.Sleep(passTime)
		})
	}()
	defer func() { stop(); <-done }()

	var got []time.Time
	for len(got) < passes-1 {
		select {
		case boundary := <-recorded:
			got = append(got, boundary)
		case <-time.After(runLeasedDeadline):
			t.Fatalf("the loop recorded %d periods in %v with an interval of %v; want %d", len(got), runLeasedDeadline, interval, passes-1)
		}
	}
	for i := 1; i < len(got); i++ {
		if step := got[i].Sub(got[i-1]); step != interval {
			t.Errorf("pass %d served the period at %s and the pass after it the period at %s, %v later: %d period(s) between them got no pass (each pass takes %v of the %v interval)",
				i+1, got[i-1].UTC().Format("15:04:05.000"), got[i].UTC().Format("15:04:05.000"), step, int(step/interval)-1, passTime, interval)
		}
	}
}
