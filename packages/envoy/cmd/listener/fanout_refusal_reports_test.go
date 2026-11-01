package main

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The fan-out reports its own refusal of a message on a followed topic for every recipient it
// delivers to: a session on the core path, portless or on a serving port, and one whose entry is
// gone. Each refusal records its recipient before publishing its exception, so an identical
// redelivery inside the attempt window is a dedupe skip rather than a second exception
// (TestListenerDeliveryHandler_FanoutRefusalDedupesAfterExceptionDelivery). Two tests already
// hold that for a serving-port recipient on the core path: an unadvertised mode refused for one
// of them leaves a capable one its message
// (TestListenerDeliveryHandler_FanoutRefusesUnadvertisedDeliveryMode), and a mode that cannot be
// read is refused for every one of them, whatever each advertises
// (TestListenerDeliveryHandler_FanoutRefusesUnreadableOrAmbiguousDeliveryMode). The recipients
// below are the ones they leave: a portless session on the core path, and one with no entry.
//
// None of them is on consumer delivery, so this test pins what the fan-out does where no consumer
// relays the message: the two core-path rows pass on the tree before round 6's fix as after it,
// and are not evidence for that fix. The branch the fix changes, a portless consumer session's
// refusal, is pinned by TestTheFanoutReportsARefusalOfAPortlessSessionOnConsumerDelivery and
// TestAPortlessConsumerSessionIsReportedAnUnreadableDeliveryMode, which fail on that tree.
//
// A session with no entry leaves no attempt record whichever refusal it meets (INFRA-594): a
// mode it does not advertise cannot be judged against an entry there is none of, so the message
// is taken and the resolution that then fails records nothing, and a mode that cannot be read is
// refused before the entry is read at all but recorded only where the resolution succeeded. So
// nothing is left for the relay of a session that registers again to hold a later copy under
// (TestAnUnreadableDeliveryModeToASessionWithNoEntryLeavesNoHoldForItsRelay).
func TestTheFanoutReportsItsOwnRefusalsForTheSessionsItDeliversTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		// entry is the recipient's session entry, nil for a recipient that has none.
		entry   *session.SessionEntry
		payload string
		// wantException is whether the fan-out publishes a delivery_failed exception naming the
		// recipient, and wantLog the line it writes for it.
		wantException bool
		wantLog       string
		// wantAttempt is whether an attempt record of the recipient is left behind, which is what
		// makes a redelivery inside the window a dedupe skip rather than a second exception.
		wantAttempt bool
		// core hands the message to the core lane rather than to the durable, for a recipient the
		// durable path never fans out to: a portless self-subscribed session takes a stored
		// message on its own subscription, so the durable acknowledges it unread
		// (durableNeedsDelivery, takesStoredMessagesItself) and only a live or core message
		// reaches the fan-out for such a session.
		core bool
	}{
		{
			name:          "a portless session on the core path does not advertise the mode",
			entry:         &session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"},
			payload:       `{"delivery":{"mode":"btw"}}`,
			wantException: true,
			wantLog:       "listener subscriber does not advertise delivery mode",
			wantAttempt:   true,
			core:          true,
		},
		{
			name:          "a portless session on the core path is sent a mode that cannot be read",
			entry:         &session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Capabilities: []string{"btw"}},
			payload:       `{"delivery":{"mode":"bogus"}}`,
			wantException: true,
			wantLog:       "listener delivery mode cannot be read unambiguously",
			wantAttempt:   true,
			core:          true,
		},
		{
			// Refused, since judgeFollowedTopic reads a mode nobody can advertise before it looks
			// for an entry, but recorded nowhere: the resolution of this recipient fails
			// (session.ErrNoSessionRecord), and an attempt record naming a push that was never
			// made is one the session's own relay would hold this message under once the session
			// registers again on a serving port.
			name:          "a session with no entry is sent a mode that cannot be read",
			payload:       `{"delivery":{"mode":"bogus"}}`,
			wantException: true,
			wantLog:       "listener delivery mode cannot be read unambiguously",
		},
		{
			// No entry is no refusal: a mode nobody advertises cannot be judged against an entry
			// there is none of (judgeFollowedTopic), so the message is taken and the resolution
			// fails instead, which records nothing.
			name:    "a session with no entry cannot be judged against a mode it does not advertise",
			payload: `{"delivery":{"mode":"btw"}}`,
			wantLog: "listener delivery failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pushes atomic.Int32
			harness := newListenerDeliveryHarness(t, listenerTransport(func(*http.Request) (*http.Response, error) {
				pushes.Add(1)
				return listenerDeliveryResponse(http.StatusNoContent), nil
			}))
			sessionID := "ses_fanout_refusal_" + id.New()
			topic := "notifications.github.acme.fanout-refusal-" + id.New() + ".mention"
			if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
				t.Fatalf("register %s's interest: %v", sessionID, err)
			}
			if tc.entry != nil {
				if err := harness.sessions.Put(sessionID, *tc.entry); err != nil {
					t.Fatalf("register %s's entry: %v", sessionID, err)
				}
			}
			probe := exceptionProbe(t, harness.client)

			handler := harness.handler
			if tc.core {
				handler = harness.coreHandler
			}
			item := listenerTestEnvelope(topic, "fanout-refusal-"+id.New())
			item.Payload = tc.payload
			handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
			if err := harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the exception the fan-out published: %v", err)
			}

			if tc.wantException {
				if recipient := exceptionRecipient(t, assertDeliveryException(t, probe, item, "delivery_failed")); recipient != sessionID {
					t.Fatalf("the exception names recipient %q, want %q", recipient, sessionID)
				}
			} else {
				noException(t, probe)
			}
			if got := pushes.Load(); got != 0 {
				t.Fatalf("the fan-out pushed %s %d times, want none", sessionID, got)
			}
			if logs := harness.logs.String(); !strings.Contains(logs, tc.wantLog) {
				t.Fatalf("the fan-out never logged %q:\n%s", tc.wantLog, logs)
			}
			if got := harness.config.attemptCache.Seen(item.DedupeKey, sessionID); got != tc.wantAttempt {
				t.Fatalf("an attempt record of %s is there: %t, want %t", sessionID, got, tc.wantAttempt)
			}
		})
	}
}

