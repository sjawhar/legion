//go:build multiprocess

// The hand-back rows of the two-pod suite (multiprocess_harness_test.go): what a graceful stop,
// one after a node loss, one during another pod's settle hold, one with messages on the wire and
// one with a retry outstanding must leave each session, and the SIGKILLs beside them. Its helpers
// set a row's pods up for a hand-over chain.

package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// mpTakeoverHolder waits until one of pods logs that it took the machine's expired lease, and
// returns that pod and the other one.
func mpTakeoverHolder(t *testing.T, within time.Duration, first, second *mpPod) (holder, other *mpPod) {
	t.Helper()
	mpEventually(t, within, "a standby to take the expired lease", func() bool {
		for _, pod := range []*mpPod{first, second} {
			if strings.Contains(pod.Output.String(), `"from":"expired"`) {
				holder = pod
			}
		}
		return holder != nil
	})
	if holder == first {
		return first, second
	}
	return second, first
}

// A lost node, then a graceful stop of the pod that took its expired lease, with a second standby
// running (a rolling update, or a rescheduled pod, after a node loss). The server keeps the lost
// pod's connection, and with it the durable's push binding, until its ping timeout, minutes in
// production; the second standby takes the released lease and must deliver all the same: every
// message published since the node was lost reaches its session, and every message published after
// the SIGTERM within 10 s of it. A standby that waited for the lost pod's binding to end instead
// would log "durable is push-bound by a subscriber without the lease; waiting for its binding to
// end" and deliver nothing until that connection goes (3 min 42 s against a real nats-server
// 2.10.29), while the server redelivers each message to that connection every ack wait until
// MaxDeliver is spent, so a message published meanwhile is never delivered at all.
func TestTwoPodsGracefulStopAfterANodeLossHandsOverWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_lossstop_one", "ses_mp_lossstop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	serving.awaitAll(t, publisher.since(frozen), mpHardBound+5*time.Second)

	signalled := holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	serving.awaitAll(t, publisher.since(frozen), mpGracefulBound+5*time.Second)
	serving.requireWithin(t, publisher.since(signalled), mpGracefulBound, signalled)
	mpWaitActive(t, other, time.Second)
}

// A lost node, then a graceful stop of the pod that took its expired lease with no standby
// running at all, then a pod started after it. The lost pod's connection still holds the durable's
// push binding, and the stopping pod's linger ends "not taken" a second after its release
// (lingerUntaken), so from then on that binding is the deliver subject's only subscriber: what the
// stopping pod handed back, and every message published after it drained, reaches a connection
// that acknowledges nothing and comes back only at its ack wait. That stop writes no
// clean-hand-back mark - its linger saw no other pod receiving - so the pod started afterwards
// settles for an ack wait, 7 s, not for a hand-back's 2 s delay: every message reaches its
// session in the order it was published. A record counted a clean hand-back by what it carries
// alone would let the new pod hold for 3 s and deliver the messages published after the stop
// before the older ones still due from the lost node's binding.
func TestTwoPodsGracefulStopWithNoStandbyAfterANodeLossKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_alone_one", "ses_mp_alone_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpWaitActive(t, b, mpHardBound)
	serving.awaitAll(t, publisher.since(frozen), mpHardBound+5*time.Second)

	// No standby is left: the lease record is released to nobody, and the lost node's binding has
	// the deliver subject to itself until a pod starts.
	b.Terminate(t)
	b.WaitExit(t, "SIGTERM")
	time.Sleep(3 * time.Second) // publish on with no pod receiving
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitActive(t, c, 30*time.Second)
	time.Sleep(2 * time.Second) // publish on past the take-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpHardBound+20*time.Second)
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	if line := mpLineWith(c.Output.String(), "settle hold ended", `"hand_back_marked":false`); line == "" {
		t.Fatalf("%s did not hold for an ack wait after a stop that marked no hand-back:\n%s",
			c.instance, mpTail(mpLinesWithout(c.Output.String()), 40))
	}
}

