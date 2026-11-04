// Package storetest gives each Postgres-backed Dispatch test a fully migrated database of its
// own, and waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an
// ordinary package rather than a _test.go file so every package whose tests need a Dispatch
// database can import it; Go cannot share test-only code across packages.
//
// The databases themselves come from internal/pgclone, which this package and the secrets
// broker's storetest share: every database is a clone of a template this test process migrates
// once, a test's database is emptied and handed to the next test rather than dropped, and one a
// test changed beyond its rows is dropped instead. Its package comment says what each of those
// costs and why. What this package decides is Dispatch's half: the environment variable, the
// migrations, the snapshot schema's name and the size of the pool each test's store opens.
//
// Running every migration, each in its own committed transaction, is most of the Postgres work a
// test would otherwise do, so a package's wall time would scale with its test count times the
// speed of the Postgres it runs against; a clone costs one create database. Emptying a database
// is one truncate, of the tables that hold rows, which writes little; creating a database and
// dropping one each force a checkpoint and wait for it, and every package testing against the
// same server queues on that server's one checkpointer. Those same transactions hold back the
// instance-wide safe horizon the api package's live-feed and since-read tests wait on
// (docs/testing.md).
//
// OpenFresh is for a test that must have a database no other test has written into.
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to the
// whole server DatabaseURLEnv names, which can hold other databases (the one
// packages/envoy/scripts/dev-postgres.sh runs holds the local Dispatch's), and a change to them
// outlives the run.
package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgclone"
)

// DatabaseURLEnv names the Postgres server the tests run against. Its database path is ignored:
// the admin connection uses the postgres database, and every test gets a database of its own.
const DatabaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

// seedSchema holds the harness's copy of every table the template has rows in. It lives in the
// template rather than in this process, so every clone carries it from its first page and no
// test pays for taking the snapshot; the fingerprint leaves it out, since it is the harness's
// and not the schema under test.
const seedSchema = "storetest_seed"

// testPoolSize bounds the shared pool of every store Open hands a test, in place of production's
// sharedPoolSize. A store per test database against one Postgres is what the harness holds, and
// that server bounds them all: CI's service container takes no command of its own, so its
// max_connections is PostgreSQL's default 100, less the 3 slots it reserves for superusers. Under
// go test, GOMAXPROCS packages build and run at once - 4 on CI's runners - and tests within a
// package will run at once too, so the live stores are a handful, not one. Sixteen is above what
// any one test is known to hold at once: the deepest lock-wait rig queues three writers behind a
// held transaction while the rest of the pool serves the test's reads (WaitForLockWaiters's
// callers). The pools Pool.separate opens - the rooms, lease, outbox, listener, health,
// compaction and horizon pools - keep their own sizes, so this bounds the shared pool alone.
//
// It is not a free knob: a test writes against the number. Halving it to eight failed
// TestConcurrentSendsUnderOneKeyMakeOneMessage (internal/dispatch/api), which holds a table lock
// and waits for the other fifteen connections of this pool to queue behind it. Lowering it means
// reading that test with it.
const testPoolSize = 16

// shared hands out this package's databases. Main drops what its tests leave.
var shared = pgclone.New(pgclone.Config{
	Name:           "storetest",
	DatabaseURLEnv: DatabaseURLEnv,
	SkipMessage:    DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests",
	DatabasePrefix: "dispatch_test_",
	TemplatePrefix: "dispatch_template_",
	SeedSchema:     seedSchema,
	Migrate:        migrateTemplate,
})

// Open returns a store on a database carrying every migration and nothing another test wrote,
// which goes back on the free list once t ends. It skips t when DISPATCH_TEST_DATABASE_URL is
// unset.
func Open(t testing.TB) *store.Store {
	t.Helper()
	return open(t, true)
}

// OpenFresh returns a store on a database cloned for t alone and dropped once t ends. Use it for
// a test that must have a database no other test has written into: one that counts what a
// server, a sweep or a migration wrote across the whole of a table, where a row an earlier test
// left in a table the reset does not reach would change the answer, and one that leaves a
// session or a process of its own on the database past its cleanup, which a database that goes
// back on the free list cannot carry. Every other test uses Open: a clone and a drop each force
// a checkpoint that every package on the server queues on (the package comment).
func OpenFresh(t testing.TB) *store.Store {
	t.Helper()
	return open(t, false)
}

// open returns a store on a database of its own, taken from the free list when reusable allows
// it and the list has one, and cloned from the template when it does not.
func open(t testing.TB, reusable bool) *store.Store {
	t.Helper()
	if !shared.Installed() {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }")
	}
	name := shared.Take(t, reusable)
	ctx := context.Background()
	database, err := store.Open(ctx, shared.URL(name), store.WithMaxConns(testPoolSize))
	if err != nil {
		shared.Drop(name)
		t.Fatalf("open isolated database: %v", err)
	}
	if reusable {
		// The pool closes before the database is queued, which is what Release is handed the
		// close for: a database with a session still on it is dropped rather than reused.
		t.Cleanup(func() { shared.Release(name, database.Pool.Close) })
	} else {
		t.Cleanup(func() {
			database.Pool.Close()
			shared.Drop(name)
		})
	}
	return database
}

// migrateTemplate migrates the template every Open clones. The harness reads the migrated schema
// through the store's own pool, so the template is opened the way a test's database is.
func migrateTemplate(ctx context.Context, dsn string) (pgclone.Querier, func(), error) {
	database, err := store.Open(ctx, dsn, store.WithMaxConns(testPoolSize))
	if err != nil {
		return nil, nil, fmt.Errorf("open template database: %w", err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Pool.Close()
		return nil, nil, fmt.Errorf("migrate template database: %w", err)
	}
	return database.Pool, database.Pool.Close, nil
}

// Main runs the package's tests, waits for the last of their databases to be emptied, drops
// every database its tests left - the free list's and the ones no reset could take back - then
// drops the template Open cloned from. A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return shared.Main(m) }
