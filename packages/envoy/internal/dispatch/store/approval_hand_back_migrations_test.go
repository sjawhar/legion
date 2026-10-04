package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// waitForQueuedLock waits until some session is queued, not yet granted, for mode on relation: a
// migration the test started, which the test then holds up or races. It fails t after 20 s.
func waitForQueuedLock(t *testing.T, store *Store, relation, mode string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting bool
		if err := store.Pool.QueryRow(ctx, `
			select exists(select 1 from pg_locks where relation = $1::regclass and mode = $2 and not granted)
		`, relation, mode).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session was seen waiting for %s's %s", relation, mode)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 0064 backfills the version every existing approval request was shown to, then keeps an older
// binary's approval insert valid by adding requested_version from version before the check runs. A
// moved request may advance version while preserving this value, which is how ask reads distinguish
// a human-ready request from one an agent still owns. Its handed_back_reply_id starts null on every
// row and names a reply in the comments table (0066's key, validated), on an approval ask only.
// From 0065 a comment that names no created_at is stamped when its insert runs, so two comments
// one transaction inserts are ordered as they were written.
func TestMigrate0064To0066RecordApprovalHandBacks(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 63)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
		insert into asks (issue_key, author, question, options, kind, approval)
			values ('CORE-1', '{"kind":"session","id":"s"}', 'Approve spec.md (version 7)?',
				'[{"label":"Approve"},{"label":"Request changes"}]', 'approval',
				'{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":7}');
	`); err != nil {
		t.Fatalf("seed pre-0064 approval ask: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var requestedVersion *int
	if err := store.Pool.QueryRow(ctx, `
		select (approval->>'requested_version')::integer
		from asks where kind = 'approval'
	`).Scan(&requestedVersion); err != nil {
		t.Fatalf("read migrated requested version: %v", err)
	}
	if requestedVersion == nil || *requestedVersion != 7 {
		t.Fatalf("migrated requested_version = %v, want 7", requestedVersion)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, options, kind, approval)
		values ('CORE-1', '{"kind":"session","id":"s"}', 'Approve spec.md (version 8)?',
			'[{"label":"Approve"},{"label":"Request changes"}]', 'approval',
			'{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":8}')
	`); err != nil {
		t.Fatalf("older approval insert: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, `
		select (approval->>'requested_version')::integer
		from asks where question = 'Approve spec.md (version 8)?'
	`).Scan(&requestedVersion); err != nil {
		t.Fatalf("read older approval insert: %v", err)
	}
	if requestedVersion == nil || *requestedVersion != 8 {
		t.Fatalf("older requested_version = %v, want 8", requestedVersion)
	}
	var refusal *pgconn.PgError
	for _, requested := range []string{"0", "8.5", `"8"`, "9", "10000000000"} {
		_, err := store.Pool.Exec(ctx, `
			insert into asks (issue_key, author, question, options, kind, approval)
			values ('CORE-1', '{"kind":"session","id":"s"}', 'Approve spec.md (version 8)?',
				'[{"label":"Approve"},{"label":"Request changes"}]', 'approval',
				jsonb_build_object(
					'artifact_id', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b',
					'name', 'spec.md',
					'version', 8,
					'requested_version', $1::jsonb
				)
			)
		`, requested)
		if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "asks_approval_kind_check" {
			t.Fatalf("requested_version %s error = %v, want asks_approval_kind_check", requested, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, options, kind, approval)
		values ('CORE-1', '{"kind":"session","id":"s"}', 'Approve spec.md (version 8)?',
			'[{"label":"Approve"},{"label":"Request changes"}]', 'approval',
			'{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":8,"requested_version":8}')
	`); err != nil {
		t.Fatalf("approval with requested_version: %v", err)
	}
	var handedBack *string
	if err := store.Pool.QueryRow(ctx, `
		select handed_back_reply_id::text from asks where question = 'Approve spec.md (version 7)?'
	`).Scan(&handedBack); err != nil || handedBack != nil {
		t.Fatalf("migrated handed_back_reply_id = %v (%v), want null", handedBack, err)
	}
	var replyID string
	if err := store.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Why?')
		returning id::text
	`).Scan(&replyID); err != nil {
		t.Fatalf("seed reply: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		update asks set handed_back_reply_id = $1 where question = 'Approve spec.md (version 7)?'
	`, replyID); err != nil {
		t.Fatalf("record a hand-back on an approval ask: %v", err)
	}
	for _, refused := range []struct {
		name, sql, code, constraint string
	}{
		{"a question ask", `insert into asks (issue_key, author, question, handed_back_reply_id)
			values ('CORE-1', '{"kind":"session","id":"s"}', 'Which?', '` + replyID + `')`,
			"23514", "asks_handed_back_reply_approval"},
		{"a reply that does not exist", `update asks set handed_back_reply_id = gen_random_uuid()
			where question = 'Approve spec.md (version 7)?'`,
			"23503", "asks_handed_back_reply_id_fkey"},
	} {
		_, err := store.Pool.Exec(ctx, refused.sql)
		if !errors.As(err, &refusal) || refusal.Code != refused.code || refusal.ConstraintName != refused.constraint {
			t.Fatalf("handed_back_reply_id on %s: error = %v, want %s", refused.name, err, refused.constraint)
		}
	}
	// Two comments one transaction inserts with no created_at: the default stamps each when it is
	// inserted, where now() gave both the transaction's start.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var first, second time.Time
	const insertComment = `insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', $1) returning created_at`
	if err := tx.QueryRow(ctx, insertComment, "First.").Scan(&first); err != nil {
		t.Fatalf("insert the first comment: %v", err)
	}
	if err := tx.QueryRow(ctx, insertComment, "Second.").Scan(&second); err != nil {
		t.Fatalf("insert the second comment: %v", err)
	}
	if !second.After(first) {
		t.Fatalf("comments inserted in one transaction: created_at %s then %s, want the second later", first, second)
	}
}

