package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
)

// The broker's set is numbered 1 to N with none missing; pgmigratetest.CheckNumberedOneToN says
// why.
func TestMigrationSetIsNumberedOneToN(t *testing.T) {
	if err := pgmigratetest.CheckNumberedOneToN(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// Every file in the broker's migrations directory reaches the runner;
// pgmigratetest.CheckEmbedsEveryFile says why that needs a test.
func TestEveryMigrationFileIsEmbedded(t *testing.T) {
	if err := pgmigratetest.CheckEmbedsEveryFile(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The census's textual reading of which tables a migration touches is held to the broker's set
// too: every name it finds is a table the set itself creates.
func TestTouchedTablesOfEveryMigrationAreTablesTheSetCreates(t *testing.T) {
	if err := pgmigratetest.CheckTouchedTablesAreKnown(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The broker's runner applies its whole set in one transaction, so a set it accepted with two
// files numbered 2 would apply the first, pass over the second as already applied, and commit.
func TestMigrateRefusesTwoMigrationsSharingAVersionBeforeApplyingAny(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t)
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  {Data: []byte("create table guard_first (id integer)")},
		"migrations/0002_second.up.sql": {Data: []byte("create table guard_second (id integer)")},
		"migrations/0002_third.up.sql":  {Data: []byte("create table guard_third (id integer)")},
	}

	err := s.migrate(ctx, set)
	left := existingTables(t, s, "guard_first", "guard_second", "guard_third", "broker_schema_migrations")
	if err == nil {
		t.Errorf("migrate accepted two migrations numbered 2 and reported success; the tables it left: %v", left)
	} else {
		for _, want := range []string{"0002_second.up.sql", "0002_third.up.sql", "version 2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("migrate error = %q, want it to name %q", err, want)
			}
		}
	}
	if len(left) != 0 {
		t.Errorf("the refused set left %v behind; want nothing applied, not even the version table", left)
	}
}

// A broker migration whose lock another transaction holds gives up once the lock timeout runs
// out, naming itself, the lock and its holder, rather than waiting for as long as the holder lives
// with every broker request on that table queued behind it.
func TestMigrateBoundsTheWaitForALockAnotherTransactionHolds(t *testing.T) {
	lockTimeout := pgmigrate.LockTimeout
	ctx := context.Background()
	s := emptySchemaStore(t)
	if _, err := s.Pool.Exec(ctx, "create table guard_held (id integer)"); err != nil {
		t.Fatalf("create the held table: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, "lock table guard_held in access share mode"); err != nil {
		t.Fatalf("hold the table: %v", err)
	}
	set := fstest.MapFS{
		"migrations/0001_widen_held.up.sql": {Data: []byte("alter table guard_held add column wider integer")},
	}

	bound := 6 * lockTimeout
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	started := time.Now()
	err = s.migrate(bounded, set)
	waited := time.Since(started)
	if bounded.Err() != nil {
		t.Fatalf("migrate waited %s for the held lock, until the test's own %s bound cancelled it (%v): nothing bounds a migration's lock wait", waited.Round(time.Millisecond), bound, err)
	}
	if err == nil {
		t.Fatalf("migrate applied a migration whose lock another transaction held, after %s", waited.Round(time.Millisecond))
	}
	if waited < lockTimeout || waited > lockTimeout+10*time.Second {
		t.Errorf("migrate gave up after %s, want about the %s lock timeout", waited.Round(time.Millisecond), lockTimeout)
	}
	t.Logf("migrate gave up after %s: %v", waited.Round(time.Millisecond), err)
	holderPID := strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	for _, want := range []string{"0001_widen_held.up.sql", "AccessExclusiveLock", "guard_held", "pid " + holderPID, "pg_stat_activity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migrate error = %q, want it to name %q", err, want)
		}
	}
	if left := existingTables(t, s, "broker_schema_migrations"); len(left) != 0 {
		t.Errorf("the migration that gave up committed %v; want its whole transaction rolled back", left)
	}
}

// emptySchemaStore opens a store on a schema of its own on the BROKER_TEST_DATABASE_URL server,
// created empty and dropped when t ends. A test that migrates a set of its own needs one: the
// server's shared schema already records the broker's real versions, which would pass over the
// test set's numbers as applied.
func emptySchemaStore(t *testing.T) *Store {
	t.Helper()
	raw := testDatabaseURL(t)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schema := "broker_migrate_test_" + hex.EncodeToString(suffix[:])
	adminExec(t, raw, "create schema "+schema)
	t.Cleanup(func() { adminExec(t, raw, "drop schema "+schema+" cascade") })
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse BROKER_TEST_DATABASE_URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	s, err := Open(context.Background(), parsed.String())
	if err != nil {
		t.Fatalf("open the test schema: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	return s
}

func adminExec(t *testing.T, databaseURL, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to BROKER_TEST_DATABASE_URL: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// existingTables reports which of names exist on the store's search path.
func existingTables(t *testing.T, s *Store, names ...string) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(),
		"select name from unnest($1::text[]) as name where to_regclass(name) is not null order by name", names)
	if err != nil {
		t.Fatalf("look for tables: %v", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read tables: %v", err)
	}
	return found
}
