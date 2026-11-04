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

<<<<<<< conflict 1 of 4
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
// A live holder answers no receipt, so its forward counts as delivered once the stream stored it:
// the sender is answered 200 after the one forward, and the holder is not sent the envelope again.
func TestPublishRouteAnswersStoredOnceTheHoldersForwardIsKept(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "stored-forward"
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 // A holder whose first forward draws no receipt, as one whose connection is moving to another
 // server, gets the same envelope again, and the sender's answer is the 200 of its receipt.
 func TestPublishRouteForwardsARoleMessageAgainUntilItsHolderConfirms(t *testing.T) {
-	harness := newListenerDeliveryHarness(t, nil)
+	harness := newListenerDeliveryHarness(t)
 	const role = "second-attempt"
>>>>>>> conflict 1 of 4 ends
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

<<<<<<< conflict 2 of 4
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
// A holder whose session holds no NATS connection - its process is down, or it reads its messages
// from its own JetStream consumer - is answered 200 all the same: the stream holds the forward on
// the holder's agent subject, where the holder's consumer delivers it with acknowledgement and
// redelivery. Nothing answers a receipt there, and the send waits for none.
func TestPublishRouteStoresTheForwardForAHolderThatIsNotConnected(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "offline-holder"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_offline", role, nil)
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 // A holder that never confirms leaves the sender a 504 naming receipt_timeout once the send's
 // window has passed, rather than a 200 for a message nobody took.
 func TestPublishRouteAnswersReceiptTimeoutWhenTheHolderNeverConfirms(t *testing.T) {
-	harness := newListenerDeliveryHarness(t, nil)
+	harness := newListenerDeliveryHarness(t)
 	const role = "silent-holder"
 	claimRoleFor(t, harness.registry, harness.sessions, "ses_silent", role, nil)
 	frames := startStandIn(t, testnats.URL(t), "ses_silent", never)
>>>>>>> conflict 2 of 4 ends
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

// The publish route names its role forward with the forward's own Nats-Msg-Id (bus.ForwardMsgID
// of the forward's dedupe key and the holder's agent subject), so the stream, which captures
// agent subjects, keeps one copy of the message for its holder however often the route sends it.
// Two sends of one idempotency key through two routes - a second listener, which holds no record
// of the first one's confirmation - forward the envelope to the holder twice, and the stream
// stores that one copy.
func TestPublishRouteStoresOneCopyOfARoleForwardSentTwice(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const role = "stored-once"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_one_copy", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_one_copy")
	probe := exceptionProbe(t, harness.client)
	body := `{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"twice over","idempotency_key":"stored-once-1"}`

	for send, deps := range []*listenerDeps{harness.publishDeps(t), harness.publishDeps(t)} {
		recorder, elapsed := publishThrough(deps, body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("send %d: status %d after %s, want 200; body %s", send+1, recorder.Code, elapsed, recorder.Body.String())
		}
	}
	// Both sends forwarded, so the stream took the message twice and stored it under one name.
	for i, frame := range expectFrames(t, frames, 2, 300*time.Millisecond) {
		if frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-once-1" {
			t.Fatalf("forward %d: dedupe key %s, want %s", i+1, frame.DedupeKey, roleForwardDedupePrefix+"publish.stored-once-1")
		}
	}
	subject := contracts.AgentSubject("ses_one_copy")
	if count := storedForwards(t, harness.client, subject); count != 1 {
		t.Fatalf("the stream holds %d messages on %s after two forwards of publish.stored-once-1, want the one copy their Nats-Msg-Id keeps", count, subject)
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
	harness := newListenerDeliveryHarness(t)
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
	harness := newListenerDeliveryHarness(t)
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
	harness := newListenerDeliveryHarness(t)
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

<<<<<<< conflict 3 of 4
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 // An ad-hoc topic message is confirmed with every self-subscribed session registered for the
 // topic, on any machine; the sender is told which one did not confirm, and the one that did took
 // the forward once. A session served on a port is not asked for a receipt.
 func TestPublishRouteNamesTheAdHocTopicSubscriberThatDidNotConfirm(t *testing.T) {
-	harness := newListenerDeliveryHarness(t, nil)
+	harness := newListenerDeliveryHarness(t)
 	const topic = "notifications.team.confirm-squad"
 	register := func(sessionID string, entry session.SessionEntry) {
 		t.Helper()
 		if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: entry.MachineID}, []string{"notifications.team.>"}); err != nil {
 			t.Fatalf("register %s's interest: %v", sessionID, err)
 		}
 		if err := harness.sessions.Put(sessionID, entry); err != nil {
 			t.Fatalf("register %s: %v", sessionID, err)
 		}
 	}
 	register("ses_confirming", session.SessionEntry{MachineID: "other-machine", SelfSubscribed: true})
 	register("ses_silent", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
-	register("ses_served", session.SessionEntry{MachineID: "test-machine", Port: 1})
+	// A session whose entry records no way of receiving at all is no recipient: it is sent
+	// nothing and named nowhere in the answer.
+	register("ses_no_way", session.SessionEntry{MachineID: "test-machine"})
 	confirming := startStandIn(t, testnats.URL(t), "ses_confirming", always)
 	startStandIn(t, testnats.URL(t), "ses_silent", never)
-	served := startStandIn(t, testnats.URL(t), "ses_served", always)
+	noWay := startStandIn(t, testnats.URL(t), "ses_no_way", always)
 
 	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+topic+`","message":"deploy done","idempotency_key":"squad-1"}`)
 	if recorder.Code != http.StatusGatewayTimeout || elapsed > 4500*time.Millisecond {
 		t.Fatalf("status %d after %s, want 504 inside 4.5s; body %s", recorder.Code, elapsed, recorder.Body.String())
 	}
 	var failure topicSendFailure
 	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
 		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
 	}
 	if failure.Reason != "not_confirmed" || failure.DedupeKey != "publish.squad-1" || failure.EventID == "" ||
 		len(failure.Delivered) != 1 || failure.Delivered[0] != "ses_confirming" ||
 		len(failure.Undelivered) != 1 || failure.Undelivered[0] != (undeliveredRecipient{SessionID: "ses_silent", Reason: "receipt_timeout"}) {
 		t.Fatalf("answer %s, want ses_confirming delivered and ses_silent undelivered by receipt_timeout", recorder.Body.String())
 	}
 	frame := expectFrames(t, confirming, 1, 300*time.Millisecond)[0]
 	if frame.EventID != failure.EventID || frame.Topic != topic || frame.DedupeKey != "publish.squad-1" {
 		t.Fatalf("ses_confirming received event %s topic %s dedupe key %s, want the answered event unchanged", frame.EventID, frame.Topic, frame.DedupeKey)
 	}
-	expectFrames(t, served, 0, 100*time.Millisecond)
-}
-
-// With the live topics lane on, the lane receives the route's own publish on the topic and its
-// fanout records, in the delivery handlers' dedupe cache, every self-subscribed session on this
-// machine whose push it skipped: such a session holds its own subscription, so there is nothing to
-// push to, and nothing was heard from it. A recipient that never sends its receipt must still be
-// named as not confirming, rather than counted delivered from that record.
-func TestPublishRouteDoesNotCountTheLiveLanesSkippedPushAsAConfirmation(t *testing.T) {
-	harness := newListenerDeliveryHarness(t, nil)
-	live, err := openLiveTopics([]string{testnats.URL(t)}, harness.config.machineID, liveDeliveryHandler(harness.config))
-	if err != nil {
-		t.Fatalf("open the live topics lane: %v", err)
-	}
-	t.Cleanup(live.Close)
-	const topic = "notifications.team.live-lane-silent"
-	if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_silent", MachineID: harness.config.machineID}, []string{topic}); err != nil {
-		t.Fatalf("register ses_silent's interest: %v", err)
-	}
-	if err := harness.sessions.Put("ses_silent", session.SessionEntry{MachineID: harness.config.machineID, SelfSubscribed: true}); err != nil {
-		t.Fatalf("register ses_silent: %v", err)
-	}
-	frames := startStandIn(t, testnats.URL(t), "ses_silent", never)
-
-	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+topic+`","message":"anyone there","idempotency_key":"lane-1"}`)
-	if recorder.Code != http.StatusGatewayTimeout || elapsed > 4500*time.Millisecond {
-		t.Fatalf("status %d after %s, want 504 inside 4.5s; body %s", recorder.Code, elapsed, recorder.Body.String())
-	}
-	var failure topicSendFailure
-	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
-		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
-	}
-	if failure.Reason != "not_confirmed" || failure.DedupeKey != "publish.lane-1" || len(failure.Delivered) != 0 ||
-		len(failure.Undelivered) != 1 || failure.Undelivered[0] != (undeliveredRecipient{SessionID: "ses_silent", Reason: "receipt_timeout"}) {
-		t.Fatalf("answer %s, want ses_silent undelivered by receipt_timeout and nobody delivered", recorder.Body.String())
-	}
-	for _, frame := range expectFrames(t, frames, 2, 300*time.Millisecond) {
-		if frame.EventID != failure.EventID {
-			t.Fatalf("ses_silent received event %s, want only the answered %s", frame.EventID, failure.EventID)
-		}
-	}
-	// The lane did run for this send: without its record the test would not tell the fix apart.
-	if !harness.config.dedupeCache.Seen("publish.lane-1", "ses_silent") {
-		t.Fatalf("the live lane did not record ses_silent's skipped push of publish.lane-1, so the test exercised nothing")
-	}
+	expectFrames(t, noWay, 0, 100*time.Millisecond)
 }
 
 // An ad-hoc topic whose own publish did not reach NATS is answered 504 publish_failed, saying the
 // topic was not published and listing no recipient, whether the window ran out while the bus
 // waited for a reconnect or the publish failed at once on a closed connection: the status does not
 // depend on how soon the failure came, and a topic with no recipient is not answered as if a
 // session had failed to confirm it.
 func TestPublishRouteAnswersPublishFailedForAnAdHocTopicItCouldNotPublish(t *testing.T) {
 	const topic = "notifications.team.unpublished"
 	cases := []struct {
 		name   string
 		deps   func(t *testing.T) *listenerDeps
 		cause  string
 		within time.Duration
 	}{
 		{
 			name: "the hub stays down for the whole window",
 			deps: func(t *testing.T) *listenerDeps {
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
 				deps := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
 				srv.Stop(t)
 				waitFor(t, 5*time.Second, "the listener's connection to be reconnecting", client.Conn.IsReconnecting)
 				return deps
 			},
 			cause:  "bus: waiting for NATS to reconnect: context deadline exceeded",
 			within: 4500 * time.Millisecond,
 		},
 		{
 			name: "the listener's connection is closed",
 			deps: func(t *testing.T) *listenerDeps {
-				deps := newListenerDeliveryHarness(t, nil).publishDeps(t)
+				deps := newListenerDeliveryHarness(t).publishDeps(t)
 				closed, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("closed-publisher"))
 				if err != nil {
 					t.Fatalf("connect: %v", err)
 				}
 				closed.Close()
 				deps.client = closed
 				return deps
 			},
 			cause:  "bus: client is stopped",
 			within: time.Second,
 		},
 	}
 	for _, tc := range cases {
 		t.Run(tc.name, func(t *testing.T) {
 			deps := tc.deps(t)
 			recorder, elapsed := publishThrough(deps, `{"topic":"`+topic+`","message":"through no hub","idempotency_key":"unpublished-1"}`)
 			if recorder.Code != http.StatusGatewayTimeout || elapsed > tc.within {
 				t.Fatalf("status %d after %s, want 504 inside %s; body %s", recorder.Code, elapsed, tc.within, recorder.Body.String())
 			}
 			var failure topicPublishFailure
 			if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil || failure.EventID == "" {
 				t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
 			}
 			want := `{"error":"` + topic + ` was not published within 4s: ` + tc.cause + `; no session was sent it",` +
 				`"reason":"publish_failed","event_id":"` + failure.EventID + `","dedupe_key":"publish.unpublished-1"}` + "\n"
 			if body := recorder.Body.String(); body != want {
 				t.Fatalf("body %q, want %q", body, want)
 			}
 		})
 	}
 }
 
 // An ad-hoc topic send that forwarded nothing, because its own publish did not reach NATS or NATS
 // refused it, counts each self-subscribed recipient it did not reach failed under
 // messagesDelivered, as a send answered not_confirmed counts each recipient that did not confirm;
 // with no recipient it counts nothing.
 func TestPublishRouteCountsEachRecipientOfAnAdHocTopicItForwardedNothingTo(t *testing.T) {
 	cases := []struct {
 		name       string
 		recipients []string
 		closed     bool
 		message    string
 		status     int
 		want       string
 	}{
 		{
 			name:       "the publish did not reach NATS",
 			recipients: []string{"ses_first", "ses_second"},
 			closed:     true,
 			message:    "through no hub",
 			status:     http.StatusGatewayTimeout,
 			want:       `test_messages_delivered{delivery_status="failed"} 2`,
 		},
 		{
 			name:       "NATS refused the publish",
 			recipients: []string{"ses_first", "ses_second"},
 			message:    strings.Repeat("m", 2<<20),
 			status:     http.StatusRequestEntityTooLarge,
 			want:       `test_messages_delivered{delivery_status="failed"} 2`,
 		},
 		{
 			name:    "the publish did not reach NATS and nobody is subscribed",
 			closed:  true,
 			message: "through no hub",
 			status:  http.StatusGatewayTimeout,
 		},
 	}
 	for _, tc := range cases {
 		t.Run(tc.name, func(t *testing.T) {
-			harness := newListenerDeliveryHarness(t, nil)
+			harness := newListenerDeliveryHarness(t)
 			for _, sessionID := range tc.recipients {
 				if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-machine"}, []string{"notifications.team.>"}); err != nil {
 					t.Fatalf("register %s's interest: %v", sessionID, err)
 				}
 				if err := harness.sessions.Put(sessionID, session.SessionEntry{MachineID: "other-machine", SelfSubscribed: true}); err != nil {
 					t.Fatalf("register %s: %v", sessionID, err)
 				}
 			}
 			deps := harness.publishDeps(t)
 			if tc.closed {
 				closed, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("closed-publisher"))
 				if err != nil {
 					t.Fatalf("connect: %v", err)
 				}
 				closed.Close()
 				deps.client = closed
 			}
 			body, err := json.Marshal(map[string]string{"topic": "notifications.team.unsent", "message": tc.message, "idempotency_key": "unsent-1"})
 			if err != nil {
 				t.Fatalf("marshal the request: %v", err)
 			}
 			recorder, _ := publishThrough(deps, string(body))
 			if recorder.Code != tc.status {
 				t.Fatalf("status %d, want %d; body %.300s", recorder.Code, tc.status, recorder.Body.String())
 			}
 			scrape := httptest.NewRecorder()
 			harness.metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
 			counted := scrape.Body.String()
 			if tc.want != "" && !strings.Contains(counted, tc.want) {
 				t.Fatalf("metrics do not hold %s:\n%s", tc.want, counted)
 			}
 			if tc.want == "" && strings.Contains(counted, "test_messages_delivered{") {
 				t.Fatalf("a send with no recipient counted a delivery:\n%s", counted)
 			}
 		})
 	}
 }
 
 // An ad-hoc topic sent while the listener's connection to its server is down is answered inside
 // the send's window, as packages/envoy/AGENTS.md says of both confirmed sends, with an error rather
 // than a 200. The topic's own core publish waits on the reconnect too, so it must end with the
 // window rather than after the bus's own 5 s reconnect wait, which outlasts envoy-client's 5 s
 // attempt: the sender's client would abort before the answer arrives.
 func TestPublishRouteAnswersAnAdHocTopicInsideItsWindowWhileTheServerIsDown(t *testing.T) {
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
 	if _, err := registry.Upsert(store.Interest{SessionID: "ses_recipient", MachineID: "other-machine"}, []string{"notifications.team.>"}); err != nil {
 		t.Fatalf("register ses_recipient's interest: %v", err)
 	}
 	if err := sessions.Put("ses_recipient", session.SessionEntry{MachineID: "other-machine", SelfSubscribed: true}); err != nil {
 		t.Fatalf("register ses_recipient: %v", err)
 	}
 	startStandIn(t, uri, "ses_recipient", always)
 	deps := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
 
 	srv.Stop(t)
 	// Publish once the client has seen the drop and is reconnecting, as a listener whose server
 	// went away is, rather than in the moment before it notices.
 	waitFor(t, 5*time.Second, "the listener's connection to be reconnecting", client.Conn.IsReconnecting)
 	recorder, elapsed := publishThrough(deps, `{"topic":"notifications.team.outage","message":"through a stopped hub","idempotency_key":"outage-1"}`)
 	if recorder.Code == http.StatusOK || elapsed > 4500*time.Millisecond {
 		t.Fatalf("status %d after %s, want an error inside 4.5s; body %s", recorder.Code, elapsed, recorder.Body.String())
 	}
 }
 
