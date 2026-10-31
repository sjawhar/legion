package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// durableAlarms counts the ERROR records of the take-over alarm in a rig's log.
func durableAlarms(logs *cmdtest.LockedBuffer) int {
	return len(durableAlarmRecords(logs))
}

// durableAlarmRecords are the ERROR records of the take-over alarm in a rig's log.
func durableAlarmRecords(logs *cmdtest.LockedBuffer) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == durableTakeoverAlarmLine && record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

// During the first roll onto the lease, the durable is push-bound by a build from before it
// (nats.Bind). The pod that takes the lease from an absent record waits that binding out, renewing
// its lease, alarms at ERROR once it has waited past the alarm threshold and again at each repeat,
// neither deletes the durable to take it nor rewrites its policy under the old build (whose
// nats.Bind refuses a durable whose filter is not its own), and receives as soon as the old build
// lets go, correcting the policy then.
func TestAPreLeaseBindingIsWaitedOutAndAlarmedOn(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-pre-lease"
	consumer := bus.ListenerDurable(machineID)
	old, binding := durableHeldElsewhere(t, uri, consumer)
	before, err := old.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}

	timing := testReceiveTiming()
	timing.alarmAfter, timing.alarmEvery = 300*time.Millisecond, 200*time.Millisecond
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-new", handle, timing, testLeaseTiming())
	rig.awaitState(t, receiveAwaitingBinding, 10*time.Second)
	waitFor(t, 10*time.Second, "the alarm and its repeat", func() bool { return durableAlarms(rig.logs) >= 2 })
	if !rig.lease.Holding() {
		t.Fatal("the waiting pod let its lease lapse")
	}
	if state := rig.receiver.report().State; state != receiveAwaitingBinding {
		t.Fatalf("the receive loop is %s while the old build holds the durable, want it waiting", state)
	}
	if !binding.IsValid() {
		t.Fatal("the old build's binding is gone")
	}
	info, err := old.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !info.Created.Equal(before.Created) || !info.PushBound || !reflect.DeepEqual(info.Config, before.Config) {
		t.Fatalf("the durable while the new pod waits: %+v %v, want the old build's as it made it, still push-bound", info, err)
	}

	old.Close()
	rig.awaitState(t, receiveActive, 5*time.Second)
	publishDurable(t, rig.client, "after the old build")
	if message := awaitReceived(t, received, 5*time.Second, "the new pod"); message.data != "after the old build" {
		t.Fatalf("the new pod received %q", message.data)
	}
	info, err = rig.client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !info.Created.Equal(before.Created) || info.Config.FilterSubject != consumerFilterAll || info.Config.AckWait != consumerAckWait {
		t.Fatalf("the durable once the new pod receives: %+v %v, want the same durable under the consumer policy", info, err)
	}
}

