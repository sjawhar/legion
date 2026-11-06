//go:build multiprocess

// Two listener builds for one machine id, as a roll from one release to the next runs them
// (INFRA-557): one build that compiles in a stream subject the other does not know, and, when
// ENVOY_LISTENER_OLD_BINARY names one, a build from before the delivery lease. Each row starts the
// builds' real binaries through multiprocess_harness_test.go's harness and checks that no message
// on any subject is skipped, doubled, or dropped by both while the machine's delivery passes from
// one build to the other. Like the rows beside it it uses nothing a build before INFRA-557
// (819dc283) lacks but cmdtest.Build's tags, so it runs there with that harness beside it.

package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

const (
	// tbEvery is how often a row publishes, each time on the next of its topics in turn.
	tbEvery = 40 * time.Millisecond
)

// tbTargets is a topic on every subject the plain build's stream stores (bus.StreamSubjects),
// each with its own session, named for the subject so a report names the subject that lost or
// doubled a message. A subject no session may follow is left out: /v1/interests/subscribe drops
// a topic reaching a private stream subject other than the session's own agent subject
// (bus.ReachesPrivateStreamSubject: the Legion controller lane and the failed-delivery lanes),
// so a session on one would never be relayed.
//
// `notifications.twobuild.>`, the subject only the other build's stream carries
// (internal/bus/stream_extra_twobuild.go), gets no session: a session consumer's filter is this
// build's own list of the subjects the stream stores (bus.StreamCaptures, through the
// registration's followed topics), so a pod of the plain build cannot relay a topic on a subject
// it does not compile in, whatever the durable's filter takes.
// A session following such a topic is served only while a pod of the build that knows the
// subject holds the term; that is a property of per-session consumers, not of this merge, and it
// is recorded in the pull request rather than measured here.
func tbTargets() []mpTarget {
	subjects := bus.StreamSubjects()
	var targets []mpTarget
	for _, subject := range subjects {
		prefix := strings.TrimSuffix(subject, ">")
		name := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(prefix, bus.NotificationTopicPrefix), "."), ".", "_")
		sessionID := "ses_tb_" + name
		if prefix == "notifications.agent." {
			targets = append(targets, mpInbox(sessionID))
			continue
		}
		topic := prefix + "mp-twobuild"
		if bus.ReachesPrivateStreamSubject(sessionID, topic) {
			continue
		}
		targets = append(targets, mpTarget{topic: topic, session: sessionID})
	}
	return targets
}

// tbRegister registers every target's session through the pod, on consumer delivery, subscribed to
// its topic unless the topic is its own inbox.
func tbRegister(t *testing.T, pod *mpPod, serving *mpNATSSession, targets []mpTarget) {
	t.Helper()
	for _, target := range targets {
		if target == mpInbox(target.session) {
			pod.registerNATS(t, serving, target.session)
			continue
		}
		pod.registerNATS(t, serving, target.session, target.topic)
	}
}

// tbKill SIGKILLs the pod between two publishes, once every message published so far has reached
// its session and the durable holds none unacknowledged, and resumes publishing a second after the
// pod has exited. A message a killed pod holds is TestTwoPodsSIGKILLHandsOverInOrderWithin15s's;
// here the kill is the hand-over from one build to the other. Nor is a message sent over core
// NATS, which the server hands the killed pod before it sees that pod's connection close,
// publishable then: a core message is delivered at most once, and the second after the exit is
// the server's to drop the dead pod's subscription. A message the previous steps skipped is
// named here, by awaitAll, before the kill.
func tbKill(t *testing.T, publisher *mpPublisher, serving *mpNATSSession, pod *mpPod) time.Time {
	t.Helper()
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	serving.awaitAll(t, slices.Clone(publisher.sent), 30*time.Second)
	mpEventually(t, 10*time.Second, "the durable to hold no message unacknowledged", func() bool { return publisher.ackPending() == 0 })
	killed := pod.kill(t)
	pod.WaitExit(t, "SIGKILL")
	time.Sleep(time.Second)
	return killed
}

// tbRequireEachOnce waits for every message to reach its session, then past the ack wait for any
// repeat, and fails the test unless each reached it exactly once.
func tbRequireEachOnce(t *testing.T, publisher *mpPublisher, serving *mpNATSSession) {
	t.Helper()
	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireKnown(t, messages)
	serving.requireOnce(t, messages)
	perSession := map[string]int{}
	for _, m := range messages {
		perSession[m.session]++
	}
	t.Logf("%d messages, each delivered exactly once: %v", len(messages), perSession)
}

// No message on any subject is skipped, doubled, or dropped by both while two builds share the
// machine's delivery: a plain build and one built with the envoy_twobuild tag, which compiles in a
// stream subject more, through a graceful and a hard take-over in each direction. The plain build is
// active and the other build standby; a SIGTERM of the plain pod makes the other build active; a
// plain pod starts as its standby; a SIGKILL of the other build makes the plain build active again.
// A message is published every 40 ms throughout, on each subject the stream stores, the added one
// included, each to a session of its own: each session takes each of its messages exactly once.
// Before INFRA-557 the plain build's durable filter lacks the added subject, so a message on it
// the stream stores is skipped.
func TestTwoBuildsSkipOrDoubleNoMessage(t *testing.T) {
	plain := cmdtest.Build(t, "envoy-listener")

	t.Run("a build with a stream subject more", func(t *testing.T) {
		extra := cmdtest.Build(t, "envoy-listener-twobuild", "envoy_twobuild")
		uri := mpNATS(t)
		serving := mpStartNATSSessions(t, uri)
		targets := tbTargets()
		plainA := mpStartPod(t, plain, uri, "pod-a")
		mpWaitActive(t, plainA, 30*time.Second)
		tbRegister(t, plainA, serving, targets)
		publisher := mpConnectPublisher(t, uri)
		stop := publisher.everyOn(tbEvery, targets...)
		time.Sleep(3 * time.Second)

		extraB := mpStartPod(t, extra, uri, "pod-b")
		mpWaitStandby(t, extraB, 30*time.Second)
		time.Sleep(3 * time.Second)

		signalled := plainA.Terminate(t)
		// A build before INFRA-557 keeps the durable through its shutdown, and the standby binds
		// only after it.
		mpWaitActive(t, extraB, 60*time.Second)
		t.Logf("%s (envoy_twobuild) took over %s after %s's SIGTERM", extraB.instance, time.Since(signalled).Round(time.Millisecond), plainA.instance)
		plainA.WaitExit(t, "SIGTERM")
		time.Sleep(3 * time.Second)

		plainC := mpStartPod(t, plain, uri, "pod-c")
		mpWaitStandby(t, plainC, 30*time.Second)
		time.Sleep(3 * time.Second)

		killed := tbKill(t, publisher, serving, extraB)
		mpWaitActive(t, plainC, 30*time.Second)
		t.Logf("%s (plain) took over %s after %s's SIGKILL", plainC.instance, time.Since(killed).Round(time.Millisecond), extraB.instance)
		time.Sleep(3 * time.Second)
		stop()

		tbRequireEachOnce(t, publisher, serving)
	})
}
