package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// A room this process holds for a document another process writes follows that write: the write
// announces itself in its own transaction (documentHeadChannel and documentChangedChannel), and
// this process's room sync catches the room up from doc_updates (catchUpRoom) instead of closing
// it. Only a room whose stored history was replaced under it - a rebuild, a prune, a compaction
// that folded the rows it stands on - is closed and reloaded. The tests here run the catch-up over
// two services on one database, as production's two tasks are (processPair,
// room_behind_head_test.go).

// catchUpNotes is what a service's catch-up reports while a test watches it: decided, one artifact
// id per notification decided on the lane, and reads, one per catch-up read.
type catchUpNotes struct {
	decided chan string
	reads   chan string
}

// watchCatchUps hooks service's catch-up so the test hears each decision and each read. It must
// run before the room sync starts, which then reads the hooks from its own goroutine.
func watchCatchUps(t *testing.T, service *Service) catchUpNotes {
	t.Helper()
	notes := catchUpNotes{decided: make(chan string, 256), reads: make(chan string, 256)}
	note := func(into chan string, what string) func(string) {
		return func(room string) {
			select {
			case into <- room:
			default:
				t.Errorf("the test read no %s note for %s", what, room)
			}
		}
	}
	service.catchUps.decided = note(notes.decided, "decision")
	service.catchUps.afterRead = note(notes.reads, "read")
	return notes
}

// runCatchUps watches service's catch-up and runs its room sync on a listener hub of its own
// (runRoomSync), returning once the first LISTEN stands and the sweep after it has run.
func runCatchUps(t *testing.T, service *Service) catchUpNotes {
	t.Helper()
	notes := watchCatchUps(t, service)
	runRoomSync(t, newRoomSync(service))
	drain(notes.reads)
	return notes
}

// drain empties notes of what is already in it.
func drain(notes chan string) {
	for {
		select {
		case <-notes:
		default:
			return
		}
	}
}

// waitNote waits for the next note for artifactID, ignoring any other document's.
func waitNote(t *testing.T, notes chan string, artifactID, what string) {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case room := <-notes:
			if room == artifactID {
				return
			}
		case <-deadline:
			t.Fatalf("%s: nothing heard for %s within %v", what, artifactID, roomWait(t))
		}
	}
}

// countNotes is how many notes for artifactID arrive within quiet of each other.
func countNotes(notes chan string, artifactID string, quiet time.Duration) int {
	count := 0
	for {
		select {
		case room := <-notes:
			if room == artifactID {
				count++
			}
		case <-time.After(quiet):
			return count
		}
	}
}

