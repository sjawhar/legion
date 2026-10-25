package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pglisten"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Step 6a refuses the durable writes of a room behind the head; these are its other half. A room
// this process holds for a document the other process wrote is closed within the notification
// round trip (stale_room.go), so its browsers reconnect and re-sync the stored document instead of
// reading text the store no longer holds until the room leaves at roomIdleTimeout. The tests here
// run the evictor over two services on one database, as production's two tasks are.

// staleRoomNote is one notification the evictor under test decided, and what it decided.
type staleRoomNote struct {
	room    string
	evicted bool
}

// runStaleRoomEvictor starts service's evictor, once its LISTEN stands: a notification sent
// before that reaches nobody, so a test that writes first would prove nothing. The notes are
// what it decided, one per notification, so a test waits for its own write's notification to be
// decided rather than guessing how long that takes. It stops with the test.
func runStaleRoomEvictor(t *testing.T, service *Service) <-chan staleRoomNote {
	t.Helper()
	return runStaleRoomEvictorListening(t, newStaleRoomEvictor(service))
}

// runStaleRoomEvictorListening is runStaleRoomEvictor for a test that builds the evictor itself,
// to set the clock its interval is measured on or its listener's waits.
func runStaleRoomEvictorListening(t *testing.T, evictor *staleRoomEvictor) <-chan staleRoomNote {
	t.Helper()
	notes, connected := startStaleRoomEvictor(t, evictor)
	select {
	case <-connected:
	case <-time.After(roomWait(t)):
		t.Fatal("the stale-room evictor never listened")
	}
	return notes
}

// startStaleRoomEvictor runs evictor until the test ends and reports what it does: notes, one per
// notification it decided, and connected, one per LISTEN that stood and the sweep that follows it
// (staleRoomEvictor.connected). It waits for neither, because a test whose dial is down at the
// start has to write before any LISTEN stands.
func startStaleRoomEvictor(t *testing.T, evictor *staleRoomEvictor) (<-chan staleRoomNote, <-chan struct{}) {
	t.Helper()
	listening := make(chan struct{}, 4)
	notes := make(chan staleRoomNote, 64)
	evictor.listening = func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	}
	evictor.handled = func(artifactID string, evicted bool) {
		select {
		case notes <- staleRoomNote{room: artifactID, evicted: evicted}:
		default:
			t.Errorf("the test read no notification note for %s", artifactID)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		evictor.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the stale-room evictor did not return within 10s of its context ending")
		}
	})
	return notes, listening
}

// waitStaleRoomNote is the evictor's decision on the next notification for artifactID, ignoring
// any other document's.
func waitStaleRoomNote(t *testing.T, notes <-chan staleRoomNote, artifactID, when string) staleRoomNote {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case note := <-notes:
			if note.room == artifactID {
				return note
			}
		case <-deadline:
			t.Fatalf("%s: the evictor heard nothing for %s", when, artifactID)
		}
	}
}

// requireNoStaleRoomNote fails unless the evictor decides no notification for artifactID. The two
// rows that use it are about a room nothing notifies for again, and a notification decided for it
// would be a second explanation for the eviction each of them waits on. A notification already on
// its way is milliseconds behind the write that sent it - that round trip is what
// TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip measures - so a tenth of a
// second is past it.
func requireNoStaleRoomNote(t *testing.T, notes <-chan staleRoomNote, artifactID, when string) {
	t.Helper()
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case note := <-notes:
			if note.room == artifactID {
				t.Fatalf("%s: the evictor decided a notification for %s (evicted=%t)", when, artifactID, note.evicted)
			}
		case <-deadline:
			return
		}
	}
}

