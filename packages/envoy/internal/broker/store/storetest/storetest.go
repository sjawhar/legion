// Package storetest gives each Postgres-backed broker test a fully migrated database of its own,
// waits for backends to queue behind a lock a test holds (AwaitLockWaiters), and writes the rows
// only a database writer other than the broker could (Exec, ForgeRequestSignature,
// CopyAsOtherKind), for the tests that pin what a release or an authentication refuses. It is an
// ordinary package rather than a _test.go file so every broker package can import it.
//
// Every database is a clone of a template this test process migrates once. Migrate applies the
// whole set inside one transaction under a database-wide advisory lock (internal/broker/store),
// so tests that each migrated a schema of their own on one shared database both re-ran every
// migration and queued on that one lock, which is most of the Postgres work a broker test did
// (dispatch://INFRA-1075). A clone costs one create database.
//
// A test's database is not dropped when the test ends: the test's cleanup hands it to one of the
// harness's reset workers, which empties it and puts it on a free list the next caller takes
// from, so a package makes as many databases as it has tests in flight rather than one per test,
// and no test waits for a reset it did not cause. Emptying is one truncate of the tables that
// hold rows, which writes little, while creating a database and dropping one each force a
// checkpoint that every package testing against the same server queues on.
//
// The template is not empty - the broker_schema_migrations row of every migration, and whatever
// the migrations seeded - so emptying a database has to leave behind what a fresh clone holds:
// prepare snapshots every table of the template that has rows into a schema of the harness's
// own, which every clone then carries, and the reset copies those rows back and puts every
// sequence of schema public where the template left it.
//
// A database a test changed beyond its rows is dropped rather than put back: the reset compares
// a fingerprint of the database - the tables and columns of schema public with their defaults,
// the relations, functions and triggers it holds there, the schemas it holds and its own
// settings, which is where a test that ran alter database shows up - with the template's, and
// any difference, a session the test left connected, or a reset that failed sends the database
// to the background drops. OpenFresh is for a test that must have a database no other test has
// written into, or that leaves a session or a process of its own on the database past its
// cleanup.
//
// A clone copies the template's files (strategy file_copy), because Postgres's default from 15
// writes every page of the template through the write-ahead log instead (docs/testing.md, "A
// database a Go test creates"). The harness changes no Postgres setting: fsync,
// full_page_writes and max_wal_size belong to the whole server DatabaseURLEnv names, which can
// hold other databases, and a change to them outlives the run.
//
// A package whose tests call URL, Open or OpenFresh uses Main as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }. Main is what drops the free list
// and then the template. A package that installs none still works: with nobody to drop those at
// the end, each call makes a database of its own, migrates it and drops it with the test, which
// is what every broker test cost before this harness, and what the one or two calls such a
// package makes cost now.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

// seedSchema holds the harness's copy of every table the template has rows in. It lives in the
// template rather than in this process, so every clone carries it from its first page and no
// test pays for taking the snapshot; the fingerprint leaves it out, since it is the harness's
// and not the schema under test.
const seedSchema = "broker_storetest_seed"

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
// wait for. The queue bounds what the harness can be holding, so a package whose tests are
// quicker than a reset keeps at most resetWorkers + resetQueue databases beyond the ones its
// tests hold rather than cloning the server full of them.
const (
	resetWorkers = 4
	resetQueue   = 8
)

var shared struct {
	// main is set by Main before the package's tests run. Without it nothing drops the template
	// or the free list, so every caller gets a database of its own instead (the package comment).
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

	// freeMu guards free, the databases a reset emptied and gave back, and recycling carries the
	// databases their tests are done with to the resetWorkers goroutines that empty them.
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

// serverURL is the DatabaseURLEnv server every database here is made on, skipping t when the
// variable is unset: a run with no server is skipped rather than failed, which is the contract
// URL, Open, OpenFresh and DatabaseURL all state.
func serverURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed broker tests")
	}
	return raw
}

// randomName mints the name of the database one test gets of its own, kind naming what it is in
// a failure. It is random rather than derived from t.Name() because two concurrent runs of one
// test - this package's tests and a sibling package's, against the one server - would otherwise
// mint the same name and act on each other's rows.
func randomName(t testing.TB, kind string) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("random %s name: %v", kind, err)
	}
	return "broker_test_" + suffix
}

func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}

// URL returns the DSN of a database carrying every migration and nothing another test wrote,
// which goes back on the free list once t ends. params are extra connection parameters as
// key=value (e.g. "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	return openURL(t, true, params)
}

