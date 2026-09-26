package redeliver_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/testnats"
)

const clientID = "Iv1.redeliver"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// count reports how many log lines carry msg and every one of the attributes.
func (b *lockedBuffer) count(msg string, attrs ...string) int {
	n := 0
	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.Contains(line, `msg="`+msg+`"`) {
			continue
		}
		matched := true
		for _, attr := range attrs {
			if !strings.Contains(line, attr) {
				matched = false
			}
		}
		if matched {
			n++
		}
	}
	return n
}

type harness struct {
	t       *testing.T
	webhook *githubapptest.Webhook
	client  *githubapp.Client
	state   natsgo.KeyValue
	logs    *lockedBuffer
	clock   *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	nc, err := natsgo.Connect(testnats.URL(t))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	state, err := redeliver.OpenState(js)
	if err != nil {
		t.Fatalf("open redelivery state: %v", err)
	}
	key, pemText := githubapptest.Key(t)
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, clientID, nil)
	c := &clock{now: time.Now().UTC().Truncate(time.Second)}
	webhook.SetClock(c.Now)
	client, err := githubapp.New(&auth.AppConfig{ClientID: clientID, ClientSecret: "secret", PEM: pemText}, webhook.URL())
	if err != nil {
		t.Fatalf("App client: %v", err)
	}
	return &harness{t: t, webhook: webhook, client: client, state: state, logs: &lockedBuffer{}, clock: c}
}

func (h *harness) sweeper() *redeliver.Sweeper {
	return &redeliver.Sweeper{
		GitHub: h.client,
		State:  h.state,
		Logger: slog.New(slog.NewTextHandler(h.logs, nil)),
		Now:    h.clock.Now,
	}
}

// fail records an attempt GitHub made that the receiver answered with code, ago before now.
func (h *harness) fail(guid string, code int, ago time.Duration) githubapptest.Delivery {
	return h.webhook.Record(githubapptest.Attempt{GUID: guid, Event: "pull_request_review", Payload: []byte(`{}`)}, "submitted", 7, code, h.clock.Now().Add(-ago))
}

func (h *harness) sweep(opts redeliver.Options) redeliver.Report {
	h.t.Helper()
	report, err := h.sweeper().Sweep(context.Background(), opts)
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
	return report
}

func (h *harness) requests() []int64 { return h.webhook.Requests() }

func outcome(report redeliver.Report, guid string) redeliver.Outcome {
	for _, decision := range report.Decisions {
		if decision.GUID == guid {
			return decision.Outcome
		}
	}
	return ""
}

func TestSweepRedeliversAServerFailureOnce(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-a", http.StatusServiceUnavailable, time.Minute)

	report := h.sweep(redeliver.Options{})
	if got := outcome(report, "guid-a"); got != redeliver.Redelivered {
		t.Fatalf("first sweep outcome %q, want %q", got, redeliver.Redelivered)
	}
	if got, want := h.requests(), []int64{failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("redelivery requests %v, want %v", got, want)
	}
	if h.logs.count("webhook redelivered", "guid=guid-a", "attempt=1") != 1 {
		t.Fatalf("no redelivery log line for guid-a:\n%s", h.logs)
	}

	// The redelivery succeeded, so GitHub recorded it OK: nothing more to do.
	h.clock.advance(10 * time.Minute)
	report = h.sweep(redeliver.Options{})
	if got := outcome(report, "guid-a"); got != redeliver.Delivered {
		t.Fatalf("second sweep outcome %q, want %q", got, redeliver.Delivered)
	}
	if got := len(h.requests()); got != 1 {
		t.Fatalf("redelivery requests after a delivered redelivery = %d, want 1", got)
	}
}

// A 4xx is the receiver refusing the request itself (a bad signature, a body over its cap): the
// same bytes fail the same way, so the sweep never asks for them again and says so once.
func TestSweepNeverRedeliversAClientError(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-refused", http.StatusBadRequest, time.Minute)

	for range 2 {
		h.sweep(redeliver.Options{})
		h.clock.advance(5 * time.Minute)
	}
	if got := h.requests(); len(got) != 0 {
		t.Fatalf("redelivery requests %v, want none for a 4xx", got)
	}
	if got := h.logs.count("webhook delivery refused terminally", "guid=guid-refused", "level=ERROR"); got != 1 {
		t.Fatalf("terminal refusal logged %d times, want once:\n%s", got, h.logs)
	}
}

