package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// deliveryWait bounds every wait for an event this package's pipeline produces: a JetStream
	// publish and delivery, a KV watch and one HTTP push, all on loopback.
	deliveryWait = 10 * time.Second
	// settleWindow is how long a test waits to show that nothing more arrives, once the consumer
	// has handled every message the test published. The pipeline has no work left then, so the
	// window catches a push a goroutine still owes rather than one the consumer has yet to make.
	settleWindow = 300 * time.Millisecond
	// testNakDelay is the redelivery delay the stand-in consumer asks for, short so a test that
	// needs a retry waits for the retry rather than for a production-sized delay.
	testNakDelay = 150 * time.Millisecond
)

// testEnv holds a complete test environment: NATS, listener handler, mock sessions.
type testEnv struct {
	t         *testing.T
	ctx       context.Context
	client    *bus.Client
	registry  *store.Registry
	sessions  *session.SessionRegistry
	deliverer session.Deliverer
	dedupe    *dedupe.Cache
	// handled reports each message the stand-in consumer (startConsumer) has finished handling,
	// so a test waits on the pipeline rather than on a clock: a session stand-in answers its
	// push inside that handler, so every delivery a handled message made is already recorded.
	handled chan struct{}
	// nakDelay is the redelivery delay the stand-in consumer asks for when a push failed.
	nakDelay time.Duration
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
	uri := testnats.URL(t)

	// Connect bus client
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(client.Close)

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
		handled:   make(chan struct{}, 64),
		nakDelay:  testNakDelay,
	}
}

// awaitHandled waits for the stand-in consumer to finish handling n messages. Once it has, every
// push those messages made has been answered, so a session stand-in's recorded deliveries are
// final and a test can require that it received nothing more.
func (env *testEnv) awaitHandled(t *testing.T, n int) {
	t.Helper()
	for handled := range n {
		select {
		case <-env.handled:
		case <-time.After(deliveryWait):
			t.Fatalf("the consumer handled %d of %d messages within %s", handled, n, deliveryWait)
		}
	}
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
	_, err := env.client.JS().Subscribe("", func(msg *natsgo.Msg) {
		// Reported last, once the handler has finished every push this message made, so a test
		// that waits on it knows the pipeline has no work left for that message.
		defer func() { env.handled <- struct{}{} }()
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
			interest, err := env.registry.Get(context.Background(), sessionID)
			var interestPtr *store.Interest
			if err == nil {
				interestPtr = &interest
			}
			entry, entryErr := env.sessions.Get(sessionID)
			var entryPtr *session.SessionEntry
			if entryErr == nil {
				entryPtr = &entry
			}
			result := session.HandleAgentMessage(item, sessionID, machineID, interestPtr, entryPtr, &env.deliverer)
			if result.Delivered {
				env.dedupe.Record(item.DedupeKey, sessionID)
			}
			if result.ShouldNAK {
				msg.NakWithDelay(env.nakDelay)
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
			// The listener resolves the subscriber's record and delivers through it
			// (fanoutDelivery, cmd/listener/delivery.go); the Deliverer reads no registry.
			targetEntry, targetErr := env.sessions.Get(interest.SessionID)
			var target *session.SessionEntry
			if targetErr == nil {
				target = &targetEntry
			}
			if _, err := env.deliverer.DeliverWithResultForEntry(item, interest, target); err != nil {
				failed = true
			} else {
				env.dedupe.Record(item.DedupeKey, interest.SessionID)
			}
		}
		if failed {
			msg.NakWithDelay(env.nakDelay)
		} else {
			msg.Ack()
		}
	},
		natsgo.BindStream(bus.Stream),
		natsgo.ConsumerFilterSubjects(bus.StreamSubjects()...),
		natsgo.DeliverNew(),
		natsgo.AckExplicit(),
		natsgo.ManualAck(),
		natsgo.AckWait(10*time.Second),
	)
	if err != nil {
		env.t.Fatalf("failed to subscribe: %v", err)
	}
}

