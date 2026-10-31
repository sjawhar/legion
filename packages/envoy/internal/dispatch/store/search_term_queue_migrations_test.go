package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The migrations of the related-terms refresh's queue these tests name: 0158 adds the snapshot
// and the snapshot_at a pass of this build writes beside a project's counts, and 0159 adds the
// five triggers that fill the queue 0157 creates.
const (
	searchTermTotalsColumns = 158
	searchTermQueueTriggers = 159
)

// 0159 is five functions and five triggers and nothing else, which is what lets one migration
// carry all five: the pre-deploy census refuses any migration that locks a table above its size
// limit, and the exemption that lifts the limit is per migration, not per table. No statement in
// it reads a row, so the time it holds its locks does not grow with any of the five tables
// (pgmigrate tableSizeExemption). The limit is injected at one byte so the test needs no
// gigabyte, over a database at 0158, where the item tables are already above that.
func TestCensusExemptsTheSearchTermQueueTriggersFromTheTableLimit(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, searchTermTotalsColumns)
	report, err := census(context.Background(), store.Pool.Config().ConnString(),
		migrationsThrough(t, searchTermQueueTriggers), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the search term queue triggers were refused: %v", refusals(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want 0159 alone, exempt as functions and triggers", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches comments: ") ||
		!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the three migrations locks above ACCESS SHARE, read as the census reads it. The
// queue's own migration is one create table, which names no table a session could already hold,
// and it declares no foreign key, which would have named the table it points at; the columns
// migration touches the one small table it alters; and the triggers migration touches the four
// item tables and document_search, which is the lock window the test below holds to an order.
func TestTheSearchTermQueueMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name string
		file string
		want []string
	}{
		{"0157, the queue", "0157_search_term_queue.up.sql", nil},
		{"0158, the snapshot columns", "0158_search_term_totals_snapshot.up.sql", []string{"search_term_totals"}},
		{
			"0159, the five triggers", "0159_search_term_queue_triggers.up.sql",
			[]string{"asks", "comments", "document_search", "issues", "messages"},
		},
	} {
		t.Run(migration.name, func(t *testing.T) {
			sql, err := migrationFiles.ReadFile("migrations/" + migration.file)
			if err != nil {
				t.Fatalf("read %s: %v", migration.file, err)
			}
			if touched := pgmigrate.TouchedTables(string(sql)); !slices.Equal(touched, migration.want) {
				t.Fatalf("%s touches %v, want %v", migration.file, touched, migration.want)
			}
		})
	}
}

// 0159 takes its five locks in one fixed order - asks, comments, issues, messages, then
// document_search - so that one transaction takes the five in one window instead of five
// migrations taking one each, and always in that order, so two migrations written to this rule
// cannot each hold a lock the other waits for. The order is the migration's own claim about
// itself, and this holds the file to it.
func TestTheSearchTermQueueTriggersLockTheirTablesInOneOrder(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/0159_search_term_queue_triggers.up.sql")
	if err != nil {
		t.Fatalf("read 0159: %v", err)
	}
	text := string(sql)
	placed := -1
	for _, trigger := range []string{
		"asks_search_term_queue",
		"comments_search_term_queue",
		"issues_search_term_queue",
		"messages_search_term_queue",
		"document_search_term_queue",
	} {
		at := strings.Index(text, "create trigger "+trigger+" ")
		if at < 0 {
			t.Fatalf("0159 creates no trigger %s", trigger)
		}
		if at < placed {
			t.Fatalf("0159 creates its trigger %s at %d, before the one above it at %d", trigger, at, placed)
		}
		placed = at
	}
}
