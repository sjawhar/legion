package cireconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// pullHead is one open pull request's head, as the pass reads it: the installation token and the
// shared scope its reads are budgeted under, the owner and name GitHub itself gives the
// repository, and the pull request's number and head commit.
type pullHead struct {
	token  string
	scope  string
	owner  string
	repo   string
	number string
	sha    string
}

// key is what the pass remembers this head's ETags under. It is the head's own identity, the same
// four parts cistore keys its record by.
func (h pullHead) key() string {
	return h.owner + "/" + h.repo + "#" + h.number + "@" + h.sha
}

// pageOf is one page of a conditional listing as a pass keeps it: the ETag GitHub answered with,
// the items that answer held, and whether the page was full, which the next read needs to know
// whether a page follows it. A 304 carries none of the three, so a page answered 304 keeps what
// the pass that read it in full left.
type pageOf[T any] struct {
	etag  string
	items []T
	full  bool
	// modified says GitHub answered this page 200 in the read that built it, so its items are the
	// ones that read must process. A 304's items were processed by the pass that read them.
	modified bool
}

// conditionalPage is one page as GitHub answered it, whatever the listing lists: githubapp's
// ObjectPage and PullRequestPage reduced to the four things readPages needs of either.
type conditionalPage[T any] struct {
	items       []T
	etag        string
	notModified bool
	full        bool
}

// errTooManyPages ends a listing GitHub keeps reporting full (maxPages).
var errTooManyPages = fmt.Errorf("the listing did not end within %d pages", maxPages)

// readPages reads one conditional listing whole: pages 1..N, N being the pages the last pass
// kept, each conditional on that page's ETag, and then, while the last page read came back full,
// the page after it, which no pass has an ETag for. It stops at the first page that is not full.
//
// It counts each request against the pass's own total, which the pace is left between, and
// against tally, the count the pass's log line reports this listing under.
func readPages[T any](ctx context.Context, r *Reconciler, c *counts, tally *int, kept []pageOf[T], read func(ctx context.Context, page int, etag string) (conditionalPage[T], error)) ([]pageOf[T], error) {
	var pages []pageOf[T]
	for number := 1; number <= maxPages; number++ {
		var before pageOf[T]
		if number <= len(kept) {
			before = kept[number-1]
		}
		r.pace(ctx, c)
		c.requests++
		*tally++
		answer, err := read(ctx, number, before.etag)
		if err != nil {
			return nil, err
		}
		page := pageOf[T]{etag: answer.etag, items: answer.items, full: answer.full, modified: true}
		if answer.notModified {
			page = pageOf[T]{etag: etagOr(answer.etag, before.etag), items: before.items, full: before.full}
		}
		pages = append(pages, page)
		if !page.full {
			return pages, nil
		}
	}
	return nil, errTooManyPages
}

// readHead reads one head's check runs and check suites and records what GitHub holds that the
// listener's record of that head lacks. It answers the stop a rate limit ended the pass at, and
// nil for every other outcome: a head the pass could not read is named at WARN and the pass goes
// on to the next.
func (r *Reconciler) readHead(ctx context.Context, writer Writer, head pullHead, c *counts) *stop {
	c.heads++
	kept := r.kept(writer, head)
	runs, err := readPages(ctx, r, c, &c.headRequests, kept.runs, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckRuns(ctx, head.token, head.owner, head.repo, head.sha, page, etag)
		return objectPage(answer), err
	})
	if at, failed := r.headFailed(err, head, c); failed {
		return at
	}
	suites, err := readPages(ctx, r, c, &c.headRequests, kept.suites, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckSuites(ctx, head.token, head.owner, head.repo, head.sha, page, etag)
		return objectPage(answer), err
	})
	if at, failed := r.headFailed(err, head, c); failed {
		return at
	}
	wanted, ok := r.record(ctx, writer, head, runs, suites, c)
	if !ok {
		// A write that did not land must not be hidden behind an ETag: the next pass reads this
		// head in full and writes it again.
		delete(r.heads, head.key())
		return nil
	}
	remember(&r.heads, head.key(), &headState{
		runs:   runs,
		suites: suites,
		// Once a head's answer has held something to record, it holds it in every later answer
		// too, so the record it was written to is worth asking after at every pass.
		observed: kept.observed || wanted > 0,
	})
	if everyPageKept(runs) && everyPageKept(suites) {
		c.notModified++
	}
	return nil
}

// kept is what the pass remembers of one head, and an empty state when its ETags must not be
// used: the record they stand for is gone (the bucket's seven-day TTL, or a delete), so a
// conditional read would answer 304 and leave the head with no record at all.
func (r *Reconciler) kept(writer Writer, head pullHead) headState {
	state, ok := r.heads[head.key()]
	if !ok {
		return headState{}
	}
	if !state.observed {
		// Nothing was recorded from this head's last answer, so there is no record to have lost.
		return *state
	}
	has, err := writer.HasRecord(head.owner, head.repo, head.number, head.sha)
	if err != nil {
		r.Logger.Warn("ci reconcile did not read whether a head still has its CI record",
			"repository", head.owner+"/"+head.repo, "pr", head.number, "sha", head.sha, "error", err)
		return headState{}
	}
	if !has {
		return headState{}
	}
	return *state
}

