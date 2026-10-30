package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The delivery lease's timing. Every pod of one machine runs a lease over one record; the pod
// holding it is the machine's one deliverer and every other pod is a standby. The holder renews
// every leaseRenewEvery and counts itself the holder until the start of its last successful
// renewal + leaseTTL - leaseMargin, on its monotonic clock. A standby reads the record every
// leaseStandbyPoll and takes it once the same revision has stayed unchanged for a full leaseTTL
// on its own monotonic clock, measured from when it first saw that revision. No wall clocks are
// compared: the holder's validity ends at its renewal's start + 4 s, and a standby takes no
// earlier than that write + 5 s, which is safe while the two pods' clock rates differ by less
// than 20 %.
const (
	leaseRenewEvery  = time.Second
	leaseTTL         = 5 * time.Second
	leaseMargin      = time.Second
	leaseStandbyPoll = 500 * time.Millisecond
)

// leaseConfig carries the lease's timing, so a test of this package shortens it;
// productionLeaseConfig is the only value the listener runs with.
type leaseConfig struct {
	renewEvery  time.Duration
	ttl         time.Duration
	margin      time.Duration
	standbyPoll time.Duration
}

func productionLeaseConfig() leaseConfig {
	return leaseConfig{renewEvery: leaseRenewEvery, ttl: leaseTTL, margin: leaseMargin, standbyPoll: leaseStandbyPoll}
}

// leaseAcquisition is how a pod took the lease: what the record was when it wrote over it. The
// receive loop acts on it through leaseStatus.Settle. A term taken over an expired record that was
// not released (the holder was killed or lost its node, or this pod's own renewals lapsed and then
// resumed) must settle: the receive loop subscribes at once, beside any push binding, which it
// counts as the dead holder's, and holds what it receives until every message the durable has
// outstanding is in its hands, then delivers it in stream order, so the messages the old holder
// had in hand go before newer ones. So must a term taken from an absent or released record that
// carries stale_binding or unsettled. A term taken from an absent or released record with neither
// flag subscribes and delivers at once, and counts a push-bound durable as a pre-lease build's.
type leaseAcquisition int

const (
	leaseTakenAbsent leaseAcquisition = iota + 1
	leaseTakenReleased
	leaseTakenExpired
)

func (a leaseAcquisition) String() string {
	switch a {
	case leaseTakenAbsent:
		return "absent"
	case leaseTakenReleased:
		return "released"
	case leaseTakenExpired:
		return "expired"
	default:
		return "none"
	}
}

// leaseStatus is a lease at one instant.
type leaseStatus struct {
	// Holding is whether this process holds the lease now: it holds the record and is inside its
	// validity on the monotonic clock.
	Holding bool
	// Term counts this process's acquisitions, a resume after its renewals lapsed included, so a
	// term the receive loop subscribed for differs from a later one even when no change between
	// them reached it.
	Term uint64
	// How is how Term's acquisition was taken; zero before the first.
	How leaseAcquisition
	// ActiveInstance is the holder the record names as this process last wrote or read it, empty
	// when there is no record or it is released: a standby's /healthz names it.
	ActiveInstance string
	// ServiceSince is the record's service_since as last written or read, zero when no record
	// carried one.
	ServiceSince time.Time
	// Settle is whether Term must settle before it delivers (leaseAcquisition): it was taken over
	// an expired record, resumed after a lapse, or taken from a record carrying stale_binding or
	// unsettled.
	Settle bool
	// HandBack is whether Term is a candidate for a clean hand-back: it was taken from a released
	// record carrying unsettled and neither ack_wait_owed nor stale_binding, so the pod that
	// released it stopped gracefully, NAKed what it held with the hand-back delay, left no ack
	// wait owed and had no lost node's binding open beside its own subscription. Whether that
	// stop then went through cleanly is not read from the record at all: the stopping pod writes
	// a mark for the revision Replaced names once it has, and the term's settle hold asks the
	// lease for it (HandBackMarked).
	HandBack bool
	// Replaced is the revision of the lease record Term's acquiring write replaced, zero when it
	// replaced none (an absent record, or this process's own resumed one). It is the revision a
	// clean-hand-back mark of the pod before this term is keyed by.
	Replaced uint64
	// Resumed is whether Term is this process's own record resumed after its renewals lapsed (the
	// lapse branch of renew), rather than a record another pod wrote: no other pod held the lease
	// between the term before it and this one. Only the lease knows that, and the receive loop
	// needs it to tell its own resume from a take-over over a dead holder's record, both of which
	// How reports as expired.
	Resumed bool
}

