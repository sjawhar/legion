package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// A set in which two files share a version is refused whole, before the runner touches the
// database. The runner records a migration by its version alone, so a set it accepted would apply
// the first file numbered 2, pass over the second as already applied, and report success.
func TestMigrateRefusesTwoMigrationsSharingAVersionBeforeApplyingAny(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  {Data: []byte("create table guard_first (id integer)")},
		"migrations/0002_second.up.sql": {Data: []byte("create table guard_second (id integer)")},
		"migrations/0002_third.up.sql":  {Data: []byte("create table guard_third (id integer)")},
	}

	err := store.migrate(ctx, set)
	left := existingTables(t, store, "guard_first", "guard_second", "guard_third", "schema_migrations")
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

// A migration whose lock another transaction holds gives up once the runner's lock timeout runs
// out, naming itself, the lock it wanted and the session that held it, and applies nothing.
// Unbounded, it waits for as long as the holder lives, and every read and write of the table
// queues behind its request for that long.
func TestMigrateBoundsTheWaitForALockAnotherTransactionHolds(t *testing.T) {
	lockTimeout := pgmigrate.LockTimeout
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if _, err := store.Pool.Exec(ctx, "create table guard_held (id integer)"); err != nil {
		t.Fatalf("create the held table: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	// ACCESS SHARE is what any read of the table takes, and it conflicts with the ACCESS EXCLUSIVE
	// lock ALTER TABLE asks for.
	if _, err := held.Exec(ctx, "lock table guard_held in access share mode"); err != nil {
		t.Fatalf("hold the table: %v", err)
	}
	set := fstest.MapFS{
		"migrations/0001_widen_held.up.sql": {Data: []byte("alter table guard_held add column wider integer")},
	}

	// The test's own bound on the wait, six times the runner's: a runner that does not bound its
	// lock waits shows up here as a wait this long.
	bound := 6 * lockTimeout
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	started := time.Now()
	err = store.migrate(bounded, set)
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
	var widened bool
	if err := store.Pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns
		where table_name = 'guard_held' and column_name = 'wider')`).Scan(&widened); err != nil {
		t.Fatalf("look for the migration's column: %v", err)
	}
	var recorded bool
	if err := store.Pool.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = 1)").Scan(&recorded); err != nil {
		t.Fatalf("look for the migration's version: %v", err)
	}
	if widened || recorded {
		t.Errorf("the migration that gave up left its column (%t) or its version (%t) behind", widened, recorded)
	}
}

// existingTables reports which of names exist in the store's database.
func existingTables(t *testing.T, store *Store, names ...string) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(),
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
