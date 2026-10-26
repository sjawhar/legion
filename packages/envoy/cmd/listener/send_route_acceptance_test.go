package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sendBody is one send request's JSON, written the way a client writes it, so each test below
// states only the fields it is about.
func sendBody(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// postJSON is an inbound request to one of the two message routes, as the listener's own mux
// hands it over: the handlers are called directly, so the context is the request's own and the
// send budget (sendRequestBudget) is measured from the call.
func postJSON(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

// serveRecorded serves one request through a handler built over a logger of its own and returns
// the answer with the single record line that handler wrote. Each call gets a fresh buffer, so
// "exactly one line" is asserted per request rather than over a whole test: one line per request
// is the whole point of the record, and a buffer shared across requests could only ever show that
// at least one line was written.
func serveRecorded(t *testing.T, build func(*logging.Logger) http.HandlerFunc, request *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	logs := &cmdtest.LockedBuffer{}
	recorder := httptest.NewRecorder()
	build(logging.NewWithWriter("test", logs)).ServeHTTP(recorder, request)
	written := strings.TrimSpace(logs.String())
	lines := strings.Split(written, "\n")
	if written == "" || len(lines) != 1 {
		t.Fatalf("one request wrote %d record lines, want exactly one:\n%s", len(lines), logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("the record line is not JSON: %v\n%s", err, lines[0])
	}
	return recorder, record
}

// assertRecordFields compares the named fields of one record line, field by field, as the
// starting gate's own record test does. A JSON number unmarshals into float64, so a status is
// named as float64(200) here.
func assertRecordFields(t *testing.T, record map[string]any, want map[string]any) {
	t.Helper()
	for field, expected := range want {
		if record[field] != expected {
			line, _ := json.Marshal(record)
			t.Fatalf("the record's %s = %#v, want %#v: %s", field, record[field], expected, line)
		}
	}
}

// A send reads the target from the session bucket itself when this listener's cache has not seen
// it: the cache follows the bucket through a watcher and so trails it, and a sender told its
// destination is gone routes around the pane. This drives the send route with a session another
// listener registered a moment ago, through a listener whose cache was warm before that write and
// has followed nothing since (trailingListeners), which is the same miss a cache still refilling
// after a reconnect has.
//
// The negative control is the Get below: if the cache held the target the 200 would prove nothing
// about reading through to the bucket, since every warm-cache send already answers 200. The
// delivery read back from notifications.agent.<id> is the second half: a 200 alone would pass
// even if the envelope never left the listener.
func TestSendReadsTheSessionStoreWhileTheCacheIsCold(t *testing.T) {
	const target = "ses_cold_cache_target"
	registrar, cold := trailingListeners(t)
	if err := registrar.sessions.Put(target, session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/" + target, Title: "cold target",
	}); err != nil {
		t.Fatalf("register %s through the other listener: %v", target, err)
	}
	received, err := cold.client.Conn.SubscribeSync(contracts.AgentSubject(target))
	if err != nil {
		t.Fatalf("subscribe to %s: %v", contracts.AgentSubject(target), err)
	}
	t.Cleanup(func() { _ = received.Unsubscribe() })
	if err := cold.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	if entry, err := cold.sessions.Get(target); err == nil {
		t.Fatalf("the sending listener's cache already holds %s (%+v): the send below would prove nothing", target, entry)
	} else if !strings.Contains(err.Error(), nats.ErrKeyNotFound.Error()) {
		t.Fatalf("the sending listener's cache answers %v for %s, want %v", err, target, nats.ErrKeyNotFound)
	}

	recorder := httptest.NewRecorder()
	sendHandler(cold, logging.New("test")).ServeHTTP(recorder, postJSON("/v1/messages/send", sendBody(map[string]any{
		"target_session": target,
		"source":         "agent",
		"source_session": "ses_cold_cache_sender",
		"message":        "the bucket holds you even when my cache does not",
	})))
	if recorder.Code != http.StatusOK {
		t.Fatalf("send to a session only the bucket holds: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	var answered contracts.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}

	message, err := received.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("nothing reached %s: %v", contracts.AgentSubject(target), err)
	}
	var delivered contracts.Envelope
	if err := json.Unmarshal(message.Data, &delivered); err != nil {
		t.Fatalf("decode the delivered envelope: %v", err)
	}
	if delivered.EventID != answered.EventID || delivered.Topic != contracts.AgentSubject(target) {
		t.Fatalf("delivered %+v, want the answered envelope %+v on the target's subject", delivered, answered)
	}
}

// A send whose bus connection has gone away under the listener is answered 503 naming the bus,
// inside the five seconds the HTTP client gives one attempt (envoy-client's transport), rather
// than left to that caller's own timeout, which it would retry blind. This drives the real
// handler over a real connection cut at the TCP level, which is what the hub's virtual machine
// being recreated did on 2026-10-23.
//
// The target is registered first and its entry read back from the warm cache, so the session read
// cannot be what fails: the 503 can only come from the publish. The negative control is the
// identical request before the cut, which answers 200 — without it a rig that refused every send
// (a bad body, an unregistered target) would pass this test with a 503 of a different kind.
func TestSendAnswersServiceUnavailableWhileTheBusIsUnreachable(t *testing.T) {
	const target = "ses_cut_bus_target"
	// The relay the reconnect test uses (reconnect_shutdown_test.go): cut below closes every live
	// link and refuses every connection after it, as the hub's virtual machine being recreated
	// (2026-10-23) did to the listener's bus connection.
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
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
	if err := sessions.Put(target, session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/" + target, Title: "cut target",
	}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	if _, err := sessions.Get(target); err != nil {
		t.Fatalf("the listener's cache does not hold %s (%v): the 503 below could come from the session read", target, err)
	}
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	send := func() (*httptest.ResponseRecorder, time.Duration) {
		recorder := httptest.NewRecorder()
		started := time.Now()
		sendHandler(state, logging.New("test")).ServeHTTP(recorder, postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_cut_bus_sender",
			"message":        "is the bus there?",
		})))
		return recorder, time.Since(started)
	}

	if reachable, _ := send(); reachable.Code != http.StatusOK {
		t.Fatalf("the same send with the bus reachable: status %d, body %s; want 200", reachable.Code, reachable.Body.String())
	}

	relay.cut()
	// nats.go notices the closed link from its own read loop, so the send below is issued on a
	// connection already known to be down rather than on one that is about to be: the measured
	// time is the handler's, not the client's time to notice.
	for deadline := time.Now().Add(5 * time.Second); client.Conn.IsConnected(); {
		if time.Now().After(deadline) {
			t.Fatal("the NATS client still reports itself connected 5s after the relay cut every link")
		}
		time.Sleep(10 * time.Millisecond)
	}

	recorder, elapsed := send()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("send with the bus unreachable: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: the sender is being asked to try the same route again", got)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("the 503 took %s, past the 4s that leaves the caller's 5s attempt time to read it", elapsed)
	}
	t.Logf("bus unreachable: 503 in %s: %s", elapsed, refusal.Error)
}

