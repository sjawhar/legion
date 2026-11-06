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
	return roles.NewBuckets(client, roles.Windows{}), registry, sessions, js
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
	if claim, err := roles.NewBuckets(client, roles.Windows{}).RoleClaim(context.Background(), "reviewer"); err != nil || claim.HolderSessionID != "" {
		t.Fatalf("the claim with no role bucket is %+v, %v; want none", claim, err)
	}

	holders, registry, _, js := bucketsRig(t)
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim the role: %v", err)
	}
	kv, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	stored, err := kv.Get("reviewer")
	if err != nil {
		t.Fatalf("read the stored claim: %v", err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "reviewer"); err != nil || claim.HolderSessionID != "ses_holder" || claim.Revision != stored.Revision() {
		t.Fatalf("the claim is %+v, %v; want ses_holder's at revision %d", claim, err, stored.Revision())
	}
	legacy, err := kv.Put("legacy", []byte("ses_legacy"))
	if err != nil {
		t.Fatalf("store a bare claim: %v", err)
	}
	if claim, err := holders.RoleClaim(context.Background(), "legacy"); err != nil || claim.HolderSessionID != "ses_legacy" || claim.Revision != legacy {
		t.Fatalf("the bare claim is %+v, %v; want ses_legacy's at revision %d", claim, err, legacy)
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
	if _, err := holders.Holder(ended, "ses_holder"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a session read under an ended context answered %v, want the context's error", err)
	}
}

// A holder is judged by its own last registration and by the windows Buckets was given, never by
// how long the session bucket keeps a record: a record the bucket still holds is answered
// whatever its age, and the state it reads follows the windows alone. Before this a record older
// than the bucket's TTL read as a session that is not there, and the resolution released its
// role claim on that word (dispatch://INFRA-392).
func TestBucketsJudgeAHolderByItsRegistrationAndNotTheBucketsAge(t *testing.T) {
	holders, _, _, js := bucketsRig(t)
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	// Written now, so the bucket holds it; last registered long before, as a session that
	// stopped heartbeating leaves its record.
	stalled := session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().Add(-10 * time.Minute).UnixMilli()}
	stored, err := json.Marshal(stalled)
	if err != nil {
		t.Fatalf("encode the stalled record: %v", err)
	}
	if _, err := kv.Put("ses_stalled", stored); err != nil {
		t.Fatalf("store the stalled record: %v", err)
	}
	entry, err := holders.Holder(context.Background(), "ses_stalled")
	if err != nil || entry.UpdatedAt != stalled.UpdatedAt {
		t.Fatalf("the stalled holder read %+v, %v; want the record the bucket holds", entry, err)
	}
	if state := holders.EntryState(entry); state != session.Unresponsive {
		t.Fatalf("a holder that last registered 10m ago reads %s, want unresponsive on the package's windows", state)
	}
	if seen := holders.LastSeen("ses_stalled"); seen != 0 {
		t.Fatalf("LastSeen is %d, want 0: Dispatch remembers no session whose record has gone", seen)
	}
	// The windows alone decide the state, so these read the same record through Buckets built
	// with windows of their own and no bucket to read.
	for _, tc := range []struct {
		name    string
		windows roles.Windows
		want    session.State
	}{
		{"windows the registration is inside", roles.Windows{Live: time.Hour, Horizon: 2 * time.Hour}, session.Live},
		{"a gone horizon the registration is past", roles.Windows{Live: time.Minute, Horizon: 2 * time.Minute}, session.Gone},
	} {
		if state := roles.NewBuckets(nil, tc.windows).EntryState(entry); state != tc.want {
			t.Fatalf("%s: the holder reads %s, want %s", tc.name, state, tc.want)
		}
	}
}

// Holder reads a registered session from the bucket; a session the bucket does not hold, one
// whose value does not decode and one no key may name are each gone (nats.ErrKeyNotFound).
func TestBucketsReadAHolderFromTheSessionBucket(t *testing.T) {
	holders, _, sessions, js := bucketsRig(t)
	if err := sessions.Put("ses_live", session.SessionEntry{SelfSubscribed: true, Title: "live"}); err != nil {
		t.Fatalf("register the session: %v", err)
	}
	entry, err := holders.Holder(context.Background(), "ses_live")
	if err != nil || !entry.SelfSubscribed || entry.Title != "live" {
		t.Fatalf("the live holder read %+v, %v", entry, err)
	}
	if state := holders.EntryState(entry); state != session.Live {
		t.Fatalf("the holder that just registered reads %s, want live", state)
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
	}{
		{"not registered", "ses_missing"},
		{"garbled", "ses_garbled"},
		{"a key nats.go refuses", "ses:bad"},
	} {
		if _, err := holders.Holder(context.Background(), tc.session); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("%s: Holder returned %v, want nats.ErrKeyNotFound", tc.name, err)
		}
	}
}
