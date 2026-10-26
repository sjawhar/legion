package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

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
func (pair processPair) leaveTaskABehind(t *testing.T, connection uint64, peer model.Actor, markdown string) {
	t.Helper()
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, connection, peer, appendBlocks(t, markdown))
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
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 1, bob,
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
	editAsConnectedPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
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
		state.roomGeneration = service.nextRoomGeneration()
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
