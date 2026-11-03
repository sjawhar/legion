package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// withOwedTrigger is censusBase plus a pending 0002 that adds a row trigger to things: a function and
// a create trigger, which takes SHARE ROW EXCLUSIVE on things and so waits behind an open write.
func withOwedTrigger() fstest.MapFS {
	return fstest.MapFS{
		"migrations/0001_things.up.sql": censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_owed.up.sql": {Data: []byte(
			"create function owe() returns trigger language plpgsql as $$ begin return null; end $$;\n" +
				"create trigger things_owed after insert on things for each row execute function owe();")},
	}
}

// holdWrite opens a connection of its own and leaves a transaction open that has written to
// things, holding ROW EXCLUSIVE on it, as a request that writes and has not committed does. It
// returns the transaction and the holder's backend pid.
func holdWrite(t *testing.T, store *Store) (pgx.Tx, uint32) {
	t.Helper()
	ctx := context.Background()
	holder, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Rollback(context.Background()) })
	if _, err := held.Exec(ctx, "insert into things (id, kind) values (1, 'written')"); err != nil {
		t.Fatal(err)
	}
	return held, holder.PgConn().PID()
}

// A migration queued behind an open write gives up at its lock_timeout, rolls back, holding
// nothing while it pauses, and tries again; once the writer commits, the next attempt applies it.
// The timed-out attempt is logged with the lock, the writer and the writer's diagnosis.
func TestMigrateTriesALockTimeoutAgainAndAppliesOnceTheWriterCommits(t *testing.T) {
	ctx := context.Background()
	store, _ := migratedToOne(t)
	logs := captureLogs(t)
	held, pid := holdWrite(t, store)
	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(ctx, withOwedTrigger()) }()
	waitForQueuedLock(t, store, "things", "ShareRowExclusiveLock")
	// Past the first attempt's lock_timeout and into the pause, then let the writer go.
	time.Sleep(pgmigrate.LockTimeout + pgmigrate.LockTimeoutRetryPause/2)
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migrate once the writer committed: %v", err)
	}
	if got := schemaVersion(t, store); got != 2 {
		t.Errorf("schema version = %d, want 2", got)
	}
	out := logs.String()
	holder := "pid " + strconv.FormatUint(uint64(pid), 10)
	for _, want := range []string{
		"the runner tries it again", "migration=0002_things_owed.up.sql", "attempt=1", "attempts=3",
		"gave up waiting for ShareRowExclusiveLock on things", holder, "diagnosis of " + holder,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "attempt=2") {
		t.Errorf("a second attempt timed out after the writer committed:\n%s", out)
	}
}

// A migration whose lock wait outlasts its lock_timeout on every attempt fails the runner after
// pgmigrate.LockTimeoutAttempts attempts, applies nothing, and its error names the writer it was
// queued behind with the writer's diagnosis. Each attempt is logged.
func TestMigrateFailsNamingTheWriterOnceEveryAttemptTimesOut(t *testing.T) {
	ctx := context.Background()
	store, _ := migratedToOne(t)
	logs := captureLogs(t)
	_, pid := holdWrite(t, store)
	err := store.migrate(ctx, withOwedTrigger())
	var lockTimeout *pgmigrate.LockTimeoutError
	if !errors.As(err, &lockTimeout) || lockTimeout.Migration != "0002_things_owed.up.sql" {
		t.Fatalf("migrate = %v, want a lock timeout of 0002", err)
	}
	holder := "pid " + strconv.FormatUint(uint64(pid), 10)
	for _, want := range []string{
		"after 3 attempts:", "gave up waiting for ShareRowExclusiveLock on things",
		"it was queued behind " + holder,
		"diagnosis of " + holder + ", read when refused: state idle in transaction, xact_start ",
		"relation things RowExclusiveLock granted",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if got := schemaVersion(t, store); got != 1 {
		t.Errorf("schema version = %d, want 1: the migration must not apply", got)
	}
	out := logs.String()
	if n := strings.Count(out, "migration=0002_things_owed.up.sql"); n != pgmigrate.LockTimeoutAttempts {
		t.Errorf("logged %d timed-out attempts, want %d:\n%s", n, pgmigrate.LockTimeoutAttempts, out)
	}
	if !strings.Contains(out, "on its last attempt; the runner fails") {
		t.Errorf("logs lack the last attempt:\n%s", out)
	}
}
