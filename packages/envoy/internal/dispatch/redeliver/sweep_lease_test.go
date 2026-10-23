package redeliver_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
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
		held <- duty.Exclusive(context.Background(), leases, webhookRedeliveryLease, 0, func(context.Context) {
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
