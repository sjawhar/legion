// Package ghnotify polls GitHub's Notifications API for the threads a person takes part in and
// publishes each new comment, review, close and reopen on the topic, with the fields, the webhook
// route publishes for the same change. It covers the repositories whose owners have not installed
// Envoy's GitHub App, so no webhook reaches Envoy from them.
//
// It is an optional part of the webhook listener: the listener starts it only when
// ENVOY_GITHUB_NOTIFICATIONS_TOKEN holds a value, hands it a publisher, a logger and the metrics
// registry, and reads Health for /healthz. It imports no listener package and no bus; it reads
// GitHub with GET alone and never marks a notification read.
package ghnotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

const (
	// minInterval is GitHub's least poll interval; a larger X-Poll-Interval wins.
	minInterval = time.Minute
	// listPageSize and maxListPages bound one poll's list: 50 threads a page, 20 pages.
	listPageSize = 50
	maxListPages = 20
	// detailPageSize and maxDetailPages bound each of a thread's lists: 100 items a page, 10
	// pages. A list without since (reviews, issue events) reads its newest 10 pages.
	detailPageSize = 100
	maxDetailPages = 10
	// maxConcurrentThreads is how many threads are fetched at once.
	maxConcurrentThreads = 4
	// rateFloor is the X-RateLimit-Remaining under which no detail fetch is sent until
	// X-RateLimit-Reset.
	rateFloor = 500
	// expiryWarning is how long before the token's expiry the poller starts its daily warning.
	expiryWarning = 14 * 24 * time.Hour
	// maxAnswerBytes bounds one answer the poller decodes.
	maxAnswerBytes = 64 << 20
)

// The poller's log lines.
const (
	msgPoll          = "github notifications poll"
	msgDropped       = "github notifications thread dropped"
	msgUnreadable    = "github notifications thread unreadable"
	msgUnauthorized  = "github notifications token refused"
	msgSSO           = "github notifications owner requires single sign-on"
	msgExpiry        = "github notifications token expires soon"
	msgTruncated     = "github notifications list truncated"
	msgUnbuildable   = "github notifications change skipped"
	msgInvalidChange = "github notifications envelope invalid"
)

// Publisher publishes an envelope and reports whether the stream already held it. *bus.Client is
// one; the poller counts only what the stream stored.
type Publisher interface {
	PublishReportingDuplicate(contracts.Envelope) (duplicate bool, err error)
}

// Health is the poller's report for /healthz. Nothing reads it to stop the process.
type Health struct {
	// LastSuccess is the time of the last successful poll, zero before the first.
	LastSuccess time.Time
	// Unreadable24h is how many threads the poller could not read in the last 24 hours.
	Unreadable24h int
	// Degraded is true when no poll has succeeded in Config.DegradedAfter.
	Degraded bool
}

// Poller polls GitHub's notifications and publishes what changed. Its state is in memory.
type Poller struct {
	cfg     Config
	skip    map[string]struct{}
	client  *client
	pub     Publisher
	log     *logging.Logger
	m       *pollerMetrics
	started time.Time

	mu            sync.Mutex
	cur           cursor
	threads       map[string]*threadState
	deferred      map[string]deferredThread
	dropped       map[string]droppedThread
	ssoUntil      map[string]time.Time
	unreadable    map[string]time.Time
	lastSuccess   time.Time
	pollInterval  time.Duration
	rateRemaining int
	floorUntil    time.Time
	authLogged    time.Time
	expiryWarned  time.Time
}

