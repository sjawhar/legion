package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// --- Unit Tests (no NATS needed — test Match() against pre-populated cache) ---

func TestMatch_FiltersByMachineID(t *testing.T) {
	r := cachedRegistry(map[string]Interest{
		"ses_a": {SessionID: "ses_a", MachineID: "machine-A", Topics: []string{"notifications.>"}},
		"ses_b": {SessionID: "ses_b", MachineID: "machine-B", Topics: []string{"notifications.>"}},
		"ses_c": {SessionID: "ses_c", MachineID: "machine-A", Topics: []string{"notifications.>"}},
	})

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

// MatchAll is Match without the machine filter: a sender confirming delivery to every subscribed
// session must find the ones registered through another machine's listener, by an exact topic or
// a wildcard alike, and none whose patterns miss the topic.
func TestMatchAll_ReturnsMatchingSessionsOnEveryMachine(t *testing.T) {
	r := cachedRegistry(map[string]Interest{
		"ses_b_exact": {SessionID: "ses_b_exact", MachineID: "machine-B", Topics: []string{"notifications.team.deploy"}},
		"ses_a_star":  {SessionID: "ses_a_star", MachineID: "machine-A", Topics: []string{"notifications.team.*"}},
		"ses_a_other": {SessionID: "ses_a_other", MachineID: "machine-A", Topics: []string{"notifications.team.review"}},
		"ses_b_chev":  {SessionID: "ses_b_chev", MachineID: "machine-B", Topics: []string{"notifications.slack.>", "notifications.>"}},
		"ses_b_slack": {SessionID: "ses_b_slack", MachineID: "machine-B", Topics: []string{"notifications.slack.>"}},
	})

	got := r.MatchAll("notifications.team.deploy")
	ids := make([]string, len(got))
	for i, item := range got {
		ids[i] = item.SessionID
	}
	want := []string{"ses_a_star", "ses_b_chev", "ses_b_exact"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("MatchAll(notifications.team.deploy) = %v, want %v in SessionID order", ids, want)
	}

	if got := r.MatchAll("notifications.agent.ses_x.extra.tokens"); len(got) != 1 || got[0].SessionID != "ses_b_chev" {
		t.Fatalf("MatchAll of a topic only notifications.> matches = %v, want [ses_b_chev]", got)
	}
}

func TestMatch_WildcardPatterns(t *testing.T) {
	r := cachedRegistry(map[string]Interest{
		"ses_exact":   {SessionID: "ses_exact", MachineID: "m1", Topics: []string{"notifications.github.sjawhar.legion.pr"}},
		"ses_star":    {SessionID: "ses_star", MachineID: "m1", Topics: []string{"notifications.github.*.*.pr"}},
		"ses_chevron": {SessionID: "ses_chevron", MachineID: "m1", Topics: []string{"notifications.github.>"}},
		"ses_slack":   {SessionID: "ses_slack", MachineID: "m1", Topics: []string{"notifications.slack.>"}},
	})

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
	r := cachedRegistry(map[string]Interest{
		"ses_thread_a": {SessionID: "ses_thread_a", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.1111111111_111111.>",
		}},
		"ses_thread_b": {SessionID: "ses_thread_b", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.2222222222_222222.>",
		}},
		"ses_channel": {SessionID: "ses_channel", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.>",
		}},
	})

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
	r := cachedRegistry(map[string]Interest{})
	got := r.Match("m1", "notifications.test")
	if len(got) != 0 {
		t.Fatalf("expected 0 results from empty cache, got %d", len(got))
	}
}

