package docs

// What the document compaction yields to: the requests. The compaction queue's worker
// (compactionQueue.run) and the hourly sweep (compactionSweepPass) ask a requestYield before every
// fold, and wait while the request admission is under pressure, bounded so that no document waits
// longer than compactionMaxDeferral past the moment it came due.

import (
	"context"
	"sync/atomic"
	"time"
)

// RequestClassLoad is what one class of the request admission holds now: the units its admitted
// requests hold (InFlight) against the units that class may hold (Limit), and how many requests are
// queued for room (Queued).
type RequestClassLoad struct {
	Limit, InFlight, Queued int
}

// RequestLoad is the request admission as the document compaction reads it, and the whole of what
// the service knows about the requests (Deps.Requests): the state of each class, read together.
// cmd/dispatch hands the service the one admission limiter the API admits through, so the
// compaction and the requests read one set of counts. A service with none (every test but the
// compaction's own, and api.NewDeps's fallback service) folds without ever waiting, as Dispatch did
// before the yield.
type RequestLoad interface {
	Classes() []RequestClassLoad
}

// What counts as pressure, and how long a fold may be held off for it. The share is of a class's
// own limit, and both numbers are measured on the load test's recovery step
// (packages/envoy/loadtest, 0.5x:60s,10x:60s,0.5x:120s), where the two classes hold 28 and 24
// units.
//
// The first measurement set the share at half a class's limit, and that was wrong: through the
// whole of the wave the folds were slowing - 20 to 40 folds a second for the minute after the drop
// - the admitted requests held 0 to 5 units of each class with nothing queued at any 5 s sample,
// because 30 requests a second answered in 50 to 150 ms hold about two units. Admission fills only
// once latency is already seconds long, so a share high enough to mean "no room" never trips while
// background work is merely taking the processor. A quarter of a class's limit - 7 of the 28 read
// units, 6 of the 24 write units - is what the measurement separates: a healthy 0.5x sits at 0 to
// 5, its busier samples at 7 to 20, and the 10x minute pins both classes with 44 to 56 requests
// queued.
//
// And a share alone is not enough, which is the other half of that measurement: folds that each
// take tens of milliseconds of processor, run back to back, slow requests that admission never
// counts as pressed. So a fold waits for compactionFoldGap behind the last one whenever any
// request at all is in flight - a server with nothing in flight folds as fast as it likes - which
// holds the fold rate to ten a second where the wave ran at 19 to 47. Ten a second is as slow as
// the gap may be: at 500 ms the queue's length grew steadily through the whole of the step's
// second 0.5x minute (377 to 494 documents at 2.4 folds a second), so the folds no longer kept up
// with the documents the ordinary load makes due, and compaction falling behind for good is what
// the deferral bound exists to prevent.
//
// compactionMaxDeferral is what keeps the compaction from being starved in turn. A document comes
// due no sooner than compactTail (2 minutes) after the rows it folds were written, so 45 seconds
// past that puts the worst case at about two and three quarter minutes from a write to its fold,
// well inside the compactMaxTailAge (15 minutes) the schedule already allows. A document waiting
// its turn in the queue is not deferred and is not bounded by it: the bound is on how long the
// requests may hold one fold back.
//
// compactionYieldWait is how long the worker waits before it reads the load again. It is a lock on
// the limiter each time, so it is slow enough to cost the admission nothing (at most 900 reads over
// the whole deferral) and quick enough that a fold starts within a tick of the requests easing.
const (
	compactionPressureShare = 25
	compactionMaxDeferral   = 45 * time.Second
	compactionYieldWait     = 50 * time.Millisecond
	compactionFoldGap       = 100 * time.Millisecond
)