// consumerInfoDeniedNATS runs a NATS server with JetStream and two users, and returns a URL for
// each: granted holds every permission, and denied every one but reading a consumer of the stream
// (CONSUMER.INFO), so a pod whose bus client connects as denied cannot read its durable while its
// lease renews on a granted connection. NATS answers a request it denies with nothing, so each
// read waits out the JetStream wait (10 s) before it fails.
func consumerInfoDeniedNATS(t *testing.T) (granted, denied string) {
	t.Helper()
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: granted, password: granted },
    { user: denied, password: denied, permissions: { publish: { allow: [">"], deny: [%q] } } }
  ]
}
`, "$JS.API.CONSUMER.INFO."+bus.Stream+".*")
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	return strings.Replace(uri, "nats://", "nats://granted:granted@", 1), strings.Replace(uri, "nats://", "nats://denied:denied@", 1)
}

// A lease holder that cannot prepare its durable (here it may not read it) says so in /healthz,
// 200 degraded with the reason, and once its failures have lasted the alarm bound, timed from the
// start of the first, logs the alarm line at ERROR with that reason and gives the lease up: the
// other pod takes it at its next poll and delivers what was published meanwhile, and the stuck pod,
// a standby again, does not take it back from a pod that receives. Before, the holder retried
// forever and renewed its lease, so no standby ever tried, /healthz answered 200 healthy and no
// alarm fired.
func TestAHolderThatCannotPrepareItsDurableAlarmsAndGivesTheLeaseUp(t *testing.T) {
	granted, denied := consumerInfoDeniedNATS(t)
	const machineID = "box-stuck"
	// The bound is past one failed read, so /healthz is read between the first failure and the
	// give-up.
	timing := testReceiveTiming()
	timing.alarmAfter = 15 * time.Second
	stuckHandle, stuckReceived := recordingHandler()
	stuck := newReceiverRigVia(t, denied, granted, machineID, "pod-stuck", stuckHandle, timing, testLeaseTiming())
	stuck.lease.Start(context.Background())
	stuck.receiver.Start()
	stuck.awaitState(t, receivePreparing, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, granted, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "pod-b to read pod-stuck's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-stuck"
	})
	publishDurable(t, standby.client, "while the holder is stuck")

	get := healthzFor(t, stuck.client, stuck.receiver)
	var code int
	var body map[string]any
	// A holder that does not receive yet is already degraded; this waits for the failed read it
	// reports with that.
	waitFor(t, 15*time.Second, "pod-stuck's /healthz to report its failing durable", func() bool {
		code, body = get()
		return body["receive_failing_ms"] != nil
	})
	if code != http.StatusOK || body["status"] != "degraded" || body["role"] != "standby" || body["receive"] != receivePreparing.String() ||
		!strings.Contains(fmt.Sprint(body["error"]), "read the durable consumer") {
		t.Fatalf("pod-stuck's /healthz while its durable read fails = %d %v, want 200 degraded, role standby, receive %q and an error naming the failed read",
			code, body, receivePreparing.String())
	}
	if durableAlarms(stuck.logs) != 0 {
		t.Fatal("pod-stuck alarmed before its failures lasted the alarm bound")
	}

	message := awaitReceived(t, received, 30*time.Second, "pod-b once pod-stuck gave the lease up")
	if message.data != "while the holder is stuck" {
		t.Fatalf("pod-b received %q", message.data)
	}
	alarms := durableAlarmRecords(stuck.logs)
	if len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
		t.Fatalf("pod-stuck's alarms: %v, want one at ERROR whose reason names the failed read", alarms)
	}
	if waited, _ := alarms[0]["waited_ms"].(float64); time.Duration(waited)*time.Millisecond < timing.alarmAfter {
		t.Fatalf("pod-stuck alarmed after %v ms of failures, want the %s bound", alarms[0]["waited_ms"], timing.alarmAfter)
	}
	// pod-stuck polls every 50 ms as a standby: a second is twenty chances to take the lease back.
	time.Sleep(time.Second)
	if stuck.lease.Holding() || !standby.lease.Holding() {
		t.Fatalf("after the give-up: pod-stuck holding %v, pod-b holding %v; want pod-b alone", stuck.lease.Holding(), standby.lease.Holding())
	}
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" || body["role"] != "standby" || body["active_instance"] != "pod-b" {
		t.Fatalf("pod-stuck's /healthz after the give-up = %d %v, want 200 healthy, a standby naming pod-b", code, body)
	}
	noneReceived(t, stuckReceived, time.Millisecond, "pod-stuck")
}

// Two pods of one machine, each connected as the machine's nkey user holding exactly the grant
// envoy-infra renders for a listener (testnats.ListenerGrant), bus and lease connection alike.
// pod-x takes the lease while a pre-lease build binds the durable, waits that binding out, and
// then cannot read its durable for the alarm bound, so it gives the lease up. pod-y takes it,
// waits the binding out in its turn, receives once the old build exits, and dies hard (its
// connections cut, no release) with a message in hand. pod-x takes the expired record: that term
// is over a dead holder's record, not pod-x's own resumed after a lapse, so it settles, holding
// what it receives until the dead pod's message is back at its ack wait, and delivers that
// message before the newer one. A give-up that left pod-x counting the binding it had waited out
// as its own would keep the term from settling, and pod-x would deliver the newer message first,
// the dead pod's one ack wait later.
func TestAPodThatGaveTheLeaseUpWhileWaitingOutABindingSettlesAHardTakeoverAfterIt(t *testing.T) {
	const machineID = "box-gave-up"
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, machineID)})
	consumer := bus.ListenerDurable(machineID)
	old, _ := durableHeldElsewhere(t, granted.AdminURL, consumer)
	admin, err := bus.ConnectOwningStream([]string{granted.AdminURL}, bus.WithReplicas(1), bus.WithName("receive-test admin"))
	if err != nil {
		t.Fatalf("connect the admin: %v", err)
	}
	t.Cleanup(admin.Close)

	// pod-x reads its durable through a relay the test freezes; its lease is on a connection of
	// its own, which renews throughout.
	xLink := startListenerRelay(t, granted.URL())
	timing := testReceiveTiming()
	timing.alarmAfter = 2 * time.Second
	handle, received := recordingHandler()
	x := newReceiverRigAs(t, xLink.relayed, granted.URL(), seed, machineID, "pod-x", handle, timing, testLeaseTiming())
	x.lease.Start(context.Background())
	x.receiver.Start()
	x.awaitState(t, receiveAwaitingBinding, 10*time.Second)

	// pod-y's connections both go through relays, which the test severs for its hard death.
	yLink, yLeaseLink := startListenerRelay(t, granted.URL()), startListenerRelay(t, granted.URL())
	inHand, release := make(chan struct{}, 1), make(chan struct{})
	y := newReceiverRigAs(t, yLink.relayed, yLeaseLink.relayed, seed, machineID, "pod-y", func(context.Context, *natsgo.Msg) {
		select {
		case inHand <- struct{}{}:
		default:
		}
		<-release
	}, testReceiveTiming(), testLeaseTiming())
	t.Cleanup(func() { close(release) })
	y.lease.Start(context.Background())
	y.receiver.Start()
	waitFor(t, 5*time.Second, "pod-y to read pod-x's lease", func() bool { return y.lease.Status().ActiveInstance == "pod-x" })

	thaw := xLink.freeze()
	t.Cleanup(thaw)
	waitFor(t, 30*time.Second, "pod-x to give the lease up and pod-y to take it", y.lease.Holding)
	if !strings.Contains(x.logs.String(), "giving the listener lease up") {
		t.Fatalf("pod-y holds the lease but pod-x never gave it up:\n%s", x.logs.String())
	}
	y.awaitState(t, receiveAwaitingBinding, 5*time.Second)
	old.Close()
	y.awaitState(t, receiveActive, 10*time.Second)
	thaw()
	waitFor(t, 30*time.Second, "pod-x's bus connection to answer again", func() bool {
		_, err := x.client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil
	})
	if x.lease.Holding() {
		t.Fatal("pod-x took the lease back from pod-y")
	}

	publishDurable(t, admin, "older")
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("pod-y never took the older message")
	}
	yLink.sever() // pod-y dies hard: both its connections go, and it releases nothing
	yLeaseLink.sever()
	waitFor(t, 5*time.Second, "the server to drop pod-y's binding, the older message still outstanding with it", func() bool {
		info, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound && info.NumAckPending == 1
	})
	publishDurable(t, admin, "newer")

	waitFor(t, 10*time.Second, "pod-x to take pod-y's expired lease", x.lease.Holding)
	if status := x.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-x's lease = %+v, want taken over pod-y's expired record, settling", status)
	}
	first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-x's first message")
	second := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-x's second message")
	if first.data != "older" || second.data != "newer" {
		t.Fatalf("pod-x delivered %q then %q, want the older message pod-y died holding first", first.data, second.data)
	}
	if end := x.settleHoldEnd(t, time.Second); end["outcome"] != "settled" || end["messages"] != float64(2) {
		t.Fatalf("pod-x's settle hold ended %v, want settled holding both messages", end)
	}
}

// Two pods of one machine, each connected as the machine's nkey user holding exactly the grant
// envoy-infra renders for a listener. pod-x takes the lease while a pre-lease build binds the
// durable and waits that binding out; then its lease connection stalls past the lease's validity,
// so its term ends with the lease lost rather than given up. pod-y takes the expired record,
// receives beside the old build's binding, and dies hard (its connections cut, no release) with a
// message in hand. pod-x takes pod-y's expired record: the lease reports it no resume of pod-x's
// own record (leaseStatus.Resumed), so the binding pod-x once waited out counts for nothing and
// the term settles, holding what it receives until the dead pod's message is back at its ack wait
// and delivering that before the newer one. A flag that outlived a term ended by losing the
// lease would keep the term from settling, and pod-x would deliver the newer message first, the
// dead pod's one ack wait later.
func TestAPodThatLostTheLeaseWhileWaitingOutABindingSettlesAHardTakeoverAfterIt(t *testing.T) {
	const machineID = "box-lost-lease"
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, machineID)})
	consumer := bus.ListenerDurable(machineID)
	old, _ := durableHeldElsewhere(t, granted.AdminURL, consumer)
	admin, err := bus.ConnectOwningStream([]string{granted.AdminURL}, bus.WithReplicas(1), bus.WithName("receive-test admin"))
	if err != nil {
		t.Fatalf("connect the admin: %v", err)
	}
	t.Cleanup(admin.Close)

	// pod-x's lease is on a connection through a relay the test freezes; its bus connection
	// answers throughout, so nothing but the lease ends its term.
	xLeaseLink := startListenerRelay(t, granted.URL())
	handle, received := recordingHandler()
	x := newReceiverRigAs(t, granted.URL(), xLeaseLink.relayed, seed, machineID, "pod-x", handle, testReceiveTiming(), testLeaseTiming())
	x.lease.Start(context.Background())
	x.receiver.Start()
	x.awaitState(t, receiveAwaitingBinding, 10*time.Second)

	// pod-y's connections both go through relays, which the test severs for its hard death.
	yLink, yLeaseLink := startListenerRelay(t, granted.URL()), startListenerRelay(t, granted.URL())
	inHand, release := make(chan struct{}, 1), make(chan struct{})
	y := newReceiverRigAs(t, yLink.relayed, yLeaseLink.relayed, seed, machineID, "pod-y", func(context.Context, *natsgo.Msg) {
		select {
		case inHand <- struct{}{}:
		default:
		}
		<-release
	}, testReceiveTiming(), testLeaseTiming())
	t.Cleanup(func() { close(release) })
	y.lease.Start(context.Background())
	y.receiver.Start()
	waitFor(t, 5*time.Second, "pod-y to read pod-x's lease", func() bool { return y.lease.Status().ActiveInstance == "pod-x" })

	thaw := xLeaseLink.freeze()
	t.Cleanup(thaw)
	waitFor(t, 30*time.Second, "pod-y to take pod-x's expired record", y.lease.Holding)
	// The lapse is logged by the renewal that first starts past the validity, which the frozen
	// relay holds up, so it can follow pod-y's take-over.
	waitFor(t, 30*time.Second, "pod-x's lease to lapse, ending its term", func() bool {
		return strings.Contains(x.logs.String(), "listener lease lapsed")
	})
	if strings.Contains(x.logs.String(), "giving the listener lease up") {
		t.Fatalf("pod-x gave the lease up; this row ends its term by losing the lease:\n%s", x.logs.String())
	}
	y.awaitState(t, receiveActive, 10*time.Second)
	old.Close()
	thaw()
	waitFor(t, 30*time.Second, "pod-x to step down to a standby", func() bool {
		return !x.lease.Holding() && x.lease.Status().ActiveInstance == "pod-y"
	})

	publishDurable(t, admin, "older")
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("pod-y never took the older message")
	}
	yLink.sever() // pod-y dies hard: both its connections go, and it releases nothing
	yLeaseLink.sever()
	waitFor(t, 5*time.Second, "the server to drop pod-y's binding, the older message still outstanding with it", func() bool {
		info, err := admin.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound && info.NumAckPending == 1
	})
	publishDurable(t, admin, "newer")

	waitFor(t, 10*time.Second, "pod-x to take pod-y's expired lease", x.lease.Holding)
	if status := x.lease.Status(); status.How != leaseTakenExpired || !status.Settle || status.Resumed {
		t.Fatalf("pod-x's lease = %+v, want taken over pod-y's expired record, settling, no resume of its own", status)
	}
	first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-x's first message")
	second := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-x's second message")
	if first.data != "older" || second.data != "newer" {
		t.Fatalf("pod-x delivered %q then %q, want the older message pod-y died holding first", first.data, second.data)
	}
	if end := x.settleHoldEnd(t, time.Second); end["outcome"] != "settled" || end["messages"] != float64(2) {
		t.Fatalf("pod-x's settle hold ended %v, want settled holding both messages", end)
	}
}

// A term taken from a hand-back candidate that ends before it ever subscribed - a holder stuck on
// its durable that gives the lease up, or a stop while it prepares - releases the record owing an
// ack wait. No stopping pod ever saw this one receiving, and a lost node's binding had the
// deliver subject to itself meanwhile, so what the durable pushed comes back only at its ack
// wait: the next holder's record carries the debt, which makes its term no candidate at all, its
// hold the 7 s one, and the message the lost binding was pushed reaches its session before a
// newer one. A release that carried no debt would leave the next holder waiting for a mark that
// never comes, and its hold would run to the same 7 s - but a term that stopped mid-hold may owe
// what it held, which only the debt says.
func TestATermThatNeverSubscribedOwesTheNextHolderAnAckWait(t *testing.T) {
	for _, row := range []struct {
		name, machineID string
		giveUp          bool
	}{
		{"gives the lease up", "box-handback-giveup", true},
		{"stops before it subscribed", "box-handback-stop", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			granted, denied := consumerInfoDeniedNATS(t)
			// A lost node's connection still push-binds the durable and acknowledges nothing.
			client := bindAsADeadHolder(t, granted, row.machineID)
			timing := testReceiveTiming()
			timing.alarmAfter, timing.alarmEvery = 2*time.Second, time.Hour
			// pod-a polls slowly as a standby, so after it has released the record it does not
			// take its own back while the row reads it.
			stuckLease := testLeaseTiming()
			stuckLease.standbyPoll = 2 * time.Second
			stuckHandle, stuckReceived := recordingHandler()
			stuck := newReceiverRigVia(t, denied, granted, row.machineID, "pod-a", stuckHandle, timing, stuckLease)
			// The record is written last, as a standby reads a graceful hand-back's.
			putLeaseRecord(t, granted, row.machineID, `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`)
			stuck.lease.Start(context.Background())
			stuck.receiver.Start()
			waitFor(t, 10*time.Second, "pod-a to take the hand-back", func() bool {
				status := stuck.lease.Status()
				return status.Holding && status.Settle && status.HandBack
			})
			stuck.awaitState(t, receivePreparing, 10*time.Second)

			if row.giveUp {
				waitFor(t, 30*time.Second, "pod-a to give the lease up", func() bool {
					return strings.Contains(stuck.logs.String(), "giving the listener lease up")
				})
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				stuck.receiver.Stop(ctx)
				cancel()
			}
			noneReceived(t, stuckReceived, time.Millisecond, "pod-a, which never subscribed")
			stuck.awaitRecord(t, "released, unsettled and owing an ack wait", func(record leaseRecord) bool {
				return record.Holder == "pod-a" && record.Released && record.Unsettled && record.AckWaitOwed
			})

			handle, received := recordingHandler()
			next := newReceiverRig(t, granted, row.machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
			next.lease.Start(context.Background())
			waitFor(t, 15*time.Second, "pod-b to take the released record", next.lease.Holding)
			if status := next.lease.Status(); !status.Settle || status.HandBack {
				t.Fatalf("pod-b's lease = %+v, want settling and no hand-back candidate: pod-a's term never subscribed", status)
			}
			// The lost node's binding takes the older message alone, before pod-b subscribes.
			publishToTheDeadHolder(t, client, row.machineID, "older")
			next.receiver.Start()
			must := next.logRecord(t, "listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand", 10*time.Second)
			if must["cap"] != (consumerAckWait + settleMargin).String() {
				t.Fatalf("pod-b's settle hold = %v, want the %s cap of a term owed an ack wait", must, consumerAckWait+settleMargin)
			}
			next.awaitState(t, receiveActive, 10*time.Second)
			publishDurable(t, client, "newer")
			for _, want := range []string{"older", "newer"} {
				if message := awaitReceived(t, received, consumerAckWait+10*time.Second, "pod-b's "+want); message.data != want {
					t.Fatalf("pod-b delivered %q, want %q: the messages in the order they were published", message.data, want)
				}
			}
		})
	}
}
