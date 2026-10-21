package docs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
	evictor := newStaleRoomEvictor(service)
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
	select {
	case <-listening:
	case <-time.After(roomWait(t)):
		t.Fatal("the stale-room evictor never listened")
	}
	return notes
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
