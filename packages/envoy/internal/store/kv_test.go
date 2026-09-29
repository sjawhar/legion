package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// --- Unit Tests (no NATS needed — test Match() against pre-populated cache) ---

func TestMatch_FiltersByMachineID(t *testing.T) {
	r := &Registry{cache: map[string]Interest{
		"ses_a": {SessionID: "ses_a", MachineID: "machine-A", Topics: []string{"notifications.>"}},
		"ses_b": {SessionID: "ses_b", MachineID: "machine-B", Topics: []string{"notifications.>"}},
		"ses_c": {SessionID: "ses_c", MachineID: "machine-A", Topics: []string{"notifications.>"}},
	}}

	got := r.Match("machine-A", "notifications.test")
	if len(got) != 2 {
		t.Fatalf("expected 2 results for machine-A, got %d", len(got))
	}
	ids := []string{got[0].SessionID, got[1].SessionID}
	sort.Strings(ids)
	if ids[0] != "ses_a" || ids[1] != "ses_c" {
		t.Fatalf("expected [ses_a, ses_c], got %v", ids)
	}

	got = r.Match("machine-B", "notifications.test")
	if len(got) != 1 || got[0].SessionID != "ses_b" {
		t.Fatalf("expected [ses_b], got %v", got)
	}

	got = r.Match("machine-X", "notifications.test")
	if len(got) != 0 {
		t.Fatalf("expected 0 results for unknown machine, got %d", len(got))
	}
}

func TestMatch_WildcardPatterns(t *testing.T) {
	r := &Registry{cache: map[string]Interest{
		"ses_exact":   {SessionID: "ses_exact", MachineID: "m1", Topics: []string{"notifications.github.sjawhar.legion.pr"}},
		"ses_star":    {SessionID: "ses_star", MachineID: "m1", Topics: []string{"notifications.github.*.*.pr"}},
		"ses_chevron": {SessionID: "ses_chevron", MachineID: "m1", Topics: []string{"notifications.github.>"}},
		"ses_slack":   {SessionID: "ses_slack", MachineID: "m1", Topics: []string{"notifications.slack.>"}},
	}}

	got := r.Match("m1", "notifications.github.sjawhar.legion.pr")
	ids := make([]string, len(got))
	for i, item := range got {
		ids[i] = item.SessionID
	}
	sort.Strings(ids)

	if len(ids) != 3 {
		t.Fatalf("expected 3 matches (exact, star, chevron), got %d: %v", len(ids), ids)
	}
	if ids[0] != "ses_chevron" || ids[1] != "ses_exact" || ids[2] != "ses_star" {
		t.Fatalf("expected [ses_chevron, ses_exact, ses_star], got %v", ids)
	}

	// Star should not match wrong suffix
	got = r.Match("m1", "notifications.github.sjawhar.legion.issue")
	ids = make([]string, len(got))
	for i, item := range got {
		ids[i] = item.SessionID
	}
	sort.Strings(ids)
	if len(ids) != 1 || ids[0] != "ses_chevron" {
		t.Fatalf("expected only ses_chevron for .issue topic, got %v", ids)
	}
}

func TestMatch_SlackThreadIsolation(t *testing.T) {
	r := &Registry{cache: map[string]Interest{
		"ses_thread_a": {SessionID: "ses_thread_a", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.1111111111_111111.>",
		}},
		"ses_thread_b": {SessionID: "ses_thread_b", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.2222222222_222222.>",
		}},
		"ses_channel": {SessionID: "ses_channel", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.>",
		}},
	}}

	// Thread A message: only ses_thread_a and ses_channel
	gotA := r.Match("m1", "notifications.slack.T123.C456.thread.1111111111_111111.message")
	idsA := make([]string, len(gotA))
	for i, item := range gotA {
		idsA[i] = item.SessionID
	}
	sort.Strings(idsA)
	if len(idsA) != 2 || idsA[0] != "ses_channel" || idsA[1] != "ses_thread_a" {
		t.Fatalf("thread A message: expected [ses_channel, ses_thread_a], got %v", idsA)
	}

	// Thread B mention: only ses_thread_b and ses_channel
	gotB := r.Match("m1", "notifications.slack.T123.C456.thread.2222222222_222222.mention")
	idsB := make([]string, len(gotB))
	for i, item := range gotB {
		idsB[i] = item.SessionID
	}
	sort.Strings(idsB)
	if len(idsB) != 2 || idsB[0] != "ses_channel" || idsB[1] != "ses_thread_b" {
		t.Fatalf("thread B mention: expected [ses_channel, ses_thread_b], got %v", idsB)
	}

	// Channel-level message (no thread): only ses_channel
	gotC := r.Match("m1", "notifications.slack.T123.C456.message")
	idsC := make([]string, len(gotC))
	for i, item := range gotC {
		idsC[i] = item.SessionID
	}
	if len(idsC) != 1 || idsC[0] != "ses_channel" {
		t.Fatalf("channel message: expected [ses_channel], got %v", idsC)
	}
}

func TestMatch_EmptyCache(t *testing.T) {
	r := &Registry{cache: map[string]Interest{}}
	got := r.Match("m1", "notifications.test")
	if len(got) != 0 {
		t.Fatalf("expected 0 results from empty cache, got %d", len(got))
	}
}

func TestMatch_MultipleTopicsOnOneEntry(t *testing.T) {
	r := &Registry{cache: map[string]Interest{
		"ses_multi": {
			SessionID: "ses_multi",
			MachineID: "m1",
			Topics:    []string{"notifications.github.>", "notifications.slack.>"},
		},
	}}

	// Matches first topic
	got := r.Match("m1", "notifications.github.sjawhar.legion.pr")
	if len(got) != 1 || got[0].SessionID != "ses_multi" {
		t.Fatalf("expected match via github topic, got %v", got)
	}

	// Matches second topic
	got = r.Match("m1", "notifications.slack.T1.C1.mention")
	if len(got) != 1 || got[0].SessionID != "ses_multi" {
		t.Fatalf("expected match via slack topic, got %v", got)
	}

	// No match for unrelated topic
	got = r.Match("m1", "notifications.agent.ses_other")
	if len(got) != 0 {
		t.Fatalf("expected 0 matches for agent topic, got %d", len(got))
	}
}

// --- Unit Tests for mergeForUpsert (pure logic, no NATS) ---
//
// Regression coverage for the silent-fallback bug that caused Atlas's pr.11416.>
// subscription to be silently truncated to [agent-self] across 235 dropped events:
// when the interest bucket's Get returned a transient error, the original Upsert silently
// treated it the same as ErrKeyNotFound and clobbered durable state with whatever
// the heartbeat sent (only the agent topic).

func TestMergeForUpsert_SuccessfulGetMergesTopics(t *testing.T) {
	cur := Interest{
		SessionID: "ses_x",
		MachineID: "m1",
		Dir:       "/workspace",
		Topics:    []string{"topic.a", "topic.b"},
	}
	item := Interest{SessionID: "ses_x", MachineID: "m1", Dir: "/workspace"}

	result, err := mergeForUpsert(cur, nil, item, []string{"topic.c"}, 42)
	if err != nil {
		t.Fatalf("mergeForUpsert returned unexpected error: %v", err)
	}
	wantTopics := []string{"topic.a", "topic.b", "topic.c"}
	if len(result.Topics) != len(wantTopics) {
		t.Fatalf("expected %v topics, got %v", wantTopics, result.Topics)
	}
	for i, want := range wantTopics {
		if result.Topics[i] != want {
			t.Fatalf("topic[%d]: expected %s, got %s", i, want, result.Topics[i])
		}
	}
	if result.UpdatedAt != 42 {
		t.Fatalf("expected UpdatedAt=42, got %d", result.UpdatedAt)
	}
}

func TestMergeForUpsert_ErrKeyNotFoundCreatesNewEntry(t *testing.T) {
	item := Interest{
		SessionID: "ses_new",
		MachineID: "m1",
		Dir:       "/workspace",
	}

	result, err := mergeForUpsert(Interest{}, natsgo.ErrKeyNotFound, item, []string{"topic.fresh"}, 100)
	if err != nil {
		t.Fatalf("ErrKeyNotFound must be treated as new entry, not propagated: %v", err)
	}
	if result.SessionID != "ses_new" {
		t.Fatalf("expected SessionID ses_new, got %q", result.SessionID)
	}
	if result.MachineID != "m1" {
		t.Fatalf("expected MachineID m1, got %q", result.MachineID)
	}
	if len(result.Topics) != 1 || result.Topics[0] != "topic.fresh" {
		t.Fatalf("expected [topic.fresh], got %v", result.Topics)
	}
}

func TestMergeForUpsert_TransientErrorRefusesToWrite(t *testing.T) {
	// This is the regression test for the Atlas dropout bug. A transient KV error
	// (NOT ErrKeyNotFound) MUST propagate up so the caller refuses to silently
	// overwrite durable state with whatever was passed in.
	transient := errors.New("nats: connection lost")
	item := Interest{
		SessionID: "ses_atlas",
		MachineID: "m1",
		Dir:       "/workspace",
	}

	_, err := mergeForUpsert(Interest{}, transient, item, []string{"topic.heartbeat"}, 99)
	if err == nil {
		t.Fatal("transient KV error MUST be returned, not silently swallowed")
	}
	if !errors.Is(err, transient) {
		t.Fatalf("expected returned error to wrap transient (%v), got %v", transient, err)
	}
}

