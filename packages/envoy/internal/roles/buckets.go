package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// readTimeout bounds one read of Buckets, the bucket's open included, within the caller's own
// context: nats.go's KV calls take no context and wait out the JetStream MaxWait (10 s), so a read
// that has not answered by then is left to finish on its own.
const readTimeout = 2 * time.Second

// JetStream is the client Buckets reads through: its JetStream context on the current connection
// (bus.Client.JS) and that connection itself (bus.Client.Connection), which a bucket handle's
// watch side is opened on (bus.OpenKeyValue).
type JetStream interface {
	JS() nats.JetStreamContext
	Connection() *nats.Conn
}

// Buckets is a ClaimStore and a SessionLookup that read the role bucket and the session bucket
// themselves, one direct read per call, with no watcher and no cache: what Dispatch's outbox
// resolves a role-routed event's holder through. Each bucket is opened on the client's connection
// at its first read, and again once the client has replaced that connection. A missing role
// bucket holds no claim; a missing session bucket is a read that fails, since it says nothing of
// whether a holder is there.
//
// It writes nothing: a role's claim is the listener's to release, and only once its holder is
// gone (the package comment states the rule). The Windows it carries are what a holder's record
// is judged by, and they are the only measure of a holder here: how long the session bucket keeps
// a record is a storage window and no verdict on the session, so a record the bucket still holds
// past its maximum age is judged by its own last registration, as every other reader judges one.
type Buckets struct {
	Windows

	client JetStream

	mu       sync.Mutex
	js       nats.JetStreamContext
	roles    *bus.KeyValue
	sessions *bus.KeyValue
}

// NewBuckets returns Buckets reading through client and judging a holder's record by windows. It
// takes the windows rather than defaulting them, so no caller reaches the package's compiled
// windows by forgetting to say which it means: a process judging a holder on windows its
// deployment did not set calls a holder the listener still calls unresponsive gone, and marks
// its event published with nothing sent. A caller that does mean the compiled windows passes a
// zero Windows, whose own zero fields are session.LiveWindow and session.GoneHorizon.
func NewBuckets(client JetStream, windows Windows) *Buckets {
	return &Buckets{Windows: windows, client: client}
}

// bucket returns the handle of bucket on the client's current connection, opening it when it is
// not open on that connection yet.
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
	kv, err := bus.OpenKeyValue(b.client.Connection(), js, name)
	if err != nil {
		return bus.KeyValue{}, err
	}
	*handle = &kv
	return kv, nil
}

// within runs read, waiting for it no longer than ctx and at most readTimeout (bus.Bounded). A
// read that did not answer in time is named by what.
func within[T any](ctx context.Context, what string, read func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	value, answered, err := bus.Bounded(ctx, read)
	if !answered {
		return value, fmt.Errorf("%s: %w", what, err)
	}
	return value, err
}

// RoleClaim reads role's claim from the role bucket within ctx, decoded as store.Registry decodes
// one (store.DecodeRoleClaim): a zero claim without an error when there is no role bucket and
// when the bucket holds no claim for the role.
func (b *Buckets) RoleClaim(ctx context.Context, role string) (store.RoleClaim, error) {
	return within(ctx, "read role "+role+"'s claim", func() (store.RoleClaim, error) {
		kv, err := b.bucket(bus.RoleClaimBucket)
		if errors.Is(err, nats.ErrBucketNotFound) {
			return store.RoleClaim{}, nil
		}
		if err != nil {
			return store.RoleClaim{}, err
		}
		entry, err := kv.Get(role)
		if errors.Is(err, nats.ErrKeyNotFound) {
			return store.RoleClaim{}, nil
		}
		if err != nil {
			return store.RoleClaim{}, err
		}
		return store.DecodeRoleClaim(entry.Value())
	})
}

// Holder reads sessionID's record from the session bucket, whatever state that record is in:
// judging it is Windows.EntryState's, so a record the bucket still holds past its maximum age is
// answered here and judged by its own last registration. A key the bucket does not hold, one no
// read may name (bus.ErrRefused) and a value that does not decode are nats.ErrKeyNotFound: no
// record stands for the session. Any other error is a read that did not answer.
func (b *Buckets) Holder(ctx context.Context, sessionID string) (session.SessionEntry, error) {
	entry, err := within(ctx, "read session "+sessionID, func() (nats.KeyValueEntry, error) {
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
	return holder, nil
}

// LastSeen is 0 for every session: Dispatch reads the session bucket and keeps no memory of a
// record that has gone, so a holder whose record the bucket no longer holds is one nothing here
// can date (Resolution.LastSeen says what a caller makes of that).
func (b *Buckets) LastSeen(string) int64 { return 0 }
