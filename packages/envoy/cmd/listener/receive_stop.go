package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// end ends the term being served. On a lost lease (ctx still live) it unsubscribes, the queued
// messages are dropped without an ack, to come back after the ack wait to whoever holds the lease,
// and the message in hand finishes, unless a shutdown comes meanwhile; a shutdown after the loss
// then gives the message in hand inflightGrace, cancels its handling and NAKs what is queued. A
// shutdown (ctx ended) of a term that still holds the lease hands its messages back (handBack),
// and one without a lease drains (endWithoutLease).
//
// The term stops electing this pod the relayer of its machine's session consumers before any of
// that (sessionRelays.setOwned, set by durableReceiver.begin): every relay stops here, so the
// pod that takes over is the only one pulling each session's consumer, and each consumer's
// position in the stream is left for it.
func (r *durableReceiver) end(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	r.term = nil
	r.state = receiveStandby
	// No term is receiving from here, whatever this one's end does with its subscription, so the
	// lease's signal reads this pod's connection alone again (durableReceiver.receiveUp).
	r.subscribed.Store(nil)
	r.mu.Unlock()
	r.relays.setOwned(false)
	switch {
	case ctx.Err() == nil:
		r.endLost(ctx, t)
	case r.lease == nil:
		r.endWithoutLease(t)
	case r.lease.Holding() && r.lease.Status().Term == t.leaseTerm:
		r.handBack(t)
	default:
		r.endLost(ctx, t)
	}
	t.cancel()
	close(t.stopped)
}