// --- TESTS ---

func TestDelivery_AgentMessageDeliveredExactlyOnce(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_target", target.port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_target", "hello", "dedupe-1"))

	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_DuplicateEnvelopeDeduped(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_target", target.port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_target", "hello", "dedupe-same")
	publishEnvelope(env, item)
	target.expectDelivery(t, deliveryWait)
	// Publish again with same dedupe key
	publishEnvelope(env, item)
	env.awaitHandled(t, 2)

	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_BroadcastDeliveredToAllSubscribers(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	one := newSessionRecorder(t)
	two := newSessionRecorder(t)

	env.registerSession("ses_one", one.port, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_two", two.port, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "hey", "dedupe-broadcast"))

	env.awaitHandled(t, 1)
	one.expectDelivery(t, deliveryWait)
	two.expectDelivery(t, deliveryWait)
	one.expectNoDelivery(t, settleWindow)
	two.expectNoDelivery(t, settleWindow)
}

func TestDelivery_BroadcastSkipsSender(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	sender := newSessionRecorder(t)
	other := newSessionRecorder(t)

	env.registerSession("ses_sender", sender.port, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_other", other.port, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	// Publish with SourceSession matching one of the subscribers
	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "team broadcast", "dedupe-echo")
	item.SourceSession = "ses_sender"
	publishEnvelope(env, item)

	env.awaitHandled(t, 1)
	other.expectDelivery(t, deliveryWait)
	// The sender's own broadcast reached the other subscriber, so the fan-out that skipped the
	// sender has run: it never pushed to it.
	sender.expectNoDelivery(t, settleWindow)
	other.expectNoDelivery(t, settleWindow)
}

func TestDelivery_BroadcastEmptySourceSessionDeliveredToAll(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	a := newSessionRecorder(t)
	b := newSessionRecorder(t)

	env.registerSession("ses_a", a.port, []string{"notifications.slack.*.*.mention"})
	env.registerSession("ses_b", b.port, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	// Publish with empty SourceSession — no echo skip should happen
	item := newEnvelope("slack", "notifications.slack.T123.C456.mention", "no sender", "dedupe-nosource")
	item.SourceSession = ""
	publishEnvelope(env, item)

	env.awaitHandled(t, 1)
	a.expectDelivery(t, deliveryWait)
	b.expectDelivery(t, deliveryWait)
	a.expectNoDelivery(t, settleWindow)
	b.expectNoDelivery(t, settleWindow)
}

func TestDelivery_DeliveryFailureRetries(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	// Start with a dead port (no mock server)
	env.registerSession("ses_dead", 1, []string{"notifications.slack.*.*.mention"})

	// Also register a live session
	live := newSessionRecorder(t)
	env.registerSession("ses_live", live.port, []string{"notifications.slack.*.*.mention"})

	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "test", "dedupe-retry"))

	// The dead session fails, causing NAK. The live session gets delivered on retry.
	// With dedupe, the live session should still only get 1 delivery. Two handlings are the
	// first delivery and the redelivery the NAK asked for, so the retry has run.
	live.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 2)
	live.expectNoDelivery(t, settleWindow)
}

func TestDelivery_NoRegistryEntryNoDelivery(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.registry.Upsert(store.Interest{
		SessionID: "ses_ghost",
		MachineID: "test-machine",
		Dir:       "/test",
	}, []string{"notifications.agent.ses_ghost"})

	ghost := newSessionRecorder(t) // a server nothing registered a session record for

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_ghost", "hello", "dedupe-ghost"))

	env.awaitHandled(t, 1)
	ghost.expectNoDelivery(t, settleWindow)
}

