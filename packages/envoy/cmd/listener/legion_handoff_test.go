package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// handoffAgent is the stopped implementer the hand-off tests address, under the Legion project
	// token infra (legionEntry).
	handoffAgent     = "ses-impl"
	handoffRoleToken = "legion-infra-infra-204-architect"
	handoffRoleTopic = contracts.RoleTopicPrefix + handoffRoleToken
)

// handoffListener is one listener over its own connection: its /v1 routes and its durable's
// delivery handler, every one wired with the Legion hand-off, and the log it writes.
type handoffListener struct {
	*legionListener
	client   *bus.Client
	registry *store.Registry
	config   listenerDeliveryHandlerConfig
	logs     *cmdtest.LockedBuffer
}

func newHandoffListener(t *testing.T, client *bus.Client, machineID string) *handoffListener {
	t.Helper()
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the session cache: %v", err)
	}
	agents, err := legionagents.Open(client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	logs := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s log:\n%s", machineID, logs.String())
		}
	})
	logger := logging.NewWithWriter(machineID, logs)
	handoff := &legionHandoff{client: client, registry: registry, sessions: sessions, agents: agents, logger: logger}
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions, legionAgents: agents, handoff: handoff}
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, machineID, logger)
	dedupeCache := dedupe.New(10 * time.Minute)
	attemptCache := dedupe.New(5 * time.Minute)
	t.Cleanup(dedupeCache.Stop)
	t.Cleanup(attemptCache.Stop)
	met := metrics.New()
	config := listenerDeliveryHandlerConfig{
		client:            client,
		forwardRole:       client.RequestCoreTo,
		registry:          registry,
		sessions:          sessions,
		machineID:         machineID,
		deliverer:         &session.Deliverer{MachineID: machineID, RequestLimit: 5 * time.Second, Sessions: sessions},
		dedupeCache:       dedupeCache,
		attemptCache:      attemptCache,
		logger:            logger,
		messagesReceived:  met.NewCounter("test_messages_received", "test"),
		messagesDelivered: met.NewCounter("test_messages_delivered", "test"),
		messagesNAKed:     met.NewCounter("test_messages_naked", "test"),
		deliveryDuration:  met.NewHistogram("test_delivery_duration", "test", metrics.DefaultBuckets),
		handoff:           handoff,
	}
	return &handoffListener{legionListener: &legionListener{deps: deps, mux: mux}, client: client, registry: registry, config: config, logs: logs}
}

// register records a Legion agent of the project infra through the registry's route.
func (l *handoffListener) register(t *testing.T, sessionID, issue, role, started string) {
	t.Helper()
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/"+sessionID, legionEntry(sessionID, issue, role, started))
}

// beat writes the project infra's answering record through its route, as the daemon writes it.
func (l *handoffListener) beat(t *testing.T, answering bool, at time.Time) {
	t.Helper()
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/projects/infra",
		fmt.Sprintf(`{"answering":%t,"updated_at":%q}`, answering, at.UTC().Format("2006-01-02T15:04:05.000Z07:00")))
}

// handoffs is how many hand-offs the stream holds for sessionID, and the last one's bytes.
func handoffs(t *testing.T, client *bus.Client, sessionID string) (uint64, []byte) {
	t.Helper()
	subject := legionHandoffSubject("infra", sessionID)
	info, err := client.JS().StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	count := info.State.Subjects[subject]
	if count == 0 {
		return 0, nil
	}
	last, err := client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the last hand-off on %s: %v", subject, err)
	}
	return count, last.Data
}

// wireHandoff is a hand-off as LegionAgentHandoffSchema and intake.Handoff read it: exactly these
// five fields.
type wireHandoff struct {
	OriginalTopic string          `json:"original_topic"`
	TargetSession string          `json:"target_session"`
	RoleToken     string          `json:"role_token"`
	Project       string          `json:"project"`
	Envelope      json.RawMessage `json:"envelope"`
}

