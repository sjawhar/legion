package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// documentSearchTriggerFixture is one issue and one document with two versions, which migration
// 20261101182454's trigger has written into document_search: version 1 holds triggerOldWord and
// version 2 triggerWord. Each test below writes on from there.
type documentSearchTriggerFixture struct {
	handler  http.Handler
	database *store.Store
	issueKey string
	document string
}

const (
	triggerOldWord = "quagga"
	triggerWord    = "zebra"
)

func newDocumentSearchTriggerFixture(t *testing.T) documentSearchTriggerFixture {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TRIG", "Range survey", "The herd crosses in spring.")
	document := uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nQuagga sightings in the north.\n")
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nZebra sightings in the north.\n")
	return documentSearchTriggerFixture{handler: handler, database: database, issueKey: issue.Key, document: document.ID}
}

// documentSearchStored is one row of document_search as these tests read it: its version, that
// version's time, and its vector's lexemes, which Postgres unnests rather than the test parsing
// the vector's text form.
type documentSearchStored struct {
	number    int
	createdAt time.Time
	lexemes   []string
}

// readDocumentSearchRow reads a document's row, and whether it has one at all.
func readDocumentSearchRow(t *testing.T, database *store.Store, artifactID string) (documentSearchStored, bool) {
	t.Helper()
	var row documentSearchStored
	err := database.Pool.QueryRow(context.Background(), `
		select number, created_at, (select array_agg(u.lexeme order by u.lexeme) from unnest(search) u)
		  from document_search where artifact_id = $1
	`, artifactID).Scan(&row.number, &row.createdAt, &row.lexemes)
	if errors.Is(err, pgx.ErrNoRows) {
		return documentSearchStored{}, false
	}
	if err != nil {
		t.Fatalf("read document_search(%s): %v", artifactID, err)
	}
	return row, true
}

// requireDocumentSearchRow fails unless the document's row names version number and its vector
// holds holds and not lacks; either word empty asks nothing of it.
func requireDocumentSearchRow(t *testing.T, database *store.Store, artifactID string, number int, holds, lacks string) documentSearchStored {
	t.Helper()
	row, found := readDocumentSearchRow(t, database, artifactID)
	if !found {
		t.Fatalf("document %s has no document_search row", artifactID)
	}
	if row.number != number {
		t.Fatalf("document_search names version %d, want %d (lexemes %v)", row.number, number, row.lexemes)
	}
	if holds != "" && !slices.Contains(row.lexemes, holds) {
		t.Fatalf("document_search's lexemes %v do not hold %q", row.lexemes, holds)
	}
	if lacks != "" && slices.Contains(row.lexemes, lacks) {
		t.Fatalf("document_search's lexemes %v still hold %q", row.lexemes, lacks)
	}
	return row
}

// A3, the first case. A word only an old version of a document holds is in no version search
// reads, so the route answers nothing for it, and the document's row holds the latest version's
// words alone.
func TestAWordOnlyAnOldVersionHoldsFindsNothing(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	requireDocumentSearchRow(t, fixture.database, fixture.document, 2, triggerWord, triggerOldWord)
	if response := searchResponse(t, fixture.handler, "q="+triggerOldWord); len(response.Results) != 0 {
		t.Fatalf("search for %q answered %+v, want nothing: only the document's first version holds it",
			triggerOldWord, response.Results)
	}
	if response := searchResponse(t, fixture.handler, "q="+triggerWord); len(response.Results) != 1 ||
		response.Results[0].ID != fixture.document {
		t.Fatalf("search for %q answered %+v, want the document %s alone", triggerWord, response.Results, fixture.document)
	}
}

