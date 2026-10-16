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
