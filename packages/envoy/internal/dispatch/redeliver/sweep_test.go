package redeliver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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
	state   bus.KeyValue
	logs    *lockedBuffer
	clock   *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	state := openState(t)
	key, pemText := githubapptest.Key(t)
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, clientID, nil)
	c := &clock{now: time.Now().UTC().Truncate(time.Second)}
	webhook.SetClock(c.Now)
	client, err := githubapp.New(&auth.AppConfig{ClientID: clientID, PEM: pemText}, webhook.URL())
	if err != nil {
		t.Fatalf("App client: %v", err)
	}
	return &harness{t: t, webhook: webhook, client: client, state: state, logs: &lockedBuffer{}, clock: c}
}

// openState opens the sweep's state bucket on the test's own NATS account.
func openState(tb testing.TB) bus.KeyValue {
	tb.Helper()
	nc, err := natsgo.Connect(testnats.URL(tb))
	if err != nil {
		tb.Fatalf("connect NATS: %v", err)
	}
	tb.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		tb.Fatalf("JetStream: %v", err)
	}
	state, err := redeliver.OpenState(js, 1)
	if err != nil {
		tb.Fatalf("open redelivery state: %v", err)
	}
	return state
}

func (h *harness) sweeper() *redeliver.Sweeper {
	return &redeliver.Sweeper{
		GitHub: h.client,
		Open:   func() (bus.KeyValue, error) { return h.state, nil },
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

// The listener answers a delivery it cannot take now with a 4xx as plainly as with a 5xx (401 for
// a signature it cannot check, 400 for a body it cannot use, 422 for a publish NATS refused), and
// what it cannot take now it may take later. So every failed attempt is asked for again on the
// doubling backoff, past five requests, until GitHub records one OK; the sweep then says once, at
// INFO, how many requests the delivery took.
func TestSweepAsksAgainForEveryFailedDeliveryUntilGitHubRecordsItOK(t *testing.T) {
	h := newHarness(t)
	answers := []int{
		http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnauthorized,
		http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusUnprocessableEntity, http.StatusOK,
	}
	var redeliveries atomic.Int64
	h.webhook.SetRedeliver(func(githubapptest.Attempt) int {
		return answers[min(int(redeliveries.Add(1)), len(answers))-1]
	})
	h.fail("guid-refused", http.StatusUnauthorized, 0)

	if got := outcome(h.sweep(redeliver.Options{}), "guid-refused"); got != redeliver.Redelivered {
		t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}
	for i, backoff := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour} {
		h.clock.advance(backoff - time.Second)
		if got := outcome(h.sweep(redeliver.Options{}), "guid-refused"); got != redeliver.Waiting {
			t.Fatalf("a second before backoff %s: outcome %q, want %q", backoff, got, redeliver.Waiting)
		}
		h.clock.advance(time.Second)
		if got := outcome(h.sweep(redeliver.Options{}), "guid-refused"); got != redeliver.Redelivered {
			t.Fatalf("after backoff %s (the redelivery before it answered %d): outcome %q, want %q", backoff, answers[i], got, redeliver.Redelivered)
		}
		if got, want := len(h.requests()), i+2; got != want {
			t.Fatalf("requests after backoff %s = %d, want %d", backoff, got, want)
		}
	}

	// The seventh redelivery was answered 200, so GitHub recorded it OK.
	h.clock.advance(time.Minute)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-refused"); got != redeliver.Delivered {
		t.Fatalf("after the redelivery answered 200: outcome %q, want %q", got, redeliver.Delivered)
	}
	h.clock.advance(time.Hour)
	h.sweep(redeliver.Options{})
	if got := len(h.requests()); got != 7 {
		t.Fatalf("requests %d, want 7: a delivered delivery was asked for again", got)
	}
	recovered := []string{"level=INFO", "guid=guid-refused", "event=pull_request_review", "action=submitted", "repository_id=7", "requests=7"}
	if got := h.logs.count("webhook delivery recovered", recovered...); got != 1 {
		t.Fatalf("recovery logged %d times with %v, want once:\n%s", got, recovered, h.logs)
	}
}