func TestDelivery_PromptAsyncBodyContainsNotification(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_target", target.port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_target", "important message", "dedupe-body"))

	delivery := target.expectDelivery(t, deliveryWait)

	var body struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal([]byte(delivery.body), &body); err != nil {
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
	if !strings.Contains(text, "important message") {
		t.Errorf("body missing payload: %s", text)
	}
}

// --- NEW TESTS ---

func TestRoleTransfer_ExactlyOneHolder(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	a := newSessionRecorder(t)
	b := newSessionRecorder(t)
	env.registerSession("ses_role_a", a.port, nil)
	env.registerSession("ses_role_b", b.port, nil)

	if _, err := env.registry.SetRole("ses_role_a", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for A failed: %v", err)
	}
	if _, err := env.registry.SetRole("ses_role_b", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for B failed: %v", err)
	}

	waitUntil(t, deliveryWait, "the role to name exactly one holder", func() bool {
		holderA, errA := env.registry.Get(context.Background(), "ses_role_a")
		holderB, errB := env.registry.Get(context.Background(), "ses_role_b")
		matches := env.registry.Match("test-machine", "notifications.role.legion-controller")
		return errA == nil && errB == nil &&
			!slices.Contains(holderA.Topics, "notifications.role.legion-controller") &&
			slices.Contains(holderB.Topics, "notifications.role.legion-controller") &&
			len(matches) == 1 && matches[0].SessionID == "ses_role_b"
	})
}

func TestDelivery_WildcardTopicMatching(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_wildcard", target.port, []string{"notifications.slack.*.*.mention"})
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T123.C456.mention", "wildcard test", "dedupe-wildcard"))

	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_UnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	broadcastTopic := "notifications.slack.T999.C999.mention"
	env.registerSession("ses_unsub", target.port, []string{broadcastTopic})
	env.startConsumer("test-machine")

	// First publish — should be delivered
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "before unsub", "dedupe-unsub-1"))
	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)

	// Unsubscribe from the broadcast topic
	if err := env.registry.Remove(context.Background(), "ses_unsub", []string{broadcastTopic}); err != nil {
		t.Fatalf("failed to remove interest: %v", err)
	}
	// The cache follows the bucket through its watcher: wait for the removal to reach it rather
	// than for a span of time the watch is expected to fit in.
	waitForMatchCount(t, env, "test-machine", broadcastTopic, 0)

	// Second publish — should NOT be delivered (topic removed)
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "after unsub", "dedupe-unsub-2"))
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_ReplyToInBody(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_reply", target.port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_reply", "reply test", "dedupe-reply")
	item.SourceSession = "ses_sender_xyz"
	publishEnvelope(env, item)

	delivery := target.expectDelivery(t, deliveryWait)
	var parsed struct {
		Parts []map[string]string `json:"parts"`
	}
	if err := json.Unmarshal([]byte(delivery.body), &parsed); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if len(parsed.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parsed.Parts))
	}
	text := parsed.Parts[0]["text"]
	header := "[NOTIFICATION to you from ses_sender_xyz]"
	if !strings.Contains(text, header) {
		t.Fatalf("text should contain header %q, got: %s", header, text)
	}
	replyInstruction := `Reply With: envoy_send(session_id="ses_sender_xyz", message="...")`
	if !strings.Contains(text, replyInstruction) {
		t.Fatalf("text should contain exact reply instruction %q, got: %s", replyInstruction, text)
	}
}

func TestDelivery_SessionReRegistration(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	a := newSessionRecorder(t)
	env.registerSession("ses_rereg", a.port, nil)
	env.startConsumer("test-machine")

	// First publish — delivered to port A
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_rereg", "to A", "dedupe-rereg-1"))
	a.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)

	b := newSessionRecorder(t)
	if err := env.sessions.Put("ses_rereg", session.SessionEntry{
		Port:      b.port,
		MachineID: "test-machine",
		Dir:       "/test",
	}); err != nil {
		t.Fatalf("failed to re-register session entry: %v", err)
	}
	// The record the consumer reads is the cache's: wait for it to name port B rather than for
	// a span of time the write is expected to fit in.
	waitUntil(t, deliveryWait, "the session record to name port B", func() bool {
		entry, err := env.sessions.Get("ses_rereg")
		return err == nil && entry.Port == b.port
	})

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_rereg", "to B", "dedupe-rereg-2"))
	b.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	b.expectNoDelivery(t, settleWindow)
	a.expectNoDelivery(t, settleWindow)
}

