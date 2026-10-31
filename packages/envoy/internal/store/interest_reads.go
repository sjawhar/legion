package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/routing"
)

// ErrMalformedInterest is an interest record the bucket holds and this build cannot decode. The bus
// answered it, so it is not a read that failed, and reading it again returns the same record; the
// error wrapping it carries the decode failure.
var ErrMalformedInterest = errors.New("malformed interest record")

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A cache hit answers in the
// caller's goroutine; only the bucket read on a miss waits on ctx (bus.Bounded). The cache holds
// only records it decoded (interestCache.Apply evicts the rest), so only the bucket read can meet
// a record this build cannot decode: that is ErrMalformedInterest, logged at WARN with the session
// it belongs to. A read of a bucket since recreated, or one the revision fence drops, is answered
// but not cached: the new bucket's read delivers what it holds.
func (r *Registry) Get(ctx context.Context, sessionID string) (Interest, error) {
	if item, ok := r.Cached(sessionID); ok {
		return item, nil
	}
	kv, epoch := r.watcher.Handle()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return Interest{}, fmt.Errorf("read %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return Interest{}, err
	}
	var fresh Interest
	if err := json.Unmarshal(entry.Value(), &fresh); err != nil {
		r.logger().Warn("registry read an interest record it cannot decode",
			slog.String("session_id", sessionID),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return Interest{}, fmt.Errorf("%w: %w", ErrMalformedInterest, err)
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, fresh, entry.Revision())
	r.mu.Unlock()
	return fresh, nil
}

// Cached returns sessionID's interest from the cache alone, with no bucket read on a miss, for a
// caller that must make no per-key read, such as one joining a whole list with its interests.
func (r *Registry) Cached(sessionID string) (Interest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache.Get(sessionID)
}

// Match returns the cached interests registered on machineID whose topics match topic: a
// listener delivers live only to the sessions of its own machine.
func (r *Registry) Match(machineID string, topic string) []Interest {
	return r.match(topic, func(item Interest) bool { return item.MachineID == machineID })
}

// MatchAll returns every cached interest whose topics match topic, on any machine, sorted by
// SessionID for deterministic output: a sender that must confirm delivery to each subscribed
// session needs them all, wherever each session registered.
func (r *Registry) MatchAll(topic string) []Interest {
	out := r.match(topic, func(Interest) bool { return true })
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// match returns the cached interests keep accepts that have a topic pattern matching topic.
func (r *Registry) match(topic string, keep func(Interest) bool) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache.All() {
		if !keep(item) {
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