// lease is one pod's view of its machine's delivery lease, the record lease.<durable> in the
// listener state bucket. Start runs it; Holding and Status read it at any time; Changes signals
// each acquisition and each loss; Release gives it up on a graceful stop.
type lease struct {
	kv  bus.KeyValue
	key string
	// handBackKeys begins this machine's clean-hand-back marks (bus.ListenerHandBackPrefix): one
	// key per lease revision a graceful stop of this machine vouched for.
	handBackKeys string
	instance     string
	boot         string
	cfg          leaseConfig
	now          func() time.Time
	logger       *logging.Logger
	changes      chan struct{}
	// renewals asks the running loop for a renewal now rather than at its next turn
	// (SetReceiving), or to give the lease up (GiveUp).
	renewals chan struct{}

	// The loop's own state, read and written only by step (and by Release once the loop ended):
	// the revision a standby is timing and when it first saw it, whether the holder's lapse has
	// been signalled, whether a streak of failed bucket calls has been logged, and the revision
	// of the record this process last released on giving the lease up and when (GiveUp).
	seenRev        uint64
	seenAt         time.Time
	lapseSignalled bool
	failing        bool
	givenUpRev     uint64
	givenUpAt      time.Time

	mu         sync.Mutex
	held       bool
	rev        uint64
	validUntil time.Time
	term       uint64
	how        leaseAcquisition
	settle     bool
	// handBack is whether the term is a candidate for a clean hand-back and replaced the revision
	// of the record its acquiring write wrote over, which the mark that would vouch for that
	// hand-back is keyed by (leaseStatus, HandBackMarked).
	handBack       bool
	replaced       uint64
	resumed        bool
	record         leaseRecord
	deliverSubject string
	// staleBinding, unsettled and receiving are the holder's stale_binding, unsettled and
	// receiving, written into the record from the next renewal or release on (SetStaleBinding,
	// Settled, Unsettle, SetReceiving). ackWaitOwed is the ack wait the term owes its successor,
	// which its release writes as ack_wait_owed: set by the acquisition of a term that must settle
	// (acquire, and renew's lapse) and by a release that outlasted its bound (Release), and
	// cleared when the term's settle hold ends (Settled). Every settling term owes it until then,
	// a term taken from a hand-back included: the hold's end is what says its predecessor's
	// messages are in hand, whether it ended on the durable's count or on the hand-back's mark.
	staleBinding bool
	unsettled    bool
	receiving    bool
	ackWaitOwed  bool
	// released is the revision this term's own release wrote, zero while it has written none,
	// and releasedStale the stale_binding that release carried: the clean-hand-back mark of a
	// graceful stop vouches for that revision, and only a release that found no binding beside
	// its own may be vouched for (MarkHandBack). Each term starts with both cleared
	// (startTermLocked), so no stop is vouched for by a release an earlier term made - the
	// give-up of a term this pod then took the lease back after, say.
	released      uint64
	releasedStale bool
	// giveUpTerm is the term GiveUp asked the loop to give up.
	giveUpTerm uint64
	stop       context.CancelFunc
	done       chan struct{}
}

// newLease makes machineID's lease over kv for this process, instance, with timing cfg and now
// as its clock. now must carry a monotonic reading (time.Now in production): the lease only
// subtracts its readings and never compares them with another pod's.
func newLease(kv bus.KeyValue, machineID, instance string, cfg leaseConfig, now func() time.Time, logger *logging.Logger) *lease {
	var boot [8]byte
	_, _ = rand.Read(boot[:]) // crypto/rand.Read never returns an error
	return &lease{
		kv:           kv,
		key:          bus.ListenerLeaseKey(machineID),
		handBackKeys: bus.ListenerHandBackPrefix(bus.ListenerDurable(machineID)),
		instance:     instance,
		boot:         hex.EncodeToString(boot[:]),
		cfg:          cfg,
		now:          now,
		logger:       logger,
		changes:      make(chan struct{}, 1),
		renewals:     make(chan struct{}, 1),
	}
}

