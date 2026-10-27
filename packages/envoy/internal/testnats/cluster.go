package testnats

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Cluster is n nats-servers in this test process, routed to each other with JetStream clustered
// across them, as a replicated hub runs. Each member is a Server start made, so Stop and Start work
// on one member as on a lone server: a restart keeps the member's store, its client URL and its
// route URL.
type Cluster struct {
	Servers []*Server
}

// StartCluster runs a cluster of n servers with JetStream, each with a store of its own, shut down
// when the test ends, and returns it once JetStream answers on every member with a meta leader that
// knows all n (AwaitReady).
//
// A member's routes, like its clients, go through a relay this process owns: the member listens
// for routes on a port it picks at each start, and advertises the relay's port, which it holds for
// the test's life, as its route URL. Every member's configuration names every relay's port as a
// route, its own included (nats-server ignores a route to itself, and counts the routes to size
// the JetStream meta group when it bootstraps). So no port is chosen by binding and releasing it,
// which another process could take in between, and a member restarted on a new route port is still
// found where its peers' configurations say. Members do not advertise their own client ports, so a
// client reaches each one only through its relay, and a stopped member's URL refuses as a stopped
// lone server's does.
func StartCluster(t testing.TB, n int) *Cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("a cluster needs at least 2 servers, not %d", n)
	}
	members := make([]atomic.Pointer[Server], n)
	routes := make([]string, n)
	listeners := make([]net.Listener, n)
	for i := range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("hold member %d's route port: %v", i, err)
		}
		listeners[i] = listener
		routes[i] = listener.Addr().String()
		member := &members[i]
		routeRelay := newRelay(listener, func() (string, error) {
			server := member.Load()
			if server == nil {
				return "", errors.New("the NATS server has not started")
			}
			return server.clusterAddress()
		})
		t.Cleanup(routeRelay.Close)
	}
	cluster := &Cluster{Servers: make([]*Server, n)}
	for i := range n {
		server := start(t, clusterConfig(i, routes), []string{"-js"})
		server.name = memberName(i)
		members[i].Store(server)
		cluster.Servers[i] = server
	}
	cluster.AwaitReady(t)
	return cluster
}

// clusterConfig is the configuration of member i of a cluster whose route relays listen at routes.
func clusterConfig(i int, routes []string) string {
	urls := make([]string, len(routes))
	for j, route := range routes {
		urls[j] = fmt.Sprintf("%q", "nats-route://"+route)
	}
	return fmt.Sprintf(`server_name: %q
jetstream {}
cluster {
  name: "envoy-test"
  listen: "127.0.0.1:-1"
  advertise: %q
  no_advertise: true
  routes: [ %s ]
}
`, memberName(i), routes[i], strings.Join(urls, ", "))
}

// memberName is member i's server_name.
func memberName(i int) string { return fmt.Sprintf("n%d", i+1) }

// Name is member i's server_name (Server.Name).
func (c *Cluster) Name(i int) string { return c.Servers[i].Name() }

// Member is the index of the member whose server_name is name, or -1 when none has it.
func (c *Cluster) Member(name string) int {
	for i := range c.Servers {
		if c.Name(i) == name {
			return i
		}
	}
	return -1
}

// URLs are the members' client URLs, each the same for the test's life.
func (c *Cluster) URLs() []string {
	urls := make([]string, len(c.Servers))
	for i, server := range c.Servers {
		urls[i] = server.URL()
	}
	return urls
}

// URL is every member's client URL in one, comma-separated, as nats.go takes a server list: a
// client connects to whichever member answers and reconnects to another.
func (c *Cluster) URL() string { return strings.Join(c.URLs(), ",") }

// AwaitReady waits, within connectTimeout, until one running member leads the JetStream meta group,
// that leader counts every running member, and no stopped one, as an online peer, every running
// member is current with it, and JetStream answers an account info request on every running
// member. A test calls it after it starts a stopped member again, or after it stops one, before it
// asks JetStream for anything that needs the meta group.
func (c *Cluster) AwaitReady(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = c.metaReady(); err == nil {
			break
		}
		time.Sleep(retryInterval)
	}
	if err != nil {
		t.Fatalf("the JetStream cluster was not ready within %s: %v", connectTimeout, err)
	}
	for _, server := range c.Servers {
		if server.running() {
			Connect(t, server.URL()).Close()
		}
	}
}

// metaReady reports why the meta group is not ready yet, or nil when it is.
func (c *Cluster) metaReady() error {
	leaders, running := 0, 0
	var online []string
	for i, server := range c.Servers {
		srv := server.natsServer()
		if srv == nil {
			continue
		}
		running++
		if !srv.JetStreamIsCurrent() {
			return fmt.Errorf("member %s is not current with the meta group", c.Name(i))
		}
		if srv.JetStreamIsLeader() {
			leaders++
			online = srv.JetStreamClusterPeers()
		}
	}
	if leaders != 1 {
		return fmt.Errorf("%d running members lead the meta group, want 1", leaders)
	}
	if len(online) != running {
		return fmt.Errorf("the meta leader counts %d members online (%v), want the %d running", len(online), online, running)
	}
	return nil
}
