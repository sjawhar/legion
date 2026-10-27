package bus

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// testKeepInterval is the keeper's period in these tests (withKeepInterval).
	testKeepInterval = 250 * time.Millisecond
	// keepWait bounds every wait for the keeper to restore what was taken: a few periods, and the
	// time a cluster takes to apply a replica change it is asked for.
	keepWait = 20 * testKeepInterval
	// keeperByteLimit is the byte limit of the program whose settings the keeper restores.
	keeperByteLimit = 2_000_000
)

// awaitKept polls the stream until kept reports it holds what the keeper restores, at the latest
// for keepWait, and returns its info.
func awaitKept(t *testing.T, js nats.JetStreamContext, stream, what string, kept func(*nats.StreamInfo) bool) *nats.StreamInfo {
	t.Helper()
	deadline := time.Now().Add(keepWait)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil && kept(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s not kept within %s: %+v (%v)", stream, what, keepWait, info, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// heldFor checks the stream for periods keeper periods and fails the test when still stops
// reporting that it is as it was left.
func heldFor(t *testing.T, js nats.JetStreamContext, stream, what string, periods int, still func(*nats.StreamInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(periods) * testKeepInterval)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		info, err := js.StreamInfo(stream)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		if !still(info) {
			t.Fatalf("%s: %s changed: %+v", stream, what, info.Config)
		}
	}
}

// keptAtSettings reports whether the stream has the setting program's three copies and byte limit
// and an id, id itself when it is not empty.
func keptAtSettings(id string) func(*nats.StreamInfo) bool {
	return func(info *nats.StreamInfo) bool {
		got := info.Config.Metadata[StreamIDMetadataKey]
		return info.Config.Replicas == 3 && info.Config.MaxBytes == keeperByteLimit && got != "" && (id == "" || got == id)
	}
}

// Two programs start at the same moment against an R1 stream whose subject list is old, so both
// update it: one with three copies and a byte limit, one with the defaults, one copy and no limit.
// JetStream's stream update has no compare-and-swap, so the default program's update can land
// after the other's raise and put the stream back at one copy with no limit. Whichever write wins,
// within a few keeper periods after both starts the stream has three copies, the byte limit and
// its id (the one it had, or the one a start gave it), and the setting program's bucket has three
// copies, every round. Each round runs on a cluster of its own, so it starts from a fresh stream;
// a round whose race the default program won logs the keeper's restore.
func TestTheKeeperEndsTheMixedSettingsRaceAtThreeCopiesTheByteLimitAndTheID(t *testing.T) {
	logs := captureLogs(t)
	const bucket = "envoy_keeper_race"
	oldSubjects := slices.Clone(streamSubjects[:len(streamSubjects)-1])
	for round := range 5 {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			cluster, js := startClusterJetStream(t)
			deployed := *streamCfg
			deployed.Subjects = oldSubjects
			deployed.Replicas = 1
			id := ""
			if round%2 == 0 {
				id = fmt.Sprintf("deployed-id-%d", round)
				deployed.Metadata = map[string]string{StreamIDMetadataKey: id}
			}
			if _, err := js.AddStream(&deployed); err != nil {
				t.Fatalf("deploy the R1 stream with the old subjects: %v", err)
			}
			if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
				t.Fatalf("create the R1 bucket: %v", err)
			}
			restoresBefore := len(logs.recordsWithMsg(t, "envoy nats keeper restored the stream"))

			programs := []struct {
				options  []ConnectOption
				replicas int
			}{
				{options: []ConnectOption{WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), WithKeptBuckets(bucket), withKeepInterval(testKeepInterval)}, replicas: 3},
				{options: []ConnectOption{WithKeptBuckets(bucket)}, replicas: 1},
			}
			clients := make([]*Client, len(programs))
			errs := make([]error, len(programs))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i, program := range programs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					clients[i], errs[i] = ConnectOwningStream(cluster.URLs(), program.options...)
					if errs[i] == nil {
						_, errs[i] = EnsureKeyValue(clients[i].JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: program.replicas})
					}
				}()
			}
			close(start)
			wg.Wait()
			for _, client := range clients {
				if client != nil {
					t.Cleanup(client.Close)
				}
			}
			for i, err := range errs {
				if err != nil {
					t.Fatalf("start %d: %v", i, err)
				}
			}

			awaitKept(t, js, Stream, "three copies, the byte limit and the id", keptAtSettings(id))
			awaitKept(t, js, "KV_"+bucket, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
			for _, client := range clients {
				client.Close()
			}
			t.Logf("the keeper restored the stream %d times this round", len(logs.recordsWithMsg(t, "envoy nats keeper restored the stream"))-restoresBefore)
		})
	}
}

