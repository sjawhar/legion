package main

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A PORTLESS session on consumer delivery, whose own consumer holds
// the stored message, is reported exactly one delivery_failed for a mode that cannot be read.
//
// This is the case review round 6's fix changes, now the `!relayJudges` of the fan-out's refusal
// branch (`relayJudges := relayed && target.Port > 0`). The relay of a portless session publishes
// frames and judges nothing, so a refusal left to it is reported by nobody.
func TestAPortlessConsumerSessionIsReportedAnUnreadableDeliveryMode(t *testing.T) {
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	portless := "ses_portless_" + id.New()
	topic := "notifications.github.acme.portless-" + id.New() + ".mention"
	rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(request.Body)
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

	rig.subscribe(t, consumerRegistration(portless, "epoch-1", topic))
	entry := rig.entry(t, portless)
	if entry.Delivery != session.DeliveryConsumer || entry.Port != 0 {
		t.Fatalf("the entry of %s records delivery %q and port %d, want the consumer's and none", portless, entry.Delivery, entry.Port)
	}

	// A second recipient on the topic, a serving-port session of this machine on the core path:
	// the durable path must deliver to it, so it reads the message and fans it out over every
	// interest, the portless consumer session included. Without it the durable acknowledges the
	// message unread, since a session whose consumer holds it takes it itself.
	core := "ses_portless_core_" + id.New()
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", core, err)
	}
	if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: 4097, MachineID: "test-machine", Host: "127.0.0.1", Capabilities: []string{"btw"}}); err != nil {
		t.Fatalf("register %s's entry: %v", core, err)
	}

	durable := "listener-portless-refusal-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	item := listenerTestEnvelope(topic, "portless-unreadable-"+id.New())
	item.Payload = `{"delivery":{"mode":"bogus"}}`
	ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store the message: %v", err)
	}
	awaitFanoutFinished(t, rig, durable, ack.Sequence)

	if reasons := deliveryExceptionsFor(t, exceptions, portless, 3*time.Second); len(reasons) != 1 || reasons[0] != "delivery_failed" {
		t.Fatalf("the exception lane of %s carried %v, want one delivery_failed: nobody else reports a portless session's refusal", portless, reasons)
	}
}
