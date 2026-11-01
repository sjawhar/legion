package refs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The grammar re-index takes its transaction id only at its write phase: everything it reads - the
// source's owner and text under that owner's lock, the comment's block index, the edges the
// comment holds - and everything it plans comes first, with no transaction id held, and its writes
// go as one batch (the index's statements, the replacement's, the owed row's delete). The owner
// lock tests above show that the pass holds no transaction id while it waits for the lock; this
// one shows it holds none through the reads and the planning that follow the lock, which is what
// an xid window measures.
//
// The rig (storetest.XidTracer) fits this pass as it fits the API's routes and the document
// service's own transactions: the pass runs under a marked context on a traced pool, and the rig
// answers the window of the one transaction it ran - from the statement that gave it its
// transaction id through the COMMIT - from the statements' own results and a poller of
// pg_stat_activity. A window of two statements, the write batch and the COMMIT, is the whole of
// the pass's write phase and nothing else. Against the pass that indexed the blocks at once and
// then read and planned the references, the window holds those reads too.
func TestTheGrammarReindexTakesItsTransactionIdOnlyAtItsWritePhase(t *testing.T) {
	tracer := storetest.NewXidTracer()
	database := &store.Store{Pool: storetest.Traced(t, storetest.Open(t).Pool, tracer, nil)}
	ctx := context.Background()
	commentAt := time.Date(2026, 6, 1, 2, 3, 4, 0, time.UTC)
	// A comment a build older than the block grammar wrote and indexed: its blocks were never
	// indexed, and the edge that build left names an issue the body no longer cites, so the
	// re-index writes a block index, a block mention, the comment's own edges, and the owed row's
	// delete - every statement of a full write phase.
	const body = "First paragraph cites dispatch://CORE-2.\n\nSecond paragraph."

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
	`, body, commentAt).Scan(&comment); err != nil {
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

	var reindexed error
	report := tracer.Measure(t, database.Pool, func() {
		reindexed = ReindexOwedItem(storetest.MarkXid(ctx), database.Pool, OwedItem{
			Kind: "comment", ID: comment, EnqueuedAt: enqueuedAt,
		}, itemBlockServer)
	})
	if reindexed != nil {
		t.Fatalf("the re-index failed: %v", reindexed)
	}
	storetest.AssertXidWindows(t, report)
	if len(report.Windows) != 1 {
		t.Fatalf("windows = %d, unsighted = %d, unjudged = %d; want the re-index's one transaction",
			len(report.Windows), len(report.Unsighted), len(report.Unjudged))
	}
	window := report.Windows[0]
	if len(window.Statements) != 2 {
		t.Fatalf("the re-index held its transaction id for %d statements, want 2 (its write batch and the COMMIT): it reads or plans with the transaction id held\n%s",
			len(window.Statements), window.Detail())
	}
	// The one statement before the COMMIT is the whole write phase: the block index's first
	// statement, the reconciliation of the comment's edges, and the owed row's delete, in one
	// batch the server runs back to back.
	for _, queued := range []string{
		"insert into comment_block_state",
		"delete from refs",
		"insert into refs",
		"delete from block_grammar_owed",
	} {
		if !strings.Contains(window.Statements[0].SQL, queued) {
			t.Errorf("the window's first statement holds no %q, want the pass's whole write phase as one batch\n%s",
				queued, window.Detail())
		}
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())

	// What the pass wrote is what a re-index writes: the body's two blocks, the mention the first
	// holds, the comment's own edge to it, and no owed row.
	var revision int
	var owedAt *time.Time
	if err := database.Pool.QueryRow(ctx, `
		select revision, owed_at from comment_block_state where comment_id = $1::uuid
	`, comment).Scan(&revision, &owedAt); err != nil {
		t.Fatalf("read the comment's block state: %v", err)
	}
	if revision != 1 || owedAt != nil {
		t.Errorf("the comment's block state = revision %d, owed at %v; want revision 1 and not owed", revision, owedAt)
	}
	links := commentRows(t, database, "comment's block mentions", `
		select block_id || ' ' || to_kind || ' ' || to_id
		from comment_block_refs where comment_id = $1::uuid order by block_id, to_id
	`, comment)
	if want := []string{"b1 issue CORE-2"}; !slices.Equal(links, want) {
		t.Errorf("the comment's block mentions = %v, want %v", links, want)
	}
	held := commentRows(t, database, "comment's references", `
		select to_kind || ' ' || to_id from refs where from_kind = 'comment' and from_id = $1 order by to_id
	`, comment)
	if want := []string{"issue CORE-2"}; !slices.Equal(held, want) {
		t.Errorf("the comment's references = %v, want %v: the edge the older build left is gone", held, want)
	}
	var stillOwed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from block_grammar_owed where kind = 'comment' and id = $1::uuid
	`, comment).Scan(&stillOwed); err != nil {
		t.Fatalf("read what the comment is owed: %v", err)
	}
	if stillOwed != 0 {
		t.Errorf("the comment is owed %d indexes with the block grammar, want none", stillOwed)
	}
}
