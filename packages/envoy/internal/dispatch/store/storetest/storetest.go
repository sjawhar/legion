// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database, and
// waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an ordinary
// package rather than a _test.go file so every package whose tests need a Dispatch database can
// import it; Go cannot share test-only code across packages.
//
// Open clones each database from one migrated template, which the first test process to want it
// builds and every other shares. Running every migration, each in its own committed transaction,
// is most of the Postgres work a test would otherwise do, so a package's wall time would scale
// with its test count times the speed of the Postgres it runs against; a clone costs one create
// database. Nothing is handed from one test to the next: a test's database is dropped when that
// test ends, so no test can be given a database another test wrote in.
//
// The template is named for the migration set it carries (store.MigrationsFingerprint), built
// under an advisory lock and under a name of its own until it is whole, and never dropped: the
// test processes of one run start minutes apart and a process that dropped it would take it from
// the ones still cloning. A set that gains, loses or edits a migration is a template of its own,
// so a stale one is never cloned - it is only left on the server. Every CI job that runs a
// Postgres service runs a container deleted with the job; a server that outlives a run, the one
// packages/envoy/scripts/dev-postgres.sh runs, keeps one 13 MB database per migration set a run
// there has tested, which `drop database` by hand reclaims (docs/testing.md).
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead: 6.4 MB of the
// 12.7 MB template per clone, 16 GiB over one run of the api and docs packages. A file copy writes
// a few hundred bytes of log; Postgres forces a checkpoint before and after the copy, and the drop
// at the end of the test forces one more. What makes that affordable is the server rather than any
// machinery here: every CI job that runs a Postgres service runs a container deleted with the job,
// with fsync, full_page_writes and synchronous_commit off, where a forced checkpoint writes nothing
// durable (dispatch://INFRA-1075).
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to the
// whole server DatabaseURLEnv names, which can hold other databases (the one
// packages/envoy/scripts/dev-postgres.sh runs holds the local Dispatch's), and a change to them
// outlives the run.
//
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops before it returns, so no test database outlives the package. Dropping a database forces a
// checkpoint and waits for it, and every package testing against the same server queues on its
// one checkpointer: a test that waited for its own drop spent that queue's time on its clock.
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DatabaseURLEnv names the Postgres server the tests run against. Its database path is ignored:
// the admin connection uses the postgres database, and every test gets a database of its own.
const DatabaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

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

var shared struct {
	// main is set by Main before the package's tests run; Open refuses to run without it, since
	// only Main waits for the per-test drops.
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
	if _, err := shared.admin.Exec(ctx, cloneStatement(name)); err != nil {
		t.Fatalf("clone isolated database from template: %v", err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	database, err := store.Open(ctx, databaseURL(shared.server, name), store.WithMaxConns(testPoolSize))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// OpenFresh is Open. Both hand a test a database no other test has written into, and both names
// are kept because a call site says in them what the test needs of its database: OpenFresh's
// callers are the tests that count what a server, a sweep or a migration wrote across the whole
// of a table, and the ones that leave a session or a process of their own on the database past
// their cleanup.
func OpenFresh(t testing.TB) *store.Store {
	t.Helper()
	return Open(t)
}

// cloneStatement returns the create database that makes name a copy of the template's files
// rather than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name string) string {
	return "create database " + name + " template " + shared.template + " strategy file_copy"
}

// Main runs the package's tests and waits for their databases to drop. A package whose tests call
// Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
//
// The template stays: it is named after the migration set and shared by every test process on the
// server (prepare), so a process that dropped it would take it from the processes still cloning
// it.
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
	return code
}

// dropDatabase drops name without waiting for it; Main waits for every drop before it returns.
// Dropping a database forces a checkpoint and waits for it, which a test that waited would spend
// on its own clock (the package comment); drops in flight together share one checkpoint.
func dropDatabase(name string) {
	shared.drops.Go(func() {
		if _, err := shared.admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			shared.dropMu.Lock()
			shared.dropFails = append(shared.dropFails, fmt.Sprintf("drop isolated database %s: %v", name, err))
			shared.dropMu.Unlock()
		}
	})
}

