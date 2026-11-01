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

// servePortPush is one durable message's push to one serving-port session, handed to the session's
// worker. forward marks the stored copy of a role forward, whose delivery the role lane reported.
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
type servePortWorkers struct {
	cfg    listenerDeliveryHandlerConfig
	ctx    context.Context
	cancel context.CancelFunc
	runs   sync.WaitGroup

	mu sync.Mutex
	// queues holds each session's pushes in arrival order, its head the one being pushed. A session
	// has a key exactly while its worker runs.
	queues  map[string][]servePortPush
	stopped bool
}

// startServePortWorkers starts the workers' progress loop. cfg is the delivery configuration the
// pushes are made (deliverer, sessions, registry) and reported (applyDeliveryOutcome) through.
func startServePortWorkers(cfg listenerDeliveryHandlerConfig) *servePortWorkers {
	ctx, cancel := context.WithCancel(context.Background())
	w := &servePortWorkers{cfg: cfg, ctx: ctx, cancel: cancel, queues: map[string][]servePortPush{}}
	w.runs.Go(w.keepInProgress)
	return w
}

// enqueue queues push behind its session's earlier pushes, starting the session's worker when it
// has none, and holds push's message until the push ends. It reports false, holding nothing, when
// the workers stopped or are full: the caller hands the message back to the durable.
func (w *servePortWorkers) enqueue(push servePortPush) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	queue, running := w.queues[push.sessionID]
	if w.stopped || (!running && len(w.queues) >= servePortWorkerLimit) || len(queue) >= servePortQueueLimit {
		return false
	}
	push.settle.hold()
	w.queues[push.sessionID] = append(queue, push)
	if !running {
		w.runs.Go(func() { w.work(push.sessionID) })
	}
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

// work is sessionID's worker: it pushes the session's queued messages in order and ends once the
// queue is empty, or when the workers stop.
func (w *servePortWorkers) work(sessionID string) {
	for {
		w.mu.Lock()
		queue := w.queues[sessionID]
		if len(queue) == 0 {
			delete(w.queues, sessionID)
			w.mu.Unlock()
			return
		}
		head := queue[0]
		w.mu.Unlock()

		retry, ended := w.pushUntilSettled(head)
		if !ended {
			return
		}
		w.mu.Lock()
		queue = w.queues[sessionID]
		queue[0] = servePortPush{}
		w.queues[sessionID] = queue[1:]
		w.mu.Unlock()
		head.settle.end(retry)
	}
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

// pushUntilSettled pushes push until its port answers 204, or until its session is no longer a
// serving port this listener owns, and reports the outcome: whether the message needs a retry, and
// whether the push ended at all (false when the workers stopped first).
func (w *servePortWorkers) pushUntilSettled(push servePortPush) (retry, ended bool) {
	cfg := w.cfg
	ended = pushWithBackoff(w.ctx, cfg.logger, push.sessionID, push.item, func(attempt int) (bool, error) {
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
		if err := cfg.deliverer.Push(w.ctx, push.item, push.sessionID, entry); err != nil {
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
