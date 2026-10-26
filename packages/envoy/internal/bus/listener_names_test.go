package bus_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The listener's durable and role-lane queue are the names cmd/listener spelled itself before they
// moved here: a space in the machine id becomes a hyphen in both.
func TestListenerDurableAndQueueKeepTheListenersNames(t *testing.T) {
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

// The live lane's queue group spells the machine id as the role lanes' does, and is a group of its
// own: in the role lanes' group the live lane would be a member NATS could hand a role message to.
func TestListenerLiveQueueIsItsOwnGroupForTheMachine(t *testing.T) {
	for _, tc := range []struct{ machineID, queue string }{
		{"example-host-a", "envoy-listener-live-example-host-a"},
		{"example host", "envoy-listener-live-example-host"},
	} {
		got := bus.ListenerLiveQueue(tc.machineID)
		if got != tc.queue {
			t.Errorf("ListenerLiveQueue(%q) = %q, want %q", tc.machineID, got, tc.queue)
		}
		if got == bus.ListenerQueue(tc.machineID) {
			t.Errorf("ListenerLiveQueue(%q) = ListenerQueue's %q", tc.machineID, got)
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

// The Legion agent registry's bucket is the name the INFRA-204 architect fixed, which the listener's
// NATS grant (packages/envoy/infra/core/permissions.go) and every reader of the bucket share.
func TestLegionAgentBucketKeepsItsName(t *testing.T) {
	if got := bus.LegionAgentBucket; got != "legion_agents" {
		t.Errorf("LegionAgentBucket = %q, want %q", got, "legion_agents")
	}
}
