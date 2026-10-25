package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// These tests drive the built listener against a real NATS and a fake serving port: the OpenCode
// plugin's way of being reached, where the listener POSTs each notification to
// /session/<id>/prompt_async. They use only what the listener had before INFRA-122 (its binary,
// its /v1 API and the bus), so the file compiles unchanged at the commit before the change. There
// TestAServingPortReceivesATopicTheStreamDoesNotStore and
// TestAListenerReachesASessionAtTheHostItRecorded fail, each with no prompt arriving; at the
// change's head both pass. The rest hold what the live topics lane (ENVOY_SUBSCRIBE_ALL_TOPICS=1)
// must leave as it was, and how it ends a start whose grant refuses it.

// servingPort is a session's fake serve process: an httptest server on 127.0.0.1 that records the
// text of every prompt the listener POSTs to /session/<id>/prompt_async and answers with status.
type servingPort struct {
	sessionID string
	port      int
	prompts   chan string
}

// startServingPort serves sessionID's prompt_async route on a loopback port of its own until the
// test ends. A request to any other method or path fails the test: the listener reached the port
// with something other than a prompt for this session.
func startServingPort(t *testing.T, sessionID string, status int) *servingPort {
	t.Helper()
	serving := &servingPort{sessionID: sessionID, prompts: make(chan string, 16)}
	path := "/session/" + sessionID + "/prompt_async"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("%s's serving port was sent %s %s, want POST %s", sessionID, r.Method, r.URL.Path, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode the prompt sent to %s: %v", sessionID, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var text strings.Builder
		for _, part := range body.Parts {
			text.WriteString(part.Text)
		}
		serving.prompts <- text.String()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	serving.port = endpointPort(t, server.URL)
	return serving
}

// awaitPrompt returns the next prompt this port receives, and fails the test when none arrives
// within the bound.
func (p *servingPort) awaitPrompt(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case text := <-p.prompts:
		return text
	case <-time.After(within):
		t.Fatalf("no prompt reached %s's serving port within %s", p.sessionID, within)
		return ""
	}
}

// noPrompt fails the test when this port receives a prompt within the window.
func (p *servingPort) noPrompt(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case text := <-p.prompts:
		t.Fatalf("%s's serving port received another prompt within %s, want none:\n%s", p.sessionID, window, text)
	case <-time.After(window):
	}
}

// registerServingSession registers sessionID with the listener as the OpenCode plugin does: its
// serving port, the host it runs on, and its topics. The listener must answer 200.
func registerServingSession(t *testing.T, listener *listenerProcess, serving *servingPort, host string, topics ...string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id": serving.sessionID,
		"topics":     topics,
		"port":       serving.port,
		"host":       host,
	})
	if err != nil {
		t.Fatalf("encode the registration of %s: %v", serving.sessionID, err)
	}
	postListener(t, listener.port, "/v1/interests/subscribe", string(body))
}

// connectPublisher opens a bus client of the test's own, apart from the listener's: a publish
// through it reaches NATS as one from any session holding its own connection does, core NATS for a
// topic the stream does not store and JetStream for one it does (bus.Client.Publish).
func connectPublisher(t *testing.T, uri string) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{uri}, bus.WithName("live-topics-test-publisher"))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// newEnvelope is a valid envelope from source on topic carrying message, with ids of its own.
func newEnvelope(source, topic, message string) contracts.Envelope {
	eventID := "evt-" + id.New()
	return contracts.Envelope{
		EventID:        eventID,
		Source:         source,
		SourceEventID:  eventID,
		Topic:          topic,
		DedupeKey:      "live-topics-test." + eventID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: message,
		Payload:        message,
		TraceID:        "trace-" + eventID,
	}
}

