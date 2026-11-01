package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// registerServingPortSession records sessionID as a session on the core path served at port on
// 127.0.0.1, registered through the harness's listener.
func registerServingPortSession(t *testing.T, harness listenerDeliveryHarness, sessionID string, port int) {
	t.Helper()
	if _, err := harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{contracts.AgentSubject(sessionID)}); err != nil {
		t.Fatalf("register %s's interest: %v", sessionID, err)
	}
	if err := harness.sessions.Put(sessionID, session.SessionEntry{Port: port, MachineID: "test-machine", Host: "127.0.0.1"}); err != nil {
		t.Fatalf("register %s's entry: %v", sessionID, err)
	}
}

// awaitDurableState waits up to 5 s for durable to hold pending messages for an ack with every
// message through floor acked.
func awaitDurableState(t *testing.T, client *bus.Client, durable string, pending int, floor uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := client.JS().ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read the durable %s: %v", durable, err)
		}
		if info.NumAckPending == pending && info.AckFloor.Stream >= floor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the durable %s has %d pending ack, ack floor %d; want %d pending and everything through %d acked", durable, info.NumAckPending, info.AckFloor.Stream, pending, floor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lastStreamSeq is the stream's last sequence.
func lastStreamSeq(t *testing.T, client *bus.Client) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	return info.State.LastSeq
}

// requirePushes reads the pushes port answers, failing the test unless they carry want's messages
// with want's statuses, in order, each within within.
func requirePushes(t *testing.T, port *scriptedPort, within time.Duration, want ...scriptedPush) {
	t.Helper()
	for i, expected := range want {
		push := port.next(t, within)
		if push.status != expected.status || !strings.Contains(push.text, expected.text) {
			t.Fatalf("push %d to %s answered %d carrying:\n%s\nwant %d carrying %q", i+1, port.sessionID, push.status, push.text, expected.status, expected.text)
		}
	}
}

// On the durable path a serving-port session whose port hangs holds up no other session: the other
// session's messages are pushed while the hanging port holds the first push, and acknowledged. The
// hanging session's two messages wait in its own queue, the first pushed again after the port
// refuses it, and reach the port in stream order once it accepts; each is acknowledged only on its
// 204. With the push inline in the durable's one handler, the other session waits out the hang, and
// the refused message, NAKed back to the durable, is passed by the second.
func TestServePortWorkersAHangingPortHoldsUpNoOtherSessionAndItsOwnMessagesStayInOrder(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	cfg := wireTestDeliveryWorkers(t, harness.config)
	slow := startScriptedPort(t, "ses_slow_"+id.New(), http.StatusInternalServerError)
	release := slow.hold()
	t.Cleanup(release)
	fast := startScriptedPort(t, "ses_fast_"+id.New())
	registerServingPortSession(t, harness, slow.sessionID, slow.port)
	registerServingPortSession(t, harness, fast.sessionID, fast.port)
	durable := "listener-workers-" + id.New()
	subscription, err := startListenerSubscription(harness.client, durable, uncancelled(jetStreamDeliveryHandler(cfg)))
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })

	slowFirst := newEnvelope("agent", contracts.AgentSubject(slow.sessionID), "the slow session's first message")
	fastFirst := newEnvelope("agent", contracts.AgentSubject(fast.sessionID), "the fast session's first message")
	slowSecond := newEnvelope("agent", contracts.AgentSubject(slow.sessionID), "the slow session's second message")
	fastSecond := newEnvelope("agent", contracts.AgentSubject(fast.sessionID), "the fast session's second message")
	published := time.Now()
	for _, item := range []contracts.Envelope{slowFirst, fastFirst, slowSecond, fastSecond} {
		publish(t, harness.client, item)
	}

	requirePushes(t, fast, 2*time.Second,
		scriptedPush{text: fastFirst.Payload, status: http.StatusNoContent},
		scriptedPush{text: fastSecond.Payload, status: http.StatusNoContent})
	if took := time.Since(published); took > 1500*time.Millisecond {
		t.Fatalf("the fast session's messages took %s behind the hanging port, want under 1.5 s", took)
	}
	// The fast session's two messages are acknowledged; the slow session's two wait for its port.
	awaitDurableState(t, harness.client, durable, 2, 0)

	release()
	requirePushes(t, slow, 10*time.Second,
		scriptedPush{text: slowFirst.Payload, status: http.StatusInternalServerError},
		scriptedPush{text: slowFirst.Payload, status: http.StatusNoContent},
		scriptedPush{text: slowSecond.Payload, status: http.StatusNoContent})
	slow.noPush(t, 2*time.Second)
	awaitDurableState(t, harness.client, durable, 0, lastStreamSeq(t, harness.client))
}

