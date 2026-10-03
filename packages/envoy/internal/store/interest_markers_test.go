package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/wait"
)

// captureJSONLogs routes slog.Default() into a buffer for the test and decodes the records back, so
// a test asserts a line's fields rather than its rendering. Open takes slog.Default() as the
// registry's logger and hands it to its cache watcher, and a collector takes the registry's, so
// the pass's lines and the cache's warm-up line both land here when it runs before Open.
func captureJSONLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if line == "" {
				continue
			}
			record := map[string]any{}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode a log record: %v (%s)", err, line)
			}
			records = append(records, record)
		}
		return records
	}
}

// collectorOf is the collector StartInterestMarkerCollector runs, without its ticker, so a test
// drives its passes one at a time.
func collectorOf(registry *Registry) *interestMarkerCollector {
	return &interestMarkerCollector{registry: registry, log: registry.logger()}
}

// jsonRecord returns the one record whose msg is msg, failing when there is none or more than one.
func jsonRecord(t *testing.T, records []map[string]any, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, record := range records {
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %q record, got %d of them in:\n%v", msg, len(found), records)
	}
	return found[0]
}

// rawInterestBucket creates the interest bucket directly, so a test can seed it before Open, which
// finds it already there.
func rawInterestBucket(t *testing.T, conn *natsgo.Conn) (natsgo.JetStreamContext, natsgo.KeyValue) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	return js, kv
}

// seedMarkers leaves count delete markers in kv, one per key that no longer exists: what the
// interest reaper leaves behind for every session that ever died.
func seedMarkers(t *testing.T, kv natsgo.KeyValue, count int) {
	t.Helper()
	for i := range count {
		key := fmt.Sprintf("ses_gone_%03d", i)
		if _, err := kv.Put(key, []byte(`{"session_id":"`+key+`"}`)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
	}
}

// seedLive puts count live interests in kv and returns each key's revision.
func seedLive(t *testing.T, kv natsgo.KeyValue, count int) map[string]uint64 {
	t.Helper()
	revisions := map[string]uint64{}
	for i := range count {
		key := fmt.Sprintf("ses_live_%03d", i)
		revision, err := kv.Put(key, []byte(`{"session_id":"`+key+`","machine_id":"m1"}`))
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		revisions[key] = revision
	}
	return revisions
}

// bucketState is the stream state behind a KV bucket: how many subjects it carries (one per key
// that has a message, live or a delete marker) and where its sequence space runs.
func bucketState(t *testing.T, js natsgo.JetStreamContext, bucket string) natsgo.StreamState {
	t.Helper()
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("stream info of KV_%s: %v", bucket, err)
	}
	return info.State
}

