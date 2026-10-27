package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

const (
	// publisherHold names the hold (duty.Hold) a process publishes the outbox under: one process
	// at a time holds it, and only the holder reads and sends events.
	publisherHold = "dispatch-outbox"
	// eventsChannel is the channel migration 0107's trigger notifies once per statement that
	// inserts events, whichever process committed it.
	eventsChannel = "dispatch_events"
	// natsLossGrace is how long the holder's NATS connection may stay down before the holder gives
	// the hold up, so that a process whose NATS works can publish instead.
	natsLossGrace = 3 * time.Second
	// probeTimeout bounds the flush that tells a connection that is gone or silent from one whose
	// server still answers, after a batch's sends failed for the connection (scanBatch).
	probeTimeout = 2 * time.Second
	// holdRetry is how long a standby waits to try the hold again after the database failed to
	// answer the take or the read of the current hold's lapse, or after a probe of its own NATS
	// connection went unanswered (answering).
	holdRetry = time.Second
)

// publisherHoldOptions are the publisher hold's parameters: it lasts 6 s on the database's clock
// after each renewal, is renewed every 2 s, and reports itself lost 2 s before it lapses, so a
// process that is killed is replaced by a standby within 6 s of its last renewal.
var publisherHoldOptions = duty.HoldOptions{TTL: 6 * time.Second, Renew: 2 * time.Second, Margin: 2 * time.Second}

// holdEnd is why a holder stopped publishing.
type holdEnd string

const (
	// stopping: ctx ended; the hold is released.
	stopping holdEnd = "stopping"
	// natsLost: NATS stayed down for natsLossGrace, or a batch's sends failed and the probe of
	// their connection failed too; the hold is released.
	natsLost holdEnd = "NATS lost"
	// holdLost: the hold reported itself lost, or its deadline passed during a batch; the hold is
	// dropped, since the name may already be another's.
	holdLost holdEnd = "lost"
)

// Outbox publishes durable Dispatch events to NATS, one process at a time. Every process runs one;
// the one holding the publisher hold (publisherHold) reads and sends events, and the others stand
// by to take the hold over.
type Outbox struct {
	deps Deps
	// instance names this process in the hold's row and in every log line about the hold and its
	// batches; token is the hold's token, the host and process id.
	instance uuid.UUID
	token    string
	// wake is the holder's one-slot wake channel: every source of a scan (signal) sends to it
	// without blocking, so wakes that come while one is pending fold into it.
	wake chan struct{}
	// standbyWake is the standby's one-slot wake channel (signalStandby): the release of the
	// publisher hold, a new LISTEN connection and the NATS connection changing, and nothing the
	// holder's events cause, so a standby tries the hold only when it may be free.
	standbyWake chan struct{}
	// hold is the publisher hold while this process holds it, and nil otherwise.
	hold atomic.Pointer[duty.Hold]
	// claims counts the batches this outbox read, one per scanBatch: how often it scanned.
	claims atomic.Int64
	// probeBeforeHold is set when this process gave the publisher hold up for NATS lost, and
	// cleared once a probe of its connection is answered (answering). Only Run's goroutine reads
	// and writes it.
	probeBeforeHold bool
}

// New returns an outbox for deps, with an instance id of its own. Run starts it.
func New(deps Deps) *Outbox {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return &Outbox{
		deps:        deps,
		instance:    uuid.New(),
		token:       fmt.Sprintf("%s/%d", host, os.Getpid()),
		wake:        make(chan struct{}, 1),
		standbyWake: make(chan struct{}, 1),
	}
}

// Publishing reports whether this process publishes the outbox now: it holds the publisher hold
// and the hold's loss deadline has not passed. A nil outbox (NATS disabled) never publishes.
func (o *Outbox) Publishing() bool {
	if o == nil {
		return false
	}
	hold := o.hold.Load()
	return hold != nil && time.Now().Before(hold.Deadline())
}

