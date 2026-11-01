package bus

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The role lane's subscription carries the lane's pending limits, as it holds it after connecting
// and as a restore binds it again on a connection dialled to replace a closed one, so nats.go
// queues a role message rather than dropping it however far behind the handler falls, and the
// buffer it may grow to is still bounded. A subscription SubscribeCore makes keeps nats.go's
// defaults.
func TestRoleLaneSubscriptionCarriesTheLanesPendingLimits(t *testing.T) {
	uri := testnats.URL(t)
	lane, _, err := ConnectRoleLane([]string{uri}, "example-machine", func(*nats.Msg) {})
	if err != nil {
		t.Fatalf("connect the role lane: %v", err)
	}
	t.Cleanup(lane.Close)

	lane.subscriptionsMu.Lock()
	registration := lane.subscriptions[coreSubscription]
	lane.subscriptionsMu.Unlock()
	if messages, bytes, err := registration.active.PendingLimits(); err != nil || messages != RoleLanePendingMsgs || bytes != RoleLanePendingBytes {
		t.Fatalf("the role lane's pending limits = %d messages, %d bytes (%v); want %d and %d", messages, bytes, err, RoleLanePendingMsgs, RoleLanePendingBytes)
	}
	rebound, err := bind(registration, lane.Conn, lane.js)
	if err != nil {
		t.Fatalf("bind the registration again: %v", err)
	}
	t.Cleanup(func() { _ = rebound.Unsubscribe() })
	if messages, bytes, err := rebound.PendingLimits(); err != nil || messages != RoleLanePendingMsgs || bytes != RoleLanePendingBytes {
		t.Fatalf("the role lane bound again has pending limits %d messages, %d bytes (%v); want %d and %d", messages, bytes, err, RoleLanePendingMsgs, RoleLanePendingBytes)
	}

	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	plain, err := client.SubscribeCore("notifications.team.plain", func(*nats.Msg) {})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if messages, bytes, err := plain.PendingLimits(); err != nil || messages != nats.DefaultSubPendingMsgsLimit || bytes != nats.DefaultSubPendingBytesLimit {
		t.Fatalf("a plain core subscription's pending limits = %d messages, %d bytes (%v); want nats.go's defaults", messages, bytes, err)
	}
}