// URLFresh returns the DSN of a database cloned for t alone and dropped once t ends; it is
// OpenFresh's DSN, for a caller that opens the database itself or hands it to a process.
func URLFresh(t testing.TB, params ...string) string {
	t.Helper()
	return openURL(t, false, params)
}

// Open is store.Open on a fresh URL(t, params...), closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	return openStore(t, URL(t, params...))
}

// OpenFresh returns a store on a database cloned for t alone and dropped once t ends. Use it for
// a test that must have a database no other test has written into - one that counts what a
// sweep or a migration wrote across the whole of a table, where a row an earlier test left in a
// table the reset does not reach would change the answer - and for one that leaves a session or
// a process of its own on the database past its cleanup, which a database that goes back on the
// free list cannot carry. Every other test uses Open: a clone and a drop each force a checkpoint
// that every package on the server queues on (the package comment).
func OpenFresh(t testing.TB, params ...string) *store.Store {
	t.Helper()
	return openStore(t, URLFresh(t, params...))
}

func openStore(t testing.TB, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// openURL makes the database one caller gets: one off the free list when reusable allows it and
// the list has one, a clone of the template when it does not, and, with no Main to drop either,
// a database of its own that it migrates and drops.
func openURL(t testing.TB, reusable bool, params []string) string {
	t.Helper()
	raw := serverURL(t)
	if !shared.main {
		return ownMigratedURL(t, raw, params)
	}
	shared.once.Do(func() { shared.err = prepare(raw) })
	if shared.err != nil {
		t.Fatalf("prepare the migrated template database: %v", shared.err)
	}
	name := ""
	if reusable {
		name = takeFree()
	}
	if name == "" {
		name = randomName(t, "database")
		if _, err := shared.admin.Exec(context.Background(), cloneStatement(name)); err != nil {
			t.Fatalf("clone a test database from the template: %v", err)
		}
	}
	if reusable {
		t.Cleanup(func() { shared.recycling <- name })
	} else {
		t.Cleanup(func() { dropDatabase(name) })
	}
	return databaseURL(shared.server, name, params...)
}

// ownMigratedURL is what a package with no Main gets: a database of its own, migrated and
// dropped with the test, which is what every broker test cost before the template.
func ownMigratedURL(t testing.TB, raw string, params []string) string {
	t.Helper()
	name := randomName(t, "database")
	admin(t, raw, "create database "+name+" strategy file_copy")
	t.Cleanup(func() { admin(t, raw, "drop database "+name+" with (force)") })
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	dsn := databaseURL(*server, name, params...)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	return dsn
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
		shared.freeMu.Lock()
		shared.free = append(shared.free, name)
		shared.freeMu.Unlock()
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
// hold rows, the template's own rows put back, and every sequence of schema public set where the
// template left it.
//
// It truncates the tables that hold rows rather than all of them because a truncate costs what
// the relations it empties cost, rows or no rows: Postgres gives every table and every index of
// one a new file. Which tables hold rows is one statement asking each of them whether a row
// exists, and it is read from the database rather than from what the test said it wrote, so a
// row a trigger or a background loop of the code under test wrote is in it too. The tables the
// template holds rows in are always among them, since a test may have changed those rows without
// adding a table to the set, and cascade adds whatever references what is truncated.
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
// of that schema, the functions and the triggers of that schema, the schemas the database holds,
// and the settings the database carries, which is where a test that ran alter database ... set
// shows up (they live in pg_db_role_setting, not in pg_database). It leaves out the harness's
// own snapshot schema, which every clone carries and no test may change.
//
// Functions, triggers and column defaults are in it because a test that leaves one behind leaves
// the next test a database that runs code it never wrote, which a truncate does not undo: a
// truncate empties a table, it does not restore what a test altered about one.
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
// drops the template they were cloned from. A package whose tests call URL, Open or OpenFresh
// uses it as its TestMain:
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
//
// It asks for a plain drop first and only forces one that will not go. A forced drop terminates
// every session still on the database, which needs pg_signal_backend, a privilege the server a
// run is pointed at need not grant its test role; and a database the harness drops has had its
// pool closed already, so what a plain drop meets is a backend the server has not finished
// closing rather than a session anything is using. The plain drop is therefore retried for a
// moment, and the force is the last resort for a session a test really left behind.
func dropDatabase(name string) {
	shared.drops.Go(func() {
		var err error
		for deadline := time.Now().Add(dropWait); ; time.Sleep(strangerWait) {
			if _, err = shared.admin.Exec(context.Background(), "drop database "+name); err == nil {
				return
			}
			if time.Now().After(deadline) {
				break
			}
		}
		if _, forced := shared.admin.Exec(context.Background(), "drop database "+name+" with (force)"); forced == nil {
			return
		}
		shared.dropMu.Lock()
		shared.dropFails = append(shared.dropFails, fmt.Sprintf("drop test database %s: %v", name, err))
		shared.dropMu.Unlock()
	})
}

// dropWait is how long a plain drop is retried before the force is tried: long enough for the
// backends of a pool this harness has closed to go, short enough that a run ends promptly.
const dropWait = 5 * time.Second

// prepare opens the admin pool and migrates the template every clone comes from.
func prepare(raw string) error {
	server, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	shared.server = *server
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return fmt.Errorf("parse the admin database URL: %w", err)
	}
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open the admin pool: %w", err)
	}
	shared.admin = admin

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := "broker_template_" + suffix
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create the template database: %w", err)
	}
	shared.template = name
	database, err := store.Open(ctx, databaseURL(shared.server, name))
	if err != nil {
		return fmt.Errorf("open the template database: %w", err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate the template database: %w", err)
	}
	if err := prepareReset(ctx, database.Pool); err != nil {
		return err
	}
	database.Close()
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close the template database to connections: %w", err)
	}
	// The workers start last, once everything a reset reads is built: nothing reaches them
	// before a caller has been handed a database, and the only caller that runs before this
	// returns is the one that called prepare.
	shared.recycling = make(chan string, resetQueue)
	shared.recyclers.Add(resetWorkers)
	for range resetWorkers {
		go recycleWorker()
	}
	return nil
}