func TestDelivery_DedupeWindowExpiry(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	const dedupeWindow = 100 * time.Millisecond
	env.dedupe = dedupe.New(dedupeWindow)

	target := newSessionRecorder(t)
	env.registerSession("ses_expiry", target.port, nil)
	env.startConsumer("test-machine")

	item := newEnvelope("agent", "notifications.agent.ses_expiry", "expiry test", "dedupe-expiry")
	publishEnvelope(env, item)
	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	// The second publish must land past the dedupe window, which the cache itself reports.
	waitUntil(t, deliveryWait, "the dedupe window to expire", func() bool {
		return !env.dedupe.Seen("dedupe-expiry", "ses_expiry")
	})

	// Same dedupe key again — should be delivered because window expired
	publishEnvelope(env, item)
	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_MalformedEnvelope(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_healthy", target.port, []string{"notifications.github.acme.widgets.*"})
	env.startConsumer("test-machine")

	// Publish raw invalid JSON
	_, err := env.client.JS().Publish("notifications.github.acme.widgets.bad", []byte("not json {{{"))
	if err != nil {
		t.Fatalf("failed to publish invalid JSON: %v", err)
	}

	// Publish valid JSON with empty required fields
	emptyEnv := contracts.Envelope{Topic: "notifications.github.acme.widgets.bad"}
	emptyData, _ := json.Marshal(emptyEnv)
	_, err = env.client.JS().Publish("notifications.github.acme.widgets.bad", emptyData)
	if err != nil {
		t.Fatalf("failed to publish empty envelope: %v", err)
	}

	// Verify consumer is still alive by delivering a valid message
	publishEnvelope(env, newEnvelope("github", "notifications.github.acme.widgets.valid", "still alive", "dedupe-alive"))
	target.expectDelivery(t, deliveryWait)
	// The three publishes are handled in stream order, so the valid one's delivery already shows
	// that neither malformed message stopped the consumer; the count makes it explicit.
	env.awaitHandled(t, 3)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_ConcurrentPublish(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_concurrent", target.port, []string{"notifications.slack.*.*.concurrent"})
	env.startConsumer("test-machine")

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.concurrent", fmt.Sprintf("msg-%d", idx), fmt.Sprintf("dedupe-concurrent-%d", idx)))
		}(i)
	}
	wg.Wait()

	for range 10 {
		target.expectDelivery(t, deliveryWait)
	}
	env.awaitHandled(t, 10)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_MaxDeliverExhaustion(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.registry.Upsert(store.Interest{
		SessionID: "ses_exhaust",
		MachineID: "test-machine",
		Dir:       "/test",
	}, []string{contracts.AgentSubject("ses_exhaust")})

	// Custom consumer: short AckWait, MaxDeliver(3), always NAK (no dedupe)
	const exhaustAckWait = 300 * time.Millisecond
	attempts := &atomic.Int32{}
	attempted := make(chan struct{}, 8)
	_, err := env.client.JS().Subscribe("", func(msg *natsgo.Msg) {
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
		attempted <- struct{}{}
		msg.NakWithDelay(env.nakDelay)
	},
		natsgo.BindStream(bus.Stream),
		natsgo.ConsumerFilterSubjects(bus.StreamSubjects()...),
		natsgo.DeliverNew(),
		natsgo.AckExplicit(),
		natsgo.ManualAck(),
		natsgo.AckWait(exhaustAckWait),
		natsgo.MaxDeliver(3),
	)
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_exhaust", "exhaust test", "dedupe-exhaust"))

	// Wait for the three deliveries MaxDeliver allows, each asked for by the previous NAK.
	for attempt := range 3 {
		select {
		case <-attempted:
		case <-time.After(deliveryWait):
			t.Fatalf("the consumer made %d of 3 delivery attempts within %s", attempt, deliveryWait)
		}
	}
	if count := attempts.Load(); count != 3 {
		t.Fatalf("expected exactly 3 delivery attempts (MaxDeliver=3), got %d", count)
	}

	// Verify retries have stopped: a fourth would come one NAK delay, or one expired AckWait,
	// after the third, and this window is several of each.
	select {
	case <-attempted:
		t.Fatalf("retries should have stopped at 3, but the consumer delivered again (%d attempts)", attempts.Load())
	case <-time.After(4 * exhaustAckWait):
	}
}

// --- REGRESSION TESTS ---

func TestDelivery_NoAgentFieldInPromptAsync(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	env.registerSession("ses_noagent", target.port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_noagent", "check body", "dedupe-noagent"))

	delivery := target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)

	// Parse as generic map to detect unexpected keys
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(delivery.body), &raw); err != nil {
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
	if err := json.Unmarshal([]byte(delivery.body), &body); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if len(body.Parts) != 1 || body.Parts[0]["type"] != "text" {
		t.Fatalf("unexpected body structure: %+v", body)
	}
}

func TestDelivery_SessionWithKVAndInterestDeliveredOnce(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)

	env.registerSession("ses_both", target.port, nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_both", "double check", "dedupe-both"))

	// A second (buggy) delivery would be made by the same handler, so it is already made once
	// the message is handled.
	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_UnsubscribeEmptyTopicsDeletesAll(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)
	broadcastTopic := "notifications.slack.T888.C888.mention"
	env.registerSession("ses_emptyunsub", target.port, []string{broadcastTopic})
	env.startConsumer("test-machine")

	// Verify delivery works before unsubscribe
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "before unsub", "dedupe-emptyunsub-1"))
	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)

	// Remove with empty topics = delete the entire KV entry
	if err := env.registry.Remove(context.Background(), "ses_emptyunsub", []string{}); err != nil {
		t.Fatalf("failed to remove all interests: %v", err)
	}
	waitForMatchCount(t, env, "test-machine", broadcastTopic, 0)

	// Verify the interest entry is completely gone
	_, err := env.registry.Get(context.Background(), "ses_emptyunsub")
	if err == nil {
		t.Fatal("expected error from Get after Remove(empty), but entry still exists")
	}

	// Publish again with different dedupe key — should NOT be delivered
	publishEnvelope(env, newEnvelope("slack", broadcastTopic, "after unsub", "dedupe-emptyunsub-2"))
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_StaleSubDedupeProtectsLiveSession(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	broadcastTopic := "notifications.slack.*.*.mention"
	concreteTopic := "notifications.slack.T777.C777.mention"

	// Session A: dead port (nothing listening on port 1)
	env.registerSession("ses_stale", 1, []string{broadcastTopic})

	// Session B: live mock server
	b := newSessionRecorder(t)
	env.registerSession("ses_live_b", b.port, []string{broadcastTopic})

	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("slack", concreteTopic, "dedupe guard", "dedupe-stale"))

	// Wait for at least one NAK retry cycle: the dead session's push fails, the consumer NAKs,
	// and the redelivery is the second handling. Session B must still have exactly one delivery
	// — dedupe prevents the retry duplicate.
	b.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 2)
	b.expectNoDelivery(t, settleWindow)
}

