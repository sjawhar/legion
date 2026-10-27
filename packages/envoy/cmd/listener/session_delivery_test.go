package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sessionConsumerRig is a listener's /v1 routes and durable handler over the in-process server,
// with a session-consumer connection of its own as main opens one, and a second view of the
// session consumers the test reads them through.
type sessionConsumerRig struct {
	harness   listenerDeliveryHarness
	deps      *listenerDeps
	mux       *http.ServeMux
	consumers *bus.SessionConsumers
}

func newSessionConsumerRig(t *testing.T) *sessionConsumerRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	return newSessionConsumerRigOn(t, client, url)
}

func newSessionConsumerRigOn(t *testing.T, client *bus.Client, url string) *sessionConsumerRig {
	t.Helper()
	harness := newListenerDeliveryHarnessOn(t, client, listenerTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected prompt_async request")
	}))
	delivery := connectSessionDelivery([]string{url}, harness.config.logger)
	t.Cleanup(delivery.close)
	if delivery.consumers == nil {
		t.Fatal("the session-consumer connection did not open")
	}
	observer, err := bus.ConnectSessionConsumers([]string{url})
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(observer.Close)
	deps := &listenerDeps{client: client, registry: harness.registry, sessions: harness.sessions, delivery: delivery}
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, "test-machine", harness.config.logger)
	return &sessionConsumerRig{harness: harness, deps: deps, mux: mux, consumers: observer}
}

// subscribeAnswer is what a test reads of a registration's answer.
type subscribeAnswer struct {
	raw      string
	Delivery *struct {
		Mode     string `json:"mode"`
		Subject  string `json:"subject"`
		StreamID string `json:"stream_id"`
		Gap      *bool  `json:"gap"`
		Reason   string `json:"reason"`
	} `json:"delivery"`
}

func (rig *sessionConsumerRig) post(t *testing.T, path, body string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s %s: status %d: %s", path, body, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

func (rig *sessionConsumerRig) subscribe(t *testing.T, body string) subscribeAnswer {
	t.Helper()
	answer := subscribeAnswer{raw: rig.post(t, "/v1/interests/subscribe", body)}
	if err := json.Unmarshal([]byte(answer.raw), &answer); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.raw, err)
	}
	return answer
}

func (rig *sessionConsumerRig) streamID(t *testing.T) string {
	t.Helper()
	id, err := rig.consumers.StreamID(context.Background())
	if err != nil {
		t.Fatalf("read the stream id: %v", err)
	}
	return id
}

// filters is the filter set of sessionID's consumer, sorted.
func (rig *sessionConsumerRig) filters(t *testing.T, sessionID string) []string {
	t.Helper()
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer of %s: %v", sessionID, err)
	}
	filters := slices.Clone(info.Config.FilterSubjects)
	if info.Config.FilterSubject != "" {
		filters = append(filters, info.Config.FilterSubject)
	}
	slices.Sort(filters)
	return filters
}

// awaitPending waits until sessionID's consumer has want messages pending, and fails the test
// when it does not within 2 s.
func (rig *sessionConsumerRig) awaitPending(t *testing.T, sessionID string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		info, err := rig.consumers.Info(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("read the consumer of %s: %v", sessionID, err)
		}
		if info.NumPending == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the consumer of %s has %d messages pending, want %d (start %d, delivered %d, filters %v)",
				sessionID, info.NumPending, want, info.Config.OptStartSeq, info.Delivered.Stream, info.Config.FilterSubjects)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// store stores an envelope on subject and returns its stream sequence.
func (rig *sessionConsumerRig) store(t *testing.T, subject string) uint64 {
	t.Helper()
	ack, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, listenerTestEnvelope(subject, "session-consumer-"+subject)))
	if err != nil {
		t.Fatalf("store a message on %s: %v", subject, err)
	}
	return ack.Sequence
}

func (rig *sessionConsumerRig) entry(t *testing.T, sessionID string) session.SessionEntry {
	t.Helper()
	entry, err := rig.harness.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read the session entry of %s: %v", sessionID, err)
	}
	return entry
}

