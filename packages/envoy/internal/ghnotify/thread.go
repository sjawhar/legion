package ghnotify

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// outcome is how processing one thread ended, and so what it allows the cursor.
type outcome int

const (
	// outcomeDone: the thread was published, dropped or found unreadable, and its record written.
	outcomeDone outcome = iota
	// outcomeDeferred: the thread's owner requires single sign-on; it is kept for a retry an hour
	// on, so the cursor may move past it.
	outcomeDeferred
	// outcomeRetry: a transient failure; the thread is read again next poll and the cursor stays.
	outcomeRetry
	// outcomeFloor: the rate floor held a detail fetch back; the cursor stays, but the poll is no
	// failure.
	outcomeFloor
	// outcomeUnauthorized: GitHub refused the token; the cursor stays.
	outcomeUnauthorized
	// outcomeRateLimited: GitHub asked the poller to wait; the cursor stays.
	outcomeRateLimited
)

type threadResult struct {
	outcome   outcome
	wait      time.Duration
	published int
	err       error
}

// job is one thread to process in a poll and the since a thread with no published state starts
// from. listed is a thread of this poll's list, which alone decide the cursor; deferred is a
// thread kept for a single sign-on retry.
type job struct {
	n        notification
	since    time.Time
	listed   bool
	deferred bool
}

// plan is the poll's jobs: each listed thread once, from the poll's since, but a deferred one from
// the since it was deferred with and only once its retry is due; and every due deferred thread
// the list did not name.
func (p *Poller) plan(threads []notification, since, now time.Time) []job {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var jobs []job
	for _, n := range threads {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		if s := p.threads[n.ID]; s != nil && s.deferred != nil {
			if !now.Before(s.deferred.retryAt) {
				jobs = append(jobs, job{n: n, since: s.deferred.since, listed: true, deferred: true})
			}
			continue
		}
		jobs = append(jobs, job{n: n, since: since, listed: true})
	}
	for threadID, s := range p.threads {
		if s.deferred != nil && !seen[threadID] && !now.Before(s.deferred.retryAt) {
			jobs = append(jobs, job{n: s.deferred.n, since: s.deferred.since, deferred: true})
		}
	}
	return jobs
}

// processAll processes the jobs, maxConcurrentThreads at a time. A thread GitHub rate-limits stops
// the rest: they end as retries.
func (p *Poller) processAll(ctx context.Context, jobs []job) []threadResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]threadResult, len(jobs))
	slots := make(chan struct{}, maxConcurrentThreads)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if err := ctx.Err(); err != nil {
				results[i] = threadResult{outcome: outcomeRetry, err: err}
				return
			}
			results[i] = p.processThread(ctx, j)
			if results[i].outcome == outcomeRateLimited {
				cancel()
			}
		}()
	}
	wg.Wait()
	return results
}

func (p *Poller) processThread(ctx context.Context, j job) threadResult {
	result := p.thread(ctx, j)
	if j.deferred && result.outcome == outcomeDone {
		p.update(j.n.ID, func(s *threadState) { s.deferred = nil })
	}
	return result
}

