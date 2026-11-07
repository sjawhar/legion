package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// deleteInterest deletes sessionID's interest at the revision it reads (bus.KeyValue.DeleteAtRead),
// each bucket call waiting no longer than ctx (bus.Bounded). A delete ctx abandoned may still land
// after this answer, and that is safe: it deletes only the revision it read, so a write that landed
// since is kept, and a key it found absent it deletes at none, as DeleteAtRead always has.
func (r *Registry) deleteInterest(ctx context.Context, sessionID string) error {
	// The fence read before the delete is the bucket's this handle reaches only when the read under
	// way began in its epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.DeleteAtRead(sessionID) })
	if !answered {
		return fmt.Errorf("delete %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.reconcileDeleted(ctx, kv, epoch, sessionID, max(revision, read))
	return nil
}

// deleteInterestAtRevision deletes sessionID's interest only while revision is its latest, each
// bucket call waiting no longer than ctx (bus.Bounded); a write since then makes JetStream refuse
// the delete with wrong last sequence (10071, which nats.go names nats.ErrKeyExists), which the
// caller reads as the interest kept. Revision 0 names no revision, which a conditional delete
// cannot express, so that interest is deleted at the revision it reads (deleteInterest). The cache
// follows the bucket afterwards as it does after deleteInterest. A delete ctx abandoned may still
// land after this answer, and that is safe: it deletes only the revision it checked.
func (r *Registry) deleteInterestAtRevision(ctx context.Context, sessionID string, revision uint64) error {
	if revision == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	kv, epoch := r.watcher.Handle()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, kv.Delete(sessionID, nats.LastRevision(revision))
	})
	if !answered {
		return fmt.Errorf("delete %s's interest record at revision %d: %w", sessionID, revision, err)
	}
	if err != nil {
		return err
	}
	r.reconcileDeleted(ctx, kv, epoch, sessionID, revision)
	return nil
}

// reconcileDeleted brings sessionID's cache entry in line with the bucket after a delete through
// kv, a handle of epoch, that succeeded past revision: it applies the key's latest history entry,
// which is the delete marker unless a write has landed since, or, when the history cannot be read
// within ctx, evicts the entry behind a fence just past revision.
func (r *Registry) reconcileDeleted(ctx context.Context, kv bus.KeyValue, epoch uint64, sessionID string, revision uint64) {
	// A history that does not answer falls back to the fence below: the delete itself answered.
	// ReadHistory ends with ctx; bus.RequestTimeout bounds it for a caller that answers no request
	// and so passes a context that never ends.
	historyCtx, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
	entries, err := kv.ReadHistory(historyCtx, sessionID)
	cancel()
	r.mu.Lock()
	if err != nil || len(entries) == 0 {
		// KeyValue.Delete does not return its revision. A successful delete must
		// still fence off any watcher update from the preceding revision while the
		// delete marker remains pending.
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		r.mu.Unlock()
		return
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
}

// Upsert is UpsertWithin with no deadline of its own, for a caller that answers no request.
func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	return r.UpsertWithin(context.Background(), item, topics)
}

// UpsertWithin adds topics to item.SessionID's interest, merged onto the stored one
// (mergeForUpsert), stamped with the registry's clock and written by compare-and-set
// (writeInterest), each bucket call waiting no longer than ctx (bus.Bounded): an HTTP route passes
// its request's own context. It returns the record as it now stands. A record this build cannot
// decode (ErrMalformedInterest) is replaced by item and topics alone, and the registry logs that at
// WARN naming the session. A write ctx abandoned may still land after this answer. That is safe: it
// is a compare-and-set at the revision it read, so it replaces only that revision and never a write
// that landed since, and it writes the record the caller's retry merges again.
//
// A heartbeat re-sends the same topics every 90 seconds, so UpsertWithin returns the current row
// without writing when the encoding of merged equals that of the row it holds, apart from
// UpdatedAt, and the row is younger than InterestRefreshAge. An unchanged row is thus rewritten
// once per InterestRefreshAge, its UpdatedAt lags the session's last heartbeat by less than that,
// and Reap adds InterestRefreshAge to its grace window (InterestReapAllowance), so that lag never
// shortens a paused session's grace. A changed row is written, and so is a record it cannot
// decode; any other failed read refuses the write (mergeForUpsert). The session row
// (session.SessionRegistry.Put) is written on every heartbeat: its bucket's MaxAge is the liveness
// clock, and NATS connect notices ($SYS) are not available to replace it.
func (r *Registry) UpsertWithin(ctx context.Context, item Interest, topics []string) (Interest, error) {
	var malformed bool
	merged, revision, err := r.writeInterest(ctx, item.SessionID, func(cur Interest, curErr error) (Interest, writeAction, error) {
		malformed = errors.Is(curErr, ErrMalformedInterest)
		now := r.now().UnixMilli()
		merged, err := mergeForUpsert(cur, curErr, item, topics, now)
		if err != nil {
			return Interest{}, writeStore, err
		}
		if curErr == nil && now-cur.UpdatedAt < InterestRefreshAge.Milliseconds() {
			same, err := storesTheSameInterest(cur, merged)
			if err != nil {
				return Interest{}, writeStore, err
			}
			if same {
				return cur, writeSkip, nil
			}
		}
		return merged, writeStore, nil
	})
	if err != nil {
		return Interest{}, err
	}
	if malformed {
		r.logger().Warn("registry replaced an interest record it cannot decode",
			slog.String("session_id", merged.SessionID),
			slog.Uint64("revision", revision),
			slog.Any("topics", merged.Topics),
		)
	}
	return merged, nil
}

