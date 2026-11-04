package main

import (
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The fan-out reports its own refusal of a message on a followed topic for every recipient it
// delivers to: a session on the core path and one whose entry is gone. Each refusal records its
// recipient before publishing its exception, so an identical redelivery inside the attempt window
// is a dedupe skip rather than a second exception
// (TestListenerDeliveryHandler_FanoutRefusalDedupesAfterExceptionDelivery).
//
// Neither recipient is on consumer delivery, so this test pins what the fan-out does where no
// consumer holds the message. The recipient whose own consumer does is pinned by
// TestTheFanoutReportsARefusalOfASessionOnConsumerDelivery below.
//
// A session with no entry leaves no attempt record whichever refusal it meets (INFRA-594): a
// mode it does not advertise cannot be judged against an entry there is none of, so the message
// is taken and the resolution that then fails records nothing, and a mode that cannot be read is
// refused before the entry is read at all but recorded only where the resolution succeeded.
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
		// core hands the message to the core lane rather than to the durable, so both lanes are
		// driven here. The verdict is the same either way, because judgeFollowedTopic is never
		// told the lane: its signature takes no deliveryMessage, so a core message earns the
		// verdict a durable one would. The durable path does read a stored message for each of
		// these recipients, an entry no consumer of its own backs and an entry there is none of
		// alike (durableNeedsDelivery): a message only the consumers of the sessions following
		// its topic hold is the one it acknowledges unread.
		core bool
	}{
		{
			name:          "a self-subscribed session on the core path does not advertise the mode",
			entry:         &session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"},
			payload:       `{"delivery":{"mode":"btw"}}`,
			wantException: true,
			wantLog:       "listener subscriber does not advertise delivery mode",
			wantAttempt:   true,
			core:          true,
		},
		{
			name:          "a self-subscribed session on the core path is sent a mode that cannot be read",
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
			// (session.ErrNoSessionRecord), and an attempt record names a delivery that was never
			// made.
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
			harness := newListenerDeliveryHarness(t)
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
			if harness.config.dedupeCache.Seen(item.DedupeKey, sessionID) {
				t.Fatalf("the fan-out recorded %s as having taken a message it was refused", sessionID)
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

// A recipient the fan-out read an entry for and nothing reaches -- an entry with no consumer of
// its own holding the message (session.ErrNoSessionRecord), the record a build before
// dispatch://INFRA-771 left for a session on the core path -- has its attempt recorded before
// the exception of its refusal is published, so a redelivery of the identical message inside the
// attempt window is the attempt-dedupe skip judgeFollowedTopic makes of it and raises no second
// exception. This path is that recipient's one report (fanoutDelivery), so without the record
// each stored copy of one event, and each redelivery of every copy, would raise an exception of
// its own.
//
// The two recipients beside it record nothing and give that dedupe up, each for its own reason:
// a session whose entry is gone was never read, so a record would name a delivery that could not
// have been made (the rows above with no entry), and a session whose own consumer holds the
// message is delivered by that consumer's relay, which a record would make hold its copy, the
// session's later messages behind it.
func TestTheFanoutRecordsARecipientItCannotReachBeforeItsException(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	sessionID := "ses_fanout_no_path_" + id.New()
	topic := "notifications.github.acme.fanout-no-path-" + id.New() + ".mention"
	if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", sessionID, err)
	}
	// The entry of a session on no delivery path at all: no consumer of its own holding its
	// messages, so the fan-out's one resolution of it is session.ErrNoSessionRecord.
	if err := harness.sessions.Put(sessionID, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register %s's entry: %v", sessionID, err)
	}
	probe := exceptionProbe(t, harness.client)

	// The refusal: a delivery mode this entry does not advertise. The core lane carries it, as it
	// carries every message such a session is fanned out, the durable path acknowledging a stored
	// one unread (durableNeedsDelivery).
	item := listenerTestEnvelope(topic, "fanout-no-path-"+id.New())
	item.Payload = `{"delivery":{"mode":"btw"}}`
	message := marshalListenerEnvelope(t, item)
	harness.coreHandler(&natsgo.Msg{Data: message})
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception the fan-out published: %v", err)
	}
	if recipient := exceptionRecipient(t, assertDeliveryException(t, probe, item, "delivery_failed")); recipient != sessionID {
		t.Fatalf("the exception names recipient %q, want %q", recipient, sessionID)
	}
	if !harness.config.attemptCache.Seen(item.DedupeKey, sessionID) {
		t.Fatalf("the fan-out published the exception of %s and recorded no attempt for it: every redelivery inside the window raises another", sessionID)
	}

	// The identical message again, as a redelivery of the same stored copy reaches this listener.
	harness.coreHandler(&natsgo.Msg{Data: message})
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the redelivery: %v", err)
	}
	noException(t, probe)
	if logs := harness.logs.String(); !strings.Contains(logs, "listener attempt-dedupe skip") {
		t.Fatalf("the redelivery of %s was not the attempt-dedupe skip:\n%s", sessionID, logs)
	}
}

