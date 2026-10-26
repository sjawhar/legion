package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// deleteInterest deletes sessionID's interest at the revision it reads (bus.KeyValue.DeleteAtRead).
func (r *Registry) deleteInterest(sessionID string) error {
	// The fence read before the delete is the bucket's this handle reaches only when the read under
	// way began in its epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	read, err := kv.DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	revision = max(revision, read)

	ctx, cancel := context.WithTimeout(context.Background(), bus.RequestTimeout)
	entries, err := kv.ReadHistory(ctx, sessionID)
	cancel()
	r.mu.Lock()
	if err != nil || len(entries) == 0 {
		// KeyValue.Delete does not return its revision. A successful delete must
		// still fence off any watcher update from the preceding revision while the
		// delete marker remains pending.
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		r.mu.Unlock()
		return nil
	}
	latest := kvwatch.JetStreamEntry(entries[len(entries)-1])
	_, malformed := r.cache.Apply(r.watcher, epoch, latest)
	r.mu.Unlock()
	if malformed != nil {
		r.logger().Warn("registry cache evicted malformed value",
			slog.String("key", latest.Key),
			slog.Uint64("revision", latest.Revision),
			slog.String("error", malformed.Error()),
		)
	}
	return nil
}

func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	cur, getErr := r.Get(item.SessionID)
	merged, err := mergeForUpsert(cur, getErr, item, topics, r.now().UnixMilli())
	if err != nil {
		return Interest{}, err
	}
	buf, err := json.Marshal(merged)
	if err != nil {
		return Interest{}, err
	}
	kv, epoch := r.watcher.Handle()
	revision, err := kv.Put(merged.SessionID, buf)
	if err != nil {
		return Interest{}, err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, merged.SessionID, merged, revision)
	r.mu.Unlock()
	return merged, nil
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics.
func mergeForUpsert(cur Interest, getErr error, item Interest, topics []string, now int64) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) {
		return Interest{}, getErr
	}
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = now
	item = Merge(item, topics)
	sort.Strings(item.Topics)
	return item, nil
}

func (r *Registry) Remove(sessionID string, topics []string) error {
	if len(topics) == 0 {
		item, err := r.Get(sessionID)
		if err == nil {
			if err := r.releaseRoleClaims(sessionID, item.Topics); err != nil {
				return err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) {
			return err
		}
		if err := r.releaseAllRoleClaims(sessionID); err != nil {
			return err
		}
	} else if err := r.releaseRoleClaims(sessionID, topics); err != nil {
		return err
	}
	return r.removeInterestTopics(sessionID, topics)
}

func (r *Registry) removeInterestTopics(sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(sessionID)
	}
	item, err := r.Get(sessionID)
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	if len(item.Topics) == 0 {
		return r.deleteInterest(sessionID)
	}
	item.UpdatedAt = r.now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	kv, epoch := r.watcher.Handle()
	revision, err := kv.Put(sessionID, buf)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, item, revision)
	r.mu.Unlock()
	return nil
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