// bucketOps reads every key the bucket still holds a message for, and what that message is.
func bucketOps(t *testing.T, kv natsgo.KeyValue) map[string]natsgo.KeyValueOp {
	t.Helper()
	watcher, err := kv.Watch(natsgo.AllKeys, natsgo.MetaOnly())
	if err != nil {
		t.Fatalf("watch the bucket: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	ops := map[string]natsgo.KeyValueOp{}
	for entry := range watcher.Updates() {
		if entry == nil {
			return ops
		}
		ops[entry.Key()] = entry.Operation()
	}
	t.Fatal("the bucket's scan ended before it had delivered every key")
	return nil
}

// openRegistry opens the registry on conn and waits for its interest cache.
func openRegistry(t *testing.T, conn *natsgo.Conn) *Registry {
	t.Helper()
	registry, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	waitFor(t, 30*time.Second, registry.watcher.Ready)
	return registry
}

// A restart streams every delete marker in the interest bucket before its cache is ready, and
// nothing else removes one, so they pile up behind the few live keys (LEGION-374). One
// collection pass removes them, and every live key keeps its value and its revision: the floor is
// the lowest revision the pass's own scan of the stream saw as a PUT, so nothing below it belongs
// to a live key (the bucket keeps one message per subject).
func TestAPassCollectsTheDeleteMarkersAndKeepsEveryLiveKey(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	const (
		markers = 40
		live    = 3
	)
	seedMarkers(t, kv, markers)
	revisions := seedLive(t, kv, live)

	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)

	// The cost the restart pays today, off the registry's own warm-up line.
	warmUp := jsonRecord(t, records(), "interest registry cache warm-up")
	if warmUp["entries"] != float64(live) || warmUp["delete_markers"] != float64(markers) {
		t.Fatalf("the warm-up line says entries=%v delete_markers=%v, want %d live keys behind %d markers: %v",
			warmUp["entries"], warmUp["delete_markers"], live, markers, warmUp)
	}
	if before := bucketState(t, js, Bucket); before.NumSubjects != markers+live {
		t.Fatalf("the seeded bucket carries %d subjects, want %d", before.NumSubjects, markers+live)
	}

	collector := collectorOf(registry)
	purged, err := collector.pass(js, nil)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != markers {
		t.Fatalf("the pass purged %d messages, want the %d delete markers", purged, markers)
	}
	after := bucketState(t, js, Bucket)
	if after.NumSubjects != live {
		t.Fatalf("after one pass the bucket carries %d subjects, want the %d live keys", after.NumSubjects, live)
	}
	for key, revision := range revisions {
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("live key %s is gone after the pass: %v", key, err)
		}
		if entry.Revision() != revision {
			t.Fatalf("live key %s is at revision %d after the pass, want its stored %d", key, entry.Revision(), revision)
		}
		if want := `{"session_id":"` + key + `","machine_id":"m1"}`; string(entry.Value()) != want {
			t.Fatalf("live key %s reads %q after the pass, want %q", key, entry.Value(), want)
		}
	}
	// The line names each read for what it is: read 1 and read 2 both come before the purge, and
	// only the purged_ fields are the stream the purge left.
	floor := uint64(0)
	for _, revision := range revisions {
		if floor == 0 || revision < floor {
			floor = revision
		}
	}
	collected := jsonRecord(t, records(), "interest markers collected")
	for field, want := range map[string]uint64{
		"floor": floor, "purged": markers, "purged_first_seq": floor, "purged_msgs": live,
		"read1_last_seq": floor + live - 1, "read2_last_seq": floor + live - 1,
	} {
		if collected[field] != float64(want) {
			t.Fatalf("the collection line's %s = %v, want %d: %v", field, collected[field], want, collected)
		}
	}
	for _, field := range []string{"read1_first_seq", "read1_created", "read2_first_seq", "read2_created"} {
		if _, ok := collected[field]; !ok {
			t.Fatalf("the collection line has no %s: %v", field, collected)
		}
	}
	// A second pass has nothing below the floor and purges nothing, so every listener may run it.
	again, err := collector.pass(js, nil)
	if err != nil || again != 0 {
		t.Fatalf("a second pass purged %d messages (%v), want 0: the purge must be idempotent", again, err)
	}
}