// publish publishes item through client and flushes, so the server holds it when publish returns.
func publish(t *testing.T, client *bus.Client, item contracts.Envelope) {
	t.Helper()
	if err := client.Publish(item); err != nil {
		t.Fatalf("publish %s: %v", item.Topic, err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publish of %s: %v", item.Topic, err)
	}
}

// requirePromptCarries fails the test when a prompt's text does not name the envelope's topic or
// carry its message: the session would not know what arrived or where.
func requirePromptCarries(t *testing.T, text string, item contracts.Envelope) {
	t.Helper()
	if !strings.Contains(text, "Topic: "+item.Topic) {
		t.Errorf("the prompt does not name the topic %s:\n%s", item.Topic, text)
	}
	if !strings.Contains(text, item.Payload) {
		t.Errorf("the prompt does not carry the message %q:\n%s", item.Payload, text)
	}
}

// A session on a serving port subscribed to a topic the stream does not store, such as a team
// topic, receives it when a listener on its machine is told its grant lets it subscribe to every
// notification topic (ENVOY_SUBSCRIBE_ALL_TOPICS=1). Before INFRA-122 the listener took only the
// stream's durable and the role lanes off the bus, so such a publish went out over core NATS to
// no one and the session was never told: this test fails there with no prompt in 2 s. The topic is
// published from a bus client of the test's own, as a session holding its own NATS connection
// publishes, so nothing in the listener's /v1 path can hand it to the session. A second prompt
// within a second would be the same publish delivered twice.
func TestAServingPortReceivesATopicTheStreamDoesNotStore(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "live-topics",
		"ENVOY_SUBSCRIBE_ALL_TOPICS=1", "ENVOY_HOST_BRIDGE=127.0.0.1")
	listener.waitHealthy(t)

	topic := "notifications.team." + id.New()
	serving := startServingPort(t, "ses_live_"+id.New(), http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)

	item := newEnvelope("agent", topic, "deploy finished on the team topic")
	item.SourceSession = "ses_publisher_" + id.New()
	publish(t, connectPublisher(t, uri), item)

	text := serving.awaitPrompt(t, 2*time.Second)
	requirePromptCarries(t, text, item)
	serving.noPrompt(t, time.Second)
}

// A listener with no host bridge reaches a session on its own machine at the host the session
// recorded when it registered. Before INFRA-122 it ignored the recorded host and dialled
// host.docker.internal for every session on its machine, which is no address of the session's
// serving port (here bound on 127.0.0.1 only), so the delivery failed and was retried without end:
// this test fails there with no prompt in 10 s. The topic is one the stream stores
// (notifications.github.>), so the prompt comes through the listener's durable, whose first
// delivery the 10 s bound covers with room.
func TestAListenerReachesASessionAtTheHostItRecorded(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "recorded-host")
	listener.waitHealthy(t)

	topic := "notifications.github.acme.repo" + id.New() + ".pr.1"
	serving := startServingPort(t, "ses_host_"+id.New(), http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)

	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, connectPublisher(t, uri), item)

	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), item)
}

// liveLaneEnv is the environment of a listener with the live topics lane on, which reaches a
// session on its own machine at 127.0.0.1, where the tests' serving ports listen.
var liveLaneEnv = []string{"ENVOY_SUBSCRIBE_ALL_TOPICS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"}

// liveLaneLine is the line a listener logs once NATS has accepted its live topics subscription.
const liveLaneLine = "live topics subscribed"

// holdRole registers a session that takes its own messages off the bus, as Oh My Pi does, has it
// claim role through listener, and subscribes on its agent subject for it, answering each role
// forward's receipt. It returns the session id and every envelope the session receives there.
func holdRole(t *testing.T, listener *listenerProcess, uri, role string) (string, <-chan contracts.Envelope) {
	t.Helper()
	sessionID := "ses_holder_" + id.New()
	postListener(t, listener.port, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","topics":[],"self_subscribed":true}`)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	received := make(chan contracts.Envelope, 16)
	if _, err := conn.Subscribe(contracts.AgentSubject(sessionID), func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("decode what %s received: %v", sessionID, err)
			return
		}
		_ = msg.Respond(nil)
		received <- item
	}); err != nil {
		t.Fatalf("subscribe for %s: %v", sessionID, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription for %s: %v", sessionID, err)
	}
	return sessionID, received
}

