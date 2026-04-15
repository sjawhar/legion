package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

type deliveryEvent struct {
	path string
	body string
}

type sessionRecorder struct {
	events chan deliveryEvent
	port   int
}

func newSessionRecorder(t *testing.T) *sessionRecorder {
	t.Helper()
	recorder := &sessionRecorder{events: make(chan deliveryEvent, 32)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read recorder body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		recorder.events <- deliveryEvent{path: r.URL.Path, body: string(body)}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &recorder.port); err != nil || recorder.port == 0 {
		if _, err := fmt.Sscanf(srv.URL, "http://[::1]:%d", &recorder.port); err != nil || recorder.port == 0 {
			t.Fatalf("parse recorder port from %q", srv.URL)
		}
	}
	return recorder
}

func (r *sessionRecorder) expectDelivery(t *testing.T, timeout time.Duration) deliveryEvent {
	t.Helper()
	select {
	case event := <-r.events:
		return event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for delivery")
		return deliveryEvent{}
	}
}

func (r *sessionRecorder) expectNoDelivery(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case event := <-r.events:
		t.Fatalf("unexpected delivery: %+v", event)
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
	client, err := bus.Connect([]string{env.client.Conn.ConnectedUrl()}, bus.WithReplicas(1))
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
		"notifications.>",
		h.handle,
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
	interest, err := h.env.registry.Get(sessionID)
	var interestPtr *store.Interest
	if err == nil {
		interestPtr = &interest
	}
	h.attemptCache.Record(item.DedupeKey, sessionID)
	result := session.HandleAgentMessage(item, sessionID, h.machineID, interestPtr, &h.env.deliverer)
	if result.Delivered {
		h.dedupeCache.Record(item.DedupeKey, sessionID)
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
		if err := h.env.deliverer.Deliver(item, interest); err != nil {
			if fanoutShouldNAK(h.env.sessions, interest.SessionID, err) {
				failed = true
			}
			continue
		}
		h.dedupeCache.Record(item.DedupeKey, interest.SessionID)
	}
	if failed {
		_ = msg.NakWithDelay(h.nakDelay)
		return
	}
	_ = msg.Ack()
}

func fanoutShouldNAK(sessions session.SessionLookup, sessionID string, err error) bool {
	if err == nil {
		return false
	}
	_, getErr := sessions.Get(sessionID)
	return getErr == nil
}

type mockSessionLookup struct {
	entry session.SessionEntry
	err   error
}

func (m *mockSessionLookup) Get(string) (session.SessionEntry, error) {
	if m.err != nil {
		return session.SessionEntry{}, m.err
	}
	return m.entry, nil
}

func (m *mockSessionLookup) Put(string, session.SessionEntry) error { return nil }

func (m *mockSessionLookup) Delete(string) error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
	recorder := newSessionRecorder(t)
	env.registerSession("ses_webhook", recorder.port, []string{"notifications.slack.*.*.mention"})
	waitForMatchCount(t, env, "test-machine", "notifications.slack.T123.C456.mention", 1)

	listener := newListenerHarness(t, env, "test-machine", "listener-webhook")
	listener.start()

	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "hello from slack", "dedupe-webhook")
	item.SourceSession = "ses_origin"
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if event.path != "/session/ses_webhook/prompt_async" {
		t.Fatalf("unexpected delivery path: %s", event.path)
	}
	if !strings.Contains(event.body, "hello from slack") {
		t.Fatalf("delivery body missing webhook payload: %s", event.body)
	}
	if !strings.Contains(event.body, item.Topic) {
		t.Fatalf("delivery body missing topic: %s", event.body)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_AgentToAgent_LocalMachine(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t)
	env.registerSession("ses_local", recorder.port, nil)

	listener := newListenerHarness(t, env, "test-machine", "listener-agent-local")
	listener.start()

	item := newEnvelope("agent", contracts.AgentSubject("ses_local"), "private hello", "dedupe-agent-local")
	item.SourceSession = "ses_sender"
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if event.path != "/session/ses_local/prompt_async" {
		t.Fatalf("unexpected delivery path: %s", event.path)
	}
	if !strings.Contains(event.body, "private hello") {
		t.Fatalf("delivery body missing agent payload: %s", event.body)
	}
	if !strings.Contains(event.body, "ses_sender") || !strings.Contains(event.body, "reply to this message") {
		t.Fatalf("delivery body missing reply instruction: %s", event.body)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_AgentToAgent_RemoteMachine(t *testing.T) {
	env := setupTestEnv(t)
	defer func(original session.SessionLookup, originalClient *http.Client) {
		env.deliverer.Sessions = original
		env.deliverer.HTTPClient = originalClient
	}(env.deliverer.Sessions, env.deliverer.HTTPClient)

	env.deliverer.Sessions = &mockSessionLookup{entry: session.SessionEntry{Port: 7777, MachineID: "remote-machine", Dir: "/remote"}}
	env.deliverer.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "remote-machine" {
			t.Fatalf("expected remote-machine host, got %s", r.URL.Hostname())
		}
		return nil, session.ErrWrongMachine
	})}
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_remote", MachineID: "remote-machine", Dir: "/remote"}, []string{contracts.AgentSubject("ses_remote")}); err != nil {
		t.Fatalf("upsert remote interest: %v", err)
	}

	listener := newListenerHarness(t, env, "test-machine", "listener-agent-remote")
	listener.start()

	publishEnvelope(env, newEnvelope("agent", contracts.AgentSubject("ses_remote"), "remote hello", "dedupe-agent-remote"))

	waitUntil(t, 5*time.Second, "remote agent message acked", func() bool {
		consumerInfo, err := listener.client.JS().ConsumerInfo(bus.Stream, listener.consumer)
		if err != nil {
			return false
		}
		return consumerInfo.NumPending == 0 && consumerInfo.AckFloor.Consumer > 0
	})
}

