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
	// outcomeDone: the thread was published, dropped or found unreadable, and its state written.
	outcomeDone outcome = iota
	// outcomeDeferred: the thread's owner requires single sign-on; it is kept for a retry an hour
	// on, so the cursor may move past it.
	outcomeDeferred
	// outcomeRetry: a transient failure; nothing was written and the cursor stays.
	outcomeRetry
	// outcomeFloor: the rate floor held a detail fetch back; the cursor stays.
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

// job is one thread to process in a poll and the since a thread with no state starts from. listed
// is a thread of this poll's list, which alone decide the cursor; deferred is a thread kept for a
// single sign-on retry.
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
		if d, ok := p.deferred[n.ID]; ok {
			if !now.Before(d.retryAt) {
				jobs = append(jobs, job{n: n, since: d.since, listed: true, deferred: true})
			}
			continue
		}
		jobs = append(jobs, job{n: n, since: since, listed: true})
	}
	for threadID, d := range p.deferred {
		if !seen[threadID] && !now.Before(d.retryAt) {
			jobs = append(jobs, job{n: d.n, since: d.since, deferred: true})
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
		p.mu.Lock()
		delete(p.deferred, j.n.ID)
		p.mu.Unlock()
	}
	return result
}

// thread processes one thread (the spec's "One thread"): it reads the issue, and the pull request
// for one, then every comment, review comment, submitted review, close and reopen after the
// thread's high water less the margin, publishes those not already published in time order, and
// writes the thread's new state.
func (p *Poller) thread(ctx context.Context, j job) threadResult {
	n := j.n
	now := p.cfg.Now()
	owner, name := repositoryNames(n.Repository)
	repository := owner + "/" + name
	if owner == "" || name == "" {
		p.markUnreadable(n, repository, 0, p.startState(n.ID, j.since), now)
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
		p.markUnreadable(n, repository, 0, p.startState(n.ID, j.since), now)
		return threadResult{outcome: outcomeDone}
	}
	if until, waiting := p.ssoActive(owner, now); waiting {
		p.deferThread(n, j.since, until)
		return threadResult{outcome: outcomeDeferred}
	}

	state := p.startState(n.ID, j.since)
	changes := changeSet{after: state.highWater.Add(-overlap), items: map[string]item{}}
	fail := func(err error) threadResult { return p.failed(err, j, owner, repository, state, now) }
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	since := func() url.Values { return url.Values{"since": {changes.after.UTC().Format(time.RFC3339)}} }

	issue, err := p.detailObject(ctx, base+"/issues/"+number)
	if err != nil {
		return fail(err)
	}
	isPull := n.Subject.Type == "PullRequest"
	var pull map[string]any
	if isPull {
		if pull, err = p.detailObject(ctx, base+"/pulls/"+number); err != nil {
			return fail(err)
		}
	}
	comments, err := p.detailList(ctx, base+"/issues/"+number+"/comments", since(), false)
	if err != nil {
		return fail(err)
	}
	for _, comment := range comments {
		p.collect(&changes, repository)(issueCommentItem(comment, issue, n.Repository))
	}
	if isPull {
		reviewComments, err := p.detailList(ctx, base+"/pulls/"+number+"/comments", since(), false)
		if err != nil {
			return fail(err)
		}
		for _, comment := range reviewComments {
			p.collect(&changes, repository)(reviewCommentItem(comment, pull, n.Repository))
		}
		reviews, err := p.detailList(ctx, base+"/pulls/"+number+"/reviews", url.Values{}, true)
		if err != nil {
			return fail(err)
		}
		for _, review := range reviews {
			if state, _ := review["state"].(string); strings.EqualFold(state, "PENDING") {
				continue
			}
			p.collect(&changes, repository)(reviewItem(review, pull, n.Repository))
		}
	}
	events, err := p.detailList(ctx, base+"/issues/"+number+"/events", url.Values{}, true)
	if err != nil {
		return fail(err)
	}
	for _, event := range events {
		if kind, _ := event["event"].(string); kind != "closed" && kind != "reopened" {
			continue
		}
		p.collect(&changes, repository)(issueEventItem(event, issue, pull, n.Repository))
	}

	items := changes.sorted()
	highWater := state.highWater
	for _, it := range items {
		if it.at.After(highWater) {
			highWater = it.at
		}
	}
	published := 0
	for _, it := range items {
		if _, seen := state.margin[it.sourceID]; seen {
			continue
		}
		stored, err := p.publish(it, repository)
		published += stored
		if err != nil {
			return threadResult{outcome: outcomeRetry, published: published, err: fmt.Errorf("publish %s: %w", it.sourceID, err)}
		}
	}
	margin := map[string]time.Time{}
	for _, it := range items {
		if it.at.After(highWater.Add(-overlap)) {
			margin[it.sourceID] = it.at
		}
	}
	p.writeThread(n.ID, threadState{highWater: highWater, margin: margin, lastChange: latest(highWater, updatedAt(n))})
	return threadResult{outcome: outcomeDone, published: published}
}

// changeSet is a thread's changes after the margin's start, one per source event id, so a change
// a page boundary shows twice is published once.
type changeSet struct {
	after time.Time
	items map[string]item
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

// collect adds a built change to s when it is after the margin's start, and logs one it could not
// build from GitHub's object.
func (p *Poller) collect(s *changeSet, repository string) func(item, error) {
	return func(it item, err error) {
		if err != nil {
			p.log.Warn(msgUnbuildable, slog.String("repository", repository), slog.String("error", err.Error()))
			return
		}
		if it.at.After(s.after) {
			s.items[it.sourceID] = it
		}
	}
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

// failed is what a thread's failed fetch means for the thread (the spec's "One thread").
func (p *Poller) failed(err error, j job, owner, repository string, state threadState, now time.Time) threadResult {
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
		p.markUnreadable(j.n, repository, fe.status, state, now)
		return threadResult{outcome: outcomeDone}
	default:
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

// markUnreadable records a thread the poller cannot read (any 403 but a rate limit or single
// sign-on, a 404, or a thread GitHub described without a repository or number): one WARN line, the
// unreadable count, and the thread's high water moved to its update, so it is not read again until
// it changes.
func (p *Poller) markUnreadable(n notification, repository string, status int, state threadState, now time.Time) {
	p.log.Warn(msgUnreadable,
		slog.String("repository", repository),
		slog.Int("status", status),
		slog.String("thread_id", n.ID),
		slog.String("subject_type", n.Subject.Type),
		slog.String("reason", reasonUnreadable))
	p.m.dropped.Inc([2]string{"reason", reasonUnreadable})
	highWater := latest(state.highWater, updatedAt(n))
	p.mu.Lock()
	p.unreadable[n.ID] = now
	p.threads[n.ID] = &threadState{highWater: highWater, margin: state.margin, lastChange: highWater}
	p.mu.Unlock()
}

// drop records a thread the poller publishes nothing for, once for each of its updates.
func (p *Poller) drop(n notification, repository, reason string, now time.Time) {
	p.mu.Lock()
	if prior, ok := p.dropped[n.ID]; ok && prior.updatedAt == n.UpdatedAt {
		p.mu.Unlock()
		return
	}
	p.dropped[n.ID] = droppedThread{updatedAt: n.UpdatedAt, at: now}
	p.mu.Unlock()
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

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
