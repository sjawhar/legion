package bus

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests here run on an in-process three-server cluster (testnats.StartCluster), one per test
// or subtest, so each has the stream and its buckets to itself; a test that needs a single server
// says so.

const (
	// testStreamMaxBytes is a byte limit every member of a test cluster can reserve.
	testStreamMaxBytes = 8 << 20
	// clusterWait bounds every wait for the cluster to settle: a leader, a peer catching up, a
	// consumer moved to the stream's new peers.
	clusterWait = 30 * time.Second
)

// startClusterJetStream starts a three-server cluster and returns it with a JetStream context
// connected to every member.
func startClusterJetStream(t *testing.T) (*testnats.Cluster, nats.JetStreamContext) {
	t.Helper()
	cluster := testnats.StartCluster(t, 3)
	return cluster, clusterJetStream(t, cluster)
}

// clusterJetStream is a JetStream context connected to whichever running member of cluster answers.
func clusterJetStream(t *testing.T, cluster *testnats.Cluster) nats.JetStreamContext {
	t.Helper()
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// startOnCluster runs a start of a deployed program on cluster: ConnectOwningStream with options.
func startOnCluster(t *testing.T, cluster *testnats.Cluster, options ...ConnectOption) *Client {
	t.Helper()
	client, err := ConnectOwningStream(cluster.URLs(), options...)
	if err != nil {
		t.Fatalf("start with %d options: %v", len(options), err)
	}
	t.Cleanup(client.Close)
	return client
}

// copies is how many copies of the stream the cluster holds, its leader's and its replicas'.
func copies(info *nats.StreamInfo) int {
	if info.Cluster == nil {
		return 0
	}
	return 1 + len(info.Cluster.Replicas)
}

// awaitStream waits until every one of the stream's copies is current with its leader and returns
// its info.
func awaitStream(t *testing.T, js nats.JetStreamContext, stream string) *nats.StreamInfo {
	t.Helper()
	deadline := time.Now().Add(clusterWait)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && copies(info) == info.Config.Replicas &&
			!slices.ContainsFunc(info.Cluster.Replicas, func(peer *nats.PeerInfo) bool { return !peer.Current || peer.Offline }) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %s did not settle within %s: %+v (%v)", stream, clusterWait, info, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// deployedClusterStream is the stream as a deployment with three copies and a byte limit holds it,
// with limits this binary does not own and an id, changed by change.
func deployedClusterStream(change func(*nats.StreamConfig)) nats.StreamConfig {
	deployed := *streamCfg
	deployed.Replicas = 3
	deployed.MaxBytes = testStreamMaxBytes
	deployed.MaxMsgsPerSubject = 1_000
	deployed.MaxMsgSize = 64 << 10
	deployed.Metadata = map[string]string{StreamIDMetadataKey: "deployed-id", "owner": "operations"}
	change(&deployed)
	return deployed
}

// On an existing three-copy stream with a byte limit and an id, a start that changes its subjects,
// its age or its duplicate window keeps the three copies, the byte limit, DiscardOld, the id and
// every limit it does not own (MaxMsgsPerSubject, MaxMsgSize), and corrects the age and window.
// That holds for a start whose settings match the deployment's and for one whose replica setting
// is 1 with no byte limit set: a start never lowers the copies and never drops a deployed limit.
func TestAReconcileOnTheClusterKeepsTheCopiesTheByteLimitTheIDAndTheLimitsItDoesNotOwn(t *testing.T) {
	for _, drift := range []struct {
		name   string
		change func(*nats.StreamConfig)
	}{
		{"subjects", func(c *nats.StreamConfig) {
			c.Subjects = slices.DeleteFunc(slices.Clone(streamCfg.Subjects), func(s string) bool { return s == "notifications.legion.>" })
		}},
		{"a one-minute age", func(c *nats.StreamConfig) { c.MaxAge, c.Duplicates = time.Minute, time.Minute }},
		{"no duplicate window", func(c *nats.StreamConfig) { c.Duplicates = 0 }},
	} {
		for _, start := range []struct {
			name    string
			options []ConnectOption
		}{
			{"setting 3 and the byte limit", []ConnectOption{WithReplicas(3), WithStreamMaxBytes(testStreamMaxBytes)}},
			{"setting 1 and no byte limit", []ConnectOption{WithReplicas(1)}},
		} {
			t.Run(drift.name+"/"+start.name, func(t *testing.T) {
				cluster, js := startClusterJetStream(t)
				deployed := deployedClusterStream(drift.change)
				if _, err := js.AddStream(&deployed); err != nil {
					t.Fatalf("deploy the stream: %v", err)
				}
				before := awaitStream(t, js, Stream).Config

				startOnCluster(t, cluster, start.options...)

				info := awaitStream(t, js, Stream)
				got := info.Config
				if got.Replicas != 3 || copies(info) != 3 {
					t.Errorf("stream replicas = %d with %d copies, want 3", got.Replicas, copies(info))
				}
				if got.MaxBytes != testStreamMaxBytes || got.Discard != nats.DiscardOld {
					t.Errorf("stream byte limit = %d discarding %v, want %d discarding old", got.MaxBytes, got.Discard, testStreamMaxBytes)
				}
				if got.MaxAge != streamDuplicateWindow || got.Duplicates != streamDuplicateWindow {
					t.Errorf("stream age = %s and duplicate window = %s, want %s", got.MaxAge, got.Duplicates, streamDuplicateWindow)
				}
				for _, subject := range streamCfg.Subjects {
					if !slices.Contains(got.Subjects, subject) {
						t.Errorf("stream subjects = %v, want %s among them", got.Subjects, subject)
					}
				}
				if !maps.Equal(got.Metadata, before.Metadata) {
					t.Errorf("stream metadata = %v, want the deployed %v", got.Metadata, before.Metadata)
				}
				if got.MaxMsgsPerSubject != before.MaxMsgsPerSubject || got.MaxMsgSize != before.MaxMsgSize {
					t.Errorf("stream MaxMsgsPerSubject = %d and MaxMsgSize = %d, want the deployed %d and %d", got.MaxMsgsPerSubject, got.MaxMsgSize, before.MaxMsgsPerSubject, before.MaxMsgSize)
				}
			})
		}
	}
}

// A start whose setting is 3 raises an R1 stream to three copies, keeping its messages, its
// metadata and its first and last sequence, and every durable consumer created with no replica
// count of its own (as the listener's and Dispatch's two are) moves to three copies with it.
func TestAStartRaisesAnR1StreamToThreeCopiesKeepingItsMessagesAndMovingItsConsumers(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(1))
	for i := range 5 {
		if _, err := js.Publish(fmt.Sprintf("notifications.agent.s%d", i), []byte(fmt.Sprintf("message %d", i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	durables := []string{ListenerDurable("test-machine"), DispatchDeliveryDurable, DispatchJobDurable}
	for _, durable := range durables {
		if _, err := js.AddConsumer(Stream, &nats.ConsumerConfig{Durable: durable, AckPolicy: nats.AckExplicitPolicy}); err != nil {
			t.Fatalf("create durable %s: %v", durable, err)
		}
	}
	before := awaitStream(t, js, Stream)
	if copies(before) != 1 {
		t.Fatalf("deployed stream has %d copies, want 1", copies(before))
	}

	startOnCluster(t, cluster, WithReplicas(3))

	after := awaitStream(t, js, Stream)
	if after.Config.Replicas != 3 || copies(after) != 3 {
		t.Fatalf("stream replicas = %d with %d copies, want 3", after.Config.Replicas, copies(after))
	}
	if after.State.Msgs != before.State.Msgs || after.State.FirstSeq != before.State.FirstSeq || after.State.LastSeq != before.State.LastSeq {
		t.Fatalf("stream state = %+v after the raise, want the deployed %+v", after.State, before.State)
	}
	if !maps.Equal(after.Config.Metadata, before.Config.Metadata) || after.Config.Metadata[StreamIDMetadataKey] == "" {
		t.Fatalf("stream metadata = %v after the raise, want the deployed %v", after.Config.Metadata, before.Config.Metadata)
	}
	for seq := before.State.FirstSeq; seq <= before.State.LastSeq; seq++ {
		msg, err := js.GetMsg(Stream, seq)
		if err != nil {
			t.Fatalf("read message %d after the raise: %v", seq, err)
		}
		if want := fmt.Sprintf("message %d", seq-1); string(msg.Data) != want {
			t.Fatalf("message %d = %q, want %q", seq, msg.Data, want)
		}
	}
	for _, durable := range durables {
		deadline := time.Now().Add(clusterWait)
		for {
			info, err := js.ConsumerInfo(Stream, durable)
			if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && 1+len(info.Cluster.Replicas) == 3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("durable %s did not reach three copies within %s: %+v (%v)", durable, clusterWait, info, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// EnsureKeyValue raises an existing R1 bucket to the deployment's three copies, and the update
// changes nothing else about the bucket's stream, whatever else the opener's configuration says.
// OpenKeyValue, the rebuild path, raises nothing, and a bucket is never lowered.
func TestEnsureKeyValueRaisesAnR1BucketToThreeCopiesChangingNothingElse(t *testing.T) {
	_, js := startClusterJetStream(t)
	const bucket = "envoy_raise_test"
	created, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:      bucket,
		Description: "deployed by operations",
		History:     5,
		TTL:         time.Hour,
		MaxBytes:    1 << 20,
		Storage:     nats.FileStorage,
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("create the R1 bucket: %v", err)
	}
	if _, err := created.Put("key", []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	before := awaitStream(t, js, "KV_"+bucket)

	counting := &countingJetStream{JetStreamContext: js}
	if _, err := OpenKeyValue(counting, bucket); err != nil {
		t.Fatalf("open the bucket for a rebuild: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("OpenKeyValue wrote %v, want nothing", writes)
	}

	kv, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, TTL: 7 * 24 * time.Hour, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("ensure the bucket at three copies: %v", err)
	}

	after := awaitStream(t, js, "KV_"+bucket)
	if copies(after) != 3 {
		t.Fatalf("bucket stream has %d copies, want 3", copies(after))
	}
	want := before.Config
	want.Replicas = 3
	if !reflect.DeepEqual(after.Config, want) {
		t.Fatalf("bucket stream config = %+v after the raise, want the deployed one with only replicas 3: %+v", after.Config, want)
	}
	entry, err := kv.Get("key")
	if err != nil || string(entry.Value()) != "value" {
		t.Fatalf("get after the raise = %v, %v; want value", entry, err)
	}

	if _, err := EnsureKeyValue(counting, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("ensure the bucket at one copy: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("a start at one copy wrote %v to a three-copy bucket, want nothing", writes)
	}
	if got := awaitStream(t, js, "KV_"+bucket).Config.Replicas; got != 3 {
		t.Fatalf("bucket replicas = %d after a start at one copy, want 3", got)
	}
}

// A stream created with a byte limit has it and discards its oldest messages first; a later start
// that sets no limit keeps the deployed one, and one that sets another writes that.
func TestTheStreamTakesTheDeploymentsByteLimitAndKeepsADeployedOne(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(testStreamMaxBytes))
	info := awaitStream(t, js, Stream)
	if info.Config.MaxBytes != testStreamMaxBytes || info.Config.Discard != nats.DiscardOld || copies(info) != 3 {
		t.Fatalf("created stream byte limit = %d discarding %v with %d copies, want %d discarding old with 3", info.Config.MaxBytes, info.Config.Discard, copies(info), testStreamMaxBytes)
	}

	startOnCluster(t, cluster, WithReplicas(3))
	if got := awaitStream(t, js, Stream).Config.MaxBytes; got != testStreamMaxBytes {
		t.Fatalf("stream byte limit = %d after a start with none set, want the deployed %d", got, testStreamMaxBytes)
	}

	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(2*testStreamMaxBytes))
	if got := awaitStream(t, js, Stream).Config.MaxBytes; got != 2*testStreamMaxBytes {
		t.Fatalf("stream byte limit = %d after a start setting %d, want that", got, 2*testStreamMaxBytes)
	}
}

// Publishing past the byte limit drops the oldest messages first, and every publish is still
// taken (single server, small limit).
func TestAFullStreamDropsItsOldestMessagesFirst(t *testing.T) {
	const limit = 16 << 10
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithStreamMaxBytes(limit))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	payload := []byte(strings.Repeat("x", 1<<10))
	const published = 64
	for i := range published {
		if _, err := js.Publish(fmt.Sprintf("notifications.agent.s%d", i), payload); err != nil {
			t.Fatalf("publish %d past the limit: %v", i, err)
		}
	}
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Bytes > limit || info.State.LastSeq != published || info.State.FirstSeq <= 1 || info.State.Msgs != published-info.State.FirstSeq+1 {
		t.Fatalf("stream state = %+v, want the newest messages within %d bytes, through sequence %d", info.State, limit, published)
	}
	if _, err := js.GetMsg(Stream, 1); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("read the first message = %v, want it dropped", err)
	}
}

// A raise while a publisher sends loses nothing the stream acknowledged: every publish is either
// acknowledged and then stored at its sequence, or answered with an error.
func TestRaisingTheStreamWhileAPublisherSendsKeepsEveryAcknowledgedMessage(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(1))

	acked := map[uint64]string{}
	var failed int
	raised := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		after := 0
		for i := 0; after < 50; i++ {
			select {
			case <-raised:
				after++
			default:
			}
			data := fmt.Sprintf("message %d", i)
			ack, err := js.Publish("notifications.agent.publisher", []byte(data), nats.MsgId(data))
			if err != nil {
				failed++
				continue
			}
			acked[ack.Sequence] = data
		}
	}()
	startOnCluster(t, cluster, WithReplicas(3))
	close(raised)
	<-done

	awaitStream(t, js, Stream)
	if len(acked) == 0 {
		t.Fatalf("no publish was acknowledged (%d failed)", failed)
	}
	for seq, data := range acked {
		msg, err := js.GetMsg(Stream, seq)
		if err != nil {
			t.Fatalf("read acknowledged message %d (%q): %v", seq, data, err)
		}
		if string(msg.Data) != data {
			t.Fatalf("message %d = %q, want the acknowledged %q", seq, msg.Data, data)
		}
	}
	t.Logf("%d publishes acknowledged and stored, %d answered with an error", len(acked), failed)
}

// Two starts raising the same R1 stream and the same R1 bucket at once both succeed and leave
// three copies of each.
func TestTwoStartsRaisingOneStreamAtOnceBothSucceed(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(1))
	const bucket = "envoy_race_test"
	if _, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the R1 bucket: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := ConnectOwningStream(cluster.URLs(), WithReplicas(3))
			if err != nil {
				errs[i] = err
				return
			}
			defer client.Close()
			_, errs[i] = EnsureKeyValue(client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("start %d: %v", i, err)
		}
	}
	for _, stream := range []string{Stream, "KV_" + bucket} {
		if info := awaitStream(t, js, stream); info.Config.Replicas != 3 || copies(info) != 3 {
			t.Fatalf("%s replicas = %d with %d copies, want 3", stream, info.Config.Replicas, copies(info))
		}
	}
}

// A raise the cluster refuses, more copies than it has servers, fails the start naming the replica
// setting, for the stream and for a bucket, and leaves both as they were.
func TestARefusedRaiseNamesTheReplicaSetting(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(1))
	const bucket = "envoy_refused_test"
	if _, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the R1 bucket: %v", err)
	}

	client, err := ConnectOwningStream(cluster.URLs(), WithReplicas(5))
	if err == nil {
		client.Close()
		t.Fatal("a start raised the stream to five copies on three servers")
	}
	var apiErr *nats.APIError
	if !strings.HasPrefix(err.Error(), ReplicasEnvVar+"=5: ") || !errors.As(err, &apiErr) {
		t.Fatalf("start error = %q, want the server's refusal naming %s=5", err, ReplicasEnvVar)
	}
	_, err = EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 5})
	if err == nil || !strings.HasPrefix(err.Error(), ReplicasEnvVar+"=5: ") || !errors.As(err, &apiErr) {
		t.Fatalf("raise the bucket to five copies = %v, want the server's refusal naming %s=5", err, ReplicasEnvVar)
	}
	for _, stream := range []string{Stream, "KV_" + bucket} {
		if info := awaitStream(t, js, stream); info.Config.Replicas != 1 {
			t.Fatalf("%s replicas = %d after a refused raise, want 1", stream, info.Config.Replicas)
		}
	}
}

