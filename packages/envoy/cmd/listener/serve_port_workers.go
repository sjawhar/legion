package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

const (
	// servePortWorkerLimit bounds the serving-port sessions with a worker at once; a push for one
	// more session is handed back to the durable while the limit holds.
	servePortWorkerLimit = 1024
	// servePortQueueLimit bounds one session's pushes queued in its worker, its head included. Each
	// holds its durable message unacknowledged, and the durable's MaxAckPending is shared by every
	// session of the machine, so a push past it is handed back to the durable: one session whose
	// port never accepts holds at most this many of the durable's pending messages.
	servePortQueueLimit = 32
	// servePortProgressInterval is how often a worker's queued messages are kept in progress, well
	// inside the durable's AckWait (applyListenerConsumerPolicy, consumerAckWait: 6 s, short
	// because every holder keeps the messages it holds alive this way, so a dead holder's are
	// redelivered to the standby that takes the lease over quickly), so none is delivered again
	// while it waits for its port.
	servePortProgressInterval = 2 * time.Second
	// servePortTermStopPoll is how often the end of a receive term reads whether the workers have
	// left its pushes (servePortWorkers.endTerm), which its cancel ends within a round trip.
	servePortTermStopPoll = 5 * time.Millisecond
)

// durableSettle settles one durable message once its handler and every push the handler handed to
// a serving-port worker have ended: it acks the message, or NAKs it when any of them asked for a
// retry, after the longest delay any of them asked for (durableRetryDelay for end's retry). Every
// acknowledgement call on the message goes through it, under its lock, so the worker that keeps the
// message in progress never races the one that settles it.
type durableSettle struct {
	mu      sync.Mutex
	msg     *nats.Msg
	open    int
	retry   bool
	delay   time.Duration
	settled bool
}

func newDurableSettle(msg *nats.Msg) *durableSettle {
	return &durableSettle{msg: msg, open: 1}
}

// hold adds a party the message waits for: a push handed to a worker.
func (s *durableSettle) hold() {
	s.mu.Lock()
	s.open++
	s.mu.Unlock()
}

// end ends one party, asking for a retry after durableRetryDelay when retry is set; the last one
// settles the message.
func (s *durableSettle) end(retry bool) {
	if retry {
		s.retryAfter(durableRetryDelay)
		return
	}
	s.finish(false, 0)
}

// retryAfter ends one party, asking for a retry after delay; the last one settles the message.
func (s *durableSettle) retryAfter(delay time.Duration) {
	s.finish(true, delay)
}

func (s *durableSettle) finish(retry bool, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if retry {
		s.retry = true
		s.delay = max(s.delay, delay)
	}
	s.open--
	if s.open > 0 || s.settled {
		return
	}
	s.settled = true
	if s.retry {
		_ = s.msg.NakWithDelay(s.delay)
		return
	}
	_ = s.msg.Ack()
}

// progress restarts the message's ack wait while it is unsettled.
func (s *durableSettle) progress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settled {
		_ = s.msg.InProgress()
	}
}

// abandon ends this message's settling by the workers: the receive term that took it is over, so
// no push of it acks it, NAKs it or keeps it in progress any more, and the term's stop disposes
// of the message itself (servePortWorkers.endTerm). It returns the message, and nil when it is
// settled or abandoned already, so a message held for several sessions is disposed of once.
func (s *durableSettle) abandon() *nats.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return nil
	}
	s.settled = true
	return s.msg
}

// servePortPush is one durable message's push to one serving-port session, handed to the session's
// worker. forward marks the stored copy of a role forward, whose delivery the role lane reported.
// term is the receive term whose message this is: the handling context of that term
// (deliveryMessage.ctx), which the workers take as the term's identity, so the term's end stops
// its pushes and takes their messages back (servePortWorkers.endTerm). It is nil for a push of no
// term: the core and live lanes hand none over, and a rig that calls the durable's handler itself
// has no term behind it.
type servePortPush struct {
	item      contracts.Envelope
	sessionID string
	settle    *durableSettle
	// attempt is the pair's delivery-ledger attempt the path that handed this push over claimed;
	// the worker settles it with the push's outcome, so every pod of the machine reads what this
	// one delivered (delivery.go's ledgerAttempt).
	attempt dedupe.Attempt
	// storedSeq is the stream sequence of the message, which says whether the session's consumer
	// has come to hold it while this push waited (target).
	storedSeq uint64
	forward   bool
	// logLine is the line a delivered push logs, "" for none.
	logLine string
	term    context.Context
	timer   metrics.Timer
}

