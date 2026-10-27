package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/session"
)

// settledMessage is a durable message whose ack or NAK the test reads back: its reply subject is
// an inbox the test subscribes, as a JetStream message's is the consumer's ack subject, and
// nats.go settles a message through its reply subject and its subscription's connection alone.
func settledMessage(t *testing.T, conn *natsgo.Conn, data []byte) (*natsgo.Msg, func(t *testing.T) string) {
	t.Helper()
	inbox := natsgo.NewInbox()
	sub, err := conn.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("subscribe the settlement inbox: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &natsgo.Msg{Data: data, Reply: inbox, Sub: sub}, func(t *testing.T) string {
		t.Helper()
		settlement, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("read the message's settlement: %v", err)
		}
		return string(settlement.Data)
	}
}

// nakDelay reads the delay of a NAK nats.go sent as `-NAK {"delay": <ns>}`.
func nakDelay(t *testing.T, settlement string) time.Duration {
	t.Helper()
	body, ok := strings.CutPrefix(settlement, "-NAK ")
	if !ok {
		t.Fatalf("settlement = %q, want a NAK with a delay", settlement)
	}
	var delay struct {
		Delay int64 `json:"delay"`
	}
	if err := json.Unmarshal([]byte(body), &delay); err != nil {
		t.Fatalf("decode the NAK %q: %v", settlement, err)
	}
	return time.Duration(delay.Delay)
}

// countingPrompts is a session transport that answers every prompt_async 204 and counts them.
func countingPrompts(prompts *atomic.Int32) listenerTransport {
	return listenerTransport(func(*http.Request) (*http.Response, error) {
		prompts.Add(1)
		return listenerDeliveryResponse(http.StatusNoContent), nil
	})
}

