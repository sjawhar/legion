package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// The delivery harness names its counters test_messages_*; they are the listener's
// envoy_messages_delivered_total and envoy_messages_naked_total (main.go) under a test name, so the
// series below are those two counters' series.
const (
	deliveredCounter = "test_messages_delivered"
	nakedCounter     = "test_messages_naked"
)

// deliveryRecord is one line of the listener's delivery log, which logging.DeliveryLog writes as
// JSON: the four fields every delivery line carries, and the three the deferral and give-up lines
// add. A field the line does not carry decodes as its zero value, which is how an assertion tells
// a give-up line (no attempt) from a deferral.
type deliveryRecord struct {
	Level          string `json:"level"`
	Msg            string `json:"msg"`
	SessionID      string `json:"session_id"`
	Topic          string `json:"topic"`
	EventID        string `json:"event_id"`
	DeliveryStatus string `json:"delivery_status"`
	Reason         string `json:"reason"`
	Attempt        uint64 `json:"attempt"`
	// WindowDeferrals is the deferral budget the attempt was measured against, which a deferral
	// line carries in place of the stream age it named before: the window is counted in
	// deliveries from the first one, so the age bounds nothing.
	WindowDeferrals uint64 `json:"window_deferrals"`
	// Error is the error a failure line carries, exceptionPublishFailedLine's among them.
	Error string `json:"error"`
}

// deliveryRecords decodes every line the harness logged whose msg is message. Counting them is how
// the tests below assert "exactly one", which is the whole claim of a give-up: a message disposed
// of once, however many times it was delivered.
func deliveryRecords(t *testing.T, logs, message string) []deliveryRecord {
	t.Helper()
	var found []deliveryRecord
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record deliveryRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record.Msg == message {
			found = append(found, record)
		}
	}
	return found
}

// metricsBody is the listener's /metrics as an operator reads it, from the harness's own registry.
func metricsBody(t *testing.T, harness listenerDeliveryHarness) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	harness.metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/metrics: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// counterValue reads one counter series out of a metrics body: series is the metric's name with its
// label set, as /metrics renders it (`test_messages_delivered{delivery_status="delivered"}`), or a
// bare name for an unlabelled counter. It reports -1 when the body carries no such series, since a
// label set is written only once it has been incremented - so "never incremented" is the absence of
// the line, not a zero on it, and the HELP and TYPE lines, which begin with "# ", are never it.
func counterValue(t *testing.T, body, series string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		var count int64
		if _, err := fmt.Sscanf(value, "%d", &count); err != nil {
			t.Fatalf("metrics line %q does not carry a count: %v", line, err)
		}
		return count
	}
	return -1
}

// deliveredSeries is the delivered counter's series for one delivery_status label.
func deliveredSeries(status string) string {
	return fmt.Sprintf(`%s{delivery_status=%q}`, deliveredCounter, status)
}

// A delivery to an agent subject reads its target from the session bucket itself when this
// listener's cache has not seen it, exactly as the send route does
// (TestSendReadsTheSessionStoreWhileTheCacheIsCold, send_route_acceptance_test.go): the cache
// follows the bucket through a watcher and so trails it, and a delivery that takes the cache's miss
// for an answer loses the message - it is counted skipped and ACKed away, with nothing left to
// redeliver. This drives the listener's own delivery handler with a session another listener
// registered a moment ago, through a listener whose cache was warm before that write and has
// followed nothing since, which is the same miss a cache still refilling after a reconnect has.
//
// The negative control is the Get below: if the cache held the target the push would prove nothing
// about reading through to the bucket, since every warm-cache delivery already pushes.
func TestAgentDeliveryReadsTheSessionStoreWhileTheCacheIsCold(t *testing.T) {
	const target = "ses_cold_cache_delivery_target"
	// The handler runs on this goroutine, so the pushes it makes are recorded without
	// synchronisation.
	var pushed []string
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		pushed = append(pushed, request.URL.String())
		return listenerDeliveryResponse(http.StatusNoContent), nil
	}))

	// trailingListeners' arrangement (api_test.go), applied to the delivery harness: the
	// delivering listener's cache is warmed and its watcher stopped, and the target is registered
	// through a second registry on the same account, so the record reaches the bucket and never
	// reaches the cache.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := harness.sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the delivering listener's session cache: %v", err)
	}
	harness.sessions.StopWatch()
	registrar, err := session.OpenSessionRegistry(harness.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the registering listener's session registry: %v", err)
	}
	if err := registrar.Put(target, session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/" + target, Title: "cold target",
	}); err != nil {
		t.Fatalf("register %s through the other listener: %v", target, err)
	}

	if entry, err := harness.sessions.Get(target); err == nil {
		t.Fatalf("the delivering listener's cache already holds %s (%+v): the delivery below would prove nothing about reading through to the bucket", target, entry)
	} else if !strings.Contains(err.Error(), natsgo.ErrKeyNotFound.Error()) {
		t.Fatalf("the delivering listener's cache answers %v for %s, want %v", err, target, natsgo.ErrKeyNotFound)
	}

	item := listenerTestEnvelope(contracts.AgentSubject(target), "cold-cache-agent-delivery")
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	if len(pushed) != 1 {
		t.Fatalf("the cold-cache delivery pushed %d times (%v), want exactly one push to the record only the bucket holds:\n%s", len(pushed), pushed, harness.logs.String())
	}
	if want := fmt.Sprintf(":1/session/%s/prompt_async", target); !strings.HasSuffix(pushed[0], want) {
		t.Fatalf("the push went to %s, want a request ending %s - the port and session the bucket's record names", pushed[0], want)
	}
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("delivered")); got != 1 {
		t.Fatalf("%s = %d, want 1 - a delivery that read through to the bucket is delivered, not skipped or given up:\n%s", deliveredSeries("delivered"), got, body)
	}
}

// unplaceableDeliveryRun is one envelope driven through the listener's own
// jetStreamDeliveryHandler over a real durable consumer. The package's other delivery tests hand
// the handler a synthetic &natsgo.Msg{Data: data}, which carries no JetStream metadata: the
// handler can bound its delivery count against nothing, so it gives up at once, and nothing about
// a retry can be learned from it. Here the server holds the message and performs the redelivery
// the handler's NAK asks for, as it does in production.
type unplaceableDeliveryRun struct {
	harness listenerDeliveryHarness
	item    contracts.Envelope
	// exceptions reads the delivery exceptions published for the envelope's own topic.
	exceptions *natsgo.Subscription
	// consumer names the run's durable, for a test that reads back what the stream still holds.
	consumer string
}

