// The session reaper: the one loop that deletes a session's registration record, and the one
// place a session's absence from the bucket is decided. Nothing else may delete a record for a
// session that stopped answering - a lookup, a publish, a role-lane delivery and the role reaper
// all read a state instead (StateOf) - so a session that is merely unresponsive keeps its record,
// its claims and its messages until it is gone, and only this pass takes that record away.

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/metrics"
)

// reapStoredMessageCap bounds the messages one reaped session's stored-message count reads, and
// reapStoredReadBudget bounds the time a whole pass spends on those counts, as replayMaxMessages
// and replayReadBudget bound one /v1/replay answer (cmd/listener/replay.go). A pass holds no
// caller's deadline, but it shares one bus with every delivery this listener makes, and counts
// that read a whole retention window per record would take that bus away from them. A count that
// stopped at either bound says so on its log line, so the number is never read as the whole of
// what was stored.
const (
	reapStoredMessageCap = 500
	reapStoredReadBudget = 5 * time.Second
)

// SessionReaper is what a pass of the session reaper reads the stream through and counts on. A
// zero value counts nothing and reads nothing: the pass still deletes and logs, which is what a
// test that cares only about the deletes builds.
type SessionReaper struct {
	// Client reads the notification stream for the messages stored for a session the pass
	// deleted (storedAfterLastSeen). Nil leaves that count out and the log line's at zero.
	Client *bus.Client
	// Reaped counts the sessions that reached the gone horizon with no clean stop: one per
	// record this pass deleted. A session that unregistered itself leaves no record and is
	// counted nowhere.
	Reaped *metrics.Counter
	// StoredAfterLastSeen counts their messages stored after their last registration. It is not
	// a count of unread messages: the listener keeps no read receipt, so a message the session
	// took over its own subscription before it stalled and one it never saw are the same message
	// here.
	StoredAfterLastSeen *metrics.Counter
}

// goneCandidate is one record a pass judged gone, with the revision the copy held for it when the
// pass judged it. The delete is conditioned on that revision, so a registration that lands
// between the decision and the delete keeps the session: the delete is refused and the record
// stands until a later pass judges it again.
type goneCandidate struct {
	sessionID string
	lastSeen  int64
	revision  uint64
}

// StartSessionReaper runs a pass at each tick of interval until ctx ends, and returns a channel
// closed once it has returned: a pass in flight when ctx ends finishes first, as store's reapers
// do (store.Registry.StartReaper). The listener's shutdown waits on it before it drains NATS,
// since a pass reads and writes the bus.
func (r *SessionRegistry) StartSessionReaper(ctx context.Context, interval time.Duration, reaper SessionReaper) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// A tick and the end can both be ready, and select picks either.
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			reaped := r.ReapSessions(ctx, reaper)
			r.logger().Info("session reaper cycle",
				slog.Int("reaped", reaped),
				slog.Int64("elapsed_ms", time.Since(started).Milliseconds()),
			)
		}
	}()
	return done
}

// ReapSessions is one pass of the session reaper, and returns how many records it deleted.
//
// It first writes the deployment's maximum age on the bucket and reads back the age the bucket
// really carries (applyMaxAge), so an age raised or rolled back by another process reaches this
// listener within one pass and with no restart, and a record stands for as long as the deployment
// says before the bucket's own backstop drops it.
//
// Then it deletes every record past the gone horizon, each at exactly the revision the pass
// judged (deleteGone). A session that registered again in between keeps its record: the delete is
// refused, the pass counts and logs nothing for it, and the next pass judges the new record on
// its own age. Only a delete that succeeded is logged and counted, so the line and the counter
// name sessions whose records this listener really took away.
//
// Nothing here fails the pass: an age it could not write, a bucket that did not answer a delete
// and a stored-message count that failed are each logged and the pass goes on, since the next
// tick asks the same questions again.
func (r *SessionRegistry) ReapSessions(ctx context.Context, reaper SessionReaper) int {
	if err := r.applyMaxAge(); err != nil {
		r.logger().Warn("session reaper could not settle the session bucket's maximum age", slog.String("error", err.Error()))
	}
	now := r.now()
	candidates := r.goneCandidates(now)
	if len(candidates) == 0 {
		return 0
	}
	stream := reaper.streamState(ctx)
	// One budget for the whole pass, not one per record: a pass that reaped sixty records with a
	// budget each could spend sixty times it on the one bus every delivery this listener makes
	// crosses, and the next tick would already be due.
	deadline := time.Now().Add(reapStoredReadBudget)
	reaped := 0
	for _, candidate := range candidates {
		deleted, err := r.deleteGone(ctx, candidate)
		if err != nil {
			r.logger().Warn("session reaper kept a record it could not delete",
				slog.String("session_id", candidate.sessionID),
				slog.String("error", err.Error()),
			)
			continue
		}
		if !deleted {
			continue // registered again between the pass's decision and its delete
		}
		reaped++
		stored, bounded := reaper.storedAfterLastSeen(ctx, candidate.sessionID, candidate.lastSeen, stream, deadline)
		reaper.Reaped.Inc()
		reaper.StoredAfterLastSeen.Add(int64(stored))
		r.logger().Warn("session reaper deleted the record of a session that reached the gone horizon with no clean stop",
			slog.String("session_id", candidate.sessionID),
			slog.Int64("last_seen", candidate.lastSeen),
			slog.String("age", now.Sub(time.UnixMilli(candidate.lastSeen)).String()),
			slog.Int("messages_stored_after_last_seen", stored),
			slog.Bool("messages_stored_after_last_seen_bounded", bounded),
		)
	}
	return reaped
}