func TestDelivery_KVFirstDeliverySkipsFile(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	target := newSessionRecorder(t)
	env.registerKVSession("ses_kv", target.port)
	env.registerInterest("ses_kv", nil)

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_kv", "kv test", "dedupe-kv-first"))

	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_BroadcastUsesKVPort(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	target := newSessionRecorder(t)
	env.registerKVSession("ses_bcast_kv", target.port)
	env.registerInterest("ses_bcast_kv", []string{"notifications.slack.*.*.mention"})

	env.startConsumer("test-machine")
	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.mention", "broadcast kv", "dedupe-bcast-kv"))

	target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

// sessionLivenessTTL is the liveness window of the two tests below, short so each waits out one
// expiry rather than a production window. The registry enforces the TTL itself, reporting an
// entry past its expiry as absent, so the wait is the bucket's and not a watcher's.
const sessionLivenessTTL = 300 * time.Millisecond

func TestDelivery_KVEntryExpiresButInterestPersists(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t, withSessionTTL(sessionLivenessTTL))

	target := newSessionRecorder(t)
	env.registerKVSession("ses_expiry_check", target.port)
	env.registerInterest("ses_expiry_check", nil)

	if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
		t.Fatalf("expected KV entry to exist: %v", err)
	}

	waitUntil(t, deliveryWait, "the session entry to expire", func() bool {
		_, err := env.sessions.Get("ses_expiry_check")
		return err != nil
	})
	interest, err := env.registry.Get(context.Background(), "ses_expiry_check")
	if err != nil {
		t.Fatalf("interest should persist after session expiry, got: %v", err)
	}
	if interest.SessionID != "ses_expiry_check" {
		t.Fatalf("wrong interest session ID: %s", interest.SessionID)
	}
}

