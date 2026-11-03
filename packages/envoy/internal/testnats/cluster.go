package testnats

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
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
	return StartClusterConfig(t, n, "")
}

// StartClusterConfig is StartCluster with config added to the end of every member's configuration,
// as an authorization block that every member enforces alike. AwaitReady connects to each member
// with no credential and asks JetStream for its account info, so a config that declares users names
// a no_auth_user whose account has JetStream.
func StartClusterConfig(t testing.TB, n int, config string) *Cluster {
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
		server := start(t, clusterConfig(i, routes)+config, []string{"-js"})
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

// AwaitReady waits until one running member leads the JetStream meta group, that leader counts
// every running member, and no stopped one, as an online peer, every running member is current
// with it, and JetStream answers an account info request on every running member. A test calls it
// after it starts a stopped member again, or after it stops one, before it asks JetStream for
// anything that needs the meta group.
//
// A member that no other running member's meta group hears from within muteAfter is restarted, at
// most muteRestarts times in one call. Each restart starts the listening, and the connectTimeout
// wait, over for every member, since a restarted member has heard no one yet.
//
// nats-server 2.10.29 can start a member whose every Raft message is dropped: its
// setSystemAccount starts the Raft send queue's goroutine (server/server.go:1710), which registers
// its client with s.SystemAccount() (server/sendq.go:48) before setSystemAccount stores the account
// (server/server.go:1717), and a client with no account delivers nothing (server/client.go:3882).
// Such a member follows the leader but never answers it or votes, so the leader never counts it,
// for as long as that start runs; a restart is a fresh start. Under -race on two CPUs with four
// test processes, 29 of 1,920 fresh clusters had such a member. nats-server v2.15.2 hands the
// queue its account instead (newSendQ(acc)).
func (c *Cluster) AwaitReady(t testing.TB) {
	t.Helper()
	// listening is when AwaitReady began listening for the members: its call, or its last restart.
	listening := time.Now()
	restarts := 0
	for {
		err := c.metaReady()
		if err == nil {
			break
		}
		if restarts < muteRestarts {
			if i := c.mute(time.Since(listening)); i >= 0 {
				t.Logf("restart cluster member %s: no other member's JetStream meta group heard from it within %s (%v)", c.Name(i), muteAfter, err)
				c.Servers[i].stop()
				c.Servers[i].launch(t)
				restarts++
				listening = time.Now()
				continue
			}
		}
		if time.Since(listening) >= connectTimeout {
			t.Fatalf("JetStream was not ready within %s (%d members restarted): %v", connectTimeout, restarts, err)
		}
		time.Sleep(retryInterval)
	}
	for _, server := range c.Servers {
		if server.running() {
			Connect(t, server.URL()).Close()
		}
	}
}

const (
	// muteAfter is how long AwaitReady listens for a running member before it restarts it. A
	// working member is heard sooner whether or not the meta group has a leader: a leader hears
	// each follower answer its heartbeat every second, and without a leader every member campaigns
	// within nats-server's longest election timeout, 9 seconds, so the members it asks for a vote
	// hear it and it hears their answers. Under -race on two CPUs, most beside three more such test
	// processes, the 1,925 fresh clusters without a mute member were all ready within 7.8 seconds.
	muteAfter = 15 * time.Second
	// muteRestarts caps the restarts one AwaitReady makes, so a member nothing can hear, such as
	// the last running one of a cluster that lost its quorum, fails the wait instead of being
	// restarted for ever.
	muteRestarts = 3
	// metaGroupName is the Raft group JetStream's meta group runs as.
	metaGroupName = "_meta_"
)

// mute is the index of a running member that no other running member's meta group has heard from
// in the last listened, once that is muteAfter or longer, or -1 when there is none.
func (c *Cluster) mute(listened time.Duration) int {
	if listened < muteAfter {
		return -1
	}
	groups := make([]*natsserver.RaftzGroup, len(c.Servers))
	for i, server := range c.Servers {
		if srv := server.natsServer(); srv != nil {
			groups[i] = metaGroup(srv)
		}
	}
	for i, group := range groups {
		if group != nil && !heard(groups, i, listened) {
			return i
		}
	}
	return -1
}

// heard reports whether a member other than i last heard member i's meta group within the last
// within, by each member's own Raft view of the group (groups, nil for a member without one).
func heard(groups []*natsserver.RaftzGroup, i int, within time.Duration) bool {
	for j, group := range groups {
		if j == i || group == nil {
			continue
		}
		peer, ok := group.Peers[groups[i].ID]
		if !ok || peer.LastSeen == "" {
			continue
		}
		if seen, err := time.ParseDuration(peer.LastSeen); err == nil && seen < within {
			return true
		}
	}
	return false
}

// metaGroup is srv's Raft view of the JetStream meta group, or nil while it has none.
func metaGroup(srv *natsserver.Server) *natsserver.RaftzGroup {
	status := srv.Raftz(&natsserver.RaftzOptions{GroupFilter: metaGroupName})
	if status == nil {
		return nil
	}
	for _, groups := range *status {
		if group, ok := groups[metaGroupName]; ok {
			return &group
		}
	}
	return nil
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
