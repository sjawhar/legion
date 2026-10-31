package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sjawhar/envoy/internal/id"
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
	delivery, err := connectSessionDelivery([]string{url}, harness.config.logger)
	if err != nil {
		t.Fatalf("the session-consumer connection did not open: %v", err)
	}
	t.Cleanup(delivery.close)
	observer, err := bus.ConnectSessionConsumers([]string{url})
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(observer.Close)
	delivery.relay = &recordingRelay{}
	deps := &listenerDeps{client: client, registry: harness.registry, sessions: harness.sessions, consumers: delivery}
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
	} `json:"delivery"`
}

// subscribeRefusal is what a test reads of a registration the listener refused: its status and
// the body naming why (deliveryRefusal).
type subscribeRefusal struct {
	raw    string
	status int
	Error  string `json:"error"`
	Reason string `json:"reason"`
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

// refusedSubscribe posts a registration the listener is expected to refuse and returns its answer;
// a registration it took fails the test.
func (rig *sessionConsumerRig) refusedSubscribe(t *testing.T, body string) subscribeRefusal {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
	refusal := subscribeRefusal{raw: recorder.Body.String(), status: recorder.Code}
	if recorder.Code == http.StatusOK {
		t.Fatalf("POST /v1/interests/subscribe %s was taken: %s", body, refusal.raw)
	}
	if err := json.Unmarshal([]byte(refusal.raw), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", refusal.raw, err)
	}
	return refusal
}

// requireRefused fails the test unless the refusal is status with reason.
func (r subscribeRefusal) requireRefused(t *testing.T, status int, reason string) {
	t.Helper()
	if r.status != status || r.Reason != reason {
		t.Fatalf("refusal %d %s, want %d with reason %s", r.status, r.raw, status, reason)
	}
	if r.Error == "" {
		t.Fatalf("refusal %s carries no error sentence", r.raw)
	}
}

// requireNothingWritten fails the test unless sessionID has neither an interest row nor a session
// entry: a refused registration writes neither.
func (rig *sessionConsumerRig) requireNothingWritten(t *testing.T, sessionID string) {
	t.Helper()
	if interest, err := rig.harness.registry.Get(context.Background(), sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("interest row of %s: %+v (%v), want none", sessionID, interest, err)
	}
	if entry, err := rig.harness.sessions.Get(sessionID); err == nil {
		t.Fatalf("session entry of %s: %+v, want none", sessionID, entry)
	}
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
	want := []string{"notifications.agent." + sessionID, "notifications.dispatch.issue.A.>", "notifications.team.deploy"}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v (the stream stores a team topic, so the session's consumer carries it)", got, want)
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

// A Legion controller's notices reach it through its own consumer: the registration of the
// session holding legion-<project>-controller carries notifications.legion.<project>.controller
// into the consumer's filter, so a notice published while the controller was away between
// registrations is pending for it when it comes back, and another project's lane is not. Until
// INFRA-771 that lane reached the session as a live core subscription alone, which handed it
// nothing published while it was gone.
func TestSessionConsumerFiltersOnTheControllerNoticeLaneOfTheRoleItHolds(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_controller"
		role      = "legion-ACME-controller"
		lane      = "notifications.legion.ACME.controller"
	)
	claimRoles(t, rig.harness.registry, sessionID, role)
	awaitRoles(t, rig.harness.registry, sessionID, role)
	registration := `{"session_id":"ses_controller","self_subscribed":true,
		"topics":["notifications.legion.ACME.controller"],"delivery":{"consumer":true,"epoch":"e"}}`
	rig.subscribe(t, registration)

	want := []string{contracts.AgentSubject(sessionID), lane}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v: the lane the controller registered must be in them", got, want)
	}

	// The controller is away; a notice and another project's are published; it registers again.
	rig.store(t, lane)
	rig.store(t, "notifications.legion.OTHER.controller")
	answer := rig.subscribe(t, registration)
	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	rig.awaitPending(t, sessionID, 1)
}

// A heartbeat that leaves the lane out of its body keeps it in the consumer's filter and in the
// stored row alike: one read of the session's roles, for the body's topics and the row's
// together, decides both (store.Registry.PlanInterest, UpsertPlanned). A plugin sends such a
// heartbeat whenever its registration carries only what the session subscribed to in this
// process, and the lane dropped from a filter takes every notice published after it with it.
func TestSessionConsumerKeepsTheControllerNoticeLaneAHeartbeatOmits(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_controller_heartbeat"
		role      = "legion-ACME-controller"
		lane      = "notifications.legion.ACME.controller"
	)
	claimRoles(t, rig.harness.registry, sessionID, role)
	awaitRoles(t, rig.harness.registry, sessionID, role)
	rig.subscribe(t, `{"session_id":"ses_controller_heartbeat","self_subscribed":true,
		"topics":["notifications.legion.ACME.controller"],"delivery":{"consumer":true,"epoch":"e"}}`)

	rig.subscribe(t, `{"session_id":"ses_controller_heartbeat","self_subscribed":true,
		"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	stored, err := rig.harness.registry.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the stored interest row: %v", err)
	}
	if !slices.Contains(stored.Topics, lane) {
		t.Fatalf("stored topics %v, want the lane %s the holder registered", stored.Topics, lane)
	}
	// The row holds the session's own agent subject, which sessionConsumerFilter names anyway,
	// and the consumer keeps each subject once (bus.SessionConsumers).
	want := slices.Compact(slices.Sorted(slices.Values(sessionConsumerFilter(sessionID, stored.Topics))))
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v: the filter and the stored row must name one set after a heartbeat that omits the lane", got, want)
	}
	// A notice published after that heartbeat is the consumer's to deliver.
	rig.store(t, lane)
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

// A session id no consumer can be named after is refused for good: no later registration of that
// id can be given one, so the refusal is 409 and nothing is written for it.
func TestSessionConsumerInvalidSessionIDIsRefused(t *testing.T) {
	rig := newSessionConsumerRig(t)

	refusal := rig.refusedSubscribe(t, `{"session_id":"ses.dotted","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	refusal.requireRefused(t, http.StatusConflict, "invalid_session_id")
	rig.requireNothingWritten(t, "ses.dotted")
}

// A registration carrying no delivery field is a plugin older than consumer delivery. Nothing
// would deliver to it, so it is refused 409 and leaves no row, no entry and no consumer: the
// plugin learns it must ask for a consumer instead of waiting on a subscription of its own.
func TestSessionConsumerRegistrationWithoutDeliveryIsRefused(t *testing.T) {
	rig := newSessionConsumerRig(t)

	refusal := rig.refusedSubscribe(t, `{"session_id":"ses_plain","self_subscribed":true,"topics":["notifications.dispatch.issue.A.>"]}`)

	refusal.requireRefused(t, http.StatusConflict, "not_requested")
	if _, err := rig.consumers.Info(context.Background(), "ses_plain"); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("consumer of ses_plain: %v, want none", err)
	}
	rig.requireNothingWritten(t, "ses_plain")
}

// A registration that carries delivery and asks for no consumer is refused the same way.
func TestSessionConsumerRegistrationAskingForNoConsumerIsRefused(t *testing.T) {
	rig := newSessionConsumerRig(t)

	refusal := rig.refusedSubscribe(t, `{"session_id":"ses_no_ask","self_subscribed":true,"topics":[],"delivery":{"consumer":false,"epoch":"e"}}`)

	refusal.requireRefused(t, http.StatusConflict, "not_requested")
	rig.requireNothingWritten(t, "ses_no_ask")
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
	rig.deps.consumers.relay = relay
	body := func(epoch string) string {
		return `{"session_id":"ses_epoch","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"` + epoch + `"}}`
	}

	rig.subscribe(t, body("first"))
	rig.subscribe(t, body("first"))
	rig.subscribe(t, body("second"))
	// A registration the listener refuses tells the relay nothing: it holds no consumer to relay.
	rig.refusedSubscribe(t, `{"session_id":"ses_epoch_refused","self_subscribed":true,"topics":[]}`)

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
// acknowledges them with nothing pushed and nothing reported, on its agent subject and on a
// followed topic alike. An entry the previous build left for a session on the core path, portless
// and on no consumer, is unreachable: its stored message is reported as undeliverable, which is
// the honest answer and shows the exception lanes are heard.
func TestSessionConsumerMessageRaisesNoExceptionOnTheDurablePath(t *testing.T) {
	rig := newSessionConsumerRig(t)
	exceptions := subscribeExceptionLanes(t, rig.harness.client.Conn)
	if err := rig.harness.sessions.Put("ses_core_only", session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"}); err != nil {
		t.Fatalf("write the entry of a session the previous build left on the core path: %v", err)
	}
	rig.harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(contracts.AgentSubject("ses_core_only"), "core-only"))})
	if _, err := exceptions.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("the unreachable session's message raised no exception: %v", err)
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