// storesTheSameInterest reports whether writing next would store the row cur already holds. What
// the skip asks is whether the write would change the bucket, so it compares the encoding of next
// with that of cur rather than the fields one by one: a field added to Interest, a key only a newer
// build knows (Interest.unknown), and the difference between a null and an empty topic list are
// then covered by the encoding itself. cur is stamped with next's UpdatedAt, the one field a write
// of an unchanged row would move.
func storesTheSameInterest(cur, next Interest) (bool, error) {
	cur.UpdatedAt = next.UpdatedAt
	stored, err := encodeInterest(cur)
	if err != nil {
		return false, err
	}
	buf, err := encodeInterest(next)
	if err != nil {
		return false, err
	}
	return bytes.Equal(buf, stored), nil
}

// writeAction is what writeInterest does with what its change returned: store it, delete the
// stored interest, or leave the bucket as it is.
type writeAction uint8

const (
	writeStore writeAction = iota
	writeRemove
	writeSkip
)

// writeInterest writes sessionID's interest by compare-and-set on its KV revision, so a write another
// listener made between this one's read and its write is never overwritten. change computes the
// interest to write from the stored one and the error reading it (nats.ErrKeyNotFound when there is
// none, ErrMalformedInterest for a record this build cannot decode), or asks for the stored one to be
// deleted (writeRemove) or left alone (writeSkip, for a write that would store the row the bucket
// already holds). The first attempt changes the cached interest. A cache miss, or a write the bucket
// refuses because the revision moved, reads the bucket entry and changes that: an Update or a delete
// at its revision, or a Create when there is none. After writeAttempts refused writes it returns
// ErrWriteConflict, having written nothing. Each bucket call waits no longer than ctx (bus.Bounded).
// It returns the interest written and the revision written, 0 for a delete or for nothing to delete,
// and for a skip the revision the interest stands at.
//
// Every revision it writes at is the copy's own fence for the key, read through the handle it writes
// through (kvwatch.Copy.FenceFor), so a write made through a handle on a bucket since recreated
// names no revision of the new bucket's.
func (r *Registry) writeInterest(ctx context.Context, sessionID string, change func(cur Interest, curErr error) (next Interest, action writeAction, err error)) (Interest, uint64, error) {
	r.mu.RLock()
	_, epoch := r.watcher.Handle()
	cur, found := r.cache.Get(sessionID)
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	var curErr error
	read := !found || revision == 0
	for range writeAttempts {
		if read {
			cur, revision, curErr = r.readInterest(ctx, sessionID)
		}
		read = true
		next, action, err := change(cur, curErr)
		if err != nil {
			return Interest{}, 0, err
		}
		var written uint64
		switch {
		case action == writeSkip:
			return next, revision, nil
		case action == writeRemove && revision == 0:
			return next, 0, nil
		case action == writeRemove:
			err = r.deleteInterestAtRevision(ctx, sessionID, revision)
		default:
			written, err = r.putInterestAt(ctx, sessionID, next, revision)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		if err != nil {
			return Interest{}, 0, err
		}
		return next, written, nil
	}
	return Interest{}, 0, fmt.Errorf("%w: %s", ErrWriteConflict, sessionID)
}

// putInterestAt writes item as sessionID's interest while revision is its latest (0: while it has
// none), waiting no longer than ctx (bus.Bounded), applies the write to the cache, and returns the
// revision written.
func (r *Registry) putInterestAt(ctx context.Context, sessionID string, item Interest, revision uint64) (uint64, error) {
	buf, err := encodeInterest(item)
	if err != nil {
		return 0, err
	}
	kv, epoch := r.watcher.Handle()
	written, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		if revision == 0 {
			return kv.Create(sessionID, buf)
		}
		return kv.Update(sessionID, buf, revision)
	})
	if !answered {
		return 0, fmt.Errorf("write %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, item, written)
	r.mu.Unlock()
	return written, nil
}

// readInterest reads sessionID's interest from the bucket, waiting no longer than ctx (bus.Bounded),
// applies it to the cache, and returns it with its revision. A key with no value is
// nats.ErrKeyNotFound at revision 0. A value that does not decode is ErrMalformedInterest at its
// revision, logged at WARN with the session it belongs to.
func (r *Registry) readInterest(ctx context.Context, sessionID string) (Interest, uint64, error) {
	kv, epoch := r.watcher.Handle()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return Interest{}, 0, fmt.Errorf("read %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return Interest{}, 0, err
	}
	r.mu.Lock()
	r.cache.Apply(r.watcher, epoch, kvwatch.LegacyEntry(entry))
	r.mu.Unlock()
	item, err := decodeInterestValue(entry.Value())
	if err != nil {
		r.logger().Warn("registry read an interest record it cannot decode",
			slog.String("session_id", sessionID),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return Interest{}, entry.Revision(), fmt.Errorf("%w: %w", ErrMalformedInterest, err)
	}
	return item, entry.Revision(), nil
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound or ErrMalformedInterest)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics. ErrMalformedInterest is merged the same way: the record answered, so this is
// not the transient case, its topics are ones this build never routed (the cache holds no record
// it cannot decode), and the session's own subscribe is the authority on its record.
//
// The merged row keeps no topic reaching a private stream subject other than the
// session's own agent subject (dropPrivateStreamTopics): a row stored before the
// listener refused such a topic is cleaned at the session's next registration.
//
// The machine id and the directory are the requesting registration's, and the stored row is the
// fallback for whichever of the two the request leaves empty. A session that registers again
// through another listener therefore moves its row to that listener, and every reader of the row
// - the fan-out index (subjectIndex.set, which moves the session's subjects to the new root),
// Registry.Match, the durable path's ownership check and the sender stamp of a message the
// session sends (cmd/listener's senderStamp) - names the listener the session is registered
// through now. A request that names no machine or no directory says nothing about where the
// session runs, so for it the stored value stands: a role claim carries the claiming listener's
// machine id and no directory (SetRoleWithPrevious), and claiming a role must not clear the
// row's Cwd.
func mergeForUpsert(cur Interest, getErr error, item Interest, topics []string, now int64) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) && !errors.Is(getErr, ErrMalformedInterest) {
		return Interest{}, getErr
	}
	requested := item
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(requested.MachineID, cur.MachineID)
	item.Dir = first(requested.Dir, cur.Dir)
	item.UpdatedAt = now
	item = Merge(item, topics)
	item.Topics = dropPrivateStreamTopics(requested.SessionID, item.Topics)
	sort.Strings(item.Topics)
	return item, nil
}