// A redelivery the receiver fails again is retried after a doubling backoff, at most MaxAttempts
// times; then the sweep gives up and says so once.
func TestSweepRetriesWithBackoffThenGivesUp(t *testing.T) {
	h := newHarness(t)
	h.webhook.SetRedeliver(func(githubapptest.Attempt) int { return http.StatusServiceUnavailable })
	h.fail("guid-down", http.StatusServiceUnavailable, 0)

	h.sweep(redeliver.Options{})
	if got := len(h.requests()); got != 1 {
		t.Fatalf("requests after the first sweep = %d, want 1", got)
	}
	h.clock.advance(time.Minute)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-down"); got != redeliver.Waiting {
		t.Fatalf("a sweep inside the first backoff: outcome %q, want %q", got, redeliver.Waiting)
	}
	h.clock.advance(time.Minute + time.Second) // the first backoff, two minutes, has passed
	h.sweep(redeliver.Options{})
	if got := len(h.requests()); got != 2 {
		t.Fatalf("requests after the first backoff = %d, want 2", got)
	}
	for i, backoff := range []time.Duration{4 * time.Minute, 8 * time.Minute, 16 * time.Minute} {
		h.clock.advance(backoff - time.Second)
		if got := outcome(h.sweep(redeliver.Options{}), "guid-down"); got != redeliver.Waiting {
			t.Fatalf("a second before backoff %s: outcome %q, want %q", backoff, got, redeliver.Waiting)
		}
		h.clock.advance(time.Second)
		h.sweep(redeliver.Options{})
		if got, want := len(h.requests()), i+3; got != want {
			t.Fatalf("requests after backoff %s = %d, want %d", backoff, got, want)
		}
	}
	if got := len(h.requests()); got != redeliver.MaxAttempts {
		t.Fatalf("requests = %d, want MaxAttempts (%d)", got, redeliver.MaxAttempts)
	}

	for range 3 {
		h.clock.advance(20 * time.Minute)
		h.sweep(redeliver.Options{})
	}
	if got := len(h.requests()); got != redeliver.MaxAttempts {
		t.Fatalf("requests after giving up = %d, want %d", got, redeliver.MaxAttempts)
	}
	if got := h.logs.count("webhook redelivery exhausted", "guid=guid-down", "level=ERROR"); got != 1 {
		t.Fatalf("exhaustion logged %d times, want once:\n%s", got, h.logs)
	}
}

