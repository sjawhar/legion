package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds that have not started. A send
// past it is not queued: the claim its attempt committed with is given back, so the attempt is
// pending and unclaimed, and the delivery sweep sends it once it has waited a claim lease
// (resumeOwedDeliveries).
const maxQueuedMentionSends = 4096

// mentionClaimRefresh is how often the mention sender writes the claims of the sends waiting on
// its lanes again (refreshHeldClaims). It is well inside model.DeliveryClaimLapse, so a claim this
// sender holds never lapses while the process is alive however long its lane is held, and a
// process that stops or dies writes none again, so every claim it still held lapses within one
// lease and the delivery sweep takes those attempts.
const mentionClaimRefresh = model.DeliveryClaimLapse / 3

// mentionDrainTimeout is how long the sends in flight when the server's lifetime ends may run
// before their context is cancelled. It overlaps the HTTP and document drain inside the runtime's
// stop grace (cmd/dispatch/shutdown.go).
const mentionDrainTimeout = 10 * time.Second

// mentionReleaseTimeout bounds the statement the drain makes to give back the claims of the sends
// it never started (wait). It runs on a context the lifetime's end does not cancel, so a stop
// that takes longer than mentionDrainTimeout still leaves those attempts pending and unclaimed
// for the next delivery sweep pass, and it is short because the pool closes behind it.
const mentionReleaseTimeout = 5 * time.Second

// mentionSend is one comment mention for the mention sender to deliver.
type mentionSend struct {
	comment model.Comment
	created model.Event
	mention ResolvedMention
	actor   model.Actor
	// attempt is the number of the comment_deliveries row the send was queued for: 1 for the
	// send its comment queues, the owed row's for a send a keyed repeat of that comment queues.
	attempt int
	// claimed is the claim this process holds on that row: taken when the row was committed
	// (createCommentFor) or when a keyed repeat put an owed row back on a lane (replayComment),
	// written again while the send waits on its lane (refreshHeldClaims), given back when the
	// sender refuses the send (enqueue), and taken over by the send itself when its lane starts
	// it (claimCommentDeliveryAttempt).
	claimed time.Time
}

// mentionKey names one attempt: its comment, its target and its number.
type mentionKey struct {
	comment, target string
	attempt         int
}

func (m mentionSend) key() mentionKey {
	return mentionKey{comment: m.comment.ID, target: m.mention.Target, attempt: m.attempt}
}

// mentionClaim is one attempt the sender holds and the claim it last wrote on it.
type mentionClaim struct {
	key       mentionKey
	claimedAt time.Time
}

func (m mentionSend) claim() mentionClaim {
	return mentionClaim{key: m.key(), claimedAt: m.claimed}
}

// lane is the recipient a send is ordered by: the session the mention pinned when its lookup
// found one, and the target as written otherwise. role:X shares session:<X's holder>'s lane only
// when its lookup found the holder; an unanswered role:X is keyed by its string, so two comments
// to one recipient, one naming it by role and one by session, can then arrive in either order.
func (m mentionSend) lane() string {
	if m.mention.SessionID != nil {
		return "session:" + *m.mention.SessionID
	}
	return m.mention.Target
}

