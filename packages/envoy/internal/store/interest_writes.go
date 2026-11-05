package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

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
//
// It reads the session's roles itself (controllerLaneRoles). A caller that already read them to
// decide the same topics elsewhere, the listener's registration route filtering a session
// consumer, plans once and writes that plan instead (PlanInterest, UpsertPlanned).
func (r *Registry) UpsertWithin(ctx context.Context, item Interest, topics []string) (Interest, error) {
	cur, getErr := r.Get(ctx, item.SessionID)
	roles, err := r.controllerLaneRoles(item.SessionID, cur.Topics, item.Topics, topics)
	if err != nil {
		return Interest{}, err
	}
	return r.upsert(ctx, cur, getErr, item, InterestPlan{Roles: roles, Topics: topics})
}

// PlanInterest plans the interest row a registration of sessionID naming requested will store:
// the topics it will hold (MergedTopics of the stored row and requested), the roles read to
// decide them, read once for the row's topics and requested together (controllerLaneRoles),
// requested itself and the bucket revision of the row it read. The listener's registration route
// filters the session's consumer on the plan and hands the same plan back to UpsertPlanned, so a
// registration whose body leaves out a controller notice lane its row holds keeps that lane in
// the filter and in the row alike: the roles behind both are this one read, which the row's own
// topics are part of.
//
// A session with no row, and a row this build cannot decode, plan from no stored topics: the
// registration's own topics are the authority on such a record, as the write that follows treats
// it (mergeForUpsert). Any other read that did not answer is returned, and so is a role view
// that cannot answer yet (ErrRoleViewNotReady): the route refuses the registration rather than
// filter a consumer on a row the write would decide differently.
func (r *Registry) PlanInterest(ctx context.Context, sessionID string, requested []string) (InterestPlan, error) {
	return r.planInterest(ctx, sessionID, slices.Clone(requested), false)
}

// PlanInterestAfterConflict is PlanInterest over the row the bucket now holds rather than the
// cached one: the caller's last plan was refused its write (UpsertPlanned, ErrInterestPlanConflict)
// by the writer that stored that row, and a plan made from a cache still behind it would be
// refused again. Its caller filters the session's consumer on this plan before writing it, as it
// did for the first, so the filter and the row still name one set after a conflict.
func (r *Registry) PlanInterestAfterConflict(ctx context.Context, sessionID string, requested []string) (InterestPlan, error) {
	return r.planInterest(ctx, sessionID, slices.Clone(requested), true)
}

// planInterest is PlanInterest over a row read from the cache, or from the bucket alone when
// fromBucket is set: a plan made again after the bucket refused a write at the revision the last
// plan read must see the row that refused it, which the cache may not hold yet.
func (r *Registry) planInterest(ctx context.Context, sessionID string, requested []string, fromBucket bool) (InterestPlan, error) {
	cur, revision, err := r.getWithRevision(ctx, sessionID, fromBucket)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
		return InterestPlan{}, err
	}
	roles, err := r.controllerLaneRoles(sessionID, cur.Topics, requested)
	if err != nil {
		return InterestPlan{}, err
	}
	return InterestPlan{
		Roles:    roles,
		Topics:   MergedTopics(sessionID, roles, cur.Topics, requested),
		Revision: revision,
	}, nil
}

// ErrInterestPlanConflict is a write whose interest row was rewritten under the plan it was made
// for: another writer stored the row between the plan and the write, so the compare-and-swap
// found a revision that is not the plan's. Its caller plans again (PlanInterestAfterConflict),
// filters the session's consumer on the new plan and writes again, a bounded number of times
// (registrationPlanAttempts in cmd/listener, whose loop this error belongs to); past that the
// listener answers the bus-unavailable 503 and the plugin's next heartbeat registers again.
// Nothing is stored for a refused write, so the row keeps whatever the write that won it holds.
var ErrInterestPlanConflict = errors.New("the interest record changed under the plan of this registration")

