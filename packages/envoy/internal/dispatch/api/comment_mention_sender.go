package api

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds that have not started. A send
// past it is not queued: its attempt stays pending and unclaimed, and a stranded pass sends it once
// it has waited a claim lease and the queue has room (strandedCommentMentions).
const maxQueuedMentionSends = 4096

// mentionDrainTimeout is how long the sends in flight when the server's Lifetime ends may run
// before their context is cancelled. It overlaps the HTTP and document drain inside the runtime's
// stop grace (cmd/dispatch/shutdown.go).
const mentionDrainTimeout = 10 * time.Second

// strandedPassInterval is how often the mention sender runs its stranded pass after the one at
// start: half the claim lease (claimLease). An attempt left unclaimed is sent by whichever
// process's pass reads it first, within the lease and one interval of its creation, and one a
// killed process held within the lease and one interval of that process taking its claim.
const strandedPassInterval = 30 * time.Second

// mentionSend is one comment mention for the mention sender to deliver.
type mentionSend struct {
	comment model.Comment
	created model.Event
	mention ResolvedMention
	actor   model.Actor
	// attempt is the number of the comment_deliveries row the send was queued for: 1 for the
	// send its comment queues, the stranded row's for a send the stranded pass queues.
	attempt int
}

// mentionKey names one attempt: its comment, its target and its number.
type mentionKey struct {
	comment, target string
	attempt         int
}

func (m mentionSend) key() mentionKey {
	return mentionKey{comment: m.comment.ID, target: m.mention.Target, attempt: m.attempt}
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
// Every database statement the sender makes goes through its two functions, send and stranded,
// which register binds to the server (sendCommentMention, strandedCommentMentions).
//
// Sends run on the server's Lifetime without its cancellation: once the Lifetime ends nothing more
// is queued and no queued send starts, and the sends in flight get mentionDrainTimeout before their
// context is cancelled. A send refused once the sender stopped - a comment that commits during the
// HTTP drain - or for the queue's bound, or queued and never started, leaves its attempt pending
// with no claim. A process killed with sends in flight leaves their attempts pending with a claim
// that lapses a lease after it was taken. Every process's sender runs a stranded pass at start and
// every strandedPassInterval (recoverStranded), which sends each such attempt once it has waited a
// lease, whichever process left it: in a rolling update the successor starts before the old
// process is stopped, so only the successor's later passes find what the old process leaves.
// Order is kept within one process only: an attempt a pass resumes, after a crash, a handoff or a
// full queue, can reach its recipient after comments accepted later.
type mentionSender struct {
	// lifetime ends when the server shuts down; ctx is what sends run on.
	lifetime context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	send     func(context.Context, mentionSend)
	stranded func(ctx context.Context, limit int, held []mentionKey) ([]mentionSend, error)
	// interval is how often the stranded pass runs after the one at start.
	interval time.Duration
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
	// held is every attempt this sender queued whose send has not finished, so that neither a
	// stranded pass nor a comment queues one twice.
	held map[mentionKey]struct{}
	// closed is set by wait, which closes stop with it; nothing starts after it.
	closed bool
	stop   chan struct{}
	done   sync.WaitGroup
}

func newMentionSender(
	lifetime context.Context,
	interval time.Duration,
	send func(context.Context, mentionSend),
	stranded func(ctx context.Context, limit int, held []mentionKey) ([]mentionSend, error),
) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	return &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send, stranded: stranded, interval: interval,
		capacity: maxQueuedMentionSends, lanes: map[string][]mentionSend{}, held: map[mentionKey]struct{}{},
		stop: make(chan struct{}),
	}
}

// stoppedLocked reports whether the sender starts nothing more: the Lifetime ended, or wait ran.
func (m *mentionSender) stoppedLocked() bool {
	return m.closed || m.lifetime.Err() != nil
}

// start runs the stranded pass now and every interval after, until the sender stops.
func (m *mentionSender) start() {
	m.done.Go(func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			m.recoverStranded()
			select {
			case <-ticker.C:
			case <-m.stop:
				return
			case <-m.lifetime.Done():
				return
			}
		}
	})
}

