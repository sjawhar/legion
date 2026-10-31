package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// publishDeps is the harness's stores as main hands them to the /v1 routes, with the delivery
// handlers' own config and a confirmation record of the route's own, as main wires the publish
// route.
func (h listenerDeliveryHarness) publishDeps(t *testing.T) *listenerDeps {
	t.Helper()
	return &listenerDeps{
		client:    h.client,
		registry:  h.registry,
		sessions:  h.sessions,
		delivery:  &h.config,
		confirmed: newConfirmedRecord(t),
	}
}

// newConfirmedRecord is the publish route's own confirmation record as main makes it, stopped
// when the test ends.
func newConfirmedRecord(t *testing.T) *dedupe.Cache {
	t.Helper()
	confirmed := dedupe.New(10 * time.Minute)
	t.Cleanup(confirmed.Stop)
	return confirmed
}

// withPublishRoute gives d what the publish route needs beyond its stores, as main wires it: a
// delivery config over d's stores with a dedupe cache, startRoleForward(client) as the forward, a
// logger and a delivered counter, and a confirmation record.
func withPublishRoute(t *testing.T, d *listenerDeps) *listenerDeps {
	t.Helper()
	cache := dedupe.New(10 * time.Minute)
	t.Cleanup(cache.Stop)
	logs := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("publish route log:\n%s", logs.String())
		}
	})
	d.delivery = &listenerDeliveryHandlerConfig{
		client:            d.client,
		forwardRole:       startRoleForward(d.client),
		registry:          d.registry,
		sessions:          d.sessions,
		dedupeCache:       cache,
		logger:            logging.NewWithWriter("test", logs),
		messagesDelivered: metrics.New().NewCounter("test_messages_delivered", "test"),
	}
	d.confirmed = newConfirmedRecord(t)
	return d
}

// startStandIn is a session process on a NATS connection of its own to the test's server, as
// pi-envoy and claude-envoy hold one: it subscribes to its agent subject and hands over every frame
// it receives. It answers nothing, as those hosts answer nothing: a role forward counts as
// delivered once the stream acknowledged storing it (startRoleForward).
func startStandIn(t *testing.T, uri, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	frames := make(chan contracts.Envelope, 16)
	subscription, err := conn.Subscribe(contracts.AgentSubject(sessionID), func(message *natsgo.Msg) {
		var frame contracts.Envelope
		if err := json.Unmarshal(message.Data, &frame); err != nil {
			t.Errorf("stand-in %s decode frame: %v", sessionID, err)
			return
		}
		frames <- frame
	})
	if err != nil {
		t.Fatalf("stand-in %s subscribe: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("stand-in %s flush: %v", sessionID, err)
	}
	return frames
}

// claimRoleFor registers sessionID as a live self-subscribed session advertising capabilities and
// gives it role.
func claimRoleFor(t *testing.T, registry *store.Registry, sessions *session.SessionRegistry, sessionID, role string, capabilities []string) {
	t.Helper()
	if err := sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: capabilities}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	if _, err := registry.SetRole(sessionID, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, sessionID, err)
	}
}

// publishThrough sends body to the publish route and returns its answer and how long it took.
func publishThrough(d *listenerDeps, body string) (*httptest.ResponseRecorder, time.Duration) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	publishHandler(d, logging.New("test")).ServeHTTP(recorder, request)
	return recorder, time.Since(started)
}

// expectFrames reads want frames from frames, then fails on any further one inside quiet.
func expectFrames(t *testing.T, frames <-chan contracts.Envelope, want int, quiet time.Duration) []contracts.Envelope {
	t.Helper()
	var got []contracts.Envelope
	for len(got) < want {
		select {
		case frame := <-frames:
			got = append(got, frame)
		case <-time.After(5 * time.Second):
			t.Fatalf("the stand-in received %d frames, want %d", len(got), want)
		}
	}
	select {
	case frame := <-frames:
		t.Fatalf("the stand-in received frame %d (event %s, dedupe key %s), want %d", want+1, frame.EventID, frame.DedupeKey, want)
	case <-time.After(quiet):
	}
	return got
}

