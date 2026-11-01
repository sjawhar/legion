package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// recipientSender is the session every publish in these tests names as its source: it follows the
// topic it publishes on, so each count shows that the sender itself is never a recipient.
const recipientSender = "ses_sender"

// recipientRig is the publish route over one NATS server, with the known-session store the counts
// read and a clock of its own for it, as main wires the route with every store open.
type recipientRig struct {
	harness listenerDeliveryHarness
	deps    *listenerDeps
	known   *session.KnownSessions
	url     string
	// clock is what the known-session store stamps each record with; a test moves it.
	clock time.Time
}

func newRecipientRig(t *testing.T) *recipientRig {
	t.Helper()
	harness := newListenerDeliveryHarness(t, nil)
	waitForRegistry(t, harness.registry)
	rig := &recipientRig{harness: harness, url: testnats.URL(t), clock: time.Now()}
	known, err := session.OpenKnownSessions(harness.client.Conn,
		session.WithKnownSessionReplicas(1),
		session.WithKnownSessionClock(func() time.Time { return rig.clock }))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	deps := harness.publishDeps(t)
	deps.known = known
	rig.deps, rig.known = deps, known
	return rig
}

// register runs one registration of sessionID on topics through the real route, as a session that
// holds its own NATS connection does: that writes the interest row, the session entry and the
// known-session record. A registration that names a serving port instead would write no record
// here, since this rig wires no session-consumer connection and a port registration whose
// consumer call failed is not recorded (sessionDeliveryOutcome.recorded).
//
// An ad-hoc send confirms its message with every self-subscribed recipient (topicRecipients), so a
// session registered here and still live when a publish runs needs a stand-in to answer its
// receipt.
func (rig *recipientRig) register(t *testing.T, sessionID string, topics []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"topics":          topics,
		"self_subscribed": true,
		"title":           "a counted session",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	subscribeHandler(rig.deps, "test-machine", logging.New("test")).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("register %s: status=%d body=%s", sessionID, recorder.Code, recorder.Body.String())
	}
}

// stop ends sessionID the way a session that shuts down cleanly does, through the real route: its
// session entry goes and its known-session record stays, moved to the store's clock.
func (rig *recipientRig) stop(t *testing.T, sessionID string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	deleteSessionHandler(rig.deps.sessions, rig.known, logging.New("test")).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stop %s: status=%d body=%s", sessionID, recorder.Code, recorder.Body.String())
	}
	if _, err := rig.deps.sessions.Get(sessionID); err == nil {
		t.Fatalf("%s still holds a session entry, so it did not stop", sessionID)
	}
}

// reapInterest removes sessionID's interest row, as the reaper removes the row of a session that
// has been dead longer than the grace window plus store.InterestRefreshAge
// (store.Registry.Reap). It deletes the row through store.Registry.Remove rather than driving a
// reap pass, which would need the registry's own clock moved past that allowance: both go through
// deleteInterest, so the state they leave - no row in the bucket and none in the cache - is the
// same, and that state is what the count reads.
func (rig *recipientRig) reapInterest(t *testing.T, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.harness.registry.Remove(ctx, sessionID, nil); err != nil {
		t.Fatalf("remove %s's interest row: %v", sessionID, err)
	}
	if _, cached := rig.harness.registry.CachedInterest(sessionID); cached {
		t.Fatalf("%s still has an interest row, so it is not in the state the reaper leaves", sessionID)
	}
}