// An operator update that lowers the stream to one copy, removes its byte limit and deletes its id
// is undone within a few periods by the keeper of a program whose settings are three copies and a
// byte limit, which logs each field it restored. The id it restores is derived, and stays as it is
// over the passes after. A foreign subject and another age the same update set are left as they
// are: only a start reconciles those.
func TestTheKeeperRestoresTheCopiesTheByteLimitAndTheIDAnOperatorTookAway(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), withKeepInterval(testKeepInterval))
	info := awaitStream(t, js, Stream)

	lowered := info.Config
	lowered.Replicas = 1
	lowered.MaxBytes = -1
	lowered.Metadata = maps.Clone(info.Config.Metadata)
	delete(lowered.Metadata, StreamIDMetadataKey)
	lowered.Subjects = append(slices.Clone(info.Config.Subjects), "operations.foreign.>")
	lowered.MaxAge = info.Config.MaxAge + time.Hour
	if _, err := js.UpdateStream(&lowered); err != nil {
		t.Fatalf("operator update: %v", err)
	}

	restored := awaitKept(t, js, Stream, "three copies, the byte limit and an id", keptAtSettings(""))
	if !slices.Equal(restored.Config.Subjects, lowered.Subjects) || restored.Config.MaxAge != lowered.MaxAge {
		t.Fatalf("the keeper changed what only a start reconciles: subjects %v, age %s; want %v, %s", restored.Config.Subjects, restored.Config.MaxAge, lowered.Subjects, lowered.MaxAge)
	}
	id := restored.Config.Metadata[StreamIDMetadataKey]
	if want := deployedStreamID(restored.Created); id != want {
		t.Fatalf("restored id = %q, want the derived %q", id, want)
	}
	heldFor(t, js, Stream, "the restored configuration", 4, func(info *nats.StreamInfo) bool {
		return reflect.DeepEqual(info.Config, restored.Config)
	})

	var fields []string
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper restored the stream") {
		if record["level"] != "WARN" || record["stream"] != Stream {
			t.Fatalf("keeper record = %v, want a WARN naming %s", record, Stream)
		}
		fields = append(fields, fmt.Sprint(record["restored"]))
	}
	logged := strings.Join(fields, ", ")
	for _, want := range []string{"replicas 1 -> 3", fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit), StreamIDMetadataKey + " added as " + id} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the keeper's lines restored %q, want %q among them", logged, want)
		}
	}
}