// A write between the scan and the purge lands above the floor and survives it, whichever write it
// is: the floor is a sequence the scan already saw, and the bucket only hands out higher ones.
func TestWritesBetweenTheScanAndThePurgeSurvive(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 8)
	revisions := seedLive(t, kv, 3)
	registry := openRegistry(t, conn)

	var added uint64
	purged, err := collectorOf(registry).pass(js, func(pass *interestPass) {
		revision, err := kv.Put("ses_arrived", []byte(`{"session_id":"ses_arrived","machine_id":"m1"}`))
		if err != nil {
			t.Fatalf("put ses_arrived between the scan and the purge: %v", err)
		}
		added = revision
		if err := kv.Delete("ses_live_000"); err != nil {
			t.Fatalf("delete ses_live_000 between the scan and the purge: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 8 {
		t.Fatalf("the pass purged %d messages, want the 8 markers it scanned", purged)
	}
	if added <= revisions["ses_live_002"] {
		t.Fatalf("the write during the pass got revision %d, at or below the floor's own %d", added, revisions["ses_live_002"])
	}
	ops := bucketOps(t, kv)
	if ops["ses_arrived"] != natsgo.KeyValuePut {
		t.Fatalf("the key written during the pass is %v afterwards, want a PUT: %v", ops["ses_arrived"], ops)
	}
	if ops["ses_live_000"] != natsgo.KeyValueDelete {
		t.Fatalf("the delete during the pass is %v afterwards, want its marker: %v", ops["ses_live_000"], ops)
	}
	for _, key := range []string{"ses_live_001", "ses_live_002"} {
		if ops[key] != natsgo.KeyValuePut {
			t.Fatalf("live key %s is %v after the pass, want a PUT: %v", key, ops[key], ops)
		}
	}
	for key := range ops {
		if strings.HasPrefix(key, "ses_gone_") {
			t.Fatalf("the pass left marker %s behind: %v", key, ops)
		}
	}
}

// A pass whose scan read a stream that is no longer there purges nothing. Deleting and creating the
// bucket numbers its revisions from 1 again, so the floor the earlier scan computed is meaningless
// against the new stream -- and a floor above its last sequence would make the server's unfiltered
// purge compact the WHOLE stream (nats-server filestore.go Compact: a floor past LastSeq purges
// everything). The two reads are compared for exactly this; here the replacement's sequence space
// is lower as well, and the creation time, which bounds identity, is what the pass reports.
func TestAPassWhoseScanReadAReplacedStreamPurgesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 12)
	seedLive(t, kv, 3)
	// Before Open: the registry takes slog.Default() as its logger when it opens.
	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)

	purged, err := collectorOf(registry).pass(js, func(pass *interestPass) {
		if err := js.DeleteKeyValue(Bucket); err != nil {
			t.Fatalf("delete the interest bucket: %v", err)
		}
		recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
		seedLive(t, recreated, 2)
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 0 {
		t.Fatalf("the pass purged %d messages from a replaced stream, want 0", purged)
	}
	state := bucketState(t, js, Bucket)
	if state.NumSubjects != 2 || state.Msgs != 2 {
		t.Fatalf("the recreated bucket holds %d subjects and %d messages, want its 2 live keys", state.NumSubjects, state.Msgs)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "replaced") {
		t.Fatalf("a replaced stream logged %v; want a WARN saying the stream was replaced", refusal)
	}
}

// The same guard from the other side: a floor above the stream's last sequence is refused, because
// that is the one value that makes an unfiltered purge delete every message in the bucket.
func TestAFloorAboveTheStreamsLastSequencePurgesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 6)
	seedLive(t, kv, 3)
	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)
	before := bucketState(t, js, Bucket)

	purged, err := collectorOf(registry).pass(js, func(pass *interestPass) {
		pass.floor = before.LastSeq + 1
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a floor above the last sequence purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, Bucket); state.Msgs != before.Msgs || state.NumSubjects != before.NumSubjects {
		t.Fatalf("a floor above the last sequence changed the bucket: %d messages and %d subjects, want %d and %d",
			state.Msgs, state.NumSubjects, before.Msgs, before.NumSubjects)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "last sequence") {
		t.Fatalf("a floor above the last sequence logged %v; want a WARN naming it", refusal)
	}
}

// A sequence space that moved backwards between the two reads, with the stream's creation time
// unchanged, is what a JetStream restore leaves: the creation time is the snapshot's, so it says
// nothing, and only the sequences do. The pass refuses.
func TestASequenceSpaceThatMovedBackwardsPurgesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 6)
	seedLive(t, kv, 3)
	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)
	before := bucketState(t, js, Bucket)

	purged, err := collectorOf(registry).pass(js, func(pass *interestPass) {
		// Read 1 claims a higher last sequence than the stream now reports, which is what a
		// restore from an older snapshot produces between the two reads.
		pass.read1.LastSeq += 5
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a sequence space that moved backwards purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, Bucket); state.Msgs != before.Msgs {
		t.Fatalf("a refused pass changed the bucket: %d messages, want %d", state.Msgs, before.Msgs)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "backward") {
		t.Fatalf("a sequence space that moved backwards logged %v; want a WARN saying so", refusal)
	}
}

