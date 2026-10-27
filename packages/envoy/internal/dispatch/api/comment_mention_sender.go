package api

import (
	"context"
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
// past it is not queued: its attempt stays pending and unclaimed, and the recovery pass the
// sender runs once its queue empties sends it.
const maxQueuedMentionSends = 4096

// mentionDrainTimeout is how long the sends in flight when the server's Lifetime ends may run
// before their context is cancelled. It overlaps the HTTP and document drain inside the runtime's
// stop grace (cmd/dispatch/shutdown.go).
const mentionDrainTimeout = 10 * time.Second

// strandedMentionWindow is how far back the recovery pass looks for a stranded attempt. It covers
// a deploy or a crash loop; an older pending attempt has been in front of its author long enough
// that a late delivery is worse than the retry the author already has.
const strandedMentionWindow = "1 hour"

// strandedClaimMargin is how long after a claim lapses by the database's clock the recovery pass
// that waits for it runs, so the pass reads that claim as lapsed (claimLapsed).
const strandedClaimMargin = time.Second

// strandedRead is what one read of the recovery pass found.
type strandedRead struct {
	// sends are the attempts to resume, oldest first.
	sends []mentionSend
	// held reports whether a stranded attempt is held by a live claim taken before the read's
	// cutoff, and lapse is how long, by the database's clock, until the newest such claim lapses.
	held  bool
	lapse time.Duration
	// at is the database's clock when the read began.
	at time.Time
}

// mentionSend is one comment mention for the mention sender to deliver.
type mentionSend struct {
	comment model.Comment
	created model.Event
	mention ResolvedMention
	actor   model.Actor
}

// lane is the recipient a send is ordered by: the session the mention pinned when its lookup
// found one, so role:X and session:<X's holder> share one, and the target as written otherwise.
func (m mentionSend) lane() string {
	if m.mention.SessionID != nil {
		return "session:" + *m.mention.SessionID
	}
	return m.mention.Target
}

// mentionSender delivers comment mentions after the comment's response, one FIFO lane per
// recipient (mentionSend.lane), so each recipient gets the comments this process accepted in the
// order it accepted them. A lane is served by at most one goroutine and at most mentionSendLanes
// lanes send at once; a goroutine whose lane still has sends while another lane waits moves on to
// the waiting lane, so one busy recipient cannot hold the others back.
//
// Every database statement the sender makes goes through its two functions, send and stranded,
// which register binds to the server (sendCommentMention, strandedCommentMentions).
//
// Sends run on the server's Lifetime without its cancellation: once the Lifetime ends no queued
// send starts, and the sends in flight get mentionDrainTimeout before their context is
// cancelled. A send that never started leaves its attempt pending with no claim, for the next
// process's recovery pass. A process killed with sends in flight leaves their attempts pending
// with a live claim, which the next process's recovery pass sends once that claim lapses
// (recoverStrandedLocked). Order is kept within one process only: an attempt the recovery pass
// resumes can reach its recipient after comments accepted later.
type mentionSender struct {
	// lifetime ends when the server shuts down; ctx is what sends run on.
	lifetime context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	send     func(context.Context, mentionSend)
	stranded func(ctx context.Context, limit int, claimedBefore *time.Time) (strandedRead, error)
	// capacity is maxQueuedMentionSends; only a test lowers it.
	capacity int

	mu sync.Mutex
	// lanes holds each lane's sends that have not started. A key is present while its lane is
	// served or waiting in ready, even once its last send has started.
	lanes map[string][]mentionSend
	// ready is the lanes waiting for a goroutine, in the order they started waiting.
	ready []string
	// serving is how many goroutines serve lanes; queued counts the sends in lanes.
	serving int
	queued  int
	// recoverOnDrain is set when a send was refused for the bound, so the recovery pass runs once
	// the queue is empty.
	recoverOnDrain bool
	// started is the database's clock at the sender's first recovery read. A claim taken before it
	// is not this sender's: it belongs to a process that ran before this one, or one beside it.
	started *time.Time
	// lapsed is the recovery pass waiting for the newest claim from before started to lapse; nil
	// while none waits.
	lapsed *time.Timer
	// closed is set by wait; nothing starts after it.
	closed bool
	done   sync.WaitGroup
}

func newMentionSender(
	lifetime context.Context,
	send func(context.Context, mentionSend),
	stranded func(ctx context.Context, limit int, claimedBefore *time.Time) (strandedRead, error),
) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	return &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send, stranded: stranded,
		capacity: maxQueuedMentionSends, lanes: map[string][]mentionSend{},
	}
}