func TestMergeForUpsert_PreservesMachineIDAndDirFromExistingEntry(t *testing.T) {
	// Heartbeats from the plugin do not send MachineID. The cached state must win.
	cur := Interest{
		SessionID: "ses_x",
		MachineID: "example-host-mx",
		Dir:       "/home/ubuntu/example-repo/rl-eval/default",
		Topics:    []string{"notifications.agent.ses_x", "notifications.github.foo.bar.>"},
	}
	// Simulates the listener's subscribe handler call shape: MachineID is set by
	// listener config and matches cur, but heartbeat body sends only agent topic.
	item := Interest{SessionID: "ses_x", MachineID: "example-host-mx", Dir: "/home/ubuntu/example-repo/rl-eval/default"}

	result, err := mergeForUpsert(cur, nil, item, []string{"notifications.agent.ses_x"}, 200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MachineID != "example-host-mx" {
		t.Fatalf("MachineID lost: got %q", result.MachineID)
	}
	if result.Dir != "/home/ubuntu/example-repo/rl-eval/default" {
		t.Fatalf("Dir lost: got %q", result.Dir)
	}
	// CRITICAL: the github topic must survive heartbeats that send only agent topic.
	foundGithub := false
	for _, topic := range result.Topics {
		if topic == "notifications.github.foo.bar.>" {
			foundGithub = true
			break
		}
	}
	if !foundGithub {
		t.Fatalf("github topic was lost during heartbeat merge: %v", result.Topics)
	}
}

// --- Integration Tests (testcontainers NATS) ---

var (
	sharedNATSOnce sync.Once
	sharedNATSURI  string
	sharedNATSErr  error
	// sharedNATSContainer is the container the tests share, which TestMain terminates.
	sharedNATSContainer *tcnats.NATSContainer
)

func sharedTestNATSURI(t *testing.T) string {
	t.Helper()
	sharedNATSOnce.Do(func() {
		ctr, err := tcnats.Run(context.Background(), testnats.Image)
		if err != nil {
			sharedNATSErr = errors.Join(err, testcontainers.TerminateContainer(ctr))
			return
		}
		sharedNATSURI, sharedNATSErr = ctr.ConnectionString(context.Background())
		if sharedNATSErr != nil {
			sharedNATSErr = errors.Join(sharedNATSErr, testcontainers.TerminateContainer(ctr))
			return
		}
		sharedNATSContainer = ctr
	})
	if sharedNATSErr != nil {
		t.Fatalf("failed to start shared NATS: %v", sharedNATSErr)
	}
	return sharedNATSURI
}

// bucketNames are the interest and role buckets one test uses on the shared server.
type bucketNames struct{ interests, roles string }

var testBucketNames = struct {
	sync.Mutex
	next  int
	names map[testing.TB]bucketNames
}{names: map[testing.TB]bucketNames{}}

// testBuckets names buckets no other test uses, so no test deletes and recreates a bucket on the
// shared server: nats-server removes a deleted stream's directories from background goroutines,
// and a same-named bucket created right after the delete races that cleanup ("error creating
// store for stream").
func testBuckets(t testing.TB) bucketNames {
	testBucketNames.Lock()
	defer testBucketNames.Unlock()
	if names, ok := testBucketNames.names[t]; ok {
		return names
	}
	testBucketNames.next++
	names := bucketNames{
		interests: fmt.Sprintf("%s_%d", Bucket, testBucketNames.next),
		roles:     fmt.Sprintf("%s_%d", RoleBucket, testBucketNames.next),
	}
	testBucketNames.names[t] = names
	t.Cleanup(func() {
		testBucketNames.Lock()
		delete(testBucketNames.names, t)
		testBucketNames.Unlock()
	})
	return names
}

// withTestBuckets opens the registry on t's own buckets.
func withTestBuckets(t testing.TB) OpenOption {
	names := testBuckets(t)
	return func(o *openOpts) { o.interestBucket, o.roleBucket = names.interests, names.roles }
}

func connectNATS(t *testing.T) (*natsgo.Conn, func()) {
	t.Helper()
	conn := testnats.Connect(t, sharedTestNATSURI(t))
	return conn, conn.Close
}

func putInterest(t *testing.T, kv natsgo.KeyValue, item Interest) {
	t.Helper()
	buf, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("failed to marshal interest: %v", err)
	}
	if _, err := kv.Put(item.SessionID, buf); err != nil {
		t.Fatalf("failed to put interest: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for observable state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pollMatch retries Match() until the expected count is reached or timeout fires.
func pollMatch(t *testing.T, r *Registry, machineID, topic string, wantCount int, timeout time.Duration) []Interest {
	t.Helper()
	var got []Interest
	waitFor(t, timeout, func() bool {
		got = r.Match(machineID, topic)
		return len(got) == wantCount
	})
	return got
}

func TestOpen_WatchPopulatesExistingKeys(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	// Pre-populate the KV bucket before Open()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_preload", MachineID: "m1", Topics: []string{"notifications.>"}})

	// Open registry — watch() populates cache asynchronously
	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Cache is populated asynchronously by watch(); poll until ready
	got := pollMatch(t, reg, "m1", "notifications.test", 1, 5*time.Second)
	if got[0].SessionID != "ses_preload" {
		t.Fatalf("expected ses_preload, got %s", got[0].SessionID)
	}
}

func TestWatch_PropagatesUpsert(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Initially empty
	got := reg.Match("m1", "notifications.test")
	if len(got) != 0 {
		t.Fatalf("expected empty cache initially, got %d", len(got))
	}

	// Upsert writes to KV — the watcher should propagate to cache
	if _, err := reg.Upsert(Interest{SessionID: "ses_new", MachineID: "m1"}, []string{"notifications.>"}); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	// Poll with bounded timeout
	got = pollMatch(t, reg, "m1", "notifications.test", 1, 5*time.Second)
	if got[0].SessionID != "ses_new" {
		t.Fatalf("expected ses_new, got %s", got[0].SessionID)
	}
}

func TestWatch_PropagatesDelete(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	// Pre-populate
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_del", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify pre-loaded (async via watch)
	pollMatch(t, reg, "m1", "notifications.test", 1, 5*time.Second)

	// Delete via KV Delete operation (empty topics = delete all)
	if err := reg.Remove("ses_del", nil); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Poll until cache reflects the delete
	pollMatch(t, reg, "m1", "notifications.test", 0, 5*time.Second)
}

func TestWatch_PropagatesPurge(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	// Pre-populate
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_purge", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify pre-loaded (async via watch)
	pollMatch(t, reg, "m1", "notifications.test", 1, 5*time.Second)

	// Purge via NATS KV API (different from Delete — removes all revisions)
	if err := kv.Purge("ses_purge"); err != nil {
		t.Fatalf("Purge failed: %v", err)
	}

	// Poll until cache reflects the purge
	pollMatch(t, reg, "m1", "notifications.test", 0, 5*time.Second)
}

func TestMatch_IndependentOfKVAfterStartup(t *testing.T) {
	ctx := context.Background()
	ctr, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	// Safety net: a double close is a no-op.
	t.Cleanup(conn.Close)

	// Pre-populate and open
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_survive", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify cache is populated via watch() before NATS shutdown
	pollMatch(t, reg, "m1", "notifications.test", 1, 5*time.Second)

	// Kill NATS — KV is now unreachable
	conn.Close()
	if err := ctr.Terminate(ctx); err != nil {
		t.Fatalf("failed to terminate NATS: %v", err)
	}

	// Match should still return correct results from cache
	got := reg.Match("m1", "notifications.test")
	if len(got) != 1 {
		t.Fatalf("expected 1 result from cache after NATS shutdown, got %d", len(got))
	}
	if got[0].SessionID != "ses_survive" {
		t.Fatalf("expected ses_survive after shutdown, got %s", got[0].SessionID)
	}
}

func TestOpen_EmptyBucketSucceeds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed on empty bucket: %v", err)
	}

	got := reg.Match("m1", "notifications.test")
	if len(got) != 0 {
		t.Fatalf("expected 0 results from empty registry, got %d", len(got))
	}
}

// --- Cold Cache Tests (KV fallback on cache miss — regression for #367) ---
// These tests construct a Registry with an empty cache but a populated KV bucket,
// guaranteeing the cache is cold. This simulates the warm-up window after serve
// restart when watch() hasn't populated the cache yet.

// coldRegistry creates a Registry with an empty cache backed by the given KV buckets.
// Its watcher is never started, so the cache stays cold for the test's lifetime.
func coldRegistry(t *testing.T, conn *natsgo.Conn) (*Registry, natsgo.KeyValue) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).interests, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	roleKV, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).roles, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create role KV bucket: %v", err)
	}
	r := &Registry{roleKV: bus.KeyValue{KeyValue: roleKV}, now: time.Now, cache: map[string]Interest{}, cacheRevisions: map[string]uint64{}}
	r.watcher = kvwatch.New("interest registry", bus.KeyValue{KeyValue: kv}, r.applyWatched, r.resetCache)
	return r, kv
}

func TestGet_ColdCacheFallsBackToKV(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_cold", MachineID: "m1", Topics: []string{"topic.a", "topic.b"}})

	item, err := reg.Get("ses_cold")
	if err != nil {
		t.Fatalf("Get failed on cold cache: %v", err)
	}
	if item.SessionID != "ses_cold" {
		t.Fatalf("expected ses_cold, got %s", item.SessionID)
	}
	sort.Strings(item.Topics)
	if len(item.Topics) != 2 || item.Topics[0] != "topic.a" || item.Topics[1] != "topic.b" {
		t.Fatalf("expected [topic.a topic.b], got %v", item.Topics)
	}
}

