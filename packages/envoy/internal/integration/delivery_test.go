package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// testEnv holds a complete test environment: NATS, listener handler, mock sessions.
type testEnv struct {
	t         *testing.T
	ctx       context.Context
	client    *bus.Client
	registry  *store.Registry
	sessions  session.SessionLookup
	deliverer session.Deliverer
	dedupe    *dedupe.Cache
}

type testEnvOption func(*testEnvOpts)

type testEnvOpts struct{ sessionTTL time.Duration }

func withSessionTTL(d time.Duration) testEnvOption {
	return func(o *testEnvOpts) { o.sessionTTL = d }
}

func setupTestEnv(t *testing.T, options ...testEnvOption) *testEnv {
	t.Helper()
	opts := testEnvOpts{sessionTTL: 10 * time.Second}
	for _, o := range options {
		o(&opts)
	}
	ctx := context.Background()

	// Start real NATS with JetStream
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	t.Cleanup(func() { ctr.Terminate(ctx) })

	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get NATS URI: %v", err)
	}

	// Connect bus client
	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })

	// Set up interest registry (NATS KV)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(opts.sessionTTL))
	if err != nil {
		t.Fatalf("failed to create session registry: %v", err)
	}

	deliverer := session.Deliverer{
		MachineID:    "test-machine",
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}

	dc := dedupe.New(5 * time.Minute)

	return &testEnv{
		t:         t,
		ctx:       ctx,
		client:    client,
		registry:  registry,
		sessions:  sessions,
		deliverer: deliverer,
		dedupe:    dc,
	}
}

// mockSession starts a mock HTTP server that counts prompt_async calls.
func mockSession(t *testing.T) (port int, deliveries *atomic.Int32, bodies *[]string, server *httptest.Server) {
	t.Helper()
	var count atomic.Int32
	var mu sync.Mutex
	var capturedBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		buf := make([]byte, 65536)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		capturedBodies = append(capturedBodies, string(buf[:n]))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	p := 0
	fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &p)
	if p == 0 {
		fmt.Sscanf(srv.URL, "http://[::1]:%d", &p)
	}
	return p, &count, &capturedBodies, srv
}

// registerInterest subscribes interest in the KV store (envoy_interests bucket).
func (env *testEnv) registerInterest(sessionID string, topics []string) {
	env.t.Helper()
	allTopics := append([]string{contracts.AgentSubject(sessionID)}, topics...)
	env.registry.Upsert(store.Interest{
		SessionID: sessionID,
		MachineID: "test-machine",
		Dir:       "/test",
	}, allTopics)
}

func (env *testEnv) registerSession(sessionID string, port int, topics []string) {
	env.t.Helper()
	env.registerKVSession(sessionID, port)
	env.registerInterest(sessionID, topics)
}

// registerKVSession writes a session-liveness entry to the envoy_sessions KV bucket.
func (env *testEnv) registerKVSession(sessionID string, port int) {
	env.t.Helper()
	if err := env.sessions.Put(sessionID, session.SessionEntry{
		Port:      port,
		MachineID: "test-machine",
		Dir:       "/test",
	}); err != nil {
		env.t.Fatal(err)
	}
}

func publishEnvelope(env *testEnv, item contracts.Envelope) {
	env.t.Helper()
	if err := env.client.Publish(item); err != nil {
		env.t.Fatalf("failed to publish: %v", err)
	}
}

func newEnvelope(source, topic, summary, dedupeKey string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Source:         source,
		SourceSession:  "ses_sender",
		SourceEventID:  fmt.Sprintf("src-%d", time.Now().UnixNano()),
		Topic:          topic,
		DedupeKey:      dedupeKey,
		IssuedAt:       time.Now().UnixMilli(),
		PayloadSummary: summary,
		TraceID:        fmt.Sprintf("trace-%d", time.Now().UnixNano()),
	}
}

