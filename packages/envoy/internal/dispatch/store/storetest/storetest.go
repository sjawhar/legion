// Package storetest gives each Postgres-backed Dispatch test a fully migrated database of its
// own, and waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an
// ordinary package rather than a _test.go file so every package whose tests need a Dispatch
// database can import it; Go cannot share test-only code across packages.
//
// Every database is a clone of a template this test process migrates once. Running every
// migration, each in its own committed transaction, is most of the Postgres work a test would
// otherwise do, so a package's wall time would scale with its test count times the speed of the
// Postgres it runs against; a clone costs one create database.
//
// A test's database is not dropped when the test ends: the test's cleanup hands it to one of
// the harness's reset workers, which empties it and puts it on a free list the next Open takes
// from, so a package makes as many databases as it has in flight rather than one per test, and
// no test waits for a reset it did not cause. Emptying is one truncate, of the tables that hold
// rows, which writes little; creating a database and dropping one each force a checkpoint and
// wait for it, and every package testing against the same server queues on that server's one
// checkpointer. Those same transactions hold back the instance-wide safe horizon the api
// package's live-feed and since-read tests wait on (docs/testing.md).
//
// The template is not empty - the schema_migrations row of every migration, and the rows the
// migrations seeded - so emptying a database has to leave behind what a fresh clone holds:
// prepare snapshots every table of the template that has rows into a schema of the harness's
// own, storetest_seed, which every clone then carries, and the reset copies those rows back and
// puts every sequence of schema public where the template left it.
//
// A database a test changed beyond its rows is dropped rather than put back. The reset compares
// a fingerprint of the database - the tables and columns of schema public, the functions and
// triggers it holds there, the schemas it holds, and its own settings, which is where a test
// that ran alter database shows up - with the template's, and ignores the snapshot schema in
// it. Any difference, a session of the test still connected, or a reset that fails, sends the
// database to the background drops. OpenFresh is for a test that must have a database no other
// test has written into.
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead: 6.4 MB of the
// 12.7 MB template per clone, 16 GiB over one run of the api and docs packages. A file copy writes
// a few hundred bytes of log; Postgres forces a checkpoint before and after the copy.
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to the
// whole server DatabaseURLEnv names, which can hold other databases (the one
// packages/envoy/scripts/dev-postgres.sh runs holds the local Dispatch's), and a change to them
// outlives the run.
//
// A database that is dropped rather than reused drops in the background, and Main waits for the
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
const testPoolSize = 16

// resetLockTimeout bounds how long a reset waits for a table's lock. A reset that waits is a
// session the test left behind holding one, which is reason to drop the database rather than
// reason to wait: the test has ended and nothing is coming to release it.
const resetLockTimeout = "10s"

// strangerWait and strangerTries bound how long a reset waits for the sessions of the pool the
// test just closed to go. A closed pool's backends exit on their own, a moment after the socket
// closes, and any session still on the database past that wait is a test's leak: the reset
// counts it and the database is dropped rather than put back.
const (
	strangerWait  = 20 * time.Millisecond
	strangerTries = 10
)

// resetWorkers is how many databases are emptied at once, and resetQueue how many wait for a
// worker. A test's cleanup hands its database to a worker rather than emptying it itself: the
// reset's truncate is a transaction of its own, which the test that follows has no reason to
// wait for, as a test has never waited for its database to drop. The queue bounds what the
// harness can be holding: a package whose tests are quicker than a reset keeps at most
// resetWorkers + resetQueue databases beyond the ones its tests hold, and the cleanup after
// that waits for a worker rather than cloning the server full of databases. Four workers put no
// more transactions on the server than the drops of today's harness do, which are unbounded.
const (
	resetWorkers = 4
	resetQueue   = 8
)

