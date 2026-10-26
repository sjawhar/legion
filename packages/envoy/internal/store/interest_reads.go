package store

import (
	"encoding/json"
	"sort"

	"github.com/sjawhar/envoy/internal/routing"
)

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A read of a bucket since recreated is
// answered but not cached: the new bucket's read delivers what it holds.
func (r *Registry) Get(sessionID string) (Interest, error) {
	if item, ok := r.Cached(sessionID); ok {
		return item, nil
	}
	kv, epoch := r.watcher.Handle()
	entry, err := kv.Get(sessionID)
	if err != nil {
		return Interest{}, err
	}
	var fresh Interest
	err = json.Unmarshal(entry.Value(), &fresh)
	if err == nil {
		r.mu.Lock()
		r.cache.Put(r.watcher, epoch, sessionID, fresh, entry.Revision())
		r.mu.Unlock()
	}
	return fresh, err
}

// Cached returns sessionID's interest from the cache alone, with no bucket read on a miss, for a
// caller that must make no per-key read, such as one joining a whole list with its interests.
func (r *Registry) Cached(sessionID string) (Interest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache.Get(sessionID)
}

func (r *Registry) Match(machineID string, topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache.All() {
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

// List returns all cached interests sorted by SessionID for deterministic output, or
// kvwatch.ErrRefilling while the copy cannot be served (Servable): before its first complete read
// of the bucket, and once it has not followed the bucket for longer than the stale bound.
func (r *Registry) List() ([]Interest, error) {
	if err := r.Servable(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, r.cache.Len())
	for _, item := range r.cache.All() {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}
