package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The compaction queue folds nothing while the request admission is under pressure, and folds as
// soon as it eases. Without the yield the worker folds the moment a document comes due, whatever
// the requests are doing, which is what let a burst's folds - every document it wrote, as their
// rows passed compactTail - answer requests at a 1.0 to 2.1 s p99 for the minute after the load had
// dropped (INFRA-592).
func TestTheCompactionQueueWaitsWhileTheRequestsPressAndFoldsWhenTheyEase(t *testing.T) {
	load := &pressingRequests{}
	load.press(true)
	folds, queue := yieldingCompactionQueue(t, load, time.Now)
	queue.note("a-document")

	// Long enough that a worker that did not yield would have folded several times over: the
	// queue checks a noted document at once here, and a fold of this step takes no time at all.
	time.Sleep(10 * compactionYieldWait)
	if folded := folds.Load(); folded != 0 {
		t.Fatalf("%d fold(s) ran while the requests were pressing, want none until they ease", folded)
	}
	if deferred := queue.yield.deferred.Load(); deferred != 0 {
		t.Fatalf("the yield counted %d deferred fold(s) before any fold ran; it counts a fold once, as it runs", deferred)
	}

	load.press(false)
	waitFor(t, 30*time.Second, "the fold the requests were holding back", func() bool { return folds.Load() > 0 })
	if deferred := queue.yield.deferred.Load(); deferred != 1 {
		t.Fatalf("the yield counted %d deferred fold(s), want the one it held back", deferred)
	}
	if forced := queue.yield.forced.Load(); forced != 0 {
		t.Fatalf("the yield forced %d fold(s) through, want none: the requests eased well inside the deferral bound", forced)
	}
}

// A document is folded no later than compactionMaxDeferral past the moment it came due, however the
// requests stand, so yielding to the requests can never starve the compaction. The clock the yield
// reads is a test's here, a whole deferral ahead of the queue's own, so the document this notes is
// already past its bound when the worker weighs the requests against it.
func TestTheCompactionQueueFoldsAtTheDeferralBoundHoweverTheRequestsStand(t *testing.T) {
	load := &pressingRequests{}
	load.press(true)
	ahead := func() time.Time { return time.Now().Add(compactionMaxDeferral + time.Second) }
	folds, queue := yieldingCompactionQueue(t, load, ahead)
	queue.note("a-document")

	waitFor(t, 30*time.Second, "the fold the deferral bound let through", func() bool { return folds.Load() > 0 })
	if forced := queue.yield.forced.Load(); forced == 0 {
		t.Fatal("the yield counted no forced fold, so the fold did not go through on the deferral bound")
	}
	if !load.pressing.Load() {
		t.Fatal("the test cleared the pressure it was meant to hold")
	}
}

// pressingRequests is a request admission a test presses and eases: pressed, one class holds every
// unit of its limit, which is what requestYield reads as pressure; eased, both classes hold
// nothing.
type pressingRequests struct {
	pressing atomic.Bool
}

func (r *pressingRequests) press(pressing bool) { r.pressing.Store(pressing) }

func (r *pressingRequests) Classes() []RequestClassLoad {
	if r.pressing.Load() {
		return []RequestClassLoad{{Limit: 28, InFlight: 28}, {Limit: 24}}
	}
	return []RequestClassLoad{{Limit: 28}, {Limit: 24}}
}

// yieldingCompactionQueue is a compaction queue of its own - no service, no database - whose step
// only counts the folds it is asked for and whose yield reads load on the clock now. It checks a
// noted document at once, so a fold that does not run is the yield holding it back and nothing
// else.
func yieldingCompactionQueue(t *testing.T, load RequestLoad, now func() time.Time) (*atomic.Int64, *compactionQueue) {
	t.Helper()
	folds := &atomic.Int64{}
	step := func(context.Context, string, bool) compactionNext {
		folds.Add(1)
		return compactionNext{kind: compactionDone}
	}
	queue := startCompactionQueue(step, newRequestYield(load, now, compactionMaxDeferral, compactionFoldGap))
	queue.mu.Lock()
	queue.timing = compactionTiming{backoff: time.Millisecond, backoffCap: time.Millisecond}
	queue.mu.Unlock()
	t.Cleanup(func() {
		queue.shutDown()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		queue.awaitStopped(ctx)
	})
	return folds, queue
}
