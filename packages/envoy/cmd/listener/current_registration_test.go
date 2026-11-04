package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// consumerRegistrationIn is consumerRegistration with a directory: the body a session in consumer
// mode at epoch, running in dir and following topics, sends to the listener it registers through.
// It names no port, as every shipped plugin's registration does. The directory and the handling
// listener's own machine id are what the interest row records, and the sender stamp of every
// message the session sends reads them from that row (senderStamp), so a session that registers
// again through another listener, from another directory, is stamped with the registration it has
// now only if the row moved with it.
func consumerRegistrationIn(t *testing.T, sessionID, epoch, dir string, topics ...string) string {
	t.Helper()
	body, err := json.Marshal(subscribeBody{
		SessionID:      sessionID,
		Dir:            dir,
		Topics:         append([]string{}, topics...),
		SelfSubscribed: true,
		Delivery:       &subscribeDelivery{Consumer: true, Epoch: epoch},
	})
	if err != nil {
		t.Fatalf("encode the registration of %s in %s: %v", sessionID, dir, err)
	}
	return string(body)
}

// answerLaneReceipts subscribes to sessionID's agent subject on a NATS connection of the session's
// own, as a plugin in consumer mode holds one beside its relay subject, and answers the receipt of
// every role forward that arrives there: a frame whose envelope names another topic and asks for a
// reply (envoy-client's expectsLaneReceipt). It returns those forwards. The session's own messages
// arrive on this subject too, from the stream's publish and carrying its acknowledgement inbox as
// their reply, and are neither answered nor returned: their envelope names the subject itself.
//
// The forward is the handshake that tells the role message's sender it was taken. The message
// itself reaches the session as a frame of its consumer, which the stream's stored copy of the
// forward feeds, so a role message counted once here and counted once on the relay subject reached
// the session once on each lane, as it does in production.
func answerLaneReceipts(t *testing.T, url, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	agent := contracts.AgentSubject(sessionID)
	forwards := make(chan contracts.Envelope, 16)
	if _, err := conn.Subscribe(agent, func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("decode what %s received on %s: %v", sessionID, agent, err)
			return
		}
		if msg.Reply == "" || item.Topic == agent {
			return
		}
		if err := msg.Respond(nil); err != nil {
			t.Errorf("answer the receipt of a forward to %s: %v", sessionID, err)
			return
		}
		forwards <- item
	}); err != nil {
		t.Fatalf("subscribe %s to %s: %v", sessionID, agent, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription of %s to %s: %v", sessionID, agent, err)
	}
	return forwards
}

// requireOneForward fails the test unless exactly one forward, on topic, arrived within the window.
func requireOneForward(t *testing.T, forwards <-chan contracts.Envelope, topic string, within time.Duration) contracts.Envelope {
	t.Helper()
	var first contracts.Envelope
	select {
	case first = <-forwards:
	case <-time.After(within):
		t.Fatalf("no forward on %s reached the session within %s", topic, within)
	}
	if first.Topic != topic {
		t.Fatalf("the forward carries topic %s, want %s", first.Topic, topic)
	}
	select {
	case again := <-forwards:
		t.Fatalf("a second forward reached the session: %s on %s", again.EventID, again.Topic)
	case <-time.After(500 * time.Millisecond):
	}
	return first
}

// subscribeCore subscribes to subject on a connection of the test's own, apart from any listener's,
// so a message published there is read where any session holding its own connection reads it.
func subscribeCore(t *testing.T, url, subject string) *natsgo.Subscription {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	subscription, err := conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	return subscription
}

