package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The receive loop's timing.
//
// A term that must settle (leaseStatus.Settle: the lease was taken over an expired record nobody
// released, its holder killed or its node lost, or this pod resumed after its renewals lapsed, or
// from a record that says a pod of this build may still hold messages or a push binding) subscribes
// at once, beside any push binding, and holds every message it receives, keeping them alive with
// InProgress, until every message the durable has outstanding is in its hands: it reads the
// durable every settlePoll and ends the hold once NumAckPending is no more than the messages it
// holds, or at the cap of two consumerAckWait and settleMargin, 13 s. Then it hands the held batch
// to the serial handler in stream order before it goes on live. The messages the dead holder had
// received, and the ones the server went on pushing to a lost node's connection, come back once
// their ack wait runs out, so they arrive in the held batch beside the newer ones, and the sort
// puts them back before them: a session keeps the order of its events across a hard take-over.
// With nothing outstanding the hold ends at its first read. A message a handler NAKed with a delay
// stays outstanding until its delay ends, which holds the hold to its cap.
//
// A pod that takes the lease from an absent or released record with neither stale_binding nor
// unsettled and finds the durable push-bound finds a listener build from before the lease bound to
// it with nats.Bind, during the first roll onto this build: a released holder has already
// unsubscribed, and an absent record means no holder of this build. It polls the durable every
// durableBindPoll, renewing its lease meanwhile, and subscribes once the binding is gone. Waiting
// past durableTakeoverAlarmAfter, it logs durableTakeoverAlarmLine at ERROR, which the deployed
// alarm matches, and again every durableTakeoverAlarmEvery; it never exits for this, since a
// restart would wait the same.
//
// The holder sends InProgress every inProgressEvery for every message it has received and not
// settled, queued or in hand, so none waits out the ack wait while this pod is alive. On a
// shutdown the message in hand gets inflightGrace to finish before its handling context is
// cancelled, which aborts a prompt in flight; inflightCancelWait bounds the wait for the handler
// to return after that.
const (
	durableTakeoverAlarmLine  = "lease holder cannot deliver from the durable"
	durableTakeoverAlarmAfter = 135 * time.Second
	durableTakeoverAlarmEvery = 5 * time.Minute
	durableBindPoll           = time.Second
	inProgressEvery           = 2 * time.Second
	inflightGrace             = 3 * time.Second
	inflightCancelWait        = time.Second
	// holderLoopsStopWait bounds how long the end of a term waits for a pass of the holder-only
	// loops in flight, measured from when they were told to stop.
	holderLoopsStopWait = 5 * time.Second
	// settlePoll is how often a settle hold reads the durable's outstanding messages.
	settlePoll = 250 * time.Millisecond
	// settleMargin is how much longer than two ack waits a settle hold lasts at most. A message
	// a dead holder had in hand comes back one ack wait after its last InProgress; one the server
	// redelivered before this pod subscribed went to a lost node's lingering connection alone and
	// comes back one ack wait later again. The margin covers a redelivery the server sends a
	// moment after its ack wait runs out.
	settleMargin = time.Second
	// receiveQueueCapacity is the local queue's: the durable sends no more than MaxAckPending
	// messages the holder has not settled, so the queue never fills; twice that leaves room for a
	// redelivery that lands as a NAK is being processed.
	receiveQueueCapacity = 2 * consumerMaxAckPending
)

// listenerLeaseUnavailable is what /healthz says about a listener whose NATS user may not use the
// listener state bucket. It then delivers without a lease, as the machine's one deliverer,
// receiving only while no other subscriber holds the durable.
const listenerLeaseUnavailable = "unavailable: missing NATS grant for KV bucket " + bus.ListenerStateBucket

// receiveConfig carries the receive loop's timing, so a test of this package shortens it;
// productionReceiveConfig is the only value the listener runs with.
type receiveConfig struct {
	// settlePoll is how often a settle hold reads the durable, and settleCap the longest it lasts.
	settlePoll      time.Duration
	settleCap       time.Duration
	bindPoll        time.Duration
	alarmAfter      time.Duration
	alarmEvery      time.Duration
	inProgressEvery time.Duration
	inflightGrace   time.Duration
}