// startConsumer subscribes to NATS and processes messages using the same logic as the listener.
func (env *testEnv) startConsumer(machineID string) {
	env.t.Helper()
	_, err := env.client.JS().Subscribe("notifications.>", func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			msg.Ack()
			return
		}
		if item.Validate() != nil {
			msg.Ack()
			return
		}

		// Agent path
		if item.Source == "agent" {
			sessionID := item.Topic[len("notifications.agent."):]
			if env.dedupe.Seen(item.DedupeKey, sessionID) {
				msg.Ack()
				return
			}
			interest, err := env.registry.Get(sessionID)
			var interestPtr *store.Interest
			if err == nil {
				interestPtr = &interest
			}
			result := session.HandleAgentMessage(item, sessionID, machineID, interestPtr, &env.deliverer)
			if result.Delivered {
				env.dedupe.Record(item.DedupeKey, sessionID)
			}
			if result.ShouldNAK {
				msg.NakWithDelay(1 * time.Second)
			} else {
				msg.Ack()
			}
			return
		}

		// Broadcast path
		items := env.registry.Match(machineID, item.Topic)
		if len(items) == 0 {
			msg.Ack()
			return
		}
		var failed bool
		for _, interest := range items {
			if env.dedupe.Seen(item.DedupeKey, interest.SessionID) {
				continue
			}
			if item.SourceSession != "" && item.SourceSession == interest.SessionID {
				continue
			}
			if err := env.deliverer.Deliver(item, interest); err != nil {
				failed = true
			} else {
				env.dedupe.Record(item.DedupeKey, interest.SessionID)
			}
		}
		if failed {
			msg.NakWithDelay(1 * time.Second)
		} else {
			msg.Ack()
		}
	}, natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck(), natsgo.AckWait(10*time.Second))
	if err != nil {
		env.t.Fatalf("failed to subscribe: %v", err)
	}
}

// --- TESTS ---

func TestE2E_AgentMessageDeliveredExactlyOnce(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_target", port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_target", "hello", "dedupe-1"))

	// Wait for delivery
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", count)
	}
}

func TestE2E_DuplicateEnvelopeDeduped(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_target", port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_target", "hello", "dedupe-same")
	publishEnvelope(env, item)
	time.Sleep(1 * time.Second)
	// Publish again with same dedupe key
	publishEnvelope(env, item)
	time.Sleep(2 * time.Second)

	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery (second deduped), got %d", count)
	}
}

func TestE2E_BroadcastDeliveredToAllSubscribers(t *testing.T) {
	env := setupTestEnv(t)

	port1, deliveries1, _, _ := mockSession(t)
	port2, deliveries2, _, _ := mockSession(t)

	env.registerSession("ses_one", port1, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_two", port2, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "hey", "dedupe-broadcast"))

	time.Sleep(2 * time.Second)
	if c1 := deliveries1.Load(); c1 != 1 {
		t.Fatalf("session 1: expected 1 delivery, got %d", c1)
	}
	if c2 := deliveries2.Load(); c2 != 1 {
		t.Fatalf("session 2: expected 1 delivery, got %d", c2)
	}
}

func TestE2E_BroadcastSkipsSender(t *testing.T) {
	env := setupTestEnv(t)

	portSender, deliveriesSender, _, _ := mockSession(t)
	portOther, deliveriesOther, _, _ := mockSession(t)

	env.registerSession("ses_sender", portSender, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_other", portOther, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	// Publish with SourceSession matching one of the subscribers
	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "team broadcast", "dedupe-echo")
	item.SourceSession = "ses_sender"
	publishEnvelope(env, item)

	time.Sleep(2 * time.Second)
	if c := deliveriesSender.Load(); c != 0 {
		t.Fatalf("sender should NOT receive its own broadcast, got %d deliveries", c)
	}
	if c := deliveriesOther.Load(); c != 1 {
		t.Fatalf("other subscriber expected 1 delivery, got %d", c)
	}
}

func TestE2E_BroadcastEmptySourceSessionDeliveredToAll(t *testing.T) {
	env := setupTestEnv(t)

	port1, deliveries1, _, _ := mockSession(t)
	port2, deliveries2, _, _ := mockSession(t)

	env.registerSession("ses_a", port1, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_b", port2, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	// Publish with empty SourceSession — no echo skip should happen
	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "no sender", "dedupe-nosource")
	item.SourceSession = ""
	publishEnvelope(env, item)

	time.Sleep(2 * time.Second)
	if c := deliveries1.Load(); c != 1 {
		t.Fatalf("session A expected 1 delivery, got %d", c)
	}
	if c := deliveries2.Load(); c != 1 {
		t.Fatalf("session B expected 1 delivery, got %d", c)
	}
}

func TestE2E_DeliveryFailureRetries(t *testing.T) {
	env := setupTestEnv(t)

	// Start with a dead port (no mock server)
	env.registerSession("ses_dead", 1, []string{"notifications.slack.*.*.mention"})

	// Also register a live session
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_live", port, []string{"notifications.slack.*.*.mention"})

	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "test", "dedupe-retry"))

	// The dead session fails, causing NAK. The live session gets delivered on retry.
	// With dedupe, the live session should still only get 1 delivery.
	time.Sleep(5 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("live session: expected exactly 1 delivery (dedupe protects from retry duplication), got %d", count)
	}
}