// New builds a poller from cfg, filling every zero value with its default. It refuses a lookback
// past 48 hours with ErrLookbackTooLong, and an empty token, publisher or logger. A nil registry
// is a registry of the poller's own.
func New(cfg Config, pub Publisher, logger *logging.Logger, reg *metrics.Registry) (*Poller, error) {
	cfg.Token = strings.TrimSpace(cfg.Token)
	switch {
	case cfg.Token == "":
		return nil, errors.New("ghnotify: a token is required")
	case pub == nil:
		return nil, errors.New("ghnotify: a publisher is required")
	case logger == nil:
		return nil, errors.New("ghnotify: a logger is required")
	case cfg.Lookback < 0:
		return nil, fmt.Errorf("ghnotify: the lookback %s is negative", cfg.Lookback)
	case cfg.Lookback > maxLookback:
		return nil, fmt.Errorf("%w: %s", ErrLookbackTooLong, cfg.Lookback)
	case cfg.DegradedAfter < 0:
		return nil, fmt.Errorf("ghnotify: the degraded bound %s is negative", cfg.DegradedAfter)
	}
	if cfg.Lookback == 0 {
		cfg.Lookback = defaultLookback
	}
	if cfg.DegradedAfter == 0 {
		cfg.DegradedAfter = defaultDegradedAfter
	}
	if cfg.MentionTrigger = strings.TrimSpace(cfg.MentionTrigger); cfg.MentionTrigger == "" {
		cfg.MentionTrigger = defaultTrigger
	}
	if cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"); cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("ghnotify: the API base %q is not an absolute http or https URL", cfg.BaseURL)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if reg == nil {
		reg = metrics.New()
	}
	skip := map[string]struct{}{}
	owners := parseOwners(strings.Join(cfg.SkipOwners, ","))
	for _, owner := range owners {
		skip[owner] = struct{}{}
	}
	cfg.SkipOwners = owners
	now := cfg.Now()
	return &Poller{
		cfg:           cfg,
		skip:          skip,
		client:        &client{base: base, token: cfg.Token, http: cfg.HTTPClient},
		pub:           pub,
		log:           logger,
		m:             newPollerMetrics(reg),
		started:       now,
		cur:           cursor{since: now.Add(-cfg.Lookback)},
		threads:       map[string]*threadState{},
		deferred:      map[string]deferredThread{},
		dropped:       map[string]droppedThread{},
		ssoUntil:      map[string]time.Time{},
		unreadable:    map[string]time.Time{},
		pollInterval:  minInterval,
		rateRemaining: -1,
	}, nil
}

// Run polls at once and then at each interval GitHub allows, until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := p.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		timer.Reset(next)
	}
}

// Health reports the poller's last success, its unreadable threads of the last day, and whether it
// has gone DegradedAfter without a successful poll since it was built.
func (p *Poller) Health() Health {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	reference := p.lastSuccess
	if reference.IsZero() {
		reference = p.started
	}
	unreadable := 0
	for _, at := range p.unreadable {
		if now.Sub(at) <= unreadableWindow {
			unreadable++
		}
	}
	return Health{
		LastSuccess:   p.lastSuccess,
		Unreadable24h: unreadable,
		Degraded:      now.Sub(reference) > p.cfg.DegradedAfter,
	}
}

// pollOutcome is what one poll did, for its metrics, its line and its next interval.
type pollOutcome struct {
	status    int
	result    string
	threads   int
	published int
	wait      time.Duration
	err       error
}

// poll lists the threads changed since the cursor, processes each, and returns how long to wait
// before the next poll. The cursor moves only when every listed thread succeeded.
func (p *Poller) poll(ctx context.Context) time.Duration {
	now := p.cfg.Now()
	p.prune(now)
	cur := p.cursor()
	list, err := p.listNotifications(ctx, cur)
	if ctx.Err() != nil {
		return minInterval
	}
	out := pollOutcome{status: list.status}
	if err != nil {
		fe := failureOf(err)
		out.err = err
		switch fe.kind {
		case failUnauthorized:
			out.result = resultUnauthorized
			p.unauthorized(now)
		case failRateLimited:
			out.result, out.wait = resultRateLimited, fe.wait
		default:
			out.result = resultError
		}
		return p.finish(out)
	}

	jobs := p.plan(list.threads, cur.since, now)
	out.threads = len(list.threads)
	results := p.processAll(ctx, jobs)
	if ctx.Err() != nil {
		return minInterval
	}
	listedOK, allOK := true, true
	for i, result := range results {
		out.published += result.published
		if result.outcome == outcomeDone || result.outcome == outcomeDeferred {
			continue
		}
		allOK = false
		if jobs[i].listed {
			listedOK = false
		}
		out.result = worse(out.result, result.outcome)
		if result.wait > out.wait {
			out.wait = result.wait
		}
		if out.err == nil {
			out.err = result.err
		}
	}
	if !list.notModified && listedOK {
		p.mu.Lock()
		p.cur = cursor{lastModified: list.lastModified, since: list.date.Add(-overlap)}
		p.mu.Unlock()
	}
	if allOK {
		out.result = resultOK
		if list.notModified {
			out.result = resultNotModified
		}
		p.mu.Lock()
		p.lastSuccess = now
		p.mu.Unlock()
		p.m.lastSuccess.Set(now.Unix())
	}
	return p.finish(out)
}

