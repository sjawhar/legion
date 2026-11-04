// Package pgclone hands each Postgres-backed test of a package a database of its own, cloned
// from one migrated template, and takes it back when the test ends: it empties the database and
// puts it on a free list the next test takes from, or drops it when a test changed more than its
// rows. It is the half of internal/dispatch/store/storetest and internal/broker/store/storetest
// that knows neither schema - every table, column, sequence and constraint it works on is read
// from the catalog of the template at prepare time - so the two harnesses keep their own
// exported surface, their own environment variable and their own migrations, and hand a Pool the
// handful of things that differ (Config).
//
// Every database is a clone of a template the test process migrates once. Running every
// migration is most of the Postgres work a test would otherwise do, so a package's wall time
// would scale with its test count; a clone costs one create database.
//
// A test's database is not dropped when the test ends: the cleanup hands it to one of the pool's
// reset workers, which empties it and puts it on the free list, so a package makes as many
// databases as it has tests in flight rather than one per test, and no test waits for a reset it
// did not cause. Emptying is one truncate, of the tables that hold rows, which writes little;
// creating a database and dropping one each force a checkpoint and wait for it, and every
// package testing against the same server queues on that server's one checkpointer.
//
// A reset is held to the server's clock, not the test's, but it is still server time a package
// pays for once a test: the reset keeps the connection it emptied a database through beside that
// database (recycle), so the database comes back to a session whose statement cache already
// holds the reset's plans, and reads what it compares from pg_catalog rather than from
// information_schema's views (fingerprintQuery, rowProbe).
//
// The template is not empty - the schema_migrations row of every migration, and the rows the
// migrations seeded - so emptying a database has to leave behind what a fresh clone holds:
// prepare snapshots every table of the template that has rows into a schema of the pool's own
// (Config.SeedSchema), which every clone then carries, and the reset copies those rows back and
// puts every sequence of schema public where the template left it.
//
// A database a test changed beyond its rows is dropped rather than put back. The reset compares
// a fingerprint of the database with the template's (fingerprintQuery) and ignores the snapshot
// schema in it. Any difference, a session of the test still connected, or a reset that fails,
// sends the database to the background drops.
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead (docs/testing.md,
// "A database a Go test creates"). The pool changes no Postgres setting: fsync, full_page_writes
// and max_wal_size belong to the whole server, which can hold other databases, and a change to
// them outlives the run.
package pgclone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what the pool reads and writes a database through: the pool a harness migrated the
// template with at prepare, and the one connection a reset holds on the database it is emptying.
// *pgxpool.Pool, *pgx.Conn and Dispatch's *store.Pool all satisfy it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config is everything a harness's own schema and name decide. The pool holds nothing else of
// either harness: it reads the tables, columns, sequences and constraints it works on from the
// template the harness migrated.
type Config struct {
	// Name prefixes every line Main writes to standard error, and is the harness package's own
	// name, since that is the word a reader of a failing run greps for.
	Name string

	// DatabaseURLEnv names the Postgres server the harness's tests run against, and SkipMessage
	// is what a test is skipped with when it is unset. The variable's database path is ignored:
	// the admin connection uses the server's postgres database, and every test gets a database
	// of its own.
	DatabaseURLEnv string
	SkipMessage    string

	// DatabasePrefix opens the name of every database a test is handed, TemplatePrefix the
	// template's, so two harnesses on one server never meet.
	DatabasePrefix string
	TemplatePrefix string

	// SeedSchema is the schema the template's own rows are snapshotted into. It lives in the
	// template rather than in the test process, so every clone carries it from its first page
	// and no test pays for taking the snapshot; the fingerprint leaves it out, since it is the
	// pool's and not the schema under test.
	SeedSchema string

	// Migrate opens the template database at dsn and migrates it. It hands back the querier the
	// pool reads the migrated schema and writes the snapshot through, and the close of the
	// connections it opened, which the pool calls before it refuses the template new ones.
	Migrate func(ctx context.Context, dsn string) (template Querier, close func(), err error)
}

// resetLockTimeout bounds how long a reset waits for a table's lock. A reset that waits is a
// session the test left behind holding one, which is reason to drop the database rather than
// reason to wait: the test has ended and nothing is coming to release it.
const resetLockTimeout = "10s"

// strangerWait and strangerTries bound how long a reset waits for the client backends of the
// pool the test just closed to go. A closed pool's backends exit on their own, a moment after
// the socket closes, and any client session still on the database past that wait is a test's
// leak: the reset counts it and the database is dropped rather than put back.
const (
	strangerWait  = 20 * time.Millisecond
	strangerTries = 10
)

// dropWait is how long a plain drop is retried before the force is tried, and dropRetry how long
// it waits between tries: long enough for the backends of a pool this harness has closed to go,
// short enough that a run ends promptly.
const (
	dropWait  = 5 * time.Second
	dropRetry = 20 * time.Millisecond
)

// maxResetWorkers bounds how many databases are emptied at once, and defaultParallelism is how
// many tests the pool assumes run at once when it cannot read go test's own -parallel. The
// workers and the queue behind them are sized per package from that number (resetSizes): a
// package's cleanup hands its database to a worker rather than emptying it itself, since the
// reset's truncate is a transaction of its own that the test which follows has no reason to wait
// for, and the queue bounds what the pool can be holding - a package whose tests are quicker
// than a reset keeps at most workers + queue databases beyond the ones its tests hold, and the
// cleanup after that waits for a worker rather than cloning the server full of databases.
const (
	maxResetWorkers    = 4
	defaultParallelism = 4
)

// Pool hands out the databases of one harness. A package holds one, as a package-level variable:
// its tests run in one process and will run at the same time (t.Parallel), so every field below
// that a test reaches is guarded.
type Pool struct {
	config Config

	// installed is set by Main before the package's tests run. Only Main drops the template and
	// the free list and waits for the per-test drops, so a harness whose package installed no
	// Main does something else (Installed).
	installed bool
	once      sync.Once
	err       error
	server    url.URL
	admin     *pgxpool.Pool
	template  string

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
	fingerprint string
	restores    []string
	sequences   []string

	// freeMu guards free, the databases a reset emptied and gave back, and recycling carries the
	// databases their tests are done with to the reset workers that empty them.
	freeMu    sync.Mutex
	free      []string
	recycling chan string
	recyclers sync.WaitGroup

	// keptMu guards kept, the connection each database was last reset through, held open for the
	// next reset of that database (recycle), and keptHeld, those databases oldest session first,
	// which is the one keptConns closes when the pool holds too many.
	keptMu   sync.Mutex
	kept     map[string]*pgx.Conn
	keptHeld []string

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string

	// resetMu guards what Main reports about the databases no reset put back: a reset that could
	// not run at all is worth a line, since a harness that never reuses a database is as slow as
	// one that drops every database, and so is the count of the databases a test changed the
	// schema of, which are dropped rather than reused by design.
	resetMu    sync.Mutex
	resetFails int
	resetFirst string
	changed    int
}

// New returns the pool a harness hands its tests databases from. It opens nothing: the first
// test to ask for a database migrates the template.
func New(config Config) *Pool {
	return &Pool{config: config, kept: map[string]*pgx.Conn{}}
}

// Installed reports whether Main runs this process's tests, and so whether anything drops the
// template and the free list at the end of the run.
func (p *Pool) Installed() bool { return p.installed }

// Server is the Postgres server every database of this pool is made on, as the harness's
// environment variable spells it, skipping t when the variable is unset: a run with no server is
// skipped rather than failed.
func (p *Pool) Server(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(p.config.DatabaseURLEnv)
	if raw == "" {
		t.Skip(p.config.SkipMessage)
	}
	return raw
}

// Take hands t a database of its own and answers its name: one off the free list when reusable
// allows it and the list has one, a clone of the template when it does not. The caller gives it
// back with Release, which empties it for the next test, or with Drop.
func (p *Pool) Take(t testing.TB, reusable bool) string {
	t.Helper()
	raw := p.Server(t)
	p.once.Do(func() { p.err = p.prepare(raw) })
	if p.err != nil {
		t.Fatalf("prepare the migrated template database: %v", p.err)
	}
	name := ""
	if reusable {
		name = p.takeFree()
	}
	if name == "" {
		name = p.RandomName(t)
		if _, err := p.admin.Exec(context.Background(), p.CloneStatement(name)); err != nil {
			t.Fatalf("clone a test database from the template: %v", err)
		}
	}
	return name
}

// URL is the DSN of one of this pool's databases, carrying params as extra connection parameters
// given as key=value.
func (p *Pool) URL(database string, params ...string) string {
	return DatabaseURL(p.server, database, params...)
}

// RandomName mints the name of a database one test gets of its own. It is random rather than
// derived from t.Name() because two concurrent runs of one test - this package's tests and a
// sibling package's, against the one server - would otherwise mint the same name and act on each
// other's rows.
func (p *Pool) RandomName(t testing.TB) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return p.config.DatabasePrefix + suffix
}

