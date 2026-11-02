package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
)

func waitUntil(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	if fn() {
		return
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeoutCh := time.After(timeout)
	for {
		select {
		case <-timeoutCh:
			t.Fatalf("timed out waiting for %s", desc)
		case <-ticker.C:
			if fn() {
				return
			}
		}
	}
}

func waitForMatchCount(t *testing.T, env *testEnv, machineID, topic string, want int) {
	t.Helper()
	waitUntil(t, 5*time.Second, fmt.Sprintf("%d matches for %s", want, topic), func() bool {
		return len(env.registry.Match(machineID, topic)) == want
	})
}

// A webhook's event reaches the session that follows its topic whole: the topic it was published
// on, its payload summary and the sender that published it all arrive at the session's own
// subscription, and nothing else does.
func TestAWebhookEventReachesTheSessionFollowingItsTopicWhole(t *testing.T) {
	env := setupTestEnv(t)
	recipient := env.registerSession("ses_webhook", []string{"notifications.slack.*.*.mention"})
	topic := "notifications.slack.T123.C456.mention"
	waitForMatchCount(t, env, "test-machine", topic, 1)

	item := newEnvelope("slack", topic, "hello from slack", "dedupe-webhook")
	item.SourceSession = "ses_origin"
	publishEnvelope(env, item)

	got := recipient.receives(t, 5*time.Second)
	if got.EventID != item.EventID || got.Topic != item.Topic {
		t.Fatalf("the session received %+v, want the published envelope on %s", got, item.Topic)
	}
	if got.PayloadSummary != "hello from slack" || got.Source != "slack" || got.SourceSession != "ses_origin" {
		t.Fatalf("the session received %+v, want the webhook's payload from its own sender", got)
	}
	recipient.receivesNothing(t, 300*time.Millisecond)
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

	reaped, err := env.registry.Reap(func(string) bool { return false }, store.InterestReapGrace)
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
