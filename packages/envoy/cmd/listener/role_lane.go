package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// roleForwardWaitLimit bounds the role forwards the role lane holds at once, queued behind their
// holder's earlier forwards or waiting for their outcome. When it is reached the role lane's
// handler waits for one to end, which only holds role messages in the lane's subscription, whose
// pending limits are far above any backlog that wait can build (bus.ConnectRoleLane).
const roleForwardWaitLimit = 4096

// roleLaneSlowConsumerInterval is how often the role lane's slow-consumer count is read for a change
// to log (watchRoleLaneSlowConsumers).
const roleLaneSlowConsumerInterval = 5 * time.Second

// roleForward is one role message the role lane resolved to its holder, every guard passed: item is
// the message as it arrived, sessionID its holder, and storedOnly set for a holder that answers no
// receipt (roleForwardStoredOnly).
type roleForward struct {
	item       contracts.Envelope
	sessionID  string
	storedOnly bool
}

// roleForwarder runs the role lane's forwards off its NATS handler, so a holder slow to answer, or
// one that never does, holds up no other role message. Forwards to one holder are published in the
// order the handler handed them over, by a worker per holder with forwards queued; each forward's
// wait for its outcome runs on a goroutine of its own, and the outcome becomes the forward's
// delivery status and exception as it always has (finishRoleForward). Forwards queued and waiting
// together are bounded by roleForwardWaitLimit.
type roleForwarder struct {
	// cfg is the delivery configuration the forwards are published (forwardRole) and reported
	// through.
	cfg   listenerDeliveryHandlerConfig
	slots chan struct{}
	// pending counts the forwards handed over and not yet reported, for wait.
	pending sync.WaitGroup

	mu sync.Mutex
	// queues holds each holder's forwards not yet published, in arrival order. A holder has a key
	// exactly while its worker runs.
	queues map[string][]roleForward
}

func newRoleForwarder(cfg listenerDeliveryHandlerConfig) *roleForwarder {
	return &roleForwarder{cfg: cfg, slots: make(chan struct{}, roleForwardWaitLimit), queues: map[string][]roleForward{}}
}

// forwarded is the envelope published on the holder's agent subject: the message under the
// role-forward dedupe key, which the durable path recognises and acknowledges.
func (f roleForward) forwarded() contracts.Envelope {
	forwarded := f.item
	forwarded.DedupeKey = roleForwardDedupePrefix + f.item.DedupeKey
	return forwarded
}

// enqueue queues forward behind its holder's earlier forwards, starting the holder's worker when it
// has none, once a slot is free.
func (f *roleForwarder) enqueue(forward roleForward) {
	f.slots <- struct{}{}
	f.pending.Add(1)
	f.mu.Lock()
	queue, running := f.queues[forward.sessionID]
	f.queues[forward.sessionID] = append(queue, forward)
	f.mu.Unlock()
	if !running {
		go f.publish(forward.sessionID)
	}
}

// publish is sessionID's worker: it publishes the holder's queued forwards in order, hands each
// one's wait to a goroutine of its own, and ends once the queue is empty.
func (f *roleForwarder) publish(sessionID string) {
	subject := contracts.AgentSubject(sessionID)
	for {
		f.mu.Lock()
		queue := f.queues[sessionID]
		if len(queue) == 0 {
			delete(f.queues, sessionID)
			f.mu.Unlock()
			return
		}
		forward := queue[0]
		queue[0] = roleForward{}
		f.queues[sessionID] = queue[1:]
		f.mu.Unlock()

		wait, err := f.cfg.forwardRole(subject, forward.forwarded(), roleReceiptTimeout, forward.storedOnly)
		if err != nil {
			f.finish(forward, err)
			continue
		}
		go func() { f.finish(forward, wait()) }()
	}
}

// finish reports forward's outcome and frees its slot.
func (f *roleForwarder) finish(forward roleForward, err error) {
	finishRoleForward(f.cfg, forward, err)
	<-f.slots
	f.pending.Done()
}

// outstanding is how many forwards are queued or waiting for their outcome.
func (f *roleForwarder) outstanding() int {
	return len(f.slots)
}

// wait waits, within ctx, for every forward handed over to be reported. The shutdown runs it once
// the role lane has drained, so no forward is handed over meanwhile, and before the main client
// drains, since the forwards and their exceptions are published through it. It reports whether all
// were.
func (f *roleForwarder) wait(ctx context.Context) bool {
	return waitWithin(ctx, &f.pending)
}

// startRoleForward is forwardRole over client: a forward to a holder that answers receipts waits
// for the stream's acknowledgement and the receipt (bus.Client.StartRequestCoreTo), and one to a
// holder that answers none for the acknowledgement alone (bus.Client.StartRequestStoredTo).
func startRoleForward(client *bus.Client) func(subject string, item contracts.Envelope, timeout time.Duration, storedOnly bool) (func() error, error) {
	return func(subject string, item contracts.Envelope, timeout time.Duration, storedOnly bool) (func() error, error) {
		start := client.StartRequestCoreTo
		if storedOnly {
			start = client.StartRequestStoredTo
		}
		forward, err := start(subject, item, timeout)
		if err != nil {
			return nil, err
		}
		return forward.Wait, nil
	}
}