// storedSubject is the subject push's message is stored on, which with storedSeq says whether the
// session's consumer holds it (session.SessionEntry.ConsumerRelaysStored): the envelope's own
// topic, except for the stored copy of a role forward, which is stored on the holder's agent
// subject while keeping the role topic of the message it carries.
func (p servePortPush) storedSubject() string {
	if p.forward {
		return contracts.AgentSubject(p.sessionID)
	}
	return p.item.Topic
}

// servePortWorkers pushes the durable path's messages to serving-port sessions off the durable's
// handler: one FIFO worker per session with pushes queued, which pushes its head message until the
// port answers 204, with a wait that doubles from pushRetryFirst to pushRetryMax between tries, and
// only then takes the next. The handler hands a push over and returns, so a session slow to accept
// holds up no other session, and its own messages reach it in stream order; the message is
// acknowledged once its pushes are accepted (durableSettle) and kept in progress meanwhile, never
// NAKed back to the shared durable while its session can take it, since a later message would then
// pass it. A worker whose session is no longer a serving port this listener owns hands its pushes
// back to the durable, which delivers them again under the rules of the session's new state. A
// worker ends once its queue is empty.
//
// Every push is a message of the receive term whose handler handed it over (servePortPush.term),
// and ends with that term: the end of a term stops its pushes and takes their messages back
// (endTerm), so a pod that lost the lease pushes nothing more and a pod that stopped gracefully
// hands those messages to the next holder with the rest of what it held (receive_stop.go). A
// push handed over after its term ended is refused, and its message goes back to the durable.
type servePortWorkers struct {
	cfg    listenerDeliveryHandlerConfig
	ctx    context.Context
	cancel context.CancelFunc
	runs   sync.WaitGroup

	mu sync.Mutex
	// queues holds each session's pushes in arrival order, its head the one being pushed. A session
	// has a key exactly while its worker runs, so a queue the end of a term empties keeps its key
	// until that worker reads it empty: a second worker for one session would push its messages
	// out of order.
	queues map[string][]servePortPush
	// terms holds what the workers keep of each receive term with pushes here, by that term's
	// handling context (servePortPush.term); a push of no term has no entry.
	terms   map[context.Context]*servePortTerm
	stopped bool
}

// servePortTerm is what the workers hold of one receive term: the context its pushes are made
// under, which the term's end cancels (endTerm), and how many of its pushes a worker is inside
// right now, which that end waits for.
type servePortTerm struct {
	ctx    context.Context
	cancel context.CancelFunc
	// pushing counts the term's pushes a worker is inside (pushUntilSettled); ended is set once
	// the term's end took its pushes back, after which the workers take no push of it.
	pushing int
	ended   bool
}

// startServePortWorkers starts the workers' progress loop. cfg is the delivery configuration the
// pushes are made (deliverer, sessions, registry) and reported (applyDeliveryOutcome) through.
func startServePortWorkers(cfg listenerDeliveryHandlerConfig) *servePortWorkers {
	ctx, cancel := context.WithCancel(context.Background())
	w := &servePortWorkers{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		queues: map[string][]servePortPush{},
		terms:  map[context.Context]*servePortTerm{},
	}
	w.runs.Go(w.keepInProgress)
	return w
}

// enqueue queues push behind its session's earlier pushes, starting the session's worker when it
// has none, and holds push's message until the push ends. It reports false, holding nothing, when
// the workers stopped or are full, or when the receive term whose message this is has ended: the
// caller hands the message back to the durable.
func (w *servePortWorkers) enqueue(push servePortPush) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	queue, running := w.queues[push.sessionID]
	if w.stopped || (!running && len(w.queues) >= servePortWorkerLimit) || len(queue) >= servePortQueueLimit {
		return false
	}
	if !w.holdTermLocked(push.term) {
		return false
	}
	push.settle.hold()
	w.queues[push.sessionID] = append(queue, push)
	if !running {
		w.runs.Go(func() { w.work(push.sessionID) })
	}
	return true
}