// Start runs the lease until ctx ends or Release stops it: a standby polls and takes the record,
// a holder renews it. Start is called once.
func (l *lease) Start(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	l.mu.Lock()
	l.stop, l.done = stop, done
	l.mu.Unlock()
	go func() {
		defer close(done)
		for {
			timer := time.NewTimer(l.step())
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-l.renewals:
				timer.Stop()
			}
		}
	}()
}

// Changes signals that the lease was acquired or lost. It holds at most one signal, so changes
// that come faster than the reader collapse into one: the reader reads Status after each signal
// and acts on the state there (a Term that differs from the one it acted on is a new
// acquisition). A loss is signalled when the holder's validity runs out without a renewal (as
// soon as the loop next wakes, which is no later than the validity's end unless a bucket call is
// in flight then), when a renewal finds another pod's record, and on Release. Holding is exact at
// every instant and is what gates each message.
func (l *lease) Changes() <-chan struct{} { return l.changes }

// Holding reports whether this process holds the lease now.
func (l *lease) Holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdingLocked()
}

func (l *lease) holdingLocked() bool {
	return l.held && l.now().Before(l.validUntil)
}

// Status reads the lease now.
func (l *lease) Status() leaseStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := leaseStatus{
		Holding:      l.holdingLocked(),
		Term:         l.term,
		How:          l.how,
		ServiceSince: l.record.ServiceSince,
		Settle:       l.settle,
		HandBack:     l.handBack,
		Replaced:     l.replaced,
		Resumed:      l.resumed,
	}
	if !l.record.Released {
		status.ActiveInstance = l.record.Holder
	}
	return status
}

// SetDeliverSubject records the durable's deliver subject this process receives on, written
// into the record from the next renewal or acquisition on.
func (l *lease) SetDeliverSubject(subject string) {
	l.mu.Lock()
	l.deliverSubject = subject
	l.mu.Unlock()
}

// SetStaleBinding records stale_binding for term (leaseFields): whether its holder subscribed
// while the durable was push-bound by a binding it did not wait out, written into the record from
// the next renewal or release on. A term that is no longer current changes nothing.
func (l *lease) SetStaleBinding(term uint64, stale bool) {
	l.mu.Lock()
	if l.term == term {
		l.staleBinding = stale
	}
	l.mu.Unlock()
}

// Settled records that term's settle hold ended: every message the previous holder can still have
// outstanding is in this one's hands, so it clears unsettled from the next renewal or release on
// and the ack wait the term owed its successor with it. A term that is no longer current changes
// nothing.
func (l *lease) Settled(term uint64) {
	l.mu.Lock()
	if l.term == term {
		l.unsettled, l.ackWaitOwed = false, false
	}
	l.mu.Unlock()
}

// Unsettle records that term's holder handed messages back on a graceful stop, setting unsettled
// from the next renewal or release on, so the next holder settles. A term that is no longer
// current changes nothing.
func (l *lease) Unsettle(term uint64) {
	l.mu.Lock()
	if l.term == term {
		l.unsettled = true
	}
	l.mu.Unlock()
}

// SetReceiving records that term's holder has subscribed to the durable's deliver subject, and
// has the running loop renew at once to write receiving, which a pod that stopped gracefully
// waits for before it lets go of the deliver subject (durableReceiver.linger). It clears no ack
// wait: every settling term, one taken from a clean hand-back included, owes its successor one
// until its settle hold ends (Settled). A term that is no longer current changes nothing.
func (l *lease) SetReceiving(term uint64) {
	l.mu.Lock()
	set := l.term == term && !l.receiving
	if set {
		l.receiving = true
	}
	l.mu.Unlock()
	if set {
		select {
		case l.renewals <- struct{}{}:
		default:
		}
	}
}

// GiveUp has the running loop release term's record, as Release does, and go on as a standby, so
// another pod takes the lease at its next poll: the receive loop gives the lease up when it cannot
// prepare or subscribe to the durable (durableReceiver.abandon). The loop does not take back the
// record it released for two standby polls, time for every other running standby to read and
// take it; a pod that runs alone takes it back after that and tries again. A term that is no
// longer current, or no longer held, changes nothing.
func (l *lease) GiveUp(term uint64) {
	l.mu.Lock()
	set := l.held && l.term == term
	if set {
		l.giveUpTerm = term
	}
	l.mu.Unlock()
	if set {
		select {
		case l.renewals <- struct{}{}:
		default:
		}
	}
}

