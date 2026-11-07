package refs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const refsGrammarServer = "https://dispatch.example"

// owedRows is every row of the re-index queue as "kind id", in kind then id order.
func owedRows(t *testing.T, q Queryer) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select kind || ' ' || id::text from refs_grammar_owed order by kind, id
	`)
	if err != nil {
		t.Fatalf("read refs_grammar_owed: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan refs_grammar_owed: %v", err)
	}
	return owed
}

// seedRefsGrammarIssue creates project CORE and its issue CORE-1 on the pool.
func seedRefsGrammarIssue(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
}

// A write this build makes owes the drain nothing: the row's own trigger enqueues it and the
// write's index of its text, in the same transaction, settles it. Only a write of a build without
// the whole grammar is left owed.
func TestAWriteLeavesNothingOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	comment := createReferenceComment(t, tx, "CORE-1")
	if owed := owedRows(t, tx); len(owed) != 1 || owed[0] != "comment "+comment {
		t.Fatalf("the comment's own write owes %v, want the comment alone", owed)
	}

	if _, err := ReplaceCounted(ctx, tx, NewReader(ctx, tx, "comment", comment, refsGrammarServer), "Blocked on CORE-1."); err != nil {
		t.Fatalf("index the comment: %v", err)
	}

	if owed := owedRows(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the write indexed its own text, want it empty", owed)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if owed := owedRows(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the write committed, want it empty", owed)
	}
}

// A document version's write settles what the document owes and leaves its scope owed: the
// version's own text is indexed by the write, while the `#id`s written around the document resolve
// against the question blocks this version declares, which only a re-read of those texts stores.
func TestADocumentVersionLeavesItsScopeOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	document, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	versionAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, 'Blocked on CORE-1.', $2)
	`, document, versionAt); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	wantOwed := []string{"artifact " + document, "scope " + document}
	if owed := owedRows(t, tx); len(owed) != 2 || owed[0] != wantOwed[0] || owed[1] != wantOwed[1] {
		t.Fatalf("the version's write owes %v, want %v", owed, wantOwed)
	}

	reader := NewReader(ctx, tx, "artifact", document, refsGrammarServer)
	if _, err := ReplaceDocumentCounted(ctx, tx, document, 1, versionAt,
		text.Refs(reader.Mentions("Blocked on CORE-1.")), nil, nil); err != nil {
		t.Fatalf("index the version: %v", err)
	}

	if owed := owedRows(t, tx); len(owed) != 1 || owed[0] != "scope "+document {
		t.Fatalf("the queue holds %v after the version indexed its own text, want its scope alone", owed)
	}
}

// A drain settles a row only while it still holds the enqueued_at it listed: a write that lands
// between the listing and the re-index moves that time, and the source is left owed, with nothing
// written from the text the drain read, for the next pass to read its newer text.
func TestAMovedOwedRowStaysOwedAndIsNotIndexed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Blocked on CORE-1.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	listed, err := ListOwed(ctx, database.Pool, RefsGrammarOwed, 10)
	if err != nil {
		t.Fatalf("list what is owed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != comment {
		t.Fatalf("owed = %#v, want the comment alone", listed)
	}
	if _, err := database.Pool.Exec(ctx, `
		update refs_grammar_owed set enqueued_at = enqueued_at + interval '1 second' where kind = 'comment' and id = $1
	`, comment); err != nil {
		t.Fatalf("move the comment's enqueued_at: %v", err)
	}

	indexed, err := ReindexOwed(ctx, database.Pool, listed[0], refsGrammarServer)
	if err != nil {
		t.Fatalf("re-index the comment: %v", err)
	}

	if indexed {
		t.Fatalf("the drain re-indexed a comment a write moved, want it left owed")
	}
	if owed := owedRows(t, database.Pool); len(owed) != 1 || owed[0] != "comment "+comment {
		t.Fatalf("the queue holds %v, want the comment still owed", owed)
	}
	var edges int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from refs where from_kind = 'comment' and from_id = $1`, comment).Scan(&edges); err != nil {
		t.Fatalf("count the comment's edges: %v", err)
	}
	if edges != 0 {
		t.Fatalf("the drain wrote %d edges from text it left owed, want none", edges)
	}

	// The next pass lists the time the write left and re-indexes the comment.
	listed, err = ListOwed(ctx, database.Pool, RefsGrammarOwed, 10)
	if err != nil {
		t.Fatalf("list what is owed again: %v", err)
	}
	if indexed, err := ReindexOwed(ctx, database.Pool, listed[0], refsGrammarServer); err != nil || !indexed {
		t.Fatalf("re-index the comment on the next pass = %v (%v), want it indexed", indexed, err)
	}
	if owed := owedRows(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the re-index, want it empty", owed)
	}
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'issue' and to_id = 'CORE-1'`,
		comment).Scan(&edges); err != nil {
		t.Fatalf("count the comment's edges: %v", err)
	}
	if edges != 1 {
		t.Fatalf("the comment holds %d edges to CORE-1 after its re-index, want the bare key's one", edges)
	}
}

// The scope drain reads a source before it locks one. Every markdown version enqueues a `scope`
// row, and draining it walks every source whose text belongs to that document - for a spec, every
// message, ask and comment of its issue - where a text that writes no `#id` has nothing the
// re-index would move. So the text is read with no lock first and only a text that holds a
// candidate has its row taken `for no key update`, the lock an edit of that text holds: a source
// one holds is passed over at once rather than waited for.
func TestTheScopeDrainReadsASourceBeforeItLocksIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	write := func(body string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body)
			values ('CORE-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text
		`, body).Scan(&id); err != nil {
			t.Fatalf("write the comment %q: %v", body, err)
		}
		return id
	}
	quiet, citing := write("Blocked on CORE-1."), write("See #q1.")
	edit, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer edit.Rollback(ctx)
	if _, err := edit.Exec(ctx, `select 1 from comments where id = $1 for no key update`, quiet); err != nil {
		t.Fatalf("hold the comment's row as an edit does: %v", err)
	}

	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	indexed, err := ReindexScopeSource(bounded, database.Pool, "comment", quiet, refsGrammarServer)

	if err != nil || indexed {
		t.Fatalf("the scope drain of a comment an edit holds = %v (%v), want it passed over without waiting", indexed, err)
	}
	// A text that writes a `#id` is still read under its own row and re-indexed from what that
	// read answers: the unlocked read decides only whether to open the transaction.
	if indexed, err := ReindexScopeSource(ctx, database.Pool, "comment", citing, refsGrammarServer); err != nil || !indexed {
		t.Fatalf("the scope drain of a comment that writes a `#id` = %v (%v), want it re-indexed", indexed, err)
	}
}

