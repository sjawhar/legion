// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database. It is
// an ordinary package rather than a _test.go file so every package whose tests need a Dispatch
// database can import it; Go cannot share test-only code across packages.
//
// Open clones each database from a template this test process migrates once. Running every
// migration, each in its own committed transaction, is most of the Postgres work a test would
// otherwise do, so a package's wall time would scale with its test count times the speed of the
// Postgres it runs against; a clone costs one create database.
//
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops. Dropping a database forces a checkpoint and waits for it, and every package testing
// against the same server queues on its one checkpointer: a test that waited for its own drop
// spent that queue's time on its clock.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DatabaseURLEnv names the Postgres server the tests run against. Its database path is ignored:
// the admin connection uses the postgres database, and every test gets a database of its own.
const DatabaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

var shared struct {
	// main is set by Main before the package's tests run; Open refuses to run without it, since
	// only Main drops the template and waits for the per-test drops.
	main     bool
	once     sync.Once
	err      error
	server   url.URL
	admin    *pgxpool.Pool
	template string

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
}

// Open returns a store on a new database carrying every migration, dropped after t ends. It skips
// t when DISPATCH_TEST_DATABASE_URL is unset.
func Open(t testing.TB) *store.Store {
	t.Helper()
	if !shared.main {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }")
	}
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	shared.once.Do(func() { shared.err = prepare(raw) })
	if shared.err != nil {
		t.Fatalf("prepare migrated template database: %v", shared.err)
	}

	ctx := context.Background()
	name := randomName(t, "dispatch_test_")
	if _, err := shared.admin.Exec(ctx, "create database "+name+" template "+shared.template); err != nil {
		t.Fatalf("clone isolated database from template: %v", err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	database, err := store.Open(ctx, databaseURL(shared.server, name))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// Main runs the package's tests, waits for their databases to drop, then drops the template Open
// cloned from. A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int {
	shared.main = true
	code := m.Run()
	if shared.admin == nil {
		return code
	}
	defer shared.admin.Close()
	shared.drops.Wait()
	for _, failure := range shared.dropFails {
		fmt.Fprintf(os.Stderr, "storetest: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	if shared.template == "" {
		return code
	}
	if _, err := shared.admin.Exec(context.Background(), "drop database "+shared.template); err != nil {
		fmt.Fprintf(os.Stderr, "storetest: drop template database %s: %v\n", shared.template, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// dropDatabase drops name without waiting for it; Main waits for every drop. Drops in flight
// together share one checkpoint.
func dropDatabase(name string) {
	shared.drops.Go(func() {
		if _, err := shared.admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			shared.dropMu.Lock()
			shared.dropFails = append(shared.dropFails, fmt.Sprintf("drop isolated database %s: %v", name, err))
			shared.dropMu.Unlock()
		}
	})
}

// prepare opens the admin pool and migrates the template every Open clones.
func prepare(raw string) error {
	server, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	shared.server = *server
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL(shared.server, "postgres"))
	if err != nil {
		return fmt.Errorf("parse admin database URL: %w", err)
	}
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	shared.admin = admin

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := "dispatch_template_" + suffix
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	shared.template = name
	database, err := store.Open(ctx, databaseURL(shared.server, name))
	if err != nil {
		return fmt.Errorf("open template database: %w", err)
	}
	err = database.Migrate(ctx)
	database.Pool.Close()
	if err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close template database to connections: %w", err)
	}
	return nil
}

func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

func randomName(t testing.TB, prefix string) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return prefix + suffix
}

func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}
