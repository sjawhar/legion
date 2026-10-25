package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
// change's head both pass.

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
