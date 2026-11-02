package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// deliveryEvent is one envelope the listener harness below delivered to a session. A session
// takes its messages off its own NATS subscription, so the harness hands it nothing: the event
// is recorded where the harness resolves the session and records the delivery, which is where a
// push to the session's serve port was made until dispatch://INFRA-889.
type deliveryEvent struct {
	sessionID string
	item      contracts.Envelope
}

// sessionRecorder is a registered session and the deliveries made to it.
type sessionRecorder struct {
	sessionID string
	events    chan deliveryEvent
}

// newSessionRecorder registers sessionID, following topics, and records every delivery to it.
func newSessionRecorder(t *testing.T, env *testEnv, sessionID string, topics []string) *sessionRecorder {
	t.Helper()
	events := env.watchDeliveries(sessionID)
	env.registerSession(sessionID, topics)
	return &sessionRecorder{sessionID: sessionID, events: events}
}

func (r *sessionRecorder) expectDelivery(t *testing.T, timeout time.Duration) deliveryEvent {
	t.Helper()
	select {
	case event := <-r.events:
		return event
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for a delivery to %s", r.sessionID)
		return deliveryEvent{}
	}
}

func (r *sessionRecorder) expectNoDelivery(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case event := <-r.events:
		t.Fatalf("unexpected delivery to %s: %s", r.sessionID, event.item.EventID)
	case <-time.After(timeout):
	}
}

type listenerHarness struct {
	t            *testing.T
	env          *testEnv
	client       *bus.Client
	machineID    string
	consumer     string
	dedupeCache  *dedupe.Cache
	attemptCache *dedupe.Cache
	nakDelay     time.Duration
	sub          *natsgo.Subscription
	once         sync.Once
}

func newListenerHarness(t *testing.T, env *testEnv, machineID, consumer string) *listenerHarness {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{env.client.Conn.ConnectedUrl()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect listener harness: %v", err)
	}
	h := &listenerHarness{
		t:            t,
		env:          env,
		client:       client,
		machineID:    machineID,
		consumer:     consumer,
		dedupeCache:  dedupe.New(5 * time.Minute),
		attemptCache: dedupe.New(5 * time.Minute),
		nakDelay:     50 * time.Millisecond,
	}
	t.Cleanup(h.close)
	return h
}

func (h *listenerHarness) start() {
	h.t.Helper()
	if h.sub != nil {
		return
	}
	sub, err := h.client.Subscribe(
		"",
		h.handle,
		natsgo.BindStream(bus.Stream),
		natsgo.ConsumerFilterSubjects(bus.StreamSubjects()...),
		natsgo.Durable(h.consumer),
		natsgo.AckExplicit(),
		natsgo.ManualAck(),
		natsgo.AckWait(2*time.Second),
		natsgo.MaxAckPending(256),
		natsgo.MaxDeliver(20),
	)
	if err != nil {
		h.t.Fatalf("subscribe listener harness: %v", err)
	}
	h.sub = sub
	if err := h.client.Conn.FlushTimeout(2 * time.Second); err != nil {
		h.t.Fatalf("flush subscribe: %v", err)
	}
}

func (h *listenerHarness) close() {
	h.once.Do(func() {
		if h.sub != nil {
			_ = h.sub.Unsubscribe()
			h.sub = nil
		}
		if h.client != nil {
			h.client.Close()
		}
	})
}

func (h *listenerHarness) handle(msg *natsgo.Msg) {
	var item contracts.Envelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		_ = msg.Ack()
		return
	}
	if err := item.Validate(); err != nil {
		_ = msg.Ack()
		return
	}
	if item.Source == "agent" {
		h.handleAgent(msg, item)
		return
	}
	h.handleFanout(msg, item)
}

func (h *listenerHarness) handleAgent(msg *natsgo.Msg, item contracts.Envelope) {
	sessionID := item.Topic[len(contracts.AgentTopicPrefix):]
	if h.dedupeCache.Seen(item.DedupeKey, sessionID) {
		_ = msg.Ack()
		return
	}
	if h.attemptCache.Seen(item.DedupeKey, sessionID) {
		_ = msg.Ack()
		return
	}
	interest, err := h.env.registry.Get(context.Background(), sessionID)
	var interestPtr *store.Interest
	if err == nil {
		interestPtr = &interest
	}
	entry, entryErr := h.env.sessions.Get(sessionID)
	var entryPtr *session.SessionEntry
	if entryErr == nil {
		entryPtr = &entry
	}
	h.attemptCache.Record(item.DedupeKey, sessionID)
	result := session.HandleAgentMessage(item, sessionID, h.machineID, interestPtr, entryPtr, 0)
	if result.Delivered {
		h.dedupeCache.Record(item.DedupeKey, sessionID)
		h.env.recordDelivery(sessionID, item)
	}
	if result.ShouldNAK {
		_ = msg.NakWithDelay(h.nakDelay)
		return
	}
	_ = msg.Ack()
}

