package redeliver_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// webhookRedeliveryLease is the name the running sweep takes its lease under, written here as the
// string rather than read from the package: the name is the contract between two envoy-dispatch
// processes, and a test that held a different one would prove nothing.
const webhookRedeliveryLease = "webhook-redelivery"

// redeliverTestLeases opens the lease pool a running sweep takes webhook-redelivery on, from the
// Postgres DISPATCH_TEST_DATABASE_URL names; a test that needs it skips (not fails) when the
// variable is unset, matching every other Postgres-backed test in this module. A lease is an
// advisory lock and reads no table, but the period a sweep is due by is a row of duty_passes
// (duty.Exclusive, migration 0115), which a pass reads and writes under its lease, so this pool's
// database carries the migrations.
//
// That row is cleared first: this database is the server's own and outlives the run, where
// storetest's clones do not, so a sweep recorded by an earlier run inside the interval a test
// below ticks on would make that test's own sweep skip.
func redeliverTestLeases(t *testing.T) duty.Leases {
	t.Helper()
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the redelivery sweep's lease tests")
	}
	ctx := context.Background()
	connections, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	t.Cleanup(connections.Close)
	pool := store.NewPool(connections)
	if err := (&store.Store{Pool: pool}).Migrate(ctx); err != nil {
		t.Fatalf("migrate the lease pool's database: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from duty_passes where lease = $1`, webhookRedeliveryLease); err != nil {
		t.Fatalf("clear an earlier run's recorded sweep: %v", err)
	}
	leases, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// captureDefaultLogs collects what the lease itself logs, which goes to the default logger rather
// than to the sweeper's own.
func captureDefaultLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(buffer, nil))
	return buffer
}

// countingGitHub is a GitHub the sweep must not reach: a call here is a listing the lease was
// supposed to keep this process from spending on the installation's rate limit.
type countingGitHub struct{ calls atomic.Int64 }

func (g *countingGitHub) Deliveries(context.Context, time.Time, func() error) ([]githubapp.Delivery, error) {
	g.calls.Add(1)
	return nil, nil
}

func (g *countingGitHub) Redeliver(context.Context, int64) error {
	g.calls.Add(1)
	return nil
}

// The running sweep lists nothing while another caller - the other envoy-dispatch process's own
// sweep - holds the webhook-redelivery lease. The lease is not what makes a redelivery safe (the
// KV compare-and-set on each delivery's record is); it is what keeps N processes from spending N
// times the listing calls on one installation's rate limit for one result.
func TestARunningSweepWhoseLeaseIsHeldElsewhereListsNothing(t *testing.T) {
	leases := redeliverTestLeases(t)
	logs := captureDefaultLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 0: this caller stands for the other process's sweep while it is inside it, which is all
	// this row is about, and records no sweep of its own -- so the running sweep below skips for
	// the lease and for nothing else. It holds the lease on a context of its own, not the test's:
	// the test cancels that one to stop the running sweep, and a pass whose context is cancelled
	// inside it is one shutdown cut short, which reports false whether or not it held the lease
	// (duty.Exclusive). The other process this caller stands for is not shutting down.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(context.Background(), leases, webhookRedeliveryLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the webhook-redelivery lease")
	}

	github := &countingGitHub{}
	// State is the zero KV: a sweep that ran would reach into it for its cursor and panic,
	// which fails this test louder still than the count below.
	sweeper := &redeliver.Sweeper{GitHub: github, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.Run(ctx, time.Hour, leases)
	}()
	for deadline := time.Now().Add(20 * time.Second); logs.count("dispatch duty: leased pass skipped", "lease="+webhookRedeliveryLease) == 0; {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the running sweep never reported skipping the %s lease:\n%s", webhookRedeliveryLease, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the sweep was free to list")
	}
	if calls := github.calls.Load(); calls != 0 {
		t.Fatalf("the sweep made %d GitHub calls under a lease held elsewhere; want none", calls)
	}
}

// timedGitHub is a GitHub with no failed delivery that records when each listing began.
type timedGitHub struct {
	mu       sync.Mutex
	listings []time.Time
}

func (g *timedGitHub) Deliveries(context.Context, time.Time, func() error) ([]githubapp.Delivery, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listings = append(g.listings, time.Now())
	return nil, nil
}

func (g *timedGitHub) Redeliver(context.Context, int64) error { return nil }

func (g *timedGitHub) times() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.listings)
}

// sweepEnds records when each sweep's summary line, the last thing a sweep does, was written.
type sweepEnds struct {
	mu   sync.Mutex
	ends []time.Time
}

func (e *sweepEnds) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`msg="webhook redelivery sweep"`)) {
		e.mu.Lock()
		e.ends = append(e.ends, time.Now())
		e.mu.Unlock()
	}
	return len(p), nil
}

func (e *sweepEnds) times() []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.ends)
}

