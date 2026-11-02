package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
)

// A session's record outlives its answering by a long way: the bucket's maximum age is two hours
// where the live window is minutes, so a record read without its state says a stalled session is
// still a serving port. Both push paths below read the state instead, and push to a live session
// only; before that they read the record alone (sessions.Get) and both tests fail, the first with
// the verdict "push it" and the second with a push to the dead port.

// silentAcker answers a relayed message as the consumer's acker does and counts nothing: the
// relay push under test keeps the message in progress and never settles it.
type silentAcker struct{ inProgress int }

func (a *silentAcker) Ack() error                       { return nil }
func (a *silentAcker) InProgress() error                { a.inProgress++; return nil }
func (a *silentAcker) NakWithDelay(time.Duration) error { return nil }

// recordingDeliverer is a deliverer whose pushes land in a channel instead of on a port.
func recordingDeliverer(pushed chan<- string) *session.Deliverer {
	return &session.Deliverer{
		MachineID:    "test-machine",
		RequestLimit: 2 * time.Second,
		HTTPClient: &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
			pushed <- request.URL.String()
			return listenerDeliveryResponse(http.StatusNoContent), nil
		})},
	}
}

// The durable path's worker pushes to a serving port only while that session answers. A session
// that has stopped heartbeating keeps its record, and pushing to it means a failed HTTP call and
// a WARN at every retry until the gone horizon, so the message goes back to the durable instead
// and is delivered again under whatever state the session is in then.
func TestAServingPortPushIsHandedBackOnceItsSessionStopsAnswering(t *testing.T) {
	d, now := stalledSessionsRig(t)
	const sessionID = "ses_serving_port_stalls"
	register(t, d, sessionID, session.SessionEntry{Port: 4321, Host: "127.0.0.1", MachineID: "test-machine"})
	pushed := make(chan string, 4)
	workers := &servePortWorkers{cfg: listenerDeliveryHandlerConfig{
		registry:  d.registry,
		sessions:  d.sessions,
		machineID: "test-machine",
		deliverer: recordingDeliverer(pushed),
	}}
	push := servePortPush{
		item:      listenerTestEnvelope("notifications.test.serving-port-stall", "serving-port-stall"),
		sessionID: sessionID,
	}

	entry, verdict := workers.target(push)
	if verdict != servePortPushIt || entry.Port != 4321 {
		t.Fatalf("a live serving port answers verdict %d and port %d, want the push and 4321", verdict, entry.Port)
	}

	// Past the live window and inside the gone horizon: the record stands, and the session is
	// not answering.
	now.advance(90 * time.Second)
	if _, state := d.sessions.Lookup(sessionID); state != session.Unresponsive {
		t.Fatalf("%s reads %s, want unresponsive: the check below would prove nothing", sessionID, state)
	}

	if _, verdict := workers.target(push); verdict != servePortGone {
		t.Fatalf("a serving port that stopped answering answers verdict %d, want %d, the message handed back to the durable",
			verdict, servePortGone)
	}
}

// The relay of a session's own consumer pushes on the same rule. A session that has stopped
// answering is refused exactly as one with no record is: the message stays unacked in its
// consumer, nothing reaches its port, and the backoff asks again, so the push is made as soon as
// the session answers again.
func TestASessionRelayStopsPushingOnceItsSessionStopsAnswering(t *testing.T) {
	d, now := stalledSessionsRig(t)
	const sessionID = "ses_relay_stalls"
	register(t, d, sessionID, session.SessionEntry{Port: 4322, Host: "127.0.0.1", MachineID: "test-machine"})
	pushed := make(chan string, 4)
	relays := &sessionRelays{
		deliverer: recordingDeliverer(pushed),
		rules:     followedTopicRules{sessions: d.sessions},
	}
	item := listenerTestEnvelope("notifications.test.relay-stall", "relay-stall")
	msg := bus.SessionMessage{SessionAcker: &silentAcker{}}

	if err := relays.push(context.Background(), sessionID, item, msg); err != nil {
		t.Fatalf("push to the live session: %v", err)
	}
	select {
	case got := <-pushed:
		if !strings.Contains(got, sessionID) {
			t.Fatalf("the live session's push went to %s, want its own port", got)
		}
	default:
		t.Fatal("the live session's port was never pushed")
	}

	now.advance(90 * time.Second)
	if _, state := d.sessions.Lookup(sessionID); state != session.Unresponsive {
		t.Fatalf("%s reads %s, want unresponsive: the check below would prove nothing", sessionID, state)
	}

	err := relays.push(context.Background(), sessionID, item, msg)
	if err == nil {
		t.Fatal("the relay pushed to a session that stopped answering, where every such push fails at a dead port and is retried with a WARN until the gone horizon")
	}
	if !strings.Contains(err.Error(), string(session.Unresponsive)) {
		t.Fatalf("the refusal reads %q, want it to name the state that refused the push", err)
	}
	select {
	case got := <-pushed:
		t.Fatalf("a stalled session's port was pushed at %s", got)
	default:
	}
}