// requireOneRoleMessage fails the test unless the holder receives exactly one envelope, the role
// message on topic carrying message, within the window.
func requireOneRoleMessage(t *testing.T, received <-chan contracts.Envelope, topic, message string, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	var got []contracts.Envelope
	for collecting := true; collecting; {
		select {
		case item := <-received:
			got = append(got, item)
		case <-deadline:
			collecting = false
		}
	}
	if len(got) != 1 || got[0].Topic != topic || got[0].PayloadSummary != message {
		t.Fatalf("the role holder received %+v within %s, want the one message %q on %s", got, window, message, topic)
	}
}

// awaitSessionsListed waits up to 10 s for listener's GET /v1/sessions to list each session with
// its topics, so a delivery through this listener finds them in its caches.
func (p *listenerProcess) awaitSessionsListed(t *testing.T, sessionIDs ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := callListener(t, p.port, http.MethodGet, "/v1/sessions", "")
		var listed []struct {
			SessionID string   `json:"session_id"`
			Topics    []string `json:"topics"`
		}
		missing := sessionIDs
		if status == http.StatusOK && json.Unmarshal([]byte(body), &listed) == nil {
			missing = nil
			for _, sessionID := range sessionIDs {
				found := false
				for _, row := range listed {
					found = found || (row.SessionID == sessionID && len(row.Topics) > 0)
				}
				if !found {
					missing = append(missing, sessionID)
				}
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener on port %d does not list %v within 10s: %d %s", p.port, missing, status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// With the live topics lane on, the listener takes every stored message off the bus twice, once
// through its durable and once through the live lane, and every role message twice, once through
// the role lane and once through the live lane. Each still reaches its session once: a topic the
// stream stores and an inbox message at a serving port, and a role message at its holder.
func TestAStoredTopicStillReachesAServingPortOnce(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "stored-once", liveLaneEnv...)
	listener.waitHealthy(t)
	publisher := connectPublisher(t, uri)

	topic := "notifications.github.acme.repo" + id.New() + ".pr.1"
	serving := startServingPort(t, "ses_stored_"+id.New(), http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)

	stored := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, publisher, stored)
	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), stored)
	serving.noPrompt(t, 2*time.Second)

	inbox := newEnvelope("agent", contracts.AgentSubject(serving.sessionID), "a message for this session alone")
	inbox.SourceSession = "ses_sender_" + id.New()
	publish(t, publisher, inbox)
	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), inbox)
	serving.noPrompt(t, 2*time.Second)

	role := "stored-once-" + id.New()
	_, received := holdRole(t, listener, uri, role)
	postListener(t, listener.port, "/v1/messages/publish", `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"a message for whoever holds the role"}`)
	requireOneRoleMessage(t, received, contracts.RoleTopicPrefix+role, "a message for whoever holds the role", 2*time.Second)
}