// The keeper raises a bucket its program names (WithKeptBuckets) back to the deployment's copies
// after an operator lowered it, changing nothing else about it, and logs the raise. A bucket it
// does not name stays lowered. A named bucket that does not exist, as one a program opens only
// for a feature it has not configured, is skipped: the keeper creates nothing, updates nothing and
// logs nothing about it.
func TestTheKeeperRaisesTheBucketsItsProgramNamesBackToTheirCopies(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	logs := captureLogs(t)
	const listed, unlisted, missing = "envoy_keeper_listed", "envoy_keeper_unlisted", "envoy_keeper_missing"

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	var missingRequests atomic.Int32
	for _, subject := range []string{"$JS.API.STREAM.UPDATE.KV_" + missing, "$JS.API.STREAM.CREATE.KV_" + missing} {
		if _, err := watcher.Subscribe(subject, func(*nats.Msg) { missingRequests.Add(1) }); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}

	client := startOnCluster(t, cluster, WithReplicas(3), WithKeptBuckets(listed, missing), withKeepInterval(testKeepInterval))
	before := map[string]nats.StreamConfig{}
	for _, bucket := range []string{listed, unlisted} {
		if _, err := EnsureKeyValue(client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3, History: 5, TTL: time.Hour}); err != nil {
			t.Fatalf("open %s at three copies: %v", bucket, err)
		}
		stream := "KV_" + bucket
		info := awaitStream(t, js, stream)
		before[bucket] = info.Config
		lowered := info.Config
		lowered.Replicas = 1
		if _, err := js.UpdateStream(&lowered); err != nil {
			t.Fatalf("lower %s to one copy: %v", stream, err)
		}
	}

	raised := awaitKept(t, js, "KV_"+listed, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
	if !reflect.DeepEqual(raised.Config, before[listed]) {
		t.Fatalf("raised %s = %+v, want it as it was before it was lowered, %+v", listed, raised.Config, before[listed])
	}
	heldFor(t, js, "KV_"+unlisted, "one copy", 4, func(info *nats.StreamInfo) bool { return info.Config.Replicas == 1 })

	if _, err := js.StreamInfo("KV_" + missing); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("read KV_%s = %v, want it still missing", missing, err)
	}
	if n := missingRequests.Load(); n != 0 {
		t.Fatalf("the keeper sent %d create or update requests for the missing bucket, want none", n)
	}
	if failures := logs.recordsWithMsg(t, "envoy nats keeper could not raise a bucket"); len(failures) != 0 {
		t.Fatalf("the keeper logged failed raises %v, want none", failures)
	}
	raises := map[string]any{}
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper raised a bucket") {
		raises[fmt.Sprint(record["bucket"])] = record["replicas"]
	}
	if want := map[string]any{listed: "1 -> 3"}; !reflect.DeepEqual(raises, want) {
		t.Fatalf("the keeper's raises logged %v, want %v", raises, want)
	}
}

// The keeper of a program with the default settings, one copy and no byte limit, on a stream that
// has an id, sends no stream update over several periods, whatever the stream's copies and limit:
// it never lowers either, and raises no bucket it names, at one copy as well. The update count is
// read from the API subject itself.
func TestAKeeperWithTheDefaultSettingsSendsNoUpdate(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit))
	awaitStream(t, js, Stream)

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	var updates atomic.Int32
	if _, err := watcher.Subscribe("$JS.API.STREAM.UPDATE.>", func(*nats.Msg) { updates.Add(1) }); err != nil {
		t.Fatalf("watch stream updates: %v", err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}

	client := startOnCluster(t, cluster, WithKeptBuckets("envoy_keeper_default"), withKeepInterval(testKeepInterval))
	if _, err := EnsureKeyValue(client.JS(), &nats.KeyValueConfig{Bucket: "envoy_keeper_default", Storage: nats.FileStorage}); err != nil {
		t.Fatalf("open a bucket at the default copies: %v", err)
	}
	time.Sleep(6 * testKeepInterval)
	if n := updates.Load(); n != 0 {
		t.Fatalf("the default program sent %d stream updates, want none", n)
	}

	// The watch sees an update when one is sent, so the count above is not vacuous.
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if _, err := js.UpdateStream(&info.Config); err != nil {
		t.Fatalf("update the stream: %v", err)
	}
	deadline := time.Now().Add(keepWait)
	for updates.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch did not see an update sent to the stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close and Drain end the keeper before they return: once the client stops, a stream lowered to
// one copy stays there.
func TestStoppingTheClientStopsItsKeeper(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{name: "close", stop: (*Client).Close},
		{name: "drain", stop: func(client *Client) { _ = client.Drain(5 * time.Second) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			cluster, js := startClusterJetStream(t)
			client := startOnCluster(t, cluster, WithReplicas(3), withKeepInterval(testKeepInterval))
			info := awaitStream(t, js, Stream)

			stop.stop(client)
			select {
			case <-client.keeperDone:
			default:
				t.Fatalf("%s returned with the keeper still running", stop.name)
			}

			lowered := info.Config
			lowered.Replicas = 1
			if _, err := js.UpdateStream(&lowered); err != nil {
				t.Fatalf("lower the stream to one copy: %v", err)
			}
			heldFor(t, js, Stream, "one copy", 6, func(info *nats.StreamInfo) bool { return info.Config.Replicas == 1 })
		})
	}
}
