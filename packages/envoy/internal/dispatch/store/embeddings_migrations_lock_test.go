package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The embeddings trigger migrations (0072-0076) take one table's lock at a time, each in its
// own transaction, exactly as the search migrations (0056-0061) do and for the same reason:
// while a transaction holds `messages` (the last table, 0077) with a lock CREATE TRIGGER's own
// SHARE ROW EXCLUSIVE conflicts with, the runner applies 0073-0076 and commits each, gives up on
// 0077 at pgmigrate.LockTimeout, and meanwhile a read of `issues` - whose lock a single-file
// migration would still be holding - answers at once. Once the holder ends, the runner finishes.
// The holder must take at least ROW EXCLUSIVE: a plain SELECT's ACCESS SHARE is compatible with
// CREATE TRIGGER's own lock (unlike the search migrations, which also ALTER TABLE ADD COLUMN and
// so need ACCESS EXCLUSIVE, conflicting with a reader) - a no-op UPDATE matching no row still
// takes the table-level lock its statement needs.
func TestEmbeddingsMigrationsLockOneTableAtATime(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 71)
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "update messages set id = id where false"); err != nil {
		t.Fatalf("hold messages: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()

	// Wait until the runner is queued on messages, then read issues with a one-second bound.
	waitForQueuedLock(t, store, "messages", "ShareRowExclusiveLock")
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
	if version != 75 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 75 (0072-0075 committed one by one)", version)
	}

	err = <-migrated
	var lockTimeout *pgmigrate.LockTimeoutError
	if err == nil || !errors.As(err, &lockTimeout) || !strings.Contains(lockTimeout.Migration, "0076_messages_embeddings_trigger") {
		t.Fatalf("the runner should have given up on 0076 at its lock timeout, got: %v", err)
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
