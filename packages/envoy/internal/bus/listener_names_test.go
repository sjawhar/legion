package bus_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The listener's durable is the name cmd/listener spelled itself before it moved here: a space in
// the machine id becomes a hyphen. The retired role-lane group, which no listener subscribes in
// any more and the grant admits for one release, spells a machine id the same way.
func TestListenerDurableAndTheRetiredQueueKeepTheListenersNames(t *testing.T) {
	for _, tc := range []struct{ machineID, durable, queue string }{
		{"example-host-a", "listener-example-host-a", "envoy-listener-example-host-a"},
		{"example host", "listener-example-host", "envoy-listener-example-host"},
	} {
		if got := bus.ListenerDurable(tc.machineID); got != tc.durable {
			t.Errorf("ListenerDurable(%q) = %q, want %q", tc.machineID, got, tc.durable)
		}
		if got := bus.ListenerQueue(tc.machineID); got != tc.queue {
			t.Errorf("ListenerQueue(%q) = %q, want %q", tc.machineID, got, tc.queue)
		}
	}
}

// The group every listener subscribes the role lanes in names no machine, so a role message is
// handed to one listener and forwarded once however many machines run one. A machine named
// role-lane gives ListenerQueue that same name; core.ListenerPermissions writes one entry for
// the two.
func TestListenerRoleQueueNamesNoMachine(t *testing.T) {
	if got := bus.ListenerRoleQueue; got != "envoy-listener-role-lane" {
		t.Errorf("ListenerRoleQueue = %q, want %q", got, "envoy-listener-role-lane")
	}
}

// The live lane's queue group spells the machine id, and is a group of its own: in the role lanes'
// group the live lane would be a member NATS could hand a role message to.
func TestListenerLiveQueueIsItsOwnGroupForTheMachine(t *testing.T) {
	for _, tc := range []struct{ machineID, queue string }{
		{"example-host-a", "envoy-listener-live-example-host-a"},
		{"example host", "envoy-listener-live-example-host"},
	} {
		got := bus.ListenerLiveQueue(tc.machineID)
		if got != tc.queue {
			t.Errorf("ListenerLiveQueue(%q) = %q, want %q", tc.machineID, got, tc.queue)
		}
		if got == bus.ListenerRoleQueue {
			t.Errorf("ListenerLiveQueue(%q) = ListenerRoleQueue's %q", tc.machineID, got)
		}
	}
}

func TestDispatchNamesKeepDispatchsExistingNames(t *testing.T) {
	if got := bus.DispatchDeliveryDurable; got != "delivery-events" {
		t.Errorf("DispatchDeliveryDurable = %q, want %q", got, "delivery-events")
	}
	if got := bus.DispatchJobDurable; got != "delivery-jobs" {
		t.Errorf("DispatchJobDurable = %q, want %q", got, "delivery-jobs")
	}
	if got := bus.WebhookRedeliveryBucket; got != "envoy_webhook_redelivery" {
		t.Errorf("WebhookRedeliveryBucket = %q, want %q", got, "envoy_webhook_redelivery")
	}
}
