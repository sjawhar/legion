package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testEnv is what a listener and the sessions registered through it share, over the test's own
// NATS server: the client a producer publishes through, the interest registry a fan-out reads, and
// the session registry a delivery resolves its target in.
type testEnv struct {
	t        *testing.T
	url      string
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
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

	return &testEnv{
		t:        t,
		url:      uri,
		client:   client,
		registry: registry,
		sessions: sessions,
	}
}

// sessionStandIn is a session's own end of a delivery: a registered session that takes its messages
// off NATS subscriptions it holds itself, as every session on the core path does (cmd/listener's
// sessionDelivery.register: a registration that asks for no consumer "takes its messages off its
// own NATS subscription"). Its entry records SelfSubscribed, so a listener hands such a session
// nothing of its own - session.Resolve answers skipped - and every message counted here is one that
// arrived on a subject the session subscribed to, as it arrives in production.
type sessionStandIn struct {
	sessionID string
	received  chan contracts.Envelope
}

// registerSession registers sessionID as a live self-subscribed session of this machine following
// topics - the session entry and the interest row a registration writes - and subscribes it, on a
// NATS connection of its own, to each subject such a session reads its messages from.
func (env *testEnv) registerSession(sessionID string, topics []string) *sessionStandIn {
	env.t.Helper()
	standIn := &sessionStandIn{sessionID: sessionID, received: make(chan contracts.Envelope, 64)}
	conn := testnats.Connect(env.t, env.url)
	env.t.Cleanup(conn.Close)
	for _, subject := range standInSubjects(sessionID, topics) {
		if _, err := conn.Subscribe(subject, standIn.take(env.t)); err != nil {
			env.t.Fatalf("subscribe the session %s to %s: %v", sessionID, subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		env.t.Fatalf("flush the subscriptions of the session %s: %v", sessionID, err)
	}
	env.registerKVSession(sessionID)
	env.registerInterest(sessionID, topics)
	return standIn
}

// standInSubjects is what a self-subscribed session following topics holds a subscription on: its
// own agent subject and each of those topics.
func standInSubjects(sessionID string, topics []string) []string {
	return append([]string{contracts.AgentSubject(sessionID)}, topics...)
}

// take hands the test every envelope the session's subscriptions receive.
func (s *sessionStandIn) take(t *testing.T) natsgo.MsgHandler {
	return func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("the session %s cannot decode what arrived on %s: %v", s.sessionID, msg.Subject, err)
			return
		}
		select {
		case s.received <- item:
		default:
			t.Errorf("the session %s received more messages than its test read", s.sessionID)
		}
	}
}

// receives returns the next envelope the session received, failing the test when none arrives
// within the bound.
func (s *sessionStandIn) receives(t *testing.T, within time.Duration) contracts.Envelope {
	t.Helper()
	select {
	case item := <-s.received:
		return item
	case <-time.After(within):
		t.Fatalf("no message reached the session %s within %s", s.sessionID, within)
		return contracts.Envelope{}
	}
}

// receivesNothing fails the test when a message reaches the session within the window.
func (s *sessionStandIn) receivesNothing(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case item := <-s.received:
		t.Fatalf("the session %s received %s on %s within %s, want nothing", s.sessionID, item.EventID, item.Topic, window)
	case <-time.After(window):
	}
}

// registerInterest subscribes interest in the KV store (envoy_interests bucket).
func (env *testEnv) registerInterest(sessionID string, topics []string) {
	env.t.Helper()
	if _, err := env.registry.Upsert(store.Interest{
		SessionID: sessionID,
		MachineID: "test-machine",
		Dir:       "/test",
	}, standInSubjects(sessionID, topics)); err != nil {
		env.t.Fatalf("register the interest of the session %s: %v", sessionID, err)
	}
}

