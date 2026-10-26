package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// embeddingsPendingSinceVersion adds embeddings.pending_since and dead_model, and teaches
// embeddings_enqueue to set them on a text change (INFRA-393); the next version drops the removed
// backfill-embeddings subcommand's checkpoint table.
const embeddingsPendingSinceVersion = 117

// TestMigrationAddsPendingSince runs the shipped files: a row pending at the migration takes its
// own created_at as pending_since and an embedded row the migration's time; a row dead at the
// migration records embed.BedrockModel, the identity every release before it embedded with, as
// its dead_model, and the pending and embedded rows record none; an insert naming neither new
// column, as the release before this one makes, gets pending_since from the default;
// embeddings_enqueue on a text change starts the row's wait again and clears dead_model, and on an
// unchanged text leaves both.
func TestMigrationAddsPendingSince(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, embeddingsPendingSinceVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", embeddingsPendingSinceVersion-1, err)
	}
	waitingSince := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if _, err := store.Pool.Exec(ctx, `
		insert into embeddings (kind, id, text_snapshot, content_hash, created_at) values ('issue', 'OLD-1', 'Pending', 'h1', $1);
	`, waitingSince); err != nil {
		t.Fatalf("seed a pending row: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, created_at) values ('issue', 'OLD-2', 'Embedded', 'h2', 'h2', $1);
	`, waitingSince); err != nil {
		t.Fatalf("seed an embedded row: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into embeddings (kind, id, text_snapshot, content_hash, created_at, dead, confirmed_failures, attempt_count) values ('issue', 'OLD-3', 'Dead', 'h4', $1, true, 10, 12);
	`, waitingSince); err != nil {
		t.Fatalf("seed a dead row: %v", err)
	}
	migrated := time.Now()
	if err := store.migrate(ctx, migrationsThrough(t, embeddingsPendingSinceVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", embeddingsPendingSinceVersion, err)
	}

	if pendingSince, deadModel := readPendingSince(t, store, "issue", "OLD-1"); !pendingSince.Equal(waitingSince) || deadModel != nil {
		t.Errorf("pending row: pending_since %v dead_model %v, want its created_at %v and no dead_model", pendingSince, deadModel, waitingSince)
	}
	if pendingSince, deadModel := readPendingSince(t, store, "issue", "OLD-2"); pendingSince.Before(migrated.Add(-time.Second)) || deadModel != nil {
		t.Errorf("embedded row: pending_since %v dead_model %v, want the migration's time (after %v) and no dead_model", pendingSince, deadModel, migrated)
	}
	if _, deadModel := readPendingSince(t, store, "issue", "OLD-3"); deadModel == nil || *deadModel != embed.BedrockModel {
		t.Errorf("dead row: dead_model %v, want %s", deadModel, embed.BedrockModel)
	}

	inserted := time.Now()
	if _, err := store.Pool.Exec(ctx, `insert into embeddings (kind, id, text_snapshot, content_hash) values ('comment', 'NEW-1', 'New', 'h3')`); err != nil {
		t.Fatalf("insert naming neither new column: %v", err)
	}
	if pendingSince, deadModel := readPendingSince(t, store, "comment", "NEW-1"); pendingSince.Before(inserted.Add(-time.Second)) || deadModel != nil {
		t.Errorf("new row: pending_since %v dead_model %v, want the insert's time and no dead_model", pendingSince, deadModel)
	}

	if _, err := store.Pool.Exec(ctx, `select embeddings_enqueue('ask', 'ASK-1', 'Question? ')`); err != nil {
		t.Fatalf("enqueue an ask: %v", err)
	}
	dyingSince := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := store.Pool.Exec(ctx, `
		update embeddings set dead = true, dead_model = 'us.cohere.embed-v4:0', pending_since = $1 where kind = 'ask' and id = 'ASK-1'
	`, dyingSince); err != nil {
		t.Fatalf("mark the ask dead: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `select embeddings_enqueue('ask', 'ASK-1', 'Question? ')`); err != nil {
		t.Fatalf("enqueue the same text: %v", err)
	}
	if pendingSince, deadModel := readPendingSince(t, store, "ask", "ASK-1"); !pendingSince.Equal(dyingSince) || deadModel == nil || *deadModel != "us.cohere.embed-v4:0" {
		t.Errorf("after the same text: pending_since %v dead_model %v, want both unchanged (%v, us.cohere.embed-v4:0)", pendingSince, deadModel, dyingSince)
	}
	changed := time.Now()
	if _, err := store.Pool.Exec(ctx, `select embeddings_enqueue('ask', 'ASK-1', 'Question? Answer')`); err != nil {
		t.Fatalf("enqueue a changed text: %v", err)
	}
	var dead bool
	if err := store.Pool.QueryRow(ctx, `select dead from embeddings where kind = 'ask' and id = 'ASK-1'`).Scan(&dead); err != nil {
		t.Fatalf("read dead: %v", err)
	}
	if pendingSince, deadModel := readPendingSince(t, store, "ask", "ASK-1"); pendingSince.Before(changed.Add(-time.Second)) || deadModel != nil || dead {
		t.Errorf("after a changed text: pending_since %v dead_model %v dead %t, want now, none, alive", pendingSince, deadModel, dead)
	}
}

// readPendingSince reads one embeddings row's pending_since and dead_model.
func readPendingSince(t *testing.T, store *Store, kind, id string) (time.Time, *string) {
	t.Helper()
	var pendingSince time.Time
	var deadModel *string
	if err := store.Pool.QueryRow(context.Background(), `select pending_since, dead_model from embeddings where kind = $1 and id = $2`, kind, id).Scan(&pendingSince, &deadModel); err != nil {
		t.Fatalf("read %s %s: %v", kind, id, err)
	}
	return pendingSince, deadModel
}

// TestMigrationDropsTheBackfillCheckpoints: the removed backfill-embeddings subcommand's
// checkpoint table, rows and all, is gone after the next version.
func TestMigrationDropsTheBackfillCheckpoints(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, embeddingsPendingSinceVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", embeddingsPendingSinceVersion, err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into embeddings_backfill_progress (kind, last_id, done) values ('issue', 'X-9', true)`); err != nil {
		t.Fatalf("seed a checkpoint: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, embeddingsPendingSinceVersion+1)); err != nil {
		t.Fatalf("migrate to %d: %v", embeddingsPendingSinceVersion+1, err)
	}
	var exists bool
	if err := store.Pool.QueryRow(ctx, `select to_regclass('embeddings_backfill_progress') is not null`).Scan(&exists); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if exists {
		t.Error("embeddings_backfill_progress still exists after the migration")
	}
}
