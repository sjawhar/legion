package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A comment owed an index with this build's block grammar is re-indexed under the comment's owner
// lock, the lock an edit of the comment holds from before its first read until it commits. An edit
// that has read the comment's references and block index and planned both therefore keeps the
// re-index out, and the re-index, let in by the commit, reads the body the edit wrote and indexes
// that one. Taking only the comment row, which an edit no longer takes, the re-index would read
// the stored body beneath that plan and index it: the edit's revision would be written over the
// one just indexed, the block the edited body dropped would stay current and keep its mention, and
// the comment would cite what neither body cites. Its owed row carries the enqueued_at the pass
// listed, which this edit leaves where it is - the body it writes holds no block reference, so the
// owing trigger does not fire - so the re-index settles it.
func TestTheGrammarReindexWaitsForTheCommentsOwnerLock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
	editedAt := time.Date(2026, 5, 2, 3, 4, 5, 0, time.UTC)
	// The comment a build older than the block grammar wrote and indexed, and the body its author
	// edits it to: the second paragraph, with the mention of CORE-3, is gone, and the first now
	// cites CORE-2.
	const storedBody = "First paragraph.\n\nSecond cites dispatch://CORE-3."
	const editedBody = "First paragraph cites dispatch://CORE-2."

	fixtures, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixtures: %v", err)
	}
	defer fixtures.Rollback(ctx)
	createReferenceProject(t, fixtures, "CORE")
	createReferenceIssue(t, fixtures, "CORE-1", "CORE")
	for _, cited := range []struct {
		key    string
		number int
		rank   string
	}{{"CORE-2", 2, "V"}, {"CORE-3", 3, "W"}} {
		if _, err := fixtures.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'CORE', $2, 'Cited', '{"kind":"user","id":"alice"}', $3)
		`, cited.key, cited.number, cited.rank); err != nil {
			t.Fatalf("create %s: %v", cited.key, err)
		}
	}
	var comment string
	if err := fixtures.QueryRow(ctx, `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', $1, $2)
		returning id::text
	`, storedBody, commentAt).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if _, err := fixtures.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at)
		values ('comment', $1, 'issue', 'CORE-3', $2)
	`, comment, commentAt); err != nil {
		t.Fatalf("write the reference the older build indexed: %v", err)
	}
	var enqueuedAt time.Time
	if err := fixtures.QueryRow(ctx, `
		insert into block_grammar_owed (kind, id) values ('comment', $1::uuid) returning enqueued_at
	`, comment).Scan(&enqueuedAt); err != nil {
		t.Fatalf("owe the comment an index with the block grammar: %v", err)
	}
	if err := fixtures.Commit(ctx); err != nil {
		t.Fatalf("commit the fixtures: %v", err)
	}

	// The edit's read phase, as PATCH /api/v1/comments/{id} runs it: the comment's owner lock
	// (api.lockedComment), then the reads and the plan of its next revision and of the references
	// the body it writes cites, holding no transaction id and writing nothing.
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

	// The grammar re-index draining the comment, on a connection of its own.
	reindexed := make(chan reconcileAnswer, 1)
	go func() {
		err := ReindexOwedItem(ctx, database.Pool, OwedItem{Kind: "comment", ID: comment, EnqueuedAt: enqueuedAt}, itemBlockServer)
		reindexed <- reconcileAnswer{indexed: err == nil, err: err}
	}()
	if holdingXid := awaitOwnerLockWaiter(t, database, reindexed); holdingXid != 0 {
		t.Fatalf("%d backends hold a transaction id while they wait on the owner lock, want none: the re-index reads and plans before its first write", holdingXid)
	}
	var revision int
	var owedAt *time.Time
	if err := database.Pool.QueryRow(ctx, `
		select revision, owed_at from comment_block_state where comment_id = $1::uuid
	`, comment).Scan(&revision, &owedAt); err != nil {
		t.Fatalf("read the comment's block state: %v", err)
	}
	if revision != 0 || owedAt == nil {
		t.Fatalf("the comment's block state while the edit holds the owner lock = revision %d, owed at %v; want revision 0 and still owed: the re-index writes nothing until the edit commits",
			revision, owedAt)
	}

	// The edit's write phase: its update, the index of the body it writes and that body's
	// references, back to back, and the commit that lets the re-index in.
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
	case answer := <-reindexed:
		if answer.err != nil {
			t.Fatalf("the re-index failed: %v", answer.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the re-index never finished after the edit released the owner lock")
	}

	// What the index holds is the edited body at the edit's revision: one block, its one text row,
	// the mention that block holds, and the comment's own reference to it.
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
	if want := []string{"b1 at 0 revision 1 " + editedBody}; !slices.Equal(blocks, want) {
		t.Errorf("the comment's blocks = %v, want %v", blocks, want)
	}
	texts := commentRows(t, database, "comment's block texts", `
		select block_id || ' revision ' || revision || ' ' || coalesce(text, '(removed)')
		from comment_block_texts where comment_id = $1::uuid order by revision, block_id
	`, comment)
	if want := []string{"b1 revision 1 " + editedBody}; !slices.Equal(texts, want) {
		t.Errorf("the comment's block texts = %v, want %v: one revision written once", texts, want)
	}
	links := commentRows(t, database, "comment's block mentions", `
		select block_id || ' ' || to_kind || ' ' || to_id
		from comment_block_refs where comment_id = $1::uuid order by block_id, to_id
	`, comment)
	if want := []string{"b1 issue CORE-2"}; !slices.Equal(links, want) {
		t.Errorf("the comment's block mentions = %v, want %v: what the edited body cites, from the block holding it", links, want)
	}
	held := commentRows(t, database, "comment's references", `
		select to_kind || ' ' || to_id from refs where from_kind = 'comment' and from_id = $1 order by to_id
	`, comment)
	if want := []string{"issue CORE-2"}; !slices.Equal(held, want) {
		t.Errorf("the comment's references = %v, want %v: the dropped paragraph's mention is gone", held, want)
	}
	var stillOwed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from block_grammar_owed where kind = 'comment' and id = $1::uuid
	`, comment).Scan(&stillOwed); err != nil {
		t.Fatalf("read what the comment is owed: %v", err)
	}
	if stillOwed != 0 {
		t.Errorf("the comment is owed %d indexes with the block grammar, want none: the edit left its enqueued_at where the pass listed it", stillOwed)
	}
}