// A lost node, then, while the pod that took its expired lease still holds what it received for
// one ack wait, a graceful stop of that pod, with a second standby running. The lost pod is gone
// for good by then (killed, its connection closed), so no push binding outlives it. The messages
// the server pushed to the lost pod come back over one ack wait, after newer ones, some to the
// stopping holder and the rest to the second standby, which takes the released lease: each
// session must still take its messages in the order they were published, as after a hard
// take-over alone. A stopping holder that NAKed what it holds in the order it received it, with
// the second standby delivering at once and without the hold, would let messages published
// before the take-over reach their session after newer ones.
func TestTwoPodsGracefulStopDuringAHardTakeoversHoldKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_holdstop_one", "ses_mp_holdstop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	a.kill(t)
	a.relay.close()             // the lost pod's connection goes, so the second standby finds no binding
	time.Sleep(2 * time.Second) // inside the new holder's hold of one ack wait and a second
	holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	mpWaitActive(t, other, time.Second)
}

// A graceful stop of the active pod while messages are on the wire to it: its link delivers
// 300 ms late, so when it unsubscribes the server has already pushed it the messages of the last
// 300 ms. Per-session order must still hold across the hand-over (README "Take-over": the stopping
// pod NAKs what it has not handled, and the durable redelivers those before newer messages). A
// stopping pod that dropped what reaches it after its unsubscribe without a NAK would let the
// standby deliver newer messages at once and the dropped ones come back one ack wait (6 s)
// later, behind them. Without the delay the same happens under load: at a message every 2 ms one
// message in about three stops arrived 6 s late and out of order.
func TestTwoPodsGracefulStopWithMessagesOnTheWireKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	sessions := []string{"ses_mp_wirestop_one", "ses_mp_wirestop_two"}
	for _, sessionID := range sessions {
		a.registerPort(t, serving, sessionID)
	}
	a.relay.delay(300 * time.Millisecond)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(50*time.Millisecond, sessions...)
	time.Sleep(3 * time.Second)
	a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	mpWaitActive(t, b, time.Second)
}

// SIGKILL of the active pod while a prompt to session T is in flight, then more messages to T. The
// standby takes over: T must take every message, the one in flight at the kill included, in the
// order they were published and within the hard bound (architect Q2: in order within 15 s; a
// repeat with the same id is allowed). TestTwoPodsSIGKILLHandsOverInOrderWithin15s leaves the
// message in flight out of what it checks. The new holder must take the killed pod's ledger
// attempt for that message over at once: deferred as another pod's live attempt, it would reach
// T 35 s after the kill, after every newer message. The killed pod is the lease holder whose
// expired record the new holder took, so its attempt is no live pod's.
func TestTwoPodsSIGKILLMidPromptKeepsTheSessionsOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartServingPort(t)
	const blocked = "ses_mp_midprompt"
	a.registerPort(t, serving, blocked)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(blocked)
	first := publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, blocked)
	}
	killed := a.kill(t)
	stop := publisher.every(200*time.Millisecond, blocked)
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{first}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+30*time.Second)
	serving.requireOrder(t, messages)
	serving.requireWithin(t, messages, mpHardBound, killed)
	mpWaitActive(t, b, time.Second)
}

// mpRetryOutstanding registers on pod a a session whose prompts fail and two healthy sessions,
// named after row, and has a prompt the failing session and NAK its message for the delivery
// handler's 30 s retry, which keeps that message in the durable's pending until its delay ends. It
// returns the serving port, the publisher and the healthy sessions.
func mpRetryOutstanding(t *testing.T, uri string, a *mpPod, row string) (serving *mpServingPort, publisher *mpPublisher, healthy []string) {
	t.Helper()
	serving = mpStartServingPort(t)
	failing := "ses_mp_" + row + "_failing"
	healthy = []string{"ses_mp_" + row + "_one", "ses_mp_" + row + "_two"}
	for _, sessionID := range append([]string{failing}, healthy...) {
		a.registerPort(t, serving, sessionID)
	}
	publisher = mpConnectPublisher(t, uri)
	serving.fail(failing)
	retry := publisher.mustPublish(t, failing)
	a.awaitEventLine(t, 10*time.Second, "listener agent delivery failed", retry.event)
	time.Sleep(500 * time.Millisecond) // past the NAK that follows the failure's record
	if pending := publisher.ackPending(); pending != 1 {
		t.Fatalf("the durable has %d messages outstanding after the failed prompt, want its retry alone", pending)
	}
	return serving, publisher, healthy
}