// A bus that goes silent without closing the connection - a partition, or a hub host that stops
// answering without resetting its sockets - is unreachable too, and a send and a publish answer it
// with the same named 503 inside the caller's attempt. nats.go cannot tell such a link is dead
// until its pings go unanswered, minutes later, so every bus call made on it waits: the target's
// read and the publish are bounded by sendRequestBudget, and so must every other bus call the
// route makes before it answers, the sender stamp's read of the source session's interest record
// (senderStamp) among them. That read is the cache's miss read through to the bucket, as it is for
// any sender whose interest record the cache does not hold: one that subscribed to nothing, or any
// sender while the cache refills after a reconnect, which is the case this issue began with.
//
// The relay freezes the link by discarding everything the client writes from the moment it is
// armed, keeping the connection open, so the client still reports itself connected. The target is
// in the warm session cache, so its read is not what is under test. The negative control is the
// identical request before the freeze, answered 200 on the same rig.
func TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	const (
		target = "ses_frozen_bus_target"
		sender = "ses_frozen_bus_sender_with_no_interest_record"
		within = 4 * time.Second
	)
	for _, route := range []struct {
		name    string
		handler func(*listenerDeps) http.HandlerFunc
		path    string
		body    map[string]any
	}{
		{
			name:    "send",
			handler: func(d *listenerDeps) http.HandlerFunc { return sendHandler(d, logging.New("test")) },
			path:    "/v1/messages/send",
			body:    map[string]any{"target_session": target, "source": "agent", "source_session": sender, "message": "is the bus there?"},
		},
		{
			name:    "publish",
			handler: func(d *listenerDeps) http.HandlerFunc { return publishHandler(d, logging.New("test")) },
			path:    "/v1/messages/publish",
			body:    map[string]any{"topic": "notifications.github.example-org.example-repo.pr.1", "source": "agent", "source_session": sender, "message": "is the bus there?"},
		},
	} {
		t.Run(route.name, func(t *testing.T) {
			relay := startListenerRelay(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
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
			if err := sessions.Put(target, session.SessionEntry{
				Port: 1, MachineID: "test-machine", Dir: "/test/" + target, Title: "frozen target",
			}); err != nil {
				t.Fatalf("register %s: %v", target, err)
			}
			if _, err := sessions.Get(target); err != nil {
				t.Fatalf("the listener's cache does not hold %s (%v): the target's read could be what fails", target, err)
			}
			state := &listenerDeps{client: client, registry: registry, sessions: sessions}
			serve := func() (*httptest.ResponseRecorder, time.Duration) {
				recorder := httptest.NewRecorder()
				started := time.Now()
				route.handler(state).ServeHTTP(recorder, postJSON(route.path, sendBody(route.body)))
				return recorder, time.Since(started)
			}
			if reachable, _ := serve(); reachable.Code != http.StatusOK {
				t.Fatalf("the same %s with the bus answering: status %d, body %s; want 200", route.name, reachable.Code, reachable.Body.String())
			}

			// The next protocol line the client writes arms the swallow; every byte after it is
			// discarded and the connection stays open.
			relay.arm("\r\n", 1)
			recorder, elapsed := serve()
			if elapsed >= within {
				t.Fatalf("the %s answered after %s (status %d, body %s), want inside %s - a bus that stopped answering must be refused inside the caller's 5 s attempt, not left to its timeout",
					route.name, elapsed, recorder.Code, recorder.Body.String(), within)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s with the bus silent: status %d, body %s; want 503", route.name, recorder.Code, recorder.Body.String())
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
				t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
		})
	}
}

// A bus that is reachable and does not accept the publish is the same 503, carrying the server's
// own reason: here the notification stream is deleted under the running listener, so the
// JetStream publish to notifications.agent.<id> is left without a stream to acknowledge it
// (nats.ErrNoStreamResponse). The record is asserted with it, because a 503 nothing records is
// the failure this route's line exists to end.
//
// The two answers that must not be swept into that 503 are asserted in the same test, against the
// same live bus: an envelope past the server's max payload is still the caller's own 413, and a
// subject NATS does not accept is still a 400, neither carrying Retry-After, since retrying
// either sends the same bytes to the same refusal. The invalid subject is reached through the
// publish route, not the send route: a send's subject is built from target_session, and a target
// that would make an invalid agent subject is first read from the session bucket, where nats.go
// refuses the key and the route answers 404 before the bus ever sees the subject. A publish's
// topic goes to the bus as written, so `notifications..x` clears the handler's prefix checks and
// is refused by the bus's own subject check (bus.ErrInvalidSubject).
func TestSendAnswersServiceUnavailableWhenTheBusRefusesThePublish(t *testing.T) {
	const target = "ses_refused_publish_target"
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{target: 1})
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	// The stream goes after every store is open: opening one reconciles the stream back.
	if err := client.JS().DeleteStream(bus.Stream); err != nil {
		t.Fatalf("delete the notification stream: %v", err)
	}

	recorder, record := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
		postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        "the stream is gone",
		})))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("send the bus refuses: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	reason, found := strings.CutPrefix(refusal.Error, busUnansweredPrefix+": ")
	if !found || strings.TrimSpace(reason) == "" {
		t.Fatalf("the refusal reads %q, want %q followed by the bus's own reason", refusal.Error, busUnansweredPrefix)
	}
	if !strings.Contains(reason, nats.ErrNoStreamResponse.Error()) {
		t.Fatalf("the refusal carries %q, want the bus's own answer to a publish no stream takes (%v)", reason, nats.ErrNoStreamResponse)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	assertRecordFields(t, record, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "bus_unavailable",
		"status":         float64(http.StatusServiceUnavailable),
		"target_session": target,
	})
	t.Logf("bus refused the publish: %s", refusal.Error)

	// An envelope past the server's max payload: nats.go refuses it before sending, which the bus
	// reports as bus.ErrTooLarge, and that is the caller's own 413, not a bus that did not answer.
	oversized, oversizedRecord := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
		postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        strings.Repeat("a", 2<<20),
		})))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized send: status %d, body %s; want 413", oversized.Code, oversized.Body.String())
	}
	if got := oversized.Header().Get("Retry-After"); got != "" {
		t.Fatalf("an oversized send carries Retry-After %q: the same bytes are refused however often they are sent", got)
	}
	assertRecordFields(t, oversizedRecord, map[string]any{
		"msg": "listener send", "level": "WARN", "outcome": "too_large",
		"status": float64(http.StatusRequestEntityTooLarge),
	})

	// A topic with an empty token: no stream subject matches it and NATS does not accept it, so
	// the bus refuses it as bus.ErrInvalidSubject and the route answers 400.
	invalid, invalidRecord := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
		postJSON("/v1/messages/publish", sendBody(map[string]any{
			"topic":          "notifications..x",
			"source":         "agent",
			"source_session": "ses_refused_publish_sender",
			"message":        "a subject NATS will not take",
		})))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("a publish to notifications..x: status %d, body %s; want 400", invalid.Code, invalid.Body.String())
	}
	if got := invalid.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a publish NATS refuses carries Retry-After %q: the same subject is refused however often it is sent", got)
	}
	assertRecordFields(t, invalidRecord, map[string]any{
		"msg": "listener publish", "level": "WARN", "outcome": "invalid_request",
		"status": float64(http.StatusBadRequest), "topic": "notifications..x",
	})
}