// ungrantedListener is a listener binary whose NATS user carries the listener grant a hub carried
// before session consumers, so every session it serves is on the core path and its serving-port
// sessions are delivered from its durable; client is the test's own connection as that user.
type ungrantedListener struct {
	*listenerProcess
	client *bus.Client
}

func startUngrantedListener(t *testing.T) ungrantedListener {
	t.Helper()
	binary := cmdtest.Build(t, "envoy-listener")
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, listenerGrantBeforeSessionConsumers()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	listener := startListenerProcess(t, binary, grant.URL, "test-machine", "NATS_NKEY_SEED="+seed)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	client, err := bus.Connect([]string{grant.URL}, bus.WithName("serve-port-test"))
	if err != nil {
		t.Fatalf("connect the test's client: %v", err)
	}
	t.Cleanup(client.Close)
	return ungrantedListener{listenerProcess: listener, client: client}
}

// A serving-port session holding a role, on a listener whose grant carries no session consumers,
// gets the role message's stored forward from the listener's durable: the listener pushes it while
// the port answers 500 three times and then is down for 10 s, keeps it unacknowledged meanwhile,
// acknowledges it on the 204, and pushes it no more after that. Before, the durable acknowledged
// the stored forward away and the port never saw the message.
func TestServePortHolderGetsTheStoredRoleForwardUntilThePortAcceptsOnTheDurablePath(t *testing.T) {
	listener := startUngrantedListener(t)
	durable := bus.ListenerDurable("test-machine")
	sessionID := "ses_serve_holder_" + id.New()
	role := "serve-holder-" + id.New()
	port := startScriptedPort(t, sessionID, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	registerScriptedSession(t, listener.listenerProcess, port)
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)

	item := newEnvelope("agent", contracts.RoleTopicPrefix+role, "a message for the role's holder")
	publish(t, listener.client, item)
	for range 3 {
		push := port.next(t, 15*time.Second)
		if push.status != http.StatusInternalServerError {
			t.Fatalf("push answered %d, want the scripted 500", push.status)
		}
		requirePromptCarries(t, push.text, item)
	}
	port.down()
	downAt := time.Now()
	forwardSeq := lastStreamSeq(t, listener.client)
	awaitDurableState(t, listener.client, durable, 1, forwardSeq-1)
	time.Sleep(time.Until(downAt.Add(10 * time.Second)))
	info, err := listener.client.JS().ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if info.NumAckPending != 1 || info.AckFloor.Stream >= forwardSeq {
		t.Fatalf("after 10 s of the port down the durable has %d pending ack, ack floor %d; want the forward (seq %d) still pending", info.NumAckPending, info.AckFloor.Stream, forwardSeq)
	}
	port.up(t)

	accepted := port.next(t, 30*time.Second)
	if accepted.status != http.StatusNoContent {
		t.Fatalf("push answered %d, want 204", accepted.status)
	}
	requirePromptCarries(t, accepted.text, item)
	awaitDurableState(t, listener.client, durable, 0, forwardSeq)
	port.noPush(t, 3*time.Second)
}

