// The session reaper: the one loop that deletes a session's registration record, and the one
// place a session's absence from the bucket is decided. Nothing else may delete a record for a
// session that stopped answering - a lookup, a publish, a role-lane delivery and the role reaper
// all read a state instead (StateOf) - so a session that is merely unresponsive keeps its record,
// its claims and its messages until it is gone, and only this pass takes that record away.

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
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/metrics"
)

// reapStoredMessageCap bounds the reads one reaped session's stored-message count makes, and
// reapStoredReadBudget bounds the time a whole pass spends on those counts, as replayMaxMessages
// and replayReadBudget bound one /v1/replay answer (cmd/listener/replay.go). A pass holds no
// caller's deadline, but it shares one bus with every delivery this listener makes, and counts
// that read a whole retention window per record would take that bus away from them. Each read is
// held to the pass's own deadline as well, since a stored-message read waits 10 s of its own
// otherwise (bus.Client.NextStoredMessage), which is twice the budget for every count a pass
// makes. A count that stopped at either bound says so on its log line, so the number is never
// read as the whole of what was stored.
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
// (store.Registry.deleteInterestAt).
//
// A copy that holds no revision for the key, because a watch moved the epoch since its read began
// (kvwatch.Copy.FenceFor), names none for the delete to be conditioned on, which a conditional
// delete cannot express. Such a record is read from the bucket and judged again on its own last
// registration (judgeGone), and deleted at the revision that read answered, so the delete is
// still of a record this pass judged gone and never of one written after it: a session that
// registered in between reads live or unresponsive and keeps its record, and a key the bucket no
// longer holds is no delete at all. Both answer false and no error, so a pass counts and logs
// only sessions whose records it really took away.
//
// The copy is told what the delete did either way, so the row and the key's revision fence follow
// the bucket without waiting for the watcher's own delivery.
func (r *SessionRegistry) deleteGone(ctx context.Context, candidate goneCandidate) (bool, error) {
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	r.mu.RUnlock()
	revision := candidate.revision
	if revision == 0 {
		judged, err := r.judgeGone(ctx, kv, candidate.sessionID)
		if err != nil || judged == 0 {
			return false, err
		}
		revision = judged
	}
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
	r.mu.Lock()
	r.cache.Evict(r.watcher, epoch, candidate.sessionID, revision+1)
	r.unlockAndNotify()
	return true, nil
}