// oneDeferralWindow is a retry window that buys exactly one deferral: the handler spends a window
// in whole unplaceableRetryDelay steps counted from the first delivery, so one delay and a half
// buys one deferral whatever the delay itself is. The first delivery is deferred and the
// redelivery, the second, is given up - a decision made on the delivery count alone, so no slow
// machine can move it.
const oneDeferralWindow = unplaceableRetryDelay + unplaceableRetryDelay/2

// unplaceableHarness is the listener the runs below drive: a transport that fails the test on any
// push, for the runs whose sessions have nothing to push to at all.
func unplaceableHarness(t *testing.T) listenerDeliveryHarness {
	t.Helper()
	return newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("a session with nothing to push to must never be pushed to, got %s", request.URL)
	}))
}

// startUnplaceableDeliveryRun drives one envelope for target - a session the bucket has no record
// of and no interest names - through its own harness at window.
func startUnplaceableDeliveryRun(t *testing.T, target string, window, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	harness := unplaceableHarness(t)
	// The harness builds its handler before a test can set the window, so each run builds its own
	// from the harness's configuration - the one seam the harness exposes for exactly this.
	cfg := harness.config
	cfg.unplaceableRetryWindow = window
	return startUnplaceableDelivery(t, harness, cfg, target, soak)
}

// startUnplaceableDelivery publishes one envelope for target, lets it sit in the stream for soak,
// and then runs a handler built from cfg over target's own durable consumer. It returns once the
// handler is bound; the deliveries happen on the subscription's goroutine. Several runs may share
// one harness: each has its own subject, consumer and exception probe, and they report into the
// one registry and the one log an operator would read.
//
// The publish comes first because a run that ages its message needs the message in the stream
// before anything can deliver it, and the subscription is made before the consumer is created,
// never after: the server pushes a pending message the instant the consumer exists, and a push to
// an inbox nobody is listening on is lost until AckWait, which would make the first delivery the
// handler sees its second.
func startUnplaceableDelivery(t *testing.T, harness listenerDeliveryHarness, cfg listenerDeliveryHandlerConfig, target string, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	return startDeliveryThrough(t, harness, jetStreamDeliveryHandler(cfg), target, soak)
}

// startDeliveryThrough is startUnplaceableDelivery with the subscription's handler given whole,
// for a run that must put something in front of the listener's own handler.
func startDeliveryThrough(t *testing.T, harness listenerDeliveryHarness, handler natsgo.MsgHandler, target string, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	return startEnvelopeDeliveryThrough(t, harness, handler, target, listenerTestEnvelope(contracts.AgentSubject(target), "unplaceable-"+target), soak)
}

// startEnvelopeDeliveryThrough is startDeliveryThrough with the envelope given whole, for a run
// whose envelope must carry something the default one does not; item.Topic is target's subject.
func startEnvelopeDeliveryThrough(t *testing.T, harness listenerDeliveryHarness, handler natsgo.MsgHandler, target string, item contracts.Envelope, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	subject := item.Topic
	exceptions, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + subject)
	if err != nil {
		t.Fatalf("subscribe the exception probe for %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}
	if err := harness.client.Publish(item); err != nil {
		t.Fatalf("publish %s to %s: %v", item.EventID, subject, err)
	}
	time.Sleep(soak)

	inbox := natsgo.NewInbox()
	subscription, err := harness.client.Conn.Subscribe(inbox, handler)
	if err != nil {
		t.Fatalf("subscribe the delivery handler on %s: %v", inbox, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the delivery subscription: %v", err)
	}

	consumer := "listener-" + strings.ReplaceAll(target, "_", "-")
	// The listener's own consumer policy - explicit ack, consumerMaxDeliver - over this one
	// subject, so the run bounds nothing the production consumer does not. AckWait is the one
	// departure: the redeliveries here are the ones the handler asks for by NAK, and a 60 s
	// consumerAckWait would hold a handler that asked for neither an ack nor a retry past any
	// deadline this test could wait out.
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: inbox}
	applyListenerConsumerPolicy(&config, []string{subject})
	config.AckWait = 10 * time.Second
	if _, err := harness.client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the durable %s over %s: %v", consumer, subject, err)
	}
	t.Cleanup(func() { _ = harness.client.JS().DeleteConsumer(bus.Stream, consumer) })
	return unplaceableDeliveryRun{harness: harness, item: item, exceptions: exceptions, consumer: consumer}
}