func TestMatch_MultipleTopicsOnOneEntry(t *testing.T) {
	r := cachedRegistry(map[string]Interest{
		"ses_multi": {
			SessionID: "ses_multi",
			MachineID: "m1",
			Topics:    []string{"notifications.github.>", "notifications.slack.>"},
		},
	})

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
// Regression coverage for a silent fallback: an Upsert that treats a transient error from the
// interest bucket's Get as ErrKeyNotFound clobbers durable state with whatever the heartbeat sent
// (only the agent topic), truncating the session's subscriptions to its own topic.

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

// A row stored before the listener refused topics reaching a private stream subject keeps them
// until the session's next registration, which drops them: another session's agent subject, a
// failed-delivery lane wildcard, a Legion controller lane and notifications.> go, while the
// session's own agent subject, the issue topic it followed and the topic it registers now stay.
func TestMergeForUpsert_DropsStoredTopicsReachingAPrivateStreamSubject(t *testing.T) {
	cur := Interest{
		SessionID: "ses_x",
		MachineID: "m1",
		Topics: []string{
			"notifications.agent.ses_other",
			"notifications.agent.ses_x",
			"notifications.dispatch.issue.KEY-1.>",
			"notifications.envoy.exceptions.>",
			"notifications.legion.ACME.controller",
			"notifications.>",
		},
	}
	item := Interest{SessionID: "ses_x", MachineID: "m1"}

	result, err := mergeForUpsert(cur, nil, item, []string{"notifications.agent.ses_x", "notifications.github.acme.widgets.pr.7.>"}, 7)
	if err != nil {
		t.Fatalf("mergeForUpsert returned unexpected error: %v", err)
	}
	want := []string{"notifications.agent.ses_x", "notifications.dispatch.issue.KEY-1.>", "notifications.github.acme.widgets.pr.7.>"}
	if !slices.Equal(result.Topics, want) {
		t.Fatalf("merged topics = %v, want %v", result.Topics, want)
	}
}

// --- Integration Tests (a real NATS server) ---

// connectNATS connects to the package's shared NATS server, in the JetStream account
// testnats.URL hands t, where no earlier test made an interest or role bucket.
func connectNATS(t *testing.T) (*natsgo.Conn, func()) {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
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
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
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
	if _, err := reg.Upsert(Interest{SessionID: "ses_new", MachineID: "m1"}, []string{"notifications.test.>"}); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	// Poll with bounded timeout
	got = pollMatch(t, reg, "m1", "notifications.test.x", 1, 5*time.Second)
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
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
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
	if err := reg.Remove(context.Background(), "ses_del", nil); err != nil {
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
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
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
	srv, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	// Safety net: a double close is a no-op.
	t.Cleanup(conn.Close)

	// Pre-populate and open
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
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
	srv.Terminate(t)

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
// Its watcher is never started, so the cache stays cold for the test's lifetime.
func coldRegistry(t *testing.T, conn *natsgo.Conn) (*Registry, natsgo.KeyValue) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("failed to create KV bucket: %v", err)
	}
	roleKV, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
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

	item, err := reg.Get(context.Background(), "ses_cold")
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
	if _, err := reg.Get(context.Background(), "ses_pop"); err != nil {
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

	_, err := reg.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if err != natsgo.ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// A read whose context has already ended still answers from the cache: Get's cache hit never
// consults ctx. Every read that would go to the bucket - a Get that misses, a claim read, and a
// release's read - instead returns ctx's end, naming the call, and a release left that way deletes
// nothing.
func TestReadsWhoseContextHasEndedAnswerOnlyFromTheCache(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_cached", MachineID: "m1", Topics: []string{"topic.a"}})
	if _, err := reg.Get(context.Background(), "ses_cached"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	const role = "reviewer"
	if _, err := reg.SetRole("ses_lapsed", "m1", role, false); err != nil {
		t.Fatalf("seed the role claim: %v", err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	if item, err := reg.Get(ended, "ses_cached"); err != nil || item.SessionID != "ses_cached" {
		t.Fatalf("cached Get under an ended context = %+v, %v; want the cached interest", item, err)
	}
	if _, err := reg.Get(ended, "ses_uncached"); !errors.Is(err, context.Canceled) || err.Error() != "read ses_uncached's interest record: context canceled" {
		t.Fatalf("uncached Get under an ended context = %v, want it to name the read and wrap context.Canceled", err)
	}
	if _, err := reg.RoleClaim(ended, role); !errors.Is(err, context.Canceled) || err.Error() != "read role reviewer's claim: context canceled" {
		t.Fatalf("RoleClaim under an ended context = %v, want it to name the read and wrap context.Canceled", err)
	}
	release, err := reg.ReleaseExpiredRoleClaim(ended, role, "ses_lapsed", 0)
	if release != ExpiredRoleClaimRetained || !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "release role reviewer's expired claim held by ses_lapsed: ") {
		t.Fatalf("release under an ended context = %v, %v; want retained, naming the release and wrapping context.Canceled", release, err)
	}
	assertRoleHolder(t, reg, role, "ses_lapsed")
}

// A claim record the bucket holds and no read can decode is ErrMalformedRoleClaim, so a caller can
// tell it from a read that did not answer, with the detail of what is wrong kept in the message.
func TestRoleClaimReadsAnUndecodableRecordAsErrMalformedRoleClaim(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	for record, detail := range map[string]string{
		`{"holder_session_id":""}`: "malformed role claim: it names no holder",
		`{"holder_session_id":7}`:  "malformed role claim: json: ",
		"  ":                       "malformed role claim: a legacy record with an empty holder",
	} {
		if _, err := reg.roleKV.Put("reviewer", []byte(record)); err != nil {
			t.Fatalf("store the claim record %q: %v", record, err)
		}
		_, err := reg.RoleClaim(context.Background(), "reviewer")
		if !errors.Is(err, ErrMalformedRoleClaim) || !strings.HasPrefix(err.Error(), detail) {
			t.Fatalf("RoleClaim of %q = %v, want ErrMalformedRoleClaim naming %q", record, err, detail)
		}
	}
}

// An interest record the bucket holds and this build cannot decode is ErrMalformedInterest, so a
// caller can tell it from a read that did not answer, with the decode failure kept in the message,
// and the registry logs it at WARN naming the session. Nothing is cached from it.
func TestGetReadsAnUndecodableInterestRecordAsErrMalformedInterest(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const sessionID = "ses_unreadable_interest"
	if _, err := kv.Put(sessionID, []byte("not a json interest record")); err != nil {
		t.Fatalf("store the interest record: %v", err)
	}
	item, err := reg.Get(context.Background(), sessionID)
	if !errors.Is(err, ErrMalformedInterest) || !strings.HasPrefix(err.Error(), "malformed interest record: invalid character ") {
		t.Fatalf("Get of an undecodable record = %+v, %v; want ErrMalformedInterest naming the decode failure", item, err)
	}
	if item.SessionID != "" || item.Topics != nil {
		t.Fatalf("Get of an undecodable record returned %+v, want the zero Interest", item)
	}
	for _, cached := range reg.List() {
		if cached.SessionID == sessionID {
			t.Fatalf("the cache holds %+v after an undecodable read", cached)
		}
	}
	for _, want := range []string{"level=WARN", `msg="registry read an interest record it cannot decode"`, "session_id=" + sessionID, "invalid character"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("registry logs %q, want them to contain %q", logs.String(), want)
		}
	}
}

const undecodableInterestRecord = "not a json interest record"

// storeUndecodableInterest writes a record no build can decode as sessionID's interest record, and
// a claim on role naming sessionID as its holder.
func storeUndecodableInterest(t *testing.T, reg *Registry, kv natsgo.KeyValue, sessionID, role string) {
	t.Helper()
	if _, err := kv.Put(sessionID, []byte(undecodableInterestRecord)); err != nil {
		t.Fatalf("store %s's interest record: %v", sessionID, err)
	}
	claim, err := json.Marshal(RoleClaim{HolderSessionID: sessionID, ClaimedAt: 1})
	if err != nil {
		t.Fatalf("encode the claim on %s: %v", role, err)
	}
	if _, err := reg.roleKV.Put(role, claim); err != nil {
		t.Fatalf("store the claim on %s: %v", role, err)
	}
}

// Removing every topic of a session whose interest record this build cannot decode deletes the
// record and releases the session's role claims, as it does for a record that decodes: failing on
// the read would leave nothing that can clear the record.
func TestRemoveAllDeletesAnUndecodableInterestRecordAndReleasesItsRoles(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const sessionID = "ses_undecodable_remove_all"
	storeUndecodableInterest(t, reg, kv, sessionID, "reviewer")
	if err := reg.Remove(context.Background(), sessionID, nil); err != nil {
		t.Fatalf("Remove(%s, nil) over an undecodable record: %v", sessionID, err)
	}
	if entry, err := kv.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the bucket's %s key after Remove = %v, %v; want ErrKeyNotFound", sessionID, entry, err)
	}
	if holder, err := reg.RoleHolder("reviewer"); err != nil || holder != "" {
		t.Fatalf("reviewer's holder after Remove = %q, %v; want the claim released", holder, err)
	}
}

// Removing some topics of a session whose interest record this build cannot decode releases the
// role claims those topics name and leaves the record as it is, logged at WARN naming the session:
// the record routes none of its topics here, so there is nothing to take out of it.
func TestRemoveTopicsLeavesAnUndecodableInterestRecordAndReleasesTheirRoles(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const sessionID = "ses_undecodable_remove_topics"
	storeUndecodableInterest(t, reg, kv, sessionID, "reviewer")
	if err := reg.Remove(context.Background(), sessionID, []string{"notifications.role.reviewer"}); err != nil {
		t.Fatalf("Remove(%s, its role topic) over an undecodable record: %v", sessionID, err)
	}
	if holder, err := reg.RoleHolder("reviewer"); err != nil || holder != "" {
		t.Fatalf("reviewer's holder after Remove = %q, %v; want the claim released", holder, err)
	}
	entry, err := kv.Get(sessionID)
	if err != nil || string(entry.Value()) != undecodableInterestRecord {
		t.Fatalf("the bucket's %s record after Remove = %v, %v; want it unchanged", sessionID, entry, err)
	}
	for _, want := range []string{"level=WARN", `msg="registry left an interest record it cannot decode unchanged"`, "session_id=" + sessionID} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("registry logs %q, want them to contain %q", logs.String(), want)
		}
	}
}

// A subscribe over an interest record this build cannot decode writes the record it asks for,
// without the undecodable record's topics, and logs the replacement at WARN naming the session: the
// session's own subscribe is the authority on its record. The session's role claims live in the
// role bucket, so they are kept.
func TestUpsertReplacesAnUndecodableInterestRecord(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const sessionID = "ses_undecodable_upsert"
	storeUndecodableInterest(t, reg, kv, sessionID, "reviewer")
	item, err := reg.Upsert(Interest{SessionID: sessionID, MachineID: "m1", Dir: "/workspace"},
		[]string{"topic.fresh", "notifications.agent." + sessionID})
	if err != nil {
		t.Fatalf("Upsert over an undecodable record: %v", err)
	}
	entry, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's record after Upsert: %v", sessionID, err)
	}
	var stored Interest
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("the record Upsert wrote does not decode: %q: %v", entry.Value(), err)
	}
	want := "notifications.agent." + sessionID + " topic.fresh"
	if got := strings.Join(stored.Topics, " "); got != want || stored.MachineID != "m1" || stored.Dir != "/workspace" {
		t.Fatalf("the record Upsert wrote = %+v, want topics %q on m1 in /workspace", stored, want)
	}
	if got := strings.Join(item.Topics, " "); got != want {
		t.Fatalf("Upsert returned topics %q, want %q", got, want)
	}
	if holder, err := reg.RoleHolder("reviewer"); err != nil || holder != sessionID {
		t.Fatalf("reviewer's holder after Upsert = %q, %v; want the claim kept for %s", holder, err, sessionID)
	}
	for _, want := range []string{"level=WARN", `msg="registry replaced an interest record it cannot decode"`, "session_id=" + sessionID} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("registry logs %q, want them to contain %q", logs.String(), want)
		}
	}
}

