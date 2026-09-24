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
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// The listener opens its interest registry, session registry and CI store from client.Conn once,
// at start. A NATS server restart must leave that state working: the connection reconnects in
// place rather than closing, so a JetStream handle taken from it before the restart is still bound
// to a live connection afterwards.
func TestAServerRestartKeepsJetStreamStateTakenFromTheConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()

	ctx := context.Background()
	// A fixed host port survives the container's stop and start, as a server's address does.
	ctr, err := tcnats.Run(ctx, testnats.Image, testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
		hostConfig.PortBindings = nat.PortMap{"4222/tcp": {{HostIP: "127.0.0.1", HostPort: port}}}
	}))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri := "nats://127.0.0.1:" + port
	testnats.Connect(t, uri).Close()

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

	stopTimeout := time.Second
	if err := ctr.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop NATS: %v", err)
	}
	if err := ctr.Start(ctx); err != nil {
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