// A message for a session this listener cannot place is kept in the stream and redelivered for the
// length of the retry window, and only then given up - once, with a record, a counter and a
// no_holder exception of its own. Before this the same message was ACKed away on its first delivery
// as a skip, which is a message lost with no trace an operator could act on.
//
// The window is oneDeferralWindow, so the arithmetic is: the first delivery is deferral 1 of the
// one the window buys; the redelivery it asks for arrives 5 s later as delivery 2, past what the
// window paid for, and is given up. Both are counts, not clocks, so no slow machine moves either.
//
// The redelivery must not be swallowed as an attempt-dedupe skip: the deferral clears the attempt
// entry the delivery recorded, and without that clearing the retry this test proves would be
// disposed of before it reached the session at all. The negative control is the zero-window
// subtest: there the same message is given up on its first delivery, with no NAK and no deferral,
// so a pass here cannot come from a rig that defers nothing.
func TestUnplaceableAgentDeliveryRetriesThenGivesUp(t *testing.T) {
	t.Run("a window retries, then gives up once", func(t *testing.T) {
		run := startUnplaceableDeliveryRun(t, "ses_unplaceable_retried", oneDeferralWindow, 0)
		harness := run.harness

		waitFor(t, 15*time.Second, "the first delivery to be deferred", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) > 0
		})
		deferrals := deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")
		if deferrals[0].Reason != "no_session_record" {
			t.Fatalf("the deferral names reason %q, want no_session_record - the bucket's own word that no record exists:\n%s", deferrals[0].Reason, harness.logs.String())
		}
		if deferrals[0].Attempt != 1 {
			t.Fatalf("the deferral is attempt %d, want the first delivery:\n%s", deferrals[0].Attempt, harness.logs.String())
		}
		// The line names the bound the attempt was measured against, not the message's stream
		// age: an operator reading "attempt 1 of 1 deferral" can see why the next delivery is
		// the last, which the age never told them.
		if deferrals[0].WindowDeferrals != 1 {
			t.Fatalf("the deferral names window_deferrals %d, want the one deferral this window buys:\n%s", deferrals[0].WindowDeferrals, harness.logs.String())
		}
		if deferrals[0].SessionID != strings.TrimPrefix(run.item.Topic, contracts.AgentTopicPrefix) {
			t.Fatalf("the deferral names session %q, want the envelope's target %q", deferrals[0].SessionID, run.item.Topic)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != 1 {
			t.Fatalf("%s = %d, want 1 - a deferred delivery is NAKed back to the stream, not acknowledged:\n%s", nakedCounter, got, body)
		}
		for _, status := range []string{"delivered", "skipped", "unplaceable_no_record", "unplaceable_no_push_target", "unplaceable_push_failed"} {
			if got := counterValue(t, body, deliveredSeries(status)); got != -1 {
				t.Fatalf("%s = %d while the message is still in the stream, want no series at all - a deferral disposes of nothing:\n%s", deliveredSeries(status), got, body)
			}
		}
		// The no_holder notice goes out once, when the message is given up: published per attempt
		// it would be read back by every listener in the fleet a dozen times over.
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Fatalf("a deferred delivery published a delivery exception already: %s", string(message.Data))
		}

		waitFor(t, 20*time.Second, "the redelivered message to be given up", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) > 0
		})
		logs := harness.logs.String()
		if skips := deliveryRecords(t, logs, "listener attempt-dedupe skip"); len(skips) != 0 {
			t.Fatalf("the redelivery was swallowed as an attempt-dedupe skip (%d lines): the deferral must clear the attempt entry it recorded, or the retry never reaches the session:\n%s", len(skips), logs)
		}
		if got := len(deliveryRecords(t, logs, "listener agent delivery deferred")); got != 1 {
			t.Fatalf("%d deferral lines, want exactly one - a window of one deferral defers the first delivery only:\n%s", got, logs)
		}
		giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
		if len(giveUps) != 1 {
			t.Fatalf("%d give-up lines, want exactly one per message:\n%s", len(giveUps), logs)
		}
		if giveUps[0].Level != "WARN" {
			t.Fatalf("the give-up line is %s, want WARN - a message given up is the operator's business:\n%s", giveUps[0].Level, logs)
		}
		if giveUps[0].Reason != "no_session_record" || giveUps[0].DeliveryStatus != "unplaceable_no_record" {
			t.Fatalf("the give-up line names reason %q and delivery_status %q, want no_session_record and unplaceable_no_record - the status must be the counter's own label:\n%s", giveUps[0].Reason, giveUps[0].DeliveryStatus, logs)
		}
		body = metricsBody(t, harness)
		if got := counterValue(t, body, deliveredSeries("unplaceable_no_record")); got != 1 {
			t.Fatalf("%s = %d, want exactly 1 - the message is given up once, however many times it was delivered:\n%s", deliveredSeries("unplaceable_no_record"), got, body)
		}
		if got := counterValue(t, body, deliveredSeries("skipped")); got != -1 {
			t.Fatalf("%s = %d, want no series at all - a message given up is not a skip:\n%s", deliveredSeries("skipped"), got, body)
		}
		assertDeliveryException(t, run.exceptions, run.item, "no_holder")
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Fatalf("a second delivery exception went out for one message: %s", string(message.Data))
		}
	})

	t.Run("a zero window gives up on the first delivery", func(t *testing.T) {
		run := startUnplaceableDeliveryRun(t, "ses_unplaceable_at_once", 0, 0)
		harness := run.harness

		waitFor(t, 15*time.Second, "the first delivery to be given up", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) > 0
		})
		logs := harness.logs.String()
		if got := len(deliveryRecords(t, logs, "listener agent delivery deferred")); got != 0 {
			t.Fatalf("%d deferral lines at a zero window, want none - nothing is held back when nothing is retried:\n%s", got, logs)
		}
		if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 1 {
			t.Fatalf("%d give-up lines, want exactly one:\n%s", got, logs)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != -1 {
			t.Fatalf("%s = %d at a zero window, want no series at all - a message given up on its first delivery is never NAKed:\n%s", nakedCounter, got, body)
		}
		if got := counterValue(t, body, deliveredSeries("unplaceable_no_record")); got != 1 {
			t.Fatalf("%s = %d, want 1:\n%s", deliveredSeries("unplaceable_no_record"), got, body)
		}
		assertDeliveryException(t, run.exceptions, run.item, "no_holder")
	})

	// The regression lock for the clock this bound is measured on. The window is counted from
	// this message's first delivery, not from the time the stream stored it, so a message that
	// has been sitting in the stream for longer than the whole window still gets every deferral
	// the window buys. That message is not a corner case: it is a backlog replayed into a
	// listener whose caches are cold after a restart or during a rolling deploy, which is the
	// case this retry exists for, and a window measured from the store timestamp gives up on it
	// on its first delivery - with a give-up record, an exception and a counter, but without
	// ever having waited for the session it could not place.
	t.Run("a message older than the window is still deferred on its first delivery", func(t *testing.T) {
		run := startUnplaceableDeliveryRun(t, "ses_unplaceable_aged", oneDeferralWindow, oneDeferralWindow+time.Second)
		harness := run.harness

		waitFor(t, 15*time.Second, "the aged message's first delivery to be deferred", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) > 0
		})
		logs := harness.logs.String()
		deferrals := deliveryRecords(t, logs, "listener agent delivery deferred")
		if deferrals[0].Attempt != 1 {
			t.Fatalf("the deferral is attempt %d, want the first delivery - the message aged in the stream, it was not delivered and NAKed:\n%s", deferrals[0].Attempt, logs)
		}
		if deferrals[0].Reason != "no_session_record" {
			t.Fatalf("the deferral names reason %q, want no_session_record:\n%s", deferrals[0].Reason, logs)
		}
		if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 0 {
			t.Fatalf("%d give-up lines on the first delivery of a message older than the window, want none - the window is counted from the first delivery, not from the time the stream stored the message:\n%s", got, logs)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != 1 {
			t.Fatalf("%s = %d, want 1 - the aged message is NAKed back to the stream like any other:\n%s", nakedCounter, got, body)
		}
		if got := counterValue(t, body, deliveredSeries("unplaceable_no_record")); got != -1 {
			t.Fatalf("%s = %d, want no series at all - nothing has been given up yet:\n%s", deliveredSeries("unplaceable_no_record"), got, body)
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Fatalf("the aged message's first delivery published a delivery exception: %s", string(message.Data))
		}
	})
}

