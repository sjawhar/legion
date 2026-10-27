package main

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The Dispatch server's starts of the stream here go through dispatchStreamOptions over what
// resolveBootConfig read from the settings table, as main's do, on an in-process three-server
// cluster (testnats.StartCluster), one per test or subtest, unless a test says single server.

const (
	// replicasTestMaxBytes is a byte limit every member of a test cluster can reserve.
	replicasTestMaxBytes = 8 << 20
	// replicasClusterWait bounds every wait for the cluster to settle: a leader, a peer catching
	// up.
	replicasClusterWait = 30 * time.Second
	// replicasTestClientID is the GitHub App the tests' webhook sweeper signs as.
	replicasTestClientID = "Iv1.replicas"
)

// startDispatchStream is the Dispatch server's publisher connect for the settings overrides
// names, as main makes it on the servers at urls: it reconciles the stream (dispatchStreamOptions).
func startDispatchStream(t *testing.T, urls []string, overrides map[string]string) (*bus.Client, error) {
	t.Helper()
	env := bootEnvironment(overrides)
	boot, err := resolveBootConfig(env)
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	client, err := bus.ConnectOwningStream(urls, dispatchStreamOptions(boot, env)...)
	if err == nil {
		t.Cleanup(client.Close)
	}
	return client, err
}

// startDispatchNATS is all the Dispatch server's boot writes to NATS for overrides when it holds
// an App key: the publisher's connect (startDispatchStream) and the webhook sweeper's state bucket
// (webhookSweeper), opened at the settings' replica count.
func startDispatchNATS(t *testing.T, urls []string, overrides map[string]string) error {
	t.Helper()
	client, err := startDispatchStream(t, urls, overrides)
	if err != nil {
		return err
	}
	_, pemText := githubapptest.Key(t)
	app := &auth.AppConfig{ClientID: replicasTestClientID, PEM: pemText}
	_, err = webhookSweeper(client, app, "", resolveWith(t, overrides).NATSReplicas)
	return err
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

// On an existing three-copy stream with a byte limit and an id, a Dispatch start whose stream
// differs in its subjects, its age or its duplicate window keeps the three copies, the byte limit,
// DiscardOld and the id, and corrects the subjects, the age and the window: with the deployment's
// settings (ENVOY_NATS_REPLICAS=3 and the byte limit), and with both settings unset, a replica
// count of 1 and no byte limit, which neither lowers the copies nor drops the limit.
func TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	for _, drift := range []struct {
		name   string
		change func(*natsgo.StreamConfig)
	}{
		{"a changed subject list", func(c *natsgo.StreamConfig) { c.Subjects = c.Subjects[:len(c.Subjects)-1] }},
		{"a one-minute age", func(c *natsgo.StreamConfig) { c.MaxAge, c.Duplicates = time.Minute, time.Minute }},
		{"no duplicate window", func(c *natsgo.StreamConfig) { c.Duplicates = 0 }},
	} {
		for _, settings := range []struct {
			name      string
			overrides map[string]string
		}{
			{"the deployment's settings", map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(replicasTestMaxBytes)}},
			{"the settings unset", nil},
		} {
			t.Run(drift.name+"/"+settings.name, func(t *testing.T) {
				cluster := testnats.StartCluster(t, 3)
				js := clusterJetStream(t, cluster)
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
				drift.change(&deployed)
				if _, err := js.AddStream(&deployed); err != nil {
					t.Fatalf("deploy the stream: %v", err)
				}
				before := awaitSettledStream(t, js, bus.Stream).Config

				if _, err := startDispatchStream(t, cluster.URLs(), settings.overrides); err != nil {
					t.Fatalf("start Dispatch's stream: %v", err)
				}

				after := awaitSettledStream(t, js, bus.Stream)
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
			})
		}
	}
}

// `redeliver-webhooks` with ENVOY_NATS_REPLICAS=3 raises the R1 webhook redelivery bucket an
// earlier sweep at the default created to three copies, as the server's sweeper does.
func TestRedeliverWebhooksRaisesAnR1RedeliveryBucketToTheReplicaSetting(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	js := clusterJetStream(t, cluster)
	if _, err := redeliver.OpenState(js, 1); err != nil {
		t.Fatalf("create the R1 redelivery bucket: %v", err)
	}
	stream := "KV_" + bus.WebhookRedeliveryBucket
	if info := awaitSettledStream(t, js, stream); streamCopies(info) != 1 {
		t.Fatalf("the redelivery bucket has %d copies, want 1", streamCopies(info))
	}
	key, pemText := githubapptest.Key(t)
	github := githubapptest.NewWebhook(t, &key.PublicKey, replicasTestClientID, nil)

	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"redeliver-webhooks", "--since", "1h", "--dry-run"}, envGetter(map[string]string{
		"NATS_URLS":                cluster.URL(),
		"XDG_CONFIG_HOME":          t.TempDir(),
		"DISPATCH_APP_CLIENT_ID":   replicasTestClientID,
		"DISPATCH_APP_PEM_B64":     pemText,
		"DISPATCH_GITHUB_API_BASE": github.URL(),
		"ENVOY_NATS_REPLICAS":      "3",
	}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("redeliver-webhooks --dry-run: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if info := awaitSettledStream(t, js, stream); info.Config.Replicas != 3 || streamCopies(info) != 3 {
		t.Fatalf("the redelivery bucket has replicas %d with %d copies, want 3", info.Config.Replicas, streamCopies(info))
	}
}

// On a single server with the default settings, the Dispatch server's start creates the stream and
// the webhook redelivery bucket at one copy, and its next start creates and updates nothing.
func TestADefaultDispatchStartOnASingleServerCreatesEverythingAtOneCopyAndTheNextWritesNothing(t *testing.T) {
	_, url := testnats.Start(t)
	writes := watchStreamWrites(t, url)

	if err := startDispatchNATS(t, []string{url}, nil); err != nil {
		t.Fatalf("first start: %v", err)
	}
	streams := []string{bus.Stream, "KV_" + bus.WebhookRedeliveryBucket}
	var created []string
	for _, stream := range streams {
		created = append(created, "$JS.API.STREAM.CREATE."+stream)
	}
	if got := writes.take(t); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(created))) {
		t.Fatalf("the first start wrote %v, want a create of each of %v", got, streams)
	}
	conn := testnats.Connect(t, url)
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

	if err := startDispatchNATS(t, []string{url}, nil); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := writes.take(t); len(got) != 0 {
		t.Fatalf("the second start wrote %v, want nothing", got)
	}
}

// streamWrites records every stream create and update a server's clients ask for: a request on
// its $JS.API.STREAM.CREATE or $JS.API.STREAM.UPDATE subject, a bucket's stream (KV_<bucket>)
// among them, reaches this subscriber as it reaches the server.
type streamWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// watchStreamWrites starts recording the stream writes asked of the server at url.
func watchStreamWrites(t *testing.T, url string) *streamWrites {
	t.Helper()
	conn := testnats.Connect(t, url)
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