// goneCandidates is every row of the copy whose last registration puts its session past the gone
// horizon, with the revision the copy holds for each. It takes the shared lock once and asks
// nothing of the bus while it holds it, as store's interest reaper collects its own candidates.
func (r *SessionRegistry) goneCandidates(now time.Time) []goneCandidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, epoch := r.watcher.Handle()
	var candidates []goneCandidate
	for sessionID, cs := range r.cache.All() {
		if r.stateOf(cs.entry, now) != Gone {
			continue
		}
		candidates = append(candidates, goneCandidate{
			sessionID: sessionID,
			lastSeen:  cs.entry.UpdatedAt,
			revision:  r.cache.FenceFor(epoch, sessionID),
		})
	}
	return candidates
}

// deleteGone deletes candidate's record only while the bucket's latest revision is the one the
// pass judged it on, and reports whether it did. A registration that landed since makes JetStream
// refuse the delete with wrong last sequence (10071, which nats.go names nats.ErrKeyExists), and
// the record is kept: false and no error, as store's interest reaper keeps a row written again
// (store.Registry.deleteInterestAt). A copy that holds no revision for the key, because a watch
// moved the epoch since its read began (kvwatch.Copy.FenceFor), names no revision, which a
// conditional delete cannot express, so that record is deleted at the revision the bucket reads
// for it (bus.KeyValue.DeleteAtRead) - still a delete of the record the pass can see and not of
// one written after it.
//
// The copy is told what the delete did either way, so the row and the key's revision fence follow
// the bucket without waiting for the watcher's own delivery.
func (r *SessionRegistry) deleteGone(ctx context.Context, candidate goneCandidate) (bool, error) {
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	r.mu.RUnlock()
	revision := candidate.revision
	if revision == 0 {
		read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.DeleteAtRead(candidate.sessionID) })
		if !answered {
			return false, fmt.Errorf("delete session %s's record: %w", candidate.sessionID, err)
		}
		if err != nil {
			return false, err
		}
		revision = read
	} else {
		_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
			return struct{}{}, kv.Delete(candidate.sessionID, nats.LastRevision(revision))
		})
		if !answered {
			return false, fmt.Errorf("delete session %s's record at revision %d: %w", candidate.sessionID, revision, err)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	r.cache.Evict(r.watcher, epoch, candidate.sessionID, revision+1)
	r.mu.Unlock()
	return true, nil
}

// streamState is the sequence space the notification stream holds, read once per pass (one
// STREAM.INFO) for every stored-message count the pass makes. A reaper with no bus client, and a
// read that failed, leave it zero, which counts nothing: the pass's own work is the delete, and a
// count it could not make must not hold one up.
func (reaper SessionReaper) streamState(ctx context.Context) nats.StreamState {
	if reaper.Client == nil {
		return nats.StreamState{}
	}
	js := reaper.Client.JS()
	if js == nil {
		return nats.StreamState{}
	}
	info, err := js.StreamInfo(bus.Stream, nats.Context(ctx))
	if err != nil {
		return nats.StreamState{}
	}
	return info.State
}

// storedAfterLastSeen counts the messages the notification stream holds on sessionID's own agent
// subject that it stored after lastSeen, reading forward from the stream's first sequence
// (bus.Client.NextStoredMessage) and stopping where the stream holds no more
// (nats.ErrMsgNotFound).
//
// It is the messages stored after the session's last registration and nothing more. The listener
// keeps no read receipt, so it cannot say which of them the session read: a self-subscribed
// session takes its messages over its own NATS subscription, and one that stalled after taking
// them leaves exactly the same stream behind as one that took none. The number says how much a
// replacement would have to replay, not how much was lost.
//
// bounded reports a count that stopped at reapStoredMessageCap, at the pass's read deadline, at
// the end of ctx or at a read that failed, so a partial count is never read as the whole of what
// was stored.
func (reaper SessionReaper) storedAfterLastSeen(ctx context.Context, sessionID string, lastSeen int64, stream nats.StreamState, deadline time.Time) (count int, bounded bool) {
	if reaper.Client == nil || stream.LastSeq == 0 || lastSeen <= 0 {
		return 0, false
	}
	subject := contracts.AgentSubject(sessionID)
	after := time.UnixMilli(lastSeen)
	// The cap is on messages read, not on messages counted. The walk starts at the stream's
	// first sequence, and every message this subject holds from before the session's last
	// registration is read and discarded on the way to the ones that count - a count capped on
	// its hits alone would read a whole retention window to reach a cap it never hit.
	reads := 0
	for seq := stream.FirstSeq; seq <= stream.LastSeq; {
		if reads >= reapStoredMessageCap || time.Now().After(deadline) || ctx.Err() != nil {
			return count, true
		}
		reads++
		stored, err := reaper.Client.NextStoredMessage(ctx, subject, seq)
		if errors.Is(err, nats.ErrMsgNotFound) {
			return count, false
		}
		if err != nil {
			return count, true
		}
		if stored.Sequence > stream.LastSeq {
			return count, false
		}
		if stored.Time.After(after) {
			count++
		}
		seq = stored.Sequence + 1
	}
	return count, false
}