// worse is the poll result naming the gravest thread failure seen so far.
func worse(result string, o outcome) string {
	rank := map[string]int{"": 0, resultError: 1, resultRateFloor: 2, resultUnauthorized: 3, resultRateLimited: 4}
	next := map[outcome]string{
		outcomeRetry: resultError, outcomeFloor: resultRateFloor,
		outcomeUnauthorized: resultUnauthorized, outcomeRateLimited: resultRateLimited,
	}[o]
	if rank[next] > rank[result] {
		return next
	}
	return result
}

// finish records a poll and returns the next interval: the larger of 60 s and the last
// X-Poll-Interval, or the wait a rate-limited answer asked for when that is longer. A poll that
// published or failed writes one INFO line; a quiet one writes none, and the metrics count it.
func (p *Poller) finish(out pollOutcome) time.Duration {
	p.mu.Lock()
	next := p.pollInterval
	remaining := p.rateRemaining
	p.mu.Unlock()
	next = max(next, out.wait)
	p.m.pollInterval.Set(int64(next / time.Second))
	p.m.polls.Inc([2]string{"result", out.result})
	if out.published > 0 || (out.result != resultOK && out.result != resultNotModified) {
		attrs := []slog.Attr{
			slog.Int("status", out.status),
			slog.String("result", out.result),
			slog.Int("threads", out.threads),
			slog.Int("published", out.published),
			slog.Int64("next_interval_seconds", int64(next/time.Second)),
			slog.Int("rate_remaining", remaining),
		}
		if out.err != nil {
			attrs = append(attrs, slog.String("error", out.err.Error()))
		}
		p.log.Info(msgPoll, attrs...)
	}
	return next
}

// unauthorized logs a 401 at most once an hour. The listener goes on serving webhooks.
func (p *Poller) unauthorized(now time.Time) {
	p.mu.Lock()
	if !p.authLogged.IsZero() && now.Sub(p.authLogged) < time.Hour {
		p.mu.Unlock()
		return
	}
	p.authLogged = now
	p.mu.Unlock()
	p.log.Error(msgUnauthorized, slog.Int("status", http.StatusUnauthorized),
		slog.String("detail", "GitHub refused the notifications token; replace it, or the poller publishes nothing"))
}

// notification is one thread of GitHub's notification list.
type notification struct {
	ID         string         `json:"id"`
	Reason     string         `json:"reason"`
	UpdatedAt  string         `json:"updated_at"`
	Subject    subject        `json:"subject"`
	Repository map[string]any `json:"repository"`
}

type subject struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Type  string `json:"type"`
}

// listResult is one poll's read of the notification list.
type listResult struct {
	status       int
	notModified  bool
	threads      []notification
	date         time.Time
	lastModified string
}

// listNotifications reads the participating threads updated since the cursor, read ones included,
// through every page up to maxListPages, conditional on the cursor's Last-Modified.
func (p *Poller) listNotifications(ctx context.Context, cur cursor) (listResult, error) {
	query := url.Values{
		"participating": {"true"},
		"all":           {"true"},
		"since":         {cur.since.UTC().Format(time.RFC3339)},
		"per_page":      {strconv.Itoa(listPageSize)},
	}
	header := http.Header{}
	if cur.lastModified != "" {
		header.Set("If-Modified-Since", cur.lastModified)
	}
	resp, err := p.fetch(ctx, "/notifications", query, header)
	if err != nil {
		return listResult{status: failureOf(err).status}, err
	}
	res := listResult{status: resp.StatusCode, lastModified: resp.Header.Get("Last-Modified")}
	res.date, err = http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		res.date = p.cfg.Now()
	}
	if resp.StatusCode == http.StatusNotModified {
		resp.Body.Close()
		res.notModified = true
		return res, nil
	}
	links := parseLinks(resp.Header.Get("Link"))
	if err := decodeAnswer(resp, &res.threads); err != nil {
		return res, err
	}
	next := links["next"]
	for pages := 1; next != ""; pages++ {
		if pages >= maxListPages {
			p.log.Warn(msgTruncated, slog.Int("pages", maxListPages), slog.Int("threads", len(res.threads)))
			break
		}
		resp, err := p.fetch(ctx, next, nil, nil)
		if err != nil {
			return res, err
		}
		next = parseLinks(resp.Header.Get("Link"))["next"]
		var page []notification
		if err := decodeAnswer(resp, &page); err != nil {
			return res, err
		}
		res.threads = append(res.threads, page...)
	}
	return res, nil
}