func productionReceiveConfig() receiveConfig {
	return receiveConfig{
		settlePoll:      settlePoll,
		settleCap:       2*consumerAckWait + settleMargin,
		bindPoll:        durableBindPoll,
		alarmAfter:      durableTakeoverAlarmAfter,
		alarmEvery:      durableTakeoverAlarmEvery,
		inProgressEvery: inProgressEvery,
		inflightGrace:   inflightGrace,
	}
}

// receiveState is where the receive loop stands.
type receiveState int

const (
	// receiveStandby: this pod does not hold the lease (or, without one, has not started).
	receiveStandby receiveState = iota
	// receivePreparing: it holds the lease and is reading, creating or correcting the durable.
	receivePreparing
	// receiveAwaitingBinding: the durable is push-bound by a subscriber without the lease.
	receiveAwaitingBinding
	// receiveActive: it is subscribed to the durable's deliver subject and delivering.
	receiveActive
)

func (s receiveState) String() string {
	switch s {
	case receivePreparing:
		return "preparing the durable"
	case receiveAwaitingBinding:
		return "waiting for another subscriber's push binding to end"
	case receiveActive:
		return "receiving"
	default:
		return "standby"
	}
}

// receiveReport is the receive loop at one instant, as /healthz reads it.
type receiveReport struct {
	State receiveState
	// Subject is the deliver subject the active subscription is on, and SubscriptionValid whether
	// that subscription is valid on the client's current connection; both are zero unless State
	// is receiveActive.
	Subject           string
	SubscriptionValid bool
	// NoLease is set when the listener runs without a lease (listenerLeaseUnavailable), and Lease
	// is the lease's status otherwise.
	NoLease bool
	Lease   leaseStatus
}

// durableReceiverOptions is what a durableReceiver is made of.
type durableReceiverOptions struct {
	client   *bus.Client
	consumer string
	// lease is this pod's delivery lease; nil runs without one (listenerLeaseUnavailable).
	lease *lease
	// handle delivers one message and settles it.
	handle durableHandler
	// loops starts the loops only the deliverer runs, under ctx, and returns a channel per loop
	// that closes once it has returned; nil starts none.
	loops func(ctx context.Context) []<-chan struct{}
	// refused is called once the durable is refused (errListenerDurableRefused); the receive loop
	// ends after it.
	refused func(error)
	logger  *logging.Logger
	timing  receiveConfig
}

// durableReceiver receives the machine's durable while this pod holds the delivery lease: it
// subscribes to the durable's deliver subject with a core subscription (never nats.Bind, never
// through the bus client's JetStream subscription slot), hands the messages one at a time to the
// delivery handler, keeps every unsettled one alive with InProgress, and unsubscribes when the
// lease is lost or released. Without a lease it receives whenever no other subscriber holds the
// durable.
type durableReceiver struct {
	durableReceiverOptions

	// reconnects signals that the client's connection came back or was replaced (reconnected).
	reconnects chan struct{}
	// awaitedBinding is set, by the receive loop alone, while this pod has waited out a push
	// binding without the lease and not yet subscribed. A term taken over an expired record then
	// is this pod's own after its renewals lapsed (lease.go counts that resume as expired), and the
	// binding is still the pre-lease build's, not a dead holder's: it is waited out again rather
	// than received beside, and the term does not settle. A term taken from a released record
	// carrying stale_binding or unsettled is another holder's word that the binding is this
	// build's, and settles regardless.
	awaitedBinding bool

	mu    sync.Mutex
	conn  *nats.Conn
	state receiveState
	term  *receiveTerm
	stop  context.CancelFunc
	done  chan struct{}
}

func newDurableReceiver(options durableReceiverOptions) *durableReceiver {
	return &durableReceiver{
		durableReceiverOptions: options,
		reconnects:             make(chan struct{}, 1),
		conn:                   options.client.Conn,
	}
}

// Start runs the receive loop until Stop. Start is called once; the lease, when there is one, is
// started by the caller.
func (r *durableReceiver) Start() {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.stop, r.done = stop, done
	r.mu.Unlock()
	go func() {
		defer close(done)
		r.run(ctx)
	}()
}

