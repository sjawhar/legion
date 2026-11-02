// The session registry's last-seen times: when each session last wrote its registration, which
// LastSeen answers for a session that has lapsed, from the listener state bucket's shared record
// (WithLastSeenBucket) or from this process's own (SessionRegistry.seenLocal).

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dedupe"
)

// WithLastSeenBucket sets the listener state bucket (bus.ListenerStateBucket), where every
// registration write also records the session's last-seen time under `seen.<session token>`
// (lastSeenKey), the one record every listener pod answers LastSeen for a lapsed session from, a
// pod that started after the session lapsed included. Without it the registry keeps last-seen
// times in process (SessionRegistry.seenLocal): a listener whose NATS user lacks the grant for
// that bucket runs so, and a restart forgets them.
func WithLastSeenBucket(kv bus.KeyValue) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.seen = kv }
}

// WithLastSeenRetention sets how long after a session leaves the cache LastSeen still answers its
// final heartbeat; the id's revision fence is kept as long. The default, and any value shorter
// than the bucket's TTL, is one TTL: the fence must outlive the id's leaving by at least one TTL
// for dropping it to stay safe (expireDue). The listener keeps it for as long as the interest
// reaper can still hold a dead session's interest, so its interests API can report the session's
// last heartbeat for every interest row it still lists. With the listener state bucket
// (WithLastSeenBucket) every pod answers out of the session's `seen.` record for this whole
// window, and the bucket keeps a record longer than the listener ever sets this retention
// (listenerStateMaxAge, cmd/listener/lease.go).
func WithLastSeenRetention(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.lastSeenRetention = d }
}

// retention is how long after a session leaves the cache its last-seen record and revision fence
// are kept: WithLastSeenRetention's value, and never less than one TTL, so a registry built from a
// struct literal keeps them one TTL. It means nothing when the TTL is not positive: then nothing
// expires and no record is kept.
func (r *SessionRegistry) retention() time.Duration {
	return max(r.lastSeenRetention, r.ttl)
}

