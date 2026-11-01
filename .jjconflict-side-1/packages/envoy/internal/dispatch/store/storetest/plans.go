package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Plan is the one way a pool Planned opens lets Postgres scan a table.
type Plan string

const (
	// SequentialScan reads a table's rows in the order they lie in it, where an update writes a
	// row's new version after the rest.
	SequentialScan Plan = "sequential scan"
	// IndexScan reads a table's rows through an index, in the index's order.
	IndexScan Plan = "index scan"
)

// Plans is every Plan, for a test that reads under each.
var Plans = []Plan{SequentialScan, IndexScan}

// Planned opens a pool on pool's database whose every connection scans tables with plan alone,
// closed after t ends, with every pool it opens apart (a listener's). A table a test fills holds
// too few rows for Postgres to choose either plan as it does for a production read, so a test that
// reads under each is how it shows a read answers in one order whichever plan serves it.
func Planned(t testing.TB, pool *store.Pool, plan Plan) *store.Pool {
	t.Helper()
	var settings map[string]string
	switch plan {
	case SequentialScan:
		settings = map[string]string{"enable_indexscan": "off", "enable_indexonlyscan": "off", "enable_bitmapscan": "off"}
	case IndexScan:
		settings = map[string]string{"enable_seqscan": "off", "enable_bitmapscan": "off"}
	default:
		t.Fatalf("storetest.Planned: unknown plan %q", plan)
	}
	config := pool.Config()
	for name, value := range settings {
		config.ConnConfig.RuntimeParams[name] = value
	}
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool reading with a %s: %v", plan, err)
	}
	planned := store.NewPool(opened)
	t.Cleanup(planned.Close)
	return planned
}
