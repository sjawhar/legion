package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The search migrations take one table's ACCESS EXCLUSIVE lock at a time, each in its own
// transaction: while a transaction holds `messages` (the last table, 0061), the runner applies
// 0056-0060 and commits each, gives up on 0061 at pgmigrate.LockTimeout, and meanwhile a read of
// `issues` - whose lock a single-file migration would still be holding - answers at once. Once the
// holder ends, the runner finishes. This is what dispatch_rollout_rehearsal.sh, which holds one
// table at a time, cannot see across tables.
func TestSearchMigrationsLockOneTableAtATime(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 55)
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "select count(*) from messages"); err != nil {
		t.Fatalf("hold messages: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()

	// Wait until the runner is queued on messages, then read issues with a one-second bound.
	deadline := time.Now().Add(20 * time.Second)
	waiting := false
	for !waiting && time.Now().Before(deadline) {
		if err := store.Pool.QueryRow(ctx, `
			select exists(select 1 from pg_locks l join pg_stat_activity a using (pid)
			              where l.relation = 'messages'::regclass and l.mode = 'AccessExclusiveLock' and not l.granted)
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("the runner was never seen waiting for messages' ACCESS EXCLUSIVE lock")
	}
	var version int
	var issues int
	err = func() error {
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "set local statement_timeout = '1s'"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "select count(*) from issues").Scan(&issues); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version)
	}()
	if err != nil {
		t.Fatalf("while the runner waited for messages, a read of issues did not answer within 1 s: %v", err)
	}
	if version != 60 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 60 (0056-0060 committed one by one)", version)
	}

	err = <-migrated
	var lockTimeout *pgmigrate.LockTimeoutError
	if err == nil || !errors.As(err, &lockTimeout) || !strings.Contains(lockTimeout.Migration, "0061_messages_search_trigger") {
		t.Fatalf("the runner should have given up on 0061 at its lock timeout, got: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release messages: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate once the holder ended: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if last := migrations[len(migrations)-1].Version; version != last {
		t.Errorf("schema_migrations at %d after the holder ended, want %d, the highest migration", version, last)
	}
}