// LastSeen is sessionID's last registration write, in Unix milliseconds, or 0 when there is none to
// report. While the session is live it is the live registration's UpdatedAt. Once the session lapsed
// (its registration expired or was deleted) it is the time its last registration write recorded
// (recordSeen), for the registry's retention (one TTL unless WithLastSeenRetention set longer)
// after that registration's expiry, so callers can report why an otherwise durable reference
// stopped resolving.
//
// With the listener state bucket (WithLastSeenBucket) that time is the session's `seen.` record
// alone, the server's timestamp of the write, which every pod reads alike, a pod started after the
// session lapsed included: every pod of the fleet answers one time for one session through the
// whole window, and a record the bucket has not got (it was never written, its MaxAge removed it,
// or it could not be read) is no answer on any of them. The bucket keeps a record for
// listenerStateMaxAge after its last write, which is sized past this window, so the record
// outlives the answer rather than ending it. Without the bucket the time is this process's own
// record (seenLocal), the UpdatedAt of every registration this process wrote or its watcher saw,
// which a restart forgets. The window is measured on this process's clock against the server's
// timestamp; a skew of seconds against a TTL of minutes moves its end by that skew.
//
// Its read of the bucket waits no longer than ctx (bus.Bounded), so a caller answering a request
// keeps its own budget: a bucket that does not answer costs that budget once, and every further
// row of the same request reads an ended ctx and answers at once. A read ctx ended is a WARN and
// no time, as an unreadable record is.
//
// It takes only the shared lock and changes nothing: an expired entry the expiry timer has not
// removed yet answers the lapsed session's record, which is what its removal leaves in place, so
// the answer is the same before and after the removal.
func (r *SessionRegistry) LastSeen(ctx context.Context, sessionID string) int64 {
	now := time.Now()
	r.mu.RLock()
	cs, cached := r.cache[sessionID]
	at := r.seenLocal[sessionID]
	r.mu.RUnlock()
	if cached && !cs.expired(now) {
		return cs.entry.UpdatedAt
	}
	if r.ttl <= 0 {
		return 0
	}
	if r.seen.KeyValue != nil {
		stored, err := r.readSeen(ctx, sessionID)
		if err != nil {
			r.logger().Warn("session last-seen unavailable: its record in the listener state bucket could not be read",
				slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
		}
		at = stored
	}
	if at <= 0 || !now.Before(r.seenUntil(at)) {
		return 0
	}
	return at
}

// seenUntil is when a lapsed session's last-seen time at (Unix milliseconds) stops being reported:
// the retention after the expiry of the registration written at at.
func (r *SessionRegistry) seenUntil(at int64) time.Time {
	return time.UnixMilli(at).Add(r.ttl + r.retention())
}

// lastSeenUntilLocked is when sessionID's per-process last-seen record and revision fence may be
// dropped after its entry left the cache at left: the retention past that, and never before the
// record's own end (seenUntil), so nothing LastSeen still reports is dropped. Callers must hold
// r.mu.
func (r *SessionRegistry) lastSeenUntilLocked(sessionID string, left time.Time) time.Time {
	until := left.Add(r.retention())
	if at, ok := r.seenLocal[sessionID]; ok && r.seenUntil(at).After(until) {
		return r.seenUntil(at)
	}
	return until
}

// seenRecord is the value of a session's `seen.` record in the listener state bucket: at is the
// server's timestamp, in Unix milliseconds, of the session's last registration write.
type seenRecord struct {
	At int64 `json:"at"`
}

// lastSeenKey is sessionID's last-seen key in the listener state bucket, `seen.<session token>`
// (bus.LastSeenPrefix), with the session token every key of that bucket names a session by
// (dedupe.SessionToken).
func lastSeenKey(sessionID string) string {
	return bus.LastSeenPrefix + dedupe.SessionToken(sessionID)
}

// recordSeen records the registration write at revision written as sessionID's last-seen time in
// the listener state bucket, when the registry has it: it reads the write back from the session
// bucket for the server's timestamp, the clock the registration's expiry runs on, and puts it under
// lastSeenKey, each bucket call waiting no longer than ctx (bus.Bounded). A failure of either logs a
// WARN and leaves the registration, which is written, as it is: the record keeps the previous
// write's time. The read is by key, a direct get on the key's own subject, which the listener's
// grant allows; a get by revision is a direct get on the stream's bare subject, which it does not.
// A read that finds another revision, or no record, means a newer write replaced this one, and
// that write records its own.
func (r *SessionRegistry) recordSeen(ctx context.Context, kv bus.KeyValue, sessionID string, written uint64) {
	if r.seen.KeyValue == nil {
		return
	}
	stored, _, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if errors.Is(err, nats.ErrKeyNotFound) || err == nil && stored.Revision() != written {
		return
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded: the registration write could not be read back",
			slog.String("session_id", sessionID), slog.Uint64("revision", written), slog.String("error", err.Error()))
		return
	}
	value, err := json.Marshal(seenRecord{At: stored.Created().UnixMilli()})
	if err == nil {
		_, _, err = bus.Bounded(ctx, func() (uint64, error) { return r.seen.Put(lastSeenKey(sessionID), value) })
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded in the listener state bucket",
			slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
	}
}

// readSeen reads sessionID's last-seen time, in Unix milliseconds, from its record in the listener
// state bucket: 0 when there is none. The read waits no longer than ctx (bus.Bounded), so a route
// that answers a caller is held no longer than its own budget by a bucket that does not answer;
// a read ctx ended is an error naming the key, which the caller reports as no time.
func (r *SessionRegistry) readSeen(ctx context.Context, sessionID string) (int64, error) {
	key := lastSeenKey(sessionID)
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return r.seen.Get(key) })
	if !answered {
		return 0, fmt.Errorf("read %s from the listener state bucket: %w", key, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var record seenRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, fmt.Errorf("decode %s: %w", entry.Key(), err)
	}
	return record.At, nil
}