// GitHub keeps a delivery 72 hours. One still failing 72 hours after its first failed attempt is
// expired: the sweep says so once, at ERROR, and never asks for it again.
func TestSweepExpiresADeliveryStillFailingAfter72Hours(t *testing.T) {
	h := newHarness(t)
	h.webhook.SetRedeliver(func(githubapptest.Attempt) int { return http.StatusServiceUnavailable })
	first := h.fail("guid-down", http.StatusServiceUnavailable, 0)

	h.sweep(redeliver.Options{})
	for hour := range 71 {
		h.clock.advance(time.Hour)
		if got := outcome(h.sweep(redeliver.Options{}), "guid-down"); got != redeliver.Redelivered {
			t.Fatalf("%d hours after the first failed attempt: outcome %q, want %q", hour+1, got, redeliver.Redelivered)
		}
	}
	if got := len(h.requests()); got != 72 {
		t.Fatalf("requests in the 72 hours = %d, want 72", got)
	}
	h.clock.advance(time.Hour - time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-down"); got != redeliver.Waiting {
		t.Fatalf("a second before 72 hours: outcome %q, want %q", got, redeliver.Waiting)
	}
	h.clock.advance(time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-down"); got != redeliver.Expired {
		t.Fatalf("72 hours after the first failed attempt: outcome %q, want %q", got, redeliver.Expired)
	}
	for range 3 {
		h.clock.advance(time.Hour)
		h.sweep(redeliver.Options{})
	}
	if got := len(h.requests()); got != 72 {
		t.Fatalf("requests %d, want 72: an expired delivery was asked for again", got)
	}
	expired := []string{
		"level=ERROR", "guid=guid-down", "event=pull_request_review", "action=submitted", "repository_id=7", "requests=72",
		"first_delivered_at=" + first.DeliveredAt.Format(time.RFC3339), `reason="GitHub keeps a delivery 72 hours"`,
	}
	if got := h.logs.count("webhook delivery expired undelivered", expired...); got != 1 {
		t.Fatalf("expiry logged %d times with %v, want once:\n%s", got, expired, h.logs)
	}
}

// An earlier build closed a delivery for good when the listener answered it 4xx (terminal) or
// after five failed requests (exhausted). Such a record reads as open, so a delivery lost before
// the deploy, and still inside GitHub's 72 hours, is asked for again.
func TestSweepAsksAgainForALegacyTerminalOrExhaustedRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		record func(failed githubapptest.Delivery, lastRequest time.Time) string
	}{
		{"terminal", http.StatusBadRequest, func(githubapptest.Delivery, time.Time) string {
			return `{"attempts":0,"refusals":0,"terminal":true,"delivery":{"id":0,"guid":"","delivered_at":"0001-01-01T00:00:00Z","redelivery":false,"status":"","status_code":0,"event":"","action":"","repository_id":0}}`
		}},
		{"exhausted", http.StatusServiceUnavailable, func(failed githubapptest.Delivery, lastRequest time.Time) string {
			return fmt.Sprintf(`{"attempts":5,"refusals":0,"last_attempt_at":%q,"answered":[%d],"exhausted":true,`+
				`"delivery":{"id":%d,"guid":"guid-legacy","delivered_at":%q,"redelivery":false,"status":%q,"status_code":503,"event":"pull_request_review","action":"submitted","repository_id":7}}`,
				lastRequest.Format(time.RFC3339), failed.ID, failed.ID, failed.DeliveredAt.Format(time.RFC3339), failed.Status)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			failed := h.fail("guid-legacy", tc.status, 30*time.Minute)
			if _, err := h.state.Create("guid.guid-legacy", []byte(tc.record(failed, h.clock.Now().Add(-10*time.Minute)))); err != nil {
				t.Fatalf("write the legacy record: %v", err)
			}
			if got := outcome(h.sweep(redeliver.Options{}), "guid-legacy"); got != redeliver.Redelivered {
				t.Fatalf("outcome %q, want %q", got, redeliver.Redelivered)
			}
			if got, want := h.requests(), []int64{failed.ID}; !slices.Equal(got, want) {
				t.Fatalf("requests %v, want %v", got, want)
			}
		})
	}
}