func TestGet_ColdCachePopulatesCacheOnHit(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_pop", MachineID: "m1", Topics: []string{"topic.x"}})

	// First Get falls back to KV and caches the result
	if _, err := reg.Get("ses_pop"); err != nil {
		t.Fatalf("first Get failed: %v", err)
	}

	// Verify the cache was populated (Match only reads cache)
	got := reg.Match("m1", "topic.x")
	if len(got) != 1 || got[0].SessionID != "ses_pop" {
		t.Fatalf("expected Match to find cached entry, got %v", got)
	}
}

func TestGet_ColdCacheMissReturnsError(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)

	_, err := reg.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if err != natsgo.ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestUpsert_TransientGetErrorPropagatesAndDoesNotClobber(t *testing.T) {
	// Integration regression for the Atlas dropout bug: when the interest bucket's Get fails
	// transiently (not ErrKeyNotFound), Upsert MUST return the error rather
	// than silently writing a truncated state to KV.
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{
		SessionID: "ses_atlas",
		MachineID: "m1",
		Dir:       "/workspace",
		Topics:    []string{"topic.agent", "topic.github.pr.>"},
	})

	// Close the NATS connection BEFORE the cache has been warmed for this key.
	// coldRegistry skips the eager load, so the cache is empty and Upsert will
	// fall through to the interest bucket's Get, which now returns a transient error.
	conn.Close()

	_, err := reg.Upsert(
		Interest{SessionID: "ses_atlas", MachineID: "m1", Dir: "/workspace"},
		[]string{"topic.agent"}, // heartbeat-shaped payload
	)
	if err == nil {
		t.Fatal("Upsert must return error when KV is unreachable; silent fallback is the bug")
	}
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("transient KV failure must not be conflated with ErrKeyNotFound; got: %v", err)
	}
}

func TestUpsert_ColdCacheMergesWithExistingKVEntry(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)

	// Pre-populate KV with existing session that has topics A and B
	putInterest(t, kv, Interest{
		SessionID: "ses_merge",
		MachineID: "m1",
		Dir:       "/workspace",
		Topics:    []string{"topic.a", "topic.b"},
	})

	// Upsert with topic C — cache is cold, must read from KV to merge
	result, err := reg.Upsert(Interest{SessionID: "ses_merge", MachineID: "m1"}, []string{"topic.c"})
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	// Must have all three topics merged (not just the new one)
	expected := []string{"topic.a", "topic.b", "topic.c"}
	if len(result.Topics) != 3 {
		t.Fatalf("expected 3 topics after merge, got %d: %v", len(result.Topics), result.Topics)
	}
	for i, want := range expected {
		if result.Topics[i] != want {
			t.Fatalf("topic[%d]: expected %s, got %s (full: %v)", i, want, result.Topics[i], result.Topics)
		}
	}

	// Also verify MachineID and Dir were preserved from the KV entry
	if result.MachineID != "m1" {
		t.Fatalf("expected MachineID m1, got %s", result.MachineID)
	}
	if result.Dir != "/workspace" {
		t.Fatalf("expected Dir /workspace, got %s", result.Dir)
	}

	// Also verify the persisted KV entry matches the returned struct
	entry, err := kv.Get("ses_merge")
	if err != nil {
		t.Fatalf("KV Get failed after Upsert: %v", err)
	}
	var persisted Interest
	if err := json.Unmarshal(entry.Value(), &persisted); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(persisted.Topics) != 3 {
		t.Fatalf("expected 3 persisted topics, got %d: %v", len(persisted.Topics), persisted.Topics)
	}
	for i, want := range expected {
		if persisted.Topics[i] != want {
			t.Fatalf("persisted topic[%d]: expected %s, got %s", i, want, persisted.Topics[i])
		}
	}
}
func TestUpsert_ColdCacheDuplicateTopicDeduped(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_dedup", MachineID: "m1", Topics: []string{"topic.a", "topic.b"}})

	// Upsert with topic A (already exists) — should deduplicate
	result, err := reg.Upsert(Interest{SessionID: "ses_dedup", MachineID: "m1"}, []string{"topic.a"})
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	expected := []string{"topic.a", "topic.b"}
	if len(result.Topics) != len(expected) {
		t.Fatalf("expected topics %v, got %v", expected, result.Topics)
	}
	for i, want := range expected {
		if result.Topics[i] != want {
			t.Fatalf("topic[%d]: expected %s, got %s (full: %v)", i, want, result.Topics[i], result.Topics)
		}
	}
}

func TestRemove_ColdCacheReadsFromKV(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_rm", MachineID: "m1", Topics: []string{"topic.a", "topic.b", "topic.c"}})

	// Remove topic B — cache is cold, must read from KV
	if err := reg.Remove("ses_rm", []string{"topic.b"}); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify KV now has [topic.a, topic.c]
	entry, err := kv.Get("ses_rm")
	if err != nil {
		t.Fatalf("KV Get failed after Remove: %v", err)
	}
	var item Interest
	if err := json.Unmarshal(entry.Value(), &item); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(item.Topics) != 2 || item.Topics[0] != "topic.a" || item.Topics[1] != "topic.c" {
		t.Fatalf("expected [topic.a topic.c], got %v", item.Topics)
	}
}

func TestSetRole_ClaimNewRole(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)

	got, err := reg.SetRole("ses_role_a", "m1", "legion-controller", false)
	if err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	if got.SessionID != "ses_role_a" {
		t.Fatalf("expected session ses_role_a, got %s", got.SessionID)
	}
	if got.MachineID != "m1" {
		t.Fatalf("expected machine m1, got %s", got.MachineID)
	}
	if len(got.Topics) != 1 || got.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected role topic on returned interest, got %v", got.Topics)
	}

	entry, err := reg.roleKV.Get("legion-controller")
	if err != nil {
		t.Fatalf("roleKV.Get failed: %v", err)
	}
	var claim RoleClaim
	if err := json.Unmarshal(entry.Value(), &claim); err != nil {
		t.Fatalf("decode role claim: %v", err)
	}
	if claim.HolderSessionID != "ses_role_a" {
		t.Fatalf("role holder = %q, want ses_role_a", claim.HolderSessionID)
	}

	persisted, err := reg.Get("ses_role_a")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(persisted.Topics) != 1 || persisted.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected persisted role topic, got %v", persisted.Topics)
	}
}
func TestSetRole_PersistsClaimMetadata(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	if _, err := reg.SetRole("ses_role_a", "m1", "legion-controller", false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	entry, err := reg.roleKV.Get("legion-controller")
	if err != nil {
		t.Fatalf("get persisted role claim: %v", err)
	}
	var persisted struct {
		HolderSessionID   string `json:"holder_session_id"`
		ClaimedAt         int64  `json:"claimed_at"`
		PreviousSessionID string `json:"previous_session_id"`
	}
	if err := json.Unmarshal(entry.Value(), &persisted); err != nil {
		t.Fatalf("decode persisted role claim: %v", err)
	}
	if persisted.HolderSessionID != "ses_role_a" || persisted.ClaimedAt <= 0 ||
		persisted.PreviousSessionID != "" {
		t.Fatalf("persisted role claim = %+v", persisted)
	}
}

func TestSetRole_TransferRole(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_old", MachineID: "m1", Topics: []string{"notifications.role.legion-controller", "notifications.slack.>"}})
	if _, err := reg.roleKV.Put("legion-controller", []byte("ses_old")); err != nil {
		t.Fatalf("failed to seed role bucket: %v", err)
	}

	got, err := reg.SetRole("ses_new", "m2", "legion-controller", false)
	if err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	if got.SessionID != "ses_new" {
		t.Fatalf("expected session ses_new, got %s", got.SessionID)
	}
	if got.MachineID != "m2" {
		t.Fatalf("expected machine m2, got %s", got.MachineID)
	}

	oldEntry, err := kv.Get("ses_old")
	if err != nil {
		t.Fatalf("Get old holder failed: %v", err)
	}
	var oldHolder Interest
	if err := json.Unmarshal(oldEntry.Value(), &oldHolder); err != nil {
		t.Fatalf("unmarshal old holder failed: %v", err)
	}
	if len(oldHolder.Topics) != 1 || oldHolder.Topics[0] != "notifications.slack.>" {
		t.Fatalf("expected old holder to lose only role topic, got %v", oldHolder.Topics)
	}

	newEntry, err := kv.Get("ses_new")
	if err != nil {
		t.Fatalf("Get new holder failed: %v", err)
	}
	var newHolder Interest
	if err := json.Unmarshal(newEntry.Value(), &newHolder); err != nil {
		t.Fatalf("unmarshal new holder failed: %v", err)
	}
	if len(newHolder.Topics) != 1 || newHolder.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected new holder to gain role topic, got %v", newHolder.Topics)
	}

	entry, err := reg.roleKV.Get("legion-controller")
	if err != nil {
		t.Fatalf("roleKV.Get failed: %v", err)
	}
	var claim RoleClaim
	if err := json.Unmarshal(entry.Value(), &claim); err != nil {
		t.Fatalf("decode role claim: %v", err)
	}
	if claim.HolderSessionID != "ses_new" {
		t.Fatalf("role holder = %q, want ses_new", claim.HolderSessionID)
	}
}

func TestSetRole_Idempotent(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_same", MachineID: "m1", Topics: []string{"notifications.role.legion-controller"}})
	if _, err := reg.roleKV.Put("legion-controller", []byte("ses_same")); err != nil {
		t.Fatalf("failed to seed role bucket: %v", err)
	}

	got, err := reg.SetRole("ses_same", "m1", "legion-controller", false)
	if err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	if len(got.Topics) != 1 || got.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected single role topic, got %v", got.Topics)
	}

	persisted, err := reg.Get("ses_same")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(persisted.Topics) != 1 || persisted.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected persisted single role topic, got %v", persisted.Topics)
	}
}