// Two listeners of one machine, as a rolling deploy runs them, both with the live lane on: NATS
// hands each live message to one of them, so a session receives each event once, and the live
// lane, in a queue group apart from the role lane's, takes no role message from its holder. The
// second listener waits on the durable the first holds and never turns healthy, so its live lane
// is open once it logs so.
func TestTwoListenersOnOneMachineDeliverALiveTopicOnce(t *testing.T) {
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	first := startListenerProcess(t, binary, uri, "two-listeners", liveLaneEnv...)
	first.waitHealthy(t)
	second := startListenerProcess(t, binary, uri, "two-listeners", liveLaneEnv...)
	second.WaitForOutput(t, liveLaneLine, "subscribe failed, retrying")
	first.WaitForOutput(t, liveLaneLine)

	topic := "notifications.team." + id.New()
	serving := startServingPort(t, "ses_two_"+id.New(), http.StatusNoContent)
	registerServingSession(t, first, serving, "127.0.0.1", topic)
	role := "two-listeners-" + id.New()
	holder, received := holdRole(t, first, uri, role)
	second.awaitSessionsListed(t, serving.sessionID, holder)

	publisher := connectPublisher(t, uri)
	sent := map[string]int{}
	for index := range 3 {
		item := newEnvelope("agent", topic, fmt.Sprintf("live event %d of 3", index+1))
		item.SourceSession = "ses_publisher_" + id.New()
		publish(t, publisher, item)
		sent[item.Payload] = 0
	}
	for range 3 {
		text := serving.awaitPrompt(t, 2*time.Second)
		for message := range sent {
			if strings.Contains(text, message) {
				sent[message]++
			}
		}
	}
	serving.noPrompt(t, 2*time.Second)
	for message, count := range sent {
		if count != 1 {
			t.Fatalf("%q reached the session %d times, want once", message, count)
		}
	}

	roleMessage := newEnvelope("agent", contracts.RoleTopicPrefix+role, "a message for whoever holds the role")
	publish(t, publisher, roleMessage)
	requireOneRoleMessage(t, received, roleMessage.Topic, roleMessage.PayloadSummary, 2*time.Second)
}

// A delivery the listener refuses on a live topic is reported as every refusal is, on the topic's
// exceptions lane over core NATS, and the live lane reads that report back like any topic: a
// session subscribed to the exceptions lane is prompted with it. A report is never itself
// reported, so the failed delivery of that prompt (the watcher's port answers 500) publishes no
// second one. The refusal is a btw-mode message (frameDeliveryMode) to a subscriber that
// advertises no capability.
func TestALiveTopicExceptionIsReadBackOnce(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "exception-read-back", liveLaneEnv...)
	listener.waitHealthy(t)

	topic := "notifications.team." + id.New()
	refusing := startServingPort(t, "ses_refusing_"+id.New(), http.StatusNoContent)
	registerServingSession(t, listener, refusing, "127.0.0.1", topic)
	watcher := startServingPort(t, "ses_watcher_"+id.New(), http.StatusInternalServerError)
	registerServingSession(t, listener, watcher, "127.0.0.1", bus.ExceptionTopicPrefix+">")
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	exceptions, err := conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exceptions lanes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the exceptions subscription: %v", err)
	}

	item := newEnvelope("agent", topic, "a btw its subscriber cannot take")
	item.Payload = `{"delivery":{"mode":"btw"}}`
	publish(t, connectPublisher(t, uri), item)

	text := watcher.awaitPrompt(t, 2*time.Second)
	if !strings.Contains(text, "Topic: "+bus.ExceptionTopicPrefix+topic) || !strings.Contains(text, "delivery_failed") {
		t.Fatalf("the watcher's prompt is not the refusal's report on %s:\n%s", bus.ExceptionTopicPrefix+topic, text)
	}
	report, err := exceptions.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no report on the exceptions lanes: %v", err)
	}
	if report.Subject != bus.ExceptionTopicPrefix+topic {
		t.Fatalf("the report went to %s, want %s", report.Subject, bus.ExceptionTopicPrefix+topic)
	}
	if extra, err := exceptions.NextMsg(2 * time.Second); err == nil {
		t.Fatalf("a second report on %s: %s", extra.Subject, extra.Data)
	}
	refusing.noPrompt(t, time.Second)
}