var shared struct {
	// main is set by Main before the package's tests run; Open refuses to run without it, since
	// only Main drops the template, the free list, and waits for the per-test drops.
	main     bool
	once     sync.Once
	err      error
	server   url.URL
	admin    *pgxpool.Pool
	template string

	// A reset truncates the tables the test it follows left rows in, and the tables the template
	// itself holds rows in, which it then restores: quoted names every table of schema public,
	// seeded indexes the tables the template holds rows in, dirty asks a database which tables
	// hold rows now, restores copies the template's rows back and sequences put every sequence
	// where the template left it. fingerprint is what fingerprintQuery answers on the template.
	// prepare builds them all, once, by reading the migrated template rather than any list
	// written here, so none of them rots as migrations add tables.
	quoted      []string
	seeded      []int
	dirty       string
	restores    []string
	sequences   []string
	fingerprint string

	// freeMu guards free, the databases a reset emptied and gave back, and recycling carries
	// the databases their tests are done with to the resetWorkers goroutines that empty them.
	// Tests of one package run in one process and will run at the same time (t.Parallel), so
	// every reader of the list takes the mutex.
	freeMu    sync.Mutex
	free      []string
	recycling chan string
	recyclers sync.WaitGroup

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string

	// resetMu guards what Main reports about the resets that failed. A database whose
	// fingerprint moved is a test changing its schema and is dropped in silence; a reset that
	// could not run at all is worth a line, since a harness that never reuses a database is as
	// slow as one that drops every database and says nothing about it.
	resetMu    sync.Mutex
	resetFails int
	resetFirst string
}

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
	name := ""
	if reusable {
		name = takeFree()
	}
	if name == "" {
		name = randomName(t, "dispatch_test_")
		if _, err := shared.admin.Exec(ctx, cloneStatement(name)); err != nil {
			t.Fatalf("clone isolated database from template: %v", err)
		}
	}
	database, err := store.Open(ctx, databaseURL(shared.server, name), store.WithMaxConns(testPoolSize))
	if err != nil {
		dropDatabase(name)
		t.Fatalf("open isolated database: %v", err)
	}
	if reusable {
		t.Cleanup(func() { release(name, database) })
	} else {
		t.Cleanup(func() {
			database.Pool.Close()
			dropDatabase(name)
		})
	}
	return database
}

// takeFree takes a database off the free list, or returns "" when the list is empty.
func takeFree() string {
	shared.freeMu.Lock()
	defer shared.freeMu.Unlock()
	if len(shared.free) == 0 {
		return ""
	}
	// The last database back is the one Postgres and the page cache hold warmest.
	name := shared.free[len(shared.free)-1]
	shared.free = shared.free[:len(shared.free)-1]
	return name
}

// putFree puts an emptied database back for the next test.
func putFree(name string) {
	shared.freeMu.Lock()
	defer shared.freeMu.Unlock()
	shared.free = append(shared.free, name)
}

// release hands the database a test is done with to a reset worker, which empties it and puts
// it back on the free list, or drops it when it is not the template's copy any more. It waits
// only when every worker is busy and the queue behind them is full.
func release(name string, database *store.Store) {
	database.Pool.Close()
	shared.recycling <- name
}

// recycleWorker empties the databases their tests are done with, one at a time, until Main
// closes the queue.
func recycleWorker() {
	defer shared.recyclers.Done()
	for name := range shared.recycling {
		reusable, err := recycle(context.Background(), name)
		if err != nil {
			noteResetFailure(name, err)
		}
		if !reusable {
			dropDatabase(name)
			continue
		}
		putFree(name)
	}
}