// A message on a followed topic that a session on consumer delivery cannot be given -- its
// targeted delivery mode is one the session does not advertise, or one no session can be shown to
// advertise because it cannot be read unambiguously -- is reported by the durable path's fan-out,
// with one delivery_failed exception for that session.
//
// A session's relay publishes every frame unjudged (sessionRelays.relayNATS) and the plugin drops
// a frame whose delivery mode it cannot read without a word, so a refusal left to that relay
// would be reported by nobody. The fan-out records no attempt for it: it hands the message to
// nobody, so a record would have the relay's own copy of the next event under that key skipped as
// one already on its way (INFRA-594).
func TestTheFanoutReportsARefusalOfASessionOnConsumerDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		// payload is what the publisher sends.
		payload string
	}{
		{
			name:    "the session does not advertise the mode",
			payload: `{"delivery":{"mode":"btw"}}`,
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
			relayed := "ses_consumer_mode_" + id.New()
			topic := "notifications.github.acme.consumer-mode-" + id.New() + ".mention"
			exceptions := exceptionProbe(t, rig.harness.client)

			// The session under test: on consumer delivery, following the topic, and advertising
			// no delivery mode at all.
			rig.subscribe(t, consumerRegistration(relayed, "epoch-1", topic))
			if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer {
				t.Fatalf("the session's entry records delivery %q, want the consumer's", entry.Delivery)
			}
			keepTheDurableReading(t, rig, topic)
			durable := "listener-consumer-mode-" + id.New()
			bindTestDurable(t, rig, durable, topic)

			item := listenerTestEnvelope(topic, "consumer-mode-"+id.New())
			item.Payload = tc.payload
			ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
			if err != nil {
				t.Fatalf("store the message: %v", err)
			}
			awaitFanoutFinished(t, rig, durable, ack.Sequence)

			if reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second); len(reasons) != 1 || reasons[0] != "delivery_failed" {
				t.Fatalf("the exception lanes carried %v for the session on consumer delivery, want one delivery_failed: its relay judges nothing, so the fan-out reports the refusal", reasons)
			}
			if rig.harness.config.attemptCache.Seen(item.DedupeKey, relayed) {
				t.Fatalf("the fan-out recorded an attempt for %s, which it handed the message to nobody for", relayed)
			}
		})
	}
}

