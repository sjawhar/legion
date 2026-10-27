package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// readTimeout bounds one read of Buckets, the bucket's open included: nats.go's KV calls take no
// context and wait out the JetStream MaxWait (10 s), so a read that has not answered by then is
// left to finish on its own.
const readTimeout = 2 * time.Second

// defaultSessionTTL is the session bucket's liveness window when its status does not say, the
// window session.OpenSessionRegistry creates the bucket with.
const defaultSessionTTL = 5 * time.Minute

// JetStream is the client Buckets reads through: its JetStream context on the current connection
// (bus.Client.JS).
type JetStream interface {
	JS() nats.JetStreamContext
}

// Buckets is a ClaimStore and a SessionLookup that read the role bucket and the session bucket
// themselves, one direct read per call, with no watcher and no cache: what Dispatch's outbox
// resolves a role-routed event's holder through. Its session read is the bucket read the
// listener's roleHolderSession falls back to (session.SessionRegistry.Refresh). Each bucket is
// opened on the client's connection at its first read, and again once the client has replaced that
// connection. A missing role bucket holds no claim; a missing session bucket is a read that fails,
// since it says nothing of whether a holder is there.
type Buckets struct {
	client JetStream

	mu       sync.Mutex
	js       nats.JetStreamContext
	roles    *bus.KeyValue
	sessions *bus.KeyValue
	ttl      time.Duration
}

// NewBuckets returns Buckets reading through client.
func NewBuckets(client JetStream) *Buckets {
	return &Buckets{client: client, ttl: defaultSessionTTL}
}

// bucket returns the handle of bucket on the client's current connection, opening it when it is
// not open on that connection yet. Opening the session bucket reads its TTL.
func (b *Buckets) bucket(name string) (bus.KeyValue, error) {
	js := b.client.JS()
	b.mu.Lock()
	defer b.mu.Unlock()
	if js != b.js {
		b.js, b.roles, b.sessions = js, nil, nil
	}
	handle := &b.roles
	if name == bus.SessionBucket {
		handle = &b.sessions
	}
	if *handle != nil {
		return **handle, nil
	}
	kv, err := bus.OpenKeyValue(js, name)
	if err != nil {
		return bus.KeyValue{}, err
	}
	if name == bus.SessionBucket {
		if status, err := kv.Status(); err == nil && status.TTL() > 0 {
			b.ttl = status.TTL()
		}
	}
	*handle = &kv
	return kv, nil
}

// within runs read, waiting at most readTimeout for it.
func within[T any](what string, read func() (T, error)) (T, error) {
	type answer struct {
		value T
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		value, err := read()
		answered <- answer{value: value, err: err}
	}()
	select {
	case got := <-answered:
		return got.value, got.err
	case <-time.After(readTimeout):
		var zero T
		return zero, fmt.Errorf("%s: no answer within %s", what, readTimeout)
	}
}

// claimEntry reads role's claim and its bucket entry: a zero claim and a nil entry when there is
// none.
func (b *Buckets) claimEntry(role string) (store.RoleClaim, nats.KeyValueEntry, error) {
	type read struct {
		claim store.RoleClaim
		entry nats.KeyValueEntry
	}
	got, err := within("read role "+role+"'s claim", func() (read, error) {
		kv, err := b.bucket(bus.RoleClaimBucket)
		if errors.Is(err, nats.ErrBucketNotFound) {
			return read{}, nil
		}
		if err != nil {
			return read{}, err
		}
		entry, err := kv.Get(role)
		if errors.Is(err, nats.ErrKeyNotFound) {
			return read{}, nil
		}
		if err != nil {
			return read{}, err
		}
		claim, err := decodeClaim(entry.Value())
		if err != nil {
			return read{}, err
		}
		return read{claim: claim, entry: entry}, nil
	})
	return got.claim, got.entry, err
}

// decodeClaim reads a stored claim as store.Registry reads one: a structured claim naming a
// holder, or a bare session id an earlier build stored.
func decodeClaim(value []byte) (store.RoleClaim, error) {
	var claim store.RoleClaim
	err := json.Unmarshal(value, &claim)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return store.RoleClaim{}, errors.New("role claim has no holder")
		}
		return claim, nil
	}
	if json.Valid(value) {
		return store.RoleClaim{}, fmt.Errorf("decode role claim: %w", err)
	}
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return store.RoleClaim{}, errors.New("decode legacy role claim: empty holder")
	}
	return store.RoleClaim{HolderSessionID: holder}, nil
}

// RoleClaim reads role's claim from the role bucket.
func (b *Buckets) RoleClaim(role string) (store.RoleClaim, error) {
	claim, _, err := b.claimEntry(role)
	return claim, err
}

// ReleaseExpiredRoleClaim deletes role's claim while sessionID still holds it, conditioned on the
// revision it read, as store.Registry.ReleaseExpiredRoleClaim does: Released when the delete
// landed, Missing when the claim was already gone, Superseded when another session holds it or a
// write beat the delete. It keeps no claim back: Dispatch restores no claims when it starts, so
// none of them has the grace a listener gives the claims it restored, and sessionTTL is not read.
func (b *Buckets) ReleaseExpiredRoleClaim(role, sessionID string, _ time.Duration) (store.ExpiredRoleClaimRelease, error) {
	claim, entry, err := b.claimEntry(role)
	if err != nil {
		return store.ExpiredRoleClaimRetained, err
	}
	if entry == nil {
		return store.ExpiredRoleClaimMissing, nil
	}
	if claim.HolderSessionID != sessionID {
		return store.ExpiredRoleClaimSuperseded, nil
	}
	_, err = within("release role "+role+"'s claim", func() (struct{}, error) {
		kv, err := b.bucket(bus.RoleClaimBucket)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, kv.Delete(role, nats.LastRevision(entry.Revision()))
	})
	switch {
	case err == nil:
		return store.ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return store.ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return store.ExpiredRoleClaimSuperseded, nil
	default:
		return store.ExpiredRoleClaimRetained, err
	}
}

// Holder reads sessionID's entry from the session bucket, as session.SessionRegistry.Refresh
// reads it: a key the bucket does not hold, one no read may name (bus.ErrRefused), a value that
// does not decode, an entry older than the bucket's TTL and an entry live refuses are all
// nats.ErrKeyNotFound.
func (b *Buckets) Holder(sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	entry, err := within("read session "+sessionID, func() (nats.KeyValueEntry, error) {
		kv, err := b.bucket(bus.SessionBucket)
		if err != nil {
			return nil, err
		}
		return kv.Get(sessionID)
	})
	if errors.Is(err, bus.ErrRefused) {
		return session.SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, err)
	}
	if err != nil {
		return session.SessionEntry{}, err
	}
	var holder session.SessionEntry
	if err := json.Unmarshal(entry.Value(), &holder); err != nil {
		return session.SessionEntry{}, fmt.Errorf("%w: session %s does not decode: %w", nats.ErrKeyNotFound, sessionID, err)
	}
	if ttl := b.TTL(); ttl > 0 && time.Since(entry.Created()) > ttl {
		return session.SessionEntry{}, fmt.Errorf("%w: session %s expired", nats.ErrKeyNotFound, sessionID)
	}
	if !live(holder) {
		return session.SessionEntry{}, nats.ErrKeyNotFound
	}
	return holder, nil
}

// TTL is the session bucket's liveness window, read when the bucket was opened.
func (b *Buckets) TTL() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ttl
}