// enqueue queues one send behind the earlier sends of its lane. A send refused - the sender has
// stopped, or the queue is full - leaves its attempt pending with no claim.
func (m *mentionSender) enqueue(item mentionSend) {
	m.mu.Lock()
	full := m.queueLocked(item)
	m.mu.Unlock()
	if full {
		slog.Warn("dispatch: comment mention queue full; a stranded pass sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
	}
}

// queueLocked queues item unless the sender stopped, it holds item's attempt already, or the queue
// is full, which it reports, and starts a goroutine for its lane when the lane has none and fewer
// than mentionSendLanes run. A send refused because the sender stopped is logged here.
func (m *mentionSender) queueLocked(item mentionSend) (full bool) {
	if m.stoppedLocked() {
		slog.Warn("dispatch: comment mention not sent: the mention sender has stopped; another process's stranded pass sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
		return false
	}
	key := item.key()
	if _, mine := m.held[key]; mine {
		return false
	}
	if m.queued >= m.capacity {
		return true
	}
	m.held[key] = struct{}{}
	lane := item.lane()
	waiting, scheduled := m.lanes[lane]
	m.lanes[lane] = append(waiting, item)
	m.queued++
	switch {
	case scheduled:
	case m.serving < mentionSendLanes:
		m.serving++
		m.done.Add(1)
		go m.serve(lane)
	default:
		m.ready = append(m.ready, lane)
	}
	return false
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
// serves. ok is false when the goroutine is done: the sender stopped, or no lane has a send left
// for it.
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
	m.queued--
	return item, lane, true
}

// recoverStranded is the stranded pass: it queues the stranded attempts strandedCommentMentions
// reads - pending, unclaimed or with a lapsed claim, a claim lease old or more and from the last
// model.StrandedMentionWindow, left by any process - except those this sender holds, so a busy sender
// never queues its own unstarted sends twice, at most as many as the queue has room for. Each is
// sent through the claim protocol, so an attempt another sender claimed or settled after the read
// is not sent twice. A pass once the sender has stopped, or with the queue full, does nothing.
//
// An attempt a killed process held is read once its claim lapses, by the first pass after that,
// within claimLease and strandedPassInterval of the claim. A live process's attempt is not sent
// twice: its claim, once it lapses, is resumed through the claim protocol, and an attempt it
// settled is no longer pending. An attempt the killed process had already handed to the listener
// is resent under its own number and idempotency key (the comment, target and mode,
// deliverResolvedCommentMention), which the listener makes the envelope's dedupe key
// (agent.<session>.<key>, sendHandler in cmd/listener/api.go); the notification stream stores a
// repeat of a Dispatch key no second time and each host drops it
// (contracts.DedupeKeyNamesTheUpstreamEvent; DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts
// states for how long, and where that fails).
//
// The outbox counts an attempt no sender ever claimed abandoned once it is a lease old, and
// publishes the mentioned session's own-topic copy in place of a mention it held that copy back
// for (mentionSendLapsed and attempt.leftTo in internal/dispatch/outbox). The two are exclusive
// per attempt, by its comment_deliveries row: whichever acts first settles it. The outbox settles
// the attempt failed under the comment's owner lock, which the claim transaction takes first too
// (claimCommentDeliveryAttempt), so a claim that runs after it finds the attempt settled and sends
// nothing, and a claim that commits first leaves it claimed, which the outbox does not count
// abandoned: it waits on the send instead. An attempt a sender claimed and never settled - a send
// in flight when its process was killed - is this pass's alone within model.StrandedMentionWindow:
// its resend repeats the attempt's idempotency key, so a frame the killed process had already
// handed to the listener is dropped there. The outbox holds the copy that mention's session would
// otherwise have had as a fallback row, which its fallback pass publishes once this resend fails
// and settles once it is sent (publishFallbackCopies in internal/dispatch/outbox, the protocol's
// one description), so a mentioned session loses no comment through a kill.
func (m *mentionSender) recoverStranded() {
	m.mu.Lock()
	if m.stoppedLocked() {
		m.mu.Unlock()
		return
	}
	limit := m.capacity - m.queued
	if limit <= 0 {
		m.mu.Unlock()
		return
	}
	held := slices.AppendSeq(make([]mentionKey, 0, len(m.held)), maps.Keys(m.held))
	m.mu.Unlock()
	sends, err := m.stranded(m.ctx, limit, held)
	if err != nil {
		slog.Error("dispatch: read stranded comment mentions", "error", err)
		return
	}
	refused := 0
	m.mu.Lock()
	if m.stoppedLocked() {
		m.mu.Unlock()
		return
	}
	for _, item := range sends {
		if m.queueLocked(item) {
			refused++
		}
	}
	m.mu.Unlock()
	if refused > 0 {
		slog.Warn("dispatch: comment mention queue full; a later stranded pass sends the attempts left pending",
			"attempts", refused)
	}
}

// wait stops the sender from starting anything more, ends its periodic stranded pass and waits for
// the sends in flight and a pass that is running. Call it once the Lifetime has ended, before the
// pool closes.
func (m *mentionSender) wait() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
	}
	m.mu.Unlock()
	m.done.Wait()
	m.cancel()
}