// A message on a followed topic that a portless session on consumer delivery cannot be given --
// its targeted delivery mode is one the session does not advertise, or one no session can be
// shown to advertise because it cannot be read unambiguously -- is reported by the durable path's
// fan-out, with one delivery_failed exception for that session.
//
// Only a serving-port session's relay judges what it pushes
// (sessionRelays.followedTopicVerdict), and the fan-out leaves that session's refusals to it
// (TestServePortRelayRefusesAnUnadvertisedModeTheFanoutAlsoRefused). A portless session's relay
// publishes every frame unjudged (sessionRelays.relayNATS) and the plugin drops a frame whose
// delivery mode it cannot read without a word, so a refusal left to that relay would be reported
// by nobody. The fan-out records no attempt for it: judgeFollowedTopic is what reads such a
// record back, and the relay of a serving-port session's consumer applies that same judgement,
// so a record would make the relay of this session hold its copy -- and the session's later
// messages behind it -- until it expired (attemptCacheTTL) once the session is on a serving port
// (INFRA-594).
func TestTheFanoutReportsARefusalOfAPortlessSessionOnConsumerDelivery(t *testing.T) {
	const corePort = 4097
	for _, tc := range []struct {
		name string
		// payload is what the publisher sends, and coreTakesIt whether the core-path session,
		// which advertises "btw", is given the message: a mode that cannot be read is refused for
		// every session, so that one is refused too.
		payload     string
		coreTakesIt bool
	}{
		{
			name:        "the session does not advertise the mode",
			payload:     `{"delivery":{"mode":"btw"}}`,
			coreTakesIt: true,
		},
		{
			// Any publisher can send this: the publish route reads the mode only for sends and
			// role forwards.
			name:    "the mode cannot be read",
			payload: `{"delivery":{"mode":"bogus"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unowned, so no relay runs: the fan-out is the only path that reads the message, and
			// the test orders them by construction rather than by timing.
			rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
			relayed := "ses_portless_relayed_" + id.New()
			core := "ses_portless_core_" + id.New()
			topic := "notifications.github.acme.portless-mode-" + id.New() + ".mention"
			corePushes := make(chan string, 8)
			rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				corePushes <- string(body)
				return listenerDeliveryResponse(http.StatusNoContent), nil
			})}
			exceptions := exceptionProbe(t, rig.harness.client)

			// The session under test: no port, on consumer delivery, following the topic, and
			// advertising no delivery mode at all.
			rig.subscribe(t, consumerRegistration(relayed, "epoch-1", topic))
			if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer || entry.Port != 0 {
				t.Fatalf("the session's entry records delivery %q at port %d, want the consumer's at no port", entry.Delivery, entry.Port)
			}
			// A second interest on the topic, a serving-port session of this machine on the core
			// path that does advertise the mode: the durable path reads the message for it and
			// fans it out over every interest, this machine's consumer-delivery session included.
			if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
				t.Fatalf("register %s's interest: %v", core, err)
			}
			if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: corePort, MachineID: "test-machine", Host: "127.0.0.1", Capabilities: []string{"btw"}}); err != nil {
				t.Fatalf("register %s's entry: %v", core, err)
			}
			durable := "listener-portless-mode-" + id.New()
			bindTestDurable(t, rig, durable, topic)

			item := listenerTestEnvelope(topic, "portless-mode-"+id.New())
			item.Payload = tc.payload
			ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
			if err != nil {
				t.Fatalf("store the message: %v", err)
			}
			if tc.coreTakesIt {
				select {
				case body := <-corePushes:
					if !strings.Contains(body, item.EventID) {
						t.Fatalf("the core-path session's port was pushed:\n%s\nwant %s", body, item.EventID)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("the durable path never pushed the message to the core-path session")
				}
			} else {
				awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
			}
			awaitFanoutFinished(t, rig, durable, ack.Sequence)

			if reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second); len(reasons) != 1 || reasons[0] != "delivery_failed" {
				t.Fatalf("the exception lanes carried %v for the portless session, want one delivery_failed: its relay judges nothing, so the fan-out reports the refusal", reasons)
			}
			if rig.harness.config.attemptCache.Seen(item.DedupeKey, relayed) {
				t.Fatalf("the fan-out recorded an attempt for %s: a relay of that session on a serving port would hold its copy under it for attemptCacheTTL", relayed)
			}
		})
	}
}

// A message on a followed topic a portless session on consumer delivery takes reaches it as a
// relay frame when another interest on that topic sends the same message down the durable path:
// the session's consumer holds it and its relay is what sends it, so the fan-out pushes nothing
// and records neither an attempt nor a dedupe key for it -- records the relay of the same session
// on a serving port would hold its copy under, or ack it by (INFRA-594).
func TestThePortlessRelaySendsAFollowedTopicTheDurablePathAlsoReads(t *testing.T) {
	const corePort = 4097
	// Unowned, so no relay runs until the fan-out has read the message: the test orders the two
	// paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayed := "ses_portless_take_" + id.New()
	core := "ses_portless_take_core_" + id.New()
	topic := "notifications.github.acme.portless-take-" + id.New() + ".mention"
	corePushes := make(chan string, 8)
	rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		corePushes <- string(body)
		return listenerDeliveryResponse(http.StatusNoContent), nil
	})}

	standIn := startSessionStandIn(t, rig.url, relayed, answerDone)
	rig.subscribe(t, consumerRegistration(relayed, "epoch-1", topic))
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer || entry.Port != 0 {
		t.Fatalf("the session's entry records delivery %q at port %d, want the consumer's at no port", entry.Delivery, entry.Port)
	}
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: core, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", core, err)
	}
	if err := rig.harness.sessions.Put(core, session.SessionEntry{Port: corePort, MachineID: "test-machine", Host: "127.0.0.1"}); err != nil {
		t.Fatalf("register %s's entry: %v", core, err)
	}
	durable := "listener-portless-take-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	item := listenerTestEnvelope(topic, "portless-take-"+id.New())
	ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store the message: %v", err)
	}
	// The durable path has read the message once the core-path session's port has it.
	select {
	case body := <-corePushes:
		if !strings.Contains(body, item.EventID) {
			t.Fatalf("the core-path session's port was pushed:\n%s\nwant %s", body, item.EventID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the durable path never pushed the message to the core-path session")
	}
	awaitFanoutFinished(t, rig, durable, ack.Sequence)
	if rig.harness.config.attemptCache.Seen(item.DedupeKey, relayed) || rig.harness.config.dedupeCache.Seen(item.DedupeKey, relayed) {
		t.Fatalf("the fan-out recorded the message for %s, which it never sent it", relayed)
	}

	rig.relays.setOwned(true)
	requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
	standIn.none(t, time.Second)
}
