package embedqueue

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// whitespaceOnly is a text with nothing to embed (nothingToEmbed) that is not the empty string:
// only spaces, tabs and line breaks.
const whitespaceOnly = " \t\n  \r\n "

// settledRow is what an embeddings row holds that settling a text with nothing to embed decides.
type settledRow struct {
	settled, hasVector, dead, hasDeadModel bool
	model                                  string
	attempts, confirmed                    int
}

func readSettledRow(t *testing.T, database *store.Store, kind, id string) settledRow {
	t.Helper()
	var row settledRow
	if err := database.Pool.QueryRow(context.Background(), `
		select embedded_hash is not distinct from content_hash, embedding is not null, dead, dead_model is not null,
		       coalesce(model, ''), attempt_count, confirmed_failures
		from embeddings where kind = $1 and id = $2`, kind, id,
	).Scan(&row.settled, &row.hasVector, &row.dead, &row.hasDeadModel, &row.model, &row.attempts, &row.confirmed); err != nil {
		t.Fatalf("read %s/%s: %v", kind, id, err)
	}
	return row
}

// requireSettledWithNoVector fails unless the row is settled as a text with nothing to embed is:
// embedded at its current text by identity, with no vector, alive and with fresh counters.
func requireSettledWithNoVector(t *testing.T, database *store.Store, kind, id, identity string) {
	t.Helper()
	want := settledRow{settled: true, model: identity}
	if got := readSettledRow(t, database, kind, id); got != want {
		t.Errorf("%s/%s = %+v, want %+v (settled by %q with no vector, alive, fresh counters)", kind, id, got, want, identity)
	}
}

// requireNoBlankText fails if any call the embedder recorded carried a text with nothing to embed.
func requireNoBlankText(t *testing.T, calls [][]string) {
	t.Helper()
	for _, call := range calls {
		for _, text := range call {
			if strings.TrimSpace(text) == "" {
				t.Errorf("the embedder was given %q, a text with nothing to embed: calls %q", text, calls)
			}
		}
	}
}

// seedNothingToEmbed writes, under project, one issue with a real title and three items whose
// text has nothing to embed: a document whose markdown is empty, a comment whose body is empty
// and a message whose body is only white space. It returns the document's, comment's and
// message's ids.
func seedNothingToEmbed(t *testing.T, q querier, project string) (documentID, commentID, messageID string) {
	t.Helper()
	ctx := context.Background()
	issueKey := project + "-1"
	mustExec(t, q, `insert into projects (key, name) values ($1, 'Test')`, project)
	mustExec(t, q, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'A real title', '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, project)
	if err := q.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
		values ($1, 'empty-md', 'empty.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/empty-md')
		returning id::text
	`, project).Scan(&documentID); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	mustExec(t, q, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, '', '[]')`, documentID)
	if err := q.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', '') returning id::text
	`, issueKey).Scan(&commentID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	if err := q.QueryRow(ctx, `
		insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issueKey, whitespaceOnly).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return documentID, commentID, messageID
}

// requireOnlyTheIssueCounted fails unless Coverage counts the one real issue, current, and none
// of the items whose text has nothing to embed, in any kind or in total.
func requireOnlyTheIssueCounted(t *testing.T, database *store.Store, identity string) {
	t.Helper()
	report, err := Coverage(context.Background(), database, identity)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	for kind, items := range map[string]int64{
		"document": report.Kinds.Document.Items, "comment": report.Kinds.Comment.Items,
		"ask": report.Kinds.Ask.Items, "message": report.Kinds.Message.Items,
	} {
		if items != 0 {
			t.Errorf("%s items = %d, want 0 (its only item has nothing to embed): %+v", kind, items, report)
		}
	}
	if got := report.Total; got.Items != 1 || got.Current != 1 || got.Lacking != 0 || got.Failed != 0 || got.OldestLackingSeconds != nil {
		t.Errorf("total %+v, want the one real issue, current, and nothing lacking or failed", got)
	}
	if report.Total != report.Kinds.Issue {
		t.Errorf("total %+v, want the issue counts %+v", report.Total, report.Kinds.Issue)
	}
}