// publish sends one message on topic from recipientSender and fails on any answer but 200.
func (rig *recipientRig) publish(t *testing.T, topic, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"topic":           topic,
		"message":         "who is listening",
		"idempotency_key": key,
		"source_session":  recipientSender,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, elapsed := publishThrough(rig.deps, string(body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish on %s: status %d after %s; body %s", topic, recorder.Code, elapsed, recorder.Body.String())
	}
	return recorder
}

// assertCounts fails unless the answer carries both recipient counts, with these values.
func assertCounts(t *testing.T, recorder *httptest.ResponseRecorder, wantTotal, wantConnected int) {
	t.Helper()
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Recipients == nil || answer.RecipientsConnected == nil {
		t.Fatalf("answer %s, want recipients %d and recipients_connected %d", recorder.Body.String(), wantTotal, wantConnected)
	}
	if *answer.Recipients != wantTotal || *answer.RecipientsConnected != wantConnected {
		t.Fatalf("answer %s, want recipients %d and recipients_connected %d", recorder.Body.String(), wantTotal, wantConnected)
	}
}

// assertNoCounts fails when the answer carries either count key at all. It reads the raw JSON
// object: a decoded zero and an absent key read alike, and absent is what an older client and a
// listener with no known-session store must send.
func assertNoCounts(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	for _, key := range []string{"recipients", "recipients_connected"} {
		if raw, carried := answer[key]; carried {
			t.Fatalf("the answer carries %s: %s; want the key left out entirely, body %s", key, raw, recorder.Body.String())
		}
	}
}

// A publish on an ad-hoc topic answers with the sessions the topic is for and how many of them are
// connected: every session that follows it, whether it is registered now or stopped, counted once
// however many stores name it, and never the sender itself.
func TestAPublishCountsEverySessionTheTopicIsForOtherThanItsSender(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.team.recipient-count"
	if !bus.UsesCoreTransport(topic) {
		t.Fatalf("%s is captured by a stream subject, so this test no longer covers the core transport's branch", topic)
	}

	// The sender follows the topic too, and no count below includes it.
	rig.register(t, recipientSender, []string{topic})
	assertCounts(t, rig.publish(t, topic, "count-0"), 0, 0)

	// A session registered now: an interest row and a known-session record both name it, and it is
	// counted once. It holds its own connection, so it answers each send's receipt.
	rig.register(t, "ses_both", []string{topic})
	startStandIn(t, rig.url, "ses_both", always)
	if _, known := rig.known.Get("ses_both"); !known {
		t.Fatal("ses_both has no known-session record, so the count has only one store to dedupe")
	}
	if _, cached := rig.harness.registry.CachedInterest("ses_both"); !cached {
		t.Fatal("ses_both has no interest row, so the count has only one store to dedupe")
	}
	assertCounts(t, rig.publish(t, topic, "count-1"), 1, 1)

	// A session that registered through a listener of a release older than the known-session
	// records: an interest row and a live entry, and no record, as that listener left it. It is
	// served on a port, so this send waits for no receipt from it.
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: "ses_row_only", MachineID: "other-machine"}, []string{topic}); err != nil {
		t.Fatalf("register ses_row_only's interest: %v", err)
	}
	if err := rig.harness.sessions.Put("ses_row_only", session.SessionEntry{MachineID: "other-machine", Port: 9003}); err != nil {
		t.Fatalf("register ses_row_only: %v", err)
	}
	if record, known := rig.known.Get("ses_row_only"); known {
		t.Fatalf("ses_row_only has the known-session record %#v, so it is not the older listener's session", record)
	}

	// A session that stopped cleanly more than 20 minutes ago: its entry is gone and the reaper has
	// taken its interest row, leaving its record alone to say it follows the topic.
	rig.register(t, "ses_stopped", []string{topic})
	rig.clock = rig.clock.Add(21 * time.Minute)
	rig.stop(t, "ses_stopped")
	rig.reapInterest(t, "ses_stopped")
	if _, known := rig.known.Get("ses_stopped"); !known {
		t.Fatal("the clean stop took ses_stopped's known-session record, so nothing is left to count it by")
	}

	assertCounts(t, rig.publish(t, topic, "count-3"), 3, 2)
}

// A publish on a topic the stream captures carries the counts too: that branch publishes through
// JetStream and answers once the stream stored the message, and its sender reads the same two
// numbers as an ad-hoc topic's.
func TestAPublishOnAStreamCapturedTopicCarriesTheCounts(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.slack.channel.c-recipient-count"
	if !bus.StreamCaptures(topic) {
		t.Fatalf("no stream subject captures %s, so this test no longer covers the stream-captured branch", topic)
	}

	rig.register(t, recipientSender, []string{topic})
	rig.register(t, "ses_watcher", []string{topic})
	assertCounts(t, rig.publish(t, topic, "stream-1"), 1, 1)
}

// A role topic carries neither count: its answer names the holder the message was forwarded to,
// which is what a role send's sender reads.
func TestAPublishToARoleTopicCarriesNeitherCount(t *testing.T) {
	rig := newRecipientRig(t)
	const role = "recipient-count-holder"
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, "ses_holder", role, nil)
	startStandIn(t, rig.url, "ses_holder", always)

	recorder := rig.publish(t, contracts.RoleTopicPrefix+role, "role-1")
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" {
		t.Fatalf("answer %s, want the holder ses_holder", recorder.Body.String())
	}
	assertNoCounts(t, recorder)
}