func TestE2E_NoRegistryEntryNoDelivery(t *testing.T) {
	env := setupTestEnv(t)

	env.registry.Upsert(store.Interest{
		SessionID: "ses_ghost",
		MachineID: "test-machine",
		Dir:       "/test",
	}, []string{"notifications.agent.ses_ghost"})

	port, deliveries, _, _ := mockSession(t)
	_ = port // not registered

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_ghost", "hello", "dedupe-ghost"))

	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 0 {
		t.Fatalf("expected 0 deliveries (no registry entry), got %d", count)
	}
}

func TestE2E_PromptAsyncBodyContainsNotification(t *testing.T) {
	env := setupTestEnv(t)
	port, _, bodies, _ := mockSession(t)
	env.registerSession("ses_target", port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_target", "important message", "dedupe-body"))

	time.Sleep(2 * time.Second)
	if len(*bodies) == 0 {
		t.Fatal("no delivery received")
	}

	var body struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal([]byte((*bodies)[0]), &body); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if len(body.Parts) != 1 || body.Parts[0]["type"] != "text" {
		t.Fatalf("unexpected body structure: %+v", body)
	}
	text := body.Parts[0]["text"]
	if text == "" {
		t.Fatal("empty notification text")
	}
	// Should contain the payload
	if !contains(text, "important message") {
		t.Errorf("body missing payload: %s", text)
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- NEW TESTS ---

func TestE2E_WildcardTopicMatching(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_wildcard", port, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "wildcard test", "dedupe-wildcard"))

	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery via wildcard match, got %d", count)
	}
}

func TestE2E_UnsubscribeStopsDelivery(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	broadcastTopic := "notifications.slack.T999.C999.mention"
	env.registerSession("ses_unsub", port, []string{broadcastTopic})
	env.startConsumer("test-machine")

	// First publish — should be delivered
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "before unsub", "dedupe-unsub-1"))
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery before unsubscribe, got %d", count)
	}

	// Unsubscribe from the broadcast topic
	if err := env.registry.Remove("ses_unsub", []string{broadcastTopic}); err != nil {
		t.Fatalf("failed to remove interest: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // allow KV watch to propagate

	// Second publish — should NOT be delivered (topic removed)
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "after unsub", "dedupe-unsub-2"))
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected still 1 delivery after unsubscribe, got %d", count)
	}
}

