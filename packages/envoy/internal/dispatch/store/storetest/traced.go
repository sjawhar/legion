package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Traced opens a pool on pool's database whose every statement tracer sees, with params set on
// each of its connections, closed after t ends. The lease and listener connections opened from its
// configuration (store.Pool.Leases, store.Pool.Listener) are traced too. Only the code under test
// is given it, so everything tracer sees is that code's.
func Traced(t testing.TB, pool *store.Pool, tracer pgx.QueryTracer, params map[string]string) *store.Pool {
	t.Helper()
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	for name, value := range params {
		config.ConnConfig.RuntimeParams[name] = value
	}
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a traced pool: %v", err)
	}
	traced := store.NewPool(opened)
	t.Cleanup(traced.Close)
	return traced
}

// StatementCounter counts every statement started on the pool it traces (Traced).
type StatementCounter struct{ count atomic.Int64 }

func (c *StatementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (c *StatementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count is how many statements c has counted.
func (c *StatementCounter) Count() int64 {
	return c.count.Load()
}

// AwaitQuiet waits until counter has counted no statement for quiet, failing once within passes.
func AwaitQuiet(t testing.TB, counter *StatementCounter, quiet, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last, since := counter.Count(), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("statements did not stop for %v within %v", quiet, within)
		}
		time.Sleep(20 * time.Millisecond)
		if n := counter.Count(); n != last {
			last, since = n, time.Now()
		}
	}
}

// RequireNoStatements fails if counter counts any statement over window.
func RequireNoStatements(t testing.TB, counter *StatementCounter, window time.Duration, what string) {
	t.Helper()
	before := counter.Count()
	time.Sleep(window)
	if made := counter.Count() - before; made != 0 {
		t.Errorf("%s: %d statements over %v, want none", what, made, window)
	}
}

// StatementRecorder records the SQL of every statement run on the pool it traces (Traced).
type StatementRecorder struct {
	mu  sync.Mutex
	sql []string
}

func (r *StatementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sql = append(r.sql, data.SQL)
	r.mu.Unlock()
	return ctx
}

func (r *StatementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Statements is the SQL of every statement r has recorded, in the order they started.
func (r *StatementRecorder) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sql...)
}
