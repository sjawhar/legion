// Package storetest gives each Postgres-backed broker test a fully migrated database of its own,
// waits for backends to queue behind a lock a test holds (AwaitLockWaiters), and writes the rows
// only a database writer other than the broker could (Exec, ForgeRequestSignature,
// CopyAsOtherKind), for the tests that pin what a release or an authentication refuses. It is an
// ordinary package rather than a _test.go file so every broker package can import it.
//
// URL clones each database from one migrated template, which the first test process to want it
// builds and every other shares. Migrate applies the whole set inside one transaction under a
// database-wide advisory lock (internal/broker/store), so tests that each migrated a schema of
// their own on one shared database both re-ran every migration and queued on that one lock,
// which was most of the Postgres work a broker test did (dispatch://INFRA-1075). A clone costs
// one create database.
//
// The template is named for the migration set it carries (store.MigrationsFingerprint), built
// under an advisory lock and under a name of its own until it is whole, and never dropped: the
// test processes of one run start minutes apart and a process that dropped it would take it from
// the ones still cloning. A set that gains, loses or edits a migration is a template of its own,
// so a stale one is never cloned - it is only left on the server, which `drop database` by hand
// reclaims on a server that outlives a run (docs/testing.md).
//
// A clone copies the template's files (strategy file_copy), for the reason the Dispatch harness's
// clones do (docs/testing.md, "A database a Go test creates"): Postgres's default from 15 writes
// every page of the template into the write-ahead log instead. A file copy forces a checkpoint
// before the copy and another after it, and the drop at the end of the test forces one more; what
// makes that affordable is the server rather than any machinery here, since every CI job that
// runs a Postgres service runs a container deleted with the job, with fsync, full_page_writes and
// synchronous_commit off.
//
// Nothing is handed from one test to the next: a test's database is dropped once the test ends,
// so no test can be given a database another test wrote in. The drop runs in the background, and
// Main waits for the drops before it returns, so no test database outlives the package: dropping
// a database forces a checkpoint and waits for it, and a test that waited for its own drop spent
// on its clock the time the server's one checkpointer took to reach it.
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to
// the whole server DatabaseURLEnv names, which can hold other databases, and a change to them
// outlives the run.
//
// A package whose tests call URL, Open or OpenFresh uses Main as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }. Main waits for the tests' drops. A
// package that installs none still works: each call makes a database of its own, migrates it and
// drops it with the test, which is what every broker test cost before this harness.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/testpg"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

var shared struct {
	// main is set by Main before the package's tests run. Only Main drops the template and waits
	// for the per-test drops, so a package that installs none takes the ownMigratedURL path.
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

// serverURL is the DatabaseURLEnv server every database here is made on, skipping t when the
// variable is unset: a run with no server is skipped rather than failed, which is the contract
// URL, Open and DatabaseURL all state.
func serverURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed broker tests")
	}
	return raw
}

// randomName mints the name of the database one test gets of its own. It is random rather than
// derived from t.Name() because two concurrent runs of one test - this package's tests and a
// sibling package's, against the one server - would otherwise mint the same name and act on each
// other's rows.
func randomName(t testing.TB) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatal(err)
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

// URL returns the DSN of a database of t's own, carrying every migration and nothing another test
// wrote, dropped when t ends. params are extra connection parameters as key=value (e.g.
// "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	raw := serverURL(t)
	if !shared.main {
		return ownMigratedURL(t, raw, params)
	}
	shared.once.Do(func() { shared.err = prepare(raw) })
	if shared.err != nil {
		t.Fatalf("prepare the migrated template database: %v", shared.err)
	}
	name := randomName(t)
	if _, err := shared.admin.Exec(context.Background(), cloneStatement(name)); err != nil {
		t.Fatalf("clone a test database from the template: %v", err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	return databaseURL(shared.server, name, params...)
}

// URLFresh is URL: every database this harness hands out is one no other test has written in.
func URLFresh(t testing.TB, params ...string) string {
	t.Helper()
	return URL(t, params...)
}

// Open is store.Open on a fresh URL(t, params...), closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), URL(t, params...))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	// The store closes before the drop its own cleanup runs, which t.Cleanup's reverse order
	// gives: a plain drop meeting a session of this process's own waits that session out.
	t.Cleanup(st.Close)
	return st
}

