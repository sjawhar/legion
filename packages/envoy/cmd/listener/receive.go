package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The receive loop's timing.
//
// A pod that takes the lease over an expired record nobody released (its holder was killed or
// lost its node) waits one consumerAckWait before it subscribes, so the messages the dead holder
// had received come back to the durable, and reach this pod, before newer ones: a session keeps
// the order of its events across a hard take-over.
//
// A pod that takes the lease from an absent or released record and finds the durable push-bound
// finds a listener build from before the lease bound to it with nats.Bind, during the first roll
// onto this build: a released holder has already unsubscribed, and an absent record means no
// holder of this build. It polls the durable every durableBindPoll, renewing its lease meanwhile,
// and subscribes once the binding is gone. Waiting past durableTakeoverAlarmAfter, it logs
// durableTakeoverAlarmLine at ERROR, which the deployed alarm matches, and again every
// durableTakeoverAlarmEvery; it never exits for this, since a restart would wait the same.
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
	// receiveQueueCapacity is the local queue's: the durable sends no more than MaxAckPending
	// messages the holder has not settled, so the queue never fills; twice that leaves room for a
	// redelivery that lands as a NAK is being processed.
	receiveQueueCapacity = 2 * consumerMaxAckPending
)

// listenerLeaseUnavailable is what /healthz says, and the start logs at ERROR, about a listener
// whose NATS user may not use the listener state bucket. It then delivers without a lease, as the
// machine's one deliverer, receiving only while no other subscriber holds the durable.
const listenerLeaseUnavailable = "unavailable: missing NATS grant for KV bucket " + bus.ListenerStateBucket

// receiveConfig carries the receive loop's timing, so a test of this package shortens it;
// productionReceiveConfig is the only value the listener runs with.
type receiveConfig struct {
	ackWait         time.Duration
	bindPoll        time.Duration
	alarmAfter      time.Duration
	alarmEvery      time.Duration
	inProgressEvery time.Duration
	inflightGrace   time.Duration
}

func productionReceiveConfig() receiveConfig {
	return receiveConfig{
		ackWait:         consumerAckWait,
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
	// receiveAwaitingAckWait: it took the lease over an expired holder and waits one ack wait.
	receiveAwaitingAckWait
	// receiveAwaitingBinding: the durable is push-bound by a subscriber without the lease.
	receiveAwaitingBinding
	// receiveActive: it is subscribed to the durable's deliver subject and delivering.
	receiveActive
)

func (s receiveState) String() string {
	switch s {
	case receivePreparing:
		return "preparing the durable"
	case receiveAwaitingAckWait:
		return "waiting one ack wait after an expired holder"
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
	// than received beside.
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
		r.serve(ctx, ctx, leaseTakenAbsent)
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
	return r.serve(ctx, termCtx, status.How)
}

// serve receives the durable for one term, which termCtx bounds: it reads the durable, waits as
// how the lease was taken requires, corrects the durable's policy, subscribes, and delivers until
// termCtx ends. ctx is the receive loop's own, whose end is a shutdown. It reports whether the
// durable was refused.
//
// The policy is written only once nothing but this pod will receive: a pre-lease build that still
// holds the durable binds it again after a reconnect with nats.Bind, which refuses a durable whose
// filter is not its own ("subject does not match consumer").
func (r *durableReceiver) serve(ctx, termCtx context.Context, how leaseAcquisition) bool {
	defer r.setState(receiveStandby)
	start := time.Now()
	r.setState(receivePreparing)
	info, err := r.prepare(termCtx, false)
	if err != nil {
		return r.refuse(err)
	}
	expired := how == leaseTakenExpired && r.lease != nil && !r.awaitedBinding
	switch {
	case expired:
		r.setState(receiveAwaitingAckWait)
		r.logger.Info("listener lease taken over an expired holder; receiving after one ack wait",
			slog.String("consumer", r.consumer), slog.String("ack_wait", r.timing.ackWait.String()))
		if waitUnlessDone(termCtx, r.timing.ackWait) != nil {
			return false
		}
	case info.PushBound:
		if err = r.awaitUnbound(termCtx); err != nil {
			return r.refuse(err)
		}
	}
	r.setState(receivePreparing)
	for {
		if info, err = r.prepare(termCtx, true); err != nil {
			return r.refuse(err)
		}
		if expired && info.PushBound {
			expired = false
			r.logger.Info("durable still push-bound by the expired holder; receiving beside it", slog.String("consumer", r.consumer))
		}
		err := r.begin(info.Config.DeliverSubject)
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
		taken = how.String()
	}
	r.logger.Info("durable receiving",
		slog.String("consumer", r.consumer),
		slog.String("deliver_subject", info.Config.DeliverSubject),
		slog.String("lease", taken),
		slog.Int64("waited_ms", time.Since(start).Milliseconds()),
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
// connection, the serial handler and the InProgress ticker.
func (r *durableReceiver) begin(subject string) error {
	t := &receiveTerm{
		subject:   subject,
		queue:     make(chan *nats.Msg, receiveQueueCapacity),
		unsettled: map[*nats.Msg]struct{}{},
		closing:   make(chan struct{}),
		handled:   make(chan struct{}),
		stopped:   make(chan struct{}),
		logger:    r.logger,
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
// the durable redelivers them in the order they came.
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

	mu        sync.Mutex
	closed    bool
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

// takeQueued empties the queue and returns what it held, no longer counted unsettled.
func (t *receiveTerm) takeQueued() []*nats.Msg {
	t.mu.Lock()
	defer t.mu.Unlock()
	var queued []*nats.Msg
	for {
		select {
		case m := <-t.queue:
			delete(t.unsettled, m)
			queued = append(queued, m)
		default:
			return queued
		}
	}
}

func (t *receiveTerm) settled(m *nats.Msg) {
	t.mu.Lock()
	delete(t.unsettled, m)
	t.mu.Unlock()
}

// handle is the term's serial handler: it delivers each queued message in turn, while this pod
// holds the lease. A message received while it does not (the window before an unsubscribe lands,
// or a subscription nats.go sent again after a reconnect in place) is dropped with no ack and no
// NAK, so the durable redelivers it after the ack wait to whoever holds the lease then.
func (t *receiveTerm) handle(r *durableReceiver) {
	defer close(t.handled)
	for {
		select {
		case <-t.closing:
			return
		default:
		}
		select {
		case <-t.closing:
			return
		case m := <-t.queue:
			if r.holding() {
				r.handle(t.handling, m)
			}
			t.settled(m)
		}
	}
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
