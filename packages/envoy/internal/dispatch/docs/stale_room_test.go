package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Two envoy-dispatch processes serve one Postgres, and a room resident in one of them never
// receives the updates the other appends: only a reload brings them in. Everything a process
// renders from such a room - a settlement's version and its ask reconciliation, an upload's
// replacement, an agent's edit - would be written over text that process has never seen, so every
// one of them is refused while the room's cursor is not the durable head (ErrRoomBehindHead), and
// a write that can be made on the stored document instead is (coldFork). The tests here drive each
// of those paths through two services on one database, as production's two tasks are, and each one
// fails without the gate.

// processPair is one document served by two processes: taskA, whose rooms the tests leave behind
// the head, and taskB, the other process, on a pool of its own (storetest.SecondProcess) as a
// second envoy-dispatch task has. serverA is task A's document websocket, which the eviction
// tests connect a browser to (stale_room_eviction_test.go).
type processPair struct {
	database   *store.Store
	artifactID string
	taskA      *Service
	serverA    *httptest.Server
	taskB      *Service
}

func newProcessPair(t *testing.T) processPair {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, serverA := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, taskA, artifactID)
	return processPair{database: database, artifactID: artifactID, taskA: taskA, serverA: serverA, taskB: taskB}
}

// leaveTaskABehind loads task A's room, then has a browser on task B edit the document: task A's
// room holds every update through the head it loaded and not that one, which is the state every
// gate here refuses to write from.
//
// Task B's own room can be behind the head too, and that is what editAsBrowserPeer is for: this
// process's settlement of the seeded document can append its repair while task B's room is
// loading, which leaves that room one version short, so the gate refuses the browser's append and
// fails the room. A browser's provider reconnects and resends there, and so does this.
func (pair processPair) leaveTaskABehind(t *testing.T, connection uint64, peer model.Actor, markdown string) {
	t.Helper()
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, connection, peer, appendBlocks(t, markdown))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if behind := roomBehind(t, pair.taskA, pair.artifactID); !behind {
		t.Fatal("task A's room stands at the head after task B's edit, so nothing here is tested")
	}
}

// roomBehind reports whether service's room for artifactID stands behind the document's head.
func roomBehind(t *testing.T, service *Service, artifactID string) bool {
	t.Helper()
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("no room is resident")
	}
	cursor := service.cursorOf(live)
	if cursor == nil {
		t.Fatal("the resident room lists no cursor")
	}
	return cursor.heldThrough() != storedHead(t, service.store, artifactID)
}

// storedHead is the document's durable head, the version its stored history stands at.
func storedHead(t *testing.T, database *store.Store, artifactID string) int64 {
	t.Helper()
	var head int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the document's head: %v", err)
	}
	return head
}

// storedMarkdown is the stored document's rendering, the one every reload of it reads.
func storedMarkdown(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	markdown, err := renderTree(persistedProofTree(t, database, artifactID))
	if err != nil {
		t.Fatalf("render the stored document: %v", err)
	}
	return markdown
}

// latestVersionOf is the document's latest version: its markdown and the update cursor it records,
// past which a later settlement looks for content changes (contentChangedSinceVersion).
func latestVersionOf(t *testing.T, database *store.Store, artifactID string) (markdown string, cursor int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select markdown, doc_update_version from artifact_versions
		where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown, &cursor); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	return markdown, cursor
}

// requireRoomStillWrites fails unless service's room can still make a durable write: a browser's
// edit on it reaches the store. The gate refuses the append of a room whose cursor is behind the
// head, which fails the room, so a missed advance shows here and nowhere else - the room would be
// reloaded and refused again at its next edit.
func requireRoomStillWrites(t *testing.T, service *Service, artifactID string, connection uint64) {
	t.Helper()
	text := fmt.Sprintf("Writing again as peer %d.", connection)
	before := storedHead(t, service.store, artifactID)
	editAsConnectedPeer(t, service, artifactID, connection,
		model.Actor{Kind: "user", ID: fmt.Sprintf("peer-%d", connection)}, appendBlocks(t, text+"\n"))
	deadline := time.Now().Add(roomWait(t))
	for time.Now().Before(deadline) {
		if strings.Contains(storedMarkdown(t, service.store, artifactID), text) {
			if head := storedHead(t, service.store, artifactID); head <= before {
				t.Fatalf("the room's edit is stored at head %d, want one past %d", head, before)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the room's edit %q never reached the store (room failure: %v): its append was refused, so this room's cursor never took an update it holds",
		text, service.roomFailure(artifactID))
}

// A settlement from a room behind the head is refused before it reads the document, so it writes
// no version and clears no pending settlement; the reload settles every update, this process's
// and the other's, into one version whose cursor covers them both. Without the gate task A's
// settlement versions its own room's text alone, records doc_update_version at a head that
// includes task B's update, and deletes the doc_settlements_pending row that says the work is
// still owed: task B's words are then past no cursor any later settlement looks behind, and
// nothing is left to version them (corruptions 1 and 2 of INFRA-66).
func TestASettlementFromARoomBehindTheHeadIsRefusedAndLosesNothing(t *testing.T) {
	pair := newProcessPair(t)
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	// Task A's room has an unversioned change of its own, so a settlement of it would write a
	// version; then task B's edit leaves it behind the head.
	editAsConnectedPeer(t, pair.taskA, pair.artifactID, 1, alice, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	pair.leaveTaskABehind(t, 2, bob, "Bob's paragraph.\n")

	versions := latestVersionNumber(t, pair.taskA, pair.artifactID)
	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	if now := latestVersionNumber(t, pair.taskA, pair.artifactID); now != versions {
		markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
		t.Fatalf("task A's settlement wrote version %d from a room behind the head: %q at cursor %d", now, markdown, cursor)
	}
	if owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID); err != nil || !owed {
		t.Fatalf("the document owes a settlement = %v (%v), want true: the refused settlement cleared the one record that the work is owed", owed, err)
	}

	// The reload brings in task B's update, and that settlement versions both.
	if err := pair.taskA.Evict(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("evict task A's room: %v", err)
	}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("reload task A's room: %v", err)
	}
	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
	for _, want := range []string{"Alice's paragraph.", "Bob's paragraph."} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("the settled version = %q, want it to hold %q", markdown, want)
		}
	}
	if head := storedHead(t, pair.database, pair.artifactID); cursor != head {
		t.Fatalf("the settled version records update cursor %d, want the head %d", cursor, head)
	}
}

// A settlement refused because its room stands behind the head reloads the room rather than
// retrying on it: no retry on that room can pass the gate, and counted as failures the third
// failed the room, so every transaction that met the document until the failure's eviction had
// run - a named version among them - was refused (INFRA-218). The settlement re-arms on its own
// timer, which reads the reloaded room and versions both processes' text at the head, clearing the
// document's pending-settlement row, and the room never fails.
func TestASettlementFromARoomBehindTheHeadReloadsItAndTheNextSettlesBoth(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")
	versions := latestVersionNumber(t, pair.taskA, pair.artifactID)
	stale := pair.taskA.srv.GetDoc(pair.artifactID)
	pair.taskA.settle = 20 * time.Millisecond

	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
		t.Fatalf("the refused settlement failed task A's room: %v", err)
	}
	if live := pair.taskA.srv.GetDoc(pair.artifactID); live == stale {
		t.Fatal("task A's room is still the one behind the head after its settlement was refused there, so its next settlement is refused too")
	}
	waitFor(t, 30*time.Second, "the re-armed settlement to settle the document", func() bool {
		owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID)
		return err == nil && !owed
	})
	if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
		t.Fatalf("task A's room failed: %v", err)
	}
	if now := latestVersionNumber(t, pair.taskA, pair.artifactID); now != versions+1 {
		t.Fatalf("latest version = %d after the re-armed settlement, want %d", now, versions+1)
	}
	markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
	if !strings.Contains(markdown, "Bob's paragraph.") {
		t.Fatalf("task A's re-armed settlement versioned %q, want it to hold Bob's paragraph", markdown)
	}
	if head := storedHead(t, pair.database, pair.artifactID); cursor != head {
		t.Fatalf("the settled version records update cursor %d, want the head %d", cursor, head)
	}
	if authors := latestVersionAuthors(t, pair.taskA, pair.artifactID); !slices.Equal(authors, []model.Actor{bob}) {
		t.Fatalf("the re-armed settlement's version lists %+v, want %+v", authors, bob)
	}
}