// A write committed on process B closes the room process A holds for that document, within the
// notification's round trip and not at the room's idle timeout (a minute, roomIdleTimeout). The
// browser connected to that room sees its socket close, reconnects, and re-syncs the stored
// document, B's paragraph included. Without the eviction it would hold A's text, which the store
// no longer has, for as long as it stayed connected.
func TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip(t *testing.T) {
	pair := newProcessPair(t)
	runStaleRoomEvictor(t, pair.taskA)
	browser := connectPeer(t, pair.serverA.URL, pair.artifactID, "alice")
	waitForPeerDocument(t, browser, storedMarkdown(t, pair.database, pair.artifactID))

	started := time.Now()
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 2,
		model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, "Bob writes on the other process.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	stored := storedMarkdown(t, pair.database, pair.artifactID)
	if !strings.Contains(stored, "Bob writes on the other process.") {
		t.Fatalf("the stored document after the other process's write = %q, want its paragraph", stored)
	}

	select {
	case <-browser.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the browser's socket was still open %s after the other process's write; its copy holds %q",
			time.Since(started).Round(time.Millisecond), peerMarkdown(browser))
	}
	if closed := time.Since(started); closed >= roomIdleTimeout {
		t.Fatalf("the socket closed after %s, which is the room's idle timeout (%s), not the notification's round trip", closed, roomIdleTimeout)
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)

	// What the browser does next: its provider reconnects and syncs, and the room it reaches is
	// loaded from the stored document. A new copy, as a provider that lost its document has.
	again := connectPeer(t, pair.serverA.URL, pair.artifactID, "alice")
	waitForPeerDocument(t, again, stored)
}

// This process's own writes evict nothing: a browser typing here appends several updates a
// second, each notifying every process, and a room closed on its own write would close under
// every keystroke. The room's cursor is what tells them apart - its own append advanced it
// under the lock it appended in, so the room stands at the head - and this waits for each
// keystroke's own notification to be decided rather than for a moment to pass.
func TestARoomIsNotEvictedByItsOwnProcessesWrites(t *testing.T) {
	pair := newProcessPair(t)
	notes := runStaleRoomEvictor(t, pair.taskA)
	alice := model.Actor{Kind: "user", ID: "alice"}
	editAsConnectedPeer(t, pair.taskA, pair.artifactID, 1, alice, appendBlocks(t, "Alice types once.\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	if room == nil {
		t.Fatal("task A holds no room after its own browser's edit")
	}
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the first keystroke"); note.evicted {
		t.Fatal("task A's own write evicted its own room")
	}

	for keystroke := range 4 {
		editAsConnectedPeer(t, pair.taskA, pair.artifactID, 1, alice,
			appendBlocks(t, "Alice types again.\n"))
		waitForLandedAppends(t, pair.taskA, pair.artifactID)
		if cursor := pair.taskA.cursorOf(room); cursor == nil || cursor.heldThrough() != storedHead(t, pair.database, pair.artifactID) {
			t.Fatalf("keystroke %d: the room's cursor is not the head, so this test no longer tests what it says", keystroke)
		}
		if note := waitStaleRoomNote(t, notes, pair.artifactID, fmt.Sprintf("keystroke %d", keystroke)); note.evicted {
			t.Fatalf("keystroke %d: task A's own write evicted its own room", keystroke)
		}
		if now := pair.taskA.srv.GetDoc(pair.artifactID); now != room {
			t.Fatalf("keystroke %d: task A's room was replaced by its own process's write", keystroke)
		}
	}

	// The same room, left behind by the other process, is evicted: the five decisions above were
	// made by an evictor that does evict, on a room it was right to leave alone.
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 2,
		model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, "Bob writes on the other process.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the other process's write"); !note.evicted {
		t.Fatal("the other process's write evicted nothing, so this test never exercised an evictor that evicts")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A burst of writes on the other process evicts a room once, not once per update: a browser's
// updates each append and notify on their own, and a room closed on every one of them would be
// taken from under its readers before any reconnect could finish. The interval is measured on the
// evictor's clock, which this test holds still and then moves past staleRoomEvictInterval.
func TestABurstOnTheOtherProcessEvictsARoomOnceWithinTheInterval(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	now := time.Now()
	evictor.now = func() time.Time { return now }
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}

	evictions := 0
	for update := range 3 {
		// Each update of the burst leaves task A's room behind the head again: without the
		// interval every one of them would be an eviction.
		pair.leaveTaskABehind(t, uint64(update+2), bob, "Bob types in a burst.\n")
		if evictor.documentChanged(ctx, pair.artifactID) {
			evictions++
		}
	}
	if evictions != 1 {
		t.Fatalf("evictions over a burst of three updates = %d, want 1", evictions)
	}

	now = now.Add(staleRoomEvictInterval)
	pair.leaveTaskABehind(t, 9, bob, "Bob types after the interval.\n")
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatalf("a write %s after the eviction evicted nothing, so a room left behind after a burst stays behind", staleRoomEvictInterval)
	}
}

