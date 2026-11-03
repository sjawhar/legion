//go:build multiprocess

// The core-path rows of the two-pod suite (multiprocess_harness_test.go): what a SIGKILL and a
// graceful stop must leave a serving-port session delivered to from the machine's durable alone,
// with no session consumer of its own. Their helper is mpCorePathRetryOutstanding
// (multiprocess_handback_test.go).

package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The core path's take-over, which the rows of the other files read nowhere: every session the
// rows on mpNATS register gets a session consumer, whose relay pushes its stored messages in
// milliseconds and starts as soon as a term begins, so the durable's own hand-over of a
// serving-port session - the lease, the new holder's settle hold, then its serving-port worker -
// is measured by none of them. Production still has such sessions wherever the listener's NATS
// user carries no session consumer's call (no_grant) or a consumer call fails (consumer_error),
// so the two rows below put every session of theirs on that path (mpCorePathNATS,
// registerCorePath) and bound both take-overs of it.
//
// Each leaves messages parked at a port that refuses them outstanding on the durable
// (mpRetryOutstanding), so the new holder's settle hold never finds every outstanding message in
// hand and runs its course rather than ending early, and holds one session's first message in a
// serving-port worker of the pod that goes away, with its later messages queued behind it.
//
// SIGKILL of the active pod with that session's message in flight: every message reaches its
// session, in publish order, within 15 s of the kill, and each reaches it once - except a
// message whose delivery was in flight at the kill, which may reach it twice, the one repeat a
// hard take-over allows (README.md, "A repeat with the same event id is possible after a hard
// take-over"): the killed pod had pushed it and died before its delivered record, which is what
// would have told the pod taking over. In flight at the kill is read as published before it and
// first taken by its session within mpKillInFlight on either side of it
// (requireOnceExceptInFlight); every other message is prompted exactly once, whether the killed
// pod had time to record it or only the pod taking over ever pushed it, and a repeat of one is a
// fault of the take-over. The killed pod's worker dies with it, so what it held comes
// back at the durable's 6 s ack wait, and the new holder's hold, which runs to its 7 s cap here,
// is what puts it back before the newer ones. With settleCap raised by 30 s, as the tester
// raised it, that hold runs past the bound and this row fails.
func TestTwoPodsSIGKILLHandsOverACorePathPortSessionInOrderWithin15s(t *testing.T) {
	uri := mpCorePathNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpCorePathRetryOutstanding(t, uri, a, "corekill")
	const blocked = "ses_mp_core_kill_held"
	a.registerCorePath(t, serving, blocked)
	sessions := append([]string{blocked}, healthy...)
	watch := mpWatchSessions(t, b, sessions...)

	// The session's first message is in flight to a port that never answers it: it stays in
	// pod-a's serving-port worker, and every later message of that session queues behind it.
	inHand := serving.hold(blocked)
	publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, blocked)
	}
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	killed := a.kill(t)
	time.Sleep(3 * time.Second) // publish on after the kill
	stop()

	messages := mpTo(publisher.all(), sessions...)
	serving.awaitAll(t, messages, mpHardBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(killed), sessions...), mpHardBound, killed)
	serving.requireOrder(t, messages)
	serving.requireOnceExceptInFlight(t, messages, killed, mpKillInFlight)
	serving.requireKnown(t, publisher.all())
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, killed)
}

// The graceful stop of the same core-path session, its first message in a serving-port worker of
// the stopping pod and its later ones queued behind it: every message reaches its session once,
// in publish order, within 10 s of the SIGTERM.
//
// This is the row the stop's worker fix is for (receive_stop.go, servePortWorkers.endTerm).
// Without it the stopping pod's workers outlive its term: what they hold is in no hand-back
// batch, the stop marks the hand-over clean all the same, and the standby's hold ends on that
// mark 3 s in and pushes the session's later messages at once, while the messages those workers
// held come back only at their ack wait - after them, and past the bound.
func TestTwoPodsGracefulStopHandsOverACorePathPortSessionWithin10s(t *testing.T) {
	uri := mpCorePathNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpCorePathRetryOutstanding(t, uri, a, "corestop")
	const blocked = "ses_mp_core_stop_held"
	a.registerCorePath(t, serving, blocked)
	sessions := append([]string{blocked}, healthy...)
	watch := mpWatchSessions(t, b, sessions...)

	inHand := serving.hold(blocked)
	publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, blocked)
	}
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()
	a.WaitExit(t, "SIGTERM")

	messages := mpTo(publisher.all(), sessions...)
	serving.awaitAll(t, messages, mpGracefulBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(signalled), sessions...), mpGracefulBound, signalled)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)
	serving.requireKnown(t, publisher.all())
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, signalled)
}