func TestE2E_ReplyToInBody(t *testing.T) {
	env := setupTestEnv(t)
	port, _, bodies, _ := mockSession(t)
	env.registerSession("ses_reply", port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_reply", "reply test", "dedupe-reply")
	item.SourceSession = "ses_sender_xyz"
	publishEnvelope(env, item)

	time.Sleep(2 * time.Second)
	if len(*bodies) == 0 {
		t.Fatal("no delivery received")
	}
	var parsed struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal([]byte((*bodies)[0]), &parsed); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if len(parsed.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parsed.Parts))
	}
	text := parsed.Parts[0]["text"]
	if !contains(text, "ses_sender_xyz") {
		t.Fatalf("text should contain source session 'ses_sender_xyz', got: %s", text)
	}
	replyInstruction := `Use envoy_send(target_session="ses_sender_xyz", message="...") to reply to this message.`
	if !contains(text, replyInstruction) {
		t.Fatalf("text should contain exact reply instruction %q, got: %s", replyInstruction, text)
	}
}

func TestE2E_SessionReRegistration(t *testing.T) {
	env := setupTestEnv(t)

	portA, deliveriesA, _, _ := mockSession(t)
	env.registerSession("ses_rereg", portA, nil)
	env.startConsumer("test-machine")

	// First publish — delivered to port A
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_rereg", "to A", "dedupe-rereg-1"))
	time.Sleep(2 * time.Second)
	if count := deliveriesA.Load(); count != 1 {
		t.Fatalf("expected 1 delivery to port A, got %d", count)
	}

	portB, deliveriesB, _, _ := mockSession(t)
	if err := env.sessions.Put("ses_rereg", session.SessionEntry{
		Port:      portB,
		MachineID: "test-machine",
		Dir:       "/test",
	}); err != nil {
		t.Fatalf("failed to re-register session entry: %v", err)
	}

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_rereg", "to B", "dedupe-rereg-2"))
	time.Sleep(2 * time.Second)
	if count := deliveriesB.Load(); count != 1 {
		t.Fatalf("expected 1 delivery to port B, got %d", count)
	}
	if count := deliveriesA.Load(); count != 1 {
		t.Fatalf("port A should still have exactly 1 delivery, got %d", count)
	}
}

func TestE2E_DedupeWindowExpiry(t *testing.T) {
	env := setupTestEnv(t)
	env.dedupe = dedupe.New(500 * time.Millisecond)

	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_expiry", port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_expiry", "expiry test", "dedupe-expiry")
	publishEnvelope(env, item)
	time.Sleep(700 * time.Millisecond) // > 500ms dedupe window

	// Same dedupe key again — should be delivered because window expired
	publishEnvelope(env, item)
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 2 {
		t.Fatalf("expected 2 deliveries after dedupe window expiry, got %d", count)
	}
}

func TestE2E_MalformedEnvelope(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_healthy", port, []string{"notifications.test.*"})
	env.startConsumer("test-machine")

	// Publish raw invalid JSON
	_, err := env.client.JS().Publish("notifications.test.bad", []byte("not json {{{"))
	if err != nil {
		t.Fatalf("failed to publish invalid JSON: %v", err)
	}

	// Publish valid JSON with empty required fields
	emptyEnv := contracts.Envelope{Topic: "notifications.test.bad"}
	emptyData, _ := json.Marshal(emptyEnv)
	_, err = env.client.JS().Publish("notifications.test.bad", emptyData)
	if err != nil {
		t.Fatalf("failed to publish empty envelope: %v", err)
	}

	time.Sleep(2 * time.Second)

	// Verify consumer is still alive by delivering a valid message
	publishEnvelope(env, newEnvelope("slack", "notifications.test.valid", "still alive", "dedupe-alive"))
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery (only valid message), consumer should survive malformed input, got %d", count)
	}
}