// A role claim goes through over an interest record this build cannot decode, the claimant's or
// the role's previous holder's: the claimant's record is replaced by one naming the role, and the
// previous holder's is left as it is.
func TestSetRoleClaimsOverUndecodableInterestRecords(t *testing.T) {
	captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	const (
		claimant = "ses_undecodable_claimant"
		previous = "ses_undecodable_previous"
	)
	if _, err := kv.Put(claimant, []byte(undecodableInterestRecord)); err != nil {
		t.Fatalf("store %s's interest record: %v", claimant, err)
	}
	storeUndecodableInterest(t, reg, kv, previous, "reviewer")
	item, err := reg.SetRole(claimant, "m1", "reviewer", false)
	if err != nil {
		t.Fatalf("SetRole over undecodable records: %v", err)
	}
	if got := strings.Join(item.Topics, " "); got != "notifications.role.reviewer" {
		t.Fatalf("SetRole returned topics %q, want the role topic", got)
	}
	if holder, err := reg.RoleHolder("reviewer"); err != nil || holder != claimant {
		t.Fatalf("reviewer's holder after SetRole = %q, %v; want %s", holder, err, claimant)
	}
	entry, err := kv.Get(claimant)
	if err != nil {
		t.Fatalf("read %s's record after SetRole: %v", claimant, err)
	}
	var stored Interest
	if err := json.Unmarshal(entry.Value(), &stored); err != nil || strings.Join(stored.Topics, " ") != "notifications.role.reviewer" {
		t.Fatalf("%s's record after SetRole = %q, %v; want it to decode with the role topic", claimant, entry.Value(), err)
	}
	if entry, err := kv.Get(previous); err != nil || string(entry.Value()) != undecodableInterestRecord {
		t.Fatalf("%s's record after SetRole = %v, %v; want it unchanged", previous, entry, err)
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

// manualClock is a registry clock (WithClock) the test moves by hand.
type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Set moves the clock to at.
func (c *manualClock) Set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// clockedRegistry opens a registry on the clock, over an interest bucket no earlier test wrote,
// waits for its cache, and returns it with that bucket.
func clockedRegistry(t *testing.T, clock *manualClock) (*Registry, natsgo.KeyValue) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, err := Open(conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("interest cache not ready: %v", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("interest bucket: %v", err)
	}
	return reg, kv
}

// interestRevision is the bucket's revision of the session's interest row.
func interestRevision(t *testing.T, kv natsgo.KeyValue, sessionID string) uint64 {
	t.Helper()
	entry, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("get interest %s: %v", sessionID, err)
	}
	return entry.Revision()
}

// upsertOrFail upserts the heartbeat-shaped item and topics and returns what Upsert returned.
func upsertOrFail(t *testing.T, reg *Registry, sessionID string, topics ...string) Interest {
	t.Helper()
	item, err := reg.Upsert(Interest{SessionID: sessionID, MachineID: "m1", Dir: "/work/" + sessionID}, topics)
	if err != nil {
		t.Fatalf("Upsert %s: %v", sessionID, err)
	}
	return item
}

// A heartbeat that changes nothing writes the interest row only once the row is
// InterestRefreshAge old: before that Upsert returns the stored row, UpdatedAt and all, and the
// bucket gains no revision; at that age, and past it, the row is written and restamped.
func TestUpsertWritesAnUnchangedRowOnlyOnceItIsInterestRefreshAgeOld(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	topics := []string{"notifications.agent.ses_hb", "notifications.team.heartbeat"}

	upsertOrFail(t, reg, "ses_hb", topics...)
	created := interestRevision(t, kv, "ses_hb")
	for _, after := range []time.Duration{90 * time.Second, 180 * time.Second, 270 * time.Second, InterestRefreshAge - time.Millisecond} {
		clock.Set(start.Add(after))
		got := upsertOrFail(t, reg, "ses_hb", topics...)
		if revision := interestRevision(t, kv, "ses_hb"); revision != created {
			t.Fatalf("unchanged heartbeat %v after the write moved the row from revision %d to %d", after, created, revision)
		}
		if got.UpdatedAt != start.UnixMilli() {
			t.Fatalf("unchanged heartbeat %v after the write returned updated_at %d, want the stored row's %d", after, got.UpdatedAt, start.UnixMilli())
		}
	}

	refreshed := start.Add(InterestRefreshAge)
	clock.Set(refreshed)
	if got := upsertOrFail(t, reg, "ses_hb", topics...); got.UpdatedAt != refreshed.UnixMilli() {
		t.Fatalf("heartbeat at InterestRefreshAge returned updated_at %d, want %d", got.UpdatedAt, refreshed.UnixMilli())
	}
	if revision := interestRevision(t, kv, "ses_hb"); revision != created+1 {
		t.Fatalf("heartbeat at InterestRefreshAge left revision %d, want one write (%d)", revision, created+1)
	}

	clock.Set(refreshed.Add(90 * time.Second))
	upsertOrFail(t, reg, "ses_hb", topics...)
	if revision := interestRevision(t, kv, "ses_hb"); revision != created+1 {
		t.Fatalf("heartbeat 90s after the refresh moved the row to revision %d, want %d", revision, created+1)
	}

	clock.Set(refreshed.Add(InterestRefreshAge + time.Minute))
	upsertOrFail(t, reg, "ses_hb", topics...)
	if revision := interestRevision(t, kv, "ses_hb"); revision != created+2 {
		t.Fatalf("heartbeat past InterestRefreshAge left revision %d, want one more write (%d)", revision, created+2)
	}
	entry, err := kv.Get("ses_hb")
	if err != nil {
		t.Fatalf("get ses_hb: %v", err)
	}
	var stored Interest
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode ses_hb: %v", err)
	}
	if want := refreshed.Add(InterestRefreshAge + time.Minute).UnixMilli(); stored.UpdatedAt != want {
		t.Fatalf("stored updated_at = %d, want the last write's %d", stored.UpdatedAt, want)
	}
}