// A redelivery request GitHub refuses delivered nothing, so it must not read as a redelivery
// that succeeded: it is asked again after the backoff.
func TestARefusedRequestIsRetriedNotMistakenForDelivered(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-refused-request", http.StatusServiceUnavailable, time.Minute)
	h.webhook.Refuse(failed.ID, http.StatusUnprocessableEntity)

	if got := outcome(h.sweep(redeliver.Options{}), "guid-refused-request"); got != redeliver.RequestRefused {
		t.Fatalf("outcome %q, want %q", got, redeliver.RequestRefused)
	}
	if h.logs.count("webhook redelivery request refused", "guid=guid-refused-request", "level=WARN") != 1 {
		t.Fatalf("no refusal warning:\n%s", h.logs)
	}

	h.webhook.Refuse(failed.ID, 0)
	h.clock.advance(time.Minute)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-refused-request"); got != redeliver.Waiting {
		t.Fatalf("inside the backoff: outcome %q, want %q", got, redeliver.Waiting)
	}
	h.clock.advance(time.Minute + time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-refused-request"); got != redeliver.Redelivered {
		t.Fatalf("after the backoff: outcome %q, want %q", got, redeliver.Redelivered)
	}
	if got, want := h.requests(), []int64{failed.ID, failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

// An attempt that got no HTTP answer (a listener slower than GitHub's ten seconds, or down) has
// no status code from 400 to 599, which is all GitHub's status=failure filter returns. The sweep
// takes every attempt whose status is not OK, as GitHub's own redelivery script does: such a
// delivery is redelivered, and a redelivery that got no answer is asked again rather than read as
// delivered.
func TestADeliveryThatGotNoAnswerIsRedelivered(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-no-answer", 0, time.Minute)
	h.webhook.SetRedeliver(func(githubapptest.Attempt) int { return 0 })

	if got := outcome(h.sweep(redeliver.Options{}), "guid-no-answer"); got != redeliver.Redelivered {
		t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}
	h.clock.advance(2*time.Minute + time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-no-answer"); got != redeliver.Redelivered {
		t.Fatalf("after the redelivery got no answer either: outcome %q, want %q", got, redeliver.Redelivered)
	}
	if got := len(h.requests()); got != 2 {
		t.Fatalf("redelivery requests %d, want 2", got)
	}
}

// A refused request adds no attempt to GitHub's log, so a delivery the sweep acted on from a
// window wider than its hour (a gap resume, or an operator's --since) is not listed again. It is
// still asked again after the backoff, and a delivery GitHub keeps refusing ends exhausted with
// its ERROR line, like one listed every time.
func TestARefusedDeliveryOutsideTheWindowIsAskedAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(h *harness) redeliver.Options
	}{
		{"a gap resume", func(h *harness) redeliver.Options {
			h.sweep(redeliver.Options{}) // the cursor, then Dispatch is down for three hours
			h.clock.advance(3 * time.Hour)
			return redeliver.Options{}
		}},
		{"an operator's --since", func(h *harness) redeliver.Options {
			h.sweep(redeliver.Options{})
			return redeliver.Options{Since: h.clock.Now().Add(-72 * time.Hour)}
		}},
	} {
		t.Run(tc.name+" then accepted", func(t *testing.T) {
			h := newHarness(t)
			opts := tc.first(h)
			old := h.fail("guid-old", http.StatusServiceUnavailable, 2*time.Hour)
			h.webhook.Refuse(old.ID, http.StatusUnprocessableEntity)
			if got := outcome(h.sweep(opts), "guid-old"); got != redeliver.RequestRefused {
				t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.RequestRefused)
			}

			h.webhook.Refuse(old.ID, 0)
			h.clock.advance(2*time.Minute + time.Second)
			if got := outcome(h.sweep(redeliver.Options{}), "guid-old"); got != redeliver.Redelivered {
				t.Fatalf("the next sweep after the backoff: outcome %q, want %q", got, redeliver.Redelivered)
			}
			if got, want := h.requests(), []int64{old.ID, old.ID}; !slices.Equal(got, want) {
				t.Fatalf("requests %v, want %v", got, want)
			}
		})
		t.Run(tc.name+" then refused to the end", func(t *testing.T) {
			h := newHarness(t)
			opts := tc.first(h)
			old := h.fail("guid-old", http.StatusServiceUnavailable, 2*time.Hour)
			h.webhook.Refuse(old.ID, http.StatusUnprocessableEntity)
			h.sweep(opts)
			for range 8 {
				h.clock.advance(17 * time.Minute) // past every backoff
				h.sweep(redeliver.Options{})
			}
			if got := len(h.requests()); got != redeliver.MaxAttempts {
				t.Fatalf("requests %d, want MaxAttempts (%d)", got, redeliver.MaxAttempts)
			}
			if got := h.logs.count("webhook redelivery exhausted", "guid=guid-old", "level=ERROR"); got != 1 {
				t.Fatalf("exhaustion logged %d times, want once:\n%s", got, h.logs)
			}
		})
	}
}

