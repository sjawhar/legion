package main

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's starts of the stream here go through listenerStreamOptions over what config.Load
// read, as main's do, on an in-process three-server cluster (testnats.StartCluster), one per test
// or subtest, unless a test says single server.

const (
	// replicasTestMaxBytes is a byte limit every member of a test cluster can reserve.
	replicasTestMaxBytes = 8 << 20
	// replicasClusterWait bounds every wait for the cluster to settle: a leader, a peer catching
	// up, a consumer moved to the stream's new peers.
	replicasClusterWait = 30 * time.Second
)

// listenerSettings is the configuration config.Load reads with NATS_URLS urls and ENVOY_NATS_REPLICAS
// and ENVOY_STREAM_MAX_BYTES as values names them: a setting values lacks is unset.
func listenerSettings(t *testing.T, urls string, values map[string]string) config.Service {
	t.Helper()
	t.Setenv("ENVOY_MACHINE_ID", "replicas-test")
	t.Setenv("NATS_URLS", urls)
	for _, name := range []string{bus.ReplicasEnvVar, bus.StreamMaxBytesEnvVar} {
		// t.Setenv restores the variable after the test; Unsetenv then leaves it unset for this one.
		t.Setenv(name, "")
		if value, ok := values[name]; ok {
			t.Setenv(name, value)
		} else if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	cfg, err := config.Load(9020)
	if err != nil {
		t.Fatalf("load the listener's settings: %v", err)
	}
	return cfg
}

// startListenerStream is a listener start's connect, which reconciles the stream as main's does.
func startListenerStream(t *testing.T, cfg config.Service) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream(cfg.NATSURLs, listenerStreamOptions(cfg)...)
	if err != nil {
		t.Fatalf("start the listener's stream with %d replicas and byte limit %d: %v", cfg.NATSReplicas, cfg.StreamMaxBytes, err)
	}
	t.Cleanup(client.Close)
	return client
}

