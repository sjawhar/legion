// The deregistration's fallback (releaseAllRoleClaimsByScan) is one scan of the whole role
// bucket, and the request waiting for it has a budget. Two mechanisms hold that scan to the
// budget, and the two are independent:
//
//   - the caller's bound. The listing runs under bus.Bounded, which returns at the request's end
//     while the listing's goroutine runs on, so the fallback answers its caller rather than the
//     bucket.
//   - the scan's own stop. roleClaimsUntil's visit function returns ctx.Err() before it records
//     a claim, which ends the scan (kvwatch.ScanExistingKeys) and has it stop and drain its
//     watcher, so a listing the request walked away from costs the bucket nothing more.
//
// Each is invisible to the other's test, which is why there are two cases here rather than one. A
// scan that is delivered nothing never runs its visit function, so its own stop cannot fire and
// only the caller's bound can answer that deregistration; a scan that is being delivered entries
// reaches its visit function, and its caller's bound answers while the listing's goroutine runs
// on behind the answer, so only the scan's own stop ends the reading of the bucket. Before these
// two cases nothing in the tree held either bound: the review that asked for them deleted the
// ctx.Err() check and ran the package green, then called the listing with no bus.Bounded and ran
// it green again.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

const (
	// stalledListingDeadline is the request's budget in the stalled-listing case. It stands in
	// for the route's own, and what matters about it is that it is a fiftieth of
	// kvwatch.ScanIdleTimeout (10 s), the earliest a stalled scan ends on its own account.
	stalledListingDeadline = 200 * time.Millisecond
	// stalledListingBound is how long that deregistration may take. It is ten times the deadline
	// the request carries, so a loaded runner does not fail the case, and a fifth of
	// kvwatch.ScanIdleTimeout, so a fallback that waited for the stalled listing fails it on any
	// runner.
	stalledListingBound = 2 * time.Second

	// walkedAwayClaims is how many claims the bucket holds in the walked-away case, and
	// walkedAwayEntryDelay how long the fake watch holds each one before it hands it over: the
	// whole bucket takes walkedAwayClaims * walkedAwayEntryDelay (2 s) to deliver, which is more
	// than ten times the request's budget below, so a scan that read the bucket to its end is
	// told apart from one that stopped by a count rather than by a clock.
	walkedAwayClaims     = 200
	walkedAwayEntryDelay = 10 * time.Millisecond
	// walkedAwayDeadline is the request the deregistration is given, long enough for its listing
	// to be handed a few claims and far short of the bucket.
	walkedAwayDeadline = 150 * time.Millisecond
	// walkedAwayCeiling is the most entries a stopped listing may have been handed. The request
	// above pays for about fifteen, so this leaves an order of magnitude, and it is half the
	// bucket, so a listing that read the bucket to its end cannot pass under it.
	walkedAwayCeiling = walkedAwayClaims / 2
	// walkedAwayDrift is how many entries the fake may forward after the scan has stopped: the
	// one that can already be in flight on its way to the drain when the stop lands, and one
	// more for the reader of these numbers.
	walkedAwayDrift = 2
)

// watchRoleBucketThrough installs watch as the watch side of reg's role bucket handle, leaving
// the legacy side as it is, and puts the previous handle back when the test ends. It is
// countRoleBucketCalls' sibling for the other side of that handle: a listing of the bucket's keys
// is a watch of it (kvwatch.ScanExistingKeys), not a Get, so a case about what a listing does
// fakes this side and reads no counter on the other. The swap is made under the mutex that guards
// the handle (Registry.kvMu), which every reader of it takes (Registry.roles), for the reason
// swapRoleBucketHandle's is.
func watchRoleBucketThrough(t *testing.T, reg *Registry, watch jetstream.KeyValue) {
	t.Helper()
	reg.kvMu.Lock()
	previous := reg.roleKV
	reg.roleKV = bus.NewKeyValue(previous.Legacy(), watch, previous.Streams())
	reg.kvMu.Unlock()
	t.Cleanup(func() {
		reg.kvMu.Lock()
		defer reg.kvMu.Unlock()
		reg.roleKV = previous
	})
}

