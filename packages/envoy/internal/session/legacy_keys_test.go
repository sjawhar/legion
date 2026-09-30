package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener built before the KV key check registered sessions under ids this one refuses to write
// (testnats.LegacyKeys). The registry opens over them and serves them from its cache, and removing
// one deletes it whether or not this build can read its key.
func TestASessionRegistryOverKeysAnEarlierListenerStoredServesAndDeletesThem(t *testing.T) {
	client := setupNATS(t)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	raw, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: client.Bucket, TTL: 10 * time.Second, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create session bucket: %v", err)
	}
	readable, unreadable := testnats.LegacyKeys(client.Bucket, "s")
	for _, sessionID := range []string{"ses_ordinary", readable, unreadable} {
		entry, err := json.Marshal(SessionEntry{Port: 13381, MachineID: "earlier", UpdatedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode entry: %v", err)
		}
		if _, err := raw.Put(sessionID, entry); err != nil {
			t.Fatalf("store the %d-byte session: %v", len(sessionID), err)
		}
	}

	reg, err := client.OpenRegistry(WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("open over the earlier listener's keys: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the cache: %v", err)
	}
	for _, sessionID := range []string{"ses_ordinary", readable, unreadable} {
		if _, err := reg.Get(sessionID); err != nil {
			t.Fatalf("get the %d-byte session: %v", len(sessionID), err)
		}
	}
	for _, sessionID := range []string{unreadable, readable} {
		if err := reg.Delete(sessionID); err != nil {
			t.Fatalf("delete the %d-byte session: %v", len(sessionID), err)
		}
		if _, err := reg.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("get the deleted %d-byte session = %v, want ErrKeyNotFound", len(sessionID), err)
		}
	}
	keys, err := raw.Keys()
	if err != nil || !slices.Equal(keys, []string{"ses_ordinary"}) {
		t.Fatalf("the bucket holds %d keys (%v) after both deletes, want only ses_ordinary", len(keys), err)
	}
}