// holdTermLocked reports whether the workers take a push of term, recording the term when they
// hold none of its pushes yet. A push of no term is always taken; one of a term that has ended,
// or whose handling is cancelled, is not: that term's messages are its stop's to dispose of
// (endTerm), so this one goes back to the durable instead of outliving the term.
func (w *servePortWorkers) holdTermLocked(term context.Context) bool {
	if term == nil {
		return true
	}
	if state, held := w.terms[term]; held {
		return !state.ended
	}
	if term.Err() != nil {
		return false
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.terms[term] = &servePortTerm{ctx: ctx, cancel: cancel}
	return true
}

// stop ends every worker within ctx, leaving their messages unsettled: the server delivers each
// again once its ack wait runs out.
func (w *servePortWorkers) stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.cancel()
	if !waitWithin(ctx, &w.runs) {
		w.cfg.logger.Warn("serving-port workers still running at shutdown")
	}
}

// endTerm takes back every push of term the workers hold: it stops those pushes and the keeping
// of their messages in progress, and returns the messages, which nothing here settles any more,
// for the term's stop to dispose of - a graceful stop NAKs them in its hand-back batch, and a
// lost lease leaves them to their ack wait (receive_stop.go). A push handed over after this is
// refused (enqueue), so the term's messages end with the term.
//
// idle reports whether every worker had left the term's pushes within wait: one still inside a
// push holds a message of the term that its stop cannot account for, so a graceful stop writes no
// clean-hand-back mark for it (durableReceiver.markHandBack).
func (w *servePortWorkers) endTerm(term context.Context, wait time.Duration) (parked []*nats.Msg, idle bool) {
	if w == nil || term == nil {
		return nil, true
	}
	w.mu.Lock()
	state := w.terms[term]
	if state == nil {
		w.mu.Unlock()
		return nil, true
	}
	state.ended = true
	for sessionID, queue := range w.queues {
		kept := queue[:0]
		for _, push := range queue {
			if push.term != term {
				kept = append(kept, push)
				continue
			}
			if msg := push.settle.abandon(); msg != nil {
				parked = append(parked, msg)
			}
		}
		clear(queue[len(kept):])
		w.queues[sessionID] = kept
	}
	w.mu.Unlock()
	state.cancel()
	idle = w.awaitTermIdle(state, wait)
	w.mu.Lock()
	delete(w.terms, term)
	w.mu.Unlock()
	return parked, idle
}

// awaitTermIdle waits for the workers to leave the term's pushes, for wait at most, and reports
// whether they did.
func (w *servePortWorkers) awaitTermIdle(state *servePortTerm, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		w.mu.Lock()
		pushing := state.pushing
		w.mu.Unlock()
		if pushing == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(servePortTermStopPoll)
	}
}

// work is sessionID's worker: it pushes the session's queued messages in order and ends once the
// queue is empty, or when the workers stop.
func (w *servePortWorkers) work(sessionID string) {
	for {
		head, ctx, left, ok := w.head(sessionID)
		if !ok {
			return
		}
		retry, ended := w.pushUntilSettled(ctx, head)
		left()
		if !ended {
			// The workers stopped, or the end of this push's term cancelled it. A push still at
			// the head is the stopped workers': it is left unsettled, for a later delivery of its
			// message. One the term's end took back is gone from the queue, and what a later term
			// queued behind it is this worker's to push.
			if !w.takenBack(sessionID, head) {
				return
			}
			continue
		}
		w.settleHead(sessionID, head, retry)
	}
}

// head takes sessionID's head push, the context it is pushed under - its term's, which the term's
// end cancels, or the workers' own for a push of no term - and the end of the count of its term's
// pushes in flight. ok is false once the session's queue is empty, which ends its worker.
func (w *servePortWorkers) head(sessionID string) (push servePortPush, ctx context.Context, left func(), ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	queue := w.queues[sessionID]
	if len(queue) == 0 {
		delete(w.queues, sessionID)
		return servePortPush{}, nil, nil, false
	}
	push = queue[0]
	state := w.terms[push.term]
	if state == nil {
		return push, w.ctx, func() {}, true
	}
	state.pushing++
	return push, state.ctx, func() {
		w.mu.Lock()
		state.pushing--
		w.mu.Unlock()
	}, true
}

// takenBack reports whether push is no longer its session's head: the end of its term took it
// back while the worker was pushing it (endTerm).
func (w *servePortWorkers) takenBack(sessionID string, push servePortPush) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	queue := w.queues[sessionID]
	return len(queue) == 0 || queue[0].settle != push.settle
}

