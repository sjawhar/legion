package main

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logging"
)

// receiveTerm is one subscription's worth of receiving: the local in-order queue its core
// subscription fills, the messages received and not yet settled, and the serial handler's state.
type receiveTerm struct {
	// subject, sub and conn are guarded by durableReceiver.mu.
	subject string
	sub     *nats.Subscription
	conn    *nats.Conn

	queue chan *nats.Msg
	// handling is the context every message of the term is handled under; cancel ends it.
	handling context.Context
	cancel   context.CancelFunc
	// closing is closed once the handler takes no more messages (stopHandling), handled once the
	// handler has returned, and stopped once the term is over, which ends the InProgress ticker.
	closing     chan struct{}
	closingOnce sync.Once
	handled     chan struct{}
	stopped     chan struct{}
	logger      *logging.Logger
	// hold, when it is above zero, is the longest the handler holds what the term receives before
	// it delivers it in stream order (settle): a term that must settle. leaseTerm is the lease
	// term the hold's end reports settled, and whose clean-hand-back mark, when the term was
	// taken from a candidate for one, can end the hold sooner (readHandBackMark).
	// markReadFailed is that read's log-once state, the settle hold's own.
	hold           time.Duration
	leaseTerm      uint64
	markReadFailed bool

	mu sync.Mutex
	// closed is set once the term takes no more messages (close).
	closed bool
	// holding is set while the settle hold lasts.
	holding bool
	// held is the batch the hold gathered, sorted by stream sequence, which the handler delivers
	// before it reads the queue again.
	held      []*nats.Msg
	unsettled map[*nats.Msg]struct{}
	// handedBack is set once a graceful stop hands the term's messages back (handBack): how many
	// times it has NAKed each stream sequence with handBackDelay. naked counts the stream
	// sequences handed back, renaked the repeats NAKed again, and dropped the repeats past
	// handBackNaksMax.
	handedBack    map[uint64]int
	handBackDelay time.Duration
	naked         int
	renaked       int
	dropped       int
}

// intake is the core subscription's handler: it queues m, in the order the durable sent it, and
// counts it unsettled from then on, so the InProgress ticker keeps it alive while it waits. Once
// the term hands its messages back, it hands m back instead (handBackLocked).
func (t *receiveTerm) intake(m *nats.Msg) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.handedBack != nil {
		t.handBackLocked(m)
		return
	}
	select {
	case t.queue <- m:
		t.unsettled[m] = struct{}{}
	default:
		t.logger.Warn("durable message dropped: the local queue is full", slog.Int("capacity", cap(t.queue)))
	}
}

// stopHandling has the handler take no more messages; intake goes on taking what arrives.
func (t *receiveTerm) stopHandling() {
	t.closingOnce.Do(func() { close(t.closing) })
}

// close stops the term taking messages, and the handler with it.
func (t *receiveTerm) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.stopHandling()
}

// handBack NAKs with delay every message the term holds or has queued, as one batch in stream
// order, and from then on has intake hand back each message that arrives (handBackLocked).
func (t *receiveTerm) handBack(delay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	batch := t.takeQueuedLocked()
	sortByStreamSequence(batch)
	t.handedBack, t.handBackDelay = map[uint64]int{}, delay
	for _, m := range batch {
		t.handBackLocked(m)
	}
}

// handBackLocked NAKs m with the hand-back delay, up to handBackNaksMax times for one stream
// sequence: a repeat reaches this pod alone when the server redelivered it before the next holder
// subscribed, and NAKed again it comes back to that holder after the delay rather than at its ack
// wait, which the next holder's settle hold does not wait for. A repeat past that is dropped with
// neither ack nor NAK, so a message the hand-back keeps going round costs at most handBackNaksMax
// deliveries. A message whose JetStream metadata cannot be read is NAKed.
func (t *receiveTerm) handBackLocked(m *nats.Msg) {
	if meta, err := m.Metadata(); err == nil {
		naks := t.handedBack[meta.Sequence.Stream]
		if naks >= handBackNaksMax {
			t.dropped++
			return
		}
		t.handedBack[meta.Sequence.Stream] = naks + 1
		if naks > 0 {
			t.renaked++
		} else {
			t.naked++
		}
	} else {
		t.naked++
	}
	_ = m.NakWithDelay(t.handBackDelay)
}

