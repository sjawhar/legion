package delivery

import (
	"fmt"
	"time"
)

// minimumSearchWindow is GitHub's own query granularity, one second, and so the narrowest window
// the halving recursion below can produce. A window must be at least two of them wide to halve
// into two windows that each hold at least one second and do not overlap; a narrower one whose
// total is past githubResultCap is reported as an error rather than split into a second half
// that starts after its own end.
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
// GitHub can actually serve: only a window too narrow to halve (under two seconds) whose total
// is past githubResultCap is an error, since results past that cap are unreachable however often
// the caller retries.
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

	// A window under two seconds is the floor: GitHub's date qualifiers cannot split finer than a
	// second, so it cannot be halved into two windows that do not overlap. Such a window whose
	// total is past maxResults but within githubResultCap is visited whole, so visit can receive
	// up to githubResultCap results at once, past the maxResults it was asked to bound. Only one
	// past githubResultCap is an error, since its results past the cap are unreachable.
	if total > maxResults && until.Sub(since) >= 2*minimumSearchWindow {
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

// formatWindowBound writes a window bound the way the GitHub query itself spells it, rather than
// through time.Time's own String, whose monotonic-clock reading ("m=-1204340.69") would reach
// the freshness row through these messages.
func formatWindowBound(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
