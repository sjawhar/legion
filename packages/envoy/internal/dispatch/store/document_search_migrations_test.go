package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The three migrations of the document-search table: 0162 creates it with its GIN index, 0163
// adds the function and the trigger that keep it at each document's latest version, and 0164
// fills it for the documents already stored.
const (
	documentSearchTable    = 162
	documentSearchTrigger  = 163
	documentSearchBackfill = 164
)

// 0163 is a function and a trigger and nothing else, which is what lets it ship: the pre-deploy
// census refuses any migration that locks a table above its size limit, and the trigger's table,
// artifact_versions, is 5.2 GiB in production. Neither statement reads a row, so the time the
// migration holds its lock does not grow with the table and the limit does not apply to it
// (pgmigrate tableSizeExemption). The limit is injected at one byte so the test needs no
// gigabyte, over a database at 0162, where artifact_versions is already above that.
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
		t.Errorf("pending = %+v, want 0163 alone, exempt as a function and a trigger", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches artifact_versions: ") ||
		!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the three migrations locks above ACCESS SHARE, read as the census reads it. The
// table's own migration declares no foreign key, since a `references` to artifacts or to
// artifact_versions would have the census size that table and refuse every deploy; the backfill
// only reads artifact_versions, which the census does not size either. The trigger's migration is
// the one that touches artifact_versions, and the test above is why it may.
func TestTheDocumentSearchMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name  string
		files string
		want  []string
	}{
		{"0162, the table and its index", "0162_document_search.up.sql", []string{"document_search"}},
		{"0163, the function and the trigger", "0163_artifact_versions_document_search_trigger.up.sql", []string{"artifact_versions"}},
		{"0164, the backfill", "0164_document_search_backfill.up.sql", []string{"document_search"}},
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
