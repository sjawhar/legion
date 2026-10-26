package main

import "testing"

// The role lane's handler, the durable's and the one the self-health rebind builds after a NATS
// flap draw on one deferral set, so a rebind holds the budget the handler it replaces was holding
// rather than opening a second one beside it. listenerDeliveryHandler refuses a config with no set,
// which catches a set left out but not a second one, so this reads the three configs main builds
// those handlers from (newListenerDeliveryHandlerConfigs) and asserts they carry the same set. Any
// one of them given a fresh newUnplaceableDeferrals() fails it.
func TestListenerDeliveryHandlersShareOneDeferralSet(t *testing.T) {
	const machineID = "test-machine"
	configs := newListenerDeliveryHandlerConfigs(listenerDeliveryHandlerConfig{machineID: machineID})
	shared := configs.roleLane.deferredUnplaceable
	if shared == nil {
		t.Fatal("the role lane's config carries no deferral set")
	}
	for _, handler := range []struct {
		name   string
		config listenerDeliveryHandlerConfig
	}{
		{"role lane", configs.roleLane},
		{"durable", configs.durable},
		{"self-health rebind", configs.rebind},
	} {
		if handler.config.deferredUnplaceable != shared {
			t.Fatalf("the %s's config carries deferral set %p, want the role lane's %p: two sets are two budgets", handler.name, handler.config.deferredUnplaceable, shared)
		}
		if handler.config.machineID != machineID {
			t.Fatalf("the %s's config names machine %q, want %q: it is not built from the dependencies main passed", handler.name, handler.config.machineID, machineID)
		}
	}
}
