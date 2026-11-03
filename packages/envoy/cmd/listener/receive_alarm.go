package main

import (
	"errors"
	"log/slog"
	"time"
)

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
