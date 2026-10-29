package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// storeSessionHeartbeat registers sessionID with the heartbeat the test chose, by writing its
// entry into the session bucket itself: SessionRegistry.Put stamps UpdatedAt with the moment of
// the write (mergeForClaim), so a holder that last heartbeated a while ago is written the way
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

// TestCoreRoleDeliverySkipsAHolderThatStoppedHeartbeating covers role delivery's liveness test of
// a role's holder (laneHolderFresh, roles.Fresh), which resolveCoreRoleHolder applies for the role
// lane: a claim naming a session whose entry is still in the session bucket, and so answers every
// read, but which has not heartbeated within session.ClaimStaleAfter is a holder role delivery
// does not forward to. The lane releases the lapsed claim and reports delivery_failed instead,
// because a message dropped into a session whose process is very likely gone is costlier than
// telling its sender the role has no live holder. The same holder, heartbeating, takes the
// message: the freshness test is what separates the two, and nothing else about the two deliveries
// differs.
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
		{"a holder that heartbeated a moment ago", time.Second, true},
		{"a holder that stopped heartbeating before the claim went stale", session.ClaimStaleAfter + time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t, nil)
			storeSessionHeartbeat(t, harness.client, holder, time.Now().Add(-tc.since))
			if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
				t.Fatalf("claim the role: %v", err)
			}

			// The holder answers the forward's receipt only after the message is recorded, so a
			// forward the lane reports delivered is on this channel by the time the handler
			// returns (bus.Client.RequestCoreTo waits for the receipt).
			forwards := make(chan *natsgo.Msg, 4)
			received, err := harness.client.Conn.Subscribe(holderSubject, func(message *natsgo.Msg) {
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
			exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + roleTopic)
			if err != nil {
				t.Fatalf("subscribe to the exception lane: %v", err)
			}
			t.Cleanup(func() { _ = exceptions.Unsubscribe() })
			if err := harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the test's subscriptions: %v", err)
			}

			item := listenerTestEnvelope(roleTopic, "role-holder-heartbeat")
			harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
			if err := harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the delivery's publications: %v", err)
			}

			exceptionReason := ""
			if message, err := exceptions.NextMsg(time.Second); err == nil {
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
			var forwarded *natsgo.Msg
			select {
			case forwarded = <-forwards:
			case <-time.After(time.Second):
			}
			claimant, err := harness.registry.RoleHolder(role)
			if err != nil {
				t.Fatalf("read the role's claim after the delivery: %v", err)
			}

			if !tc.wantDelivered {
				if forwarded != nil {
					var delivered contracts.Envelope
					if err := json.Unmarshal(forwarded.Data, &delivered); err != nil {
						t.Fatalf("decode the message the stale holder received: %v", err)
					}
					t.Fatalf("the role lane delivered event %s to %s on %s, whose session entry last heartbeated %s ago - longer than session.ClaimStaleAfter (%s); want no forward to the stale holder, the delivery_failed exception and the claim released",
						delivered.EventID, holder, holderSubject, tc.since, session.ClaimStaleAfter)
				}
				if exceptionReason != "delivery_failed" || claimant != "" {
					t.Fatalf("a role whose holder last heartbeated %s ago: exception reason %q, claim now held by %q; want reason \"delivery_failed\" and the claim released",
						tc.since, exceptionReason, claimant)
				}
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