func (h *listenerHarness) handleFanout(msg *natsgo.Msg, item contracts.Envelope) {
	items := h.env.registry.Match(h.machineID, item.Topic)
	if len(items) == 0 {
		_ = msg.Ack()
		return
	}
	var failed bool
	for _, interest := range items {
		if h.dedupeCache.Seen(item.DedupeKey, interest.SessionID) {
			continue
		}
		if h.attemptCache.Seen(item.DedupeKey, interest.SessionID) {
			continue
		}
		if item.SourceSession != "" && item.SourceSession == interest.SessionID {
			continue
		}
		h.attemptCache.Record(item.DedupeKey, interest.SessionID)
		// The listener resolves the subscriber's record and delivers through it
		// (fanoutDelivery, cmd/listener/delivery.go); session.Resolve reads no registry.
		targetEntry, targetErr := h.env.sessions.Get(interest.SessionID)
		var target *session.SessionEntry
		if targetErr == nil {
			target = &targetEntry
		}
		if _, err := session.Resolve(item, interest.SessionID, target, 0); err != nil {
			if fanoutShouldNAK(h.env.sessions, interest.SessionID, err) {
				failed = true
			}
			continue
		}
		h.dedupeCache.Record(item.DedupeKey, interest.SessionID)
		h.env.recordDelivery(interest.SessionID, item)
	}
	if failed {
		_ = msg.NakWithDelay(h.nakDelay)
		return
	}
	_ = msg.Ack()
}

