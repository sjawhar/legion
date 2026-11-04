// Package storetest gives each Postgres-backed broker test a fully migrated database of its own,
// waits for backends to queue behind a lock a test holds (AwaitLockWaiters), and writes the rows
// only a database writer other than the broker could (Exec, ForgeRequestSignature,
// CopyAsOtherKind), for the tests that pin what a release or an authentication refuses. It is an
// ordinary package rather than a _test.go file so every broker package can import it.
//
// The databases themselves come from internal/pgclone, which this package and Dispatch's
// storetest share: every database is a clone of a template this test process migrates once, a
// test's database is emptied and handed to the next caller rather than dropped, and one a test
// changed beyond its rows is dropped instead. Its package comment says what each of those costs
// and why. What this package decides is the broker's half: the environment variable, the
// migrations, and the snapshot schema's name.
//
// Migrate applies the whole set inside one transaction under a database-wide advisory lock
// (internal/broker/store), so tests that each migrated a schema of their own on one shared
// database both re-ran every migration and queued on that one lock, which is most of the
// Postgres work a broker test did (dispatch://INFRA-1075). A clone costs one create database,
// and emptying one costs one truncate of the tables that hold rows, while creating a database
// and dropping one each force a checkpoint that every package testing against the same server
// queues on.
//
// OpenFresh is for a test that must have a database no other test has written into, or that
// leaves a session or a process of its own on the database past its cleanup.
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to
// the whole server DatabaseURLEnv names, which can hold other databases, and a change to them
// outlives the run.
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
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/pgclone"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

// seedSchema holds the harness's copy of every table the template has rows in. It lives in the
// template rather than in this process, so every clone carries it from its first page and no
// test pays for taking the snapshot; the fingerprint leaves it out, since it is the harness's
// and not the schema under test.
const seedSchema = "broker_storetest_seed"

// shared hands out this package's databases. Main drops what its tests leave.
var shared = pgclone.New(pgclone.Config{
	Name:           "storetest",
	DatabaseURLEnv: DatabaseURLEnv,
	SkipMessage:    DatabaseURLEnv + " must be set to run Postgres-backed broker tests",
	DatabasePrefix: "broker_test_",
	TemplatePrefix: "broker_template_",
	SeedSchema:     seedSchema,
	Migrate:        migrateTemplate,
})

// URL returns the DSN of a database carrying every migration and nothing another test wrote,
// which goes back on the free list once t ends. params are extra connection parameters as
// key=value (e.g. "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	dsn, _ := openURL(t, true, params)
	return dsn
}

// URLFresh returns the DSN of a database cloned for t alone and dropped once t ends; it is
// OpenFresh's DSN, for a caller that opens the database itself or hands it to a process.
func URLFresh(t testing.TB, params ...string) string {
	t.Helper()
	dsn, _ := openURL(t, false, params)
	return dsn
}

// Open is store.Open on a database of t's own, closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	return openStore(t, true, params)
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
	return openStore(t, false, params)
}

// openStore opens the store a caller gets and hands its close to the cleanup that gives the
// database back, which runs it before the database is emptied or dropped.
func openStore(t testing.TB, reusable bool, params []string) *store.Store {
	t.Helper()
	dsn, holding := openURL(t, reusable, params)
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if holding == nil {
		// A package with no Main got a database of its own, which its own cleanup drops; nothing
		// takes it back, so t closes the store.
		t.Cleanup(st.Close)
		return st
	}
	holding.close = st.Close
	return st
}

// held is what a caller opened on the database it was handed. The cleanup that gives the
// database back closes it first, rather than relying on the order two cleanups were registered
// in: a database queued with a session still on it is dropped rather than emptied and reused,
// and a plain drop of one is refused. A URL caller opens the database itself and leaves this
// nil; its own cleanup, registered after the harness's, still runs first.
type held struct{ close func() }

// openURL makes the database one caller gets: one off the free list when reusable allows it and
// the list has one, a clone of the template when it does not, and, with no Main to drop either,
// a database of its own that it migrates and drops.
func openURL(t testing.TB, reusable bool, params []string) (string, *held) {
	t.Helper()
	if !shared.Installed() {
		return ownMigratedURL(t, shared.Server(t), params), nil
	}
	name := shared.Take(t, reusable)
	holding := &held{}
	if reusable {
		t.Cleanup(func() { shared.Release(name, holding.close) })
	} else {
		t.Cleanup(func() {
			if holding.close != nil {
				holding.close()
			}
			shared.Drop(name)
		})
	}
	return shared.URL(name, params...), holding
}

// ownMigratedURL is what a package with no Main gets: a database of its own, migrated and
// dropped with the test, which is what every broker test cost before the template.
func ownMigratedURL(t testing.TB, raw string, params []string) string {
	t.Helper()
	name := shared.RandomName(t)
	admin(t, raw, "create database "+name+" strategy file_copy")
	t.Cleanup(func() { admin(t, raw, "drop database "+name+" with (force)") })
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	dsn := pgclone.DatabaseURL(*server, name, params...)
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

// migrateTemplate migrates the template every clone comes from. The harness reads the migrated
// schema through the store's own pool, so the template is opened the way a test's database is.
func migrateTemplate(ctx context.Context, dsn string) (pgclone.Querier, func(), error) {
	database, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open the template database: %w", err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("migrate the template database: %w", err)
	}
	return database.Pool, database.Close, nil
}

// Main runs the package's tests, waits for the last of their databases to be emptied, drops
// every database its tests left - the free list's and the ones no reset could take back - then
// drops the template they were cloned from. A package whose tests call URL, Open or OpenFresh
// uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return shared.Main(m) }

// DatabaseURL creates a fresh, UNMIGRATED database on the DatabaseURLEnv server, drops it when t
// ends, and returns its DSN. URL remains the right helper for everything else: it hands out a
// database already carrying every migration. The DSN is unmigrated because that is what both
// callers need: a LISTEN/NOTIFY test, which reads no table at all, and a test that starts two
// broker processes against a database neither has migrated yet, which is the race those two
// processes are there to settle. A caller that wants tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	raw := shared.Server(t)
	name := shared.RandomName(t)
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