// errNoStateInABubble is what the sweeper's Open answers inside the synctest bubble below, so the
// sweeps there reach neither NATS nor GitHub. A bubble's clock advances only while every goroutine
// in it is durably blocked, which a goroutine waiting on a socket never is, so a round trip out of
// the bubble would hold that clock for as long as the wire took - and nats.go waits for its reply
// on a channel it makes on the caller's goroutine, inside the bubble, which the connection's
// response subscription then answers on from outside it: not a panic a test could read, but the
// runtime's fatal "send on synctest channel from outside bubble" (nats.go createNewRequestAndSend
// and respHandler, runtime/chan.go chansend). What a sweep does is sweep_test.go's; that the pass
// the loop runs under the lease is the sweep is the Postgres row at the end of this file.
var errNoStateInABubble = errors.New("the schedule's test opens no state")

// sweepTime stands for a real sweep's own time inside the bubble - the listing, the records it
// writes, the lease's own statements around it - so a loop that ticked on a fixed cadence rather
// than waiting after each sweep would show it missing from the gap.
const sweepTime = 10 * time.Second

// sweepLease is a lease of this test's own in duty.Exclusive's shape (duty.ExclusiveFunc): it
// records the fake clock when each sweep took the lease and when it gave it back, with the name,
// due time and period the loop handed it, and runs the sweep in between.
type sweepLease struct {
	mu      sync.Mutex
	took    []time.Time
	gave    []time.Time
	names   []string
	dues    []time.Time
	periods []time.Duration
}

func (l *sweepLease) exclusive(ctx context.Context, _ duty.Leases, name string, due time.Time, period time.Duration, pass func(context.Context)) bool {
	l.mu.Lock()
	l.took = append(l.took, time.Now())
	l.names = append(l.names, name)
	l.dues = append(l.dues, due)
	l.periods = append(l.periods, period)
	l.mu.Unlock()
	pass(ctx)
	time.Sleep(sweepTime)
	l.mu.Lock()
	l.gave = append(l.gave, time.Now())
	l.mu.Unlock()
	return true
}

// times answers when each sweep took the lease and when it gave it back.
func (l *sweepLease) times() (took, gave []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.took), slices.Clone(l.gave)
}

// The running sweep sweeps at start and then waits the interval plus up to a tenth of it again
// after every sweep (duty.Jittered), drawn afresh each period, so two processes started together
// drift apart rather than reaching the lease in step for the life of both. With the draw fixed at
// its smallest and at its largest, the first sweep is due the moment the loop starts, before any
// wait, and each later one is due, measured from the end of the sweep before it, after exactly the
// interval plus the draw: at least one interval, a plain ticker's being the interval less the
// sweep's own time, and under 1.1 of one. Each sweep is handed the moment its timer fired as its
// due time, the moment duty.Exclusive buckets its period by.
//
// It runs on the fake clock of a synctest bubble, where those gaps are exact: measured against the
// wall clock they carried whatever the lease's own statements and a loaded runner's scheduler took
// between one sweep and the next, which no allowance bounds (run 41295911468 read 1.245970106s for
// a wait of 1.099999999s). The bubble's lease is this test's own; that the wait of at least one
// interval always reaches the next period, which is what keeps a sweep from being skipped as
// served, is the row below against the real one.
func TestTheRunningSweepSweepsAtStartThenWaitsBetweenOneAndOnePointOneIntervals(t *testing.T) {
	const (
		interval = time.Minute
		sweeps   = 4
	)
	for _, tc := range []struct {
		name string
		draw func(n time.Duration) time.Duration
		wait time.Duration
	}{
		{name: "smallest draw", draw: func(time.Duration) time.Duration { return 0 }, wait: interval},
		{name: "largest draw", draw: func(n time.Duration) time.Duration { return n - 1 }, wait: interval + interval/10 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redeliver.FixJitter(t, tc.draw)
			synctest.Test(t, func(t *testing.T) {
				logs := &lockedBuffer{}
				lease := &sweepLease{}
				sweeper := &redeliver.Sweeper{
					Open:   func() (bus.KeyValue, error) { return bus.KeyValue{}, errNoStateInABubble },
					Logger: slog.New(slog.NewTextHandler(logs, nil)),
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				started := time.Now()
				go func() {
					defer close(done)
					sweeper.RunUnderLease(ctx, interval, lease.exclusive)
				}()
				// One sweep's time plus its wait is the loop's whole cycle, so this ends after
				// the fourth sweep and before the fifth is due.
				time.Sleep(sweeps*(sweepTime+tc.wait) - sweepTime)
				cancel()
				<-done

				took, gave := lease.times()
				if len(took) < sweeps || len(gave) < sweeps {
					t.Fatalf("%d sweeps took the lease and %d gave it back in the time %d cycles of the loop take, want %d of each", len(took), len(gave), sweeps, sweeps)
				}
				if first := took[0].Sub(started); first != 0 {
					t.Errorf("the first sweep was due %v after the loop started, want at start, before any wait", first)
				}
				for index := 1; index < sweeps; index++ {
					wait := took[index].Sub(gave[index-1])
					if wait < interval || wait >= interval+interval/10 {
						t.Errorf("sweep %d was due %v after sweep %d ended, want within [%v, %v)", index+1, wait, index, interval, interval+interval/10)
					}
					if wait != tc.wait {
						t.Errorf("sweep %d was due %v after sweep %d ended, want %v for this draw", index+1, wait, index, tc.wait)
					}
				}
				if len(took) != sweeps {
					t.Errorf("%d sweeps in the time %d cycles of the loop take, want %d: the loop left less between two of them than it drew", len(took), sweeps, sweeps)
				}
				if attempted := logs.count("webhook redelivery sweep failed"); attempted != len(gave) {
					t.Errorf("%d of the %d sweeps that ended ran a sweep, want all of them: the pass the loop runs under the lease is the sweep itself\n%s", attempted, len(gave), logs)
				}
				for index := range took {
					if lease.names[index] != webhookRedeliveryLease || lease.periods[index] != interval {
						t.Errorf("sweep %d took lease %q for a period of %v, want %s for %v", index+1, lease.names[index], lease.periods[index], webhookRedeliveryLease, interval)
					}
					if !lease.dues[index].Equal(took[index]) {
						t.Errorf("sweep %d was handed a due time of %v, want %v, the moment its timer fired", index+1, lease.dues[index].Sub(started), took[index].Sub(started))
					}
				}
			})
		})
	}
}

