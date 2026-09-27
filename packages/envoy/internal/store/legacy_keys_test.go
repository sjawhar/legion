package store

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

// A listener built before the KV key check stored role claims and interests under keys this one
// refuses to write (testnats.LegacyKeys). Such a key must neither stop the registry from opening
// nor fail a sweep: the registry opens and resolves every role it can read, removing a session skips
// a claim it cannot read, and the reapers delete both the entries they can read and the ones they
// cannot, whose holders no build can reach any more.
func TestARegistryOverKeysAnEarlierListenerStoredOpensAndReapsThem(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	names := testBuckets(t)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawInterests, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: names.interests, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: names.roles, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create role bucket: %v", err)
	}
	readableRole, unreadableRole := testnats.LegacyKeys(names.roles, "r")
	readableSession, unreadableSession := testnats.LegacyKeys(names.interests, "s")
	stale := time.Now().Add(-time.Hour).UnixMilli()
	for role, holder := range map[string]string{"reviewer": "ses_live", readableRole: readableSession, unreadableRole: unreadableSession} {
		claim, err := json.Marshal(RoleClaim{HolderSessionID: holder, ClaimedAt: stale})
		if err != nil {
			t.Fatalf("encode claim: %v", err)
		}
		if _, err := rawRoles.Put(role, claim); err != nil {
			t.Fatalf("store the %d-byte role: %v", len(role), err)
		}
	}
	for _, sessionID := range []string{"ses_live", readableSession, unreadableSession} {
		putInterest(t, rawInterests, Interest{SessionID: sessionID, MachineID: "earlier", Topics: []string{"notifications.agent." + sessionID[:8]}, UpdatedAt: stale})
	}

	registry, err := Open(conn, WithReplicas(1), withTestBuckets(t))
	if err != nil {
		t.Fatalf("open over the earlier listener's keys: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the cache: %v", err)
	}
	assertRoleHolder(t, registry, "reviewer", "ses_live")
	assertRoleHolder(t, registry, readableRole, readableSession)
	for _, sessionID := range []string{"ses_live", readableSession, unreadableSession} {
		if _, err := registry.Get(sessionID); err != nil {
			t.Fatalf("get the %d-byte session's interest: %v", len(sessionID), err)
		}
	}
	if err := registry.Remove("ses_elsewhere", nil); err != nil {
		t.Fatalf("remove a session while an unreadable claim is stored: %v", err)
	}

	isAlive := func(sessionID string) bool { return sessionID == "ses_live" }
	if reaped, err := registry.Reap(isAlive, 0); err != nil || reaped != 2 {
		t.Fatalf("reap = %d, %v; want both earlier sessions' interests", reaped, err)
	}
	if _, err := registry.ReapRoleClaims(isAlive, 0); err != nil {
		t.Fatalf("reap role claims: %v", err)
	}
	for bucket, want := range map[natsgo.KeyValue][]string{rawInterests: {"ses_live"}, rawRoles: {"reviewer"}} {
		keys, err := bucket.Keys()
		if err != nil && !errors.Is(err, natsgo.ErrNoKeysFound) {
			t.Fatalf("list %s: %v", bucket.Bucket(), err)
		}
		if !slices.Equal(keys, want) {
			t.Fatalf("%s holds %d keys after the reapers, want only %v", bucket.Bucket(), len(keys), want)
		}
	}
	assertRoleHolder(t, registry, "reviewer", "ses_live")
}