>>>>>>> conflict 3 of 4 ends
// A forward copy the stream stored on a session's agent subject reaches every listener's durable;
// the durable acknowledges it and delivers it to nobody, so a session subscribed to the copy's
// topic is not given the message a second time.
func TestDurableAcknowledgesAStoredForwardCopyAndDeliversNothing(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const topic = "notifications.team.stored-copy"
	harness.registerFanoutTarget(t, "ses_subscriber", topic, nil)
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
	if harness.config.dedupeCache.Seen(forwardCopy.DedupeKey, "ses_subscriber") {
		t.Fatalf("the durable delivered the stored copy to the topic's subscriber, which is sent nothing for it")
	}
}
<<<<<<< conflict 4 of 4
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 
 // A role message the publish route forwards twice, because its holder's first receipt never came,
 // is stored once on the holder's agent subject: each forward carries the same Nats-Msg-Id
 // (bus.ForwardMsgID of the forward's dedupe key and the subject), which the route chooses itself
 // since RequestCoreTo takes its MsgId from its caller, so the stream holds one copy per message
 // and recipient, not one per attempt; the durable acknowledges a stored forward copy and delivers
 // nothing.
 func TestPublishRouteStoresOneCopyOfARoleMessageItForwardsTwice(t *testing.T) {
-	harness := newListenerDeliveryHarness(t, nil)
+	harness := newListenerDeliveryHarness(t)
 	const role = "stored-once"
 	const holder = "ses_stored_once"
 	claimRoleFor(t, harness.registry, harness.sessions, holder, role, nil)
 	frames := startStandIn(t, testnats.URL(t), holder, func(n int) bool { return n >= 2 })
 
 	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"take this once","idempotency_key":"stored-once-1"}`)
 	if recorder.Code != http.StatusOK {
 		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
 	}
 	expectFrames(t, frames, 2, 300*time.Millisecond)
 	subject := contracts.AgentSubject(holder)
 	// The stream takes what one connection sends in order, so once it has acknowledged a later
 	// message, both forwards before it are stored or dropped as duplicates.
 	if _, err := harness.client.JS().Publish(subject+"_after", []byte("{}")); err != nil {
 		t.Fatalf("publish the message after the forwards: %v", err)
 	}
 	info, err := harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
 	if err != nil {
 		t.Fatalf("read the stream: %v", err)
 	}
 	if got := info.State.Subjects[subject]; got != 1 {
 		t.Fatalf("the stream holds %d copies on %s after two forwards of one role message, want 1", got, subject)
 	}
 }
>>>>>>> conflict 4 of 4 ends