// GitHub answers a request over a rate limit with 403 or 429 and says when to try again
// (Retry-After, or x-ratelimit-remaining 0 and x-ratelimit-reset). The sweep stops there, counts
// nothing against the delivery it was asking for, and sends GitHub nothing more, listing
// included, until that time has passed.
func TestARateLimitStopsTheSweepUntilGitHubsRetryTime(t *testing.T) {
	retryAfter := func(*harness) http.Header { return http.Header{"Retry-After": {"60"}} }
	for _, tc := range []struct {
		name   string
		method string
		status int
		header func(*harness) http.Header
	}{
		{"a redelivery answered 429 with Retry-After", http.MethodPost, http.StatusTooManyRequests, retryAfter},
		{"a redelivery answered 403 with Retry-After", http.MethodPost, http.StatusForbidden, retryAfter},
		{"a redelivery answered 403 with no requests remaining", http.MethodPost, http.StatusForbidden, func(h *harness) http.Header {
			return http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(h.clock.Now().Add(time.Minute).Unix(), 10)}}
		}},
		// With neither header GitHub asks for a wait of at least one minute.
		{"a redelivery answered 403 naming a secondary rate limit", http.MethodPost, http.StatusForbidden, func(*harness) http.Header { return nil }},
		{"the listing answered 429 with Retry-After", http.MethodGet, http.StatusTooManyRequests, retryAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for i := range 3 {
				h.fail(fmt.Sprintf("guid-%d", i), http.StatusServiceUnavailable, time.Minute)
			}
			h.webhook.Limit(tc.method, tc.status, tc.header(h))
			if report := h.sweep(redeliver.Options{}); report.Complete {
				t.Fatalf("a rate-limited sweep reported itself complete: %+v", report)
			}
			wantSent := 0
			if tc.method == http.MethodPost {
				wantSent = 1
			}
			if got := len(h.requests()); got != wantSent {
				t.Fatalf("redelivery requests after the limit = %d, want %d", got, wantSent)
			}
			if got := h.logs.count("webhook redelivery rate-limited by GitHub", "level=WARN"); got != 1 {
				t.Fatalf("rate limit logged %d times, want once:\n%s", got, h.logs)
			}

			// GitHub would answer now, but the sweep does not ask before the time GitHub gave.
			h.webhook.Limit(tc.method, 0, nil)
			listings := h.webhook.Listings()
			h.clock.advance(30 * time.Second)
			h.sweep(redeliver.Options{})
			if got := len(h.requests()); got != wantSent || h.webhook.Listings() != listings {
				t.Fatalf("a sweep before the retry time called GitHub: %d requests (want %d), %d listings (want %d)", got, wantSent, h.webhook.Listings(), listings)
			}

			h.clock.advance(31 * time.Second)
			report := h.sweep(redeliver.Options{})
			for i := range 3 {
				if got := outcome(report, fmt.Sprintf("guid-%d", i)); got != redeliver.Redelivered {
					t.Fatalf("after the retry time: guid-%d outcome %q, want %q", i, got, redeliver.Redelivered)
				}
			}
			if got := h.logs.count("webhook redelivered", "attempt=1"); got != 3 {
				t.Fatalf("%d deliveries redelivered as their first attempt, want 3:\n%s", got, h.logs)
			}
		})
	}
}

// pausedListing is a GitHub whose listing waits to be released and then lists nothing: a sweeper
// that has read the shared state and is still talking to GitHub while another sweeper acts.
type pausedListing struct {
	redeliver.GitHub
	entered, release chan struct{}
}

func (p pausedListing) Deliveries(context.Context, time.Time) ([]githubapp.Delivery, error) {
	close(p.entered)
	<-p.release
	return nil, nil
}