// An empty document, an empty comment and a whitespace-only message, enqueued by their triggers,
// never reach the embedder, never go dead, and are settled with no vector; Coverage counts none
// of them, and the coverage pass adds and changes nothing for them.
func TestNothingToEmbedNeverReachesTheEmbedderAndIsNotCovered(t *testing.T) {
	database := storetest.Open(t)
	documentID, commentID, messageID := seedNothingToEmbed(t, database.Pool, "BLNK")
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}

	drain(t, deps)
	requireNoBlankText(t, embedder.calls)
	if len(embedder.calls) != 1 || !slices.Equal(embedder.calls[0], []string{"A real title"}) {
		t.Errorf("embedder calls = %q, want one call with the issue's title alone", embedder.calls)
	}
	requireSettledWithNoVector(t, database, "document", documentID, configuredIdentity)
	requireSettledWithNoVector(t, database, "comment", commentID, configuredIdentity)
	requireSettledWithNoVector(t, database, "message", messageID, configuredIdentity)
	requireOnlyTheIssueCounted(t, database, configuredIdentity)

	before := snapshotRows(t, database)
	cover(t, deps)
	drain(t, deps)
	if after := snapshotRows(t, database); len(after) != len(before) {
		t.Errorf("%d rows after a pass, %d before; want the pass to add none", len(after), len(before))
	} else {
		for key, row := range before {
			if after[key] != row {
				t.Errorf("%s: the pass changed %+v to %+v", key, row, after[key])
			}
		}
	}
	requireNoBlankText(t, embedder.calls)
	requireOnlyTheIssueCounted(t, database, configuredIdentity)
}

// The coverage pass adds no row for an item no trigger saw whose text has nothing to embed, and
// Coverage never counts it as lacking.
func TestCoverAddsNoRowForAnItemWithNothingToEmbed(t *testing.T) {
	database := storetest.Open(t)
	withTriggersOff(t, database, func(q querier) { seedNothingToEmbed(t, q, "BLNO") })
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}

	cover(t, deps)
	var rows []string
	for key := range snapshotRows(t, database) {
		rows = append(rows, key)
	}
	if !slices.Equal(rows, []string{"issue/BLNO-1"}) {
		t.Errorf("rows after the pass = %v, want the real issue's alone", rows)
	}
	drain(t, deps)
	requireNoBlankText(t, embedder.calls)
	requireOnlyTheIssueCounted(t, database, configuredIdentity)
}

// One empty row among two real ones in a batch is settled, and the two real rows embed in exactly
// one Embed call in the same ProcessBatch, which counts all three done.
func TestABatchWithAnEmptyRowEmbedsTheRealRowsInOneCall(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "MIXD", "MIXD-1", "First real title")
	seedIssue(t, database, "MIXD", "MIXD-2", "")
	seedIssue(t, database, "MIXD", "MIXD-3", "Second real title")
	embedder := &fakeEmbedder{}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil || succeeded != 3 || failed != 0 || blocked || throttled {
		t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v; want 3 done (two embedded, one settled), 0 failed", succeeded, failed, blocked, throttled, err)
	}
	if len(embedder.calls) != 1 {
		t.Fatalf("embedder calls = %q, want exactly one", embedder.calls)
	}
	got := slices.Sorted(slices.Values(embedder.calls[0]))
	if want := []string{"First real title", "Second real title"}; !slices.Equal(got, want) {
		t.Errorf("the one call embedded %q, want %q", got, want)
	}
	requireSettledWithNoVector(t, database, "issue", "MIXD-2", embedder.Identity())
	for _, id := range []string{"MIXD-1", "MIXD-3"} {
		if row := readSettledRow(t, database, "issue", id); !row.settled || !row.hasVector || row.model != embedder.Identity() {
			t.Errorf("%s = %+v, want embedded with a vector by %q", id, row, embedder.Identity())
		}
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
}

// A text that changes from real to empty loses its vector with no model call, and one that then
// changes back to real is embedded normally and counted current.
func TestATextThatChangesBetweenEmptyAndRealIsSettledThenEmbedded(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "FLIP", "FLIP-1", "Real at first")
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	drain(t, deps)

	mustExec(t, database.Pool, `update issues set title = '' where key = 'FLIP-1'`)
	drain(t, deps)
	if len(embedder.calls) != 1 {
		t.Errorf("embedder calls after the title emptied = %q, want only the first title's", embedder.calls)
	}
	requireSettledWithNoVector(t, database, "issue", "FLIP-1", configuredIdentity)
	if report, err := Coverage(ctx, database, configuredIdentity); err != nil || report.Total.Items != 0 || report.Total.Failed != 0 {
		t.Errorf("coverage with the title empty = %+v, %v; want no item", report.Total, err)
	}

	mustExec(t, database.Pool, `update issues set title = 'Real again' where key = 'FLIP-1'`)
	drain(t, deps)
	if len(embedder.calls) != 2 || !slices.Equal(embedder.calls[1], []string{"Real again"}) {
		t.Errorf("embedder calls = %q, want a second call embedding the new title", embedder.calls)
	}
	if row := readSettledRow(t, database, "issue", "FLIP-1"); !row.settled || !row.hasVector || row.dead || row.model != configuredIdentity {
		t.Errorf("FLIP-1 = %+v, want embedded with a vector by %q", row, configuredIdentity)
	}
	if report, err := Coverage(ctx, database, configuredIdentity); err != nil || report.Total.Items != 1 || report.Total.Current != 1 {
		t.Errorf("coverage with the title real again = %+v, %v; want one item, current", report.Total, err)
	}
}