// A refused settlement leaves its room open, and writes nothing, while this process still has work
// in flight on it, since the close would take that work's publish with it: an open live write,
// which arms the settlement when it ends, an append the room's persistence has not stored, whose
// settlement is armed again, and a shutdown, which leaves the settlement to the next process. The
// first two then settle both processes' text once the work is done.
func TestARefusedSettlementLeavesItsRoomOpenWhileWorkIsInFlightOnIt(t *testing.T) {
	for _, inFlight := range []struct {
		name string
		// hold puts the work in flight before the settlement and returns what ends it, or nil
		// for work that leaves no settlement to wait for.
		hold func(t *testing.T, pair processPair) (end func())
	}{
		{"an open live write", func(t *testing.T, pair processPair) func() {
			write, err := pair.taskA.openLiveWrite(context.Background(), &Ledger{}, pair.artifactID)
			if err != nil {
				t.Fatalf("open a live write on task A's room: %v", err)
			}
			// The write opened with no settlement armed or running, so it owes none yet: the one
			// it arms when it ends is the one the refused settlement hands it.
			state := pair.taskA.room(pair.artifactID)
			state.mu.Lock()
			owes := state.settleDeferred
			state.mu.Unlock()
			if owes {
				t.Fatal("the write took a settlement over as it opened, so this row would not test the refused settlement's handoff")
			}
			return func() { pair.taskA.finishLiveWrite(write) }
		}},
		{"an append its persistence has not stored", func(t *testing.T, pair processPair) func() {
			// After the settlement's own wait for durable appends, before its gate: an append
			// that starts there is the one this branch is for.
			var once sync.Once
			var state *roomState
			pair.taskA.afterSettleLock = func(room string) {
				once.Do(func() {
					state = pair.taskA.room(room)
					state.durableAppends.Add(1)
				})
			}
			return func() {
				if state == nil {
					t.Fatal("the settlement never reached its gate, so no append was put in flight")
				}
				state.durableAppends.Add(-1)
			}
		}},
		{"a shutdown", func(t *testing.T, pair processPair) func() {
			pair.taskA.shutdownRooms.Store(pair.artifactID, struct{}{})
			t.Cleanup(func() { pair.taskA.shutdownRooms.Delete(pair.artifactID) })
			return nil
		}},
	} {
		t.Run(inFlight.name, func(t *testing.T) {
			pair := newProcessPair(t)
			bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
			pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")
			versions := latestVersionNumber(t, pair.taskA, pair.artifactID)
			stale := pair.taskA.srv.GetDoc(pair.artifactID)
			pair.taskA.settle = 20 * time.Millisecond
			end := inFlight.hold(t, pair)

			settleCurrentGeneration(t, pair.taskA, pair.artifactID)
			if live := pair.taskA.srv.GetDoc(pair.artifactID); live != stale {
				t.Fatal("the refused settlement closed task A's room with work in flight on it")
			}
			if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
				t.Fatalf("the refused settlement failed task A's room: %v", err)
			}
			if now := latestVersionNumber(t, pair.taskA, pair.artifactID); now != versions {
				t.Fatalf("task A's refused settlement wrote version %d", now)
			}
			if owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID); err != nil || !owed {
				t.Fatalf("the document owes a settlement = %v (%v), want true", owed, err)
			}
			if end == nil {
				return
			}

			end()
			waitFor(t, 30*time.Second, "the settlement the work left armed to settle the document", func() bool {
				owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID)
				return err == nil && !owed
			})
			if markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID); !strings.Contains(markdown, "Bob's paragraph.") ||
				cursor != storedHead(t, pair.database, pair.artifactID) {
				t.Fatalf("the settled version = %q at cursor %d, want Bob's paragraph at the head %d",
					markdown, cursor, storedHead(t, pair.database, pair.artifactID))
			}
		})
	}
}

// A close of a room behind the head - a refused settlement's, or the stale-room evictor's - refuses
// a write that would open on that room between the check that found no write in flight and the
// close: the close forgets the room's state, so the write would be forgotten with it, and the room
// that replaces it would load knowing nothing of the write while the write's commit moved that
// room's cursor past its update, so a version read from it would record text missing the write
// at a cursor claiming to hold it. The write answers 503, and once the room has reloaded the same
// write is made and stored.
func TestAWriteOpeningWhileARoomBehindTheHeadClosesIsRefused(t *testing.T) {
	for _, closer := range []struct {
		name  string
		close func(t *testing.T, pair processPair)
	}{
		{"a refused settlement", func(t *testing.T, pair processPair) {
			settleCurrentGeneration(t, pair.taskA, pair.artifactID)
		}},
		{"the stale-room evictor", func(t *testing.T, pair processPair) {
			if !newStaleRoomEvictor(pair.taskA).documentChanged(context.Background(), pair.artifactID) {
				t.Fatal("the evictor did not close task A's room behind the head")
			}
		}},
	} {
		t.Run(closer.name, func(t *testing.T) {
			pair := newProcessPair(t)
			pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
			agent := model.Actor{Kind: "session", ID: "agent-session"}
			insert := []model.EditOp{{Op: "insert", Markdown: "Agent's paragraph.", After: "Bob's paragraph."}}
			var windowErr error
			var reached atomic.Bool
			pair.taskA.afterRoomCloseReserved = func(room string) {
				if room == pair.artifactID && reached.CompareAndSwap(false, true) {
					_, windowErr = joinedApplyOps(pair.taskA, pair.artifactID, insert, agent, nil)
				}
			}

			closer.close(t, pair)
			if !reached.Load() {
				t.Fatal("the close never reserved task A's room")
			}
			if !errors.Is(windowErr, errRoomReloading) {
				t.Fatalf("a write opened as the room closed = %v, want errRoomReloading", windowErr)
			}
			if strings.Contains(storedMarkdown(t, pair.database, pair.artifactID), "Agent's paragraph.") {
				t.Fatal("the refused write reached the store")
			}
			if _, err := joinedApplyOps(pair.taskA, pair.artifactID, insert, agent, nil); err != nil {
				t.Fatalf("the write once the room has reloaded: %v", err)
			}
			if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
				t.Fatalf("task A's room failed: %v", err)
			}
			commitNamedVersion(t, pair.taskA, pair.artifactID, model.Actor{Kind: "session", ID: "versioner-a"})
			markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
			for _, want := range []string{"Bob's paragraph.", "Agent's paragraph."} {
				if !strings.Contains(markdown, want) {
					t.Fatalf("the version after the reload = %q, want it to hold %q", markdown, want)
				}
			}
			if head := storedHead(t, pair.database, pair.artifactID); cursor != head {
				t.Fatalf("the version after the reload records update cursor %d, want the head %d", cursor, head)
			}
		})
	}
}

