package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A settlement waits for its room's appends to land before it reads the room, and a shutdown's
// settlement waits a bounded time for its document's lock. Neither wait is the settlement's own
// work, so what each costs is the question here: a wait that polls costs the process a wake-up
// every millisecond for as long as the room's appends lag, and a lock wait that polls costs the
// database a statement every millisecond.

// A room's append held for half a second while its settlement falls due: the settlement waits for
// it and versions the document once it lands. Its waits wake when the append lands, not every
// millisecond of the half second, and the version follows the append promptly.
func TestALaggingRoomsSettlementWakesOnceWhenItsAppendsLand(t *testing.T) {
	const held = 500 * time.Millisecond
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	service := newShutdownTestService(t, database, appends)
	var counting atomic.Bool
	var wakes atomic.Int64
	service.settlementWoke = func(room string) {
		if room == artifactID && counting.Load() {
			wakes.Add(1)
		}
	}
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)
	versioned, _, _ := latestDocumentVersion(t, database, artifactID)
	observer := storetest.SecondProcess(t, database)

	service.settle = 20 * time.Millisecond
	counting.Store(true)
	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	select {
	case <-appends.entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's append never reached the store")
	}
	time.Sleep(held)
	released := time.Now()
	appends.let()
	waitFor(t, roomWait(t), "the settlement to version the edit", func() bool {
		number, _, _ := latestDocumentVersion(t, observer, artifactID)
		return number > versioned
	})
	written := time.Since(released)
	counting.Store(false)

	if got := wakes.Load(); got > 3 {
		t.Errorf("the settlement woke %d times while its room's append was held for %s, want at most 3", got, held)
	}
	if written > 50*time.Millisecond {
		t.Errorf("the version was written %s after the append was let go, want within 50ms", written.Round(time.Millisecond))
	}
	if _, markdown, _ := latestDocumentVersion(t, observer, artifactID); markdown != "after\n" {
		t.Errorf("the settlement versioned %q, want the held edit", markdown)
	}
}

// A settlement during Shutdown does not wait behind a writer that holds its document's lock: it
// gives up after about 100 ms and leaves the document to resume, as it always has. What the wait
// costs is the question: one lock statement with a bounded wait, not a try-lock every millisecond.
func TestShutdownsSettlementLockWaitIsOneStatement(t *testing.T) {
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := tracedStore(t, database, tracer)
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