// The rate limit is one record every sweeper on the bucket shares. A sweeper that read a limit
// already past and finished clear of it clears that record and no other: a limit another sweeper
// recorded while it ran stands, and a third sweeper sends GitHub nothing before it.
func TestASweepClearsOnlyTheRateLimitItRead(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-limited", http.StatusServiceUnavailable, time.Minute)
	h.webhook.Limit(http.MethodPost, http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}})
	h.sweep(redeliver.Options{})
	h.clock.advance(61 * time.Second) // the limit has passed, and its record is still there

	paused := pausedListing{GitHub: h.client, entered: make(chan struct{}), release: make(chan struct{})}
	first := h.sweeper()
	first.GitHub = paused
	done := make(chan error, 1)
	go func() {
		_, err := first.Sweep(context.Background(), redeliver.Options{})
		done <- err
	}()
	<-paused.entered
	limited := h.sweep(redeliver.Options{})
	if limited.RateLimitedUntil.IsZero() {
		t.Fatalf("the second sweeper was not rate-limited: %+v", limited)
	}
	close(paused.release)
	if err := <-done; err != nil {
		t.Fatalf("the first sweeper: %v", err)
	}

	h.webhook.Limit(http.MethodPost, 0, nil)
	listings, requests := h.webhook.Listings(), len(h.requests())
	third := h.sweep(redeliver.Options{})
	if h.webhook.Listings() != listings || len(h.requests()) != requests {
		t.Fatalf("a third sweeper called GitHub inside the second's limit: %d listings (was %d), %d requests (was %d)",
			h.webhook.Listings(), listings, len(h.requests()), requests)
	}
	if !third.RateLimitedUntil.Equal(limited.RateLimitedUntil) {
		t.Fatalf("the third sweeper saw a limit until %s, want the second's %s", third.RateLimitedUntil, limited.RateLimitedUntil)
	}
}

// A GUID GitHub recorded an OK attempt for is done, whatever its failures: its record is closed
// and it is never asked for again, even once its attempts are older than the window. Here GitHub
// carried out a redelivery request but its answer was lost (a 500), so the sweep first recorded a
// refusal.
func TestADeliveryGitHubRecordsAsDeliveredIsNotAskedAgain(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-answer-lost", http.StatusBadGateway, time.Minute)
	h.webhook.AnswerAfterRedelivering(failed.ID, http.StatusInternalServerError)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-answer-lost"); got != redeliver.RequestRefused {
		t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.RequestRefused)
	}

	h.clock.advance(2*time.Minute + time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-answer-lost"); got != redeliver.Delivered {
		t.Fatalf("after GitHub recorded the redelivery OK: outcome %q, want %q", got, redeliver.Delivered)
	}
	h.clock.advance(2 * time.Hour)
	h.sweep(redeliver.Options{})
	if got := len(h.requests()); got != 1 {
		t.Fatalf("redelivery requests %d, want 1: a delivered GUID was asked for again", got)
	}
}

// A 403 that carries no rate limit (the App lacks a permission) is GitHub refusing that one
// request: the sweep records the refusal and goes on to the next delivery.
func TestAForbiddenAnswerWithoutARateLimitIsARefusal(t *testing.T) {
	h := newHarness(t)
	for i := range 3 {
		failed := h.fail(fmt.Sprintf("guid-%d", i), http.StatusServiceUnavailable, time.Minute)
		h.webhook.Refuse(failed.ID, http.StatusForbidden)
	}
	report := h.sweep(redeliver.Options{})
	if got := report.Count(redeliver.RequestRefused); got != 3 || !report.Complete {
		t.Fatalf("refused %d of 3, complete=%t; want every delivery asked and refused", got, report.Complete)
	}
}

// Two sweepers over one state bucket (a deploy's overlap) request each delivery once.
func TestConcurrentSweepersRequestEachDeliveryOnce(t *testing.T) {
	h := newHarness(t)
	for i := range 20 {
		h.fail(fmt.Sprintf("guid-%02d", i), http.StatusServiceUnavailable, time.Minute)
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.sweeper().Sweep(context.Background(), redeliver.Options{}); err != nil {
				t.Errorf("sweep: %v", err)
			}
		}()
	}
	wg.Wait()

	requests := h.requests()
	unique := slices.Compact(slices.Sorted(slices.Values(requests)))
	if len(requests) != 20 || len(unique) != 20 {
		t.Fatalf("requests %d (%d distinct), want each of 20 deliveries once", len(requests), len(unique))
	}
}

