//go:build multiprocess

// The take-over rows of the two-pod suite (multiprocess_harness_test.go): which of a machine's two
// processes delivers across a reconnect, a short drop, a start together, a SIGKILL, a node loss, a
// graceful stop and a NATS restart, and what each answers meanwhile.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A reconnect in place: the active pod's link to NATS is cut for 7 s, past the lease's 5 s, then
// it reconnects to the same server, with a message published every 100 ms throughout. The standby
// takes over while the active pod is away, and once it is back only one of them delivers: every
// message reaches its session exactly once. Before INFRA-557 the standby binds the durable once
// the server drops the cut pod's interest, and the cut pod's nats.go sends its subscription again
// on the reconnect, so both deliver and messages arrive twice.
func TestTwoPodsReconnectInPlaceDeliversEachMessageOnce(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_reconnect_one", "ses_mp_reconnect_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 7 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	actives := 0
	for _, pod := range []*mpPod{a, b} {
		if pod.active() {
			actives++
		}
	}
	if actives != 1 {
		t.Fatalf("%d pods report themselves active after the reconnect, want 1", actives)
	}
	watch.requireServed(t, cut)
	t.Logf("%d messages, each prompted once across a %s cut of %s", len(messages), outage, a.instance)
}

// A short drop: the active pod's link is cut for 2 s, under the lease's 5 s. It reconnects and
// renews in time, so it stays the machine's active pod and the standby never delivers: every
// message reaches its session exactly once and within 10 s of its publish (a ledger call that
// failed while the pod's state connection reconnected once cost a message 30 s), and the standby
// never reports itself anything but a standby. Before INFRA-557 the standby may bind in the gap,
// and the reconnected pod's nats.go then subscribes beside it.
func TestTwoPodsShortDropKeepsTheActivePod(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_drop_one", "ses_mp_drop_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var notStandby []string
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if !b.standby() {
				_, body := b.health()
				notStandby = append(notStandby, fmt.Sprint(body))
			}
			select {
			case <-done:
				return
			case <-time.After(mpPoll):
			}
		}
	}()
	halt := sync.OnceFunc(func() { close(done); <-watched })
	t.Cleanup(halt)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 2 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	reconnected := time.Since(cut)
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	stopped := time.Now()
	serving.awaitAll(t, messages, 30*time.Second)
	t.Logf("%s reconnected %s after the cut; the last of %d messages arrived %s after the last publish",
		a.instance, reconnected.Round(time.Millisecond), len(messages), time.Since(stopped).Round(time.Millisecond))
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	time.Sleep(mpQuiet)
	halt()
	serving.requireOnce(t, messages)
	if len(notStandby) > 0 {
		t.Fatalf("%s was not a standby in %d polls across %s's short drop: %v", b.instance, len(notStandby), a.instance, notStandby)
	}
	if strings.Contains(b.Output.String(), "durable receiving") {
		t.Fatalf("%s received the durable across %s's short drop", b.instance, a.instance)
	}
	if !a.active() {
		t.Fatalf("%s is not the active pod after its short drop", a.instance)
	}
}

// Two pods of the machine started at the same moment, 20 times, each time on a NATS account of its
// own: once both are ready exactly one reports itself active and the other a standby, and each
// message reaches its session exactly once. Before INFRA-557 nats.go's push-bound check is
// check-then-act, and two pods started together both bind the durable, so both report themselves
// healthy, the active pod's answer then (and both deliver). The row stops at the first start that
// fails.
func TestTwoPodsStartedTogetherElectOneActivePod(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for start := range 20 {
		passed := t.Run(fmt.Sprintf("start %d", start+1), func(t *testing.T) {
			uri := mpNATS(t)
			pods := []*mpPod{mpStartPod(t, binary, uri, "pod-a"), mpStartPod(t, binary, uri, "pod-b")}
			mpEventually(t, 30*time.Second, "both pods to be ready", func() bool {
				return (pods[0].active() || pods[0].standby()) && (pods[1].active() || pods[1].standby())
			})
			// Read again, a few times, so a second pod that took the lease a moment later shows.
			for range 4 {
				var actives []string
				for _, pod := range pods {
					if pod.active() {
						actives = append(actives, pod.instance)
					}
				}
				if len(actives) != 1 {
					t.Fatalf("%d pods report themselves active (%v), want exactly 1", len(actives), actives)
				}
				time.Sleep(mpPoll)
			}
			serving := mpStartNATSSessions(t, uri)
			const sessionID = "ses_mp_together"
			pods[1].registerNATS(t, serving, sessionID)
			publisher := mpConnectPublisher(t, uri)
			for range 5 {
				publisher.mustPublish(t, sessionID)
			}
			messages := publisher.all()
			serving.awaitAll(t, messages, 10*time.Second)
			time.Sleep(time.Second)
			serving.requireOnce(t, messages)
		})
		if !passed {
			break
		}
	}
}

