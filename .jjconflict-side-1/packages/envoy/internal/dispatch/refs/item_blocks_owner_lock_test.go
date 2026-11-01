package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// reconcileAnswer is what one IndexOwedItemBlocks call off the test's goroutine answered.
type reconcileAnswer struct {
	indexed bool
	err     error
}

// ownerLockWaiters counts this test database's backends waiting on an owner lock (the statement
// store.LockOwner runs, which names its result owner_lock), and how many of them hold a
// transaction id while they wait, as api/write_phase_test.go's reader of the same view does.
func ownerLockWaiters(t *testing.T, database *store.Store) (waiting, holdingXid int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where backend_xid is not null)
		from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'
		  and query like '%owner_lock%' and pid <> pg_backend_pid()
	`).Scan(&waiting, &holdingXid); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting, holdingXid
}

// awaitOwnerLockWaiter waits until a backend waits on an owner lock and answers how many of the
// waiters hold a transaction id. A reconciliation that answers first never waited for the lock
// the edit holds, which is what the test is about, so it fails there.
func awaitOwnerLockWaiter(t *testing.T, database *store.Store, answered <-chan reconcileAnswer) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case answer := <-answered:
			t.Fatalf("the reconciliation answered (indexed %v, error %v) without waiting for the comment's owner lock, which the edit holds through its read phase",
				answer.indexed, answer.err)
		default:
		}
		if waiting, holdingXid := ownerLockWaiters(t, database); waiting > 0 {
			return holdingXid
		}
		if time.Now().After(deadline) {
			t.Fatal("the reconciliation never waited on the comment's owner lock within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// commentRows is the rows query reads of comment, each row a single text column, for a fixture
// the test compares whole.
func commentRows(t *testing.T, database *store.Store, what, query, comment string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), query, comment)
	if err != nil {
		t.Fatalf("read the %s: %v", what, err)
	}
	read, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the %s: %v", what, err)
	}
	return read
}

// A comment owed a block index is indexed by the reconciliation under the comment's owner lock,
// the lock an edit of the comment holds from before its first read until it commits. An edit that
// has read the comment's block-index state and planned its next revision therefore keeps the
// reconciliation out, and the reconciliation, let in by the commit, reads the comment indexed
// from the body the edit wrote and leaves it alone. Taking only the comment row, which an edit no
// longer takes, the reconciliation would index the stored body beneath that plan: the edit's
// revision would be written over the one just indexed, the block the edited body dropped would
// stay current and keep its mention, and the comment's text at that revision would be whichever
// of the two wrote last.
func TestOwedItemBlocksWaitForTheCommentsOwnerLock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 4, 1, 2, 3, 4, 0, time.UTC)
	editedAt := time.Date(2026, 4, 2, 3, 4, 5, 0, time.UTC)
	// The comment an older build wrote, owed its blocks by the trigger, and the body its author
	// edits it to: the second paragraph, with the only mention, is gone.
	const storedBody = "First paragraph.\n\nSecond cites dispatch://CORE-2."
	const editedBody = "First paragraph."

	fixtures, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixtures: %v", err)
	}
	defer fixtures.Rollback(ctx)
	createReferenceProject(t, fixtures, "CORE")
	createReferenceIssue(t, fixtures, "CORE-1", "CORE")
	if _, err := fixtures.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-2', 'CORE', 2, 'Cited', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create CORE-2: %v", err)
	}
	var comment string
	if err := fixtures.QueryRow(ctx, `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', $1, $2)
		returning id::text
	`, storedBody, commentAt).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if err := fixtures.Commit(ctx); err != nil {
		t.Fatalf("commit the fixtures: %v", err)
	}

	// The edit's read phase, as PATCH /api/v1/comments/{id} runs it: the comment's owner lock
	// (api.lockedComment), then the reads and the plan of its next revision, holding no
	// transaction id and writing nothing.
	editor, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer editor.Rollback(ctx)
	if err := store.LockOwner(ctx, editor, store.IssueOwner("CORE-1")); err != nil {
		t.Fatalf("take the issue's owner lock for the edit: %v", err)
	}
	index, err := ReadItemIndex(ctx, editor, ItemComment, comment)
	if err != nil {
		t.Fatalf("read the comment's block index: %v", err)
	}
	located := text.ExtractAt(editedBody, itemBlockServer)
	indexing, err := PlanItemIndex(index.Edited(), ItemComment, comment, editedBody, editedAt, located, nil)
	if err != nil {
		t.Fatalf("plan the edited comment's blocks: %v", err)
	}
	replacement, err := PlanReplace(ctx, editor, string(ItemComment), comment, text.Refs(located), nil)
	if err != nil {
		t.Fatalf("plan the edited comment's references: %v", err)
	}

	// The reconciliation draining the comment, on a connection of its own.
	reconciled := make(chan reconcileAnswer, 1)
	go func() {
		indexed, err := IndexOwedItemBlocks(ctx, database.Pool, OwedItemBlock{Kind: ItemComment, ID: comment}, itemBlockServer)
		reconciled <- reconcileAnswer{indexed: indexed, err: err}
	}()
	if holdingXid := awaitOwnerLockWaiter(t, database, reconciled); holdingXid != 0 {
		t.Fatalf("%d backends hold a transaction id while they wait on the owner lock, want none: the reconciliation reads and plans before its first write", holdingXid)
	}
	var revision int
	var owedAt *time.Time
	if err := database.Pool.QueryRow(ctx, `
		select revision, owed_at from comment_block_state where comment_id = $1::uuid
	`, comment).Scan(&revision, &owedAt); err != nil {
		t.Fatalf("read the comment's block state: %v", err)
	}
	if revision != 0 || owedAt == nil {
		t.Fatalf("the comment's block state while the edit holds the owner lock = revision %d, owed at %v; want revision 0 and still owed: the reconciliation indexes nothing until the edit commits",
			revision, owedAt)
	}

	// The edit's write phase: its update, the index of the body it writes and that body's
	// references, back to back, and the commit that lets the reconciliation in.
	if err := store.RunWrite(ctx, editor, func(b *store.WriteBatch) error {
		b.Queue("edit the comment", `update comments set body = $2, edited_at = $3 where id = $1::uuid`, comment, editedBody, editedAt)
		indexing.Queue(b)
		replacement.Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("write the edit: %v", err)
	}
	if err := editor.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}

	select {
	case answer := <-reconciled:
		if answer.err != nil {
			t.Fatalf("the reconciliation failed: %v", answer.err)
		}
		if answer.indexed {
			t.Fatal("the reconciliation indexed the comment after the edit, want it left alone: the edit indexed the body it wrote")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reconciliation never finished after the edit released the owner lock")
	}

	// What the index holds is the edited body at the edit's revision: one block, its one text
	// row, and no mention of the issue the dropped paragraph cited.
	if err := database.Pool.QueryRow(ctx, `
		select revision, owed_at from comment_block_state where comment_id = $1::uuid
	`, comment).Scan(&revision, &owedAt); err != nil {
		t.Fatalf("read the comment's block state: %v", err)
	}
	if revision != 1 || owedAt != nil {
		t.Fatalf("the comment's block state = revision %d, owed at %v; want revision 1 and not owed", revision, owedAt)
	}
	blocks := commentRows(t, database, "comment's blocks", `
		select block_id || coalesce(' at ' || position, ' removed') || ' revision ' || changed_revision || ' ' || excerpt
		from comment_blocks where comment_id = $1::uuid order by block_id
	`, comment)
	if want := []string{"b1 at 0 revision 1 First paragraph."}; !slices.Equal(blocks, want) {
		t.Errorf("the comment's blocks = %v, want %v", blocks, want)
	}
	texts := commentRows(t, database, "comment's block texts", `
		select block_id || ' revision ' || revision || ' ' || coalesce(text, '(removed)')
		from comment_block_texts where comment_id = $1::uuid order by revision, block_id
	`, comment)
	if want := []string{"b1 revision 1 First paragraph."}; !slices.Equal(texts, want) {
		t.Errorf("the comment's block texts = %v, want %v: one revision written once", texts, want)
	}
	links := commentRows(t, database, "comment's block mentions", `
		select block_id || ' ' || to_kind || ' ' || to_id
		from comment_block_refs where comment_id = $1::uuid order by block_id, to_id
	`, comment)
	if len(links) != 0 {
		t.Errorf("the comment's block mentions = %v, want none: the edited body cites nothing", links)
	}
	held := commentRows(t, database, "comment's references", `
		select to_kind || ' ' || to_id from refs where from_kind = 'comment' and from_id = $1 order by to_id
	`, comment)
	if len(held) != 0 {
		t.Errorf("the comment's references = %v, want none: the edited body cites nothing", held)
	}
}