// Template is the database every clone is made from, and Admin the pool every create database
// and drop database runs on. A harness's own test measures what one clone writes, which needs
// the template's size and the clone on one backend.
func (p *Pool) Template() string     { return p.template }
func (p *Pool) Admin() *pgxpool.Pool { return p.admin }

// CloneStatement returns the create database that makes name a copy of the template's files
// rather than a copy through the write-ahead log (the package comment says why).
func (p *Pool) CloneStatement(name string) string {
	return "create database " + name + " template " + p.template + " strategy file_copy"
}

// Release hands the database a test is done with to a reset worker, which empties it and puts it
// back on the free list, or drops it when it is not the template's copy any more. close is what
// the test held open on the database and is closed here, before the database is queued, rather
// than left to the order of the caller's cleanups: a reset that still finds a client session on
// the database drops it rather than reusing it. It waits only when every worker is busy and the
// queue behind them is full.
func (p *Pool) Release(name string, close func()) {
	if close != nil {
		close()
	}
	p.recycling <- name
}

// Drop drops name without waiting for it; Main waits for every drop. Drops in flight together
// share one checkpoint.
//
// It asks for a plain drop first and only forces one that will not go. A forced drop terminates
// every session still on the database, which needs pg_signal_backend, a privilege the server a
// run is pointed at need not grant its test role; and a database the pool drops has had what
// held it closed already, so what a plain drop meets is a backend the server has not finished
// closing rather than a session anything is using. The plain drop is therefore retried for a
// moment, and the force is the last resort for a session a test really left behind.
func (p *Pool) Drop(name string) {
	// The reset's own connection to this database goes first, and before the goroutine starts:
	// it is this process's session, and a plain drop would meet it.
	p.closeKept(name)
	p.drops.Go(func() {
		var err error
		for deadline := time.Now().Add(dropWait); ; time.Sleep(dropRetry) {
			if _, err = p.admin.Exec(context.Background(), "drop database "+name); err == nil {
				return
			}
			if time.Now().After(deadline) {
				break
			}
		}
		if _, forced := p.admin.Exec(context.Background(), "drop database "+name+" with (force)"); forced != nil {
			// Both errors: on a server that grants no pg_signal_backend the force is refused for
			// a reason of its own, which the plain drop's error does not name.
			p.dropMu.Lock()
			p.dropFails = append(p.dropFails,
				fmt.Sprintf("drop test database %s: %v; forcing it: %v", name, err, forced))
			p.dropMu.Unlock()
		}
	})
}