// Successor reads the record and reports whether another process has taken it since this one
// released it (it names another boot and is not released, or cannot be read) and whether that
// holder's subscription is up (receiving). A pod that stopped gracefully reads it once its own
// lease is released, so it lets go of the deliver subject only once another pod receives it, or
// once no other pod has taken the lease.
func (l *lease) Successor() (taken, receiving bool, err error) {
	entry, err := l.kv.Get(l.key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		return true, false, nil
	}
	taken = record.Boot != l.boot && !record.Released
	return taken, taken && record.Receiving, nil
}

// FormerHolder reports whether boot is one of the boots that held the lease before the holder of
// the record as this process last wrote or read it (former_boots): a process that delivers
// nothing more, whose unresolved delivery attempt the ledger takes over at once.
func (l *lease) FormerHolder(boot string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, former := range l.record.FormerBoots {
		if former.Boot == boot {
			return true
		}
	}
	return false
}

// Release stops the lease and, when this process holds the record, marks it released at its
// revision, no longer receiving, keeping service_since, stale_binding and former_boots as they
// stand and writing unsettled and ack_wait_owed as the term stands (lease.ackWaitOwed), so a
// standby takes it at its next poll with no TTL wait, and settles in its turn when this term had
// not settled or handed messages back. A term whose validity had run out when it was stopped - a
// lapse the renewals never resumed - releases the record unsettled and owing an ack wait,
// whatever its own flags say: the lapse dropped what the term had in hand and what the durable
// pushed it without an ack, and those come back only at their ack wait, as they would have from
// the expired record this release replaces. A record another pod has already taken is left as it
// is, and that is no error. It returns ctx.Err() when ctx ends before the loop has stopped, and
// the write's error when the record could not be marked; either way this process no longer holds
// the lease, and a record left unreleased is taken as an expired one.
//
// A release that gives up on the loop records the term's ack-wait debt before it returns, so any
// later release of that term writes ack_wait_owed (letGo). The caller's stop goes on without the
// record written: the linger that follows reads its own unreleased record, ends "not taken" at
// its floor and lets go of the deliver subject (durableReceiver.handBack), and the release that
// lands after that - the receiver's own stop - writes a record this stop vouches for with no
// clean-hand-back mark, since the deliver subject was already let go. A standby that takes it
// moments later therefore settles for an ack wait, not for the hand-back delay.
//
// A loop that has already stopped is read before ctx: the receiver's own stop calls Release again
// with the shutdown context a graceful stop can have run out of (durableReceiver.Stop, after
// handBack's release timed out), and a select over a closed done and an ended ctx together would
// pick either, skipping the write of half of those stops and leaving a standby to wait the record
// out as an expired one.
func (l *lease) Release(ctx context.Context) error {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.mu.Unlock()
	if stop != nil {
		stop()
		if err := awaitDone(ctx, done); err != nil {
			l.mu.Lock()
			l.ackWaitOwed = true
			l.mu.Unlock()
			return err
		}
	}
	_, err := l.letGo()
	return err
}

// letGo ends this process's hold and, when it held the record, marks the record released at its
// revision, no longer receiving, keeping service_since, stale_binding and former_boots as they
// stand and writing unsettled and ack_wait_owed as the term stands, or both of them set when the
// term had lapsed (Release). It records the revision it wrote and the stale_binding it wrote
// there, which a clean-hand-back mark of this stop would vouch for (MarkHandBack). It returns the
// released record's revision, or zero when it wrote none: it did not hold the record, another pod
// had already taken it (no error), or the write failed (its error).
func (l *lease) letGo() (uint64, error) {
	l.mu.Lock()
	held, lapsed, rev, record := l.held, l.held && !l.holdingLocked(), l.rev, l.record
	record.StaleBinding, record.Unsettled, record.AckWaitOwed, record.Receiving = l.staleBinding, l.unsettled || lapsed, l.ackWaitOwed || lapsed, false
	l.held, l.validUntil = false, time.Time{}
	l.mu.Unlock()
	if !held {
		return 0, nil
	}
	defer l.signal()
	record.Released = true
	value, err := record.encode()
	if err != nil {
		return 0, fmt.Errorf("encode the released lease record: %w", err)
	}
	next, err := l.kv.Update(l.key, value, rev)
	switch {
	case err == nil:
		l.mu.Lock()
		l.rev, l.record = next, record
		l.released, l.releasedStale = next, record.StaleBinding
		l.mu.Unlock()
		l.logger.Info("listener lease released", slog.String("lease", l.key), slog.Uint64("revision", next))
		return next, nil
	case errors.Is(err, nats.ErrKeyExists):
		return 0, nil
	default:
		return 0, fmt.Errorf("mark lease %s released: %w", l.key, err)
	}
}