// roleLane is the listener's role lane at shutdown: its own client (bus.ConnectRoleLane), the
// forwards its handler hands over, and stopWatch, which ends watchRoleLaneSlowConsumers.
type roleLane struct {
	client    *bus.Client
	forwards  *roleForwarder
	stopWatch func()
}

// drain ends the role lane at shutdown: it drains the lane's client within timeout, so the handler
// takes every role message the server already delivered and no more, then waits for the forwards it
// handed over, the receipt window after that at most, since a forward published by then has its
// outcome within it. A forward still waiting then is logged and left to the main client's drain.
func (l roleLane) drain(logger *logging.Logger, timeout time.Duration) {
	l.stopWatch()
	if err := l.client.Drain(timeout); err != nil {
		logger.Warn("role lane drain error", slog.String("error", err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleReceiptTimeout)
	defer cancel()
	if !l.forwards.wait(ctx) {
		logger.Warn("role forwards still waiting at shutdown", slog.Int("outstanding", l.forwards.outstanding()))
	}
}

// watchRoleLaneSlowConsumers logs each change of the role lane's slow-consumer count until ctx ends.
// The lane's pending limits are far above any backlog its handler can build, so the count stays 0
// unless nats.go reports a slow consumer anyway; the count is also the
// envoy_role_lane_slow_consumers metric.
func watchRoleLaneSlowConsumers(ctx context.Context, lane *bus.Client, logger *logging.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seen uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if count := lane.SlowConsumers(); count != seen {
			logger.Warn("role lane slow consumers", slog.Uint64("total", count), slog.Uint64("new", count-seen))
			seen = count
		}
	}
}

// roleForwardStoredOnly reports whether holder answers no receipt for a role forward: a session on a
// serving port has no NATS subscriber. The stream's copy of the forward reaches its port with
// acknowledgement and redelivery: from its session consumer, which the listener's relay pushes,
// when its entry records consumer delivery, else from the owning listener's durable
// (roleForwardCopyDelivery). Its forward counts as delivered once the stream acknowledges storing it.
func roleForwardStoredOnly(holder session.SessionEntry) bool {
	return holder.Port > 0
}

// storedForwardCopyIsDelivery reports whether the stored copy of a forward on sessionID's agent
// subject, stored at stream sequence storedSeq, is that session's own delivery rather than a copy
// of one it already has (listenerDeliveryHandler). Only a serving-port session of this listener's
// is: it answers no receipt, and its consumer does not hold this copy, so the copy pushed to its
// port is how it receives the message (roleForwardCopyDelivery). Every other recipient took the
// forward on its own NATS connection or takes the stored copy another way
// (takesStoredMessagesItself), and a session with no entry, or one on another listener, is nobody
// here to push to.
func storedForwardCopyIsDelivery(cfg listenerDeliveryHandlerConfig, sessionID string, storedSeq uint64) bool {
	entry, err := cfg.sessions.Get(sessionID)
	if err != nil || takesStoredMessagesItself(entry, storedSeq) {
		return false
	}
	return ownsServingPortSession(cfg, sessionID, entry)
}

// roleForwardCopyDelivery handles the stored copy of a role forward (roleForward.forwarded), which
// the durable reads on its holder's agent subject. A holder on NATS took the forward over its own
// subscription, a holder whose consumer holds this copy takes it from there, and a holder another
// listener owns is that listener's, so the copy is acknowledged. A serving-port holder this listener
// owns has no subscriber: the copy is pushed to its port and acknowledged only on the port's 204,
// by the holder's serving-port worker on the durable path, so its order behind the holder's earlier
// messages holds; a copy the workers cannot take goes back to the durable.
//
// Whether the holder's consumer holds this copy is read from the copy's own stream sequence
// (takesStoredMessagesItself), as every other path reads it: a copy stored below that consumer's
// start - one stored while the holder had no consumer, NAKed or left unacked, and redelivered
// after the holder registered again - is in no consumer, and this push is how the holder gets it.
// Acknowledging it on the holder's delivery mode alone loses it, and the forward was stored-only
// (roleForwardStoredOnly), so its sender was already told it was delivered.
func roleForwardCopyDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	holder, ok := "", false
	if message.mode == durableDelivery {
		holder, ok = strings.CutPrefix(message.msg.Subject, contracts.AgentTopicPrefix)
	}
	if !ok {
		message.finalize(false)
		return
	}
	if !storedForwardCopyIsDelivery(cfg, holder, message.storedSeq()) {
		message.finalize(false)
		return
	}
	if cfg.dedupeCache.Seen(item.DedupeKey, holder) || cfg.attemptCache.Seen(item.DedupeKey, holder) {
		message.finalize(false)
		return
	}
	cfg.attemptCache.Record(item.DedupeKey, holder)
	handOverOrRetry(cfg, message, servePortPush{item: item, sessionID: holder, settle: message.settle, storedSeq: message.storedSeq(), forward: true, logLine: "listener role forward pushed"})
}