// A heartbeat that adds a topic is written at once, however young the row; one that re-sends the
// stored topics in another order changes nothing, since the merge sorts them.
func TestUpsertWritesAChangedTopicSetAtOnce(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)

	upsertOrFail(t, reg, "ses_topics", "notifications.agent.ses_topics")
	created := interestRevision(t, kv, "ses_topics")

	clock.Set(start.Add(time.Second))
	got := upsertOrFail(t, reg, "ses_topics", "notifications.team.new", "notifications.agent.ses_topics")
	if revision := interestRevision(t, kv, "ses_topics"); revision != created+1 {
		t.Fatalf("a new topic left revision %d, want one write (%d)", revision, created+1)
	}
	if want := []string{"notifications.agent.ses_topics", "notifications.team.new"}; !slices.Equal(got.Topics, want) || got.UpdatedAt != start.Add(time.Second).UnixMilli() {
		t.Fatalf("Upsert with a new topic returned %+v, want topics %v stamped now", got, want)
	}

	clock.Set(start.Add(2 * time.Second))
	upsertOrFail(t, reg, "ses_topics", "notifications.team.new", "notifications.agent.ses_topics")
	upsertOrFail(t, reg, "ses_topics", "notifications.agent.ses_topics", "notifications.team.new")
	if revision := interestRevision(t, kv, "ses_topics"); revision != created+1 {
		t.Fatalf("re-sending the stored topics moved the row to revision %d, want %d", revision, created+1)
	}
}

// A row stored with a null topic list is not the [] a write would store, so the first heartbeat
// over it writes it once, however young it is, and the next one finds it unchanged.
func TestUpsertWritesARowStoredWithNullTopicsOnce(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	putInterest(t, kv, Interest{SessionID: "ses_null", MachineID: "m1", Dir: "/work/ses_null", UpdatedAt: start.UnixMilli()})
	stored := interestRevision(t, kv, "ses_null")

	clock.Set(start.Add(time.Second))
	upsertOrFail(t, reg, "ses_null")
	if revision := interestRevision(t, kv, "ses_null"); revision != stored+1 {
		t.Fatalf("Upsert over a null topic list left revision %d, want one write (%d)", revision, stored+1)
	}
	entry, err := kv.Get("ses_null")
	if err != nil {
		t.Fatalf("get ses_null: %v", err)
	}
	if !bytes.Contains(entry.Value(), []byte(`"topics":[]`)) {
		t.Fatalf("stored row = %s, want an empty topic list", entry.Value())
	}

	clock.Set(start.Add(2 * time.Second))
	upsertOrFail(t, reg, "ses_null")
	if revision := interestRevision(t, kv, "ses_null"); revision != stored+1 {
		t.Fatalf("Upsert over the rewritten row moved it to revision %d, want %d", revision, stored+1)
	}
}

