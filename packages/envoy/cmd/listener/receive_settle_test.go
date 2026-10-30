package main

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// bindAsADeadHolder creates machineID's durable and subscribes to its deliver subject on a
// connection of its own that never acks, as a holder whose node was lost without a FIN: the server
// keeps its interest, so the durable reads push-bound, and goes on pushing messages to it. It
// returns a client to publish and read the durable with.
func bindAsADeadHolder(t *testing.T, uri, machineID string) *bus.Client {
	t.Helper()
	client, _ := bindAsAHolder(t, uri, machineID, func(*natsgo.Msg) {})
	return client
}

// bindAsAHolder is bindAsADeadHolder with handle taking each message the holder receives, and the
// holder's subscription returned beside the client.
func bindAsAHolder(t *testing.T, uri, machineID string, handle natsgo.MsgHandler) (*bus.Client, *natsgo.Subscription) {
	t.Helper()
	setup := testnats.Connect(t, uri)
	t.Cleanup(setup.Close)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	consumer := bus.ListenerDurable(machineID)
	info, err := ensureListenerDurable(context.Background(), client, consumer)
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	sub, err := setup.Subscribe(info.Config.DeliverSubject, handle)
	if err != nil {
		t.Fatalf("subscribe as the holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the durable to read push-bound", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.PushBound
	})
	return client, sub
}

// leaveARetryOutstanding publishes data to machineID's durable and has a holder on a connection
// of its own NAK it for a retry a minute later, as the delivery handler NAKs a prompt that failed,
// then go: the message stays outstanding, counted in the durable's NumAckPending, for that minute.
// It returns a client to publish and read the durable with.
func leaveARetryOutstanding(t *testing.T, uri, machineID, data string) *bus.Client {
	t.Helper()
	naked := make(chan struct{}, 1)
	client, holder := bindAsAHolder(t, uri, machineID, func(msg *natsgo.Msg) {
		if msg.NakWithDelay(time.Minute) == nil {
			select {
			case naked <- struct{}{}:
			default:
			}
		}
	})
	publishDurable(t, client, data)
	select {
	case <-naked:
	case <-time.After(5 * time.Second):
		t.Fatalf("the holder never NAKed %q", data)
	}
	if err := holder.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the retry to stay outstanding with no subscriber", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && !info.PushBound && info.NumAckPending == 1 && info.NumPending == 0
	})
	return client
}

// publishToTheDeadHolder publishes data and waits until the durable has pushed it to the dead
// holder alone, and returns when it had.
func publishToTheDeadHolder(t *testing.T, client *bus.Client, machineID, data string) time.Time {
	t.Helper()
	publishDurable(t, client, data)
	waitFor(t, 5*time.Second, "the dead holder to have "+data, func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && info.NumAckPending == 1 && info.NumPending == 0
	})
	return time.Now()
}

// settleHoldEnd waits for the rig's log record of its settle hold's end and returns it.
func (rig *receiverRig) settleHoldEnd(t *testing.T, within time.Duration) map[string]any {
	t.Helper()
	return rig.logRecord(t, "settle hold ended; delivering the held messages in stream order", within)
}