// A notification the interval refuses takes no document lock. The interval is there for a burst
// on the other process - one person typing there notifies several times a second - and every one
// of those notifications is for a room that really is behind, so the unlocked head read passes it
// through to the confirmation under the document's advisory lock (roomBehindDurableHead), which
// waits behind the very append that announced the change. pglisten calls OnNotify inline on its
// one listener goroutine, so each of those waits stalls every other room's notification with it.
// This test holds the document's lock in a transaction of its own and requires a notification
// inside the interval to be decided while that lock is still held: a decision that reaches the
// lock cannot be, so a prompt refusal is the proof that the lock is never taken. With the
// interval asked after the locked confirmation, the call below waits for this test's own
// transaction instead, and the test fails on its bounded wait.
func TestANotificationRefusedByTheIntervalTakesNoDocumentLock(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	now := time.Now()
	evictor.now = func() time.Time { return now }
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}

	pair.leaveTaskABehind(t, 2, bob, "Bob types once.\n")
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to refuse the next notification by")
	}
	// Task A's room back and behind the head again, as the next update of a burst leaves it: the
	// notification below is one the evictor would confirm under the lock if the interval let it.
	pair.leaveTaskABehind(t, 3, bob, "Bob types again inside the interval.\n")

	held, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the document's lock: %v", err)
	}
	defer held.Rollback(context.Background())
	if err := lockDocumentRoom(ctx, held, pair.artifactID); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}

	decided := make(chan bool, 1)
	go func() { decided <- evictor.documentChanged(ctx, pair.artifactID) }()
	select {
	case evicted := <-decided:
		if evicted {
			t.Fatalf("a second write within %s evicted the room again, under its readers", staleRoomEvictInterval)
		}
	case <-time.After(roomWait(t)):
		t.Fatalf("the notification the interval refuses is waiting on the document's lock: the interval is asked after the locked confirmation, so every update of a burst on the other process stalls this listener goroutine behind that process's append")
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("the room the interval refused to evict is gone")
	}

	// And what refused it was the interval and nothing else: the same room, with the lock
	// released and the clock moved past the interval, is evicted.
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("release the document's lock: %v", err)
	}
	now = now.Add(staleRoomEvictInterval)
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatalf("the room was not evicted %s after its eviction, so what refused the notification above was not the interval", staleRoomEvictInterval)
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// The last write of a burst converges with no further notification. Every notification inside the
// interval's window is refused, the burst's last one among them, and a document nobody writes
// again notifies nobody again: what closes the room is the refusal itself, re-examined on the
// listener's own goroutine once the window ends (wakeIn, woke). Left there, that room holds text
// the store replaced for as long as it stays resident, which for a room with a tab in it is for
// as long as the tab is open - roomIdleTimeout is time since the last peer left. The evictor runs
// through its real run loop here, so the wake path is what drives this and not a call of the
// test's own, and the interval is measured on the evictor's clock, which this test holds still
// over the burst and then moves past the window.
func TestTheLastWriteOfABurstEvictsTheRoomWithoutAnotherNotification(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	evictor.now = func() time.Time { return time.Unix(0, clock.Load()) }
	// Past every wait this test makes, so the wake and never the idle ping is what ends the
	// listener's wait here.
	evictor.listenIdle = time.Minute
	notes := runStaleRoomEvictorListening(t, evictor)
	bob := model.Actor{Kind: "user", ID: "bob"}

	// The burst's first write, which evicts: it is the eviction the interval refuses the write
	// below by.
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 2, bob, appendBlocks(t, "Bob types once.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the burst's first write"); !note.evicted {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to refuse the burst's last write by")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)

	// Task A's room again, held resident by the load: no peer of its own is in it, and nothing on
	// this process writes it, so for roomIdleTimeout the only thing that can close it is the
	// evictor.
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room again: %v", err)
	}

	// The burst's last write: inside the window, so the interval refuses it, and nothing writes
	// this document again.
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 3, bob, appendBlocks(t, "Bob types again.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the burst's last write"); note.evicted {
		t.Fatalf("the write inside the %s window evicted the room again, under readers the eviction before it had not finished re-syncing", staleRoomEvictInterval)
	}
	if !roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room stands at the head after the burst's last write, so nothing here is tested")
	}

	clock.Add(int64(staleRoomEvictInterval))
	// Well inside roomIdleTimeout: a room this test waited that long for would be one that left
	// because nothing was in it, which is not the closure this row is about.
	waitFor(t, min(roomWait(t), roomIdleTimeout/2), fmt.Sprintf(
		"the room the interval refused for %s to be evicted when that window ended, with nothing to notify this process again",
		staleRoomEvictInterval), func() bool {
		return pair.taskA.srv.GetDoc(pair.artifactID) == nil
	})
	requireNoStaleRoomNote(t, notes, pair.artifactID, "after the window ended")
}

