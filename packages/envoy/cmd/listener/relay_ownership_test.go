package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Which pod of a machine relays its sessions' consumers is the receive term's to say and nothing
// else's: the term elects this pod when it begins (durableReceiver.begin) and stands it down when
// it ends (durableReceiver.end, sessionRelays.setOwned). The self-health monitor's durable probe
// and its rebuild, which judge whether the durable is bound to this listener's subscription, say
// nothing about it, and the two rows below are why.
//
// A second writer that went by the binding - the probe clearing the flag on an unbound durable,
// the rebuild setting it after a resubscribe - reads a state the term does not have: a resubscribe
// reports success for a pod with no term at all (durableReceiver.resubscribe), so a lease lost
// between the two would leave a standby relaying every session's consumer beside the real holder,
// each pull and each ack-floor restart racing the other's; and an unbound probe inside a live term
// that something else repaired would leave the holder relaying nothing for the rest of it.

// startRelayingReceiverRig is startReceiverRig for a pod whose term elects it the relayer of its
// machine's session consumers, as main hands its receiver the listener's relays. The set is a bare
// one, with no session to relay: these rows are about which pod is elected, not about what it
// then pulls, and sessionRelays.setOwned is the whole of the election. The lease polls slowly as a
// standby, so a pod that gives its lease up does not take it back while the row reads it.
func startRelayingReceiverRig(t *testing.T, uri, machineID, instance string, handle durableHandler) (*receiverRig, *sessionRelays) {
	t.Helper()
	leaseTiming := testLeaseTiming()
	leaseTiming.standbyPoll = 3 * time.Second
	rig := newReceiverRig(t, uri, machineID, instance, handle, testReceiveTiming(), leaseTiming)
	relays := &sessionRelays{cfg: listenerDeliveryHandlerConfig{logger: logging.New("relay-ownership " + instance)}}
	rig.receiver.relays = relays
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	return rig, relays
}

// A lease lost between an unbound probe and the resubscribe that follows it leaves the pod not
// relaying: its term is over, and the resubscribe that reports success for a pod with no term
// elects nobody.
func TestALeaseLostBetweenAnUnboundProbeAndTheResubscribeLeavesThePodNotRelaying(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-relay-ownership-lost"
	handle, _ := recordingHandler()
	rig, relays := startRelayingReceiverRig(t, uri, machineID, "pod-a", handle)
	rig.awaitState(t, receiveActive, 10*time.Second)
	if !relays.owned.Load() {
		t.Fatal("the pod receiving the durable relays none of its machine's session consumers")
	}

	// The holder's probe reads the durable unbound: it was recreated server-side, so its deliver
	// subject is no longer this pod's subscription's.
	recreateListenerDurable(t, rig.client, rig.consumer)
	if err := rig.receiver.probe(context.Background()); !errors.Is(err, errDurableUnbound) {
		t.Fatalf("the probe of a recreated durable = %v, want errDurableUnbound", err)
	}

	// The term ends before the resubscribe runs, as it does when the lease is released because
	// the receive connection is down.
	rig.lease.GiveUp(rig.lease.Status().Term)
	waitFor(t, 10*time.Second, "pod-a to give the lease up", func() bool { return !rig.lease.Holding() })
	rig.awaitState(t, receiveStandby, 10*time.Second)
	if relays.owned.Load() {
		t.Fatal("the pod goes on relaying after its term ended")
	}

	// The rebuild runs on, with nothing left to subscribe.
	if err := rig.receiver.resubscribe(context.Background()); err != nil {
		t.Fatalf("the rebuild's resubscribe on a pod with no term = %v, want nil", err)
	}
	probe := func() error { return rig.receiver.probe(context.Background()) }
	if err := rebuildListenerDependencies(context.Background(), rig.client, nil, probe, rig.receiver.resubscribe); err != nil {
		t.Fatalf("the self-health rebuild on a pod with no term: %v", err)
	}
	if relays.owned.Load() {
		t.Fatal("a pod with no term relays its machine's session consumers: it pulls every session's consumer beside the pod that holds the lease, and each ack-floor restart deletes the other's")
	}
}

// An unbound probe inside a live term leaves the holder relaying, whether or not a rebuild
// resubscribes after it: the durable's binding is repaired by the receive loop as well (repair),
// and a rebuild whose probe then finds the durable bound returns without resubscribing. Neither
// says anything about the term, which is what elects this pod.
func TestAnUnboundProbeInsideALiveTermLeavesTheHolderRelaying(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-relay-ownership-live"
	handle, _ := recordingHandler()
	rig, relays := startRelayingReceiverRig(t, uri, machineID, "pod-a", handle)
	rig.awaitState(t, receiveActive, 10*time.Second)

	recreateListenerDurable(t, rig.client, rig.consumer)
	if err := rig.receiver.probe(context.Background()); !errors.Is(err, errDurableUnbound) {
		t.Fatalf("the probe of a recreated durable = %v, want errDurableUnbound", err)
	}
	if !relays.owned.Load() {
		t.Fatal("an unbound probe stopped the relays of a pod whose term is running")
	}

	// The receive loop repairs the binding itself, so the rebuild's own probe finds the durable
	// bound again and returns without a resubscribe of its own.
	if err := rig.receiver.resubscribe(context.Background()); err != nil {
		t.Fatalf("the repair's resubscribe: %v", err)
	}
	probe := func() error { return rig.receiver.probe(context.Background()) }
	if err := rebuildListenerDependencies(context.Background(), rig.client, nil, probe, rig.receiver.resubscribe); err != nil {
		t.Fatalf("the self-health rebuild of a repaired durable: %v", err)
	}
	if !relays.owned.Load() {
		t.Fatal("the holder relays nothing for the rest of its term after an unbound probe its own repair answered")
	}
	if !rig.lease.Holding() || rig.receiver.report().State != receiveActive {
		t.Fatalf("pod-a is no longer the holder receiving: lease held %v, state %s", rig.lease.Holding(), rig.receiver.report().State)
	}
}