// registerKVSession writes a session-liveness entry to the envoy_sessions KV bucket.
func (env *testEnv) registerKVSession(sessionID string) {
	env.t.Helper()
	if err := env.sessions.Put(sessionID, session.SessionEntry{
		SelfSubscribed: true,
		MachineID:      "test-machine",
		Dir:            "/test",
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

// --- TESTS ---

// A message published to a session's own agent subject reaches that session, whole, and reaches it
// once.
func TestAMessageToASessionsAgentSubjectReachesItOnce(t *testing.T) {
	env := setupTestEnv(t)
	target := env.registerSession("ses_target", nil)

	item := newEnvelope("agent", contracts.AgentSubject("ses_target"), "hello", "dedupe-agent-subject")
	publishEnvelope(env, item)

	got := target.receives(t, 5*time.Second)
	if got.EventID != item.EventID || got.Topic != item.Topic || got.PayloadSummary != "hello" {
		t.Fatalf("the session received %+v, want the published envelope", got)
	}
	if got.SourceSession != item.SourceSession || got.TraceID != item.TraceID {
		t.Fatalf("the session received sender %q and trace %q, want %q and %q", got.SourceSession, got.TraceID, item.SourceSession, item.TraceID)
	}
	target.receivesNothing(t, 300*time.Millisecond)
}

// A message on a topic a session follows reaches every session that follows it, once each, and no
// session that follows another topic. The interest rows the fan-out reads name those sessions and
// no other.
func TestAMessageOnAFollowedTopicReachesEverySessionFollowingItAndNoOther(t *testing.T) {
	env := setupTestEnv(t)
	const mention = "notifications.slack.*.*.mention"
	followers := []*sessionStandIn{
		env.registerSession("ses_one", []string{mention}),
		env.registerSession("ses_two", []string{mention}),
	}
	elsewhere := env.registerSession("ses_three", []string{"notifications.slack.*.*.reaction"})

	topic := "notifications.slack.T123.C456.mention"
	waitForMatchCount(t, env, "test-machine", topic, len(followers))
	item := newEnvelope("slack", topic, "hey", "dedupe-followed-topic")
	publishEnvelope(env, item)

	for _, follower := range followers {
		got := follower.receives(t, 5*time.Second)
		if got.EventID != item.EventID || got.PayloadSummary != "hey" {
			t.Fatalf("the session %s received %+v, want the published envelope", follower.sessionID, got)
		}
		follower.receivesNothing(t, 300*time.Millisecond)
	}
	elsewhere.receivesNothing(t, 300*time.Millisecond)
}

// Every message of a burst published at once reaches the session following their topic: none is
// lost between the producers and the subscription the session reads.
func TestEveryMessageOfAConcurrentBurstReachesTheSessionFollowingItsTopic(t *testing.T) {
	env := setupTestEnv(t)
	const burst = 10
	target := env.registerSession("ses_concurrent", []string{"notifications.slack.*.*.concurrent"})

	var wg sync.WaitGroup
	for i := range burst {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			publishEnvelope(env, newEnvelope("slack", "notifications.slack.T1.C1.concurrent", fmt.Sprintf("msg-%d", idx), fmt.Sprintf("dedupe-concurrent-%d", idx)))
		}(i)
	}
	wg.Wait()

	received := map[string]bool{}
	for len(received) < burst {
		received[target.receives(t, 10*time.Second).PayloadSummary] = true
	}
	for i := range burst {
		if summary := fmt.Sprintf("msg-%d", i); !received[summary] {
			t.Fatalf("the session never received %s; it received %v", summary, received)
		}
	}
}

// A WhatsApp message carries the phone it is for in its subject (contracts.WhatsappSubject), so it
// reaches every session following that phone and no session following another.
func TestAWhatsappMessageReachesEverySessionFollowingThatPhoneAndNoOther(t *testing.T) {
	env := setupTestEnv(t)
	followers := []*sessionStandIn{
		env.registerSession("ses_wa_a", []string{"notifications.whatsapp.15551234567.>"}),
		env.registerSession("ses_wa_b", []string{"notifications.whatsapp.15551234567.>"}),
	}
	otherPhone := env.registerSession("ses_wa_other", []string{"notifications.whatsapp.15559876543.>"})

	item := newEnvelope("whatsapp", contracts.WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message"), "Hello from WhatsApp", "dedupe-whatsapp")
	item.SourceSession = "" // the bridge holds no session
	item.PayloadRef = "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	publishEnvelope(env, item)

	for _, follower := range followers {
		got := follower.receives(t, 5*time.Second)
		if got.EventID != item.EventID || got.PayloadRef != item.PayloadRef {
			t.Fatalf("the session %s received %+v, want the bridge's envelope", follower.sessionID, got)
		}
		follower.receivesNothing(t, 300*time.Millisecond)
	}
	otherPhone.receivesNothing(t, 500*time.Millisecond)
}

func containsTopic(topics []string, want string) bool {
	for _, topic := range topics {
		if topic == want {
			return true
		}
	}
	return false
}

func TestRoleTransfer_ExactlyOneHolder(t *testing.T) {
	env := setupTestEnv(t)
	env.registerSession("ses_role_a", nil)
	env.registerSession("ses_role_b", nil)

	if _, err := env.registry.SetRole("ses_role_a", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for A failed: %v", err)
	}
	if _, err := env.registry.SetRole("ses_role_b", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for B failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, errA := env.registry.Get(context.Background(), "ses_role_a")
		b, errB := env.registry.Get(context.Background(), "ses_role_b")
		matches := env.registry.Match("test-machine", "notifications.role.legion-controller")
		if errA == nil && errB == nil && !containsTopic(a.Topics, "notifications.role.legion-controller") && containsTopic(b.Topics, "notifications.role.legion-controller") && len(matches) == 1 && matches[0].SessionID == "ses_role_b" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	a, err := env.registry.Get(context.Background(), "ses_role_a")
	if err != nil {
		t.Fatalf("Get A failed: %v", err)
	}
	b, err := env.registry.Get(context.Background(), "ses_role_b")
	if err != nil {
		t.Fatalf("Get B failed: %v", err)
	}
	matches := env.registry.Match("test-machine", "notifications.role.legion-controller")
	t.Fatalf("timed out waiting for role transfer: A=%v B=%v matches=%v", a.Topics, b.Topics, matches)
}

func TestDelivery_KVEntryExpiresButInterestPersists(t *testing.T) {
	env := setupTestEnv(t, withSessionTTL(2*time.Second))

	env.registerKVSession("ses_expiry_check")
	env.registerInterest("ses_expiry_check", nil)

	if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
		t.Fatalf("expected KV entry to exist: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
			interest, err := env.registry.Get(context.Background(), "ses_expiry_check")
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

func TestDelivery_RegistryListReturnsSortedInterests(t *testing.T) {
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
