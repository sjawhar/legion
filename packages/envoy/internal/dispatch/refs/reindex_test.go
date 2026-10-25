package refs

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// owedRows is every refs_owed row q sees, as "kind id", sorted.
func owedRows(t *testing.T, q Queryer) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `select kind || ' ' || id from refs_owed`)
	if err != nil {
		t.Fatalf("read refs_owed: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read refs_owed: %v", err)
	}
	sort.Strings(owed)
	return owed
}

// A write that indexes its body through ReplaceCounted is not owed a re-index afterwards: the row
// its own trigger made is deleted in the same transaction. Another source's row stays.
func TestReplaceCountedSettlesItsOwnSourcesOwedReindex(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	indexed := createReferenceComment(t, tx, "ACME-1")
	other := createReferenceComment(t, tx, "ACME-1")
	if got, want := owedRows(t, tx), []string{"comment " + indexed, "comment " + other}; !reflect.DeepEqual(got, sortedStrings(want)) {
		t.Fatalf("owed after two comment inserts = %v; want %v", got, want)
	}

	if _, err := ReplaceCounted(ctx, tx, "comment", indexed, "Blocked on ACME-1.", "https://dispatch.example"); err != nil {
		t.Fatalf("index the comment: %v", err)
	}
	if got := owedRows(t, tx); !reflect.DeepEqual(got, []string{"comment " + other}) {
		t.Fatalf("owed after indexing one comment = %v; want only the other", got)
	}
}

func sortedStrings(values []string) []string {
	sort.Strings(values)
	return values
}

// rebuildFixture writes, by raw SQL as an earlier release does, a comment edited after it was
// written and a document version, each naming ACME-2 bare, and returns their ids and write times.
func rebuildFixture(t *testing.T, database *store.Store) (comment, document string, commentAt, versionAt time.Time) {
	t.Helper()
	ctx := context.Background()
	commentAt = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	versionAt = time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('ACME-2', 'ACME', 2, 'Second', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create ACME-2: %v", err)
	}
	comment = insertScopeRow(t, tx, "comment", `
		insert into comments (issue_key, author, body, created_at, edited_at)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'Blocked on ACME-2.', $1::timestamptz - interval '1 day', $1)
		returning id::text
	`, commentAt)
	document = insertScopeDocument(t, tx, "ACME-1", "plan", false)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, 'After ACME-2.', $2)
	`, document, versionAt); err != nil {
		t.Fatalf("insert the version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return comment, document, commentAt, versionAt
}

// The rebuild clears the owed row of every source it re-indexes, and an edge it adds is dated at
// the source's own write time - a comment's latest edit, a version's creation - with no source_seq,
// so old text does not sort as new under "Referenced by". A scope row, which names no source of its
// own, stays for the drain.
func TestRebuildAllClearsWhatItReindexesAndDatesNewEdgesAtTheWrite(t *testing.T) {
	database := storetest.Open(t)
	comment, document, commentAt, versionAt := rebuildFixture(t, database)
	if got := owedRows(t, database.Pool); !reflect.DeepEqual(got, sortedStrings([]string{"comment " + comment, "artifact " + document, "scope " + document})) {
		t.Fatalf("owed before the rebuild = %v", got)
	}

	if _, err := RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := owedRows(t, database.Pool); !reflect.DeepEqual(got, []string{"scope " + document}) {
		t.Fatalf("owed after the rebuild = %v; want the document's scope alone", got)
	}
	for _, source := range []struct {
		kind, id string
		at       time.Time
	}{{"comment", comment, commentAt}, {"artifact", document, versionAt}} {
		got := readProvenance(t, database, source.kind, source.id)
		if len(got) != 1 || got[0].ToKind != "issue" || got[0].ToID != "ACME-2" || got[0].SourceSeq != nil || !got[0].CreatedAt.Equal(source.at) {
			t.Errorf("%s edges = %#v; want one to ACME-2 created at %v with no source_seq", source.kind, got, source.at)
		}
	}
}

// Reindex reads a source as its live write indexes it and dates what it adds at that write; a
// source that is gone, or one no write indexes, loses its edges.
func TestReindexDropsTheEdgesOfASourceThatIsGone(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	direct := insertScopeRow(t, tx, "direct message", `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'ACME-1', 'session:s1')
		returning id::text
	`)
	gone := "00000000-0000-0000-0000-000000000001"
	insertReference(t, tx, "comment", gone, "issue", "ACME-1")
	insertReference(t, tx, "message", direct, "issue", "ACME-1")
	for _, source := range []struct{ kind, id string }{{"comment", gone}, {"message", direct}} {
		loaded, err := LoadSource(ctx, tx, source.kind, source.id)
		if err != nil {
			t.Fatalf("load %s: %v", source.kind, err)
		}
		if loaded.Exists {
			t.Fatalf("%s %s loaded as %#v; want it absent", source.kind, source.id, loaded)
		}
		if err := Reindex(ctx, tx, loaded, "https://dispatch.example"); err != nil {
			t.Fatalf("re-index %s: %v", source.kind, err)
		}
		var edges int
		if err := tx.QueryRow(ctx, `select count(*) from refs where from_kind = $1 and from_id = $2`, source.kind, source.id).Scan(&edges); err != nil {
			t.Fatal(err)
		}
		if edges != 0 {
			t.Errorf("%s %s keeps %d edges; want none", source.kind, source.id, edges)
		}
	}
}
