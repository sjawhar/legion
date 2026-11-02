package roles_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// bucketsRig is a NATS server with a bus client Buckets reads through, and the registries a
// listener writes claims and sessions through.
func bucketsRig(t *testing.T) (*roles.Buckets, *store.Registry, *session.SessionRegistry, nats.JetStreamContext) {
	t.Helper()
	_, url := testnats.Start(t)
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, url)
	registry, err := store.Open(conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn, session.WithSessionTTL(2*time.Minute))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return roles.NewBuckets(client), registry, sessions, js
}

// Before any bucket exists a role has no claim; once one does, Buckets reads the claim a listener
// wrote, and a bare session id an earlier build stored.
func TestBucketsReadAClaimAsTheRegistryWroteIt(t *testing.T) {
	_, url := testnats.Start(t)
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if claim, err := roles.NewBuckets(client).RoleClaim(context.Background(), "reviewer"); err != nil || claim.HolderSessionID != "" {
		t.Fatalf("the claim with no role bucket is %+v, %v; want none", claim, err)
	}

	holders, registry, _, js := bucketsRig(t)
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "reviewer"); err != nil || claim.HolderSessionID != "ses_holder" {
		t.Fatalf("the claim is %+v, %v; want ses_holder's", claim, err)
	}
	kv, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	if _, err := kv.Put("legacy", []byte("ses_legacy")); err != nil {
		t.Fatalf("store a bare claim: %v", err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "legacy"); err != nil || claim.HolderSessionID != "ses_legacy" {
		t.Fatalf("the bare claim is %+v, %v; want ses_legacy's", claim, err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "unclaimed"); err != nil || claim.HolderSessionID != "" {
		t.Fatalf("the unclaimed role's claim is %+v, %v; want none", claim, err)
	}
}

// Buckets decodes a claim as the listener's registry decodes one (store.DecodeRoleClaim): a record
// that names no holder is store.ErrMalformedRoleClaim, not a claim. A read under a context that
// has ended answers that context's error, naming the read.
func TestBucketsDecodeAClaimAsTheRegistryDoesWithinTheirContext(t *testing.T) {
	holders, registry, _, js := bucketsRig(t)
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	kv, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	if _, err := kv.Put("nobody", []byte(`{"holder_session_id":""}`)); err != nil {
		t.Fatalf("store a claim naming no holder: %v", err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "nobody"); !errors.Is(err, store.ErrMalformedRoleClaim) {
		t.Fatalf("a claim naming no holder read %+v, %v; want store.ErrMalformedRoleClaim", claim, err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := holders.RoleClaim(ended, "reviewer"); !errors.Is(err, context.Canceled) || err.Error() != "read role reviewer's claim: context canceled" {
		t.Fatalf("a claim read under an ended context answered %v, want the context's error naming the read", err)
	}
	if _, err := holders.Holder(ended, "ses_holder", func(session.SessionEntry) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a session read under an ended context answered %v, want the context's error", err)
	}
}

// A release deletes the claim only while the session it names still holds it: released, then
// missing once it is gone, and superseded when another session holds it.
func TestBucketsReleaseAClaimOnlyWhileItsHolderHoldsIt(t *testing.T) {
	holders, registry, _, _ := bucketsRig(t)
	if _, err := registry.SetRole("ses_gone", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	if release, err := holders.ReleaseExpiredRoleClaim(context.Background(), "reviewer", "ses_other", time.Minute); err != nil || release != store.ExpiredRoleClaimSuperseded {
		t.Fatalf("the release for another session answered %v, %v; want superseded", release, err)
	}
	if release, err := holders.ReleaseExpiredRoleClaim(context.Background(), "reviewer", "ses_gone", time.Minute); err != nil || release != store.ExpiredRoleClaimReleased {
		t.Fatalf("the release answered %v, %v; want released", release, err)
	}
	if holder, err := registry.RoleHolder("reviewer"); err != nil || holder != "" {
		t.Fatalf("the claim after the release names %q, %v; want none", holder, err)
	}
	if release, err := holders.ReleaseExpiredRoleClaim(context.Background(), "reviewer", "ses_gone", time.Minute); err != nil || release != store.ExpiredRoleClaimMissing {
		t.Fatalf("the second release answered %v, %v; want missing", release, err)
	}
}

// Holder reads a registered session from the bucket; a session the bucket does not hold, one whose
// value does not decode, one no key may name and one live refuses are each gone
// (nats.ErrKeyNotFound). TTL is the bucket's.
func TestBucketsReadAHolderFromTheSessionBucket(t *testing.T) {
	holders, _, sessions, js := bucketsRig(t)
	if err := sessions.Put("ses_live", session.SessionEntry{SelfSubscribed: true, Title: "live"}); err != nil {
		t.Fatalf("register the session: %v", err)
	}
	entry, err := holders.Holder(context.Background(), "ses_live", func(session.SessionEntry) bool { return true })
	if err != nil || !entry.SelfSubscribed || entry.Title != "live" {
		t.Fatalf("the live holder read %+v, %v", entry, err)
	}
	if ttl := holders.TTL(); ttl != 2*time.Minute {
		t.Fatalf("TTL is %s, want the bucket's 2m", ttl)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := kv.Put("ses_garbled", []byte("{not json")); err != nil {
		t.Fatalf("store a garbled session: %v", err)
	}
	for _, tc := range []struct {
		name    string
		session string
		live    func(session.SessionEntry) bool
	}{
		{"not registered", "ses_missing", func(session.SessionEntry) bool { return true }},
		{"garbled", "ses_garbled", func(session.SessionEntry) bool { return true }},
		{"refused by live", "ses_live", func(session.SessionEntry) bool { return false }},
		{"a key nats.go refuses", "ses:bad", func(session.SessionEntry) bool { return true }},
	} {
		if _, err := holders.Holder(context.Background(), tc.session, tc.live); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("%s: Holder returned %v, want nats.ErrKeyNotFound", tc.name, err)
		}
	}
	stored, err := json.Marshal(session.SessionEntry{UpdatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := kv.Put("ses_fresh", stored); err != nil {
		t.Fatalf("store a session: %v", err)
	}
	if _, err := holders.Holder(context.Background(), "ses_fresh", func(session.SessionEntry) bool { return true }); err != nil {
		t.Fatalf("a session the bucket holds read %v", err)
	}
}
