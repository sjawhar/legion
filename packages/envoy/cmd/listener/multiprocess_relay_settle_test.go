//go:build multiprocess

// The relay-settle rows of the two-pod suite (multiprocess_harness_test.go): what one frame the
// session has not answered costs when its relay's term ends, when the pod's session-consumer
// link drops, and when the pod is killed. Every row below holds one frame at the session and
// then moves delivery, so each reads the settle bound (relaySettleGrace), the successor's wait
// (relayFormerSettle) or the restart at the ack floor that follows them.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// mpRelayHold is how the session of a relay-settle row answers the one frame the row is about:
// mpHoldGated answers nothing until the row releases it, mpHoldReceived answers `received` at
// once and `done` at the release, and mpHoldNever never answers it at all.
type mpRelayHold int

const (
	mpHoldGated mpRelayHold = iota
	mpHoldReceived
	mpHoldNever
)

// mpRelayRow is the opening five of the rows below share: the machine's two pods, a session on
// consumer delivery registered through the active one, one warm message that session took, and
// one frame held at it, which is the frame the row is about. release answers that frame, and
// does nothing for mpHoldNever; it is registered as a cleanup, so a row that fails before its
// own release leaves nothing waiting on it.
type mpRelayRow struct {
	a, b      *mpPod
	serving   *mpNATSSession
	publisher *mpPublisher
	warm      mpMessage
	held      mpMessage
	release   func()
}

// mpStartRelayRow starts that opening. lanes puts pod-a behind an mpLaneRelay, for the one row
// that cuts the pod's receive connection alone and leaves its others up.
func mpStartRelayRow(t *testing.T, sessionID string, hold mpRelayHold, lanes bool) mpRelayRow {
	t.Helper()
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	var a, b *mpPod
	if lanes {
		a = mpStartPodBehindLanes(t, binary, uri, "pod-a")
		mpWaitActive(t, a, 30*time.Second)
		b = mpStartPod(t, binary, uri, "pod-b")
		mpWaitStandby(t, b, 30*time.Second)
	} else {
		a, b = mpStartPair(t, binary, uri)
	}
	serving := mpStartNATSSessions(t, uri)
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	var arrived <-chan struct{}
	release := func() {}
	switch hold {
	case mpHoldGated:
		arrived, release = serving.gate(sessionID)
	case mpHoldReceived:
		arrived, release = serving.receiveThenGate(sessionID)
	default:
		arrived = serving.hold(sessionID)
	}
	t.Cleanup(release)
	held := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, held.event)
	}
	return mpRelayRow{a: a, b: b, serving: serving, publisher: publisher, warm: warm, held: held, release: release}
}

// A frame the active pod's relay sent as its session-consumer link dropped reaches the session
// again as soon as the link is back, not half a minute later. The frame is at the session when
// the pod's session-consumer connection drops; the session answers it, but neither that reply
// nor the consumer's acknowledgement can reach the server, so the message stands unacked. The
// link comes back and the relay restarts at the ack floor at once (relayReconnected, the pull
// ending on its connection's event), so the session is given that one message a second time,
// under the same event id, and nothing else repeats.
//
// That repeat is what the merge of #2674 costs here. A frame's reply and the consumer's ack ride
// the one connection, so a drop loses both together and the relay has no second channel it could
// have recorded the delivery on - which is what the delivery ledger on the lease connection was
// for a push to a serving port. What this row holds is that one drop's cost: the frame the drop
// cut comes back once, under its own event id, and at once rather than after the pull's 30 s
// silence, and no other frame comes back with it. That is this drop's shape and not the general
// rule: every relay restart sends again every frame above the ack floor, and a frame that draws
// no reply inside the receipt wait is probed again and again, so a frame can come back more than
// once (README, "When a session can see a message twice").
func TestTwoPodsARelayedFrameTheLinksDroppedUnderIsRepeatedAtMostOnceWithItsId(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_relay_drop_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)
	// A frame the session answered, and whose acknowledgement reached the server, in the moment
	// before the drop: the successor's restart at the ack floor passes over it, so the one
	// repeat this row allows is the frame whose reply the drop lost and no other.
	justBefore := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{justBefore}, 20*time.Second)
	mpEventually(t, 10*time.Second, "the session's consumer to hold nothing unacknowledged", func() bool {
		return publisher.sessionAckPending(sessionID) == 0
	})

	arrived, release := serving.gate(sessionID)
	t.Cleanup(release)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}

	healSessions := a.lanes.drop(mpSessionConsumerConnection)
	t.Cleanup(healSessions)
	disconnected := func(name string) bool {
		return lineWith(a.Output.String(), "envoy nats disconnected", `name=`+name) != "" ||
			lineWith(a.Output.String(), "envoy nats disconnected", `name="`+name+`"`) != ""
	}
	mpEventually(t, 10*time.Second, a.instance+" to read its session-consumer connection dropped", func() bool {
		return disconnected(mpSessionConsumerConnection)
	})
	release()
	time.Sleep(200 * time.Millisecond) // the session answered; the reply and the ack are lost
	reconnects := strings.Count(a.Output.String(), "envoy nats reconnected name="+mpSessionConsumerConnection)
	healed := time.Now()
	healSessions()
	mpEventually(t, 10*time.Second, a.instance+"'s session-consumer connection to reconnect", func() bool {
		return strings.Count(a.Output.String(), "envoy nats reconnected name="+mpSessionConsumerConnection) > reconnects
	})

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	if !a.active() {
		_, body := a.health()
		t.Fatalf("%s lost the lease over the drop, so the row did not keep one relayer: %v", a.instance, body)
	}
	serving.requireOnce(t, []mpMessage{warm, justBefore, after})
	took := serving.requireAtMostTwice(t, m, "the frame the drop cut the reply of")
	serving.requireKnown(t, []mpMessage{warm, justBefore, m, after})
	serving.requireOrder(t, []mpMessage{warm, justBefore, m, after})
	if took == 2 {
		at, _ := serving.last(sessionID, m.event)
		if since := at.Sub(healed); since > 10*time.Second {
			t.Fatalf("the repeat of %s reached %s %s after the link was healed, want the relay's restart at once, not the pull's 30 s silence", m.event, sessionID, since.Round(time.Millisecond))
		}
		t.Logf("%s reached %s twice, the repeat %s after the link was healed", m.event, sessionID, at.Sub(healed).Round(time.Millisecond))
	}
}