// On a hub whose grant predates session consumers, a registration that asks for one is refused
// 503 no_grant, logged once however many sessions ask, and writes nothing: no interest row and no
// session entry, so the listener neither fans a message out to a session it cannot reach nor
// reports one as live. The grant is the negative control: the same registration on a hub that
// carries session consumers is answered consumer (the tests above).
func TestSessionConsumerWithoutTheGrantIsRefusedAndWritesNothing(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, listenerGrantBeforeSessionConsumers()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, grant.URL)

	first := rig.refusedSubscribe(t, `{"session_id":"ses_ungranted","self_subscribed":true,"topics":["notifications.dispatch.issue.U.>"],"delivery":{"consumer":true,"epoch":"e"}}`)
	second := rig.refusedSubscribe(t, `{"session_id":"ses_ungranted_too","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)

	for _, refusal := range []subscribeRefusal{first, second} {
		refusal.requireRefused(t, http.StatusServiceUnavailable, "no_grant")
	}
	if lines := strings.Count(rig.harness.logs.String(), "every registration is refused until the grant carries them"); lines != 1 {
		t.Fatalf("the missing grant was logged %d times, want once:\n%s", lines, rig.harness.logs.String())
	}
	rig.requireNothingWritten(t, "ses_ungranted")
	rig.requireNothingWritten(t, "ses_ungranted_too")
}

// A portless session, which every Oh My Pi and Claude Code session is, is delivered its stored
// message by its relay and by nothing else: both frames arrive on the session's relay subject,
// and the durable path, reading the same copies off the stream, pushes nothing, raises no
// exception and acknowledges both as skipped.
func TestSessionConsumerPortlessSessionIsDeliveredByItsRelayAlone(t *testing.T) {
	rig := newRelayRig(t)
	sessionID := "ses_portless_" + id.New()
	agentSubject := contracts.AgentSubject(sessionID)
	topic := "notifications.dispatch.issue.PORTLESS-" + id.New() + ".comment"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	exceptions := subscribeExceptionLanes(t, rig.harness.client.Conn)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", topic))
	// The rig's one client holds one subscription, so the durable of this test reads the agent
	// subject off the stream, bound before the message is stored as the listener's own durable
	// is, and the followed topic's copy is handed to the same handler below.
	bindTestDurable(t, rig, "listener-portless-"+id.New(), agentSubject)

	agent, _ := rig.storeEvent(t, agentSubject)
	followed, _ := rig.storeEvent(t, topic)
	rig.harness.handler(&natsgo.Msg{Subject: topic, Data: marshalListenerEnvelope(t, followed)})

	requireFrames(t, standIn.collect(t, 2, 5*time.Second), agent, followed)
	awaitSkipped(t, rig.harness, 2)
	requireNoException(t, exceptions, "a relayed session's stored message")
	standIn.none(t, time.Second)
}

// awaitSkipped waits until the durable path has acknowledged want stored messages with nothing
// pushed and nothing reported, and fails the test when it has not within 5 s.
func awaitSkipped(t *testing.T, harness listenerDeliveryHarness, want int) {
	t.Helper()
	line := fmt.Sprintf(`test_messages_delivered{delivery_status="skipped"} %d`, want)
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := deliveredMetrics(t, harness)
		if strings.Contains(body, line) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery metrics %s, want %s", body, line)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