// On a listener whose grant carries no session consumers, a serving-port session's messages reach
// its port from the listener's durable in stream order: the second waits behind the first while
// the port refuses the first, and is pushed once the first is accepted on its third try.
func TestServePortHolderMessagesArriveInOrderBehindARefusedOneOnTheDurablePath(t *testing.T) {
	listener := startUngrantedListener(t)
	sessionID := "ses_serve_order_" + id.New()
	port := startScriptedPort(t, sessionID, http.StatusInternalServerError, http.StatusInternalServerError)
	registerScriptedSession(t, listener.listenerProcess, port)

	first := newEnvelope("agent", contracts.AgentSubject(sessionID), "the first message")
	second := newEnvelope("agent", contracts.AgentSubject(sessionID), "the second message")
	publish(t, listener.client, first)
	publish(t, listener.client, second)

	requirePushes(t, port, 15*time.Second,
		scriptedPush{text: first.Payload, status: http.StatusInternalServerError},
		scriptedPush{text: first.Payload, status: http.StatusInternalServerError},
		scriptedPush{text: first.Payload, status: http.StatusNoContent},
		scriptedPush{text: second.Payload, status: http.StatusNoContent})
	port.noPush(t, 2*time.Second)
	awaitDurableState(t, listener.client, bus.ListenerDurable("test-machine"), 0, lastStreamSeq(t, listener.client))
}

// Two listeners, each on its own machine id and so its own durable, both read a message to a
// session neither has, and both raise its no_holder exception when they give it up: the stream
// stores one, under the Nats-Msg-Id exception.no_holder.<the message's own Nats-Msg-Id>, or its
// stream sequence when it has none. Before, each listener's exception was stored, one per listener.
// A zero retry window gives each message up on its first delivery (unplaceableAgentDelivery).
func TestOneExceptionIsStoredForAMessageToAnAbsentSessionWhicheverListenersRaiseIt(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	machines := []string{"exception-one", "exception-two"}
	for _, machineID := range machines {
		listener := startListenerProcess(t, binary, uri, machineID, "ENVOY_UNPLACEABLE_RETRY_WINDOW=0s")
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("listener %s's output:\n%s", machineID, listener.Output.String())
			}
		})
		listener.waitHealthy(t)
	}
	publisher := connectPublisher(t, uri)

	withMsgID := newEnvelope("agent", contracts.AgentSubject("ses_absent_"+id.New()), "a message with a Nats-Msg-Id")
	withoutMsgID := newEnvelope("agent", contracts.AgentSubject("ses_absent_"+id.New()), "a message without one")
	msgID := "one-exception-" + id.New()
	wantMsgIDs := map[string]string{}
	var lastSeq uint64
	for _, item := range []contracts.Envelope{withMsgID, withoutMsgID} {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal %s: %v", item.Topic, err)
		}
		var options []natsgo.PubOpt
		if item.EventID == withMsgID.EventID {
			options = append(options, natsgo.MsgId(msgID))
		}
		ack, err := publisher.JS().Publish(item.Topic, data, options...)
		if err != nil {
			t.Fatalf("store %s: %v", item.Topic, err)
		}
		origin := msgID
		if item.EventID == withoutMsgID.EventID {
			origin = strconv.FormatUint(ack.Sequence, 10)
		}
		wantMsgIDs[bus.ExceptionTopicPrefix+item.Topic] = "exception.no_holder." + origin
		lastSeq = ack.Sequence
	}
	// A listener acknowledges each message once its exception is stored.
	for _, machineID := range machines {
		awaitDurableState(t, publisher, bus.ListenerDurable(machineID), 0, lastSeq)
	}

	for subject, wantMsgID := range wantMsgIDs {
		info, err := publisher.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
		if err != nil {
			t.Fatalf("read the stream's count of %s: %v", subject, err)
		}
		if stored := info.State.Subjects[subject]; stored != 1 {
			t.Fatalf("the stream stores %d exceptions on %s, want one", stored, subject)
		}
		stored, err := publisher.JS().GetLastMsg(bus.Stream, subject)
		if err != nil {
			t.Fatalf("read the exception on %s: %v", subject, err)
		}
		if got := stored.Header.Get(natsgo.MsgIdHdr); got != wantMsgID {
			t.Errorf("the exception on %s carries Nats-Msg-Id %q, want %q", subject, got, wantMsgID)
		}
		var exception contracts.Envelope
		var payload deliveryExceptionPayload
		if err := json.Unmarshal(stored.Data, &exception); err != nil || json.Unmarshal([]byte(exception.Payload), &payload) != nil || payload.Reason != "no_holder" {
			t.Errorf("the exception on %s is %s, want a no_holder exception", subject, stored.Data)
		}
	}
}