func TestE2E_ConcurrentPublish(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	env.registerSession("ses_concurrent", port, []string{"notifications.slack.*.*.concurrent"})
	env.startConsumer("test-machine")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.concurrent", fmt.Sprintf("msg-%d", idx), fmt.Sprintf("dedupe-concurrent-%d", idx)))
		}(i)
	}
	wg.Wait()

	time.Sleep(5 * time.Second)
	if count := deliveries.Load(); count != 10 {
		t.Fatalf("expected 10 deliveries, got %d", count)
	}
}

func TestE2E_MaxDeliverExhaustion(t *testing.T) {
	env := setupTestEnv(t)

	env.registry.Upsert(store.Interest{
		SessionID: "ses_exhaust",
		MachineID: "test-machine",
		Dir:       "/test",
	}, []string{contracts.AgentSubject("ses_exhaust")})

	// Custom consumer: short AckWait, MaxDeliver(3), always NAK (no dedupe)
	attempts := &atomic.Int32{}
	_, err := env.client.JS().Subscribe("notifications.>", func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			msg.Ack()
			return
		}
		if item.Validate() != nil {
			msg.Ack()
			return
		}

		attempts.Add(1)
		msg.NakWithDelay(500 * time.Millisecond)
	}, natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck(), natsgo.AckWait(2*time.Second), natsgo.MaxDeliver(3))
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_exhaust", "exhaust test", "dedupe-exhaust"))

	// Wait for retries to exhaust: 3 deliveries × ~500ms NAK delay + margin
	time.Sleep(8 * time.Second)

	count := attempts.Load()
	if count != 3 {
		t.Fatalf("expected exactly 3 delivery attempts (MaxDeliver=3), got %d", count)
	}

	// Verify retries have stopped
	time.Sleep(3 * time.Second)
	if final := attempts.Load(); final != count {
		t.Fatalf("retries should have stopped at %d, but count changed to %d", count, final)
	}
}

// --- REGRESSION TESTS ---

func TestE2E_NoAgentFieldInPromptAsync(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, bodies, _ := mockSession(t)
	env.registerSession("ses_noagent", port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_noagent", "check body", "dedupe-noagent"))

	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery, got %d", count)
	}
	if len(*bodies) == 0 {
		t.Fatal("no body captured")
	}

	// Parse as generic map to detect unexpected keys
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte((*bodies)[0]), &raw); err != nil {
		t.Fatalf("failed to parse body as map: %v", err)
	}

	// The prompt_async body must only have {"parts":[...]} — nothing else
	for key := range raw {
		if key != "parts" {
			t.Fatalf("unexpected key %q in prompt_async body: %v", key, raw)
		}
	}

	// Also verify the parts structure is correct
	var body struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal([]byte((*bodies)[0]), &body); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if len(body.Parts) != 1 || body.Parts[0]["type"] != "text" {
		t.Fatalf("unexpected body structure: %+v", body)
	}
}

func TestE2E_SessionWithKVAndInterestDeliveredOnce(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)

	env.registerSession("ses_both", port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_both", "double check", "dedupe-both"))

	// Wait long enough that a second (buggy) delivery would have arrived
	time.Sleep(3 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", count)
	}
}

func TestE2E_UnsubscribeEmptyTopicsDeletesAll(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)
	broadcastTopic := "notifications.slack.T888.C888.mention"
	env.registerSession("ses_emptyunsub", port, []string{broadcastTopic})
	env.startConsumer("test-machine")

	// Verify delivery works before unsubscribe
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "before unsub", "dedupe-emptyunsub-1"))
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery before unsubscribe, got %d", count)
	}

	// Remove with empty topics = delete the entire KV entry
	if err := env.registry.Remove("ses_emptyunsub", []string{}); err != nil {
		t.Fatalf("failed to remove all interests: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // allow KV watch to propagate

	// Verify the interest entry is completely gone
	_, err := env.registry.Get("ses_emptyunsub")
	if err == nil {
		t.Fatal("expected error from Get after Remove(empty), but entry still exists")
	}

	// Publish again with different dedupe key — should NOT be delivered
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "after unsub", "dedupe-emptyunsub-2"))
	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected still 1 delivery after Remove(empty), got %d", count)
	}
}