// With one server of three stopped and the stream and a bucket already at three copies, a start
// succeeds and writes nothing to either; a create at three copies with one server stopped fails,
// naming the setting.
func TestOneServerStoppedAStartOfAThreeCopyDeploymentWritesNothing(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(testStreamMaxBytes))
	const bucket = "envoy_stopped_test"
	if _, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	awaitStream(t, js, Stream)
	awaitStream(t, js, "KV_"+bucket)

	cluster.Servers[2].Stop(t)
	cluster.AwaitReady(t)
	js = clusterJetStream(t, cluster)
	awaitLeader(t, js, Stream, cluster.Name(2))
	awaitLeader(t, js, "KV_"+bucket, cluster.Name(2))

	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(testStreamMaxBytes))
	startOnCluster(t, cluster, WithReplicas(3))
	counting := &countingJetStream{JetStreamContext: js}
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(3), WithStreamMaxBytes(testStreamMaxBytes)}).streamConfig()
	if err := ensureStreamWithConfig(counting, &cfg); err != nil {
		t.Fatalf("ensure the stream with one server stopped: %v", err)
	}
	if _, err := EnsureKeyValue(counting, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3}); err != nil {
		t.Fatalf("ensure the bucket with one server stopped: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("a start with one server stopped wrote %v, want nothing", writes)
	}

	_, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: "envoy_new_test", Storage: nats.FileStorage, Replicas: 3})
	if err == nil || !strings.HasPrefix(err.Error(), ReplicasEnvVar+"=3: ") {
		t.Fatalf("create a three-copy bucket with one server stopped = %v, want a refusal naming %s=3", err, ReplicasEnvVar)
	}
}

