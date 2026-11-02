package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A message whose delivery mode cannot be read is refused for a serving-port session on consumer
// delivery, and the session's LATER messages arrive all the same: the refusal leaves no attempt
// record naming a push in flight, so the relay holds nothing under it.
//
// TestServePortRelayRefusesAnUnadvertisedModeTheFanoutAlsoRefused proves the refused message is
// acked and reported once, which says the stream is not stalled. This says what that costs a
// session when it goes wrong: with the fan-out recording an attempt for its own refusal, the
// relay holds its copy, and everything stored behind it waits out attemptCacheTTL - five minutes
// in which the session is given nothing at all (INFRA-594).
func TestAnUnreadableDeliveryModeDoesNotStallTheSessionsLaterMessages(t *testing.T) {
	const (
		relayedPort = 4096
		corePort    = 4097
	)
	// Unowned, so no relay runs until the fan-out has decided both messages: the test orders the
	// two paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayed := "ses_mode_stall_relayed_" + id.New()
	core := "ses_mode_stall_core_" + id.New()
	topic := "notifications.github.acme.mode-stall-" + id.New() + ".mention"
	relayedPushes := make(chan string, 8)
	corePushes := make(chan string, 8)
	rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(request.URL.Host, strconv.Itoa(relayedPort)) {
			relayedPushes <- string(body)
		} else {
			corePushes <- string(body)
		}
		return listenerDeliveryResponse(http.StatusNoContent), nil
	})}
	exceptions, err := rig.harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + topic)
	if err != nil {
		t.Fatalf("subscribe to the topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := rig.harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}

	// The session under test: a serving port on consumer delivery, following the topic.
	rig.subscribe(t, `{"session_id":"`+relayed+`","port":`+strconv.Itoa(relayedPort)+`,"host":"127.0.0.1","topics":["`+topic+`"]}`)
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session's entry records delivery %q, want the consumer's", entry.Delivery)
	}
	// A second interest on the topic, a serving-port session of this machine on the core path, so
	// the durable path reads the message and fans it out over every interest.
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", core, err)
	}
	if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: corePort, MachineID: "test-machine", Host: "127.0.0.1", Capabilities: []string{"btw"}}); err != nil {
		t.Fatalf("register %s's entry: %v", core, err)
	}
	durable := "listener-mode-stall-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	// First, a message no session can be shown to advertise a mode for. Any publisher can send
	// this: the publish route reads the mode only for sends and role forwards.
	unreadable := listenerTestEnvelope(topic, "unreadable-mode-"+id.New())
	unreadable.Payload = `{"delivery":{"mode":"bogus"}}`
	first, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, unreadable))
	if err != nil {
		t.Fatalf("store the unreadable-mode message: %v", err)
	}
	awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
	awaitFanoutFinished(t, rig, durable, first.Sequence)

	// Then an ordinary message on the same topic, stored behind it.
	later := listenerTestEnvelope(topic, "later-"+id.New())
	second, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, later))
	if err != nil {
		t.Fatalf("store the later message: %v", err)
	}
	awaitFanoutFinished(t, rig, durable, second.Sequence)

	// The relay starts and must reach the later message. The bound is thirty seconds, where a
	// hold under an attempt record naming no push lasts attemptCacheTTL, five minutes.
	rig.relays.setOwned(true)
	select {
	case body := <-relayedPushes:
		if !strings.Contains(body, later.EventID) {
			t.Fatalf("the relayed session's port was pushed:\n%s\nwant the later message, %s", body, later.EventID)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the later message %s never reached the port: the unreadable-mode message before it stalled the session", later.EventID)
	}
	if reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second); len(reasons) != 1 || reasons[0] != "delivery_failed" {
		t.Fatalf("the exception lane of %s carried %v for the relayed session, want one delivery_failed", relayed, reasons)
	}
}