// Run publishes the outbox whenever this process holds the publisher hold, and stands by for it
// otherwise, until ctx ends. It is called once per Outbox.
//
// A standby tries the hold only while its NATS client is connected - and, once it gave the hold up
// for NATS lost, only after a probe of its connection is answered (answering) - and only when
// woken (standbyWake): by the release of the hold (duty.HoldReleasedChannel), by a timer armed to
// the moment the current hold lapses on the database's clock (duty.Expiry, read again after every
// wake), by a new LISTEN connection (a release sent while it had none is not reported), or by its
// own NATS connection changing; a standby probing a connection that does not answer probes it
// again holdRetry later. The events the holder writes and publishes never wake a standby, so a
// standby sends no statement per event. A holder scans on each of its own wakes (wake) until the
// hold is lost, NATS is lost (natsLost) or ctx ends: every insert of events (a LISTEN on
// eventsChannel, whose every new connection signals too, to catch up what it missed), this
// process's own commits (the events broker, which still reaches the outbox while the LISTEN
// connection redials), the NATS connection changing (the prompt resend of the events a lost
// connection released), and the holder's one retry timer, armed to the earliest back-off due
// (nextRetry). Nothing scans on a period.
//
// On ctx's end the holder takes no further batch, finishes the one in flight and records it, and
// releases the hold, which wakes a standby at once. Run returns once that is done and every wake
// source has stopped.
func (o *Outbox) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	// None of these carries the publisher's marker below: a connection one of them holds must
	// never count as one the publisher holds.
	wg.Go(func() { compactDaily(ctx, o.deps) })
	wg.Go(func() { o.listen(ctx) })
	wg.Go(func() { o.wakeOnCommits(ctx) })
	wg.Go(func() { o.wakeOnConnectionChange(ctx) })
	// The publisher is one of the process's transaction openers, so its context carries the
	// pool's marker: a read it ever makes while one of its transactions is open is refused
	// rather than left to deadlock the pool (store.ErrNestedAcquire).
	publishing := store.WithTransactionTracking(ctx)
	for {
		hold := o.standby(publishing)
		if hold == nil {
			return
		}
		o.probeBeforeHold = o.publish(publishing, hold) == natsLost
	}
}

// signal wakes the holder's loop, or leaves the wake already pending.
func (o *Outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// signalStandby wakes the standby's loop, or leaves the wake already pending.
func (o *Outbox) signalStandby() {
	select {
	case o.standbyWake <- struct{}{}:
	default:
	}
}

// listen holds the outbox's LISTEN connection until ctx ends: an insert of events by any process
// wakes the holder, and the release of the publisher hold wakes the standby. Every new connection
// wakes both, since a notification sent while it had none is not reported.
func (o *Outbox) listen(ctx context.Context) {
	listener := &pglisten.Listener{
		Owner:    "dispatch",
		Dial:     o.deps.Listen,
		Channels: []string{eventsChannel, duty.HoldReleasedChannel},
		OnConnected: func() {
			o.signal()
			o.signalStandby()
		},
		OnNotify: func(channel, payload string) {
			switch {
			case channel == eventsChannel:
				o.signal()
			case payload == publisherHold:
				o.signalStandby()
			}
		},
	}
	listener.Run(ctx)
}

// wakeOnCommits wakes the outbox for every event this process commits, until ctx ends. A
// subscription the broker dropped for overflowing is replaced, and wakes the outbox too, since the
// events it dropped are unpublished.
func (o *Outbox) wakeOnCommits(ctx context.Context) {
	events, unsubscribe := o.deps.Broker.Subscribe()
	defer func() { unsubscribe() }()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-events:
			if !open {
				unsubscribe()
				events, unsubscribe = o.deps.Broker.Subscribe()
			}
			o.signal()
		}
	}
}