// handBackCounts reports how many messages the term handed back, how many repeats it NAKed again
// and how many it dropped.
func (t *receiveTerm) handBackCounts() (naked, renaked, dropped int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.naked, t.renaked, t.dropped
}

// takeQueued empties the held batch and the queue and returns what they held, in the order the
// handler would have taken it, no longer counted unsettled: during a settle hold, all of it in
// stream order.
func (t *receiveTerm) takeQueued() []*nats.Msg {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.takeQueuedLocked()
}

func (t *receiveTerm) takeQueuedLocked() []*nats.Msg {
	if t.holding {
		t.gatherLocked()
	}
	queued := t.held
	t.held = nil
	for {
		select {
		case m := <-t.queue:
			queued = append(queued, m)
		default:
			for _, m := range queued {
				delete(t.unsettled, m)
			}
			return queued
		}
	}
}

func (t *receiveTerm) settled(m *nats.Msg) {
	t.mu.Lock()
	delete(t.unsettled, m)
	t.mu.Unlock()
}

// gather moves everything queued into the held batch, sorted by stream sequence, and returns how
// many distinct messages the batch holds; done ends the settle hold.
func (t *receiveTerm) gather(done bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if done {
		t.holding = false
	}
	return t.gatherLocked()
}

func (t *receiveTerm) gatherLocked() int {
	for {
		select {
		case m := <-t.queue:
			t.held = append(t.held, m)
		default:
			sortByStreamSequence(t.held)
			return distinctMessages(t.held)
		}
	}
}

// distinctMessages counts messages by stream sequence, so a message the durable redelivered while
// this pod already held it counts once; a message whose JetStream metadata cannot be read counts
// on its own.
func distinctMessages(messages []*nats.Msg) int {
	sequences := make(map[uint64]struct{}, len(messages))
	unreadable := 0
	for _, m := range messages {
		meta, err := m.Metadata()
		if err != nil {
			unreadable++
			continue
		}
		sequences[meta.Sequence.Stream] = struct{}{}
	}
	return len(sequences) + unreadable
}

// nextHeld takes the held batch's next message, or nil once it is empty.
func (t *receiveTerm) nextHeld() *nats.Msg {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.held) == 0 {
		return nil
	}
	m := t.held[0]
	t.held = t.held[1:]
	return m
}

// sortByStreamSequence orders messages by their stream sequence, in place and stably. A message
// whose JetStream metadata cannot be read keeps its position, and the others are ordered around it.
func sortByStreamSequence(messages []*nats.Msg) {
	var positions []int
	var readable []*nats.Msg
	sequences := map[*nats.Msg]uint64{}
	for index, m := range messages {
		meta, err := m.Metadata()
		if err != nil {
			continue
		}
		positions = append(positions, index)
		readable = append(readable, m)
		sequences[m] = meta.Sequence.Stream
	}
	slices.SortStableFunc(readable, func(a, b *nats.Msg) int { return cmp.Compare(sequences[a], sequences[b]) })
	for index, position := range positions {
		messages[position] = readable[index]
	}
}

// handle is the term's serial handler: it delivers each queued message in turn, while this pod
// holds the lease. A message received while it does not (the window before an unsubscribe lands,
// or a subscription nats.go sent again after a reconnect in place) is dropped with no ack and no
// NAK, so the durable redelivers it after the ack wait to whoever holds the lease then.
//
// A term that must settle first holds what it receives (settle), then delivers that batch in
// stream order before it reads the queue.
func (t *receiveTerm) handle(r *durableReceiver) {
	defer close(t.handled)
	if t.hold > 0 && !t.settle(r) {
		return
	}
	for {
		select {
		case <-t.closing:
			return
		default:
		}
		m := t.nextHeld()
		if m == nil {
			select {
			case <-t.closing:
				return
			case m = <-t.queue:
			}
		}
		if r.holding() {
			r.handle(t.handling, m)
		}
		t.settled(m)
	}
}