// awaitRecord waits until the rig's lease record satisfies cond, as its renewals write it.
func (rig *receiverRig) awaitRecord(t *testing.T, what string, cond func(leaseRecord) bool) {
	t.Helper()
	var last leaseRecord
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, err := rig.lease.kv.Get(rig.lease.key); err == nil {
			if record, err := decodeLeaseRecord(entry.Value()); err == nil {
				last = record
				if cond(record) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lease record is not %s within 5s: %+v", what, last.leaseFields)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A pod that takes the lease over an expired record nobody released subscribes at once, beside the
// dead holder's binding the server still counts, and holds what it receives while a message the
// dead holder had is outstanding: that message comes back once its ack wait runs out, after a newer
// one reached the new holder, and is still delivered first. The hold ends as soon as it has every
// outstanding message, well before its cap, and the record then says the new holder subscribed
// beside a stale binding and is no longer unsettled.
func TestASettleHoldWaitsForAMessageOutstandingWithADeadHolderAndDeliversInStreamOrder(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-expired"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	pushed := publishToTheDeadHolder(t, client, machineID, "older")

	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 10*time.Second, "pod-b to take the expired lease", rig.lease.Holding)
	if status := rig.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	subscribed := rig.awaitState(t, receiveActive, 5*time.Second)
	if after := subscribed.Sub(pushed); after >= consumerAckWait {
		t.Fatalf("pod-b subscribed %s after the older message went to the dead holder, want at once, before its redelivery", after)
	}
	if !strings.Contains(rig.logs.String(), "receiving beside it") {
		t.Fatalf("pod-b did not log receiving beside the dead holder's binding:\n%s", rig.logs.String())
	}
	// The newer message reaches pod-b at once; the older one only when its ack wait runs out.
	publishDurable(t, client, "newer")

	first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-b's first message")
	second := awaitReceived(t, received, 5*time.Second, "pod-b's second message")
	if first.data != "older" || first.delivered != 2 || second.data != "newer" || second.delivered != 1 {
		t.Fatalf("pod-b delivered %q (delivery %d) then %q (delivery %d), want the older message redelivered, then the newer one",
			first.data, first.delivered, second.data, second.delivered)
	}
	if after := first.at.Sub(pushed); after < consumerAckWait-time.Second || after > consumerAckWait+3*time.Second {
		t.Fatalf("pod-b delivered %s after the older message went to the dead holder, want once its %s ack wait brought it back", after, consumerAckWait)
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "settled" || end["messages"] != float64(2) {
		t.Fatalf("the settle hold ended %v, want settled holding both messages", end)
	}
	rig.awaitRecord(t, "marked stale_binding and settled", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.StaleBinding && !record.Unsettled
	})
}

// A term that must settle with nothing outstanding ends its hold at its first read of the durable,
// and delivers what is published then at once. The expired record it took carried stale_binding;
// finding the durable not push-bound, the new holder clears it.
func TestASettleHoldWithNothingOutstandingEndsAtOnce(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-expired-idle"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef","stale_binding":true}`)
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
	if status := rig.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	end := rig.settleHoldEnd(t, time.Second)
	if held, _ := end["held_ms"].(float64); end["outcome"] != "settled" || end["messages"] != float64(0) || held > 500 {
		t.Fatalf("the settle hold ended %v, want settled at once with nothing held", end)
	}
	publishDurable(t, rig.client, "fresh")
	published := time.Now()
	if message := awaitReceived(t, received, 5*time.Second, "pod-b"); message.data != "fresh" || message.at.Sub(published) > time.Second {
		t.Fatalf("pod-b delivered %q %s after its publish (%s after it subscribed), want it at once",
			message.data, message.at.Sub(published), message.at.Sub(subscribed))
	}
	rig.awaitRecord(t, "free of both flags", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && !record.StaleBinding && !record.Unsettled
	})
}

// The settle hold ends at its cap even while a message is still outstanding, then delivers what it
// holds; the outstanding message follows when it comes back. In production the cap is one of the
// durable's ack waits and a second, 7 s, after a hard take-over, and the hand-back delay and a
// second, 3 s, after a graceful one.
func TestASettleHoldEndsAtItsCap(t *testing.T) {
	if got := productionReceiveConfig().settleCap; got != 7*time.Second {
		t.Fatalf("the production settle cap is %s, want 7s: a %s ack wait and a second", got, consumerAckWait)
	}
	if got := productionReceiveConfig().handBackSettleCap; got != 3*time.Second {
		t.Fatalf("the production settle cap after a hand-back is %s, want 3s: the %s hand-back delay and a second", got, handBackDelay)
	}
	uri := testnats.URL(t)
	const machineID = "box-expired-cap"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	publishToTheDeadHolder(t, client, machineID, "older")

	timing := testReceiveTiming()
	timing.settleCap = 2 * time.Second
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, timing, testLeaseTiming())
	subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
	publishDurable(t, client, "newer")

	first := awaitReceived(t, received, timing.settleCap+2*time.Second, "pod-b's first message")
	if after := first.at.Sub(subscribed); first.data != "newer" || after < timing.settleCap-100*time.Millisecond || after >= consumerAckWait {
		t.Fatalf("pod-b delivered %q %s after it subscribed, want the newer message once the %s cap ended the hold", first.data, after, timing.settleCap)
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "cap" || end["messages"] != float64(1) || end["ack_pending"] != float64(2) {
		t.Fatalf("the settle hold ended %v, want at the cap holding one message of two outstanding", end)
	}
	if second := awaitReceived(t, received, consumerAckWait+3*time.Second, "pod-b's second message"); second.data != "older" || second.delivered != 2 {
		t.Fatalf("pod-b then delivered %q (delivery %d), want the older message's redelivery", second.data, second.delivered)
	}
	rig.awaitRecord(t, "settled", func(record leaseRecord) bool { return record.Holder == "pod-b" && !record.Unsettled })
}

// A settle hold lasts at most as long as the previous holder can still have messages outstanding,
// counted from the subscribe: one ack wait and a second after a take-over over an expired record
// or from a released one that is no clean hand-back, the hand-back delay and a second after one
// that is. A term is a hand-back candidate by what the released record carries (unsettled, no
// ack_wait_owed, no stale_binding), and ends its hold early only once the stopping pod's mark for
// the revision it replaced is there. A message NAKed for a retry a minute later stays outstanding
// past either cap, so the hold runs to its end and then delivers what it holds. The rule: each
// cap is what the previous holder can still have outstanding and no more, so a hand-over that
// went through waits the hand-back delay out and every other take-over an ack wait, rather than
// both waiting the sum of the two.
func TestASettleHoldsCapIsWhatThePreviousHolderCanStillHaveOutstanding(t *testing.T) {
	for _, row := range []struct {
		name, machineID, record string
		how                     leaseAcquisition
		// marked writes the stopping pod's clean-hand-back mark for the record, as a graceful
		// stop that saw this pod receiving does.
		marked bool
		limit  time.Duration
	}{
		{"expired", "box-cap-expired", `{"holder":"pod-dead","boot":"0123456789abcdef"}`, leaseTakenExpired, false, consumerAckWait + settleMargin},
		{"released with a stale binding", "box-cap-stale", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"stale_binding":true}`, leaseTakenReleased, false, consumerAckWait + settleMargin},
		{"released owing an ack wait", "box-cap-owed", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true,"ack_wait_owed":true}`, leaseTakenReleased, false, consumerAckWait + settleMargin},
		{"handed back and marked", "box-cap-handed-back", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`, leaseTakenReleased, true, handBackDelay + settleMargin},
		{"handed back, unmarked", "box-cap-handed-back-unmarked", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`, leaseTakenReleased, false, consumerAckWait + settleMargin},
		{"marked beside a stale binding", "box-cap-marked-stale", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true,"stale_binding":true}`, leaseTakenReleased, true, consumerAckWait + settleMargin},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			client := leaveARetryOutstanding(t, uri, row.machineID, "retry")
			handle, received := recordingHandler()
			rig := newReceiverRig(t, uri, row.machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
			replaced := putLeaseRecord(t, uri, row.machineID, row.record)
			if row.marked {
				putHandBackMark(t, rig.lease.kv, row.machineID, replaced)
			}
			rig.lease.Start(context.Background())
			rig.receiver.Start()
			subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
			if status := rig.lease.Status(); status.How != row.how || !status.Settle {
				t.Fatalf("pod-b's lease = %+v, want taken from a record %s, settling", status, row.how)
			}
			publishDurable(t, client, "fresh")

			first := awaitReceived(t, received, 15*time.Second, "pod-b's first message")
			if after := first.at.Sub(subscribed); first.data != "fresh" || after < row.limit-200*time.Millisecond || after > row.limit+700*time.Millisecond {
				t.Fatalf("pod-b delivered %q %s after it subscribed, want the fresh message once the %s hold ended", first.data, after.Round(time.Millisecond), row.limit)
			}
			end := rig.settleHoldEnd(t, time.Second)
			outcome := "cap"
			if row.limit == handBackDelay+settleMargin {
				outcome = "handed back"
			}
			if end["outcome"] != outcome || end["messages"] != float64(1) || end["ack_pending"] != float64(2) {
				t.Fatalf("the settle hold ended %v, want %q holding one message of two outstanding", end, outcome)
			}
			noneReceived(t, received, time.Second, "while the retry's delay runs")
		})
	}
}

