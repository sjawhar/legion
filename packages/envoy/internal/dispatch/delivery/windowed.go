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

// githubResultCap is GitHub's own hard ceiling on one filtered query: both the GraphQL issue
// search and the Actions run listing stop serving results past 1,000 for a single query,
// whatever their reported total says, so a window answering more than this must be narrowed or
// the results past it are unreachable.
const githubResultCap = 1000

// walkWindowed lists every result over [since, until] in windows, oldest window first, handing
// each completed window's results to visit -- the one implementation behind
// searchMergedPullRequests's and ListWorkflowRuns's identical "page fully, halve on overflow"
// logic, so the two cannot drift from each other.
//
// newFetcher is called once per window (the original call, and once per recursive half) and
// returns a closure that pages that window from scratch: each call to the closure returns the
// next page's items and the window's total result count (GitHub repeats the same total on every
// page of one query), until an empty page or len(results) == total ends it. The page-advancing
// mechanism is entirely the fetcher's own business -- a REST caller's own page-number counter, a
// GraphQL caller's own cursor -- and never crosses into this shared bisection logic, so a REST
// page number is never something a GraphQL caller has to carry.
//
// visit is called once per leaf window, in chronological order, with that window's own inclusive
// upper bound and every result in it: a caller recording per-step progress writes "imported
// through leafUntil" there, so a pass that fails at a later window resumes at this one instead
// of restarting the whole range. An error from visit stops the walk and is returned unwrapped,
// so a caller checking errors.As for its own typed error (a rate limit) still can.
//
// maxResults is the largest window the caller wants handed to one visit call -- its resumable
// unit of work, not GitHub's cap. A window whose total exceeds it is halved, down to windows
// GitHub can actually serve: only a window at minimumSearchWindow whose total is past
// githubResultCap is an error, since results past that cap are unreachable however often the
// caller retries.
//
// The two halves are NOT split at a bare midpoint on both sides: GitHub's date-range query
// qualifiers (merged:A..B, created:A..B) are inclusive on BOTH ends, so searching [since, mid)
// and [mid, until) as two separate inclusive-both-ends queries would double-count anything
// merged/created exactly at mid. The second half instead starts one second after the midpoint
// (GitHub's own query granularity is one second), so the two halves partition the window with
// neither a gap nor an overlap.
func walkWindowed[T any](
	since, until time.Time,
	scope string,
	maxResults int,
	newFetcher func(since, until time.Time) func() ([]T, int, error),
	visit func(leafUntil time.Time, items []T) error,
) error {
	fetch := newFetcher(since, until)
	first, total, err := fetch()
	if err != nil {
		return fmt.Errorf("%s in [%s, %s]: %w", scope, formatWindowBound(since), formatWindowBound(until), err)
	}

	if total > maxResults && until.Sub(since) > minimumSearchWindow {
		mid := since.Add(until.Sub(since) / 2)
		if err := walkWindowed(since, mid, scope, maxResults, newFetcher, visit); err != nil {
			return err
		}
		return walkWindowed(mid.Add(time.Second), until, scope, maxResults, newFetcher, visit)
	}
	if total > githubResultCap {
		return fmt.Errorf("%s: %d results in the window [%s, %s], which cannot be narrowed further",
			scope, total, formatWindowBound(since), formatWindowBound(until))
	}

	results := first
	for len(results) < total {
		items, _, err := fetch()
		if err != nil {
			return fmt.Errorf("%s in [%s, %s]: %w", scope, formatWindowBound(since), formatWindowBound(until), err)
		}
		if len(items) == 0 {
			break
		}
		results = append(results, items...)
	}
	return visit(until, results)
}

// collectWindowed is walkWindowed for a caller that wants every result at once rather than one
// window at a time.
func collectWindowed[T any](since, until time.Time, scope string, maxResults int, newFetcher func(since, until time.Time) func() ([]T, int, error)) ([]T, error) {
	var all []T
	err := walkWindowed(since, until, scope, maxResults, newFetcher, func(_ time.Time, items []T) error {
		all = append(all, items...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// formatWindowBound writes a window bound the way the GitHub query itself spells it. time.Time's
// own String carries a monotonic-clock reading ("m=-1204340.69"), which leaked into the
// production freshness row's last_error through these messages.
func formatWindowBound(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
