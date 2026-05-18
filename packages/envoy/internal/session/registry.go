package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nats-io/nats.go"
)

const SessionBucket = "envoy_sessions"

type SessionEntry struct {
	Port      int    `json:"port"`
	MachineID string `json:"machine_id"`
	Dir       string `json:"dir"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
}

type SessionRegistryOption func(*sessionRegistryOpts)

type sessionRegistryOpts struct {
	replicas int
	ttl      time.Duration
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
}

type SessionRegistry struct {
	kv nats.KeyValue
}

func OpenSessionRegistry(conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
	opts := sessionRegistryOpts{replicas: 1, ttl: 5 * time.Minute}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(SessionBucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:   SessionBucket,
			TTL:      opts.ttl,
			Replicas: opts.replicas,
			Storage:  nats.FileStorage,
		})
	}
	if err != nil {
		return nil, err
	}
	return &SessionRegistry{kv: kv}, nil
}

// Ping verifies the session KV bucket is reachable. Returns nil on success.
// See store.Registry.Ping for why this exists.
func (r *SessionRegistry) Ping() error {
	if r == nil {
		return ErrNoKV
	}
	_, err := r.kv.Status()
	return err
}

// ErrNoKV is returned when methods are called on a nil SessionRegistry.
var ErrNoKV = fmt.Errorf("session registry: KV unavailable")

func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	if r == nil {
		return ErrNoKV
	}
	entry.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = r.kv.Put(sessionID, buf)
	return err
}

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	if r == nil {
		return SessionEntry{}, ErrNoKV
	}
	entry, err := r.kv.Get(sessionID)
	if err != nil {
		return SessionEntry{}, err
	}
	var item SessionEntry
	if err := json.Unmarshal(entry.Value(), &item); err != nil {
		return SessionEntry{}, err
	}
	return item, nil
}

func (r *SessionRegistry) Delete(sessionID string) error {
	if r == nil {
		return ErrNoKV
	}
	return r.kv.Delete(sessionID)
}

// ListEntry is a single entry from List(), keyed by session ID.
type ListEntry struct {
	SessionID string `json:"session_id"`
	SessionEntry
}

// List returns all live sessions in the KV bucket. Because envoy_sessions
// has a TTL, only sessions that have been refreshed recently appear.
func (r *SessionRegistry) List() ([]ListEntry, error) {
	if r == nil {
		return nil, ErrNoKV
	}
	keys, err := r.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return []ListEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]ListEntry, 0, len(keys))
	for _, key := range keys {
		kve, err := r.kv.Get(key)
		if err != nil {
			continue // expired between Keys() and Get()
		}
		var item SessionEntry
		if err := json.Unmarshal(kve.Value(), &item); err != nil {
			continue
		}
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: item})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}
