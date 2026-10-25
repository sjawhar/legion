package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	AgeMS          int64  `json:"age_ms"`
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

// unplaceableDeliveryRun is one envelope for a session with no record at all, driven through the
// listener's own jetStreamDeliveryHandler over a real durable consumer. The package's other
// delivery tests hand the handler a synthetic &natsgo.Msg{Data: data}, which carries no JetStream
// metadata: the handler can bound neither its age nor its delivery count against that, so it gives
// up at once, and nothing about a retry can be learned from it. Here the server holds the message
// and performs the redelivery the handler's NAK asks for, as it does in production.
type unplaceableDeliveryRun struct {
	harness listenerDeliveryHarness
	item    contracts.Envelope
	// exceptions reads the delivery exceptions published for the envelope's own topic.
	exceptions *natsgo.Subscription
}

// startUnplaceableDeliveryRun creates the durable, runs the delivery handler over it with window as
// the handler's unplaceable retry window, and publishes one envelope for target - a session the
// bucket has no record of and no interest names. It returns once the publish is acknowledged; the
// deliveries happen on the subscription's goroutine.
func startUnplaceableDeliveryRun(t *testing.T, target string, window time.Duration) unplaceableDeliveryRun {
	t.Helper()
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("a session with no record must never be pushed to, got %s", request.URL)
	}))
	// The harness builds its handler before a test can set the window, so this run builds its own
	// from the harness's configuration - the one seam the harness exposes for exactly this.
	cfg := harness.config
	cfg.unplaceableRetryWindow = window

	subject := contracts.AgentSubject(target)
	consumer := "listener-" + strings.ReplaceAll(target, "_", "-")
	// The listener's own consumer policy - explicit ack, consumerMaxDeliver - over this one
	// subject, so the run bounds nothing the production consumer does not. AckWait is the one
	// departure: the redeliveries here are the ones the handler asks for by NAK, and a 60 s
	// consumerAckWait would hold a handler that asked for neither an ack nor a retry past any
	// deadline this test could wait out.
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: natsgo.NewInbox()}
	applyListenerConsumerPolicy(&config, []string{subject})
	config.AckWait = 10 * time.Second
	if _, err := harness.client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the durable %s over %s: %v", consumer, subject, err)
	}
	t.Cleanup(func() { _ = harness.client.JS().DeleteConsumer(bus.Stream, consumer) })

	item := listenerTestEnvelope(subject, "unplaceable-"+target)
	exceptions, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + subject)
	if err != nil {
		t.Fatalf("subscribe the exception probe for %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}

	subscription, err := harness.client.Subscribe("", jetStreamDeliveryHandler(cfg),
		natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the delivery handler to %s: %v", consumer, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })

	if err := harness.client.Publish(item); err != nil {
		t.Fatalf("publish %s to %s: %v", item.EventID, subject, err)
	}
	return unplaceableDeliveryRun{harness: harness, item: item, exceptions: exceptions}
}

// A message for a session this listener cannot place is kept in the stream and redelivered for the
// length of the retry window, and only then given up - once, with a record, a counter and a
// no_holder exception of its own. Before this the same message was ACKed away on its first delivery
// as a skip, which is a message lost with no trace an operator could act on.
//
// The window is 2 s against the fixed 5 s unplaceableRetryDelay, so the arithmetic is: the first
// delivery, some milliseconds after the publish, is inside the window and deferred; the one
// redelivery it asks for arrives 5 s later, past the window, and is given up. Both bounds have
// seconds of slack, so a slow machine moves neither.
//
// The redelivery must not be swallowed as an attempt-dedupe skip: the deferral clears the attempt
// entry the delivery recorded, and without that clearing the retry this test proves would be
// disposed of before it reached the session at all. The negative control is the sibling subtest: at
// a zero window the same message is given up on its first delivery, with no NAK and no deferral, so
// a pass here cannot come from a rig that defers nothing.
func TestUnplaceableAgentDeliveryRetriesThenGivesUp(t *testing.T) {
	t.Run("a window retries, then gives up once", func(t *testing.T) {
		run := startUnplaceableDeliveryRun(t, "ses_unplaceable_retried", 2*time.Second)
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
		if deferrals[0].SessionID != strings.TrimPrefix(run.item.Topic, contracts.AgentTopicPrefix) {
			t.Fatalf("the deferral names session %q, want the envelope's target %q", deferrals[0].SessionID, run.item.Topic)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != 1 {
			t.Fatalf("%s = %d, want 1 - a deferred delivery is NAKed back to the stream, not acknowledged:\n%s", nakedCounter, got, body)
		}
		for _, status := range []string{"delivered", "skipped", "unplaceable_no_record", "unplaceable_no_push_target"} {
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
			t.Fatalf("%d deferral lines, want exactly one - a 2s window against a 5s retry delay defers the first delivery only:\n%s", got, logs)
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
		run := startUnplaceableDeliveryRun(t, "ses_unplaceable_at_once", 0)
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
}

// The give-up counter separates its two causes, because an operator acts on them differently: a
// session with no record at all may be one whose registration never landed or one that ended, while
// a record that names nothing to push to is a session that registered and left no way to be
// reached. Neither is a skip: before this change both were counted delivery_status="skipped" and
// logged "listener agent session not found anywhere", which is indistinguishable from the
// self-subscribed session that really did receive the message over its own subscription.
//
// That third outcome is asserted here too, against the same handler: a session with no port that is
// self-subscribed is delivered - skipped only in the push this listener did not have to make - and
// must never be counted unplaceable. A zero retry window keeps the test fast: each delivery is
// given up on its first attempt, so no real consumer and no redelivery are needed.
func TestGiveUpCounterSeparatesItsTwoCauses(t *testing.T) {
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("neither give-up cause has anything to push to, got %s", request.URL)
	}))
	const (
		noRecord       = "ses_cause_no_record"
		noPushTarget   = "ses_cause_no_push_target"
		selfSubscribed = "ses_cause_self_subscribed"
	)

	before := metricsBody(t, harness)
	if got := counterValue(t, before, deliveredSeries("skipped")); got != -1 {
		t.Fatalf("%s = %d before any delivery, want no series at all:\n%s", deliveredSeries("skipped"), got, before)
	}

	// No record anywhere: nothing registered this session, so the bucket read answers
	// nats.ErrKeyNotFound.
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t,
		listenerTestEnvelope(contracts.AgentSubject(noRecord), "give-up-cause-no-record"))})

	// A record that names nothing to push to: no serve port, not self-subscribed. No interest is
	// registered for either session, so both take the direct path through the resolved record
	// rather than the interest path, which NAKs its failures itself.
	if err := harness.sessions.Put(noPushTarget, session.SessionEntry{MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the portless session: %v", err)
	}
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t,
		listenerTestEnvelope(contracts.AgentSubject(noPushTarget), "give-up-cause-no-push-target"))})

	after := metricsBody(t, harness)
	if got := counterValue(t, after, deliveredSeries("unplaceable_no_record")); got != 1 {
		t.Fatalf("%s = %d, want 1 for the session the bucket has no record of:\n%s", deliveredSeries("unplaceable_no_record"), got, after)
	}
	if got := counterValue(t, after, deliveredSeries("unplaceable_no_push_target")); got != 1 {
		t.Fatalf("%s = %d, want 1 for the record that names nothing to push to:\n%s", deliveredSeries("unplaceable_no_push_target"), got, after)
	}
	if got := counterValue(t, after, deliveredSeries("skipped")); got != -1 {
		t.Fatalf("%s = %d after two give-ups, want no series at all - a message nothing could be given is not a skip:\n%s", deliveredSeries("skipped"), got, after)
	}
	giveUps := deliveryRecords(t, harness.logs.String(), "listener agent delivery given up")
	if len(giveUps) != 2 {
		t.Fatalf("%d give-up lines, want one per undeliverable message:\n%s", len(giveUps), harness.logs.String())
	}
	// Each cause's own reason and counter label, named here rather than derived from one
	// another: the two are read by different readers - an operator reads the line, a dashboard
	// reads the label - and this test is what keeps them naming the same cause.
	want := map[string]struct{ reason, status string }{
		noRecord:     {reason: "no_session_record", status: "unplaceable_no_record"},
		noPushTarget: {reason: "no_push_target", status: "unplaceable_no_push_target"},
	}
	named := map[string]bool{}
	for _, record := range giveUps {
		expected, wanted := want[record.SessionID]
		if !wanted {
			t.Fatalf("a give-up line names session %s, which this test gave up nothing for:\n%s", record.SessionID, harness.logs.String())
		}
		named[record.SessionID] = true
		if record.Reason != expected.reason || record.DeliveryStatus != expected.status {
			t.Fatalf("the give-up line for %s names reason %q and delivery_status %q, want %q and %q - the status in the line must be the counter's own label:\n%s",
				record.SessionID, record.Reason, record.DeliveryStatus, expected.reason, expected.status, harness.logs.String())
		}
	}
	if len(named) != len(want) {
		t.Fatalf("the give-up lines name %v, want one for each of %s and %s - one cause's line must not stand for the other's:\n%s", named, noRecord, noPushTarget, harness.logs.String())
	}

	// The boundary: self-subscribed with no port is a delivered outcome - the session reads the
	// message off its own subscription - and only the push is skipped.
	if err := harness.sessions.Put(selfSubscribed, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t,
		listenerTestEnvelope(contracts.AgentSubject(selfSubscribed), "give-up-cause-self-subscribed"))})

	logs := harness.logs.String()
	final := metricsBody(t, harness)
	if got := counterValue(t, final, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d for a self-subscribed session with no port, want 1 - its message arrives over its own subscription:\n%s", deliveredSeries("skipped"), got, final)
	}
	for _, status := range []string{"unplaceable_no_record", "unplaceable_no_push_target"} {
		if got := counterValue(t, final, deliveredSeries(status)); got != 1 {
			t.Fatalf("%s = %d after the self-subscribed delivery, want it unchanged at 1 - a self-subscribed session is placeable:\n%s", deliveredSeries(status), got, final)
		}
	}
	if got := len(deliveryRecords(t, logs, "listener agent delivery given up")); got != 2 {
		t.Fatalf("%d give-up lines after the self-subscribed delivery, want the same 2:\n%s", got, logs)
	}
	if got := len(deliveryRecords(t, logs, "listener agent skipped push")); got != 1 {
		t.Fatalf("%d skipped-push lines, want exactly one, for the self-subscribed session:\n%s", got, logs)
	}
	if strings.Contains(logs, "listener agent session not found anywhere") {
		t.Fatalf("a delivery still logs the line that lost these messages, instead of deferring or giving up:\n%s", logs)
	}
}