func TestRemove_ReleasesRoleClaimAfterSameSessionReplacement(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	if _, err := reg.SetRole("ses_role", "m1", "legion-controller", false); err != nil {
		t.Fatalf("SetRole controller: %v", err)
	}
	if _, err := reg.SetRole("ses_role", "m1", "legion-reviewer", false); err != nil {
		t.Fatalf("SetRole reviewer: %v", err)
	}
	if err := reg.Remove("ses_role", []string{"notifications.role.legion-controller"}); err != nil {
		t.Fatalf("Remove controller role: %v", err)
	}

	if _, err := reg.roleKV.Get("legion-controller"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("controller role holder still exists: %v", err)
	}
}

func TestSetRole_OldHolderMissing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	if _, err := reg.roleKV.Put("legion-controller", []byte("ses_missing")); err != nil {
		t.Fatalf("failed to seed role bucket: %v", err)
	}

	got, err := reg.SetRole("ses_fresh", "m1", "legion-controller", false)
	if err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	if got.SessionID != "ses_fresh" {
		t.Fatalf("expected session ses_fresh, got %s", got.SessionID)
	}
	if len(got.Topics) != 1 || got.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected returned role topic, got %v", got.Topics)
	}

	assertRoleHolder(t, reg, "legion-controller", "ses_fresh")
}

// --- Reaper Tests (cross-reference sessions for stale interest cleanup) ---

func TestInterestReaper(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)

	// Create interests for alive and dead sessions (both updated 10 min ago)
	now := time.Now().UnixMilli()
	putInterest(t, kv, Interest{SessionID: "ses_alive", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: now - 600_000})
	putInterest(t, kv, Interest{SessionID: "ses_dead", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: now - 600_000})

	// Warm cache via Get (coldRegistry has no watch())
	if _, err := reg.Get("ses_alive"); err != nil {
		t.Fatalf("Get ses_alive failed: %v", err)
	}
	if _, err := reg.Get("ses_dead"); err != nil {
		t.Fatalf("Get ses_dead failed: %v", err)
	}

	// Reap with 0 grace window — all dead sessions should be reaped
	count, err := reg.Reap(func(sessionID string) bool {
		return sessionID == "ses_alive"
	}, 0)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 reaped, got %d", count)
	}

	// ses_alive should still exist in KV
	if _, err := kv.Get("ses_alive"); err != nil {
		t.Fatalf("ses_alive should still exist: %v", err)
	}

	// ses_dead should be deleted from KV
	if _, err := kv.Get("ses_dead"); err != natsgo.ErrKeyNotFound {
		t.Fatalf("ses_dead should be deleted, got err: %v", err)
	}
}
func TestReapKeepsRoleClaimsForSessionRecovery(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_reap_role"
		role      = "legion-controller"
	)
	if _, err := reg.SetRole(sessionID, "example-host", role, false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	stale, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get role interest: %v", err)
	}
	stale.UpdatedAt = time.Now().Add(-time.Minute).UnixMilli()
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale interest: %v", err)
	}
	entry, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("get durable interest: %v", err)
	}
	revision, err := reg.watcher.KV().Update(sessionID, raw, entry.Revision())
	if err != nil {
		t.Fatalf("make interest stale: %v", err)
	}
	reg.mu.Lock()
	reg.cacheInterestLocked(sessionID, stale, revision)
	reg.mu.Unlock()

	count, err := reg.Reap(func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if count != 1 {
		t.Fatalf("reaped interests = %d, want 1", count)
	}
	assertRoleHolder(t, reg, role, sessionID)
}
func TestReapRoleClaimsDropsDeadHolderAfterTTL(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_dead_role"
		role      = "legion-controller"
	)
	if _, err := reg.SetRole(sessionID, "example-host", role, false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	count, err := reg.ReapRoleClaims(func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("ReapRoleClaims: %v", err)
	}
	if count != 1 {
		t.Fatalf("reaped role claims = %d, want 1", count)
	}
	assertRoleHolder(t, reg, role, "")
}

func TestReleaseExpiredRoleClaimDoesNotReportReleaseAfterConcurrentChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	registry, _ := coldRegistry(t, conn)
	const role = "legion-controller"
	if _, err := registry.SetRole("ses_lapsed", "example-host", role, false); err != nil {
		t.Fatalf("seed lapsed role claim: %v", err)
	}
	roleKV := registry.roleKV
	registry.roleKV = bus.KeyValue{KeyValue: &racingDeleteKeyValue{
		KeyValue: roleKV,
		beforeFirstDelete: func() {
			entry, err := roleKV.Get(role)
			if err != nil {
				t.Fatalf("read lapsed role claim: %v", err)
			}
			replacement, err := json.Marshal(RoleClaim{
				HolderSessionID: "ses_replacement",
				ClaimedAt:       time.Now().UnixMilli(),
			})
			if err != nil {
				t.Fatalf("marshal replacement role claim: %v", err)
			}
			if _, err := roleKV.Update(role, replacement, entry.Revision()); err != nil {
				t.Fatalf("replace lapsed role claim: %v", err)
			}
		},
		err: natsgo.ErrKeyExists,
	}}

	release, err := registry.ReleaseExpiredRoleClaim(role, "ses_lapsed", 0)
	if err != nil {
		t.Fatalf("release lapsed role claim: %v", err)
	}
	if release != ExpiredRoleClaimSuperseded {
		t.Fatalf("release outcome = %v, want superseded", release)
	}
	assertRoleHolder(t, registry, role, "ses_replacement")
}

func TestReleaseExpiredRoleClaimDoesNotReportReleaseAfterConcurrentRemoval(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	registry, _ := coldRegistry(t, conn)
	const role = "legion-controller"
	if _, err := registry.SetRole("ses_lapsed", "example-host", role, false); err != nil {
		t.Fatalf("seed lapsed role claim: %v", err)
	}
	roleKV := registry.roleKV
	registry.roleKV = bus.KeyValue{KeyValue: &racingDeleteKeyValue{
		KeyValue: roleKV,
		beforeFirstDelete: func() {
			entry, err := roleKV.Get(role)
			if err != nil {
				t.Fatalf("read lapsed role claim: %v", err)
			}
			if err := roleKV.Delete(role, natsgo.LastRevision(entry.Revision())); err != nil {
				t.Fatalf("remove lapsed role claim: %v", err)
			}
		},
		err: natsgo.ErrKeyNotFound,
	}}

	release, err := registry.ReleaseExpiredRoleClaim(role, "ses_lapsed", 0)
	if err != nil {
		t.Fatalf("release lapsed role claim: %v", err)
	}
	if release != ExpiredRoleClaimMissing {
		t.Fatalf("release outcome = %v, want missing", release)
	}
	assertRoleHolder(t, registry, role, "")
}

func TestReleaseExpiredRoleClaimReturnsErrorWithoutRelease(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	registry, _ := coldRegistry(t, conn)
	const role = "legion-controller"
	if _, err := registry.SetRole("ses_lapsed", "example-host", role, false); err != nil {
		t.Fatalf("seed lapsed role claim: %v", err)
	}
	releaseErr := errors.New("injected conditional delete failure")
	roleKV := registry.roleKV
	registry.roleKV = bus.KeyValue{KeyValue: &racingDeleteKeyValue{
		KeyValue:          roleKV,
		beforeFirstDelete: func() {},
		err:               releaseErr,
	}}

	release, err := registry.ReleaseExpiredRoleClaim(role, "ses_lapsed", 0)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("release error = %v, want %v", err, releaseErr)
	}
	if release != ExpiredRoleClaimRetained {
		t.Fatalf("release outcome = %v, want retained", release)
	}
	assertRoleHolder(t, registry, role, "ses_lapsed")
}

func TestReaperGraceWindow(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)

	// Create interest updated 2 min ago — session is dead but within 10 min grace window
	now := time.Now().UnixMilli()
	putInterest(t, kv, Interest{SessionID: "ses_recent", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: now - 120_000})

	// Warm cache
	if _, err := reg.Get("ses_recent"); err != nil {
		t.Fatalf("Get ses_recent failed: %v", err)
	}

	// Reap with 10 min grace window — ses_recent updated only 2 min ago, should survive
	count, err := reg.Reap(func(string) bool { return false }, 10*time.Minute)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 reaped (within grace window), got %d", count)
	}

	// ses_recent should still exist in KV
	if _, err := kv.Get("ses_recent"); err != nil {
		t.Fatalf("ses_recent should still exist within grace window: %v", err)
	}
}

func TestPing_HealthyConnReturnsNil(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping on healthy conn returned error: %v", err)
	}
}

func TestPing_ClosedConnReturnsError(t *testing.T) {
	// A closed KV handle must remain observable through Ping so /healthz can
	// report the unavailable dependency while NATS reconnects.
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if err := reg.Ping(); err != nil {
		t.Fatalf("sanity: Ping before close returned error: %v", err)
	}

	conn.Close()

	if err := reg.Ping(); err == nil {
		t.Fatal("Ping after conn close should return error")
	}
}

// --- Cache readiness tests ---
//
// Follow-up to PR #610. The Upsert silent-fallback fix doesn't address the
// initial-cache-warmup race: after Open() returns, watch() populates the cache
// asynchronously, and events arriving in that window get "no matching
// interests" even when the durable KV entry has subscribers. This was ~36/235
// of Atlas's observed drops (the 07:39:38 burst right at listener restart).

func TestWaitForCacheReady_ReturnsAfterInitialScan(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady timed out on a healthy registry: %v", err)
	}
}