// signal records a change for Changes, without waiting for a reader.
func (l *lease) signal() {
	select {
	case l.changes <- struct{}{}:
	default:
	}
}

// step does one round of the lease, renewing it as the holder, giving it up when GiveUp asked for
// the current term, or reading and maybe taking it as a standby, and returns how long to wait
// before the next.
func (l *lease) step() time.Duration {
	l.mu.Lock()
	held, givingUp := l.held, l.held && l.giveUpTerm == l.term
	l.mu.Unlock()
	switch {
	case givingUp:
		return l.giveUp()
	case held:
		return l.renew()
	}
	return l.poll()
}

// giveUp releases the record as letGo does and leaves the loop a standby that does not take that
// released revision back for two standby polls (GiveUp). A record the release could not mark is
// taken, by this process too, once it expires.
func (l *lease) giveUp() time.Duration {
	rev, err := l.letGo()
	if err != nil {
		l.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
	l.seenRev, l.lapseSignalled = 0, false
	l.givenUpRev, l.givenUpAt = rev, l.now()
	return l.cfg.standbyPoll
}

// renew rewrites the holder's record at the revision it last wrote. A renewal that lands counts
// from its own start; one that started after the validity ran out is a resume, a new term taken
// as over an expired record, which must settle and so writes unsettled, owes its successor an ack
// wait until its settle hold ends, and has not subscribed yet and so writes receiving false. The
// resume is this process's own record (Resumed), which the receive loop tells from a take-over
// over another pod's expired record. A wrong revision means another pod took the record, and this
// process steps down; any other failure is retried every renewEvery, sooner when the validity ends
// first, so its end is signalled.
func (l *lease) renew() time.Duration {
	start := l.now()
	l.mu.Lock()
	lapsed := !start.Before(l.validUntil)
	record, rev := l.record, l.rev
	record.DeliverSubject = l.deliverSubject
	record.StaleBinding, record.Unsettled = l.staleBinding, l.unsettled || lapsed
	record.Receiving = l.receiving && !lapsed
	l.mu.Unlock()
	if lapsed && !l.lapseSignalled {
		l.lapseSignalled = true
		l.logger.Warn("listener lease lapsed: its renewals failed for the lease's validity", slog.String("lease", l.key))
		l.signal()
	}
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.renewEvery
	}
	next, err := l.kv.Update(l.key, value, rev)
	if errors.Is(err, nats.ErrKeyExists) {
		own, mine, err := l.ownRevision()
		if err != nil {
			l.noteFailure("read after a conflict", err)
			return l.cfg.renewEvery
		}
		if mine {
			// An earlier renewal landed though its reply was lost: the record is this process's
			// own write, so renew again at that revision. The validity is not extended until a
			// renewal's reply arrives.
			l.mu.Lock()
			l.rev = own
			l.mu.Unlock()
			return 0
		}
		l.stepDown()
		return 0
	}
	if err != nil {
		l.noteFailure("renew", err)
		l.mu.Lock()
		wait := min(l.cfg.renewEvery, l.validUntil.Sub(l.now()))
		l.mu.Unlock()
		if wait <= 0 {
			wait = l.cfg.renewEvery
		}
		return wait
	}
	l.recovered()
	l.lapseSignalled = false
	l.mu.Lock()
	l.rev, l.record = next, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	if lapsed {
		l.startTermLocked(leaseTakenExpired, true, false, 0, true)
	}
	term := l.term
	l.mu.Unlock()
	if lapsed {
		l.logger.Info("listener lease resumed: a renewal after its validity found its own record current",
			slog.String("lease", l.key), slog.Uint64("term", term), slog.Uint64("revision", next))
		l.signal()
	}
	return l.cfg.renewEvery
}