// The stale-room evictor closes the room instance it found behind the head and no other: once that
// instance has been replaced - a settlement's own reload, or any other close and load - the room
// the document holds loaded at the head, and closing it by name would drop its browsers for
// nothing and take a write opening on it with it.
func TestTheEvictorClosesOnlyTheRoomItFoundBehindTheHead(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
	stale := pair.taskA.srv.GetDoc(pair.artifactID)
	if err := pair.taskA.Evict(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("evict task A's room: %v", err)
	}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("reload task A's room: %v", err)
	}
	replacement := pair.taskA.srv.GetDoc(pair.artifactID)

	closed, err := pair.taskA.closeRoomBehindHead(context.Background(), pair.artifactID, stale)
	if err != nil || closed {
		t.Fatalf("close the replaced room = %v, %v, want false and no error", closed, err)
	}
	if live := pair.taskA.srv.GetDoc(pair.artifactID); live != replacement {
		t.Fatal("the close of the replaced instance closed the room that replaced it")
	}
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 2)
}

// A close of a room behind the head - a refused settlement's, or the stale-room evictor's - closes
// the instance it checked and no other, however that instance goes before the close reaches it.
// ygo's idle sweep retires a room without the close gate, so the room can be retired, and a
// browser's or a read's load can replace it at the head, after the close checked it and before
// ygo looks the room up by name: ygo's CloseRoom then closed the replacement, dropping its
// browsers, and the reservation stayed on the state the replacement loaded onto, refusing every
// write on it as reloading. The replacement stays open and writable, and the document settles.
func TestACloseOfARoomBehindTheHeadLeavesTheRoomThatReplacedIt(t *testing.T) {
	for _, closer := range []struct {
		name  string
		close func(t *testing.T, pair processPair)
	}{
		{"a refused settlement", func(t *testing.T, pair processPair) {
			settleCurrentGeneration(t, pair.taskA, pair.artifactID)
		}},
		{"the stale-room evictor", func(t *testing.T, pair processPair) {
			evictor := newStaleRoomEvictor(pair.taskA)
			if evictor.documentChanged(context.Background(), pair.artifactID) {
				t.Fatal("the evictor reports a close of the room it checked, which was retired before the close reached it")
			}
			// A close that closed nothing is an attempt that got no answer: the room is armed
			// again rather than its window spent (armRetry).
			if wake := evictor.wakeIn(); wake <= 0 {
				t.Fatalf("the close that closed nothing spent the room's window (wakeIn = %s)", wake)
			}
		}},
	} {
		t.Run(closer.name, func(t *testing.T) {
			pair := newProcessPair(t)
			pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
			pair.taskA.settle = 20 * time.Millisecond
			var replacement *crdt.Doc
			var reached atomic.Bool
			pair.taskA.afterRoomCloseReserved = func(room string) {
				if room != pair.artifactID || !reached.CompareAndSwap(false, true) {
					return
				}
				// ygo's idle sweep, which takes no close gate, retires the room, and a load brings
				// the document back at the head.
				if err := pair.taskA.srv.Server.CloseRoom(room, true); err != nil {
					t.Errorf("retire task A's room as the idle sweep does: %v", err)
					return
				}
				if err := pair.taskA.warmLiveDocument(context.Background(), room); err != nil {
					t.Errorf("load the room that replaces it: %v", err)
					return
				}
				replacement = pair.taskA.srv.GetDoc(room)
			}

			closer.close(t, pair)
			if !reached.Load() {
				t.Fatal("the close never reserved task A's room")
			}
			if replacement == nil {
				t.Fatal("no room replaced the one the sweep retired")
			}
			if live := pair.taskA.srv.GetDoc(pair.artifactID); live != replacement {
				t.Fatal("the close of the retired room closed the room that replaced it")
			}
			agent := model.Actor{Kind: "session", ID: "agent-session"}
			insert := []model.EditOp{{Op: "insert", Markdown: "Agent's paragraph.", After: "Bob's paragraph."}}
			if _, err := joinedApplyOps(pair.taskA, pair.artifactID, insert, agent, nil); err != nil {
				t.Fatalf("a write on the room that replaced the closed one: %v", err)
			}
			waitFor(t, 30*time.Second, "the document to settle on the room that replaced the closed one", func() bool {
				owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID)
				return err == nil && !owed
			})
			markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
			for _, want := range []string{"Bob's paragraph.", "Agent's paragraph."} {
				if !strings.Contains(markdown, want) {
					t.Fatalf("the settled version = %q, want it to hold %q", markdown, want)
				}
			}
			if head := storedHead(t, pair.database, pair.artifactID); cursor != head {
				t.Fatalf("the settled version records update cursor %d, want the head %d", cursor, head)
			}
			if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
				t.Fatalf("task A's room failed: %v", err)
			}
			requireRoomStillWrites(t, pair.taskA, pair.artifactID, 2)
		})
	}
}

