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
// cannot, whose holders no build can reach any more. An entry past even a delete's bound, which only
// another writer could store (testnats.RawOnlyKey), is skipped, and the sweeps go on.
func TestARegistryOverKeysAnEarlierListenerStoredOpensAndReapsThem(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawInterests, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: RoleBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create role bucket: %v", err)
	}
	readableRole, unreadableRole := testnats.LegacyKeys(RoleBucket, "r")
	readableSession, unreadableSession := testnats.LegacyKeys(Bucket, "s")
	rawOnlyRole, rawOnlySession := testnats.RawOnlyKey(RoleBucket, "o"), testnats.RawOnlyKey(Bucket, "o")
	stale := time.Now().Add(-time.Hour).UnixMilli()
	for role, holder := range map[string]string{"reviewer": "ses_live", readableRole: readableSession, unreadableRole: unreadableSession, rawOnlyRole: "ses_gone"} {
		claim, err := json.Marshal(RoleClaim{HolderSessionID: holder, ClaimedAt: stale})
		if err != nil {
			t.Fatalf("encode claim: %v", err)
		}
		if _, err := rawRoles.Put(role, claim); err != nil {
			t.Fatalf("store the %d-byte role: %v", len(role), err)
		}
	}
	for _, sessionID := range []string{"ses_live", readableSession, unreadableSession, rawOnlySession} {
		putInterest(t, rawInterests, Interest{SessionID: sessionID, MachineID: "earlier", Topics: []string{"notifications.agent." + sessionID[:8]}, UpdatedAt: stale})
	}

	registry, err := Open(conn, WithReplicas(1))
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
	for bucket, want := range map[natsgo.KeyValue][]string{rawInterests: {"ses_live", rawOnlySession}, rawRoles: {"reviewer", rawOnlyRole}} {
		keys, err := bucket.Keys()
		if err != nil && !errors.Is(err, natsgo.ErrNoKeysFound) {
			t.Fatalf("list %s: %v", bucket.Bucket(), err)
		}
		slices.Sort(keys)
		slices.Sort(want)
		if !slices.Equal(keys, want) {
			t.Fatalf("%s holds %d keys after the reapers, want the live one and the one past a delete's bound", bucket.Bucket(), len(keys))
		}
	}
	assertRoleHolder(t, registry, "reviewer", "ses_live")
}

// A claim takes a role from its holder and then removes the role's topic from the old holder's
// interest. When the old holder is a session an earlier listener registered under an id this
// build cannot write (testnats.LegacyKeys), the claim still succeeds, as it did before the key
// check: the caller named nothing too long, and its claim is written. The old holder's interest
// keeps the topic until the interest reaper removes it.
func TestAClaimOfARoleAnEarlierListenersSessionHeldSucceeds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawInterests, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: RoleBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create role bucket: %v", err)
	}
	readable, unreadable := testnats.LegacyKeys(Bucket, "s")
	roles := map[string]string{"legacy-readable": readable, "legacy-unreadable": unreadable}
	for role, holder := range roles {
		claim, err := json.Marshal(RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode claim: %v", err)
		}
		if _, err := rawRoles.Put(role, claim); err != nil {
			t.Fatalf("store the claim of %s: %v", role, err)
		}
		// Subscribe gives every interest its session's own agent topic, so the role's topic is not
		// the last one and removing it rewrites the interest rather than deleting it.
		putInterest(t, rawInterests, Interest{SessionID: holder, MachineID: "earlier", Topics: []string{"notifications.agent." + holder, "notifications.role." + role}, UpdatedAt: time.Now().UnixMilli()})
	}

	registry, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the cache: %v", err)
	}
	for role, holder := range roles {
		item, err := registry.SetRole("ses_new", "example-host", role, false)
		if err != nil {
			t.Fatalf("claim %s from a %d-byte session: %v", role, len(holder), err)
		}
		if !slices.Contains(item.Topics, "notifications.role."+role) {
			t.Fatalf("the claimant's interest %v lacks the role's topic", item.Topics)
		}
		assertRoleHolder(t, registry, role, "ses_new")
	}
}

