package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// UpsertWithin merges topics into item.SessionID's interest record (mergeForUpsert) and writes it,
// stamped with the registry's clock, each bucket call waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. It returns the record as it now stands. A record
// this build cannot decode (ErrMalformedInterest) is replaced by item and topics alone, and the
// registry logs that at WARN naming the session. A write ctx abandoned may still land after this
// answer. That is safe: it writes the record the caller's retry writes again, it rides the
// connection ahead of any write this listener sends on it later, so it cannot undo one, and
// against another listener's write it is last-writer-wins, as every Put already is.
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
	cur, getErr := r.Get(ctx, item.SessionID)
	now := r.now().UnixMilli()
	merged, err := mergeForUpsert(cur, getErr, item, topics, now)
	if err != nil {
		return Interest{}, err
	}
	buf, err := json.Marshal(merged)
	if err != nil {
		return Interest{}, err
	}
	if getErr == nil && now-cur.UpdatedAt < InterestRefreshAge.Milliseconds() {
		// What the skip asks is whether this write would store the row it holds, so it compares
		// the encoding of merged with that of the current row rather than the fields one by one:
		// a field added to Interest, and the difference between a null and an empty topic list,
		// are then covered by the encoding itself. The current row is stamped with merged's
		// UpdatedAt, the one field a write of an unchanged row would move.
		unchanged := cur
		unchanged.UpdatedAt = merged.UpdatedAt
		stored, err := json.Marshal(unchanged)
		if err != nil {
			return Interest{}, err
		}
		if bytes.Equal(buf, stored) {
			return cur, nil
		}
	}
	revision, epoch, err := r.putInterest(ctx, merged.SessionID, buf)
	if err != nil {
		return Interest{}, err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, merged.SessionID, merged, revision)
	r.mu.Unlock()
	if errors.Is(getErr, ErrMalformedInterest) {
		r.logger().Warn("registry replaced an interest record it cannot decode",
			slog.String("session_id", merged.SessionID),
			slog.Uint64("revision", revision),
			slog.Any("topics", merged.Topics),
		)
	}
	return merged, nil
}

// putInterest writes sessionID's interest record, waiting no longer than ctx (bus.Bounded), and
// returns its revision with the epoch of the handle it was written through, which the cache needs
// to drop a result from a bucket since recreated (kvwatch.Copy.Put).
func (r *Registry) putInterest(ctx context.Context, sessionID string, value []byte) (uint64, uint64, error) {
	kv, epoch := r.watcher.Handle()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Put(sessionID, value) })
	if !answered {
		return 0, epoch, fmt.Errorf("write %s's interest record: %w", sessionID, err)
	}
	return revision, epoch, err
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