func TestWaitForCacheReady_PrePopulatedKVIsVisibleAfterReady(t *testing.T) {
	// The whole point of this gate: after WaitForCacheReady returns, every
	// existing KV entry must be findable via Match without falling through to
	// "no matching interests".
	conn, cleanup := connectNATS(t)
	defer cleanup()

	// Pre-populate KV via a temporary registry, then Close conn to release
	// any watcher state.
	reg1, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg1.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("first WaitForCacheReady: %v", err)
	}
	if _, err := reg1.Upsert(Interest{SessionID: "ses_pre", MachineID: "m1"}, []string{"topic.pre"}); err != nil {
		t.Fatalf("pre-populate Upsert failed: %v", err)
	}

	// Second registry on the same bucket — simulates listener restart against
	// existing KV state.
	reg2, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := reg2.WaitForCacheReady(ctx2); err != nil {
		t.Fatalf("second WaitForCacheReady: %v", err)
	}

	// Critical: Match must find the pre-populated entry IMMEDIATELY after
	// WaitForCacheReady returns (no further sleeps).
	got := reg2.Match("m1", "topic.pre")
	if len(got) != 1 || got[0].SessionID != "ses_pre" {
		t.Fatalf("expected pre-populated entry visible after WaitForCacheReady; got %v", got)
	}
}

func TestRemoveWritesThroughCache(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const sessionID = "ses_remove"
	if _, err := reg.Upsert(
		Interest{SessionID: sessionID, MachineID: "example-host"},
		[]string{"notifications.a", "notifications.b"},
	); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	if err := reg.Remove(sessionID, []string{"notifications.b"}); err != nil {
		t.Fatalf("unsubscribe failed: %v", err)
	}
	if _, err := reg.Upsert(
		Interest{SessionID: sessionID, MachineID: "example-host"},
		[]string{"notifications.a"},
	); err != nil {
		t.Fatalf("heartbeat re-registration failed: %v", err)
	}

	item, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get after unsubscribe and heartbeat: %v", err)
	}
	assertInterestTopics(t, item, "notifications.a")
	if got := reg.Match("example-host", "notifications.b"); len(got) != 0 {
		t.Fatalf("removed topic must not route after heartbeat, got %v", got)
	}
}

func TestRemoveAllClearsCache(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const sessionID = "ses_remove_all"
	if _, err := reg.Upsert(
		Interest{SessionID: sessionID, MachineID: "example-host"},
		[]string{"notifications.a", "notifications.b"},
	); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	if err := reg.Remove(sessionID, nil); err != nil {
		t.Fatalf("unsubscribe all failed: %v", err)
	}

	if _, err := reg.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get after unsubscribe all error = %v, want ErrKeyNotFound", err)
	}
	if got := reg.Match("example-host", "notifications.a"); len(got) != 0 {
		t.Fatalf("unsubscribed session must not route, got %v", got)
	}
}
func TestRemoveAllReleasesRoleClaims(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_remove_all_role"
		role      = "legion-controller"
	)
	if _, err := reg.SetRole(sessionID, "example-host", role, false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if err := reg.Remove(sessionID, nil); err != nil {
		t.Fatalf("Remove all: %v", err)
	}
	assertRoleHolder(t, reg, role, "")
}
func TestRemoveAllReleasesRoleClaimMissingFromInterest(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_remove_orphaned_role"
		role      = "legion-controller"
	)
	if _, err := reg.Upsert(Interest{SessionID: sessionID, MachineID: "example-host"}, []string{"notifications.agent." + sessionID}); err != nil {
		t.Fatalf("Upsert interest: %v", err)
	}
	if _, err := reg.roleKV.Put(role, []byte(sessionID)); err != nil {
		t.Fatalf("seed orphaned role claim: %v", err)
	}
	if err := reg.Remove(sessionID, nil); err != nil {
		t.Fatalf("Remove all: %v", err)
	}
	assertRoleHolder(t, reg, role, "")
}

func TestSetRoleRollsBackWhenInterestUpsertFails(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
	)
	putInterest(t, kv, Interest{
		SessionID: oldSession,
		MachineID: "example-host",
		Topics:    []string{roleTopic, "notifications.slack.>"},
	})
	if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
		t.Fatalf("seed role claim: %v", err)
	}

	useKV(t, reg, &failingKeyValue{
		KeyValue:  kv,
		failPutAt: 1,
		err:       errors.New("injected interest write failure"),
	})
	if _, err := reg.SetRole(newSession, "example-host", role, false); err == nil {
		t.Fatal("SetRole must return the failed interest upsert")
	}

	assertRoleHolder(t, reg, role, oldSession)
	assertInterestTopicsForSession(t, reg, oldSession, roleTopic, "notifications.slack.>")
	assertInterestMissing(t, reg, newSession)
}

func TestSetRoleLeavesInterestWhenRoleWriteFails(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
	)
	putInterest(t, kv, Interest{
		SessionID: oldSession,
		MachineID: "example-host",
		Topics:    []string{roleTopic, "notifications.slack.>"},
	})
	if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
		t.Fatalf("seed role claim: %v", err)
	}

	reg.roleKV = bus.KeyValue{KeyValue: &failingKeyValue{
		KeyValue:     reg.roleKV,
		failUpdateAt: 1,
		err:          errors.New("injected role update failure"),
	}}
	if _, err := reg.SetRole(newSession, "example-host", role, false); err == nil {
		t.Fatal("SetRole must return the failed role update")
	}

	assertRoleHolder(t, reg, role, oldSession)
	assertInterestTopicsForSession(t, reg, oldSession, roleTopic, "notifications.slack.>")
	assertInterestMissing(t, reg, newSession)
}

func TestSetRoleRollsBackWhenRoleCreateFails(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_new"
		role      = "legion-controller"
	)
	reg.roleKV = bus.KeyValue{KeyValue: &failingKeyValue{
		KeyValue:     reg.roleKV,
		failCreateAt: 1,
		err:          errors.New("injected role create failure"),
	}}
	if _, err := reg.SetRole(sessionID, "example-host", role, false); err == nil {
		t.Fatal("SetRole must return the failed role create")
	}

	assertRoleHolder(t, reg, role, "")
	assertInterestMissing(t, reg, sessionID)
}

func TestSetRoleReturnsErrorAfterOldHolderCleanupFails(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
	)
	putInterest(t, kv, Interest{
		SessionID: oldSession,
		MachineID: "example-host",
		Topics:    []string{roleTopic, "notifications.slack.>"},
	})
	if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
		t.Fatalf("seed role claim: %v", err)
	}

	useKV(t, reg, &failingKeyValue{
		KeyValue:  kv,
		failPutAt: 2,
		err:       errors.New("injected old-holder cleanup failure"),
	})
	if _, err := reg.SetRole(newSession, "example-host", role, false); err == nil {
		t.Fatal("SetRole must return the failed old-holder cleanup")
	}

	assertRoleHolder(t, reg, role, newSession)
	assertInterestTopicsForSession(t, reg, newSession, roleTopic)
	assertInterestTopicsForSession(t, reg, oldSession, roleTopic, "notifications.slack.>")
	for _, want := range []string{"level=WARN", "role=legion-controller", "old_session_id=ses_old", "new_session_id=ses_new"} {
		waitFor(t, 5*time.Second, func() bool {
			return strings.Contains(logs.String(), want)
		})
	}
	t.Logf("old-holder cleanup warning: %s", strings.TrimSpace(logs.String()))
}

func TestWatcherEvictsMalformedValue(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const (
		sessionID = "ses_malformed"
		machineID = "example-host"
		topic     = "notifications.malformed"
	)
	if _, err := reg.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{topic}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pollMatch(t, reg, machineID, topic, 1, 5*time.Second)

	revision, err := reg.watcher.KV().Put(sessionID, []byte("{"))
	if err != nil {
		t.Fatalf("put malformed value: %v", err)
	}
	pollMatch(t, reg, machineID, topic, 0, 5*time.Second)
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("Get must not return a route after its watched value is malformed")
	}

	for _, want := range []string{
		"level=WARN",
		"key=" + sessionID,
		"revision=" + strconv.FormatUint(revision, 10),
	} {
		waitFor(t, 5*time.Second, func() bool {
			return strings.Contains(logs.String(), want)
		})
	}
	t.Logf("interest watcher malformed-value warning: %s", strings.TrimSpace(logs.String()))
}

// useKV rebuilds the registry's watcher over kv, a wrapper of its interest bucket handle, so the
// registry writes interests through kv from here on. The replaced watcher is stopped first.
func useKV(t *testing.T, r *Registry, kv natsgo.KeyValue) {
	t.Helper()
	r.watcher.Stop()
	r.watcher = kvwatch.New("interest registry", bus.KeyValue{KeyValue: kv}, r.applyWatched, r.resetCache)
	r.watcher.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
	}
}

type failingKeyValue struct {
	natsgo.KeyValue

	failPutAt    int
	failCreateAt int
	failUpdateAt int
	puts         int
	creates      int
	updates      int
	err          error
}

func (kv *failingKeyValue) Put(key string, value []byte) (uint64, error) {
	kv.puts++
	if kv.failPutAt == kv.puts {
		return 0, kv.err
	}
	return kv.KeyValue.Put(key, value)
}

func (kv *failingKeyValue) Create(key string, value []byte) (uint64, error) {
	kv.creates++
	if kv.failCreateAt == kv.creates {
		return 0, kv.err
	}
	return kv.KeyValue.Create(key, value)
}

func (kv *failingKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	kv.updates++
	if kv.failUpdateAt == kv.updates {
		return 0, kv.err
	}
	return kv.KeyValue.Update(key, value, revision)
}

func assertRoleHolder(t *testing.T, reg *Registry, role, want string) {
	t.Helper()
	got, err := reg.RoleHolder(role)
	if err != nil {
		t.Fatalf("RoleHolder(%q): %v", role, err)
	}
	if got != want {
		t.Fatalf("RoleHolder(%q) = %q, want %q", role, got, want)
	}
}