// Main runs the package's tests, waits for the last of their databases to be emptied, drops
// every database its tests left - the free list's and the ones no reset could take back - then
// drops the template they were cloned from. A package whose tests take a database from this pool
// uses the harness's Main as its TestMain.
func (p *Pool) Main(m *testing.M) int {
	p.installed = true
	code := m.Run()
	if p.admin == nil {
		return code
	}
	defer p.admin.Close()
	// A prepare that failed before it started the workers left no queue to close.
	if p.recycling != nil {
		close(p.recycling)
		p.recyclers.Wait()
	}
	for name := p.takeFree(); name != ""; name = p.takeFree() {
		p.Drop(name)
	}
	p.drops.Wait()
	if p.changed > 0 {
		fmt.Fprintf(os.Stderr, "%s: dropped %d database(s) whose schema a test changed\n", p.config.Name, p.changed)
	}
	if p.resetFails > 0 {
		fmt.Fprintf(os.Stderr, "%s: dropped %d database(s) whose reset failed; the first: %s\n",
			p.config.Name, p.resetFails, p.resetFirst)
	}
	for _, failure := range p.dropFails {
		fmt.Fprintf(os.Stderr, "%s: %s\n", p.config.Name, failure)
		if code == 0 {
			code = 1
		}
	}
	if p.template == "" {
		return code
	}
	if _, err := p.admin.Exec(context.Background(), "drop database "+p.template); err != nil {
		fmt.Fprintf(os.Stderr, "%s: drop template database %s: %v\n", p.config.Name, p.template, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// takeFree takes a database off the free list, or returns "" when the list is empty.
func (p *Pool) takeFree() string {
	p.freeMu.Lock()
	defer p.freeMu.Unlock()
	if len(p.free) == 0 {
		return ""
	}
	// The last database back is the one Postgres and the page cache hold warmest.
	name := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return name
}

// putFree puts an emptied database back for the next test.
func (p *Pool) putFree(name string) {
	p.freeMu.Lock()
	defer p.freeMu.Unlock()
	p.free = append(p.free, name)
}

// recycleWorker empties the databases their tests are done with, one at a time, until Main closes
// the queue.
func (p *Pool) recycleWorker() {
	defer p.recyclers.Done()
	for name := range p.recycling {
		reusable, err := p.recycle(context.Background(), name)
		if err != nil {
			p.noteResetFailure(name, err)
		}
		if !reusable {
			p.Drop(name)
			continue
		}
		p.putFree(name)
	}
}

// recycle empties name and reports whether it may serve another test.
//
// It works through the connection this pool last reset that database through, kept open since
// (takeConn): a database on the free list comes back to the session that emptied it, so the
// fingerprint and the row probe are planned once per database rather than once per reset, and no
// reset pays for a backend of its own - 216 ms on this repository's pod. That session is the one
// asking, which the stranger count excludes by pid, so it changes nothing the reset checks. It
// is an optimisation and nothing rests on it: a reset that failed because the session is gone -
// the server bounced it, or another test under the same role terminated it - is run once more on
// a connection of its own.
func (p *Pool) recycle(ctx context.Context, name string) (bool, error) {
	reusable, err := p.reset(ctx, name)
	if err == nil || !p.keptIsClosed(name) {
		return reusable, err
	}
	p.closeKept(name)
	return p.reset(ctx, name)
}

// reset is one attempt at emptying name. The stranger count is what the wait repeats, and the
// fingerprint is read once, after the wait: the backends of the pool the test just closed are
// still exiting when the database arrives, and a fingerprint read beside them is a reading of a
// database somebody still holds.
func (p *Pool) reset(ctx context.Context, name string) (bool, error) {
	conn, err := p.takeConn(ctx, name)
	if err != nil {
		return false, err
	}
	// The session is out of the pool's hands while this reset runs and goes back when it ends,
	// so neither keptConns' eviction nor a drop can close it under the statements below.
	defer p.keepConn(name, conn)
	strangers, err := p.strangers(ctx, conn)
	if err != nil {
		return false, err
	}
	for try := 0; strangers > 0 && try < strangerTries; try++ {
		time.Sleep(strangerWait)
		if strangers, err = p.strangers(ctx, conn); err != nil {
			return false, err
		}
	}
	if strangers > 0 {
		p.noteChanged()
		return false, nil
	}
	fingerprint, err := p.inspect(ctx, conn)
	if err != nil {
		return false, err
	}
	if fingerprint != p.fingerprint {
		p.noteChanged()
		return false, nil
	}
	// The probe runs only once the fingerprint has matched: it names each table of the schema,
	// and on a database a test dropped a table of it is the fingerprint's answer that decides,
	// not an error from a statement about a table that is gone.
	dirty, err := p.tablesWithRows(ctx, conn)
	if err != nil {
		return false, err
	}
	if _, err := conn.Exec(ctx, p.resetStatements(dirty)); err != nil {
		return false, fmt.Errorf("reset the database: %w", err)
	}
	return true, nil
}

// keptConns bounds how many of those sessions the pool holds at once. Each is a backend of the
// server for as long as its database lives, and a test refused a connection is a failed test:
// under -parallel 12 the api package held 24 databases, and the slots their idle sessions took
// were enough for a test to be answered "remaining connection slots are reserved" (SQLSTATE
// 53300) on a server whose max_connections is PostgreSQL's default 100. Past the bound the
// session held longest is closed, and the database it belonged to pays for a connection at its
// next reset. A package whose tests run four at a time keeps every database it has warm; a
// server with connections to spare is where this constant goes up.
const keptConns = 2 * maxResetWorkers

// takeConn is the connection name's reset runs on, taken out of the sessions the pool holds for
// as long as the reset: the one the last reset of that database used, or a new one when this is
// the database's first reset, when keptConns closed the last, or when the server did.
func (p *Pool) takeConn(ctx context.Context, name string) (*pgx.Conn, error) {
	p.keptMu.Lock()
	kept := p.kept[name]
	p.forget(name)
	p.keptMu.Unlock()
	if kept != nil {
		if !kept.IsClosed() {
			return kept, nil
		}
		_ = kept.Close(ctx)
	}
	config, err := pgx.ParseConfig(p.URL(name))
	if err != nil {
		return nil, fmt.Errorf("parse the database URL: %w", err)
	}
	config.RuntimeParams["lock_timeout"] = resetLockTimeout
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect to reset the database: %w", err)
	}
	return conn, nil
}

// keepConn gives the reset's session back for the next reset of that database, and closes the
// session the pool has held longest when that leaves it holding more than keptConns. A session
// the server ended goes back too, closed: the next reset of that database finds it so and
// recycle reads it as the reason to try again.
func (p *Pool) keepConn(name string, conn *pgx.Conn) {
	p.keptMu.Lock()
	p.forget(name)
	p.kept[name] = conn
	p.keptHeld = append(p.keptHeld, name)
	var evicted *pgx.Conn
	if len(p.keptHeld) > keptConns {
		oldest := p.keptHeld[0]
		evicted = p.kept[oldest]
		p.forget(oldest)
	}
	p.keptMu.Unlock()
	if evicted != nil {
		_ = evicted.Close(context.Background())
	}
}

// keptIsClosed reports whether the session this pool holds on name has ended under it.
func (p *Pool) keptIsClosed(name string) bool {
	p.keptMu.Lock()
	defer p.keptMu.Unlock()
	conn := p.kept[name]
	return conn != nil && conn.IsClosed()
}

// closeKept ends the connection this pool holds on name, if it holds one. Drop calls it before
// it drops the database: a database whose session this process still holds cannot be dropped
// without the force.
func (p *Pool) closeKept(name string) {
	p.keptMu.Lock()
	conn := p.kept[name]
	p.forget(name)
	p.keptMu.Unlock()
	if conn != nil {
		_ = conn.Close(context.Background())
	}
}

// forget drops name from the held sessions without closing it. The caller holds keptMu.
func (p *Pool) forget(name string) {
	delete(p.kept, name)
	for at, held := range p.keptHeld {
		if held == name {
			p.keptHeld = append(p.keptHeld[:at], p.keptHeld[at+1:]...)
			return
		}
	}
}

// strangers is how many client sessions other than this one are on the database conn is on. It is
// its own statement, and the only one the wait below repeats, because it is the only thing that
// changes while a closed pool's backends exit.
//
// Only client backends count. A worker Postgres itself runs - above all an autovacuum of a
// database that has just taken thousands of inserts, which is exactly what a test leaves behind -
// carries the database's name in pg_stat_activity too, and counting it waited out every try and
// then dropped a database nothing was wrong with.
const strangerQuery = `
	select count(*) from pg_stat_activity
	where datname = current_database() and pid <> pg_backend_pid() and backend_type = 'client backend'`

func (p *Pool) strangers(ctx context.Context, conn Querier) (int, error) {
	var strangers int
	if err := conn.QueryRow(ctx, strangerQuery).Scan(&strangers); err != nil {
		return 0, fmt.Errorf("read the sessions on the database: %w", err)
	}
	return strangers, nil
}

// resetStatements is the reset a database that has passed the fingerprint runs: one truncate of
// the tables dirty names, the template's own rows put back, and every sequence of schema public
// set where the template left it.
//
// It truncates the tables that hold rows rather than every table because a truncate costs what
// the relations it empties cost, rows or no rows: Postgres gives every table and every index of
// one a new file, and on Dispatch's schema that is about 0.6 ms a relation under gVisor, 200 ms
// for the schema against 5 ms for a test that wrote nothing. Which tables hold rows is one
// statement (rowProbe), and it is read from the database rather than from what the test said it
// wrote, so a row a trigger or a background loop of the code under test wrote is in it too. The
// tables the template holds rows in are always among them, since a test may have changed those
// rows without adding a table to the set, and cascade adds whatever references what is truncated.
func (p *Pool) resetStatements(dirty []int) string {
	clear := make(map[int]bool, len(p.seeded)+len(dirty))
	for _, at := range p.seeded {
		clear[at] = true
	}
	for _, at := range dirty {
		clear[at] = true
	}

	tables := make([]string, 0, len(clear))
	for at, quoted := range p.quoted {
		if clear[at] {
			tables = append(tables, quoted)
		}
	}
	// One Exec of the lot is one round trip and, under the simple protocol, one implicit
	// transaction: a database whose reset fails part way through is never put back.
	//
	// The analyze is what makes a reused database plan like a fresh clone. A truncate resets
	// pg_class.reltuples but leaves pg_statistic describing the rows the test before wrote, so
	// a test whose claim is about a plan - how many rows a page reads, which index a query
	// takes - reads one answer on a clone of the template and another on a database recycled
	// from a test that seeded and analysed a large table. Measured in internal/dispatch/api:
	// a page read 8 events rows on a clone and 10 on a database the 200,000-event relay test
	// had used, with the same nine rows in its window. It costs what it scans and these tables
	// have just been emptied, so it is the cheap half of the statement, not the dear one; a
	// test that wrote nothing truncates nothing and analyses nothing. It runs last, after the
	// template's own rows are back, so the statistics describe what the next test will read.
	statements := make([]string, 0, 2+len(p.restores)+len(p.sequences))
	joined := strings.Join(tables, ", ")
	if len(tables) > 0 {
		statements = append(statements, "truncate table "+joined+" restart identity cascade")
	}
	statements = append(statements, p.restores...)
	statements = append(statements, p.sequences...)
	if len(tables) > 0 {
		statements = append(statements, "analyze "+joined)
	}
	return strings.Join(statements, ";\n")
}

// noteResetFailure records a reset Main reports, and counts the rest.
func (p *Pool) noteResetFailure(name string, err error) {
	p.resetMu.Lock()
	defer p.resetMu.Unlock()
	p.resetFails++
	if p.resetFirst == "" {
		p.resetFirst = fmt.Sprintf("%s: %v", name, err)
	}
}

// noteChanged counts a database no reset was run on: its schema is not the template's any more,
// or a session of the test is still on it. Main reports the count, which is what a package pays
// for the schema its tests change: every one of these is a clone and a drop the tests that
// follow queue behind two forced checkpoints for.
func (p *Pool) noteChanged() {
	p.resetMu.Lock()
	defer p.resetMu.Unlock()
	p.changed++
}

// inspect reads the database's fingerprint, on a reset worker's clock rather than any test's.
func (p *Pool) inspect(ctx context.Context, conn Querier) (string, error) {
	var fingerprint string
	if err := conn.QueryRow(ctx, p.fingerprintQuery()).Scan(&fingerprint); err != nil {
		return "", fmt.Errorf("read the database's schema fingerprint: %w", err)
	}
	return fingerprint, nil
}

// fingerprintQuery answers what a reused database must still share with the template: the columns
// of schema public with their types, nullability and defaults, every relation of that schema -
// its sequences and indexes with its tables - the constraints, functions and triggers of that
// schema, the schemas the database holds, and the settings the database carries, which is where a
// test that ran alter database ... set search_path shows up (they live in pg_db_role_setting, not
// in pg_database: PostgreSQL moved them there). It leaves out the pool's own snapshot schema,
// which every clone carries and no test may change.
//
// It reads pg_catalog rather than information_schema, which is a set of views joining five
// catalogs and running a privilege check per column: on Dispatch's 94-table schema it measured
// 356 ms against 891 ms on a connection of its own and 44 ms against 52 ms on a kept one, and it
// is stricter rather than looser, since format_type carries the length information_schema's
// data_type drops. The relation branch already names every table of the schema, so no branch of
// its own lists them.
//
// Functions and triggers are in it because a test that leaves one behind leaves the next test a
// database that runs code it never wrote: a trigger on artifact_versions one api test added made
// an unrelated create-issue answer 500 in another, until the fingerprint saw it and the database
// was dropped rather than reused.
//
// A column's default is in it for the same reason, measured: the broadcast tests point
// messages.id at a function of their own that hands out eight fixed identifiers
// (installReverseBroadcastMessageIDDefault), and a database put back with that default still on
// it made 67 later tests of the api package fail on a duplicate messages_pkey in CI run
// 41729266538. A truncate empties a table; it does not restore what a test altered about one.
//
// A constraint is in it because nothing else here sees one: a check or a foreign key creates no
// relation, changes no column row and is no trigger, and a truncate does not remove it. The
// architecture importer's tests add a check constraint that refuses a title, and without this
// term the next test to take that database was handed a schema that refuses rows it never wrote.
//
// The schemas leave out pg_temp_N and pg_toast_temp_N: the schemas of a temporary table stay in
// the catalog after the session that made them ends, so one temporary table would otherwise make
// a database unreusable for the rest of the run, and silently, since a dropped database is silent
// by design.
func (p *Pool) fingerprintQuery() string {
	return `
select coalesce((
	select md5(string_agg(entry, E'\n' order by entry))
	from (
		select format('column|%s|%s|%s|%s|%s|%s', table_of.relname, column_of.attname,
			column_of.attnum, format_type(column_of.atttypid, column_of.atttypmod),
			column_of.attnotnull, coalesce(pg_get_expr(default_of.adbin, default_of.adrelid), '')) as entry
			from pg_attribute column_of
			join pg_class table_of on table_of.oid = column_of.attrelid
			left join pg_attrdef default_of
				on default_of.adrelid = column_of.attrelid and default_of.adnum = column_of.attnum
			where table_of.relnamespace = 'public'::regnamespace
				and table_of.relkind in ('r', 'p', 'v', 'm', 'f')
				and column_of.attnum > 0 and not column_of.attisdropped
		union all
		select format('relation|%s|%s', relation.relname, relation.relkind)
			from pg_class relation where relation.relnamespace = 'public'::regnamespace
		union all
		select format('constraint|%s|%s', held.conname, pg_get_constraintdef(held.oid))
			from pg_constraint held where held.connamespace = 'public'::regnamespace
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
			from pg_namespace where nspname <> '` + p.config.SeedSchema + `'
				and nspname not like 'pg\_temp\_%' and nspname not like 'pg\_toast\_temp\_%'
		union all
		select format('setting|%s', array_to_string(setting.setconfig, ','))
			from pg_db_role_setting setting
			join pg_database database on database.oid = setting.setdatabase
			where database.datname = current_database()
	) entries
), '')`
}

// prepare opens the admin pool, makes the template every clone comes from, has the harness
// migrate it, and starts the reset workers.
func (p *Pool) prepare(raw string) error {
	server, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", p.config.DatabaseURLEnv, err)
	}
	p.server = *server
	ctx := context.Background()
	// The admin pool is on the server's postgres database: neither create database nor drop
	// database may run against the database it names, so the variable's own database path is
	// ignored.
	config, err := pgxpool.ParseConfig(DatabaseURL(p.server, "postgres"))
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
	p.admin = admin

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := p.config.TemplatePrefix + suffix
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create the template database: %w", err)
	}
	p.template = name
	template, close, err := p.config.Migrate(ctx, DatabaseURL(p.server, name))
	if err != nil {
		return err
	}
	defer close()
	if err := p.prepareReset(ctx, template); err != nil {
		return err
	}
	close()
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close the template database to connections: %w", err)
	}
	// The workers start last, once everything a reset reads is built: nothing reaches them before
	// a test has been handed a database, and the only caller that runs before this returns is the
	// one that called prepare.
	workers, queue := resetSizes()
	p.recycling = make(chan string, queue)
	p.recyclers.Add(workers)
	for range workers {
		go p.recycleWorker()
	}
	return nil
}