// A create of the stream at three copies with one server of three stopped fails the start, and the
// error names the replica setting.
func TestOneServerStoppedACreateAtThreeCopiesFailsNamingTheSetting(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.Servers[2].Stop(t)
	cluster.AwaitReady(t)
	client, err := ConnectOwningStream(cluster.URLs(), WithReplicas(3))
	if err == nil {
		client.Close()
		t.Fatal("a start created the stream at three copies with one server of three stopped")
	}
	if !strings.HasPrefix(err.Error(), ReplicasEnvVar+"=3: ") {
		t.Fatalf("start error = %q, want it to name %s=3", err, ReplicasEnvVar)
	}
}

// A server's refusal of the stream names each setting the refused create or update carried, with
// its value, and wraps the server's error (single server). A single nats-server v2.10.29 refuses a
// create at more than one copy but takes an update to more, recording the count while it holds one
// copy, so a refused raise is TestARefusedRaiseNamesTheReplicaSetting's, on a cluster.
func TestARefusedStreamNamesTheSettingsItCarried(t *testing.T) {
	const tooLarge = int64(1) << 60
	for _, row := range []struct {
		name    string
		deploy  bool // a start at one copy and no byte limit creates the stream first
		options []ConnectOption
		want    string
	}{
		{"a create at three copies", false, []ConnectOption{WithReplicas(3)}, ReplicasEnvVar + "=3: "},
		{"a create with a byte limit the server cannot reserve", false, []ConnectOption{WithStreamMaxBytes(tooLarge)}, fmt.Sprintf("%s=%d: ", StreamMaxBytesEnvVar, tooLarge)},
		{"a create with both", false, []ConnectOption{WithReplicas(3), WithStreamMaxBytes(tooLarge)}, fmt.Sprintf("%s=3, %s=%d: ", ReplicasEnvVar, StreamMaxBytesEnvVar, tooLarge)},
		{"an update to a byte limit the server cannot reserve", true, []ConnectOption{WithStreamMaxBytes(tooLarge)}, fmt.Sprintf("%s=%d: ", StreamMaxBytesEnvVar, tooLarge)},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			if row.deploy {
				client, err := ConnectOwningStream([]string{uri})
				if err != nil {
					t.Fatalf("deploy the stream: %v", err)
				}
				client.Close()
			}
			client, err := ConnectOwningStream([]string{uri}, row.options...)
			if err == nil {
				client.Close()
				t.Fatal("the server took the stream")
			}
			var apiErr *nats.APIError
			if !strings.HasPrefix(err.Error(), row.want) || !errors.As(err, &apiErr) {
				t.Fatalf("start error = %q, want the server's error after %q", err, row.want)
			}
		})
	}
}

