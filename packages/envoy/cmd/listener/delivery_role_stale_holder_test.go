package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
)

// storeSessionHeartbeat registers sessionID with the heartbeat the test chose, by writing its
// entry into the session bucket itself: SessionRegistry.Put stamps UpdatedAt with the moment of
// the write, so a holder that last heartbeated a while ago is written the way
// another writer's entry arrives, through the bucket. The session cache dates its copy from the
// bucket entry's own creation (SessionRegistry.applyWatched), so the entry is in the cache and in
// the bucket for as long as the test runs while its heartbeat is as old as the test says.
func storeSessionHeartbeat(t *testing.T, client *bus.Client, sessionID string, heartbeat time.Time) {
	t.Helper()
	value, err := json.Marshal(session.SessionEntry{
		MachineID:      "test-machine",
		SelfSubscribed: true,
		UpdatedAt:      heartbeat.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("encode session %s's entry: %v", sessionID, err)
	}
	sessions, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := sessions.Put(sessionID, value); err != nil {
		t.Fatalf("store session %s's entry: %v", sessionID, err)
	}
}

// staleHolderRig is the rig the tests of role delivery's liveness test share: a delivery harness
// whose role is claimed by holder, whose session entry last heartbeated since ago, with the
// holder's own subscription and the role topic's exception lane already subscribed.
//
// The holder answers the forward's receipt only after the message is recorded, so a forward the
// lane reports delivered is on forwards by the time the handler returns
// (bus.Client.RequestCoreTo waits for the receipt).
type staleHolderRig struct {
	harness    listenerDeliveryHarness
	forwards   chan *natsgo.Msg
	exceptions *natsgo.Subscription
}

func newStaleHolderRig(t *testing.T, role, holder string, since time.Duration) staleHolderRig {
	t.Helper()
	harness := newListenerDeliveryHarness(t)
	storeSessionHeartbeat(t, harness.client, holder, time.Now().Add(-since))
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	forwards := make(chan *natsgo.Msg, 4)
	received, err := harness.client.Conn.Subscribe(contracts.AgentSubject(holder), func(message *natsgo.Msg) {
		select {
		case forwards <- message:
		default:
		}
		if message.Reply != "" {
			_ = message.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe the holder's subject: %v", err)
	}
	t.Cleanup(func() { _ = received.Unsubscribe() })
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the exception lane: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the test's subscriptions: %v", err)
	}
	return staleHolderRig{harness: harness, forwards: forwards, exceptions: exceptions}
}

// settled reads what the delivery just made of the message: the reason the role topic's exception
// lane carried (empty for none), the forward the holder received (nil for none) and the session
// the role's claim names now (empty once it is released).
func (r staleHolderRig) settled(t *testing.T, role string) (exceptionReason string, forwarded *natsgo.Msg, claimant string) {
	t.Helper()
	if err := r.harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the delivery's publications: %v", err)
	}
	if message, err := r.exceptions.NextMsg(time.Second); err == nil {
		var exception contracts.Envelope
		if err := json.Unmarshal(message.Data, &exception); err != nil {
			t.Fatalf("decode the delivery exception envelope: %v", err)
		}
		var payload deliveryExceptionPayload
		if err := json.Unmarshal([]byte(exception.Payload), &payload); err != nil {
			t.Fatalf("decode the delivery exception payload: %v", err)
		}
		exceptionReason = payload.Reason
	} else if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("read the exception lane: %v", err)
	}
	select {
	case forwarded = <-r.forwards:
	case <-time.After(time.Second):
	}
	claimant, err := r.harness.registry.RoleHolder(role)
	if err != nil {
		t.Fatalf("read the role's claim after the delivery: %v", err)
	}
	return exceptionReason, forwarded, claimant
}