// A room whose re-examination reaches no verdict is tried again at the next window, not dropped.
// The wake takes a room out of the deferred set to re-examine it (dueRooms), so the window the
// interval's refusal armed is spent the moment that wake begins, and a re-examination that then
// gets no answer used to leave nothing behind at all. What that drops is the burst's last write:
// every notification inside the window is refused, the last of them is one of those, and a
// document nobody writes again notifies nobody again, so the room stays resident behind the head
// with its reader on text the store replaced - roomIdleTimeout counts from the last peer leaving,
// which a room with a tab in it never reaches. Here the window is spent only by a verdict, and
// the attempt that got no answer arms the next window (armRetry), one interval on, so a room
// whose reads keep failing is retried on the interval rather than as fast as the listener loops.
//
// The no-answer path here is the unlocked head read: the pool refuses a caller already holding
// one of its connections (store.ErrNestedAcquire), which documentChanged meets as it would any
// failed read. A close that closed nothing is the other way to reach no verdict; its re-arm is
// asserted by the evictor row of TestACloseOfARoomBehindTheHeadLeavesTheRoomThatReplacedIt.
func TestAStaleRoomWhoseReExaminationGetsNoAnswerIsTriedAgainAtTheNextWindow(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	now := time.Now()
	evictor.now = func() time.Time { return now }
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}

	// The burst's first write, which evicts: the eviction the interval refuses the next one by.
	pair.leaveTaskABehind(t, 2, bob, "Bob types once.\n")
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to defer the next notification by")
	}
	// The burst's last write, inside the window: refused, and the window it arms is all that is
	// left to close the room.
	pair.leaveTaskABehind(t, 3, bob, "Bob types again inside the window.\n")
	if evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatalf("a second write within %s evicted the room again, under readers the eviction before it had not finished re-syncing", staleRoomEvictInterval)
	}
	if wake := evictor.wakeIn(); wake <= 0 {
		t.Fatalf("the refusal asks for no wake (wakeIn = %s), so the room is already dropped and this row tests nothing", wake)
	}

	// The window ends, and the re-examination it was waiting for gets no answer.
	now = now.Add(staleRoomEvictInterval)
	blind, release := store.HoldsConnection(context.Background())
	defer release()
	evictor.woke(blind)()
	if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("a re-examination that could not read the head closed the room anyway")
	}
	if !roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room stands at the head, so there is nothing left for the next window to find")
	}
	wake := evictor.wakeIn()
	if wake <= 0 {
		t.Fatalf("the re-examination that got no answer spent the room's window (wakeIn = %s): the room is behind the head with a reader in it and nothing will look at it again", wake)
	}
	if wake < staleRoomEvictInterval {
		t.Fatalf("the room was re-armed %s out, inside the %s interval: a read that goes on failing would re-run the whole decision as fast as the listener loops", wake, staleRoomEvictInterval)
	}

	// The next window, with the reads answering again: the room goes, on the wake and with
	// nothing notifying this process.
	now = now.Add(wake)
	evictor.woke(ctx)()
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A room whose re-examinations keep reaching no verdict is re-armed further out each time, as
// far as staleRoomRetryBackoffMax, and what that ceiling bounds is the rate: such a room is
// re-examined once a minute rather than every staleRoomEvictInterval for the life of the
// process, and each re-examination re-reads the head through the pool and, while the room is
// still behind, reads it again under the document's lock. Nothing ends the run on its own - no
// notification arrives to drain the room out of the deferred set, since the write that
// announced this one can be the last - so without the ceiling that cadence is for ever.
//
// The ceiling carries nothing for the listener's idle ping, which is the listener's own and
// depends on no caller: pglisten holds the ping to the time since the connection last proved a
// round trip, cuts a wake short where it falls due and pings there whatever ended the wait
// (pglisten.Listener.waitFor, listen).
//
// The reads here get no answer the way TestAStaleRoomWhoseReExaminationGetsNoAnswerIsTriedAgainAtTheNextWindow
// arranges it: the head read is made through a context that already holds one of the pool's
// connections, which the pool refuses (store.ErrNestedAcquire). The first window and the ceiling
// are what this asserts, not the steps between them: the first is the cadence the ordinary case
// converges on, and the ceiling is the rate the backoff climbs to and stays at.
func TestARoomThatKeepsGettingNoAnswerBacksOffPastTheListenersIdlePeriod(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	now := time.Now()
	evictor.now = func() time.Time { return now }
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}

	// The burst's first write, which evicts: the eviction the interval refuses the next one by.
	pair.leaveTaskABehind(t, 2, bob, "Bob types once.\n")
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to defer the next notification by")
	}
	// The burst's last write, inside the window: refused, and the window it arms is where the
	// run of attempts that get no answer begins.
	pair.leaveTaskABehind(t, 3, bob, "Bob types again inside the window.\n")
	if evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatalf("a second write within %s evicted the room again, under readers the eviction before it had not finished re-syncing", staleRoomEvictInterval)
	}
	wake := evictor.wakeIn()
	if wake <= 0 {
		t.Fatalf("the refusal asks for no wake (wakeIn = %s), so the room is already dropped and this row tests nothing", wake)
	}

	blind, release := store.HoldsConnection(context.Background())
	defer release()
	// Two seconds doubling to a minute reaches the ceiling on the sixth attempt; the bound is
	// what keeps a backoff that never arrives there from running this loop for ever.
	const attempts = 8
	var first time.Duration
	for attempt := 1; attempt <= attempts; attempt++ {
		now = now.Add(wake)
		evictor.woke(blind)()
		wake = evictor.wakeIn()
		if attempt == 1 {
			first = wake
		}
		if wake >= staleRoomRetryBackoffMax {
			break
		}
	}
	if first != staleRoomEvictInterval {
		t.Fatalf("the first re-arm after an attempt that got no answer waits %s, not the %s the ordinary case - one read that failed, answered at the next window - converges on", first, staleRoomEvictInterval)
	}
	if wake < pglisten.DefaultListenIdle {
		t.Fatalf("after %d re-examinations that reached no verdict the room is still re-armed %s out, under the listener's %s idle period and so under the ceiling of twice it: the backoff is not climbing to its bound, so a room nothing writes to again goes on re-reading its head through the pool - and, while it is still behind, under the document's lock - at that rate for the life of the process", attempts, wake, pglisten.DefaultListenIdle)
	}
	if wake != staleRoomRetryBackoffMax {
		t.Fatalf("the backoff settled at %s rather than its %s ceiling", wake, staleRoomRetryBackoffMax)
	}

	// Still the room this row is about: behind the head, resident, and waiting on that window.
	if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("a re-examination that could not read the head closed the room anyway")
	}
	if !roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room stands at the head, so there is nothing left for the backed-off window to find")
	}
}