// judgeGone reads sessionID's record from the bucket and answers the revision to delete it at
// while that record's own last registration still puts the session past the gone horizon
// (StateOf), and 0 when it does not. It is how a candidate the pass can name no revision for is
// judged a second time, immediately before the delete that acts on it: the first judgement was
// made on the copy's row, and nothing but a reading of the record the bucket holds now may take
// that record away.
//
// A key the bucket no longer holds, and a record a registration has renewed since, are both 0:
// the caller deletes nothing, counts nothing and logs nothing for them. A value this build cannot
// read is dated by nothing, and a record no reader can judge is not one this pass deletes: it is
// the error that reports the record kept.
func (r *SessionRegistry) judgeGone(ctx context.Context, kv bus.KeyValue, sessionID string) (uint64, error) {
	got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return 0, fmt.Errorf("read session %s's record before deleting it: %w", sessionID, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var stored SessionEntry
	if err := json.Unmarshal(got.Value(), &stored); err != nil {
		return 0, fmt.Errorf("judge session %s's record before deleting it: %w", sessionID, err)
	}
	if r.stateOf(stored, r.now()) != Gone {
		return 0, nil
	}
	return got.Revision(), nil
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
// subject that it stored after lastSeen. It seeks the first of them (storedWalk.firstAfter) and
// reads forward from there (bus.Client.NextStoredMessage), stopping where the stream holds no
// more (nats.ErrMsgNotFound).
//
// The seek is what spends the count's reads on the messages that answer the question. The stream
// stores in sequence order, so the messages stored after the registration are the ones from one
// sequence on, and the walk finds that sequence in a read per doubling stride rather than a read
// per message skipped: a session with a retention window of older messages on its subject would
// otherwise spend the whole cap reaching the first message that counts, and be reported as having
// had nothing stored for it when hundreds were waiting.
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
	walk := &storedWalk{
		reaper:   reaper,
		subject:  contracts.AgentSubject(sessionID),
		after:    time.UnixMilli(lastSeen),
		stream:   stream,
		deadline: deadline,
	}
	first, outcome := walk.firstAfter(ctx)
	if outcome != storedFound {
		return 0, true
	}
	for seq := first; seq <= stream.LastSeq; {
		stored, outcome := walk.next(ctx, seq)
		if outcome != storedFound {
			return count, outcome == storedStopped
		}
		count++
		seq = stored.Sequence + 1
	}
	return count, false
}

// storedWalk is one reaped session's walk of the notification stream: what it looks for, and what
// it has left to look with. The seek and the count share one walk, so the cap and the deadline
// bound the whole of that record's reading and not each phase of it.
type storedWalk struct {
	reaper   SessionReaper
	subject  string
	after    time.Time
	stream   nats.StreamState
	deadline time.Time
	reads    int
}

// storedOutcome is what one read of a walk's subject answered.
type storedOutcome uint8

const (
	// storedFound: a message on the subject at or after the sequence asked for, within the
	// sequence space the pass read for the stream.
	storedFound storedOutcome = iota
	// storedNone: the stream holds no such message, so there is nothing further out to find.
	storedNone
	// storedStopped: the read budget, the pass's deadline, ctx or a read that failed ended the
	// walk, so what it counted is a part of what was stored.
	storedStopped
)

// next reads the first message at or after seq on the walk's subject, and spends one of the
// walk's reads on it. Each read is held to the pass's own deadline: the pass carries no caller's
// deadline, and a stored-message read given none waits out its own 10 s (bus.Client's
// storedMessageWait), twice the budget of every count a pass makes.
//
// A message past the last sequence the pass read for the stream is one stored since that read,
// which this count does not reach for: it is the end of the walk, as no message at all is.
func (w *storedWalk) next(ctx context.Context, seq uint64) (bus.StoredMessage, storedOutcome) {
	if w.reads >= reapStoredMessageCap || time.Now().After(w.deadline) || ctx.Err() != nil {
		return bus.StoredMessage{}, storedStopped
	}
	w.reads++
	readCtx, cancel := context.WithDeadline(ctx, w.deadline)
	defer cancel()
	stored, err := w.reaper.Client.NextStoredMessage(readCtx, w.subject, seq)
	switch {
	case errors.Is(err, nats.ErrMsgNotFound):
		return bus.StoredMessage{}, storedNone
	case err != nil:
		return bus.StoredMessage{}, storedStopped
	case stored.Sequence > w.stream.LastSeq:
		return bus.StoredMessage{}, storedNone
	}
	return stored, storedFound
}

// firstAfter is the sequence to count the walk's subject from: the lowest one whose next message
// on that subject the stream stored after the session's last registration, which is the first
// message that counts. The stream stores in sequence order, so every message the walk reads from
// there on counts too.
//
// It steps out from the subject's first message in strides that double, each stride one read
// whatever it skips, until a read lands at or past the registration; then it halves the stride it
// overshot in until the two ends meet. A subject the stream holds nothing of past that point
// answers the sequence beyond its last message, which counts nothing: a read that found no
// message says only that there is none further out, never that the ones behind it were older than
// the registration.
func (w *storedWalk) firstAfter(ctx context.Context) (uint64, storedOutcome) {
	lo, stride := w.stream.FirstSeq, uint64(1)
	var hi uint64
	for hi == 0 {
		if lo > w.stream.LastSeq {
			return lo, storedFound
		}
		seq := w.stream.LastSeq
		if gap := w.stream.LastSeq - lo; stride-1 < gap {
			seq = lo + stride - 1
		}
		stored, outcome := w.next(ctx, seq)
		switch {
		case outcome == storedStopped:
			return 0, storedStopped
		case outcome == storedNone || stored.Time.After(w.after):
			if seq == lo {
				// The stride began where the window does: no message of the subject was
				// stepped over, so this is the first sequence that counts.
				return lo, storedFound
			}
			hi = seq
		default:
			// Older than the registration, and so is every message of the subject below it.
			lo = stored.Sequence + 1
			stride *= 2
		}
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		stored, outcome := w.next(ctx, mid)
		switch {
		case outcome == storedStopped:
			return 0, storedStopped
		case outcome == storedNone || stored.Time.After(w.after):
			hi = mid
		default:
			lo = stored.Sequence + 1
		}
	}
	return lo, storedFound
}
