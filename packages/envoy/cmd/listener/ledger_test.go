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

// Record and Clear of an attempt that holds nothing touch no bucket, as dedupe.KV's do; Keep
// touches no bucket at all.
func (unavailableLedger) Record(attempt dedupe.Attempt) error { return heldOnly(attempt) }
func (unavailableLedger) Clear(attempt dedupe.Attempt) error  { return heldOnly(attempt) }
func (unavailableLedger) Keep(dedupe.Attempt)                 {}

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

// A session that receives without this listener costs it no ledger wait and no retry. The
// durable acknowledges a stored message such a session takes itself before it decodes it
// (durableNeedsDelivery), and settles a session another listener owns before it asks the ledger
// at all, so neither reaches the ledger however down it is. The fan-out does ask for a recipient
// that reads the message on its own subscription, to record the copy it took, but that ask is
// best-effort: with the ledger down the copy is counted skipped under one WARN and the message
// is acked, where a 30 s retry would hold every recipient of it back for a copy this listener
// handed to nobody.
func TestASessionThatReceivesCostsNoLedgerWaitOrRetry(t *testing.T) {
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
	if logs := harness.logs.String(); strings.Contains(logs, ledgerUnavailableLine) {
		t.Fatalf("a session the durable settles before it decodes the message touched the ledger:\n%s", logs)
	}

	// The fan-out's recipients here are a self-subscribed session, which takes the message on its
	// own subscription, and one whose entry has lapsed while its interest stands, which this
	// listener delivers to nobody. The durable path still fans the message out, since the lapsed
	// session's interest is no session of this machine that takes it another way
	// (durableNeedsDelivery). The lapsed one costs no ledger call - its resolution failed, so
	// there is no delivery to record - and the self-subscribed one costs the one best-effort ask.
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
		t.Fatalf("the fan-out's settlement = %q, want an ack with no %s retry", got, durableRetryDelay)
	}

	logs := harness.logs.String()
	if got := strings.Count(logs, `"msg":"`+ledgerUnavailableLine+`"`); got != 1 {
		t.Fatalf("logged %d %q WARNs, want the one best-effort ask of the self-subscribed recipient:\n%s", got, ledgerUnavailableLine, logs)
	}
	if line := mpLineWith(logs, `"msg":"`+ledgerUnavailableLine+`"`); !strings.Contains(line, `"retry":false`) {
		t.Fatalf("the best-effort ask asked for a retry: %s", line)
	}
}

// The copy a self-subscribed recipient read is recorded, so the second copy of the same event -
// the same dedupe key on another topic the session follows, which normalization produces - is a
// dedupe skip and not a second delivery. Under a ledger that cannot answer, such a copy is still
// counted skipped and its message acked, with no record and no 30 s retry.
func TestASecondCopyToASelfSubscribedRecipientIsADedupe(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const self, lapsed = "ses_self_dedupe", "ses_self_dedupe_lapsed"
	const one, two = "notifications.github.acme.self-dedupe.one", "notifications.github.acme.self-dedupe.two"
	if err := harness.sessions.Put(self, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	// A recipient this listener places nowhere keeps the durable from acknowledging the message
	// unread (durableNeedsDelivery): with only the self-subscribed session interested, the
	// fan-out would never run.
	for _, sessionID := range []string{self, lapsed} {
		if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{one, two}); err != nil {
			t.Fatalf("register %s's interests: %v", sessionID, err)
		}
	}
	deliver := func(t *testing.T, cfg listenerDeliveryHandlerConfig, topic, dedupeKey string) string {
		t.Helper()
		msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, listenerTestEnvelope(topic, dedupeKey)))
		jetStreamDeliveryHandler(cfg)(context.Background(), msg)
		return settlement(t)
	}

	const key = "self-dedupe-one-event"
	if got := deliver(t, harness.config, one, key); got != "+ACK" {
		t.Fatalf("the first copy's settlement = %q, want an ack", got)
	}
	if got := deliver(t, harness.config, two, key); got != "+ACK" {
		t.Fatalf("the second copy's settlement = %q, want an ack", got)
	}
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - only the first copy reached %s on its own subscription:\n%s", deliveredSeries("skipped"), got, self, body)
	}
	if got := counterValue(t, body, deliveredSeries("dedupe")); got != 1 {
		t.Fatalf("%s = %d, want 1 - the second copy is judged against the record the first wrote:\n%s", deliveredSeries("dedupe"), got, body)
	}

	down := harness.config
	down.ledger = unavailableLedger{}
	if got := deliver(t, down, one, "self-dedupe-ledger-down"); got != "+ACK" {
		t.Fatalf("the settlement with the ledger down = %q, want an ack with no %s retry", got, durableRetryDelay)
	}
	body = metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 2 {
		t.Fatalf("%s = %d, want 2 - a copy under a ledger that cannot answer is skipped too:\n%s", deliveredSeries("skipped"), got, body)
	}
}

// ledgerDownForSession fails every Attempt for one session and answers from Ledger for the rest:
// a ledger outage of one pair alone, which is what tells a delay that recipient caused from one
// another recipient caused.
type ledgerDownForSession struct {
	dedupe.Ledger
	sessionID string
}

func (l ledgerDownForSession) Attempt(dedupeKey, sessionID string) (dedupe.Attempt, error) {
	if sessionID == l.sessionID {
		return dedupe.Attempt{}, errLedgerDown
	}
	return l.Ledger.Attempt(dedupeKey, sessionID)
}