// A notification sent while the listener has no connection converges on its reconnect. That
// notification reaches nobody and nothing sends it again, and a redial backs off as far as thirty
// seconds (pglisten.RedialBackoff), so a room the other process wrote under in that window would
// hold its reader's stale text until something wrote the document again. What catches it up is
// the sweep over the rooms this process has loaded that runs after each LISTEN stands
// (staleRoomEvictor.connected), which is why the wait below is on the reconnect and not on a
// notification.
func TestARoomLeftBehindWhileTheListenerWasDownIsEvictedOnTheReconnect(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	var down atomic.Bool
	down.Store(true)
	dial := pair.database.Pool.Listener
	evictor.dial = func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the stale-room listener has no connection")
		}
		return dial(ctx)
	}
	evictor.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	notes, connected := startStaleRoomEvictor(t, evictor)

	// Committed on the other process while the evictor has no connection: its notification
	// reaches nobody, and task A's room is left behind the head by it.
	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob"}, "Bob writes while the listener is redialing.\n")
	requireNoStaleRoomNote(t, notes, pair.artifactID, "while the listener had no connection")
	if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("task A's room left before the listener reconnected, so the reconnect proves nothing")
	}

	down.Store(false)
	select {
	case <-connected:
	case <-time.After(roomWait(t)):
		t.Fatal("the stale-room evictor never reconnected")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A room this process is in the middle of a durable write on is not evicted under it: each of
// these writers is about to advance that room's own cursor, and a room closed first loses the
// write's publish with it. The guard is the service's own in-flight state (hasWriteInFlight), and
// the room goes as soon as the write is done.
func TestARoomWithAWriteInFlightIsNotEvictedUnderIt(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob"}, "Bob writes on the other process.\n")
	evictor := newStaleRoomEvictor(pair.taskA)
	ctx := context.Background()

	for _, writer := range []struct {
		what    string
		hold    func(*roomState)
		release func(*roomState)
	}{
		{"a browser append the room's persistence has not landed",
			func(state *roomState) { state.durableAppends.Add(1) },
			func(state *roomState) { state.durableAppends.Add(-1) }},
		{"an open live write",
			func(state *roomState) { state.liveWriter = &liveWrite{} },
			func(state *roomState) { state.liveWriter = nil }},
		{"a settlement running on the room",
			func(state *roomState) { state.settling++ },
			func(state *roomState) { state.settling-- }},
	} {
		t.Run(writer.what, func(t *testing.T) {
			state := pair.taskA.lockState(pair.artifactID)
			writer.hold(state)
			pair.taskA.unlockState(pair.artifactID, state)
			if evictor.documentChanged(ctx, pair.artifactID) {
				t.Fatalf("a room with %s was evicted under it", writer.what)
			}
			if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
				t.Fatalf("the room with %s is gone", writer.what)
			}
			state = pair.taskA.lockState(pair.artifactID)
			writer.release(state)
			pair.taskA.unlockState(pair.artifactID, state)
		})
	}

	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatal("the room was not evicted once nothing was in flight on it")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A room re-examined while a write of this process is in flight on it keeps a window of its own:
// the guard that spares it is an attempt, not a verdict. The wake has already spent the window
// that brought the room here (dueRooms), and two of the three writers the guard reports do come
// back for the room - a browser append from a room behind the head is refused and fails the room,
// a settlement is refused and closes it - but a live write that appends nothing (no changes, a
// mutate that failed, an operation rolled back) commits no history and so announces none, and
// nothing else reads hasWriteInFlight. Dropped here, the room is left behind the head with a
// reader on it and nothing left to look at it again: roomIdleTimeout counts from the last peer
// leaving, which a room with a tab in it never reaches.
func TestARoomReExaminedWithAWriteInFlightKeepsItsWindow(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	now := time.Now()
	evictor.now = func() time.Time { return now }
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}

	// The burst's first write, which evicts: the eviction the interval refuses the next one by.
	pair.leaveTaskABehind(t, 2, bob, "Bob types once.\n")
	if !evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to defer the next notification by")
	}
	// The burst's last write, inside the window: refused, and the window it arms is all that is
	// left to close the room.
	pair.leaveTaskABehind(t, 3, bob, "Bob types again inside the window.\n")
	if evictor.documentChanged(ctx, pair.artifactID) {
		t.Fatalf("a second write within %s evicted the room again, under readers the eviction before it had not finished re-syncing", staleRoomEvictInterval)
	}

	// The window ends on a live write of this process that is still open on the room.
	now = now.Add(staleRoomEvictInterval)
	state := pair.taskA.lockState(pair.artifactID)
	state.liveWriter = &liveWrite{}
	pair.taskA.unlockState(pair.artifactID, state)
	evictor.woke(ctx)()
	if pair.taskA.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("the room was closed under the write in flight on it, which would take that write's publish with it")
	}
	wake := evictor.wakeIn()
	if wake <= 0 {
		t.Fatalf("the re-examination that found a write in flight spent the room's window (wakeIn = %s): the room is behind the head with a reader in it and nothing - not even a write that appends nothing - will look at it again", wake)
	}
	if wake < staleRoomEvictInterval {
		t.Fatalf("the room was re-armed %s out, inside the %s interval: the re-examination would run as fast as the listener loops for as long as the write is open", wake, staleRoomEvictInterval)
	}

	// The write finishes with nothing appended, so nothing notifies this process again: the
	// window the guard armed is what closes the room.
	state = pair.taskA.lockState(pair.artifactID)
	state.liveWriter = nil
	pair.taskA.unlockState(pair.artifactID, state)
	now = now.Add(wake)
	evictor.woke(ctx)()
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A cold write publishes into whatever room loaded while it ran, and holds no room cursor of its
// own to advance (coldFork): its commit takes its versions into that room's cursor
// (takeRoomCursors), so the room stands at the head for the whole commit-to-publish window. Left
// behind there, it reads as a room another process wrote under - which is this process's own
// write, about to publish into it - and its own browser's append, queued behind the write's
// lock and taking it the instant the commit drops it, is refused and fails the room under the
// browser that made the edit.
func TestAColdWritesOwnRoomIsNotLeftBehindBetweenItsCommitAndPublish(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	closeDocumentRoom(t, service, artifactID)

	// A browser opens the document behind the write's store read, as LEGION-504's window has it:
	// the write stays cold, and the room it publishes into is this one.
	opened := false
	service.afterForkRead = func(room string) {
		if room != artifactID || opened || service.srv.GetDoc(artifactID) != nil {
			return
		}
		opened = true
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Errorf("a browser opens the document while the cold write reads the store: %v", err)
		}
	}

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Agent's paragraph.", After: "Second."},
	}, model.Actor{Kind: "session", ID: "agent-session"}, nil); err != nil {
		t.Fatalf("apply the cold write: %v", err)
	}
	if write := joinedLiveWrite(joined, artifactID); write == nil || !write.cold {
		t.Fatal("the write forked a room, so nothing here is tested")
	}
	if !opened || service.srv.GetDoc(artifactID) == nil {
		t.Fatal("no room loaded while the cold write ran, so nothing here is tested")
	}
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit the cold write: %v", err)
	}

	if roomBehind(t, service, artifactID) {
		t.Fatal("the room the cold write is about to publish into stands behind the head between the write's commit and its publish")
	}
	if newStaleRoomEvictor(service).documentChanged(ctx, artifactID) {
		t.Fatal("this process's own cold write evicted the room it was about to publish into")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the room the cold write is about to publish into is gone")
	}
	// The browser's own append in that window, which is what the room was kept at the head for.
	requireRoomStillWrites(t, service, artifactID, 1)

	ledger.publish()
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"Agent's paragraph.", "Writing again as peer 1."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document after the cold write's publish = %q, want it to hold %q", stored, want)
		}
	}
}