// A frame the active pod's relay has had no done for when its term ends is not sent again by the
// pod that takes over. The session's reply is held when the pod is stopped; the term's end
// cancels the relay, which goes on hearing the session for relaySettleGrace
// (settleStoppedFrames), the session answers done inside it, and the stopping relay acks the
// message. The successor waits relayFormerSettle (lease.FormerSettleWait) before it restarts the
// session's consumer, so it restarts at an ack floor past that message and never sends it again.
// The session must take it exactly once. Before the settle the stopping relay forgot the frame
// unacked, the successor was given it again, and the session took it twice: CI met that as
// TestTwoPodsReceiveConnectionLostHandsOverWithin15s's exactly-once check, where
// evt-mp-0079 reached ses_mp_recvlost_one twice (run 41589787158, job 125769769345); this row
// forces that order (the tester's round 27).
func TestTwoPodsARelayedFrameInFlightWhenTheTermEndsIsNotSentAgainByTheSuccessor(t *testing.T) {
	const sessionID = "ses_mp_relay_term_end_once"
	row := mpStartRelayRow(t, sessionID, mpHoldGated, false)
	a, b, serving, publisher := row.a, row.b, row.serving, row.publisher
	warm, m, release := row.warm, row.held, row.release

	a.Terminate(t)
	// Inside relaySettleGrace of the term's end, which the stop reaches in milliseconds: the
	// stopped relay is still listening for this reply and acks the message on it.
	time.Sleep(200 * time.Millisecond)
	release()
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpHardBound)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
}

// A frame the active pod's relay still has no done for when the stopping relay's settle bound
// ends reaches the session at most twice, under its own event id, and no other message repeats.
// The session's reply is held when the pod is stopped; the stopping relay waits relaySettleGrace
// for it, the session answers only well past that, and the successor restarts the session's
// consumer at an ack floor below that message and publishes it again. Only the session's done
// says whether it wrote that frame, and it came after the bound, so no pod can know: the
// stopping pod logs the frame in doubt (relayFrameInDoubtLine) and the successor's restart sends
// again every frame above the ack floor, which here is this one (cmd/listener/README.md, "When a
// session can see a message twice").
// What the row refuses is a second repeat of it, a repeat of any other message, an event id no
// message carried, and a stopping pod that says nothing about the frame it cut (INFRA-557
// tester's round 28, the architect's edge (2)).
func TestTwoPodsARelayedFrameHeldPastTheStoppingRelaysSettleBoundIsRepeatedAtMostOnceWithItsId(t *testing.T) {
	const sessionID = "ses_mp_relay_past_settle_once"
	row := mpStartRelayRow(t, sessionID, mpHoldGated, false)
	a, b, serving, publisher := row.a, row.b, row.serving, row.publisher
	warm, m, release := row.warm, row.held, row.release

	a.Terminate(t)
	time.Sleep(relaySettleGrace + 2*time.Second) // past the stopping relay's settle bound
	release()                                    // the session answers the frame nobody waits for
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpHardBound)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, after})
	serving.requireAtMostTwice(t, m, "the frame whose reply the settle bound cut off")
	serving.requireKnown(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
	if lineWith(a.Output.String(), `"msg":"`+relayFrameInDoubtLine+`"`, m.event) == "" {
		t.Fatalf("%s logged no in-doubt line naming %s, so the frame its settle bound cut was left unsaid", a.instance, m.event)
	}
}

