package duty

import (
	"context"
	"time"
)

// WokenLoop is one change-driven background loop: the work a timer loop would redo every period
// whether or not anything changed, run instead when something says it changed. RunWoken runs it.
type WokenLoop struct {
	// Lease, when set, names the lease each pass runs under (Exclusive, with no period), so two
	// processes woken by one change do not both do its work at once. Empty runs every pass
	// without one.
	Lease string
	// Leases is the pool a leased pass takes its lease connection from (Exclusive); unused when
	// Lease is empty.
	Leases Leases
	// Wake is signalled when the work may have changed: a pglisten.Hub wake, whose buffer of one
	// and non-blocking send coalesce any number of changes into one signal. A signal that
	// arrives while a pass runs stays in that buffer and earns a pass after it, so a change the
	// running pass read too early is still served. Nil is a loop that runs only at start and
	// when its passes ask to.
	Wake <-chan struct{}
	// MinGap is the least time from the start of one pass to the start of the next, whatever
	// woke it: wakes that arrive sooner collapse into the one pass at the end of the gap, so a
	// loop under load runs no more often than once per MinGap - the period of the timer it
	// replaces - and an idle one not at all.
	MinGap time.Duration
	// HeldRetry is how long a leased loop waits, plus up to a tenth again, before trying once
	// more after an attempt whose pass did not run: the lease was held by another process, or
	// could not be taken. It must be positive when Lease is set. The retry is what keeps a wake
	// from being lost to the other process's pass, which may have read before the change that
	// woke this one; the loop keeps trying until a pass of its own runs.
	HeldRetry time.Duration
	// Pass does the work, on the loop's goroutine, and returns when the loop next needs to run
	// with no wake - an attempt's retry time, a claim's expiry - or the zero time for never.
	// Each pass's answer replaces the last one's, and a due time is held to MinGap like a wake.
	Pass func(context.Context) (next time.Time)
}

// exclusiveFunc is Exclusive's signature: RunWoken passes Exclusive itself, and a test of the
// schedule passes a lease of its own making in its place.
type exclusiveFunc func(ctx context.Context, leases Leases, name string, period time.Duration, pass func(context.Context)) bool

// RunWoken runs loop until ctx is cancelled: one pass at once, then a pass on each wake and at each
// due time a pass returned, never sooner than loop.MinGap after the previous pass started. Between
// those it holds no timer and makes no statement, so a loop with nothing to do costs nothing - the
// point of the shape, against RunJittered's pass every period whatever changed.
//
// A leased loop runs each pass under Exclusive with no period, which is mutual exclusion and
// nothing else, and the lease name passes through Exclusive like every other loop's. An attempt
// Exclusive reports did not run - the lease held elsewhere, or a failure to take it - is retried
// every loop.HeldRetry until one runs: the other process's pass may have read before the change,
// and a failure must neither spin nor drop the wake. A pass shutdown cut short also reports it
// did not run, and the loop returns then rather than retrying.
func RunWoken(ctx context.Context, loop WokenLoop) {
	loop.run(ctx, Exclusive)
}

func (l WokenLoop) run(ctx context.Context, exclusive exclusiveFunc) {
	if l.Lease != "" && l.HeldRetry <= 0 {
		panic("duty: a leased WokenLoop needs a positive HeldRetry")
	}
	var (
		// owed is whether a pass is owed regardless of any due time: the boot pass, a wake, or an
		// attempt whose pass did not run.
		owed = true
		// due is when the last pass asked to run again; zero for never.
		due time.Time
		// notBefore is the earliest the next attempt may start: MinGap after the last pass
		// started, pushed later by a held attempt's retry wait.
		notBefore time.Time
		// The one timer the loop owns, re-armed rather than replaced, and the time it is armed
		// for (zero when it is not armed).
		timer *time.Timer
		armed time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var fire <-chan time.Time
		if at, ok := nextAttempt(owed, due, notBefore); ok {
			switch {
			case timer == nil:
				timer = time.NewTimer(time.Until(at))
			case !at.Equal(armed):
				timer.Reset(time.Until(at))
			}
			armed, fire = at, timer.C
		} else if !armed.IsZero() {
			timer.Stop()
			armed = time.Time{}
		}
		select {
		case <-ctx.Done():
			return
		case <-l.Wake:
			owed = true
			continue
		case <-fire:
			armed = time.Time{}
		}
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		next, ran := l.attempt(ctx, exclusive)
		if ran {
			owed, due, notBefore = false, next, started.Add(l.MinGap)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		owed = true
		notBefore = later(notBefore, time.Now().Add(jittered(l.HeldRetry)))
	}
}

// attempt runs one pass, under the lease when the loop has one, and reports whether it ran and
// the due time it returned.
func (l WokenLoop) attempt(ctx context.Context, exclusive exclusiveFunc) (next time.Time, ran bool) {
	if l.Lease == "" {
		return l.Pass(ctx), true
	}
	ran = exclusive(ctx, l.Leases, l.Lease, 0, func(ctx context.Context) {
		next = l.Pass(ctx)
	})
	return next, ran
}

// nextAttempt is when the loop's next attempt falls, and false when nothing is owed or due: an
// owed pass at the earliest the gap allows, a due one at its time or the gap's end if that is
// later.
func nextAttempt(owed bool, due, notBefore time.Time) (time.Time, bool) {
	switch {
	case owed:
		return notBefore, true
	case !due.IsZero():
		return later(due, notBefore), true
	}
	return time.Time{}, false
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