func TestSessionConsumerRegistrationAnswersConsumerModeAndFiltersOnStoredFollowedTopics(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_consumer"

	answer := rig.subscribe(t, `{"session_id":"ses_consumer","self_subscribed":true,
		"topics":["notifications.dispatch.issue.A.>","notifications.team.deploy"],
		"delivery":{"consumer":true,"epoch":"epoch-1"}}`)

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	if answer.Delivery.Subject != "notifications.envoy.deliver."+sessionID {
		t.Fatalf("relay subject %q, want notifications.envoy.deliver.%s", answer.Delivery.Subject, sessionID)
	}
	if answer.Delivery.StreamID == "" || answer.Delivery.StreamID != rig.streamID(t) {
		t.Fatalf("stream id %q, want the stream's %q", answer.Delivery.StreamID, rig.streamID(t))
	}
	if answer.Delivery.Gap == nil || *answer.Delivery.Gap {
		t.Fatalf("answer %s, want gap false", answer.raw)
	}
	want := []string{"notifications.agent." + sessionID, "notifications.dispatch.issue.A.>"}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v (the team topic is not stored)", got, want)
	}
	if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer || entry.Epoch != "epoch-1" {
		t.Fatalf("session entry %+v, want consumer delivery at epoch-1", entry)
	}
}

func TestSessionConsumerFollowAddedBySecondRegistrationIsInTheFilterWhenItAnswers(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_follow"
	rig.subscribe(t, `{"session_id":"ses_follow","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	answer := rig.subscribe(t, `{"session_id":"ses_follow","self_subscribed":true,
		"topics":["notifications.dispatch.issue.B.>"],"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.store(t, "notifications.dispatch.issue.B.comment")
	rig.store(t, "notifications.dispatch.issue.C.comment")

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	rig.awaitPending(t, sessionID, 1)
}

func TestSessionConsumerOverlappingFollowsAreAccepted(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_overlap"

	answer := rig.subscribe(t, `{"session_id":"ses_overlap","self_subscribed":true,
		"topics":["notifications.dispatch.issue.A.>","notifications.dispatch.>"],"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.store(t, "notifications.dispatch.issue.A.comment")

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	want := []string{"notifications.agent." + sessionID, "notifications.dispatch.>"}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v", got, want)
	}
	rig.awaitPending(t, sessionID, 1)
}

func TestSessionConsumerResumePointOfTheCurrentStreamSeedsTheNewConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	// A resume point names a sequence the session wrote; 0 names none, so the first stream
	// sequence goes to a message of another session's.
	rig.store(t, contracts.AgentSubject("ses_bystander"))
	resumed := rig.store(t, contracts.AgentSubject("ses_resumed"))
	foreign := rig.store(t, contracts.AgentSubject("ses_foreign"))

	current := rig.subscribe(t, `{"session_id":"ses_resumed","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e","resume":{"stream_id":"`+rig.streamID(t)+`","seq":`+jsonUint(resumed-1)+`}}}`)
	other := rig.subscribe(t, `{"session_id":"ses_foreign","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e","resume":{"stream_id":"another-stream","seq":`+jsonUint(foreign-1)+`}}}`)

	if current.Delivery == nil || current.Delivery.Mode != "consumer" || other.Delivery == nil || other.Delivery.Mode != "consumer" {
		t.Fatalf("answers %s and %s, want delivery mode consumer", current.raw, other.raw)
	}
	// The earlier message to the resumed session is pending: its consumer starts after the resume
	// point. A resume point of another stream starts the consumer at the stream's end.
	rig.awaitPending(t, "ses_resumed", 1)
	rig.awaitPending(t, "ses_foreign", 0)
}

func jsonUint(n uint64) string {
	encoded, _ := json.Marshal(n)
	return string(encoded)
}

// A session on a serving port gets a consumer at registration with no delivery field, since its
// plugin (OpenCode's) asks nothing; its answer says so.
func TestSessionConsumerServingPortRegistrationGetsAConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_serving"

	answer := rig.subscribe(t, `{"session_id":"ses_serving","port":4096,"host":"127.0.0.1","topics":["notifications.github.acme.repo.pr.1"]}`)

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" || answer.Delivery.Subject != bus.DeliverSubject(sessionID) {
		t.Fatalf("answer %s, want delivery mode consumer on the session's relay subject", answer.raw)
	}
	want := []string{"notifications.agent." + sessionID, "notifications.github.acme.repo.pr.1"}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v", got, want)
	}
}

func TestSessionConsumerInvalidSessionIDAnswersCore(t *testing.T) {
	rig := newSessionConsumerRig(t)

	answer := rig.subscribe(t, `{"session_id":"ses.dotted","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	if answer.Delivery == nil || answer.Delivery.Mode != "core" || answer.Delivery.Reason != "invalid_session_id" {
		t.Fatalf("answer %s, want delivery core with reason invalid_session_id", answer.raw)
	}
	if entry := rig.entry(t, "ses.dotted"); entry.Delivery != "" {
		t.Fatalf("session entry %+v, want no consumer delivery", entry)
	}
}

func TestSessionConsumerRegistrationWithoutDeliveryAnswersWithoutIt(t *testing.T) {
	rig := newSessionConsumerRig(t)

	answer := rig.subscribe(t, `{"session_id":"ses_plain","self_subscribed":true,"topics":["notifications.dispatch.issue.A.>"]}`)

	if strings.Contains(answer.raw, `"delivery"`) {
		t.Fatalf("answer %s, want no delivery field", answer.raw)
	}
	if _, err := rig.consumers.Info(context.Background(), "ses_plain"); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("consumer of ses_plain: %v, want none", err)
	}
	if entry := rig.entry(t, "ses_plain"); entry.Delivery != "" || entry.Epoch != "" {
		t.Fatalf("session entry %+v, want no consumer delivery", entry)
	}
}

