package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
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
	// The durable's handler reads the subject before it decodes the message (durableNeedsDelivery),
	// so a message handed to it by hand carries the subject a publisher would have stored it on:
	// its envelope's topic.
	var stored struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("read the topic of the message handed to the durable: %v", err)
	}
	return &natsgo.Msg{Subject: stored.Topic, Data: data, Reply: inbox, Sub: sub}, func(t *testing.T) string {
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

// Two pods of one machine share the delivery ledger: pod A forwards a role message to its
// holder and records the pair delivered, and pod B, given the same message on its own role
// lane, finds that record and forwards nothing. The role-lane forward is the one delivery this
// listener makes itself, so it is the one the ledger keeps a delivered record for, and this is
// what keeps a holder from being sent the same role message twice by two pods of one machine.
func TestAPodTakingOverDoesNotForwardARoleMessageTheOldPodDelivered(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role, holder = "takeover-forward", "ses_takeover_holder"
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the holder: %v", err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	receipts, err := harness.client.Conn.Subscribe(contracts.AgentSubject(holder), func(message *natsgo.Msg) {
		if message.Reply != "" {
			_ = message.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe the holder's receipt: %v", err)
	}
	t.Cleanup(func() { _ = receipts.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the holder's subscription: %v", err)
	}
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "ledger-takeover-"+id.New())
	item.Payload = `{"type":"worker-queued"}`
	data := marshalListenerEnvelope(t, item)

	forwardOn := func(cfg listenerDeliveryHandlerConfig, forwards *int) listenerDeliveryHandlerConfig {
		cfg.forwardRole = inlineForward(func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) error {
			*forwards++
			return harness.client.RequestCoreTo(subject, item, msgID, timeout)
		})
		return wireTestRoleForwarder(t, cfg)
	}

	var podAForwards int
	podA := forwardOn(harness.config, &podAForwards)
	coreNATSDeliveryHandler(podA)(&natsgo.Msg{Data: data})
	podA.roleForwards.settled(t)
	if podAForwards != 1 {
		t.Fatalf("pod A forwarded %d times, want 1", podAForwards)
	}
	if !ledgerDelivered(t, harness.config, item.DedupeKey, holder) {
		t.Fatal("pod A's forward left no delivered record in the machine's ledger")
	}

	podBConfig := harness.config
	podBConfig.ledger = testDeliveryLedger(t, harness.client.Conn, "test-machine", "boot-pod-b")
	var podBForwards int
	podB := forwardOn(podBConfig, &podBForwards)
	coreNATSDeliveryHandler(podB)(&natsgo.Msg{Data: data})
	podB.roleForwards.settled(t)
	if podBForwards != 0 {
		t.Fatalf("pod B forwarded the role message %d times, want none: pod A's delivered record is the machine's", podBForwards)
	}
	if logs := harness.logs.String(); !strings.Contains(logs, `"msg":"listener role dedupe skip"`) {
		t.Fatalf("pod B logged no dedupe skip:\n%s", logs)
	}
}

// A pod that finds another pod's attempt younger than one message's handling limit and its
// margin cannot tell whether that pod is still delivering, so the durable lane NAKs the message
// until the attempt is old enough to take over, and nothing is reported meanwhile.
func TestADurableMessageAnotherPodIsAttemptingIsRetriedOnceItCanBeTakenOver(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const topic = "notifications.github.owner.repo.issue.8"
	harness.registerCorePathTarget(t, "ses_deferred", topic, nil)
	probe := exceptionProbe(t, harness.client)
	item := listenerTestEnvelope(topic, "ledger-deferred")
	item.Payload = `{"delivery":{"mode":"btw"}}`

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
	noException(t, probe)
}

// unavailableLedger is a delivery ledger whose bucket does not answer.
type unavailableLedger struct{}

var errLedgerDown = errors.New("nats: timeout")

func (unavailableLedger) Attempt(string, string) (dedupe.Attempt, error) {
	return dedupe.Attempt{}, errLedgerDown
}