// Two pods of one machine share the delivery ledger: pod B, taking the lease over, receives the
// redelivery of a message pod A prompted and had not acked (killed before its ack). B finds A's
// delivered record, skips the session and acks the message, so the session is prompted once.
func TestAPodTakingOverDoesNotPromptASessionTheOldPodDelivered(t *testing.T) {
	var prompts atomic.Int32
	harness := newListenerDeliveryHarness(t, countingPrompts(&prompts))
	item := listenerTestEnvelope(contracts.AgentSubject("ses_takeover"), "agent.ses_takeover.0123456789abcdef0123456789abcdef")
	harness.registerTarget(t, "ses_takeover", item.Topic)

	// Pod A (the harness, boot-test) prompts the session and records the delivery.
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	if got := prompts.Load(); got != 1 {
		t.Fatalf("pod A prompted %d times, want 1", got)
	}

	// Pod B, another boot over the same bucket, receives the message from the durable.
	podB := harness.config
	podB.ledger = testDeliveryLedger(t, harness.client.Conn, "test-machine", "boot-pod-b")
	durable := jetStreamDeliveryHandler(podB)
	consumer := bus.ListenerDurable("test-machine")
	sub, err := subscribeListenerDurable(harness.client, consumer, func(msg *natsgo.Msg) { durable(context.Background(), msg) })
	if err != nil {
		t.Fatalf("subscribe pod B to the durable: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := harness.client.Publish(item); err != nil {
		t.Fatalf("publish the redelivered message: %v", err)
	}
	waitFor(t, 10*time.Second, "pod B to ack the message", func() bool {
		info, err := harness.client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.AckFloor.Consumer == 1 && info.NumAckPending == 0
	})
	if got := prompts.Load(); got != 1 {
		t.Fatalf("the session was prompted %d times across the two pods, want 1", got)
	}
	if info, err := harness.client.JS().ConsumerInfo(bus.Stream, consumer); err != nil || info.NumRedelivered != 0 {
		t.Fatalf("consumer after pod B's skip: %+v, %v; want the message acked, not NAKed", info, err)
	}
	if logs := harness.logs.String(); !strings.Contains(logs, `"msg":"listener dedupe skip"`) {
		t.Fatalf("pod B logged no dedupe skip:\n%s", logs)
	}
}

// A pod that finds another pod's attempt younger than the request limit and its margin cannot tell
// whether that pod is still prompting, so the durable lane NAKs the message until the attempt is
// old enough to take over, and the session is not prompted meanwhile.
func TestADurableMessageAnotherPodIsAttemptingIsRetriedOnceItCanBeTakenOver(t *testing.T) {
	var prompts atomic.Int32
	harness := newListenerDeliveryHarness(t, countingPrompts(&prompts))
	item := listenerTestEnvelope(contracts.AgentSubject("ses_deferred"), "agent.ses_deferred.0123456789abcdef0123456789abcdef")
	harness.registerTarget(t, "ses_deferred", item.Topic)

	podA := testDeliveryLedger(t, harness.client.Conn, "test-machine", "boot-pod-a")
	if attempt, err := podA.Attempt(item.DedupeKey, "ses_deferred"); err != nil || attempt.Verdict != dedupe.Proceed {
		t.Fatalf("pod A's attempt = %v, %v; want it to proceed", attempt.Verdict, err)
	}

	msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
	harness.handler(msg)
	takeover := 5*time.Second + ledgerTakeoverMargin
	if delay := nakDelay(t, settlement(t)); delay > takeover || delay < takeover-5*time.Second {
		t.Fatalf("NAK delay = %s, want about %s, until pod A's attempt is %s old", delay, takeover, takeover)
	}
	if got := prompts.Load(); got != 0 {
		t.Fatalf("the session was prompted %d times while pod A's attempt was young, want 0", got)
	}
}

// unavailableLedger is a delivery ledger whose bucket does not answer.
type unavailableLedger struct{}

var errLedgerDown = errors.New("nats: timeout")

func (unavailableLedger) Attempt(string, string) (dedupe.Attempt, error) {
	return dedupe.Attempt{}, errLedgerDown
}

// Record and Clear of an attempt that holds nothing touch no bucket, as dedupe.KV's do.
func (unavailableLedger) Record(attempt dedupe.Attempt) error { return heldOnly(attempt) }
func (unavailableLedger) Clear(attempt dedupe.Attempt) error  { return heldOnly(attempt) }

func heldOnly(attempt dedupe.Attempt) error {
	if attempt.Verdict != dedupe.Proceed {
		return nil
	}
	return errLedgerDown
}

// A ledger that cannot be read leaves the durable lane unable to tell a repeat from a first
// delivery, so it retries the message after the usual 30 s rather than risk prompting twice; the
// role and live lanes, which retry nothing, deliver without it. Each says so at WARN.
func TestAnUnavailableLedgerRetriesTheDurableLaneAndLetsACoreLaneDeliver(t *testing.T) {
	var prompts atomic.Int32
	harness := newListenerDeliveryHarness(t, countingPrompts(&prompts))
	const topic = "notifications.github.owner.repo.issue.9"
	harness.registerFanoutTarget(t, "ses_unavailable", topic, nil)
	cfg := harness.config
	cfg.ledger = unavailableLedger{}
	item := listenerTestEnvelope(topic, "ledger-unavailable")
	data := marshalListenerEnvelope(t, item)

	msg, settlement := settledMessage(t, harness.client.Conn, data)
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)
	if delay := nakDelay(t, settlement(t)); delay != durableRetryDelay {
		t.Fatalf("durable NAK delay = %s, want %s", delay, durableRetryDelay)
	}
	if got := prompts.Load(); got != 0 {
		t.Fatalf("the durable lane prompted %d times without the ledger, want 0", got)
	}
	if logs := harness.logs.String(); !strings.Contains(logs, `"msg":"`+ledgerUnavailableLine+`"`) {
		t.Fatalf("no %q WARN:\n%s", ledgerUnavailableLine, logs)
	}

	liveHandler := listenerDeliveryHandler(cfg)
	liveHandler(deliveryMessage{data: data, mode: liveDelivery, msg: &natsgo.Msg{Subject: topic, Data: data}})
	if got := prompts.Load(); got != 1 {
		t.Fatalf("the live lane prompted %d times without the ledger, want 1", got)
	}
	if got := strings.Count(harness.logs.String(), `"msg":"`+ledgerUnavailableLine+`"`); got != 2 {
		t.Fatalf("logged %d %q WARNs, want one per lane", got, ledgerUnavailableLine)
	}
}

