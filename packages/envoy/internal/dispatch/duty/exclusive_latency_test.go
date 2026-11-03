package duty

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// slowWrites is a database connection whose every write waits delay first while slow is set: a
// loaded process descheduled, or a network path that stalls, between the tick and the database
// answering its period query.
type slowWrites struct {
	net.Conn
	slow  *atomic.Bool
	delay time.Duration
}

func (c slowWrites) Write(b []byte) (int, error) {
	if c.slow.Load() {
		time.Sleep(c.delay)
	}
	return c.Conn.Write(b)
}

// A lone loop's ticks come one period apart, and each must run. Here the first tick is due 50 ms
// before a period boundary on this process's clock, the clock Exclusive buckets a due time on, and
// every write on its lease connection stalls 100 ms - among them the ones that carry the period
// query - so any moment read at the database, the database's now() less a duration measured
// before the query included, lands past the boundary. The second tick, due exactly one period
// later, stalls on nothing; read by such a moment it lands in the period the first has recorded
// as served, and a lone loop loses that period, the defect
// TestExclusiveCountsALateTickInThePeriodItWasDueIn covers for a delay before the query.
func TestExclusiveCountsATickStalledAfterItsMeasureInThePeriodItWasDueIn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const name = "suggestion-outcomes"
	const period = time.Second
	const early, delay = 50 * time.Millisecond, 100 * time.Millisecond

	var slow atomic.Bool
	config := database.Pool.Config()
	config.MaxConns = 1
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return slowWrites{Conn: conn, slow: &slow, delay: delay}, nil
	}
	leases, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the stalling leases pool: %v", err)
	}
	t.Cleanup(leases.Close)

	due := beforeNextBoundary(t, period, early)
	time.Sleep(time.Until(due))
	slow.Store(true)
	ran := Exclusive(ctx, leases, name, due, period, func(context.Context) {})
	slow.Store(false)
	if !ran {
		t.Fatal("the first tick did not run, so this row measures nothing")
	}
	next := due.Add(period)
	time.Sleep(time.Until(next))
	ran = false
	if !Exclusive(ctx, leases, name, next, period, func(context.Context) { ran = true }) || !ran {
		t.Errorf("the tick due one period after a stalled one was refused (first due %v before a boundary, its writes stalled %v each); a lone loop lost that period",
			early, delay)
	}
}