// stoppedLocked reports whether the sender starts nothing more: the Lifetime ended, or wait ran.
func (m *mentionSender) stoppedLocked() bool {
	return m.closed || m.lifetime.Err() != nil
}

// enqueue queues one send behind the earlier sends of its lane. A send refused - the sender is
// stopping, or the queue is full - leaves its attempt pending with no claim.
func (m *mentionSender) enqueue(item mentionSend) {
	m.mu.Lock()
	full := m.queueLocked(item)
	m.mu.Unlock()
	if full {
		slog.Warn("dispatch: comment mention queue full; the attempt stays pending until the queue drains",
			"comment", item.comment.ID, "target", item.mention.Target)
	}
}

// queueLocked queues item unless the sender stopped or the queue is full, which it reports, and
// starts a goroutine for its lane when the lane has none and fewer than mentionSendLanes run.
func (m *mentionSender) queueLocked(item mentionSend) (full bool) {
	if m.stoppedLocked() {
		return false
	}
	if m.queued >= m.capacity {
		m.recoverOnDrain = true
		return true
	}
	key := item.lane()
	waiting, scheduled := m.lanes[key]
	m.lanes[key] = append(waiting, item)
	m.queued++
	switch {
	case scheduled:
	case m.serving < mentionSendLanes:
		m.serving++
		m.done.Add(1)
		go m.serve(key)
	default:
		m.ready = append(m.ready, key)
	}
	return false
}

// serve sends lane's queue, then any lane waiting for a goroutine, until none waits.
func (m *mentionSender) serve(lane string) {
	defer m.done.Done()
	for {
		item, next, ok := m.next(lane)
		if !ok {
			return
		}
		lane = next
		m.send(m.ctx, item)
	}
}

// next takes the next send for the goroutine serving lane, and the lane it now serves. ok is
// false when the goroutine is done: the sender stopped, or no lane has a send left for it.
func (m *mentionSender) next(lane string) (mentionSend, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
			if m.queued == 0 && m.recoverOnDrain {
				m.recoverOnDrain = false
				m.recoverStrandedLocked()
			}
			return mentionSend{}, "", false
		}
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	item := m.lanes[lane][0]
	m.lanes[lane][0] = mentionSend{}
	m.lanes[lane] = m.lanes[lane][1:]
	m.queued--
	return item, lane, true
}

// recoverStranded runs the recovery pass: at start, and when the claims a pass waits for lapse
// (recoverStrandedLocked). It queues the stranded attempts (strandedCommentMentions), each sent
// through the claim protocol, so an attempt another sender holds is not sent twice.
func (m *mentionSender) recoverStranded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lapsed = nil
	if !m.stoppedLocked() {
		m.recoverStrandedLocked()
	}
}

// recoverStrandedLocked reads one stranded attempt past the queue's free room, so when more are
// stranded than fit, at least one is refused for the bound and the pass runs again once the queue
// drains (queueLocked).
//
// A stranded attempt held by a live claim taken before the sender's first read was claimed by a
// process that ran before this one - killed with that send in flight - or by one running beside
// it. The pass leaves it alone and runs once more when the newest such claim lapses, and that
// pass applies the same rule, so passes stop once no claim from before the first read holds a
// stranded attempt: a minute after start, unless a claim is moved, which only a live sender does.
// A claim this sender takes is never waited for, so a busy sender schedules nothing. A live
// process's attempt is not sent twice: its claim, once it lapses, is resumed through the claim
// protocol, and an attempt it settled is no longer pending. An attempt the killed process had
// already handed to the listener is resent under its own number and idempotency key (the
// comment, target and mode, deliverResolvedCommentMention), which the listener makes the
// envelope's dedupe key (agent.<session>.<key>, sendHandler in cmd/listener/api.go); the
// notification stream stores a repeat of a Dispatch key no second time and each host drops it
// (contracts.DedupeKeyNamesTheUpstreamEvent; DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts
// states for how long, and where that fails).
func (m *mentionSender) recoverStrandedLocked() {
	limit := max(m.capacity-m.queued, 0) + 1
	started := m.started
	m.done.Go(func() {
		read, err := m.stranded(m.ctx, limit, started)
		if err != nil {
			slog.Error("dispatch: read stranded comment mentions", "error", err)
			return
		}
		refused := 0
		m.mu.Lock()
		if m.started == nil {
			m.started = &read.at
		}
		for _, item := range read.sends {
			if m.queueLocked(item) {
				refused++
			}
		}
		if read.held && m.lapsed == nil && !m.stoppedLocked() {
			m.lapsed = time.AfterFunc(read.lapse+strandedClaimMargin, m.recoverStranded)
		}
		m.mu.Unlock()
		if refused > 0 {
			slog.Warn("dispatch: comment mention queue full; stranded attempts stay pending until the queue drains",
				"attempts", refused)
		}
	})
}

