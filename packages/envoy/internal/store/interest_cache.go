package store

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// newInterestCopy is an empty copy of the interest bucket, which keeps every interest it can
// decode (decodeInterest).
func newInterestCopy() kvwatch.Copy[Interest] {
	return kvwatch.NewCopy(kvwatch.Rows[Interest]{Decode: decodeInterest})
}

// decodeInterest is the interest a put record of the bucket, delivered or read, leaves in the
// cache.
func decodeInterest(entry kvwatch.Entry) (Interest, bool, error) {
	var item Interest
	if err := json.Unmarshal(entry.Value, &item); err != nil {
		return Interest{}, false, err
	}
	return item, true, nil
}

// interestCache is the registry as the cache its watcher feeds (kvwatch.Cache). The hooks are its
// own type's, not the registry's methods, so nothing but the watcher calls them.
type interestCache struct{ r *Registry }

// BeginRead starts a read of the interest bucket (kvwatch.Copy.BeginRead). Every interest stays.
func (c interestCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could not
// decode, which evicted the interest. The revision fence keeps a delayed local write-through and a
// watcher update from undoing each other.
func (c interestCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	malformed := r.cache.Delivered(entry)
	r.mu.Unlock()
	if malformed != nil {
		r.logger().Warn("registry watcher evicted malformed value",
			slog.String("key", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every interest and fence entry the read found no evidence of: the bucket
// no longer holds that session's interest.
func (c interestCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.CompleteRead()
}

// WaitForCacheReady blocks until the watcher has delivered every existing interest, or until ctx
// is done. After it returns nil, Match sees every existing subscription in the bucket. Callers
// bound ctx: a watcher that fails to start releases it too, and the caller then serves the
// write-through path while /healthz exposes the unavailable registry.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}
