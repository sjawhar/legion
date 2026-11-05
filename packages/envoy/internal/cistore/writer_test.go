package cistore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openListener opens the listener's own store, which creates the CI bucket and starts the cache
// watcher a writer has not got, and waits for its cache.
func openListener(t *testing.T, conn *natsgo.Conn) *cistore.Store {
	t.Helper()
	store, err := cistore.Open(conn, logging.New("listener"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open the listener's cistore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("the listener's cache is not ready: %v", err)
	}
	return store
}

// A writer's record reaches a settlement, not only a key: it writes a check run and a check suite
// of one head into the bucket with no watcher of its own, and the listener's store sees them
// through its cache watcher and its real summary loop publishes the head's settlement. The record
// lands under cistore.Key, the key the listener's own store writes, so the two processes share one
// record per head rather than each keeping its own.
func TestAWriterRecordSettlesThroughTheListenersSummaryLoop(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store := openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Conn, logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	sub, err := client.Conn.SubscribeSync("notifications.github." + owner + "." + repo + ".pr." + number + ".checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 100*time.Millisecond, 20*time.Millisecond,
		metrics.New().NewGauge("held", ""), logging.New("loop"))

	if err := writer.Record(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		CheckName:  "build",
		CheckRunID: 7,
		URL:        "https://github.com/example-org/example-repo/runs/7",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}); err != nil {
		t.Fatalf("the writer's check run: %v", err)
	}
	if err := writer.RecordSuite(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		SuiteID:    "9001",
		AppID:      "15368",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:05Z",
	}); err != nil {
		t.Fatalf("the writer's check suite: %v", err)
	}

	msg, err := sub.NextMsg(10 * time.Second)
	if err != nil {
		t.Fatalf("the writer's head did not settle: %v", err)
	}
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("the settlement is not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("the settlement's payload is not JSON: %v\n%s", err, env.Payload)
	}
	if sum.Kind != "checks" || sum.SHA != sha || sum.Passed.Count != 1 || sum.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want one passing check of %s", sum, sha)
	}

	// Read straight from the bucket, which is where the listener's store writes the head's record
	// too: one key per head, whichever process wrote it.
	entry, err := client.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	raw, err := entry.Get(cistore.Key(owner, repo, number, sha))
	if err != nil {
		t.Fatalf("get the head's record under cistore.Key: %v", err)
	}
	var state cistore.State
	if err := json.Unmarshal(raw.Value(), &state); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	if _, ok := state.Checks["build"]; !ok {
		t.Fatalf("the record holds no build check: %+v", state)
	}
	if _, ok := state.Suites["9001"]; !ok {
		t.Fatalf("the record holds no check suite: %+v", state)
	}
}

// A writer binds an existing bucket and creates none: where the listener has never run, OpenWriter
// fails with what bus.OpenKeyValue gives and leaves the bucket unmade, so the listener's Open stays
// the one owner of its TTL and its copies.
func TestOpenWriterFailsAndCreatesNothingWithoutTheBucket(t *testing.T) {
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()

	writer, err := cistore.OpenWriter(client.Conn, logging.New("writer"))
	if !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("OpenWriter without the bucket = %v, want %v", err, natsgo.ErrBucketNotFound)
	}
	if writer != nil {
		t.Fatalf("OpenWriter returned a store for a bucket that is not there: %+v", writer)
	}
	if _, err := client.JS().KeyValue(bus.CIStateBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the CI bucket after the refused OpenWriter: %v, want %v", err, natsgo.ErrBucketNotFound)
	}
}

// HasRecord answers from the bucket: false for a head no record is held of, and true for one
// another store wrote, which this writer's own process never wrote and no cache of its own could
// answer for.
func TestHasRecordReadsTheBucketRatherThanACache(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store := openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Conn, logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	const (
		owner    = "example-org"
		repo     = "example-repo"
		number   = "42"
		recorded = "1111111111111111111111111111111111111111"
		unknown  = "2222222222222222222222222222222222222222"
	)
	has, err := writer.HasRecord(owner, repo, number, recorded)
	if err != nil {
		t.Fatalf("HasRecord before the record: %v", err)
	}
	if has {
		t.Fatal("HasRecord is true for a head the bucket holds no record of")
	}

	if err := store.Record(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        recorded,
		CheckName:  "build",
		CheckRunID: 7,
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}); err != nil {
		t.Fatalf("the second store's check run: %v", err)
	}

	has, err = writer.HasRecord(owner, repo, number, recorded)
	if err != nil {
		t.Fatalf("HasRecord after the record: %v", err)
	}
	if !has {
		t.Fatal("HasRecord is false for a head the bucket holds the record of")
	}
	has, err = writer.HasRecord(owner, repo, number, unknown)
	if err != nil {
		t.Fatalf("HasRecord for another head: %v", err)
	}
	if has {
		t.Fatal("HasRecord is true for a head nothing wrote")
	}
}