// The close of a room behind the durable head is the service's own, so a closed issue does not
// refuse it. The close runs inside a Server.Apply, and an injection a browser or an agent makes is
// refused once the document's issue is closed (allowInject): without the service's own mark on
// that context (withOwnerVerified), the close of a room whose issue closed between its load and
// the notification was refused ErrIssueClosed, and the room stayed resident with text the store no
// longer holds until its idle minute ran out. The issue closes in the database alone here, as the
// close's own transaction leaves it for the moment before SetIssueClosed runs, and as a room this
// process holds sees an issue the other process closed.
func TestACloseOfARoomBehindTheHeadIsNotRefusedByItsClosedIssue(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
	stale := pair.taskA.srv.GetDoc(pair.artifactID)
	if _, err := pair.database.Pool.Exec(context.Background(),
		`update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the document's issue: %v", err)
	}

	closed, err := pair.taskA.closeRoomBehindHead(context.Background(), pair.artifactID, stale)
	if err != nil {
		t.Fatalf("close the closed issue's room behind the head: %v", err)
	}
	if !closed {
		t.Fatal("the close of the closed issue's room behind the head closed nothing")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// A close that fails lifts the reservation it took. The reservation refuses every transaction the
// room's writer slot (openLiveWrite, errRoomReloading), so a close that returned without lifting
// it would leave a resident room answering 503 to every write until it unloaded - and a connected
// browser keeps it resident, so that is an outage a person sees rather than one access. The close
// is made to fail here as a rebuild makes it fail: a rebuild holds the document and refuses every
// injection into its room, the one refusal the close's own mark does not pass (refuseIfRebuilding).
func TestACloseOfARoomBehindTheHeadThatFailsLiftsItsReservation(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
	stale := pair.taskA.srv.GetDoc(pair.artifactID)
	var reserved atomic.Bool
	pair.taskA.afterRoomCloseReserved = func(room string) {
		if room == pair.artifactID && reserved.CompareAndSwap(false, true) {
			pair.taskA.rebuilding.Store(room, struct{}{})
		}
	}

	closed, err := pair.taskA.closeRoomBehindHead(context.Background(), pair.artifactID, stale)
	pair.taskA.rebuilding.Delete(pair.artifactID)
	if !reserved.Load() {
		t.Fatal("the close never reserved task A's room")
	}
	if closed {
		t.Fatal("the refused close reports that it closed the room")
	}
	if err == nil {
		t.Fatal("the refused close reports no error")
	}

	// The room is the one the close failed to close, and its writer slot is open again: the
	// reservation was lifted. The room still stands behind the head, so the write takes the cold
	// path rather than the room's, which is what a write on a reserved room is refused before
	// reaching (errRoomReloading).
	if live := pair.taskA.srv.GetDoc(pair.artifactID); live != stale {
		t.Fatal("the refused close left another room behind")
	}
	state := pair.taskA.lockState(pair.artifactID)
	reloading := state.reloading
	pair.taskA.unlockState(pair.artifactID, state)
	if reloading {
		t.Fatal("the refused close left its reservation on the room, which refuses every write")
	}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	insert := []model.EditOp{{Op: "insert", Markdown: "Agent's paragraph.", After: "Bob's paragraph."}}
	if _, err := joinedApplyOps(pair.taskA, pair.artifactID, insert, agent, nil); err != nil {
		t.Fatalf("a write after the refused close: %v", err)
	}
}

// A write that opens after the evictor's own check for a write in flight, and before the close
// takes the room's state lock, leaves the room open. The evictor reads that check outside any
// lock (hasWriteInFlight), so the close reads it again under the lock that admits a writer
// (closeRoomBehindHead): without the second read the close would retire the room under a write
// whose commit still advances the replacement room's cursor, which is the stale-text-at-a-head-
// cursor corruption the reservation exists to prevent. The room is left exactly as stale as it
// was found, and the evictor keeps a window of its own for it.
func TestAWriteThatOpensBeforeTheCloseTakesItsLockLeavesTheRoomOpen(t *testing.T) {
	pair := newProcessPair(t)
	evictor := newStaleRoomEvictor(pair.taskA)
	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob"}, "Bob types once.\n")
	live := pair.taskA.srv.GetDoc(pair.artifactID)
	var opened atomic.Bool
	pair.taskA.beforeCloseBehindHeadLock = func(room string) {
		if room != pair.artifactID || !opened.CompareAndSwap(false, true) {
			return
		}
		state := pair.taskA.lockState(room)
		state.liveWriter = &liveWrite{}
		pair.taskA.unlockState(room, state)
	}

	if evictor.documentChanged(context.Background(), pair.artifactID) {
		t.Fatal("the evictor closed the room under a write that opened after its own check")
	}
	if !opened.Load() {
		t.Fatal("the close never reached its state lock, so nothing here is tested")
	}
	if pair.taskA.srv.GetDoc(pair.artifactID) != live {
		t.Fatal("the room was closed under the write in flight on it")
	}
	if wake := evictor.wakeIn(); wake <= 0 {
		t.Fatalf("the attempt that found a write in flight spent the room's window (wakeIn = %s)", wake)
	}

	// The write ends, and the same close closes the room: nothing else held it open.
	state := pair.taskA.lockState(pair.artifactID)
	state.liveWriter = nil
	pair.taskA.unlockState(pair.artifactID, state)
	closed, err := pair.taskA.closeRoomBehindHead(context.Background(), pair.artifactID, live)
	if err != nil {
		t.Fatalf("close the room once its write has ended: %v", err)
	}
	if !closed {
		t.Fatal("the close of the room whose write has ended closed nothing")
	}
	waitForNoLiveDocument(t, pair.taskA, pair.artifactID)
}

// The close of a room behind the head holds the room it checked against ygo's idle sweep until it
// has closed it: the sweep retires a room no peer is in once it has been idle for the room idle
// timeout, and it takes no close gate, so a check of the instance ahead of the close would not
// hold the instance ygo then looks up by name. The close runs inside a Server.Apply, which the
// sweep never retires. A browser keeps the room resident until the close holds it, so nothing can
// retire it sooner; inside the hold the browser leaves, which stamps the room idle, and a second
// document's room is loaded and left idle after it. Once the sweep has retired that sentinel, a
// sweep pass has run with the held room idle for longer than the timeout as well, and it is still
// the room the document holds when the close closes it.
func TestACloseOfARoomBehindTheHeadHoldsItAgainstTheIdleSweep(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	// ygo's sweeper reads the timeout when the first room starts it, so it is set before any room:
	// a room idle for a second is retired at the next sweep, which then runs every second.
	service.srv.RoomIdleTimeout = time.Second
	seedServiceText(t, service, artifactID, "before")
	sentinel := createDocument(t, service.store, "Sentinel.")
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	browser := connectPeer(t, server.URL, artifactID, "alice")
	connected := func() int {
		state := service.room(artifactID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.connected)
	}
	waitFor(t, 10*time.Second, "the browser to join the document's room", func() bool {
		return connected() == 1 && service.srv.GetDoc(artifactID) != nil
	})
	live := service.srv.GetDoc(artifactID)

	const sweepBudget = 30 * time.Second
	var holdErr error
	var heldThrough bool
	service.afterRoomClosePinned = func(room string) {
		browser.Close()
		// The service forgets the browser once ygo's teardown of it has returned, which is after
		// that teardown stamped the room idle.
		for deadline := time.Now().Add(10 * time.Second); connected() != 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				holdErr = errors.New("the browser's leaving was never torn down")
				return
			}
		}
		if err := service.warmLiveDocument(context.Background(), sentinel); err != nil {
			holdErr = fmt.Errorf("load the sentinel document's room: %w", err)
			return
		}
		for deadline := time.Now().Add(sweepBudget); service.srv.GetDoc(sentinel) != nil; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				holdErr = fmt.Errorf("the idle sweep did not retire the sentinel room within %v", sweepBudget)
				return
			}
		}
		heldThrough = service.srv.GetDoc(room) == live
	}
	closed, err := service.closeRoomInstance(context.Background(), artifactID, live)
	if err != nil {
		t.Fatalf("close the room: %v", err)
	}
	if holdErr != nil {
		t.Fatal(holdErr)
	}
	if !heldThrough {
		t.Fatal("the idle sweep retired the room while the close held it")
	}
	if !closed {
		t.Fatal("the close of the room it held closed nothing")
	}
	waitForNoLiveDocument(t, service, artifactID)
}

// A write that opens while a settlement runs moves the room's generation, which ends that
// settlement, so the write takes it over and arms it when the write ends - also when the write
// changed nothing, which publishes no update whose observer would arm one. A settlement the durable
// gate refused from a room behind the head that met such a write left nothing armed: a write still
// open took it only when the refusal found the write open, and one that had ended took nothing,
// so the document stayed owed until the minute-old resumption came back for it. The first row is
// the control, which the refusal's own handoff to an open write passed before the take-over; the
// second is the take-over of a settlement already running (settling).
func TestAWriteOpenedWhileASettlementRunsArmsItWhenItEnds(t *testing.T) {
	for _, writer := range []struct {
		name string
		// open runs inside the settlement, after it read its generation and before its gate, and
		// returns what ends the write, or nil for a write that has ended already.
		open func(t *testing.T, pair processPair) (end func())
	}{
		{"still open at the refusal (control)", func(t *testing.T, pair processPair) func() {
			write, err := pair.taskA.openLiveWrite(context.Background(), &Ledger{}, pair.artifactID)
			if err != nil {
				t.Errorf("open a live write on task A's room: %v", err)
				return nil
			}
			return func() { pair.taskA.finishLiveWrite(write) }
		}},
		{"ended having changed nothing before the refusal", func(t *testing.T, pair processPair) func() {
			write, err := pair.taskA.openLiveWrite(context.Background(), &Ledger{}, pair.artifactID)
			if err != nil {
				t.Errorf("open a live write on task A's room: %v", err)
				return nil
			}
			pair.taskA.finishLiveWrite(write)
			return nil
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			pair := newProcessPair(t)
			pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
			versions := latestVersionNumber(t, pair.taskA, pair.artifactID)
			stale := pair.taskA.srv.GetDoc(pair.artifactID)
			pair.taskA.settle = 20 * time.Millisecond
			var once sync.Once
			var end func()
			pair.taskA.afterSettleLock = func(room string) {
				if room == pair.artifactID {
					once.Do(func() { end = writer.open(t, pair) })
				}
			}

			settleCurrentGeneration(t, pair.taskA, pair.artifactID)
			if end != nil {
				if live := pair.taskA.srv.GetDoc(pair.artifactID); live != stale {
					t.Fatal("the refused settlement closed task A's room with a write open on it")
				}
				if now := latestVersionNumber(t, pair.taskA, pair.artifactID); now != versions {
					t.Fatalf("task A's refused settlement wrote version %d", now)
				}
				end()
			}
			waitFor(t, 30*time.Second, "the settlement the write took over to settle the document", func() bool {
				owed, err := settlementPending(context.Background(), pair.database.Pool, pair.artifactID)
				return err == nil && !owed
			})
			if err := pair.taskA.roomFailure(pair.artifactID); err != nil {
				t.Fatalf("task A's room failed: %v", err)
			}
			markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
			if !strings.Contains(markdown, "Bob's paragraph.") || cursor != storedHead(t, pair.database, pair.artifactID) {
				t.Fatalf("the settled version = %q at cursor %d, want Bob's paragraph at the head %d",
					markdown, cursor, storedHead(t, pair.database, pair.artifactID))
			}
		})
	}
}

// A write that opens as a settlement starts - its timer fired and its callback not yet past the
// generation it compares, or the last browser leaving and settleLastPeer having taken the timer -
// ends that settlement too, and takes it over though no timer is left to stop: a fired timer is
// still registered until its callback returns, and settleLastPeer counts its settlement running
// from the moment it takes the timer. Taking over only the timer it stopped, the write that changed
// nothing armed nothing, and the document stayed owed until the minute-old resumption.
func TestAWriteOpenedAsASettlementStartsArmsItWhenItEnds(t *testing.T) {
	for _, start := range []struct {
		name string
		// fire starts the settlement the write meets, its timer armed an hour out.
		fire func(t *testing.T, service *Service, artifactID string, browser *docstest.Peer)
	}{
		{"its timer fired", func(t *testing.T, service *Service, artifactID string, _ *docstest.Peer) {
			service.scheduleSettle(artifactID)
		}},
		{"the last browser left", func(t *testing.T, _ *Service, _ string, browser *docstest.Peer) {
			browser.Close()
		}},
	} {
		t.Run(start.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(server.Close)
			browser := connectPeer(t, server.URL, artifactID, "alice")
			waitFor(t, 10*time.Second, "the browser to join the document's room", func() bool {
				return service.srv.GetDoc(artifactID) != nil
			})
			editLiveTree(t, service, artifactID, replaceRun("before", "after"))
			waitForLandedAppends(t, service, artifactID)
			if owed, err := settlementPending(context.Background(), service.store.Pool, artifactID); err != nil || !owed {
				t.Fatalf("the document owes a settlement = %v (%v), want true", owed, err)
			}
			versions := latestVersionNumber(t, service, artifactID)

			// From here every settlement runs within 20 ms of being armed; the one armed now is
			// the hour-long timer the edit armed. The hook opens a write as the settlement starts.
			// The state lock the timer check takes orders the delay's write and the hook's before
			// the settlement's goroutines read them: settleLastPeer takes that lock before it
			// settles, and a timer armed after it runs after it, while a browser's leaving reaches
			// the server only through its socket, which orders nothing.
			var once sync.Once
			service.beforeSettleStart = func(room string) {
				if room != artifactID {
					return
				}
				once.Do(func() {
					write, err := service.openLiveWrite(context.Background(), &Ledger{}, artifactID)
					if err != nil {
						t.Errorf("open a live write as the settlement starts: %v", err)
						return
					}
					service.finishLiveWrite(write)
				})
			}
			service.settle = 20 * time.Millisecond
			state := service.room(artifactID)
			state.mu.Lock()
			armed := service.isSettleTimerArmed(state.settle)
			state.mu.Unlock()
			if !armed {
				t.Fatal("the edit armed no settlement, so no settlement starts for a write to take over")
			}

			start.fire(t, service, artifactID, browser)
			waitFor(t, 10*time.Second, "the settlement the write took over to settle the document", func() bool {
				owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
				return err == nil && !owed
			})
			if now := latestVersionNumber(t, service, artifactID); now != versions+1 {
				t.Fatalf("the document's latest version is %d, want %d: one settlement of the edit", now, versions+1)
			}
		})
	}
}

// A named version on a process whose room stands behind the head records the stored document,
// which holds the other process's paragraph, at the head it records. Rendered from the room it
// recorded text missing Bob's paragraph at a cursor saying it held it, so no later settlement saw
// those words past that cursor (INFRA-218; corruption 1 of INFRA-66, on the version path).
func TestANamedVersionFromARoomBehindTheHeadRecordsTheStoredDocument(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")

	versioner := model.Actor{Kind: "session", ID: "versioner-a"}
	version := commitNamedVersion(t, pair.taskA, pair.artifactID, versioner)
	markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
	if stored := storedMarkdown(t, pair.database, pair.artifactID); markdown != stored {
		t.Fatalf("task A's named version = %q, want the stored document %q", markdown, stored)
	}
	if head := storedHead(t, pair.database, pair.artifactID); cursor != head {
		t.Fatalf("task A's named version records update cursor %d, want the head %d", cursor, head)
	}
	if !slices.Equal(version.Authors, []model.Actor{versioner, bob}) {
		t.Fatalf("task A's named version lists %+v, want %+v and %+v", version.Authors, versioner, bob)
	}
}

// A browser edit a room behind the head takes is not listed on a version that room's process names
// while the edit is in flight: the version is rendered from the stored document, which does not
// hold the edit, and the edit's append is refused from that room, so its browser resends it to the
// reloaded room and it is credited again there. Listed from the room's in-flight credits, alice was
// listed on that version and again on the one holding her resent edit (INFRA-218).
func TestAnEditInFlightOnARoomBehindTheHeadIsListedOnlyOnceItIsStored(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")

	room := pair.taskA.srv.GetDoc(pair.artifactID)
	peer, update := peerEdit(t, room, appendBlocks(t, "Alice's paragraph.\n"))
	pair.taskA.addConnection(pair.artifactID, 2, alice)
	release := holdObserver(t, pair.taskA, pair.artifactID, &pair.taskA.afterCreditUpdate, func() error {
		return crdt.ApplyUpdateV1(room, update, "peer")
	})
	inFlight := commitNamedVersion(t, pair.taskA, pair.artifactID, model.Actor{Kind: "session", ID: "versioner-a"})
	release()
	pair.taskA.removeConnection(pair.artifactID, 2)
	if slices.Contains(inFlight.Authors, alice) {
		t.Fatalf("the version named while alice's edit was in flight lists %+v, want no alice: the version does not hold her edit", inFlight.Authors)
	}
	if markdown, _ := latestVersionOf(t, pair.database, pair.artifactID); !strings.Contains(markdown, "Bob's paragraph.") || strings.Contains(markdown, "Alice's paragraph.") {
		t.Fatalf("the version named while alice's edit was in flight = %q, want Bob's paragraph and not Alice's", markdown)
	}

	resendUntilStored(t, pair.taskA, pair.artifactID, 2, alice, peer)
	commitNamedVersion(t, pair.taskA, pair.artifactID, model.Actor{Kind: "session", ID: "sweeper-a"})
	for _, author := range []model.Actor{alice, bob} {
		if listed := countVersionAuthor(t, pair.database, pair.artifactID, author); listed != 1 {
			t.Errorf("%s is listed on %d versions, want exactly one", author.ID, listed)
		}
	}
	if owed := pendingAuthorRows(t, pair.database, pair.artifactID); len(owed) != 0 {
		t.Errorf("pending authors after the last version = %+v, want none", owed)
	}
}

// A transaction whose own write takes the cold path because its room is behind the head lists none
// of that room's in-flight credits on the version it writes: the version is rendered from the
// stored document and the transaction's write, neither of which holds the in-flight edit, and that
// edit's append is refused from the room, so its browser resends it to the reloaded room, where it
// is credited again. Listed from the room's in-flight credits, alice was listed on the
// transaction's version and again on the one holding her resent edit (INFRA-218).
func TestAColdWritesVersionListsNoneOfItsRoomsEditsInFlight(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")

	room := pair.taskA.srv.GetDoc(pair.artifactID)
	peer, update := peerEdit(t, room, appendBlocks(t, "Alice's paragraph.\n"))
	pair.taskA.addConnection(pair.artifactID, 2, alice)
	release := holdObserver(t, pair.taskA, pair.artifactID, &pair.taskA.afterCreditUpdate, func() error {
		return crdt.ApplyUpdateV1(room, update, "peer")
	})
	writeThroughLedger(t, pair.taskA, pair.artifactID, agent, "Agent's paragraph.\n", withNamedVersion)
	release()
	pair.taskA.removeConnection(pair.artifactID, 2)
	markdown, cursor := latestVersionOf(t, pair.database, pair.artifactID)
	if !strings.Contains(markdown, "Bob's paragraph.") || !strings.Contains(markdown, "Agent's paragraph.") || strings.Contains(markdown, "Alice's paragraph.") {
		t.Fatalf("the cold write's version = %q, want Bob's and the agent's paragraphs and not Alice's", markdown)
	}
	if head := storedHead(t, pair.database, pair.artifactID); cursor > head {
		t.Fatalf("the cold write's version records update cursor %d, past the head %d", cursor, head)
	}
	if authors := latestVersionAuthors(t, pair.taskA, pair.artifactID); slices.Contains(authors, alice) || !slices.Contains(authors, agent) || !slices.Contains(authors, bob) {
		t.Fatalf("the cold write's version lists %+v, want %s and %s and no %s", authors, agent.ID, bob.ID, alice.ID)
	}

	resendUntilStored(t, pair.taskA, pair.artifactID, 2, alice, peer)
	commitNamedVersion(t, pair.taskA, pair.artifactID, model.Actor{Kind: "session", ID: "sweeper-a"})
	for _, author := range []model.Actor{alice, bob} {
		if listed := countVersionAuthor(t, pair.database, pair.artifactID, author); listed != 1 {
			t.Errorf("%s is listed on %d versions, want exactly one", author.ID, listed)
		}
	}
	if owed := pendingAuthorRows(t, pair.database, pair.artifactID); len(owed) != 0 {
		t.Errorf("pending authors after the last version = %+v, want none", owed)
	}
}

// An open ask whose block only the other process's room holds is not retracted: the settlement
// that cannot see it is refused before it reconciles anything. Without the gate task A's stale
// tree holds no such block, and reconcileAskBlocks retracts the ask and publishes its
// `ask.resolved` event, over a question nobody answered (corruption 3 of INFRA-66).
func TestAnOpenAskIsNotRetractedByASettlementFromARoomBehindTheHead(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob,
		appendBlocks(t, ":::ask{#bob-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	if state := askState(t, pair.database, pair.artifactID, "bob-ask"); state != "open" {
		t.Fatalf("the ask task B settled is %q, want open", state)
	}
	if behind := roomBehind(t, pair.taskA, pair.artifactID); !behind {
		t.Fatal("task A's room stands at the head after task B's ask block, so nothing here is tested")
	}

	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	if state := askState(t, pair.database, pair.artifactID, "bob-ask"); state != "open" {
		t.Fatalf("the ask is %q after task A's settlement, want open: its room cannot see the block the ask is anchored to", state)
	}
}

// askState is the state of the ask anchored to blockID of artifactID.
func askState(t *testing.T, database *store.Store, artifactID, blockID string) string {
	t.Helper()
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select state from asks where block_artifact_id = $1 and block_id = $2
	`, artifactID, blockID).Scan(&state); err != nil {
		t.Fatalf("read the ask anchored to %s: %v", blockID, err)
	}
	return state
}

