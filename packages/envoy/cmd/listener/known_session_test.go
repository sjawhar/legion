package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// knownRig is a listener's stores over one NATS server, with the known-session store on a clock
// the test moves, and the bucket handle a test reads revisions from.
type knownRig struct {
	deps  *listenerDeps
	known *session.KnownSessions
	kv    nats.KeyValue
	// clock is what the known-session store stamps and throttles against; a test moves it.
	clock time.Time
}

func setupKnownRig(t *testing.T) *knownRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	rig := &knownRig{clock: time.Now()}
	known, err := session.OpenKnownSessions(client.Conn,
		session.WithKnownSessionReplicas(1),
		session.WithKnownSessionClock(func() time.Time { return rig.clock }))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	kv, err := client.JS().KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}
	rig.deps = &listenerDeps{client: client, registry: registry, sessions: sessions, known: known}
	rig.known, rig.kv = known, kv
	return rig
}

// subscribe runs one registration of sessionID on topics through the real route.
func (r *knownRig) subscribe(t *testing.T, sessionID string, topics []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             topics,
		"self_subscribed":    true,
		"title":              "a known session",
		"capabilities":       []string{"aside"},
		"client_instance_id": "inst_one",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	subscribeHandler(r.deps, "test-machine", logging.New("test")).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("subscribe %s: status=%d body=%s", sessionID, recorder.Code, recorder.Body.String())
	}
}

// revision is the bucket revision of sessionID's record, which counts the writes it has taken, or
// 0 when the bucket holds none.
func (r *knownRig) revision(t *testing.T, sessionID string) uint64 {
	t.Helper()
	entry, err := r.kv.Get(sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s from the known-session bucket: %v", sessionID, err)
	}
	return entry.Revision()
}

// A registration writes the session's record with the topics the registration stored, the team
// topic the consumer's filter drops among them: a record built from that filter would count no
// subscriber of a team topic, which no stream subject captures.
func TestARegistrationWritesTheKnownSessionRecordWithTheRegisteredTopics(t *testing.T) {
	rig := setupKnownRig(t)
	team := "notifications.team.deploy-squad"
	rig.subscribe(t, "ses_registered", []string{team, "notifications.github.sjawhar.legion.pr.1"})

	record, ok := rig.known.Get("ses_registered")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}
	if !slices.Contains(record.Topics, team) {
		t.Errorf("the record's topics are %v, want the team topic %q the registration stored", record.Topics, team)
	}
	if !slices.Contains(record.Topics, contracts.AgentSubject("ses_registered")) {
		t.Errorf("the record's topics are %v, want the session's own agent subject", record.Topics)
	}
	if filter := sessionConsumerFilter("ses_registered", record.Topics); slices.Contains(filter, team) {
		t.Errorf("the consumer filter %v holds the team topic, so this test no longer proves the record carries what the filter drops", filter)
	}
	if record.Title != "a known session" || record.ClientInstanceID != "inst_one" || !slices.Equal(record.Capabilities, []string{"aside"}) {
		t.Errorf("record = %#v, want the registration's title, instance id and capabilities", record)
	}
	if record.LastSeen != rig.clock.UnixMilli() {
		t.Errorf("record.LastSeen = %d, want the write's clock %d", record.LastSeen, rig.clock.UnixMilli())
	}
	if got := rig.known.Match(team); len(got) != 1 || got[0].SessionID != "ses_registered" {
		t.Errorf("Match of the team topic = %#v, want the session just registered", got)
	}
}

// A clean stop keeps the record and its topics, and moves only its last_seen: a session that
// stopped is still a recipient of the topics it followed, since Envoy keeps its messages for 72
// hours and it can resume under the same id.
func TestACleanStopKeepsTheKnownSessionRecordAndMovesLastSeen(t *testing.T) {
	rig := setupKnownRig(t)
	team := "notifications.team.deploy-squad"
	rig.subscribe(t, "ses_stopping", []string{team})
	registered, ok := rig.known.Get("ses_stopping")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}

	rig.clock = rig.clock.Add(session.KnownSessionRefreshAge + time.Minute)
	recorder := httptest.NewRecorder()
	deleteSessionHandler(rig.deps.sessions, rig.known, logging.New("test")).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/ses_stopping", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := rig.deps.sessions.Get("ses_stopping"); err == nil {
		t.Fatal("the session entry is still there, so this test did not stop the session")
	}

	stopped, ok := rig.known.Get("ses_stopping")
	if !ok {
		t.Fatal("the clean stop removed the known-session record")
	}
	if !slices.Equal(stopped.Topics, registered.Topics) {
		t.Errorf("the stopped record's topics are %v, want the registered %v", stopped.Topics, registered.Topics)
	}
	if stopped.LastSeen != rig.clock.UnixMilli() {
		t.Errorf("the stopped record's last_seen = %d, want the stop's clock %d", stopped.LastSeen, rig.clock.UnixMilli())
	}
	if stopped.LastSeen <= registered.LastSeen {
		t.Errorf("the stop left last_seen at %d, want it past the registration's %d", stopped.LastSeen, registered.LastSeen)
	}
	if got := rig.known.Match(team); len(got) != 1 || got[0].SessionID != "ses_stopping" {
		t.Errorf("Match of the team topic after the stop = %#v, want the stopped session", got)
	}
}