// wakeOnConnectionChange wakes the holder and the standby at every change of its NATS connection,
// until ctx ends: a standby tries the hold once connected, and a holder resends what a lost
// connection released, or starts counting natsLossGrace.
func (o *Outbox) wakeOnConnectionChange(ctx context.Context) {
	for {
		changed := o.deps.Publisher.ConnectionChanged()
		select {
		case <-ctx.Done():
			return
		case <-changed:
			o.signal()
			o.signalStandby()
		}
	}
}

// standby returns the publisher hold once this process takes it, or nil once ctx ends. While its
// NATS client is connected and answering it tries the hold at once and after every wake, and arms a
// timer to the moment the current hold lapses; while it is connected and not answering, it probes
// again holdRetry later or at the next wake; while it is not connected, it waits for a wake alone,
// which its connection coming back gives.
func (o *Outbox) standby(ctx context.Context) *duty.Hold {
	for {
		if ctx.Err() != nil {
			return nil
		}
		var lapse *time.Timer
		var lapsed <-chan time.Time
		if o.deps.Publisher.Connected() {
			wait := holdRetry
			if o.answering() {
				var hold *duty.Hold
				hold, wait = o.tryHold(ctx)
				if hold != nil {
					return hold
				}
				if wait == 0 {
					continue
				}
			}
			lapse = time.NewTimer(wait)
			lapsed = lapse.C
		}
		select {
		case <-ctx.Done():
		case <-o.standbyWake:
		case <-lapsed:
		}
		if lapse != nil {
			lapse.Stop()
		}
	}
}

// answering reports whether this process's NATS connection answers: always, unless the process
// gave the publisher hold up for NATS lost (probeBeforeHold) and no probe of its connection has
// been answered since, in which case it probes the connection now. A connection that went silent
// still reports itself connected, so without the probe the process would take back at once the
// hold it gave up for that very connection, ahead of a standby whose NATS works.
func (o *Outbox) answering() bool {
	if !o.probeBeforeHold {
		return true
	}
	if err := o.deps.Publisher.Window().Probe(probeTimeout); err != nil {
		return false
	}
	o.probeBeforeHold = false
	return true
}

// tryHold takes the publisher hold, or returns how long until the current holder's hold lapses on
// the database's clock: zero when it already has (it was released or lapsed between the two
// statements), and holdRetry when the database did not answer.
func (o *Outbox) tryHold(ctx context.Context) (*duty.Hold, time.Duration) {
	pool := o.deps.Store.Pool
	options := publisherHoldOptions
	options.Token = o.token
	hold, err := duty.TryHold(ctx, pool, publisherHold, o.instance, options)
	if err == nil && hold != nil {
		return hold, 0
	}
	var remaining time.Duration
	held := false
	if err == nil {
		remaining, held, err = duty.Expiry(ctx, pool, publisherHold)
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch outbox: try the publisher hold", "instance", o.instance, "error", err)
		}
		return nil, holdRetry
	}
	if !held {
		return nil, 0
	}
	return nil, max(remaining, 0)
}

// publish runs this process as the holder of hold until it stops (serve), then releases the hold,
// or drops it when it was lost, and returns why it stopped.
func (o *Outbox) publish(ctx context.Context, hold *duty.Hold) holdEnd {
	o.hold.Store(hold)
	slog.Info("dispatch outbox: took the publisher hold", "instance", o.instance)
	end := o.serve(ctx, hold)
	o.hold.Store(nil)
	if end == holdLost {
		slog.Info("dispatch outbox: lost the publisher hold", "instance", o.instance)
		return end
	}
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := hold.Release(release); err != nil {
		// The row then lapses on its own, TTL after the last renewal.
		slog.Error("dispatch outbox: release the publisher hold", "instance", o.instance, "error", err)
	}
	slog.Info("dispatch outbox: released the publisher hold", "instance", o.instance, "reason", string(end))
	return end
}

