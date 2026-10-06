package delivery

import (
	"fmt"
	"time"
)

// minimumSearchWindow bounds the halving recursion shared by every GitHub list/search endpoint
// with a 1,000-result cap (issue/PR search, Actions run listing): a query answering more than
// 1,000 results in a window this narrow cannot be split further, and is reported as an error
// rather than recursing forever.
const minimumSearchWindow = time.Second

// fetchWindowed lists every result over [since, until), splitting the window in half and
// recursing whenever a window's own reported total exceeds 1,000 -- the one implementation
// behind SearchMergedPullRequests's and ListWorkflowRuns's identical "page fully, halve on
// overflow" logic (previously duplicated between github_prs.go and github_runs.go), so the two
// cannot drift from each other.
//
// newFetcher is called once per window (the original call, and once per recursive half) and
// returns a closure that pages that window from scratch: each call to the closure returns the
// next page's items and the window's total result count (GitHub repeats the same total on every
// page of one query), until an empty page or len(results) == total ends it. The page-advancing
// mechanism is entirely the fetcher's own business -- a REST caller's own page-number counter, a
// GraphQL caller's own cursor -- and never crosses into this shared bisection logic, so a REST
// page number is never something a GraphQL caller has to carry.
//
// The two halves are NOT split at a bare midpoint on both sides: GitHub's date-range query
// qualifiers (merged:A..B, created:A..B) are inclusive on BOTH ends, so searching [since, mid)
// and [mid, until) as two separate inclusive-both-ends queries would double-count anything
// merged/created exactly at mid. The second half instead starts one second after the midpoint
// (GitHub's own query granularity is one second), so the two halves partition the window with
// neither a gap nor an overlap.
func fetchWindowed[T any](since, until time.Time, scope string, newFetcher func(since, until time.Time) func() ([]T, int, error)) ([]T, error) {
	fetch := newFetcher(since, until)
	first, total, err := fetch()
	if err != nil {
		return nil, fmt.Errorf("%s in [%s, %s): %w", scope, since, until, err)
	}

	if total > 1000 {
		if until.Sub(since) <= minimumSearchWindow {
			return nil, fmt.Errorf("%s: %d results in the window [%s, %s), which cannot be narrowed further", scope, total, since, until)
		}
		mid := since.Add(until.Sub(since) / 2)
		before, err := fetchWindowed(since, mid, scope, newFetcher)
		if err != nil {
			return nil, err
		}
		after, err := fetchWindowed(mid.Add(time.Second), until, scope, newFetcher)
		if err != nil {
			return nil, err
		}
		return append(before, after...), nil
	}

	results := first
	for len(results) < total {
		items, _, err := fetch()
		if err != nil {
			return nil, fmt.Errorf("%s in [%s, %s): %w", scope, since, until, err)
		}
		if len(items) == 0 {
			break
		}
		results = append(results, items...)
	}
	return results, nil
}