// A session another listener owns, or a self-subscribed one, is never prompted by this listener,
// so its delivery writes nothing to the ledger, and a message that reaches only such sessions is
// acked as before.
func TestASessionThisListenerDoesNotPromptCostsNoLedgerWrite(t *testing.T) {
	harness := newListenerDeliveryHarness(t, countingPrompts(new(atomic.Int32)))
	cfg := harness.config
	cfg.ledger = unavailableLedger{}
	if err := harness.sessions.Put("ses_self", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	if err := harness.sessions.Put("ses_remote", session.SessionEntry{Port: 1, MachineID: "other-machine"}); err != nil {
		t.Fatalf("register the other listener's session: %v", err)
	}
	for _, sessionID := range []string{"ses_self", "ses_remote"} {
		item := listenerTestEnvelope(contracts.AgentSubject(sessionID), "unprompted-"+sessionID)
		msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
		jetStreamDeliveryHandler(cfg)(context.Background(), msg)
		if got := settlement(t); got != "+ACK" {
			t.Fatalf("%s: settlement = %q, want an ack", sessionID, got)
		}
	}
	if logs := harness.logs.String(); strings.Contains(logs, ledgerUnavailableLine) {
		t.Fatalf("a session this listener does not prompt touched the ledger:\n%s", logs)
	}
}

func TestADeliveryExceptionsIDIsItsEventRecipientAndReason(t *testing.T) {
	id := deliveryExceptionID("evt-1", "ses_a", "no_holder")
	if id != deliveryExceptionID("evt-1", "ses_a", "no_holder") {
		t.Fatal("one event, recipient and reason gave two exception ids")
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("exception id = %q, want 32 hex", id)
	}
	for _, other := range []string{
		deliveryExceptionID("evt-1", "ses_a", "delivery_failed"),
		deliveryExceptionID("evt-2", "ses_a", "no_holder"),
		deliveryExceptionID("evt-1", "ses_b", "no_holder"),
		deliveryExceptionID("evt-1ses_a", "", "no_holder"),
	} {
		if other == id {
			t.Fatalf("another event, recipient or reason gave the same exception id %q", id)
		}
	}
}

// A redelivery that ends in no_holder again publishes the same exception: one id, and one message
// in the stream.
func TestARepeatedDeliveryExceptionCarriesOneIDAndIsStoredOnce(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	item := listenerTestEnvelope(contracts.AgentSubject("ses_nowhere"), "agent.ses_nowhere.0123456789abcdef0123456789abcdef")
	exceptionTopic := bus.ExceptionTopicPrefix + item.Topic
	probe, err := harness.client.Conn.SubscribeSync(exceptionTopic)
	if err != nil {
		t.Fatalf("subscribe exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush exception probe: %v", err)
	}

	data := marshalListenerEnvelope(t, item)
	harness.handler(&natsgo.Msg{Data: data})
	harness.handler(&natsgo.Msg{Data: data})
	first := assertDeliveryException(t, probe, item, "no_holder")
	second := assertDeliveryException(t, probe, item, "no_holder")
	if first.EventID != second.EventID || first.DedupeKey != second.DedupeKey {
		t.Fatalf("the two exceptions carry ids %q and %q, keys %q and %q; want one", first.EventID, second.EventID, first.DedupeKey, second.DedupeKey)
	}
	if want := deliveryExceptionID(item.EventID, "ses_nowhere", "no_holder"); first.EventID != want || first.DedupeKey != bus.DeliveryExceptionDedupePrefix+want {
		t.Fatalf("exception id %q key %q, want %q", first.EventID, first.DedupeKey, want)
	}
	info, err := harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: exceptionTopic})
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if got := info.State.Subjects[exceptionTopic]; got != 1 {
		t.Fatalf("the stream holds %d exceptions on %s, want 1", got, exceptionTopic)
	}
}
