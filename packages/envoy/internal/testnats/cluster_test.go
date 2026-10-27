package testnats

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// StartClusterConfig gives every member the configuration it is passed: each admits a user its
// authorization holds and refuses one it does not, and its no_auth_user lets the cluster start.
func TestEveryClusterMemberTakesTheConfigItIsGiven(t *testing.T) {
	seed, public := User(t)
	stranger, _ := User(t)
	_, fallback := User(t)
	cluster := StartClusterConfig(t, 3, fmt.Sprintf("authorization {\n  users = [ {nkey: %q}, {nkey: %q} ]\n}\nno_auth_user: %q\n", public, fallback, fallback))
	for i, uri := range cluster.URLs() {
		conn, err := ConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("member %s refused a user its authorization holds: %v", cluster.Name(i), err)
		}
		conn.Close()
		if _, err := ConnectSeed(t, uri, stranger); !errors.Is(err, natsgo.ErrAuthorization) {
			t.Fatalf("member %s answered a user its authorization does not hold with %v, want an authorization violation", cluster.Name(i), err)
		}
	}
}

// A three-copy stream on the cluster keeps a leader and takes a publish with the member that led
// it stopped, and that member, started again on its own store and URLs, rejoins and catches up.
func TestAThreeCopyStreamKeepsALeaderAndTakesAPublishWithOneMemberStopped(t *testing.T) {
	cluster := StartCluster(t, 3)
	conn := connectCluster(t, cluster)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "THREE", Subjects: []string{"three.>"}, Replicas: 3, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create a three-copy stream: %v", err)
	}
	if _, err := js.Publish("three.before", []byte("before")); err != nil {
		t.Fatalf("publish with every member running: %v", err)
	}
	info := awaitStreamLeader(t, js, "THREE", "")
	if got := 1 + len(info.Cluster.Replicas); got != 3 {
		t.Fatalf("stream has %d copies, want 3: %+v", got, info.Cluster)
	}
	leader := cluster.Member(info.Cluster.Leader)
	if leader < 0 {
		t.Fatalf("stream leader %q is no member of the cluster", info.Cluster.Leader)
	}

	cluster.Servers[leader].Stop(t)
	cluster.AwaitReady(t)
	awaitStreamLeader(t, js, "THREE", cluster.Name(leader))
	ack, err := js.Publish("three.after", []byte("after"))
	if err != nil {
		t.Fatalf("publish with member %s stopped: %v", cluster.Name(leader), err)
	}
	if ack.Sequence != 2 {
		t.Fatalf("publish with a member stopped stored sequence %d, want 2", ack.Sequence)
	}

	cluster.Servers[leader].Start(t)
	cluster.AwaitReady(t)
	deadline := time.Now().Add(connectTimeout)
	for {
		info, err = js.StreamInfo("THREE")
		if err == nil && info.Cluster != nil && len(info.Cluster.Replicas) == 2 && !slices.ContainsFunc(info.Cluster.Replicas, func(p *natsgo.PeerInfo) bool { return !p.Current || p.Offline }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted member did not catch up within %s: %+v (%v)", connectTimeout, info, err)
		}
		time.Sleep(retryInterval)
	}
	if info.State.Msgs != 2 || info.State.LastSeq != 2 {
		t.Fatalf("stream state = %+v, want both messages", info.State)
	}
}

// connectCluster connects to every member of cluster in one server list, reconnecting to another
// member when the one it holds stops.
func connectCluster(t *testing.T, cluster *Cluster) *natsgo.Conn {
	t.Helper()
	Connect(t, cluster.URL()).Close()
	conn, err := natsgo.Connect(cluster.URL(), natsgo.Timeout(dialTimeout), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(retryInterval))
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// awaitStreamLeader waits for stream to have a leader other than not, and returns its info.
func awaitStreamLeader(t *testing.T, js natsgo.JetStreamContext, stream, not string) *natsgo.StreamInfo {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && info.Cluster.Leader != not {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %s had no leader other than %q within %s: %+v (%v)", stream, not, connectTimeout, info, err)
		}
		time.Sleep(retryInterval)
	}
}
