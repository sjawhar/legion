package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

func waitUntil(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	testwait.WithinPolling(t, timeout, testwait.Poll, desc, fn)
}

func TestE2E_InterestReaperCleanup(t *testing.T) {
	env := setupTestEnv(t)
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_stale", MachineID: "test-machine", Dir: "/stale"}, []string{"notifications.slack.*.*.mention"}); err != nil {
		t.Fatalf("upsert stale interest: %v", err)
	}

	interest, err := env.registry.Get(context.Background(), "ses_stale")
	if err != nil {
		t.Fatalf("get stale interest: %v", err)
	}
	interest.UpdatedAt = time.Now().Add(-(store.InterestReapGrace + store.InterestRefreshAge + time.Minute)).UnixMilli()
	data, err := json.Marshal(interest)
	if err != nil {
		t.Fatalf("marshal stale interest: %v", err)
	}
	js, err := env.client.Conn.JetStream(natsgo.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open kv bucket: %v", err)
	}
	if _, err := kv.Put("ses_stale", data); err != nil {
		t.Fatalf("put stale interest: %v", err)
	}
	waitUntil(t, 5*time.Second, "stale cache update", func() bool {
		current, getErr := env.registry.Get(context.Background(), "ses_stale")
		return getErr == nil && current.UpdatedAt == interest.UpdatedAt
	})

	reaped, err := env.registry.Reap(func(string) (bool, error) { return false, nil }, store.InterestReapGrace)
	if err != nil {
		t.Fatalf("reap stale interests: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped interest, got %d", reaped)
	}
	waitUntil(t, 5*time.Second, "stale interest deletion", func() bool {
		_, getErr := env.registry.Get(context.Background(), "ses_stale")
		return getErr != nil && errors.Is(getErr, natsgo.ErrKeyNotFound)
	})
}
