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

// assertOneKeptCopy fails t unless role's kept lane holds exactly one copy - the copy of item -
// stored under the id every writer of that lane keeps this message under (bus.KeptMsgID of its
// dedupe key and the role). That id is what collapses the copies of the several listeners which
// each took the same role publish and each found the role undeliverable into the single copy the
// role's next holder is handed.
func assertOneKeptCopy(t *testing.T, client *bus.Client, role string, item contracts.Envelope) {
	t.Helper()
	waitForKeptRoleMessage(t, client, role, item.EventID)
	subject := bus.KeptRoleSubject(role)
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream's copies on %s: %v", subject, err)
	}
	if held := info.State.Subjects[subject]; held != 1 {
		t.Fatalf("%s holds %d copies, want the one copy of event %s", subject, held, item.EventID)
	}
	stored, err := client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the copy on %s: %v", subject, err)
	}
	if got, want := stored.Header.Get(natsgo.MsgIdHdr), bus.KeptMsgID(item.DedupeKey, role); got != want {
		t.Fatalf("the copy on %s is stored under Nats-Msg-Id %q, want %q", subject, got, want)
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
	harness := newListenerDeliveryHarness(t, nil)
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

// assertStaleHolderGaveUp fails t unless the delivery forwarded nothing to the stale holder,
// released its claim and raised no exception: what the role lane's give-up and the publish
// route's both do with a holder that last heartbeated since ago, whatever each then keeps.
func assertStaleHolderGaveUp(t *testing.T, holder string, since time.Duration, exceptionReason string, forwarded *natsgo.Msg, claimant string) {
	t.Helper()
	if forwarded != nil {
		var delivered contracts.Envelope
		if err := json.Unmarshal(forwarded.Data, &delivered); err != nil {
			t.Fatalf("decode the message the stale holder received: %v", err)
		}
		t.Fatalf("role delivery forwarded event %s to %s on %s, whose session entry last heartbeated %s ago - longer than session.ClaimStaleAfter (%s); want no forward to the stale holder and the claim released",
			delivered.EventID, holder, contracts.AgentSubject(holder), since, session.ClaimStaleAfter)
	}
	if claimant != "" {
		t.Fatalf("a role whose holder last heartbeated %s ago: the claim is still held by %q; want it released",
			since, claimant)
	}
	if exceptionReason != "" {
		t.Fatalf("a role whose holder last heartbeated %s ago: the exception lane carried reason %q; want no exception, since nothing was dropped",
			since, exceptionReason)
	}
}

// TestCoreRoleDeliverySkipsAHolderThatStoppedHeartbeating covers role delivery's liveness test of
// a role's holder (laneHolderFresh, roles.Fresh), which resolveCoreRoleHolder applies for the role
// lane: a claim naming a session whose entry is still in the session bucket, and so answers every
// read, but which has not heartbeated within session.ClaimStaleAfter is a holder role delivery
// does not forward to, because a message dropped into a session whose process is very likely gone
// is costlier than not forwarding it.
//
// What the lane does with that message instead is the kept lane's rule: it releases the lapsed
// claim, as it always did, and keeps the message on the role's kept lane for whoever claims the
// role next (keepRoleLaneMessage). So the message is neither dropped into a probably-dead session
// nor lost - the copy waits for a live holder - and no delivery exception is raised for it. The
// same holder, heartbeating, takes the message: the freshness test is what separates the two, and
// nothing else about those two deliveries differs.
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
		{name: "a holder that heartbeated a moment ago", since: time.Second, wantDelivered: true},
		{name: "a holder that stopped heartbeating before the claim went stale", since: session.ClaimStaleAfter + time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStaleHolderRig(t, role, holder, tc.since)

			item := listenerTestEnvelope(roleTopic, "role-holder-heartbeat")
			rig.harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

			exceptionReason, forwarded, claimant := rig.settled(t, role)
			if !tc.wantDelivered {
				assertStaleHolderGaveUp(t, holder, tc.since, exceptionReason, forwarded, claimant)
				assertOneKeptCopy(t, rig.harness.client, role, item)
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
// keep: false against the stale holder TestCoreRoleDeliverySkipsAHolderThatStoppedHeartbeating
// holds the lane's side of, the claim is still released and nothing is kept, and the answer is
// the 404 naming holder_lapsed, the holder and claim_released, which the Legion daemon reads as
// notify.ErrNoHolder.
func TestAPublishWithKeepFalseToAStaleHolderIsRefusedAndKeepsNothing(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_role_heartbeat"
		since  = session.ClaimStaleAfter + time.Minute
	)
	rig := newStaleHolderRig(t, role, holder, since)

	refusal, _ := publishThrough(rig.harness.publishDeps(t),
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the sender redelivers this itself","keep":false}`)

	exceptionReason, forwarded, claimant := rig.settled(t, role)
	assertStaleHolderGaveUp(t, holder, since, exceptionReason, forwarded, claimant)
	// The answer #2395 wrote, which this sender still gets: its own queue holds the message, and
	// a copy kept for the role's next holder would deliver it twice.
	if refusal.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to a stale holder: status %d, body %s; want 404", refusal.Code, refusal.Body.String())
	}
	var answer roleHolderError
	if err := json.Unmarshal(refusal.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the refusal %s: %v", refusal.Body.String(), err)
	}
	if answer.Reason != roleHolderLapsed || answer.Holder != holder || answer.ClaimReleased == nil || !*answer.ClaimReleased {
		t.Fatalf("the refusal is %+v, want reason holder_lapsed naming %s with its claim released", answer, holder)
	}
	assertNothingKept(t, rig.harness.client, role)
}
