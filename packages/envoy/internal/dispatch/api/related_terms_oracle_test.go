package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file is the refresh as main writes it: the oracle of the equality tests in
// related_terms_refresh_test.go and the old release this change has to meet during a roll.
//
// The statements are a literal copy of that release's text, nothing built from this package's own
// constants: an oracle that shared a constant with the statement under test would move with it
// and the comparison would hold nothing. The cost is that an edit to a part of those statements
// this change leaves alone has to be copied here too, which is what the equality tests report
// when it is not.
//
// cmd/dispatch-seed carries the same copy, because the measurement protocol seeds a database
// migrated from main, where this release's build reads columns that are not there. Neither copy
// drifts from the other: the text they copy is main's released text, and it is frozen.

// searchTermItemsBeforeSnapshotsSQL is main's searchTermItemsSQL, which {project} stands in for
// the project key of as it does there. It differs from this release's in its document arm alone,
// which reads every artifact's latest version through a lateral join over artifact_versions where
// this release's reads document_search (migration 20261101182447): a database at main's schema
// has no document_search, and one at this head answers the same rows through either.
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

// runMainSearchTermStatsRebuild rebuilds the project's statistics as the release before this one
// does it: the claim, the row lock, the clear and the rebuild, the four statements of main's
// refreshSearchTermStats, in one transaction. It answers the project's item count, the picker's
// N.
func runMainSearchTermStatsRebuild(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the old release's rebuild of %s: %v", project, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, statement := range searchTermStatsBeforeSnapshotsPrelude {
		if _, err := tx.Exec(ctx, statement, project); err != nil {
			t.Fatalf("the old release's rebuild of %s: %v", project, err)
		}
	}
	var items int
	if err := tx.QueryRow(ctx, searchTermStatsRebuildBeforeSnapshots, project, searchTermItemsBeforeSnapshotsTSStat).Scan(&items); err != nil {
		t.Fatalf("the old release's rebuild of %s: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the old release's rebuild of %s: %v", project, err)
	}
	return items
}
