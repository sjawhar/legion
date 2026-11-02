package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// beforeItemBlocks is the last migration before comment and message blocks: 0142_item_blocks
// through 0144_messages_item_blocks_trigger apply after it. itemBlockTables is 0142, which creates
// the tables 0143 and 0144 attach their triggers to and seed.
const (
	beforeItemBlocks = 141
	itemBlockTables  = 142
)

// itemBlockState is a comment's or message's row of its block state table.
type itemBlockState struct {
	found               bool
	owedAt              *time.Time
	revision, nextBlock int
}

// readItemBlockState reads the state row of a comment (revision and next_block too) or a message.
func readItemBlockState(t *testing.T, store *Store, kind, id string) itemBlockState {
	t.Helper()
	var state itemBlockState
	var err error
	switch kind {
	case "comment":
		err = store.Pool.QueryRow(context.Background(),
			`select owed_at, revision, next_block from comment_block_state where comment_id = $1`, id).
			Scan(&state.owedAt, &state.revision, &state.nextBlock)
	case "message":
		err = store.Pool.QueryRow(context.Background(),
			`select owed_at from message_block_state where message_id = $1`, id).Scan(&state.owedAt)
	default:
		t.Fatalf("unknown item kind %q", kind)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return state
	}
	if err != nil {
		t.Fatalf("read %s block state(%s): %v", kind, id, err)
	}
	state.found = true
	return state
}

func assertItemOwed(t *testing.T, store *Store, what, kind, id string) itemBlockState {
	t.Helper()
	state := readItemBlockState(t, store, kind, id)
	if !state.found || state.owedAt == nil {
		t.Fatalf("%s: %s %s state row found %v, owed_at %v; want it owed", what, kind, id, state.found, state.owedAt)
	}
	return state
}

// A comment or message a build without the item block index writes, during the roll or after a
// rollback, is owed an index that build cannot give it: a comment on insert and on an edit of its
// body, a message (a direct one too) on insert. An edit of another column owes nothing, and an
// edit of a comment already indexed moves only owed_at, keeping its revision and next block id. A
// block-aware build's own index, in the write's transaction, leaves the item indexed, and the
// reconciliation's owed reads are served by their partial indexes.
func TestAnOlderBuildsWriteLeavesTheItemOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedBlockIndexIssue(t, store)

	comment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Alpha')
		returning id::text`)
	if state := assertItemOwed(t, store, "an inserted comment", "comment", comment); state.revision != 0 || state.nextBlock != 1 {
		t.Fatalf("an inserted comment: revision %d, next_block %d; want 0 and 1", state.revision, state.nextBlock)
	}

	// The index of a block-aware build settles it at revision 1 with blocks b1 and b2 minted.
	if _, err := store.Pool.Exec(ctx, `
		update comment_block_state set owed_at = null, revision = 1, next_block = 3 where comment_id = $1
	`, comment); err != nil {
		t.Fatalf("index comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set body = 'Alpha, edited' where id = $1`, comment); err != nil {
		t.Fatalf("edit comment: %v", err)
	}
	if state := assertItemOwed(t, store, "an edited comment", "comment", comment); state.revision != 1 || state.nextBlock != 3 {
		t.Fatalf("an edited comment: revision %d, next_block %d; want 1 and 3 kept", state.revision, state.nextBlock)
	}

	if _, err := store.Pool.Exec(ctx, `update comment_block_state set owed_at = null where comment_id = $1`, comment); err != nil {
		t.Fatalf("index edited comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set resolved = true where id = $1`, comment); err != nil {
		t.Fatalf("resolve comment: %v", err)
	}
	if state := readItemBlockState(t, store, "comment", comment); state.owedAt != nil {
		t.Fatalf("resolving a comment owed it again at %v, want only a body edit to", *state.owedAt)
	}

	// An edit of a comment already owed moves its owed_at, so a drain that read the earlier time
	// leaves it owed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `update comment_block_state set owed_at = $2 where comment_id = $1`, comment, past); err != nil {
		t.Fatalf("age the owed comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set body = 'Alpha, edited again' where id = $1`, comment); err != nil {
		t.Fatalf("edit owed comment: %v", err)
	}
	if state := assertItemOwed(t, store, "an owed comment edited again", "comment", comment); !state.owedAt.After(past) {
		t.Fatalf("an owed comment edited again kept owed_at %v, want it moved past %v", *state.owedAt, past)
	}

	issueMessage := insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Beta')
		returning id::text`)
	directMessage := insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Gamma', 'session:s1')
		returning id::text`)
	assertItemOwed(t, store, "an issue message", "message", issueMessage)
	assertItemOwed(t, store, "a direct message", "message", directMessage)

	// A block-aware build inserts the comment and indexes it in one transaction.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var indexed string
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Delta')
		returning id::text`).Scan(&indexed); err != nil {
		t.Fatalf("insert indexed comment: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		update comment_block_state set owed_at = null, revision = 1, next_block = 2 where comment_id = $1
	`, indexed); err != nil {
		t.Fatalf("index comment in its transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit indexed comment: %v", err)
	}
	if state := readItemBlockState(t, store, "comment", indexed); !state.found || state.owedAt != nil || state.revision != 1 {
		t.Fatalf("a block-aware build's comment: %+v, want indexed at revision 1", state)
	}

	for name, read := range map[string]struct{ query, index string }{
		"owed comments": {`select comment_id, owed_at from comment_block_state where owed_at is not null order by owed_at limit 200`, "comment_block_state_owed"},
		"owed messages": {`select message_id, owed_at from message_block_state where owed_at is not null order by owed_at limit 200`, "message_block_state_owed"},
	} {
		if plan := explainWithoutSeqScan(t, store, read.query); !strings.Contains(plan, read.index) {
			t.Errorf("%s: plan does not read %s:\n%s", name, read.index, plan)
		}
	}
}

// Every comment and message stored before the item block index is owed one by the migrations: a
// comment or message written before 0142, and one written after 0142 and before the trigger that
// owes its kind (an older build's write during the migrations), each gets an owed state row.
func TestTheItemBlockMigrationsOweEveryStoredItem(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeItemBlocks)
	seedBlockIndexIssue(t, store)
	insertComment := func(body string) string {
		return insertReturningID(t, store, `
			insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text`, body)
	}
	insertMessage := func(body string) string {
		return insertReturningID(t, store, `
			insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', $1)
			returning id::text`, body)
	}
	olderComment := insertComment("Older")
	olderMessage := insertMessage("Older")
	olderDirect := insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Older', 'session:s1')
		returning id::text`)

	migrateThrough(t, store, itemBlockTables)
	racedComment := insertComment("Raced")
	racedMessage := insertMessage("Raced")

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, item := range []struct{ what, kind, id string }{
		{"a comment stored before 0142", "comment", olderComment},
		{"a comment written between 0142 and its trigger", "comment", racedComment},
		{"a message stored before 0142", "message", olderMessage},
		{"a direct message stored before 0142", "message", olderDirect},
		{"a message written between 0142 and its trigger", "message", racedMessage},
	} {
		if state := assertItemOwed(t, store, item.what, item.kind, item.id); item.kind == "comment" && (state.revision != 0 || state.nextBlock != 1) {
			t.Errorf("%s: revision %d, next_block %d; want 0 and 1", item.what, state.revision, state.nextBlock)
		}
	}
}