// On a single server with the default settings, a start after the one that created the stream and
// a bucket writes nothing to either.
func TestASecondStartOnASingleServerWritesNothing(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	t.Cleanup(client.Close)
	const bucket = "envoy_single_test"
	if _, err := EnsureKeyValue(client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	counting := &countingJetStream{JetStreamContext: client.JS()}
	cfg := parseConnectOptions(nil).streamConfig()
	if err := ensureStreamWithConfig(counting, &cfg); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if _, err := EnsureKeyValue(counting, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("second open of the bucket: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("a second start wrote %v, want nothing", writes)
	}
}

// awaitLeader waits until stream has a leader other than not.
func awaitLeader(t *testing.T, js nats.JetStreamContext, stream, not string) {
	t.Helper()
	deadline := time.Now().Add(clusterWait)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" && info.Cluster.Leader != not {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %s had no leader but %q within %s: %+v (%v)", stream, not, clusterWait, info, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// countingJetStream records every stream and bucket write a start sends through it.
type countingJetStream struct {
	nats.JetStreamContext
	mu     sync.Mutex
	writes []string
}

func (js *countingJetStream) record(write string) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.writes = append(js.writes, write)
}

func (js *countingJetStream) written() []string {
	js.mu.Lock()
	defer js.mu.Unlock()
	return slices.Clone(js.writes)
}

func (js *countingJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.record("create stream " + cfg.Name)
	return js.JetStreamContext.AddStream(cfg, opts...)
}

func (js *countingJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.record("update stream " + cfg.Name)
	return js.JetStreamContext.UpdateStream(cfg, opts...)
}

func (js *countingJetStream) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	js.record("create bucket " + cfg.Bucket)
	return js.JetStreamContext.CreateKeyValue(cfg)
}