// A hand-back candidate's hold ends on the mark and no sooner than handBackSettleCap after the
// subscribe: the stopping pod writes the mark once it has seen this pod receiving, so even a mark
// that is already there when the hold starts still leaves the hand-back delay to run, which is
// how long what that pod NAKed takes to come back. Its end clears the term's debt and the stale
// binding alike: the mark says a binding this term subscribed beside was the stopping pod's own
// linger.
func TestAMarkedHandBacksHoldEndsAtTheHandBackDelayAndNotBefore(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-mark-floor"
	client := leaveARetryOutstanding(t, uri, machineID, "retry")
	handle, received := recordingHandler()
	rig := newReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	replaced := putLeaseRecord(t, uri, machineID, `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`)
	putHandBackMark(t, rig.lease.kv, machineID, replaced)
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
	if status := rig.lease.Status(); !status.HandBack || status.Replaced != replaced {
		t.Fatalf("pod-b's lease = %+v, want a hand-back candidate over revision %d", status, replaced)
	}
	publishDurable(t, client, "fresh")

	limit := handBackDelay + settleMargin
	first := awaitReceived(t, received, 15*time.Second, "pod-b's first message")
	if after := first.at.Sub(subscribed); after < limit-200*time.Millisecond || after > limit+700*time.Millisecond {
		t.Fatalf("pod-b delivered %q %s after it subscribed, want the %s hold: a mark ends it no sooner than the hand-back delay",
			first.data, after.Round(time.Millisecond), limit)
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "handed back" || end["hand_back_marked"] != true {
		t.Fatalf("the settle hold ended %v, want it ended by the hand-back's mark", end)
	}
	rig.awaitRecord(t, "settled, owing nothing, with no stale binding", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && !record.Unsettled && !record.AckWaitOwed && !record.StaleBinding
	})
}