func TestE2E_StaleSubDedupeProtectsLiveSession(t *testing.T) {
	env := setupTestEnv(t)
	broadcastTopic := "notifications.slack.*.*.mention"
	concreteTopic := "notifications.slack.T777.C777.mention"

	// Session A: dead port (nothing listening on port 1)
	env.registerSession("ses_stale", 1, []string{broadcastTopic})

	// Session B: live mock server
	portB, deliveriesB, _, _ := mockSession(t)
	env.registerSession("ses_live_b", portB, []string{broadcastTopic})

	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", concreteTopic, "dedupe guard", "dedupe-stale"))

	// Wait for at least 1 NAK retry cycle (1s NAK delay in startConsumer) + processing margin
	time.Sleep(5 * time.Second)

	// Session B must receive exactly 1 delivery — dedupe prevents the retry duplicate
	if count := deliveriesB.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery to live session (dedupe protects from NAK retry), got %d", count)
	}
}

func TestE2E_KVFirstDeliverySkipsFile(t *testing.T) {
	env := setupTestEnv(t)

	port, deliveries, _, _ := mockSession(t)
	env.registerKVSession("ses_kv", port)
	env.registerInterest("ses_kv", nil)

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_kv", "kv test", "dedupe-kv-first"))

	time.Sleep(2 * time.Second)
	if c := deliveries.Load(); c != 1 {
		t.Fatalf("expected 1 delivery via KV, got %d", c)
	}
}

func TestE2E_BroadcastUsesKVPort(t *testing.T) {
	env := setupTestEnv(t)

	port, deliveries, _, _ := mockSession(t)
	env.registerKVSession("ses_bcast_kv", port)
	env.registerInterest("ses_bcast_kv", []string{"notifications.slack.*.*.mention"})

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.mention", "broadcast kv", "dedupe-bcast-kv"))

	time.Sleep(2 * time.Second)
	if c := deliveries.Load(); c != 1 {
		t.Fatalf("expected 1 broadcast delivery via KV, got %d", c)
	}
}

func TestE2E_KVEntryExpiresButInterestPersists(t *testing.T) {
	env := setupTestEnv(t, withSessionTTL(2*time.Second))

	port, _, _, _ := mockSession(t)
	env.registerKVSession("ses_expiry_check", port)
	env.registerInterest("ses_expiry_check", nil)

	if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
		t.Fatalf("expected KV entry to exist: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
			interest, err := env.registry.Get("ses_expiry_check")
			if err != nil {
				t.Fatalf("interest should persist after session expiry, got: %v", err)
			}
			if interest.SessionID != "ses_expiry_check" {
				t.Fatalf("wrong interest session ID: %s", interest.SessionID)
			}
			return
		}
	}
	t.Fatal("KV entry did not expire within 5s (TTL was 2s)")
}