// The live lane delivers only an envelope that is valid and names as its topic the subject it
// arrived on: bytes that are no envelope, an envelope the wire rules refuse (an empty urgency) and
// an envelope naming a topic other than its subject, which its session also subscribes to, reach
// no port. Core NATS keeps one publisher's order, so a valid envelope published after them being
// the first prompt shows they were dropped, not late.
func TestALiveLaneDropsAnInvalidEnvelope(t *testing.T) {
	uri := testnats.URL(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "invalid-envelope", liveLaneEnv...)
	listener.waitHealthy(t)

	topic, other := "notifications.team."+id.New(), "notifications.team."+id.New()
	serving := startServingPort(t, "ses_invalid_"+id.New(), http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic, other)
	publisher := connectPublisher(t, uri)

	if err := publisher.Conn.Publish(topic, []byte("not an envelope")); err != nil {
		t.Fatalf("publish bytes that are no envelope: %v", err)
	}
	var wire map[string]any
	encoded, err := json.Marshal(newEnvelope("agent", topic, "an envelope with an empty urgency"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	wire["urgency"] = ""
	if encoded, err = json.Marshal(wire); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Conn.Publish(topic, encoded); err != nil {
		t.Fatalf("publish an envelope the wire rules refuse: %v", err)
	}
	if err := publisher.PublishCoreTo(topic, newEnvelope("agent", other, "an envelope naming another topic")); err != nil {
		t.Fatalf("publish an envelope naming another topic: %v", err)
	}
	valid := newEnvelope("agent", topic, "a valid envelope after them")
	publish(t, publisher, valid)

	requirePromptCarries(t, serving.awaitPrompt(t, 2*time.Second), valid)
	serving.noPrompt(t, time.Second)
}

// A listener whose NATS user may subscribe to the role lanes and reply inboxes alone, as the
// shared hub's listener's grant allows, ends its start when told to open the live lane: NATS
// refuses the subscription, and the listener exits 1 with an ERROR naming the variable that asked
// for it and the subject, rather than run delivering none of those topics.
func TestAListenerWhoseGrantRefusesEveryTopicEndsItsStart(t *testing.T) {
	const machineID = "hub-grant"
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [ { user: listener, password: listener, permissions: { subscribe: { allow: ["_INBOX.>", %q] } } } ]
}
`, contracts.RoleTopicPrefix+"> "+bus.ListenerQueue(machineID))
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	uri = strings.Replace(uri, "nats://", "nats://listener:listener@", 1)

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID, "ENVOY_SUBSCRIBE_ALL_TOPICS=1")
	listener.WaitExit(t, "its start")
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1:\n%s", code, output)
	}
	for line := range strings.SplitSeq(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["level"] != "ERROR" {
			continue
		}
		if strings.Contains(fmt.Sprint(record["msg"]), "ENVOY_SUBSCRIBE_ALL_TOPICS") && record["subject"] == "notifications.>" {
			return
		}
	}
	t.Fatalf("no ERROR names ENVOY_SUBSCRIBE_ALL_TOPICS and the subject notifications.>:\n%s", output)
}

// Every machine's listener has a durable of its own, so each takes every inbox message off the
// stream, and a listener that does not own the session still tries to reach it (the registry path
// of session.HandleAgentMessage). Before INFRA-122 that listener dialled the owning listener's
// machine id, which is no address of a session whose recorded host differs from it (a pod beside
// a listener in a cluster, here the owner "inbox-owner" and a session on 127.0.0.1), so only the
// owner delivered. Reaching a session at its recorded host must not let a second listener on the
// bus that can reach that host deliver the same inbox message again: the session receives it once.
func TestAnInboxMessageReachesAServingPortOnceWithAnotherMachinesListenerOnTheBus(t *testing.T) {
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	owner := startListenerProcess(t, binary, uri, "inbox-owner", "ENVOY_HOST_BRIDGE=127.0.0.1")
	owner.waitHealthy(t)
	other := startListenerProcess(t, binary, uri, "inbox-other")
	other.waitHealthy(t)

	serving := startServingPort(t, "ses_inbox_"+id.New(), http.StatusNoContent)
	registerServingSession(t, owner, serving, "127.0.0.1", "notifications.team."+id.New())

	inbox := newEnvelope("agent", contracts.AgentSubject(serving.sessionID), "a message for this session alone")
	inbox.SourceSession = "ses_sender_" + id.New()
	publish(t, connectPublisher(t, uri), inbox)

	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), inbox)
	serving.noPrompt(t, 3*time.Second)
}

// A listener that does not own a session leaves each inbox message to it to the owner, which
// takes the same message off its own durable and delivers it: the non-owner reports nothing. Before
// the fix the non-owner read its refusal as a session missing everywhere and published a no_holder
// exception on the inbox's exceptions lane for a message the owner had delivered, one for each
// listener that does not own the session.
func TestANonOwningListenerLeavesAnInboxMessageToTheOwnerSilently(t *testing.T) {
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	owner := startListenerProcess(t, binary, uri, "silent-owner")
	owner.waitHealthy(t)
	other := startListenerProcess(t, binary, uri, "silent-other")
	other.waitHealthy(t)

	serving := startServingPort(t, "ses_silent_"+id.New(), http.StatusNoContent)
	registerServingSession(t, owner, serving, "127.0.0.1", "notifications.team."+id.New())
	other.awaitSessionsListed(t, serving.sessionID)
	exceptions := subscribeExceptions(t, uri)

	inbox := newEnvelope("agent", contracts.AgentSubject(serving.sessionID), "a message its owner delivers")
	inbox.SourceSession = "ses_sender_" + id.New()
	publish(t, connectPublisher(t, uri), inbox)

	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), inbox)
	serving.noPrompt(t, 3*time.Second)
	if report, err := exceptions.NextMsg(time.Second); err == nil {
		t.Fatalf("an exception was published for a delivered inbox message, on %s: %s", report.Subject, report.Data)
	}
}

// A session that registered through one listener and then re-registered through another, same
// session, port and host, keeps the first listener's MachineID on its stored subscription, so that
// listener is the only one matching its topics. It is also the session's owner, and delivers a
// stored topic to it once. Before the fix the owner was read from the session entry, which the
// re-registration moved to the second listener: the first matched the topic and refused the
// session, logging a failed delivery and retrying it until the durable gave up, while the second
// matched nothing, so the session was never prompted.
func TestARehomedSessionStillReceivesItsStoredTopicOnce(t *testing.T) {
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	first := startListenerProcess(t, binary, uri, "rehome-first")
	first.waitHealthy(t)
	second := startListenerProcess(t, binary, uri, "rehome-second")
	second.waitHealthy(t)

	topic := "notifications.github.acme.repo" + id.New() + ".pr.1"
	serving := startServingPort(t, "ses_rehome_"+id.New(), http.StatusNoContent)
	registerServingSession(t, first, serving, "127.0.0.1", topic)
	registerServingSession(t, second, serving, "127.0.0.1", topic)
	first.awaitSessionMachine(t, serving.sessionID, "rehome-second")

	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, connectPublisher(t, uri), item)

	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), item)
	serving.noPrompt(t, 3*time.Second)
	for _, listener := range []*listenerProcess{first, second} {
		if output := listener.Output.String(); strings.Contains(output, "listener delivery failed") {
			t.Fatalf("the listener on port %d logged a failed delivery:\n%s", listener.port, output)
		}
	}
}

// subscribeExceptions subscribes a connection of the test's own to every exceptions lane, core
// NATS and stored alike, and flushes so the server holds the subscription when it returns.
func subscribeExceptions(t *testing.T, uri string) *natsgo.Subscription {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	exceptions, err := conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exceptions lanes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the exceptions subscription: %v", err)
	}
	return exceptions
}

// awaitSessionMachine waits up to 10 s for listener's GET /v1/sessions to list sessionID as
// registered through machineID, so a delivery through this listener reads that session entry.
func (p *listenerProcess) awaitSessionMachine(t *testing.T, sessionID, machineID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := callListener(t, p.port, http.MethodGet, "/v1/sessions", "")
		var listed []struct {
			SessionID string `json:"session_id"`
			MachineID string `json:"machine_id"`
		}
		if status == http.StatusOK && json.Unmarshal([]byte(body), &listed) == nil {
			for _, row := range listed {
				if row.SessionID == sessionID && row.MachineID == machineID {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener on port %d does not list %s as registered through %s within 10s: %d %s", p.port, sessionID, machineID, status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
