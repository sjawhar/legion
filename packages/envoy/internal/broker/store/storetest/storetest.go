// Package storetest gives each Postgres-backed broker test a migrated schema of its own on the
// BROKER_TEST_DATABASE_URL server, dropped when the test ends. Every broker package's tests run
// against one server at once, and the poller's passes act on every row of their tables, so tests
// that shared tables would act on each other's rows; with a schema per test no test can see
// another's rows, in its own package, a sibling package, or a concurrent run of either. It is an
// ordinary package rather than a _test.go file so every broker package can import it.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/store"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

// URL creates a fresh schema on the DatabaseURLEnv server, migrates it, drops it when t ends, and
// returns a connection string whose connections use that schema. params are extra connection
// parameters as key=value (e.g. "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed broker tests")
	}
	ctx := context.Background()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schema := "broker_test_" + hex.EncodeToString(suffix[:])
	admin(t, raw, "create schema "+schema)
	t.Cleanup(func() { admin(t, raw, "drop schema "+schema+" cascade") })

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	for _, param := range params {
		values, err := url.ParseQuery(param)
		if err != nil {
			t.Fatalf("connection parameter %q: %v", param, err)
		}
		for key := range values {
			query.Set(key, values.Get(key))
		}
	}
	parsed.RawQuery = query.Encode()
	dsn := parsed.String()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test schema: %v", err)
	}
	defer st.Pool.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return dsn
}

// Open is store.Open on a fresh URL(t, params...), its pool closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), URL(t, params...))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(st.Pool.Close)
	return st
}

func admin(t testing.TB, databaseURL, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", DatabaseURLEnv, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