// sendCommentMention is the mention sender's send: deliverResolvedCommentMention on initial
// terms, so it claims the attempt its comment committed, or the stranded one the stranded pass
// found, rather than opening another. A failure is logged and leaves the attempt pending, as
// deliverBroadcast's does, for a later stranded pass once any claim it took lapses.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) {
	if _, err := s.deliverResolvedCommentMention(
		store.WithTransactionTracking(ctx), item.comment, item.created, item.mention, item.actor, true,
	); err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}

// strandedCommentMentions reads at most limit of the attempts the stranded pass resumes: pending,
// with a claim that lapsed or was never taken (claimLapsed), created at least claimLease ago and
// within model.StrandedMentionWindow, and not among held, oldest first. Postgres judges both times
// against the ones it wrote. An attempt younger than the lease is left to the process that
// accepted it, which queued it behind that recipient's earlier comments, so a pass never takes a
// live process's queue out of order; one that waited a lease unsent is stranded (the process
// stopped, or its queue stalls), and a claim, never older than its attempt, lapses no sooner.
// Each is sent as its comment's author, the actor its comment.created event names, read once per
// comment; an attempt whose event cannot be read is logged and left for its author's retry. An
// attempt the outbox settled abandoned is failed, not pending, so no pass reads it
// (recoverStranded).
//
// The read of the attempts goes through comment_deliveries_pending (migration 0136: created_at
// where state = 'pending'), so a pass with nothing stranded is that one statement and reads no
// row; otherwise it reads the rows of its window and each comment's comment.created event. It
// runs on a timer because no event announces a stranded attempt: a killed process sends no
// signal, and a stopping one refuses its sends after its Lifetime ends.
func (s *server) strandedCommentMentions(ctx context.Context, limit int, held []mentionKey) ([]mentionSend, error) {
	ctx = store.WithTransactionTracking(ctx)
	heldComments := make([]string, len(held))
	heldTargets := make([]string, len(held))
	heldAttempts := make([]int, len(held))
	for i, key := range held {
		heldComments[i], heldTargets[i], heldAttempts[i] = key.comment, key.target, key.attempt
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, c.issue_key, c.artifact_id::text, d.target, d.attempt, d.delivery, d.session_id
		from comment_deliveries d
		join comments c on c.id = d.comment_id
		where d.state = 'pending' and `+claimLapsed+`
		  and d.created_at > now() - `+model.StrandedMentionWindowInterval+`
		  and d.created_at < now() - interval '`+claimLease+`'
		  and (d.comment_id::text, d.target, d.attempt) not in (
		    select * from unnest($2::text[], $3::text[], $4::int[])
		  )
		order by d.created_at, d.comment_id, d.target, d.attempt
		limit $1
	`, limit, heldComments, heldTargets, heldAttempts)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionSend, error) {
		var item mentionSend
		err := row.Scan(
			&item.comment.ID, &item.comment.IssueKey, &item.comment.ArtifactID,
			&item.mention.Target, &item.attempt, &item.mention.Delivery, &item.mention.SessionID,
		)
		return item, err
	})
	if err != nil {
		return nil, err
	}
	type createdEvent struct {
		event model.Event
		err   error
	}
	events := make(map[string]createdEvent)
	sends := make([]mentionSend, 0, len(found))
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
		sends = append(sends, item)
	}
	return sends, nil
}