// TestCoreRoleDeliverySkipsAHolderThatStoppedHeartbeating covers role delivery's test of a role's
// holder (sessionIsLive, session.StateOf), which resolveCoreRoleHolder applies for the role lane:
// a claim naming a session whose record is still in the session bucket, and so answers every
// read, but whose last registration is past session.LiveWindow is a holder role delivery does not
// forward to. The lane reports delivery_failed instead, because a message dropped into a session
// that is not answering is costlier than telling its sender the role has no live holder. What the
// lane does not do is take the role away: a holder inside session.GoneHorizon is unresponsive and
// not gone, so its claim stands and only the role reaper, finding it gone, ever releases one, and
// nothing is kept for it either, since a kept copy waits for a claim that is never made again
// (roleKeepable). The same holder, registering, takes the message: the state of its record is
// what separates the two, and nothing else about the two deliveries differs.
func TestCoreRoleDeliverySkipsAHolderThatStoppedHeartbeating(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_role_heartbeat"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	holderSubject := contracts.AgentSubject(holder)
	for _, tc := range []struct {
		name          string
		since         time.Duration
		wantDelivered bool
	}{
		{"a holder that registered a moment ago", time.Second, true},
		{"a holder whose last registration is past the live window", session.LiveWindow + time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStaleHolderRig(t, role, holder, tc.since)

			item := listenerTestEnvelope(roleTopic, "role-holder-heartbeat")
			rig.harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

			exceptionReason, forwarded, claimant := rig.settled(t, role)
			if !tc.wantDelivered {
				if forwarded != nil {
					var delivered contracts.Envelope
					if err := json.Unmarshal(forwarded.Data, &delivered); err != nil {
						t.Fatalf("decode the message the stale holder received: %v", err)
					}
					t.Fatalf("the role lane delivered event %s to %s on %s, whose session record last registered %s ago - past session.LiveWindow (%s); want no forward to the unresponsive holder, the delivery_failed exception and its claim left where it is",
						delivered.EventID, holder, holderSubject, tc.since, session.LiveWindow)
				}
				if exceptionReason != "delivery_failed" || claimant != holder {
					t.Fatalf("a role whose holder last registered %s ago: exception reason %q, claim now held by %q; want reason \"delivery_failed\" and the claim still held by %s",
						tc.since, exceptionReason, claimant, holder)
				}
				assertNothingKept(t, rig.harness.client, role)
				return
			}
			if forwarded == nil || exceptionReason != "" || claimant != holder {
				t.Fatalf("a role whose holder heartbeated %s ago: forward to %s received=%t, exception reason %q, claim now held by %q; want the forward, no exception and the claim still held by %s",
					tc.since, holderSubject, forwarded != nil, exceptionReason, claimant, holder)
			}
			var delivered contracts.Envelope
			if err := json.Unmarshal(forwarded.Data, &delivered); err != nil {
				t.Fatalf("decode the message the holder received: %v", err)
			}
			if delivered.EventID != item.EventID || delivered.Topic != roleTopic {
				t.Fatalf("the holder received event %s on topic %s; want the role message %s on %s", delivered.EventID, delivered.Topic, item.EventID, roleTopic)
			}
		})
	}
}

// A sender that holds a queue of its own and redelivers from it - the Legion daemon's notice
// queue, a phase worker's stall reminder - opts out of the keep. That opt-out is the publish
// route's own request field and nothing else: no envelope carries a keep key, so a raw role
// publish on the lane cannot ask for it and the lane always keeps (keepForNextHolder), and the
// opt-out can only be exercised through POST /v1/messages/publish, which answers its sender
// rather than publishing an exception.
//
// So that sender gets the refusal it had before the kept lane, by the route's own shape: with
// keep: false against a role nobody claims - the one state this build keeps for, since no
// resolution releases a claim (roleKeepable) - nothing is kept and the answer is the 404 naming
// unclaimed, which the Legion daemon reads as notify.ErrNoHolder.
func TestAPublishWithKeepFalseToAnUnclaimedRoleIsRefusedAndKeepsNothing(t *testing.T) {
	const role = "reviewer"
	harness := newListenerDeliveryHarness(t)

	refusal, _ := publishThrough(harness.publishDeps(t),
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the sender redelivers this itself","keep":false}`)

	// The answer #2395 wrote, which this sender still gets: its own queue holds the message, and
	// a copy kept for the role's next holder would deliver it twice.
	if refusal.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to an unclaimed role: status %d, body %s; want 404", refusal.Code, refusal.Body.String())
	}
	var answer roleHolderError
	if err := json.Unmarshal(refusal.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the refusal %s: %v", refusal.Body.String(), err)
	}
	if answer.Reason != roles.Unclaimed || answer.Holder != "" {
		t.Fatalf("the refusal is %+v, want reason unclaimed naming no holder", answer)
	}
	assertNothingKept(t, harness.client, role)
}
