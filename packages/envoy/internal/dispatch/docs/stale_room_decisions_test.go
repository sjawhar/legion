package docs

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The stale-room evictor decides every room on that room's own goroutine, whatever brought the
// room to it, and reaches a room that was still loading when the notice for it arrived. The tests
// here run it over two services on one database, as production's two tasks are.

// A notice that arrives while this process's room for the document is still loading is decided
// once that room is resident. ygo reports a loading room as absent (Server.GetDoc), so the notice
// of another process's write committed after the load read the store finds no room to decide on;
// the load's own record catches only this process's writes (roomLoad.took), and nothing else looks
// at the room once it is resident, so it would stand behind the store until a browser's edit was
// refused by the gate or the room unloaded. Here the load is held between its read of the store
// and the hook that starts its instance (OnLoadDocument), the other process writes there, and the
// room that becomes resident is closed, so the next load reads the other process's write.
func TestANoticeHeardWhileTheRoomLoadsEvictsTheRoomOnceItIsResident(t *testing.T) {
	ctx := context.Background()
	pair := newProcessPair(t)
	reader, writer := pair.taskA, pair.taskB
	reader.settle = time.Hour
	if err := reader.Evict(ctx, pair.artifactID); err != nil {
		t.Fatalf("evict the reader's room: %v", err)
	}
	waitForNoLiveDocument(t, reader, pair.artifactID)
	notes := runStaleRoomEvictor(t, reader)

	entered, release := make(chan struct{}), make(chan struct{})
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)
	var holding sync.Once
	load := reader.srv.OnLoadDocument
	reader.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == pair.artifactID {
			holding.Do(func() {
				close(entered)
				<-release
			})
		}
		return load(ctx, room, doc)
	}
	loaded := make(chan error, 1)
	go func() { loaded <- reader.warmLiveDocument(ctx, pair.artifactID) }()
	select {
	case <-entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the reader's load never reached the window it is held in")
	}
	read := storedHead(t, pair.database, pair.artifactID)

	const written = "Written on the other process while this one loads."
	if _, err := joinedApplyOps(writer, pair.artifactID, []model.EditOp{
		{Op: "insert", After: "end", Markdown: written},
	}, model.Actor{Kind: "session", ID: "agent"}, nil); err != nil {
		t.Fatalf("the other process's write: %v", err)
	}
	if head := storedHead(t, pair.database, pair.artifactID); head <= read {
		t.Fatalf("the document's head is %d after the other process's write, want past %d, the head the load read", head, read)
	}
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the other process's write during the load"); note.evicted {
		t.Fatal("the notice heard during the load evicted a room that was not resident yet")
	}

	let()
	if err := <-loaded; err != nil {
		t.Fatalf("load the reader's room: %v", err)
	}
	room := reader.srv.GetDoc(pair.artifactID)
	if room == nil {
		t.Fatal("the reader's room is not resident after its load")
	}
	if at := reader.cursorOf(room).heldThrough(); at != read {
		t.Fatalf("the loaded room stands at %d, want %d, the head its load read, so nothing here is tested", at, read)
	}
	// Well inside roomIdleTimeout: a room this test waited that long for would be one that left
	// because nothing was in it, which is not the closure this row is about.
	waitFor(t, min(roomWait(t), roomIdleTimeout/2), "the room that loaded behind the other process's write to be closed", func() bool {
		return reader.srv.GetDoc(pair.artifactID) != room
	})

	if err := reader.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("reload the reader's room: %v", err)
	}
	if roomBehind(t, reader, pair.artifactID) {
		t.Fatal("the reloaded room stands behind the head")
	}
	markdown, err := renderTree(liveTree(t, reader, pair.artifactID))
	if err != nil {
		t.Fatalf("render the reloaded room: %v", err)
	}
	if !strings.Contains(markdown, written) {
		t.Fatalf("the reloaded room holds %q, want the other process's paragraph", markdown)
	}
}