// settleHead takes the ended push off its session's queue and ends its hold on the message,
// asking for a retry when the push did. A push the end of its term took back meanwhile is off the
// queue already and its message is that end's (endTerm), so the hold it ends there is no longer
// the message's.
func (w *servePortWorkers) settleHead(sessionID string, push servePortPush, retry bool) {
	w.mu.Lock()
	if queue := w.queues[sessionID]; len(queue) > 0 && queue[0].settle == push.settle {
		queue[0] = servePortPush{}
		w.queues[sessionID] = queue[1:]
	}
	w.mu.Unlock()
	push.settle.end(retry)
}

// pushRefusedLine is the line a push to a serving port logs when the port refused it and it waits
// to be pushed again. The durable path's worker (pushUntilSettled) and the relay of a session's
// consumer (sessionRelays.pushUntilAccepted) push the same way and log it alike.
const pushRefusedLine = "listener serving-port push refused; the message waits at the head of the session's queue"

// pushWithBackoff runs try until it reports the push settled, waiting pushRetryFirst between tries
// and doubling to pushRetryMax, and logging each refusal under pushRefusedLine. try reports the
// error the port, or the transport, refused the push with. It returns false when ctx ended before
// the push settled: the message is left unsettled, for a later delivery of it.
func pushWithBackoff(ctx context.Context, logger *logging.Logger, sessionID string, item contracts.Envelope, try func(attempt int) (settled bool, err error)) bool {
	wait := pushRetryFirst
	for attempt := 1; ; attempt++ {
		settled, err := try(attempt)
		if settled {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		logger.DeliveryLog(slog.LevelWarn, pushRefusedLine, sessionID, item.Topic, item.EventID, "retrying",
			slog.Int("attempt", attempt), slog.String("retry_in", wait.String()), slog.String("error", err.Error()))
		if waitUnlessDone(ctx, wait) != nil {
			return false
		}
		wait = min(wait*2, pushRetryMax)
	}
}

// pushUntilSettled pushes push under ctx until its port answers 204, or until its session is no
// longer a serving port this listener owns, and reports the outcome: whether the message needs a
// retry, and whether the push ended at all (false when the workers stopped, or the end of the
// push's term took it back, first). ctx is the push's term's, which that end cancels, or the
// workers' own for a push of no term (head).
func (w *servePortWorkers) pushUntilSettled(ctx context.Context, push servePortPush) (retry, ended bool) {
	cfg := w.cfg
	ended = pushWithBackoff(ctx, cfg.logger, push.sessionID, push.item, func(attempt int) (bool, error) {
		entry, verdict := w.target(push)
		switch verdict {
		case servePortConsumerRelays:
			// The session's consumer holds this message after all - its entry moved to consumer
			// delivery while the push waited - and the relay of that consumer delivers it. Giving
			// up the ledger attempt the path that handed this push over claimed is what tells that
			// relay so: it holds the message while the attempt stands and pushes it once the
			// attempt is gone (session_relay.go's holdWhilePushedElsewhere). Nothing is recorded
			// delivered: only a real push writes what the relay reads as a copy the session
			// already has (session.SessionEntry.ConsumerRelaysStored).
			applyDeliveryOutcome(cfg, push.item, deliveryOutcome{
				sessionID:    push.sessionID,
				attempt:      push.attempt,
				metricStatus: push.metricStatus("skipped"),
				settle:       settleClear,
			})
			return true, nil
		case servePortSkip:
			applyDeliveryOutcome(cfg, push.item, deliveryOutcome{
				sessionID:    push.sessionID,
				attempt:      push.attempt,
				metricStatus: push.metricStatus("skipped"),
				settle:       settleRecord,
			})
			return true, nil
		case servePortGone:
			// Back to the durable, which delivers the message again under the session's new state.
			// A redelivery the durable path then skips because the session's consumer holds the
			// message (session.SessionEntry.ConsumerRelaysStored) loses nothing: clearing the
			// attempt leaves the message to the relay of that consumer, which is holding it
			// unacked until exactly that record is gone (session_relay.go).
			applyDeliveryOutcome(cfg, push.item, deliveryOutcome{
				sessionID: push.sessionID,
				attempt:   push.attempt,
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener serving-port push handed back: the session is no longer a serving port this listener owns", push.sessionID, push.item.Topic, push.item.EventID, "retrying")
				},
				settle: settleClear,
			})
			cfg.messagesNAKed.Inc()
			retry = true
			return true, nil
		}
		if err := cfg.deliverer.Push(ctx, push.item, push.sessionID, entry); err != nil {
			return false, err
		}
		applyDeliveryOutcome(cfg, push.item, deliveryOutcome{
			sessionID:    push.sessionID,
			attempt:      push.attempt,
			metricStatus: push.metricStatus("delivered"),
			duration:     push.duration(),
			log: func(logger *logging.Logger) {
				if push.logLine != "" {
					logger.DeliveryLog(slog.LevelInfo, push.logLine, push.sessionID, push.item.Topic, push.item.EventID, "delivered", slog.Int("attempts", attempt))
				}
			},
			settle: settleRecord,
		})
		return true, nil
	})
	return retry, ended
}