// A bucket deleted and created again between the two reads does not always lower the sequence
// space: a young bucket whose first sequence is still 1 is replaced by one that starts at 1 too,
// and a replacement written as far as the original passes every sequence check. Here the original
// holds a marker at sequence 1 (a delete of a key never put, which the admin delete and a reaper
// that lost a race both write) and two live keys at 2 and 3, so the scan's floor is 2; the
// replacement holds three live keys at 1-3, and a purge below 2 would delete the first of them.
// Only the creation time tells the two streams apart, so it bounds identity where the sequences
// cannot.
func TestAPassWhoseStreamWasReplacedFromTheSameFirstSequencePurgesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	if err := kv.Delete("ses_never_put"); err != nil {
		t.Fatalf("delete a key that was never put: %v", err)
	}
	seedLive(t, kv, 2)
	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)
	if original := bucketState(t, js, Bucket); original.FirstSeq != 1 || original.LastSeq != 3 {
		t.Fatalf("the original stream runs %d-%d, want 1-3", original.FirstSeq, original.LastSeq)
	}

	var replacement map[string]uint64
	purged, err := collectorOf(registry).pass(js, func(pass *interestPass) {
		if pass.floor != 2 {
			t.Fatalf("the scan's floor is %d, want 2", pass.floor)
		}
		if err := js.DeleteKeyValue(Bucket); err != nil {
			t.Fatalf("delete the interest bucket: %v", err)
		}
		replacement = seedLive(t, testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage}), 3)
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 0 {
		t.Fatalf("the pass purged %d messages from the replacement stream, want 0", purged)
	}
	state := bucketState(t, js, Bucket)
	if state.FirstSeq != 1 || state.LastSeq != 3 || state.Msgs != 3 {
		t.Fatalf("the replacement stream runs %d-%d with %d messages after the pass, want its 3 live keys at 1-3",
			state.FirstSeq, state.LastSeq, state.Msgs)
	}
	for key, revision := range replacement {
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("the replacement's live key %s is gone after the pass: %v", key, err)
		}
		if entry.Revision() != revision {
			t.Fatalf("the replacement's live key %s is at revision %d, want %d", key, entry.Revision(), revision)
		}
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "replaced") {
		t.Fatalf("a replaced stream logged %v; want a WARN saying the stream was replaced", refusal)
	}
}