// decodeHandoff reads data strictly as a hand-off whose envelope is a valid wire envelope.
func decodeHandoff(t *testing.T, data []byte) (wireHandoff, contracts.Envelope) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var handoff wireHandoff
	if err := decoder.Decode(&handoff); err != nil {
		t.Fatalf("decode hand-off %s: %v", data, err)
	}
	if err := contracts.ValidateWire(handoff.Envelope); err != nil {
		t.Fatalf("the hand-off's envelope is not a wire envelope: %v\n%s", err, handoff.Envelope)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(handoff.Envelope, &envelope); err != nil {
		t.Fatalf("decode the hand-off's envelope: %v", err)
	}
	return handoff, envelope
}

// assertNoHandoff fails when the stream holds a hand-off for sessionID.
func assertNoHandoff(t *testing.T, client *bus.Client, sessionID string) {
	t.Helper()
	if count, data := handoffs(t, client, sessionID); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none; the last: %s", count, sessionID, data)
	}
}

func noLiveSession(sessionID string) string {
	return fmt.Sprintf("{\"error\":\"no live session %s\"}\n", sessionID)
}

const unclaimedArchitect = "{\"error\":\"no holder for role " + handoffRoleToken + "\",\"reason\":\"unclaimed\"}\n"

// The heartbeat route stores the project's answering record and answers it; a path that is not a
// Legion project token, a body without answering or with an updated_at that is not RFC 3339 UTC,
// a body that is not JSON and a method other than PUT are refused and write nothing.
func TestLegionProjectRouteStoresTheAnsweringRecord(t *testing.T) {
	client := setupPublishTestClient(t)
	l := newHandoffListener(t, client, "test-machine")
	raw, err := client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}

	for _, refused := range []struct {
		status       int
		method, path string
		body         string
	}{
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/Infra", `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/in.fra", `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/", `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/infra", `{"updated_at":"2026-10-26T04:00:00Z"}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/infra", `{"answering":true,"updated_at":"2026-10-26T05:00:00+01:00"}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/infra", `{"answering":true}`},
		{http.StatusBadRequest, http.MethodPut, "/v1/legion/projects/infra", `not json`},
		{http.StatusMethodNotAllowed, http.MethodGet, "/v1/legion/projects/infra", ``},
	} {
		l.want(t, refused.status, refused.method, refused.path, refused.body)
	}
	if keys, err := raw.Keys(); !errors.Is(err, nats.ErrNoKeysFound) {
		t.Fatalf("the refusals wrote %v (%v)", keys, err)
	}

	body := l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/projects/infra", `{"answering":true,"updated_at":"2026-10-26T04:00:00.123Z"}`)
	if body != "{\"answering\":true,\"updated_at\":\"2026-10-26T04:00:00.123Z\"}\n" {
		t.Fatalf("PUT answered %q", body)
	}
	stored, err := raw.Get("project.infra")
	if err != nil {
		t.Fatalf("read project.infra: %v", err)
	}
	if string(stored.Value()) != `{"answering":true,"updated_at":"2026-10-26T04:00:00.123Z"}` {
		t.Fatalf("project.infra holds %s", stored.Value())
	}
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/projects/infra", `{"answering":false,"updated_at":"2026-10-26T04:01:00Z"}`)
	if record, found, err := l.deps.legionAgents.Project(context.Background(), "infra"); err != nil || !found || record.Answering {
		t.Fatalf("after a rewrite the record = %#v, %t, %v; want answering false", record, found, err)
	}
}

// A send to a stopped Legion agent whose daemon answers is handed to the daemon: 200 with the
// envelope and recipient a delivered send answers, and one hand-off on the agent's subject carrying
// that envelope, stamped with its sender as delivery would have it. A send repeated under its
// idempotency key is stored once.
func TestSendHandsOffAStoppedLegionAgent(t *testing.T) {
	client := setupPublishTestClient(t)
	l := newHandoffListener(t, client, "test-machine")
	l.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:15:00Z")
	l.beat(t, true, time.Now().Add(-30*time.Second))

	send := `{"target_session":"` + handoffAgent + `","source":"agent","source_session":"ses-asker","message":"why did you pick this design?","idempotency_key":"question-1"}`
	var answer sendResponse
	for range 2 {
		answer = decodeLegion[sendResponse](t, l.want(t, http.StatusOK, http.MethodPost, "/v1/messages/send", send))
	}
	if answer.Recipient != handoffAgent || answer.Topic != contracts.AgentSubject(handoffAgent) || answer.DedupeKey != "agent."+handoffAgent+".question-1" || answer.Duplicate {
		t.Fatalf("send answered %#v, want the envelope with recipient %s", answer, handoffAgent)
	}
	if answer.Sender == nil || answer.Sender.SessionID != "ses-asker" {
		t.Fatalf("send answered sender %#v, want ses-asker's stamp", answer.Sender)
	}

	count, data := handoffs(t, client, handoffAgent)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs, want the repeat stored once", count)
	}
	handoff, envelope := decodeHandoff(t, data)
	want := wireHandoff{OriginalTopic: contracts.AgentSubject(handoffAgent), TargetSession: handoffAgent, RoleToken: "legion-infra-infra-204-implementer", Project: "infra"}
	handoff.Envelope = nil
	if !reflect.DeepEqual(handoff, want) {
		t.Fatalf("hand-off = %#v, want %#v", handoff, want)
	}
	if envelope.DedupeKey != answer.DedupeKey || envelope.Payload != answer.Payload || envelope.PayloadSummary != "why did you pick this design?" ||
		envelope.Sender == nil || envelope.Sender.SessionID != "ses-asker" || envelope.Source != "agent" {
		t.Fatalf("handed-off envelope = %#v, want the one the send answered (%#v)", envelope, answer.Envelope)
	}
	if !strings.Contains(l.logs.String(), "listener handed a message to its legion daemon") {
		t.Fatalf("the hand-off was not logged:\n%s", l.logs.String())
	}
}

// A role publish with no live holder whose role token the registry names is handed to the role's
// newest session: 200 with the envelope and recipient, whether no session claims the role or its
// holder has lapsed.
func TestPublishHandsOffAStoppedLegionRole(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim string
	}{
		{name: "unclaimed"},
		{name: "holder lapsed", claim: "ses-arch-gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			l := newHandoffListener(t, client, "test-machine")
			l.register(t, "ses-arch-old", "INFRA-204", "architect", "2026-10-25T03:15:00Z")
			l.register(t, "ses-arch-new", "INFRA-204", "architect", "2026-10-26T03:15:00Z")
			l.beat(t, true, time.Now())
			if tc.claim != "" {
				if _, err := l.registry.SetRole(tc.claim, "test-machine", handoffRoleToken, false); err != nil {
					t.Fatalf("claim the role: %v", err)
				}
			}

			answer := decodeLegion[map[string]any](t, l.want(t, http.StatusOK, http.MethodPost, "/v1/messages/publish",
				`{"topic":"`+handoffRoleTopic+`","source":"dispatch","message":"a question for the architect","payload":"{\"event\":{\"type\":\"message.created\"}}"}`))
			if answer["recipient"] != "ses-arch-new" || answer["topic"] != handoffRoleTopic || answer["holder"] != nil {
				t.Fatalf("publish answered %v, want the envelope with recipient ses-arch-new and no holder", answer)
			}
			assertNoHandoff(t, client, "ses-arch-old")
			count, data := handoffs(t, client, "ses-arch-new")
			if count != 1 {
				t.Fatalf("the stream holds %d hand-offs for the newest architect, want 1", count)
			}
			handoff, envelope := decodeHandoff(t, data)
			if handoff.OriginalTopic != handoffRoleTopic || handoff.TargetSession != "ses-arch-new" || handoff.RoleToken != handoffRoleToken || handoff.Project != "infra" {
				t.Fatalf("hand-off = %#v", handoff)
			}
			if envelope.EventID != answer["event_id"] || envelope.Topic != handoffRoleTopic || envelope.Payload != `{"event":{"type":"message.created"}}` || envelope.Source != "dispatch" {
				t.Fatalf("handed-off envelope = %#v, want the one the publish answered (%v)", envelope, answer)
			}
		})
	}
}

// Every send and role publish the hand-off does not take answers byte for byte as it did before
// hand-off, and nothing is handed off: a target the registry does not name, a project whose
// answering record is missing, stale, says it does not answer, or is dated ahead, a sender that
// is envoy, a body carrying "handoff": false, and a session live on another listener that this
// listener's session cache has not seen.
func TestSendAndPublishAnswerAsBeforeWhenNotHandedOff(t *testing.T) {
	fresh := func(l *handoffListener, t *testing.T) { l.beat(t, true, time.Now()) }
	for _, tc := range []struct {
		name    string
		prepare func(l *handoffListener, t *testing.T)
		source  string
		extra   string
	}{
		{name: "unregistered target", prepare: func(l *handoffListener, t *testing.T) { l.beat(t, true, time.Now()) }},
		{name: "no answering record", prepare: func(l *handoffListener, t *testing.T) { l.registerBoth(t) }},
		{name: "stale answering record", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, true, time.Now().Add(-4*time.Minute))
		}},
		{name: "answering false", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, false, time.Now())
		}},
		{name: "answering record dated ahead", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, true, time.Now().Add(10*time.Minute))
		}},
		{name: "source envoy", source: "envoy", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			fresh(l, t)
		}},
		{name: "handoff false", extra: `,"handoff":false`, prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			fresh(l, t)
		}},
		{name: "live on another listener", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			fresh(l, t)
			// This listener's cache stops before the other listener registers the sessions, so
			// only a read through to the session bucket finds them.
			l.deps.sessions.StopWatch()
			other, err := session.OpenSessionRegistry(l.client.Conn, session.WithSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the other listener's session registry: %v", err)
			}
			for _, id := range []string{handoffAgent, "ses-arch-new"} {
				if err := other.Put(id, session.SessionEntry{Port: 1, MachineID: "other-machine"}); err != nil {
					t.Fatalf("register %s on the other listener: %v", id, err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			l := newHandoffListener(t, client, "test-machine")
			tc.prepare(l, t)
			source := tc.source
			if source == "" {
				source = "agent"
			}

			sent := l.do(t, http.MethodPost, "/v1/messages/send",
				`{"target_session":"`+handoffAgent+`","source":"`+source+`","message":"are you there?"`+tc.extra+`}`)
			if sent.Code != http.StatusNotFound || sent.Body.String() != noLiveSession(handoffAgent) {
				t.Fatalf("send = %d %q, want 404 %q", sent.Code, sent.Body.String(), noLiveSession(handoffAgent))
			}
			published := l.do(t, http.MethodPost, "/v1/messages/publish",
				`{"topic":"`+handoffRoleTopic+`","source":"`+source+`","message":"are you there?"`+tc.extra+`}`)
			if published.Code != http.StatusNotFound || published.Body.String() != unclaimedArchitect {
				t.Fatalf("publish = %d %q, want 404 %q", published.Code, published.Body.String(), unclaimedArchitect)
			}
			assertNoHandoff(t, client, handoffAgent)
			assertNoHandoff(t, client, "ses-arch-new")
		})
	}
}

// registerBoth records the implementer the sends address and the architect the role publishes do.
func (l *handoffListener) registerBoth(t *testing.T) {
	t.Helper()
	l.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:15:00Z")
	l.register(t, "ses-arch-new", "INFRA-204", "architect", "2026-10-26T03:15:00Z")
}

// A role publish on a role that is not a Legion workflow role's token answers as before.
func TestPublishToANonLegionRoleIsNotHandedOff(t *testing.T) {
	l := newHandoffListener(t, setupPublishTestClient(t), "test-machine")
	l.beat(t, true, time.Now())
	published := l.do(t, http.MethodPost, "/v1/messages/publish", `{"topic":"notifications.role.unheld","message":"anyone?"}`)
	if want := "{\"error\":\"no holder for role unheld\",\"reason\":\"unclaimed\"}\n"; published.Code != http.StatusNotFound || published.Body.String() != want {
		t.Fatalf("publish = %d %q, want 404 %q", published.Code, published.Body.String(), want)
	}
}

// An agent-subject message on the bus that no session takes is handed off by every listener whose
// durable reads it, and the stream stores one hand-off: the copies collapse on their MsgId. No
// no_holder exception is published, and neither listener mistakes the retained hand-off it then
// reads for an envelope.
func TestAgentSubjectHandOffFromTwoListenersIsStoredOnce(t *testing.T) {
	uri := testnats.URL(t)
	connect := func(owning bool) *bus.Client {
		dial := bus.Connect
		if owning {
			dial = bus.ConnectOwningStream
		}
		client, err := dial([]string{uri}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		return client
	}
	first := newHandoffListener(t, connect(true), "machine-a")
	second := newHandoffListener(t, connect(false), "machine-b")
	first.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:15:00Z")
	first.beat(t, true, time.Now())

	probe, err := first.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.AgentSubject(handoffAgent))
	if err != nil {
		t.Fatalf("subscribe exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	for i, l := range []*handoffListener{first, second} {
		sub, err := startListenerSubscription(l.client, fmt.Sprintf("listener-handoff-%d", i), jetStreamDeliveryHandler(l.config))
		if err != nil {
			t.Fatalf("start listener %d's durable: %v", i, err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	item := listenerTestEnvelope(contracts.AgentSubject(handoffAgent), "dispatch-evt-handoff-1")
	item.Source = "dispatch"
	item.Payload = `{"event":{"type":"comment.created"},"delivery":{"mode":"aside"}}`
	if err := first.client.Publish(item); err != nil {
		t.Fatalf("publish the agent message: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for _, l := range []*handoffListener{first, second} {
		for !strings.Contains(l.logs.String(), "listener agent handed off to its legion daemon") {
			if time.Now().After(deadline) {
				t.Fatalf("a listener did not hand the message off")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	count, data := handoffs(t, first.client, handoffAgent)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs, want the two listeners' copies stored once", count)
	}
	handoff, envelope := decodeHandoff(t, data)
	if handoff.OriginalTopic != item.Topic || handoff.TargetSession != handoffAgent || handoff.RoleToken != "legion-infra-infra-204-implementer" || handoff.Project != "infra" {
		t.Fatalf("hand-off = %#v", handoff)
	}
	if !reflect.DeepEqual(envelope, item) {
		t.Fatalf("handed-off envelope = %#v, want the bus message %#v", envelope, item)
	}
	if exception, err := probe.NextMsg(300 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("a handed-off message published an exception: %v %v", exception, err)
	}
	// Each durable also reads the retained hand-off, which it acknowledges without decoding.
	time.Sleep(200 * time.Millisecond)
	for _, l := range []*handoffListener{first, second} {
		if logs := l.logs.String(); strings.Contains(logs, "listener invalid envelope") || strings.Contains(logs, "listener decode failed") {
			t.Fatalf("a listener read the hand-off as an envelope:\n%s", logs)
		}
	}
}

// An agent-subject message on the bus the hand-off does not take is reported as before: a no_holder
// exception for an unregistered session, a stale answering record and a sender that is envoy,
// and nothing at all for a session another listener owns.
func TestAgentSubjectAnswersAsBeforeWhenNotHandedOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prepare   func(l *handoffListener, t *testing.T)
		source    string
		exception bool
	}{
		{name: "unregistered", exception: true, prepare: func(l *handoffListener, t *testing.T) { l.beat(t, true, time.Now()) }},
		{name: "stale answering record", exception: true, prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, true, time.Now().Add(-4*time.Minute))
		}},
		{name: "source envoy", source: "envoy", exception: true, prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, true, time.Now())
		}},
		{name: "owned by another listener", prepare: func(l *handoffListener, t *testing.T) {
			l.registerBoth(t)
			l.beat(t, true, time.Now())
			if err := l.deps.sessions.Put(handoffAgent, session.SessionEntry{Port: 1, MachineID: "other-machine"}); err != nil {
				t.Fatalf("register the session on another listener: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			l := newHandoffListener(t, client, "test-machine")
			tc.prepare(l, t)
			item := listenerTestEnvelope(contracts.AgentSubject(handoffAgent), "agent-not-handed-off")
			if tc.source != "" {
				item.Source = tc.source
			}
			probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
			if err != nil {
				t.Fatalf("subscribe exception probe: %v", err)
			}
			t.Cleanup(func() { _ = probe.Unsubscribe() })
			if err := client.Conn.Flush(); err != nil {
				t.Fatalf("flush exception probe: %v", err)
			}

			jetStreamDeliveryHandler(l.config)(&nats.Msg{Data: marshalListenerEnvelope(t, item)})

			if tc.exception {
				assertDeliveryException(t, probe, item, "no_holder")
			} else if exception, err := probe.NextMsg(300 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
				t.Fatalf("a message another listener owns published an exception: %v %v", exception, err)
			}
			assertNoHandoff(t, client, handoffAgent)
		})
	}
}
