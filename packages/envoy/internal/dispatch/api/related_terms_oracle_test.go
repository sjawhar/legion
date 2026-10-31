package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file is the harness for the refresh as main writes it: the oracle of the equality tests in
// related_terms_refresh_test.go and the old release this change has to meet during a roll.
//
// The statements themselves live in related_terms_before_snapshots.go, because cmd/dispatch-seed
// runs them too: the measurement protocol seeds a database migrated from main, where this
// release's build reads columns that are not there. They are a literal copy of main's text,
// nothing built from this package's own constants: an oracle that shared a constant with the
// statement under test would move with it and the comparison would hold nothing. The cost is
// that an edit to a part of those statements this change leaves alone has to be copied there
// too, which is what the equality tests report when it is not. Reading them here rather than
// copying them again is what keeps the text the seed runs and the text these tests call main's
// one text.

// runMainSearchTermStatsRebuild rebuilds the project's statistics as the release before this one
// does it: the claim, the row lock, the clear and the rebuild, the four statements of main's
// refreshSearchTermStats, in one transaction.
func runMainSearchTermStatsRebuild(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	items, err := refreshSearchTermStatsBeforeSnapshots(context.Background(), database.Pool, project)
	if err != nil {
		t.Fatalf("the old release's rebuild of %s: %v", project, err)
	}
	return items
}
