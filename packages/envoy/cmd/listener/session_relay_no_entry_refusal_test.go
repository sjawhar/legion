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
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A message whose delivery mode cannot be read, sent to a session on consumer delivery whose
// entry has lapsed, leaves no attempt record behind, so the relay of that session holds nothing
// when it registers again on its serving port: its later messages arrive at once rather than
// waiting out attemptCacheTTL.
//
// The lapse is the ordinary one. An entry expires while its interest stays for at least ten
// minutes (store.InterestReapGrace plus store.InterestRefreshAge) and its consumer for three days
// (bus.SessionConsumerInactiveThreshold), so the fan-out still reads the interest and judges the
// message for a session it cannot resolve. judgeFollowedTopic refuses an unreadable mode before
// it looks for an entry -- a mode no session can be shown to advertise, and one any publisher can
// send on a followed topic -- so the refusal is reported, and recording it would leave the
// session's own relay a record to hold that message under for five minutes, the session's later
// messages behind it (fanoutDelivery, session_relay.go's holdWhilePushedElsewhere).
//
// With the fan-out passing !relayed for recorded, a resolution that failed
// (session.ErrNoSessionRecord) recorded the attempt all the same, and this test fails at the
// assertion below: "the fan-out recorded an attempt of ... for a refusal whose resolution
// failed".
//
// TestAnUnreadableDeliveryModeDoesNotStallTheSessionsLaterMessages holds the same for a session
// the fan-out does resolve; this one is the recipient it cannot.
func TestAnUnreadableDeliveryModeToASessionWithNoEntryLeavesNoHoldForItsRelay(t *testing.T) {
	const (
		relayedPort = 4096
		corePort    = 4097
	)
	// Unowned, so no relay runs until the fan-out has decided both messages: the test orders the
	// two paths by construction rather than by timing.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayed := "ses_no_entry_relayed_" + id.New()
	core := "ses_no_entry_core_" + id.New()
	topic := "notifications.github.acme.no-entry-" + id.New() + ".mention"
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
	// consumer is created here, below both messages below, and outlives the entry.
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
	durable := "listener-no-entry-" + id.New()
	bindTestDurable(t, rig, durable, topic)

	// The entry lapses, and only the entry: the interest the fan-out matches on stays, and so
	// does the consumer the relay reads.
	if err := rig.harness.sessions.Delete(context.Background(), relayed); err != nil {
		t.Fatalf("delete the entry of %s: %v", relayed, err)
	}
	if _, err := rig.harness.sessions.Get(relayed); err == nil {
		t.Fatalf("the entry of %s is still readable after its delete, so the fan-out would resolve the session", relayed)
	}

	unreadable := listenerTestEnvelope(topic, "no-entry-unreadable-"+id.New())
	unreadable.Payload = `{"delivery":{"mode":"bogus"}}`
	first, err := rig.harness.client.JS().Publish(topic, marshalListenerEnvelope(t, unreadable))
	if err != nil {
		t.Fatalf("store the unreadable-mode message: %v", err)
	}
	awaitLogLine(t, rig, "listener delivery mode cannot be read unambiguously", 15*time.Second)
	awaitFanoutFinished(t, rig, durable, first.Sequence)

	if ledgerAttempted(t, rig.harness.config, unreadable.DedupeKey, relayed) {
		t.Fatalf("the fan-out left an attempt of %s in the delivery ledger for a refusal whose resolution failed: the relay of that session holds this message, and every later message behind it, for dedupe.AttemptedAge once the session registers again", relayed)
	}

	// The session registers again, on the same port and the same consumer, which starts below the
	// message above.
	rig.subscribe(t, registration)
	if entry := rig.entry(t, relayed); entry.Delivery != session.DeliveryConsumer || entry.Port != relayedPort {
		t.Fatalf("the entry of %s after it registered again records delivery %q and port %d, want the consumer's and %d", relayed, entry.Delivery, entry.Port, relayedPort)
	}
	if start, stored := rig.entry(t, relayed).ConsumerStart, first.Sequence; start > stored {
		t.Fatalf("the consumer of %s starts at %d, past the unreadable-mode message at %d: the relay would never read the message this test is about", relayed, start, stored)
	}

	later := listenerTestEnvelope(topic, "no-entry-later-"+id.New())
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
	// hold under an attempt record naming no push lasts attemptCacheTTL, five minutes.
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
		t.Fatalf("the relay of %s held a message, want none held: nothing says a push of it is in flight:\n%s", relayed, logs)
	}
	// One report from the fan-out, which cannot resolve the session, and one from the relay,
	// which refuses the same message when the session is back: the exception dedupe a recorded
	// refusal would buy is what this branch gives up.
	reasons := deliveryExceptionsFor(t, exceptions, relayed, 2*time.Second)
	if len(reasons) != 2 || reasons[0] != "delivery_failed" || reasons[1] != "delivery_failed" {
		t.Fatalf("the exception lane of %s carried %v, want the fan-out's delivery_failed and the relay's", relayed, reasons)
	}
}