// A deregistration whose role claim view cannot be served falls back to a scan of the whole role
// bucket, and that scan has an idle bound of its own measured in seconds (kvwatch.ScanIdleTimeout,
// 10 s). The request waiting for it has a budget measured in hundreds of milliseconds, and the
// fallback must answer within the request's: an HTTP handler that waits out the scan's instead is
// the 503 this issue was filed on, and the answer the caller is still owed is the one the route
// reports.
//
// The watch here delivers no entry at all, which is what makes this case about bus.Bounded and
// nothing else: the scan sits inside drainScan with an empty channel, so roleClaimsUntil's visit
// function never runs and the scan's own stop cannot fire. The only thing in the tree that can
// answer this deregistration before the scan's 10 s idle bound is the bound its caller holds. Run
// the listing as r.roleClaimsUntil(ctx) with no bus.Bounded around it and Remove answers after
// that 10 s, with the scan's timeout rather than the request's deadline.
func TestTheDeregistrationFallbackAnswersItsRequestWhileItsListingIsStillStalled(t *testing.T) {
	const session = "ses_stalled_listing"
	held := []string{"stalled-held-a", "stalled-held-b"}
	reg, _, _ := smallRoleBucketRegistry(t, session, held)
	// after 0, ended false: the watcher forwards nothing and never ends, the stalled scan
	// earlyEndKV documents.
	watchRoleBucketThrough(t, reg, earlyEndKV{KeyValue: reg.roles().WatchHandle()})

	ctx, cancel := context.WithTimeout(context.Background(), stalledListingDeadline)
	defer cancel()
	started := time.Now()
	err := reg.Remove(ctx, session, nil)
	elapsed := time.Since(started)
	t.Logf("the deregistration answered %v after %s, with the stalled scan's own idle bound at %s", err, elapsed, kvwatch.ScanIdleTimeout)

	// The request's own deadline, not the scan's timeout: a fallback that waited for the listing
	// would answer kvwatch.ErrScanTimedOut instead, and ten seconds later.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Remove(%s) over a stalled listing answered %v, want the request's own deadline", session, err)
	}
	// The wording is what the route reports to the caller, so it names what was being done and
	// for which session; a bare context error says neither.
	want := fmt.Sprintf("list the role bucket's keys to release %s's claims:", session)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Remove(%s) answered %q, want it to say %q and carry the deadline", session, err, want)
	}
	if elapsed >= stalledListingBound {
		t.Fatalf("the deregistration answered after %s, want it inside %s: the scan it is waiting for delivers nothing until its own %s idle bound, so an answer that slow is the fallback waiting for the listing rather than for its request",
			elapsed, stalledListingBound, kvwatch.ScanIdleTimeout)
	}
	// The deregistration failed, so nothing of it landed: the interest record is still there for
	// the caller's retry, as it is after any other failed sweep.
	if _, err := reg.Get(context.Background(), session); err != nil {
		t.Fatalf("read %s's interest record after the refused deregistration: %v, want the record left for the retry", session, err)
	}
}

// slowWatchKV hands out a watcher over the real bucket that forwards one entry per delay and
// counts what it has forwarded, so a case can read how much of the bucket a listing was handed.
// It is earlyEndKV's opposite number: that one stalls a scan, so the scan's visit function never
// runs, and this one keeps a scan fed, so its visit function runs on every entry and a scan that
// stops has to stop itself.
//
// The forwarding stops the moment the watcher is stopped, which is what a scan that ends does
// (kvwatch.drainScan's done: Stop, then a drain of what is left). Entries nats.go has already
// buffered are then read off the real watcher without forwarding them - leaving them unread would
// park nats.go's delivery goroutine for good on a full buffer - so the count stands still once
// the scan has stopped, and a count that goes on climbing is a listing still reading the bucket.
type slowWatchKV struct {
	jetstream.KeyValue

	delay time.Duration

	mu        sync.Mutex
	forwarded int
}

