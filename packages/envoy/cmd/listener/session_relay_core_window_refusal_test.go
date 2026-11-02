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

// A message whose delivery mode cannot be read, sent to a serving-port session whose entry one
// tick wrote on the core path, is reported once and leaves no hold for the relay of that session:
// its later messages arrive at once rather than waiting out attemptCacheTTL.
//
// The window is the ordinary one. A session re-registers on every heartbeat, and a tick whose
// consumer call fails transiently answers core, which writes the entry with no delivery
// (claim_wiring.go's sessionEntryFromSubscribe); the consumer itself stays, and the next tick
// finds it again. A message on a followed topic that arrives in that window -- an unreadable
// delivery mode, which any publisher can send -- is one the fan-out resolves and refuses itself,
// so it records the attempt together with the exception it publishes.
//
// The relay reads that message once the session is back on its consumer, whose start is at or
// below it. It judges as if no record stood (judgeFollowedTopicWithoutAttempt), finds the refusal
// the fan-out has already reported, and acks the message unpushed and unreported; only a message
// it would otherwise push is held under a record (holdWhilePushedElsewhere). With the relay
// finding the record before the mode, it held this message instead, and the session's later
// messages behind it, for the five minutes of attemptCacheTTL (INFRA-594).
//
// TestAnUnreadableDeliveryModeToASessionWithNoEntryLeavesNoHoldForItsRelay is the same stall
// through a session with no entry, and TestARefusedMessageNeverHoldsTheRelayWhateverTheEntrySaid
// drives every entry state through it.
func TestAnUnreadableDeliveryModeInACoreWindowLeavesNoHoldForItsRelay(t *testing.T) {
	const (
		relayedPort = 4096
		corePort    = 4097
	)
	// Unowned, so no relay runs until the fan-out has decided both messages: the test orders the
	// two paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayed := "ses_core_window_relayed_" + id.New()
	core := "ses_core_window_core_" + id.New()
	topic := "notifications.github.acme.core-window-" + id.New() + ".mention"
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

	// The session under test: a serving port on consumer delivery, following the topic. Its
	// consumer is created here, below both messages below, and outlives the core-path entry.
	registration := `{"session_id":"` + relayed + `","port":` + strconv.Itoa(relayedPort) + `,"host":"127.0.0.1","topics":["` + topic + `"]}`
	rig.subscribe(t, registration)
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer || entry.Port != relayedPort {
		t.Fatalf("the entry of %s records delivery %q and port %d, want the consumer's and %d", relayed, entry.Delivery, entry.Port, relayedPort)
	}
	// A second interest on the topic, a serving-port session of this machine on the core path, so
	// the durable path must deliver the message and fans it out over every interest.
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", core, err)
	}
	if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: corePort, MachineID: "test-machine", Host: "127.0.0.1", Capabilities: []string{"btw"}}); err != nil {
		t.Fatalf("register %s's entry: %v", core, err)
	}
	durable := "listener-core-window-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	// The entry a tick whose consumer call failed writes: the serving port, no delivery and no
	// capabilities, while the consumer the registration above created stays on the server.
	if err := rig.harness.sessions.Put(relayed, session.SessionEntry{Port: relayedPort, MachineID: "test-machine", Host: "127.0.0.1"}); err != nil {
		t.Fatalf("put the entry of %s on the core path: %v", relayed, err)
	}
	if entry := rig.entry(t, relayed); entry.Delivery != "" {
		t.Fatalf("the entry of %s records delivery %q, want none: this test is about the window a consumer-error tick opens", relayed, entry.Delivery)
	}

	unreadable := listenerTestEnvelope(topic, "core-window-unreadable-"+id.New())
	unreadable.Payload = `{"delivery":{"mode":"bogus"}}`
	first, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, unreadable))
	if err != nil {
		t.Fatalf("store the unreadable-mode message: %v", err)
	}
	awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
	awaitFanoutFinished(t, rig, durable, first.Sequence)

	// The fan-out resolved this session and refused it itself, so it recorded the attempt as it
	// published its exception: this is the state the relay must survive.
	if !rig.harness.config.attemptCache.Seen(unreadable.DedupeKey, relayed) {
		t.Fatalf("the fan-out recorded no attempt of %s for a refusal it resolved and reported: the state this test is about is not the one it set up", relayed)
	}

	// The next tick finds the consumer again, on the same port and the same start, which is below
	// the message above.
	rig.subscribe(t, registration)
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer || entry.Port != relayedPort {
		t.Fatalf("the entry of %s after it registered again records delivery %q and port %d, want the consumer's and %d", relayed, entry.Delivery, entry.Port, relayedPort)
	}
	if start, stored := rig.entry(t, relayed).ConsumerStart, first.Sequence; start > stored {
		t.Fatalf("the consumer of %s starts at %d, past the unreadable-mode message at %d: the relay would never read the message this test is about", relayed, start, stored)
	}

	later := listenerTestEnvelope(topic, "core-window-later-"+id.New())
	second, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, later))
	if err != nil {
		t.Fatalf("store the later message: %v", err)
	}
	awaitFanoutFinished(t, rig, durable, second.Sequence)
	select {
	case body := <-corePushes:
		if !strings.Contains(body, later.EventID) {
			t.Fatalf("the core session's port was pushed:\n%s\nwant the later message, %s", body, later.EventID)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the later message %s never reached the core session's port", later.EventID)
	}

	// The relay starts and must reach the later message. The bound is twenty seconds, where a
	// hold under the refusal's attempt record lasts attemptCacheTTL, five minutes.
	rig.relays.setOwned(true)
	select {
	case body := <-relayedPushes:
		if !strings.Contains(body, later.EventID) {
			t.Fatalf("the relayed session's port was pushed:\n%s\nwant the later message, %s", body, later.EventID)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the later message %s never reached the port of %s: the unreadable-mode message before it is held under the attempt record the fan-out left", later.EventID, relayed)
	}
	if logs := rig.harness.logs.String(); strings.Contains(logs, "session relay holds a message another path is pushing") {
		t.Fatalf("the relay of %s held a message, want none held: the record names a refusal the fan-out reported, not a push in flight:\n%s", relayed, logs)
	}
	// The fan-out's one report, and none from the relay, which reads the record as the report it
	// was written with.
	if reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second); len(reasons) != 1 || reasons[0] != "delivery_failed" {
		t.Fatalf("the exception lane of %s carried %v, want one delivery_failed: the fan-out reports this refusal and the relay reports it no second time", relayed, reasons)
	}
}
