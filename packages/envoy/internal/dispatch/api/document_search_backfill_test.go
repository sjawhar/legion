package api

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The three migrations of the document-search table, read from the store's own directory rather
// than copied here, so this test applies the text that ships.
const (
	documentSearchTableMigration    = "0162_document_search.up.sql"
	documentSearchTriggerMigration  = "0163_artifact_versions_document_search_trigger.up.sql"
	documentSearchBackfillMigration = "0164_document_search_backfill.up.sql"
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

// A4. The backfill, run the way a deploy runs it. The three migrations are undone, which leaves
// the schema the one before 0162 - they are the last three of the set and nothing else depends
// on them - and the documents are then written as a build without this change writes them, with
// no table to fill. 0162 and 0163 are applied, one more version is written in the window between
// 0163 and 0164, which the trigger catches, and 0164 fills the rest. Search then answers what
// main's own statement answers over the same rows, which is what says the backfilled table holds
// each document's latest version and no older one.
func TestTheBackfillLeavesSearchAnsweringWhatMainsStatementDoes(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	// The issue is created first: creating one runs the related-item check, which reads the
	// table these statements drop.
	issue := createInteractionIssue(t, handler, "FILL", "Range survey", "The herd crosses the plateau in spring.")
	for _, statement := range []string{
		"drop trigger artifact_versions_document_search on artifact_versions",
		"drop function artifact_versions_document_search()",
		"drop table document_search",
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	// Documents with several versions, written while no table and no trigger exist, one of them
	// holding a word in its first version alone.
	notes := uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nQuagga sightings in the north.\n")
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nZebra sightings in the north.\n")
	uploadIssueDocument(t, handler, issue.Key, "field-notes.md", "# Field notes\n\nZebra sightings on the plateau.\n")
	atlas := uploadIssueDocument(t, handler, issue.Key, "atlas.md", "# Atlas\n\nOryx ranges of the plateau.\n")

	applyStoreMigration(t, database, documentSearchTableMigration)
	applyStoreMigration(t, database, documentSearchTriggerMigration)
	if _, found := readDocumentSearchRow(t, database, notes.ID); found {
		t.Fatal("document_search holds the field notes before the backfill ran")
	}

	// The window between the trigger and the backfill: this version is in the table through the
	// trigger, and the backfill must not replace it with an older one.
	uploadIssueDocument(t, handler, issue.Key, "atlas.md", "# Atlas\n\nZebra ranges of the plateau.\n")
	requireDocumentSearchRow(t, database, atlas.ID, 2, "zebra", "oryx")

	applyStoreMigration(t, database, documentSearchBackfillMigration)
	requireDocumentSearchRow(t, database, notes.ID, 3, "plateau", "quagga")
	requireDocumentSearchRow(t, database, atlas.ID, 2, "zebra", "oryx")
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the backfilled database: %v", err)
	}

	for _, word := range []string{"zebra", "plateau", "quagga"} {
		t.Run(word, func(t *testing.T) {
			arguments := []any{word, "FILL", searchDefaultLimit, firstTerm(word), headlineOptions,
				0, contracts.SearchKindDepth, searchFusionK}
			want := documentSearchRows(t, ctx, database, mainSearchQuery, arguments, nil)
			got := documentSearchRows(t, ctx, database, searchQuery, arguments, nil)
			if !slices.Equal(got, want) {
				t.Fatalf("search for %q over the backfilled table answered\n%v\nwant main's rows\n%v", word, got, want)
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