func (k *slowWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	slow := &slowWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry), stopped: make(chan struct{})}
	go func() {
		defer close(slow.updates)
		for {
			var entry jetstream.KeyValueEntry
			select {
			case got, open := <-watcher.Updates():
				if !open {
					return
				}
				entry = got
			case <-slow.stopped:
				k.drain(watcher)
				return
			}
			select {
			case <-time.After(k.delay):
			case <-slow.stopped:
				k.drain(watcher)
				return
			}
			select {
			case slow.updates <- entry:
				k.mu.Lock()
				k.forwarded++
				k.mu.Unlock()
			case <-slow.stopped:
				k.drain(watcher)
				return
			}
		}
	}()
	return slow, nil
}

// drain reads the stopped watcher's remaining entries, without delaying or counting them: they
// reach no caller, and nats.go's delivery callback blocks on a full buffer holding the watcher's
// lock, so a stopped watcher left unread parks its subscription's goroutine.
func (k *slowWatchKV) drain(watcher jetstream.KeyWatcher) {
	for range watcher.Updates() {
	}
}

// count is how many entries the fake has handed its caller.
func (k *slowWatchKV) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.forwarded
}

type slowWatcher struct {
	jetstream.KeyWatcher

	updates chan jetstream.KeyValueEntry
	stop    sync.Once
	stopped chan struct{}
}

func (w *slowWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *slowWatcher) Stop() error {
	w.stop.Do(func() { close(w.stopped) })
	return w.KeyWatcher.Stop()
}