// mpTo is the messages published to the sessions given.
func mpTo(messages []mpMessage, sessions ...string) []mpMessage {
	var to []mpMessage
	for _, m := range messages {
		if slices.Contains(sessions, m.session) {
			to = append(to, m)
		}
	}
	return to
}

// A graceful stop of the active pod while a message to a session whose prompts fail waits out its
// 30 s retry delay, with a message published every 100 ms to two healthy sessions. The retry stays
// in the durable's pending until its delay ends, so the standby's settle hold never finds every
// outstanding message in hand: it ends on the stopping pod's clean-hand-back mark, at what that
// pod can still have outstanding, the 2 s hand-back delay and a second after the subscribe, and
// every message to a healthy session published after the SIGTERM reaches it within 10 s, each
// session's in order. Without that mark the hold would run to its 7 s cap and those messages
// would arrive about 14 s after the SIGTERM.
func TestTwoPodsGracefulStopWithARetryOutstandingDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "retrystop")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(signalled), healthy...), mpGracefulBound, signalled)
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, time.Second)
	if line := mpLineWith(a.Output.String(), "handed the durable's messages back", `"hand_back_marked":true`); line == "" {
		t.Fatalf("%s did not mark its hand-back clean:\n%s", a.instance, mpTail(mpLinesWithout(a.Output.String()), 40))
	}
	if line := mpLineWith(b.Output.String(), "settle hold ended", `"outcome":"handed back"`); line == "" {
		t.Fatalf("%s did not end its settle hold on %s's mark:\n%s", b.instance, a.instance, mpTail(mpLinesWithout(b.Output.String()), 40))
	}
}

// Graceful stops one after another, A to B to C to D, each with a prompt in hand that the serving
// port never answers and a message to a session whose prompts fail waiting out its 30 s retry
// delay, with a message published every 100 ms to two healthy sessions. Each stop comes a second
// into the prompt, with the healthy sessions' messages queued behind it; the stopping pod cancels
// the prompt after the 3 s grace and NAKs it for a retry, so with the failing session's retry it
// holds every next pod's settle hold open: each hand-over must end on the stopping pod's
// clean-hand-back mark, 3 s after the subscribe, and every message to a healthy session must
// reach it within 10 s of its publish, each session's in order. The pod taking over subscribes
// beside the stopping pod's own linger and records stale_binding for it, which the mark clears
// again; were it left standing, the next stop in the chain would mark nothing and the hand-over
// after it would hold to the 7 s cap of a hard take-over, with the messages queued at the SIGTERM
// arriving about 11 s after their publish.
func TestTwoPodsEveryGracefulStopWithAPromptInHandAndARetryOutstandingDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "everystop")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	mpGracefulStopsInTurn(t, binary, uri, serving, publisher, healthy, "everystop", a, b, "pod-c", "pod-d")
	time.Sleep(4 * time.Second) // publish on past the last hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
}