// nats.go refuses a key outside its key alphabet (`ses:bad`, `bad:role`) on every read, write and
// delete, however often it is sent, and the bus handle names that refusal bus.ErrInvalidKey, a
// bus.ErrRefused like a key past a subject's bound. No build writes such a key, but a direct bucket
// write stores one, since its subject is one NATS accepts. Such a key must not fail a sweep or a
// caller's request over every other key: the reapers skip it, and removing a session skips a claim
// stored under one.
func TestARegistryOverKeysOutsideTheNATSKeyAlphabetGoesOn(t *testing.T) {
	type stored struct {
		bucket, key string
		value       any
	}
	// open stores each entry, in order, by publishing it to the subject a put of its key goes to, as
	// a direct bucket write does (no KV call names a key outside the alphabet), and opens the
	// registry over them.
	open := func(t *testing.T, entries ...stored) (*Registry, natsgo.KeyValue, natsgo.KeyValue) {
		t.Helper()
		conn, cleanup := connectNATS(t)
		t.Cleanup(cleanup)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("jetstream: %v", err)
		}
		rawInterests, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: Bucket, Storage: natsgo.FileStorage})
		if err != nil {
			t.Fatalf("create interest bucket: %v", err)
		}
		rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: RoleBucket, Storage: natsgo.FileStorage})
		if err != nil {
			t.Fatalf("create role bucket: %v", err)
		}
		for _, entry := range entries {
			buf, err := json.Marshal(entry.value)
			if err != nil {
				t.Fatalf("encode %s in %s: %v", entry.key, entry.bucket, err)
			}
			if _, err := js.Publish("$KV."+entry.bucket+"."+entry.key, buf); err != nil {
				t.Fatalf("store %s in %s: %v", entry.key, entry.bucket, err)
			}
		}
		registry, err := Open(conn, WithReplicas(1))
		if err != nil {
			t.Fatalf("open over keys outside the key alphabet: %v", err)
		}
		t.Cleanup(registry.StopWatch)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := registry.WaitForCacheReady(ctx); err != nil {
			t.Fatalf("wait for the cache: %v", err)
		}
		return registry, rawInterests, rawRoles
	}
	stale := time.Now().Add(-time.Hour).UnixMilli()
	interest := func(sessionID string) stored {
		return stored{Bucket, sessionID, Interest{SessionID: sessionID, MachineID: "earlier", Topics: []string{"notifications.agent." + sessionID}, UpdatedAt: stale}}
	}
	claim := func(role, holder string) stored {
		return stored{RoleBucket, role, RoleClaim{HolderSessionID: holder, ClaimedAt: stale}}
	}
	keys := func(t *testing.T, bucket natsgo.KeyValue) []string {
		t.Helper()
		keys, err := bucket.Keys()
		if err != nil && !errors.Is(err, natsgo.ErrNoKeysFound) {
			t.Fatalf("list %s: %v", bucket.Bucket(), err)
		}
		slices.Sort(keys)
		return keys
	}
	isAlive := func(sessionID string) bool { return sessionID == "ses_live" }

	t.Run("the interest reaper skips an interest it cannot delete", func(t *testing.T) {
		registry, rawInterests, _ := open(t, interest("ses:bad"), interest("ses_live"), interest("ses_gone"))
		if reaped, err := registry.Reap(isAlive, 0); err != nil || reaped != 1 {
			t.Fatalf("reap = %d, %v; want ses_gone's interest, skipping ses:bad's", reaped, err)
		}
		if got, want := keys(t, rawInterests), []string{"ses:bad", "ses_live"}; !slices.Equal(got, want) {
			t.Fatalf("interests after the reaper = %v, want %v", got, want)
		}
	})

	// Removing every topic of a session releases each claim it holds, read from every key of the
	// role bucket; bad:role is stored first, so a sweep that stops there never reaches reviewer.
	t.Run("removing a session skips a claim it cannot read", func(t *testing.T) {
		registry, _, _ := open(t, claim("bad:role", "ses_leaving"), claim("reviewer", "ses_leaving"), interest("ses_leaving"))
		if err := registry.Remove("ses_leaving", nil); err != nil {
			t.Fatalf("remove a session while a claim is stored under bad:role: %v", err)
		}
		assertRoleHolder(t, registry, "reviewer", "")
		if _, err := registry.Get("ses_leaving"); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("ses_leaving's interest after its removal: %v, want it gone", err)
		}
	})

	t.Run("the role reaper skips a claim it cannot read or delete", func(t *testing.T) {
		registry, _, rawRoles := open(t, claim("bad:role", "ses_gone"), claim("reviewer", "ses_live"), claim("planner", "ses_gone"))
		if reaped, err := registry.ReapRoleClaims(isAlive, 0); err != nil || reaped != 1 {
			t.Fatalf("reap role claims = %d, %v; want planner's, skipping bad:role", reaped, err)
		}
		if got, want := keys(t, rawRoles), []string{"bad:role", "reviewer"}; !slices.Equal(got, want) {
			t.Fatalf("claims after the role reaper = %v, want %v", got, want)
		}
	})
}