// The give-up counter separates its three causes, because an operator acts on them differently: a
// session with no record at all may be one whose registration never landed or one that ended, a
// record that names nothing to push to is a session that registered before its server bound its
// serve port, and a push that failed on the wire is a session whose record says where it is and
// whose process did not answer. None of them is a skip: before this change all of them were
// counted delivery_status="skipped" and logged "listener agent session not found anywhere", which
// is indistinguishable from the self-subscribed session that really did receive the message over
// its own subscription.
//
// That fourth outcome is asserted here too, against the same handler: a session with no port that
// is self-subscribed is delivered - skipped only in the push this listener did not have to make -
// and must never be counted unplaceable.
//
// Every cause runs at a real window over a real durable, because the separation is claimed of the
// whole path and not of its last attempt: at a zero window every delivery is given up on its first
// attempt whatever its reason, so a rig with one proves nothing about which causes are retried.
// Each cause is deferred first, and only the delivery after the window's one deferral is given up.
func TestGiveUpCounterSeparatesItsThreeCauses(t *testing.T) {
	const (
		noRecord       = "ses_cause_no_record"
		noPushTarget   = "ses_cause_no_push_target"
		pushFailed     = "ses_cause_push_failed"
		selfSubscribed = "ses_cause_self_subscribed"
	)
	// Only the push-failed session's record names a port, so every push this harness sees is that
	// session's, and it fails the way an unreachable peer or a restarting session process fails:
	// on the wire, with no HTTP status at all.
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.Path, pushFailed) {
			t.Errorf("only %s has a port to push to, got a push to %s", pushFailed, request.URL)
		}
		return nil, fmt.Errorf("dial tcp: connect: connection refused")
	}))
	// No interest is registered for any of these sessions, so each takes the direct path through
	// the resolved record rather than the interest path, which NAKs its failures itself. The
	// records go in before the handlers bind, so the first delivery of each reads the record this
	// test means it to read.
	if err := harness.sessions.Put(noPushTarget, session.SessionEntry{MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the portless session: %v", err)
	}
	if err := harness.sessions.Put(pushFailed, session.SessionEntry{Port: 1, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the unreachable session: %v", err)
	}
	if err := harness.sessions.Put(selfSubscribed, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}

	before := metricsBody(t, harness)
	if got := counterValue(t, before, deliveredSeries("skipped")); got != -1 {
		t.Fatalf("%s = %d before any delivery, want no series at all:\n%s", deliveredSeries("skipped"), got, before)
	}

	// The harness builds its handler at a zero window, so this test builds its own from the
	// harness's configuration. The four runs share the harness, and so the one metrics registry
	// and the one log an operator would read.
	cfg := harness.config
	cfg.unplaceableRetryWindow = oneDeferralWindow
	for _, target := range []string{noRecord, noPushTarget, pushFailed, selfSubscribed} {
		startUnplaceableDelivery(t, harness, cfg, target, 0)
	}

	// Each cause's own reason and counter label, named here rather than derived from one
	// another: the two are read by different readers - an operator reads the line, a dashboard
	// reads the label - and this test is what keeps them naming the same cause.
	want := map[string]struct{ reason, status string }{
		noRecord:     {reason: "no_session_record", status: "unplaceable_no_record"},
		noPushTarget: {reason: "no_push_target", status: "unplaceable_no_push_target"},
		pushFailed:   {reason: "push_failed", status: "unplaceable_push_failed"},
	}

	waitFor(t, 20*time.Second, "every unplaceable cause to be deferred", func() bool {
		return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) >= len(want)
	})
	deferrals := deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")
	if len(deferrals) != len(want) {
		t.Fatalf("%d deferral lines, want one per retried cause and none for the self-subscribed session:\n%s", len(deferrals), harness.logs.String())
	}
	deferred := map[string]bool{}
	for _, record := range deferrals {
		expected, wanted := want[record.SessionID]
		if !wanted {
			t.Fatalf("a deferral line names session %s, which is not one of the retried causes:\n%s", record.SessionID, harness.logs.String())
		}
		deferred[record.SessionID] = true
		if record.Reason != expected.reason {
			t.Fatalf("the deferral for %s names reason %q, want %q:\n%s", record.SessionID, record.Reason, expected.reason, harness.logs.String())
		}
		if record.Attempt != 1 {
			t.Fatalf("the deferral for %s is attempt %d, want the first delivery:\n%s", record.SessionID, record.Attempt, harness.logs.String())
		}
	}
	if len(deferred) != len(want) {
		t.Fatalf("the deferral lines name %v, want one for each of the three retried causes - a cause given up on its first delivery is a cause this listener never waited for:\n%s", deferred, harness.logs.String())
	}

	waitFor(t, 25*time.Second, "every deferred cause to be given up", func() bool {
		return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) >= len(want)
	})
	logs := harness.logs.String()
	giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
	if len(giveUps) != len(want) {
		t.Fatalf("%d give-up lines, want one per undeliverable message:\n%s", len(giveUps), logs)
	}
	named := map[string]bool{}
	for _, record := range giveUps {
		expected, wanted := want[record.SessionID]
		if !wanted {
			t.Fatalf("a give-up line names session %s, which this test gave up nothing for:\n%s", record.SessionID, logs)
		}
		named[record.SessionID] = true
		if record.Reason != expected.reason || record.DeliveryStatus != expected.status {
			t.Fatalf("the give-up line for %s names reason %q and delivery_status %q, want %q and %q - the status in the line must be the counter's own label:\n%s",
				record.SessionID, record.Reason, record.DeliveryStatus, expected.reason, expected.status, logs)
		}
	}
	if len(named) != len(want) {
		t.Fatalf("the give-up lines name %v, want one for each cause - one cause's line must not stand for another's:\n%s", named, logs)
	}

	final := metricsBody(t, harness)
	for _, expected := range want {
		if got := counterValue(t, final, deliveredSeries(expected.status)); got != 1 {
			t.Fatalf("%s = %d, want exactly 1 - each cause is counted once, under its own label:\n%s", deliveredSeries(expected.status), got, final)
		}
	}
	// The boundary: self-subscribed with no port is a delivered outcome - the session reads the
	// message off its own subscription - and only the push is skipped.
	if got := counterValue(t, final, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d for a self-subscribed session with no port, want 1 - its message arrives over its own subscription, and nothing else here is a skip:\n%s", deliveredSeries("skipped"), got, final)
	}
	if got := len(deliveryRecords(t, logs, "listener agent skipped push")); got != 1 {
		t.Fatalf("%d skipped-push lines, want exactly one, for the self-subscribed session:\n%s", got, logs)
	}
	if strings.Contains(logs, "listener agent session not found anywhere") {
		t.Fatalf("a delivery still logs the line that lost these messages, instead of deferring or giving up:\n%s", logs)
	}
}

// preloadDeferrals takes n slots of set for messages no test stream will ever number: sequences
// counted down from the top of the range, far above anything a stream these tests publish to
// assigns, so a release keyed on a real message's sequence can never give one of them back - only
// a release that is not keyed on the message can.
func preloadDeferrals(t *testing.T, set *unplaceableDeferrals, n int) {
	t.Helper()
	for i := range n {
		if !set.take(math.MaxUint64 - uint64(i)) {
			t.Fatalf("preloading slot %d of %d was refused: the set's cap is %d", i+1, n, unplaceableDeferralCap)
		}
	}
}