// An item whose text becomes empty with the trigger off (a restore) is still repaired: its row
// takes the empty text and is settled, so the vector of the text the item no longer holds goes
// and search cannot rank the item by it.
func TestCoverRepairsAnItemWhoseTextBecameEmptyWithTheTriggerOff(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "GONE", "GONE-1", "A title that will vanish")
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	drain(t, deps)
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `update issues set title = '' where key = 'GONE-1'`)
	})

	cover(t, deps)
	drain(t, deps)
	if len(embedder.calls) != 1 {
		t.Errorf("embedder calls = %q, want only the first title's", embedder.calls)
	}
	requireSettledWithNoVector(t, database, "issue", "GONE-1", configuredIdentity)
	var text string
	if err := database.Pool.QueryRow(context.Background(), `select text_snapshot from embeddings where kind = 'issue' and id = 'GONE-1'`).Scan(&text); err != nil || text != "" {
		t.Errorf("text_snapshot = %q, %v; want the item's empty text", text, err)
	}
}

// Settling never records a hash the text no longer has: a row whose empty text a write is
// replacing, the write's transaction still open, is skipped, and once the write commits the row
// is embedded at the new text.
func TestSettlingSkipsARowAWriteIsChanging(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "RACE", "RACE-1", "")
	embedder := &fakeEmbedder{}
	deps := Deps{Store: database, Embedder: embedder}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustExec(t, tx, `update issues set title = 'Written meanwhile' where key = 'RACE-1'`)
	succeeded, failed, _, _, err := ProcessBatch(ctx, deps)
	if err != nil || succeeded != 0 || failed != 0 || len(embedder.calls) != 0 {
		t.Fatalf("ProcessBatch during the write = %d, %d, %v, calls %q; want the locked row skipped", succeeded, failed, err, embedder.calls)
	}
	if row := readSettledRow(t, database, "issue", "RACE-1"); row.settled {
		t.Fatalf("RACE-1 = %+v, settled while a write held it", row)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	succeeded, failed, _, _, err = ProcessBatch(ctx, deps)
	if err != nil || succeeded != 1 || failed != 0 {
		t.Fatalf("ProcessBatch after the write = %d, %d, %v; want 1, 0", succeeded, failed, err)
	}
	if len(embedder.calls) != 1 || !slices.Equal(embedder.calls[0], []string{"Written meanwhile"}) {
		t.Errorf("embedder calls = %q, want one call embedding the written title", embedder.calls)
	}
	if row := readSettledRow(t, database, "issue", "RACE-1"); !row.settled || !row.hasVector {
		t.Errorf("RACE-1 = %+v, want embedded at the written title", row)
	}
}

// A row that died with an empty text before the queue settled such rows - under the configured
// identity, which readmitForeignDead leaves dead, or under none - is settled by the coverage pass
// with no model call: not dead, and not counted as failed or at all.
func TestCoverSettlesADeadRowWithNothingToEmbed(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "DBLK", "DBLK-1", "")
	seedIssue(t, database, "DBLK", "DBLK-2", whitespaceOnly)
	seedIssue(t, database, "DBLK", "DBLK-3", "A real title")
	mustExec(t, database.Pool, `update embeddings set dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $1 where id = 'DBLK-1'`, configuredIdentity)
	mustExec(t, database.Pool, `update embeddings set dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = null where id = 'DBLK-2'`)
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}

	cover(t, deps)
	drain(t, deps)
	requireNoBlankText(t, embedder.calls)
	requireSettledWithNoVector(t, database, "issue", "DBLK-1", configuredIdentity)
	requireSettledWithNoVector(t, database, "issue", "DBLK-2", configuredIdentity)
	report, err := Coverage(context.Background(), database, configuredIdentity)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if got := report.Total; got.Items != 1 || got.Current != 1 || got.Failed != 0 || got.Lacking != 0 {
		t.Errorf("total %+v, want the one real issue, current, and nothing failed", got)
	}
}
