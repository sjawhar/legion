package session

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/nats-io/nats.go"
)

const SessionBucket = "envoy_sessions"

type SessionEntry struct {
	Port      int    `json:"port"`
	MachineID string `json:"machine_id"`
	Dir       string `json:"dir"`
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
	opts := sessionRegistryOpts{replicas: 3, ttl: 5 * time.Minute}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream()
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

func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	entry.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = r.kv.Put(sessionID, buf)
	return err
}

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
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
	return r.kv.Delete(sessionID)
}