func assertInterestTopicsForSession(t *testing.T, reg *Registry, sessionID string, want ...string) {
	t.Helper()
	item, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", sessionID, err)
	}
	assertInterestTopics(t, item, want...)
}

func assertInterestTopics(t *testing.T, item Interest, want ...string) {
	t.Helper()
	got := append([]string(nil), item.Topics...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(got) != len(sortedWant) {
		t.Fatalf("topics = %v, want %v", got, sortedWant)
	}
	for i := range sortedWant {
		if got[i] != sortedWant[i] {
			t.Fatalf("topics = %v, want %v", got, sortedWant)
		}
	}
}

func assertInterestMissing(t *testing.T, reg *Registry, sessionID string) {
	t.Helper()
	if _, err := reg.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%q) error = %v, want ErrKeyNotFound", sessionID, err)
	}
}

func captureRegistryLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestSetRoleTreatsSameSessionCASConflictAsConcurrentSuccess(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const (
		sessionID = "ses_role"
		role      = "legion-controller"
		roleTopic = "notifications.role.legion-controller"
	)
	racingKV := &racingCreateKeyValue{KeyValue: reg.roleKV}
	reg.roleKV = bus.KeyValue{KeyValue: racingKV}

	var concurrent Interest
	var concurrentErr error
	racingKV.beforeFirstCreate = func() {
		concurrent, concurrentErr = reg.SetRole(sessionID, "example-host", role, false)
	}
	first, firstErr := reg.SetRole(sessionID, "example-host", role, false)

	if concurrentErr != nil {
		t.Fatalf("concurrent SetRole: %v", concurrentErr)
	}
	if firstErr != nil {
		t.Fatalf("racing SetRole: %v", firstErr)
	}
	assertInterestTopics(t, concurrent, roleTopic)
	assertInterestTopics(t, first, roleTopic)
	assertRoleHolder(t, reg, role, sessionID)
	assertInterestTopicsForSession(t, reg, sessionID, roleTopic)
}

func TestSetRoleSoft_TakesUnheldAndOwnAndSupersedableRoles(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const role = "sre"

	// Unheld: a soft claim takes it.
	if _, err := reg.SetRole("ses_first", "m1", role, true); err != nil {
		t.Fatalf("soft claim of unheld role: %v", err)
	}
	assertRoleHolder(t, reg, role, "ses_first")

	// Own: a soft re-assert is a no-op success.
	if _, err := reg.SetRole("ses_first", "m1", role, true); err != nil {
		t.Fatalf("soft re-assert of own role: %v", err)
	}
	assertRoleHolder(t, reg, role, "ses_first")

	// Supersedable: the caller vouches that ses_first is dead; the claim moves.
	if _, err := reg.SetRole("ses_resumed", "m1", role, true, "ses_first"); err != nil {
		t.Fatalf("soft claim over supersedable holder: %v", err)
	}
	assertRoleHolder(t, reg, role, "ses_resumed")
	assertInterestTopicsForSession(t, reg, "ses_resumed", "notifications.role.sre")
}

func TestSetRoleSoft_RefusesLiveHolderAndLeavesClaimUntouched(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const role = "sre"
	if _, err := reg.SetRole("ses_live", "m1", role, false); err != nil {
		t.Fatalf("seed holder: %v", err)
	}

	_, err := reg.SetRole("ses_stale_transcript", "m1", role, true)
	var held *ErrRoleHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *ErrRoleHeld, got %v", err)
	}
	if held.Role != role || held.Holder != "ses_live" {
		t.Fatalf("ErrRoleHeld = %+v, want role=%s holder=ses_live", held, role)
	}
	assertRoleHolder(t, reg, role, "ses_live")
	// The refused claimant gained no role topic: nothing was written for it.
	if _, err := reg.Get("ses_stale_transcript"); err == nil {
		t.Fatal("refused soft claim left an interest row behind")
	}

	// A hard claim from the same session still wins: soft is opt-in per call.
	if _, err := reg.SetRole("ses_stale_transcript", "m1", role, false); err != nil {
		t.Fatalf("hard claim after refusal: %v", err)
	}
	assertRoleHolder(t, reg, role, "ses_stale_transcript")
}

func TestSetRoleOldHolderCleanupDoesNotDeleteNewerClaim(t *testing.T) {
	// Interleaving: role at A. B claims — its role-row CAS succeeds (row -> B)
	// — and before B's cleanup of A runs, A re-claims (row -> A). B's cleanup
	// must not delete A's fresh claim. The previous implementation released
	// through the role row whenever its holder equalled the old session, which
	// is exactly A here, so the role ended unheld with both calls succeeding.
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	const role = "release-captain"
	if _, err := reg.SetRole("ses_a", "m1", role, false); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	// B's role-row Update is the CAS write; fire A's re-claim right after it
	// succeeds and before B proceeds to old-holder cleanup.
	racing := &afterUpdateKeyValue{KeyValue: reg.roleKV}
	reg.roleKV = bus.KeyValue{KeyValue: racing}
	racing.after = func() {
		racing.after = nil
		if _, err := reg.SetRole("ses_a", "m1", role, false); err != nil {
			t.Fatalf("A re-claim during B's claim: %v", err)
		}
	}
	if _, err := reg.SetRole("ses_b", "m1", role, false); err != nil {
		t.Fatalf("B claim: %v", err)
	}

	// A claimed last: A holds the role and the row still exists.
	assertRoleHolder(t, reg, role, "ses_a")
}

type afterUpdateKeyValue struct {
	natsgo.KeyValue

	after func()
}

func (kv *afterUpdateKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	revision, err := kv.KeyValue.Update(key, value, last)
	if err == nil && kv.after != nil {
		kv.after()
	}
	return revision, err
}

func TestRemoveDoesNotOverwriteNewerWatcherValue(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const (
		sessionID = "ses_revision"
		machineID = "example-host"
	)
	if _, err := reg.Upsert(
		Interest{SessionID: sessionID, MachineID: machineID},
		[]string{"notifications.a", "notifications.b"},
	); err != nil {
		t.Fatalf("initial Upsert: %v", err)
	}
	pollMatch(t, reg, machineID, "notifications.a", 1, 5*time.Second)

	interestKV := reg.watcher.KV()
	useKV(t, reg, &interleavingPutKeyValue{
		KeyValue: interestKV,
		afterFirstPut: func() {
			putInterest(t, interestKV, Interest{
				SessionID: sessionID,
				MachineID: machineID,
				Topics:    []string{"notifications.remote"},
			})
			pollMatch(t, reg, machineID, "notifications.remote", 1, 5*time.Second)
		},
	})
	if err := reg.Remove(sessionID, []string{"notifications.b"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	item, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get after interleaving: %v", err)
	}
	assertInterestTopics(t, item, "notifications.remote")
	if got := reg.Match(machineID, "notifications.a"); len(got) != 0 {
		t.Fatalf("older write-through must not restore notifications.a: %v", got)
	}
}

type racingCreateKeyValue struct {
	natsgo.KeyValue

	creates           int
	beforeFirstCreate func()
}

func (kv *racingCreateKeyValue) Create(key string, value []byte) (uint64, error) {
	kv.creates++
	if kv.creates == 1 {
		kv.beforeFirstCreate()
		return 0, natsgo.ErrKeyExists
	}
	return kv.KeyValue.Create(key, value)
}

type racingDeleteKeyValue struct {
	natsgo.KeyValue

	deletes           int
	beforeFirstDelete func()
	err               error
}

func (kv *racingDeleteKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.deletes++
	if kv.deletes == 1 {
		kv.beforeFirstDelete()
		return kv.err
	}
	return kv.KeyValue.Delete(key, opts...)
}

type interleavingPutKeyValue struct {
	natsgo.KeyValue

	puts          int
	afterFirstPut func()
}

func (kv *interleavingPutKeyValue) Put(key string, value []byte) (uint64, error) {
	kv.puts++
	revision, err := kv.KeyValue.Put(key, value)
	if err != nil {
		return 0, err
	}
	if kv.puts == 1 {
		kv.afterFirstPut()
	}
	return revision, nil
}

func TestDeleteHistoryFailureSuppressesStaleWatcherUpdate(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const (
		sessionID = "ses_delete"
		machineID = "example-host"
		topic     = "notifications.delete"
	)
	item, err := reg.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{topic})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	entry, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("Get seeded interest: %v", err)
	}

	useKV(t, reg, &historyFailKeyValue{
		KeyValue: kv,
		err:      errors.New("injected history failure"),
	})
	if err := reg.removeInterestTopics(sessionID, nil); err != nil {
		t.Fatalf("remove all: %v", err)
	}

	// This is the same cache mutation performed by a WatchAll update that was
	// queued before the delete marker. Its older revision must stay rejected.
	reg.mu.Lock()
	reg.cacheInterestLocked(sessionID, item, entry.Revision())
	reg.mu.Unlock()

	if got := reg.Match(machineID, topic); len(got) != 0 {
		t.Fatalf("stale watcher update restored deleted route: %v", got)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("Get returned a session restored by a stale watcher update")
	}
}

type historyFailKeyValue struct {
	natsgo.KeyValue

	err error
}

func (kv *historyFailKeyValue) History(string, ...natsgo.WatchOpt) ([]natsgo.KeyValueEntry, error) {
	return nil, kv.err
}

