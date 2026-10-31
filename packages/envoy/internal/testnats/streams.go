package testnats

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// jetStreamWait bounds each JetStream request of a context Cluster.JetStream opens.
const jetStreamWait = 10 * time.Second

// JetStream is a JetStream context, its requests bounded by jetStreamWait, on whichever running
// member of the cluster answers (Connect), closed when the test ends.
func (c *Cluster) JetStream(t testing.TB) natsgo.JetStreamContext {
	t.Helper()
	conn := Connect(t, c.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(jetStreamWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// Copies is how many copies of a stream or a consumer a cluster holds, its leader's and its
// replicas', as its info's Cluster reports them: 0 for none reported.
func Copies(cluster *natsgo.ClusterInfo) int {
	if cluster == nil {
		return 0
	}
	return 1 + len(cluster.Replicas)
}

// settled reports whether cluster shows n settled copies: a leader and n-1 replicas, every one
// current with the leader and none offline. Every wait below settles on it.
func settled(cluster *natsgo.ClusterInfo, n int) bool {
	return cluster != nil && cluster.Leader != "" && len(cluster.Replicas) == n-1 &&
		!slices.ContainsFunc(cluster.Replicas, func(peer *natsgo.PeerInfo) bool { return !peer.Current || peer.Offline })
}

// AwaitSettledStream waits, within connectTimeout, until stream holds as many settled copies as
// its configuration asks (settled), and returns its info. A test that asserts the copies a write
// left reads them from the info it returns.
func AwaitSettledStream(t testing.TB, js natsgo.JetStreamContext, stream string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s did not settle", stream),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool { return settled(info.Cluster, info.Config.Replicas) })
}

// AwaitStreamCopies waits, within connectTimeout, until stream is configured with n copies and
// holds n settled ones (settled), and returns its info.
func AwaitStreamCopies(t testing.TB, js natsgo.JetStreamContext, stream string, n int) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s did not hold %d settled copies", stream, n),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool { return info.Config.Replicas == n && settled(info.Cluster, n) })
}

// AwaitConsumerCopies waits, within connectTimeout, until stream's consumer holds n settled copies
// (settled), as a durable that sets no replica count of its own does once it has followed its
// stream to n, and returns its info.
func AwaitConsumerCopies(t testing.TB, js natsgo.JetStreamContext, stream, consumer string, n int) *natsgo.ConsumerInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("consumer %s of %s did not hold %d settled copies", consumer, stream, n),
		func() (*natsgo.ConsumerInfo, error) { return js.ConsumerInfo(stream, consumer) },
		func(info *natsgo.ConsumerInfo) bool { return settled(info.Cluster, n) })
}

// AwaitStreamLeader waits, within connectTimeout, until stream has a leader other than not, the
// server_name of a member the test stopped or "" for any, and returns its info.
func AwaitStreamLeader(t testing.TB, js natsgo.JetStreamContext, stream, not string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s had no leader other than %q", stream, not),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool {
			return info.Cluster != nil && info.Cluster.Leader != "" && info.Cluster.Leader != not
		})
}

// awaitInfo reads an info every retryInterval until a read succeeds and done holds for it, and
// returns that info; after connectTimeout it fails the test with failure, the last info and the
// last error.
func awaitInfo[T any](t testing.TB, failure string, read func() (*T, error), done func(*T) bool) *T {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	for {
		info, err := read()
		if err == nil && done(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s: %+v (%v)", failure, connectTimeout, info, err)
		}
		time.Sleep(retryInterval)
	}
}

// StreamWrites records every stream create and update a JetStream account's clients ask for: a
// request on the account's $JS.API.STREAM.CREATE or $JS.API.STREAM.UPDATE subject, a bucket's
// stream (KV_<bucket>) among them, reaches its subscriber as it reaches the server.
type StreamWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// WatchStreamWrites starts recording the stream writes asked of the account uri connects to, a
// lone server's (Start) or a test's own (URL), until the test ends.
func WatchStreamWrites(t testing.TB, uri string) *StreamWrites {
	t.Helper()
	conn := Connect(t, uri)
	t.Cleanup(conn.Close)
	sub, err := conn.SubscribeSync("$JS.API.STREAM.*.*")
	if err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	return &StreamWrites{conn: conn, sub: sub}
}

// Take returns the subjects of the creates and updates asked since the last Take. Each request
// reached the subscription before the server answered it, so the flush, a round trip after the
// writer's answer, leaves every one queued here.
func (w *StreamWrites) Take(t testing.TB) []string {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the stream-write watch: %v", err)
	}
	var subjects []string
	for {
		msg, err := w.sub.NextMsg(50 * time.Millisecond)
		if err != nil {
			return subjects
		}
		if strings.HasPrefix(msg.Subject, "$JS.API.STREAM.CREATE.") || strings.HasPrefix(msg.Subject, "$JS.API.STREAM.UPDATE.") {
			subjects = append(subjects, msg.Subject)
		}
	}
}