func TestRemove_ColdCacheReadsFromKV(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_rm", MachineID: "m1", Topics: []string{"topic.a", "topic.b", "topic.c"}})

	// Remove topic B — cache is cold, must read from KV
	if err := reg.Remove(context.Background(), "ses_rm", []string{"topic.b"}); err != nil {
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

// An unsubscribe and a role claim rewrite the session's row too, and neither keeps a topic the row
// stored before the listener refused topics reaching a private stream subject: another session's
// agent subject and a failed-delivery lane are dropped, so Match stops fanning that lane out to the
// session, while its own agent subject and the issue topic it follows stay.
func TestRemoveAndSetRole_DropStoredTopicsReachingAPrivateStreamSubject(t *testing.T) {
	const lane = "notifications.envoy.exceptions.notifications.agent.ses_other"
	stale := []string{
		"notifications.agent.ses_other",
		"notifications.agent.ses_stale",
		"notifications.dispatch.issue.KEY-1.>",
		"notifications.envoy.exceptions.>",
		"topic.gone",
	}
	for name, write := range map[string]func(*Registry) ([]string, error){
		"unsubscribe": func(reg *Registry) ([]string, error) {
			return []string{"notifications.agent.ses_stale", "notifications.dispatch.issue.KEY-1.>"},
				reg.Remove(context.Background(), "ses_stale", []string{"topic.gone"})
		},
		"role set": func(reg *Registry) ([]string, error) {
			_, err := reg.SetRole("ses_stale", "m1", "reviewer", false)
			return []string{"notifications.agent.ses_stale", "notifications.dispatch.issue.KEY-1.>", "notifications.role.reviewer", "topic.gone"}, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			reg, kv := coldRegistry(t, conn)
			putInterest(t, kv, Interest{SessionID: "ses_stale", MachineID: "m1", Topics: stale})
			// Get caches the stored row, so Match fans the lane out to the session before the write.
			if _, err := reg.Get(context.Background(), "ses_stale"); err != nil {
				t.Fatalf("read the seeded row: %v", err)
			}
			if matched := reg.Match("m1", lane); len(matched) != 1 {
				t.Fatalf("Match(%s) before the write = %v, want the seeded session", lane, matched)
			}

			want, err := write(reg)
			if err != nil {
				t.Fatalf("write the row: %v", err)
			}

			entry, err := kv.Get("ses_stale")
			if err != nil {
				t.Fatalf("read the stored row: %v", err)
			}
			var item Interest
			if err := json.Unmarshal(entry.Value(), &item); err != nil {
				t.Fatalf("decode the stored row: %v", err)
			}
			got := slices.Sorted(slices.Values(item.Topics))
			if !slices.Equal(got, want) {
				t.Fatalf("stored topics = %v, want %v", got, want)
			}
			if matched := reg.Match("m1", lane); len(matched) != 0 {
				t.Fatalf("Match(%s) = %v, want no session", lane, matched)
			}
		})
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

	persisted, err := reg.Get(context.Background(), "ses_role_a")
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

	persisted, err := reg.Get(context.Background(), "ses_same")
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
	if err := reg.Remove(context.Background(), "ses_role", []string{"notifications.role.legion-controller"}); err != nil {
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
	if _, err := reg.Get(context.Background(), "ses_alive"); err != nil {
		t.Fatalf("Get ses_alive failed: %v", err)
	}
	if _, err := reg.Get(context.Background(), "ses_dead"); err != nil {
		t.Fatalf("Get ses_dead failed: %v", err)
	}

	// Reap with 0 grace window: a dead session's row older than InterestRefreshAge (this
	// one is 10 minutes old) is reaped, and the live session's row is kept.
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
	stale, err := reg.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Get role interest: %v", err)
	}
	stale.UpdatedAt = time.Now().Add(-(InterestRefreshAge + time.Minute)).UnixMilli()
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

	release, err := registry.ReleaseExpiredRoleClaim(context.Background(), role, "ses_lapsed", 0)
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

	release, err := registry.ReleaseExpiredRoleClaim(context.Background(), role, "ses_lapsed", 0)
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

	release, err := registry.ReleaseExpiredRoleClaim(context.Background(), role, "ses_lapsed", 0)
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
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return fixed }

	// Dead sessions whose rows were last written 14, exactly 15, and 16 minutes ago: the
	// listener's grace (InterestReapGrace) plus InterestRefreshAge is 15 minutes, and only a
	// row strictly older than that is reaped.
	rows := map[string]time.Duration{
		"ses_14m": 14 * time.Minute,
		"ses_15m": 15 * time.Minute,
		"ses_16m": 16 * time.Minute,
	}
	for sid, age := range rows {
		putInterest(t, kv, Interest{SessionID: sid, MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: fixed.Add(-age).UnixMilli()})
		// Warm cache
		if _, err := reg.Get(context.Background(), sid); err != nil {
			t.Fatalf("Get %s failed: %v", sid, err)
		}
	}

	count, err := reg.Reap(func(string) bool { return false }, InterestReapGrace)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 reaped (only the 16-minute row), got %d", count)
	}

	for _, sid := range []string{"ses_14m", "ses_15m"} {
		if _, err := kv.Get(sid); err != nil {
			t.Fatalf("%s should still exist within grace window plus refresh age: %v", sid, err)
		}
	}
	if _, err := kv.Get("ses_16m"); err != natsgo.ErrKeyNotFound {
		t.Fatalf("ses_16m should be deleted, got err: %v", err)
	}
}