// wait stops the sender from starting anything more and waits for the sends in flight and a
// recovery pass that is running; a pass waiting for a claim to lapse never runs. Call it once the
// Lifetime has ended, before the pool closes.
func (m *mentionSender) wait() {
	m.mu.Lock()
	m.closed = true
	if m.lapsed != nil {
		m.lapsed.Stop()
	}
	m.mu.Unlock()
	m.done.Wait()
	m.cancel()
}

// sendCommentMention is the mention sender's send: deliverResolvedCommentMention on initial
// terms, so it claims the attempt its comment committed, or the stranded one the recovery pass
// found, rather than opening another. A failure is logged and leaves the attempt pending, as
// deliverBroadcast's does.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) {
	if _, err := s.deliverResolvedCommentMention(
		store.WithTransactionTracking(ctx), item.comment, item.created, item.mention, item.actor, true,
	); err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}

// strandedCommentMentions reads at most limit of the attempts the recovery pass resumes: pending,
// with a claim that lapsed or was never taken (claimLapsed), created within strandedMentionWindow,
// oldest first. Each is sent as its comment's author, the actor its comment.created event names,
// read once per comment; an attempt whose event cannot be read is logged and left for its
// author's retry.
//
// Beside them it reads whether an attempt in that window is held by a live claim taken before
// claimedBefore, or before the read when claimedBefore is nil, and when the newest such claim
// lapses. Postgres judges both against the claimed_at it wrote, as claimLapsed does. The held
// claims are read first: a claim that lapses between the two statements is then read as held, and
// the pass that waits for it finds it resumed or still stranded, rather than read by neither.
func (s *server) strandedCommentMentions(
	ctx context.Context, limit int, claimedBefore *time.Time,
) (strandedRead, error) {
	ctx = store.WithTransactionTracking(ctx)
	var result strandedRead
	var lapse *float64
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select now(), extract(epoch from max(claimed_at) + interval '`+claimLease+`' - now())::float8
		from comment_deliveries
		where state = 'pending' and not `+claimLapsed+` and claimed_at < coalesce($1, now())
		  and created_at > now() - interval '`+strandedMentionWindow+`'
	`, claimedBefore).Scan(&result.at, &lapse); err != nil {
		return strandedRead{}, err
	}
	if lapse != nil {
		result.held, result.lapse = true, time.Duration(*lapse*float64(time.Second))
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, c.issue_key, c.artifact_id::text, d.target, d.delivery, d.session_id
		from comment_deliveries d
		join comments c on c.id = d.comment_id
		where d.state = 'pending' and `+claimLapsed+`
		  and d.created_at > now() - interval '`+strandedMentionWindow+`'
		order by d.created_at, d.comment_id, d.target, d.attempt
		limit $1
	`, limit)
	if err != nil {
		return strandedRead{}, err
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionSend, error) {
		var item mentionSend
		err := row.Scan(
			&item.comment.ID, &item.comment.IssueKey, &item.comment.ArtifactID,
			&item.mention.Target, &item.mention.Delivery, &item.mention.SessionID,
		)
		return item, err
	})
	if err != nil {
		return strandedRead{}, err
	}
	type createdEvent struct {
		event model.Event
		err   error
	}
	events := make(map[string]createdEvent)
	result.sends = make([]mentionSend, 0, len(found))
	for _, item := range found {
		created, read := events[item.comment.ID]
		if !read {
			created.event, created.err = s.loadCommentCreatedEvent(ctx, s.deps.Store.Pool, item.comment)
			events[item.comment.ID] = created
		}
		if created.err != nil {
			slog.Error("dispatch: read the comment.created event of a stranded comment mention",
				"comment", item.comment.ID, "target", item.mention.Target, "error", created.err)
			continue
		}
		item.created, item.actor = created.event, created.event.Actor
		result.sends = append(result.sends, item)
	}
	return result, nil
}