// shortestWaits fixes the loop's draw at its smallest - a wait of exactly one interval, the
// shortest it ever leaves and so the tightest case for the next period's boundary - and records
// the moment each wait was asked for, which is the moment the lease of the sweep before it was
// given back.
type shortestWaits struct {
	mu sync.Mutex
	at []time.Time
}

func (w *shortestWaits) draw(time.Duration) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.at = append(w.at, time.Now())
	return 0
}

func (w *shortestWaits) times() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.at)
}

// No sweep of a lone process is refused as a period another pass has served. A sweep is skipped
// when duty_passes already records a pass in the period its due time falls in, and the loop's own
// waits must never land one there: each wait is at least the interval, which is the period the
// sweep hands duty.Exclusive, so two sweeps are always in different periods however long a sweep
// or its lease took. This row runs the real sweep under the real lease - what the fake clock above
// models - and counts what came of each: every sweep listed, none was skipped, and the first
// listed before the loop had asked for any wait at all.
func TestNoSweepOfALoneProcessIsSkippedAsAPeriodAlreadyServed(t *testing.T) {
	const (
		interval = 100 * time.Millisecond
		sweeps   = 4
		// Far longer than four sweeps of this interval take on a database that answers, so it
		// bounds this row's liveness and measures nothing: a run that reaches it prints the
		// lease's own log, which names what was not coming.
		deadline = 30 * time.Second
	)
	leases := redeliverTestLeases(t)
	logs := captureDefaultLogs(t)
	waits := &shortestWaits{}
	redeliver.FixJitter(t, waits.draw)
	h := newHarness(t)
	github := &timedGitHub{}
	ends := &sweepEnds{}
	// The sweep's own lines go to both: ends reads the summary that closes each sweep, and
	// swept holds every line, so a sweep that failed is counted below rather than waited past.
	swept := &lockedBuffer{}
	sweeper := &redeliver.Sweeper{
		GitHub: github,
		Open:   func() (bus.KeyValue, error) { return h.state, nil },
		Logger: slog.New(slog.NewTextHandler(io.MultiWriter(ends, swept), nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.Run(ctx, interval, leases)
	}()
	for until := time.Now().Add(deadline); len(ends.times()) < sweeps; {
		if time.Now().After(until) {
			cancel()
			<-done
			t.Fatalf("%d sweeps in %v at an interval of %v, want %d: listings at %v\n%s", len(ends.times()), deadline, interval, sweeps, github.times(), logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	// Each attempt either swept or is one of these, and each of these is a fixture of this row
	// failing rather than the loop choosing: such an attempt ran no sweep and still left a wait,
	// so reading the counts below as the loop's doing would be reading the database's instead.
	// The shutdown is the one sweep that may fail, since it cancels a listing in flight.
	for _, failure := range []string{"dispatch duty: take the lease", "dispatch duty: read the loop's last pass"} {
		if attempts := logs.count(failure, "lease="+webhookRedeliveryLease); attempts != 0 {
			t.Fatalf("%q for %d of the sweep's attempts:\n%s", failure, attempts, logs)
		}
	}
	if failed := swept.count("webhook redelivery sweep failed") - swept.count("webhook redelivery sweep failed", "context canceled"); failed != 0 {
		t.Fatalf("%d sweeps failed for something other than the shutdown:\n%s", failed, swept)
	}
	if skipped := logs.count("dispatch duty: leased pass skipped", "lease="+webhookRedeliveryLease); skipped != 0 {
		t.Errorf("%d sweeps were skipped, want none: a wait of at least one interval reaches the next period\n%s", skipped, logs)
	}
	listings, drawn := github.times(), waits.times()
	if len(listings) < sweeps {
		t.Fatalf("%d listings for %d sweeps, want one each", len(listings), sweeps)
	}
	if len(drawn) == 0 {
		t.Fatal("the loop asked for no wait at all")
	}
	if !listings[0].Before(drawn[0]) {
		t.Errorf("the first sweep listed %v after the loop asked for its first wait, want it at start, before any wait", listings[0].Sub(drawn[0]))
	}
}
