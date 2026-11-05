package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The three migrations of the document-search table: 20261101182447 creates it with its GIN
// index, 20261101182454 adds the function and the trigger that keep it at each document's latest
// version, and 20261101182501 creates the one row that records how far the boot's fill of it has
// got (store.FillDocumentSearch).
const (
	documentSearchTable    = 20261101182447
	documentSearchTrigger  = 20261101182454
	documentSearchBackfill = 20261101182501
)

// 20261101182454 is a function and a trigger and nothing else, which is what lets it ship: the
// pre-deploy census refuses any migration that locks a table above its size limit, and the
// trigger's table, artifact_versions, is 5.2 GiB in production. Neither statement reads a row,
// so the time the migration holds its lock does not grow with the table and the limit does not
// apply to it (pgmigrate tableSizeExemption). The limit is injected at one byte so the test
// needs no gigabyte, over a database at 20261101182447, where artifact_versions is already above
// that.
func TestCensusExemptsTheDocumentSearchTriggerFromTheTableLimit(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, documentSearchTable)
	report, err := census(context.Background(), store.Pool.Config().ConnString(),
		migrationsThrough(t, documentSearchTrigger), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the document-search trigger was refused: %v", refusals(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want 20261101182454 alone, exempt as a function and a trigger", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches artifact_versions: ") ||
		!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the three migrations locks above ACCESS SHARE, read as the census reads it. The
// table's own migration declares no foreign key, since a `references` to artifacts or to
// artifact_versions would have the census size that table and refuse every deploy; the fill's
// progress table names neither of them, because the fill reads artifact_versions outside every
// migration. The trigger's migration is the one that touches artifact_versions, and the test
// above is why it may.
func TestTheDocumentSearchMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name  string
		files string
		want  []string
	}{
		{"20261101182447, the table and its index", "20261101182447_document_search.up.sql", []string{"document_search"}},
		{"20261101182454, the function and the trigger", "20261101182454_artifact_versions_document_search_trigger.up.sql", []string{"artifact_versions"}},
		{"20261101182501, the fill's progress row", "20261101182501_document_search_backfill.up.sql", []string{"document_search_backfill"}},
	} {
		t.Run(migration.name, func(t *testing.T) {
			sql, err := migrationFiles.ReadFile("migrations/" + migration.files)
			if err != nil {
				t.Fatalf("read %s: %v", migration.files, err)
			}
			if touched := pgmigrate.TouchedTables(string(sql)); !slices.Equal(touched, migration.want) {
				t.Fatalf("%s touches %v, want %v", migration.files, touched, migration.want)
			}
		})
	}
}