// noException fails when any delivery exception is published while the probe listens: the route
// answers its sender and publishes none.
func noException(t *testing.T, probe *natsgo.Subscription) {
	t.Helper()
	if _, err := probe.NextMsg(250 * time.Millisecond); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("the route published a delivery exception: the probe answered %v, want a timeout", err)
	}
}

func exceptionProbe(t *testing.T, client *bus.Client) *natsgo.Subscription {
	t.Helper()
	probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exception lanes: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}
	return probe
}

// A live holder answers no receipt, so its forward counts as delivered once the stream stored it:
// the sender is answered 200 after the one forward, and the holder is not sent the envelope again.
func TestPublishRouteAnswersStoredOnceTheHoldersForwardIsKept(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "stored-forward"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder")
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"take this","idempotency_key":"stored-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" || answer.DedupeKey != "publish.stored-1" {
		t.Fatalf("answer %s, want holder ses_holder and dedupe key publish.stored-1", recorder.Body.String())
	}
	for i, frame := range expectFrames(t, frames, 1, 300*time.Millisecond) {
		if frame.EventID != answer.EventID || frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-1" || frame.Topic != contracts.RoleTopicPrefix+role {
			t.Fatalf("frame %d: event %s dedupe key %s topic %s, want the answered event %s under %s on its role topic", i+1, frame.EventID, frame.DedupeKey, frame.Topic, answer.EventID, roleForwardDedupePrefix+"publish.stored-1")
		}
	}
	noException(t, probe)
}

// A holder whose session holds no NATS connection - its process is down, or it reads its messages
// from its own JetStream consumer - is answered 200 all the same: the stream holds the forward on
// the holder's agent subject, where the holder's consumer delivers it with acknowledgement and
// redelivery. Nothing answers a receipt there, and the send waits for none.
func TestPublishRouteStoresTheForwardForAHolderThatIsNotConnected(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "offline-holder"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_offline", role, nil)
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"while nobody listens","idempotency_key":"offline-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_offline" || answer.DedupeKey != "publish.offline-1" {
		t.Fatalf("answer %s, want holder ses_offline and dedupe key publish.offline-1", recorder.Body.String())
	}
	subject := contracts.AgentSubject("ses_offline")
	stored, err := harness.client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the stream's forward on %s: %v", subject, err)
	}
	var forwarded contracts.Envelope
	if err := json.Unmarshal(stored.Data, &forwarded); err != nil {
		t.Fatalf("decode the stored forward %s: %v", stored.Data, err)
	}
	if forwarded.EventID != answer.EventID || forwarded.DedupeKey != roleForwardDedupePrefix+"publish.offline-1" || forwarded.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the stream holds event %s dedupe key %s topic %s on %s, want the answered event %s under %s on its role topic",
			forwarded.EventID, forwarded.DedupeKey, forwarded.Topic, subject, answer.EventID, roleForwardDedupePrefix+"publish.offline-1")
	}
	if count := storedForwards(t, harness.client, subject); count != 1 {
		t.Fatalf("the stream holds %d messages on %s, want the one forward", count, subject)
	}
	noException(t, probe)
}

// A role-claim read that cannot finish, the server having stopped answering under a live
// connection, ends the send's first resolution at sendRequestBudget with the answer GET
// /v1/roles/<role> gives the same stalled read - the bus-unavailable 503 with Retry-After: 1,
// naming the read - rather than holding the sender past its window.
func TestPublishRouteAnswersBusUnavailableWhenTheClaimReadStalls(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	const role = "frozen-hub"
	claimRoleFor(t, registry, sessions, "ses_holder", role, nil)
	startStandIn(t, uri, "ses_holder")
	deps := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})

	thaw := srv.Freeze(t)
	t.Cleanup(thaw)
	recorder, elapsed := publishThrough(deps, `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"through a frozen hub"}`)
	if recorder.Code != http.StatusServiceUnavailable || elapsed < sendRequestBudget || elapsed > 4500*time.Millisecond {
		t.Fatalf("status %d after %s, want 503 at sendRequestBudget (%s) and inside 4.5s; body %s", recorder.Code, elapsed, sendRequestBudget, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, "read role "+role+"'s claim") {
		t.Fatalf("answer %s, want it to begin %q and name the claim read", recorder.Body.String(), busUnansweredPrefix)
	}
}