// Every inbound send and publish leaves exactly one line, so a send that was never attempted, one
// attempted and lost and one attempted and refused can be told apart from the listener's own log
// afterwards. This drives both routes against a real bus and reads each line back field by field,
// as the starting gate's record test reads its own.
//
// Each answer is asserted to be exactly one line, not at least one: a route that logged its
// request twice, or that logged a second line from elsewhere in the same request, would read as
// two requests to anyone counting the record. The refusals are the half no other system holds:
// nothing was published, so no arrival line and no stream entry will ever name them, which is why
// the line's own fields (the summary among them) are checked on a refusal that has an envelope
// and on one refused before any envelope exists.
func TestListenerRecordsEverySendAndPublishRequest(t *testing.T) {
	const target = "ses_recorded_target"
	const sender = "ses_recorded_sender"
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{target: 1, sender: 1})
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	send := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		return serveRecorded(t,
			func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) },
			postJSON("/v1/messages/send", body))
	}
	publish := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		return serveRecorded(t,
			func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
			postJSON("/v1/messages/publish", body))
	}

	accepted, acceptedRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "dispatch",
		"source_session": sender,
		"message":        "ship it",
	}))
	if accepted.Code != http.StatusOK {
		t.Fatalf("an accepted send: status %d, body %s", accepted.Code, accepted.Body.String())
	}
	var answered contracts.Envelope
	if err := json.Unmarshal(accepted.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode the answer %s: %v", accepted.Body.String(), err)
	}
	if answered.EventID == "" {
		t.Fatalf("the accepted send answered no event id: %s", accepted.Body.String())
	}
	assertRecordFields(t, acceptedRecord, map[string]any{
		"msg":             "listener send",
		"level":           "INFO",
		"outcome":         "accepted",
		"status":          float64(http.StatusOK),
		"target_session":  target,
		"event_id":        answered.EventID,
		"source":          "dispatch",
		"source_session":  sender,
		"payload_summary": "ship it",
	})

	gone, goneRecord := send(sendBody(map[string]any{
		"target_session": "ses_recorded_gone",
		"source":         "agent",
		"source_session": sender,
		"message":        "anyone there?",
	}))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("a send to a session the store does not hold: status %d, body %s; want 404", gone.Code, gone.Body.String())
	}
	assertRecordFields(t, goneRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "no_live_session",
		"status":          float64(http.StatusNotFound),
		"target_session":  "ses_recorded_gone",
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "anyone there?",
	})

	// Refused for its body: no envelope was ever built, so the line carries the destination and
	// the empty envelope fields rather than being skipped.
	empty, emptyRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "agent",
		"source_session": sender,
		"message":        "   ",
	}))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("a send with an empty message: status %d, body %s; want 400", empty.Code, empty.Body.String())
	}
	assertRecordFields(t, emptyRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "invalid_request",
		"status":          float64(http.StatusBadRequest),
		"target_session":  target,
		"event_id":        "",
		"payload_summary": "",
	})

	// Refused for the target: it is live, but the frame asks for a delivery mode it does not
	// advertise (the store holds it with no capabilities at all), so the route never publishes.
	// The envelope was built, so the line carries its fields.
	unadvertised, unadvertisedRecord := send(sendBody(map[string]any{
		"target_session": target,
		"source":         "dispatch",
		"source_session": sender,
		"message":        "steer this",
		"payload":        `{"delivery":{"mode":"steer"}}`,
	}))
	if unadvertised.Code != http.StatusForbidden {
		t.Fatalf("a send asking for a mode the target does not advertise: status %d, body %s; want 403", unadvertised.Code, unadvertised.Body.String())
	}
	assertRecordFields(t, unadvertisedRecord, map[string]any{
		"msg":             "listener send",
		"level":           "WARN",
		"outcome":         "capability_refused",
		"status":          float64(http.StatusForbidden),
		"target_session":  target,
		"source":          "dispatch",
		"source_session":  sender,
		"payload_summary": "steer this",
	})
	if eventID, _ := unadvertisedRecord["event_id"].(string); eventID == "" {
		t.Fatalf("the capability refusal's line carries no event id, though its envelope was built: %v", unadvertisedRecord)
	}

	const topic = "notifications.team.recorded-squad"
	published, publishedRecord := publish(sendBody(map[string]any{
		"topic":          topic,
		"source":         "agent",
		"source_session": sender,
		"message":        "deploy complete",
	}))
	if published.Code != http.StatusOK {
		t.Fatalf("an accepted publish: status %d, body %s", published.Code, published.Body.String())
	}
	var publishedEnvelope contracts.Envelope
	if err := json.Unmarshal(published.Body.Bytes(), &publishedEnvelope); err != nil {
		t.Fatalf("decode the answer %s: %v", published.Body.String(), err)
	}
	assertRecordFields(t, publishedRecord, map[string]any{
		"msg":             "listener publish",
		"level":           "INFO",
		"outcome":         "accepted",
		"status":          float64(http.StatusOK),
		"topic":           topic,
		"event_id":        publishedEnvelope.EventID,
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "deploy complete",
	})

	unclaimed := contracts.RoleTopicPrefix + "recorded-unclaimed"
	refused, refusedRecord := publish(sendBody(map[string]any{
		"topic":          unclaimed,
		"source":         "agent",
		"source_session": sender,
		"message":        "nobody holds this",
	}))
	if refused.Code != http.StatusNotFound {
		t.Fatalf("a publish to a role nobody holds: status %d, body %s; want 404", refused.Code, refused.Body.String())
	}
	assertRecordFields(t, refusedRecord, map[string]any{
		"msg":            "listener publish",
		"level":          "WARN",
		"outcome":        "role_unclaimed",
		"status":         float64(http.StatusNotFound),
		"topic":          unclaimed,
		"source_session": sender,
	})
	if holder, ok := refusedRecord["holder"]; ok {
		t.Fatalf("the unclaimed role's line names holder %v: there is no holder to name", fmt.Sprint(holder))
	}

	// The role is claimed, but by a session the store does not hold: the claim has lapsed, and the
	// line names the holder the answer's own body names.
	const lapsedHolder = "ses_recorded_lapsed_holder"
	lapsedRole := "recorded-lapsed"
	if _, err := registry.SetRole(lapsedHolder, "test-machine", lapsedRole, false); err != nil {
		t.Fatalf("claim role %s for a session the store does not hold: %v", lapsedRole, err)
	}
	lapsedTopic := contracts.RoleTopicPrefix + lapsedRole
	lapsed, lapsedRecord := publish(sendBody(map[string]any{
		"topic":          lapsedTopic,
		"source":         "agent",
		"source_session": sender,
		"message":        "is the holder still there?",
	}))
	if lapsed.Code != http.StatusNotFound {
		t.Fatalf("a publish to a role whose holder lapsed: status %d, body %s; want 404", lapsed.Code, lapsed.Body.String())
	}
	assertRecordFields(t, lapsedRecord, map[string]any{
		"msg":             "listener publish",
		"level":           "WARN",
		"outcome":         "role_lapsed",
		"status":          float64(http.StatusNotFound),
		"topic":           lapsedTopic,
		"source":          "agent",
		"source_session":  sender,
		"payload_summary": "is the holder still there?",
		"holder":          lapsedHolder,
	})
}