// serve scans while this process holds hold: once at once, then on each wake, while the NATS
// client is connected. It returns why it stopped: ctx ended, the hold was lost or its deadline
// passed mid-batch, or NATS was lost - down for natsLossGrace without a break, or a batch's sends
// failed for the connection and a probe of that connection failed too.
func (o *Outbox) serve(ctx context.Context, hold *duty.Hold) holdEnd {
	var disconnected, retryTimer *time.Timer
	var disconnectedFor, retryDue <-chan time.Time
	due := true
	for {
		if !o.deps.Publisher.Connected() {
			if disconnected == nil {
				disconnected = time.NewTimer(natsLossGrace)
				disconnectedFor = disconnected.C
			}
		} else {
			if disconnected != nil {
				disconnected.Stop()
				disconnected, disconnectedFor = nil, nil
			}
			if due {
				due = false
				end := o.scan(ctx, hold)
				switch {
				case ctx.Err() != nil:
					return stopping
				case end.fenced:
					return holdLost
				case end.natsLost:
					return natsLost
				}
				if retryTimer != nil {
					retryTimer.Stop()
					retryTimer, retryDue = nil, nil
				}
				if wait, ok := o.nextRetry(ctx, end.failed); ok {
					retryTimer = time.NewTimer(wait)
					retryDue = retryTimer.C
				}
			}
		}
		select {
		case <-ctx.Done():
			return stopping
		case <-hold.Lost():
			return holdLost
		case <-disconnectedFor:
			return natsLost
		case <-o.wake:
			due = true
		case <-retryDue:
			retryTimer, retryDue = nil, nil
			due = true
		}
	}
}

// scanEnd is how a scan ended: fenced, the hold's deadline passed or a record found the hold
// another's; natsLost, a batch's sends failed for the connection and the probe of it failed;
// failed, a statement failed.
type scanEnd struct {
	fenced, natsLost, failed bool
}

// scan reads and publishes batches under hold until there is nothing more to send: it takes the
// next batch while the last one published anything or was full, and nothing went wrong. It takes
// none once hold's deadline has passed. Every batch that sent anything is logged as a sent batch
// with this process's instance, the span of event ids it sent, how many events it sent at least one
// envelope of (sent) and how many it marked published (published).
func (o *Outbox) scan(ctx context.Context, hold fence) scanEnd {
	for {
		if ctx.Err() != nil {
			return scanEnd{}
		}
		if !time.Now().Before(hold.Deadline()) {
			return scanEnd{fenced: true}
		}
		o.claims.Add(1)
		batch, err := scanBatch(ctx, o.deps, hold, o.instance)
		if batch.sent > 0 {
			slog.Info("dispatch outbox: sent batch", "instance", o.instance,
				"sent", batch.sent, "published", batch.published, "first_id", batch.firstID, "last_id", batch.lastID, "started", batch.started)
		}
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Error("dispatch outbox: scan", "instance", o.instance, "error", err)
			}
			return scanEnd{failed: true}
		case batch.fenced:
			return scanEnd{fenced: true}
		case batch.natsLost:
			return scanEnd{natsLost: true}
		case batch.blocked, batch.published == 0 && batch.sendable < batchSize:
			return scanEnd{}
		}
	}
}

// nextRetry is how long until the earliest back-off of an unpublished event falls due, read from
// the database's clock, and false when no event waits on one. A scan that failed, or a read of it
// that does, is retried retry.BaseDelay later.
func (o *Outbox) nextRetry(ctx context.Context, failed bool) (time.Duration, bool) {
	if failed {
		return retry.BaseDelay, true
	}
	var micros *int64
	if err := o.deps.Store.Pool.QueryRow(ctx, `
		select (extract(epoch from min(next_attempt_at) - now()) * 1000000)::bigint
		from events
		where published_at is null
		  and next_attempt_at is not null
		  and (payload ->> 'pending') is distinct from 'true'
	`).Scan(&micros); err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch outbox: read the next retry", "instance", o.instance, "error", err)
		}
		return retry.BaseDelay, true
	}
	if micros == nil {
		return 0, false
	}
	return max(time.Duration(*micros)*time.Microsecond, 0), true
}