// recycle empties name and reports whether it may serve another test. It opens a connection of
// its own, after the test's pool has closed, so that every other session it then sees on the
// database is one the test left behind.
func recycle(ctx context.Context, name string) (bool, error) {
	config, err := pgx.ParseConfig(databaseURL(shared.server, name))
	if err != nil {
		return false, fmt.Errorf("parse the database URL: %w", err)
	}
	config.RuntimeParams["lock_timeout"] = resetLockTimeout
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return false, fmt.Errorf("connect to reset the database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	fingerprint, strangers, err := inspect(ctx, conn)
	if err != nil {
		return false, err
	}
	for try := 0; strangers > 0 && try < strangerTries; try++ {
		time.Sleep(strangerWait)
		if _, strangers, err = inspect(ctx, conn); err != nil {
			return false, err
		}
	}
	if fingerprint != shared.fingerprint || strangers > 0 {
		return false, nil
	}
	reset, err := resetStatements(ctx, conn)
	if err != nil {
		return false, err
	}
	if _, err := conn.Exec(ctx, reset); err != nil {
		return false, fmt.Errorf("reset the database: %w", err)
	}
	return true, nil
}

// resetStatements is the reset the database conn is on runs: one truncate of the tables that
// hold rows, the template's own rows put back, and every sequence of schema public set where
// the template left it.
//
// It truncates the tables that hold rows rather than all 91 of them because a truncate costs
// what the relations it empties cost, rows or no rows: Postgres gives every table and every
// index of one a new file, and on this repository's schema that is about 0.6 ms a relation under
// gVisor, 200 ms for the schema against 5 ms for a test that wrote nothing. Which tables hold
// rows is one statement asking each of them whether a row exists, about 3 ms, and it is read
// from the database rather than from what the test said it wrote, so a row a trigger or a
// background loop of the code under test wrote is in it too. The tables the template holds rows
// in are always among them, since a test may have changed those rows without adding a table to
// the set, and cascade adds whatever references what is truncated.
func resetStatements(ctx context.Context, conn *pgx.Conn) (string, error) {
	dirty, err := tablesWithRows(ctx, conn)
	if err != nil {
		return "", err
	}
	clear := make(map[int]bool, len(shared.seeded)+len(dirty))
	for _, at := range shared.seeded {
		clear[at] = true
	}
	for _, at := range dirty {
		clear[at] = true
	}

	tables := make([]string, 0, len(clear))
	for at, quoted := range shared.quoted {
		if clear[at] {
			tables = append(tables, quoted)
		}
	}
	// One Exec of the lot is one round trip and, under the simple protocol, one implicit
	// transaction: a database whose reset fails part way through is never put back.
	statements := make([]string, 0, 1+len(shared.restores)+len(shared.sequences))
	if len(tables) > 0 {
		statements = append(statements, "truncate table "+strings.Join(tables, ", ")+" restart identity cascade")
	}
	statements = append(statements, shared.restores...)
	statements = append(statements, shared.sequences...)
	return strings.Join(statements, ";\n"), nil
}

// noteResetFailure records a reset Main reports, and counts the rest.
func noteResetFailure(name string, err error) {
	shared.resetMu.Lock()
	defer shared.resetMu.Unlock()
	shared.resetFails++
	if shared.resetFirst == "" {
		shared.resetFirst = fmt.Sprintf("%s: %v", name, err)
	}
}

// inspect reads the database's fingerprint and how many sessions other than this one are on it.
// Both come from one round trip, on a reset worker's clock rather than any test's.
func inspect(ctx context.Context, conn *pgx.Conn) (string, int, error) {
	var (
		fingerprint string
		strangers   int
	)
	if err := conn.QueryRow(ctx, fingerprintQuery).Scan(&fingerprint, &strangers); err != nil {
		return "", 0, fmt.Errorf("read the database's schema fingerprint: %w", err)
	}
	return fingerprint, strangers, nil
}

// fingerprintQuery answers what a reused database must still share with the template, and how
// many sessions other than the asking one are on it. The fingerprint covers the tables and
// columns information_schema lists for schema public and each column's default, every relation
// of that schema - its sequences and indexes with its tables - the functions and the triggers of
// that schema, the schemas the database holds, and the settings the database carries, which is
// where a test that ran alter database ... set search_path shows up (they live in
// pg_db_role_setting, not in pg_database: PostgreSQL moved them there). It leaves out the
// harness's own snapshot schema, which every clone carries and no test may change.
//
// Functions and triggers are in it because a test that leaves one behind leaves the next test a
// database that runs code it never wrote: a trigger on artifact_versions one api test added
// made an unrelated create-issue answer 500 in another, until the fingerprint saw it and the
// database was dropped rather than reused.
//
// A column's default is in it for the same reason, measured: the broadcast tests point
// messages.id at a function of their own that hands out eight fixed identifiers
// (installReverseBroadcastMessageIDDefault), and a database put back with that default still on
// it made 67 later tests of the api package fail on a duplicate messages_pkey in CI run
// 41729266538. A truncate empties a table; it does not restore what a test altered about one.
const fingerprintQuery = `
select
	coalesce((
		select md5(string_agg(entry, E'\n' order by entry))
		from (
			select format('column|%s|%s|%s|%s|%s|%s', table_name, column_name, ordinal_position,
				data_type, is_nullable, coalesce(column_default, '')) as entry
				from information_schema.columns where table_schema = 'public'
			union all
			select format('table|%s|%s', table_name, table_type)
				from information_schema.tables where table_schema = 'public'
			union all
			select format('relation|%s|%s', relation.relname, relation.relkind)
				from pg_class relation where relation.relnamespace = 'public'::regnamespace
			union all
			select format('function|%s|%s', routine.oid::regprocedure::text, routine.prokind)
				from pg_proc routine where routine.pronamespace = 'public'::regnamespace
			union all
			select format('trigger|%s|%s|%s', table_of.relname, trigger_of.tgname, trigger_of.tgtype)
				from pg_trigger trigger_of
				join pg_class table_of on table_of.oid = trigger_of.tgrelid
				where table_of.relnamespace = 'public'::regnamespace and not trigger_of.tgisinternal
			union all
			select format('schema|%s', nspname)
				from pg_namespace where nspname <> '` + seedSchema + `'
			union all
			select format('setting|%s', array_to_string(setting.setconfig, ','))
				from pg_db_role_setting setting
				join pg_database database on database.oid = setting.setdatabase
				where database.datname = current_database()
		) entries
	), ''),
	(select count(*) from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid())`

// cloneStatement returns the create database that makes name a copy of the template's files
// rather than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name string) string {
	return "create database " + name + " template " + shared.template + " strategy file_copy"
}