// A3, the second case. A version written the way the old release writes it - a plain insert,
// with no code of this release in the path - reaches document_search, because the trigger is
// what writes it. That is what keeps the table right while a build without this change is still
// serving.
func TestAVersionAnOlderBuildWritesReachesDocumentSearch(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	if _, err := fixture.database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 3, '# Field notes' || chr(10) || chr(10) || 'Oryx sightings in the north.' || chr(10), '[]')
	`, fixture.document); err != nil {
		t.Fatalf("write a version as an older build would: %v", err)
	}
	requireDocumentSearchRow(t, fixture.database, fixture.document, 3, "oryx", triggerWord)
}

// A3, the third case. An edit of the latest version's markdown - which the related-recall tests
// make, and which a repair would make - rewrites its vector (migration 0058's BEFORE trigger),
// and document_search follows it.
func TestAnEditOfTheLatestVersionUpdatesDocumentSearch(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	if _, err := fixture.database.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = '# Field notes' || chr(10) || chr(10) || 'Oryx sightings in the north.' || chr(10)
		 where artifact_id = $1 and number = 2
	`, fixture.document); err != nil {
		t.Fatalf("edit the latest version: %v", err)
	}
	requireDocumentSearchRow(t, fixture.database, fixture.document, 2, "oryx", triggerWord)
}

// A3, the fourth case. An edit of an older version leaves document_search as it was: the guard
// on the trigger's upsert refuses a row whose number is below the one already stored, so a
// repair of an old version cannot make search answer its words.
func TestAnEditOfAnOlderVersionLeavesDocumentSearchAsItWas(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	before := requireDocumentSearchRow(t, fixture.database, fixture.document, 2, triggerWord, "")
	if _, err := fixture.database.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = '# Field notes' || chr(10) || chr(10) || 'Oryx sightings in the north.' || chr(10)
		 where artifact_id = $1 and number = 1
	`, fixture.document); err != nil {
		t.Fatalf("edit the older version: %v", err)
	}
	after := requireDocumentSearchRow(t, fixture.database, fixture.document, 2, triggerWord, "oryx")
	if !after.createdAt.Equal(before.createdAt) || !slices.Equal(after.lexemes, before.lexemes) {
		t.Fatalf("document_search moved from %+v to %+v on an edit of an older version", before, after)
	}
}

// A3, the fifth case. A version whose transaction opened before a higher-numbered version
// existed, and committed after it, leaves the higher number in document_search: two writers of
// one document can commit in either order, and the guard, not the commit order, decides which
// version search reads.
func TestAnOlderVersionCommittedLastLeavesTheHigherNumber(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	ctx := context.Background()
	late, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the late writer's transaction: %v", err)
	}
	defer late.Rollback(ctx)
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 4, '# Field notes' || chr(10) || chr(10) || 'Oryx sightings in the north.' || chr(10), '[]')
	`, fixture.document); err != nil {
		t.Fatalf("write version 4: %v", err)
	}
	if _, err := late.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 3, '# Field notes' || chr(10) || chr(10) || 'Gemsbok sightings in the north.' || chr(10), '[]')
	`, fixture.document); err != nil {
		t.Fatalf("write version 3 in the transaction opened first: %v", err)
	}
	if err := late.Commit(ctx); err != nil {
		t.Fatalf("commit the late writer: %v", err)
	}
	requireDocumentSearchRow(t, fixture.database, fixture.document, 4, "oryx", "gemsbok")
}

// A3, the sixth case. A version with no markdown - an image or a file upload - has a null search
// vector (every function of migration 0058's expression is strict), so the trigger's WHEN clause
// keeps it out: a column list does not restrict an insert, and without the guard the highest
// number would blank the document's row.
func TestABinaryVersionLeavesTheDocumentsRowAsItWas(t *testing.T) {
	fixture := newDocumentSearchTriggerFixture(t)
	before := requireDocumentSearchRow(t, fixture.database, fixture.document, 2, triggerWord, "")
	if _, err := fixture.database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		values ($1, 3, '\x89504e47'::bytea, 'image/png', 4, repeat('0', 64), '[]')
	`, fixture.document); err != nil {
		t.Fatalf("write a version with no markdown: %v", err)
	}
	after := requireDocumentSearchRow(t, fixture.database, fixture.document, 2, triggerWord, "")
	if !after.createdAt.Equal(before.createdAt) || !slices.Equal(after.lexemes, before.lexemes) {
		t.Fatalf("document_search moved from %+v to %+v on a version with no markdown", before, after)
	}
}