// A listing whose request has ended must stop reading the bucket, not finish the scan for a
// caller that is gone. The scan has a context of its own (kvwatch.ScanExistingKeys builds one),
// so nothing outside it ends it: roleClaimsUntil's visit function returns ctx.Err() at the first
// key it is handed once the request is over, which ends the scan and has it stop and drain its
// watcher. Left running, it holds an ordered consumer and appends to a map nobody reads until
// the bucket's last key arrives or its 10 s idle bound elapses - against a bucket already too far
// behind to serve the claim view, which is the only reason this listing runs at all - while the
// client's retry starts a second scan of the whole thing.
//
// The watch here hands the scan the bucket one claim at a time, 10 ms apart, so the scan's visit
// function runs on every one of them and the whole bucket takes 2 s to deliver against a request
// of 150 ms.
//
// The two subtests hold the two halves of that, and each is driven the only way that can hold
// its half. Called directly, roleClaimsUntil answers its own caller, so the answer is the half a
// direct call holds - the request's error and no claims - and the listing is over by the time
// that call returns: nothing is left reading the bucket, so there is nothing after the answer to
// check. Through a deregistration, which is the production shape, the listing runs inside
// bus.Bounded, which answers its caller at the request's end and leaves the listing's goroutine
// running behind that answer: there the answer says nothing about the bucket, and what the fake
// is handed after it is the whole question the scan's own stop exists for.
//
// Delete the ctx.Err() check in roleClaimsUntil and each subtest fails on its own half. The
// direct call reads the bucket to its end and answers all walkedAwayClaims claims and no error,
// two seconds late, so its error, its claims and its ceiling all fail. The deregistration still
// answers at its deadline, because bus.Bounded answers it, and its listing goes on being handed
// the bucket behind that answer, so what fails there is the check after the answer.
func TestARoleListingTheRequestWalkedAwayFromStopsReadingTheBucket(t *testing.T) {
	const session = "ses_walked_away"
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, rawInterests := coldRegistry(t, conn)
	roles, holderOf := otherSessionsClaims("walked-away", walkedAwayClaims)
	putRoleClaimsInParallel(t, reg.roles().Legacy(), roles, holderOf)
	putInterest(t, rawInterests, Interest{SessionID: session, MachineID: "m1", Topics: []string{"notifications.chat"}})
	// Through the bucket's own watch the listing is complete, which is what makes the counts
	// below a reading of how much of the bucket the stopped listing was handed rather than of
	// how much of it there is.
	seeded, err := reg.roleClaimsUntil(context.Background())
	if err != nil {
		t.Fatalf("list the role bucket's keys before the case's own watch is installed: %v", err)
	}
	if len(seeded) != walkedAwayClaims {
		t.Fatalf("the fixture's bucket holds %d claims, want %d", len(seeded), walkedAwayClaims)
	}
	if err := reg.RoleViewErr(); err == nil {
		t.Fatal("the registry's role claim view can be served, so the deregistration below would never reach the listing")
	}

	t.Run("the listing answers its caller the request's error and no claims", func(t *testing.T) {
		watch := &slowWatchKV{KeyValue: reg.roles().WatchHandle(), delay: walkedAwayEntryDelay}
		watchRoleBucketThrough(t, reg, watch)

		ctx, cancel := context.WithTimeout(context.Background(), walkedAwayDeadline)
		defer cancel()
		started := time.Now()
		claims, err := reg.roleClaimsUntil(ctx)
		elapsed := time.Since(started)
		handed := watch.count()
		t.Logf("the listing answered %v after %s, having been handed %d of the bucket's %d claims", err, elapsed, handed, walkedAwayClaims)

		// A listing short of the bucket is not a reading of it, so it is an error and no
		// claims, and the error is the request's: a caller cannot tell a partial list from a
		// complete one.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a listing whose request ended after %s answered %d claims and %v, want no claims and the request's deadline", elapsed, len(claims), err)
		}
		if claims != nil {
			t.Fatalf("the refused listing returned %d claims, want none", len(claims))
		}
		if handed > walkedAwayCeiling {
			t.Fatalf("the stopped listing had been handed %d of the bucket's %d claims by the time it answered, want no more than %d: a listing that was handed most of the bucket in a %s request is one that read past its request",
				handed, walkedAwayClaims, walkedAwayCeiling, walkedAwayDeadline)
		}
	})

	t.Run("the deregistration's listing stops reading the bucket behind its answer", func(t *testing.T) {
		watch := &slowWatchKV{KeyValue: reg.roles().WatchHandle(), delay: walkedAwayEntryDelay}
		watchRoleBucketThrough(t, reg, watch)

		ctx, cancel := context.WithTimeout(context.Background(), walkedAwayDeadline)
		defer cancel()
		started := time.Now()
		err := reg.Remove(ctx, session, nil)
		elapsed := time.Since(started)
		answered := watch.count()
		t.Logf("the deregistration answered %v after %s, its listing having been handed %d of the bucket's %d claims", err, elapsed, answered, walkedAwayClaims)

		// The fallback answers its request rather than the bucket, in the wording the route
		// reports. That bound is the stalled-listing case's subject, and it is held here only
		// so far as the rest of this case needs it: a deregistration that answered something
		// else never reached the listing this one is about.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Remove(%s) over a listing short of the bucket answered %v, want the request's own deadline", session, err)
		}
		want := fmt.Sprintf("list the role bucket's keys to release %s's claims:", session)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Remove(%s) answered %q, want it to say %q and carry the deadline", session, err, want)
		}
		if answered > walkedAwayCeiling {
			t.Fatalf("the listing had been handed %d of the bucket's %d claims by the time the deregistration answered, want no more than %d: a listing handed most of the bucket in a %s request is one that read past its request",
				answered, walkedAwayClaims, walkedAwayCeiling, walkedAwayDeadline)
		}

		// The check the answer itself cannot make, and the reason this half runs through the
		// deregistration: the listing's goroutine outlives the answer, so the bucket is read
		// again after the whole bucket could have been delivered twice over. A listing still
		// reading it would have been handed every claim by now, even on a runner several times
		// slower than this delay assumes.
		settle := 2 * walkedAwayClaims * walkedAwayEntryDelay
		time.Sleep(settle)
		after := watch.count()
		t.Logf("%s after the deregistration answered, the bucket had been read for %d claims, up from %d", settle, after, answered)
		if after > answered+walkedAwayDrift {
			t.Fatalf("the bucket was read for %d more claims in the %s after the deregistration answered (%d, up from %d), want no more than %d: the listing is still scanning the bucket for a caller that has its answer",
				after-answered, settle, after, answered, walkedAwayDrift)
		}
	})
}