// clusterJetStream is a JetStream context on whichever member of cluster answers.
func clusterJetStream(t *testing.T, cluster *testnats.Cluster) natsgo.JetStreamContext {
	t.Helper()
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// streamCopies is how many copies of a stream the cluster holds, its leader's and its replicas'.
func streamCopies(info *natsgo.StreamInfo) int {
	if info.Cluster == nil {
		return 0
	}
	return 1 + len(info.Cluster.Replicas)
}

// awaitSettledStream waits until stream has a leader and as many current copies as its
// configuration asks, and returns its info.
func awaitSettledStream(t *testing.T, js natsgo.JetStreamContext, stream string) *natsgo.StreamInfo {
	t.Helper()
	deadline := time.Now().Add(replicasClusterWait)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && streamCopies(info) == info.Config.Replicas &&
			!slices.ContainsFunc(info.Cluster.Replicas, func(peer *natsgo.PeerInfo) bool { return !peer.Current || peer.Offline }) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %s did not settle within %s: %+v (%v)", stream, replicasClusterWait, info, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// deployedThreeCopyStream is the stream as a three-copy deployment with a byte limit and an id
// holds it, changed by change.
func deployedThreeCopyStream(change func(*natsgo.StreamConfig)) natsgo.StreamConfig {
	deployed := natsgo.StreamConfig{
		Name:       bus.Stream,
		Subjects:   bus.StreamSubjects(),
		Retention:  natsgo.LimitsPolicy,
		MaxAge:     contracts.DeliveryDuplicateWindow,
		Duplicates: contracts.DeliveryDuplicateWindow,
		Discard:    natsgo.DiscardOld,
		Storage:    natsgo.FileStorage,
		Replicas:   3,
		MaxBytes:   replicasTestMaxBytes,
		Metadata:   map[string]string{bus.StreamIDMetadataKey: "deployed-id"},
	}
	change(&deployed)
	return deployed
}

// On an existing three-copy stream with a byte limit and an id, a listener start whose settings
// are the deployment's (ENVOY_NATS_REPLICAS=3 and the byte limit) and whose stream differs in its
// subjects, its age or its duplicate window keeps the three copies, the byte limit, DiscardOld and
// the id, and corrects the subjects, the age and the window.
func TestAListenerStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	for _, drift := range []struct {
		name   string
		change func(*natsgo.StreamConfig)
	}{
		{"a changed subject list", func(c *natsgo.StreamConfig) { c.Subjects = c.Subjects[:len(c.Subjects)-1] }},
		{"a one-minute age", func(c *natsgo.StreamConfig) { c.MaxAge, c.Duplicates = time.Minute, time.Minute }},
		{"no duplicate window", func(c *natsgo.StreamConfig) { c.Duplicates = 0 }},
	} {
		t.Run(drift.name, func(t *testing.T) {
			cluster := testnats.StartCluster(t, 3)
			js := clusterJetStream(t, cluster)
			deployed := deployedThreeCopyStream(drift.change)
			if _, err := js.AddStream(&deployed); err != nil {
				t.Fatalf("deploy the stream: %v", err)
			}
			before := awaitSettledStream(t, js, bus.Stream).Config

			startListenerStream(t, listenerSettings(t, cluster.URL(), map[string]string{
				bus.ReplicasEnvVar:       "3",
				bus.StreamMaxBytesEnvVar: strconv.Itoa(replicasTestMaxBytes),
			}))

			requireKeptThreeCopyStream(t, before, awaitSettledStream(t, js, bus.Stream))
		})
	}
}

// requireKeptThreeCopyStream fails the test unless after, the stream after a start, holds three
// copies, the byte limit with DiscardOld and before's metadata, its id among it, with every
// subject this binary compiles in and its age and duplicate window corrected to the binary's.
func requireKeptThreeCopyStream(t *testing.T, before natsgo.StreamConfig, after *natsgo.StreamInfo) {
	t.Helper()
	got := after.Config
	if got.Replicas != 3 || streamCopies(after) != 3 {
		t.Errorf("stream replicas = %d with %d copies, want 3", got.Replicas, streamCopies(after))
	}
	if got.MaxBytes != replicasTestMaxBytes || got.Discard != natsgo.DiscardOld {
		t.Errorf("stream byte limit = %d discarding %v, want %d discarding old", got.MaxBytes, got.Discard, replicasTestMaxBytes)
	}
	if !maps.Equal(got.Metadata, before.Metadata) || got.Metadata[bus.StreamIDMetadataKey] != "deployed-id" {
		t.Errorf("stream metadata = %v, want the deployed %v", got.Metadata, before.Metadata)
	}
	if got.MaxAge != contracts.DeliveryDuplicateWindow || got.Duplicates != contracts.DeliveryDuplicateWindow {
		t.Errorf("stream age = %s and duplicate window = %s, want %s", got.MaxAge, got.Duplicates, contracts.DeliveryDuplicateWindow)
	}
	for _, subject := range bus.StreamSubjects() {
		if !slices.Contains(got.Subjects, subject) {
			t.Errorf("stream subjects = %v, want %s among them", got.Subjects, subject)
		}
	}
}

// A listener start with ENVOY_NATS_REPLICAS=3 raises the R1 stream an earlier start at the default
// created, and the durable the listener created and bound on it (startListenerSubscription), which
// sets no replica count of its own, moves to three copies with it.
func TestRaisingTheStreamMovesTheListenersDurableToThreeCopies(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	js := clusterJetStream(t, cluster)
	first := listenerSettings(t, cluster.URL(), nil)
	client := startListenerStream(t, first)
	durable := bus.ListenerDurable(first.MachineID)
	if _, err := startListenerSubscription(client, durable, func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("create and bind the listener's durable: %v", err)
	}
	if info := awaitSettledStream(t, js, bus.Stream); info.Config.Replicas != 1 || streamCopies(info) != 1 {
		t.Fatalf("the default start's stream has replicas %d with %d copies, want 1", info.Config.Replicas, streamCopies(info))
	}

	startListenerStream(t, listenerSettings(t, cluster.URL(), map[string]string{bus.ReplicasEnvVar: "3"}))

	if info := awaitSettledStream(t, js, bus.Stream); info.Config.Replicas != 3 || streamCopies(info) != 3 {
		t.Fatalf("stream replicas = %d with %d copies after the raise, want 3", info.Config.Replicas, streamCopies(info))
	}
	deadline := time.Now().Add(replicasClusterWait)
	for {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && 1+len(info.Cluster.Replicas) == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable %s did not reach three copies within %s: %+v (%v)", durable, replicasClusterWait, info, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// On a single server with the default settings, the listener binary creates the stream and its
// four buckets (interests, role claims, sessions, CI state; the GitHub route opens the last) at one
// copy, and its next start creates and updates nothing.
func TestADefaultListenerOnASingleServerCreatesEverythingAtOneCopyAndItsNextStartWritesNothing(t *testing.T) {
	const machineID = "replicas-default"
	uri := testnats.URL(t)
	writes := watchStreamWrites(t, uri)
	binary := cmdtest.Build(t, "envoy-listener")
	start := func() {
		t.Helper()
		listener := startListenerProcess(t, binary, uri, machineID,
			"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET=replicas-secret", "ENVOY_REVIEWER_APP_ID=1")
		listener.waitHealthy(t)
		listener.Terminate(t)
		listener.WaitExit(t, "SIGTERM")
	}

	start()
	streams := []string{bus.Stream, "KV_" + bus.InterestBucket, "KV_" + bus.RoleClaimBucket, "KV_" + bus.SessionBucket, "KV_" + bus.CIStateBucket}
	var created []string
	for _, stream := range streams {
		created = append(created, "$JS.API.STREAM.CREATE."+stream)
	}
	if got := writes.take(t); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(created))) {
		t.Fatalf("the first start wrote %v, want a create of each of %v", got, streams)
	}
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	for _, stream := range streams {
		info, err := js.StreamInfo(stream)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		if info.Config.Replicas != 1 {
			t.Errorf("%s replicas = %d, want 1", stream, info.Config.Replicas)
		}
	}

	start()
	if got := writes.take(t); len(got) != 0 {
		t.Fatalf("the second start wrote %v, want nothing", got)
	}
}

// streamWrites records every stream create and update a JetStream account's clients ask for: a
// request on the account's $JS.API.STREAM.CREATE or $JS.API.STREAM.UPDATE subject, a bucket's
// stream (KV_<bucket>) among them, reaches this subscriber as it reaches the server.
type streamWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// watchStreamWrites starts recording the stream writes asked of the account uri connects to.
func watchStreamWrites(t *testing.T, uri string) *streamWrites {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	sub, err := conn.SubscribeSync("$JS.API.STREAM.*.*")
	if err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	return &streamWrites{conn: conn, sub: sub}
}

// take returns the subjects of the creates and updates asked since the last take. Each request
// reached the subscription before the server answered it, so the flush, a round trip after the
// writer's answer, leaves every one queued here.
func (w *streamWrites) take(t *testing.T) []string {
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