// A ledger outage for a self-subscribed recipient delays and retries nothing for the other
// recipients of the same fan-out. The message has two: a session that reads it on its own
// subscription, whose pair the ledger cannot answer for, and one whose entry advertises no
// delivery mode, which this listener refuses and reports. The refusal must be published and the
// message acked. A branch that retried the message on the self-subscribed recipient's ledger
// error would NAK it for durableRetryDelay and hold the refused recipient's answer, and every
// other recipient's, behind that copy this listener hands to nobody.
func TestALedgerOutageForASelfSubscribedRecipientDelaysNoOtherRecipient(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const self, refused = "ses_outage_self", "ses_outage_refused"
	const topic = "notifications.github.acme.ledger-outage.mention"
	if err := harness.sessions.Put(self, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	// The refused recipient takes no stored message itself, so the durable fans the message out
	// rather than acknowledging it unread (durableNeedsDelivery), and its entry advertises no
	// delivery mode, so the fan-out refuses it and reports that refusal.
	if err := harness.sessions.Put(refused, session.SessionEntry{MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the refused session: %v", err)
	}
	for _, sessionID := range []string{self, refused} {
		if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
			t.Fatalf("register %s's interest: %v", sessionID, err)
		}
	}
	probe := exceptionProbe(t, harness.client)

	cfg := harness.config
	cfg.ledger = ledgerDownForSession{Ledger: harness.config.ledger, sessionID: self}
	item := listenerTestEnvelope(topic, "ledger-outage-self")
	item.Payload = `{"delivery":{"mode":"btw"}}`
	msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)

	if got := settlement(t); got != "+ACK" {
		t.Fatalf("settlement = %q, want an ack: a ledger outage for %s must not hold the message back for %s", got, self, durableRetryDelay)
	}
	if recipient := exceptionRecipient(t, assertDeliveryException(t, probe, item, "delivery_failed")); recipient != refused {
		t.Fatalf("the exception names recipient %q, want %q", recipient, refused)
	}
	if ledgerDelivered(t, harness.config, item.DedupeKey, self) {
		t.Fatalf("a ledger that could not answer for %s recorded its copy all the same", self)
	}
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - %s read the message on its own subscription:\n%s", deliveredSeries("skipped"), got, self, body)
	}
	if got := counterValue(t, body, deliveredSeries("failed")); got != 1 {
		t.Fatalf("%s = %d, want the one refusal of %s:\n%s", deliveredSeries("failed"), got, refused, body)
	}
}

// The agent-subject path has the same one best-effort ask as the fan-out: the copy a
// self-subscribed session reads on its own NATS subscription. It is reached only when this
// listener's session cache misses, because a cached entry that says the session takes the
// message itself has the durable ack it before it is decoded (durableNeedsDelivery), so the
// arrangement here is trailingListeners' (api_test.go) applied to the delivery harness: the
// cache is warmed, its watcher stopped, and the session registered through a second registry on
// the same account, so the first read of the entry - the cache's, in durableNeedsDelivery -
// fails and the second - the bucket's, in liveSessionEntry - answers. With the ledger down for
// that session the copy is counted skipped under one WARN and the message is acked. An ask that
// was not best-effort here would NAK it for durableRetryDelay, holding a message this listener
// hands to nobody.
func TestASelfSubscribedSessionReadFromTheBucketCostsNoLedgerWaitOrRetry(t *testing.T) {
	const target = "ses_agent_subject_self"
	harness := newListenerDeliveryHarness(t)
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
	if err := registrar.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s through the other listener: %v", target, err)
	}
	// The negative control: a cache holding the entry would have the durable ack the message
	// unread, and the branch under test would never run.
	if entry, err := harness.sessions.Get(target); err == nil {
		t.Fatalf("the delivering listener's cache already holds %s (%+v): the durable would ack its message unread", target, entry)
	}

	cfg := harness.config
	cfg.ledger = ledgerDownForSession{Ledger: harness.config.ledger, sessionID: target}
	item := listenerTestEnvelope(contracts.AgentSubject(target), "agent-subject-self-subscribed")
	msg, settlement := settledMessage(t, harness.client.Conn, marshalListenerEnvelope(t, item))
	jetStreamDeliveryHandler(cfg)(context.Background(), msg)

	if got := settlement(t); got != "+ACK" {
		t.Fatalf("settlement = %q, want an ack: a ledger outage for %s must not hold the message back for %s", got, target, durableRetryDelay)
	}
	logs := harness.logs.String()
	if got := strings.Count(logs, `"msg":"`+ledgerUnavailableLine+`"`); got != 1 {
		t.Fatalf("logged %d %q WARNs, want the one best-effort ask of %s:\n%s", got, ledgerUnavailableLine, target, logs)
	}
	if line := mpLineWith(logs, `"msg":"`+ledgerUnavailableLine+`"`); !strings.Contains(line, `"retry":false`) {
		t.Fatalf("the best-effort ask asked for a retry: %s", line)
	}
	if ledgerDelivered(t, harness.config, item.DedupeKey, target) {
		t.Fatalf("a ledger that could not answer for %s recorded its copy all the same", target)
	}
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - %s read the message on its own subscription:\n%s", deliveredSeries("skipped"), got, target, body)
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