// Record, Clear and InDoubt of an attempt that holds nothing touch no bucket, as dedupe.KV's do;
// Keep touches no bucket at all.
func (unavailableLedger) Record(attempt dedupe.Attempt) error  { return heldOnly(attempt) }
func (unavailableLedger) Clear(attempt dedupe.Attempt) error   { return heldOnly(attempt) }
func (unavailableLedger) InDoubt(attempt dedupe.Attempt) error { return heldOnly(attempt) }
func (unavailableLedger) Keep(dedupe.Attempt)                  {}

func heldOnly(attempt dedupe.Attempt) error {
	if attempt.Verdict != dedupe.Proceed {
		return nil
	}
	return errLedgerDown
}

// A ledger that cannot be read leaves the durable lane unable to tell a repeat from a first
// delivery, so it retries the message after the usual 30 s rather than risk reporting twice; the
// core lane, which retries nothing, judges it without the ledger. Each says so at WARN.
func TestAnUnavailableLedgerRetriesTheDurableLaneAndLetsACoreLaneDeliver(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const topic = "notifications.github.owner.repo.issue.9"
	harness.registerCorePathTarget(t, "ses_unavailable", topic, nil)
	probe := exceptionProbe(t, harness.client)
	cfg := harness.config
	cfg.ledger = unavailableLedger{}
	item := listenerTestEnvelope(topic, "ledger-unavailable")
	item.Payload = `{"delivery":{"mode":"btw"}}`
	data := marshalListenerEnvelope(t, item)

	msg, settlement := settledMessage(t, harness.client.Conn, data)
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)
	if delay := nakDelay(t, settlement(t)); delay != durableRetryDelay {
		t.Fatalf("durable NAK delay = %s, want %s", delay, durableRetryDelay)
	}
	noException(t, probe)
	if logs := harness.logs.String(); !strings.Contains(logs, `"msg":"`+ledgerUnavailableLine+`"`) {
		t.Fatalf("no %q WARN:\n%s", ledgerUnavailableLine, logs)
	}

	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: topic, Data: data})
	assertDeliveryException(t, probe, item, "delivery_failed")
	if got := strings.Count(harness.logs.String(), `"msg":"`+ledgerUnavailableLine+`"`); got != 2 {
		t.Fatalf("logged %d %q WARNs, want one per lane", got, ledgerUnavailableLine)
	}
}

// failingOnceLedger fails its first Attempt, as a call made while the ledger's connection was
// reconnecting does, and answers from Ledger after that.
type failingOnceLedger struct {
	dedupe.Ledger
	failed atomic.Bool
}

func (l *failingOnceLedger) Attempt(dedupeKey, sessionID string) (dedupe.Attempt, error) {
	if l.failed.CompareAndSwap(false, true) {
		return dedupe.Attempt{}, errLedgerDown
	}
	return l.Ledger.Attempt(dedupeKey, sessionID)
}