// The deferral cap is what keeps this listener's deferrals from taking the durable's whole pending
// budget: a deferred message is NAKed rather than ACKed, so it holds one of consumerMaxAckPending
// slots for its whole window, and enough of them at once would stall this machine's durable for
// every topic. At the cap, a message every other bound would have deferred is given up at once
// instead, with the ordinary give-up record - a message disposed of with a record an operator can
// act on is the cheaper loss. The negative control is the sibling subtest: one slot short of the
// cap, the same message on the same rig is deferred, so the give-up above is the cap's doing and
// not the rig's.
func TestUnplaceableDeferralsStopAtTheirCap(t *testing.T) {
	t.Run("a message that finds the cap reached is given up at once", func(t *testing.T) {
		harness := unplaceableHarness(t)
		cfg := harness.config
		cfg.unplaceableRetryWindow = oneDeferralWindow
		held := newUnplaceableDeferrals()
		preloadDeferrals(t, held, unplaceableDeferralCap)
		cfg.deferredUnplaceable = held
		run := startUnplaceableDelivery(t, harness, cfg, "ses_cap_reached", 0)

		waitFor(t, 15*time.Second, "the delivery that found the cap reached to be given up", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) > 0
		})
		logs := harness.logs.String()
		if got := len(deliveryRecords(t, logs, "listener agent delivery deferred")); got != 0 {
			t.Fatalf("%d deferral lines at the cap, want none - the cap is what the durable's pending budget rests on:\n%s", got, logs)
		}
		giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
		if len(giveUps) != 1 {
			t.Fatalf("%d give-up lines, want exactly one:\n%s", len(giveUps), logs)
		}
		if giveUps[0].Reason != "no_session_record" || giveUps[0].DeliveryStatus != "unplaceable_no_record" {
			t.Fatalf("the give-up at the cap names reason %q and delivery_status %q, want no_session_record and unplaceable_no_record - a message given up at the cap gets the ordinary record of its own cause:\n%s", giveUps[0].Reason, giveUps[0].DeliveryStatus, logs)
		}
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the give-up, want the %d it started with - a message that was not deferred takes no slot", got, unplaceableDeferralCap)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != -1 {
			t.Fatalf("%s = %d, want no series at all - a message given up on its first delivery is never NAKed:\n%s", nakedCounter, got, body)
		}
		assertDeliveryException(t, run.exceptions, run.item, "no_holder")
	})

	t.Run("the last slot under the cap is still deferred", func(t *testing.T) {
		harness := unplaceableHarness(t)
		cfg := harness.config
		cfg.unplaceableRetryWindow = oneDeferralWindow
		held := newUnplaceableDeferrals()
		preloadDeferrals(t, held, unplaceableDeferralCap-1)
		cfg.deferredUnplaceable = held
		startUnplaceableDelivery(t, harness, cfg, "ses_cap_last_slot", 0)

		waitFor(t, 15*time.Second, "the delivery in the last slot to be deferred", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) > 0
		})
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the deferral, want the cap of %d - the deferral takes the last slot", got, unplaceableDeferralCap)
		}
		logs := harness.logs.String()
		deferrals := deliveryRecords(t, logs, "listener agent delivery deferred")
		if deferrals[0].Attempt != 1 {
			t.Fatalf("the deferral is attempt %d, want the first delivery:\n%s", deferrals[0].Attempt, logs)
		}
		if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 0 {
			t.Fatalf("%d give-up lines one slot under the cap, want none - the message is held, not disposed of:\n%s", got, logs)
		}
	})
}

// twoDeferralWindow buys two deferrals, as oneDeferralWindow buys one: the window is spent in
// whole unplaceableRetryDelay steps, and the half-step keeps the arithmetic off the boundary.
const twoDeferralWindow = 2*unplaceableRetryDelay + unplaceableRetryDelay/2

// A redelivery gives its deferral slot back. The set (unplaceableDeferrals) holds the messages
// this listener is holding deferred right now, so a slot that is taken and never released is a
// slot gone for the life of the process: after unplaceableDeferralCap deferrals the listener
// would defer nothing ever again, give every unplaceable message up on its first delivery, and
// say nothing about why - the whole retry quietly stopping, which is the shape of failure this
// issue exists to end. Nothing else pins it: the cap's own test preloads the set and reads it
// back after one disposition, and every other unplaceable test runs with the set nearly empty,
// far from the cap, so deleting the release leaves them all green.
//
// The run starts one slot under the cap, so the message holds the last slot while it is deferred,
// and the set is read at both ends: at the cap after each deferral, and back at the slot below it
// once the message is given up. Without the release the message's slot outlives it - the set
// still holds it after the give-up - and a release that is not keyed on the message would show up
// here as a count below the cap while the message is still held.
func TestARedeliveryGivesItsDeferralSlotBack(t *testing.T) {
	harness := unplaceableHarness(t)
	cfg := harness.config
	cfg.unplaceableRetryWindow = twoDeferralWindow
	held := newUnplaceableDeferrals()
	preloadDeferrals(t, held, unplaceableDeferralCap-1)
	cfg.deferredUnplaceable = held
	startUnplaceableDelivery(t, harness, cfg, "ses_slot_released", 0)

	waitFor(t, 30*time.Second, "the redelivery of the deferred message to be deferred again", func() bool {
		return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) >= 2
	})
	logs := harness.logs.String()
	deferrals := deliveryRecords(t, logs, "listener agent delivery deferred")
	if len(deferrals) != 2 {
		t.Fatalf("%d deferral lines, want exactly the two the window buys:\n%s", len(deferrals), logs)
	}
	if deferrals[0].Attempt != 1 || deferrals[1].Attempt != 2 {
		t.Fatalf("the deferrals are attempts %d and %d, want 1 and 2 - the second is the redelivery the first asked for:\n%s",
			deferrals[0].Attempt, deferrals[1].Attempt, logs)
	}
	if got := held.size(); got != unplaceableDeferralCap {
		t.Fatalf("the set holds %d slots while one message is deferred, want the cap of %d - the message holds exactly one slot across its deferrals",
			got, unplaceableDeferralCap)
	}
	if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 0 {
		t.Fatalf("%d give-up lines while the window still has a deferral left, want none:\n%s", got, logs)
	}

	waitFor(t, 30*time.Second, "the message to be given up once its deferrals are spent", func() bool {
		return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) > 0
	})
	if got := held.size(); got != unplaceableDeferralCap-1 {
		t.Fatalf("the set holds %d slots after the give-up, want the %d the run started with - a message no longer held must leave no slot behind",
			got, unplaceableDeferralCap-1)
	}
}

