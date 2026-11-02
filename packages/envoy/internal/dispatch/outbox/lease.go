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
	// concurrentBatches is how many batches a holder runs side by side (scan). A batch waits for
	// every answer of its sends, and a role forward waits roleReceiptTimeout for a holder that may
	// never answer, so one batch at a time would hold every other owner's events back for that
	// whole wait. Four lets a wake and a released owner start work while a batch waits, and keeps
	// the statements of the batches in flight, which run one at a time (lanes), from queueing
	// deeply on the outbox pool's two connections.
	concurrentBatches = 4
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
	// holdLost: the hold reported itself lost, its deadline passed during a batch, or a record
	// found it another's; its release stops its renewal and deletes the row only while it still
	// names this process, since the name may already be another's.
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
// On ctx's end the holder takes no further batch. In each batch in flight it makes no further
// send, settles the sends still in flight as lost (sendChains), records what NATS already
// answered and releases the rest (scanBatch), and then releases the hold (publish), which wakes
// a standby at once. Run returns once that is done and every wake source has stopped; a graceful
// stop of the process waits for that before it closes the pool, up to cmd/dispatch's
// outboxStopBudget.
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
	if err := o.deps.Publisher.WindowUntil(nil).Probe(probeTimeout); err != nil {
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

// publish runs this process as the holder of hold until it stops (serve), then releases the hold
// and returns why it stopped. The release comes on every end, a lost hold's too: serve also ends
// lost when a record found the hold another's with the loss deadline still ahead, and the release
// is what stops that hold's renewal before the next take starts another. Its delete and notify are
// guarded on this holder, so a hold another process has taken is left alone.
func (o *Outbox) publish(ctx context.Context, hold *duty.Hold) holdEnd {
	o.hold.Store(hold)
	slog.Info("dispatch outbox: took the publisher hold", "instance", o.instance)
	end := o.serve(ctx, hold)
	o.hold.Store(nil)
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := hold.Release(release); err != nil {
		// The renewal has stopped, and the row lapses on its own, TTL after the last renewal.
		slog.Error("dispatch outbox: release the publisher hold", "instance", o.instance, "error", err)
	}
	if end == holdLost {
		slog.Info("dispatch outbox: lost the publisher hold", "instance", o.instance)
		return end
	}
	slog.Info("dispatch outbox: released the publisher hold", "instance", o.instance, "reason", string(end))
	return end
}

// serve scans while this process holds hold: once at once, then on each wake, while the NATS
// client is connected. A scan runs its batches side by side and returns only once every one of
// them has, so serve answers a lost hold, a disconnection or ctx's end between scans and never
// while a batch is still sending. It returns why it stopped: ctx ended, the hold was lost or its
// deadline passed mid-batch, or NATS was lost - down for natsLossGrace without a break and still
// down when the grace ends, or a batch's sends failed for the connection and a probe of that
// connection failed too.
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
				// The client's state is read again before waiting: a scan takes the wakes that
				// reach it while it runs (scan), the change of this connection among them, so the
				// loop would otherwise wait on a grace it never armed.
				continue
			}
		}
		select {
		case <-ctx.Done():
			return stopping
		case <-hold.Lost():
			return holdLost
		case <-disconnectedFor:
			// A reconnect's event can wait in nats.go's callback queue behind a slow callback
			// queued before it, so ConnectionChanged has not closed although the client is
			// connected again: the client's own status decides, and a connected one serves on and
			// scans, as the reconnect's wake would have it do.
			if !o.deps.Publisher.Connected() {
				return natsLost
			}
			disconnected, disconnectedFor = nil, nil
			due = true
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

// scan reads and publishes batches under hold until there is nothing more to send. The batches
// run side by side, concurrentBatches of them at most: scan starts one at once, another whenever
// the batch that returned published anything or was full, and another at every wake that reaches
// it while batches run - an owner a batch gave back (lanes), an insert of events, a reconnect.
// Each batch leaves the owners the others hold alone (readBatch), so a batch waiting for a role
// holder's receipt holds back its own owners' later events and nothing else, while another
// owner's batch is read, sent and recorded beside it.
//
// It starts no batch once hold's deadline has passed, and none after one was fenced, lost NATS,
// failed or came back blocked; a batch that found nothing more to read ends nothing, since the
// batches still running give owners back. A fenced batch, one that lost NATS and one whose
// statement failed also end the batches still running (their context), so a hold that must go to
// another process is not held by a send waiting out a connection that will not come back; each
// settles the sends it had in flight as lost, records what NATS answered and releases the rest,
// as a stopping process's batch does. It returns why it stopped, once every batch it started
// has returned, each fenced by the hold's deadline itself. A wake it took and did not use is
// given back (signal), so the scan serve starts next sees it. Every batch that sent anything is
// logged as a sent batch with this process's instance, the span of event ids it sent, how many
// events it sent at least one envelope of (sent) and how many it marked published (published).
func (o *Outbox) scan(ctx context.Context, hold fence) scanEnd {
	deps := o.deps
	deps.lanes = newLanes(o.signal)
	// The batches' own context: a hard stop ends the ones still running at once, while ctx, which
	// is the process's, still tells a stop from an end of this scan's own making.
	batches, endBatches := context.WithCancel(ctx)
	defer endBatches()
	done := make(chan ranBatch, concurrentBatches)
	var end scanEnd
	inflight, due, stop, missed := 0, true, false, false
	for {
		if due && !stop && inflight < concurrentBatches {
			due = false
			switch {
			case ctx.Err() != nil:
				stop = true
			case !time.Now().Before(hold.Deadline()):
				end.fenced, stop = true, true
			default:
				inflight++
				o.claims.Add(1)
				go func() {
					ran := ranBatch{}
					ran.batch, ran.err = scanBatch(batches, deps, hold, o.instance)
					done <- ran
				}()
			}
		}
		if inflight == 0 {
			if missed && !stop {
				// A batch read past the owners the others held, and they have given those lanes
				// back since: read once more rather than wait for a wake that may already have
				// been spent on that batch. The read that follows leaves no owner out, since
				// none is in flight, so it sets no missed read of its own; only a batch a wake
				// starts beside it can.
				missed, due = false, true
				continue
			}
			if due {
				o.signal()
			}
			return end
		}
		select {
		case <-o.wake:
			due = true
		case ran := <-done:
			inflight--
			batch := ran.batch
			if batch.sent > 0 {
				slog.Info("dispatch outbox: sent batch", "instance", o.instance,
					"sent", batch.sent, "published", batch.published, "first_id", batch.firstID, "last_id", batch.lastID, "started", batch.started)
			}
			switch {
			case ran.err != nil:
				// A batch this scan ended, or the process's stop ended, reports that end and not
				// a failure of its own.
				if batches.Err() == nil {
					slog.Error("dispatch outbox: scan", "instance", o.instance, "error", ran.err)
					end.failed = true
				}
				stop = true
				endBatches()
			case batch.fenced:
				end.fenced, stop = true, true
				endBatches()
			case batch.natsLost:
				end.natsLost, stop = true, true
				endBatches()
			case batch.blocked:
				stop = true
			case batch.published == 0 && batch.sendable < batchSize:
				// Nothing more to read now, which the batches still running may change: the scan
				// starts none of its own accord and returns once the last one has, unless a wake
				// or an owner one of them releases falls due first. A batch that read past the
				// owners the others held may have left their events unread, so it is read again
				// once every batch has given its owners back.
				missed = missed || batch.excluded
			default:
				due = true
			}
		}
	}
}

// ranBatch is one batch a scan started, as it came back.
type ranBatch struct {
	batch batchResult
	err   error
}

// lanes is what the batches of one scan share: the owners whose events they hold in flight, the
// one statement at a time they run on the outbox's pool, and the wake a released owner gives the
// scan. Each owner is a lane of its own - a batch takes an owner by reading its events and gives
// it back once that owner's sends are settled and recorded (publishBatch), and no other batch
// reads that owner meanwhile - so an owner's events keep their order across the batches running
// side by side, and an owner waiting on a receipt holds back only itself.
type lanes struct {
	// mu guards inFlight. A batch's read is made under it, so that the read and the taking of the
	// owners it read are one act and two batches can never take one owner.
	mu       sync.Mutex
	inFlight map[string]struct{}
	// statements is the outbox pool's one statement at a time. The pool has two connections, one
	// of them the publisher hold's renewal, so a renewal would queue behind two batches running a
	// statement each. It is held around a statement, the drain of its rows included, and around
	// the sequence of statements one lookup read makes (loadLookups), and never across a send.
	statements sync.Mutex
	// wake is the scan's wake (Outbox.signal), given when an owner is released, so its next events
	// and the owners a full batch left behind are read at once.
	wake func()
}

// newLanes is the lanes of one scan, whose released owners wake it through wake.
func newLanes(wake func()) *lanes {
	return &lanes{inFlight: map[string]struct{}{}, wake: wake}
}

// reserve runs read with the owners in flight, which read leaves alone, and takes the owners of
// the rows it read, both under one lock. excluded reports that owners were left out of the read,
// so the scan knows the batch may have read past their events and reads again once they are
// given back (Outbox.scan). A batch run with no lanes - one by itself, as scanBatch's other
// callers run it - leaves no owner out and takes none.
func (l *lanes) reserve(read func(inFlight []string) ([]pendingEvent, bool, error)) (pending []pendingEvent, stopped, excluded bool, err error) {
	if l == nil {
		pending, stopped, err = read([]string{})
		return pending, stopped, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	inFlight := make([]string, 0, len(l.inFlight))
	for owner := range l.inFlight {
		inFlight = append(inFlight, owner)
	}
	pending, stopped, err = read(inFlight)
	for _, item := range pending {
		if item.owner != "" {
			l.inFlight[item.owner] = struct{}{}
		}
	}
	return pending, stopped, len(inFlight) > 0, err
}

// release gives owner's lane back, telling the scan nothing: a batch that gives its lanes back as
// it ends is read by the scan as the batch it returns, and a wake there would have the scan read
// again at once what a batch that stopped for NATS just released.
func (l *lanes) release(owner string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.inFlight, owner)
	l.mu.Unlock()
}

// releaseEarly gives owner's lane back before its batch has ended and wakes the scan, so the
// owner's next events and the owners its batch left behind are read while that batch is still
// sending.
func (l *lanes) releaseEarly(owner string) {
	if l == nil {
		return
	}
	l.release(owner)
	l.wake()
}

// lock takes the pool's one statement at a time, and unlock gives it back. Nothing takes mu while
// holding it: a batch's read takes mu first and this second, and every other statement takes this
// alone, so the two orders never cross.
func (l *lanes) lock() {
	if l != nil {
		l.statements.Lock()
	}
}

func (l *lanes) unlock() {
	if l != nil {
		l.statements.Unlock()
	}
}

// statement runs fn on the outbox's pool with no other batch's statement beside it: one
// statement, or the run of them a lookup read makes, which holds the gate from its first to its
// last.
func (l *lanes) statement(fn func() error) error {
	l.lock()
	defer l.unlock()
	return fn()
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