// With no cursor the sweep looks back one hour and no further: older failures are the backlog an
// operator decides about. After a gap it resumes from its cursor, so a failure while it was not
// running is still caught.
func TestFirstSweepLooksBackOneHourThenResumesFromItsCursor(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-backlog", http.StatusServiceUnavailable, 2*time.Hour)
	recent := h.fail("guid-recent", http.StatusServiceUnavailable, 30*time.Minute)

	h.sweep(redeliver.Options{})
	if got, want := h.requests(), []int64{recent.ID}; !slices.Equal(got, want) {
		t.Fatalf("first sweep requested %v, want only the failure inside the hour %v", got, want)
	}

	h.clock.advance(3 * time.Hour)
	inGap := h.fail("guid-in-gap", http.StatusServiceUnavailable, 2*time.Hour)
	h.sweep(redeliver.Options{})
	if got, want := h.requests(), []int64{recent.ID, inGap.ID}; !slices.Equal(got, want) {
		t.Fatalf("after a gap requested %v, want %v", got, want)
	}
	if h.logs.count("webhook redelivery sweep resuming after a gap") != 1 {
		t.Fatalf("no gap warning:\n%s", h.logs)
	}
}

// A sweep that stops at its request cap leaves its cursor where it was, so the next sweep lists
// the same window and takes the rest.
func TestASweepCutShortKeepsItsCursor(t *testing.T) {
	h := newHarness(t)
	h.sweep(redeliver.Options{}) // sets the cursor
	h.clock.advance(3 * time.Hour)
	for i := range 3 {
		h.fail(fmt.Sprintf("guid-gap-%d", i), http.StatusServiceUnavailable, 2*time.Hour)
	}

	capped := h.sweeper()
	capped.MaxRequests = 2
	if _, err := capped.Sweep(context.Background(), redeliver.Options{}); err != nil {
		t.Fatalf("capped sweep: %v", err)
	}
	if got := len(h.requests()); got != 2 {
		t.Fatalf("capped sweep made %d requests, want 2", got)
	}
	h.clock.advance(2 * time.Minute)
	h.sweep(redeliver.Options{})
	if got := len(slices.Compact(slices.Sorted(slices.Values(h.requests())))); got != 3 {
		t.Fatalf("after the next sweep %d distinct deliveries requested, want 3", got)
	}
}

// Several failed attempts under one GUID are one delivery: it is requested once, by its newest id.
func TestOneDeliveryFailedTwiceIsRequestedOnce(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-twice", http.StatusServiceUnavailable, 2*time.Minute)
	newest := h.fail("guid-twice", http.StatusServiceUnavailable, time.Minute)

	h.sweep(redeliver.Options{})
	if got, want := h.requests(), []int64{newest.ID}; !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

// A dry run reports what a sweep over the window would do and changes nothing: no request, no
// state, no alarm line. The continuous sweep that follows still looks back only one hour.
func TestDryRunReportsWithoutActing(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-backlog", http.StatusServiceUnavailable, 2*time.Hour)
	h.fail("guid-refused", http.StatusBadRequest, 10*time.Minute)

	report := h.sweep(redeliver.Options{Since: h.clock.Now().Add(-72 * time.Hour), DryRun: true})
	if got := outcome(report, "guid-backlog"); got != redeliver.WouldRedeliver {
		t.Fatalf("backlog outcome %q, want %q", got, redeliver.WouldRedeliver)
	}
	if got := outcome(report, "guid-refused"); got != redeliver.Terminal {
		t.Fatalf("refused outcome %q, want %q", got, redeliver.Terminal)
	}
	if got := h.requests(); len(got) != 0 {
		t.Fatalf("a dry run requested %v", got)
	}
	if keys, err := h.state.Keys(); err == nil && len(keys) != 0 {
		t.Fatalf("a dry run wrote state %v", keys)
	}
	if h.logs.count("webhook delivery refused terminally") != 0 {
		t.Fatalf("a dry run logged the alarm line:\n%s", h.logs)
	}

	h.sweep(redeliver.Options{})
	if got := h.requests(); len(got) != 0 {
		t.Fatalf("the continuous sweep requested %v, want nothing outside its hour", got)
	}
}
