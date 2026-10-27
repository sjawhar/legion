package bus

import (
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// clusterSize is the number of servers startJetStreamCluster runs: the fewest that hold a stream of
// three replicas, the count a deployment that replicates the stream gives it.
const clusterSize = 3

// startJetStreamCluster runs a JetStream cluster of clusterSize nats-servers in this test process,
// each with a store of its own, and returns their client URLs once the cluster can place a stream of
// clusterSize replicas. testnats runs one server, which refuses a stream of more than one replica, so
// a test of what an ensure does to a replicated stream needs this cluster. Every server is shut down
// when the test ends.
func startJetStreamCluster(t *testing.T) []string {
	t.Helper()
	routePorts := make([]int, clusterSize)
	for i := range routePorts {
		routePorts[i] = freePort(t)
	}
	routes := make([]*url.URL, clusterSize)
	for i, port := range routePorts {
		routes[i] = &url.URL{Scheme: "nats-route", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	}
	urls := make([]string, clusterSize)
	for i := range clusterSize {
		opts := &natsserver.Options{
			ServerName: fmt.Sprintf("bus-cluster-%d", i),
			Host:       "127.0.0.1",
			Port:       -1,
			JetStream:  true,
			StoreDir:   t.TempDir(),
			NoSigs:     true,
			NoLog:      true,
			Cluster:    natsserver.ClusterOpts{Name: "bus-cluster", Host: "127.0.0.1", Port: routePorts[i]},
			Routes:     routes,
		}
		srv, err := natsserver.NewServer(opts)
		if err != nil {
			t.Fatalf("create cluster server %d: %v", i, err)
		}
		go srv.Start()
		t.Cleanup(func() {
			srv.Shutdown()
			srv.WaitForShutdown()
		})
		if !srv.ReadyForConnections(10 * time.Second) {
			t.Fatalf("cluster server %d did not accept connections", i)
		}
		urls[i] = srv.ClientURL()
	}
	awaitReplicatedStreams(t, urls)
	return urls
}

// freePort is a TCP port on the loopback interface that nothing listened on a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// awaitReplicatedStreams returns once the cluster at urls has elected its meta leader and knows
// every peer: it creates a probe stream of clusterSize replicas. The probe stays, so a test that
// deletes and creates ENVOY_NOTIFICATIONS never empties the account: nats-server removes an
// account's streams directory in the background after its last stream is deleted, and a create that
// lands meanwhile can fail (testnats.RecreateKeyValue).
func awaitReplicatedStreams(t *testing.T, urls []string) {
	t.Helper()
	conn, err := nats.Connect(urls[0], nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream(nats.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("open the cluster's JetStream: %v", err)
	}
	probe := &nats.StreamConfig{Name: "PROBE", Subjects: []string{"probe.>"}, Replicas: clusterSize}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err = js.AddStream(probe)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster never placed a stream of %d replicas: %v", clusterSize, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
