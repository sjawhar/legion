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
// it receives. It answers a frame with an empty receipt when answer reports true for the frame's
// ordinal (1 for the first), as those hosts do for a frame whose topic is not their agent subject.
func startStandIn(t *testing.T, uri, sessionID string, answer func(n int) bool) <-chan contracts.Envelope {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	frames := make(chan contracts.Envelope, 16)
	var received atomic.Int32
	subscription, err := conn.Subscribe(contracts.AgentSubject(sessionID), func(message *natsgo.Msg) {
		var frame contracts.Envelope
		if err := json.Unmarshal(message.Data, &frame); err != nil {
			t.Errorf("stand-in %s decode frame: %v", sessionID, err)
			return
		}
		frames <- frame
		if answer(int(received.Add(1))) && message.Reply != "" {
			_ = message.Respond(nil)
		}
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

func always(int) bool { return true }

func never(int) bool { return false }

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

// A holder whose first forward draws no receipt, as one whose connection is moving to another
// server, gets the same envelope again, and the sender's answer is the 200 of its receipt.
func TestPublishRouteForwardsARoleMessageAgainUntilItsHolderConfirms(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "second-attempt"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder", func(n int) bool { return n >= 2 })
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"take this","idempotency_key":"retry-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" || answer.DedupeKey != "publish.retry-1" {
		t.Fatalf("answer %s, want holder ses_holder and dedupe key publish.retry-1", recorder.Body.String())
	}
	got := expectFrames(t, frames, 2, 300*time.Millisecond)
	for i, frame := range got {
		if frame.EventID != answer.EventID || frame.DedupeKey != roleForwardDedupePrefix+"publish.retry-1" || frame.Topic != contracts.RoleTopicPrefix+role {
			t.Fatalf("frame %d: event %s dedupe key %s topic %s, want the answered event %s under %s on its role topic", i+1, frame.EventID, frame.DedupeKey, frame.Topic, answer.EventID, roleForwardDedupePrefix+"publish.retry-1")
		}
	}
	noException(t, probe)
}

// A holder that never confirms leaves the sender a 504 naming receipt_timeout once the send's
// window has passed, rather than a 200 for a message nobody took.
func TestPublishRouteAnswersReceiptTimeoutWhenTheHolderNeverConfirms(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "silent-holder"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_silent", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_silent", never)
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"anyone there","idempotency_key":"silent-1"}`)
	if recorder.Code != http.StatusGatewayTimeout || elapsed > 4500*time.Millisecond {
		t.Fatalf("status %d after %s, want 504 inside 4.5s; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var failure roleSendFailure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if failure.Reason != "receipt_timeout" || failure.Holder != "ses_silent" || failure.DedupeKey != "publish.silent-1" ||
		failure.EventID == "" || failure.Attempts != 2 || failure.Error == "" {
		t.Fatalf("answer %s, want receipt_timeout for ses_silent after two forwards of publish.silent-1", recorder.Body.String())
	}
	for _, frame := range expectFrames(t, frames, 2, 300*time.Millisecond) {
		if frame.EventID != failure.EventID {
			t.Fatalf("the holder received event %s, want only the answered %s", frame.EventID, failure.EventID)
		}
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
	startStandIn(t, uri, "ses_holder", always)
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
	frames := startStandIn(t, testnats.URL(t), "ses_aside_only", always)

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
	frames := startStandIn(t, testnats.URL(t), "ses_self", always)
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
	frames := startStandIn(t, testnats.URL(t), "ses_holder", always)
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
	if err := harness.client.PublishCoreTo(contracts.AgentSubject("ses_self_subscribed"), forwardCopy); err != nil {
		t.Fatalf("store the forward copy: %v", err)
	}
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
