package bus_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// startRestartableNATS runs a NATS container on a fixed host port, which survives the container's
// stop and start as a server's address does, and returns it with its URL once it answers.
func startRestartableNATS(t *testing.T) (*tcnats.NATSContainer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	ctr, err := tcnats.Run(context.Background(), testnats.Image, testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
		hostConfig.PortBindings = nat.PortMap{"4222/tcp": {{HostIP: "127.0.0.1", HostPort: port}}}
	}))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri := "nats://127.0.0.1:" + port
	testnats.Connect(t, uri).Close()
	return ctr, uri
}

func stopNATS(t *testing.T, ctr *tcnats.NATSContainer) {
	t.Helper()
	timeout := time.Second
	if err := ctr.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("stop NATS: %v", err)
	}
}

// The listener opens its interest registry, session registry and CI store from client.Conn once,
// at start. A NATS server restart must leave that state working: the connection reconnects in
// place rather than closing, so a JetStream handle taken from it before the restart is still bound
// to a live connection afterwards.
func TestAServerRestartKeepsJetStreamStateTakenFromTheConnection(t *testing.T) {
	ctr, uri := startRestartableNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "server-restart"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	stopNATS(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	waitFor(t, 30*time.Second, "the client to reconnect", client.Connected)
	var putErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, putErr = kv.Put("key", []byte("value")); putErr == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("a KV handle taken before the restart still fails after the client reconnected: %v", putErr)
}

// A publish while the server is away fails, and a publish that failed is never sent later: the
// callers treat an error as not delivered (a role-lane forward as delivery_failed, a webhook as a
// 500 for the sender to retry), so a copy held and sent after reconnecting would arrive twice.
func TestAPublishThatFailedWhileReconnectingIsNeverSent(t *testing.T) {
	ctr, uri := startRestartableNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	stopNATS(t, ctr)
	waitFor(t, 15*time.Second, "the client to notice the server is gone", func() bool { return !client.Connected() })
	item := contracts.Envelope{
		EventID:       "evt-publish-while-reconnecting",
		Source:        "agent",
		SourceEventID: "source-publish-while-reconnecting",
		Topic:         "notifications.role.publish-while-reconnecting",
		DedupeKey:     "publish-while-reconnecting",
		IssuedAt:      contracts.NowMillis(),
	}
	if err := client.PublishCore(item); err == nil {
		t.Fatal("a core publish while the server was away reported success")
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	observer := testnats.Connect(t, uri)
	t.Cleanup(observer.Close)
	received := make(chan *natsgo.Msg, 1)
	if _, err := observer.ChanSubscribe(item.Topic, received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	waitFor(t, 30*time.Second, "the client to reconnect", client.Connected)
	select {
	case message := <-received:
		t.Fatalf("the publish that failed was sent after the reconnect: %s", message.Data)
	case <-time.After(2 * time.Second):
	}
}