// A session that registered through one listener and then through another, from another directory,
// belongs to the second registration in every way, and the first listener stopping takes nothing of
// it: a message on a stored topic it follows, a message to a role it holds and a message to its own
// agent subject each reach it once through the second listener, and a message it sends through that
// listener is stamped with the second registration's machine and directory.
//
// The stamp is the part that moves with the session only once the interest row does
// (internal/store's mergeForUpsert): senderStamp reads sender.Machine and sender.Cwd from that row,
// and a row pinned to the first registration names a listener the session no longer registers
// through and a directory it no longer runs in. The deliveries read the session's entry, which
// every registration rewrites, so they follow the session already.
func TestASessionRegisteredAgainThroughAnotherListenerIsReachedAndStampedThroughThatRegistration(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	first := startListenerProcess(t, binary, uri, "relay-first")
	second := startListenerProcess(t, binary, uri, "relay-second")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the first listener's output:\n%s\nthe second listener's output:\n%s", first.Output.String(), second.Output.String())
		}
	})
	first.waitHealthy(t)
	second.waitHealthy(t)

	run := id.New()
	sessionID := "ses_current_registration_" + run
	agent := contracts.AgentSubject(sessionID)
	const firstDir, secondDir = "/work/first-registration", "/work/second-registration"
	followed := "notifications.dispatch.issue.INFRA-889-" + run + ".>"
	comment := "notifications.dispatch.issue.INFRA-889-" + run + ".comment"
	role := "current-registration-" + run
	roleTopic := contracts.RoleTopicPrefix + role
	ownTopic := "notifications.team.current-registration-" + run

	standIn := startSessionStandIn(t, uri, sessionID, answerDone)
	forwards := answerLaneReceipts(t, uri, sessionID)
	consumers := observeConsumers(t, uri)
	publisher := connectPublisher(t, uri)
	ownMessages := subscribeCore(t, uri, ownTopic)

	postListener(t, first.port, "/v1/interests/subscribe", consumerRegistrationIn(t, sessionID, "epoch-1", firstDir, followed))
	through := newEnvelope("agent", agent, "through the first listener")
	publish(t, publisher, through)
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, through)
	// The session wrote it, so the relay the second listener starts, at the ack floor, sends it no
	// second time.
	awaitAcked(t, consumers, sessionID, frames[0].seq)

	postListener(t, second.port, "/v1/interests/subscribe", consumerRegistrationIn(t, sessionID, "epoch-1", secondDir, followed))
	second.awaitSessionMachine(t, sessionID, "relay-second")
	first.awaitSessionMachine(t, sessionID, "relay-second")
	postListener(t, second.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)

	// The listener the session registered through first stops, as the task that held it is replaced.
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	followedItem := newEnvelope("dispatch", comment, "a comment on the issue the session follows")
	publish(t, publisher, followedItem)
	requireFrames(t, standIn.collect(t, 1, 15*time.Second), followedItem)

	postListener(t, second.port, "/v1/messages/publish",
		`{"topic":"`+roleTopic+`","message":"a message for the role's holder","source":"agent","source_session":"ses_role_sender_`+run+`"}`)
	forwarded := requireOneForward(t, forwards, roleTopic, 10*time.Second)
	roleFrame := standIn.collect(t, 1, 15*time.Second)[0]
	if roleFrame.item.EventID != forwarded.EventID || roleFrame.item.Topic != roleTopic {
		t.Fatalf("the frame after the role message is %s on %s, want the forwarded %s on %s",
			roleFrame.item.EventID, roleFrame.item.Topic, forwarded.EventID, roleTopic)
	}

	inbox := newEnvelope("agent", agent, "a message for this session alone")
	publish(t, publisher, inbox)
	requireFrames(t, standIn.collect(t, 1, 15*time.Second), inbox)
	standIn.none(t, 2*time.Second)

	status, answer := callListener(t, second.port, http.MethodPost, "/v1/messages/publish",
		`{"topic":"`+ownTopic+`","message":"a message the session sends","source":"agent","source_session":"`+sessionID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("the session's publish through the second listener: status %d: %s", status, answer)
	}
	message, err := ownMessages.NextMsg(10 * time.Second)
	if err != nil {
		t.Fatalf("read the message the session sent on %s: %v", ownTopic, err)
	}
	var sent contracts.Envelope
	if err := json.Unmarshal(message.Data, &sent); err != nil {
		t.Fatalf("decode the message the session sent on %s: %v", ownTopic, err)
	}
	if sent.Sender == nil {
		t.Fatalf("the message the session sent carries no sender: %s", message.Data)
	}
	if sent.Sender.Machine != "relay-second" {
		t.Errorf("sender.machine = %q, want relay-second: the listener the session is registered through now", sent.Sender.Machine)
	}
	if sent.Sender.Cwd != secondDir {
		t.Errorf("sender.cwd = %q, want %s: the directory the second registration sent", sent.Sender.Cwd, secondDir)
	}
}
