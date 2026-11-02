package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Shutdown's drain waits for each room's appends to land before it settles the room, and a
// shutdown's settlement waits a bounded time for its document's lock. Neither wait is the
// settlement's own work, so what each costs is the question here: a wait that polls costs the
// process a wake-up every millisecond for as long as the room's appends lag, and a lock wait that
// polls costs the database a statement every millisecond. (A timer's settlement waits for no
// append: it stores the room's unstored updates itself, Ledger.flushThrough.)

// A room's append held for half a second while Shutdown drains the room: the drain waits for it
// (waitForDurableAppends) and settles the document once it lands. Its wait wakes when the append
// lands, not every millisecond of the half second, and the drain's settlement starts promptly once
// the append commits. The promptness is read between the append's commit (afterAppendCommit) and
// the settlement's start (beforeSettleStart), in process: a bound on the version's write would
// also time the settlement's own database work, which says nothing about how it is woken.
func TestShutdownsDrainWakesOnceWhenARoomsHeldAppendLands(t *testing.T) {
	const held = 500 * time.Millisecond
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	persist := NewPgVersioned(database)
	var counting atomic.Bool
	var committed, started atomic.Int64
	persist.afterAppendCommit = func(room string, _ persistence.Version) {
		if room == artifactID && counting.Load() {
			committed.CompareAndSwap(0, time.Now().UnixNano())
		}
	}
	appends := &heldStore{VersionedStore: persist, room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	service := newShutdownTestService(t, database, appends)
	var wakes atomic.Int64
	service.settlementWoke = func(room string) {
		if room == artifactID && counting.Load() {
			wakes.Add(1)
		}
	}
	service.beforeSettleStart = func(room string) {
		if room == artifactID && counting.Load() && committed.Load() != 0 {
			started.CompareAndSwap(0, time.Now().UnixNano())
		}
	}
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)
	versioned, _, _ := latestDocumentVersion(t, database, artifactID)
	observer := storetest.SecondProcess(t, database)

	// No timer's settlement runs before the drain's.
	service.settle = 30 * time.Minute
	counting.Store(true)
	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	select {
	case <-appends.entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's append never reached the store")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	time.Sleep(held)
	appends.let()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("Shutdown never returned once the room's append was let go")
	}
	counting.Store(false)

	if got := wakes.Load(); got > 3 {
		t.Errorf("the drain woke %d times while its room's append was held for %s, want at most 3", got, held)
	}
	switch {
	case committed.Load() == 0:
		t.Errorf("the held append's commit was never seen")
	case started.Load() == 0:
		t.Errorf("no settlement started after the held append committed")
	default:
		if after := time.Duration(started.Load() - committed.Load()); after > 50*time.Millisecond {
			t.Errorf("the drain's settlement started %s after its room's append committed, want within 50ms", after.Round(time.Millisecond))
		}
	}
	number, markdown, _ := latestDocumentVersion(t, observer, artifactID)
	if number <= versioned || markdown != "after\n" {
		t.Errorf("the drain versioned %q as version %d, want the held edit after version %d", markdown, number, versioned)
	}
}

// A settlement during Shutdown does not wait behind a writer that holds its document's lock: it
// gives up after about 100 ms and leaves the document to resume, as it always has. What the wait
// costs is the question: one lock statement with a bounded wait, not a try-lock every millisecond.
func TestShutdownsSettlementLockWaitIsOneStatement(t *testing.T) {
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := storetest.TracedStore(t, database, tracer)
	observer := storetest.SecondProcess(t, database)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, traced, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)

	versioned, _, _ := latestDocumentVersion(t, observer, artifactID)
	unlock := holdDocumentLock(t, observer, artifactID)
	since := tracer.mark()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	if err := service.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown with the document's lock held elsewhere: %v", err)
	}
	took := time.Since(started)
	unlock()

	if locks := tracer.statements(artifactID, "advisory_xact_lock(hashtext(", since); locks != 1 {
		t.Errorf("the shutdown's settlement sent %d lock statements for the document, want 1", locks)
	}
	if owed, err := settlementPending(context.Background(), observer.Pool, artifactID); err != nil || !owed {
		t.Errorf("the document owes no settlement (%v) after a shutdown whose settlement could not lock it, want it left to resume", err)
	}
	if number, _, _ := latestDocumentVersion(t, observer, artifactID); number != versioned {
		t.Errorf("the document's latest version is %d, want %d: the settlement left it", number, versioned)
	}
	if took < 100*time.Millisecond || took > 5*time.Second {
		t.Errorf("Shutdown took %s, want the settlement to give up on the lock after about 100ms", took.Round(time.Millisecond))
	}
}
