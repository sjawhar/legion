package session

// The session registry's deletes: the plain one DELETE /v1/sessions/<session_id> makes, the
// conditional one a caller that names a client instance or a moment makes (DeleteIf), the
// outcomes that answer it, and the cache reconciliation both end with. They live apart from
// registry.go because they are one decision read together: what a delete compares, what it
// answers for an entry it keeps, and what the cache holds afterwards.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// Delete deletes sessionID's entry at the revision it reads (bus.KeyValue.DeleteAtRead), each
// bucket call waiting no longer than ctx (bus.Bounded): DELETE /v1/sessions/<session_id> passes its
// request's own context. A delete ctx abandoned may still land after this answer, and that is safe:
// it deletes only the revision it read, so a registration that landed since is kept, and a key it
// found absent it deletes at none, as DeleteAtRead always has.
func (r *SessionRegistry) Delete(ctx context.Context, sessionID string) error {
	// The fence read before the delete is the bucket's this handle reaches only when the read under
	// way began in its epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.DeleteAtRead(sessionID) })
	if !answered {
		return fmt.Errorf("delete session %s from the bucket: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.reconcileDeleted(ctx, kv, epoch, sessionID, max(revision, read))
	return nil
}

// DeleteOutcome is what a conditional delete (DeleteIf) did with the entry it was asked to
// remove, and what DELETE /v1/sessions/<session_id> words for the client that named a condition.
// The outcomes that keep the entry are told apart because they are separate facts: one says
// another client instance's registration is stored under that session id, one that the stored
// registration is newer than the delete that asked for it, and the last that the stored value
// kept changing under the conditional delete, which says nothing about what is stored now.
// One "kept" answer for all three would make the route's reason false for whichever case it did
// not describe.
type DeleteOutcome uint8

const (
	// EntryUnanswered goes with an error: a bucket call did not answer, so nothing is known
	// about the entry. It is the zero value, so a caller that reads the outcome without the
	// error never reads a delete that did not happen as one that did.
	EntryUnanswered DeleteOutcome = iota
	// EntryDeleted: the entry is gone, which a key that was already absent counts as, since
	// Delete is idempotent.
	EntryDeleted
	// EntryKeptForAnotherInstance: the stored entry carries another client instance's id, so
	// it is another process's registration and this caller does not remove it.
	EntryKeptForAnotherInstance
	// EntryKeptForALaterRegistration: the stored entry was written after the moment the
	// delete belongs to (DeleteConditions.NotRegisteredAfter), so it is the registration of a
	// process that started since and this caller does not remove it.
	EntryKeptForALaterRegistration
	// EntryKeptAfterLostRace: the entry matched, and both conditional deletes lost to a write
	// that landed between the read and the delete — most often the deleting instance's own
	// heartbeat. The entry stayed, and whose registration it holds now is exactly what this
	// call did not learn.
	EntryKeptAfterLostRace
)

// String names the outcome, for the log line or test failure that reports one.
func (o DeleteOutcome) String() string {
	switch o {
	case EntryDeleted:
		return "deleted"
	case EntryKeptForAnotherInstance:
		return "kept for another client instance"
	case EntryKeptForALaterRegistration:
		return "kept for a later registration"
	case EntryKeptAfterLostRace:
		return "kept after a lost compare-and-swap"
	default:
		return "unanswered"
	}
}

// DeleteConditions are what a conditional delete compares the stored registration against before
// it removes it (DeleteIf). A zero field is no condition at all, and a caller that names none
// compares nothing and calls Delete instead.
type DeleteConditions struct {
	// ClientInstanceID is the client instance the caller deregisters for: the entry is
	// removed only while it carries that id or carries none (InstanceMatches). It is the
	// deregistration of a caller that mints an instance id of its own: a Claude Code /clear
	// handoff deletes the session it is handing off from before it claims that session's
	// role, and must not delete the live entry another process wrote after it resumed the
	// same session id. Empty compares no instance.
	ClientInstanceID string
	// NotRegisteredAfter is the moment the delete belongs to, in unix milliseconds: the entry
	// is removed only while it was last written at or before it (SessionEntry.UpdatedAt). It
	// is what the Legion daemon's clean stop of a dead role's session carries — the moment
	// its probe judged the agent process gone (dispatch://INFRA-392) — so the registration
	// the relaunch of that claim writes under the same session id, which is newer than the
	// stop, stays. Zero or negative compares no moment.
	NotRegisteredAfter int64
}

// keeps is the outcome that keeps stored, or EntryDeleted for an entry every condition admits.
func (c DeleteConditions) keeps(stored SessionEntry) DeleteOutcome {
	switch {
	case !InstanceMatches(stored.ClientInstanceID, c.ClientInstanceID):
		return EntryKeptForAnotherInstance
	case c.NotRegisteredAfter > 0 && stored.UpdatedAt > c.NotRegisteredAfter:
		return EntryKeptForALaterRegistration
	default:
		return EntryDeleted
	}
}

// DeleteIf deletes sessionID's entry only while the stored entry meets every condition conditions
// names (DeleteConditions.keeps), and reports what it did, each bucket call waiting no longer
// than ctx (bus.Bounded).
//
// Every comparison holds at the revision it deletes: the entry is read for its value and its
// revision together, and the delete names that revision (nats.LastRevision), so a registration
// that landed in between is kept rather than deleted on the strength of a value that is no longer
// stored. JetStream refuses that delete with wrong last sequence (10071, which nats.go names
// nats.ErrKeyExists), and the whole sequence runs once more: the common cause is the deleting
// instance's own heartbeat landing between the read and the delete, and answering "kept" for it
// would leave the predecessor's entry alive and turn the role claim that follows into a 409 for
// no reason. A second lost compare-and-swap answers EntryKeptAfterLostRace and no error, the
// outcome that says the entry stayed without claiming whose registration it is now.
//
// EntryDeleted and no error means the entry is gone; any kept outcome and no error means the
// entry stayed, and the caller keeps whatever it names; an error means the bucket did not answer,
// ctx's end among them, and the outcome is EntryUnanswered. The cache follows the bucket
// afterwards exactly as it does after Delete. A delete ctx abandoned may still land after this
// answer, and that is safe: it deletes only the revision it compared.
func (r *SessionRegistry) DeleteIf(ctx context.Context, sessionID string, conditions DeleteConditions) (DeleteOutcome, error) {
	// The handle and the fence are read together before the bucket calls, as Delete reads them:
	// the fence is the bucket's this handle reaches only when the read under way began in its
	// epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	fence := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	for attempt := 0; ; attempt++ {
		entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
		if !answered {
			return EntryUnanswered, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
		}
		if errors.Is(err, nats.ErrKeyNotFound) {
			return EntryDeleted, nil
		}
		if err != nil {
			return EntryUnanswered, err
		}
		revision := entry.Revision()
		var stored SessionEntry
		if err := json.Unmarshal(entry.Value(), &stored); err != nil {
			// A value this build cannot read names no instance and no moment, so it belongs
			// to nobody and is older than every stop: it is deleted as an entry that meets
			// every condition is, and the delete is not blocked by a record no reader can act
			// on. What json.Unmarshal decoded before the error is dropped for that reason: on
			// a type error it still fills every field it could read, wherever the bad field
			// sits, so a value carrying a client instance id or a heartbeat would otherwise be
			// compared against the caller's and kept — the opposite of what this branch
			// decides.
			stored = SessionEntry{}
			r.logger().Warn("session registry deleting a malformed value under a conditional delete",
				slog.String("key", entry.Key()),
				slog.Uint64("revision", revision),
				slog.String("error", err.Error()),
			)
		}
		if kept := conditions.keeps(stored); kept != EntryDeleted {
			return kept, nil
		}
		_, answered, err = bus.Bounded(ctx, func() (struct{}, error) {
			return struct{}{}, kv.Delete(sessionID, nats.LastRevision(revision))
		})
		if !answered {
			return EntryUnanswered, fmt.Errorf("delete session %s from the bucket at revision %d: %w", sessionID, revision, err)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			if attempt == 0 {
				continue
			}
			return EntryKeptAfterLostRace, nil
		}
		if err != nil {
			return EntryUnanswered, err
		}
		r.reconcileDeleted(ctx, kv, epoch, sessionID, max(fence, revision))
		return EntryDeleted, nil
	}
}

// reconcileDeleted brings the cache up to what the bucket holds for sessionID after a delete this
// registry made: the key's history has the last word, so a registration that landed since the
// delete is cached rather than dropped, and a value this build cannot read is evicted and logged.
// A history that does not answer leaves the fence revision+1 behind instead, above the revision
// the delete acted on, so the watcher's own in-flight update for an older revision cannot restore
// the entry. revision is the newest revision the caller knows of for the key, and epoch the one
// the kv handle was read in: a result through a handle of an earlier epoch reaches the copy no
// more here than anywhere else (kvwatch.Copy). It calls OnChange's functions for sessionID once it
// has released r.mu, when it changed the cache.
func (r *SessionRegistry) reconcileDeleted(ctx context.Context, kv bus.KeyValue, epoch uint64, sessionID string, revision uint64) {
	// A history that does not answer falls back to the fence below: the delete itself answered.
	// ReadHistory ends with ctx; bus.RequestTimeout bounds it for a caller that answers no request
	// and so passes a context that never ends.
	historyCtx, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
	entries, err := kv.ReadHistory(historyCtx, sessionID)
	cancel()
	r.mu.Lock()
	defer r.unlockAndNotify()
	if err != nil || len(entries) == 0 {
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		return
	}
	latest := kvwatch.JetStreamEntry(entries[len(entries)-1])
	if _, malformed := r.cache.Apply(r.watcher, epoch, latest); malformed != nil {
		r.logger().Warn("session registry cache evicted malformed value",
			slog.String("key", latest.Key),
			slog.Uint64("revision", latest.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}