// A pass that cannot read the bucket's stream says so: a listener that has stopped collecting must
// not look like one with nothing to collect. A read before the purge refuses the pass and names
// itself; the read after the purge only counts what it removed, so the purge is logged as done
// with its count unknown.
func TestAPassThatCannotReadTheStreamSaysWhichReadFailed(t *testing.T) {
	t.Run("the first read", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		js, kv := rawInterestBucket(t, conn)
		seedMarkers(t, kv, 3)
		seedLive(t, kv, 2)
		records := captureJSONLogs(t)
		registry := openRegistry(t, conn)
		if err := js.DeleteKeyValue(Bucket); err != nil {
			t.Fatalf("delete the interest bucket: %v", err)
		}
		if purged, _ := collectorOf(registry).pass(js, nil); purged != 0 {
			t.Fatalf("a pass over a deleted bucket purged %d messages", purged)
		}
		refusal := jsonRecord(t, records(), "interest marker collection refused")
		if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "first read") || refusal["error"] == nil {
			t.Fatalf("a pass over a deleted bucket logged %v; want one WARN naming the first read and its error", refusal)
		}
	})
	t.Run("the second read", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		js, kv := rawInterestBucket(t, conn)
		seedMarkers(t, kv, 3)
		seedLive(t, kv, 2)
		records := captureJSONLogs(t)
		registry := openRegistry(t, conn)
		purged, _ := collectorOf(registry).pass(js, func(*interestPass) {
			if err := js.DeleteKeyValue(Bucket); err != nil {
				t.Fatalf("delete the interest bucket: %v", err)
			}
		})
		if purged != 0 {
			t.Fatalf("a pass whose bucket went away purged %d messages", purged)
		}
		refusal := jsonRecord(t, records(), "interest marker collection refused")
		if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "second read") || refusal["error"] == nil {
			t.Fatalf("a pass whose bucket went away before its second read logged %v; want one WARN naming the second read and its error", refusal)
		}
		if refusal["read1_last_seq"] == nil {
			t.Fatalf("the refusal lost the first read it did take: %v", refusal)
		}
	})
	t.Run("the read after the purge", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		js, kv := rawInterestBucket(t, conn)
		seedMarkers(t, kv, 3)
		seedLive(t, kv, 2)
		records := captureJSONLogs(t)
		registry := openRegistry(t, conn)
		gone := purgeThenDelete{JetStreamContext: js, bucket: Bucket}
		if _, err := collectorOf(registry).pass(gone, nil); err == nil {
			t.Fatal("a pass whose purged stream could not be read returned no error")
		}
		collected := jsonRecord(t, records(), "interest markers collected")
		if collected["level"] != "WARN" || !strings.Contains(fmt.Sprint(collected["error"]), "purged stream") {
			t.Fatalf("a purge whose count could not be read logged %v; want the purge at WARN with the read's error", collected)
		}
		if _, ok := collected["purged"]; ok {
			t.Fatalf("a purge whose count could not be read reported one: %v", collected)
		}
		if collected["floor"] == nil || collected["read2_last_seq"] == nil {
			t.Fatalf("the purge's line lost the values it was decided on: %v", collected)
		}
	})
}

// purgeThenDelete purges as the server does, then deletes the bucket, so the read a pass makes after
// its purge finds no stream.
type purgeThenDelete struct {
	natsgo.JetStreamContext

	bucket string
}

func (j purgeThenDelete) PurgeStream(name string, opts ...natsgo.JSOpt) error {
	if err := j.JetStreamContext.PurgeStream(name, opts...); err != nil {
		return err
	}
	return j.DeleteKeyValue(j.bucket)
}

// A live key whose value this build cannot decode is still a live key. The cache evicts it
// (applyWatched) while its message stays in the stream, so a floor taken from the cache would rise
// above it and the purge would delete it; a floor taken from the bucket's own scan protects it,
// because the scan reads operations and revisions and never a value.
func TestAnUndecodableLiveValueIsKept(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 6)
	undecodable, err := kv.Put("ses_undecodable", []byte("not json"))
	if err != nil {
		t.Fatalf("put ses_undecodable: %v", err)
	}
	seedLive(t, kv, 2)
	registry := openRegistry(t, conn)
	if _, cached := registry.cache["ses_undecodable"]; cached {
		t.Fatal("the cache kept an undecodable value; this test needs the eviction that makes the cache incomplete")
	}

	if _, err := collectorOf(registry).pass(js, nil); err != nil {
		t.Fatalf("pass: %v", err)
	}
	entry, err := kv.Get("ses_undecodable")
	if err != nil {
		t.Fatalf("the undecodable live key is gone after the pass: %v", err)
	}
	if entry.Revision() != undecodable {
		t.Fatalf("the undecodable live key is at revision %d, want its stored %d", entry.Revision(), undecodable)
	}
	if state := bucketState(t, js, Bucket); state.NumSubjects != 3 {
		t.Fatalf("after the pass the bucket carries %d subjects, want the 3 live keys", state.NumSubjects)
	}
}