// A redelivery this listener never deferred gives no slot back. Only a message this listener
// deferred holds a slot, so only that message's next delivery may return one: a redelivery that
// another branch's NAK produced - the interest path's ShouldNAK - or an expired AckWait is no
// sign a slot was ever taken for it. Those redeliveries come in step with deferrals, too: a local
// session whose server is down produces interest-path NAKs and direct-path deferrals at once. A
// release on every redelivery gives back slots it never took, until the budget reads empty while
// the listener is holding its whole cap, and the cap bounds nothing.
//
// The NAK here is the run's own, put in front of the listener's handler in place of those
// branches: it takes the message's first delivery and asks for it again at once, so the handler's
// first sight of the message is delivery 2, a redelivery it never deferred. The set starts full of
// other messages' slots. A release keyed on the message returns none of them, so the redelivery
// finds the cap reached and is given up; a release on any redelivery would hand one back and the
// handler would defer the message instead. The negative control is the sibling subtest: one slot
// under the cap, the same redelivery on the same rig is deferred at attempt 2, so the give-up
// above is the cap holding and not a rig whose redeliveries can never be deferred.
func TestARedeliveryThisListenerNeverDeferredGivesNoSlotBack(t *testing.T) {
	start := func(t *testing.T, target string, preload int) (listenerDeliveryHarness, *unplaceableDeferrals) {
		t.Helper()
		harness := unplaceableHarness(t)
		cfg := harness.config
		// Two deferrals, so attempt 2 is inside the window: the redelivery is deferred whenever
		// a slot is free, and only the cap can give it up.
		cfg.unplaceableRetryWindow = twoDeferralWindow
		held := newUnplaceableDeferrals()
		preloadDeferrals(t, held, preload)
		cfg.deferredUnplaceable = held
		handler := jetStreamDeliveryHandler(cfg)
		var firstTaken atomic.Bool
		startDeliveryThrough(t, harness, func(msg *natsgo.Msg) {
			if firstTaken.CompareAndSwap(false, true) {
				if err := msg.Nak(); err != nil {
					t.Errorf("NAK the first delivery of %s: %v", target, err)
				}
				return
			}
			handler(msg)
		}, target, 0)
		return harness, held
	}

	t.Run("at the cap, the redelivery is given up and every slot stays held", func(t *testing.T) {
		harness, held := start(t, "ses_untaken_release_at_cap", unplaceableDeferralCap)

		waitFor(t, 15*time.Second, "the redelivery that found the cap reached to be given up", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")) > 0
		})
		logs := harness.logs.String()
		if got := len(deliveryRecords(t, logs, "listener agent delivery deferred")); got != 0 {
			t.Fatalf("%d deferral lines with the set full, want none - a redelivery this listener never deferred freed a slot it never took:\n%s", got, logs)
		}
		giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
		if len(giveUps) != 1 || giveUps[0].Reason != "no_session_record" {
			t.Fatalf("want exactly one give-up line naming no_session_record, got %d:\n%s", len(giveUps), logs)
		}
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the redelivery, want the %d it started with - the redelivery gave back a slot it never took", got, unplaceableDeferralCap)
		}
	})

	t.Run("one slot under the cap, the same redelivery is deferred", func(t *testing.T) {
		harness, held := start(t, "ses_untaken_release_last_slot", unplaceableDeferralCap-1)

		waitFor(t, 15*time.Second, "the redelivery to be deferred in the last slot", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) > 0
		})
		logs := harness.logs.String()
		deferrals := deliveryRecords(t, logs, "listener agent delivery deferred")
		if deferrals[0].Attempt != 2 {
			t.Fatalf("the deferral is attempt %d, want 2 - the handler's first sight of the message is the redelivery the run's own NAK asked for:\n%s", deferrals[0].Attempt, logs)
		}
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the deferral, want the cap of %d - the redelivery takes the last slot", got, unplaceableDeferralCap)
		}
	})
}

// A window longer than the durable's MaxDeliver can pay for still ends in a give-up record.
// ENVOY_UNPLACEABLE_RETRY_WINDOW has no upper bound (envDuration, main.go), and at ten minutes the
// window buys 120 deferrals against a consumer that stops redelivering after consumerMaxDeliver
// (20), silently: nothing in this repository consumes JetStream's max-deliveries advisory. The
// count the handler holds the window to is therefore capped at unplaceableMaxDeliveries, and the
// delivery that reaches it is given up - with its record, its counter and its no_holder exception -
// while the stream would still deliver it. Without that cap the message is deferred on every one of
// its twenty deliveries and then simply stops arriving, with no line, no counter and no exception:
// the loss this change exists to end, reached by an operator setting rather than a code change.
// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn checks only the default window, so nothing
// else catches the cap going.
//
// The run's own NAKs, in front of the listener's handler, take every earlier delivery and ask for
// it again at once, so the handler's first sight of the message is the delivery under test, with
// no 5 s deferral delay to wait out per attempt. The negative control is the sibling subtest: the
// same window on the same rig, one delivery earlier, is deferred, so the give-up is the cap and not
// a rig whose late deliveries can never be deferred.
func TestAWindowPastTheDurablesMaxDeliverStillGivesUpBeforeIt(t *testing.T) {
	const window = 10 * time.Minute
	if deferrals := int(window / unplaceableRetryDelay); deferrals < consumerMaxDeliver {
		t.Fatalf("a %s window buys %d deferrals, fewer than the durable's MaxDeliver of %d: it no longer reaches the cap this test exists for", window, deferrals, consumerMaxDeliver)
	}
	start := func(t *testing.T, target string, handlerFirstSees uint64) unplaceableDeliveryRun {
		t.Helper()
		harness := unplaceableHarness(t)
		cfg := harness.config
		cfg.unplaceableRetryWindow = window
		handler := jetStreamDeliveryHandler(cfg)
		var taken atomic.Uint64
		return startDeliveryThrough(t, harness, func(msg *natsgo.Msg) {
			if taken.Add(1) < handlerFirstSees {
				if err := msg.Nak(); err != nil {
					t.Errorf("NAK an early delivery of %s: %v", target, err)
				}
				return
			}
			handler(msg)
		}, target, 0)
	}

	t.Run("the delivery that reaches the cap is given up", func(t *testing.T) {
		run := start(t, "ses_window_past_max_deliver", unplaceableMaxDeliveries)
		harness := run.harness

		waitFor(t, 15*time.Second, "the delivery at the cap to be disposed of", func() bool {
			logs := harness.logs.String()
			return len(deliveryRecords(t, logs, "listener agent delivery given up"))+
				len(deliveryRecords(t, logs, "listener agent delivery deferred")) > 0
		})
		logs := harness.logs.String()
		if deferrals := deliveryRecords(t, logs, "listener agent delivery deferred"); len(deferrals) != 0 {
			t.Fatalf("delivery %d of a %s window was deferred (attempt %d of %d window deferrals): the stream stops redelivering at %d, so this message would be lost with no give-up record, no counter and no exception:\n%s",
				unplaceableMaxDeliveries, window, deferrals[0].Attempt, deferrals[0].WindowDeferrals, consumerMaxDeliver, logs)
		}
		giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
		if len(giveUps) != 1 || giveUps[0].Reason != "no_session_record" || giveUps[0].DeliveryStatus != "unplaceable_no_record" {
			t.Fatalf("want exactly one give-up line naming no_session_record and unplaceable_no_record, got %d:\n%s", len(giveUps), logs)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, deliveredSeries("unplaceable_no_record")); got != 1 {
			t.Fatalf("%s = %d, want 1:\n%s", deliveredSeries("unplaceable_no_record"), got, body)
		}
		assertDeliveryException(t, run.exceptions, run.item, "no_holder")
	})

	t.Run("one delivery earlier, the same window defers", func(t *testing.T) {
		run := start(t, "ses_window_under_max_deliver", unplaceableMaxDeliveries-1)
		harness := run.harness

		waitFor(t, 15*time.Second, "the delivery under the cap to be deferred", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred")) > 0
		})
		logs := harness.logs.String()
		deferrals := deliveryRecords(t, logs, "listener agent delivery deferred")
		if deferrals[0].Attempt != unplaceableMaxDeliveries-1 {
			t.Fatalf("the deferral is attempt %d, want %d - the handler's first sight of the message is the delivery after the run's own NAKs:\n%s", deferrals[0].Attempt, unplaceableMaxDeliveries-1, logs)
		}
		if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 0 {
			t.Fatalf("%d give-up lines one delivery under the cap, want none:\n%s", got, logs)
		}
	})
}