// startTermLocked begins a term of this process's hold and is where every term's state is set,
// whichever way it was taken: the take of a record (acquire) and the resume of this process's own
// after its renewals lapsed (renew). It counts the acquisition, records how it was taken, whether
// it must settle, whether it is a candidate for a clean hand-back and the revision its acquiring
// write replaced, which such a hand-back's mark is keyed by (handBack and replaced, both zero for
// a term that is no candidate), and whether it is a resume (leaseStatus), and sets the flags that
// follow from settle: a term that must settle is unsettled and owes its successor an ack wait
// from here until its settle hold ends (Settled), and no term has subscribed when it starts. It
// clears what an earlier term's release recorded, so this term's stop marks a hand-back only for
// a record its own release wrote (released, MarkHandBack). acquire records stale_binding itself,
// which it carries over from the record it took. Callers must hold l.mu.
func (l *lease) startTermLocked(how leaseAcquisition, settle, handBack bool, replaced uint64, resumed bool) {
	l.term++
	l.how = how
	l.settle, l.handBack, l.replaced, l.resumed = settle, handBack, replaced, resumed
	l.unsettled, l.ackWaitOwed, l.receiving = settle, settle, false
	l.released, l.releasedStale = 0, false
}

// stepDown ends the holder's hold after a renewal found another pod's record.
func (l *lease) stepDown() {
	l.mu.Lock()
	l.held, l.rev, l.validUntil = false, 0, time.Time{}
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Warn("listener lease lost: another listener holds it", slog.String("lease", l.key))
	l.signal()
}

// ownRevision reads the record after a renewal's conflict and reports its revision and whether it
// is this process's own unreleased write (its boot), which only a renewal whose reply was lost
// leaves behind.
func (l *lease) ownRevision() (uint64, bool, error) {
	entry, err := l.kv.Get(l.key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		return 0, false, nil
	}
	return entry.Revision(), record.Boot == l.boot && !record.Released, nil
}

// poll reads the record as a standby and takes it when it is absent (Create), released (Update
// at its revision, at once) or unchanged for a full ttl since this process first saw its
// revision (Update at that revision). The released revision this process wrote on giving the
// lease up it takes only two standby polls after that (GiveUp), so another standby takes it first.
func (l *lease) poll() time.Duration {
	entry, err := l.kv.Get(l.key)
	seen := l.now()
	if errors.Is(err, nats.ErrKeyNotFound) {
		l.recovered()
		l.mu.Lock()
		l.record = leaseRecord{}
		l.mu.Unlock()
		l.seenRev = 0
		return l.acquire(nil, leaseRecord{}, leaseTakenAbsent)
	}
	if err != nil {
		l.noteFailure("read", err)
		return l.cfg.standbyPoll
	}
	l.recovered()
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		// An unreadable record is taken as one that is not released, once it stays unchanged for
		// the ttl, and nothing of it is kept.
		if entry.Revision() != l.seenRev {
			l.logger.Warn("listener lease record unreadable", slog.String("lease", l.key),
				slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
		}
		record = leaseRecord{}
	}
	l.mu.Lock()
	l.record = record
	l.mu.Unlock()
	switch {
	case record.Released && entry.Revision() == l.givenUpRev && seen.Sub(l.givenUpAt) < 2*l.cfg.standbyPoll:
		return l.cfg.standbyPoll
	case record.Released:
		return l.acquire(entry, record, leaseTakenReleased)
	case entry.Revision() != l.seenRev:
		l.seenRev, l.seenAt = entry.Revision(), seen
		return l.cfg.standbyPoll
	case seen.Sub(l.seenAt) < l.cfg.ttl:
		return l.cfg.standbyPoll
	default:
		return l.acquire(entry, record, leaseTakenExpired)
	}
}