// A scan nats.go's idle timer gave up on is not a reading of the bucket: the keys it did not
// deliver are still there, and the lowest revision among those it did is no floor. The pass refuses
// and says so, and the listener stays healthy: a timed-out warm-up is not a dead cache, so Ping,
// WatchErr and the /healthz answer built on them report no fault and nothing rebuilds the watcher.
func TestAPassWhoseScanTimedOutPurgesNothingAndLeavesTheRegistryHealthy(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 6)
	seedLive(t, kv, 3)
	before := bucketState(t, js, Bucket)

	roleKV, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: RoleBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	records := captureJSONLogs(t)
	registry := &Registry{roleKV: bus.KeyValue{KeyValue: roleKV}, now: time.Now, cache: map[string]Interest{}, cacheRevisions: map[string]uint64{}}
	handle := bus.KeyValue{KeyValue: kvwatchtest.TimingOut(kv, 2)}
	registry.watcher = kvwatch.New("interest registry", handle, registry.applyWatched, registry.resetCache)
	t.Cleanup(registry.StopWatch)
	registry.watcher.Start()
	waitFor(t, 30*time.Second, registry.watcher.Ready)

	purged, err := collectorOf(registry).pass(js, nil)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a pass whose scan timed out purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, Bucket); state.Msgs != before.Msgs {
		t.Fatalf("a refused pass changed the bucket: %d messages, want %d", state.Msgs, before.Msgs)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "complete") {
		t.Fatalf("a timed-out scan logged %v; want a WARN saying the scan did not complete", refusal)
	}

	// The warm-up says timed out, and nothing else does: the registry is healthy.
	warmUp := jsonRecord(t, records(), "interest registry cache warm-up")
	if warmUp["outcome"] != "timed out" {
		t.Fatalf("the warm-up line says outcome=%v, want \"timed out\": %v", warmUp["outcome"], warmUp)
	}
	if err := registry.Ping(); err != nil {
		t.Fatalf("Ping() = %v after a timed-out warm-up; /healthz would answer 503 over a live cache", err)
	}
	if err := registry.WatchErr(); err != nil {
		t.Fatalf("WatchErr() = %v after a timed-out warm-up; self-health would rebuild a live watcher", err)
	}
}