// Acceptance A9: 100 heartbeats of a session whose registration never changes, inside the
// 10-minute refresh age, write the record at most twice. The count is the bucket's own revision,
// which counts the writes the key took, and the heartbeats run through the registration route
// itself on a clock this test moves.
func TestAHundredUnchangedHeartbeatsWriteTheRecordAtMostTwice(t *testing.T) {
	rig := setupKnownRig(t)
	topics := []string{"notifications.team.deploy-squad"}
	const heartbeats = 100
	// 100 heartbeats five seconds apart span 8m15s, inside the refresh age, as a session's own
	// 90-second heartbeat would not: the point is the number of writes, not the interval.
	step := 5 * time.Second
	if total := step * (heartbeats - 1); total >= session.KnownSessionRefreshAge {
		t.Fatalf("the heartbeats span %s, which is not inside the %s refresh age", total, session.KnownSessionRefreshAge)
	}
	for i := range heartbeats {
		if i > 0 {
			rig.clock = rig.clock.Add(step)
		}
		rig.subscribe(t, "ses_heartbeat", topics)
	}
	if revision := rig.revision(t, "ses_heartbeat"); revision > 2 {
		t.Errorf("%d unchanged heartbeats wrote the record %d times, want at most 2", heartbeats, revision)
	} else {
		t.Logf("%d unchanged heartbeats inside %s wrote the record %d time(s)", heartbeats, session.KnownSessionRefreshAge, revision)
	}

	// A change to the registration is written at once, whatever the throttle's window.
	rig.subscribe(t, "ses_heartbeat", append(slices.Clone(topics), "notifications.team.another-squad"))
	record, ok := rig.known.Get("ses_heartbeat")
	if !ok {
		t.Fatal("the record is gone after a changed registration")
	}
	if !slices.Contains(record.Topics, "notifications.team.another-squad") {
		t.Errorf("the record's topics are %v, want the topic the changed registration added", record.Topics)
	}
}

// A subscription change writes the record too: an unsubscribe sets the session consumer's filter
// to what is left, and the record follows with the topics the registry still stores.
func TestARefollowWritesTheKnownSessionRecord(t *testing.T) {
	rig := newSessionConsumerRig(t)
	clock := time.Now()
	known, err := session.OpenKnownSessions(rig.deps.client.Conn,
		session.WithKnownSessionReplicas(1),
		session.WithKnownSessionClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	rig.deps.known = known

	kept, dropped := "notifications.team.deploy-squad", "notifications.team.other-squad"
	rig.subscribe(t, `{"session_id":"ses_refollow","topics":["`+kept+`","`+dropped+`"],`+
		`"delivery":{"consumer":true,"epoch":"e1"}}`)
	registered, ok := known.Get("ses_refollow")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}
	if !slices.Contains(registered.Topics, dropped) {
		t.Fatalf("the registered record's topics are %v, want the topic the unsubscribe will remove", registered.Topics)
	}

	clock = clock.Add(session.KnownSessionRefreshAge + time.Minute)
	rig.post(t, "/v1/interests/unsubscribe", `{"session_id":"ses_refollow","topics":["`+dropped+`"]}`)

	after, ok := known.Get("ses_refollow")
	if !ok {
		t.Fatal("the unsubscribe removed the known-session record")
	}
	if slices.Contains(after.Topics, dropped) {
		t.Errorf("the record's topics are %v, want the unsubscribed %q gone", after.Topics, dropped)
	}
	if !slices.Contains(after.Topics, kept) {
		t.Errorf("the record's topics are %v, want the topic the session still follows", after.Topics)
	}
	if after.LastSeen != clock.UnixMilli() {
		t.Errorf("the record's last_seen = %d, want the refollow's clock %d", after.LastSeen, clock.UnixMilli())
	}
}

// A listener whose NATS user may not open the bucket runs without the store: the routes that
// write a record carry no nil check, so a registration and a clean stop answer as they always
// have, and every read answers no record.
func TestTheRoutesServeAListenerWithNoKnownSessionStore(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	deps := &listenerDeps{registry: registry, sessions: sessions}

	recorder := httptest.NewRecorder()
	subscribeHandler(deps, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(`{"session_id":"ses_degraded","topics":["notifications.team.deploy-squad"],"self_subscribed":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("subscribe: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	stop := httptest.NewRecorder()
	deleteSessionHandler(sessions, deps.known, logging.New("test")).ServeHTTP(
		stop, httptest.NewRequest(http.MethodDelete, "/v1/sessions/ses_degraded", nil))
	if stop.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", stop.Code, stop.Body.String())
	}

	if record, ok := deps.known.Get("ses_degraded"); ok {
		t.Errorf("a listener with no known-session store answered %#v, want no record", record)
	}
	if got := deps.known.Match("notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("a listener with no known-session store matched %#v, want none", got)
	}
}
