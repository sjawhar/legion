package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// newInterestCopy is the copy of the interest bucket r keeps, which holds every interest it can
// decode (decodeInterest) and keeps r's subject index in step with its rows (kvwatch.Rows).
func newInterestCopy(r *Registry) kvwatch.Copy[Interest] {
	return kvwatch.NewCopy(kvwatch.Rows[Interest]{Decode: decodeInterest, Stored: r.indexInterestLocked, Removed: r.unindexInterestLocked})
}

// indexInterestLocked and unindexInterestLocked keep the subject index in step with the copy: the
// index is what Match and MatchAll walk, so every interest the copy stores is set in it and every
// one it removes is taken out. The copy calls each under r.mu, held for writing.
func (r *Registry) indexInterestLocked(sessionID string, item Interest) {
	if r.index == nil {
		r.index = newSubjectIndex()
	}
	r.index.set(sessionID, item)
}

func (r *Registry) unindexInterestLocked(sessionID string, _ Interest) {
	if r.index != nil {
		r.index.remove(sessionID)
	}
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

// WaitForCacheReady blocks until both views answer, or until ctx is done. The interest watcher is
// waited for as ready (kvwatch.Watcher.WaitReady), having ended its first scan or failed to
// start, and the role claim watcher as servable (kvwatch.Watcher.WaitComplete): the interest view
// answers from whatever its copy holds, so a partial read still serves, while the role claim view
// refuses every read until a read of its bucket has completed (ErrRoleViewNotReady), so readiness
// alone would let a caller serve a view that answers nothing. After it returns nil, Match sees
// every existing subscription in the bucket and RolesHeldBy answers. A read whose scan timed out
// releases readiness without completing, and the watcher reads the bucket again itself after it,
// so this wait ends on that re-read rather than hanging. Callers bound ctx: a watcher that fails
// to start releases it too, and so does one that can complete no read at all, through its
// terminal error or its stop; the caller then serves the write-through path while /healthz
// exposes the unavailable registry. Its error names the cache that did not answer.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	if err := r.watcher.WaitReady(ctx); err != nil {
		return fmt.Errorf("interest cache: %w", err)
	}
	if err := r.roleWatcher.WaitComplete(ctx); err != nil {
		return fmt.Errorf("role claim cache: %w", err)
	}
	return nil
}