// UpsertPlanned is UpsertWithin for a caller holding a plan (PlanInterest): it writes the row
// that plan decided, under the roles that decided it, rather than reading the session's roles
// again.
//
// The write is one compare-and-swap at the revision the plan read (putInterestAt), because
// seconds pass between the plan and this call - the listener's registration route creates or
// re-filters the session's consumer in between - and an unsubscribe landing in that window would
// otherwise be undone: the plan merged the row as it stood, so writing it back puts every topic
// the unsubscribe removed into the row again, and the row only ever grows from there. A refused
// write is ErrInterestPlanConflict and stores nothing. Planning again is the caller's, not this
// call's: the filter of the session's consumer is built from the plan, so a plan made here would
// store a row the filter was never set for, and the two would name different sets until the
// session's next heartbeat (InterestPlan, cmd/listener's subscribeHandler).
func (r *Registry) UpsertPlanned(ctx context.Context, item Interest, plan InterestPlan) (Interest, error) {
	cur, getErr := r.Get(ctx, item.SessionID)
	merged, buf, unchanged, err := r.interestRow(cur, getErr, item, plan)
	if err != nil {
		return Interest{}, err
	}
	if unchanged {
		return cur, nil
	}
	revision, epoch, stored, err := r.putInterestAt(ctx, merged.SessionID, buf, plan.Revision)
	if err != nil {
		return Interest{}, err
	}
	if !stored {
		return Interest{}, fmt.Errorf("write %s's interest record: %w", item.SessionID, ErrInterestPlanConflict)
	}
	r.recordInterest(merged, revision, epoch, getErr)
	return merged, nil
}

// upsert is UpsertWithin's write, over the row cur that getErr reports the read of and the plan
// its caller decided. It writes unconditionally, where UpsertPlanned compares and swaps:
// UpsertWithin reads and writes within the one call, so no window stands between them for
// another writer to lose a topic in, and UpsertWithin's own docstring says why a write of its
// that lands after its answer is safe.
func (r *Registry) upsert(ctx context.Context, cur Interest, getErr error, item Interest, plan InterestPlan) (Interest, error) {
	merged, buf, unchanged, err := r.interestRow(cur, getErr, item, plan)
	if err != nil {
		return Interest{}, err
	}
	if unchanged {
		return cur, nil
	}
	revision, epoch, err := r.putInterest(ctx, merged.SessionID, buf)
	if err != nil {
		return Interest{}, err
	}
	r.recordInterest(merged, revision, epoch, getErr)
	return merged, nil
}

// interestRow is the row a write of item under plan over the row cur that getErr reports the read
// of stores, and its encoding, or unchanged when the bucket already holds that row young enough
// to leave alone (UpsertWithin's heartbeat skip).
func (r *Registry) interestRow(cur Interest, getErr error, item Interest, plan InterestPlan) (merged Interest, buf []byte, unchanged bool, err error) {
	now := r.now().UnixMilli()
	merged, err = mergeForUpsert(cur, getErr, item, now, plan)
	if err != nil {
		return Interest{}, nil, false, err
	}
	buf, err = json.Marshal(merged)
	if err != nil {
		return Interest{}, nil, false, err
	}
	if getErr == nil && now-cur.UpdatedAt < InterestRefreshAge.Milliseconds() {
		// What the skip asks is whether this write would store the row it holds, so it compares
		// the encoding of merged with that of the current row rather than the fields one by one:
		// a field added to Interest, and the difference between a null and an empty topic list,
		// are then covered by the encoding itself. The current row is stamped with merged's
		// UpdatedAt, the one field a write of an unchanged row would move.
		stored := cur
		stored.UpdatedAt = merged.UpdatedAt
		encoded, err := json.Marshal(stored)
		if err != nil {
			return Interest{}, nil, false, err
		}
		if bytes.Equal(buf, encoded) {
			return merged, buf, true, nil
		}
	}
	return merged, buf, false, nil
}