// An upload from a room behind the head is rendered from the stored document, so the document it
// leaves is the one its version records. Without the gate the replacement is computed on the stale
// fork, which never held task B's paragraph: the replacement deletes what the fork held and the
// paragraph survives the merge, so the stored document holds text the version does not
// (corruption 6 of INFRA-66).
func TestAnUploadFromARoomBehindTheHeadReplacesTheStoredDocument(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")

	const uploaded = "First.\n\nSecond, uploaded on task A.\n"
	uploadOn(t, pair.taskA, pair.artifactID, uploaded, model.Actor{Kind: "session", ID: "uploader-on-task-a"})
	markdown, _ := latestVersionOf(t, pair.database, pair.artifactID)
	if markdown != uploaded {
		t.Fatalf("the upload's version = %q, want %q", markdown, uploaded)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); stored != uploaded {
		t.Fatalf("the stored document after the upload = %q, want %q: the upload replaced a document it had not read", stored, uploaded)
	}
}

// An agent's edit whose room is behind the head is made on the stored document and succeeds: the
// cold path is what keeps the gate from answering 503 for a write this process can still make
// correctly. The edit lands beside task B's paragraph, which the room it would otherwise have
// forked never held (corruption 7 of INFRA-66: a write made over text its writer never saw).
func TestAnEditFromARoomBehindTheHeadTakesTheColdPathAndSucceeds(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	pair.leaveTaskABehind(t, 1, bob, "Bob's paragraph.\n")

	if _, err := joinedApplyOps(pair.taskA, pair.artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Agent's paragraph.", After: "Bob's paragraph."},
	}, model.Actor{Kind: "session", ID: "agent-session"}, nil); err != nil {
		t.Fatalf("an agent's edit on a room behind the head: %v", err)
	}
	stored := storedMarkdown(t, pair.database, pair.artifactID)
	for _, want := range []string{"Bob's paragraph.", "Agent's paragraph."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document after the edit = %q, want it to hold %q", stored, want)
		}
	}
}

