package testnats

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// StartCluster runs n NATS servers with JetStream that form one cluster, as a hub's servers do,
// and returns them once every server has a route to every other and JetStream has a meta leader
// that every server follows. Each is a server this package's start makes, behind a relay of its
// own, so its URL survives Stop and Start; a stopped server started again rejoins the cluster,
// because its route port, written into its configuration, is the same at every start. Each has
// the rig's short lame duck (rigLameDuck), so Server.LameDuck takes seconds.
//
// The route ports come from the band below the machine's ephemeral range that gates take theirs
// from (gatePorts), where no :0 bind can be handed one while its server is down. The servers
// advertise no client URLs (no_advertise), so a client reaches only the relay URLs a test gives
// it and never a server's own port, which a restart changes. The cluster's name is its first
// route port's, which no other cluster running on the machine can hold at the same time, so two
// clusters whose ports meet across a restart refuse each other's routes rather than merge.
func StartCluster(t testing.TB, n int) []*Server {
	t.Helper()
	if n < 2 {
		t.Fatalf("a cluster needs at least two servers, asked for %d", n)
		return nil
	}
	// Every route port is held until all are chosen, so no two servers draw the same one; each is
	// freed for its server to listen on.
	listeners := make([]net.Listener, n)
	for i := range listeners {
		listeners[i] = listenInGateBand(t)
		if listeners[i] == nil {
			return nil
		}
	}
	routes := make([]string, n)
	for i, listener := range listeners {
		routes[i] = listener.Addr().String()
		_ = listener.Close()
	}
	name := "testnats-" + strings.TrimPrefix(routes[0], "127.0.0.1:")
	servers := make([]*Server, n)
	for i := range servers {
		servers[i] = start(t, clusterConfig(name, fmt.Sprintf("%s-s%d", name, i+1), routes[i], routes), []string{"-js"})
	}
	awaitCluster(t, servers)
	return servers
}

// clusterConfig is the configuration of the cluster server serverName, whose routes listen on
// listen, in the cluster clusterName whose servers' routes listen on routes.
func clusterConfig(clusterName, serverName, listen string, routes []string) string {
	var config strings.Builder
	fmt.Fprintf(&config, "host: 127.0.0.1\nserver_name: %q\n", serverName)
	fmt.Fprintf(&config, "cluster {\n  name: %q\n  listen: %q\n  no_advertise: true\n  routes: [\n", clusterName, listen)
	for _, route := range routes {
		fmt.Fprintf(&config, "    %q\n", "nats-route://"+route)
	}
	config.WriteString("  ]\n}\n")
	return config.String()
}

// awaitCluster waits, within connectTimeout, until every server runs, has a route to each of the
// others and follows a JetStream meta leader one of them is, and fails the test naming what each
// server lacked when that does not happen. A test that stops and starts a cluster server calls it
// to wait for the server to rejoin.
func awaitCluster(t testing.TB, servers []*Server) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	for {
		lacking := clusterLacks(servers)
		if lacking == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster did not form within %s: %s", connectTimeout, lacking)
			return
		}
		time.Sleep(retryInterval)
	}
}

// clusterLacks names what keeps servers from being one formed cluster, or returns "" when nothing
// does.
func clusterLacks(servers []*Server) string {
	var lacks []string
	leaders := 0
	for _, server := range servers {
		server.mu.Lock()
		srv := server.srv
		server.mu.Unlock()
		if srv == nil {
			lacks = append(lacks, server.Name()+" is stopped")
			continue
		}
		if routes := srv.NumRemotes(); routes != len(servers)-1 {
			lacks = append(lacks, fmt.Sprintf("%s has routes to %d servers, want %d", server.Name(), routes, len(servers)-1))
		}
		if !srv.JetStreamIsCurrent() {
			lacks = append(lacks, server.Name()+" does not follow a current JetStream meta leader")
		}
		if srv.JetStreamIsLeader() {
			leaders++
		}
	}
	if leaders != 1 {
		lacks = append(lacks, fmt.Sprintf("%d servers are JetStream meta leader, want 1", leaders))
	}
	return strings.Join(lacks, "; ")
}