// Main runs the package's tests, waits for the last of their databases to be emptied, drops
// every database its tests left - the free list's and the ones no reset could take back - then
// drops the template Open cloned from. A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int {
	shared.main = true
	code := m.Run()
	if shared.admin == nil {
		return code
	}
	defer shared.admin.Close()
	// A prepare that failed before it started the workers left no queue to close.
	if shared.recycling != nil {
		close(shared.recycling)
		shared.recyclers.Wait()
	}
	for name := takeFree(); name != ""; name = takeFree() {
		dropDatabase(name)
	}
	shared.drops.Wait()
	if shared.resetFails > 0 {
		fmt.Fprintf(os.Stderr, "storetest: dropped %d database(s) whose reset failed; the first: %s\n",
			shared.resetFails, shared.resetFirst)
	}
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
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	shared.template = name
	database, err := store.Open(ctx, databaseURL(shared.server, name), store.WithMaxConns(testPoolSize))
	if err != nil {
		return fmt.Errorf("open template database: %w", err)
	}
	defer database.Pool.Close()
	if err := database.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	if err := prepareReset(ctx, database.Pool); err != nil {
		return err
	}
	database.Pool.Close()
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close template database to connections: %w", err)
	}
	// The workers start last, once everything a reset reads is built: nothing reaches them
	// before an Open hands a test a database, and the only Open that runs before this returns
	// is the one that called prepare.
	shared.recycling = make(chan string, resetQueue)
	shared.recyclers.Add(resetWorkers)
	for range resetWorkers {
		go recycleWorker()
	}
	return nil
}

// prepareReset reads the migrated template - its tables, the ones the migrations left rows in,
// and its sequences - writes the snapshot every clone carries, and builds the pieces a reset is
// made of and the fingerprint every reused database is held to. It runs on the template, so
// each clone is born with the snapshot and every piece is read from the schema as it is rather
// than from a list written here.
func prepareReset(ctx context.Context, pool *store.Pool) error {
	tables, err := publicTables(ctx, pool)
	if err != nil {
		return err
	}
	columns, err := insertableColumns(ctx, pool)
	if err != nil {
		return err
	}
	shared.quoted = make([]string, len(tables))
	for i, table := range tables {
		shared.quoted[i] = pgx.Identifier{"public", table}.Sanitize()
	}
	shared.dirty = rowProbe(shared.quoted)
	seeded, err := tablesWithRows(ctx, pool)
	if err != nil {
		return err
	}
	shared.seeded = seeded

	if _, err := pool.Exec(ctx, "create schema "+pgx.Identifier{seedSchema}.Sanitize()); err != nil {
		return fmt.Errorf("create the template's snapshot schema: %w", err)
	}
	for _, at := range seeded {
		table := tables[at]
		of := columns[table]
		if len(of) == 0 {
			return fmt.Errorf("the template's table %s holds rows in no column a snapshot can insert", table)
		}
		list := strings.Join(of, ", ")
		source := pgx.Identifier{seedSchema, table}.Sanitize()
		target := shared.quoted[at]
		if _, err := pool.Exec(ctx, "create table "+source+" as select "+list+" from "+target); err != nil {
			return fmt.Errorf("snapshot the template's %s rows: %w", table, err)
		}
		// overriding system value is what lets the rows of a table whose identity column is
		// generated always go back in with the values the template gave them. Postgres accepts
		// the clause on a table that has no identity column at all, so every restore carries it.
		shared.restores = append(shared.restores,
			"insert into "+target+" ("+list+") overriding system value select "+list+" from "+source)
	}
	if shared.sequences, err = sequenceResets(ctx, pool); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, fingerprintQuery).Scan(&shared.fingerprint, new(int)); err != nil {
		return fmt.Errorf("read the template's schema fingerprint: %w", err)
	}
	return nil
}