// settle is a term's settle hold. It delivers nothing while the InProgress ticker keeps what
// arrives alive, and reads the durable every settlePoll until its NumAckPending is no more than
// the distinct messages the term holds, which means every message outstanding is in this pod's
// hands: what a dead holder had received, or the server pushed to a lost node's lingering
// connection, comes back once its ack wait runs out, and what a stopping holder handed back once
// the hand-back delay does, after newer messages pushed to this pod, and the stream-order sort
// puts it back before them. The batch is gathered before each read, so every message it counts
// was still outstanding at the read. A read that fails keeps the hold. The hold ends at its cap
// regardless, and its end clears the lease term's unsettled and the ack wait it owed.
//
// A term taken from a hand-back candidate reads the stopping pod's mark beside the durable, and
// ends the hold once that mark is there and handBackSettleCap has passed since the subscribe
// (readHandBackMark): the mark says the stop NAKed what it held with the hand-back delay and
// held the deliver subject until this pod was receiving, so nothing of it is waiting out an ack
// wait. It comes after this pod is receiving, which is why the hold is where it is read and not
// the subscribe. A mark that is absent, or that cannot be read, leaves the hold to its cap.
//
// It reports false when the term closed first.
func (t *receiveTerm) settle(r *durableReceiver) bool {
	start := time.Now()
	ctx, cancel := context.WithCancel(t.handling)
	defer cancel()
	go func() {
		select {
		case <-t.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	limit := time.NewTimer(t.hold)
	defer limit.Stop()
	poll := time.NewTicker(r.timing.settlePoll)
	defer poll.Stop()
	outcome, pending, failing, marked := "cap", -1, false, false
read:
	for {
		held := t.gather(false)
		marked = marked || t.readHandBackMark(ctx, r)
		if marked && time.Since(start) >= r.timing.handBackSettleCap {
			outcome = "handed back"
			break read
		}
		info, err := listenerDurable(ctx, r.client, r.consumer)
		switch {
		case err == nil && info != nil:
			if failing {
				failing = false
				r.logger.Info("settle hold: durable read recovered", slog.String("consumer", r.consumer))
			}
			pending = info.NumAckPending
			if pending <= held {
				outcome = "settled"
				break read
			}
		case ctx.Err() == nil && !failing:
			failing = true
			reason := "the durable consumer does not exist"
			if err != nil {
				reason = err.Error()
			}
			r.logger.Warn("settle hold: durable read failed; holding on", slog.String("consumer", r.consumer), slog.String("error", reason))
		}
		select {
		case <-t.closing:
			return false
		case <-limit.C:
			break read
		case <-poll.C:
		}
	}
	held := t.gather(true)
	if r.lease != nil {
		r.lease.Settled(t.leaseTerm)
	}
	r.logger.Info("settle hold ended; delivering the held messages in stream order",
		slog.String("consumer", r.consumer),
		slog.String("outcome", outcome),
		slog.Int64("held_ms", time.Since(start).Milliseconds()),
		slog.Int("messages", held),
		slog.Int("ack_pending", pending),
		slog.Bool("hand_back_marked", marked),
	)
	return true
}

// readHandBackMark reports whether the pod that released the record this term was taken from has
// marked that hand-over clean (lease.HandBackMarked). The lease answers a term that is no
// candidate for one, or is no longer current, with false and no bucket call, so the settle hold
// of any term may ask. The mark also says that a binding this term subscribed beside was that
// stop's own linger, so the stale binding the term recorded is cleared with it. A read that
// fails is logged once, as a failed durable read is, and holds on.
func (t *receiveTerm) readHandBackMark(ctx context.Context, r *durableReceiver) bool {
	if r.lease == nil {
		return false
	}
	seen, err := r.lease.HandBackMarked(ctx, t.leaseTerm)
	if err != nil {
		if ctx.Err() == nil && !t.markReadFailed {
			t.markReadFailed = true
			r.logger.Warn("settle hold: hand-back mark read failed; holding on",
				slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		}
		return false
	}
	if !seen {
		return false
	}
	r.lease.SetStaleBinding(t.leaseTerm, false)
	return true
}

// keepAlive sends InProgress every interval for each message the term has received and not
// settled, until the term is over.
func (t *receiveTerm) keepAlive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopped:
			return
		case <-ticker.C:
		}
		t.mu.Lock()
		unsettled := make([]*nats.Msg, 0, len(t.unsettled))
		for m := range t.unsettled {
			unsettled = append(unsettled, m)
		}
		t.mu.Unlock()
		for _, m := range unsettled {
			_ = m.InProgress()
		}
	}
}
