package store

import (
	"encoding/json"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/routing"
)

const Bucket = "envoy_interests"
const RoleBucket = "envoy_roles"

type Registry struct {
	kv     nats.KeyValue
	roleKV nats.KeyValue
	mu     sync.RWMutex
	cache  map[string]Interest
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct{ replicas int }

// WithReplicas overrides the KV bucket replica count. Use 1 for single-node test NATS.
func WithReplicas(n int) OpenOption {
	return func(o *openOpts) { o.replicas = n }
}

func Open(conn *nats.Conn, options ...OpenOption) (*Registry, error) {
	opts := openOpts{replicas: 1}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := openBucket(js, Bucket, opts.replicas)
	if err != nil {
		return nil, err
	}
	roleKV, err := openBucket(js, RoleBucket, opts.replicas)
	if err != nil {
		return nil, err
	}
	r := &Registry{kv: kv, roleKV: roleKV, cache: map[string]Interest{}}
	// Skip eager load — watch() populates cache asynchronously via KV watcher.
	// The synchronous load() did N individual kv.Get() calls that block indefinitely
	// when the KV stream leader is on a remote node.
	go r.watch()
	return r, nil
}

func openBucket(js nats.JetStreamContext, bucket string, replicas int) (nats.KeyValue, error) {
	kv, err := js.KeyValue(bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Replicas: replicas, Storage: nats.FileStorage})
	}
	return kv, err
}

func (r *Registry) watch() {
	w, err := r.kv.WatchAll()
	if err != nil {
		log.Printf("registry watch failed: %v", err)
		return
	}
	for entry := range w.Updates() {
		if entry == nil {
			continue
		}
		r.mu.Lock()
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			delete(r.cache, entry.Key())
		} else {
			var item Interest
			if err := json.Unmarshal(entry.Value(), &item); err == nil {
				r.cache[entry.Key()] = item
			}
		}
		r.mu.Unlock()
	}
}

func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	cur, err := r.Get(item.SessionID)
	if err == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = time.Now().UnixMilli()
	item = Merge(item, topics)
	sort.Strings(item.Topics)
	buf, err := json.Marshal(item)
	if err != nil {
		return Interest{}, err
	}
	_, err = r.kv.Put(item.SessionID, buf)
	return item, err
}

func (r *Registry) Remove(sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry)
	if len(topics) == 0 {
		return r.kv.Delete(sessionID)
	}
	item, err := r.Get(sessionID)
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	if len(item.Topics) == 0 {
		return r.kv.Delete(sessionID)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = r.kv.Put(sessionID, buf)
	return err
}

func (r *Registry) SetRole(sessionID, machineID, role string) (Interest, error) {
	roleTopic := "notifications.role." + role
	entry, err := r.roleKV.Get(role)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return Interest{}, err
	}
	if err == nil {
		oldSessionID := string(entry.Value())
		if oldSessionID != "" && oldSessionID != sessionID {
			if removeErr := r.Remove(oldSessionID, []string{roleTopic}); removeErr != nil && !errors.Is(removeErr, nats.ErrKeyNotFound) {
				return Interest{}, removeErr
			}
		}
	}

	item, err := r.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}
	if _, err := r.roleKV.Put(role, []byte(sessionID)); err != nil {
		return Interest{}, err
	}
	return item, nil
}

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache.
func (r *Registry) Get(sessionID string) (Interest, error) {
	r.mu.RLock()
	item, ok := r.cache[sessionID]
	r.mu.RUnlock()
	if ok {
		return item, nil
	}
	entry, err := r.kv.Get(sessionID)
	if err != nil {
		return Interest{}, err
	}
	var fresh Interest
	err = json.Unmarshal(entry.Value(), &fresh)
	if err == nil {
		r.mu.Lock()
		r.cache[sessionID] = fresh
		r.mu.Unlock()
	}
	return fresh, err
}

func (r *Registry) Match(machineID string, topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache {
		if item.MachineID != machineID {
			continue
		}
		for _, pattern := range item.Topics {
			if routing.Match(pattern, topic) {
				out = append(out, item)
				break
			}
		}
	}
	return out
}

// List returns all cached interests sorted by SessionID for deterministic output.
func (r *Registry) List() []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, len(r.cache))
	for _, item := range r.cache {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func curValue(value string) string {
	return value
}