// The load is where a room's cursor starts: the head its load read (keepInstance), including the
// update the other process appended before it. A load that recorded anything else would refuse
// this room's first write.
func TestARoomLoadedAfterAnotherProcessesAppendStandsAtTheHead(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	// The room the seed left on task A would be the one behind the head; this test is about the
	// load that replaces it.
	if err := pair.taskA.Evict(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("evict the room task A seeded: %v", err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room after task B's append: %v", err)
	}
	if behind := roomBehind(t, pair.taskA, pair.artifactID); behind {
		t.Fatal("the loaded room stands behind the head it was loaded at")
	}
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 2)
}

// A browser append takes in the version it stored, so the next edit of the same room is stored
// too. Without that advance the room's second edit is refused, the room fails, and every edit
// after a reload meets the same refusal.
func TestARoomWritesAgainAfterItsOwnBrowserAppend(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	editAsConnectedPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "alice"}, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, service, artifactID)
	requireRoomStillWrites(t, service, artifactID, 2)
}

// A committed live write is appended outside the room and published into it, so the publish is
// what takes its version in (publishLiveUpdate). Without that advance a room left behind by its
// own process's write refuses the next browser edit.
func TestARoomWritesAgainAfterACommittedWritePublishesIntoIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Agent's paragraph.", After: "Second."},
	}, model.Actor{Kind: "session", ID: "agent-session"}, nil); err != nil {
		t.Fatalf("an agent's edit into the resident room: %v", err)
	}
	requireRoomStillWrites(t, service, artifactID, 1)
}

// A settlement that stamps block ids writes that repair into its own room and appends it in its
// transaction, so the room takes the version it is stored at there (settleRoomWithin). Without
// that advance every settlement that repairs anything would leave its own room unable to write.
func TestARoomWritesAgainAfterItsSettlementAppendsARepair(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "A paragraph with no block id."))
	before := storedHead(t, service.store, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	if head := storedHead(t, service.store, artifactID); head <= before {
		t.Fatalf("the document's head is %d after the settlement, want one past %d: it stamped no block id, so no repair was appended", head, before)
	}
	requireRoomStillWrites(t, service, artifactID, 1)
}