// The capability a pass needs, off a real server's own trace rather than off a reading of the
// client library: one publish to $JS.API.STREAM.PURGE.KV_<bucket>, and no purge of any other stream
// or through any JetStream domain. nats.go builds that subject, so a call site in this repository
// cannot settle what goes on the wire; the server's trace can, and a grant that denies exactly that
// subject shows it is the one the purge needs. The negative control is also the refusal path the
// deployed listener must survive: a NATS user without the capability leaves every marker where it
// was, logs one WARN however many passes run, and keeps the cache healthy (LEGION-374); the
// deployed listeners connect under agent-c's `envoyNatsAuthorization`.
func TestThePurgeSendsTheStreamPurgeSubjectItsGrantMustAllow(t *testing.T) {
	// Each subtest starts a server of its own (startGrantedServer), whose grant names the purge
	// subject of the interest bucket.
	t.Run("a grant that allows it purges, and the trace names the subject", func(t *testing.T) {
		purgeSubject := "$JS.API.STREAM.PURGE.KV_" + Bucket
		granted := startGrantedServer(t, nil)
		purged, err := collectorOf(granted.registry).pass(granted.js, nil)
		if err != nil || purged == 0 {
			t.Fatalf("a pass under a grant that allows the purge = %d, %v; want the markers purged", purged, err)
		}
		trace := granted.traceThrough(t)
		purges := tracedPurges(trace)
		t.Logf("purges the server traced: %v", purges)
		if !slices.Contains(purges, purgeSubject) {
			t.Fatalf("the server's trace holds no publish to %s; the purge went somewhere else. Every publish it traced:\n%s",
				purgeSubject, strings.Join(tracedPublishLines(trace), "\n"))
		}
		for _, subject := range purges {
			if subject != purgeSubject {
				t.Fatalf("the pass published to another purge subject, %s, besides %s", subject, purgeSubject)
			}
		}
	})

	t.Run("a grant that denies it purges nothing, warns once and stays healthy", func(t *testing.T) {
		purgeSubject := "$JS.API.STREAM.PURGE.KV_" + Bucket
		records := captureJSONLogs(t)
		granted := startGrantedServer(t, []string{purgeSubject})
		before := bucketState(t, granted.js, Bucket)
		// The server answers a purge the grant refuses with nothing, so the passes purge through a
		// context that waits a second; on granted.js each pass would wait out its whole 10 s.
		// nats.go's PurgeStream ignores a per-call wait, so the bound has to be the context's.
		refusedPurgeJS, err := granted.conn.JetStream(natsgo.MaxWait(time.Second))
		if err != nil {
			t.Fatalf("jetstream for the refused purge: %v", err)
		}
		// One collector for both passes, as the listener runs one: its latch is what says once.
		collector := collectorOf(granted.registry)
		for pass := range 2 {
			purged, err := collector.pass(refusedPurgeJS, nil)
			if err == nil || purged != 0 {
				t.Fatalf("pass %d under a grant that denies the purge = %d, %v; want no purge and the error", pass, purged, err)
			}
			t.Logf("pass %d: %v", pass, err)
		}
		if state := bucketState(t, granted.js, Bucket); state.Msgs != before.Msgs || state.NumSubjects != before.NumSubjects {
			t.Fatalf("a refused purge changed the bucket: %d messages and %d subjects, want %d and %d",
				state.Msgs, state.NumSubjects, before.Msgs, before.NumSubjects)
		}
		refusal := jsonRecord(t, records(), "interest marker collection could not purge the bucket")
		if refusal["level"] != "WARN" {
			t.Fatalf("a refused purge logged %v, want one WARN for two passes", refusal)
		}
		var violations []string
		for _, line := range granted.traceThrough(t) {
			if strings.Contains(line, "Permissions Violation for Publish to") {
				violations = append(violations, line)
			}
		}
		if len(violations) == 0 {
			t.Fatal("the server logged no permissions violation; the grant did not refuse the purge")
		}
		for _, line := range violations {
			if !strings.Contains(line, purgeSubject) {
				t.Fatalf("the grant refused another subject too: %s", strings.TrimSpace(line))
			}
			t.Logf("server refusal: %s", strings.TrimSpace(line))
		}
		// The listener serves on: its cache is live and its buckets answer.
		if err := granted.registry.Ping(); err != nil {
			t.Fatalf("Ping() = %v after a refused purge; the listener must stay healthy", err)
		}
		if _, err := granted.kv.Get("ses_live_000"); err != nil {
			t.Fatalf("a live interest is unreadable after a refused purge: %v", err)
		}
	})
}

// grantedServer is a tracing NATS server with one nkey user, the connection a test's registry and
// passes share, and the interest bucket on that server.
type grantedServer struct {
	ctr      *tcnats.NATSContainer
	conn     *natsgo.Conn
	registry *Registry
	// js waits as long as the listener's own context does (10 s, internal/bus): on a loaded machine
	// a seeding put can take more than a second.
	js natsgo.JetStreamContext
	kv natsgo.KeyValue
}