// A graceful stop of a pod during a settle hold over an expired record, the dead holder's binding
// gone with its connection (a SIGKILL), marks the released record ack_wait_owed as well as
// unsettled, and not stale_binding, since it found no binding beside its own: the message the dead
// holder had comes back one ack wait after its delivery, so the next holder's hold must wait that
// long too, not the hand-back delay a graceful hand-back allows. The next holder then delivers it
// before the newer message the stopped pod handed back.
func TestAGracefulStopDuringAnAckWaitHoldOwesTheNextHolderAnAckWait(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-stop-in-ack-wait-hold"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client, dead := bindAsAHolder(t, uri, machineID, func(*natsgo.Msg) {})
	publishToTheDeadHolder(t, client, machineID, "older")
	if err := dead.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the dead holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the dead holder's binding to go", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && !info.PushBound
	})

	stopping := startReceiverRig(t, uri, machineID, "pod-b", func(_ context.Context, msg *natsgo.Msg) {
		t.Errorf("pod-b delivered %q during its hold", msg.Data)
	}, testReceiveTiming(), testLeaseTiming())
	stopping.awaitState(t, receiveActive, 10*time.Second)
	if status := stopping.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	publishDurable(t, client, "newer")
	waitFor(t, 5*time.Second, "pod-b to hold the newer message", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && info.NumAckPending == 2 && info.NumPending == 0
	})
	time.Sleep(200 * time.Millisecond) // past pod-b's intake of what the server sent it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopping.receiver.Stop(ctx)
	if end := handBackEnd(t, stopping); end["handed_back"] != float64(1) || end["linger_end"] != "not taken" {
		t.Fatalf("pod-b's hand-back ended %v, want the newer message handed back and the linger ended with no pod to take the lease", end)
	}
	stopping.awaitRecord(t, "released, unsettled, owing an ack wait, without a stale binding", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.Released && record.Unsettled && record.AckWaitOwed && !record.StaleBinding
	})

	handle, received := recordingHandler()
	next := startReceiverRig(t, uri, machineID, "pod-c", handle, testReceiveTiming(), testLeaseTiming())
	if must := next.logRecord(t, "listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand", 5*time.Second); must["cap"] != (consumerAckWait + settleMargin).String() {
		t.Fatalf("pod-c's settle hold = %v, want the %s cap of a hard take-over", must, consumerAckWait+settleMargin)
	}
	for _, want := range []string{"older", "newer"} {
		if message := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-c's "+want); message.data != want {
			t.Fatalf("pod-c delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
	}
	if end := next.settleHoldEnd(t, time.Second); end["outcome"] != "settled" {
		t.Fatalf("pod-c's settle hold ended %v, want settled", end)
	}
}