// Stop ends receiving in the shutdown's order: the holder-only loops are told to stop, the
// subscription ends, the message in hand gets inflightGrace before its handling is cancelled,
// every queued message is NAKed for the next holder, and then the lease is marked released, so a
// standby takes it at its next poll. It returns once that is done or ctx ends; a lease left
// unreleased at ctx's end is taken as an expired one.
func (r *durableReceiver) Stop(ctx context.Context) {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.mu.Unlock()
	if stop != nil {
		stop()
		select {
		case <-done:
		case <-ctx.Done():
			r.logger.Warn("durable receive loop still settling at shutdown")
		}
	}
	if r.lease == nil {
		return
	}
	if err := r.lease.Release(ctx); err != nil {
		r.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
}

// reconnected is the client's reconnect hook: conn is the client's connection after a reconnect
// in place or a replacement, which the receive loop moves the subscription onto when it is no
// longer valid there. It never fails, so it never holds up the client's recovery.
func (r *durableReceiver) reconnected(conn *nats.Conn) error {
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	select {
	case r.reconnects <- struct{}{}:
	default:
	}
	return nil
}

// holding reports whether this pod may deliver now: it holds the lease, or runs without one.
func (r *durableReceiver) holding() bool {
	return r.lease == nil || r.lease.Holding()
}

func (r *durableReceiver) setState(state receiveState) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

// report reads the receive loop now.
func (r *durableReceiver) report() receiveReport {
	r.mu.Lock()
	report := receiveReport{State: r.state, NoLease: r.lease == nil}
	if t := r.term; t != nil {
		report.Subject = t.subject
		report.SubscriptionValid = t.sub != nil && t.sub.IsValid() && t.conn == r.conn
	}
	r.mu.Unlock()
	if r.lease != nil {
		report.Lease = r.lease.Status()
	}
	return report
}

// run is the receive loop: without a lease it serves until ctx ends; with one it serves each term
// of the lease this pod acquires, from the acquisition until the term ends.
func (r *durableReceiver) run(ctx context.Context) {
	if r.lease == nil {
		r.serve(ctx, ctx, leaseStatus{How: leaseTakenAbsent})
		return
	}
	var served uint64
	for {
		if status := r.lease.Status(); status.Holding && status.Term != served {
			served = status.Term
			if r.serveTerm(ctx, status) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.lease.Changes():
		case <-r.reconnects:
		}
	}
}

// serveTerm serves one term of the lease, under a context that ends when the term does (the lease
// is lost, or taken again as a new term) or ctx does. It reports whether the durable was refused.
func (r *durableReceiver) serveTerm(ctx context.Context, status leaseStatus) bool {
	termCtx, cancel := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-termCtx.Done():
				return
			case <-r.lease.Changes():
				if now := r.lease.Status(); !now.Holding || now.Term != status.Term {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watched
	}()
	return r.serve(ctx, termCtx, status)
}

// serve receives the durable for one term, which termCtx bounds and status describes: it reads the
// durable, waits out a pre-lease build's binding when the term does not settle, corrects the
// durable's policy, subscribes, and delivers until termCtx ends; a term that settles subscribes at
// once, beside any binding, and holds what it receives until every outstanding message is in hand
// (receiveTerm.settle), then delivers that in stream order. ctx is the receive loop's own, whose
// end is a shutdown. It reports whether the durable was refused.
//
// Before it subscribes it records stale_binding in the lease: set when it subscribes beside a
// binding it did not wait out, cleared when it finds the durable not push-bound.
//
// The policy is written only once nothing but this pod will receive: a pre-lease build that still
// holds the durable binds it again after a reconnect with nats.Bind, which refuses a durable whose
// filter is not its own ("subject does not match consumer").
func (r *durableReceiver) serve(ctx, termCtx context.Context, status leaseStatus) bool {
	defer r.setState(receiveStandby)
	start := time.Now()
	r.setState(receivePreparing)
	info, err := r.prepare(termCtx, false)
	if err != nil {
		return r.refuse(err)
	}
	settle := status.Settle && !(r.awaitedBinding && status.How == leaseTakenExpired)
	var settleCap time.Duration
	if settle {
		settleCap = r.timing.settleCap
		r.logger.Info("listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand",
			slog.String("consumer", r.consumer), slog.String("lease", status.How.String()), slog.String("cap", settleCap.String()))
	} else {
		if status.Settle {
			r.lease.Settled(status.Term)
		}
		if info.PushBound {
			if err = r.awaitUnbound(termCtx); err != nil {
				return r.refuse(err)
			}
		}
	}
	r.setState(receivePreparing)
	beside := false
	for {
		if info, err = r.prepare(termCtx, true); err != nil {
			return r.refuse(err)
		}
		switch {
		case r.lease == nil:
		case !info.PushBound:
			r.lease.SetStaleBinding(status.Term, false)
		case settle:
			r.lease.SetStaleBinding(status.Term, true)
			if !beside {
				beside = true
				r.logger.Info("durable still push-bound by a lease holder that is gone; receiving beside it", slog.String("consumer", r.consumer))
			}
		}
		err := r.begin(info.Config.DeliverSubject, settleCap, status.Term)
		if err == nil {
			r.awaitedBinding = false
			break
		}
		r.logger.Warn("durable subscribe failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		if waitUnlessDone(termCtx, r.timing.bindPoll) != nil {
			return false
		}
	}
	taken := "unavailable"
	if r.lease != nil {
		taken = status.How.String()
	}
	r.logger.Info("durable receiving",
		slog.String("consumer", r.consumer),
		slog.String("deliver_subject", info.Config.DeliverSubject),
		slog.String("lease", taken),
		slog.Int64("waited_ms", time.Since(start).Milliseconds()),
		slog.Bool("settle", settle),
	)
	loopsCtx, stopLoops := context.WithCancel(context.Background())
	var loops []<-chan struct{}
	if r.loops != nil {
		loops = r.loops(loopsCtx)
	}
	for {
		select {
		case <-termCtx.Done():
			stopped := time.Now()
			stopLoops()
			r.end(ctx)
			waitLoops(r.logger, loops, stopped.Add(holderLoopsStopWait))
			return false
		case <-r.reconnects:
			r.repair(termCtx)
		}
	}
}

// refuse reports a refused durable, the one error prepare returns besides its context's end, and
// whether it was one.
func (r *durableReceiver) refuse(err error) bool {
	if !errors.Is(err, errListenerDurableRefused) {
		return false
	}
	if r.refused != nil {
		r.refused(err)
	}
	return true
}

// prepare reads the durable, creating it when it is missing and, when correct is set, correcting
// its policy when it has drifted (correctListenerDurable), retrying every bindPoll until it
// answers, and records its deliver subject in the lease. It returns a refusal
// (errListenerDurableRefused) or ctx's error.
func (r *durableReceiver) prepare(ctx context.Context, correct bool) (*nats.ConsumerInfo, error) {
	failing := false
	for {
		info, err := listenerDurable(ctx, r.client, r.consumer)
		if err == nil && (correct || info == nil) {
			info, err = correctListenerDurable(ctx, r.client, r.consumer, info)
		}
		if err == nil {
			if failing {
				r.logger.Info("durable consumer read recovered", slog.String("consumer", r.consumer))
			}
			if r.lease != nil {
				r.lease.SetDeliverSubject(info.Config.DeliverSubject)
			}
			return info, nil
		}
		if errors.Is(err, errListenerDurableRefused) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !failing {
			failing = true
			r.logger.Warn("durable consumer read failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		}
		if err := waitUnlessDone(ctx, r.timing.bindPoll); err != nil {
			return nil, err
		}
	}
}

// awaitUnbound waits, polling the durable every bindPoll without writing it, until no subscriber
// holds its push binding. It logs durableTakeoverAlarmLine at ERROR once it has waited alarmAfter,
// and again every alarmEvery, and never gives up: only ctx's end or a refusal ends it.
func (r *durableReceiver) awaitUnbound(ctx context.Context) error {
	r.setState(receiveAwaitingBinding)
	r.awaitedBinding = true
	r.logger.Info("durable is push-bound by a subscriber without the lease; waiting for its binding to end",
		slog.String("consumer", r.consumer))
	start := time.Now()
	var alarmed time.Time
	for {
		if err := waitUnlessDone(ctx, r.timing.bindPoll); err != nil {
			return err
		}
		info, err := r.prepare(ctx, false)
		if err != nil {
			return err
		}
		waited := time.Since(start)
		if !info.PushBound {
			r.logger.Info("durable push binding ended", slog.String("consumer", r.consumer), slog.Int64("waited_ms", waited.Milliseconds()))
			return nil
		}
		if waited >= r.timing.alarmAfter && (alarmed.IsZero() || time.Since(alarmed) >= r.timing.alarmEvery) {
			alarmed = time.Now()
			r.logger.Error(durableTakeoverAlarmLine,
				slog.String("consumer", r.consumer),
				slog.String("reason", "another subscriber holds the durable's push binding"),
				slog.Int64("waited_ms", waited.Milliseconds()),
			)
		}
	}
}

// begin starts a term's receiving: a core subscription on subject on the client's current
// connection, the serial handler and the InProgress ticker. A settleCap above zero has the handler
// first hold what arrives until the durable's outstanding messages are all in hand, or for
// settleCap at most, then deliver it in stream order (receiveTerm.settle); term is the lease term
// whose unsettled the hold's end clears.
func (r *durableReceiver) begin(subject string, settleCap time.Duration, term uint64) error {
	t := &receiveTerm{
		subject:   subject,
		queue:     make(chan *nats.Msg, receiveQueueCapacity),
		unsettled: map[*nats.Msg]struct{}{},
		closing:   make(chan struct{}),
		handled:   make(chan struct{}),
		stopped:   make(chan struct{}),
		logger:    r.logger,
		settleCap: settleCap,
		leaseTerm: term,
		holding:   settleCap > 0,
	}
	t.handling, t.cancel = context.WithCancel(context.Background())
	r.mu.Lock()
	conn := r.conn
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		r.mu.Unlock()
		t.cancel()
		return err
	}
	t.sub, t.conn = sub, conn
	r.term, r.state = t, receiveActive
	r.mu.Unlock()
	// The server counts the subscription once it has read it; a flush that fails, while the
	// connection is down, leaves it to nats.go to send again on the reconnect.
	_ = conn.FlushTimeout(time.Second)
	go t.handle(r)
	go t.keepAlive(r.timing.inProgressEvery)
	return nil
}

// end ends the term being served: it unsubscribes and stops taking messages, then settles. On a
// lost lease (ctx still live) the queued messages are dropped without an ack, to come back after
// the ack wait to whoever holds the lease, and the message in hand finishes, unless a shutdown
// comes meanwhile. On a shutdown (ctx ended) the message in hand gets inflightGrace, then its
// handling is cancelled, and every queued message is NAKed for the next holder, in that order so
// the durable redelivers them in the order they came; during a settle hold, the held messages
// are NAKed in stream order, and the lease's unsettled stays set for the next holder.
func (r *durableReceiver) end(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	r.term = nil
	r.state = receiveStandby
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
	t.cancel()
	close(t.stopped)
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

// repair moves the term's subscription onto the client's current connection when it is no longer
// valid there: the client replaced its connection, or the subscription ended.
func (r *durableReceiver) repair(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	stale := t != nil && (t.sub == nil || !t.sub.IsValid() || t.conn != r.conn)
	r.mu.Unlock()
	if !stale {
		return
	}
	if err := r.resubscribe(ctx); err != nil {
		r.logger.Warn("durable resubscribe failed; the self-health monitor retries it", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
	}
}

// resubscribe moves the term's subscription onto the durable's deliver subject on the client's
// current connection, creating the durable through ensureListenerDurable when it is missing. A
// pod that is not receiving has nothing to move.
func (r *durableReceiver) resubscribe(ctx context.Context) error {
	r.mu.Lock()
	receiving := r.term != nil
	r.mu.Unlock()
	if !receiving {
		return nil
	}
	info, err := ensureListenerDurable(ctx, r.client, r.consumer)
	if err != nil {
		return fmt.Errorf("read the durable consumer: %w", err)
	}
	subject := info.Config.DeliverSubject
	r.mu.Lock()
	t := r.term
	if t == nil {
		r.mu.Unlock()
		return nil
	}
	if t.sub != nil {
		_ = t.sub.Unsubscribe()
	}
	conn := r.conn
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		t.sub = nil
		r.mu.Unlock()
		return fmt.Errorf("subscribe to the durable's deliver subject: %w", err)
	}
	t.sub, t.conn, t.subject = sub, conn, subject
	r.mu.Unlock()
	if r.lease != nil {
		r.lease.SetDeliverSubject(subject)
	}
	_ = conn.FlushTimeout(time.Second)
	r.logger.Info("durable resubscribed", slog.String("consumer", r.consumer), slog.String("deliver_subject", subject))
	return nil
}

// probe is the self-health monitor's durable check, which only the receiving pod makes: a standby
// has no subscription to check, and probes healthy. On the receiving pod a consumer that is gone
// is nats.ErrConsumerNotFound, and one whose deliver subject is not the subscription's, or whose
// subscription is no longer valid on the client's connection, errDurableUnbound; both are
// terminal, so the monitor's rebuild resubscribes. A healthy durable whose policy has drifted is
// corrected, and one the policy refuses (listenerDurableRefusal) is reported as its refusal, which
// the monitor logs and does not rebuild. Any other read error is reported as success, deliberately:
// a JetStream deadline or a reconnect gap must not count as a terminal failure, since the monitor
// splits its counts on it. Each read runs under ctx, so the shutdown cuts one the server has not
// answered.
//
// A mismatch is confirmed by a second read, durableProbeConfirm later, since a read can land in
// the receive loop's own resubscribe.
func (r *durableReceiver) probe(ctx context.Context) error {
	for read := range 2 {
		if read > 0 && waitUnlessDone(ctx, durableProbeConfirm) != nil {
			return nil
		}
		if report := r.report(); report.State != receiveActive {
			return nil
		}
		info, err := listenerDurable(ctx, r.client, r.consumer)
		switch {
		case errors.Is(err, errListenerDurableRefused):
			return err
		case err != nil:
			return nil
		case info == nil:
			return nats.ErrConsumerNotFound
		}
		report := r.report()
		if report.State != receiveActive {
			return nil
		}
		if report.SubscriptionValid && info.Config.DeliverSubject == report.Subject {
			if _, err := correctListenerDurable(ctx, r.client, r.consumer, info); err != nil {
				return fmt.Errorf("correct the durable consumer's policy: %w", err)
			}
			return nil
		}
	}
	return errDurableUnbound
}

// waitLoops waits for each loop to return, until deadline.
func waitLoops(logger *logging.Logger, loops []<-chan struct{}, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for _, done := range loops {
		select {
		case <-done:
		case <-timer.C:
			logger.Warn("a holder-only loop was still running when receiving ended")
			return
		}
	}
}

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
	// closing is closed once the term takes no more messages, handled once the handler has
	// returned, and stopped once the term is over, which ends the InProgress ticker.
	closing chan struct{}
	handled chan struct{}
	stopped chan struct{}
	logger  *logging.Logger
	// settleCap, when above zero, is the longest the handler holds what the term receives before
	// it delivers it in stream order (settle): a term that must settle. leaseTerm is the lease
	// term the hold's end reports settled.
	settleCap time.Duration
	leaseTerm uint64

	mu     sync.Mutex
	closed bool
	// holding is set while the settle hold lasts.
	holding bool
	// held is the batch the hold gathered, sorted by stream sequence, which the handler delivers
	// before it reads the queue again.
	held      []*nats.Msg
	unsettled map[*nats.Msg]struct{}
}

// intake is the core subscription's handler: it queues m, in the order the durable sent it, and
// counts it unsettled from then on, so the InProgress ticker keeps it alive while it waits.
func (t *receiveTerm) intake(m *nats.Msg) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	select {
	case t.queue <- m:
		t.unsettled[m] = struct{}{}
	default:
		t.logger.Warn("durable message dropped: the local queue is full", slog.Int("capacity", cap(t.queue)))
	}
}

// close stops the term taking messages.
func (t *receiveTerm) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	close(t.closing)
}

// takeQueued empties the held batch and the queue and returns what they held, in the order the
// handler would have taken it, no longer counted unsettled: during a settle hold, all of it in
// stream order.
func (t *receiveTerm) takeQueued() []*nats.Msg {
	t.mu.Lock()
	defer t.mu.Unlock()
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
	if t.settleCap > 0 && !t.settle(r) {
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
// connection, comes back once its ack wait runs out, after newer messages pushed to this pod, and
// the stream-order sort puts it back before them. The batch is gathered before each read, so
// every message it counts was still outstanding at the read. A read that fails keeps the hold.
// The hold ends at settleCap regardless, and its end clears the lease term's unsettled. It reports
// false when the term closed first.
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
	limit := time.NewTimer(t.settleCap)
	defer limit.Stop()
	poll := time.NewTicker(r.timing.settlePoll)
	defer poll.Stop()
	outcome, pending, failing := "cap", -1, false
read:
	for {
		held := t.gather(false)
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
	)
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
