package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file holds the refresh as main writes it, copied from the text this change replaces: the
// statement that counts every item of a project, and the items it counts them over, whose
// document arm reads each artifact's latest version through a lateral join over
// artifact_versions. They are the oracle of the equality tests in related_terms_refresh_test.go
// and the old release this change has to meet during a roll.
//
// Every piece is a literal copy, nothing built from the package's own constants: an oracle that
// shared a constant with the statement under test would move with it and the comparison would
// hold nothing. The cost is that an edit to a part of these statements this change leaves alone
// has to be copied here too, which is what the equality test reports when it is not.

// mainSearchTermItemsSQL is main's searchTermItemsSQL. This release's text differs in its
// document arm alone, which reads document_search (migration 0154) where this reads every
// artifact's latest version through a lateral join.
const mainSearchTermItemsSQL = `
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

// mainSearchTermItemsTSStat is main's searchTermItemsTSStat.
var mainSearchTermItemsTSStat = strings.ReplaceAll(mainSearchTermItemsSQL, "{project}", "%1$L")

// mainSearchTermStatsRebuild is main's searchTermStatsRebuild, the statement the release before
// this one runs for every project every ten minutes. It writes items and refreshed_at and
// nothing else: it takes no snapshot, leaves whatever snapshot the row holds in place, and drops
// no queued change. That is why a pass of this build reads a row whose refreshed_at is not its
// own snapshot_at as somebody else's and counts the project in full.
var mainSearchTermStatsRebuild = `
with stats as (
  insert into search_term_stats (project_key, lexeme, ndoc)
  select $1, word, ndoc from ts_stat(format($2::text, $1::text))
)
update search_term_totals
   set items = (select count(*) from (` + strings.ReplaceAll(mainSearchTermItemsSQL, "{project}", "$1::text") + `
       ) items),
       refreshed_at = statement_timestamp()
 where project_key = $1
returning items`

// runMainSearchTermStatsRebuild rebuilds the project's statistics as the release before this one
// does it: the claim, the row lock, the clear and the rebuild, the four statements of main's
// refreshSearchTermStats, in one transaction.
func runMainSearchTermStatsRebuild(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the old release's rebuild: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`insert into search_term_totals (project_key, items, refreshed_at) values ($1, 0, now())
on conflict (project_key) do nothing`,
		`select 1 from search_term_totals where project_key = $1 for update`,
		`delete from search_term_stats where project_key = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, project); err != nil {
			t.Fatalf("the old release's rebuild of %s: %v", project, err)
		}
	}
	var items int
	if err := tx.QueryRow(ctx, mainSearchTermStatsRebuild, project, mainSearchTermItemsTSStat).Scan(&items); err != nil {
		t.Fatalf("the old release's rebuild of %s: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the old release's rebuild: %v", err)
	}
	return items
}