// headFailed reports one head's failed read. A rate limit ends the pass (the stop it answers);
// every other failure drops what the pass kept of the head, so the next pass reads it in full,
// and leaves the pass to the next head.
func (r *Reconciler) headFailed(err error, head pullHead, c *counts) (*stop, bool) {
	if err == nil {
		return nil, false
	}
	delete(r.heads, head.key())
	if at := stopAt(err, head.scope); at != nil {
		return at, true
	}
	var tooLarge *githubapp.ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		// GitHub's answer is past what githubapp reads at all, so this head cannot be read: it is
		// counted and named, and the pass goes on rather than failing every head behind it.
		c.tooLarge++
		r.Logger.Warn("ci reconcile skipped a head whose answer from GitHub was too large",
			"repository", head.owner+"/"+head.repo, "pr", head.number, "sha", head.sha, "error", err)
		return nil, true
	}
	r.Logger.Warn("ci reconcile did not read a head",
		"repository", head.owner+"/"+head.repo, "pr", head.number, "sha", head.sha, "error", err)
	return nil, true
}

// record writes every observation the rules say to record from the pages GitHub answered 200
// with, under the pull request's number and the owner and name GitHub itself gave. wanted counts
// the observations those rules named, whether or not the record already held them; ok is false
// when a write failed, which is what makes the pass drop this head's ETags.
func (r *Reconciler) record(ctx context.Context, writer Writer, head pullHead, runs, suites []pageOf[object], c *counts) (wanted int, ok bool) {
	ok = true
	repository := head.owner + "/" + head.repo
	for _, page := range runs {
		if !page.modified {
			continue
		}
		for _, raw := range page.items {
			observation, mapped := contracts.GithubCIObservation("check_run", raw, head.owner, head.repo, head.number)
			if !mapped {
				continue
			}
			// Only a completed run is recorded. cistore refuses to settle a head while any check
			// its record holds is queued or running, and nothing ever takes a check out of a
			// record, so one run left queued at that commit -- an attempt GitHub abandoned, a job
			// nobody ran -- would stop ci-ok ever reaching the daemon for that head, which is the
			// failure this pass exists to mend. The listener records a running run because the
			// event completing it follows; a pass reading a listing has no such promise.
			if observation.Status != "completed" {
				continue
			}
			switch contracts.ReviewerVerdictDecision(observation.CheckName, observation.AppID, r.ReviewerAppID) {
			case contracts.VerdictNoReviewerApp:
				r.Logger.Warn("ci reconcile dropped a verdict check: DISPATCH_REVIEWER_APP_ID is not set",
					"check", observation.CheckName, "app_id", observation.AppID, "repository", repository, "pr", head.number)
				continue
			case contracts.VerdictUntrusted:
				r.Logger.Warn("ci reconcile ignored a verdict check another App created",
					"check", observation.CheckName, "app_id", observation.AppID,
					"expected_app_id", r.ReviewerAppID, "repository", repository, "pr", head.number)
				continue
			}
			wanted++
			if err := writer.Record(ctx, observation); err != nil {
				ok = false
				r.Logger.Warn("ci reconcile did not record a check run",
					"check", observation.CheckName, "repository", repository, "pr", head.number, "sha", head.sha, "error", err)
				continue
			}
			c.records++
		}
	}
	for _, page := range suites {
		if !page.modified {
			continue
		}
		for _, raw := range page.items {
			observation, mapped := contracts.GithubCIObservation("check_suite", raw, head.owner, head.repo, head.number)
			if !mapped {
				continue
			}
			// A suite that is not completed holds the head exactly as a queued run does
			// (cistore's settlementReady), and no suite goes through the reviewer rule: a suite
			// has no name, and the rule is about the verdict checks a name says.
			if observation.Status != "completed" {
				continue
			}
			wanted++
			if err := writer.RecordSuite(ctx, observation); err != nil {
				ok = false
				r.Logger.Warn("ci reconcile did not record a check suite",
					"suite", observation.SuiteID, "repository", repository, "pr", head.number, "sha", head.sha, "error", err)
				continue
			}
			c.records++
		}
	}
	return wanted, ok
}

// objectPage is githubapp's answer as readPages takes it.
func objectPage(answer githubapp.ObjectPage) conditionalPage[object] {
	return conditionalPage[object]{
		items:       answer.Objects,
		etag:        answer.ETag,
		notModified: answer.NotModified,
		full:        answer.Full,
	}
}

// everyPageKept reports whether every page of a listing answered 304, which is GitHub saying the
// pass has already read all of it.
func everyPageKept[T any](pages []pageOf[T]) bool {
	for _, page := range pages {
		if page.modified {
			return false
		}
	}
	return len(pages) > 0
}