// resetSizes is how many databases this package empties at once and how many wait behind them.
// Both follow the package's own parallelism, which is what bounds how many databases it can have
// in flight: go test parses its flags inside M.Run, and a harness's Main calls that before the
// first test asks for a database, so -parallel and a package's own flag.Set are both readable
// here. A package whose tests do not overlap much holds a handful of databases rather than the
// dozen a fixed queue held, and one that runs twelve tests at once does not block a test's
// cleanup behind a queue sized for four.
func resetSizes() (workers, queue int) {
	parallelism := defaultParallelism
	if parsed := flag.Lookup("test.parallel"); parsed != nil {
		if value, ok := parsed.Value.(flag.Getter); ok {
			if set, ok := value.Get().(int); ok && set > 0 {
				parallelism = set
			}
		}
	}
	return min(parallelism, maxResetWorkers), parallelism
}

// prepareReset reads the migrated template - its tables, the ones the migrations left rows in,
// and its sequences - writes the snapshot every clone carries, and builds the pieces a reset is
// made of and the fingerprint every reused database is held to. It runs on the template, so each
// clone is born with the snapshot and every piece is read from the schema as it is rather than
// from a list written here.
func (p *Pool) prepareReset(ctx context.Context, template Querier) error {
	tables, err := publicTables(ctx, template)
	if err != nil {
		return err
	}
	columns, err := insertableColumns(ctx, template)
	if err != nil {
		return err
	}
	p.quoted = make([]string, len(tables))
	for i, table := range tables {
		p.quoted[i] = pgx.Identifier{"public", table}.Sanitize()
	}
	p.dirty = rowProbe(p.quoted)
	seeded, err := p.tablesWithRows(ctx, template)
	if err != nil {
		return err
	}
	p.seeded = seeded

	if _, err := template.Exec(ctx, "create schema "+pgx.Identifier{p.config.SeedSchema}.Sanitize()); err != nil {
		return fmt.Errorf("create the template's snapshot schema: %w", err)
	}
	for _, at := range seeded {
		table := tables[at]
		of := columns[table]
		if len(of) == 0 {
			return fmt.Errorf("the template's table %s holds rows in no column a snapshot can insert", table)
		}
		list := strings.Join(of, ", ")
		source := pgx.Identifier{p.config.SeedSchema, table}.Sanitize()
		target := p.quoted[at]
		if _, err := template.Exec(ctx, "create table "+source+" as select "+list+" from "+target); err != nil {
			return fmt.Errorf("snapshot the template's %s rows: %w", table, err)
		}
		// overriding system value is what lets the rows of a table whose identity column is
		// generated always go back in with the values the template gave them. Postgres accepts
		// the clause on a table that has no identity column at all, so every restore carries it.
		p.restores = append(p.restores,
			"insert into "+target+" ("+list+") overriding system value select "+list+" from "+source)
	}
	if p.sequences, err = sequenceResets(ctx, template); err != nil {
		return err
	}
	if err := template.QueryRow(ctx, p.fingerprintQuery()).Scan(&p.fingerprint); err != nil {
		return fmt.Errorf("read the template's schema fingerprint: %w", err)
	}
	return nil
}

