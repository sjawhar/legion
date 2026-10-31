package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
// holds, or at its cap, counted from the subscribe. Then it hands the held batch to the serial
// handler in stream order before it goes on live. The cap is what the previous holder can still
// have outstanding then: consumerAckWait and settleMargin, 7 s, since the messages a dead holder
// had received come back one ack wait after its last InProgress, and the ones the server pushed
// to a lost node's connection alone before this pod subscribed one ack wait after that push, so
// all are back one ack wait after the subscribe, in the held batch beside the newer ones, and the
// sort puts them back before them: a session keeps the order of its events across a hard
// take-over. A term taken from a clean hand-back ends its hold sooner, at handBackDelay and
// settleMargin, 3 s: the stopping holder NAKed everything it held with handBackDelay before its
// release, and NAKed what reached it with the same delay until it read this pod receiving, so
// nothing of it went to a lost node's binding, or to no subscriber at all, alone. That is not
// read from the lease record or from any clock: the stopping pod writes a mark for the record it
// released once its hand-over went through, and the hold reads that mark every settlePoll and
// ends at the later of the mark and 3 s after the subscribe (lease.HandBackMarked). A mark that
// is not there, or that cannot be read, holds the term to the 7 s cap, which is what a message
// that reached no subscriber of this machine's needs. With nothing outstanding the hold ends at
// its first read. A message a handler NAKed with a longer delay (a retry) stays outstanding until
// its delay ends, so the hold runs to its cap, or to its mark, and the retry arrives after it, as
// it would have on the previous holder.
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
// A holder that cannot read, create, correct or subscribe to the durable retries every
// durableBindPoll and times its failures from the start of the first since the term began or an
// attempt last succeeded (durableReceiver.failed). Once they have lasted durableTakeoverAlarmAfter
// it logs durableTakeoverAlarmLine at ERROR with the failure as its reason and gives the lease up
// (lease.GiveUp), so a standby takes it at its next poll and tries in its place; this pod goes back
// to standby and takes the lease again, after two standby polls, only when no other pod has. A
// listener without a lease has no one to give it to: it keeps retrying and logs the line again
// every durableTakeoverAlarmEvery.
//
// A holder whose receive connection goes down keeps none of that: the lease reads the signal
// this loop gives it (durableReceiver.receiveUp) and, once the connection has been down for
// leaseReceiveLost, releases the record, so a standby takes it while this pod's own connection
// to the lease bucket is still renewing (lease.releaseReceiveLost). The term then ends as any
// lost lease's does - the subscription on the dead connection is dropped and what is queued is
// left to come back at its ack wait (durableReceiver.endLost) - since no hand-back can be NAKed
// over a connection that is down. This pod takes no lease back while the connection stays down,
// and stands by as any other pod once it is up.
//
// The holder sends InProgress every inProgressEvery for every message it has received and not
// settled, queued or in hand, so none waits out the ack wait while this pod is alive. On a
// shutdown the message in hand gets inflightGrace to finish before its handling context is
// cancelled, which aborts a prompt in flight; inflightCancelWait bounds the wait for the handler
// to return after that.
//
// A holder's graceful stop hands its messages back (durableReceiver.handBack) and keeps its
// subscription until the next holder has one, since nats-server loses a message when a push
// consumer's last subscriber goes while a first delivery of it is still in the server's send queue
// (the consumer counts the delivery undone below zero, and the message leaves the pending list at
// its ack wait, never redelivered). It NAKs what it holds with handBackDelay, so the durable
// redelivers it once the next holder has subscribed, releases the lease unsettled, so the next
// holder settles, and then lingers: it NAKs, with the same delay, each message that reaches it, a
// repeat of one it already handed back included (handBackNaksMax NAKs of one message at most), and
// reads the lease record every lingerPoll until it names another holder that is receiving, or
// until lingerMax after the batch was NAKed. A record no other pod has taken ends the linger too:
// a running standby takes a released record at its next poll, so there is none to wait for. No end
// but a receiving holder comes before lingerUntaken and a poll have passed since the release, so
// a standby that is still taking the record has the deliver subject held for it. Then it drains
// its subscription, waiting handBackDrainWait at most for the server's word that it sends no
// more, and writes the lease's clean-hand-back mark for the record it released when all of this
// went through: its own release wrote that record, the term had no lost node's binding open, the
// message in hand's handler returned, the linger ended with another pod receiving, and the server
// confirmed the drain. The next holder's settle hold reads that mark and ends at 3 s rather than
// 7 s (lease.MarkHandBack, durableReceiver.handBack); a stop that falls short of any of it writes
// nothing and leaves the next holder the 7 s
// hold. handBackReleaseWait bounds the wait for the lease's loop to stop before the release, and
// listenerStateRequestWait the mark's write.
// The 3 s grace, its cancel's 1 s, lingerMax (the flush and the release inside it), the 2 s drain
// and the mark's 2 s write come to 11 s at most besides a lease call that hangs; a flush and a
// release that both run to their bounds (1 s and 2 s) leave the linger holding its floor past
// that cap, 1.25 s after the release, and 12.25 s in all. The 10 s the shutdown gives the receive
// stop and the HTTP stop together bounds none of that: it is how long the shutdown waits for the
// receive stop. Past it the shutdown goes on to the HTTP stop with no time left, then to the NATS
// drains and os.Exit(1), while the hand-back may still be running; client.Drain ends the
// subscription of a hand-back still lingering or draining, and os.Exit(1) ends only a mark write
// still in flight, and the next holder then settles at its 7 s cap, as it does after any stop that
// wrote no mark. The record a stop releases
// carries the ack wait its term still owes (lease.ackWaitOwed): a term that must settle owes one
// until its settle hold ends, so a stop before that leaves the next holder's hold to wait an ack
// wait. A release the lease's loop outlasts records that debt before it gives up (lease.Release),
// and the stop writes no mark: the linger before it read an unreleased record, ended "not taken"
// and let go of the deliver subject, so nothing vouches for the release that lands after it.
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
	// settleMargin is how much longer a settle hold lasts at most than the wait for the messages
	// the previous holder can still have outstanding (one ack wait after a hard stop, the
	// hand-back delay after a graceful one): it covers a redelivery the server sends a moment
	// after that wait runs out, and the stopping pod's lingerPoll and a round trip after a
	// graceful one.
	settleMargin = time.Second
	// handBackDelay is the NAK delay of every message a graceful stop hands back: long enough for
	// a standby to take the released lease and subscribe, so the redelivery reaches it.
	handBackDelay = 2 * time.Second
	// handBackNaksMax is how many times a stopping pod NAKs one message with handBackDelay: the
	// hand-back and two repeats, which reach it alone when the next holder has not subscribed yet.
	// A repeat past it is dropped and comes back at its ack wait. Each NAK is one more delivery
	// against the durable's MaxDeliver of 20.
	handBackNaksMax = 3
	// lingerPoll is how often a pod that stopped gracefully reads the lease record for the next
	// holder's receiving, and lingerMax the longest it keeps its subscription after it NAKed what
	// it held. lingerUntaken, two standby polls, is how long after its release it waits for any
	// other pod to take the lease at all, and so, with a poll on top, the floor of any linger that
	// no receiving holder ends: a standby that is taking the record then still finds the deliver
	// subject held for it.
	lingerPoll    = 250 * time.Millisecond
	lingerMax     = 3 * time.Second
	lingerUntaken = 2 * leaseStandbyPoll
	// handBackDrainWait bounds the drain of a stopping pod's subscription: past a slow link's
	// latency, since what the server pushed before the drain must still reach the pod.
	// handBackReleaseWait bounds its wait for the lease's loop to stop before the release.
	handBackDrainWait   = 2 * time.Second
	handBackReleaseWait = 2 * time.Second
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
	// settlePoll is how often a settle hold reads the durable, and the hand-back's mark with it;
	// settleCap is the longest any hold lasts, and handBackSettleCap the soonest one ends on that
	// mark (lease.HandBackMarked).
	settlePoll        time.Duration
	settleCap         time.Duration
	handBackSettleCap time.Duration
	bindPoll          time.Duration
	alarmAfter        time.Duration
	alarmEvery        time.Duration
	inProgressEvery   time.Duration
	inflightGrace     time.Duration
	// handBackDelay, lingerPoll, lingerMax and lingerUntaken time a graceful stop's hand-back.
	handBackDelay time.Duration
	lingerPoll    time.Duration
	lingerMax     time.Duration
	lingerUntaken time.Duration
}