// A notification for a document this process holds no room for does nothing: it loads no room,
// and it is most of the traffic on the channel, since every append of both processes notifies.
func TestANotificationForADocumentWithNoRoomHereDoesNothing(t *testing.T) {
	pair := newProcessPair(t)
	elsewhere := createDocument(t, pair.database, "# Elsewhere")
	seedServiceText(t, pair.taskB, elsewhere, "Only the other process holds this.\n")
	waitForLandedAppends(t, pair.taskB, elsewhere)
	evictor := newStaleRoomEvictor(pair.taskA)

	if evictor.documentChanged(context.Background(), elsewhere) {
		t.Fatal("a document this process holds no room for was evicted")
	}
	if pair.taskA.srv.GetDoc(elsewhere) != nil {
		t.Fatal("the notification loaded a room for a document nobody here had open")
	}
}

// A durable change is announced by the transaction that made it, so it is announced only if that
// transaction commits: an append rolled back - the gate's refusal, a commit that failed - leaves
// the stored document as it was, and must evict nobody's room.
func TestADurableAppendIsAnnouncedOnlyWhenItsTransactionCommits(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	heard := listenForDocumentChanges(t, database)
	persist := NewPgVersioned(database)
	ctx := context.Background()

	for _, commit := range []bool{false, true} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the append: %v", err)
		}
		if _, err := persist.AppendUpdateTx(ctx, tx, artifactID, paragraphUpdate(), true); err != nil {
			t.Fatalf("append an update: %v", err)
		}
		select {
		case room := <-heard:
			t.Fatalf("the append announced %s before its transaction ended", room)
		case <-time.After(200 * time.Millisecond):
		}
		if !commit {
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll the append back: %v", err)
			}
			select {
			case room := <-heard:
				t.Fatalf("a rolled-back append announced %s", room)
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the append: %v", err)
		}
		select {
		case room := <-heard:
			if room != artifactID {
				t.Fatalf("the append announced %s, want %s", room, artifactID)
			}
		case <-time.After(roomWait(t)):
			t.Fatal("a committed append announced nothing")
		}
	}
}

