package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The term-statistics build of the release before migration 0166, and the choice between it and
// this release's. cmd/dispatch-seed ends a seed with one build for every project, and the
// measurement protocol runs that tool with --no-migrate against a database migrated from main as
// well as against one migrated to this head. Main's search_term_totals carries no snapshot
// column and its schema no search_term_queue, so this release's build reads columns and tables
// that are not there: every project of such a database would fail, and the seed would report a
// data set whose term statistics were never built.
//
// The statements below are a literal copy of main's refreshSearchTermStats, nothing built from
// this release's own constants. A copy that shared a constant with the statements it is compared
// against (searchTermItemsSQL, searchTermStatsStep, searchTermStatsRebuild) would move with them,
// and the equality tests of related_terms_refresh_test.go, which hold this release's build to
// what main's writes, would hold nothing. They are both the build cmd/dispatch-seed runs on
// main's schema and the oracle those tests read (runMainSearchTermStatsRebuild), so the text the
// seed runs and the text the tests call main's cannot drift apart.

// searchTermItemsBeforeSnapshotsSQL is main's searchTermItemsSQL, which {project} stands in for
// the project key of as it does there. It differs from this release's in its document arm alone,
// which reads every artifact's latest version through a lateral join over artifact_versions where
// this release's reads document_search (migration 0162): a database at main's schema has no
// document_search, and one at this head answers the same rows through either.
const searchTermItemsBeforeSnapshotsSQL = `
select i.search from issues i where i.project_key = {project}
union all
select v.search from artifacts a
  join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
 where a.kind = 'doc' and a.project_key = {project}
union all
select c.search from comments c
  left join issues i on i.key = c.issue_key
  left join artifacts a on a.id = c.artifact_id
 where coalesce(i.project_key, a.project_key) = {project}
union all
select k.search from asks k
  left join issues i on i.key = k.issue_key
  left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
 where coalesce(i.project_key, a.project_key) = {project}
union all
select m.search from messages m join issues i on i.key = m.issue_key where i.project_key = {project}`

// searchTermItemsBeforeSnapshotsTSStat is main's searchTermItemsTSStat: the items statement as
// the format() string ts_stat's query is built from in SQL, %1$L quoting the project key as a
// literal so the key never enters SQL text by Go concatenation.
var searchTermItemsBeforeSnapshotsTSStat = strings.ReplaceAll(searchTermItemsBeforeSnapshotsSQL, "{project}", "%1$L")

// searchTermStatsBeforeSnapshotsPrelude is the three statements main's refresh runs before its
// rebuild, in the order it runs them: the claim that makes the project's totals row exist, the
// row lock that serialises two refreshes of one project, and the clear of the project's lexemes
// the rebuild then writes again. The row the claim writes carries items and refreshed_at and
// nothing else, which is every column main's search_term_totals has. Each takes the project key
// as $1.
var searchTermStatsBeforeSnapshotsPrelude = []string{
	`insert into search_term_totals (project_key, items, refreshed_at) values ($1, 0, now())
on conflict (project_key) do nothing`,
	`select 1 from search_term_totals where project_key = $1 for update`,
	`delete from search_term_stats where project_key = $1`,
}

// searchTermStatsRebuildBeforeSnapshots is main's searchTermStatsRebuild, the statement the
// release before this one runs for every project every ten minutes. It writes items and
// refreshed_at and nothing else: it takes no snapshot, leaves whatever snapshot the row holds in
// place, and drops no queued change. That is why a pass of this release's build reads a row whose
// refreshed_at is not its own snapshot_at as somebody else's and counts the project in full.
//
// Parameters: $1 the project key, $2 searchTermItemsBeforeSnapshotsTSStat.
var searchTermStatsRebuildBeforeSnapshots = `
with stats as (
  insert into search_term_stats (project_key, lexeme, ndoc)
  select $1, word, ndoc from ts_stat(format($2::text, $1::text))
)
update search_term_totals
   set items = (select count(*) from (` + strings.ReplaceAll(searchTermItemsBeforeSnapshotsSQL, "{project}", "$1::text") + `
       ) items),
       refreshed_at = statement_timestamp()
 where project_key = $1
returning items`