// requestYield holds one piece of background work back while the requests press. A service makes
// one (New), the compaction's, shared by the queue's worker and the sweep, which never fold at the
// same time: the sweep hands every document it cannot fold to the queue rather than compacting
// beside it. The settlements were measured against it and are not held back (AGENTS.md, the
// settlement slots): a settlement that waits is one whose room's generation is likelier to move
// while it waits, which ends it and arms another, so deferring them multiplies them.
type requestYield struct {
	// load is the request admission, nil in a service that was handed none.
	load RequestLoad
	// maxDeferral is the longest the requests may hold one piece of this work back past the
	// moment it came due, and gap the least time between two pieces of it while any request is in
	// flight; zero paces nothing.
	maxDeferral, gap time.Duration
	// now and rest are the clock and the wait, the service's own; a test moves them.
	now  func() time.Time
	rest func(ctx context.Context, d time.Duration) bool
	// deferred counts the pieces that waited for the requests at all, forced those the deferral
	// bound let through while the requests were still pressing, and paced those the gap held back
	// on a busy server.
	deferred, forced, paced atomic.Int64
	// last is when the last piece was let through. It is read and written by whichever worker is
	// running the work, and the two that share one of these never run at once.
	last time.Time
}

func newRequestYield(load RequestLoad, now func() time.Time, maxDeferral, gap time.Duration) *requestYield {
	return &requestYield{load: load, maxDeferral: maxDeferral, gap: gap, now: now, rest: sleepUnlessDone}
}

// sleepUnlessDone waits d, and reports false when ctx ended first.
func sleepUnlessDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// await holds its caller before one piece of background work that came due at due - a fold, a
// settlement: while the requests press it waits, until they ease, until ctx ends, or until this
// work's maxDeferral past due, when the piece goes through anyway. Once they have eased it still
// spaces the pieces behind the last one while any request is in flight (gap), so a backlog does not
// take the processors back to back on a server that is serving.
//
// It logs nothing: a burst leaves one of these per document it wrote, and the counters below are
// what says how often it held one back.
func (y *requestYield) await(ctx context.Context, due time.Time) {
	if y == nil || y.load == nil {
		return
	}
	bound := due.Add(y.maxDeferral)
	waited, paced := false, false
	for {
		pressing, serving := classifyRequestLoad(y.load)
		now := y.now()
		switch {
		case pressing && !now.Before(bound):
			y.forced.Add(1)
		case pressing:
			waited = true
			if y.rest(ctx, compactionYieldWait) {
				continue
			}
		case serving && y.gap > 0 && now.Sub(y.last) < y.gap:
			paced = true
			if y.rest(ctx, y.gap-now.Sub(y.last)) {
				continue
			}
		}
		break
	}
	if waited {
		y.deferred.Add(1)
	}
	if paced {
		y.paced.Add(1)
	}
	y.last = y.now()
}

// classifyRequestLoad reads every class of the request admission once: pressing is a class with a
// request queued for room or holding at least compactionPressureShare of its limit, serving one
// holding anything at all.
func classifyRequestLoad(load RequestLoad) (pressing, serving bool) {
	for _, class := range load.Classes() {
		if class.Queued > 0 {
			pressing = true
		}
		if class.InFlight > 0 {
			serving = true
		}
		if class.Limit > 0 && class.InFlight*100 >= class.Limit*compactionPressureShare {
			pressing = true
		}
	}
	return pressing, serving || pressing
}

// CompactionsDeferred is how many folds have waited for the requests to ease
// (dispatch_doc_compactions_deferred_total), CompactionsForced how many the deferral bound let
// through while they were still pressing (dispatch_doc_compactions_forced_total), and
// CompactionsPaced how many the gap between folds held back on a busy server
// (dispatch_doc_compactions_paced_total). CompactionsQueued is how many documents the compaction
// queue holds now, the one in flight included (dispatch_doc_compactions_queued): what a burst
// leaves to fold.
func (s *Service) CompactionsDeferred() int64 {
	return s.compactions.yield.deferred.Load()
}

func (s *Service) CompactionsForced() int64 {
	return s.compactions.yield.forced.Load()
}

func (s *Service) CompactionsPaced() int64 {
	return s.compactions.yield.paced.Load()
}

func (s *Service) CompactionsQueued() int64 {
	return int64(s.compactions.held())
}
