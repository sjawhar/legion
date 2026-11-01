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

// drainAnswer is what one drain of a comment off the test's goroutine answered: whether it indexed
// the comment, and the error it failed with.
type drainAnswer struct {
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
// waiters hold a transaction id. A drain that answers first never waited for the lock the edit
// holds, which is what the test is about, so it fails there.
func awaitOwnerLockWaiter(t *testing.T, database *store.Store, answered <-chan drainAnswer) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case answer := <-answered:
			t.Fatalf("the drain answered (indexed %v, error %v) without waiting for the comment's owner lock, which the edit holds through its read phase",
				answer.indexed, answer.err)
		default:
		}
		if waiting, holdingXid := ownerLockWaiters(t, database); waiting > 0 {
			return holdingXid
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never waited on the comment's owner lock within 10s")
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

// A comment a pass owes an index - the block index the trigger owes the reconciliation, or an
// index with this build's block grammar, owed a re-index by the grammar pass - is indexed under
// the comment's owner lock, the lock an edit of the comment holds from before its first read
// until it commits. An edit that has read the comment's block index and its references and
// planned both therefore keeps the drain out, and the drain, let in by the commit, reads the
// comment as the edit left it: the re-index indexes the body the edit wrote, and the
// reconciliation finds the edit's own index and leaves the comment alone. Taking only the comment
// row, which an edit no longer takes, a drain would read the stored body beneath that plan and
// index it: the edit's revision would be written over the one just indexed, the block the edited
// body dropped would stay current and keep its mention, the comment's text at that revision would
// be whichever of the two wrote last, and the comment would cite what neither body cites.
//
// Both drainers run the one edit below, so each of its assertions holds for both, and each case is
// named for the drainer a failure came from.
func TestADrainOfACommentWaitsForItsOwnerLock(t *testing.T) {
	for _, drainer := range []struct {
		name string
		// storedBody is the body the comment holds when the drain lists it, and editedBody the
		// body its author edits it to under the owner lock.
		storedBody, editedBody string
		// fixture writes the rows this drainer's case needs beyond the project, the issue, the
		// cited CORE-2 and the comment, in the fixtures transaction, and answers the time its
		// queue listed the comment at, the zero time where the case queues no row.
		fixture func(t *testing.T, tx pgx.Tx, comment string, commentAt time.Time) time.Time
		// drain runs the drainer on a connection of its own, as its pass runs it.
		drain func(ctx context.Context, database *store.Store, comment string, enqueuedAt time.Time) drainAnswer
		// wantIndexed is whether the drain, let in by the edit's commit, indexes the comment
		// itself or finds the edit's index and leaves it alone.
		wantIndexed bool
		// wantMentions and wantRefs are the block mentions and the references the edited body
		// leaves the comment with.
		wantMentions, wantRefs []string
	}{
		{
			name: "the owed item blocks reconciliation",
			// The comment an older build wrote, owed its blocks by the trigger, and the body its
			// author edits it to: the second paragraph, with the only mention, is gone.
			storedBody: "First paragraph.\n\nSecond cites dispatch://CORE-2.",
			editedBody: "First paragraph.",
			drain: func(ctx context.Context, database *store.Store, comment string, _ time.Time) drainAnswer {
				indexed, err := IndexOwedItemBlocks(ctx, database.Pool, OwedItemBlock{Kind: ItemComment, ID: comment}, itemBlockServer)
				return drainAnswer{indexed: indexed, err: err}
			},
		},
		{
			name: "the grammar re-index",
			// The comment a build older than the block grammar wrote and indexed, and the body
			// its author edits it to: the second paragraph, with the mention of CORE-3, is gone,
			// and the first now cites CORE-2.
			storedBody: "First paragraph.\n\nSecond cites dispatch://CORE-3.",
			editedBody: "First paragraph cites dispatch://CORE-2.",
			fixture: func(t *testing.T, tx pgx.Tx, comment string, commentAt time.Time) time.Time {
				t.Helper()
				ctx := context.Background()
				if _, err := tx.Exec(ctx, `
					insert into issues (key, project_key, number, title, created_by, rank)
					values ('CORE-3', 'CORE', 3, 'Cited', '{"kind":"user","id":"alice"}', 'W')
				`); err != nil {
					t.Fatalf("create CORE-3: %v", err)
				}
				if _, err := tx.Exec(ctx, `
					insert into refs (from_kind, from_id, to_kind, to_id, created_at)
					values ('comment', $1, 'issue', 'CORE-3', $2)
				`, comment, commentAt); err != nil {
					t.Fatalf("write the reference the older build indexed: %v", err)
				}
				var enqueuedAt time.Time
				if err := tx.QueryRow(ctx, `
					insert into block_grammar_owed (kind, id) values ('comment', $1::uuid) returning enqueued_at
				`, comment).Scan(&enqueuedAt); err != nil {
					t.Fatalf("owe the comment an index with the block grammar: %v", err)
				}
				return enqueuedAt
			},
			drain: func(ctx context.Context, database *store.Store, comment string, enqueuedAt time.Time) drainAnswer {
				err := ReindexOwedItem(ctx, database.Pool, OwedItem{Kind: "comment", ID: comment, EnqueuedAt: enqueuedAt}, itemBlockServer)
				return drainAnswer{indexed: err == nil, err: err}
			},
			wantIndexed:  true,
			wantMentions: []string{"b1 issue CORE-2"},
			wantRefs:     []string{"issue CORE-2"},
		},
	} {
		t.Run(drainer.name, func(t *testing.T) {
			database := storetest.Open(t)
			ctx := context.Background()
			commentAt := time.Date(2026, 5, 1, 2, 3, 4, 0, time.UTC)
			editedAt := time.Date(2026, 5, 2, 3, 4, 5, 0, time.UTC)

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
			`, drainer.storedBody, commentAt).Scan(&comment); err != nil {
				t.Fatalf("write the comment: %v", err)
			}
			var enqueuedAt time.Time
			if drainer.fixture != nil {
				enqueuedAt = drainer.fixture(t, fixtures, comment, commentAt)
			}
			if err := fixtures.Commit(ctx); err != nil {
				t.Fatalf("commit the fixtures: %v", err)
			}

			// The edit's read phase, as PATCH /api/v1/comments/{id} runs it: the comment's owner
			// lock (api.lockedComment), then the reads and the plan of its next revision and of
			// the references the body it writes cites, holding no transaction id and writing
			// nothing.
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
			located := text.ExtractAt(drainer.editedBody, itemBlockServer)
			indexing, err := PlanItemIndex(index.Edited(), ItemComment, comment, drainer.editedBody, editedAt, located, nil)
			if err != nil {
				t.Fatalf("plan the edited comment's blocks: %v", err)
			}
			replacement, err := PlanReplace(ctx, editor, string(ItemComment), comment, text.Refs(located), nil)
			if err != nil {
				t.Fatalf("plan the edited comment's references: %v", err)
			}

			// The drainer draining the comment, on a connection of its own.
			drained := make(chan drainAnswer, 1)
			go func() {
				drained <- drainer.drain(ctx, database, comment, enqueuedAt)
			}()
			if holdingXid := awaitOwnerLockWaiter(t, database, drained); holdingXid != 0 {
				t.Fatalf("%d backends hold a transaction id while they wait on the owner lock, want none: the drain reads and plans before its first write", holdingXid)
			}
			var revision int
			var owedAt *time.Time
			if err := database.Pool.QueryRow(ctx, `
				select revision, owed_at from comment_block_state where comment_id = $1::uuid
			`, comment).Scan(&revision, &owedAt); err != nil {
				t.Fatalf("read the comment's block state: %v", err)
			}
			if revision != 0 || owedAt == nil {
				t.Fatalf("the comment's block state while the edit holds the owner lock = revision %d, owed at %v; want revision 0 and still owed: the drain writes nothing until the edit commits",
					revision, owedAt)
			}

			// The edit's write phase: its update, the index of the body it writes and that body's
			// references, back to back, and the commit that lets the drain in.
			if err := store.RunWrite(ctx, editor, func(b *store.WriteBatch) error {
				b.Queue("edit the comment", `update comments set body = $2, edited_at = $3 where id = $1::uuid`, comment, drainer.editedBody, editedAt)
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
			case answer := <-drained:
				if answer.err != nil {
					t.Fatalf("the drain failed: %v", answer.err)
				}
				if answer.indexed != drainer.wantIndexed {
					t.Fatalf("the drain answered indexed %v after the edit, want %v: it reads the comment the edit indexed, never the body the edit wrote over",
						answer.indexed, drainer.wantIndexed)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the drain never finished after the edit released the owner lock")
			}

			// What the index holds is the edited body at the edit's revision: one block, its one
			// text row, the mentions that block holds, and the comment's own references.
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
			if want := []string{"b1 at 0 revision 1 " + drainer.editedBody}; !slices.Equal(blocks, want) {
				t.Errorf("the comment's blocks = %v, want %v", blocks, want)
			}
			texts := commentRows(t, database, "comment's block texts", `
				select block_id || ' revision ' || revision || ' ' || coalesce(text, '(removed)')
				from comment_block_texts where comment_id = $1::uuid order by revision, block_id
			`, comment)
			if want := []string{"b1 revision 1 " + drainer.editedBody}; !slices.Equal(texts, want) {
				t.Errorf("the comment's block texts = %v, want %v: one revision written once", texts, want)
			}
			links := commentRows(t, database, "comment's block mentions", `
				select block_id || ' ' || to_kind || ' ' || to_id
				from comment_block_refs where comment_id = $1::uuid order by block_id, to_id
			`, comment)
			if !slices.Equal(links, drainer.wantMentions) {
				t.Errorf("the comment's block mentions = %v, want %v: what the edited body cites, from the block holding it", links, drainer.wantMentions)
			}
			held := commentRows(t, database, "comment's references", `
				select to_kind || ' ' || to_id from refs where from_kind = 'comment' and from_id = $1 order by to_id
			`, comment)
			if !slices.Equal(held, drainer.wantRefs) {
				t.Errorf("the comment's references = %v, want %v: the dropped paragraph's mention is gone", held, drainer.wantRefs)
			}
			var stillOwed int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from block_grammar_owed where kind = 'comment' and id = $1::uuid
			`, comment).Scan(&stillOwed); err != nil {
				t.Fatalf("read what the comment is owed: %v", err)
			}
			if stillOwed != 0 {
				t.Errorf("the comment is owed %d indexes with the block grammar, want none: a row the pass listed keeps its enqueued_at through the edit, so the drain settles it, and the body the edit writes holds no block reference, so the owing trigger lists no new one", stillOwed)
			}
		})
	}
}