// Remove releases sessionID's role claims and removes topics from its interest record, or deletes
// the record when topics is empty, each bucket call waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. A write or delete ctx abandoned may still land
// after this answer; releaseReadRoleClaim, deleteInterest and removeInterestTopics each say why
// that is safe. A record this build cannot decode (ErrMalformedInterest) is deleted all the same;
// removing some topics leaves it as it is (removeInterestTopics). The claims a deregistration
// releases come from two sources: the role topics of the session's own interest record
// (releaseRoleClaims), which runs first, and then the roles the role claim view names it the
// holder of (releaseAllRoleClaims).
func (r *Registry) Remove(ctx context.Context, sessionID string, topics []string) error {
	if len(topics) == 0 {
		item, err := r.Get(ctx, sessionID)
		if err == nil {
			if err := r.releaseRoleClaims(ctx, sessionID, item.Topics); err != nil {
				return err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
			return err
		}
		if err := r.releaseAllRoleClaims(ctx, sessionID); err != nil {
			return err
		}
	} else if err := r.releaseRoleClaims(ctx, sessionID, topics); err != nil {
		return err
	}
	return r.removeInterestTopics(ctx, sessionID, topics)
}

// removeInterestTopics removes topics from sessionID's stored interest by compare-and-set
// (writeInterest), deleting the interest when none is left; no topics removes it whole. Each bucket
// call waits no longer than ctx; a write ctx abandoned may still land after this answer, and is safe
// for the reasons UpsertWithin's is. A record this build cannot decode routes none of its topics
// here, so there is nothing to remove from it: it is left for the session's next subscribe to
// replace or a delete to remove, and the registry logs that at WARN naming the session.
func (r *Registry) removeInterestTopics(ctx context.Context, sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	_, _, err := r.writeInterest(ctx, sessionID, func(cur Interest, curErr error) (Interest, writeAction, error) {
		if curErr != nil {
			return Interest{}, writeStore, curErr
		}
		item := Remove(cur, topics)
		item.Topics = dropPrivateStreamTopics(sessionID, item.Topics)
		if len(item.Topics) == 0 {
			return item, writeRemove, nil
		}
		item.UpdatedAt = r.now().UnixMilli()
		return item, writeStore, nil
	})
	if errors.Is(err, ErrMalformedInterest) {
		r.logger().Warn("registry left an interest record it cannot decode unchanged",
			slog.String("session_id", sessionID),
			slog.Any("topics", topics),
		)
		return nil
	}
	return err
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