// mentionSender delivers comment mentions after the comment's response, one FIFO lane per
// recipient (mentionSend.lane), so each recipient gets the comments this process accepted on one
// lane in the order it accepted them. role:X shares its holder's lane only when its lookup found
// the holder; an unanswered role:X is keyed by its string, so two comments to one recipient can
// then arrive in either order. A lane is served by at most one goroutine and at most
// mentionSendLanes lanes send at once; a goroutine whose lane still has sends while another lane
// waits moves on to the waiting lane, so one busy recipient cannot hold the others back.
//
// A queued send holds its attempt's claim, so nothing else sends it: the attempt is committed
// claimed (createCommentFor), this sender writes that claim again every mentionClaimRefresh while
// the send waits (refreshHeldClaims), and the lane's own send takes it over
// (claimCommentDeliveryAttempt, which accepts this sender's live claim and refuses another
// sender's). That is what keeps a held lane in order: however long a recipient's first send
// takes, the comments queued behind it are not owed to the delivery sweep, which would otherwise
// send them past it (resumeOwedDeliveries). The outbox counts each of them live for the first
// model.DeliveryClaimLapse of its attempt's life, holding the mentioned session's copy back; past
// that lease it counts the attempt resumed (model.MentionResumedSQL) and keeps that copy as a
// comment_fallback_copies row, which the outbox's fallback pass settles with nothing published
// once the lane's send lands, and publishes should that send fail (publishFallbackCopies in
// internal/dispatch/outbox).
//
// The statements the sender makes go through its three functions, which register binds to the
// server: send (sendCommentMention), refresh (refreshMentionClaims) and release
// (releaseMentionClaims).
//
// Sends run on the server's lifetime without its cancellation: once the lifetime ends nothing more
// is queued, no queued send starts and no claim is written again, and the sends in flight get
// mentionDrainTimeout before their context is cancelled. A send refused once the sender stopped -
// a comment that commits during the HTTP drain - or for the queue's bound gives its attempt's
// claim back, and so does the drain for every send it never started (wait), leaving each attempt
// pending and unclaimed. A send in flight when the process is killed, and a queued one a kill
// leaves no drain for, leave their attempt pending under a claim nothing writes again, which
// lapses a lease after it was last written. Every such attempt is owed, and the
// delivery sweep sends it once its claim has lapsed and it has waited a lease, whichever process
// left it (delivery_sweep.go). Order is kept within one process only: an attempt the sweep
// resumes, after a crash, a handoff or a full queue, can reach its recipient after comments
// accepted later.
type mentionSender struct {
	// lifetime ends when the server shuts down; ctx is what sends run on.
	lifetime context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	send     func(context.Context, mentionSend)
	// refresh writes the claims of the sends that have not started again and answers what it
	// wrote; release gives claims back.
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error)
	release func(context.Context, []mentionClaim) error
	// capacity is maxQueuedMentionSends; only a test lowers it.
	capacity int
	// claimRefresh is how often keepClaims writes the claims of the queued sends again:
	// mentionClaimRefresh, which only a test shortens.
	claimRefresh time.Duration

	mu sync.Mutex
	// lanes holds each lane's sends that have not started. A key is present while its lane is
	// served or waiting in ready, even once its last send has started.
	lanes map[string][]mentionSend
	// ready is the lanes waiting for a goroutine, in the order they started waiting.
	ready []string
	// serving is how many goroutines serve lanes.
	serving int
	// claims is the claim this sender holds on each send that has not started, as it last wrote
	// it, so one statement writes them all again and no statement of its own ever touches the
	// claim a send has taken over. Its size is how many sends are queued, which capacity bounds.
	claims map[mentionKey]time.Time
	// held is every attempt this sender queued whose send has not finished, so that one attempt is
	// never queued twice: a keyed repeat of a comment answers while its first send is still queued.
	held map[mentionKey]struct{}
	// closed is set by wait; nothing starts after it, and stopped is closed with it.
	closed  bool
	stopped chan struct{}
	done    sync.WaitGroup
}

// startMentionSender builds the server's mention sender (register), which sends on the server's
// lifetime and drains for at most mentionDrainTimeout once it ends. An attempt no sender reaches
// is the delivery sweep's (resumeOwedDeliveries).
func (s *server) startMentionSender() *mentionSender {
	return newMentionSender(
		s.lifetime, mentionClaimRefresh, s.sendCommentMention, s.refreshMentionClaims, s.releaseMentionClaims)
}

func newMentionSender(
	lifetime context.Context,
	claimRefresh time.Duration,
	send func(context.Context, mentionSend),
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error),
	release func(context.Context, []mentionClaim) error,
) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	m := &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send, refresh: refresh, release: release,
		capacity: maxQueuedMentionSends, claimRefresh: claimRefresh, lanes: map[string][]mentionSend{},
		claims: map[mentionKey]time.Time{}, held: map[mentionKey]struct{}{}, stopped: make(chan struct{}),
	}
	m.done.Add(1)
	go m.keepClaims()
	return m
}

// stoppedLocked reports whether the sender starts nothing more: the lifetime ended, or wait ran.
func (m *mentionSender) stoppedLocked() bool {
	return m.closed || m.lifetime.Err() != nil
}

