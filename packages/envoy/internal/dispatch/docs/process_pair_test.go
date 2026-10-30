package docs

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The fixture every test of two envoy-dispatch processes on one database runs on: the rooms
// behind the durable head (room_behind_head_test.go), the catch-up (room_catch_up_test.go), the
// room queries (room_query_test.go), the awareness relay (awareness_relay_test.go) and the
// conditional edit queue (conditional_edit_queue_test.go).

// processPair is one document served by two processes: taskA, whose rooms the tests leave behind
// the head, and taskB, the other process, on a pool of its own (storetest.SecondProcess) as a
// second envoy-dispatch task has. serverA and serverB are their document websockets, which the
// catch-up tests (room_catch_up_test.go) and the awareness tests (awareness_relay_test.go)
// connect browsers to. Task B's room appends pass gateB, which is open until a test holds it
// (gatedAppends), so an update task B's room applied stays unstored for as long as a test needs
// (room_query_test.go). Neither process runs room sync; a test that needs it runs it
// (processPair.runRoomSync, runCatchUps).
type processPair struct {
	database   *store.Store
	artifactID string
	taskA      *Service
	serverA    *httptest.Server
	taskB      *Service
	serverB    *httptest.Server
	gateB      *gatedAppends
}

// newProcessPair is the pair on a document of two paragraphs, settled: where most of the tests
// over the pair start.
func newProcessPair(t *testing.T) processPair {
	t.Helper()
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	return pair
}

// newSeededProcessPair is the pair on a document seeded with markdown and not settled, which
// leaves task A holding no room of its own: where the room-query tests start.
func newSeededProcessPair(t *testing.T, markdown string) processPair {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, serverA := newTestServiceInstance(t, database)
	taskB, gateB := newGatedServiceInstance(t, storetest.SecondProcess(t, database))
	serverB := newTestServer(t, taskB)
	seedServiceText(t, taskA, artifactID, markdown)
	return processPair{
		database: database, artifactID: artifactID,
		taskA: taskA, serverA: serverA,
		taskB: taskB, serverB: serverB, gateB: gateB,
	}
}

// runRoomSync runs both processes' room sync, as cmd/dispatch does: each process's listener makes
// it a member (roomSyncMembers) and carries what the other announces.
func (pair processPair) runRoomSync(t *testing.T) {
	t.Helper()
	runRoomSync(t, newRoomSync(pair.taskA))
	runRoomSync(t, newRoomSync(pair.taskB))
}

// openReader connects login's reader to the document on server and waits for the document.
func (pair processPair) openReader(t *testing.T, server *httptest.Server, login string) *docstest.Peer {
	t.Helper()
	reader := connectPeer(t, server.URL, pair.artifactID, login)
	if err := reader.WaitForText(peerMarkdown, storedMarkdown(t, pair.database, pair.artifactID), roomWait(t)); err != nil {
		t.Fatalf("%s's reader never received the document: %v", login, err)
	}
	return reader
}

// heldEditOnB holds task B's appends and has actor's browser on task B make edit, which task B's
// room applies and then waits to store at the gate. actor stays connected to task B's room.
func (pair processPair) heldEditOnB(t *testing.T, connection uint64, actor model.Actor, edit func(tree *pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	pair.gateB.hold()
	pair.taskB.addConnection(pair.artifactID, connection, actor)
	editAsPeer(t, pair.taskB, pair.artifactID, edit)
	waitFor(t, roomWait(t), "task B's append of the edit to wait at the gate", func() bool { return pair.gateB.waiting.Load() == 1 })
}

// leaveTaskABehind loads task A's room, then has a browser on task B edit the document: task A's
// room holds every update through the head it loaded and not that one, which is the state every
// derived write's gate refuses to write from until a catch-up takes the edit in.
func (pair processPair) leaveTaskABehind(t *testing.T, connection uint64, peer model.Actor, markdown string) {
	t.Helper()
	pair.leaveTaskABehindWith(t, connection, peer, appendBlocks(t, markdown))
}

// leaveTaskABehindWith is leaveTaskABehind for a browser edit other than an appended block.
func (pair processPair) leaveTaskABehindWith(t *testing.T, connection uint64, peer model.Actor, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, connection, peer, edit)
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if behind := roomBehind(t, pair.taskA, pair.artifactID); !behind {
		t.Fatal("task A's room stands at the head after task B's edit, so nothing here is tested")
	}
}

// replaceTaskAsHistory loads task A's room and then replaces the document's stored history under
// it, as a rebuild on the other process does (replaceStoredHistory), with the seeded text and Bob's
// paragraph: no stored row the room could take in leads from what it holds to the new head, so it
// is closed and reloaded rather than caught up.
func (pair processPair) replaceTaskAsHistory(t *testing.T) {
	t.Helper()
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	replaceStoredHistory(t, pair.database, pair.artifactID, "First.\n\nSecond.\n\nBob's paragraph.\n")
}
