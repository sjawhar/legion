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