func productionReceiveConfig() receiveConfig {
	return receiveConfig{
		settlePoll:        settlePoll,
		settleCap:         consumerAckWait + settleMargin,
		handBackSettleCap: handBackDelay + settleMargin,
		bindPoll:          durableBindPoll,
		alarmAfter:        durableTakeoverAlarmAfter,
		alarmEvery:        durableTakeoverAlarmEvery,
		inProgressEvery:   inProgressEvery,
		inflightGrace:     inflightGrace,
		handBackDelay:     handBackDelay,
		lingerPoll:        lingerPoll,
		lingerMax:         lingerMax,
		lingerUntaken:     lingerUntaken,
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
	// Deliverer is whether this pod is the machine's deliverer as far as it knows, from the one
	// snapshot of the lease the report takes (durableReceiver.report): it holds the lease or runs
	// without one, the lease record names this pod's own instance though it is not holding (its
	// renewals lapsed and no other pod has taken the record yet), or the record names no holder
	// at all as this pod last read it. With State other than receiveActive it says that nothing
	// of the durable reaches a session and this pod knows of no other pod that would deliver it,
	// which no answer of /healthz may call healthy.
	Deliverer bool
	// Failing is how long the term has failed to read, create, correct or subscribe to the
	// durable (durableReceiver.failed), zero while it is not failing; FailingReason is the latest
	// failure, and Stuck says the failures have lasted the alarm bound (alarmAfter).
	Failing       time.Duration
	FailingReason string
	Stuck         bool
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
	// clientConn is the main client's connection as the receive loop last heard of it: the one
	// the constructor was handed, and the one each reconnect hook reports (reconnected). It is
	// an atomic because the lease's signal reads it (receiveUp) and must take no lock a call
	// into nats.go can hold: nats.go holds its connection's lock across a reconnect dial that a
	// route dropping this pod's packets leaves hanging, and durableReceiver.begin holds r.mu
	// across the subscribe that waits for it.
	clientConn atomic.Pointer[nats.Conn]
	// subscribed is the term being served as the signal reads it, nil while no term is receiving
	// (termSubscription). The receive loop stores it whole: a term's successful subscribe
	// (begin, resubscribe), a resubscribe that left the term without one, and the term's end.
	subscribed atomic.Pointer[termSubscription]
	// awaitedBinding is set, by the receive loop alone, while the term it serves has waited out a
	// push binding and not yet subscribed, and cleared at the start of every term that is not this
	// pod's own resumed one (serve), so it says only this: the term this one resumes was waiting
	// out a pre-lease build's binding. A term the lease reports resumed (leaseStatus.Resumed) is
	// this pod's own record after its renewals lapsed, which it counts as taken over an expired
	// record; with the flag set, the binding it was waiting out is still the pre-lease build's,
	// not a dead holder's, so it is waited out again rather than received beside, and the term
	// does not settle. Any other acquisition is another pod's record, dead or handed back, and
	// settles as that record says.
	awaitedBinding bool

	mu    sync.Mutex
	state receiveState
	term  *receiveTerm
	stop  context.CancelFunc
	done  chan struct{}
	// failingSince is when the first of the term's failed attempts on the durable began, zero
	// while there is none since the term began or an attempt last succeeded; failingReason is the
	// latest failure, and failAlarm paces their alarm (durableReceiver.failed).
	failingSince  time.Time
	failingReason string
	failAlarm     alarmThrottle
}

func newDurableReceiver(options durableReceiverOptions) *durableReceiver {
	r := &durableReceiver{
		durableReceiverOptions: options,
		reconnects:             make(chan struct{}, 1),
	}
	r.clientConn.Store(options.client.Conn)
	return r
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
// handler takes no more messages, the message in hand gets inflightGrace before its handling is
// cancelled, and a holder hands every message it has not handled back to the next one, releases
// the lease, so a standby takes it at its next poll, and keeps its subscription until the next
// holder receives (durableReceiver.handBack). It returns once that is done or ctx ends; a lease
// left unreleased at ctx's end is taken as an expired one. A loop that has already returned is
// read before ctx (awaitDone), which a hand-back that ran to its bounds can have ended first, so
// a stop that did everything it owed does not report itself unfinished. The release here is the
// one that writes the record when the hand-back's own release outlasted its bound, and it writes
// the ack-wait debt that release recorded (lease.Release).
func (r *durableReceiver) Stop(ctx context.Context) {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.mu.Unlock()
	if stop != nil {
		stop()
		if awaitDone(ctx, done) != nil {
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
// longer valid there. It never fails, so it never holds up the client's recovery, and it takes
// no lock of the receive loop's, so a hook that lands while a subscribe is in flight waits for
// nothing (clientConn).
func (r *durableReceiver) reconnected(conn *nats.Conn) error {
	r.clientConn.Store(conn)
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

// termSubscription is the term being served as the lease's signal reads it: conn is the
// connection its subscription was made on, and nil when the term holds none at all, which a
// resubscribe that failed leaves. It is stored whole, under one atomic, so a reader takes no
// lock for it (durableReceiver.subscribed).
type termSubscription struct {
	conn *nats.Conn
}

// receiveUp is the signal the delivery lease judges this pod's receiving by
// (lease.SetReceiveSignal): the lease holder releases the record once it has been down for
// leaseReceiveLost, and no pod takes the record while it is down. It is the state /healthz
// reports as an inactive subscription on the active pod, read here rather than there: the
// client's connection is not connected, or the term is receiving and its subscription is no
// longer valid on that connection, which a connection the client replaced leaves until the
// resubscribe (report, repair). A holder that is still preparing - reading the durable, waiting
// out a pre-lease build's binding, settling before it has subscribed - is receiving as far as
// this goes, as /healthz counts it a standby rather than an inactive subscription: its
// connection is up and its term is on its way.
//
// It loads atomics and nothing else. The lease reads it from its own loop, the one that renews
// the record and releases it when this pod stops receiving, so it may wait on no lock a call
// into nats.go can hold: nats.go holds its connection's lock across every reconnect dial and the
// connect init after it, which a route that drops this pod's packets leaves hanging for the
// connect timeout, and durableReceiver.begin holds the receive loop's lock across the subscribe
// that waits for that same lock. The receive loop keeps both atomics current from what it
// already learns: the client's reconnect hook (reconnected) and each term's subscribe and end
// (begin, resubscribe, end).
func (r *durableReceiver) receiveUp() bool {
	if !r.client.Connected() {
		return false
	}
	subscribed := r.subscribed.Load()
	return subscribed == nil || (subscribed.conn != nil && subscribed.conn == r.clientConn.Load())
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
		report.SubscriptionValid = t.sub != nil && t.sub.IsValid() && t.conn == r.clientConn.Load()
	}
	if !r.failingSince.IsZero() {
		report.Failing = time.Since(r.failingSince)
		report.FailingReason = r.failingReason
		report.Stuck = report.Failing >= r.timing.alarmAfter
	}
	r.mu.Unlock()
	// One snapshot answers both questions the report asks of the lease, so the hold and the
	// record it names are the lease at one instant rather than two reads a change falls between.
	if r.lease == nil {
		report.Deliverer = true
		return report
	}
	status := r.lease.Status()
	report.Lease = status
	report.Deliverer = status.Holding ||
		status.ActiveInstance == "" || status.ActiveInstance == r.lease.instance
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
// (receiveTerm.settle), for settleCap at most, or until the clean-hand-back mark of the stop it
// took the record from is there and handBackSettleCap has passed since the subscribe, then
// delivers that in stream order.
// ctx is the receive loop's own, whose end is a shutdown. It reports whether the durable was
// refused. A term whose reads, writes or subscribes of the durable fail for alarmAfter gives the
// lease up (abandon).
//
// Nothing about the hand-back is judged here: the hold is the ack wait's from the start, and the
// mark the stopping pod writes once its hand-over went through is what shortens it, read inside
// the hold (lease.HandBackMarked). So a term that is slow to subscribe - a long prepare, a
// durable that refuses a subscribe and is retried - or whose SUB the server never read holds for
// the 7 s cap by doing nothing: no mark reaches a term whose stopping pod never saw it receiving.
//
// Before it subscribes it records stale_binding in the lease: set when it subscribes beside a
// binding it did not wait out, cleared when it finds the durable not push-bound. A term taken
// from a hand-back candidate records it beside a binding as any other settling term does, and the
// mark is what clears it (receiveTerm.settle): a binding beside such a term is the stopping pod's
// own linger, since a binding the stopping pod had found beside its own would have left
// stale_binding on the record it released and made this term no candidate - but only the mark
// says that linger is what it was.
//
// A term the lease reports resumed (leaseStatus.Resumed) while this pod was waiting out a
// pre-lease build's binding (awaitedBinding) is this pod's own term again, with that binding still
// the pre-lease build's: it waits it out again rather than settling beside it. The flag is cleared
// at the start of every other term, so a term that ended any other way - the lease lost, the lease
// given up, a shutdown - leaves nothing behind for a later take-over over a dead holder's record.
//
// The policy is written only once nothing but this pod will receive: a pre-lease build that still
// holds the durable binds it again after a reconnect with nats.Bind, which refuses a durable whose
// filter is not its own ("subject does not match consumer").
func (r *durableReceiver) serve(ctx, termCtx context.Context, status leaseStatus) bool {
	defer r.setState(receiveStandby)
	defer r.succeeded()
	start := time.Now()
	if !status.Resumed {
		r.awaitedBinding = false
	}
	// The flag stands only for a term this pod resumed, the line above having cleared it for
	// every other: a term that waited a pre-lease build's binding out and resumed waits it out
	// again rather than settling beside it.
	settle := status.Settle && !r.awaitedBinding
	r.setState(receivePreparing)
	info, err := r.prepare(termCtx, false)
	if err != nil {
		return r.abandon(err, status)
	}
	r.succeeded()
	var hold time.Duration
	if settle {
		hold = r.timing.settleCap
		r.logger.Info("listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand",
			slog.String("consumer", r.consumer), slog.String("lease", status.How.String()), slog.String("cap", hold.String()),
			slog.Bool("hand_back", status.HandBack))
	} else {
		if status.Settle {
			r.lease.Settled(status.Term)
		}
		if info.PushBound {
			if err = r.awaitUnbound(termCtx); err != nil {
				return r.abandon(err, status)
			}
		}
	}
	r.setState(receivePreparing)
	beside := false
	for {
		attempt := time.Now()
		if info, err = r.prepare(termCtx, true); err != nil {
			return r.abandon(err, status)
		}
		switch {
		case r.lease == nil:
		case !info.PushBound:
			r.lease.SetStaleBinding(status.Term, false)
		case settle:
			r.lease.SetStaleBinding(status.Term, true)
			if !beside {
				beside = true
				r.logger.Info("durable still push-bound by an earlier lease holder; receiving beside it", slog.String("consumer", r.consumer))
			}
		}
		err := r.begin(info.Config.DeliverSubject, hold, status.Term)
		if err == nil {
			r.awaitedBinding = false
			r.succeeded()
			break
		}
		r.logger.Warn("durable subscribe failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		if r.failed(attempt, fmt.Errorf("subscribe to the durable's deliver subject: %w", err)) {
			return r.abandon(errDurableStuck, status)
		}
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

// errDurableStuck is what prepare returns once a holder's failures on the durable have lasted
// alarmAfter (durableReceiver.failed): the term gives the lease up (abandon).
var errDurableStuck = errors.New("the lease holder could not prepare the durable within the alarm bound")

// abandon ends a term that stopped, with err, before it received. A holder stuck on the durable
// (errDurableStuck) gives the lease up (lease.GiveUp), so a standby takes it at its next poll and
// tries in its place. The record it releases carries unsettled and ack_wait_owed as the term left
// them: a term that had to settle hands its hold on, and one that never subscribed still owes the
// ack wait its acquisition took on, so the next holder's hold waits as long. A refused durable is
// reported (refuse). It reports whether the durable was refused.
func (r *durableReceiver) abandon(err error, status leaseStatus) bool {
	if !errors.Is(err, errDurableStuck) {
		return r.refuse(err)
	}
	r.logger.Warn("giving the listener lease up: this pod cannot deliver from the durable", slog.String("consumer", r.consumer))
	r.lease.GiveUp(status.Term)
	return false
}

// refuse reports a refused durable (errListenerDurableRefused) and whether err was one.
func (r *durableReceiver) refuse(err error) bool {
	if !errors.Is(err, errListenerDurableRefused) {
		return false
	}
	if r.refused != nil {
		r.refused(err)
	}
	return true
}

// alarmThrottle paces durableTakeoverAlarmLine over one wait the term cannot deliver through:
// the line is due once the wait has lasted alarmAfter, and again every alarmEvery while it
// lasts. A term keeps one for its failures on the durable (durableReceiver.failed) and one for
// each wait on another subscriber's push binding (awaitUnbound), so both pace the one line alike.
type alarmThrottle struct {
	// alarmed is when the line was last logged for this wait, zero while it has not been.
	alarmed time.Time
}

// due reports whether the alarm is due at now for a wait that has lasted waited, and records
// this logging of it when it is.
func (a *alarmThrottle) due(timing receiveConfig, now time.Time, waited time.Duration) bool {
	if waited < timing.alarmAfter || (!a.alarmed.IsZero() && now.Sub(a.alarmed) < timing.alarmEvery) {
		return false
	}
	a.alarmed = now
	return true
}

// alarm logs durableTakeoverAlarmLine at ERROR, which the deployed alarm matches: reason is why
// the term is not delivering, waited how long that has lasted.
func (r *durableReceiver) alarm(reason string, waited time.Duration) {
	r.logger.Error(durableTakeoverAlarmLine,
		slog.String("consumer", r.consumer),
		slog.String("reason", reason),
		slog.Int64("waited_ms", waited.Milliseconds()),
	)
}

// failed records an attempt on the durable that began at attempt and failed with err. The
// failures are timed from the start of the first of them since the term began or an attempt last
// succeeded (succeeded), as awaitUnbound times its wait, and their alarm is paced by the same
// throttle (alarmThrottle), with err as its reason. It reports whether the term gives the lease
// up for them: a holder does once they have lasted alarmAfter; a listener without a lease, the
// machine's one deliverer, never does.
func (r *durableReceiver) failed(attempt time.Time, err error) bool {
	now := time.Now()
	r.mu.Lock()
	if r.failingSince.IsZero() {
		r.failingSince = attempt
	}
	r.failingReason = err.Error()
	failing := now.Sub(r.failingSince)
	alarm := r.failAlarm.due(r.timing, now, failing)
	r.mu.Unlock()
	if alarm {
		r.alarm(err.Error(), failing)
	}
	return r.lease != nil && failing >= r.timing.alarmAfter
}

// succeeded ends the failures failed times.
func (r *durableReceiver) succeeded() {
	r.mu.Lock()
	r.failingSince, r.failingReason, r.failAlarm = time.Time{}, "", alarmThrottle{}
	r.mu.Unlock()
}

// prepare reads the durable, creating it when it is missing and, when correct is set, correcting
// its policy when it has drifted (correctListenerDurable), retrying every bindPoll until it
// answers, and records its deliver subject in the lease. Each failure counts toward the term's
// alarm bound (failed). It returns a refusal (errListenerDurableRefused), errDurableStuck once a
// holder's failures have lasted that bound, or ctx's error.
func (r *durableReceiver) prepare(ctx context.Context, correct bool) (*nats.ConsumerInfo, error) {
	failing := false
	for {
		attempt := time.Now()
		step := "read"
		info, err := listenerDurable(ctx, r.client, r.consumer)
		if err == nil && (correct || info == nil) {
			step = "correct the policy of"
			if info == nil {
				step = "create"
			}
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
		err = fmt.Errorf("%s the durable consumer: %w", step, err)
		if !failing {
			failing = true
			r.logger.Warn("durable consumer read failed; retrying", slog.String("consumer", r.consumer), slog.String("error", err.Error()))
		}
		if r.failed(attempt, err) {
			return nil, errDurableStuck
		}
		if err := waitUnlessDone(ctx, r.timing.bindPoll); err != nil {
			return nil, err
		}
	}
}

// awaitUnbound waits, polling the durable every bindPoll without writing it, until no subscriber
// holds its push binding. Its wait is alarmed on by the same throttle as a term's failures
// (alarmThrottle, durableReceiver.failed), and it never gives up for the binding: only ctx's
// end, a refusal or reads that fail for alarmAfter (prepare) end it.
func (r *durableReceiver) awaitUnbound(ctx context.Context) error {
	r.setState(receiveAwaitingBinding)
	r.awaitedBinding = true
	r.logger.Info("durable is push-bound by a subscriber without the lease; waiting for its binding to end",
		slog.String("consumer", r.consumer))
	start := time.Now()
	var alarm alarmThrottle
	for {
		if err := waitUnlessDone(ctx, r.timing.bindPoll); err != nil {
			return err
		}
		info, err := r.prepare(ctx, false)
		if err != nil {
			return err
		}
		r.succeeded()
		waited := time.Since(start)
		if !info.PushBound {
			r.logger.Info("durable push binding ended", slog.String("consumer", r.consumer), slog.Int64("waited_ms", waited.Milliseconds()))
			return nil
		}
		if alarm.due(r.timing, time.Now(), waited) {
			r.alarm("another subscriber holds the durable's push binding", waited)
		}
	}
}

// begin starts a term's receiving: a core subscription on subject on the client's current
// connection, the serial handler and the InProgress ticker. A hold above zero has the handler
// first hold what arrives until the durable's outstanding messages are all in hand, or for that
// long at most, then deliver it in stream order (receiveTerm.settle); term is the lease term
// whose unsettled the hold's end clears. Once the server has the subscription, the lease records
// the term receiving, which a pod that stopped gracefully waits for before it lets go of the
// deliver subject - and, after that, before it writes the mark that shortens this term's hold. A
// flush that fails leaves the SUB to nats.go's reconnect, records no term receiving and so earns
// no mark: the hold runs to its 7 s cap, which is what the messages that reached no subscriber
// meanwhile need.
func (r *durableReceiver) begin(subject string, hold time.Duration, term uint64) error {
	t := &receiveTerm{
		subject:   subject,
		queue:     make(chan *nats.Msg, receiveQueueCapacity),
		unsettled: map[*nats.Msg]struct{}{},
		closing:   make(chan struct{}),
		handled:   make(chan struct{}),
		stopped:   make(chan struct{}),
		logger:    r.logger,
		hold:      hold,
		leaseTerm: term,
		holding:   hold > 0,
	}
	t.handling, t.cancel = context.WithCancel(context.Background())
	r.mu.Lock()
	conn := r.clientConn.Load()
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		r.mu.Unlock()
		t.cancel()
		return err
	}
	t.sub, t.conn = sub, conn
	r.term, r.state = t, receiveActive
	r.subscribed.Store(&termSubscription{conn: conn})
	r.mu.Unlock()
	// The server counts the subscription once it has read it; a flush that fails, while the
	// connection is down, leaves it to nats.go to send again on the reconnect, and the term is
	// recorded receiving once a resubscribe on the new connection has flushed.
	flushed := conn.FlushTimeout(time.Second) == nil
	if flushed && r.lease != nil {
		r.lease.SetReceiving(term)
	}
	go t.handle(r)
	go t.keepAlive(r.timing.inProgressEvery)
	return nil
}

// repair moves the term's subscription onto the client's current connection when it is no longer
// valid there: the client replaced its connection, or the subscription ended.
func (r *durableReceiver) repair(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	stale := t != nil && (t.sub == nil || !t.sub.IsValid() || t.conn != r.clientConn.Load())
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
	conn := r.clientConn.Load()
	sub, err := conn.Subscribe(subject, t.intake)
	if err != nil {
		t.sub = nil
		r.subscribed.Store(&termSubscription{})
		r.mu.Unlock()
		return fmt.Errorf("subscribe to the durable's deliver subject: %w", err)
	}
	t.sub, t.conn, t.subject = sub, conn, subject
	r.subscribed.Store(&termSubscription{conn: conn})
	r.mu.Unlock()
	if r.lease != nil {
		r.lease.SetDeliverSubject(subject)
	}
	if conn.FlushTimeout(time.Second) == nil && r.lease != nil {
		r.lease.SetReceiving(t.leaseTerm)
	}
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

// waitLoops waits for each loop to return, until deadline. A loop that has already returned is
// read before the deadline (awaitDone), so a pass that ended just as it ran out is not reported
// as still running.
func waitLoops(logger *logging.Logger, loops []<-chan struct{}, deadline time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, done := range loops {
		if awaitDone(ctx, done) != nil {
			logger.Warn("a holder-only loop was still running when receiving ended")
			return
		}
	}
}