// replaceStoredHistory replaces artifactID's stored history with markdown as a rebuild does
// (RebuildTx: every row deleted, one seeded at head+1), in a transaction of its own on a store that
// is no process's, so its announcement names another sender than every service in the test.
func replaceStoredHistory(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper()
	tree, err := parseInput(markdown)
	if err != nil {
		t.Fatalf("parse the rebuilt document: %v", err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatalf("encode the rebuilt document: %v", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := NewPgVersioned(database).RebuildTx(ctx, tx, artifactID, seed); err != nil {
		t.Fatalf("rebuild the stored history: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the rebuild: %v", err)
	}
}

// A write on process B reaches the browser on process A's room within the notification's round
// trip, and the room is not closed: the browser's socket stays open, the room is the instance it
// was, its cursor stands at the head, and it writes again. This is the behaviour the stale-room
// eviction used to answer with a close and a reconnect.
func TestAWriteOnTheOtherProcessReachesThisProcessesBrowserWithoutAClose(t *testing.T) {
	pair := newProcessPair(t)
	runCatchUps(t, pair.taskA)
	browser := connectPeer(t, pair.serverA.URL, pair.artifactID, "alice")
	if err := browser.WaitForText(peerMarkdown, storedMarkdown(t, pair.database, pair.artifactID), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	room := pair.taskA.srv.GetDoc(pair.artifactID)

	started := time.Now()
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 2,
		model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, "Bob writes on the other process.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	stored := storedMarkdown(t, pair.database, pair.artifactID)
	if !strings.Contains(stored, "Bob writes on the other process.") {
		t.Fatalf("the stored document after the other process's write = %q, want its paragraph", stored)
	}
	if err := browser.WaitForText(peerMarkdown, stored, roomWait(t)); err != nil {
		t.Fatalf("the browser on task A never saw task B's write: %v", err)
	}
	t.Logf("task B's write reached the browser on task A %v after it began", time.Since(started).Round(time.Millisecond))
	select {
	case <-browser.Ended:
		t.Fatal("the browser's socket closed: the room was closed rather than caught up")
	default:
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("task A's room was replaced rather than caught up")
	}
	waitFor(t, roomWait(t), "task A's room to stand at the head", func() bool {
		return !roomBehind(t, pair.taskA, pair.artifactID)
	})
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 3)
}

// A head announcement this process sent itself costs no catch-up read: every append of this
// process announces itself, a browser typing here several times a second, and its room already
// holds what it announced. Its bare twin on documentChangedChannel is dropped with it. Another
// process's announcement for the same room does read, which is the control.
func TestAHeadAnnouncementFromThisProcessReadsNothing(t *testing.T) {
	pair := newProcessPair(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	editAsConnectedPeer(t, pair.taskA, pair.artifactID, 1, alice, appendBlocks(t, "Alice types once.\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	notes := runCatchUps(t, pair.taskA)
	drain(notes.decided)

	for keystroke := range 3 {
		editAsConnectedPeer(t, pair.taskA, pair.artifactID, 1, alice, appendBlocks(t, "Alice types again.\n"))
		waitForLandedAppends(t, pair.taskA, pair.artifactID)
		// The head announcement and its bare twin, decided in the order they arrived.
		waitNote(t, notes.decided, pair.artifactID, fmt.Sprintf("keystroke %d's head announcement", keystroke))
		waitNote(t, notes.decided, pair.artifactID, fmt.Sprintf("keystroke %d's bare announcement", keystroke))
	}
	if reads := countNotes(notes.reads, pair.artifactID, 100*time.Millisecond); reads != 0 {
		t.Fatalf("task A's own appends cost %d catch-up reads, want none", reads)
	}

	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 2, model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, "Bob writes.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	waitNote(t, notes.reads, pair.artifactID, "the other process's append")
}

// The cursor holds the versions an instance appended past a gap - its own browsers' appends while
// another process's row was not caught up yet - and folds them into through once the gap fills,
// as roomHold.since folds into roomHold.loaded.
func TestOwnAppendsMadeWhileBehindFoldIntoTheCursor(t *testing.T) {
	cursor := &roomCursor{through: 4}
	advanceRoomCursor(cursor, 6)
	advanceRoomCursor(cursor, 7)
	if through, above := cursor.held(); through != 4 || !slices.Equal(above, []int64{6, 7}) {
		t.Fatalf("the cursor holds %d and %v after appends past the gap, want 4 and [6 7]", through, above)
	}
	advanceRoomCursor(cursor, 3)
	advanceRoomCursor(cursor, 7)
	if through, above := cursor.held(); through != 4 || !slices.Equal(above, []int64{6, 7}) {
		t.Fatalf("the cursor holds %d and %v after versions it already held, want 4 and [6 7]", through, above)
	}
	advanceRoomCursor(cursor, 5)
	if through, above := cursor.held(); through != 7 || len(above) != 0 {
		t.Fatalf("the cursor holds %d and %v once the gap filled, want 7 and nothing above", through, above)
	}
}

// A browser's append from a room behind the head is stored, at the head, and the room never fails:
// a Yjs update commutes with the other process's, so the gate admits it (appendBrowser). The
// room's cursor holds it past the gap the other process's row leaves, and the catch-up that takes
// that row in folds both, so the room then stands at the head and its derived writes pass the gate
// again.
func TestABrowserAppendFromARoomBehindTheHeadIsStoredAndTheRoomNeverFails(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	cursor := pair.taskA.cursorOf(room)
	behindAt := cursor.heldThrough()

	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 2)
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 3)
	if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
		t.Fatalf("task A's room failed on its browser's append from behind the head: %v", err)
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("task A's room was replaced")
	}
	head := storedHead(t, pair.database, pair.artifactID)
	through, above := cursor.held()
	if through != behindAt || !slices.Equal(above, []int64{head - 1, head}) {
		t.Fatalf("task A's cursor holds %d and %v, want %d (behind task B's row) and its own two appends [%d %d]",
			through, above, behindAt, head-1, head)
	}

	outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
	if err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	if through, above := cursor.held(); through != head || len(above) != 0 {
		t.Fatalf("task A's cursor holds %d and %v after its catch-up, want the head %d and nothing above", through, above, head)
	}
	if markdown := liveMarkdownOf(t, pair.taskA, pair.artifactID); !strings.Contains(markdown, "Bob's paragraph.") {
		t.Fatalf("task A's room renders %q after its catch-up, want Bob's paragraph", markdown)
	}
	commitNamedVersion(t, pair.taskA, pair.artifactID, model.Actor{Kind: "session", ID: "versioner-a"})
	if markdown, at := latestVersionOf(t, pair.database, pair.artifactID); at != head || markdown != storedMarkdown(t, pair.database, pair.artifactID) {
		t.Fatalf("task A's named version = %q at %d, want the stored document at the head %d", markdown, at, head)
	}
}

// liveMarkdownOf renders service's resident room of artifactID.
func liveMarkdownOf(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	markdown, err := renderTree(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("render the room: %v", err)
	}
	return markdown
}

// A catch-up applies only the rows its instance does not hold: a version the cursor already holds
// past the gap - an append of the instance's own, or a published write - is skipped, since its
// update is in the room. Here the cursor claims a row whose update the room does not hold, so a
// catch-up that applied it anyway shows as that row's paragraph in the room.
func TestACatchUpSkipsTheRowsItsInstanceHolds(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's first paragraph.\n")
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, "Bob's second paragraph.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	cursor := pair.taskA.cursorOf(room)
	head := storedHead(t, pair.database, pair.artifactID)
	if head-cursor.heldThrough() < 2 {
		t.Fatalf("task A's room is %d rows behind the head, want at least two, so nothing here is tested", head-cursor.heldThrough())
	}
	advanceRoomCursor(cursor, persistence.Version(head))

	outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
	if err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	markdown := liveMarkdownOf(t, pair.taskA, pair.artifactID)
	if !strings.Contains(markdown, "Bob's first paragraph.") {
		t.Fatalf("task A's room renders %q, want the row it lacked", markdown)
	}
	if strings.Contains(markdown, "Bob's second paragraph.") {
		t.Fatalf("task A's room renders %q: the catch-up applied a row its cursor holds", markdown)
	}
	if through := cursor.heldThrough(); through != head {
		t.Fatalf("task A's cursor holds %d after its catch-up, want the head %d", through, head)
	}
}

// Another process's update is credited on that process, whose append recorded its authors, and
// introduced its ask blocks there: a catch-up that applies it credits none of this room's
// connected peers with it (creditContentChange) and names none of them the author of its ask
// block (observeAskBlocksForUpdate). Credited, alice - connected to task A, typing nothing - was
// listed on the version holding bob's words, and named the author of bob's ask.
func TestARemoteUpdateCreditsNoLocalPeerAndAttributesNoAskBlock(t *testing.T) {
	pair := newProcessPair(t)
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob,
		":::ask{#bob-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n")
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	pair.taskA.addConnection(pair.artifactID, 7, alice)
	t.Cleanup(func() { pair.taskA.removeConnection(pair.artifactID, 7) })
	state := pair.taskA.room(pair.artifactID)
	state.mu.Lock()
	before := state.creditSeq.Load()
	state.mu.Unlock()

	outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
	if err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	state.mu.Lock()
	credited := state.creditSeq.Load() - before
	inflight := len(state.inflight)
	lastActor := state.lastActor
	_, observed := state.askBlocks["bob-ask"]
	author, attributed := state.askAuthors["bob-ask"]
	state.mu.Unlock()
	if credited != 0 || inflight != 0 {
		t.Fatalf("the catch-up credited %d edit(s) to task A's peers (%d in flight), want none", credited, inflight)
	}
	if lastActor != nil && actorKey(*lastActor) == actorKey(alice) {
		t.Fatalf("task A's latest edit source is %+v, the connected peer who wrote nothing", *lastActor)
	}
	if !observed {
		t.Fatal("task A's room did not observe the ask block the catch-up brought in, so nothing here is tested")
	}
	if attributed {
		t.Fatalf("task A attributes bob's ask block to %+v, want no local author", author)
	}
	if owed := pendingAuthorRows(t, pair.database, pair.artifactID); slices.Contains(owed, alice) {
		t.Fatalf("pending authors = %+v, want no %s", owed, alice.ID)
	}
}

// A rebuild on the other process replaces the stored history under task A's resident room: every
// row deleted and one fresh one seeded at head+1, which no row the room could take in leads to.
// The rebuild announces itself, task A's catch-up finds the history replaced, and the room is
// closed - its browser's socket ends - and reloads the rebuilt document.
func TestARebuildOnTheOtherProcessClosesAResidentRoom(t *testing.T) {
	pair := newProcessPair(t)
	runCatchUps(t, pair.taskA)
	browser := connectPeer(t, pair.serverA.URL, pair.artifactID, "alice")
	if err := browser.WaitForText(peerMarkdown, storedMarkdown(t, pair.database, pair.artifactID), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	replaceStoredHistory(t, pair.database, pair.artifactID, "Rebuilt elsewhere.\n")
	select {
	case <-browser.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the browser's socket was still open %v after the rebuild; its copy holds %q", roomWait(t), peerMarkdown(browser))
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
	again := connectPeer(t, pair.serverA.URL, pair.artifactID, "alice")
	if err := again.WaitForText(peerMarkdown, storedMarkdown(t, pair.database, pair.artifactID), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if markdown := peerMarkdown(again); !strings.Contains(markdown, "Rebuilt elsewhere.") {
		t.Fatalf("the reloaded room serves %q, want the rebuilt document", markdown)
	}
}

// A compaction keeps the head row, into which it folds the rows before it, and announces nothing:
// a room standing at the head is unaffected, its next catch-up finds a history it holds, and a
// write on the other process after the compaction is caught up as any other.
func TestACompactionLeavesACurrentRoomOpen(t *testing.T) {
	pair := newProcessPair(t)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	for keystroke := range 3 {
		requireRoomStillWrites(t, pair.taskA, pair.artifactID, uint64(keystroke+1))
	}
	if deleted, err := pair.taskB.persistence.Compact(context.Background(), pair.artifactID, compactKeep); err != nil || deleted == 0 {
		t.Fatalf("compact the document on task B = %d rows, %v, want some folded", deleted, err)
	}

	outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
	if err != nil || outcome != catchUpCurrent {
		t.Fatalf("catch task A's room up after the compaction = %v, %v, want catchUpCurrent", outcome, err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 9, model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, "Bob writes after the compaction.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	outcome, err = pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
	if err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up after the other process's write = %v, %v, want catchUpApplied", outcome, err)
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("task A's room was closed")
	}
	if markdown := liveMarkdownOf(t, pair.taskA, pair.artifactID); !strings.Contains(markdown, "Bob writes after the compaction.") {
		t.Fatalf("task A's room renders %q, want the write made after the compaction", markdown)
	}
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 4)
}

// A catch-up that reads in the moment between this process's own write recording its version on
// the cursor and that write committing - a live write records them before its commit
// (Ledger.takeRoomCursors), and so does settlement's own repair - sees a head below the cursor,
// which reads as a replaced history. The catch-up confirms that verdict under the document's lock
// before it closes anything (historyReplacedUnderLock), and under it the write has committed, so
// the room stays open.
func TestACatchUpInsideThisProcessesOwnCommitWindowClosesNothing(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	cursor := service.cursorOf(room)

	// The window, held open as a write holds it: the document's lock taken in the write's
	// transaction, the update appended there, its version recorded on the cursor, no commit yet.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer tx.Rollback(ctx)
	version, err := NewPgVersioned(database).AppendUpdateTx(ctx, tx, artifactID, paragraphUpdate(), true)
	if err != nil {
		t.Fatalf("append the write's update: %v", err)
	}
	advanceRoomCursor(cursor, version)

	caught := make(chan struct{})
	go func() {
		defer close(caught)
		service.catchUps.catchUp(ctx, artifactID)
	}()
	waitFor(t, roomWait(t), "the catch-up to confirm its verdict under the document's lock", func() bool {
		return waitingAdvisoryLocks(t, database) > 0
	})
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	select {
	case <-caught:
	case <-time.After(roomWait(t)):
		t.Fatal("the catch-up never finished")
	}
	if service.srv.GetDoc(artifactID) != room {
		t.Fatal("the catch-up closed the room this process was writing")
	}
}

// waitingAdvisoryLocks is how many advisory locks of this test's own database are waited for.
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

// A catch-up whose read fails is tried again, the first time one catchUpRetryInterval later and
// each time after further out, to catchUpRetryBackoffMax: nothing else comes back for the room, and
// a read that goes on failing must not be re-run as fast as the lane loops. The reads fail here
// the way a pool refuses a second connection under a transaction (store.ErrNestedAcquire). Once
// the reads answer again the wake catches the room up.
func TestAFailedCatchUpReadIsTriedAgainWithBackoff(t *testing.T) {
	pair := newProcessPair(t)
	catchUps := pair.taskA.catchUps
	now := time.Now()
	catchUps.now = func() time.Time { return now }
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob"}, "Bob's paragraph.\n")

	blind, release := store.HoldsConnection(context.Background())
	defer release()
	catchUps.catchUp(blind, pair.artifactID)
	if wake := catchUps.wakeIn(); wake != catchUpRetryInterval {
		t.Fatalf("a failed read is tried again in %s, want %s", wake, catchUpRetryInterval)
	}
	var wake time.Duration
	for range 8 {
		now = now.Add(catchUps.wakeIn())
		catchUps.woke(blind)
		if wake = catchUps.wakeIn(); wake >= catchUpRetryBackoffMax {
			break
		}
	}
	if wake != catchUpRetryBackoffMax {
		t.Fatalf("the backoff settled at %s, want its %s ceiling", wake, catchUpRetryBackoffMax)
	}
	if !roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room stands at the head, so the retries had nothing to do")
	}

	now = now.Add(wake)
	catchUps.woke(context.Background())
	if roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("the retry with the reads answering again did not catch task A's room up")
	}
	if wake := catchUps.wakeIn(); wake != 0 {
		t.Fatalf("a retry that caught the room up left one armed (wakeIn = %s)", wake)
	}
}

// A notification for a document this process holds no room for reads nothing and loads nothing.
func TestACatchUpForADocumentWithNoRoomHereDoesNothing(t *testing.T) {
	pair := newProcessPair(t)
	elsewhere := createDocument(t, pair.database, "# Elsewhere")
	seedServiceText(t, pair.taskB, elsewhere, "Only the other process holds this.\n")
	waitForLandedAppends(t, pair.taskB, elsewhere)
	notes := watchCatchUps(t, pair.taskA)

	pair.taskA.catchUps.catchUp(context.Background(), elsewhere)
	if pair.taskA.srv.GetDoc(elsewhere) != nil {
		t.Fatal("the catch-up loaded a room for a document nobody here had open")
	}
	if reads := countNotes(notes.reads, elsewhere, 0); reads != 0 {
		t.Fatalf("the catch-up read %d time(s) for a document with no room here", reads)
	}
}

// A notification sent while the listener has no connection reaches nobody and is never sent
// again, and a redial backs off as far as thirty seconds (pglisten.RedialBackoff): the sweep after
// each LISTEN stands catches up every room this process has loaded (roomSync.connected,
// roomCatchUps.sweep), which is why the wait below is on the reconnect and not on a notification.
func TestARoomLeftBehindWhileTheListenerWasDownIsCaughtUpOnTheReconnect(t *testing.T) {
	pair := newProcessPair(t)
	hub := newListenerHub(pair.taskA)
	up := make(chan struct{})
	var dials atomic.Int32
	dial := hub.Dial
	hub.Dial = func(ctx context.Context) (*pgx.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("the listener hub has no connection")
		}
		select {
		case <-up:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx)
	}
	connected, _ := startRoomSync(t, hub, newRoomSync(pair.taskA))
	room := func() *crdt.Doc {
		if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
			t.Fatalf("load task A's room: %v", err)
		}
		return pair.taskA.srv.GetDoc(pair.artifactID)
	}()
	waitForCatchUpsIdle(t, pair.taskA, pair.artifactID)

	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob"}, "Bob writes while the listener is redialing.\n")
	close(up)
	select {
	case <-connected:
	case <-time.After(roomWait(t)):
		t.Fatal("the room sync never reconnected")
	}
	if roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room is still behind the head after the reconnect's sweep")
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("task A's room was replaced rather than caught up")
	}
}