func TestE2E_ResumeDeliveryAfterReRegistration(t *testing.T) {
	env := setupTestEnv(t, withSessionTTL(2*time.Second))

	portA, deliveriesA, _, _ := mockSession(t)
	env.registerKVSession("ses_resume", portA)
	env.registerInterest("ses_resume", nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_resume", "msg1", "dedupe-resume-1"))
	time.Sleep(2 * time.Second)
	if c := deliveriesA.Load(); c != 1 {
		t.Fatalf("expected 1 delivery to port A, got %d", c)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, err := env.sessions.Get("ses_resume"); err != nil {
			break
		}
	}

	portB, deliveriesB, _, _ := mockSession(t)
	env.registerKVSession("ses_resume", portB)

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_resume", "msg2", "dedupe-resume-2"))
	time.Sleep(2 * time.Second)
	if c := deliveriesB.Load(); c != 1 {
		t.Fatalf("expected 1 delivery to resumed port B, got %d", c)
	}
}

// TestE2E_CrossMachineAgentMessageACKsWithoutDelivery verifies that when an agent
// message targets a session on machine-B, machine-A's listener ACKs (does not deliver
// or NAK). This prevents wasting MaxDeliver budget on wrong-machine retries.
func TestE2E_CrossMachineAgentMessageACKsWithoutDelivery(t *testing.T) {
	env := setupTestEnv(t)
	// Override deliverer machine ID to match the consumer's machine-A
	env.deliverer.MachineID = "machine-A"
	// Create a mock session server (simulates the target)
	port, deliveries, _, _ := mockSession(t)

	// Register the session in KV as belonging to machine-B
	if err := env.sessions.Put("ses_on_b", session.SessionEntry{
		Port:      port,
		MachineID: "machine-B",
		Dir:       "/test",
	}); err != nil {
		t.Fatalf("failed to put session entry: %v", err)
	}

	// Register interest (any machine can see interests)
	env.registry.Upsert(store.Interest{
		SessionID: "ses_on_b",
		MachineID: "machine-B",
		Dir:       "/test",
	}, []string{contracts.AgentSubject("ses_on_b")})

	// Start consumer as machine-A (not the session's machine)
	env.startConsumer("machine-A")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_on_b", "cross-machine test", "dedupe-cross-1"))

	// Wait long enough for delivery + potential retries
	time.Sleep(3 * time.Second)

	// Machine-A should NOT have delivered (session belongs to machine-B)
	if count := deliveries.Load(); count != 0 {
		t.Fatalf("machine-A should not deliver to machine-B's session, got %d deliveries", count)
	}
}

// TestE2E_CrossMachineBroadcastFilteredByMatch verifies that broadcast messages
// are only delivered to sessions on the local machine (registry.Match filters by machine_id).
func TestE2E_CrossMachineBroadcastFilteredByMatch(t *testing.T) {
	env := setupTestEnv(t)
	// Override deliverer machine ID to match the consumer's machine-A
	env.deliverer.MachineID = "machine-A"
	portLocal, deliveriesLocal, _, _ := mockSession(t)
	portRemote, deliveriesRemote, _, _ := mockSession(t)

	// Register local session (machine-A)
	env.registry.Upsert(store.Interest{
		SessionID: "ses_local",
		MachineID: "machine-A",
		Dir:       "/test",
	}, []string{"notifications.slack.*.*.mention"})
	if err := env.sessions.Put("ses_local", session.SessionEntry{
		Port:      portLocal,
		MachineID: "machine-A",
		Dir:       "/test",
	}); err != nil {
		t.Fatal(err)
	}

	// Register remote session (machine-B)
	env.registry.Upsert(store.Interest{
		SessionID: "ses_remote",
		MachineID: "machine-B",
		Dir:       "/test",
	}, []string{"notifications.slack.*.*.mention"})
	if err := env.sessions.Put("ses_remote", session.SessionEntry{
		Port:      portRemote,
		MachineID: "machine-B",
		Dir:       "/test",
	}); err != nil {
		t.Fatal(err)
	}

	// Consumer is machine-A
	env.startConsumer("machine-A")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.mention", "broadcast", "dedupe-cross-bcast"))

	time.Sleep(3 * time.Second)

	// Local session should receive the broadcast
	if c := deliveriesLocal.Load(); c != 1 {
		t.Fatalf("local session expected 1 delivery, got %d", c)
	}
	// Remote session should NOT receive (Match filters by machine_id)
	if c := deliveriesRemote.Load(); c != 0 {
		t.Fatalf("remote session should not receive broadcast on machine-A, got %d", c)
	}
}

func TestE2E_RegistryListReturnsSortedInterests(t *testing.T) {
	env := setupTestEnv(t)

	env.registerInterest("ses_charlie", []string{"notifications.test.>"})
	env.registerInterest("ses_alpha", []string{"notifications.test.>"})
	env.registerInterest("ses_bravo", []string{"notifications.test.>"})

	// Allow watcher to propagate Upsert events to cache
	time.Sleep(500 * time.Millisecond)

	items := env.registry.List()
	if len(items) != 3 {
		t.Fatalf("expected 3 interests, got %d", len(items))
	}
	if items[0].SessionID != "ses_alpha" {
		t.Fatalf("expected first item to be ses_alpha, got %s", items[0].SessionID)
	}
	if items[1].SessionID != "ses_bravo" {
		t.Fatalf("expected second item to be ses_bravo, got %s", items[1].SessionID)
	}
	if items[2].SessionID != "ses_charlie" {
		t.Fatalf("expected third item to be ses_charlie, got %s", items[2].SessionID)
	}
}

// --- WHATSAPP MCP BRIDGE INTEGRATION ---

func TestE2E_WhatsappTopicRouting(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, bodies, _ := mockSession(t)

	// Subscribe to all WhatsApp messages for phone 15551234567
	whatsappPattern := "notifications.whatsapp.15551234567.>"
	env.registerSession("ses_whatsapp", port, []string{whatsappPattern})
	env.startConsumer("test-machine")

	// Publish a WhatsApp envelope (simulating what the MCP bridge would publish)
	waTopic := contracts.WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message")
	waEnv := newEnvelope("whatsapp", waTopic, "Hello from WhatsApp", "dedupe-wa-1")
	waEnv.SourceSession = "" // Bridge has no session
	waEnv.PayloadRef = "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	publishEnvelope(env, waEnv)

	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 1 {
		t.Fatalf("expected 1 delivery for WhatsApp message, got %d", count)
	}

	// Verify the body contains the WhatsApp message content
	if len(*bodies) == 0 {
		t.Fatal("no body captured")
	}
	if !contains((*bodies)[0], "Hello from WhatsApp") {
		t.Fatalf("body should contain WhatsApp message, got: %s", (*bodies)[0])
	}
}

func TestE2E_WhatsappDifferentPhoneNotDelivered(t *testing.T) {
	env := setupTestEnv(t)
	port, deliveries, _, _ := mockSession(t)

	// Subscribe to messages for phone 15551234567 only
	env.registerSession("ses_wa_filtered", port, []string{"notifications.whatsapp.15551234567.>"})
	env.startConsumer("test-machine")

	// Publish message for a DIFFERENT phone number
	waTopicOther := contracts.WhatsappSubject("15559876543", "5551234567@s.whatsapp.net", "message")
	publishEnvelope(env, newEnvelope("whatsapp", waTopicOther, "Wrong phone", "dedupe-wa-wrong"))

	time.Sleep(2 * time.Second)
	if count := deliveries.Load(); count != 0 {
		t.Fatalf("expected 0 deliveries for wrong phone, got %d", count)
	}
}

func TestE2E_WhatsappMultipleSubscribers(t *testing.T) {
	env := setupTestEnv(t)

	port1, deliveries1, _, _ := mockSession(t)
	port2, deliveries2, _, _ := mockSession(t)

	// Both sessions subscribe to WhatsApp messages
	env.registerSession("ses_wa_a", port1, []string{"notifications.whatsapp.15551234567.>"})
	env.registerSession("ses_wa_b", port2, []string{"notifications.whatsapp.15551234567.>"})
	env.startConsumer("test-machine")

	waTopic := contracts.WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message")
	item := newEnvelope("whatsapp", waTopic, "Broadcast WhatsApp", "dedupe-wa-broadcast")
	item.SourceSession = ""
	publishEnvelope(env, item)

	time.Sleep(2 * time.Second)
	if c1 := deliveries1.Load(); c1 != 1 {
		t.Fatalf("session A expected 1 delivery, got %d", c1)
	}
	if c2 := deliveries2.Load(); c2 != 1 {
		t.Fatalf("session B expected 1 delivery, got %d", c2)
	}
}
