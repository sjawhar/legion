package integration

import (
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// On a three-copy deployment (an in-process three-server cluster, replica count 3), each of the
// five buckets Envoy opens is created at three copies by the opener its program calls: interests
// and role claims (store.Open), sessions (session.OpenSessionRegistry), CI state (cistore.Open) and
// webhook redelivery (redeliver.OpenState).
func TestEveryBucketIsCreatedAtThreeCopiesOnAThreeCopyDeployment(t *testing.T) {
	const replicas = 3
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}

	registry, err := store.Open(conn, store.WithReplicas(replicas))
	if err != nil {
		t.Fatalf("open the interest and role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn, session.WithSessionReplicas(replicas))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	ci, err := cistore.Open(conn, logging.New("test"), cistore.WithReplicas(replicas))
	if err != nil {
		t.Fatalf("open the CI store: %v", err)
	}
	t.Cleanup(ci.StopWatch)
	if _, err := redeliver.OpenState(js, replicas); err != nil {
		t.Fatalf("open the webhook redelivery state: %v", err)
	}

	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.CIStateBucket, bus.WebhookRedeliveryBucket} {
		stream := "KV_" + bucket
		deadline := time.Now().Add(30 * time.Second)
		for {
			info, err := js.StreamInfo(stream)
			if err == nil && info.Config.Replicas == replicas && info.Cluster != nil && info.Cluster.Leader != "" &&
				1+len(info.Cluster.Replicas) == replicas && !slices.ContainsFunc(info.Cluster.Replicas, func(peer *natsgo.PeerInfo) bool { return !peer.Current || peer.Offline }) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("bucket %s did not hold %d current copies within 30s: %+v (%v)", bucket, replicas, info, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
