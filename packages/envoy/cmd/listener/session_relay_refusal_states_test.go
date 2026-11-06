package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// A message on a followed topic that is refused -- its delivery mode cannot be read, which any
// publisher can send -- is reported exactly once for the session it was refused for, whatever
// that session's entry said when the fan-out judged it, and the session's later messages reach it
// all the same.
//
// The rows are the three entry states a refusal is judged against. A session's relay publishes
// every frame it pulls and judges none of them (sessionRelays.relayNATS), and the plugin drops a
// frame whose delivery mode it cannot read without a word, so a refusal nobody reports is a
// refusal nobody hears: the fan-out reports each of these itself (fanoutDelivery). What differs
// between the rows is only whether it also records the attempt, which is what makes an identical
// redelivery inside the window a dedupe skip rather than a second exception: it records one where
// its own resolution succeeded and it is the path that would deliver the message, and none where
// the resolution failed or the session's own consumer holds the message (fanoutRefusalOutcome).
func TestARefusalIsReportedOnceWhateverTheSessionsEntrySaid(t *testing.T) {
	for _, tc := range []struct {
		name string
		// beforeTheRefusal leaves the session's entry in the state the fan-out judges the refused
		// message against; nil leaves the entry its registration wrote.
		beforeTheRefusal func(t *testing.T, rig *relayRig, sessionID string)
		// registersAgain: the session registers once more after the refusal, which puts it back
		// on its consumer, whose start is still below the refused message.
		registersAgain bool
		// wantAttempt is whether the fan-out leaves an attempt record of the refusal behind.
		wantAttempt bool
	}{
		{
			// The session's own consumer holds the message, so the fan-out hands it to nobody:
			// it reports the refusal and records nothing, since a record here would have the
			// relay's own copy of the next event skipped as one already on its way.
			name: "a session on consumer delivery",
		},
		{
			// An entry lapses while its interest stays for at least ten minutes
			// (store.InterestReapGrace plus store.InterestRefreshAge) and its consumer for three
			// days (bus.SessionConsumerInactiveThreshold), so the fan-out judges a message for a
			// session it cannot resolve. A refusal whose resolution failed names a delivery that
			// was never made, and records nothing.
			name: "a session whose entry has lapsed and that registers again",
			beforeTheRefusal: func(t *testing.T, rig *relayRig, sessionID string) {
				t.Helper()
				if err := rig.harness.sessions.Delete(context.Background(), sessionID); err != nil {
					t.Fatalf("delete the entry of %s: %v", sessionID, err)
				}
			},
			registersAgain: true,
		},
		{
			// The entry a heartbeat tick whose consumer call failed transiently writes: self
			// subscribed, no delivery, the consumer still on the server
			// (sessionDelivery.refused, claim_wiring.go's sessionEntryFromSubscribe). The
			// fan-out resolves this session and would deliver to it itself, so it records the
			// attempt together with the exception it publishes.
			name: "a session whose entry a consumer-error tick wrote on the core path",
			beforeTheRefusal: func(t *testing.T, rig *relayRig, sessionID string) {
				t.Helper()
				if err := rig.harness.sessions.Put(sessionID, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"}); err != nil {
					t.Fatalf("put the entry of %s on the core path: %v", sessionID, err)
				}
			},
			registersAgain: true,
			wantAttempt:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unowned, so no relay runs until the fan-out has decided both messages: the test
			// orders the two paths by construction rather than by timing.
			rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
			relayed := "ses_refused_state_" + id.New()
			topic := "notifications.github.acme.refused-state-" + id.New() + ".mention"
			exceptions, err := rig.harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + topic)
			if err != nil {
				t.Fatalf("subscribe to the topic's exception lane: %v", err)
			}
			t.Cleanup(func() { _ = exceptions.Unsubscribe() })
			if err := rig.harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the exception probe: %v", err)
			}

			standIn := startSessionStandIn(t, rig.url, relayed, answerDone)
			registration := consumerRegistration(relayed, "epoch-1", topic)
			rig.subscribe(t, registration)
			if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer {
				t.Fatalf("the entry of %s records delivery %q, want the consumer's", relayed, entry.Delivery)
			}
			keepTheDurableReading(t, rig, topic)
			durable := "listener-refused-state-" + id.New()
			bindTestDurable(t, rig, durable, topic)
			if tc.beforeTheRefusal != nil {
				tc.beforeTheRefusal(t, rig, relayed)
			}

			unreadable := listenerTestEnvelope(topic, "refused-state-"+id.New())
			unreadable.Payload = `{"delivery":{"mode":"bogus"}}`
			first, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, unreadable))
			if err != nil {
				t.Fatalf("store the unreadable-mode message: %v", err)
			}
			awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
			awaitFanoutFinished(t, rig, durable, first.Sequence)
			if got := rig.harness.config.attemptCache.Seen(unreadable.DedupeKey, relayed); got != tc.wantAttempt {
				t.Fatalf("an attempt record of %s is there: %t, want %t", relayed, got, tc.wantAttempt)
			}

			if tc.registersAgain {
				rig.subscribe(t, registration)
				entry := rig.entry(t, relayed)
				if entry.Delivery != session.DeliveryConsumer || entry.ConsumerStart > first.Sequence {
					t.Fatalf("the entry of %s after it registered again records delivery %q and consumer start %d, want the consumer's and a start at or below %d",
						relayed, entry.Delivery, entry.ConsumerStart, first.Sequence)
				}
			}

			later := listenerTestEnvelope(topic, "refused-state-later-"+id.New())
			second, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, later))
			if err != nil {
				t.Fatalf("store the later message: %v", err)
			}
			awaitFanoutFinished(t, rig, durable, second.Sequence)

			// The relay starts and must reach the later message, which is stored behind the
			// refused one: the refusal is the fan-out's to report and nothing about it may stop
			// the session's consumer.
			rig.relays.setOwned(true)
			awaitRelayFrame(t, standIn, later, 20*time.Second)
			reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second)
			if len(reasons) != 1 || reasons[0] != "delivery_failed" {
				t.Fatalf("the exception lane of %s carried %v, want one delivery_failed for the one refused message", relayed, reasons)
			}
		})
	}
}

// awaitRelayFrame waits for the frame carrying item among those the session's stand-in receives,
// failing the test when none arrives within the bound. A relay publishes every frame unjudged, so
// the frames before it are the ones the relay refused nothing of.
func awaitRelayFrame(t *testing.T, standIn *sessionStandIn, item contracts.Envelope, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case frame := <-standIn.frames:
			if frame.item.EventID == item.EventID {
				return
			}
		case <-deadline:
			t.Fatalf("the frame carrying %s never reached the session within %s", item.EventID, within)
		}
	}
}
