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

type Registry struct {
	kv    nats.KeyValue
	mu    sync.RWMutex
	cache map[string]Interest
}

func Open(conn *nats.Conn) (*Registry, error) {
	js, err := conn.JetStream()
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: Bucket, Replicas: 3, Storage: nats.FileStorage})
	}
	if err != nil {
		return nil, err
	}
	r := &Registry{kv: kv, cache: map[string]Interest{}}
	if err := r.load(); err != nil {
		return nil, err
	}
	go r.watch()
	return r, nil
}

func (r *Registry) load() error {
	keys, err := r.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		entry, err := r.kv.Get(key)
		if err != nil {
			continue
		}
		var item Interest
		if err := json.Unmarshal(entry.Value(), &item); err != nil {
			continue
		}
		r.cache[key] = item
	}
	return nil
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

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func curValue(value string) string {
	return value
}
