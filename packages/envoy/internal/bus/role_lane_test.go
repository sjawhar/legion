package bus_test

import (
	"os"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const roleLaneMachine = "example machine"

// requireRoleMessage publishes a role message as another connection of the same user and requires
// the lane's handler to receive it within 5 s.
func requireRoleMessage(t *testing.T, uri string, received <-chan string) {
	t.Helper()
	publisher, err := bus.Dial("role-publisher", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	const topic = contracts.RoleTopicPrefix + "reviewer"
	if err := publisher.Publish(topic, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	select {
	case got := <-received:
		if got != topic {
			t.Fatalf("the role lane received %s, want %s", got, topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role lane received nothing")
	}
}

// A user granted the shared role-lane queue group subscribes in it.
func TestRoleLaneSubscribesInTheSharedQueueGroup(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, "_INBOX.>", contracts.RoleTopicPrefix+"> "+bus.RoleLaneQueue), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	received := make(chan string, 1)
	lane, queue, err := bus.ConnectRoleLane([]string{uri}, roleLaneMachine, func(msg *natsgo.Msg) { received <- msg.Subject })
	if err != nil {
		t.Fatalf("connect the role lane: %v", err)
	}
	t.Cleanup(lane.Close)
	if queue != bus.RoleLaneQueue {
		t.Fatalf("the role lane subscribed in %q, want %q", queue, bus.RoleLaneQueue)
	}
	if !lane.CoreSubOK() {
		t.Fatal("CoreSubOK is false for the accepted role lane")
	}
	requireRoleMessage(t, uri, received)
}

// A user whose grant predates the shared group, holding only its machine's role-lane queue entry,
// is refused the shared group, and the lane subscribes in the machine's group instead and receives
// role messages there.
func TestRoleLaneFallsBackToTheMachinesQueueGroupWhenTheSharedOneIsRefused(t *testing.T) {
	seed, public := testnats.User(t)
	machineQueue := bus.ListenerQueue(roleLaneMachine)
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, "_INBOX.>", contracts.RoleTopicPrefix+"> "+machineQueue), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	received := make(chan string, 1)
	lane, queue, err := bus.ConnectRoleLane([]string{uri}, roleLaneMachine, func(msg *natsgo.Msg) { received <- msg.Subject })
	if err != nil {
		t.Fatalf("connect the role lane: %v", err)
	}
	t.Cleanup(lane.Close)
	if queue != machineQueue {
		t.Fatalf("the role lane subscribed in %q, want the machine's %q", queue, machineQueue)
	}
	if !lane.CoreSubOK() {
		t.Fatal("CoreSubOK is false for the role lane in the machine's group")
	}
	requireRoleMessage(t, uri, received)
}

// A user granted neither group gets an error, not a lane that delivers nothing.
func TestRoleLaneWithNeitherQueueGroupGrantedIsAnError(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, "_INBOX.>"), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	if lane, _, err := bus.ConnectRoleLane([]string{uri}, roleLaneMachine, func(*natsgo.Msg) {}); err == nil {
		lane.Close()
		t.Fatal("ConnectRoleLane succeeded for a user granted no role-lane queue group")
	}
}

// A client counts each slow consumer its connection reports: a core subscription narrowed to one
// pending message, its handler held, falls behind at the third message.
func TestClientCountsSlowConsumers(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	const topic = "notifications.team.slow"
	sub, err := client.SubscribeCore(topic, func(*natsgo.Msg) { <-release })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.SetPendingLimits(1, -1); err != nil {
		t.Fatalf("narrow the pending limits: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)
	for range 3 {
		if err := publisher.Publish(topic, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the publishes: %v", err)
	}
	waitFor(t, 10*time.Second, "the client to count the slow consumer", func() bool { return client.SlowConsumers() == 1 })
}