// thread processes one thread (the spec's "One thread"): it reads every comment, review comment,
// submitted review, close and reopen after the thread's high water less the margin, publishes
// those not already published in time order, and writes the thread's new record.
func (p *Poller) thread(ctx context.Context, j job) threadResult {
	n := j.n
	now := p.cfg.Now()
	rec := p.record(n.ID)
	if rec.settledAt(n) {
		// Dropped or written off at this very update: nothing to read until the thread changes.
		return threadResult{outcome: outcomeDone}
	}
	start := rec.startHighWater(j.since)
	owner, name := repositoryNames(n.Repository)
	repository := owner + "/" + name
	if owner == "" || name == "" {
		p.markUnreadable(n, repository, 0, 0, start, now)
		return threadResult{outcome: outcomeDone}
	}
	if _, skip := p.skip[strings.ToLower(owner)]; skip {
		p.drop(n, repository, reasonSkippedOwner, now)
		return threadResult{outcome: outcomeDone}
	}
	if n.Subject.Type != "Issue" && n.Subject.Type != "PullRequest" {
		p.drop(n, repository, reasonUnmapped, now)
		return threadResult{outcome: outcomeDone}
	}
	number := subjectNumber(n.Subject.URL)
	if number == "" {
		p.markUnreadable(n, repository, 0, 0, start, now)
		return threadResult{outcome: outcomeDone}
	}
	if until, waiting := p.ssoActive(owner, now); waiting {
		p.deferThread(n, j.since, until)
		return threadResult{outcome: outcomeDeferred}
	}

	changes, err := p.readThread(ctx, n, owner, name, number, start)
	if err != nil {
		return p.failed(err, j, owner, repository, start, now)
	}
	if rec.failures > 0 {
		p.update(n.ID, func(s *threadState) { s.failures = 0 })
	}
	items := changes.sorted()
	highWater := start
	for _, it := range items {
		if it.at.After(highWater) {
			highWater = it.at
		}
	}
	published := 0
	for _, it := range items {
		if _, seen := rec.margin[it.sourceID]; seen {
			continue
		}
		stored, err := p.publish(it, repository)
		published += stored
		if err != nil {
			// The stream did not take the change, so the thread is read again next poll from the
			// same record. A stream that does not answer is the listener's trouble, not the
			// thread's: it counts nothing toward writing the thread off.
			return threadResult{outcome: outcomeRetry, published: published, err: fmt.Errorf("publish %s: %w", it.sourceID, err)}
		}
	}
	margin := map[string]time.Time{}
	for _, it := range items {
		if it.at.After(highWater.Add(-overlap)) {
			margin[it.sourceID] = it.at
		}
	}
	p.update(n.ID, func(s *threadState) {
		s.highWater, s.margin = highWater, margin
		s.lastChange = latest(s.lastChange, latest(highWater, changedAt(n, now)))
		s.failures, s.settled = 0, ""
	})
	return threadResult{outcome: outcomeDone, published: published}
}

// detailSource is one of a thread's lists: its path, how it is paged, and how one of its objects
// becomes a change, or is passed over (keep false).
type detailSource struct {
	path  string
	read  func(ctx context.Context, path string) ([]map[string]any, error)
	build func(object map[string]any) (it item, keep bool, err error)
}

// readThread reads one thread's changes after its high water less the margin: the issue, the pull
// request for one, then each of its lists.
func (p *Poller) readThread(ctx context.Context, n notification, owner, name, number string, highWater time.Time) (*changeSet, error) {
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	issue, err := p.detailObject(ctx, base+"/issues/"+number)
	if err != nil {
		return nil, err
	}
	var pull map[string]any
	if n.Subject.Type == "PullRequest" {
		if pull, err = p.detailObject(ctx, base+"/pulls/"+number); err != nil {
			return nil, err
		}
	}
	changes := &changeSet{after: highWater.Add(-overlap), items: map[string]item{}}
	for _, source := range p.detailSources(base, number, issue, pull, n.Repository, changes.after) {
		objects, err := source.read(ctx, source.path)
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			it, keep, err := source.build(object)
			switch {
			case err != nil:
				p.log.Warn(msgUnbuildable, slog.String("repository", owner+"/"+name), slog.String("error", err.Error()))
			case keep:
				changes.add(it)
			}
		}
	}
	return changes, nil
}