// startGrantedServer starts a tracing NATS server whose one nkey user may publish to everything
// except deny, opens a registry on it as the listener does (one connection, its own JetStream
// context) and seeds the interest bucket with markers behind live keys.
func startGrantedServer(t *testing.T, deny []string) grantedServer {
	t.Helper()
	seed, public := testnats.User(t)
	quoted := make([]string, len(deny))
	for i, subject := range deny {
		quoted[i] = fmt.Sprintf("%q", subject)
	}
	config := fmt.Sprintf("trace: true\njetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: "+
		"{ publish: { allow: [\">\"], deny: [%s] }, subscribe: { allow: [\">\"] } } } ]\n}\n",
		public, strings.Join(quoted, ", "))
	ctr, err := tcnats.Run(context.Background(), testnats.Image, tcnats.WithConfigFile(strings.NewReader(config)))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start the granted NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(context.Background())
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	pair, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatalf("read the user's seed: %v", err)
	}
	var conn *natsgo.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err = natsgo.Connect(uri,
			natsgo.Nkey(public, func(nonce []byte) ([]byte, error) { return pair.Sign(nonce) }),
			natsgo.Timeout(time.Second), natsgo.NoReconnect())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect to the granted NATS: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	waitFor(t, 30*time.Second, func() bool { _, err := js.AccountInfo(); return err == nil })
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	seedMarkers(t, kv, 6)
	seedLive(t, kv, 3)
	registry, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	waitFor(t, 30*time.Second, registry.watcher.Ready)
	return grantedServer{ctr: ctr, conn: conn, registry: registry, js: js, kv: kv}
}

// traceSentinels numbers the sentinels traceThrough publishes, so each call waits for its own
// sentinel's line rather than for one an earlier call on the same server put in the log.
var traceSentinels atomic.Uint64

// traceWait bounds how long traceThrough waits for the sentinel's line to reach the container's log.
const traceWait = 5 * time.Second

// traceThrough publishes a sentinel on the server's connection and returns every line of the
// server's log once the sentinel's own trace line is in it. The server traces a connection's
// operations in the order it reads them, and traces what it sends back while handling one (a
// permissions violation) before it reads the next. Docker copies the container's output into its
// log in that order, a moment after the server writes it, so a read taken as soon as a pass returns
// can miss the pass's last lines. Once the sentinel's line is in the log, every line for what the
// connection sent before it is there too. Past traceWait it fails the test, naming the sentinel's
// line and every publish the server traced.
func (g grantedServer) traceThrough(t *testing.T) []string {
	t.Helper()
	sentinel := fmt.Sprintf("trace.sentinel.%d", traceSentinels.Add(1))
	if err := g.conn.Publish(sentinel, nil); err != nil {
		t.Fatalf("publish the trace sentinel: %v", err)
	}
	if err := g.conn.Flush(); err != nil {
		t.Fatalf("flush the trace sentinel: %v", err)
	}
	want := "[PUB " + sentinel + " "
	if err := wait.ForLog(want).WithStartupTimeout(traceWait).WaitUntilReady(context.Background(), g.ctr); err != nil {
		t.Fatalf("the server's log holds no line with %q after %s (%v). Every publish it traced:\n%s",
			want, traceWait, err, strings.Join(tracedPublishLines(serverLog(t, g.ctr)), "\n"))
	}
	return serverLog(t, g.ctr)
}

// serverLog is every line of the container's log.
func serverLog(t *testing.T, ctr *tcnats.NATSContainer) []string {
	t.Helper()
	logs, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	return strings.Split(string(text), "\n")
}

// tracedPublish matches the line a tracing server writes for each publish a client sends,
// `<<- [PUB <subject> [<reply>] <size>]`, or HPUB when it carries headers, and captures the whole
// subject, so a subject is compared exactly and a longer one that starts with it is not taken for it.
var tracedPublish = regexp.MustCompile(`<<- \[H?PUB (\S+) `)

// tracedPublishLines is every line of trace that records a client's publish.
func tracedPublishLines(trace []string) []string {
	var lines []string
	for _, line := range trace {
		if tracedPublish.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

// tracedPurges is the subject of every stream purge a client published in trace, through a
// JetStream domain or none: a domain puts its name ahead of the API, $JS.<domain>.API.STREAM.PURGE.
func tracedPurges(trace []string) []string {
	var purges []string
	for _, line := range trace {
		if match := tracedPublish.FindStringSubmatch(line); match != nil && strings.Contains(match[1], ".API.STREAM.PURGE.") {
			purges = append(purges, match[1])
		}
	}
	return purges
}