// A session read that did not answer is no evidence the session is gone, so the send that made it
// is refused 503 naming the bus rather than 404 naming the session. The 404 is the one answer a
// sender routes around the pane on, so it must rest on the bucket's own word
// (nats.ErrKeyNotFound) and on nothing else — reporting an absence this listener never checked is
// the incident INFRA-141 is about. It is the one row of the send route's status map that only an
// unanswered read reaches: the cold-cache test's read answers, the bus-unreachable and
// bus-refusal tests fail at the publish with the target already in the cache, and the absent
// target's 404 is the bucket answering. Collapsing the two errors back into one 404 leaves every
// other test in this package green.
//
// The negative control is the first send: the same route, the same listener, the same
// unregistered target, with the bucket answering. It is the 404, and the second send differs from
// it in one thing only — the bucket no longer answers — so a rig that refused every send could
// not produce the pair.
func TestSendRefusesTheBusRatherThanTheSessionWhenTheStoreReadDoesNotAnswer(t *testing.T) {
	const target = "ses_unanswered_store_read"
	const sender = "ses_unanswered_store_sender"
	_, observer := trailingListeners(t)
	send := func() (*httptest.ResponseRecorder, map[string]any, time.Duration) {
		t.Helper()
		started := time.Now()
		recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
			return sendHandler(observer, logger)
		}, postJSON("/v1/messages/send", sendBody(map[string]any{
			"target_session": target,
			"source":         "agent",
			"source_session": sender,
			"message":        "is the pane there?",
		})))
		return recorder, record, time.Since(started)
	}

	answered, answeredRecord, _ := send()
	if answered.Code != http.StatusNotFound {
		t.Fatalf("a send whose bucket read answered that the key is gone: status %d, body %s; want 404", answered.Code, answered.Body.String())
	}
	if got, want := strings.TrimSpace(answered.Body.String()), fmt.Sprintf(`{"error":"no live session %s"}`, target); got != want {
		t.Fatalf("the 404 body is %s, want %s", got, want)
	}
	assertRecordFields(t, answeredRecord, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "no_live_session",
		"status":         float64(http.StatusNotFound),
		"target_session": target,
		"source_session": sender,
	})

	stallSessionReads(t, observer.sessions)
	unanswered, unansweredRecord, elapsed := send()
	if unanswered.Code != http.StatusServiceUnavailable {
		t.Fatalf("a send whose bucket read never answered: status %d, body %s; want 503, since nothing was learned about %s", unanswered.Code, unanswered.Body.String(), target)
	}
	var refusal apiError
	if err := json.Unmarshal(unanswered.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", unanswered.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
	}
	if strings.Contains(refusal.Error, "no live session") {
		t.Fatalf("the refusal reads %q: an unanswered read must not be reported as the session being gone", refusal.Error)
	}
	if got := unanswered.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: the sender is being asked to try the same route again", got)
	}
	// The read's own bound (sendSessionReadTimeout) is what ends this request, and it is a
	// fraction of the whole send budget: a handler that waited the bucket out would spend the
	// caller's attempt on a read that was never going to answer.
	if elapsed >= sendRequestBudget {
		t.Fatalf("the 503 took %s, past the whole send budget %s, for a read bounded at %s", elapsed, sendRequestBudget, sendSessionReadTimeout)
	}
	assertRecordFields(t, unansweredRecord, map[string]any{
		"msg":            "listener send",
		"level":          "WARN",
		"outcome":        "bus_unavailable",
		"status":         float64(http.StatusServiceUnavailable),
		"target_session": target,
		"source_session": sender,
	})
}