// The hard take-over of the one delivery path that still runs through the listener: the relay
// over NATS. The active pod relays a frame to a session that never answers it - its `received`
// reply is still out - and the pod is SIGKILLed with that frame in flight. Nothing settled it:
// the killed pod wrote no ack, so the message stands unacked in the session's consumer, and the
// standby that takes the lease over restarts that consumer at its ack floor and sends it again.
// The session must have it within the 15 s a hard take-over is bounded at, every other message
// exactly once, and that one frame at most twice, under its own event id.
//
// The bound is the lease's, not the durable's. A session on its own consumer is relayed from
// the moment a term begins, so nothing of this delivery waits out the successor's settle hold:
// raising receiveTiming.settleCap by 30 s leaves the row passing (measured: the session had the
// frame 4.849 s after the kill either way). What the 15 s pays for is reading the dead holder's
// record unchanged for a full leaseTTL, taking it at the next leaseStandbyPoll, and restarting
// the session's consumer at its ack floor. Raise leaseTTL by 30 s and the row fails on the
// 15 s bound, which is what makes the bound the row's subject rather than a side effect of how
// fast the box is.
//
// settleCap keeps its own guard on the durable lane, where a take-over's delivery does wait for
// the hold: TestASettleHoldEndsAtItsCap (cmd/listener/receive_settle_test.go) pins the
// production cap at the durable's ack wait and a second, and holds a take-over's first delivery
// behind a cap of its own. Raise settleCap by 30 s and that row fails.
func TestTwoPodsSIGKILLWithAFrameUnansweredReachesTheSessionWithin15s(t *testing.T) {
	const sessionID = "ses_mp_kill_frame_once"
	row := mpStartRelayRow(t, sessionID, mpHoldNever, false)
	a, b, serving, publisher := row.a, row.b, row.serving, row.publisher
	warm, m := row.warm, row.held

	killed := a.kill(t)
	a.WaitExit(t, "SIGKILL")
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, mpHardBound+15*time.Second)
	serving.requireWithin(t, []mpMessage{m}, mpHardBound, killed)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, after})
	serving.requireAtMostTwice(t, m, "the frame in flight at the kill")
	serving.requireKnown(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
}

// A frame the session received but had not written when its relay's term ended is repeated at
// most once, under its own event id. The session answers the frame "received" at once - it has
// the frame - and "done" only once it has written it, here 2 s past the stopping relay's settle
// grace. The pod is stopped in between. "received" says the client decoded the frame, not that
// its host wrote it, so the stopping relay settles a frame on "done" alone (settleStoppedFrames
// waits for outstanding, which only done or nak clears): this one is left in doubt when the
// grace ends and the successor sends it once more. The session must take that frame at most
// twice and every other message exactly once, and be given no event id no message
// carried. The repeat costs the host nothing: the real client writes a redelivered copy once
// (packages/envoy-client/src/consumer-delivery.ts joins a copy of a frame it still holds and
// drops one it has written). The same holds when the term ends for a lost receive connection
// rather than a stop (INFRA-557 tester's round 29).
func TestTwoPodsARelayedFrameReceivedButNotDoneAtAStopIsRepeatedAtMostOnceWithItsId(t *testing.T) {
	const sessionID = "ses_mp_relay_received_once"
	row := mpStartRelayRow(t, sessionID, mpHoldReceived, false)
	a, b, serving, publisher := row.a, row.b, row.serving, row.publisher
	warm, m, release := row.warm, row.held, row.release

	a.Terminate(t)
	time.Sleep(relaySettleGrace + 2*time.Second) // the session is still writing the frame it received
	release()
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpHardBound)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, after})
	serving.requireAtMostTwice(t, m, "the frame received but not written when the term ended")
	serving.requireKnown(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
}

// A frame the session answers inside the stopping relay's settle grace reaches it once when the
// term ends for a lost receive connection, not a stop. The session has the frame ("received") and
// is writing it when the pod's receive connection alone is cut; the pod releases the lease after
// leaseReceiveLost and its relays stop, while its session-consumer connection stays up. The
// session answers "done" 200 ms after that release, inside relaySettleGrace, so the stopping relay
// acks the message. The standby takes the released lease at its next poll - about 100 ms after the
// release, well inside the grace - and must not restart the session's consumer until
// relayFormerSettle has passed (lease.FormerSettleWait), or it is given the message again before
// the ack lands. This is the one row that reads that wait: the stop rows reach the successor only
// after the stopping pod has exited. With relayFormerSettle at zero it fails 3 of 3, the message
// taken twice (INFRA-557 tester's round 29).
func TestTwoPodsARelayedFrameAnsweredInsideTheGraceOfAReceiveLostReleaseIsTakenOnce(t *testing.T) {
	const sessionID = "ses_mp_relay_recvlost_grace_once"
	row := mpStartRelayRow(t, sessionID, mpHoldReceived, true)
	a, b, serving, publisher := row.a, row.b, row.serving, row.publisher
	warm, m, release := row.warm, row.held, row.release
	t.Cleanup(a.lanes.cut(mpReceiveConnection(a.instance)))
	mpEventually(t, mpHardBound, a.instance+" to release the lease for its lost receive connection", func() bool {
		return lineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`) != ""
	})
	time.Sleep(200 * time.Millisecond) // inside relaySettleGrace of the release
	release()
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
}