// endLost ends a term whose lease was lost: it unsubscribes and stops taking messages, drops what
// is queued while ctx is live and waits for the message in hand, and once ctx has ended (a
// shutdown) gives the message in hand inflightGrace, cancels its handling and NAKs what is queued.
// Either way the pushes the term handed to the serving-port workers end with it.
func (r *durableReceiver) endLost(ctx context.Context, t *receiveTerm) {
	r.mu.Lock()
	sub, conn := t.sub, t.conn
	t.sub = nil
	r.mu.Unlock()
	if sub != nil {
		_ = sub.Unsubscribe()
		_ = conn.FlushTimeout(time.Second)
	}
	t.close()
	if ctx.Err() == nil {
		dropped := len(t.takeQueued())
		r.logger.Info("listener lease lost; stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("dropped", dropped))
		select {
		case <-t.handled:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		r.settleInHand(t)
	}
	// The pushes this term handed to the serving-port workers are the term's as much as its queue
	// is, and are taken back once nothing of the term is handing more over: the workers stop
	// pushing them and keeping their messages in progress (servePortWorkers.endTerm), so a pod
	// whose lease is gone pushes nothing more, and the server gives those messages to the pod
	// holding the lease now once their ack wait runs out, as it does the queue's.
	parked, _ := r.workers.endTerm(t.handling, servePortTermStopWait)
	if ctx.Err() == nil {
		if len(parked) > 0 {
			r.logger.Info("dropped the serving-port pushes of the term whose lease was lost",
				slog.String("consumer", r.consumer), slog.Int("dropped_pushes", len(parked)))
		}
		return
	}
	queued := append(t.takeQueued(), parked...)
	for _, m := range queued {
		_ = m.Nak()
	}
	r.logger.Info("stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("naked", len(queued)))
}

// endWithoutLease ends a listener's receiving without a lease on a shutdown: the handler takes no
// more messages, the message in hand gets inflightGrace, the subscription drains, and everything
// queued, the serving-port workers' pushes of this term included, is NAKed at once, in stream
// order, and flushed.
func (r *durableReceiver) endWithoutLease(t *receiveTerm) {
	t.stopHandling()
	r.settleInHand(t)
	conn, _ := r.drain(t)
	t.close()
	parked, _ := r.workers.endTerm(t.handling, servePortTermStopWait)
	queued := append(t.takeQueued(), parked...)
	sortByStreamSequence(queued)
	for _, m := range queued {
		_ = m.Nak()
	}
	if conn != nil {
		_ = conn.FlushTimeout(time.Second)
	}
	r.logger.Info("stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("naked", len(queued)))
}

// handBack is a holder's graceful stop. The handler takes no more messages while the subscription
// stays and what arrives is queued; the message in hand gets inflightGrace. Then every message
// queued or held is NAKed with handBackDelay, as one batch in stream order, the lease's term is
// marked unsettled, so the next holder settles, and the NAKs are flushed before the lease is
// released, which writes the ack wait the term still owes (lease.ackWaitOwed). The pod then
// lingers on its subscription, so the deliver subject keeps a subscriber while the server pushes
// to it: each message that reaches it is NAKed with the same delay, up to handBackNaksMax times
// for one stream sequence, so a repeat that reached this pod alone before the next holder
// subscribed comes back to it after the delay; a repeat past that is dropped with neither ack nor
// NAK (it comes back at its ack wait). The linger ends once the lease record names another holder
// that is receiving, or lingerMax after the batch was NAKed, the flush and the release included,
// or once no other pod has taken the record; no end but a receiving holder comes before
// lingerUntaken and a poll have passed since the release, so a standby still taking the record
// finds the deliver subject held for it. Then the subscription drains.
//
// A stop that went through all of that writes the lease's clean-hand-back mark for the record it
// released (markHandBack), which is the whole of what its successor's settle hold rests on: its
// 3 s cap says no message of this hand-over is waiting out an ack wait, and only this pod knows
// that. Nothing is vouched for by timing. A handler still running after its cancel holds a
// message this stop cannot account for, so it marks nothing either (settleInHand).
//
// A release the lease's loop outlasts (handBackReleaseWait) writes nothing and records the term's
// ack-wait debt instead (lease.Release): the linger then reads this pod's own unreleased record,
// ends "not taken" at its floor and lets go of the deliver subject, the release the receiver's
// own stop makes next writes the record with that debt (durableReceiver.Stop), and this stop
// marks nothing, since by then the deliver subject has been let go. A standby that takes that
// record must settle for an ack wait.
func (r *durableReceiver) handBack(t *receiveTerm) {
	t.stopHandling()
	handled := r.settleInHand(t)
	// The pushes this term handed to the serving-port workers are its messages too, and are
	// taken back now that nothing of the term hands more over: the workers stop pushing them and
	// keeping them in progress, and give them up unsettled (servePortWorkers.endTerm), so they
	// are NAKed in the batch below with everything else the term holds and the next holder is
	// given them after the hand-back delay. A worker still inside a push when the take-back's
	// wait ran out holds a message of this term that this stop cannot account for, which is what
	// idle says (markHandBack).
	parked, idle := r.workers.endTerm(t.handling, servePortTermStopWait)
	deadline := time.Now().Add(r.timing.lingerMax)
	t.handBack(r.timing.handBackDelay, parked)
	r.lease.Unsettle(t.leaseTerm)
	r.mu.Lock()
	conn := t.conn
	r.mu.Unlock()
	_ = conn.FlushTimeout(time.Second)
	releaseCtx, cancel := context.WithTimeout(context.Background(), handBackReleaseWait)
	err := r.lease.Release(releaseCtx)
	cancel()
	if err != nil {
		r.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
	lingered, end := r.linger(deadline)
	_, drained := r.drain(t)
	t.close()
	marked, why := r.markHandBack(end, drained, handled, idle)
	handedBack, renaked, dropped := t.handBackCounts()
	fields := []slog.Attr{
		slog.String("consumer", r.consumer),
		slog.Int("handed_back", handedBack),
		slog.Int64("lingered_ms", lingered.Milliseconds()),
		slog.String("linger_end", string(end)),
		slog.Int("renaked_repeats", renaked),
		slog.Int("dropped_repeats", dropped),
		slog.Bool("hand_back_marked", marked),
	}
	if !marked {
		fields = append(fields, slog.String("no_mark", why))
	}
	r.logger.Info("handed the durable's messages back to the next holder", fields...)
}

// markHandBack writes the lease's clean-hand-back mark for the record this stop released, and
// reports whether it wrote one and, when it did not, why in a few words for the stop's own log
// line. Every part of the hand-over must have gone through: end is the linger's, which must be
// another pod receiving on the deliver subject, drained the server's word that this pod's
// subscription is closed, rather than the drain's wait running out with messages possibly still
// coming, handled whether the message in hand's handler returned, since one still running holds a
// message that comes back only at its ack wait, long after the successor's 3 s hold has ended,
// and workersIdle whether the serving-port workers had left this term's pushes, since one they
// are still inside holds a message of the term the same way (servePortWorkers.endTerm). The lease
// adds what it knows: its own release wrote the record, and that record carried no stale binding
// (lease.MarkHandBack). A write that fails leaves the next holder the settle hold of a hard
// take-over and nothing worse, so it is a warning and no more; listenerStateRequestWait bounds
// it, and a stop past the shutdown's budget can be cut off before it by the listener's own exit
// (shutdownListener).
func (r *durableReceiver) markHandBack(end lingerEnd, drained, handled, workersIdle bool) (bool, string) {
	switch {
	case end != lingerEndReceiving:
		return false, noMarkNoSuccessor
	case !drained:
		return false, noMarkUndrained
	case !handled:
		return false, noMarkHandlerRunning
	case !workersIdle:
		return false, noMarkWorkerPushing
	}
	ctx, cancel := context.WithTimeout(context.Background(), listenerStateRequestWait)
	defer cancel()
	marked, err := r.lease.MarkHandBack(ctx)
	switch {
	case err != nil:
		r.logger.Warn("clean hand-back not marked; the next holder settles for an ack wait",
			slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		return false, noMarkWriteFailed
	case !marked:
		return false, noMarkRelease
	}
	return true, ""
}

// The reasons markHandBack gives for writing no mark, the no_mark of the hand-back log line, one
// for each gate in the order it checks them.
const (
	noMarkNoSuccessor    = "no successor was receiving when the linger ended"
	noMarkUndrained      = "the server did not confirm the drain"
	noMarkHandlerRunning = "the message in hand was still being handled after its cancel"
	noMarkWorkerPushing  = "a serving-port worker still held a message of the term after its cancel"
	noMarkWriteFailed    = "the mark's write failed"
	noMarkRelease        = "this stop's own release wrote no record, or one beside a lost node's binding"
)

// lingerEnd is how a stopping pod's linger ended, the linger_end of its hand-back log line.
type lingerEnd string

const (
	// lingerEndReceiving is the end a clean hand-back needs: the lease record named another
	// holder of this machine receiving on the deliver subject before this pod let go of it.
	lingerEndReceiving lingerEnd = "receiving holder seen"
	// lingerEndTimeout is the linger run to its cap with no such holder read.
	lingerEndTimeout lingerEnd = "timeout"
	// lingerEndUntaken is every read answering that no other pod has taken the record, past the
	// floor that holds the deliver subject for a standby still taking it.
	lingerEndUntaken lingerEnd = "not taken"
)

// linger waits, reading the lease record every lingerPoll, until it names another holder that is
// receiving, or until deadline, or until every read has answered that no other pod has taken the
// record, and reports how long it waited and which ended it. Only lingerEndReceiving earns the
// stop its clean-hand-back mark (durableReceiver.markHandBack).
//
// It starts just after the release, and holds the subscription for lingerUntaken and a poll past
// that start however far the deadline has run: the deadline is set before the flush and the
// release, which a slow one can leave in the past. That floor is what gives a standby taking the
// record the time to subscribe at all, so the linger has a receiving holder to see. A take-over
// read as taken past the floor is waited for until the deadline all the same, which is where a
// stop with its cap unspent holds the deliver subject for a successor still preparing.
func (r *durableReceiver) linger(deadline time.Time) (time.Duration, lingerEnd) {
	start := time.Now()
	bridged := start.Add(r.timing.lingerUntaken + r.timing.lingerPoll)
	if deadline.Before(bridged) {
		deadline = bridged
	}
	poll := time.NewTicker(r.timing.lingerPoll)
	defer poll.Stop()
	untaken := true
	for {
		taken, receiving, err := r.lease.Successor()
		switch {
		case err != nil:
			untaken = false
		case receiving:
			return time.Since(start), lingerEndReceiving
		case taken:
			untaken = false
		}
		switch now := time.Now(); {
		case !now.Before(deadline):
			return time.Since(start), lingerEndTimeout
		case untaken && !now.Before(bridged):
			return time.Since(start), lingerEndUntaken
		}
		<-poll.C
	}
}

// drain ends the term's subscription as nats.go drains one: the server is told to stop sending,
// and what it sent before that still reaches intake. It waits handBackDrainWait at most, then
// unsubscribes, and returns the subscription's connection (nil when there was none) and whether
// the server confirmed the subscription closed inside that wait. A drain that ran out, or that
// the client refused, can have left messages the server had sent unreceived, so no clean
// hand-back is marked for it (durableReceiver.markHandBack).
func (r *durableReceiver) drain(t *receiveTerm) (*nats.Conn, bool) {
	r.mu.Lock()
	sub, conn := t.sub, t.conn
	t.sub = nil
	r.mu.Unlock()
	if sub == nil {
		return nil, false
	}
	closed := sub.StatusChanged(nats.SubscriptionClosed)
	if sub.Drain() != nil {
		_ = sub.Unsubscribe()
		return conn, false
	}
	wait := time.NewTimer(handBackDrainWait)
	defer wait.Stop()
	select {
	case <-closed:
		return conn, true
	case <-wait.C:
		_ = sub.Unsubscribe()
		return conn, false
	}
}

// settleInHand gives the message in hand inflightGrace to settle, then cancels its handling, which
// aborts a prompt in flight and NAKs the message, and waits inflightCancelWait more for the
// handler to return. It reports whether the handler returned: one still running holds a message
// this pod can no longer keep alive, which comes back at its ack wait, long after the next
// holder's 3 s hold would have ended, so a graceful stop writes no clean-hand-back mark for it
// (markHandBack).
func (r *durableReceiver) settleInHand(t *receiveTerm) bool {
	grace := time.NewTimer(r.timing.inflightGrace)
	defer grace.Stop()
	select {
	case <-t.handled:
		return true
	case <-grace.C:
	}
	r.logger.Info("cancelling the message in hand after the shutdown grace", slog.String("consumer", r.consumer), slog.String("grace", r.timing.inflightGrace.String()))
	t.cancel()
	wait := time.NewTimer(inflightCancelWait)
	defer wait.Stop()
	select {
	case <-t.handled:
		return true
	case <-wait.C:
		r.logger.Warn("the message in hand was still being handled after its cancel", slog.String("consumer", r.consumer))
		return false
	}
}