// publicTables names every table of schema public, read from the catalog so that a migration
// adding a table needs no change here.
func publicTables(ctx context.Context, template Querier) ([]string, error) {
	rows, err := template.Query(ctx, `
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
func insertableColumns(ctx context.Context, template Querier) (map[string][]string, error) {
	rows, err := template.Query(ctx, `
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

// rowProbe is the statement tablesWithRows runs: it asks the catalog which of the tables holds a
// page at all, and answers with the table's position in the pool's quoted names, so no name is
// read back out of the statement.
//
// It is one plan over a values list rather than one exists subquery per table, and
// pg_relation_size reads what the catalog already holds instead of scanning each table for its
// first row: 513 ms against 900 ms on a connection of its own and 9.8 ms against 22 ms on a kept
// one. A table that holds a row always holds a page, so no table the reset must empty is missed.
// A table whose rows were deleted or rolled back keeps its pages until it is vacuumed and is
// truncated here for nothing, which costs the one relation it empties and is what the reset
// would do anyway for a table the template seeded.
func rowProbe(quoted []string) string {
	probes := make([]string, len(quoted))
	for i, table := range quoted {
		probes[i] = "(" + strconv.Itoa(i) + ", '" + strings.ReplaceAll(table, "'", "''") + "'::regclass)"
	}
	return "select at from (values " + strings.Join(probes, ", ") +
		") as probe(at, held) where pg_relation_size(held) > 0"
}

// tablesWithRows gives the positions in the pool's quoted names of the tables that hold rows: on
// the template, the tables the migrations seeded; on a database a test is done with, the tables
// the reset has to empty.
func (p *Pool) tablesWithRows(ctx context.Context, from Querier) ([]int, error) {
	rows, err := from.Query(ctx, p.dirty)
	if err != nil {
		return nil, fmt.Errorf("read which tables hold rows: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("read which tables hold rows: %w", err)
	}
	return found, nil
}

// sequenceResets returns one setval per sequence of schema public, putting it where the template
// left it. pg_sequences answers a sequence nextval has never been called on with a null
// last_value, which is the state setval's third argument writes as false.
func sequenceResets(ctx context.Context, template Querier) ([]string, error) {
	rows, err := template.Query(ctx, `
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

// DatabaseURL is server's DSN pointed at database, carrying params as extra connection parameters
// given as key=value.
func DatabaseURL(server url.URL, database string, params ...string) string {
	server.Path = "/" + database
	if len(params) > 0 {
		query := server.Query()
		for _, param := range params {
			values, err := url.ParseQuery(param)
			if err != nil {
				panic("pgclone: connection parameter " + param + ": " + err.Error())
			}
			for key := range values {
				query.Set(key, values.Get(key))
			}
		}
		server.RawQuery = query.Encode()
	}
	return server.String()
}

// randomSuffix is the 8 random bytes every name this package mints ends in.
func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}
