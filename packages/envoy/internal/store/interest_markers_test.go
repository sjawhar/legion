package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// captureJSONLogs routes slog.Default() into a buffer for the test and decodes the records back, so
// a test asserts a line's fields rather than its rendering. Open takes slog.Default() as the
// registry's logger, and kvwatch logs through the same default, so both the pass's lines and the
// cache's warm-up line land here.
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

// rawInterestBucket creates t's interest bucket directly, so a test can seed it before Open, which
// finds it already there.
func rawInterestBucket(t *testing.T, conn *natsgo.Conn) (natsgo.JetStreamContext, natsgo.KeyValue) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
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

// openRegistry opens the registry on t's buckets and waits for its interest cache.
func openRegistry(t *testing.T, conn *natsgo.Conn) *Registry {
	t.Helper()
	registry, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	waitFor(t, 30*time.Second, registry.watcher.Ready)
	return registry
}

// A restart streams every delete marker in the interest bucket before its cache is ready, and
// nothing ever removed one: production's bucket holds 41,873 subjects for 40 live keys, and the
// on-prem listeners spend 17-25 s of every restart replaying the 41,833 markers (LEGION-374). One
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
	if before := bucketState(t, js, testBuckets(t).interests); before.NumSubjects != markers+live {
		t.Fatalf("the seeded bucket carries %d subjects, want %d", before.NumSubjects, markers+live)
	}

	purged, err := registry.CollectInterestMarkers(js)
	if err != nil {
		t.Fatalf("CollectInterestMarkers: %v", err)
	}
	if purged != markers {
		t.Fatalf("the pass purged %d messages, want the %d delete markers", purged, markers)
	}
	after := bucketState(t, js, testBuckets(t).interests)
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
	// A second pass has nothing below the floor and purges nothing, so every listener may run it.
	again, err := registry.CollectInterestMarkers(js)
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
	purged, err := registry.collectInterestMarkers(js, func(pass *interestPass) {
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
		t.Fatalf("collectInterestMarkers: %v", err)
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
// everything). Both reads' sequence spaces are compared for exactly this.
func TestAPassWhoseScanReadAReplacedStreamPurgesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, kv := rawInterestBucket(t, conn)
	seedMarkers(t, kv, 12)
	seedLive(t, kv, 3)
	// Before Open: the registry takes slog.Default() as its logger when it opens.
	records := captureJSONLogs(t)
	registry := openRegistry(t, conn)
	bucket := testBuckets(t).interests

	purged, err := registry.collectInterestMarkers(js, func(pass *interestPass) {
		if err := js.DeleteKeyValue(bucket); err != nil {
			t.Fatalf("delete the interest bucket: %v", err)
		}
		recreated := recreateBucket(t, js, bucket)
		seedLive(t, recreated, 2)
	})
	if err != nil {
		t.Fatalf("collectInterestMarkers: %v", err)
	}
	if purged != 0 {
		t.Fatalf("the pass purged %d messages from a replaced stream, want 0", purged)
	}
	state := bucketState(t, js, bucket)
	if state.NumSubjects != 2 || state.Msgs != 2 {
		t.Fatalf("the recreated bucket holds %d subjects and %d messages, want its 2 live keys", state.NumSubjects, state.Msgs)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "backward") {
		t.Fatalf("a replaced stream logged %v; want a WARN saying the sequence space moved backwards", refusal)
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
	bucket := testBuckets(t).interests
	before := bucketState(t, js, bucket)

	purged, err := registry.collectInterestMarkers(js, func(pass *interestPass) {
		pass.floor = before.LastSeq + 1
	})
	if err != nil {
		t.Fatalf("collectInterestMarkers: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a floor above the last sequence purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, bucket); state.Msgs != before.Msgs || state.NumSubjects != before.NumSubjects {
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
	bucket := testBuckets(t).interests
	before := bucketState(t, js, bucket)

	purged, err := registry.collectInterestMarkers(js, func(pass *interestPass) {
		// Read 1 claims a higher last sequence than the stream now reports, which is what a
		// restore from an older snapshot produces between the two reads.
		pass.before.LastSeq += 5
	})
	if err != nil {
		t.Fatalf("collectInterestMarkers: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a sequence space that moved backwards purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, bucket); state.Msgs != before.Msgs {
		t.Fatalf("a refused pass changed the bucket: %d messages, want %d", state.Msgs, before.Msgs)
	}
	refusal := jsonRecord(t, records(), "interest marker collection refused")
	if refusal["level"] != "WARN" || !strings.Contains(fmt.Sprint(refusal["reason"]), "backward") {
		t.Fatalf("a sequence space that moved backwards logged %v; want a WARN saying so", refusal)
	}
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

	if _, err := registry.CollectInterestMarkers(js); err != nil {
		t.Fatalf("CollectInterestMarkers: %v", err)
	}
	entry, err := kv.Get("ses_undecodable")
	if err != nil {
		t.Fatalf("the undecodable live key is gone after the pass: %v", err)
	}
	if entry.Revision() != undecodable {
		t.Fatalf("the undecodable live key is at revision %d, want its stored %d", entry.Revision(), undecodable)
	}
	if state := bucketState(t, js, testBuckets(t).interests); state.NumSubjects != 3 {
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
	before := bucketState(t, js, testBuckets(t).interests)

	roleKV, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).roles, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	records := captureJSONLogs(t)
	registry := &Registry{roleKV: bus.KeyValue{KeyValue: roleKV}, now: time.Now, cache: map[string]Interest{}, cacheRevisions: map[string]uint64{}}
	handle := bus.KeyValue{KeyValue: timingOutKV{KeyValue: kv, after: 2}}
	registry.watcher = kvwatch.New("interest registry", handle, registry.applyWatched, registry.resetCache)
	t.Cleanup(registry.StopWatch)
	registry.watcher.Start()
	waitFor(t, 30*time.Second, registry.watcher.Ready)

	purged, err := registry.CollectInterestMarkers(js)
	if err != nil {
		t.Fatalf("CollectInterestMarkers: %v", err)
	}
	if purged != 0 {
		t.Fatalf("a pass whose scan timed out purged %d messages, want 0", purged)
	}
	if state := bucketState(t, js, testBuckets(t).interests); state.Msgs != before.Msgs {
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
// client library: one publish to $JS.API.STREAM.PURGE.KV_<bucket>, and nothing else new. nats.go
// builds that subject, so a call site in this repository cannot settle what goes on the wire; the
// server's trace can, and a grant that denies exactly that subject shows it is the one the purge
// needs. The negative control is also the refusal path the deployed listener must survive: a NATS
// user without the capability leaves every marker where it was, logs one WARN however many passes
// run, and keeps the cache healthy (LEGION-374's permission census; the on-prem and production
// listeners connect under agent-c's `envoyNatsAuthorization`).
func TestThePurgeSendsTheStreamPurgeSubjectItsGrantMustAllow(t *testing.T) {
	// Each subtest gets its own buckets (testBuckets is keyed by the running test), so the subject
	// its grant names is computed from the same test's bucket.
	t.Run("a grant that allows it purges, and the trace names the subject", func(t *testing.T) {
		purgeSubject := "$JS.API.STREAM.PURGE.KV_" + testBuckets(t).interests
		ctr, registry, js, _ := grantedRegistry(t, nil)
		purged, err := registry.CollectInterestMarkers(js)
		if err != nil || purged == 0 {
			t.Fatalf("CollectInterestMarkers under a grant that allows the purge = %d, %v; want the markers purged", purged, err)
		}
		traced := serverLogLines(t, ctr, "[PUB "+purgeSubject)
		if len(traced) == 0 {
			t.Fatalf("the server's trace holds no publish to %s; the purge went somewhere else:\n%s",
				purgeSubject, strings.Join(serverLogLines(t, ctr, "$JS.API.STREAM."), "\n"))
		}
		for _, line := range traced {
			t.Logf("server trace: %s", strings.TrimSpace(line))
		}
		for _, line := range serverLogLines(t, ctr, "$JS.API.STREAM.PURGE") {
			if !strings.Contains(line, purgeSubject) {
				t.Fatalf("the pass published to another purge subject: %s", strings.TrimSpace(line))
			}
		}
	})

	t.Run("a grant that denies it purges nothing, warns once and stays healthy", func(t *testing.T) {
		bucket := testBuckets(t).interests
		purgeSubject := "$JS.API.STREAM.PURGE.KV_" + bucket
		records := captureJSONLogs(t)
		ctr, registry, js, kv := grantedRegistry(t, []string{purgeSubject})
		before := bucketState(t, js, bucket)
		for pass := range 2 {
			purged, err := registry.CollectInterestMarkers(js)
			if err == nil || purged != 0 {
				t.Fatalf("pass %d under a grant that denies the purge = %d, %v; want no purge and the error", pass, purged, err)
			}
			t.Logf("pass %d: %v", pass, err)
		}
		if state := bucketState(t, js, bucket); state.Msgs != before.Msgs || state.NumSubjects != before.NumSubjects {
			t.Fatalf("a refused purge changed the bucket: %d messages and %d subjects, want %d and %d",
				state.Msgs, state.NumSubjects, before.Msgs, before.NumSubjects)
		}
		refusal := jsonRecord(t, records(), "interest marker collection could not purge the bucket")
		if refusal["level"] != "WARN" {
			t.Fatalf("a refused purge logged %v, want one WARN for two passes", refusal)
		}
		violations := serverLogLines(t, ctr, "Permissions Violation for Publish to")
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
		if err := registry.Ping(); err != nil {
			t.Fatalf("Ping() = %v after a refused purge; the listener must stay healthy", err)
		}
		if _, err := kv.Get("ses_live_000"); err != nil {
			t.Fatalf("a live interest is unreadable after a refused purge: %v", err)
		}
	})
}

// grantedRegistry starts a tracing NATS server whose one nkey user may publish to everything except
// deny, opens a registry on it as the listener does (one connection, its own JetStream context) and
// seeds t's interest bucket with markers behind live keys. The JetStream context bounds its
// requests at a second, so a purge the grant refuses -- which the server answers with nothing --
// does not hold the test for the listener's own 10 s.
func grantedRegistry(t *testing.T, deny []string) (*tcnats.NATSContainer, *Registry, natsgo.JetStreamContext, natsgo.KeyValue) {
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
	js, err := conn.JetStream(natsgo.MaxWait(time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	waitFor(t, 30*time.Second, func() bool { _, err := js.AccountInfo(); return err == nil })
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	seedMarkers(t, kv, 6)
	seedLive(t, kv, 3)
	registry, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	waitFor(t, 30*time.Second, registry.watcher.Ready)
	return ctr, registry, js, kv
}

// serverLogLines is every line of the container's log holding want.
func serverLogLines(t *testing.T, ctr *tcnats.NATSContainer, want string) []string {
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
	var found []string
	for _, line := range strings.Split(string(text), "\n") {
		if strings.Contains(line, want) {
			found = append(found, line)
		}
	}
	return found
}

// recreateBucket creates bucket again after a delete, retrying while the server is still removing
// the old stream's directories from its background goroutines ("error creating store for stream").
func recreateBucket(t *testing.T, js natsgo.JetStreamContext, bucket string) natsgo.KeyValue {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Replicas: 1, Storage: natsgo.FileStorage})
		if err == nil {
			return kv
		}
		if time.Now().After(deadline) {
			t.Fatalf("recreate %s: %v", bucket, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// timingOutKV hands out a watcher over the real bucket that ends its scan the way nats.go's idle
// timer does (nats.go v1.50.0 kv.go:1145-1156): after `after` entries it puts ErrKeyWatcherTimeout
// on Error() and sends the nil marker a complete scan also sends, then stays open, as nats.go's
// watcher does — the timer gives up on the initial values, it does not end the subscription.
type timingOutKV struct {
	natsgo.KeyValue

	after int
}

func (k timingOutKV) Watch(keys string, opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(keys, opts...)
	if err != nil {
		return nil, err
	}
	timed := &timingOutWatcher{KeyWatcher: watcher, updates: make(chan natsgo.KeyValueEntry), faults: make(chan error, 1)}
	go func() {
		defer close(timed.updates)
		for range k.after {
			entry, ok := <-watcher.Updates()
			if !ok || entry == nil {
				return
			}
			timed.updates <- entry
		}
		timed.faults <- natsgo.ErrKeyWatcherTimeout
		timed.updates <- nil
		for entry := range watcher.Updates() {
			timed.updates <- entry
		}
	}()
	return timed, nil
}

func (k timingOutKV) WatchAll(opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	return k.Watch(natsgo.AllKeys, opts...)
}

type timingOutWatcher struct {
	natsgo.KeyWatcher

	updates chan natsgo.KeyValueEntry
	faults  chan error
}

func (w *timingOutWatcher) Updates() <-chan natsgo.KeyValueEntry { return w.updates }

func (w *timingOutWatcher) Error() <-chan error { return w.faults }