// recordsFor keeps the records that name sessionID, for runs that share one harness's log.
func recordsFor(records []deliveryRecord, sessionID string) []deliveryRecord {
	var kept []deliveryRecord
	for _, record := range records {
		if record.SessionID == sessionID {
			kept = append(kept, record)
		}
	}
	return kept
}

// A push that failed is retried once, however long the window, and the causes that push nothing
// keep the window's whole allowance. A failed push may have landed: session.Deliverer's prompt
// returns the same error for a session server that refused the prompt, a connection that never
// reached it, and an answer lost after the server took the prompt, and every deferral clears the
// attempt-cache entry that would otherwise stop a redelivery pushing again - so each retry of such
// a push can hand the session an instruction it already has. At the default window's dozen
// deferrals that is the same instruction up to thirteen times; at pushFailedDeferrals it is twice
// at most.
//
// Both messages run at the default window on one harness. The push-failed one is deferred on its
// first delivery and given up on its second, having been pushed exactly twice; the no-record one,
// in the same window, is deferred again on its second delivery - which is the negative control:
// the give-up at attempt 2 is the push-failure bound and not a window that ran out.
func TestAPushFailureIsDeferredOnceWhereOtherCausesAreDeferredFurther(t *testing.T) {
	const (
		pushFailed = "ses_bound_push_failed"
		noRecord   = "ses_bound_no_record"
	)
	windowDeferrals := uint64(defaultUnplaceableRetryWindow / unplaceableRetryDelay)
	if windowDeferrals < pushFailedDeferrals+2 {
		t.Fatalf("the default window buys %d deferrals, which cannot show a push failure stopping at %d while another cause goes on", windowDeferrals, pushFailedDeferrals)
	}
	var pushes atomic.Int32
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.Path, pushFailed) {
			t.Errorf("only %s has a port to push to, got a push to %s", pushFailed, request.URL)
		}
		pushes.Add(1)
		return nil, fmt.Errorf("dial tcp: connect: connection refused")
	}))
	if err := harness.sessions.Put(pushFailed, session.SessionEntry{Port: 1, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the unreachable session: %v", err)
	}
	cfg := harness.config
	cfg.unplaceableRetryWindow = defaultUnplaceableRetryWindow
	startUnplaceableDelivery(t, harness, cfg, pushFailed, 0)
	startUnplaceableDelivery(t, harness, cfg, noRecord, 0)

	waitFor(t, 20*time.Second, "the push-failed message to be given up", func() bool {
		return len(recordsFor(deliveryRecords(t, harness.logs.String(), "listener agent delivery given up"), pushFailed)) > 0
	})
	waitFor(t, 20*time.Second, "the no-record message to be deferred on its second delivery", func() bool {
		return len(recordsFor(deliveryRecords(t, harness.logs.String(), "listener agent delivery deferred"), noRecord)) >= 2
	})
	logs := harness.logs.String()

	pushDeferrals := recordsFor(deliveryRecords(t, logs, "listener agent delivery deferred"), pushFailed)
	if len(pushDeferrals) != pushFailedDeferrals {
		t.Fatalf("%d deferral lines for the failed push, want %d - a push that may have landed is retried once:\n%s", len(pushDeferrals), pushFailedDeferrals, logs)
	}
	if pushDeferrals[0].Attempt != 1 || pushDeferrals[0].Reason != "push_failed" {
		t.Fatalf("the failed push's deferral is attempt %d with reason %q, want attempt 1 and push_failed:\n%s", pushDeferrals[0].Attempt, pushDeferrals[0].Reason, logs)
	}
	if pushDeferrals[0].WindowDeferrals != pushFailedDeferrals {
		t.Fatalf("the failed push's deferral names window_deferrals %d, want %d - the line names the allowance the decision was made on, not the window's:\n%s", pushDeferrals[0].WindowDeferrals, pushFailedDeferrals, logs)
	}
	pushGiveUps := recordsFor(deliveryRecords(t, logs, "listener agent delivery given up"), pushFailed)
	if len(pushGiveUps) != 1 || pushGiveUps[0].Reason != "push_failed" || pushGiveUps[0].DeliveryStatus != "unplaceable_push_failed" {
		t.Fatalf("want exactly one give-up line for the failed push, naming push_failed and unplaceable_push_failed:\n%s", logs)
	}
	if got := pushes.Load(); got != pushFailedDeferrals+1 {
		t.Fatalf("the session was pushed %d times, want %d - each retry of a push that may have landed repeats it", got, pushFailedDeferrals+1)
	}

	recordDeferrals := recordsFor(deliveryRecords(t, logs, "listener agent delivery deferred"), noRecord)
	if recordDeferrals[1].Attempt != 2 || recordDeferrals[1].Reason != "no_session_record" {
		t.Fatalf("the no-record message's second deferral is attempt %d with reason %q, want attempt 2 and no_session_record:\n%s", recordDeferrals[1].Attempt, recordDeferrals[1].Reason, logs)
	}
	if recordDeferrals[1].WindowDeferrals != windowDeferrals {
		t.Fatalf("the no-record message's deferral names window_deferrals %d, want the window's %d:\n%s", recordDeferrals[1].WindowDeferrals, windowDeferrals, logs)
	}
	if got := len(recordsFor(deliveryRecords(t, logs, "listener agent delivery given up"), noRecord)); got != 0 {
		t.Fatalf("%d give-up lines for the no-record message at attempt 2, want none - the window still has deferrals for it:\n%s", got, logs)
	}
}