// searchTermTotalsSnapshotColumn answers whether search_term_totals carries 0166's snapshot
// column, which is what says this release's build can run against the database at all.
//
// The schema is read once, from the catalogue, rather than by running this release's build and
// falling back on its error: an error-driven fallback cannot tell a missing column from a real
// failure, so a build that failed for its own reason would be retried as main's and write
// statistics nobody asked for, where this step has to fail its command when the build genuinely
// fails.
const searchTermTotalsSnapshotColumn = `select exists (
  select 1 from information_schema.columns
   where table_schema = current_schema() and table_name = 'search_term_totals'
     and column_name = 'snapshot')`

// BuildEverySearchTermStats builds every project's term statistics on whichever of the two
// schemas the database carries, and is what cmd/dispatch-seed's last step calls: a database
// migrated to this head is built by RefreshEverySearchTermStats, exactly as a running server
// builds it, so the first measured pass over the seeded data is a step pass and not a first-use
// full build; a database migrated from main, which the measurement protocol seeds with
// --no-migrate, is built by main's own four statements.
//
// A project whose build fails is logged and the rest are built, as a server's pass does it. The
// caller's own count of the rows built is what fails the command: a build runs in one
// transaction, so a project that failed has no search_term_totals row to count.
func BuildEverySearchTermStats(ctx context.Context, pool *store.Pool) error {
	var snapshots bool
	if err := pool.QueryRow(ctx, searchTermTotalsSnapshotColumn).Scan(&snapshots); err != nil {
		return fmt.Errorf("read whether search_term_totals carries a snapshot column: %w", err)
	}
	if snapshots {
		RefreshEverySearchTermStats(ctx, pool)
		return nil
	}
	return buildEverySearchTermStatsBeforeSnapshots(ctx, pool)
}

// buildEverySearchTermStatsBeforeSnapshots is one whole pass of main's build: every project, each
// in its own transaction, and no sweep of the queued changes, which is a table main's schema does
// not have.
func buildEverySearchTermStatsBeforeSnapshots(ctx context.Context, pool *store.Pool) error {
	started := time.Now()
	projects, err := searchTermStatsProjects(ctx, pool)
	if err != nil {
		return fmt.Errorf("list the projects whose search term statistics to build: %w", err)
	}
	built, items := 0, 0
	for _, project := range projects {
		count, err := refreshSearchTermStatsBeforeSnapshots(ctx, pool, project)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("dispatch: build a project's search term statistics on the schema before snapshots",
				"project", project, "error", err)
			continue
		}
		built++
		items += count
	}
	slog.Info("dispatch: search term statistics built on the schema before snapshots",
		"projects", built, "failed", len(projects)-built, "items", items,
		"duration", time.Since(started).Round(time.Millisecond))
	return nil
}

// refreshSearchTermStatsBeforeSnapshots rebuilds one project's statistics as the release before
// this one does it: the claim, the row lock, the clear and the counting rebuild, the four
// statements of main's refreshSearchTermStats, in one transaction. It answers the project's item
// count, the picker's N.
func refreshSearchTermStatsBeforeSnapshots(ctx context.Context, pool *store.Pool, project string) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, statement := range searchTermStatsBeforeSnapshotsPrelude {
		if _, err := tx.Exec(ctx, statement, project); err != nil {
			return 0, err
		}
	}
	var items int
	if err := tx.QueryRow(ctx, searchTermStatsRebuildBeforeSnapshots, project, searchTermItemsBeforeSnapshotsTSStat).Scan(&items); err != nil {
		return 0, err
	}
	return items, tx.Commit(ctx)
}