func fanoutShouldNAK(sessions *session.SessionRegistry, sessionID string, err error) bool {
	if err == nil {
		return false
	}
	_, getErr := sessions.Get(sessionID)
	return getErr == nil
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	if fn() {
		return
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeoutCh := time.After(timeout)
	for {
		select {
		case <-timeoutCh:
			t.Fatalf("timed out waiting for %s", desc)
		case <-ticker.C:
			if fn() {
				return
			}
		}
	}
}

func waitForMatchCount(t *testing.T, env *testEnv, machineID, topic string, want int) {
	t.Helper()
	waitUntil(t, 5*time.Second, fmt.Sprintf("%d matches for %s", want, topic), func() bool {
		return len(env.registry.Match(machineID, topic)) == want
	})
}

func TestE2E_WebhookToSession(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t, env, "ses_webhook", []string{"notifications.slack.*.*.mention"})
	waitForMatchCount(t, env, "test-machine", "notifications.slack.T123.C456.mention", 1)

	listener := newListenerHarness(t, env, "test-machine", "listener-webhook")
	listener.start()

	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "hello from slack", "dedupe-webhook")
	item.SourceSession = "ses_origin"
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if event.sessionID != "ses_webhook" || event.item.EventID != item.EventID {
		t.Fatalf("delivered %+v to %s, want the published envelope to ses_webhook", event.item, event.sessionID)
	}
	if event.item.PayloadSummary != "hello from slack" || event.item.Topic != item.Topic {
		t.Fatalf("delivered %+v, want the webhook's payload on its own topic", event.item)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_AgentToAgent_LocalMachine(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t, env, "ses_local", nil)

	listener := newListenerHarness(t, env, "test-machine", "listener-agent-local")
	listener.start()

	item := newEnvelope("agent", contracts.AgentSubject("ses_local"), "private hello", "dedupe-agent-local")
	item.SourceSession = "ses_sender"
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if event.item.EventID != item.EventID || event.item.PayloadSummary != "private hello" {
		t.Fatalf("delivered %+v, want the published envelope", event.item)
	}
	if event.item.SourceSession != "ses_sender" {
		t.Fatalf("the delivered envelope names sender %q, want ses_sender", event.item.SourceSession)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_RestartResilience(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t, env, "ses_restart", []string{"notifications.slack.*.*.mention"})
	waitForMatchCount(t, env, "test-machine", "notifications.slack.T10.C10.mention", 1)

	listenerA := newListenerHarness(t, env, "test-machine", "listener-restart")
	listenerA.start()
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "before restart", "dedupe-restart-1"))
	first := recorder.expectDelivery(t, 5*time.Second)
	if first.item.PayloadSummary != "before restart" {
		t.Fatalf("first delivery was %q, want the first envelope", first.item.PayloadSummary)
	}
	listenerA.close()

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "during downtime 1", "dedupe-restart-2"))
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "during downtime 2", "dedupe-restart-3"))

	listenerB := newListenerHarness(t, env, "test-machine", "listener-restart")
	listenerB.start()

	seen := map[string]bool{}
	for len(seen) < 2 {
		event := recorder.expectDelivery(t, 5*time.Second)
		if event.item.PayloadSummary == "during downtime 1" {
			seen["during downtime 1"] = true
		}
		if event.item.PayloadSummary == "during downtime 2" {
			seen["during downtime 2"] = true
		}
	}

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "after restart", "dedupe-restart-4"))
	event := recorder.expectDelivery(t, 5*time.Second)
	if event.item.PayloadSummary != "after restart" {
		t.Fatalf("expected the post-restart envelope, got %q", event.item.PayloadSummary)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_DeadSessionFanout(t *testing.T) {
	env := setupTestEnv(t)
	broadcastTopic := "notifications.slack.*.*.mention"
	concreteTopic := "notifications.slack.T20.C20.mention"
	recorder := newSessionRecorder(t, env, "ses_live", []string{broadcastTopic})
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_dead", MachineID: "test-machine", Dir: "/dead"}, []string{broadcastTopic}); err != nil {
		t.Fatalf("upsert dead interest: %v", err)
	}
	waitForMatchCount(t, env, "test-machine", concreteTopic, 2)

	listener := newListenerHarness(t, env, "test-machine", "listener-dead-fanout")
	listener.start()

	publishEnvelope(env, newEnvelope("slack", concreteTopic, "fanout payload", "dedupe-dead-fanout"))
	event := recorder.expectDelivery(t, 5*time.Second)
	if event.item.PayloadSummary != "fanout payload" {
		t.Fatalf("the live session was delivered %q, want the published envelope", event.item.PayloadSummary)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_IdempotencyKey(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t, env, "ses_idempotent", nil)

	listener := newListenerHarness(t, env, "test-machine", "listener-idempotent")
	listener.start()

	item := newEnvelope("agent", contracts.AgentSubject("ses_idempotent"), "only once", "dedupe-idempotent")
	publishEnvelope(env, item)
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if event.item.PayloadSummary != "only once" {
		t.Fatalf("delivered %q, want the published envelope", event.item.PayloadSummary)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_InterestReaperCleanup(t *testing.T) {
	env := setupTestEnv(t)
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_stale", MachineID: "test-machine", Dir: "/stale"}, []string{"notifications.slack.*.*.mention"}); err != nil {
		t.Fatalf("upsert stale interest: %v", err)
	}

	interest, err := env.registry.Get(context.Background(), "ses_stale")
	if err != nil {
		t.Fatalf("get stale interest: %v", err)
	}
	interest.UpdatedAt = time.Now().Add(-(store.InterestReapGrace + store.InterestRefreshAge + time.Minute)).UnixMilli()
	data, err := json.Marshal(interest)
	if err != nil {
		t.Fatalf("marshal stale interest: %v", err)
	}
	js, err := env.client.Conn.JetStream(natsgo.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open kv bucket: %v", err)
	}
	if _, err := kv.Put("ses_stale", data); err != nil {
		t.Fatalf("put stale interest: %v", err)
	}
	waitUntil(t, 5*time.Second, "stale cache update", func() bool {
		current, getErr := env.registry.Get(context.Background(), "ses_stale")
		return getErr == nil && current.UpdatedAt == interest.UpdatedAt
	})

	reaped, err := env.registry.Reap(func(string) bool { return false }, store.InterestReapGrace)
	if err != nil {
		t.Fatalf("reap stale interests: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped interest, got %d", reaped)
	}
	waitUntil(t, 5*time.Second, "stale interest deletion", func() bool {
		_, getErr := env.registry.Get(context.Background(), "ses_stale")
		return getErr != nil && errors.Is(getErr, natsgo.ErrKeyNotFound)
	})
}