// publicTables names every table of schema public, read from the catalog so that a migration
// adding a table needs no change here.
func publicTables(ctx context.Context, pool *store.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select table_name from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE'
		order by table_name`)
	if err != nil {
		return nil, fmt.Errorf("read the template's tables: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("read the template's tables: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the template's tables: %w", err)
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("the migrated template holds no table in schema public")
	}
	return tables, nil
}

// insertableColumns gives, per table of schema public, the columns a snapshot copies and an
// insert puts back, in the table's own order. A stored generated column is left out: it is
// computed from the row, and an insert that names it is refused.
func insertableColumns(ctx context.Context, pool *store.Pool) (map[string][]string, error) {
	rows, err := pool.Query(ctx, `
		select table_name, column_name from information_schema.columns
		where table_schema = 'public' and is_generated = 'NEVER'
		order by table_name, ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("read the template's columns: %w", err)
	}
	defer rows.Close()
	columns := map[string][]string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("read the template's columns: %w", err)
		}
		columns[table] = append(columns[table], pgx.Identifier{column}.Sanitize())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the template's columns: %w", err)
	}
	return columns, nil
}

// rowProbe is the statement tablesWithRows runs: it asks each table whether a row exists, which
// costs a scan of as much of the table as it takes to find one, and answers with the table's
// position in shared.quoted, so no name is spelled into the statement.
func rowProbe(quoted []string) string {
	probes := make([]string, len(quoted))
	for i, table := range quoted {
		probes[i] = "select " + strconv.Itoa(i) + " where exists (select 1 from " + table + ")"
	}
	return strings.Join(probes, " union all ")
}

// tablesWithRows gives the positions in shared.quoted of the tables that hold rows: on the
// template, the tables the migrations seeded; on a database a test is done with, the tables the
// reset has to empty.
func tablesWithRows(ctx context.Context, from rowQuerier) ([]int, error) {
	rows, err := from.Query(ctx, shared.dirty)
	if err != nil {
		return nil, fmt.Errorf("read which tables hold rows: %w", err)
	}
	defer rows.Close()
	var found []int
	for rows.Next() {
		var at int
		if err := rows.Scan(&at); err != nil {
			return nil, fmt.Errorf("read which tables hold rows: %w", err)
		}
		found = append(found, at)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which tables hold rows: %w", err)
	}
	return found, nil
}

// rowQuerier is what tablesWithRows reads through: the template's pool at prepare, and the one
// connection a reset holds on the database it is emptying.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// sequenceResets returns one setval per sequence of schema public, putting it where the template
// left it. pg_sequences answers a sequence nextval has never been called on with a null
// last_value, which is the state setval's third argument writes as false.
func sequenceResets(ctx context.Context, pool *store.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select sequencename, coalesce(last_value, start_value), last_value is not null
		from pg_sequences where schemaname = 'public'
		order by sequencename`)
	if err != nil {
		return nil, fmt.Errorf("read the template's sequences: %w", err)
	}
	defer rows.Close()
	var resets []string
	for rows.Next() {
		var (
			sequence string
			value    int64
			called   bool
		)
		if err := rows.Scan(&sequence, &value, &called); err != nil {
			return nil, fmt.Errorf("read the template's sequences: %w", err)
		}
		resets = append(resets, fmt.Sprintf("select setval('%s', %d, %t)",
			pgx.Identifier{"public", sequence}.Sanitize(), value, called))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the template's sequences: %w", err)
	}
	return resets, nil
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