func TestSessionConsumerUnsubscribeShrinksTheFilter(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_shrink"
	rig.subscribe(t, `{"session_id":"ses_shrink","self_subscribed":true,
		"topics":["notifications.dispatch.issue.A.>","notifications.dispatch.issue.B.>"],"delivery":{"consumer":true,"epoch":"e"}}`)

	rig.post(t, "/v1/interests/unsubscribe", `{"session_id":"ses_shrink","topics":["notifications.dispatch.issue.B.>"]}`)

	want := []string{"notifications.agent." + sessionID, "notifications.dispatch.issue.A.>"}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters after the unsubscribe %v, want %v", got, want)
	}
}

func TestSessionConsumerRoleClaimKeepsConsumerDelivery(t *testing.T) {
	rig := newSessionConsumerRig(t)
	rig.subscribe(t, `{"session_id":"ses_claimant","topics":[],"delivery":{"consumer":true,"epoch":"epoch-7"}}`)

	rig.post(t, "/v1/roles/set", `{"session_id":"ses_claimant","role":"session-consumer-claim"}`)

	entry := rig.entry(t, "ses_claimant")
	if entry.Delivery != session.DeliveryConsumer || entry.Epoch != "epoch-7" || !entry.SelfSubscribed || entry.MachineID != "test-machine" {
		t.Fatalf("session entry after the role claim %+v, want consumer delivery at epoch-7, self-subscribed, on test-machine", entry)
	}
}

// recordingRelay records what the session-consumer manager tells the relay.
type recordingRelay struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingRelay) registered(sessionID string, epochChanged bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := sessionID + " same epoch"
	if epochChanged {
		call = sessionID + " new epoch"
	}
	r.calls = append(r.calls, call)
}

func TestSessionConsumerRegistrationTellsTheRelayWhenTheEpochChanges(t *testing.T) {
	rig := newSessionConsumerRig(t)
	relay := &recordingRelay{}
	rig.deps.delivery.relay = relay
	body := func(epoch string) string {
		return `{"session_id":"ses_epoch","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"` + epoch + `"}}`
	}

	rig.subscribe(t, body("first"))
	rig.subscribe(t, body("first"))
	rig.subscribe(t, body("second"))
	rig.subscribe(t, `{"session_id":"ses_epoch_core","self_subscribed":true,"topics":[]}`)

	want := []string{"ses_epoch new epoch", "ses_epoch same epoch", "ses_epoch new epoch"}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if !slices.Equal(relay.calls, want) {
		t.Fatalf("relay heard %v, want %v", relay.calls, want)
	}
}

// subscribeExceptionLanes subscribes over core NATS to every exception lane, through conn.
func subscribeExceptionLanes(t *testing.T, conn *natsgo.Conn) *natsgo.Subscription {
	t.Helper()
	exceptions, err := conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exception lanes: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	return exceptions
}

func requireNoException(t *testing.T, exceptions *natsgo.Subscription, what string) {
	t.Helper()
	if msg, err := exceptions.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("%s raised an exception on %s: %s", what, msg.Subject, msg.Data)
	}
}

func deliveredMetrics(t *testing.T, harness listenerDeliveryHarness) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	harness.metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return recorder.Body.String()
}