// TestMain terminates the NATS container this package's tests share once they have all run.
// Nothing else would: CI disables Ryuk, and without it a container outlives the test binary.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedNATSContainer != nil {
		if err := testcontainers.TerminateContainer(sharedNATSContainer); err != nil {
			fmt.Fprintf(os.Stderr, "terminate the shared NATS container: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

// The registry's clock stamps what it records: an interest's UpdatedAt and a role claim's
// ClaimedAt come from the clock Open was given, as the grace window does.
func TestTheRegistryClockStampsWhatItRecords(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t), WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	interest, err := reg.Upsert(Interest{SessionID: "ses_clock", MachineID: "m1"}, []string{"notifications.agent.ses_clock"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if interest.UpdatedAt != at.UnixMilli() {
		t.Fatalf("interest UpdatedAt = %d, want the registry clock's %d", interest.UpdatedAt, at.UnixMilli())
	}
	if _, err := reg.SetRole("ses_clock", "m1", "clock-role", false); err != nil {
		t.Fatalf("set role: %v", err)
	}
	claim, err := reg.RoleClaim("clock-role")
	if err != nil {
		t.Fatalf("role claim: %v", err)
	}
	if claim.ClaimedAt != at.UnixMilli() {
		t.Fatalf("role ClaimedAt = %d, want the registry clock's %d", claim.ClaimedAt, at.UnixMilli())
	}
}

// A rebuild onto an interest bucket that was deleted and created again refills the cache from the
// new bucket. The recreated stream numbers its revisions from 1, so a cache still fenced by the
// old bucket's revisions would keep an old value for every key the new bucket has not yet written
// past, and a key the new bucket does not hold at all.
func TestRewatchOntoARecreatedBucketRefillsTheCache(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	for i := range 20 {
		if _, err := reg.Upsert(Interest{SessionID: "ses_a", MachineID: "m1"}, []string{fmt.Sprintf("notifications.old.%d", i)}); err != nil {
			t.Fatalf("upsert ses_a: %v", err)
		}
	}
	if _, err := reg.Upsert(Interest{SessionID: "ses_ghost", MachineID: "m1"}, []string{"notifications.ghost"}); err != nil {
		t.Fatalf("upsert ses_ghost: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(reg.Match("m1", "notifications.ghost")) == 1 })

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	bucket := testBuckets(t).interests
	if err := js.DeleteKeyValue(bucket); err != nil {
		t.Fatalf("delete the interest bucket: %v", err)
	}
	recreated, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("recreate the interest bucket: %v", err)
	}
	putInterest(t, recreated, Interest{SessionID: "ses_a", MachineID: "m1", Topics: []string{"notifications.new"}})
	putInterest(t, recreated, Interest{SessionID: "ses_b", MachineID: "m1", Topics: []string{"notifications.b"}})
	if err := reg.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return len(reg.Match("m1", "notifications.new")) == 1 && len(reg.Match("m1", "notifications.b")) == 1
	})
	if got := reg.Match("m1", "notifications.ghost"); len(got) != 0 {
		t.Fatalf("the cache kept a session the recreated bucket does not hold: %v", got)
	}
}

// A Rewatch that cannot open the role bucket on the new connection moves nothing: the interest
// watcher and both bucket handles stay on the connection they were on, so the registry keeps
// working there instead of writing interests through one connection and roles through another.
func TestARewatchThatCannotOpenTheRoleBucketMovesNothing(t *testing.T) {
	first, closeFirst := connectNATS(t)
	defer closeFirst()
	reg, err := Open(first, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	js, err := first.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(testBuckets(t).roles); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	second, closeSecond := connectNATS(t)
	if err := reg.Rewatch(second); err == nil {
		t.Fatal("Rewatch onto a connection without the role bucket succeeded")
	}
	closeSecond()
	if _, err := reg.Upsert(Interest{SessionID: "ses_still_first", MachineID: "m1"}, []string{"notifications.agent.x"}); err != nil {
		t.Fatalf("Upsert after the failed Rewatch: %v; the interest handle moved to the closed connection", err)
	}
	if err := reg.WatchErr(); err != nil {
		t.Fatalf("WatchErr after the failed Rewatch: %v; the interest watcher moved to the closed connection", err)
	}
}

// Opening the registry snapshots the revision of every stored role claim, so a claim restored by a
// restart keeps its holder's grace while the bucket still holds the revision the restart read. It
// must read them in one pass over the bucket rather than one round trip per key, and the snapshot
// it ends up with must be the bucket's: every claim at the revision stored for it, no entry for a
// deleted key, and the grace those revisions exist for.
func TestOpeningTheRegistryReadsEveryRoleRevisionWithoutAGetPerKey(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	names := testBuckets(t)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: names.roles, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	const claims = 64
	want := map[string]uint64{}
	for i := range claims + 1 {
		role := fmt.Sprintf("role-%03d", i)
		revision := putRoleClaim(t, rawRoles, role, fmt.Sprintf("ses_%03d", i))
		want[role] = revision
	}
	// A deleted claim leaves a marker on its subject, which the bucket keeps until something
	// purges it. The marker is not a claim and gets no grace.
	deleted := fmt.Sprintf("role-%03d", claims)
	if err := rawRoles.Delete(deleted); err != nil {
		t.Fatalf("delete %s: %v", deleted, err)
	}
	delete(want, deleted)

	// Every read of one key is a request the store sends on this connection: a direct get when the
	// bucket allows one, a stream message get otherwise.
	gets, err := conn.SubscribeSync(fmt.Sprintf("$JS.API.DIRECT.GET.KV_%s.>", names.roles))
	if err != nil {
		t.Fatalf("watch direct gets: %v", err)
	}
	defer func() { _ = gets.Unsubscribe() }()
	legacyGets, err := conn.SubscribeSync(fmt.Sprintf("$JS.API.STREAM.MSG.GET.KV_%s", names.roles))
	if err != nil {
		t.Fatalf("watch stream message gets: %v", err)
	}
	defer func() { _ = legacyGets.Unsubscribe() }()
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the watches: %v", err)
	}

	registry, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush after Open: %v", err)
	}
	if sent := pending(t, gets) + pending(t, legacyGets); sent != 0 {
		t.Fatalf("Open sent %d per-key reads of the role bucket holding %d claims; want none, one pass over the bucket", sent, claims)
	}

	if len(registry.restoredRoleRevisions) != len(want) {
		t.Fatalf("Open restored %d role revisions, want %d", len(registry.restoredRoleRevisions), len(want))
	}
	for role, revision := range want {
		if got := registry.restoredRoleRevisions[role]; got != revision {
			t.Fatalf("restored revision of %s = %d, want the stored %d", role, got, revision)
		}
	}
	// What the snapshot is for, through the caller that reads it: an absent holder keeps its claim
	// for one session TTL after the restart, and a key the bucket no longer holds has none to keep.
	if release, err := registry.ReleaseExpiredRoleClaim("role-000", "ses_000", time.Minute); err != nil || release != ExpiredRoleClaimRetained {
		t.Fatalf("restored claim within its grace = %v, %v; want it retained", release, err)
	}
	if release, err := registry.ReleaseExpiredRoleClaim(deleted, fmt.Sprintf("ses_%03d", claims), time.Minute); err != nil || release != ExpiredRoleClaimMissing {
		t.Fatalf("deleted claim = %v, %v; want it missing", release, err)
	}
}

// pending counts the requests sub has received. conn.Flush has already round-tripped every request
// the store sent, so the server has echoed each one back to sub before this reads the count.
func pending(t *testing.T, sub *natsgo.Subscription) int {
	t.Helper()
	count, _, err := sub.Pending()
	if err != nil {
		t.Fatalf("pending on %s: %v", sub.Subject, err)
	}
	return count
}

func putRoleClaim(t *testing.T, kv natsgo.KeyValue, role, holder string) uint64 {
	t.Helper()
	value, err := json.Marshal(RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode claim: %v", err)
	}
	revision, err := kv.Put(role, value)
	if err != nil {
		t.Fatalf("store %s: %v", role, err)
	}
	return revision
}

// A restart's own log says how much of the role bucket it read. The SRE reading a staging restart
// (listener ready 84 s after the update began) could not tell from the logs whether the revision
// scan had completed, because nothing recorded its size. The two counts differ for a reason worth
// seeing: restored is the claims that got their grace back, keys is everything the bucket made the
// listener stream to find them, delete markers included, which is what grows without bound.
func TestOpeningTheRegistryLogsHowManyRoleClaimsItRestored(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).roles, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	const (
		claims  = 5
		markers = 3
	)
	for i := range claims {
		putRoleClaim(t, rawRoles, fmt.Sprintf("role-%d", i), fmt.Sprintf("ses_%d", i))
	}
	for i := range markers {
		gone := fmt.Sprintf("gone-%d", i)
		putRoleClaim(t, rawRoles, gone, "ses_gone")
		if err := rawRoles.Delete(gone); err != nil {
			t.Fatalf("delete %s: %v", gone, err)
		}
	}

	logs := captureRegistryLogs(t)
	registry, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(registry.StopWatch)

	want := fmt.Sprintf(`msg="restored role claims" restored=%d keys=%d`, claims, claims+markers)
	if got := logs.String(); !strings.Contains(got, want) {
		t.Fatalf("the restart logged no line saying how much of the role bucket it read.\nwant: %s\ngot:\n%s", want, got)
	}
}

// earlyEndKV hands out a watcher over the real bucket that ends its scan early, the two ways
// nats.go ends one early (nats.go v1.50.0 kv.go). With no fault it closes the updates channel
// after `after` entries and never sends the nil marker, which is what a subscription that ends
// mid-scan looks like (:1170). With one it sends the marker after `after` entries and puts the
// fault on Error() first, which is what the idle timer does when no entry arrives within the
// JetStream MaxWait (:1145-1156).
type earlyEndKV struct {
	natsgo.KeyValue

	after int
	fault error
}

