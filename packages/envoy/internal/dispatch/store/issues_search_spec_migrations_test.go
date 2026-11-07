package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The pair that puts an issue's primary document in the issue's own keyword vector:
// 20261105145242 redefines the issues trigger's function and re-indexes the issues stored before
// it, and 20261105145243 adds the trigger on artifact_versions that keeps the vector current.
const (
	beforeIssuesSearchSpec = 20261103070013
	issuesSearchSpec       = 20261105145242
	issuesSearchSpecTrig   = 20261105145243
)

// seedIssueWithSpec writes one issue, its primary document and one version of it, at whatever
// schema the caller has migrated to, and returns the issue's key.
func seedIssueWithSpec(t *testing.T, ctx context.Context, store *Store, number int, title, spec string) string {
	t.Helper()
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core') on conflict do nothing
	`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	key := fmt.Sprintf("CORE-%d", number)
	if _, err := store.Pool.Exec(ctx, `
		with i as (
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'CORE', $2, $3, '{"kind":"user","id":"alice"}', $4) returning key
		),
		a as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select $1, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id
		)
		insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, $5, '[]' from a
	`, key, number, title, fmt.Sprintf("%d", number), spec); err != nil {
		t.Fatalf("seed issue %s: %v", key, err)
	}
	return key
}

// matchesIssue reads whether one issue's keyword vector matches query.
func matchesIssue(t *testing.T, ctx context.Context, store *Store, key, query string) bool {
	t.Helper()
	var matches bool
	if err := store.Pool.QueryRow(ctx,
		`select search @@ websearch_to_tsquery('english', $2) from issues where key = $1`, key, query,
	).Scan(&matches); err != nil {
		t.Fatalf("match %q against %s: %v", query, key, err)
	}
	return matches
}

// rankIssue reads ts_rank_cd of one issue's keyword vector against query, the rank api/search.go
// orders the issue arm by.
func rankIssue(t *testing.T, ctx context.Context, store *Store, key, query string) float32 {
	t.Helper()
	var rank float32
	if err := store.Pool.QueryRow(ctx,
		`select ts_rank_cd(search, websearch_to_tsquery('english', $2)) from issues where key = $1`, key, query,
	).Scan(&rank); err != nil {
		t.Fatalf("rank %q against %s: %v", query, key, err)
	}
	return rank
}

// 20261105145242 on a database that already holds issues: an issue whose title shares no word
// with its primary document is found by that document's words once the migration has re-indexed
// it, and the words of its key and title still outrank them, so the spec rides at a lower weight
// rather than at the title's.
func TestIssuesSearchSpecMigrationReindexesTheIssuesStoredBeforeIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeIssuesSearchSpec)
	const spec = "The rhumb lines of the Tyrrhenian portolan cross at Capo Testa.\n"
	key := seedIssueWithSpec(t, ctx, store, 1, "Tide tables for the equinox", spec)
	bare := seedIssueWithSpec(t, ctx, store, 2, "Sailing directions", "   \n")

	if matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Fatal("the issue already matched a word only its spec holds before the migration")
	}
	var beforeBare string
	if err := store.Pool.QueryRow(ctx, `select search::text from issues where key = $1`, bare).Scan(&beforeBare); err != nil {
		t.Fatalf("read the spec-less issue's vector: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply the issue-spec search migrations: %v", err)
	}

	if !matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Error("the re-index did not give the issue the words of its primary document")
	}
	if !matchesIssue(t, ctx, store, key, "equinox") {
		t.Error("the re-index dropped the issue's own title from its vector")
	}
	if spec, title := rankIssue(t, ctx, store, key, "Tyrrhenian"), rankIssue(t, ctx, store, key, "equinox"); spec >= title {
		t.Errorf("a spec word ranks %v and a title word %v on the same issue; the spec must rank below the title", spec, title)
	}
	var afterBare string
	if err := store.Pool.QueryRow(ctx, `select search::text from issues where key = $1`, bare).Scan(&afterBare); err != nil {
		t.Fatalf("read the spec-less issue's vector: %v", err)
	}
	if afterBare != beforeBare {
		t.Errorf("an issue whose document holds no text was re-indexed:\n before %s\n after  %s", beforeBare, afterBare)
	}
}

// 20261105145243's trigger re-indexes an issue when its primary document gains a version, which
// is the only write that changes the issue's vector without touching the issue's own row.
func TestTheIssuesSearchTriggerIndexesAVersionWrittenAfterIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := seedIssueWithSpec(t, ctx, store, 1, "Tide tables for the equinox", "Nothing of note.\n")
	if matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Fatal("the issue matched a word no version of its document holds")
	}

	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select a.id, 2, 'The rhumb lines of the Tyrrhenian portolan cross at Capo Testa.', '[]'
		from artifacts a where a.issue_key = $1 and a.is_primary
	`, key); err != nil {
		t.Fatalf("write a second version: %v", err)
	}

	if !matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Error("the version's words did not reach the issue's vector")
	}
	if matchesIssue(t, ctx, store, key, "Nothing of note") {
		t.Error("the issue's vector still holds the version the new one replaced")
	}
}

// 20261105145243 is a function and a trigger and nothing else, which is what lets it ship: the
// pre-deploy census refuses a migration that locks a table above its size limit, and the trigger's
// table, artifact_versions, is 5.2 GiB in production. Neither statement reads a row, so the time
// the migration holds its lock does not grow with the table and the limit does not apply to it
// (pgmigrate tableSizeExemption). This is why the re-index of the rows already stored is
// 20261105145242's and not this migration's: an update is not such a statement, and one in this
// file would cost the exemption and refuse every deploy carrying it. The limit is injected at one
// byte so the test needs no gigabyte.
func TestCensusExemptsTheIssuesSearchTriggerFromTheTableLimit(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, issuesSearchSpec)
	report, err := census(context.Background(), store.Pool.Config().ConnString(),
		migrationsThrough(t, issuesSearchSpecTrig), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the issues-search trigger was refused: %v", refusals(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want 20261105145243 alone, exempt as a function and a trigger", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches artifact_versions: ") ||
		!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the two migrations locks above ACCESS SHARE, read as the census reads it. The
// re-index names issues alone: it reads each issue's latest primary version through 0140's
// issue_embedding_spec, under ACCESS SHARE, which the census leaves out of a migration's tables on
// purpose, so no deploy is refused over the size of artifact_versions.
func TestTheIssuesSearchSpecMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name string
		file string
		want []string
	}{
		{"20261105145242, the expression and the re-index", "20261105145242_issues_search_spec.up.sql", []string{"issues"}},
		{"20261105145243, the function and the trigger", "20261105145243_artifact_versions_issues_search_trigger.up.sql", []string{"artifact_versions"}},
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