func TestDelivery_ResumeDeliveryAfterReRegistration(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t, withSessionTTL(sessionLivenessTTL))

	a := newSessionRecorder(t)
	env.registerKVSession("ses_resume", a.port)
	env.registerInterest("ses_resume", nil)
	env.startConsumer("test-machine")

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_resume", "msg1", "dedupe-resume-1"))
	a.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)

	waitUntil(t, deliveryWait, "the session entry to expire", func() bool {
		_, err := env.sessions.Get("ses_resume")
		return err != nil
	})

	b := newSessionRecorder(t)
	env.registerKVSession("ses_resume", b.port)
	waitUntil(t, deliveryWait, "the session record to name port B", func() bool {
		entry, err := env.sessions.Get("ses_resume")
		return err == nil && entry.Port == b.port
	})

	publishEnvelope(env, newEnvelope("agent", "notifications.agent.ses_resume", "msg2", "dedupe-resume-2"))
	b.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	b.expectNoDelivery(t, settleWindow)
}

// TestE2E_CrossMachineAgentMessageACKsWithoutDelivery verifies that when an agent
// message targets a session on machine-B, machine-A's listener ACKs (does not deliver
// or NAK). This prevents wasting MaxDeliver budget on wrong-machine retries.
func TestDelivery_CrossMachineAgentMessageACKsWithoutDelivery(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	// Override deliverer machine ID to match the consumer's machine-A
	env.deliverer.MachineID = "machine-A"
	// Create a mock session server (simulates the target)
	target := newSessionRecorder(t)

	// Register the session in KV as belonging to machine-B
	if err := env.sessions.Put("ses_on_b", session.SessionEntry{
		Port:      target.port,
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

	// Machine-A should NOT have delivered (session belongs to machine-B), and it acked rather
	// than NAKing, so the one handling is the only one there is.
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

// TestE2E_CrossMachineBroadcastFilteredByMatch verifies that broadcast messages
// are only delivered to sessions on the local machine (registry.Match filters by machine_id).
func TestDelivery_CrossMachineBroadcastFilteredByMatch(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	// Override deliverer machine ID to match the consumer's machine-A
	env.deliverer.MachineID = "machine-A"
	local := newSessionRecorder(t)
	remote := newSessionRecorder(t)

	// Register local session (machine-A)
	env.registry.Upsert(store.Interest{
		SessionID: "ses_local",
		MachineID: "machine-A",
		Dir:       "/test",
	}, []string{"notifications.slack.*.*.mention"})
	if err := env.sessions.Put("ses_local", session.SessionEntry{
		Port:      local.port,
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
		Port:      remote.port,
		MachineID: "machine-B",
		Dir:       "/test",
	}); err != nil {
		t.Fatal(err)
	}

	// Consumer is machine-A
	env.startConsumer("machine-A")

	publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.mention", "broadcast", "dedupe-cross-bcast"))

	// Local session should receive the broadcast
	local.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	// Remote session should NOT receive (Match filters by machine_id)
	remote.expectNoDelivery(t, settleWindow)
	local.expectNoDelivery(t, settleWindow)
}

func TestDelivery_RegistryListReturnsSortedInterests(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.registerInterest("ses_charlie", []string{"notifications.test.>"})
	env.registerInterest("ses_alpha", []string{"notifications.test.>"})
	env.registerInterest("ses_bravo", []string{"notifications.test.>"})

	// The cache follows the bucket through its watcher: wait for the three upserts to reach it.
	waitUntil(t, deliveryWait, "the three interests to reach the cache", func() bool {
		return len(env.registry.List()) == 3
	})

	items := env.registry.List()
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

func TestDelivery_WhatsappTopicRouting(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)

	// Subscribe to all WhatsApp messages for phone 15551234567
	whatsappPattern := "notifications.whatsapp.15551234567.>"
	env.registerSession("ses_whatsapp", target.port, []string{whatsappPattern})
	env.startConsumer("test-machine")

	// Publish a WhatsApp envelope (simulating what the MCP bridge would publish)
	waTopic := contracts.WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message")
	waEnv := newEnvelope("whatsapp", waTopic, "Hello from WhatsApp", "dedupe-wa-1")
	waEnv.SourceSession = "" // Bridge has no session
	waEnv.PayloadRef = "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	publishEnvelope(env, waEnv)

	delivery := target.expectDelivery(t, deliveryWait)
	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)

	// Verify the body contains the WhatsApp message content
	if !strings.Contains(delivery.body, "Hello from WhatsApp") {
		t.Fatalf("body should contain WhatsApp message, got: %s", delivery.body)
	}
}

func TestDelivery_WhatsappDifferentPhoneNotDelivered(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	target := newSessionRecorder(t)

	// Subscribe to messages for phone 15551234567 only
	env.registerSession("ses_wa_filtered", target.port, []string{"notifications.whatsapp.15551234567.>"})
	env.startConsumer("test-machine")

	// Publish message for a DIFFERENT phone number
	waTopicOther := contracts.WhatsappSubject("15559876543", "5551234567@s.whatsapp.net", "message")
	publishEnvelope(env, newEnvelope("whatsapp", waTopicOther, "Wrong phone", "dedupe-wa-wrong"))

	env.awaitHandled(t, 1)
	target.expectNoDelivery(t, settleWindow)
}

func TestDelivery_WhatsappMultipleSubscribers(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	a := newSessionRecorder(t)
	b := newSessionRecorder(t)

	// Both sessions subscribe to WhatsApp messages
	env.registerSession("ses_wa_a", a.port, []string{"notifications.whatsapp.15551234567.>"})
	env.registerSession("ses_wa_b", b.port, []string{"notifications.whatsapp.15551234567.>"})
	env.startConsumer("test-machine")

	waTopic := contracts.WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message")
	item := newEnvelope("whatsapp", waTopic, "Broadcast WhatsApp", "dedupe-wa-broadcast")
	item.SourceSession = ""
	publishEnvelope(env, item)

	env.awaitHandled(t, 1)
	a.expectDelivery(t, deliveryWait)
	b.expectDelivery(t, deliveryWait)
	a.expectNoDelivery(t, settleWindow)
	b.expectNoDelivery(t, settleWindow)
}