// OpenFresh is Open, for the tests that say in the name what they need of their database: one no
// other test has written in, and one they may leave a session or a process of their own on.
func OpenFresh(t testing.TB, params ...string) *store.Store {
	t.Helper()
	return Open(t, params...)
}

// ownMigratedURL is what a package with no Main gets: a database of its own, migrated and dropped
// with the test, which is what every broker test cost before the template.
func ownMigratedURL(t testing.TB, raw string, params []string) string {
	t.Helper()
	name := randomName(t)
	// The database is a file copy of template1 (docs/testing.md, "A database a Go test creates").
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

// cloneStatement returns the create database that makes name a copy of the template's files
// rather than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name string) string {
	return "create database " + name + " template " + shared.template + " strategy file_copy"
}

// Main runs the package's tests and waits for their databases to drop.
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
// on its own clock (the package comment); drops in flight together share one checkpoint. It
// forces the drop, since a test may have left a session of its own on the database.
func dropDatabase(name string) {
	shared.drops.Go(func() {
		if _, err := shared.admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			shared.dropMu.Lock()
			shared.dropFails = append(shared.dropFails, fmt.Sprintf("drop test database %s: %v", name, err))
			shared.dropMu.Unlock()
		}
	})
}

// prepare opens the admin pool and finds the template every URL clones: the one database named
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
		return fmt.Errorf("parse the admin database URL: %w", err)
	}
	// One create and one drop a test run on this pool, so it is sized for the tests a package
	// runs at once rather than for the drops alone.
	config.MaxConns = 8
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open the admin pool: %w", err)
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
const templatePrefix = "broker_template_"

// buildingPrefix opens the name a template is migrated under, before the rename that gives it the
// name the clones read. A template exists under its own name only once it carries every
// migration, so a build that fails or a process that dies mid-build leaves no half-migrated
// database for the next process to clone.
const buildingPrefix = templatePrefix + "building_"

// templateLockKey is the advisory lock a process takes before it looks for the template or builds
// one. Advisory locks are the cluster's rather than one database's, so every test process on the
// server queues on this one whichever database its admin pool connects to. It differs from the
// Dispatch harness's key because the two harnesses share no template and need not queue on each
// other, and from the key a migration run takes inside the database it migrates.
const templateLockKey int64 = 8330101

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
		return fmt.Errorf("create the template database: %w", err)
	}
	st, err := store.Open(ctx, databaseURL(shared.server, building))
	if err != nil {
		return fmt.Errorf("open the template database: %w", err)
	}
	err = st.Migrate(ctx)
	st.Close()
	if err != nil {
		return fmt.Errorf("migrate the template database: %w", err)
	}
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+building+" with allow_connections false"); err != nil {
		return fmt.Errorf("close the template database to connections: %w", err)
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

// databaseURL is server's DSN pointed at database, carrying params as extra connection parameters
// given as key=value.
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

// PerPodLoginSchema applies by hand the one statement the release after this one migrates:
// dropping launcher_credentials_live_subject, 0015's one-live-credential-per-subject index,
// which migration 0029 deliberately leaves in place so that a roll of the broker serves the
// previous generation and this one against one database (0029's own comment). Until it is
// dropped a subject holds one live credential whatever its pod, which is what this release
// ships; a test that drives the per-pod machine login calls this first, so it runs against the
// schema the next release leaves, which enroll.Service.IssueTokenLoginTx reads for itself.
func PerPodLoginSchema(t testing.TB, st *store.Store) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `drop index if exists launcher_credentials_live_subject`); err != nil {
		t.Fatalf("drop launcher_credentials_live_subject: %v", err)
	}
}

// DatabaseURL creates a fresh, UNMIGRATED database on the DatabaseURLEnv server, drops it when t
// ends, and returns its DSN. URL remains the right helper for everything else: it hands out a
// database already carrying every migration. The DSN is unmigrated because that is what both
// callers need: a LISTEN/NOTIFY test, which reads no table at all, and a test that starts two
// broker processes against a database neither has migrated yet, which is the race those two
// processes are there to settle. A caller that wants tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	return testpg.DatabaseOn(t, serverURL(t), "broker_test")
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