// One room's re-examination waiting on its document's lock holds no other document up. The wake
// that re-examines a room once its interval's window has passed (woke) reaches the confirmation
// under the document's advisory lock (roomBehindDurableHead) for a room still behind the head,
// and waits there for as long as a writer holds that lock. Made on the listener's goroutine, that
// wait would leave every notification behind it unread: no other room's decision would start, and
// the head tracker would go on trusting a connection nobody reads, so a cached cold read of a
// document the other process had just written would be served stale for up to headTrustWindow.
// Here the lock is held while the wake comes due, and the other process then writes two other
// documents: the notice of one, which this process holds a cached cold read of, is decided, and
// the next read returns the new text; the notice of the other, whose room here it leaves behind,
// closes that room. Once the lock is released the waiting room is closed too.
func TestAStaleRoomWaitingOnItsDocumentsLockHoldsNoOtherDocumentUp(t *testing.T) {
	ctx := context.Background()
	pair := newProcessPair(t)
	reader, writer := pair.taskA, pair.taskB
	agent := model.Actor{Kind: "session", ID: "agent"}
	cold := createDocument(t, pair.database, "# Cold")
	seedServiceText(t, writer, cold, "Cold 0.\n")
	resident := createDocument(t, pair.database, "# Resident")
	seedServiceText(t, writer, resident, "Resident 0.\n")

	evictor := newStaleRoomEvictor(reader)
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	evictor.now = func() time.Time { return time.Unix(0, clock.Load()) }
	evictor.listenIdle = time.Minute
	notes := runStaleRoomEvictorListening(t, evictor)
	bob := model.Actor{Kind: "user", ID: "bob"}

	// The eviction the interval defers the next write's re-examination by.
	if err := reader.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load the reader's room: %v", err)
	}
	editAsBrowserPeer(t, writer, pair.artifactID, 2, bob, appendBlocks(t, "Bob types once.\n"))
	waitForLandedAppends(t, writer, pair.artifactID)
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the first write"); !note.evicted {
		t.Fatal("the first write on the other process evicted nothing, so the interval holds no eviction to defer the next one by")
	}
	waitForNoLiveDocument(t, reader, pair.artifactID)
	if err := reader.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load the reader's room again: %v", err)
	}
	editAsBrowserPeer(t, writer, pair.artifactID, 3, bob, appendBlocks(t, "Bob types again.\n"))
	waitForLandedAppends(t, writer, pair.artifactID)
	if note := waitStaleRoomNote(t, notes, pair.artifactID, "the write inside the window"); note.evicted {
		t.Fatalf("the write inside the %s window evicted the room again", staleRoomEvictInterval)
	}
	if !roomBehind(t, reader, pair.artifactID) {
		t.Fatal("the reader's room stands at the head after the write inside the window, so nothing here is tested")
	}

	// The two other documents as this process holds them: a cached cold read, and a room.
	if text, err := reader.Text(ctx, cold); err != nil || text != "Cold 0.\n" {
		t.Fatalf("the first cold read = %q (%v), want the seeded text", text, err)
	}
	if cacheEntry(t, reader, cold) == nil {
		t.Fatal("the cold read cached nothing, so nothing here is tested")
	}
	if err := reader.warmLiveDocument(ctx, resident); err != nil {
		t.Fatalf("load the reader's room for the resident document: %v", err)
	}
	residentRoom := reader.srv.GetDoc(resident)

	// The window ends while a writer holds the document's lock: the wake's re-examination of the
	// room behind the head waits on it.
	unlock := holdDocumentLock(t, pair.database, pair.artifactID)
	clock.Add(int64(staleRoomEvictInterval))
	waitFor(t, roomWait(t), "the wake's re-examination to wait on the document's lock", func() bool {
		return waitingAdvisoryLocks(t, pair.database) >= 1
	})

	if _, err := joinedApplyOps(writer, cold, []model.EditOp{
		{Op: "replace", Find: "Cold 0.", With: "Cold 1."},
	}, agent, nil); err != nil {
		t.Fatalf("the other process's write of the cold document: %v", err)
	}
	if note := waitStaleRoomNote(t, notes, cold, "the cold document's write while a room waits on its lock"); note.evicted {
		t.Fatal("the cold document's notice evicted a room this process does not hold")
	}
	if text, err := reader.Text(ctx, cold); err != nil || text != "Cold 1.\n" {
		t.Fatalf("the cold read once the other process's write was decided = %q (%v), want the new text", text, err)
	}

	if _, err := joinedApplyOps(writer, resident, []model.EditOp{
		{Op: "replace", Find: "Resident 0.", With: "Resident 1."},
	}, agent, nil); err != nil {
		t.Fatalf("the other process's write of the resident document: %v", err)
	}
	if note := waitStaleRoomNote(t, notes, resident, "the resident document's write while a room waits on its lock"); !note.evicted {
		t.Fatal("the resident document's room, which the other process's write left behind, was not evicted while another room waited on its lock")
	}
	if reader.srv.GetDoc(resident) == residentRoom {
		t.Fatal("the resident document's room is still resident after its eviction")
	}
	if reader.srv.GetDoc(pair.artifactID) == nil {
		t.Fatal("the room whose re-examination waits on the lock was closed while the lock was held, so nothing here waited")
	}

	unlock()
	waitFor(t, min(roomWait(t), roomIdleTimeout/2), "the room the wake re-examined to be evicted once the document's lock was released", func() bool {
		return reader.srv.GetDoc(pair.artifactID) == nil
	})
}