// SIGKILL of the active pod while it has a message in hand, on a port session T whose prompt never
// answers, and a second message to T queued behind it on T's own consumer. The standby takes over:
// both of T's messages and every message published after the kill reach their sessions within
// 15 s, each session's in the order they were published, and the standby lists the registered
// sessions throughout. Before INFRA-557 the durable's 60 s ack wait holds the queued message with
// the killed pod (and any message the server pushed to it as it died), so those come after the
// newer ones, and late. What waits behind the prompt in hand is T's own consumer's since main's
// per-session consumers: another session's messages no longer queue behind it, and the standby's
// relay restarts that consumer at its ack floor, which is what brings both messages back at once
// rather than at the consumer's own 60 s ack wait.
func TestTwoPodsSIGKILLHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const blocked, queued, other = "ses_mp_kill_t", "ses_mp_kill_s", "ses_mp_kill_u"
	for _, sessionID := range []string{blocked, queued, other} {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, blocked, queued, other)
	publisher := mpConnectPublisher(t, uri)

	// The blocked session answers every frame `nak` until the kill, so the killed pod's relay
	// took nothing to it and the successor must deliver both, in stream order.
	serving.fail(blocked)
	first := publisher.mustPublish(t, blocked)
	mpEventually(t, 10*time.Second, a.instance+" to relay "+first.event+" to "+blocked, func() bool {
		return serving.failures(blocked) >= 1
	})
	behind := publisher.mustPublish(t, blocked)
	mpEventually(t, 10*time.Second, a.instance+" to hold the queued message too", func() bool {
		return publisher.sessionAckPending(blocked) >= 2
	})

	killed := a.kill(t)
	stop := publisher.every(100*time.Millisecond, queued, other)
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	serving.accept(blocked)     // the successor's frames are taken; the killed pod's never were
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{first, behind}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, killed)
	serving.requireOrder(t, messages)
	time.Sleep(time.Second)
	serving.requireOnce(t, messages)
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, killed)
}

// A serving port that refuses its session's prompts: the relay of that session's consumer keeps
// the message at the head of the session's queue and pushes it again (pushUntilAccepted), and the
// pod that takes delivery over pushes it again in its turn, because the message is its session's
// consumer's and no pod's memory. Across a graceful stop and then a SIGKILL of the pod holding it,
// each refused message reaches its session exactly once, within the take-over bound of the event
// that moved delivery: 10 s of the stop, 15 s of the kill. Before main's per-session consumers a
// refused push was the durable's to NAK and its message came back to the machine's durable, which
// is why the hand-back rows park their outstanding messages elsewhere (mpRetryOutstanding).
func TestTwoPodsAPortRetryReachesItsSessionAcrossAStopAndAKill(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_port_retry"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	serving.fail(sessionID)
	overTheStop := publisher.mustPublish(t, sessionID)
	mpEventually(t, 10*time.Second, a.instance+"'s push to be refused", func() bool { return serving.failures(sessionID) > 0 })
	signalled := a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpGracefulBound)
	serving.accept(sessionID)
	serving.awaitAll(t, []mpMessage{overTheStop}, mpGracefulBound+5*time.Second)
	serving.requireWithin(t, []mpMessage{overTheStop}, mpGracefulBound, signalled)

	// The next standby starts only now, so each take-over has one pod to go to.
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving.fail(sessionID)
	refused := serving.failures(sessionID)
	overTheKill := publisher.mustPublish(t, sessionID)
	mpEventually(t, 10*time.Second, b.instance+"'s push to be refused", func() bool { return serving.failures(sessionID) > refused })
	killed := b.kill(t)
	mpWaitActive(t, c, mpHardBound)
	serving.accept(sessionID)
	serving.awaitAll(t, []mpMessage{overTheKill}, mpHardBound+5*time.Second)
	serving.requireWithin(t, []mpMessage{overTheKill}, mpHardBound, killed)

	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{overTheStop, overTheKill})
}

// Node loss: the active pod's link freezes, its sockets left open, so the server keeps its
// subscription as it does for a node that vanished. The messages published during the freeze
// reach their sessions through the standby within 15 s, each session's in order; once the link
// thaws, the old pod delivers none of them again. Before INFRA-557 the server keeps the frozen
// pod's binding for minutes and the standby cannot bind.
func TestTwoPodsNodeLossHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_loss_one", "ses_mp_loss_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
	time.Sleep(2 * time.Second) // publish on past the take-over
	stop()

	messages := publisher.since(frozen)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, frozen)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)

	thaw()
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	watch.requireServed(t, frozen)
}