// Remove releases the role claims of sessionID that release authorises (RoleReleaseAuthority)
// and removes topics from its interest record, or deletes the record when topics is empty, each
// bucket call waiting no longer than ctx (bus.Bounded): an HTTP route passes its request's own
// context. A write or delete ctx abandoned may still land after this answer;
// releaseReadRoleClaim, deleteInterest and removeInterestTopics each say why that is safe. A
// record this build cannot decode (ErrMalformedInterest) is deleted all the same; removing some
// topics leaves it as it is (removeInterestTopics).
//
// How the release is authorised is the caller's to state and is never inferred from the topics.
// The one thing release decides is which claims are freed, and a release that authorises none
// frees none - which is a request from a second process that resumed this session's id, or one
// from a client that cannot say which process it is, and neither may take a role from the
// process that holds it.
//
// What the release freed decides one thing in the interest half too. A role lane topic the
// caller named whose claim still names this session and which this release did not free stays
// in the record (releaseRoleClaims answers which topics those are), because one interest record
// serves every process running under one session id and that topic is the interest half of a
// claim that still stands, written there by the grant (SetRoleWithPrevious). No delivery rides
// on it - a role lane is not stream-captured, so it never enters a session's consumer filter
// (cmd/listener's sessionConsumerFilter), and a role message reaches its holder's agent subject
// with the holder read off the claim (cmd/listener's resolveCoreRoleHolder) - but the record is
// where a deregistration of this session finds the role claims it frees (the empty-topics
// branch below, releaseRoleClaims), so a release from a process that does not hold the claim
// would otherwise take the lane topic out from under the process that does and leave the claim
// standing with nothing in the record naming it. Every other topic the caller named is
// removed exactly as before - one that is not a role lane's, a wildcard, a role no record
// claims, a role another session holds, and a role whose claim this release did free. A caller
// whose every named topic was kept writes nothing at all, since an empty list is how
// removeInterestTopics is told to delete the whole record.
//
// Remove answers the topics it took out of the record, and that list is what POST
// /v1/interests/unsubscribe reports as removed. It is the removed list rather than the one the
// caller named because the two part company over a kept lane topic: the caller named it, the
// record still holds it, and an answer echoing the request would report a removal that did not
// happen. A deregistration answers no list at all - it deletes the whole record, so there is no
// list of named topics to report, and its route writes the empty list for it.
//
// An empty topics is the whole of the session's interests, so the claims a deregistration
// releases come from two sources: the role topics of the session's own interest record
// (releaseRoleClaims), which runs first, and then the roles the role claim view names it the
// holder of (releaseAllRoleClaims). A role the session holds whose topic the record has lost is
// therefore released too, by the same authority as the rest.
//
// Whether this release may mark a claim it cannot free handed back (RoleClaim.HandedBack) is
// decided here and nowhere else, and carried on the release itself
// (RoleReleaseAuthority.handsBack, which no caller outside this package can set): a release
// marks exactly when the caller named topics of its own, and stated no process, and did not
// force. The named topics are what make the release a hand-back of those very roles - the
// caller named that role's lane topic - while the empty-topics branch expands the stored
// record's own topics and then sweeps the view, which is a deregistration of everything the
// session has and a hand-back of no one role, so it marks nothing whatever those topics turn
// out to be. A release that names a process frees the claims of that process and leaves
// another process's alone, and the administrator's force frees them all; neither leaves a
// mark, since a mark is for the hand-back that could free nothing.
func (r *Registry) Remove(ctx context.Context, sessionID string, topics []string, release RoleReleaseAuthority) ([]string, error) {
	if len(topics) == 0 {
		item, err := r.Get(ctx, sessionID)
		if err == nil {
			if _, err := r.releaseRoleClaims(ctx, sessionID, item.Topics, release); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
			return nil, err
		}
		if err := r.releaseAllRoleClaims(ctx, sessionID, release); err != nil {
			return nil, err
		}
		return nil, r.removeInterestTopics(ctx, sessionID, topics)
	}
	release.handsBack = release.ClientInstanceID == "" && !release.Force
	kept, err := r.releaseRoleClaims(ctx, sessionID, topics, release)
	if err != nil {
		return nil, err
	}
	removed := topicsExcept(topics, kept)
	if len(removed) == 0 {
		return removed, nil
	}
	if err := r.removeInterestTopics(ctx, sessionID, removed); err != nil {
		return nil, err
	}
	return removed, nil
}

// topicsExcept is topics less the ones in kept, which releaseRoleClaims left standing. It
// returns topics itself where nothing was kept, the usual case, so a release that freed what it
// named allocates nothing here. kept holds the role lane topics of one request, which is a
// handful at most, so each topic is weighed against it by a walk rather than by a map built for
// the purpose.
func topicsExcept(topics, kept []string) []string {
	if len(kept) == 0 {
		return topics
	}
	removed := make([]string, 0, len(topics)-len(kept))
	for _, topic := range topics {
		if !slices.Contains(kept, topic) {
			removed = append(removed, topic)
		}
	}
	return removed
}

// removeInterestTopics removes topics from sessionID's interest record, deleting it when none is
// left or topics is empty, each bucket call waiting no longer than ctx. A write ctx abandoned may
// still land after this answer, and is safe for the reasons UpsertWithin's is. A record this build
// cannot decode routes none of its topics here, so there is nothing to remove from it: it is left
// for the session's next subscribe to replace or a delete to remove, and the registry logs that at
// WARN naming the session.
func (r *Registry) removeInterestTopics(ctx context.Context, sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	item, err := r.Get(ctx, sessionID)
	if errors.Is(err, ErrMalformedInterest) {
		r.logger().Warn("registry left an interest record it cannot decode unchanged",
			slog.String("session_id", sessionID),
			slog.Any("topics", topics),
		)
		return nil
	}
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	item.Topics = dropPrivateStreamTopics(sessionID, item.Topics)
	if len(item.Topics) == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	item.UpdatedAt = r.now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	revision, epoch, err := r.putInterest(ctx, sessionID, buf)
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
