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
func (r *durableReceiver) end(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	r.term = nil
	r.state = receiveStandby
	r.mu.Unlock()
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
		queued := t.takeQueued()
		for _, m := range queued {
			_ = m.Nak()
		}
		r.logger.Info("stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("naked", len(queued)))
	}
}

// endWithoutLease ends a listener's receiving without a lease on a shutdown: the handler takes no
// more messages, the message in hand gets inflightGrace, the subscription drains, and everything
// queued is NAKed at once, in stream order, and flushed.
func (r *durableReceiver) endWithoutLease(t *receiveTerm) {
	t.stopHandling()
	r.settleInHand(t)
	conn := r.drain(t)
	t.close()
	queued := t.takeQueued()
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
// lingerUntaken and a poll have passed since the release, so this subscription is on the deliver
// subject for at least that long. That floor is what a take-over counts on: a term whose
// subscription the server has within leaseHandBackBridge of the release, a lingerPoll inside the
// floor, settles for the hand-back delay alone, and one slower than that settles for an ack wait
// instead (durableReceiver.linger, lease.HandBackHolds). Then the subscription
// drains, by the same rule.
//
// A release the lease's loop outlasts (handBackReleaseWait) writes nothing and records the term's
// ack-wait debt instead (lease.Release): the linger then reads this pod's own unreleased record,
// ends "not taken" at its floor and lets go of the deliver subject, and the release the
// receiver's own stop makes next writes the record with that debt (durableReceiver.Stop). No
// linger follows that write, so a standby taking it moments later must settle for an ack wait.
func (r *durableReceiver) handBack(t *receiveTerm) {
	t.stopHandling()
	r.settleInHand(t)
	deadline := time.Now().Add(r.timing.lingerMax)
	t.handBack(r.timing.handBackDelay)
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
	lingered, how := r.linger(deadline)
	r.drain(t)
	t.close()
	handedBack, renaked, dropped := t.handBackCounts()
	r.logger.Info("handed the durable's messages back to the next holder",
		slog.String("consumer", r.consumer),
		slog.Int("handed_back", handedBack),
		slog.Int64("lingered_ms", lingered.Milliseconds()),
		slog.String("linger_end", how),
		slog.Int("renaked_repeats", renaked),
		slog.Int("dropped_repeats", dropped),
	)
}

// linger waits, reading the lease record every lingerPoll, until it names another holder that is
// receiving, or until deadline, or until every read has answered that no other pod has taken the
// record, and reports how long it waited and which ended it.
//
// It starts just after the release, and holds the subscription for lingerUntaken and a poll past
// that start however far the deadline has run: the deadline is set before the flush and the
// release, which a slow one can leave in the past. That floor is this stop's guarantee to its
// successor - a term whose subscription the server has within leaseHandBackBridge of the
// release, a lingerPoll inside it, settles for the hand-back delay alone, and judges itself no
// hand-back past that (lease.HandBackHolds), so nothing rests on the linger lasting longer. A
// take-over read as taken past the floor is waited for until the deadline all the same, which is
// where a stop with its cap unspent holds the deliver subject for a successor still preparing.
func (r *durableReceiver) linger(deadline time.Time) (time.Duration, string) {
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
			return time.Since(start), "receiving holder seen"
		case taken:
			untaken = false
		}
		switch now := time.Now(); {
		case !now.Before(deadline):
			return time.Since(start), "timeout"
		case untaken && !now.Before(bridged):
			return time.Since(start), "not taken"
		}
		<-poll.C
	}
}

// drain ends the term's subscription as nats.go drains one: the server is told to stop sending,
// and what it sent before that still reaches intake. It waits handBackDrainWait at most, then
// unsubscribes, and returns the subscription's connection (nil when there was none).
func (r *durableReceiver) drain(t *receiveTerm) *nats.Conn {
	r.mu.Lock()
	sub, conn := t.sub, t.conn
	t.sub = nil
	r.mu.Unlock()
	if sub == nil {
		return nil
	}
	closed := sub.StatusChanged(nats.SubscriptionClosed)
	if sub.Drain() != nil {
		_ = sub.Unsubscribe()
		return conn
	}
	wait := time.NewTimer(handBackDrainWait)
	defer wait.Stop()
	select {
	case <-closed:
	case <-wait.C:
		_ = sub.Unsubscribe()
	}
	return conn
}

// settleInHand gives the message in hand inflightGrace to settle, then cancels its handling, which
// aborts a prompt in flight and NAKs the message, and waits inflightCancelWait more for the
// handler to return.
func (r *durableReceiver) settleInHand(t *receiveTerm) {
	grace := time.NewTimer(r.timing.inflightGrace)
	defer grace.Stop()
	select {
	case <-t.handled:
		return
	case <-grace.C:
	}
	r.logger.Info("cancelling the message in hand after the shutdown grace", slog.String("consumer", r.consumer), slog.String("grace", r.timing.inflightGrace.String()))
	t.cancel()
	wait := time.NewTimer(inflightCancelWait)
	defer wait.Stop()
	select {
	case <-t.handled:
	case <-wait.C:
		r.logger.Warn("the message in hand was still being handled after its cancel", slog.String("consumer", r.consumer))
	}
}