// Both queues are drained by one statement (OwedQueue.listQuery), and it orders each by its own
// table's columns: an unqualified `id` names the selected `id::text` instead, so the planner
// sorts a text it casts per row, and on the whole grammar's queue it cannot read the index that
// carries all three order columns in order at all. A uuid's canonical text sorts as its bytes
// do, so no row order can tell the two spellings apart; the plan can, and this reads it. The
// whole grammar's queue reads its index in order and sorts nothing, which is what its index is
// for; the block grammar's index carries enqueued_at alone, so that queue sorts the tie group,
// by the table's uuid rather than by a text cast per row.
func TestTheDrainSortsEachQueueByItsTablesID(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for _, queue := range []OwedQueue{BlockGrammarOwed, RefsGrammarOwed} {
		rows, err := database.Pool.Query(ctx, "explain (costs off) "+queue.listQuery(), 200)
		if err != nil {
			t.Fatalf("explain the drain of %s: %v", queue.table, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the plan of the drain of %s: %v", queue.table, err)
		}
		plan := strings.Join(lines, "\n")
		for _, line := range lines {
			key, sorts := strings.CutPrefix(strings.TrimSpace(line), "Sort Key: ")
			if !sorts {
				continue
			}
			if want := "enqueued_at, kind, id"; key != want {
				t.Errorf("the drain of %s sorts by %q; want %q, the table's own columns, and never a cast of one:\n%s",
					queue.table, key, want, plan)
			}
		}
	}
}