// A durable message whose ledger call failed while the ledger's connection was reconnecting is
// asked about again once the connection is back, and judged then, rather than NAKed for 30 s:
// a short drop of the link otherwise held every message in flight for half a minute. A connection
// that does not come back within the wait still retries the message after the 30 s.
func TestALedgerCallThatFailedInAReconnectIsMadeAgainOnceTheConnectionIsBack(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const topic = "notifications.github.owner.repo.issue.10"
	harness.registerCorePathTarget(t, "ses_reconnect", topic, nil)
	probe := exceptionProbe(t, harness.client)
	cfg := harness.config
	cfg.ledger = &failingOnceLedger{Ledger: testDeliveryLedger(t, harness.client.Conn, "test-machine", "boot-reconnect")}
	var waits atomic.Int32
	cfg.ledgerReconnected = func(context.Context) bool { waits.Add(1); return true }

	refused := listenerTestEnvelope(topic, "ledger-reconnect")
	refused.Payload = `{"delivery":{"mode":"btw"}}`
	msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, refused))
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)
	if got := settlement(t); got != "+ACK" {
		t.Fatalf("settlement = %q, want the refusal reported and the message acked", got)
	}
	assertDeliveryException(t, probe, refused, "delivery_failed")
	if got := waits.Load(); got != 1 {
		t.Fatalf("the durable lane waited for the ledger's connection %d times, want 1", got)
	}
	if logs := harness.logs.String(); strings.Contains(logs, ledgerUnavailableLine) {
		t.Fatalf("a ledger call that answered on its retry logged %q:\n%s", ledgerUnavailableLine, logs)
	}

	cfg.ledger = unavailableLedger{}
	cfg.ledgerReconnected = func(context.Context) bool { waits.Add(1); return false }
	stillDown := listenerTestEnvelope(topic, "ledger-still-down")
	stillDown.Payload = `{"delivery":{"mode":"btw"}}`
	msg, settlement = settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, stillDown))
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)
	if delay := nakDelay(t, settlement(t)); delay != durableRetryDelay {
		t.Fatalf("NAK delay with the connection still down = %s, want %s", delay, durableRetryDelay)
	}
	noException(t, probe)
}

// The durable lane writes nothing to the ledger for a session that receives - one reading its
// own NATS subscription, one whose own consumer the relay pulls, one another listener owns -
// and a message that reaches only such sessions is acked. The agent-subject path never decodes
// one for a session that takes the stored message itself (durableNeedsDelivery), and settles a
// session another listener owns before it asks the ledger at all. The fan-out holds to the same
// rule: it asks the ledger for a recipient it refuses, whose exception the pair's record
// dedupes, and for no other. A recipient that simply takes the message would otherwise cost two
// KV round trips on the durable's handler for a delivery this pod never makes, and a 30 s retry
// of the message for every one of its recipients while the ledger is down. The core lane's
// agent-subject delivery does record the pair a self-subscribed session took, which is what
// dedupes the next copy of it (TestListenerDeliveryHandler_DedupesSuccessfulDelivery).
func TestASessionThatReceivesCostsNoLedgerWrite(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	cfg.ledger = unavailableLedger{}
	if err := harness.sessions.Put("ses_self", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_remote", MachineID: "other-machine"}, nil); err != nil {
		t.Fatalf("register the other listener's stored subscription: %v", err)
	}
	if err := harness.sessions.Put("ses_remote", session.SessionEntry{SelfSubscribed: true, MachineID: "other-machine"}); err != nil {
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

	// The fan-out's recipients here are a self-subscribed session, which takes the message on its
	// own subscription, and one whose entry has lapsed while its interest stands, which this
	// listener delivers to nobody. The durable path still fans the message out, since the lapsed
	// session's interest is no session of this machine that takes it another way
	// (durableNeedsDelivery), and neither recipient costs a ledger call.
	const topic = "notifications.github.owner.repo.fanout-unprompted"
	for _, sessionID := range []string{"ses_self", "ses_lapsed"} {
		if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
			t.Fatalf("register %s's interest in %s: %v", sessionID, topic, err)
		}
	}
	item := listenerTestEnvelope(topic, "unprompted-fanout")
	msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)
	if got := settlement(t); got != "+ACK" {
		t.Fatalf("the fan-out's settlement = %q, want an ack", got)
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

// A delivery that ends in no_holder again, once the first one's record has aged out, publishes
// the same exception: one id, and one message in the stream. Inside the attempt window the give-up
// keeps its ledger attempt and the second delivery is an attempt-dedupe skip instead
// (TestAGiveUpWhoseNoticeCanNeverBePublishedIsStillTerminal), so the record is aged out between
// the two, which is what dedupe.AttemptedAge does on its own.
func TestARepeatedDeliveryExceptionCarriesOneIDAndIsStoredOnce(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
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
	forgetLedgerRecord(t, harness, item.DedupeKey, "ses_nowhere")
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