// A stopping pod that gets back, alone, a message it already handed back (the server redelivered it
// before any other pod subscribed) NAKs it with the hand-back delay again rather than dropping it,
// so the pod that takes the lease next delivers it after that delay instead of one ack wait later,
// which its hold, the hand-back delay and a second, does not wait for.
func TestAStoppingPodHandsARepeatBackAgain(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-repeat"
	inHand := make(chan struct{}, 1)
	active := startReceiverRig(t, uri, machineID, "pod-a", func(ctx context.Context, msg *natsgo.Msg) {
		inHand <- struct{}{}
		<-ctx.Done()
		_ = msg.Nak()
	}, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	// pod-b runs its lease alone at first: it takes the released record and subscribes only once
	// pod-a has let go, so every redelivery during pod-a's linger reaches pod-a alone.
	handle, received := recordingHandler()
	next := newReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	next.lease.Start(context.Background())
	waitFor(t, 5*time.Second, "pod-b to read pod-a's lease", func() bool {
		return next.lease.Status().ActiveInstance == "pod-a"
	})
	publishDurable(t, active.client, "first")
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("pod-a never took the message")
	}

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	end := handBackEnd(t, active)
	if end["handed_back"] != float64(1) || end["linger_end"] != "timeout" || end["dropped_repeats"] != float64(0) {
		t.Fatalf("pod-a's hand-back ended %v, want the message handed back, its repeat handed back again rather than dropped, and the linger run to its cap", end)
	}

	next.receiver.Start()
	message := awaitReceived(t, received, 15*time.Second, "pod-b's message")
	if after := message.at.Sub(stopped); message.data != "first" || after >= consumerAckWait {
		t.Fatalf("pod-b delivered %q %s after pod-a's stop, want the repeat handed back again, before an ack wait", message.data, after.Round(time.Millisecond))
	}
}