// acquire writes this process's record over previous (nil: none) and, once it reads back the
// write's server timestamp, holds the lease from the write's start. service_since starts again
// at that timestamp when no record was there, when it carried none, or when its last write is
// more than two ttl older than this one, since then no pod of the machine was serving; otherwise
// it is kept. stale_binding is carried over, ack_wait_owed is read and written false, unsettled is
// written when the term must settle (leaseAcquisition), and receiving is written false until the
// term subscribes. A term that must settle owes its successor an ack wait from here until its
// settle hold ends, a term taken from a clean hand-back included (SetReceiving, Settled). A
// released record carrying unsettled and neither ack_wait_owed nor stale_binding makes the term a
// candidate for a clean hand-back, and the revision it replaces is what the stopping pod's mark
// for that hand-back is keyed by (HandBackMarked). former_boots adds previous's boot
// to previous's own (formerBoots): the acquiring write stamps it with previous's timestamp, the
// latest server time known before it, and once the write's own timestamp is read back it is
// stamped with that. The record is renewed at
// once to store service_since, acquired_at and that stamp. The write is read back by key, a direct
// get on the key's own subject, which the listener's grant allows; a get by revision is a direct
// get on the stream's bare subject, which it does not. A read back that finds another revision, or
// no record, means another write landed first: the lease is not held and the record is polled
// again at once. A write whose timestamp cannot be read back is not counted as held: the record
// then expires as any unreleased one does, and a standby, this one included, takes it after the
// ttl.
func (l *lease) acquire(previous nats.KeyValueEntry, record leaseRecord, how leaseAcquisition) time.Duration {
	start := l.now()
	l.mu.Lock()
	record.DeliverSubject = l.deliverSubject
	l.mu.Unlock()
	before := record
	if previous != nil {
		record.FormerBoots = formerBoots(before, l.boot, previous.Created())
	}
	record.Holder, record.Boot, record.Released, record.AcquiredAt = l.instance, l.boot, false, time.Time{}
	settle := how == leaseTakenExpired || record.StaleBinding || record.Unsettled
	record.Unsettled, record.AckWaitOwed, record.Receiving = settle, false, false
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.standbyPoll
	}
	var rev uint64
	if previous == nil {
		rev, err = l.kv.Create(l.key, value)
	} else {
		rev, err = l.kv.Update(l.key, value, previous.Revision())
	}
	if errors.Is(err, nats.ErrKeyExists) {
		l.seenRev = 0
		return 0
	}
	if err != nil {
		l.noteFailure("take", err)
		return l.cfg.standbyPoll
	}
	written, err := l.kv.Get(l.key)
	if err != nil || written.Revision() != rev {
		l.seenRev, l.seenAt = rev, l.now()
		if err == nil || errors.Is(err, nats.ErrKeyNotFound) {
			return 0
		}
		l.noteFailure("read back", err)
		return l.cfg.standbyPoll
	}
	record.AcquiredAt = written.Created()
	if previous != nil {
		record.FormerBoots = formerBoots(before, l.boot, written.Created())
	}
	if previous == nil || record.ServiceSince.IsZero() || written.Created().Sub(previous.Created()) > 2*l.cfg.ttl {
		record.ServiceSince = written.Created()
	}
	// A released record carrying unsettled, no debt and no stale binding is a candidate for a
	// clean hand-back: the pod that released it stopped gracefully, NAKed what it held with the
	// hand-back delay, left no ack wait owed and had no lost node's binding open beside its own
	// subscription. Nothing here says that stop went on to hand the deliver subject over: the
	// stopping pod writes a mark for this revision once it has, and the term's settle hold reads
	// it (HandBackMarked). A record that carries a debt or a stale binding is no candidate
	// however fresh it is, and one this term replaced is a candidate however old.
	handBack := how == leaseTakenReleased && before.Unsettled && !before.AckWaitOwed && !before.StaleBinding
	var replaced uint64
	if handBack {
		replaced = previous.Revision()
	}
	l.mu.Lock()
	l.held, l.rev, l.record = true, rev, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	l.startTermLocked(how, settle, handBack, replaced, false)
	l.staleBinding = record.StaleBinding
	term := l.term
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Info("listener lease acquired", slog.String("lease", l.key), slog.String("from", how.String()),
		slog.Uint64("term", term), slog.Uint64("revision", rev), slog.Time("service_since", record.ServiceSince),
		slog.Bool("settle", settle), slog.Bool("stale_binding", record.StaleBinding),
		slog.Bool("hand_back", handBack), slog.Uint64("replaced", replaced))
	l.signal()
	return l.renew()
}

// noteFailure logs the first failed bucket call of a streak; recovered logs its end.
func (l *lease) noteFailure(action string, err error) {
	if l.failing {
		return
	}
	l.failing = true
	l.logger.Warn("listener lease state unavailable", slog.String("lease", l.key),
		slog.String("action", action), slog.String("error", err.Error()))
}

func (l *lease) recovered() {
	if !l.failing {
		return
	}
	l.failing = false
	l.logger.Info("listener lease state available again", slog.String("lease", l.key))
}