// A consumer-delivery session's stored messages wait in its consumer, so the durable path
// acknowledges them with nothing pushed and nothing reported, on its agent subject and on a followed
// topic alike. The same session on the core path, neither self-subscribed nor on a serving port, has
// its message reported as undeliverable, which shows the exception lanes are heard.
func TestSessionConsumerMessageRaisesNoExceptionOnTheDurablePath(t *testing.T) {
	rig := newSessionConsumerRig(t)
	exceptions := subscribeExceptionLanes(t, rig.harness.client.Conn)
	rig.subscribe(t, `{"session_id":"ses_core_only","topics":[]}`)
	rig.harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(contracts.AgentSubject("ses_core_only"), "core-only"))})
	if _, err := exceptions.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("the core-path session's undeliverable message raised no exception: %v", err)
	}

	rig.subscribe(t, `{"session_id":"ses_relayed","topics":["notifications.dispatch.issue.R.>"],"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(contracts.AgentSubject("ses_relayed"), "relayed-agent"))})
	rig.harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope("notifications.dispatch.issue.R.comment", "relayed-topic"))})

	requireNoException(t, exceptions, "a consumer-delivery session's message")
	if body := deliveredMetrics(t, rig.harness); !strings.Contains(body, `test_messages_delivered{delivery_status="skipped"} 2`) {
		t.Fatalf("delivery metrics %s, want two skipped", body)
	}
}

// listenerGrantBeforeSessionConsumers is the listener's grant as a hub carries it before session
// consumers (infra/core ListenerPermissions without their entries) for machine test-machine.
func listenerGrantBeforeSessionConsumers() []string {
	durable := bus.ListenerDurable("test-machine")
	allow := []string{
		bus.NotificationTopicPrefix + ">",
		"$JS.API.STREAM.INFO." + bus.Stream,
		"$JS.API.STREAM.CREATE." + bus.Stream,
		"$JS.API.STREAM.UPDATE." + bus.Stream,
		"$JS.API.STREAM.MSG.GET." + bus.Stream,
		"$JS.API.CONSUMER.INFO." + bus.Stream + "." + durable,
		"$JS.API.CONSUMER.CREATE." + bus.Stream + "." + durable,
		"$JS.ACK." + bus.Stream + "." + durable + ".>",
		"$JS.API.INFO",
	}
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.CIStateBucket} {
		stream := "KV_" + bucket
		allow = append(allow,
			"$JS.API.STREAM.INFO."+stream,
			"$JS.API.STREAM.CREATE."+stream,
			"$JS.API.DIRECT.GET."+stream+".>",
			"$JS.API.STREAM.MSG.GET."+stream,
			"$KV."+bucket+".>",
			"$JS.API.CONSUMER.CREATE."+stream+".>",
			"$JS.API.CONSUMER.DELETE."+stream+".>",
			"$JS.FC."+stream+".>",
		)
	}
	return append(allow, "$JS.API.STREAM.PURGE.KV_"+bus.InterestBucket)
}

// On a hub whose grant predates session consumers, a registration that asks for one answers core
// for want of the grant, logged once however many sessions ask, and the session's messages reach it
// as before: over its core subscription, with the durable acknowledging the stored copy as skipped.
func TestSessionConsumerWithoutTheGrantAnswersCoreAndDeliversAsBefore(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, listenerGrantBeforeSessionConsumers()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, grant.URL)
	standIn, err := bus.Connect([]string{grant.URL}, bus.WithName("session-stand-in"))
	if err != nil {
		t.Fatalf("connect the session stand-in: %v", err)
	}
	t.Cleanup(standIn.Close)
	inbox, err := standIn.Conn.SubscribeSync(contracts.AgentSubject("ses_ungranted"))
	if err != nil {
		t.Fatalf("subscribe the session stand-in: %v", err)
	}
	exceptions := subscribeExceptionLanes(t, standIn.Conn)

	first := rig.subscribe(t, `{"session_id":"ses_ungranted","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)
	second := rig.subscribe(t, `{"session_id":"ses_ungranted_too","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	for _, answer := range []subscribeAnswer{first, second} {
		if answer.Delivery == nil || answer.Delivery.Mode != "core" || answer.Delivery.Reason != "no_grant" {
			t.Fatalf("answer %s, want delivery core with reason no_grant", answer.raw)
		}
	}
	if lines := strings.Count(rig.harness.logs.String(), "sessions stay on core delivery until the grant carries them"); lines != 1 {
		t.Fatalf("the missing grant was logged %d times, want once:\n%s", lines, rig.harness.logs.String())
	}
	if entry := rig.entry(t, "ses_ungranted"); entry.Delivery != "" {
		t.Fatalf("session entry %+v, want no consumer delivery", entry)
	}

	item := listenerTestEnvelope(contracts.AgentSubject("ses_ungranted"), "ungranted")
	data := marshalListenerEnvelope(t, item)
	if _, err := client.JS().Publish(item.Topic, data); err != nil {
		t.Fatalf("store the message: %v", err)
	}
	if _, err := inbox.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("the session's core subscription did not receive its message: %v", err)
	}
	rig.harness.handler(&natsgo.Msg{Data: data})
	requireNoException(t, exceptions, "the core-path session's message")
	if body := deliveredMetrics(t, rig.harness); !strings.Contains(body, `test_messages_delivered{delivery_status="skipped"} 1`) {
		t.Fatalf("delivery metrics %s, want the stored copy skipped", body)
	}
}