// acceptingGitHub accepts every redelivery request and never makes the redelivery: GitHub answered
// 202, and its log shows no new attempt.
type acceptingGitHub struct {
	redeliver.GitHub
	mu       sync.Mutex
	requests []int64
}

func (g *acceptingGitHub) Redeliver(_ context.Context, deliveryID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, deliveryID)
	return nil
}

func (g *acceptingGitHub) requested() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// GitHub answers a redelivery request 202 and makes the redelivery afterwards. A request whose
// redelivery never appears in the log delivered nothing, so the delivery is asked for again when
// its next request is due.
func TestSweepAsksAgainWhenDueForAnAcceptedRequestWithNoNewAttempt(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-dropped", http.StatusServiceUnavailable, time.Minute)
	github := &acceptingGitHub{GitHub: h.client}
	sweeper := h.sweeper()
	sweeper.GitHub = github
	sweep := func() redeliver.Outcome {
		t.Helper()
		report, err := sweeper.Sweep(context.Background(), redeliver.Options{})
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return outcome(report, "guid-dropped")
	}

	if got := sweep(); got != redeliver.Redelivered {
		t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}
	for _, backoff := range []time.Duration{2 * time.Minute, 4 * time.Minute} {
		h.clock.advance(backoff - time.Second)
		if got := sweep(); got != redeliver.Waiting {
			t.Fatalf("a second before backoff %s: outcome %q, want %q", backoff, got, redeliver.Waiting)
		}
		h.clock.advance(time.Second)
		if got := sweep(); got != redeliver.Redelivered {
			t.Fatalf("after backoff %s with no new attempt: outcome %q, want %q", backoff, got, redeliver.Redelivered)
		}
	}
	if got, want := github.requested(), []int64{failed.ID, failed.ID, failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

// A failed attempt leaves the running sweep's hour long before GitHub's 72 hours are up. A delivery
// whose record is open is decided from that record when the listing no longer returns its
// attempts: here an operator's sweep asked for a failure two hours old, GitHub accepted and never
// made the redelivery, and the next running sweep asks again.
func TestSweepDecidesAnOpenDeliveryWhoseAttemptsLeftTheWindowFromItsRecord(t *testing.T) {
	h := newHarness(t)
	github := &acceptingGitHub{GitHub: h.client}
	sweeper := h.sweeper()
	sweeper.GitHub = github
	sweep := func(opts redeliver.Options) redeliver.Report {
		t.Helper()
		report, err := sweeper.Sweep(context.Background(), opts)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return report
	}
	sweep(redeliver.Options{}) // the cursor
	failed := h.fail("guid-old", http.StatusServiceUnavailable, 2*time.Hour)
	if got := outcome(sweep(redeliver.Options{Since: h.clock.Now().Add(-72 * time.Hour)}), "guid-old"); got != redeliver.Redelivered {
		t.Fatalf("the operator's sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}

	h.clock.advance(2*time.Minute + time.Second)
	report := sweep(redeliver.Options{})
	if report.Listed != 0 {
		t.Fatalf("the running sweep listed %d failed attempts, want none inside its hour", report.Listed)
	}
	if got := outcome(report, "guid-old"); got != redeliver.Redelivered {
		t.Fatalf("the running sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}
	if got, want := github.requested(), []int64{failed.ID, failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

// GitHub answers a redelivery request 404 when it no longer has the delivery. No later request can
// succeed, so the delivery is expired with that reason, logged once, and never asked for again.
func TestSweepExpiresADeliveryWhoseRequestIsAnswered404(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-gone", http.StatusServiceUnavailable, time.Minute)
	h.webhook.Refuse(failed.ID, http.StatusNotFound)

	if got := outcome(h.sweep(redeliver.Options{}), "guid-gone"); got != redeliver.Expired {
		t.Fatalf("outcome %q, want %q", got, redeliver.Expired)
	}
	h.clock.advance(2*time.Minute + time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-gone"); got != redeliver.Closed {
		t.Fatalf("the next sweep: outcome %q, want %q", got, redeliver.Closed)
	}
	if got, want := h.requests(), []int64{failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
	expired := []string{"level=ERROR", "guid=guid-gone", "requests=1", `reason="GitHub no longer has the delivery"`}
	if got := h.logs.count("webhook delivery expired undelivered", expired...); got != 1 {
		t.Fatalf("expiry logged %d times with %v, want once:\n%s", got, expired, h.logs)
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
// still asked again after the backoff, and a delivery GitHub keeps refusing is asked for at every
// due time, past five requests, like one listed every time.
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
		t.Run(tc.name+" then refused past five requests", func(t *testing.T) {
			h := newHarness(t)
			opts := tc.first(h)
			old := h.fail("guid-old", http.StatusServiceUnavailable, 2*time.Hour)
			h.webhook.Refuse(old.ID, http.StatusUnprocessableEntity)
			h.sweep(opts)
			for range 8 {
				h.clock.advance(time.Hour) // past every backoff
				h.sweep(redeliver.Options{})
			}
			if got := len(h.requests()); got != 9 {
				t.Fatalf("requests %d, want 9: the first and one at each of eight due times", got)
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

func (p pausedListing) Deliveries(context.Context, time.Time, func() error) ([]githubapp.Delivery, error) {
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

// pausedClaim is the sweep's state bucket, whose first Create (a sweeper claiming a delivery)
// waits to be released once it is written: a sweeper preempted between its claim and its request.
type pausedClaim struct {
	natsgo.KeyValue
	once             sync.Once
	entered, release chan struct{}
}

func (p *pausedClaim) Create(key string, value []byte) (uint64, error) {
	revision, err := p.KeyValue.Create(key, value)
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return revision, err
}

// A sweeper reads the shared rate limit again right before each request it sends GitHub, so one
// that claimed a delivery before another sweeper recorded a limit sends nothing once it is in
// force, and undoes its claim.
func TestASweepPausedBeforeItsRequestSendsNothingUnderANewLimit(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-older", http.StatusServiceUnavailable, 2*time.Minute)
	h.fail("guid-newer", http.StatusServiceUnavailable, time.Minute)
	h.webhook.Limit(http.MethodPost, http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}})

	paused := &pausedClaim{KeyValue: h.state, entered: make(chan struct{}), release: make(chan struct{})}
	second := h.sweeper()
	second.Open = func() (bus.KeyValue, error) { return bus.KeyValue{KeyValue: paused}, nil }
	reports := make(chan redeliver.Report, 1)
	go func() {
		report, err := second.Sweep(context.Background(), redeliver.Options{})
		if err != nil {
			t.Errorf("the paused sweeper: %v", err)
		}
		reports <- report
	}()
	<-paused.entered // it has claimed guid-newer and not yet asked GitHub for it
	first := h.sweep(redeliver.Options{})
	if first.RateLimitedUntil.IsZero() || len(h.requests()) != 1 {
		t.Fatalf("the first sweeper sent %d requests and recorded no limit: %+v", len(h.requests()), first)
	}
	close(paused.release)
	report := <-reports
	if got := len(h.requests()); got != 1 {
		t.Fatalf("redelivery requests %d, want 1: the paused sweeper asked GitHub inside the new limit", got)
	}
	if report.Complete || !report.RateLimitedUntil.Equal(first.RateLimitedUntil) {
		t.Fatalf("the paused sweeper reported complete=%t until %s, want it stopped at %s", report.Complete, report.RateLimitedUntil, first.RateLimitedUntil)
	}

	// Its claim was undone: once the limit passes, guid-newer is asked for as its first attempt.
	h.webhook.Limit(http.MethodPost, 0, nil)
	h.clock.advance(2*time.Minute + time.Second)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-newer"); got != redeliver.Redelivered {
		t.Fatalf("after the limit: guid-newer outcome %q, want %q", got, redeliver.Redelivered)
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

// With no cursor the sweep looks back the 72 hours GitHub keeps, so the first failed attempt of
// every delivery GitHub still has is in its listing. After a gap it resumes from its cursor, so a
// failure while it was not running is still caught.
func TestFirstSweepLooksBack72HoursThenResumesFromItsCursor(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-gone", http.StatusServiceUnavailable, 73*time.Hour)
	backlog := h.fail("guid-backlog", http.StatusServiceUnavailable, 50*time.Hour)
	recent := h.fail("guid-recent", http.StatusServiceUnavailable, 30*time.Minute)

	h.sweep(redeliver.Options{})
	if got, want := h.requests(), []int64{recent.ID, backlog.ID}; !slices.Equal(got, want) {
		t.Fatalf("first sweep requested %v, want the failures inside the 72 hours %v", got, want)
	}

	h.clock.advance(3 * time.Hour)
	inGap := h.fail("guid-in-gap", http.StatusServiceUnavailable, 2*time.Hour)
	h.sweep(redeliver.Options{})
	if got, want := h.requests(), []int64{recent.ID, backlog.ID, inGap.ID}; !slices.Equal(got, want) {
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
// state, no alarm line, no cursor. The continuous sweep that follows is a first sweep.
func TestDryRunReportsWithoutActing(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-expiring", http.StatusServiceUnavailable, 72*time.Hour)
	h.fail("guid-backlog", http.StatusServiceUnavailable, 2*time.Hour)
	h.fail("guid-refused", http.StatusBadRequest, 10*time.Minute)

	report := h.sweep(redeliver.Options{Since: h.clock.Now().Add(-72 * time.Hour), DryRun: true})
	for guid, want := range map[string]redeliver.Outcome{
		"guid-expiring": redeliver.Expired,
		"guid-backlog":  redeliver.WouldRedeliver,
		"guid-refused":  redeliver.WouldRedeliver,
	} {
		if got := outcome(report, guid); got != want {
			t.Fatalf("%s outcome %q, want %q", guid, got, want)
		}
	}
	if got := h.requests(); len(got) != 0 {
		t.Fatalf("a dry run requested %v", got)
	}
	if keys, err := h.state.Keys(); err == nil && len(keys) != 0 {
		t.Fatalf("a dry run wrote state %v", keys)
	}
	if h.logs.count("webhook delivery expired undelivered") != 0 {
		t.Fatalf("a dry run logged the alarm line:\n%s", h.logs)
	}

	h.sweep(redeliver.Options{})
	if got := len(h.requests()); got != 2 {
		t.Fatalf("the continuous sweep made %d requests, want 2: a first sweep over the 72 hours", got)
	}
	if got := h.logs.count("webhook delivery expired undelivered", "guid=guid-expiring"); got != 1 {
		t.Fatalf("the continuous sweep logged the expiry %d times, want once:\n%s", got, h.logs)
	}
}

// The running sweep's summary line counts every outcome, so each delivery it lists is under one of
// them. A failure GitHub has since recorded OK stays in the sweep's hour as closed: while the
// summary left closed out, production logged deliveries=65 with every count at zero for the hour
// after all 65 were redelivered and delivered.
func TestTheSweepSummaryCountsEveryOutcome(t *testing.T) {
	leases := redeliverTestLeases(t)
	h := newHarness(t)
	h.fail("guid-settled", http.StatusServiceUnavailable, time.Minute)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-settled"); got != redeliver.Redelivered {
		t.Fatalf("first sweep: outcome %q, want %q", got, redeliver.Redelivered)
	}
	h.clock.advance(redeliver.Interval)
	if got := outcome(h.sweep(redeliver.Options{}), "guid-settled"); got != redeliver.Delivered {
		t.Fatalf("second sweep: outcome %q, want %q", got, redeliver.Delivered)
	}
	h.clock.advance(redeliver.Interval)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.sweeper().Run(ctx, time.Hour, leases)
	}()
	for deadline := time.Now().Add(10 * time.Second); h.logs.count("webhook redelivery sweep") == 0; {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no summary line:\n%s", h.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	want := []string{
		" failed_attempts=1", " deliveries=1",
		" redelivered=0", " would_redeliver=0", " request_refused=0", " rate_limited=0",
		" waiting=0", " delivered=0", " expired=0",
		" closed=1", " claimed_elsewhere=0", " complete=true",
	}
	if h.logs.count("webhook redelivery sweep", want...) != 1 {
		t.Fatalf("summary line does not carry %v:\n%s", want, h.logs)
	}
}

// listingGitHub lists the same failed attempts on every call and counts the redelivery requests
// made.
type listingGitHub struct {
	listed   []githubapp.Delivery
	requests atomic.Int64
}

func (g *listingGitHub) Deliveries(context.Context, time.Time, func() error) ([]githubapp.Delivery, error) {
	return g.listed, nil
}

func (g *listingGitHub) Redeliver(context.Context, int64) error {
	g.requests.Add(1)
	return nil
}

// BenchmarkSweepWithLongPendingRecords is one running sweep over n open deliveries, each listed,
// each with a record whose last request was refused a minute ago, so none is due: the sweep reads
// every record and asks GitHub for nothing.
func BenchmarkSweepWithLongPendingRecords(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			state := openState(b)
			now := time.Now().UTC().Truncate(time.Second)
			github := &listingGitHub{}
			for i := range n {
				delivery := githubapp.Delivery{
					ID: int64(i + 1), GUID: fmt.Sprintf("guid-%05d", i), DeliveredAt: now.Add(-time.Hour),
					Status: "Invalid HTTP Response: 503", StatusCode: http.StatusServiceUnavailable,
					Event: "pull_request", Action: "opened", RepositoryID: 7,
				}
				github.listed = append(github.listed, delivery)
				data, err := json.Marshal(map[string]any{
					"attempts": 1, "refusals": 1, "last_attempt_at": now.Add(-time.Minute),
					"next_attempt_at": now.Add(time.Hour), "first_delivered_at": delivery.DeliveredAt,
					"delivery": delivery,
				})
				if err != nil {
					b.Fatalf("encode record: %v", err)
				}
				if _, err := state.Create("guid."+delivery.GUID, data); err != nil {
					b.Fatalf("write record %d: %v", i, err)
				}
			}
			sweeper := &redeliver.Sweeper{GitHub: github, Open: func() (bus.KeyValue, error) { return state, nil }, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
			for b.Loop() {
				report, err := sweeper.Sweep(context.Background(), redeliver.Options{})
				if err != nil {
					b.Fatalf("sweep: %v", err)
				}
				if got := report.Count(redeliver.Waiting); got != n {
					b.Fatalf("%d of %d deliveries waiting", got, n)
				}
			}
			if got := github.requests.Load(); got != 0 {
				b.Fatalf("the sweeps made %d redelivery requests, want none", got)
			}
		})
	}
}

// A sweeper opens its state when it sweeps, as envoy-dispatch builds it for both the running loop
// and the redeliver-webhooks command, and only once: the command calls Sweep directly, with no
// loop ahead of it to open the bucket.
func TestASweepOpensTheStateItWasBuiltWithout(t *testing.T) {
	h := newHarness(t)
	failed := h.fail("guid-a", http.StatusServiceUnavailable, time.Minute)
	sweeper := h.sweeper()
	opens := 0
	sweeper.Open = func() (bus.KeyValue, error) {
		opens++
		return h.state, nil
	}

	report, err := sweeper.Sweep(context.Background(), redeliver.Options{Since: h.clock.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := outcome(report, "guid-a"); got != redeliver.Redelivered {
		t.Fatalf("outcome %q, want %q", got, redeliver.Redelivered)
	}
	if got, want := h.requests(), []int64{failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("redelivery requests %v, want %v", got, want)
	}
	if _, err := sweeper.Sweep(context.Background(), redeliver.Options{Since: h.clock.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if opens != 1 {
		t.Fatalf("the state was opened %d times over two sweeps, want once", opens)
	}
}

// A sweep whose state cannot be opened returns the error and asks GitHub nothing.
func TestASweepWhoseStateWillNotOpenFails(t *testing.T) {
	h := newHarness(t)
	h.fail("guid-a", http.StatusServiceUnavailable, time.Minute)
	sweeper := h.sweeper()
	refused := errors.New("NATS is not connected")
	sweeper.Open = func() (bus.KeyValue, error) { return bus.KeyValue{}, refused }

	if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); !errors.Is(err, refused) {
		t.Fatalf("sweep error %v, want %v", err, refused)
	}
	if got := h.requests(); len(got) != 0 {
		t.Fatalf("a sweep without its state requested %v", got)
	}
}
