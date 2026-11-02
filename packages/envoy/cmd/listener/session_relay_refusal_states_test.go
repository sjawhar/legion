package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A message on a followed topic that is refused -- its delivery mode cannot be read, which any
// publisher can send -- never leaves the session's relay holding anything, whatever the session's
// entry said when the fan-out judged it, and the session's later messages arrive at once rather
// than waiting out attemptCacheTTL.
//
// The rows are the four entry states a refusal is judged against, each of which left that stall
// behind in its turn (INFRA-594, review rounds 5 to 8). The relay no longer answers them one at a
// time: it judges as if no attempt record stood (judgeFollowedTopicWithoutAttempt) and reads the
// record only to tell a push in flight, which it holds its copy for, from a refusal the fan-out
// recorded as it reported it, which it acks unreported (sessionRelays.followedTopicVerdict). So a
// record is a hold only where the relay would otherwise push.
//
// Who reports the refusal, and how often, follows from the fan-out's rules rather than from this
// one, and each row pins what the code does: one report, except for a session whose entry is gone,
// which the fan-out cannot resolve and records nothing for, so the relay reports the same refusal
// again when the session is back -- two reports of one refusal, the exception dedupe that branch
// gives up to leave the relay nothing to hold.
func TestARefusedMessageNeverHoldsTheRelayWhateverTheEntrySaid(t *testing.T) {
	const (
		relayedPort = 4096
		corePort    = 4097
	)
	for _, tc := range []struct {
		name string
		// portless: the session registers in consumer mode with no serving port, so its relay
		// publishes frames rather than pushing (sessionRelays.relayNATS).
		portless bool
		// beforeTheRefusal leaves the session's entry in the state the fan-out judges the refused
		// message against; nil leaves the entry its registration wrote.
		beforeTheRefusal func(t *testing.T, rig *relayRig, sessionID string)
		// registersAgain: the session registers once more after the refusal, which puts it back
		// on its consumer, whose start is still below the refused message.
		registersAgain bool
		// wantExceptions: the delivery_failed exceptions the session is sent for the one refused
		// message.
		wantExceptions int
	}{
		{
			// The fan-out leaves this session's refusal to its relay, which judges what it pushes
			// and reports it: one report, no record, nothing held (review round 5).
			name:           "a serving-port session on consumer delivery",
			wantExceptions: 1,
		},
		{
			// The portless relay publishes every frame unjudged and the plugin drops a frame
			// whose mode it cannot read without a word, so the fan-out reports this refusal
			// itself and records nothing for a session it does not push to (review round 6). The
			// later message reaches the session as a relay frame, not as a push.
			name:           "a portless session on consumer delivery",
			portless:       true,
			wantExceptions: 1,
		},
		{
			// The fan-out cannot resolve a session whose entry is gone, so it records nothing and
			// the relay, finding no record, reports the same refusal a second time once the
			// session has registered again (review round 7). Two reports, by design: the price of
			// leaving that relay nothing to hold.
			name: "a session with no entry that registers again on its serving port",
			beforeTheRefusal: func(t *testing.T, rig *relayRig, sessionID string) {
				t.Helper()
				if err := rig.harness.sessions.Delete(context.Background(), sessionID); err != nil {
					t.Fatalf("delete the entry of %s: %v", sessionID, err)
				}
			},
			registersAgain: true,
			wantExceptions: 2,
		},
		{
			// The entry a heartbeat tick whose consumer call failed transiently writes: the
			// serving port, no delivery, the consumer still on the server. The fan-out resolves
			// and refuses this session itself, so it records the attempt together with its
			// exception, and the relay acks that message unreported once the next tick puts the
			// session back on its consumer (review round 8).
			name: "a serving-port session whose entry a consumer-error tick wrote on the core path",
			beforeTheRefusal: func(t *testing.T, rig *relayRig, sessionID string) {
				t.Helper()
				if err := rig.harness.sessions.Put(sessionID, session.SessionEntry{Port: relayedPort, MachineID: "test-machine", Host: "127.0.0.1"}); err != nil {
					t.Fatalf("put the entry of %s on the core path: %v", sessionID, err)
				}
			},
			registersAgain: true,
			wantExceptions: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unowned, so no relay runs until the fan-out has decided both messages: the test
			// orders the two paths by construction rather than by timing.
			rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
			relayed := "ses_refused_state_" + id.New()
			core := "ses_refused_state_core_" + id.New()
			topic := "notifications.github.acme.refused-state-" + id.New() + ".mention"
			relayedPushes := make(chan string, 8)
			rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(request.URL.Host, strconv.Itoa(relayedPort)) {
					relayedPushes <- string(body)
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

			var standIn *sessionStandIn
			registration := `{"session_id":"` + relayed + `","port":` + strconv.Itoa(relayedPort) + `,"host":"127.0.0.1","topics":["` + topic + `"]}`
			if tc.portless {
				standIn = startSessionStandIn(t, rig.url, relayed, answerDone)
				registration = consumerRegistration(relayed, "epoch-1", topic)
			}
			rig.subscribe(t, registration)
			if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer {
				t.Fatalf("the entry of %s records delivery %q, want the consumer's", relayed, entry.Delivery)
			}
			// A second interest on the topic, a serving-port session of this machine on the core
			// path, so the durable path must deliver the message and fans it out over every
			// interest.
			if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
				t.Fatalf("register %s's interest: %v", core, err)
			}
			if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: corePort, MachineID: "test-machine", Host: "127.0.0.1", Capabilities: []string{"btw"}}); err != nil {
				t.Fatalf("register %s's entry: %v", core, err)
			}
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
			// The core-path session is refused the same mode, so the line is written whichever
			// row leaves this session's refusal to its relay.
			awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
			awaitFanoutFinished(t, rig, durable, first.Sequence)

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

			// The relay starts and must reach the later message. The bound is twenty seconds,
			// where a hold under an attempt record lasts attemptCacheTTL, five minutes.
			rig.relays.setOwned(true)
			if tc.portless {
				awaitRelayFrame(t, standIn, later, 20*time.Second)
			} else {
				select {
				case body := <-relayedPushes:
					if !strings.Contains(body, later.EventID) {
						t.Fatalf("the port of %s was pushed:\n%s\nwant the later message, %s", relayed, body, later.EventID)
					}
				case <-time.After(20 * time.Second):
					t.Fatalf("the later message %s never reached the port of %s: the refused message before it is held", later.EventID, relayed)
				}
			}
			if logs := rig.harness.logs.String(); strings.Contains(logs, "session relay holds a message another path is pushing") {
				t.Fatalf("the relay of %s held a message, want none held: no push of the refused message is in flight:\n%s", relayed, logs)
			}
			reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second)
			if len(reasons) != tc.wantExceptions {
				t.Fatalf("the exception lane of %s carried %v, want %d delivery_failed for the one refused message", relayed, reasons, tc.wantExceptions)
			}
			for _, reason := range reasons {
				if reason != "delivery_failed" {
					t.Fatalf("the exception lane of %s carried %v, want every one a delivery_failed", relayed, reasons)
				}
			}
		})
	}
}

// awaitRelayFrame waits for the frame carrying item among those the session's stand-in receives,
// failing the test when none arrives within the bound. A portless session's relay publishes every
// frame unjudged, so the frames before it are the ones the relay refused nothing of.
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
			t.Fatalf("the message %s never reached the session as a relay frame within %s", item.EventID, within)
		}
	}
}