// A lost node, then graceful stops one after another of the pod that took its expired lease and of
// each pod after it, B to C to D, each with a prompt in hand and a retry outstanding as in
// TestTwoPodsEveryGracefulStopWithAPromptInHandAndARetryOutstandingDeliversWithin10s. The server
// keeps the lost pod's connection, and with it the durable's push binding, until its ping timeout,
// minutes in production, so B subscribes beside it and marks the record stale_binding, which no
// later holder can clear while that binding stands. A stop whose term had such a binding open
// marks no clean hand-back: what the durable pushed to that connection it may have pushed there
// alone, and that comes back only at its ack wait. So every hand-over of this row settles for
// 7 s, the hard take-over's bound, rather than the hand-back's 3 s, and every message to a
// healthy session still reaches it in order, within the 15 s a hard take-over is given. The cost
// is the node loss's: it lasts until the server drops that connection, and no mark is written in
// the meantime, which is the safe answer rather than the fast one.
func TestTwoPodsEveryGracefulStopAfterANodeLossWithAPromptInHandAndARetryOutstandingDeliversInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	t.Cleanup(a.relay.freeze()) // the node is lost: its connection stays open, silent
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	mpWaitActive(t, holder, mpHardBound)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, holder, "lossstops")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	mpGracefulStopsInTurn(t, binary, uri, serving, publisher, healthy, "lossstops", holder, other, "pod-d")
	time.Sleep(4 * time.Second) // publish on past the last hand-over
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpHardBound+10*time.Second)
	serving.requireWithin(t, messages, mpHardBound, time.Time{})
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	for _, pod := range []*mpPod{holder, other} {
		if line := mpLineWith(pod.Output.String(), "handed the durable's messages back", `"hand_back_marked":true`); line != "" {
			t.Fatalf("%s marked its hand-back clean though a lost node's binding stood beside it: %s", pod.instance, line)
		}
	}
}

// mpGracefulStopsInTurn stops stopping gracefully, then each pod that takes its place in turn, with
// next the first stop's standby and each later standby a pod started then, named after standbys,
// one per stop but the first. Each stop comes once the active pod has delivered every message to
// healthy published so far, past its own settle hold, and a second into a prompt that the serving
// port never answers, to a session of row's registered for it, with healthy's messages queued
// behind it. It returns once the last stop's standby is active.
func mpGracefulStopsInTurn(t *testing.T, binary, uri string, serving *mpServingPort, publisher *mpPublisher, healthy []string, row string, stopping, next *mpPod, standbys ...string) {
	t.Helper()
	for handOver := range len(standbys) + 1 {
		sessionID := fmt.Sprintf("ses_mp_%s_slow_%d", row, handOver+1)
		stopping.registerPort(t, serving, sessionID)
		sent := mpTo(publisher.all(), healthy...)
		mpEventually(t, mpGracefulBound, stopping.instance+" to deliver what was published before its prompt in hand", func() bool {
			return serving.tookAll(sent)
		})
		inHand := serving.hold(sessionID)
		publisher.mustPublish(t, sessionID)
		select {
		case <-inHand:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never prompted %s", stopping.instance, sessionID)
		}
		time.Sleep(time.Second) // healthy's messages queue behind the prompt in hand
		stopping.Terminate(t)
		stopping.WaitExit(t, "SIGTERM")
		mpWaitActive(t, next, mpGracefulBound)
		if handOver == len(standbys) {
			return
		}
		standby := mpStartPod(t, binary, uri, standbys[handOver])
		mpWaitStandby(t, standby, 30*time.Second)
		stopping, next = next, standby
	}
}

// SIGKILL of the active pod while a message to a session whose prompts fail waits out its 30 s
// retry delay, with a message published every 100 ms to two healthy sessions. The standby takes the
// expired lease and its settle hold never finds every outstanding message in hand: it ends at what
// the killed pod can still have outstanding, one 6 s ack wait and a second after the subscribe, and
// every message to a healthy session published after the kill reaches it within 15 s, each
// session's in order. A hold that ran to its 13 s cap instead would let those messages arrive
// about 19 s after the kill.
func TestTwoPodsSIGKILLWithARetryOutstandingHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "retrykill")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	killed := a.kill(t)
	time.Sleep(3 * time.Second) // publish on after the kill
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpHardBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(killed), healthy...), mpHardBound, killed)
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	mpWaitActive(t, b, time.Second)
}