// A session that heartbeats every 90 seconds and then pauses keeps its interest row as it did when
// every heartbeat wrote the row: the unchanged row is not rewritten for up to InterestRefreshAge,
// so its UpdatedAt lags the last heartbeat, but the listener's reaper (InterestReapGrace, plus
// InterestRefreshAge in Reap) deletes nothing within InterestReapGrace of that heartbeat, long
// after the session row's 5-minute TTL has made the session dead. A session back under the same
// id at 6 or at 9 minutes finds its row and topics.
func TestAPausedSessionKeepsItsInterestThroughTheReapGrace(t *testing.T) {
	const sessionTTL = 5 * time.Minute // the session bucket's MaxAge, the liveness clock
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	const github = "notifications.github.acme.widgets.pr.>"
	ids := []string{"ses_back6", "ses_back9", "ses_gone"}
	lastHeartbeat := map[string]time.Time{}
	heartbeat := func(sessionID string, topics ...string) Interest {
		t.Helper()
		lastHeartbeat[sessionID] = clock.Now()
		return upsertOrFail(t, reg, sessionID, topics...)
	}
	isAlive := func(sessionID string) bool { return clock.Now().Sub(lastHeartbeat[sessionID]) < sessionTTL }
	reapNothing := func(when string) {
		t.Helper()
		count, err := reg.Reap(isAlive, InterestReapGrace)
		if err != nil {
			t.Fatalf("%s: Reap: %v", when, err)
		}
		if count != 0 {
			t.Fatalf("%s: Reap deleted %d interests, want none", when, count)
		}
		if _, err := kv.Get("ses_gone"); err != nil {
			t.Fatalf("%s: ses_gone's interest row is gone: %v", when, err)
		}
	}

	revisions := map[string]uint64{}
	for _, id := range ids {
		heartbeat(id, "notifications.agent."+id, github)
		revisions[id] = interestRevision(t, kv, id)
	}
	for beat := 1; beat <= 3; beat++ {
		clock.Set(start.Add(time.Duration(beat) * 90 * time.Second))
		for _, id := range ids {
			heartbeat(id, "notifications.agent."+id, github)
			if revision := interestRevision(t, kv, id); revision != revisions[id] {
				t.Fatalf("heartbeat %d moved %s's unchanged row from revision %d to %d", beat, id, revisions[id], revision)
			}
		}
		reapNothing(fmt.Sprintf("heartbeat %d", beat))
	}
	last := clock.Now()

	for _, after := range []time.Duration{90 * time.Second, sessionTTL, 6 * time.Minute, 9 * time.Minute, InterestReapGrace - time.Millisecond, InterestReapGrace} {
		clock.Set(last.Add(after))
		if after >= sessionTTL && isAlive("ses_gone") {
			t.Fatalf("ses_gone is alive %v after its last heartbeat, past the session TTL", after)
		}
		reapNothing(fmt.Sprintf("%v after the last heartbeat", after))
		var back string
		switch after {
		case 6 * time.Minute:
			back = "ses_back6"
		case 9 * time.Minute:
			back = "ses_back9"
		default:
			continue
		}
		if isAlive(back) {
			t.Fatalf("%s is alive %v after its last heartbeat, past the session TTL", back, after)
		}
		got := heartbeat(back, "notifications.agent."+back)
		if want := []string{"notifications.agent." + back, github}; !slices.Equal(got.Topics, want) {
			t.Fatalf("%s back %v after its last heartbeat has topics %v, want %v", back, after, got.Topics, want)
		}
	}

	// The reaper does take the row that never came back, once it is InterestReapGrace plus
	// InterestRefreshAge past its last write.
	clock.Set(start.Add(InterestReapGrace + InterestRefreshAge + time.Millisecond))
	count, err := reg.Reap(isAlive, InterestReapGrace)
	if err != nil {
		t.Fatalf("final Reap: %v", err)
	}
	if count != 1 {
		t.Fatalf("final Reap deleted %d interests, want ses_gone's alone", count)
	}
	if _, err := kv.Get("ses_gone"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_gone should be reaped, got %v", err)
	}
}

// upsertWithin upserts item on another goroutine and reports whether it finished within five
// seconds, failing the test when it did not or when it failed. A reaper pass calls it from isAlive,
// so a pass that held the interest lock there would make it time out rather than deadlock.
func upsertWithin(t *testing.T, reg *Registry, item Interest, topics []string) bool {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := reg.Upsert(item, topics)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Upsert(%s) during the reaper pass: %v", item.SessionID, err)
			return false
		}
		return true
	case <-time.After(5 * time.Second):
		t.Errorf("Upsert(%s) did not finish while Reap was calling isAlive: the pass holds the interest lock", item.SessionID)
		return false
	}
}

func TestReapCallsIsAliveWithoutHoldingTheInterestLock(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_dead", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: time.Now().Add(-10 * time.Minute).UnixMilli()})
	if _, err := reg.Get(context.Background(), "ses_dead"); err != nil {
		t.Fatalf("Get ses_dead failed: %v", err)
	}

	upserted := false
	count, err := reg.Reap(func(sessionID string) bool {
		if sessionID == "ses_dead" {
			upserted = upsertWithin(t, reg, Interest{SessionID: "ses_other", MachineID: "m1"}, []string{"notifications.agent.ses_other"})
		}
		return false
	}, 0)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if !upserted {
		t.Fatal("isAlive never upserted ses_other")
	}
	if count != 1 {
		t.Fatalf("expected 1 reaped, got %d", count)
	}
	if _, err := kv.Get("ses_dead"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_dead should be deleted, got err: %v", err)
	}
	if _, err := kv.Get("ses_other"); err != nil {
		t.Fatalf("ses_other, upserted during the pass, should exist: %v", err)
	}
}

func TestReapKeepsAnInterestReupsertedDuringThePass(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_back", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: time.Now().Add(-10 * time.Minute).UnixMilli()})
	if _, err := reg.Get(context.Background(), "ses_back"); err != nil {
		t.Fatalf("Get ses_back failed: %v", err)
	}

	// The session heartbeats again while the pass is deciding it is dead.
	count, err := reg.Reap(func(sessionID string) bool {
		if sessionID == "ses_back" {
			upsertWithin(t, reg, Interest{SessionID: "ses_back", MachineID: "m1"}, []string{"notifications.agent.ses_back"})
		}
		return false
	}, 0)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 reaped, got %d", count)
	}
	if _, err := kv.Get("ses_back"); err != nil {
		t.Fatalf("ses_back, re-upserted during the pass, should exist: %v", err)
	}
	if _, err := reg.Get(context.Background(), "ses_back"); err != nil {
		t.Fatalf("ses_back should stay cached: %v", err)
	}
}

func TestReapKeepsAnInterestWrittenAfterItsRecheck(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_raced", MachineID: "m1", Topics: []string{"notifications.>"}, UpdatedAt: time.Now().Add(-10 * time.Minute).UnixMilli()})
	if _, err := reg.Get(context.Background(), "ses_raced"); err != nil {
		t.Fatalf("Get ses_raced failed: %v", err)
	}
	// Another listener's write lands after the pass re-checked the candidate and before its delete
	// reaches the bucket.
	fresh := Interest{SessionID: "ses_raced", MachineID: "m2", Topics: []string{"notifications.>"}, UpdatedAt: time.Now().UnixMilli()}
	reg.watcher = kvwatch.New("interest registry", bus.KeyValue{KeyValue: &putBeforeDeleteKeyValue{
		KeyValue:     kv,
		beforeDelete: func() { putInterest(t, kv, fresh) },
	}}, reg.applyWatched, reg.resetCache)

	count, err := reg.Reap(func(string) bool { return false }, 0)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 reaped, got %d", count)
	}
	entry, err := kv.Get("ses_raced")
	if err != nil {
		t.Fatalf("ses_raced, written before the delete landed, should exist: %v", err)
	}
	var got Interest
	if err := json.Unmarshal(entry.Value(), &got); err != nil {
		t.Fatalf("decode ses_raced: %v", err)
	}
	if got.MachineID != "m2" {
		t.Fatalf("ses_raced = %+v, want the write that landed during the pass", got)
	}
}