// A rebuild replaces a document's whole stored history in its own transaction (RebuildTx), and
// announces it there too: every room resident elsewhere holds a history that no longer exists.
func TestARebuildAnnouncesTheDocumentsChange(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	// invalid makes the document's first load unreadable, which is what a rebuild needs to see
	// before it replaces the history (rebuild_test.go).
	service := New(Deps{Store: database, Persistence: &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	heard := listenForDocumentChanges(t, database)

	replacement := "Rebuilt.\n"
	if _, _, err := rebuildDocument(t, service, artifactID, &replacement); err != nil {
		t.Fatalf("rebuild the document: %v", err)
	}
	select {
	case room := <-heard:
		if room != artifactID {
			t.Fatalf("the rebuild announced %s, want %s", room, artifactID)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the rebuild announced nothing")
	}
}

// paragraphUpdate is one valid V1 update: an empty paragraph, which any document takes.
func paragraphUpdate() []byte {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(transaction *crdt.Transaction) {
		fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("paragraph"))
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// listenForDocumentChanges is a connection of its own listening on documentChangedChannel,
// handing each notification's artifact id to the test.
func listenForDocumentChanges(t *testing.T, database *store.Store) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	heard := make(chan string, 16)
	listening := make(chan struct{})
	done := make(chan struct{})
	listener := &pglisten.Listener{
		Dial:        database.Pool.Listener,
		Channels:    []string{documentChangedChannel},
		OnConnected: sync.OnceFunc(func() { close(listening) }),
		OnNotify: func(_, artifactID string) {
			select {
			case heard <- artifactID:
			case <-ctx.Done():
			}
		},
	}
	go func() {
		defer close(done)
		listener.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-listening:
	case <-time.After(roomWait(t)):
		t.Fatal("the test's own listener never listened")
	}
	return heard
}

// A browser's append commits inside the session lock it took the document under, and advances its
// room's cursor before it drops that lock (appendUpdate). Between the two the store stands one
// past the room, which is what a notification read through the pool sees - this process's own
// write, read as another process's. The evictor reads the room again under that lock before it
// closes anything (roomBehindDurableHead), where the append has either dropped the lock, cursor
// advanced, or still holds it, so the second read waits for it. Deciding on the pool read alone
// closes a healthy room mid-write, and the browser that was typing into it is disconnected.
func TestARoomIsNotEvictedBetweenItsOwnAppendsCommitAndItsCursor(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	cursor := service.cursorOf(service.srv.GetDoc(artifactID))
	if cursor == nil {
		t.Fatal("the resident room lists no cursor")
	}

	// The window, held open as an append holds it: the document's session lock taken, the update
	// committed under it, and the room's cursor not advanced yet.
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the appending connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("take the document's session lock: %v", err)
	}
	appended, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the append: %v", err)
	}
	version, err := NewPgVersioned(database).AppendUpdateTx(ctx, appended, artifactID, paragraphUpdate(), true)
	if err != nil {
		t.Fatalf("append an update under the lock: %v", err)
	}
	if err := appended.Commit(ctx); err != nil {
		t.Fatalf("commit the append under the lock: %v", err)
	}
	if !roomBehind(t, service, artifactID) {
		t.Fatal("the room stands at the head with its append's cursor still to come, so nothing here is tested")
	}

	decided := make(chan bool, 1)
	go func() { decided <- newStaleRoomEvictor(service).documentChanged(ctx, artifactID) }()
	waitFor(t, roomWait(t), "the evictor to wait on the document's lock", func() bool {
		return waitingAdvisoryLocks(t, database) > 0
	})
	// What the append does next, where it does it: the cursor, and then the lock.
	advanceRoomCursor(cursor, version)
	if _, err := conn.Exec(ctx, `select pg_advisory_unlock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("release the document's session lock: %v", err)
	}

	select {
	case evicted := <-decided:
		if evicted {
			t.Fatal("the room was closed between its own append's commit and that append's cursor")
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the evictor never decided the notification of this process's own append")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the room this process was appending to is gone")
	}
	requireRoomStillWrites(t, service, artifactID, 1)
}

// waitingAdvisoryLocks is how many advisory locks of this test's own database are waited for: the
// evictor's read of a room under the document's lock, while the append that holds it has not
// finished.
func waitingAdvisoryLocks(t *testing.T, database *store.Store) int {
	t.Helper()
	var waiting int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_locks
		where locktype = 'advisory' and not granted
		  and database = (select oid from pg_database where datname = current_database())
	`).Scan(&waiting); err != nil {
		t.Fatalf("read the advisory locks waited for: %v", err)
	}
	return waiting
}