// 0066 bounds its own lock wait below deadlock_timeout. A comment write that goes on to write asks
// while 0066 waits for comments closes a cycle with it, and 0066 gives up first, applying nothing,
// before Postgres's deadlock check runs on either side: the write commits, and the next run
// applies 0066. Under the runner's five-second bound that cycle aborts one side with 40P01.
func TestMigrate0066GivesUpBeforeACommentWriteThatWritesAsksDeadlocks(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 65)
	var askID string
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
	`); err != nil {
		t.Fatalf("seed an issue: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question)
		values ('CORE-1', '{"kind":"session","id":"s"}', 'Ship it?')
		returning id::text
	`).Scan(&askID); err != nil {
		t.Fatalf("seed an ask: %v", err)
	}
	writer, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment write: %v", err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `
		insert into comments (issue_key, author, body, ask_id, turn)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Not yet.', $1, 'agent')
	`, askID); err != nil {
		t.Fatalf("insert the reply: %v", err)
	}
	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()
	waitForQueuedLock(t, store, "comments", "ShareRowExclusiveLock")
	if _, err := writer.Exec(ctx, `update asks set urgency = 'high' where id = $1`, askID); err != nil {
		t.Fatalf("the comment write's ask update failed while 0066 waited for comments: %v", err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the comment write: %v", err)
	}
	err = <-migrated
	var lockTimeout *pgmigrate.LockTimeoutError
	if !errors.As(err, &lockTimeout) || !strings.Contains(lockTimeout.Migration, "0066_asks_handed_back_reply_fkey") {
		t.Fatalf("0066 should have given up at its own lock timeout, got: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate once the comment write ended: %v", err)
	}
	var applied bool
	if err := store.Pool.QueryRow(ctx, `select exists(select 1 from schema_migrations where version = 66)`).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !applied {
		t.Fatal("0066 was not applied by the run after the comment write ended")
	}
}
