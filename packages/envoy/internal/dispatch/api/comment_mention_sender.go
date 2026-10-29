package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds that have not started. A send
// past it is not queued: its attempt stays pending and unclaimed, and the delivery sweep sends it
// once it has waited a claim lease (resumeOwedDeliveries).
const maxQueuedMentionSends = 4096

// mentionDrainTimeout is how long the sends in flight when the server's lifetime ends may run
// before their context is cancelled. It overlaps the HTTP and document drain inside the runtime's
// stop grace (cmd/dispatch/shutdown.go).
const mentionDrainTimeout = 10 * time.Second

// mentionSend is one comment mention for the mention sender to deliver.
type mentionSend struct {
	comment model.Comment
	created model.Event
	mention ResolvedMention
	actor   model.Actor
	// attempt is the number of the comment_deliveries row the send was queued for: 1 for the
	// send its comment queues, the owed row's for a send a keyed repeat of that comment queues.
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
// Every database statement the sender makes goes through its one function, send, which register
// binds to the server (sendCommentMention).
//
// Sends run on the server's lifetime without its cancellation: once the lifetime ends nothing more
// is queued and no queued send starts, and the sends in flight get mentionDrainTimeout before their
// context is cancelled. A send refused once the sender stopped - a comment that commits during the
// HTTP drain - or for the queue's bound, or queued and never started, leaves its attempt pending
// with no claim. A process killed with sends in flight leaves their attempts pending with a claim
// that lapses a lease after it was taken. Every such attempt is owed, and the delivery sweep sends
// it once its claim has lapsed and it has waited a lease, whichever process left it
// (delivery_sweep.go). Order is kept within one process only: an attempt the sweep resumes, after
// a crash, a handoff or a full queue, can reach its recipient after comments accepted later.
type mentionSender struct {
	// lifetime ends when the server shuts down; ctx is what sends run on.
	lifetime context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	send     func(context.Context, mentionSend)
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
	// held is every attempt this sender queued whose send has not finished, so that one attempt is
	// never queued twice: a keyed repeat of a comment answers while its first send is still queued.
	held map[mentionKey]struct{}
	// closed is set by wait; nothing starts after it.
	closed bool
	done   sync.WaitGroup
}

// startMentionSender builds the server's mention sender (register), which sends on the server's
// lifetime and drains for at most mentionDrainTimeout once it ends. An attempt no sender reaches
// is the delivery sweep's (resumeOwedDeliveries).
func (s *server) startMentionSender() *mentionSender {
	return newMentionSender(s.lifetime, s.sendCommentMention)
}

func newMentionSender(lifetime context.Context, send func(context.Context, mentionSend)) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	return &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send,
		capacity: maxQueuedMentionSends, lanes: map[string][]mentionSend{}, held: map[mentionKey]struct{}{},
	}
}

// stoppedLocked reports whether the sender starts nothing more: the lifetime ended, or wait ran.
func (m *mentionSender) stoppedLocked() bool {
	return m.closed || m.lifetime.Err() != nil
}

// enqueue queues one send behind the earlier sends of its lane. A send refused - the sender has
// stopped, or the queue is full - leaves its attempt pending with no claim.
func (m *mentionSender) enqueue(item mentionSend) {
	m.mu.Lock()
	full := m.queueLocked(item)
	m.mu.Unlock()
	if full {
		slog.Warn("dispatch: comment mention queue full; the delivery sweep sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
	}
}

// queueLocked queues item unless the sender stopped, it holds item's attempt already, or the queue
// is full, which it reports, and starts a goroutine for its lane when the lane has none and fewer
// than mentionSendLanes run. A send refused because the sender stopped is logged here.
func (m *mentionSender) queueLocked(item mentionSend) (full bool) {
	if m.stoppedLocked() {
		slog.Warn("dispatch: comment mention not sent: the mention sender has stopped; the delivery sweep sends the pending attempt",
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

// wait stops the sender from starting anything more and waits for the sends in flight. Call it
// once the lifetime has ended, before the pool closes.
func (m *mentionSender) wait() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.done.Wait()
	m.cancel()
}

// sendCommentMention is the mention sender's send: deliverResolvedCommentMention bound to the
// attempt the send was queued for, so it claims that attempt rather than opening another. A
// failure is logged and leaves the attempt pending, as deliverBroadcast's does, for the delivery
// sweep once any claim it took lapses.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) {
	if _, err := s.deliverResolvedCommentMention(
		store.WithTransactionTracking(ctx), item.comment, item.created, item.mention, item.actor, item.attempt,
	); err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}