// recordInterest caches the row a write stored at its revision, through the handle of epoch it
// was written with (kvwatch.Copy.Put), and logs the replacement of a record this build cannot
// decode, which getErr reports.
func (r *Registry) recordInterest(merged Interest, revision, epoch uint64, getErr error) {
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

// putInterestAt writes sessionID's interest record only while its latest revision is revision,
// creating it when revision is 0, which names the session that had no row when its plan read the
// bucket. It reports the revision written, the epoch of the handle it was written through, and
// whether the bucket took the write: a bucket that refused it holds another writer's row since,
// which is no error here (nats.ErrKeyExists, the wrong-last-sequence answer a Create and an
// Update share), and the caller plans again over that row. Each call waits no longer than ctx
// (bus.Bounded); a write ctx abandoned may still land, and is safe for the same reason a
// conditional delete is: it stores only over the very revision it read, so it cannot undo a write
// that landed after it.
func (r *Registry) putInterestAt(ctx context.Context, sessionID string, value []byte, revision uint64) (uint64, uint64, bool, error) {
	kv, epoch := r.watcher.Handle()
	written, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		if revision == 0 {
			return kv.Create(sessionID, value)
		}
		return kv.Update(sessionID, value, revision)
	})
	if !answered {
		return 0, epoch, false, fmt.Errorf("write %s's interest record at revision %d: %w", sessionID, revision, err)
	}
	if errors.Is(err, nats.ErrKeyExists) {
		return 0, epoch, false, nil
	}
	if err != nil {
		return 0, epoch, false, err
	}
	return written, epoch, true, nil
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
// The merged row keeps no topic reaching a private stream subject that is not the session's own,
// and holds them sorted (MergedTopics): a row stored before the listener refused such a topic is
// cleaned at the session's next registration. plan is the topics to merge in and the roles the
// session holds (controllerLaneRoles), which decide the one private subject a session registers
// beside its own agent subject: the controller notice lane of the project whose controller role
// it holds. The listener's registration route filters the session's consumer on the row this
// merge produces, having planned it with these same roles (PlanInterest).
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
func mergeForUpsert(cur Interest, getErr error, item Interest, now int64, plan InterestPlan) (Interest, error) {
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
	item.Topics = MergedTopics(requested.SessionID, plan.Roles, item.Topics, plan.Topics)
	return item, nil
}

// controllerLaneRoles reports the roles sessionID holds (RolesHeldBy) when one of the topic sets
// names a Legion controller notice lane (bus.NamesControllerNoticeLane), and nil when none does:
// that lane is the one private stream subject whose owner the rule reads a session's roles for
// (bus.ReachesPrivateStreamSubject), so a write naming none makes no view read and is refused for
// none. A write that names one is refused while the view cannot answer (ErrRoleViewNotReady)
// rather than storing the row without the lane: the row drives the session's consumer filter, and
// a notice published while the lane was out of that filter reaches the controller on no later
// path.
func (r *Registry) controllerLaneRoles(sessionID string, topics ...[]string) ([]string, error) {
	named := slices.ContainsFunc(topics, bus.NamesControllerNoticeLane)
	if !named {
		return nil, nil
	}
	return r.RolesHeldBy(sessionID)
}

// Remove releases sessionID's role claims and removes topics from its interest record, or deletes
// the record when topics is empty, each bucket call waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. A write or delete ctx abandoned may still land after
// this answer; releaseRoleClaim, deleteInterest and removeInterestTopics each say why that is safe.
// A record this build cannot decode (ErrMalformedInterest) is deleted all the same; removing some
// topics leaves it as it is (removeInterestTopics).
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
	roles, err := r.controllerLaneRoles(sessionID, item.Topics)
	if err != nil {
		return err
	}
	item.Topics = dropPrivateStreamTopics(sessionID, roles, item.Topics)
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