// A released record that is unsettled (its holder handed its messages back, and lingers on its
// subscription) or carries stale_binding (its holder subscribed beside an expired holder's
// lingering binding) tells the next holder that a binding it finds is a pod of this build's, not a
// pre-lease build's: it subscribes at once beside the binding and settles, delivering a message the
// binding had before a newer one. It marks the record stale_binding for a binding it did not wait
// out, a hand-back candidate's included: only the stopping pod's mark says that binding was its
// own linger, and no pod wrote one here. A released record with neither flag still counts a
// binding as a pre-lease build's, and waits it out.
func TestAReleasedRecordUnsettledOrWithAStaleBindingSettlesBesideTheBinding(t *testing.T) {
	for _, row := range []struct {
		name, machineID, record string
		settle                  bool
		// handBack has the binding NAK the first message it receives with the hand-back delay,
		// as a graceful stop does before it releases the record unsettled.
		handBack bool
	}{
		{"unsettled", "box-released-unsettled", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`, true, true},
		{"stale binding", "box-released-stale", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"stale_binding":true}`, true, false},
		{"neither flag", "box-released-plain", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true}`, false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			var handedBack atomic.Bool
			client, _ := bindAsAHolder(t, uri, row.machineID, func(msg *natsgo.Msg) {
				if row.handBack && handedBack.CompareAndSwap(false, true) {
					_ = msg.NakWithDelay(handBackDelay)
				}
			})
			pushed := publishToTheDeadHolder(t, client, row.machineID, "older")
			handle, received := recordingHandler()
			// The record is written once the rig is ready, as a standby takes a hand-back.
			rig := newReceiverRig(t, uri, row.machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
			putLeaseRecord(t, uri, row.machineID, row.record)
			rig.lease.Start(context.Background())
			rig.receiver.Start()
			waitFor(t, 10*time.Second, "pod-b to take the released lease", rig.lease.Holding)
			if status := rig.lease.Status(); status.How != leaseTakenReleased || status.Settle != row.settle {
				t.Fatalf("pod-b's lease = %+v, want taken from a released record, settle %v", status, row.settle)
			}
			if !row.settle {
				rig.awaitState(t, receiveAwaitingBinding, 5*time.Second)
				noneReceived(t, received, time.Second, "pod-b while the binding stands")
				if state := rig.receiver.report().State; state != receiveAwaitingBinding {
					t.Fatalf("pod-b's receive loop is %s, want it waiting out the binding", state)
				}
				return
			}
			if after := rig.awaitState(t, receiveActive, 5*time.Second).Sub(pushed); after >= consumerAckWait {
				t.Fatalf("pod-b subscribed %s after the older message went to the binding, want at once", after)
			}
			if !strings.Contains(rig.logs.String(), "receiving beside it") {
				t.Fatalf("pod-b did not log receiving beside the binding:\n%s", rig.logs.String())
			}
			publishDurable(t, client, "newer")
			first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-b's first message")
			second := awaitReceived(t, received, 5*time.Second, "pod-b's second message")
			if first.data != "older" || second.data != "newer" {
				t.Fatalf("pod-b delivered %q then %q, want the older message first", first.data, second.data)
			}
			rig.awaitRecord(t, "stale_binding and settled", func(record leaseRecord) bool {
				return record.Holder == "pod-b" && record.StaleBinding && !record.Unsettled
			})
		})
	}
}

// A graceful stop during a settle hold hands back what it holds and releases the record still
// unsettled, owing an ack wait and carrying stale_binding, ending its linger once no other pod
// takes the lease, so the next holder settles in its turn: beside the binding it holds the
// handed-back messages until the message the binding had comes back, and delivers all three in the
// order they were published.
func TestAGracefulStopDuringASettleHoldHandsTheHoldOn(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-stop-in-hold"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	publishToTheDeadHolder(t, client, machineID, "older")

	var stoppedHandled atomic.Int32
	stopping := startReceiverRig(t, uri, machineID, "pod-b", func(_ context.Context, msg *natsgo.Msg) {
		stoppedHandled.Add(1)
		_ = msg.Ack()
	}, testReceiveTiming(), testLeaseTiming())
	stopping.awaitState(t, receiveActive, 10*time.Second)
	publishDurable(t, client, "newer one")
	publishDurable(t, client, "newer two")
	waitFor(t, 5*time.Second, "pod-b to hold both newer messages", func() bool {
		return strings.Contains(stopping.logs.String(), "receiving beside it") && func() bool {
			info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
			return err == nil && info.NumAckPending == 3 && info.NumPending == 0
		}()
	})
	time.Sleep(200 * time.Millisecond) // past pod-b's intake of what the server sent it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopping.receiver.Stop(ctx)
	if got := stoppedHandled.Load(); got != 0 {
		t.Fatalf("pod-b delivered %d messages during its hold, want none", got)
	}
	if end := handBackEnd(t, stopping); end["handed_back"] != float64(2) || end["linger_end"] != "not taken" {
		t.Fatalf("pod-b's hand-back ended %v, want the two messages it held handed back and the linger ended with no pod to take the lease", end)
	}
	stopping.awaitRecord(t, "released, unsettled, owing an ack wait, with a stale binding", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.Released && record.Unsettled && record.AckWaitOwed && record.StaleBinding
	})

	handle, received := recordingHandler()
	next := startReceiverRig(t, uri, machineID, "pod-c", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "pod-c to take the released lease", next.lease.Holding)
	if status := next.lease.Status(); status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("pod-c's lease = %+v, want taken from a released record, settling", status)
	}
	for _, want := range []string{"older", "newer one", "newer two"} {
		if message := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-c's "+want); message.data != want {
			t.Fatalf("pod-c delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
	}
	if end := next.settleHoldEnd(t, time.Second); end["outcome"] != "settled" {
		t.Fatalf("pod-c's settle hold ended %v, want settled", end)
	}
}

// During a settle hold, what the term holds is taken in stream order, a message the durable sent
// twice counted once; afterwards the held batch comes first and the queue behind it in arrival
// order.
func TestASettleHoldsMessagesAreTakenInStreamOrder(t *testing.T) {
	sub := &natsgo.Subscription{}
	message := func(stream uint64) *natsgo.Msg {
		return &natsgo.Msg{Sub: sub, Reply: "$JS.ACK.ENVOY_NOTIFICATIONS.listener-box.1." + strconv.FormatUint(stream, 10) + ".1.0.0"}
	}
	newTerm := func() *receiveTerm {
		return &receiveTerm{queue: make(chan *natsgo.Msg, 8), unsettled: map[*natsgo.Msg]struct{}{}, closing: make(chan struct{}), holding: true}
	}
	five, three, again, nine := message(5), message(3), message(3), message(9)
	term := newTerm()
	for _, m := range []*natsgo.Msg{five, three, again} {
		term.intake(m)
	}
	if held := term.gather(false); held != 2 {
		t.Fatalf("the hold counts %d messages, want 2: stream sequence 3 twice and 5", held)
	}
	term.intake(nine)
	term.intake(message(1))
	taken := term.takeQueued()
	var sequences []uint64
	for _, m := range taken {
		meta, _ := m.Metadata()
		sequences = append(sequences, meta.Sequence.Stream)
	}
	if !reflect.DeepEqual(sequences, []uint64{1, 3, 3, 5, 9}) || len(term.unsettled) != 0 {
		t.Fatalf("taken during the hold: stream sequences %v, %d left unsettled; want 1 3 3 5 9 and none", sequences, len(term.unsettled))
	}

	term = newTerm()
	term.intake(five)
	term.intake(three)
	term.gather(true)
	term.intake(nine)
	term.intake(message(1))
	sequences = nil
	for _, m := range term.takeQueued() {
		meta, _ := m.Metadata()
		sequences = append(sequences, meta.Sequence.Stream)
	}
	if !reflect.DeepEqual(sequences, []uint64{3, 5, 9, 1}) {
		t.Fatalf("taken after the hold: stream sequences %v, want the held 3 5, then the queue's 9 1", sequences)
	}
}

// A held batch is delivered in stream order; a message whose JetStream metadata cannot be read
// keeps the place it arrived in, and the others are ordered around it.
func TestSortByStreamSequenceKeepsAnUnreadableMessageInPlace(t *testing.T) {
	sub := &natsgo.Subscription{}
	message := func(stream uint64) *natsgo.Msg {
		return &natsgo.Msg{Sub: sub, Reply: "$JS.ACK.ENVOY_NOTIFICATIONS.listener-box.1." + strconv.FormatUint(stream, 10) + ".1.0.0"}
	}
	unreadable := &natsgo.Msg{Sub: sub, Reply: "_INBOX.not-jetstream"}
	five, three, nine, one := message(5), message(3), message(9), message(1)
	batch := []*natsgo.Msg{five, unreadable, three, nine, one}
	sortByStreamSequence(batch)
	want := []*natsgo.Msg{one, unreadable, three, five, nine}
	for index := range want {
		if batch[index] != want[index] {
			t.Fatalf("position %d holds %q, want %q", index, batch[index].Reply, want[index].Reply)
		}
	}
}