// A message on a followed topic a session on consumer delivery takes reaches it as a relay frame
// when another interest on that topic sends the same message down the durable path: the session's
// consumer holds it and its relay is what sends it, so the fan-out hands it to nobody and records
// neither an attempt nor a dedupe key for it -- records that would have the next copy of the same
// event skipped as one the session already has (INFRA-594).
func TestTheRelaySendsAFollowedTopicTheDurablePathAlsoReads(t *testing.T) {
	// Unowned, so no relay runs until the fan-out has read the message: the test orders the two
	// paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayed := "ses_consumer_take_" + id.New()
	topic := "notifications.github.acme.consumer-take-" + id.New() + ".mention"

	standIn := startSessionStandIn(t, rig.url, relayed, answerDone)
	rig.subscribe(t, consumerRegistration(relayed, "epoch-1", topic))
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session's entry records delivery %q, want the consumer's", entry.Delivery)
	}
	keepTheDurableReading(t, rig, topic)
	durable := "listener-consumer-take-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	item := listenerTestEnvelope(topic, "consumer-take-"+id.New())
	ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store the message: %v", err)
	}
	awaitFanoutFinished(t, rig, durable, ack.Sequence)
	if rig.harness.config.attemptCache.Seen(item.DedupeKey, relayed) || rig.harness.config.dedupeCache.Seen(item.DedupeKey, relayed) {
		t.Fatalf("the fan-out recorded the message for %s, which it never sent it", relayed)
	}

	rig.relays.setOwned(true)
	requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
	standIn.none(t, time.Second)
}

// A message on a topic the session that published it follows is skipped for that session as an
// echo: the fan-out hands it back to nobody, reports no failure of it, and records neither a
// dedupe key nor an attempt for it, so the next event under that key still reaches it. Every
// other session following the topic is given the message as usual, by the relay of its own
// consumer, which the fan-out records nothing for either (INFRA-594).
//
// Both sessions are on consumer delivery, the shape every live registration has, and the message
// is stored, so the durable path's fan-out is the lane that reads it: the listener's core
// handler is wired to the role lane alone (bus.ConnectRoleLane on notifications.role.>). The
// third interest is what keeps that path reading the topic at all (keepTheDurableReading): with
// only the two consumers' interests the durable would acknowledge the message before it decoded
// it (durableNeedsDelivery), and the fan-out would never judge it.
func TestTheFanoutSkipsAMessageOnAFollowedTopicAsAnEchoOfTheSessionThatPublishedIt(t *testing.T) {
	// Unowned, so no relay runs until the fan-out has read the message: the test orders the two
	// paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	run := id.New()
	sender := "ses_echo_sender_" + run
	other := "ses_echo_other_" + run
	topic := "notifications.team.echo-" + run
	standIn := startSessionStandIn(t, rig.url, other, answerDone)
	exceptions := exceptionProbe(t, rig.harness.client)
	for _, sessionID := range []string{sender, other} {
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", topic))
		if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
			t.Fatalf("the entry of %s records delivery %q, want the consumer's", sessionID, entry.Delivery)
		}
	}
	keepTheDurableReading(t, rig, topic)
	durable := "listener-echo-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	item := listenerTestEnvelope(topic, "echo-"+run)
	item.SourceSession = sender
	ack, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store the message: %v", err)
	}
	awaitFanoutFinished(t, rig, durable, ack.Sequence)

	for _, sessionID := range []string{sender, other} {
		if rig.harness.config.dedupeCache.Seen(item.DedupeKey, sessionID) || rig.harness.config.attemptCache.Seen(item.DedupeKey, sessionID) {
			t.Fatalf("the fan-out recorded %s, which it handed the message to nobody for", sessionID)
		}
	}
	if reasons := deliveryExceptionsFor(t, exceptions, sender, 2*time.Second); len(reasons) != 0 {
		t.Fatalf("the exception lanes carried %v for %s, want none: the message it published itself is skipped, not failed", reasons, sender)
	}
	var echoes []string
	for _, line := range strings.Split(rig.harness.logs.String(), "\n") {
		if strings.Contains(line, "listener skip echo") {
			echoes = append(echoes, line)
		}
	}
	if len(echoes) != 1 || !strings.Contains(echoes[0], sender) {
		t.Fatalf("the fan-out logged %d echoes, want one naming %s:\n%s", len(echoes), sender, rig.harness.logs.String())
	}

	// The message the fan-out handed to nobody is still the other session's to receive: the
	// relay of its own consumer is what sends it.
	rig.relays.setOwned(true)
	requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
	standIn.none(t, time.Second)
}
