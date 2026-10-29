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
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// tbExtraSubject is the stream subject internal/bus/stream_extra_twobuild.go adds to a binary
	// built with the envoy_twobuild tag, and no plain build compiles in.
	tbExtraSubject = "notifications.twobuild.>"
	// tbLiveTopic is a topic no build's stream stores, which only a listener's live lane delivers.
	tbLiveTopic = "notifications.team.mp-twobuild"
	// tbEvery is how often a row publishes, each time on the next of its topics in turn.
	tbEvery = 40 * time.Millisecond
	// tbLive turns every pod's live lane on, so the topics the stream does not store are delivered
	// and the live lane's split of the topics from the durable's is under test. Only a host's grant
	// allows the subscription to every topic it makes, so the rows' pods hold testnats.HostGrant.
	tbLive = "ENVOY_SUBSCRIBE_ALL_TOPICS=1"
)

// tbTargets is a topic on every subject the stream stores, the plain build's (bus.StreamSubjects)
// and, with extra, tbExtraSubject, and tbLiveTopic, each with its own port session, named for the
// subject so a report names the subject that lost or doubled a message. A subject no session may
// follow is left out: /v1/interests/subscribe drops a topic reaching a private stream subject other
// than the session's own agent subject (bus.ReachesPrivateStreamSubject: the Legion controller lane
// and the failed-delivery lanes), so a port session on one would never be prompted.
func tbTargets(extra bool) []mpTarget {
	subjects := bus.StreamSubjects()
	if extra {
		subjects = append(subjects, tbExtraSubject)
	}
	targets := []mpTarget{{topic: tbLiveTopic, session: "ses_tb_team"}}
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

// tbRegister registers every target's session through the pod, on the serving port, subscribed to
// its topic unless the topic is its own inbox.
func tbRegister(t *testing.T, pod *mpPod, serving *mpServingPort, targets []mpTarget) {
	t.Helper()
	for _, target := range targets {
		if target == mpInbox(target.session) {
			pod.registerPort(t, serving, target.session)
			continue
		}
		pod.registerPort(t, serving, target.session, target.topic)
	}
}

// tbKill SIGKILLs the pod between two publishes, once every message published so far has reached
// its session and the durable holds none unacknowledged, and resumes publishing a second after the
// pod has exited. A message a killed pod holds is TestTwoPodsSIGKILLHandsOverInOrderWithin15s's;
// here the kill is the hand-over from one build to the other. Nor is a live-lane message NATS hands
// the killed pod before the server sees its connection close publishable: the live lane delivers
// at most once on a kill (dispatch://INFRA-14), and the second after the exit is the server's to
// drop the dead pod's queue subscription. A message the previous steps skipped is named here, by
// awaitAll, before the kill.
func tbKill(t *testing.T, publisher *mpPublisher, serving *mpServingPort, pod *mpPod) time.Time {
	t.Helper()
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	serving.awaitAll(t, slices.Clone(publisher.sent), 30*time.Second)
	mpEventually(t, 10*time.Second, "the durable to hold no message unacknowledged", func() bool { return publisher.ackPending() == 0 })
	killed := pod.kill(t)
	select {
	case <-pod.Exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was still running 10 s after SIGKILL", pod.instance)
	}
	time.Sleep(time.Second)
	return killed
}

// tbRequireEachOnce waits for every message to reach its session, then past the ack wait for any
// repeat, and fails the test unless each reached it exactly once.
func tbRequireEachOnce(t *testing.T, publisher *mpPublisher, serving *mpServingPort) {
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
	t.Logf("%d messages, each prompted exactly once: %v", len(messages), perSession)
}

// No message on any subject is skipped, doubled, or dropped by both while two builds share the
// machine's delivery: a plain build and one built with the envoy_twobuild tag, which compiles in a
// stream subject more, through a graceful and a hard take-over in each direction. The plain build is
// active and the other build standby; a SIGTERM of the plain pod makes the other build active; a
// plain pod starts as its standby; a SIGKILL of the other build makes the plain build active again.
// A message is published every 40 ms throughout, on each subject the stream stores, the added one
// included, and on a topic it does not store, which only the live lane delivers, each to a port
// session of its own: each session takes each of its messages exactly once. Before INFRA-557 the
// plain build's durable filter lacks the added subject, so a message on it the stream stores is
// skipped, and the plain build's live lane, splitting the topics by the subjects it compiles in,
// delivers that subject beside the other build's durable, so it is doubled.
//
// With ENVOY_LISTENER_OLD_BINARY naming a build from before the delivery lease (819dc283 built by
// hand, as the plan's recorded proof), the first roll onto the lease runs too: the old build active,
// this build standby, waiting on the old build's binding, then a SIGTERM of the old build hands the
// machine's delivery to this build with no message skipped or doubled.
func TestTwoBuildsSkipOrDoubleNoMessage(t *testing.T) {
	plain := cmdtest.Build(t, "envoy-listener")

	t.Run("a build with a stream subject more", func(t *testing.T) {
		extra := cmdtest.Build(t, "envoy-listener-twobuild", "envoy_twobuild")
		_, uri := mpStartNATS(t, testnats.HostGrant)
		serving := mpStartServingPort(t)
		targets := tbTargets(true)
		plainA := mpStartPod(t, plain, uri, "pod-a", tbLive)
		mpWaitActive(t, plainA, 30*time.Second)
		tbRegister(t, plainA, serving, targets)
		publisher := mpConnectPublisher(t, uri)
		stop := publisher.everyOn(tbEvery, targets...)
		time.Sleep(3 * time.Second)

		extraB := mpStartPod(t, extra, uri, "pod-b", tbLive)
		mpWaitStandby(t, extraB, 30*time.Second)
		// Past the 5 s after which the plain pod's live lane reads the subject the other build added
		// to the stream.
		time.Sleep(8 * time.Second)

		signalled := plainA.Terminate(t)
		// A build before INFRA-557 keeps the durable through its shutdown, and the standby binds
		// only after it.
		mpWaitActive(t, extraB, 60*time.Second)
		t.Logf("%s (envoy_twobuild) took over %s after %s's SIGTERM", extraB.instance, time.Since(signalled).Round(time.Millisecond), plainA.instance)
		plainA.WaitExit(t, "SIGTERM")
		time.Sleep(3 * time.Second)

		plainC := mpStartPod(t, plain, uri, "pod-c", tbLive)
		mpWaitStandby(t, plainC, 30*time.Second)
		time.Sleep(3 * time.Second)

		killed := tbKill(t, publisher, serving, extraB)
		mpWaitActive(t, plainC, 30*time.Second)
		t.Logf("%s (plain) took over %s after %s's SIGKILL", plainC.instance, time.Since(killed).Round(time.Millisecond), extraB.instance)
		time.Sleep(3 * time.Second)
		stop()

		tbRequireEachOnce(t, publisher, serving)
	})

	t.Run("a build from before the lease, then this build", func(t *testing.T) {
		old := os.Getenv("ENVOY_LISTENER_OLD_BINARY")
		if old == "" {
			t.Skip("ENVOY_LISTENER_OLD_BINARY names no listener built from before INFRA-557")
		}
		_, uri := mpStartNATS(t, testnats.HostGrant)
		serving := mpStartServingPort(t)
		targets := tbTargets(false)
		oldA := mpStartPod(t, old, uri, "pod-old", tbLive)
		mpWaitActive(t, oldA, 30*time.Second)
		tbRegister(t, oldA, serving, targets)
		publisher := mpConnectPublisher(t, uri)
		stop := publisher.everyOn(tbEvery, targets...)
		time.Sleep(3 * time.Second)

		newB := mpStartPod(t, plain, uri, "pod-new", tbLive)
		mpEventually(t, 30*time.Second, newB.instance+" to wait on "+oldA.instance+"'s binding of the durable", func() bool {
			status, body := newB.health()
			return status == http.StatusOK && body["role"] == "standby" && body["receive"] == "waiting for another subscriber's push binding to end"
		})
		time.Sleep(5 * time.Second)

		signalled := oldA.Terminate(t)
		// The old build keeps its binding through its HTTP shutdown and its NATS drain.
		mpWaitActive(t, newB, 60*time.Second)
		t.Logf("%s took over %s after %s's SIGTERM", newB.instance, time.Since(signalled).Round(time.Millisecond), oldA.instance)
		oldA.WaitExit(t, "SIGTERM")
		time.Sleep(3 * time.Second)
		stop()

		tbRequireEachOnce(t, publisher, serving)
	})
}