// detailSources are the lists a thread's changes come from: the issue comments, and for a pull
// request its review comments and reviews, then the issue events. A list GitHub filters by since
// is read from after; the others are read by their newest pages.
func (p *Poller) detailSources(base, number string, issue, pull, repository map[string]any, after time.Time) []detailSource {
	sinceAfter := func(ctx context.Context, path string) ([]map[string]any, error) {
		return p.listSince(ctx, path, after)
	}
	kept := func(it item, err error) (item, bool, error) { return it, true, err }
	issuePath := base + "/issues/" + number
	sources := []detailSource{{
		path: issuePath + "/comments",
		read: sinceAfter,
		build: func(comment map[string]any) (item, bool, error) {
			return kept(issueCommentItem(comment, issue, repository))
		},
	}}
	if pull != nil {
		pullPath := base + "/pulls/" + number
		sources = append(sources, detailSource{
			path: pullPath + "/comments",
			read: sinceAfter,
			build: func(comment map[string]any) (item, bool, error) {
				return kept(reviewCommentItem(comment, pull, repository))
			},
		}, detailSource{
			path: pullPath + "/reviews",
			read: p.listNewestPages,
			build: func(review map[string]any) (item, bool, error) {
				if reviewState, _ := review["state"].(string); strings.EqualFold(reviewState, "PENDING") {
					return item{}, false, nil
				}
				return kept(reviewItem(review, pull, repository))
			},
		})
	}
	return append(sources, detailSource{
		path: issuePath + "/events",
		read: p.listNewestPages,
		build: func(event map[string]any) (item, bool, error) {
			if kind, _ := event["event"].(string); kind != "closed" && kind != "reopened" {
				return item{}, false, nil
			}
			return kept(issueEventItem(event, issue, pull, repository))
		},
	})
}

// changeSet is a thread's changes after the margin's start, one per source event id, so a change
// a page boundary shows twice is published once.
type changeSet struct {
	after time.Time
	items map[string]item
}

func (s *changeSet) add(it item) {
	if it.at.After(s.after) {
		s.items[it.sourceID] = it
	}
}

func (s *changeSet) sorted() []item {
	out := make([]item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].sourceID < out[j].sourceID
	})
	return out
}

// publish publishes a change's envelopes and counts those the stream stored. An envelope that does
// not validate drops the change with one ERROR line rather than holding the cursor for ever.
func (p *Poller) publish(it item, repository string) (int, error) {
	stored := 0
	for _, envelope := range it.envelopes(p.cfg.MentionTrigger) {
		if err := envelope.Validate(); err != nil {
			p.log.Error(msgInvalidChange, slog.String("repository", repository),
				slog.String("source_event_id", it.sourceID), slog.String("error", err.Error()))
			return stored, nil
		}
		duplicate, err := p.pub.PublishReportingDuplicate(envelope)
		if err != nil {
			return stored, err
		}
		if !duplicate {
			stored++
			p.m.events.Inc([2]string{"kind", it.event})
		}
	}
	return stored, nil
}

// failed is what a thread's failed fetch means for the thread (the spec's "One thread"). A
// transient failure keeps the thread's record and the cursor, until the thread has failed
// maxFailures polls in a row: then it is written off as unreadable, so the cursor and the
// poller's health move on.
func (p *Poller) failed(err error, j job, owner, repository string, start, now time.Time) threadResult {
	fe := failureOf(err)
	switch fe.kind {
	case failRateLimited:
		return threadResult{outcome: outcomeRateLimited, wait: fe.wait, err: err}
	case failFloor:
		return threadResult{outcome: outcomeFloor, err: err}
	case failUnauthorized:
		p.unauthorized(now)
		return threadResult{outcome: outcomeUnauthorized, err: err}
	case failSSO:
		p.deferThread(j.n, j.since, p.ssoRefused(owner, repository, now))
		return threadResult{outcome: outcomeDeferred}
	case failUnreadable:
		p.markUnreadable(j.n, repository, fe.status, 0, start, now)
		return threadResult{outcome: outcomeDone}
	default:
		rec := p.update(j.n.ID, func(s *threadState) {
			s.failures++
			s.lastChange = latest(s.lastChange, changedAt(j.n, now))
		})
		if rec.failures >= maxFailures {
			p.markUnreadable(j.n, repository, fe.status, rec.failures, start, now)
			return threadResult{outcome: outcomeDone}
		}
		return threadResult{outcome: outcomeRetry, err: err}
	}
}