func TestE2E_RestartResilience(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t)
	env.registerSession("ses_restart", recorder.port, []string{"notifications.slack.*.*.mention"})
	waitForMatchCount(t, env, "test-machine", "notifications.slack.T10.C10.mention", 1)

	listenerA := newListenerHarness(t, env, "test-machine", "listener-restart")
	listenerA.start()
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "before restart", "dedupe-restart-1"))
	first := recorder.expectDelivery(t, 5*time.Second)
	if !strings.Contains(first.body, "before restart") {
		t.Fatalf("first delivery body mismatch: %s", first.body)
	}
	listenerA.close()

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "during downtime 1", "dedupe-restart-2"))
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "during downtime 2", "dedupe-restart-3"))

	listenerB := newListenerHarness(t, env, "test-machine", "listener-restart")
	listenerB.start()

	seen := map[string]bool{}
	for len(seen) < 2 {
		event := recorder.expectDelivery(t, 5*time.Second)
		if strings.Contains(event.body, "during downtime 1") {
			seen["during downtime 1"] = true
		}
		if strings.Contains(event.body, "during downtime 2") {
			seen["during downtime 2"] = true
		}
	}

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T10.C10.mention", "after restart", "dedupe-restart-4"))
	event := recorder.expectDelivery(t, 5*time.Second)
	if !strings.Contains(event.body, "after restart") {
		t.Fatalf("expected post-restart delivery, got: %s", event.body)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_DeadSessionFanout(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t)
	broadcastTopic := "notifications.slack.*.*.mention"
	concreteTopic := "notifications.slack.T20.C20.mention"
	env.registerSession("ses_live", recorder.port, []string{broadcastTopic})
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_dead", MachineID: "test-machine", Dir: "/dead"}, []string{broadcastTopic}); err != nil {
		t.Fatalf("upsert dead interest: %v", err)
	}
	waitForMatchCount(t, env, "test-machine", concreteTopic, 2)

	listener := newListenerHarness(t, env, "test-machine", "listener-dead-fanout")
	listener.start()

	publishEnvelope(env, newEnvelope("slack", concreteTopic, "fanout payload", "dedupe-dead-fanout"))
	event := recorder.expectDelivery(t, 5*time.Second)
	if !strings.Contains(event.body, "fanout payload") {
		t.Fatalf("live session body mismatch: %s", event.body)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_IdempotencyKey(t *testing.T) {
	env := setupTestEnv(t)
	recorder := newSessionRecorder(t)
	env.registerSession("ses_idempotent", recorder.port, nil)

	listener := newListenerHarness(t, env, "test-machine", "listener-idempotent")
	listener.start()

	item := newEnvelope("agent", contracts.AgentSubject("ses_idempotent"), "only once", "dedupe-idempotent")
	publishEnvelope(env, item)
	publishEnvelope(env, item)

	event := recorder.expectDelivery(t, 5*time.Second)
	if !strings.Contains(event.body, "only once") {
		t.Fatalf("delivery body mismatch: %s", event.body)
	}
	recorder.expectNoDelivery(t, 300*time.Millisecond)
}

func TestE2E_InterestReaperCleanup(t *testing.T) {
	env := setupTestEnv(t)
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_stale", MachineID: "test-machine", Dir: "/stale"}, []string{"notifications.slack.*.*.mention"}); err != nil {
		t.Fatalf("upsert stale interest: %v", err)
	}

	interest, err := env.registry.Get("ses_stale")
	if err != nil {
		t.Fatalf("get stale interest: %v", err)
	}
	interest.UpdatedAt = time.Now().Add(-11 * time.Minute).UnixMilli()
	data, err := json.Marshal(interest)
	if err != nil {
		t.Fatalf("marshal stale interest: %v", err)
	}
	js, err := env.client.Conn.JetStream(natsgo.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(store.Bucket)
	if err != nil {
		t.Fatalf("open kv bucket: %v", err)
	}
	if _, err := kv.Put("ses_stale", data); err != nil {
		t.Fatalf("put stale interest: %v", err)
	}
	waitUntil(t, 5*time.Second, "stale cache update", func() bool {
		current, getErr := env.registry.Get("ses_stale")
		return getErr == nil && current.UpdatedAt == interest.UpdatedAt
	})

	reaped, err := env.registry.Reap(func(string) bool { return false }, 10*time.Minute)
	if err != nil {
		t.Fatalf("reap stale interests: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped interest, got %d", reaped)
	}
	waitUntil(t, 5*time.Second, "stale interest deletion", func() bool {
		_, getErr := env.registry.Get("ses_stale")
		return getErr != nil && errors.Is(getErr, natsgo.ErrKeyNotFound)
	})
}