// enqueue queues one send behind the earlier sends of its lane. A send refused - the sender has
// stopped, or the queue is full - gives its attempt's claim back, so the attempt is pending with
// no claim, as it is when no sender ever held it.
func (m *mentionSender) enqueue(ctx context.Context, item mentionSend) {
	m.mu.Lock()
	queued, full := m.queueLocked(item)
	m.mu.Unlock()
	if full {
		slog.Warn("dispatch: comment mention queue full; the delivery sweep sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
	}
	if queued {
		return
	}
	if err := m.release(ctx, []mentionClaim{item.claim()}); err != nil {
		slog.Error("dispatch: give back the claim of a comment mention this sender refused",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}

// queueLocked queues item unless the sender stopped or the queue is full, which it reports, and
// starts a goroutine for its lane when the lane has none and fewer than mentionSendLanes run. It
// reports whether this sender holds item's attempt once it returns: it queued it, or it holds it
// already, a keyed repeat answering while the first send is still queued, whose fresh claim this
// sender takes over for it. A send refused because the sender stopped is logged here.
func (m *mentionSender) queueLocked(item mentionSend) (queued, full bool) {
	if m.stoppedLocked() {
		slog.Warn("dispatch: comment mention not sent: the mention sender has stopped; the delivery sweep sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
		return false, false
	}
	key := item.key()
	if _, mine := m.held[key]; mine {
		// A keyed repeat claimed the attempt anew before it asked (claimQueuedMention), so the
		// claim its send waits under is the one that repeat just wrote, and the refreshes and the
		// drain's release move that one. A send a lane has already started is not among the claims
		// this sender writes: its claim is the send's own, and nothing here touches it.
		if _, waiting := m.claims[key]; waiting {
			m.claims[key] = item.claimed
		}
		return true, false
	}
	if len(m.claims) >= m.capacity {
		return false, true
	}
	m.held[key] = struct{}{}
	m.claims[key] = item.claimed
	lane := item.lane()
	waiting, scheduled := m.lanes[lane]
	m.lanes[lane] = append(waiting, item)
	switch {
	case scheduled:
	case m.serving < mentionSendLanes:
		m.serving++
		m.done.Add(1)
		go m.serve(lane)
	default:
		m.ready = append(m.ready, lane)
	}
	return true, false
}

// serve sends lane's queue, then any lane waiting for a goroutine, until none waits.
func (m *mentionSender) serve(lane string) {
	defer m.done.Done()
	var finished mentionKey
	for {
		item, next, ok := m.next(lane, finished)
		if !ok {
			return
		}
		lane, finished = next, item.key()
		m.send(m.ctx, item)
	}
}

// next releases finished, the attempt whose send the goroutine serving lane has just finished (the
// zero key when it has finished none), and takes that goroutine's next send and the lane it now
// serves. The send it takes leaves the claims this sender writes again: from here its attempt's
// claim is the send's own. ok is false when the goroutine is done: the sender stopped, or no lane
// has a send left for it.
func (m *mentionSender) next(lane string, finished mentionKey) (mentionSend, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, finished)
	if m.stoppedLocked() {
		m.serving--
		return mentionSend{}, "", false
	}
	if len(m.lanes[lane]) > 0 && len(m.ready) > 0 {
		m.ready = append(m.ready, lane)
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	for len(m.lanes[lane]) == 0 {
		delete(m.lanes, lane)
		if len(m.ready) == 0 {
			m.serving--
			return mentionSend{}, "", false
		}
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	item := m.lanes[lane][0]
	m.lanes[lane][0] = mentionSend{}
	m.lanes[lane] = m.lanes[lane][1:]
	item.claimed = m.claims[item.key()]
	delete(m.claims, item.key())
	return item, lane, true
}

// keepClaims writes the claims of the sends waiting on the lanes again every claimRefresh
// (mentionClaimRefresh), until the lifetime ends or wait runs. Nothing is written again after
// that, which is what hands a stopped process's queue to the delivery sweep a lease later.
func (m *mentionSender) keepClaims() {
	defer m.done.Done()
	refresh := time.NewTicker(m.claimRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-m.stopped:
			return
		case <-refresh.C:
			m.refreshHeldClaims(m.ctx)
		}
	}
}

// refreshHeldClaims writes the claim of every send that has not started again, in one statement,
// and records what that statement wrote: the next refresh and the lane's own send both read the
// claim this sender last took. A send the lanes have handed out is not in the snapshot, and the
// statement moves only a row that still carries the claim this sender wrote, so a refresh can
// never move the claim a starting send has just taken out from under that send's settle.
func (m *mentionSender) refreshHeldClaims(ctx context.Context) {
	m.mu.Lock()
	queued := make([]mentionClaim, 0, len(m.claims))
	for key, claimed := range m.claims {
		queued = append(queued, mentionClaim{key: key, claimedAt: claimed})
	}
	m.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	refreshed, err := m.refresh(ctx, queued)
	if err != nil {
		slog.Error("dispatch: write the claims of the queued comment mentions again",
			"attempts", len(queued), "error", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, claim := range refreshed {
		if _, waiting := m.claims[claim.key]; waiting {
			m.claims[claim.key] = claim.claimedAt
		}
	}
}

// wait stops the sender from starting anything more, waits for the sends in flight, and gives
// back the claims of the sends that never started: a process that stops knows it will send none
// of them, so their attempts are left pending and unclaimed and the next delivery sweep pass
// sends them once they have waited a lease, rather than a lease after this process last wrote
// their claims. That release runs on a context of its own, which the lifetime's end does not
// cancel and mentionReleaseTimeout bounds, so a stop that takes longer than mentionDrainTimeout
// still gives the claims back rather than failing cancelled. Call it once the lifetime has ended,
// before the pool closes.
func (m *mentionSender) wait() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stopped)
	}
	m.mu.Unlock()
	m.done.Wait()
	// No goroutine serves a lane now and nothing more is queued, so what is left is final.
	m.mu.Lock()
	unstarted := make([]mentionClaim, 0, len(m.claims))
	for key, claimed := range m.claims {
		unstarted = append(unstarted, mentionClaim{key: key, claimedAt: claimed})
	}
	clear(m.claims)
	m.mu.Unlock()
	if len(unstarted) > 0 {
		release, cancelRelease := context.WithTimeout(
			context.WithoutCancel(m.lifetime), mentionReleaseTimeout)
		defer cancelRelease()
		if err := m.release(release, unstarted); err != nil {
			slog.Error("dispatch: give back the claims of the comment mentions this sender never started",
				"attempts", len(unstarted), "error", err)
		}
	}
	m.cancel()
}

// sendCommentMention is the mention sender's send: deliverResolvedCommentMention bound to the
// attempt the send was queued for and to the claim this sender holds on it, so it takes that
// attempt over rather than opening another. A failure is logged and leaves the attempt pending, as
// deliverBroadcast's does, for the delivery sweep once the claim it took lapses.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) {
	if _, err := s.deliverResolvedCommentMention(
		store.WithTransactionTracking(ctx), item.comment, item.created, item.mention, item.actor,
		attemptClaim{attempt: item.attempt, claimedAt: item.claimed},
	); err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}

// mentionClaimArrays is claims as the four arrays the sender's statements match a comment_deliveries
// row against: its comment, target and attempt, and the claim this sender wrote on it.
func mentionClaimArrays(claims []mentionClaim) (comments, targets []string, attempts []int32, claimed []time.Time) {
	comments = make([]string, len(claims))
	targets = make([]string, len(claims))
	attempts = make([]int32, len(claims))
	claimed = make([]time.Time, len(claims))
	for i, claim := range claims {
		comments[i], targets[i] = claim.key.comment, claim.key.target
		attempts[i], claimed[i] = int32(claim.key.attempt), claim.claimedAt
	}
	return comments, targets, attempts, claimed
}

// refreshMentionClaims is the mention sender's refresh: one statement writes the claim of every
// attempt whose send is still waiting on a lane, and answers the claims it wrote. A row that no
// longer carries the claim this sender wrote is left alone and not answered - its send has taken
// it over, or another sender has, or it has settled - so refreshHeldClaims leaves that send's
// recorded claim as it was, and every later statement of this sender's, the drain's release
// included, passes over the row too, since each is scoped to the claim it holds.
func (s *server) refreshMentionClaims(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(queued)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		update comment_deliveries d set claimed_at = now()
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as queued(comment_id, target, attempt, claimed_at)
		where d.comment_id = queued.comment_id and d.target = queued.target and d.attempt = queued.attempt
		  and d.state = 'pending' and d.claimed_at = queued.claimed_at
		returning d.comment_id::text, d.target, d.attempt, d.claimed_at
	`, comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionClaim, error) {
		var claim mentionClaim
		err := row.Scan(&claim.key.comment, &claim.key.target, &claim.key.attempt, &claim.claimedAt)
		return claim, err
	})
}

// releaseMentionClaims is the mention sender's release: one statement gives back the claims the
// attempts were committed with, so an attempt no lane took, or none started, is pending and
// unclaimed. Each is scoped to the claim this sender wrote, so an attempt another sender has
// since taken keeps that sender's claim.
func (s *server) releaseMentionClaims(ctx context.Context, held []mentionClaim) error {
	comments, targets, attempts, claimed := mentionClaimArrays(held)
	_, err := s.deps.Store.Pool.Exec(ctx, `
		update comment_deliveries d set claimed_at = null
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as held(comment_id, target, attempt, claimed_at)
		where d.comment_id = held.comment_id and d.target = held.target and d.attempt = held.attempt
		  and d.state = 'pending' and d.claimed_at = held.claimed_at
	`, comments, targets, attempts, claimed)
	return err
}

// claimQueuedMention takes the claim on an owed attempt a keyed repeat is about to put back on a
// lane (replayComment), so the delivery sweep leaves it to this process exactly as it leaves the
// attempt a comment of this process's own queued. ok is false when another sender holds it: that
// sender owns the send, and nothing is queued for it here.
func (s *server) claimQueuedMention(ctx context.Context, key mentionKey) (claimed time.Time, ok bool, err error) {
	err = s.deps.Store.Pool.QueryRow(ctx, `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and `+model.DeliveryClaimFreeSQL+`
		returning claimed_at
	`, key.comment, key.target, key.attempt).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return claimed, true, nil
}