// The active pod's receive connection alone is lost: the relay freezes the connection its main
// client holds, the one the machine's durable is received on, in both directions and closes every
// reconnect it makes under that name, while the delivery lease's own connection - a second socket
// of the same pod, `envoy-listener-lease pod-a` - goes on renewing the lease record. A message is
// published every 100 ms from before the cut. The standby must take the lease over and deliver:
// every message reaches its session exactly once, in publish order, the first published after the
// cut within 15 s of it; the cut pod must hold the lease again at no poll of the 20 s that
// follow, while the standby keeps delivering; and once the cut heals the old pod must answer
// /healthz 200 role standby. Before this round the cut pod renews the record on its own
// connection for as long as that one is up - answering /healthz 503 `subscription inactive` and
// delivering nothing meanwhile - so no standby takes over until the pod's self-health monitor
// kills it, ten minutes on.
func TestTwoPodsReceiveConnectionLostHandsOverWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvlost_one", "ses_mp_recvlost_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	var heal func()
	cut := publisher.quiesce(t, serving, func() { heal = a.lanes.cut(mpReceiveConnection(a.instance)) })
	t.Cleanup(func() { heal() })

	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	taken := time.Since(cut)
	released := mpLineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`)
	if released == "" {
		t.Fatalf("%s released the lease without logging why:\n%s", a.instance, mpTail(mpLinesWithout(a.Output.String()), 40))
	}
	if lines := strings.Count(a.Output.String(), "listener lease released: the receive connection is down"); lines != 1 {
		t.Fatalf("%s logged the release of its lost receive connection %d times, want one line: %s", a.instance, lines, released)
	}
	t.Logf("%s logged %s", a.instance, released)
	// The cut pod takes the lease back at no poll while its receive connection is down, and the
	// standby holds the record and goes on delivering throughout.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(mpPoll) {
		record := publisher.stored(t, bus.ListenerStateBucket, bus.ListenerLeaseKey(mpMachine))
		if record["holder"] != b.instance || record["released"] == true {
			t.Fatalf("%s into the cut the lease record names %v (released %v), want %s holding it",
				time.Since(cut).Round(time.Millisecond), record["holder"], record["released"], b.instance)
		}
		if a.active() {
			_, body := a.health()
			t.Fatalf("%s into the cut %s reports itself active again: %v", time.Since(cut).Round(time.Millisecond), a.instance, body)
		}
	}
	stop()

	messages := publisher.since(cut)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)
	first, _ := serving.first(sessions[0], messages[0].event)
	t.Logf("%s took the lease %s after the cut of %s's receive connection; the first message published after it reached its session %s after the cut, %d messages in all",
		b.instance, taken.Round(time.Millisecond), a.instance, first.Sub(cut).Round(time.Millisecond), len(messages))

	heal()
	mpWaitStandby(t, a, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	watch.requireServed(t, cut)
}

// The active pod's receive connection comes back inside the bound: the relay freezes it only
// until the pod's client has noticed the cut, then heals it, so nats.go reconnects on the same
// relay about a second later, inside leaseReceiveLost. The pod keeps the lease in the term it
// held across the cut, the standby never receives, and every message reaches its session once and
// within 10 s of its publish.
func TestTwoPodsReceiveConnectionBackInsideTheBoundKeepsTheLease(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvback_one", "ses_mp_recvback_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)
	acquisitions := strings.Count(a.Output.String(), `"msg":"listener lease acquired"`)
	reconnects := mainClientReconnects(a.Output.String())

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	heal := a.lanes.cut(mpReceiveConnection(a.instance))
	t.Cleanup(heal)
	cut := time.Now()
	mpEventually(t, 15*time.Second, a.instance+"'s client to notice the cut", func() bool {
		return strings.Contains(a.Output.String(), `envoy nats disconnected name="`+mpReceiveConnection(a.instance)+`"`)
	})
	noticed := time.Now()
	heal()
	mpEventually(t, 15*time.Second, a.instance+" to reconnect", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	back := time.Since(noticed)
	time.Sleep(5 * time.Second) // publish on past the bound the holder would have released at
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOnce(t, messages)
	t.Logf("%s noticed the cut %s after it and was back %s later, inside the %s bound",
		a.instance, noticed.Sub(cut).Round(time.Millisecond), back.Round(time.Millisecond), leaseReceiveLost)
	if back >= leaseReceiveLost {
		t.Fatalf("%s was back %s after it noticed the cut, past the bound %s: the row tested nothing", a.instance, back, leaseReceiveLost)
	}
	if !a.active() {
		_, body := a.health()
		t.Fatalf("%s is not the active pod after a cut it reconnected inside the bound from: %v", a.instance, body)
	}
	if took := strings.Count(a.Output.String(), `"msg":"listener lease acquired"`); took != acquisitions {
		t.Fatalf("%s acquired the lease %d times across the cut, want the term it held throughout", a.instance, took-acquisitions)
	}
	if strings.Contains(a.Output.String(), "listener lease released: the receive connection is down") {
		t.Fatalf("%s released the lease for a receive connection that was back inside the bound", a.instance)
	}
	if strings.Contains(b.Output.String(), "durable receiving") {
		t.Fatalf("%s received the durable across %s's short receive cut", b.instance, a.instance)
	}
}

// A graceful stop: the active pod is in hand on a prompt that never answers when it is sent
// SIGTERM, with a message published every 100 ms before and after. Every message reaches its
// session within 10 s of its publish; a message prompted twice is the same message, with its own
// event id. Before INFRA-557 the stopping pod keeps the durable bound for ~23 s, through its HTTP
// shutdown and its NATS drain, so the messages wait.
func TestTwoPodsGracefulStopDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const slow = "ses_mp_graceful_slow"
	sessions := []string{"ses_mp_graceful_one", "ses_mp_graceful_two"}
	for _, sessionID := range append([]string{slow}, sessions...) {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, append([]string{slow}, sessions...)...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 10 })
	inHand := serving.hold(slow)
	publisher.mustPublish(t, slow)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never prompted %s", a.instance, slow)
	}
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireKnown(t, messages)
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, signalled)
}

// A standby waits as long as the active pod runs: 140 s on, past the 135 s after which a standby
// once gave up and exited, it is still up, /healthz 200 role standby, and it lists the registered
// sessions in every poll; then a SIGTERM of the active pod hands delivery to it. Before INFRA-557
// the standby exits at 135 s.
func TestTwoPodsStandbyWaitsPast140s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_standby_waits"
	a.registerNATS(t, serving, sessionID)
	watch := mpWatchSessions(t, b, sessionID)
	started := time.Now()

	// The standby is checked by mpPod.standby here, which a build before the lease answers too, so
	// that build fails on its exit rather than on its /healthz; the role is checked after the wait.
	for time.Since(started) < 140*time.Second {
		select {
		case <-b.Exited:
			t.Fatalf("%s exited %s into its standby", b.instance, time.Since(started).Round(time.Second))
		case <-time.After(mpPoll):
		}
		if !b.standby() {
			status, body := b.health()
			t.Fatalf("%s %s into its standby is not a standby: /healthz %d %v", b.instance, time.Since(started).Round(time.Second), status, body)
		}
	}
	if status, body := b.health(); status != http.StatusOK || body["role"] != "standby" {
		t.Fatalf("%s's /healthz after %s as a standby: %d %v, want 200 role standby", b.instance, time.Since(started).Round(time.Second), status, body)
	}

	signalled := a.Terminate(t)
	mpWaitActive(t, b, mpGracefulBound)
	publisher := mpConnectPublisher(t, uri)
	m := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	watch.requireServed(t, started)
	t.Logf("%s took over %s after %s's SIGTERM", b.instance, time.Since(signalled).Round(time.Millisecond), a.instance)
}

// Readiness: a listener started while its NATS is down answers /healthz 503 "starting" and stays
// up, past the ten one-second connect tries after which it once exited; once NATS is up it is
// 200 active within 10 s, and a second pod started then is 200 standby. Before INFRA-557 the
// listener answers 200 "starting" with nothing ready, then exits.
func TestTwoPodsReadinessWaitsForNATS(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	server.Stop(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPod(t, binary, uri, "pod-a")
	mpEventually(t, 10*time.Second, a.instance+" to answer /healthz", func() bool { status, _ := a.health(); return status != 0 })
	for started := time.Now(); time.Since(started) < 15*time.Second; time.Sleep(mpPoll) {
		select {
		case <-a.Exited:
			t.Fatalf("%s exited while NATS was down", a.instance)
		default:
		}
		if status, body := a.health(); status != http.StatusServiceUnavailable || body["status"] != "starting" {
			t.Fatalf("%s's /healthz while NATS is down: %d %v, want 503 starting", a.instance, status, body)
		}
	}

	server.Start(t)
	upAt := time.Now()
	mpEventually(t, 10*time.Second, a.instance+" to be 200 active once NATS is up", func() bool {
		status, body := a.health()
		return status == http.StatusOK && body["role"] == "active"
	})
	t.Logf("%s was active %s after NATS came up", a.instance, time.Since(upAt).Round(time.Millisecond))
	b := mpStartPod(t, binary, uri, "pod-b")
	mpEventually(t, 30*time.Second, b.instance+" to be 200 standby", func() bool {
		status, body := b.health()
		return status == http.StatusOK && body["role"] == "standby"
	})
}

// The architect's contract for a message sent while the active pod is deleted, at the listener's
// API: the standby sits behind a 500 ms slow link, so its caches trail the bucket. While the active
// pod is stopped, by SIGTERM and by SIGKILL, sessions keep registering through it every 250 ms for
// as long as it answers, and each registered session is sent to through the standby every 250 ms,
// the first send at once, until the standby has taken over. Every send answers within 10 s, 2xx
// or a retryable 503 with Retry-After, and never 404 "no live session". The end-to-end resend is
// INFRA-562's. Before INFRA-557 the standby's cache has not seen the registration yet and answers
// 404 "no live session".
func TestTwoPodsSendThroughTheStandbyDuringATakeover(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, stopping := range []struct {
		name string
		stop func(t *testing.T, p *mpPod) time.Time
	}{
		{"SIGTERM", func(t *testing.T, p *mpPod) time.Time { return p.Terminate(t) }},
		{"SIGKILL", func(t *testing.T, p *mpPod) time.Time { return p.kill(t) }},
	} {
		t.Run(stopping.name, func(t *testing.T) {
			uri := mpNATS(t)
			a, b := mpStartPair(t, binary, uri)
			b.relay.delay(500 * time.Millisecond)

			type send struct {
				session string
				answer  mpAnswer
			}
			var (
				mu         sync.Mutex
				sends      []send
				registered []string
				senders    sync.WaitGroup
			)
			stopSending := make(chan struct{})
			sendTo := func(sessionID string) {
				defer senders.Done()
				body := fmt.Sprintf(`{"target_session":%q,"message":"send during the take-over","source":"agent","source_session":"ses_mp_sender"}`, sessionID)
				for {
					answer := b.call(http.MethodPost, "/v1/messages/send", body)
					mu.Lock()
					sends = append(sends, send{session: sessionID, answer: answer})
					mu.Unlock()
					select {
					case <-stopSending:
						return
					case <-time.After(mpPoll):
					}
				}
			}
			var refusal string
			register := func(index int) bool {
				sessionID := fmt.Sprintf("ses_mp_takeover_%03d", index)
				answer := a.call(http.MethodPost, "/v1/interests/subscribe", fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, sessionID))
				if answer.err != nil || answer.status != http.StatusOK {
					refusal = fmt.Sprintf("%d %s %v", answer.status, strings.TrimSpace(answer.body), answer.err)
					return false
				}
				mu.Lock()
				registered = append(registered, sessionID)
				mu.Unlock()
				senders.Add(1)
				go sendTo(sessionID)
				return true
			}

			index := 0
			for ; index < 6; index++ {
				if !register(index) {
					t.Fatalf("%s refused a registration before it was stopped: %s", a.instance, refusal)
				}
				time.Sleep(mpPoll)
			}
			mu.Lock()
			watch := mpWatchSessions(t, b, slices.Clone(registered)...)
			mu.Unlock()
			stoppedAt := stopping.stop(t, a)
			after := 0
			for ; register(index); index++ {
				after++
				time.Sleep(mpPoll)
			}
			mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
			time.Sleep(time.Second) // send on past the take-over
			close(stopSending)
			senders.Wait()
			watch.requireServed(t, stoppedAt)

			mu.Lock()
			defer mu.Unlock()
			retryable := 0
			for _, s := range sends {
				answer := s.answer
				switch {
				case answer.err != nil:
					t.Fatalf("send to %s through %s: %v after %s, want an answer within 10 s", s.session, b.instance, answer.err, answer.took)
				case answer.status == http.StatusNotFound:
					t.Fatalf("send to %s through %s: 404 %s", s.session, b.instance, answer.body)
				case answer.status == http.StatusServiceUnavailable && answer.header.Get("Retry-After") != "":
					retryable++
				case answer.status < 200 || answer.status > 299:
					t.Fatalf("send to %s through %s: %d %s, want 2xx or a 503 with Retry-After", s.session, b.instance, answer.status, answer.body)
				}
			}
			t.Logf("%d sessions registered through %s, %d of them after its %s, until it answered %s; %d sends through %s, %d answered a retryable 503, %s after the stop",
				len(registered), a.instance, after, stopping.name, refusal, len(sends), b.instance, retryable, time.Since(stoppedAt).Round(time.Millisecond))
		})
	}
}

// A standby across a NATS restart registers no JetStream subscription of the durable, so its
// client's recovery has nothing to resubscribe and logs no "envoy nats recovery resubscribe
// failed"; both pods come back, one active and one a standby. Before INFRA-557 the standby's
// pending bind is a subscription the recovery retries and logs that failure for.
func TestTwoPodsStandbyAcrossANATSRestartLogsNoResubscribeFailure(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)

	disconnects := strings.Count(b.Output.String(), "envoy nats disconnected")
	server.Stop(t)
	mpEventually(t, 10*time.Second, b.instance+" to see NATS go", func() bool {
		return strings.Count(b.Output.String(), "envoy nats disconnected") > disconnects
	})
	server.Start(t)
	mpEventually(t, 30*time.Second, "one pod active and the other a standby after the restart", func() bool {
		return (a.active() && b.standby()) || (b.active() && a.standby())
	})
	// A recovery's first attempt runs at the reconnect and the next a second later: watch past both.
	const failure = "envoy nats recovery resubscribe failed"
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(mpPoll) {
		if line := mpLineWith(b.Output.String(), failure); line != "" {
			t.Fatalf("%s, the standby, logged across the NATS restart: %s", b.instance, line)
		}
	}
}

// Two pods of one machine can be told apart in their logs: every JSON record of each carries its
// own instance, and internal/bus's text lines, which keep Go's positional format, name each pod's
// NATS client. Before INFRA-557 the records carry only machine_id.
func TestTwoPodsLogTheirOwnInstance(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	// A dropped link has each pod's client log its disconnect and reconnect in text.
	a.relay.drop()
	b.relay.drop()
	for _, pod := range []*mpPod{a, b} {
		mpEventually(t, 10*time.Second, pod.instance+" to log its reconnect", func() bool {
			return mainClientReconnects(pod.Output.String()) > 0
		})
	}
	for _, pod := range []*mpPod{a, b} {
		records := 0
		for line := range strings.SplitSeq(pod.Output.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) != nil || record["msg"] == nil {
				continue
			}
			records++
			if record["instance"] != pod.instance {
				t.Fatalf("%s's record has instance %v, want %q: %s", pod.instance, record["instance"], pod.instance, line)
			}
		}
		if records == 0 {
			t.Fatalf("%s logged no JSON record", pod.instance)
		}
		textLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO envoy nats disconnected name="envoy-listener ` + regexp.QuoteMeta(pod.instance) + `"`)
		if !textLine.MatchString(pod.Output.String()) {
			t.Fatalf("%s's bus disconnect line is not date, time, message naming the client envoy-listener %s", pod.instance, pod.instance)
		}
	}
}