// waitForCatchUpsIdle waits until no catch-up of artifactID is queued, waiting for its room to
// load, or waiting to be tried again on service's lane: a room that loads with room sync running
// schedules one (keepInstance), which would otherwise land in the middle of what the test sets up.
func waitForCatchUpsIdle(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	catchUps := service.catchUps
	waitFor(t, roomWait(t), "the room's scheduled catch-up to run", func() bool {
		catchUps.mu.Lock()
		defer catchUps.mu.Unlock()
		_, loading := catchUps.loading[artifactID]
		_, deferred := catchUps.deferred[artifactID]
		return !catchUps.pending[artifactID] && !loading && !deferred
	})
}

// An old build's write during a roll announces only the bare artifact id. A burst of those for one
// room that arrives before its catch-up runs costs that one catch-up: a request for a room whose
// catch-up is queued adds nothing. The lane is held while the burst arrives, so every
// announcement is queued before the first is decided.
func TestABurstOfBareAnnouncementsCostsOneCatchUp(t *testing.T) {
	pair := newProcessPair(t)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	notes := runCatchUps(t, pair.taskA)
	catchUps := pair.taskA.catchUps
	catchUps.mu.Lock()
	lane := catchUps.lane
	catchUps.mu.Unlock()
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(release)
	lane.push(func(context.Context) { <-held })

	const burst = 5
	update := paragraphUpdate()
	for range burst {
		if _, err := pair.database.Pool.Exec(context.Background(), `
			with appended as (
				insert into doc_updates (artifact_id, version, update, content_changed)
				select $1, coalesce(max(version), 0) + 1, $2, true from doc_updates where artifact_id = $1
			)
			select pg_notify('`+documentChangedChannel+`', $3)
		`, pair.artifactID, update, pair.artifactID); err != nil {
			t.Fatalf("append as an old build: %v", err)
		}
	}
	waitFor(t, roomWait(t), "the burst's announcements to queue on the lane", func() bool {
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return len(lane.queue) >= burst
	})
	release()
	for range burst {
		waitNote(t, notes.decided, pair.artifactID, "the burst's announcements")
	}
	waitNote(t, notes.reads, pair.artifactID, "the burst's catch-up")
	if reads := countNotes(notes.reads, pair.artifactID, 300*time.Millisecond); reads != 0 {
		t.Fatalf("a burst of %d bare announcements cost %d catch-up reads, want one", burst, reads+1)
	}
	if roomBehind(t, pair.taskA, pair.artifactID) {
		t.Fatal("task A's room is behind the head after the burst's catch-up")
	}
}

