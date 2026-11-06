package testnats

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// A cluster member answers a stream lookup out of the JetStream metadata it has applied itself.
// nats-server 2.10.29 routes a KV create to the meta leader, which answers the client as soon as
// the bucket's stream is made, while every member applies the meta entry that carries the new
// assignment on its own clock; until a member has applied it, that member answers "stream not
// found" for the bucket that was just created, the member the create was sent through included.
// Measured here with three in-process members on two CPUs, 67 of 75 member reads taken the moment
// a create was answered still said so, the slowest for 206 ms.
//
// Whatever reaches the bucket next reaches it through whichever member its own connection is on:
// bus.EnsureKeyValue opens the jetstream handle its KeyValue carries, a rebuild calls
// bus.OpenKeyValue, a second program looks the bucket up before it raises it, and a first put
// writes to it. So a test that creates a bucket on a cluster and then has the code under test
// open it, raise it, look it up or write to it creates it with CreateKeyValue, and one whose
// code under test creates the bucket itself calls AwaitBucket after it.

const (
	// bucketCreateWait bounds each JetStream request of a context BucketJetStream opens. A
	// bucket create on a cluster is proposed through the meta group and answered once the
	// bucket's stream is made, the slowest thing these tests ask a JetStream client for: 25
	// creates on an idle two-CPU machine took from 60 ms to 1.03 s. This is a hang guard, two
	// orders of magnitude away from that, not a budget a loaded runner can use up.
	bucketCreateWait = 2 * time.Minute

	// bucketLookupWait bounds one member's lookup of a bucket's stream, which that member
	// answers out of metadata it holds. A hang guard on the same footing.
	bucketLookupWait = 10 * time.Second
)

// BucketJetStream is a JetStream context on conn whose requests are bounded by bucketCreateWait.
// A test whose code under test creates a bucket on a cluster opens its context here rather than
// with Cluster.JetStream, whose ten seconds one create can use up on a loaded machine.
func BucketJetStream(t testing.TB, conn *natsgo.Conn) natsgo.JetStreamContext {
	t.Helper()
	js, err := conn.JetStream(natsgo.MaxWait(bucketCreateWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// CreateKeyValue creates the KV bucket config names on conn, within bucketCreateWait, and returns
// its handle once every running member of the cluster answers for the bucket's stream
// (AwaitBucket). conn is the connection whose grant the create is made under, so a test of a
// client's permissions creates the bucket as that client.
func (c *Cluster) CreateKeyValue(t testing.TB, conn *natsgo.Conn, config *natsgo.KeyValueConfig) natsgo.KeyValue {
	t.Helper()
	kv, err := BucketJetStream(t, conn).CreateKeyValue(config)
	if err != nil {
		t.Fatalf("create KV bucket %s: %v", config.Bucket, err)
	}
	c.AwaitBucket(t, config.Bucket)
	return kv
}

// AwaitBucket waits, within connectTimeout, until every running member of the cluster both
// answers for the stream of bucket and holds the interest a write to the bucket is routed by. A
// member answers a lookup out of the metadata it has applied, and routes a write by the
// subscription the bucket's stream holds on its subjects, which reaches a member that does not
// host the stream over the cluster's routes; the two arrive on their own clocks, and a write
// through a member that has neither yet is answered "no responders", which nats.go gives a
// caller as `nats: no response from stream`.
func (c *Cluster) AwaitBucket(t testing.TB, bucket string) {
	t.Helper()
	stream := "KV_" + bucket
	// A subject of the bucket, which the stream's own `$KV.<bucket>.>` subscription matches: the
	// interest a put, a delete or a watch of any key is routed by.
	subject := "$KV." + bucket + ".a"
	for i, server := range c.Servers {
		if !server.running() {
			continue
		}
		c.awaitBucketOn(t, i, server, stream, subject)
	}
}

// awaitBucketOn waits for member i to answer for stream and to hold the interest subject is
// routed by, the member's own account read in this process.
func (c *Cluster) awaitBucketOn(t testing.TB, i int, server *Server, stream, subject string) {
	t.Helper()
	conn := Connect(t, server.URL())
	defer conn.Close()
	js, err := conn.JetStream(natsgo.MaxWait(bucketLookupWait))
	if err != nil {
		t.Fatalf("open JetStream on member %s: %v", c.Name(i), err)
	}
	deadline := time.Now().Add(connectTimeout)
	for {
		_, err := js.StreamInfo(stream)
		routed := false
		if srv := server.natsServer(); srv != nil {
			routed = srv.GlobalAccount().SubscriptionInterest(subject)
		}
		if err == nil && routed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("member %s did not answer for stream %s and route %s within %s: %v (routed %t)", c.Name(i), stream, subject, connectTimeout, err, routed)
		}
		time.Sleep(retryInterval)
	}
}