// A settlement superseded after it wrote its repair commits that repair alone, on its own path,
// and the room takes that version in there too.
func TestARoomWritesAgainAfterASupersededSettlementAppendsItsRepair(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "A paragraph with no block id."))
	// The settlement is superseded between its read and its supersession check, as a write
	// opening on the room supersedes it: it then commits the repair it has already written into
	// the room and writes no version.
	superseded := false
	service.afterSettleRead = func(room string) {
		if room != artifactID || superseded {
			return
		}
		superseded = true
		state := service.lockState(room)
		state.roomGeneration++
		service.unlockState(room, state)
	}
	versions := latestVersionNumber(t, service, artifactID)
	before := storedHead(t, service.store, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	service.afterSettleRead = nil
	if !superseded {
		t.Fatal("the settlement never reached the window its supersession was arranged in")
	}
	if head := storedHead(t, service.store, artifactID); head <= before {
		t.Fatalf("the document's head is %d after the superseded settlement, want one past %d: it appended no repair", head, before)
	}
	if now := latestVersionNumber(t, service, artifactID); now != versions {
		t.Fatalf("the superseded settlement wrote version %d, want none past %d", now, versions)
	}
	requireRoomStillWrites(t, service, artifactID, 1)
}

// A cold write is outside the gate: it holds no room, so nothing says the document it was made
// on is behind the head. A browser that opens the document behind the write's store read and
// types moves the head under it, and the write's own append still lands - its update is a Yjs
// update, which commutes with the browser's, and the room that holds the document after the
// publish holds both edits. Gated on the head its store read saw, the write answered 503 for a
// conflict Yjs resolves (coldFork, appendingCursor).
func TestAColdWriteAppendsAfterAnotherWriterMovedTheHead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	closeDocumentRoom(t, service, artifactID)
	read := storedHead(t, service.store, artifactID)

	// The browser opens the document behind the write's store read and types, and its append is
	// durable before the write appends: the head the write meets is past the one it forked from.
	opened := false
	service.afterForkRead = func(room string) {
		if room != artifactID || opened || service.srv.GetDoc(artifactID) != nil {
			return
		}
		opened = true
		editAsConnectedPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "alice"},
			appendBlocks(t, "Alice's paragraph.\n"))
		waitForLandedAppends(t, service, artifactID)
	}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Agent's paragraph.", After: "Second."},
	}, model.Actor{Kind: "session", ID: "agent-session"}, nil); err != nil {
		t.Fatalf("a cold write whose head moved under it: %v", err)
	}
	if !opened {
		t.Fatal("the write never read the store, so no head moved under it")
	}
	if head := storedHead(t, service.store, artifactID); head <= read+1 {
		t.Fatalf("the document's head is %d, want it past %d: the browser's append and the write's own", head, read+1)
	}
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"Alice's paragraph.", "Agent's paragraph."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document after the cold write = %q, want it to hold %q", stored, want)
		}
	}
}

// A browser's append waits on the document's advisory lock while a transaction of this process
// writes the same document, and takes that lock the instant the commit drops it. The commit
// records its versions on the room's cursor before it commits (takeRoomCursors), so the append
// finds a cursor at the head that commit wrote and is stored. Recorded after the commit instead,
// they left the room one version behind the head it reads under that lock: the append was refused
// (ErrRoomBehindHead), which failed the room under the browser whose edit it was and dropped
// every update the room still owed the store with it.
func TestABrowserAppendQueuedBehindThisProcessesWriteIsNotRefused(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	appended := false
	service.afterLiveCommit = func(room string) {
		if room != artifactID || appended {
			return
		}
		appended = true
		// The lock's next holder, in the window it takes it in: the write's version is durable
		// and its publish has not run.
		requireRoomStillWrites(t, service, artifactID, 1)
	}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Agent's paragraph.", After: "Second."},
	}, model.Actor{Kind: "session", ID: "agent-session"}, nil); err != nil {
		t.Fatalf("an agent's edit into the resident room: %v", err)
	}
	if !appended {
		t.Fatal("nothing appended between the write's commit and its publish, so nothing here is tested")
	}
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"Agent's paragraph.", "Writing again as peer 1."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document = %q, want it to hold %q", stored, want)
		}
	}
}

// A browser's append raises the document's head when it commits and takes that version into its
// room's cursor a moment later, both inside the document's advisory lock
// (PgVersioned.appendUpdate). A write that reads the cursor and the head without that lock lands
// between the two and reads a healthy room as one another process left behind, so it forks from
// the stored document rather than from the room (forkLive). The store of a room still writing
// holds less than that room - what its persistence worker has not appended yet - and takes what
// it does hold in the order the room's update observers fired, not the order the room applied
// them (consumeUpdateClass), so a write that forks it can read a state the document never held:
// an update whose deletes landed without the inserts they replaced leaves a tree with no block in
// it at all, which every write starting from it refuses (ErrDocOutsideSchema). That is the
// intermittent failure of TestTheUpdateObserverNeverRendersAWriteHalfWay. Here the room holds a
// second browser edit the store has not taken, so a fork of the store is one the write can see is
// not the whole document.
func TestAWriteDoesNotForkFromTheStoreInsideAnAppendsCursorWindow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	appends, ok := service.persistence.(*PgVersioned)
	if !ok {
		t.Fatalf("the service appends through %T, which holds no window between a commit and its cursor", service.persistence)
	}
	committed, release := make(chan struct{}), make(chan struct{})
	var opened, closed sync.Once
	releaseWindow := func() { closed.Do(func() { close(release) }) }
	// Before the service's own cleanup, which cannot shut down while an append is held here.
	t.Cleanup(releaseWindow)
	appends.afterAppendCommit = func(room string, _ persistence.Version) {
		held := false
		if room == artifactID {
			opened.Do(func() { held = true })
		}
		if !held {
			return
		}
		close(committed)
		<-release
	}

	// The browser's first edit commits its append and holds the window open from inside it. Its
	// second reaches the room while that append is still there, so the room holds a paragraph the
	// store has not taken.
	if err := browserStyleWrite(service, artifactID, appendBlocks(t, "Alpha paragraph.\n"), make(chan struct{})); err != nil {
		t.Fatalf("the browser's first edit: %v", err)
	}
	select {
	case <-committed:
	case <-time.After(roomWait(t)):
		t.Fatal("the browser's append never committed, so no window opened")
	}
	if err := browserStyleWrite(service, artifactID, appendBlocks(t, "Bravo paragraph.\n"), make(chan struct{})); err != nil {
		t.Fatalf("the browser's second edit: %v", err)
	}

	var cold bool
	var forked string
	written := make(chan error, 1)
	go func() {
		written <- joinedWrite(service, func(ctx context.Context) error {
			if err := service.ProjectMark(ctx, artifactID, "mark-in-the-window", MarkRecord{
				Kind: "comment", By: "user:bob", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Text: "note",
			}, model.Actor{Kind: "user", ID: "bob"}); err != nil {
				return err
			}
			write := joinedLiveWrite(ctx, artifactID)
			cold = write.cold
			tree, err := treeOf(write.fork)
			if err != nil {
				return err
			}
			forked, err = documentMarkdown(tree)
			return err
		})
	}()
	// The write has made its fork's decision once it waits on the document's advisory lock: for
	// the confirmation that decision needs, or, having taken it without one, for its own append.
	waitForLockWait(t, context.Background(), service.store, "%pg_advisory_xact_lock%", written)
	releaseWindow()
	if err := <-written; err != nil {
		t.Fatalf("a mark projection made inside the window: %v", err)
	}
	if cold {
		t.Error("the write forked the stored document while its room held every update the store did")
	}
	if !strings.Contains(forked, "Bravo paragraph.") {
		t.Errorf("the write forked %q, want it to hold the paragraph only its room held", forked)
	}
}