// prepare opens the admin pool and finds the template every Open clones: the one database named
// after this build's migration set, built here when no process has built it yet.
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
	// Every clone and every drop runs on this pool, one of each per test rather than one per
	// package, so it is sized for the tests a package runs at once rather than for the drops
	// alone. It is still bounded: several package binaries test against the one server and each
	// holds a pool of its own.
	config.MaxConns = 8
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	shared.admin = admin

	fingerprint, err := store.MigrationsFingerprint()
	if err != nil {
		return err
	}
	name := templatePrefix + fingerprint
	// One builder at a time, whatever set it builds: the lock also covers the scratch databases
	// a build leaves behind when it fails, which the builder holding it clears.
	unlock, err := lockTemplates(ctx, admin)
	if err != nil {
		return err
	}
	defer unlock()
	built, err := databaseExists(ctx, admin, name)
	if err != nil {
		return err
	}
	if !built {
		if err := buildTemplate(ctx, admin, name); err != nil {
			return err
		}
	}
	shared.template = name
	return nil
}

// templatePrefix opens the name of the shared template and of the scratch database a build of it
// runs in, so that clearing the scratch of a build that failed names no other database.
const templatePrefix = "dispatch_template_"

// buildingPrefix opens the name a template is migrated under, before the rename that gives it the
// name the clones read. A template exists under its own name only once it carries every
// migration, so a build that fails or a process that dies mid-build leaves no half-migrated
// database for the next process to clone.
const buildingPrefix = templatePrefix + "building_"

// templateLockKey is the advisory lock a process takes before it looks for the template or builds
// one. Advisory locks are the cluster's rather than one database's, so every test process on the
// server queues on this one whichever database its admin pool connects to. It is nothing to do
// with store.migrationLockKey, which a migration run takes inside the database it migrates.
const templateLockKey int64 = 8150101

// lockTemplates holds templateLockKey on a connection of its own and returns the release. A
// session-level lock rather than a transaction's, because the build it covers runs on another
// connection, in transactions of its own.
func lockTemplates(ctx context.Context, admin *pgxpool.Pool) (func(), error) {
	conn, err := admin.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire a connection for the template lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", templateLockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take the template lock: %w", err)
	}
	return func() {
		if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1)", templateLockKey); err != nil {
			fmt.Fprintf(os.Stderr, "storetest: release the template lock: %v\n", err)
		}
		conn.Release()
	}, nil
}

func databaseExists(ctx context.Context, admin *pgxpool.Pool, name string) (bool, error) {
	var exists bool
	if err := admin.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("look for database %s: %w", name, err)
	}
	return exists, nil
}

// buildTemplate migrates a scratch database and renames it to name, under the lock prepare holds.
func buildTemplate(ctx context.Context, admin *pgxpool.Pool, name string) error {
	if err := dropStaleBuilds(ctx, admin); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	building := buildingPrefix + suffix
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+building+" strategy file_copy"); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	database, err := store.Open(ctx, databaseURL(shared.server, building), store.WithMaxConns(testPoolSize))
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
	if _, err := admin.Exec(ctx, "alter database "+building+" with allow_connections false"); err != nil {
		return fmt.Errorf("close template database to connections: %w", err)
	}
	if _, err := admin.Exec(ctx, "alter database "+building+" rename to "+name); err != nil {
		return fmt.Errorf("name the migrated template %s: %w", name, err)
	}
	return nil
}

// dropStaleBuilds removes what a build that failed or a process that died mid-build left behind.
// It runs under the template lock, which no other build holds, so every such database is finished
// with.
func dropStaleBuilds(ctx context.Context, admin *pgxpool.Pool) error {
	rows, err := admin.Query(ctx, "select datname from pg_database where datname like $1", buildingPrefix+"%")
	if err != nil {
		return fmt.Errorf("look for half-built templates: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("look for half-built templates: %w", err)
	}
	for _, name := range stale {
		if _, err := admin.Exec(ctx, "drop database "+name+" with (force)"); err != nil {
			return fmt.Errorf("drop the half-built template %s: %w", name, err)
		}
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
