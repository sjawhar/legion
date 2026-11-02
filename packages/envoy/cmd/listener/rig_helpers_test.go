package main

// The rig several of this package's tests share: a bus client of the test's own, the envelopes it
// publishes through it, and the wait for a started listener to list a session as registered
// through a machine. They are here, in a file that tests nothing itself, because the tests that
// use them are spread across this package and no one of those files owns them.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// connectPublisher opens a bus client of the test's own, apart from the listener's: a publish
// through it reaches NATS as one from any session holding its own connection does, core NATS for a
// topic the stream does not store and JetStream for one it does (bus.Client.Publish).
func connectPublisher(t *testing.T, uri string) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{uri}, bus.WithName("listener-test-publisher"))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// newEnvelope is a valid envelope from source on topic carrying message, with ids of its own. Its
// dedupe key keeps the prefix it was written with: role_lane_test.go compares a role forward's
// dedupe key against that prefix, so the two are one choice.
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