// A room that loads while this process's own write commits is not born behind the head. ygo
// publishes no document for a room that is still loading (Server.GetDoc), so the commit finds no
// cursor of that room to record its version on and the publish that would is behind the load: the
// version goes to the record the load leaves its instance (roomLoad, tookResidentVersion), and the
// room the load starts stands at the head. Born one behind, that room is refused its browser's
// every append, and the reload each refusal forces can lose the same race again.
func TestARoomLoadingWhileThisProcessesWriteCommitsStandsAtTheHead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	closeDocumentRoom(t, service, artifactID)

	// The load is held where the window is: ygo's LoadDoc has read the store and the head it
	// stands at, and the hook that starts the room's instance has not run.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == artifactID {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return load(ctx, room, doc)
	}
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	select {
	case <-entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's load never reached the window it is held in")
	}

	read := storedHead(t, service.store, artifactID)
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
		t.Fatalf("apply the write while the room loads: %v", err)
	}
	if write := joinedLiveWrite(joined, artifactID); write == nil || !write.cold {
		t.Fatal("the write forked the loading room, so nothing here is tested")
	}
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit the write while the room loads: %v", err)
	}
	if head := storedHead(t, service.store, artifactID); head != read+1 {
		t.Fatalf("the document's head is %d after the write, want %d: the load read %d", head, read+1, read)
	}

	close(release)
	if err := <-loaded; err != nil {
		t.Fatalf("load the room while the write committed: %v", err)
	}
	if roomBehind(t, service, artifactID) {
		t.Fatal("the room that loaded while this process's write committed stands behind the head")
	}
	ledger.publish()
	// The browser's own append on that room, which is what standing at the head is for.
	requireRoomStillWrites(t, service, artifactID, 1)
	stored := storedMarkdown(t, service.store, artifactID)
	for _, want := range []string{"Agent's paragraph.", "Writing again as peer 1."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document = %q, want it to hold %q", stored, want)
		}
	}
}

// A room that loads while a repair of this process's commits is the opposite case, and it is not
// born at the head. A repair - the block-id backfill's stamp here, settlement's on the same
// shape - is written into one room instance before it is appended (applySuppressed), and nothing
// replays it into a room that loaded since: the replacement reads the store, which did not hold
// the repair when it read. So that room genuinely does not hold the appended version, it stands
// behind the head, and the gate has to refuse its next durable write - were it credited with the
// version (tookResidentVersion, which a live write's commit does call because its publish then
// applies the update to the room it reaches), this process would version text it never saw.
// Standing behind is not where it ends: the refusal fails the room and the reload reads the
// repair from the store, which is the recovery TestARoomBehindTheHeadIsReloadedAndWritesAgain
// pins for the two-process case.
//
// The window is the backfill's alone: it holds no room and no document lock between the stamp
// and the append that stores it, so a failure of that room there evicts it and the document
// reloads under the append. A settlement cannot reach this window - its transaction holds the
// document's advisory lock from before it reads the room until it commits, so a failed room's
// eviction, which compacts under that lock, cannot finish, and allowInject holds every loader
// behind that eviction.
func TestARoomLoadingWhileAStampCommitsStandsBehindTheHeadAndReloads(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	// failingBrowserAppendStore fails nothing here; it is the hook that runs just before the
	// backfill's append, while the backfill holds no lock of its own.
	appends := &failingBrowserAppendStore{VersionedStore: NewPgVersioned(database)}
	service := New(Deps{Store: database, Persistence: appends, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	alignLatestVersionWithUpdates(t, service, artifactID)

	// The reload is held where the window is: ygo's LoadDoc has read the store and the head it
	// stands at, and the hook that starts the room's instance has not run. Only the reload the
	// backfill's own window arms is held - the backfill loads the room itself to stamp it.
	entered, release := make(chan struct{}), make(chan struct{})
	var holding sync.Once
	var hold atomic.Bool
	loadDocument := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == artifactID && hold.Load() {
			holding.Do(func() {
				close(entered)
				<-release
			})
		}
		return loadDocument(ctx, room, doc)
	}

	var read int64
	reloaded := make(chan error, 1)
	reload := func() {
		read = storedHead(t, service.store, artifactID)
		hold.Store(true)
		service.failRoom(artifactID, errors.New("test: the stamped room failed under the backfill"))
		if err := service.awaitRoomRecovery(context.Background(), artifactID); err != nil {
			t.Errorf("evict the room the backfill stamped: %v", err)
			return
		}
		go func() { reloaded <- service.warmLiveDocument(context.Background(), artifactID) }()
		select {
		case <-entered:
		case err := <-reloaded:
			t.Errorf("the reload returned without reaching the load window: %v", err)
		case <-time.After(roomWait(t)):
			t.Error("the reload never reached the window it is held in")
		}
	}
	appends.beforeTx.Store(&reload)
	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill the document's block ids: %v", err)
	}
	if len(reports) != 1 || reports[0].Err != nil || reports[0].Stamped != 1 {
		t.Fatalf("backfill reports = %#v, want the one document, stamping one block", reports)
	}
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatalf("reload the room while the stamp committed: %v", err)
	}
	head := storedHead(t, service.store, artifactID)
	if head != read+1 {
		t.Fatalf("the document's head is %d after the stamp, want %d: the load read %d", head, read+1, read)
	}

	// What makes standing behind the head right here: the room that loaded has none of the stamp.
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the room that loaded while the stamp committed holds the stamp, so nothing here is tested")
	}
	live := service.srv.GetDoc(artifactID)
	if at := service.cursorOf(live).heldThrough(); at != read {
		t.Fatalf("the room that loaded while the stamp committed holds the document through update %d, want the head it read, %d", at, read)
	}

	// And the recovery that ends it: the browser's edit is refused, which fails the room; the
	// reload reads the stamp from the store and the resent edit lands on it.
	editAsBrowserPeer(t, service, artifactID, 1, model.Actor{Kind: "user", ID: "alice"},
		appendBlocks(t, "Alice types on the room that loaded without the stamp.\n"))
	if behind := roomBehind(t, service, artifactID); behind {
		t.Fatal("the room that replaced the one behind the head stands behind it too")
	}
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
		t.Fatalf("the reloaded room holds %d unstamped block(s), want the stamp the backfill stored", repairs)
	}
	if stored := storedMarkdown(t, service.store, artifactID); !strings.Contains(stored, "Alice types on the room that loaded without the stamp.") {
		t.Fatalf("the stored document = %q, want it to hold the edit the room resent", stored)
	}
	requireRoomStillWrites(t, service, artifactID, 2)
}

// A room that does stand behind the head - the other process appended what this one's room never
// took - recovers. Its browser's append is refused, which fails the room; the failure's eviction
// reloads the document at the head, the browser's provider sends its edit to the room that
// replaced it, and that room writes again, this time with nothing refused. Recovery is the whole
// answer to a room behind the head, so it has to end somewhere: a room refused, failed, reloaded
// and refused again is the loop INFRA-66's reproduction fell into, and a room that cannot be born
// behind (roomLoad) is what keeps the reload out of it.
func TestARoomBehindTheHeadIsReloadedAndWritesAgain(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 1, model.Actor{Kind: "user", ID: "bob"}, "Bob writes on the other process.\n")
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 2, model.Actor{Kind: "user", ID: "alice"},
		appendBlocks(t, "Alice types on the room behind the head.\n"))
	if behind := roomBehind(t, pair.taskA, pair.artifactID); behind {
		t.Fatal("the room that replaced the one behind the head stands behind it too")
	}
	stored := storedMarkdown(t, pair.database, pair.artifactID)
	for _, want := range []string{"Bob writes on the other process.", "Alice types on the room behind the head."} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored document after the reload = %q, want it to hold %q", stored, want)
		}
	}
	requireRoomStillWrites(t, pair.taskA, pair.artifactID, 3)
}