// metricStatus is status for the delivery metrics, or "" for a role forward's stored copy, whose
// delivery the role lane counted.
func (p servePortPush) metricStatus(status string) string {
	if p.forward {
		return ""
	}
	return status
}

func (p servePortPush) duration() *metrics.Timer {
	if p.forward {
		return nil
	}
	return &p.timer
}

// servePortVerdict is what a worker does with its head push, read from the session's state at
// each try.
type servePortVerdict uint8

const (
	// servePortPushIt: the session is a serving port this listener owns; push to it.
	servePortPushIt servePortVerdict = iota
	// servePortConsumerRelays: the session's own consumer holds the message and its relay pushes
	// it; this push is dropped and nothing is recorded for it.
	servePortConsumerRelays
	// servePortSkip: the session takes the message without a push (it subscribes on NATS itself);
	// the message is delivered.
	servePortSkip
	// servePortGone: the session is gone, or no longer this listener's serving port; the message
	// goes back to the durable.
	servePortGone
)

// target reads push's session's current state: its entry, and what to do with the push. The
// verdicts are session.Deliverer.Resolve's, resolved again at each try because the entry may have
// moved since the path that handed the push over resolved it: a session whose consumer holds this
// stored message is relayed from there, one that subscribes on NATS itself with no port has it,
// and a message stored below a consumer's start is in no consumer, so this push is how the session
// gets it.
func (w *servePortWorkers) target(push servePortPush) (session.SessionEntry, servePortVerdict) {
	entry, err := w.cfg.sessions.Get(push.sessionID)
	if err != nil {
		return session.SessionEntry{}, servePortGone
	}
	interest, err := w.cfg.registry.Get(context.Background(), push.sessionID)
	if err != nil {
		interest = store.Interest{SessionID: push.sessionID}
	}
	// The copy the session's consumer may hold is this message as it is stored (storedSubject).
	// A session whose entry moved to consumer delivery under this push is relayed from there, and
	// the relay of that consumer holds the message unacked while this push is in flight rather
	// than acking it on the attempt the handing-over path recorded (session_relay.go's
	// holdWhilePushedElsewhere): dropping the push below hands the message to that relay rather
	// than losing it.
	result, err := w.cfg.deliverer.Resolve(push.item, interest, &entry, push.storedSubject(), push.storedSeq)
	switch {
	case err != nil:
		return entry, servePortGone
	case result.Relayed:
		return entry, servePortConsumerRelays
	case result.Push:
		return entry, servePortPushIt
	default:
		return entry, servePortSkip
	}
}

// ownsServingPortSession reports whether sessionID, of entry, is a serving-port session this
// listener delivers to (session.Deliverer.OwnsServingPort), read from its stored interest row when
// it has one.
func ownsServingPortSession(cfg listenerDeliveryHandlerConfig, sessionID string, entry session.SessionEntry) bool {
	if entry.Port <= 0 {
		return false
	}
	interest, err := cfg.registry.Get(context.Background(), sessionID)
	if err != nil {
		interest = store.Interest{SessionID: sessionID}
	}
	return cfg.deliverer.OwnsServingPort(interest, entry)
}

// keepInProgress keeps every queued message in progress every servePortProgressInterval until the
// workers stop.
func (w *servePortWorkers) keepInProgress() {
	ticker := time.NewTicker(servePortProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
		// One durable message's settle sits in the queue of every session it was pushed to; it is
		// kept in progress once.
		w.mu.Lock()
		settles := make(map[*durableSettle]struct{}, len(w.queues))
		for _, queue := range w.queues {
			for _, push := range queue {
				settles[push.settle] = struct{}{}
			}
		}
		w.mu.Unlock()
		for settle := range settles {
			settle.progress()
		}
	}
}
