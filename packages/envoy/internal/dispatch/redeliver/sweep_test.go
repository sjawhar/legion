package redeliver_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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

	// The redelivery succeeded, so GitHub recorded no further failure: nothing more to do.
	h.clock.advance(10 * time.Minute)
	report = h.sweep(redeliver.Options{})
	if got := outcome(report, "guid-a"); got != redeliver.Pending {
		t.Fatalf("second sweep outcome %q, want %q", got, redeliver.Pending)
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