// prepareReset reads the migrated template - its tables, the ones the migrations left rows in,
// and its sequences - writes the snapshot every clone carries, and builds the pieces a reset is
// made of and the fingerprint every reused database is held to. It runs on the template, so each
// clone is born with the snapshot and every piece is read from the schema as it is rather than
// from a list written here.
func prepareReset(ctx context.Context, pool *pgxpool.Pool) error {
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
func publicTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select table_name from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE'
		order by table_name`)
	if err != nil {
		return nil, fmt.Errorf("read the template's tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
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
func insertableColumns(ctx context.Context, pool *pgxpool.Pool) (map[string][]string, error) {
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
	found, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
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
func sequenceResets(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
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

// databaseURL is server's DSN pointed at database, carrying params as extra connection
// parameters given as key=value.
func databaseURL(server url.URL, database string, params ...string) string {
	server.Path = "/" + database
	if len(params) > 0 {
		query := server.Query()
		for _, param := range params {
			values, err := url.ParseQuery(param)
			if err != nil {
				panic("storetest: connection parameter " + param + ": " + err.Error())
			}
			for key := range values {
				query.Set(key, values.Get(key))
			}
		}
		server.RawQuery = query.Encode()
	}
	return server.String()
}

// DatabaseURL creates a fresh, UNMIGRATED database on the DatabaseURLEnv server, drops it when t
// ends, and returns its DSN. URL remains the right helper for everything else: it hands out a
// database already carrying every migration. The DSN is unmigrated because that is what both
// callers need: a LISTEN/NOTIFY test, which reads no table at all, and a test that starts two
// broker processes against a database neither has migrated yet, which is the race those two
// processes are there to settle. A caller that wants tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	raw := serverURL(t)
	name := randomName(t, "database")
	// Both statements go through admin, whose bare Exec runs them outside a transaction: a
	// create database cannot run inside one. The drop forces the connections a test left behind
	// (a listener that outlived its own cleanup, a pool closing) off the database, because a
	// database one connection still holds cannot be dropped, and a leaked one would otherwise
	// leave a database behind that no later run can clean up either. The database is a file copy
	// of template1 (docs/testing.md, "A database a Go test creates").
	admin(t, raw, "create database "+name+" strategy file_copy")
	t.Cleanup(func() { admin(t, raw, "drop database "+name+" with (force)") })

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	parsed.Path = "/" + name
	return parsed.String()
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

// Querier is what AwaitLockWaiters polls through: a pool, or a connection of its own when the
// pool's connections may all be waiting.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AwaitLockWaiters waits until n backends wait on a lock behind holder's backend and answers their
// pids, failing t after ten seconds. A backend counts when it waits on a lock (wait_event_type
// 'Lock') that holder's backend holds, or that a backend already counted holds or is ahead of it
// in the queue for: a second waiter for one row waits on the first waiter's tuple lock, and
// pg_blocking_pids names only that first waiter, never the holder. Waiters behind other holders,
// in other tests on the same server, never count.
func AwaitLockWaiters(t testing.TB, q Querier, holder pgx.Tx, n int) []uint32 {
	t.Helper()
	ctx := context.Background()
	pid := holder.Conn().PgConn().PID()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting []int32
		if err := q.QueryRow(ctx, `with recursive waiting(pid) as (
				select pid from pg_stat_activity where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid))
				union
				select a.pid from pg_stat_activity a, waiting w where a.wait_event_type = 'Lock' and w.pid = any(pg_blocking_pids(a.pid))
			)
			select coalesce(array_agg(pid order by pid), '{}') from waiting`, int32(pid)).Scan(&waiting); err != nil {
			t.Fatalf("read the backends waiting behind %d: %v", pid, err)
		}
		if len(waiting) >= n {
			pids := make([]uint32, len(waiting))
			for i, w := range waiting {
				pids[i] = uint32(w)
			}
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waited on a lock behind %d, want %d", len(waiting), pid, n)
		}
	}
}

// Exec is a tamper that runs statements as a writer other than the broker would: $1 is the
// record id wherever a statement names it, and an update must change exactly one row.
func Exec(statements ...string) func(t testing.TB, st *store.Store, recordID string) {
	return func(t testing.TB, st *store.Store, recordID string) {
		t.Helper()
		for _, statement := range statements {
			var args []any
			if strings.Contains(statement, "$1") {
				args = []any{recordID}
			}
			tag, err := st.Pool.Exec(context.Background(), statement, args...)
			if err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
			if strings.HasPrefix(statement, "update") && tag.RowsAffected() != 1 {
				t.Fatalf("%s changed %d rows, want 1", statement, tag.RowsAffected())
			}
		}
	}
}

// ForgeRequestSignature writes a copy of record recordID whose embedded request object carries an
// altered signature, under the id its own body hashes to (the append-only trigger refuses updates,
// not inserts), with an approved event by the record's own approver, and returns the copy's id.
// Every link of that chain but the requester's signature holds, so only the request object's
// re-verification can refuse it.
func ForgeRequestSignature(t testing.TB, st *store.Store, recordID string) string {
	t.Helper()
	body, kind := readRecord(t, st, recordID)
	parts := strings.Split(body.Request, ".")
	if len(parts) != 3 {
		t.Fatalf("request object has %d parts, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 {
		t.Fatalf("decode request object signature: %v", err)
	}
	signature[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	body.Request = strings.Join(parts, ".")
	return insertApprovedCopy(t, st, recordID, body, kind)
}

// CopyAsOtherKind writes a copy of record recordID under the other record kind ('agent_secret'
// for a 'launcher_credential' record, and the reverse), its body differing only in an expiry one
// second later so it hashes to an id of its own, with an approved event by the record's own
// approver, and returns the copy's id. Every link of that chain holds but the kind, so only a
// chain verifier's scope to its own kind can refuse it.
func CopyAsOtherKind(t testing.TB, st *store.Store, recordID string) string {
	t.Helper()
	body, kind := readRecord(t, st, recordID)
	body.ExpiresAt = body.ExpiresAt.Add(time.Second)
	other := "agent_secret"
	if kind == other {
		other = "launcher_credential"
	}
	return insertApprovedCopy(t, st, recordID, body, other)
}

func readRecord(t testing.TB, st *store.Store, recordID string) (record.Body, string) {
	t.Helper()
	var canonical, kind string
	if err := st.Pool.QueryRow(context.Background(), `select body, kind from credential_requests where id=$1`, recordID).Scan(&canonical, &kind); err != nil {
		t.Fatalf("read record %s: %v", recordID, err)
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		t.Fatalf("parse record %s: %v", recordID, err)
	}
	return body, kind
}

// insertApprovedCopy writes body as a record of kind under the id it hashes to, its other columns
// copied from recordID, with an approved event by the body's approver, and returns the copy's id.
func insertApprovedCopy(t testing.TB, st *store.Store, recordID string, body record.Body, kind string) string {
	t.Helper()
	ctx := context.Background()
	copyID := body.ID()
	if _, err := st.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, enrollment_id, code, created_at, expires_at)
		select $2, $3, $4, approver, enrollment_id, code, created_at, expires_at from credential_requests where id=$1`,
		recordID, copyID, body.Canonical(), kind); err != nil {
		t.Fatalf("insert record copy: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, login, actor) values ($1, 'approved', $2, $3)`,
		copyID, body.Approver, record.HumanActor(body.Approver)); err != nil {
		t.Fatalf("insert record copy's approval: %v", err)
	}
	return copyID
}