// The active pod's receive connection alone is lost to a route that drops its packets, so its
// reconnects are dials that hang as well: the relay freezes the main client's connection and holds
// every connection the pod opens from then on before its first byte, while the lease connection
// goes on renewing. nats.go holds the connection's lock across each reconnect dial and its connect
// init, up to its connect timeout every time, so a receive signal read through that connection's
// state waits for it. The holder must still release the lease on its own bound
// (leaseReceiveLost), logging why, and the standby must take it over and deliver every message in
// publish order, the first published after the cut within 15 s of it, as when the reconnects are
// refused (TestTwoPodsReceiveConnectionLostHandsOverWithin15s). At 580089b5 (the INFRA-557
// tester's round-18 finding) the lease loop blocks reading that state, its renewals lapse instead
// of its release landing, the standby takes the expired record about 8 s after the cut, and the
// first message after the cut reaches its session 13.9-16.0 s after it (2 of 7 runs past 15 s).
func TestTwoPodsReceiveConnectionLostToADroppingRouteReleasesTheLeaseWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvhole_one", "ses_mp_recvhole_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	var heal, unhole func()
	cut := publisher.quiesce(t, serving, func() {
		unhole = a.lanes.blackholeNew()
		heal = a.lanes.cut(mpReceiveConnection(a.instance))
	})
	t.Cleanup(func() { unhole(); heal() })

	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	time.Sleep(5 * time.Second) // publish on past the take-over
	stop()
	if mpLineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`) == "" {
		t.Fatalf("%s never released the lease for its lost receive connection; its lease lines:\n%s", a.instance,
			mpLinesMatching(a.Output.String(), "lease"))
	}
	messages := publisher.since(cut)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)
}

// mpLinesMatching is the lines of output that contain needle.
func mpLinesMatching(output, needle string) string {
	var kept []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, needle) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// The active pod's receive connection is lost again while its reconnect hooks are still running
// from the reconnect before. The relay drops every JetStream stream-info request of the role claim
// bucket on that connection, which the listener's KV rewatch hook - the first of its reconnect
// hooks - makes first, so the hook waits out its 10 s request timeout and fails, and keeps failing
// on every retry. The connection is cut
// until the pod notices, then healed, so nats.go reconnects in place and the hooks start; a
// second later, with the rewatch still waiting, the connection is cut again on a route that drops
// its packets, its reconnect dials hanging, while the lease connection goes on renewing. The
// holder must release the lease on its own bound and the standby must take it over: every message
// reaches its session once, and every message published after the second cut within 15 s of it
// and in publish order. At 9c247139 the client ran its reconnect hooks on nats.go's callback
// goroutine, so the disconnect of the second cut waited behind the hanging rewatch, the receive
// signal read up for as long as the hooks took, and the take-over came long past 15 s.
func TestTwoPodsReceiveConnectionLostWhileItsReconnectHooksHangHandsOverWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_hookhang_one", "ses_mp_hookhang_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)
	receive := mpReceiveConnection(a.instance)
	named := `name="` + receive + `"`
	reconnects := func() int {
		return strings.Count(mpLinesMatching(a.Output.String(), named), "envoy nats reconnected")
	}
	hookFailures := func() int {
		return strings.Count(mpLinesMatching(a.Output.String(), named), "envoy nats reconnect hook failed")
	}

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	undrop := a.lanes.dropRequests(receive, "$JS.API.STREAM.INFO.KV_"+bus.RoleClaimBucket)
	t.Cleanup(undrop)
	before := reconnects()
	var healFirst func()
	publisher.quiesce(t, serving, func() { healFirst = a.lanes.cut(receive) })
	t.Cleanup(func() { healFirst() })
	mpEventually(t, 10*time.Second, a.instance+" to notice the first cut", func() bool {
		return strings.Contains(mpLinesMatching(a.Output.String(), named), "envoy nats disconnected")
	})
	healFirst()
	mpEventually(t, 10*time.Second, a.instance+" to reconnect in place", func() bool { return reconnects() > before })
	failedBefore := hookFailures()
	time.Sleep(time.Second)
	if hookFailures() != failedBefore {
		t.Fatalf("%s's reconnect hooks failed within a second of the reconnect, so the row does not cut while they run:\n%s",
			a.instance, mpLinesMatching(a.Output.String(), named))
	}
	if record := publisher.stored(t, bus.ListenerStateBucket, bus.ListenerLeaseKey(mpMachine)); record["holder"] != a.instance || record["released"] == true {
		t.Fatalf("after the reconnect in place the lease record names %v (released %v), want %s holding it",
			record["holder"], record["released"], a.instance)
	}

	var unhole, heal func()
	cut := time.Now()
	unhole = a.lanes.blackholeNew()
	heal = a.lanes.cut(receive)
	t.Cleanup(func() { unhole(); heal() })
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	taken := time.Since(cut)
	time.Sleep(5 * time.Second) // publish on past the take-over
	stop()
	if hookFailures() == failedBefore {
		t.Fatalf("%s's reconnect hooks had not failed by the end of the row, so the rewatch did not hang:\n%s",
			a.instance, mpLinesMatching(a.Output.String(), named))
	}

	all := publisher.since(time.Time{})
	serving.awaitAll(t, all, mpHardBound+10*time.Second)
	serving.requireOnce(t, all)
	messages := publisher.since(cut)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	t.Logf("%s took the lease %s after the second cut, with %s's rewatch hanging; %d messages after it", b.instance,
		taken.Round(time.Millisecond), a.instance, len(messages))
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
// for a push to a serving port. The guarantee is therefore at most one repeat under the same
// event id, the same shape as the settle bound's, and what this row holds is that it is at most
// one and that it comes back at once rather than after the pull's 30 s silence (INFRA-557
// tester's round 24).
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
		return mpLineWith(a.Output.String(), "envoy nats disconnected", `name=`+name) != "" ||
			mpLineWith(a.Output.String(), "envoy nats disconnected", `name="`+name+`"`) != ""
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
	took := serving.counts()[m.event]
	if took > 2 {
		t.Fatalf("%s to %s reached it %d times, want once, or twice as the frame the drop cut the reply of", m.event, sessionID, took)
	}
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

// A frame the active pod's relay has unanswered when its term ends is not sent again by the pod
// that takes over. The session's reply is held when the pod is stopped; the term's end cancels
// the relay, which goes on hearing the session for relaySettleGrace (settleStoppedFrames), the
// session answers inside it, and the stopping relay acks the message. The successor waits
// relayFormerSettle (lease.FormerSettleWait) before it restarts the session's consumer, so it
// restarts at an ack floor past that message and never sends it again. The session must take it
// exactly once. Before the settle the stopping relay forgot the frame unacked, the successor was
// given it again, and the session took it twice: CI met that as
// TestTwoPodsReceiveConnectionLostHandsOverWithin15s's "evt-mp-0079 to ses_mp_recvlost_one
// prompted 2 times" (run 41589787158, job 125769769345); this row forces that order (INFRA-557
// tester's round 27).
func TestTwoPodsARelayedFrameInFlightWhenTheTermEndsIsNotSentAgainByTheSuccessor(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_relay_term_end_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	arrived, release := serving.gate(sessionID)
	t.Cleanup(release)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}

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

// A frame the active pod's relay still has unanswered when the stopping relay's settle bound ends
// reaches the session at most twice, under its own event id, and no other message repeats. The
// session's reply is held when the pod is stopped; the stopping relay waits relaySettleGrace for
// it, the session answers only well past that, and the successor restarts the session's consumer
// at an ack floor below that message and publishes it again. Only the session's reply says
// whether it took that frame, and it came after the bound, so no pod can know: the stopping pod
// logs the frame in doubt (relayFrameInDoubtLine) and the successor sends it once more, which is
// the one repeat the root's at-least-once contract allows for a frame whose reply had not come
// back when its pod's term ended (cmd/listener/README.md, "State the pods share"). What the row
// refuses is a second repeat of it, a repeat of any other message, an event id no message
// carried, and a stopping pod that says nothing about the frame it cut (INFRA-557 tester's
// round 28, the architect's edge (2)).
func TestTwoPodsARelayedFrameHeldPastTheStoppingRelaysSettleBoundIsRepeatedAtMostOnceWithItsId(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_relay_past_settle_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	arrived, release := serving.gate(sessionID)
	t.Cleanup(release)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}

	a.Terminate(t)
	time.Sleep(relaySettleGrace + 2*time.Second) // past the stopping relay's settle bound
	release()                                    // the session answers the frame nobody waits for
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpHardBound)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, after})
	if took := serving.counts()[m.event]; took > 2 {
		t.Fatalf("%s to %s reached it %d times, want once, or twice as the frame whose reply the settle bound cut off", m.event, sessionID, took)
	}
	serving.requireKnown(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
	if mpLineWith(a.Output.String(), `"msg":"`+relayFrameInDoubtLine+`"`, m.event) == "" {
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
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_kill_frame_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	arrived := serving.hold(sessionID)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}

	killed := a.kill(t)
	select {
	case <-a.Exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was still running 10 s after SIGKILL", a.instance)
	}
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, mpHardBound+15*time.Second)
	serving.requireWithin(t, []mpMessage{m}, mpHardBound, killed)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, after})
	if took := serving.counts()[m.event]; took > 2 {
		t.Fatalf("%s to %s reached it %d times, want once, or twice as the frame in flight at the kill", m.event, sessionID, took)
	}
	serving.requireKnown(t, []mpMessage{warm, m, after})
	serving.requireOrder(t, []mpMessage{warm, m, after})
}

// A frame whose "received" reply reached the stopping relay is not sent again by the pod that
// takes over, however long the session then takes to write it. The session answers the frame
// "received" at once - it has the frame - and "done" only once it has written it, here 2 s past
// the stopping relay's settle grace. The pod is stopped in between. A received frame is not in
// doubt: its session said it has it, and the frame's reply subscription heard that before the
// term ended. The session must take the message exactly once. At fde2b129 the stopping relay
// counts a frame as settled only on "done" (settleStoppedFrames waits for outstanding, which
// "received" does not clear), logs this one in doubt when its grace ends, and the successor sends
// it again: twice. The same holds when the term ends for a lost receive connection rather than a
// stop (INFRA-557 tester's round 29, the architect's rule that a frame whose received reply
// arrived never repeats).
func TestTwoPodsARelayedFrameItsSessionReceivedIsNotSentAgainAfterAStop(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_relay_received_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	arrived, release := serving.receiveThenGate(sessionID)
	t.Cleanup(release)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}

	a.Terminate(t)
	time.Sleep(relaySettleGrace + 2*time.Second) // the session is still writing the frame it received
	release()
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpHardBound)

	after := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m, after}, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, m, after})
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
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_relay_recvlost_grace_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	arrived, release := serving.receiveThenGate(sessionID)
	t.Cleanup(release)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never relayed %s to the session", a.instance, m.event)
	}
	t.Cleanup(a.lanes.cut(mpReceiveConnection(a.instance)))
	mpEventually(t, mpHardBound, a.instance+" to release the lease for its lost receive connection", func() bool {
		return mpLineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`) != ""
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
