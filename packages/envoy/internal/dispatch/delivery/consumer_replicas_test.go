package delivery

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Dispatch's two durables, created with the policy their intakes stamp on them (which sets no
// replica count), follow the stream when a start raises it from one copy to the deployment's three:
// a durable left on one copy would lose its place with that one server.
func TestDispatchDurablesMoveToThreeCopiesWhenAStartRaisesTheStream(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	start := func(replicas int) *bus.Client {
		t.Helper()
		client, err := bus.ConnectOwningStream(cluster.URLs(), bus.WithReplicas(replicas))
		if err != nil {
			t.Fatalf("start the stream at %d copies: %v", replicas, err)
		}
		t.Cleanup(client.Close)
		return client
	}
	js := start(1).JS()
	for _, config := range []natsgo.ConsumerConfig{deliveryConsumerConfig(), jobConsumerConfig()} {
		if _, err := js.AddConsumer(bus.Stream, &config); err != nil {
			t.Fatalf("create durable %s: %v", config.Durable, err)
		}
	}

	start(3)

	for _, durable := range []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable} {
		deadline := time.Now().Add(30 * time.Second)
		for {
			info, err := js.ConsumerInfo(bus.Stream, durable)
			if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && 1+len(info.Cluster.Replicas) == 3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("durable %s did not reach three copies: %+v (%v)", durable, info, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
