package store

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
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

// --- Integration Tests (testcontainers NATS) ---

func connectNATS(t *testing.T) (*natsgo.Conn, func()) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to get NATS URI: %v", err)
	}
	conn, err := natsgo.Connect(uri)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to connect: %v", err)
	}
	cleanup := func() {
		conn.Close()
		ctr.Terminate(ctx)
	}
	return conn, cleanup
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

// pollMatch retries Match() until the expected count is reached or timeout fires.
func pollMatch(t *testing.T, r *Registry, machineID, topic string, wantCount int, timeout time.Duration) []Interest {
	t.Helper()
	deadline := time.After(timeout)
	for {
		got := r.Match(machineID, topic)
		if len(got) == wantCount {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for Match() to return %d results (got %d)", wantCount, len(got))
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestOpen_WatchPopulatesExistingKeys(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	// Pre-populate the KV bucket before Open()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_preload", MachineID: "m1", Topics: []string{"notifications.>"}})

	// Open registry — watch() populates cache asynchronously
	reg, err := Open(conn, WithReplicas(1))
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

	reg, err := Open(conn, WithReplicas(1))
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
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_del", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1))
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
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_purge", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1))
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
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to get URI: %v", err)
	}
	conn, err := natsgo.Connect(uri)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to connect: %v", err)
	}
	// Safety net: double-close and double-terminate are no-ops
	t.Cleanup(func() {
		conn.Close()
		ctr.Terminate(ctx)
	})

	// Pre-populate and open
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_survive", MachineID: "m1", Topics: []string{"notifications.>"}})

	reg, err := Open(conn, WithReplicas(1))
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

	reg, err := Open(conn, WithReplicas(1))
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
// No watch() goroutine is started, so the cache stays cold for the test's lifetime.
func coldRegistry(t *testing.T, conn *natsgo.Conn) (*Registry, natsgo.KeyValue) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	roleKV, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: RoleBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create role KV bucket: %v", err)
	}
	return &Registry{kv: kv, roleKV: roleKV, cache: map[string]Interest{}}, kv
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

	got, err := reg.SetRole("ses_role_a", "m1", "legion-controller")
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
	if string(entry.Value()) != "ses_role_a" {
		t.Fatalf("expected role holder ses_role_a, got %q", string(entry.Value()))
	}

	persisted, err := reg.Get("ses_role_a")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(persisted.Topics) != 1 || persisted.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected persisted role topic, got %v", persisted.Topics)
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

	got, err := reg.SetRole("ses_new", "m2", "legion-controller")
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
	if string(entry.Value()) != "ses_new" {
		t.Fatalf("expected role holder ses_new, got %q", string(entry.Value()))
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

	got, err := reg.SetRole("ses_same", "m1", "legion-controller")
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

func TestSetRole_OldHolderMissing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	if _, err := reg.roleKV.Put("legion-controller", []byte("ses_missing")); err != nil {
		t.Fatalf("failed to seed role bucket: %v", err)
	}

	got, err := reg.SetRole("ses_fresh", "m1", "legion-controller")
	if err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	if got.SessionID != "ses_fresh" {
		t.Fatalf("expected session ses_fresh, got %s", got.SessionID)
	}
	if len(got.Topics) != 1 || got.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected returned role topic, got %v", got.Topics)
	}

	entry, err := reg.roleKV.Get("legion-controller")
	if err != nil {
		t.Fatalf("roleKV.Get failed: %v", err)
	}
	if string(entry.Value()) != "ses_fresh" {
		t.Fatalf("expected role holder ses_fresh, got %q", string(entry.Value()))
	}
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

	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping on healthy conn returned error: %v", err)
	}
}

func TestPing_ClosedConnReturnsError(t *testing.T) {
	// Regression for the sami listener stuck-after-recovery scenario: the bus
	// recovery path replaces *nats.Conn but leaves Registry.kv handles bound to
	// the original closed conn. Ping must surface that as an error so the
	// listener can self-terminate and let restart policy recover.
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1))
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