// A give-up whose no_holder notice can never be published is still the message's end: the counter
// and the give-up line are written, the failed notice is logged exceptionPublishFailedLine, the
// message is acknowledged, and the session is pushed no more than its allowance however many
// deliveries of it follow. Before this the give-up published the notice first and, when that
// failed, NAKed the message and cleared its attempt entry before writing anything: no counter, no
// give-up line, and a redelivery with neither a dedupe nor an attempt entry, which ran the whole
// handler again and pushed again, every 30 s, to the durable's MaxDeliver.
//
// The notice fails here for good, not for one attempt, by the reviewer's own reachable case: the
// notice embeds the original payload, so an envelope just under the server's max payload yields a
// notice past it, which the bus refuses bus.ErrTooLarge every time it is published. The session
// has a port and refuses every push, so the cause is push_failed, whose allowance is
// pushFailedDeferrals deferrals and so pushFailedDeferrals+1 pushes.
//
// Two reads prove nothing more follows. The durable is read back: the message must be
// acknowledged, nothing pending, where the NAK this replaces leaves it pending for a redelivery
// 30 s on. And the envelope is delivered once more, as the redelivery an ACK lost on the wire
// would bring: the give-up keeps the attempt entry, so that delivery is an attempt-dedupe skip and
// pushes nothing, where an entry cleared at the give-up would push a third time.
func TestAGiveUpWhoseNoticeCanNeverBePublishedIsStillTerminal(t *testing.T) {
	const target = "ses_notice_too_large"
	var pushes atomic.Int32
	harness := newListenerDeliveryHarness(t, listenerTransport(func(*http.Request) (*http.Response, error) {
		pushes.Add(1)
		return nil, fmt.Errorf("dial tcp: connect: connection refused")
	}))
	if err := harness.sessions.Put(target, session.SessionEntry{Port: 1, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the unreachable session: %v", err)
	}
	item := listenerTestEnvelope(contracts.AgentSubject(target), "unplaceable-"+target)
	// The payload fills the envelope to 256 bytes under the server's max payload, so the envelope
	// itself is published and delivered, and the notice - the same payload plus the notice's own
	// fields and a second level of JSON quoting - is past it.
	bare := marshalListenerEnvelope(t, item)
	item.Payload = strings.Repeat("p", int(harness.client.Conn.MaxPayload())-len(bare)-256)
	cfg := harness.config
	cfg.unplaceableRetryWindow = defaultUnplaceableRetryWindow
	run := startEnvelopeDeliveryThrough(t, harness, jetStreamDeliveryHandler(cfg), target, item, 0)

	waitFor(t, 20*time.Second, "the message to be given up and its notice to fail", func() bool {
		logs := harness.logs.String()
		return len(deliveryRecords(t, logs, "listener agent delivery given up")) > 0 &&
			len(deliveryRecords(t, logs, exceptionPublishFailedLine)) > 0
	})
	waitFor(t, 10*time.Second, "the durable to hold nothing pending for the message", func() bool {
		info, err := harness.client.JS().ConsumerInfo(bus.Stream, run.consumer)
		return err == nil && info.NumAckPending == 0
	})
	info, err := harness.client.JS().ConsumerInfo(bus.Stream, run.consumer)
	if err != nil {
		t.Fatalf("read the durable %s back: %v", run.consumer, err)
	}
	if want := uint64(pushFailedDeferrals + 1); info.Delivered.Consumer != want || info.AckFloor.Consumer != want {
		t.Fatalf("the durable delivered %d and acknowledged through %d, want both %d - the delivery that gave the message up acknowledges it, notice or none", info.Delivered.Consumer, info.AckFloor.Consumer, want)
	}

	// The further delivery: the same envelope through the same handler state, as a redelivery.
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	logs := harness.logs.String()
	giveUps := deliveryRecords(t, logs, "listener agent delivery given up")
	if len(giveUps) != 1 || giveUps[0].Reason != "push_failed" || giveUps[0].DeliveryStatus != "unplaceable_push_failed" {
		t.Fatalf("want exactly one give-up line, naming push_failed and unplaceable_push_failed:\n%s", logs)
	}
	failures := deliveryRecords(t, logs, exceptionPublishFailedLine)
	if len(failures) != 1 || failures[0].Topic != item.Topic || !strings.Contains(failures[0].Error, bus.ErrTooLarge.Error()) {
		t.Fatalf("want exactly one %q line for %s naming %q - the notice was refused, once, and said so:\n%s", exceptionPublishFailedLine, item.Topic, bus.ErrTooLarge, logs)
	}
	if skips := deliveryRecords(t, logs, "listener attempt-dedupe skip"); len(skips) != 1 {
		t.Fatalf("%d attempt-dedupe skips, want the further delivery's one - the given-up message's attempt entry is what keeps it from pushing again:\n%s", len(skips), logs)
	}
	if got := pushes.Load(); got != pushFailedDeferrals+1 {
		t.Fatalf("the session was pushed %d times, want %d - the allowance for a push that may have landed, however many deliveries follow:\n%s", got, pushFailedDeferrals+1, logs)
	}
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("unplaceable_push_failed")); got != 1 {
		t.Fatalf("%s = %d, want 1 - the give-up is counted whether or not its notice went out:\n%s", deliveredSeries("unplaceable_push_failed"), got, body)
	}
	if got := counterValue(t, body, nakedCounter); got != pushFailedDeferrals {
		t.Fatalf("%s = %d, want %d - the deferral's NAK and nothing for the notice that failed:\n%s", nakedCounter, got, pushFailedDeferrals, body)
	}
	if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("a notice past the server's max payload was published after all (%d bytes): the run proves nothing about a notice that cannot go out", len(message.Data))
	}
}