// ssoRefused starts an hour's wait on an owner that requires single sign-on for the token, with
// one WARN line, and returns when it ends. A thread of the owner refused while the wait runs
// starts no second wait and writes no second line.
func (p *Poller) ssoRefused(owner, repository string, now time.Time) time.Time {
	key := strings.ToLower(owner)
	p.mu.Lock()
	if until, ok := p.ssoUntil[key]; ok && now.Before(until) {
		p.mu.Unlock()
		return until
	}
	until := now.Add(ssoRetry)
	p.ssoUntil[key] = until
	p.mu.Unlock()
	p.log.Warn(msgSSO, slog.String("owner", owner), slog.String("repository", repository),
		slog.String("retry_at", until.UTC().Format(time.RFC3339)))
	return until
}

// deferThread keeps n for a retry at retryAt, from the since it was first deferred with.
func (p *Poller) deferThread(n notification, since, retryAt time.Time) {
	now := p.cfg.Now()
	p.update(n.ID, func(s *threadState) {
		d := deferral{n: n, since: since, retryAt: retryAt}
		if s.deferred != nil {
			d.since = s.deferred.since
		}
		s.deferred = &d
		s.lastChange = latest(s.lastChange, changedAt(n, now))
	})
}

// markUnreadable writes a thread off at its current update: one WARN line, the unreadable count,
// and its high water moved to that update, so it is neither read nor logged again until it
// changes (thread's settled guard). It is a 403 that is neither a rate limit nor single sign-on, a
// 404, a thread GitHub described without a repository or number, or maxFailures transient failures
// in a row, whose count the line carries.
func (p *Poller) markUnreadable(n notification, repository string, status, failures int, start, now time.Time) {
	attrs := []slog.Attr{
		slog.String("repository", repository),
		slog.Int("status", status),
		slog.String("thread_id", n.ID),
		slog.String("subject_type", n.Subject.Type),
		slog.String("reason", reasonUnreadable),
	}
	if failures > 0 {
		attrs = append(attrs, slog.Int("failures", failures))
	}
	p.log.Warn(msgUnreadable, attrs...)
	p.m.dropped.Inc([2]string{"reason", reasonUnreadable})
	highWater := latest(start, updatedAt(n))
	p.update(n.ID, func(s *threadState) {
		s.highWater = highWater
		s.lastChange = latest(s.lastChange, latest(highWater, changedAt(n, now)))
		s.failures, s.settled, s.unreadableAt = 0, n.UpdatedAt, now
	})
}

// drop records a thread the poller publishes nothing for, with one INFO line for each of its
// updates (thread's settled guard).
func (p *Poller) drop(n notification, repository, reason string, now time.Time) {
	p.update(n.ID, func(s *threadState) {
		s.settled = n.UpdatedAt
		s.lastChange = latest(s.lastChange, changedAt(n, now))
	})
	p.m.dropped.Inc([2]string{"reason", reason})
	p.log.Info(msgDropped,
		slog.String("repository", repository),
		slog.String("subject_type", n.Subject.Type),
		slog.String("reason", reason),
		slog.String("thread_id", n.ID),
		slog.String("notification_reason", n.Reason))
}

// repositoryNames is a notification's repository owner and name.
func repositoryNames(repository map[string]any) (string, string) {
	owner, _ := repository["owner"].(map[string]any)
	login, _ := owner["login"].(string)
	name, _ := repository["name"].(string)
	if login == "" || name == "" {
		full, _ := repository["full_name"].(string)
		if o, r, ok := strings.Cut(full, "/"); ok {
			login, name = o, r
		}
	}
	return login, name
}

// subjectNumber is the issue or pull request number at the end of a subject URL.
func subjectNumber(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	number := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return ""
	}
	return number
}

func updatedAt(n notification) time.Time {
	at, _ := time.Parse(time.RFC3339, n.UpdatedAt)
	return at
}

// changedAt is the thread's update, or now when GitHub gave none the poller can read, so its
// record is not forgotten at once.
func changedAt(n notification, now time.Time) time.Time {
	if at := updatedAt(n); !at.IsZero() {
		return at
	}
	return now
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