// A holder that does not advertise the payload's delivery mode is refused as /v1/messages/send
// refuses it, naming the holder, and is sent nothing.
func TestPublishRouteRefusesADeliveryModeTheHolderDoesNotAdvertise(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "no-steer"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_aside_only", role, []string{"aside"})
	frames := startStandIn(t, testnats.URL(t), "ses_aside_only")

	recorder, _ := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"steer now","payload":"{\"delivery\":{\"mode\":\"steer\"}}"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"error":"session ses_aside_only () does not advertise steer","reason":"capability","holder":"ses_aside_only"}`+"\n" {
		t.Fatalf("body %q", body)
	}
	expectFrames(t, frames, 0, 300*time.Millisecond)
}

// A sender that holds the role itself is answered 200 and nothing is sent, to the holder or on the
// role lane, as the role lane skips an echo.
func TestPublishRouteSendsNothingToASenderHoldingTheRole(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "self-held"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_self", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_self")
	lane, err := harness.client.Conn.SubscribeSync(contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role lane: %v", err)
	}
	t.Cleanup(func() { _ = lane.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the lane probe: %v", err)
	}

	recorder, _ := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"note to self","source_session":"ses_self"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_self" {
		t.Fatalf("answer %s (%v), want holder ses_self", recorder.Body.String(), err)
	}
	expectFrames(t, frames, 0, 300*time.Millisecond)
	if _, err := lane.NextMsg(100 * time.Millisecond); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("the send went out on the role lane: the probe answered %v, want a timeout", err)
	}
}

// A re-send under the key of a message its holder already confirmed is answered 200 and not
// forwarded again: the holder took it once.
func TestPublishRouteDoesNotForwardARepeatKeyItsHolderConfirmed(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "repeat-key"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder")
	deps := harness.publishDeps(t)
	body := `{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"once","idempotency_key":"repeat-1"}`

	first, _ := publishThrough(deps, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first send: status %d, want 200; body %s", first.Code, first.Body.String())
	}
	expectFrames(t, frames, 1, 100*time.Millisecond)
	repeat, _ := publishThrough(deps, body)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat send: status %d, want 200; body %s", repeat.Code, repeat.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(repeat.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_holder" || answer.DedupeKey != "publish.repeat-1" {
		t.Fatalf("repeat answer %s (%v), want holder ses_holder under publish.repeat-1", repeat.Body.String(), err)
	}
	expectFrames(t, frames, 0, 300*time.Millisecond)
}

// A forward copy the stream stored on a session's agent subject reaches every listener's durable;
// the durable acknowledges it and delivers it to nobody, so a serve-port session subscribed to the
// copy's topic is not sent the message a second time.
func TestDurableAcknowledgesAStoredForwardCopyAndDeliversNothing(t *testing.T) {
	var pushes atomic.Int32
	harness := newListenerDeliveryHarness(t, listenerTransport(func(*http.Request) (*http.Response, error) {
		pushes.Add(1)
		return listenerDeliveryResponse(http.StatusNoContent), nil
	}))
	const topic = "notifications.team.stored-copy"
	harness.registerFanoutTarget(t, "ses_served", topic, nil)
	consumer := "listener-forward-copy-" + id.New()
	subscription, err := startListenerSubscription(harness.client, consumer, harness.handler)
	if err != nil {
		t.Fatalf("start the durable: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })

	forwardCopy := listenerTestEnvelope(topic, "publish.stored-copy")
	publishTo(t, harness.client, contracts.AgentSubject("ses_self_subscribed"), forwardCopy)
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := harness.client.JS().ConsumerInfo(bus.Stream, consumer)
		if err != nil {
			t.Fatalf("read the durable: %v", err)
		}
		if info.Delivered.Stream >= 1 && info.AckFloor.Stream >= info.Delivered.Stream && info.NumAckPending == 0 {
			if info.Delivered.Consumer != 1 {
				t.Fatalf("the durable delivered the copy %d times, want once", info.Delivered.Consumer)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the durable did not acknowledge the stored copy: %+v", info)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := pushes.Load(); got != 0 {
		t.Fatalf("the durable pushed the copy to serve-port sessions %d times, want none", got)
	}
}
