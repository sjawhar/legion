package api

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two migrations the fill runs behind, read from the store's own directory rather than
// copied here, so this test applies the text that ships. The fill itself is no migration:
// 20261101182501 creates the one row that records how far it has got, and
// store.FillDocumentSearch runs it in committed batches at boot, after every migration and
// before the server serves (cmd/dispatch/main.go).
const (
	documentSearchTableMigration   = "20261101182447_document_search.up.sql"
	documentSearchTriggerMigration = "20261101182454_artifact_versions_document_search_trigger.up.sql"
)

// applyStoreMigration applies one migration of the store's set, as the runner applies it: the
// whole file in one statement batch.
func applyStoreMigration(t *testing.T, database *store.Store, name string) {
	t.Helper()
	sql, err := os.ReadFile("../store/migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := database.Pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

// undoDocumentSearch drops 20261101182447's table and 20261101182454's function and trigger,
// which leaves the schema the one before 20261101182447 and lets a test write documents as a
// build without this change writes them, with no table to fill. 20261101182501's progress row
// stands: a database that has never run a pass holds it as the migration wrote it, which is the
// state the fill starts from.
func undoDocumentSearch(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		"drop trigger artifact_versions_document_search on artifact_versions",
		"drop function artifact_versions_document_search()",
		"drop table document_search",
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// fillDocumentSearch runs the boot's fill and answers the rows it wrote.
func fillDocumentSearch(t *testing.T, database *store.Store) int64 {
	t.Helper()
	written, err := database.FillDocumentSearch(context.Background())
	if err != nil {
		t.Fatalf("fill document_search: %v", err)
	}
	return written
}

// documentsWithMarkdown counts the documents the fill covers: an artifact with at least one
// version carrying markdown.
func documentsWithMarkdown(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var documents int64
	if err := database.Pool.QueryRow(context.Background(),
		"select count(distinct artifact_id) from artifact_versions where markdown is not null").Scan(&documents); err != nil {
		t.Fatalf("count the documents to fill: %v", err)
	}
	return documents
}

// countRows answers the one count statement asks of the test's database.
func countRows(t *testing.T, database *store.Store, statement string) int64 {
	t.Helper()
	var count int64
	if err := database.Pool.QueryRow(context.Background(), statement).Scan(&count); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return count
}

// A4. The fill, run the way a boot runs it. The three migrations of the document-search table
// are undone, and the documents are then written as a build without this change writes them,
// with no table to fill. 20261101182447 and 20261101182454 are applied, one more version is
// written in the window between 20261101182454 and the fill, which the trigger catches, and the
// fill writes the rest. Search then answers what main's own statement answers over the same
// rows, which is what says the filled table holds each document's latest version and no older
// one.
func TestTheBackfillLeavesSearchAnsweringWhatMainsStatementDoes(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	// The issue is created first: creating one runs the related-item check, which reads the
	// table undoDocumentSearch drops.
	issue := createInteractionIssue(t, handler, "FILL", "Range survey", "The herd crosses the plateau in spring.")
	undoDocumentSearch(t, database)

	// Documents with several versions, written while no table and no trigger exist, one of them
	// holding a word in its first version alone.
	notes := uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nQuagga sightings in the north.\n")
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nZebra sightings in the north.\n")
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nZebra sightings on the plateau.\n")
	atlas := uploadIssueDocument(t, handler, issue.Key, "atlas.md", "# Atlas\n\nOryx ranges of the plateau.\n")

	applyStoreMigration(t, database, documentSearchTableMigration)
	applyStoreMigration(t, database, documentSearchTriggerMigration)
	if _, found := readDocumentSearchRow(t, database, notes.ID); found {
		t.Fatal("document_search holds the field notes before the fill ran")
	}

	// The window between the trigger and the fill: this version is in the table through the
	// trigger, and the fill must not replace it with an older one.
	uploadIssueDocument(t, handler, issue.Key, "atlas.md", "# Atlas\n\nZebra ranges of the plateau.\n")
	requireDocumentSearchRow(t, database, atlas.ID, 2, "zebra", "oryx")

	if written := fillDocumentSearch(t, database); written == 0 {
		t.Fatal("the fill wrote no row, so it filled nothing")
	}
	requireDocumentSearchRow(t, database, notes.ID, 3, "plateau", "quagga")
	requireDocumentSearchRow(t, database, atlas.ID, 2, "zebra", "oryx")
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the filled database: %v", err)
	}

	for _, word := range []string{"zebra", "plateau", "quagga"} {
		t.Run(word, func(t *testing.T) {
			arguments := []any{word, "FILL", searchDefaultLimit, firstTerm(word), headlineOptions,
				0, contracts.SearchKindDepth, searchFusionK}
			want := documentSearchRows(t, ctx, database, mainSearchQuery, arguments, nil)
			got := documentSearchRows(t, ctx, database, searchQuery, arguments, nil)
			if !slices.Equal(got, want) {
				t.Fatalf("search for %q over the filled table answered\n%v\nwant main's rows\n%v", word, got, want)
			}
			answer := strings.Join(want, "\n")
			switch word {
			case "quagga":
				if strings.Contains(answer, notes.ID) {
					t.Fatalf("search for %q named the field notes, whose first version alone holds it:\n%v", word, want)
				}
			default:
				if !strings.Contains(answer, notes.ID) || !strings.Contains(answer, atlas.ID) {
					t.Fatalf("search for %q named neither document, so the comparison decided nothing:\n%v", word, want)
				}
			}
		})
	}
}

// A pass over more documents than one batch holds leaves every document in the table, at its
// latest version. The documents are seeded as rows rather than written through the API, because
// what this case is about is the count: store.DocumentSearchFillBatch of them plus two, so the
// keyset takes a full batch, a partial one, and then the empty batch that says the pass is
// finished. The statements are counted on the pool the fill runs on, so the batches are read
// rather than assumed.
func TestTheFillCoversEveryDocumentOverMoreThanOneBatch(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "BATCH", "Range survey", "The herd crosses the plateau in spring.")
	undoDocumentSearch(t, database)

	seeded := store.DocumentSearchFillBatch + 2
	if _, err := database.Pool.Exec(ctx, `
		with seeded as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select $1, 'BATCH', 'seeded-' || n, 'seeded-' || n || '.md', 'doc', false,
				'{"kind":"user","id":"alice"}'
			from generate_series(1, $2) n
			returning id
		)
		insert into artifact_versions (artifact_id, number, markdown)
		select seeded.id, version.number,
			case version.number when 1 then 'Quagga sightings in the north.' else 'Zebra sightings on the plateau.' end
		from seeded, generate_series(1, 2) version(number)
	`, issue.Key, seeded); err != nil {
		t.Fatalf("seed %d documents: %v", seeded, err)
	}

	applyStoreMigration(t, database, documentSearchTableMigration)
	applyStoreMigration(t, database, documentSearchTriggerMigration)

	documents := documentsWithMarkdown(t, database)
	if documents <= int64(store.DocumentSearchFillBatch) {
		t.Fatalf("%d documents to fill, want more than one batch of %d", documents, store.DocumentSearchFillBatch)
	}
	recorder := &storetest.StatementRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	written, err := traced.FillDocumentSearch(ctx)
	if err != nil {
		t.Fatalf("fill document_search over %d documents: %v", documents, err)
	}
	if written != documents {
		t.Fatalf("the fill wrote %d rows, want one for each of the %d documents", written, documents)
	}

	// One batch for each full batch of documents, one for the two left over, and the empty
	// batch that takes no further id and marks the pass finished.
	wantBatches := int(documents)/store.DocumentSearchFillBatch + 1
	if int(documents)%store.DocumentSearchFillBatch != 0 {
		wantBatches++
	}
	var batches, reads int
	for _, statement := range recorder.Statements() {
		switch {
		case strings.Contains(statement, "insert into document_search (artifact_id"):
			batches++
		case strings.Contains(statement, "select done from document_search_backfill"):
			reads++
		}
	}
	if batches != wantBatches {
		t.Fatalf("the fill ran %d batches over %d documents, want %d", batches, documents, wantBatches)
	}
	if reads != 1 {
		t.Fatalf("the fill read its progress row %d times, want once", reads)
	}

	if rows := countRows(t, database, "select count(*) from document_search"); rows != documents {
		t.Fatalf("document_search holds %d rows after the fill, want %d, one for each document", rows, documents)
	}
	behind := countRows(t, database, `
		select count(*) from document_search d
		 where d.number <> (select max(v.number) from artifact_versions v
		                     where v.artifact_id = d.artifact_id and v.markdown is not null)
	`)
	if behind != 0 {
		t.Fatalf("%d of the filled rows name a version that is not their document's latest", behind)
	}
	if done := countRows(t, database, "select count(*) from document_search_backfill where done"); done != 1 {
		t.Fatal("the pass did not record itself finished")
	}
}

// The write guard under batching. A version that commits while the pass runs is in the table
// through 20261101182454's trigger, at a number above the one the running batch read, and the
// batch must leave it there. The version write is held open until the batch's insert has queued
// behind its uncommitted row, which is the order of that race: the batch read artifact_versions
// before the write committed, so the row it carries is the older version.
func TestAVersionCommittedWhileTheFillRunsIsNotReplacedByTheOlderOneItRead(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "RACE", "Range survey", "The herd crosses the plateau in spring.")
	undoDocumentSearch(t, database)
	notes := uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nQuagga sightings in the north.\n")
	applyStoreMigration(t, database, documentSearchTableMigration)
	applyStoreMigration(t, database, documentSearchTriggerMigration)
	// Every document of the issue but this one, whose row the fill must leave alone: the
	// issue's own spec document is a document the fill writes too.
	documents := documentsWithMarkdown(t, database)

	writing, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer writing.Rollback(ctx)
	if _, err := writing.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown)
		values ($1, 2, 'Zebra sightings on the plateau.')
	`, notes.ID); err != nil {
		t.Fatalf("write the second version: %v", err)
	}

	type filled struct {
		written int64
		err     error
	}
	done := make(chan filled, 1)
	go func() {
		written, err := database.FillDocumentSearch(context.Background())
		done <- filled{written: written, err: err}
	}()
	awaitFillQueued(t, database)
	if err := writing.Commit(ctx); err != nil {
		t.Fatalf("commit the version write: %v", err)
	}

	select {
	case answer := <-done:
		if answer.err != nil {
			t.Fatalf("fill document_search beside a committing version write: %v", answer.err)
		}
		if answer.written != documents-1 {
			t.Fatalf("the fill wrote %d rows over %d documents, want one for every document but the raced one, whose row names a higher version than the fill read",
				answer.written, documents)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fill did not finish once the version write committed")
	}
	requireDocumentSearchRow(t, database, notes.ID, 2, "zebra", "quagga")
}

// awaitFillQueued waits for the fill's batch statement to queue behind a lock, which is what
// says the batch has read artifact_versions and reached its own insert. The statement's text
// names it, so no other session queued on the same lock is taken for it.
func awaitFillQueued(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%document_search_backfill for update%")
		if err != nil {
			t.Fatalf("read the sessions waiting on a lock: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the fill's batch never queued behind the open version write")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The cheap boot: once the pass is finished, a run reads the progress row and nothing else. The
// statements are read off the pool the second run is given, so "reads no large table" is counted
// rather than reasoned about.
func TestAFillAfterThePassIsFinishedReadsOneRowAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	createInteractionIssue(t, handler, "BOOT", "Range survey", "The herd crosses the plateau in spring.")
	if written := fillDocumentSearch(t, database); written == 0 {
		t.Fatal("the first fill wrote no row, so the second run proves nothing")
	}
	rows := countRows(t, database, "select count(*) from document_search")

	recorder := &storetest.StatementRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	written, err := traced.FillDocumentSearch(ctx)
	if err != nil {
		t.Fatalf("fill document_search a second time: %v", err)
	}
	if written != 0 {
		t.Fatalf("the second fill wrote %d rows, want none", written)
	}
	statements := recorder.Statements()
	if len(statements) != 1 || !strings.Contains(statements[0], "document_search_backfill") {
		t.Fatalf("the second fill ran %v, want the progress row's read alone", statements)
	}
	for _, statement := range statements {
		if strings.Contains(statement, "artifact_versions") {
			t.Fatalf("the second fill read artifact_versions: %q", statement)
		}
	}
	if after := countRows(t, database, "select count(*) from document_search"); after != rows {
		t.Fatalf("document_search holds %d rows after the second fill, want the %d it held", after, rows)
	}
}