// putBeforeDeleteKeyValue runs beforeDelete before its first Delete reaches the bucket, and then
// sends that Delete as it was asked to.
type putBeforeDeleteKeyValue struct {
	natsgo.KeyValue

	deletes      int
	beforeDelete func()
}

func (kv *putBeforeDeleteKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.deletes++
	if kv.deletes == 1 {
		kv.beforeDelete()
	}
	return kv.KeyValue.Delete(key, opts...)
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
	// A closed KV handle must remain observable through Ping so /healthz can
	// report the unavailable dependency while NATS reconnects.
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

// --- Cache readiness tests ---
//
// The initial-cache-warmup race: after Open() returns, watch() populates the cache
// asynchronously, and events arriving in that window get "no matching
// interests" even when the durable KV entry has subscribers.

func TestWaitForCacheReady_ReturnsAfterInitialScan(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, err := Open(conn, WithReplicas(1))
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
	reg1, err := Open(conn, WithReplicas(1))
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
	reg2, err := Open(conn, WithReplicas(1))
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
	if err := reg.Remove(context.Background(), sessionID, []string{"notifications.b"}); err != nil {
		t.Fatalf("unsubscribe failed: %v", err)
	}
	if _, err := reg.Upsert(
		Interest{SessionID: sessionID, MachineID: "example-host"},
		[]string{"notifications.a"},
	); err != nil {
		t.Fatalf("heartbeat re-registration failed: %v", err)
	}

	item, err := reg.Get(context.Background(), sessionID)
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
	if err := reg.Remove(context.Background(), sessionID, nil); err != nil {
		t.Fatalf("unsubscribe all failed: %v", err)
	}

	if _, err := reg.Get(context.Background(), sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
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
	if err := reg.Remove(context.Background(), sessionID, nil); err != nil {
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
	if err := reg.Remove(context.Background(), sessionID, nil); err != nil {
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
		KeyValue:     kv,
		failCreateAt: 1,
		err:          errors.New("injected interest write failure"),
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

// A claim that landed is the success: the old holder's role topic a failed cleanup leaves gives it
// no role, since the claim alone says who holds one, so the failure is logged at WARN and not
// returned.
func TestSetRoleSucceedsWhenOldHolderCleanupFails(t *testing.T) {
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
		KeyValue:     kv,
		failUpdateAt: 1,
		err:          errors.New("injected old-holder cleanup failure"),
	})
	if _, err := reg.SetRole(newSession, "example-host", role, false); err != nil {
		t.Fatalf("SetRole returned %v after its claim landed, want success", err)
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

	reg, err := Open(conn, WithReplicas(1))
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
	if _, err := reg.Get(context.Background(), sessionID); err == nil {
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
	if err := r.watcher.WaitReady(ctx); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
	}
}

type failingKeyValue struct {
	natsgo.KeyValue

	failCreateAt int
	failUpdateAt int
	creates      int
	updates      int
	err          error
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
	item, err := reg.Get(context.Background(), sessionID)
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
	if _, err := reg.Get(context.Background(), sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%q) error = %v, want ErrKeyNotFound", sessionID, err)
	}
}

func captureRegistryLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
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
	if _, err := reg.Get(context.Background(), "ses_stale_transcript"); err == nil {
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

	reg, err := Open(conn, WithReplicas(1))
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
	useKV(t, reg, &interleavingWriteKeyValue{
		KeyValue: interestKV,
		afterFirstWrite: func() {
			putInterest(t, interestKV, Interest{
				SessionID: sessionID,
				MachineID: machineID,
				Topics:    []string{"notifications.remote"},
			})
			pollMatch(t, reg, machineID, "notifications.remote", 1, 5*time.Second)
		},
	})
	if err := reg.Remove(context.Background(), sessionID, []string{"notifications.b"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	item, err := reg.Get(context.Background(), sessionID)
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

// interleavingWriteKeyValue runs afterFirstWrite once the first write through it (Put, Create or
// Update) has landed, before the registry applies its write-through.
type interleavingWriteKeyValue struct {
	natsgo.KeyValue

	writes          int
	afterFirstWrite func()
}

func (kv *interleavingWriteKeyValue) wrote(revision uint64, err error) (uint64, error) {
	if err != nil {
		return 0, err
	}
	kv.writes++
	if kv.writes == 1 {
		kv.afterFirstWrite()
	}
	return revision, nil
}

func (kv *interleavingWriteKeyValue) Put(key string, value []byte) (uint64, error) {
	return kv.wrote(kv.KeyValue.Put(key, value))
}

func (kv *interleavingWriteKeyValue) Create(key string, value []byte) (uint64, error) {
	return kv.wrote(kv.KeyValue.Create(key, value))
}

func (kv *interleavingWriteKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	return kv.wrote(kv.KeyValue.Update(key, value, last))
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
	if err := reg.removeInterestTopics(context.Background(), sessionID, nil); err != nil {
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
	if _, err := reg.Get(context.Background(), sessionID); err == nil {
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

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// The registry's clock stamps what it records: an interest's UpdatedAt and a role claim's
// ClaimedAt come from the clock Open was given, as the grace window does.
func TestTheRegistryClockStampsWhatItRecords(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	reg, err := Open(conn, WithReplicas(1), WithClock(func() time.Time { return at }))
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
	claim, err := reg.RoleClaim(context.Background(), "clock-role")
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
	reg, err := Open(conn, WithReplicas(1))
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
	if err := js.DeleteKeyValue(bus.InterestBucket); err != nil {
		t.Fatalf("delete the interest bucket: %v", err)
	}
	// Through the rig's retry, as every other delete-then-create of one name in this module is:
	// nats-server answers a delete before it has removed the account's streams directory, and a
	// create inside that window is refused. The role bucket Open made is still in this account, so
	// the directory is not being removed here — but that is a fact about this test's neighbours,
	// not about the call, and the retry does not depend on it.
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
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
	reg, err := Open(first, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	js, err := first.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	roles, roleWatch := reg.roles(), reg.roleWatcher.KV()
	roleGeneration, _ := reg.roleWatcher.Complete()
	second, closeSecond := connectNATS(t)
	if err := reg.Rewatch(second); err == nil {
		t.Fatal("Rewatch onto a connection without the role bucket succeeded")
	}
	closeSecond()
	if _, err := reg.Upsert(Interest{SessionID: "ses_still_first", MachineID: "m1"}, []string{"notifications.agent.x"}); err != nil {
		t.Fatalf("Upsert after the failed Rewatch: %v; the interest handle moved to the closed connection", err)
	}
	// WatchErr was the interest watcher's error alone when this test was written. It now joins the
	// role claim watcher's, which ends on its own here because the test deleted its bucket, so the
	// interest watcher's is read directly, and the role side is held to moving nothing too.
	if err := reg.watcher.Err(); err != nil {
		t.Fatalf("the interest watcher's error after the failed Rewatch: %v; it moved to the closed connection", err)
	}
	if reg.roles() != roles || reg.roleWatcher.KV() != roleWatch {
		t.Fatal("a Rewatch that could not open the role bucket moved a role bucket handle")
	}
	if generation, _ := reg.roleWatcher.Complete(); generation != roleGeneration {
		t.Fatal("a Rewatch that could not open the role bucket replaced the role claim watcher")
	}
}

// A Rewatch whose interest watcher cannot move moves nothing either: the role handle and the role
// claim watcher stay on the connection they were on, so the registry does not read and write the
// two buckets through different connections, and the role claim view still answers there.
func TestARewatchWhoseInterestWatcherCannotMoveMovesNothing(t *testing.T) {
	first, closeFirst := connectNATS(t)
	defer closeFirst()
	reg, err := Open(first, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	if _, err := reg.SetRole("ses_role_holder", "m1", "reviewer", false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		roles, err := reg.RolesHeldBy("ses_role_holder")
		return err == nil && slices.Equal(roles, []string{"reviewer"})
	})
	js, err := first.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.InterestBucket); err != nil {
		t.Fatalf("delete the interest bucket: %v", err)
	}
	roles, roleWatch := reg.roles(), reg.roleWatcher.KV()
	roleGeneration, _ := reg.roleWatcher.Complete()
	second, closeSecond := connectNATS(t)
	if err := reg.Rewatch(second); err == nil {
		t.Fatal("Rewatch onto a connection without the interest bucket succeeded")
	}
	closeSecond()
	if reg.roles() != roles || reg.roleWatcher.KV() != roleWatch {
		t.Fatal("a Rewatch whose interest watcher could not move moved a role bucket handle")
	}
	if generation, _ := reg.roleWatcher.Complete(); generation != roleGeneration {
		t.Fatal("a Rewatch whose interest watcher could not move replaced the role claim watcher")
	}
	if got, err := reg.RolesHeldBy("ses_role_holder"); err != nil || !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("after the failed Rewatch RolesHeldBy = %v, %v; want [reviewer] from the view on the first connection", got, err)
	}
	if _, err := reg.RoleClaim(context.Background(), "reviewer"); err != nil {
		t.Fatalf("a role claim read after the failed Rewatch: %v; the role handle moved to the closed connection", err)
	}
}

// A claim another listener's registry writes reaches this registry's CachedRoleHolder through the
// role watcher, and so do its release and its replacement, and the cache answers with no read of the
// bucket, so the role lane resolves a holder without a round trip per message.
func TestCachedRoleHolderFollowsAnotherRegistrysClaimsWithoutReadingTheBucket(t *testing.T) {
	uri := testnats.URL(t)
	reader, writer := testnats.Connect(t, uri), testnats.Connect(t, uri)
	defer reader.Close()
	defer writer.Close()
	readerRegistry, err := Open(reader, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the reading registry: %v", err)
	}
	t.Cleanup(readerRegistry.StopWatch)
	writerRegistry, err := Open(writer, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the writing registry: %v", err)
	}
	t.Cleanup(writerRegistry.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := readerRegistry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the reading registry: %v", err)
	}
	gets, err := reader.SubscribeSync(fmt.Sprintf("$JS.API.DIRECT.GET.KV_%s.>", bus.RoleClaimBucket))
	if err != nil {
		t.Fatalf("watch direct gets: %v", err)
	}
	defer func() { _ = gets.Unsubscribe() }()
	legacyGets, err := reader.SubscribeSync(fmt.Sprintf("$JS.API.STREAM.MSG.GET.KV_%s", bus.RoleClaimBucket))
	if err != nil {
		t.Fatalf("watch stream message gets: %v", err)
	}
	defer func() { _ = legacyGets.Unsubscribe() }()
	reads := func() int {
		t.Helper()
		if err := reader.Flush(); err != nil {
			t.Fatalf("flush the reader: %v", err)
		}
		return pending(t, gets) + pending(t, legacyGets)
	}
	watched := func(role string) string {
		readerRegistry.holdersMu.RLock()
		defer readerRegistry.holdersMu.RUnlock()
		return readerRegistry.holders[role]
	}
	requireHolder := func(role, want string) {
		t.Helper()
		waitFor(t, 5*time.Second, func() bool { return watched(role) == want })
		before := reads()
		holder, err := readerRegistry.CachedRoleHolder(role)
		if err != nil || holder != want {
			t.Fatalf("CachedRoleHolder(%s) = %q, %v; want %q", role, holder, err, want)
		}
		if after := reads(); after != before {
			t.Fatalf("CachedRoleHolder(%s) read the bucket %d times, want none", role, after-before)
		}
	}

	if _, err := writerRegistry.SetRole("ses_first", "m1", "reviewer", false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	requireHolder("reviewer", "ses_first")
	if _, err := writerRegistry.SetRole("ses_second", "m1", "reviewer", false); err != nil {
		t.Fatalf("take the role over: %v", err)
	}
	requireHolder("reviewer", "ses_second")
	if err := writerRegistry.Remove(context.Background(), "ses_second", []string{contracts.RoleTopicPrefix + "reviewer"}); err != nil {
		t.Fatalf("release the role: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return watched("reviewer") == "" })
	if holder, err := readerRegistry.CachedRoleHolder("reviewer"); err != nil || holder != "" {
		t.Fatalf("CachedRoleHolder after the release = %q, %v; want no holder", holder, err)
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