func (k earlyEndKV) Watch(keys string, opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(keys, opts...)
	if err != nil {
		return nil, err
	}
	ended := &earlyEndWatcher{KeyWatcher: watcher, updates: make(chan natsgo.KeyValueEntry), faults: make(chan error, 1)}
	go func() {
		defer close(ended.updates)
		for delivered := 0; delivered < k.after; delivered++ {
			entry, ok := <-watcher.Updates()
			if !ok || entry == nil {
				return
			}
			ended.updates <- entry
		}
		if k.fault != nil {
			ended.faults <- k.fault
			ended.updates <- nil
		}
	}()
	return ended, nil
}

type earlyEndWatcher struct {
	natsgo.KeyWatcher

	updates chan natsgo.KeyValueEntry
	faults  chan error
}

func (w *earlyEndWatcher) Updates() <-chan natsgo.KeyValueEntry { return w.updates }

func (w *earlyEndWatcher) Error() <-chan error { return w.faults }

// A revision snapshot that ends before the bucket's keys are all delivered is not a snapshot: the
// claims it missed are still in the bucket, and each one is a restored holder that would lose the
// grace a restart owes it and be released a session TTL early. nats.go ends a scan early two ways
// and they look different to the caller - a closed updates channel, and its own idle timeout,
// which arrives as the same nil marker a complete scan ends with. Neither may return what the scan
// managed to read; Open returns the error unchanged, failing the start as a failed read did when
// this was one Get per key.
func TestARoleRevisionScanThatEndsEarlyIsAnErrorNotAShortSnapshot(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBuckets(t).roles, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	for i := range 4 {
		putRoleClaim(t, rawRoles, fmt.Sprintf("role-%d", i), fmt.Sprintf("ses_%d", i))
	}

	for _, ending := range []struct {
		name  string
		fault error
		want  string
	}{
		{name: "the subscription ends mid-scan", want: "ended after 1 keys"},
		{name: "the idle timer gives up mid-scan", fault: natsgo.ErrKeyWatcherTimeout, want: "stopped after 1 keys"},
	} {
		t.Run(ending.name, func(t *testing.T) {
			handle := bus.KeyValue{KeyValue: earlyEndKV{KeyValue: rawRoles, after: 1, fault: ending.fault}}
			revisions, err := roleRevisions(handle)
			if err == nil {
				t.Fatalf("a scan that ended after 1 of 4 claims returned %d revisions and no error", len(revisions))
			}
			if revisions != nil {
				t.Fatalf("a failed scan returned %d revisions; a partial snapshot must not reach the registry", len(revisions))
			}
			if !strings.Contains(err.Error(), ending.want) {
				t.Fatalf("error = %q, want it to say %q: how the scan ended and how many keys it read", err, ending.want)
			}
			if ending.fault != nil && !errors.Is(err, ending.fault) {
				t.Fatalf("error = %q, want it to carry %v", err, ending.fault)
			}
		})
	}
}

// armOn arms once trigger has passed through it, keeping the last trigger-length bytes so a
// trigger split across two reads is still seen.
type armOn struct {
	trigger []byte
	armed   *atomic.Bool
	tail    []byte
}

func (a *armOn) Write(p []byte) (int, error) {
	if !a.armed.Load() {
		a.tail = append(a.tail, p...)
		if bytes.Contains(a.tail, a.trigger) {
			a.armed.Store(true)
		} else if len(a.tail) > len(a.trigger) {
			a.tail = append(a.tail[:0], a.tail[len(a.tail)-len(a.trigger):]...)
		}
	}
	return len(p), nil
}

// holdAfter passes writes through to `to` until it is armed and budget bytes have gone by, then
// buffers everything for `hold` and sends it on. It keeps taking the writes, so the server never
// blocks and never sees a slow consumer; the client simply hears nothing for `hold` and then the
// link recovers. A hold longer than the reader's JetStream MaxWait is what fires nats.go's
// initial-values timer, and the recovery afterwards is what lets a reader that took the timer's
// marker for a finished scan go on to succeed with less than the bucket.
type holdAfter struct {
	to     io.Writer
	armed  *atomic.Bool
	budget int
	hold   time.Duration

	mu        sync.Mutex
	forwarded int
	holding   bool
	released  bool
	held      []byte
}

func (h *holdAfter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.armed.Load() && h.forwarded >= h.budget && !h.released {
		if !h.holding {
			h.holding = true
			time.AfterFunc(h.hold, h.release)
		}
		h.held = append(h.held, p...)
		return len(p), nil
	}
	if h.armed.Load() {
		h.forwarded += len(p)
	}
	return h.to.Write(p)
}

func (h *holdAfter) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.released = true
	if len(h.held) > 0 {
		_, _ = h.to.Write(h.held)
		h.held = nil
	}
}

// stallingProxy forwards a NATS connection to target until the client creates the watcher named by
// trigger and budget bytes of that watcher's scan have reached it, then holds the server's bytes
// for hold and sends them on: a relayed link that goes quiet mid-scan and comes back. It returns
// the URL to connect to.
func stallingProxy(t *testing.T, target, trigger string, budget int, hold time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			var armed atomic.Bool
			go func() {
				defer func() { _ = server.Close() }()
				_, _ = io.Copy(server, io.TeeReader(client, &armOn{trigger: []byte(trigger), armed: &armed}))
			}()
			go func() {
				defer func() { _ = client.Close() }()
				_, _ = io.Copy(&holdAfter{to: client, armed: &armed, budget: budget, hold: hold}, server)
			}()
		}
	}()
	return "nats://" + listener.Addr().String()
}

// The listener's own failure, through real nats.go: a link that goes quiet partway through the
// revision scan and then comes back must fail Open, not open the registry on the claims that
// happened to arrive. nats.go's idle timer reports the quiet as ErrKeyWatcherTimeout on Error()
// and then sends the same nil entry a finished scan sends, so a reader that takes the nil for a
// finished scan restores a snapshot missing most of the bucket, and every claim it misses is a
// restored holder released a session TTL early. Which error Open reports depends on how it reads
// the bucket; that it reports one is the contract. The deterministic halves of both endings,
// including that the timeout is carried, are in
// TestARoleRevisionScanThatEndsEarlyIsAnErrorNotAShortSnapshot.
//
// The stall shape is load-bearing and a weaker one hides the defect, so do not simplify it to a
// link that stays down. stallingProxy holds the server's bytes past Open's JetStream MaxWait, so
// the timer fires, and then sends them on, so the reader is free to carry on and succeed with part
// of the bucket instead of failing on a later request. That recovery is asserted, not assumed: the
// stalled.Flush below has to answer before Open's outcome is judged, so a hold that never releases
// — whether edited in or produced by load ending the run early — reds this test instead of passing
// it. Held against three builds of this package:
//
//	88f9fbaa  Keys() plus a Get per key             FAIL  "restoring 232 of 400 claims", err=nil
//	3fa4774d  one watch, closed-channel check only  FAIL  "restoring 232 of 400 claims", err=nil
//	this build                                      PASS  "the bucket's watch stopped after 232
//	                                                      keys: nats: key watcher timed out
//	                                                      waiting for initial keys"
//
// How far each partial scan got varies with the link; that it completed and returned no error is
// the constant. A one-hour hold, the weakening, reds here on the Flush: "the link never came back
// after the hold ... nats: timeout".
//
// 88f9fbaa is main, so the defect predates the one-pass read: Keys() is itself a watch carrying
// the same timer. With a stall that never recovers all three would pass, main because its per-key
// Gets then time out on a dead link — a second route to a failed start that hides the first — and
// that is the run the Flush assertion refuses.
//
// Those FAIL rows can flip to PASS under load, and do: this test's review saw 2 false passes at
// 88f9fbaa in 45 iterations, both "context deadline exceeded", and 3fa4774d passed 1 of 3 runs
// here the same way. A run whose requests time out before the release never reaches the shape
// being measured. It has never gone red on correct code, so this is a demonstration rather than
// the regression lock. The lock is TestARoleRevisionScanThatEndsEarlyIsAnErrorNotAShortSnapshot,
// which is deterministic and failed on both of those builds in every run, plus the Flush here
// against anyone weakening the hold.
func TestOpenFailsWhenTheRoleRevisionScanStalls(t *testing.T) {
	names := testBuckets(t)
	direct, cleanup := connectNATS(t)
	defer cleanup()
	js, err := direct.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: names.roles, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	const claims = 400
	for i := range claims {
		putRoleClaim(t, rawRoles, fmt.Sprintf("role-%03d", i), fmt.Sprintf("ses_%03d", i))
	}

	target := strings.TrimPrefix(sharedTestNATSURI(t), "nats://")
	stalled := testnats.Connect(t, stallingProxy(t, target, "$JS.API.CONSUMER.CREATE.KV_"+names.roles, 16*1024, 13*time.Second))
	defer stalled.Close()

	registry, err := Open(stalled, WithReplicas(1), withTestBuckets(t))
	// The link has to have come back before Open's outcome means anything: a stall that stays down
	// fails every build, main by its per-key Gets timing out, so this run could not tell them
	// apart. Assert it rather than trusting the hold, since load can end a run before the release
	// too.
	if err := stalled.Flush(); err != nil {
		t.Fatalf("the link never came back after the hold, so this run cannot tell a build that fails the stalled scan from one that returns part of the bucket: %v", err)
	}
	if err == nil {
		t.Cleanup(registry.StopWatch)
		t.Fatalf("Open succeeded over a link that stopped mid-scan, restoring %d of %d claims; the rest lose their grace", len(registry.restoredRoleRevisions), claims)
	}
	t.Logf("Open over a stalled link that came back: %v", err)
}