// A listener whose NATS user may not open the known-session bucket runs without the store, and
// answers every publish with neither count rather than with the live sessions alone: its sender
// sees the answer this release's listeners sent before the counts existed.
func TestAListenerWithNoKnownSessionStoreCarriesNeitherCount(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	waitForRegistry(t, harness.registry)
	deps := harness.publishDeps(t)
	if deps.known != nil {
		t.Fatal("the deps hold a known-session store, so this test is not the degraded listener's")
	}
	if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_subscriber", MachineID: "other-machine"}, []string{"notifications.team.degraded-count"}); err != nil {
		t.Fatalf("register ses_subscriber's interest: %v", err)
	}
	if err := harness.sessions.Put("ses_subscriber", session.SessionEntry{MachineID: "other-machine", Port: 9021}); err != nil {
		t.Fatalf("register ses_subscriber: %v", err)
	}

	recorder, elapsed := publishThrough(deps, `{"topic":"notifications.team.degraded-count","message":"who is listening","idempotency_key":"degraded-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	assertNoCounts(t, recorder)
}

// Acceptance A8: the counts cost no JetStream API request. 100 publishes over warm caches send
// none, and the probe proves it is watching by seeing the one call the test makes itself.
func TestAHundredPublishesSendNoJetStreamAPIRequest(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.team.recipient-count-a8"
	rig.register(t, recipientSender, []string{topic})
	// One session the known-session records alone name, stopped so that it is counted and not
	// connected, and one the interest rows alone name, served on a port: both matchers are read on
	// every publish below, and no session holds a connection this send must take a receipt from,
	// so each of the hundred answers as soon as the topic is published.
	rig.register(t, "ses_stopped", []string{topic})
	rig.stop(t, "ses_stopped")
	rig.reapInterest(t, "ses_stopped")
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: "ses_port", MachineID: "other-machine"}, []string{topic}); err != nil {
		t.Fatalf("register ses_port's interest: %v", err)
	}
	if err := rig.harness.sessions.Put("ses_port", session.SessionEntry{MachineID: "other-machine", Port: 9041}); err != nil {
		t.Fatalf("register ses_port: %v", err)
	}
	// One publish before the probe starts, so anything a first publish would set up is already
	// set up and the window below holds the steady state.
	assertCounts(t, rig.publish(t, topic, "a8-warm"), 2, 1)

	probe := watchJetStreamAPI(t, rig.url, rig.harness.client.Conn, "$JS.API.CONSUMER.>", "$JS.API.STREAM.INFO.>")
	for i := range 100 {
		assertCounts(t, rig.publish(t, topic, "a8-"+strconv.Itoa(i)), 2, 1)
	}

	control := "$JS.API.STREAM.INFO." + bus.Stream
	if _, err := rig.harness.client.JS().StreamInfo(bus.Stream); err != nil {
		t.Fatalf("the probe's control call: %v", err)
	}
	if calls := probe.awaitCall(t, control); len(calls) > 0 {
		t.Fatalf("100 publishes sent %d JetStream API requests: %v", len(calls), calls)
	}
}

// jetStreamAPIProbe watches the JetStream API requests one connection sends. A second connection
// to the same server subscribes to the API subjects given, and each request is attributed to its
// sender by the response inbox it carries, which is one connection's alone
// (jetStreamInboxPrefix): a NATS message says nothing else about who sent it. A route that must
// answer from memory asserts the probe took none of its calls while it ran.
type jetStreamAPIProbe struct {
	mu       sync.Mutex
	observed []string
	prefix   string
}

// watchJetStreamAPI starts a probe of the JetStream API calls conn sends on subjects, over a
// connection of its own to uri, and returns it ready: its subscriptions are flushed, so every call
// conn sends after this returns is the probe's to see.
func watchJetStreamAPI(t *testing.T, uri string, conn *natsgo.Conn, subjects ...string) *jetStreamAPIProbe {
	t.Helper()
	probe := &jetStreamAPIProbe{prefix: jetStreamInboxPrefix(t, conn)}
	watcher := testnats.Connect(t, uri)
	t.Cleanup(watcher.Close)
	for _, subject := range subjects {
		subscription, err := watcher.Subscribe(subject, func(msg *natsgo.Msg) {
			if !strings.HasPrefix(msg.Reply, probe.prefix) {
				return
			}
			probe.mu.Lock()
			probe.observed = append(probe.observed, msg.Subject)
			probe.mu.Unlock()
		})
		if err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = subscription.Unsubscribe() })
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the JetStream API probe: %v", err)
	}
	return probe
}

// awaitCall waits for a call on subject, the control call its caller makes to show the probe is
// watching, and returns every call the probe took before it. The server hands one connection's
// messages on in the order it took them, so once the control call has arrived every call that
// connection sent before it has arrived too.
func (p *jetStreamAPIProbe) awaitCall(t *testing.T, subject string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		index := slices.Index(p.observed, subject)
		var before []string
		if index >= 0 {
			before = slices.Clone(p.observed[:index])
		}
		p.mu.Unlock()
		if index >= 0 {
			return before
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe did not see the control call on %s within 5s, so it is watching nothing", subject)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// jetStreamInboxPrefix is the prefix every response inbox conn puts on a request carries, which no
// other connection uses: nats.go derives one per connection (nats.Conn.NewRespInbox) and answers
// each request on it.
func jetStreamInboxPrefix(t *testing.T, conn *natsgo.Conn) string {
	t.Helper()
	inbox := conn.NewRespInbox()
	cut := strings.LastIndex(inbox, ".")
	if cut < 0 {
		t.Fatalf("the connection's response inbox %q has no prefix to watch by", inbox)
	}
	return inbox[:cut+1]
}
