package testnats

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Three cluster servers are one cluster with one JetStream: a stream made at three replicas
// through one server's URL gets a leader and a current replica on each of the other two, so a test
// built on the rig stores on every server as a hub's three servers do.
func TestAClusterReplicatesAStreamOnEveryServer(t *testing.T) {
	servers := StartCluster(t, 3)
	names := serverNames(servers)
	if slices.Contains(names, "") || len(slices.Compact(slices.Sorted(slices.Values(names)))) != 3 {
		t.Fatalf("the cluster's server names are %v, want three distinct names", names)
	}

	conn := Connect(t, servers[0].URL())
	t.Cleanup(conn.Close)
	js := jetStream(t, conn)
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "REPLICATED", Subjects: []string{"replicated.>"}, Replicas: 3}); err != nil {
		t.Fatalf("add a stream at three replicas: %v", err)
	}
	awaitStream(t, js, "REPLICATED", "a leader and a current replica on every other server", func(cluster *natsgo.ClusterInfo) bool {
		return everyReplicaCurrent(cluster, names)
	})
}

// A server in lame duck closes its clients' connections, and a client that knows the cluster's
// other URLs reconnects to one of them: the move a hub's server change forces on every session.
// The client knows only the URLs it was given, since the servers advertise none of their own.
func TestALameDuckServerSendsItsClientsToAnotherServer(t *testing.T) {
	servers := StartCluster(t, 3)
	urls := make([]string, len(servers))
	for i, server := range servers {
		urls[i] = server.URL()
	}
	conn, err := natsgo.Connect(strings.Join(urls, ","),
		natsgo.DontRandomize(),
		natsgo.Timeout(dialTimeout),
		natsgo.ReconnectWait(100*time.Millisecond),
		natsgo.MaxReconnects(-1),
	)
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(conn.Close)
	if got := conn.ConnectedServerName(); got != servers[0].Name() {
		t.Fatalf("the client is connected to %q, want the first URL's server %q", got, servers[0].Name())
	}
	if discovered := conn.DiscoveredServers(); len(discovered) != 0 {
		t.Fatalf("the servers advertised %v to the client, want no URL beyond the ones it was given", discovered)
	}

	servers[0].LameDuck(t)

	others := serverNames(servers[1:])
	deadline := time.Now().Add(connectTimeout)
	for !conn.IsConnected() || !slices.Contains(others, conn.ConnectedServerName()) {
		if time.Now().After(deadline) {
			t.Fatalf("the client did not move to another server within %s of the lame duck: status %s, connected to %q, want one of %v", connectTimeout, conn.Status(), conn.ConnectedServerName(), others)
		}
		time.Sleep(retryInterval)
	}
}

// A cluster server stopped and started again rejoins the cluster it left, because its route port
// is the same at every start: its routes come back, and its replica of a stream catches up with
// what the other two stored while it was down.
func TestAStoppedClusterServerRejoinsWithItsReplica(t *testing.T) {
	servers := StartCluster(t, 3)
	names := serverNames(servers)
	conn := Connect(t, servers[0].URL())
	t.Cleanup(conn.Close)
	js := jetStream(t, conn)
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "REJOINED", Subjects: []string{"rejoined.>"}, Replicas: 3}); err != nil {
		t.Fatalf("add a stream at three replicas: %v", err)
	}
	awaitStream(t, js, "REJOINED", "every replica current", func(cluster *natsgo.ClusterInfo) bool {
		return everyReplicaCurrent(cluster, names)
	})

	stopped := servers[2]
	name := stopped.Name()
	stopped.Stop(t)
	// The control that keeps the rejoin honest: while the server is down its replica is not
	// current, so the wait below that finds it current has seen it come back.
	awaitStream(t, js, "REJOINED", name+"'s replica not current", func(cluster *natsgo.ClusterInfo) bool {
		return !everyReplicaCurrent(cluster, names)
	})
	awaitPublish(t, js, "rejoined.while-down")

	stopped.Start(t)
	if got := stopped.Name(); got != name {
		t.Errorf("the restarted server is named %q, want %q as before", got, name)
	}
	awaitCluster(t, servers)
	awaitStream(t, js, "REJOINED", "every replica current again", func(cluster *natsgo.ClusterInfo) bool {
		return everyReplicaCurrent(cluster, names)
	})
}

func serverNames(servers []*Server) []string {
	names := make([]string, len(servers))
	for i, server := range servers {
		names[i] = server.Name()
	}
	return names
}

func jetStream(t *testing.T, conn *natsgo.Conn) natsgo.JetStreamContext {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// everyReplicaCurrent reports whether the stream has a leader and a current, online replica on
// every other server names lists.
func everyReplicaCurrent(cluster *natsgo.ClusterInfo, names []string) bool {
	if cluster == nil || cluster.Leader == "" {
		return false
	}
	seen := []string{cluster.Leader}
	for _, replica := range cluster.Replicas {
		if !replica.Current || replica.Offline {
			return false
		}
		seen = append(seen, replica.Name)
	}
	slices.Sort(seen)
	return slices.Equal(seen, slices.Sorted(slices.Values(names)))
}

// awaitStream waits, within connectTimeout, until the stream's cluster state satisfies want, and
// fails the test naming the state it last read. A stream between leaders answers its info with an
// error, which is read again.
func awaitStream(t *testing.T, js natsgo.JetStreamContext, stream, desc string, want func(*natsgo.ClusterInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var last string
	for {
		info, err := js.StreamInfo(stream, natsgo.MaxWait(dialTimeout))
		if err == nil && want(info.Cluster) {
			return
		}
		last = describeStream(info, err)
		if time.Now().After(deadline) {
			t.Fatalf("stream %s did not have %s within %s: %s", stream, desc, connectTimeout, last)
		}
		time.Sleep(retryInterval)
	}
}

func describeStream(info *natsgo.StreamInfo, err error) string {
	if err != nil {
		return err.Error()
	}
	if info.Cluster == nil {
		return "no cluster information"
	}
	replicas := make([]string, len(info.Cluster.Replicas))
	for i, replica := range info.Cluster.Replicas {
		replicas[i] = fmt.Sprintf("%s(current=%t offline=%t)", replica.Name, replica.Current, replica.Offline)
	}
	return fmt.Sprintf("leader %q, replicas %v", info.Cluster.Leader, replicas)
}

// awaitPublish publishes to subject, retrying within connectTimeout while the stream elects a new
// leader in place of a stopped one.
func awaitPublish(t *testing.T, js natsgo.JetStreamContext, subject string) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	for {
		_, err := js.Publish(subject, []byte(subject), natsgo.AckWait(dialTimeout))
		if err == nil {
			return
		}
		if time.Now().After(deadline) || errors.Is(err, natsgo.ErrConnectionClosed) {
			t.Fatalf("publish %s: %v", subject, err)
		}
		time.Sleep(retryInterval)
	}
}