// documentHeadNotification is one documentHeadChannel or documentChangedChannel notification a
// test's own listener heard.
type documentHeadNotification struct {
	channel string
	payload string
}

// listenForDocumentChanges is a connection of its own listening on documentHeadChannel and
// documentChangedChannel, handing each notification to the test in the order it arrived.
func listenForDocumentChanges(t *testing.T, database *store.Store) <-chan documentHeadNotification {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	heard := make(chan documentHeadNotification, 16)
	listening := make(chan struct{})
	done := make(chan struct{})
	listener := &pglisten.Listener{
		Dial:        database.Pool.Listener,
		Channels:    []string{documentHeadChannel, documentChangedChannel},
		OnConnected: sync.OnceFunc(func() { close(listening) }),
		OnNotify: func(channel, payload string) {
			select {
			case heard <- documentHeadNotification{channel: channel, payload: payload}:
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

// requireAnnouncement waits for the head announcement of artifactID at head from sender, and then
// its bare twin, in that order.
func requireAnnouncement(t *testing.T, heard <-chan documentHeadNotification, artifactID string, head int64, sender, what string) {
	t.Helper()
	for _, want := range []string{documentHeadChannel, documentChangedChannel} {
		select {
		case got := <-heard:
			if got.channel != want {
				t.Fatalf("%s: heard %s first, want %s then %s", what, got.channel, documentHeadChannel, documentChangedChannel)
			}
			if want == documentChangedChannel {
				if got.payload != artifactID {
					t.Fatalf("%s: the bare announcement names %s, want %s", what, got.payload, artifactID)
				}
				continue
			}
			var announced documentHead
			if err := json.Unmarshal([]byte(got.payload), &announced); err != nil {
				t.Fatalf("%s: decode the head announcement %q: %v", what, got.payload, err)
			}
			if announced != (documentHead{R: artifactID, V: head, P: sender}) {
				t.Fatalf("%s: the head announcement is %+v, want %+v", what, announced, documentHead{R: artifactID, V: head, P: sender})
			}
		case <-time.After(roomWait(t)):
			t.Fatalf("%s: no %s announcement within %v", what, want, roomWait(t))
		}
	}
}

// requireNoAnnouncement fails when anything is heard within a fifth of a second.
func requireNoAnnouncement(t *testing.T, heard <-chan documentHeadNotification, what string) {
	t.Helper()
	select {
	case got := <-heard:
		t.Fatalf("%s: heard %s %q", what, got.channel, got.payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// A durable change is announced by the transaction that made it, so it is announced only if that
// transaction commits: an append rolled back - the gate's refusal, a commit that failed - leaves
// the stored document as it was, and must wake nobody's catch-up. The announcement carries the
// head the append left and the process that made it.
func TestADurableAppendIsAnnouncedOnlyWhenItsTransactionCommits(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	heard := listenForDocumentChanges(t, database)
	persist := NewPgVersioned(database)
	persist.processID = "process-under-test"
	ctx := context.Background()

	for _, commit := range []bool{false, true} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the append: %v", err)
		}
		version, err := persist.AppendUpdateTx(ctx, tx, artifactID, paragraphUpdate(), true)
		if err != nil {
			t.Fatalf("append an update: %v", err)
		}
		requireNoAnnouncement(t, heard, "before the append's transaction ended")
		if !commit {
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll the append back: %v", err)
			}
			requireNoAnnouncement(t, heard, "a rolled-back append")
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the append: %v", err)
		}
		requireAnnouncement(t, heard, artifactID, int64(version), "process-under-test", "a committed append")
	}
}

// A transaction a caller joined can append to one document several times - an accept writes its
// text change and its margin projection - and announces it once, at its commit, with the last
// version it appended (Ledger.announceAppends), not once per append.
func TestAJoinedTransactionAnnouncesEachDocumentOnceWithItsLastVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	heard := listenForDocumentChanges(t, service.store)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	for _, paragraph := range []string{"Agent's first.", "Agent's second."} {
		if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "insert", Markdown: paragraph, After: "Second."}}, agent, nil); err != nil {
			t.Fatalf("apply %q: %v", paragraph, err)
		}
	}
	write := joinedLiveWrite(joined, artifactID)
	if write == nil || len(write.versions) != 2 {
		t.Fatal("the transaction did not append twice, so nothing here is tested")
	}
	last := int64(write.versions[1])
	requireNoAnnouncement(t, heard, "before the commit")
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	requireAnnouncement(t, heard, artifactID, last, service.ProcessID(), "the joined transaction's commit")
	requireNoAnnouncement(t, heard, "after the commit's one announcement")
}

// A rebuild replaces a document's whole stored history in its own transaction (RebuildTx), and
// announces it there too, with the head it seeded: every room resident elsewhere holds a history
// that no longer exists.
func TestARebuildAnnouncesTheDocumentsChange(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	// invalid makes the document's first load unreadable, which is what a rebuild needs to see
	// before it replaces the history (rebuild_test.go).
	service := New(Deps{Store: database, Persistence: &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	heard := listenForDocumentChanges(t, database)

	replacement := "Rebuilt.\n"
	report, _, err := rebuildDocument(t, service, artifactID, &replacement)
	if err != nil {
		t.Fatalf("rebuild the document: %v", err)
	}
	// The store under test is built outside New, so it announces with no sender.
	requireAnnouncement(t, heard, artifactID, report.Head, "", "the rebuild")
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