// fetch sends one GET and returns a 200 or 304 answer for the caller to read and close; any other
// answer, and any failure to get one, is a *fetchError. Every answer's rate and expiry headers are
// read first.
func (p *Poller) fetch(ctx context.Context, path string, query url.Values, header http.Header) (*http.Response, error) {
	resp, err := p.client.get(ctx, path, query, header)
	if err != nil {
		return nil, &fetchError{kind: failTransient, err: err}
	}
	p.observe(resp.Header)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return nil, classify(resp, p.cfg.Now())
}

// observe reads an answer's X-Poll-Interval, its rate limit, which arms or lifts the rate floor,
// and the token's expiry, which warns once a day from 14 days before it.
func (p *Poller) observe(h http.Header) {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if seconds, err := strconv.Atoi(strings.TrimSpace(h.Get("X-Poll-Interval"))); err == nil {
		p.pollInterval = max(minInterval, time.Duration(seconds)*time.Second)
	}
	if remaining, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining"))); err == nil {
		p.rateRemaining = remaining
		p.m.rateRemaining.Set(int64(remaining))
		if remaining < rateFloor {
			reset, ok := rateLimitReset(h)
			if !ok {
				reset = now.Add(minInterval)
			}
			p.floorUntil = reset
		} else {
			p.floorUntil = time.Time{}
		}
	}
	if raw := strings.TrimSpace(h.Get("GitHub-Authentication-Token-Expiration")); raw != "" {
		expiry, ok := parseExpiry(raw)
		if ok && expiry.Sub(now) <= expiryWarning && (p.expiryWarned.IsZero() || now.Sub(p.expiryWarned) >= 24*time.Hour) {
			p.expiryWarned = now
			p.log.Warn(msgExpiry,
				slog.String("expires_at", expiry.UTC().Format(time.RFC3339)),
				slog.Int("days_left", int(expiry.Sub(now).Hours()/24)))
		}
	}
}

func parseExpiry(raw string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

func (p *Poller) floorActive() bool {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return now.Before(p.floorUntil)
}

// detailObject reads one object of a thread.
func (p *Poller) detailObject(ctx context.Context, path string) (map[string]any, error) {
	if p.floorActive() {
		return nil, &fetchError{kind: failFloor}
	}
	resp, err := p.fetch(ctx, path, nil, nil)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := decodeAnswer(resp, &object); err != nil {
		return nil, err
	}
	return object, nil
}

// detailList reads one of a thread's lists, 100 items a page. A list read with since follows its
// pages from the first, up to maxDetailPages; tail is for a list without since, oldest first, and
// reads its newest maxDetailPages pages when it has more.
func (p *Poller) detailList(ctx context.Context, path string, query url.Values, tail bool) ([]map[string]any, error) {
	query.Set("per_page", strconv.Itoa(detailPageSize))
	items, links, err := p.detailPage(ctx, path, query)
	if err != nil {
		return nil, err
	}
	if last := links["last"]; tail && pageOf(last) > maxDetailPages {
		items = nil
		for page := pageOf(last) - maxDetailPages + 1; page <= pageOf(last); page++ {
			got, _, err := p.detailPage(ctx, withPage(last, page), nil)
			if err != nil {
				return nil, err
			}
			items = append(items, got...)
		}
		return items, nil
	}
	next := links["next"]
	for pages := 1; next != "" && pages < maxDetailPages; pages++ {
		got, links, err := p.detailPage(ctx, next, nil)
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
		next = links["next"]
	}
	return items, nil
}

func (p *Poller) detailPage(ctx context.Context, path string, query url.Values) ([]map[string]any, map[string]string, error) {
	if p.floorActive() {
		return nil, nil, &fetchError{kind: failFloor}
	}
	resp, err := p.fetch(ctx, path, query, nil)
	if err != nil {
		return nil, nil, err
	}
	links := parseLinks(resp.Header.Get("Link"))
	var items []map[string]any
	if err := decodeAnswer(resp, &items); err != nil {
		return nil, nil, err
	}
	return items, links, nil
}

// decodeAnswer decodes and closes an answer's body; a body that will not decode is transient.
func decodeAnswer(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBytes)).Decode(v); err != nil {
		return &fetchError{kind: failTransient, status: resp.StatusCode, err: fmt.Errorf("ghnotify: decode %s: %w", resp.Request.URL.Path, err)}
	}
	return nil
}
