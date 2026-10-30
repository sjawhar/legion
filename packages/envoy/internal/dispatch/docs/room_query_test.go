package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Room queries (room_query.go) are how one process asks every other one about a document room: a
// flush waits for the other process's room to store what it applied, and a share hands over what
// it holds and has not stored, with who is connected there. The tests here run two
// services on one database, each on a pool of its own and each with its room sync running, as two
// envoy-dispatch processes do. Task B's room appends pass a gate the test holds (gatedAppends), so
// an update its room applied stays unstored for as long as a test needs.

// newQueryProcesses is the two-process pair (processPair, process_pair_test.go) the room
// queries are asked over: the lost-edit document, both processes' room sync running, and task B's
// room resident, while task A holds none.
func newQueryProcesses(t *testing.T) processPair {
	t.Helper()
	pair := newSeededProcessPair(t, lostEditDocument)
	pair.runRoomSync(t)
	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	return pair
}

// captureWarnings sends every log line at WARN and above to the returned log until the test ends.
func captureWarnings(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// storedDocumentWith is the stored document's rendering with updates applied over it.
func storedDocumentWith(t *testing.T, database *store.Store, artifactID string, updates ...[]byte) string {
	t.Helper()
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	doc := newDocumentCopy()
	for _, update := range append([][]byte{loaded.Update}, updates...) {
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("apply an update to the stored document: %v", err)
		}
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return markdown
}

// A flush is answered only once the other process's room has stored what it applied: task B's
// room applied bob's edit and holds its append at the gate, and task A's flush returns once the
// gate opens, with the edit stored and task B's answer saying it holds the room. It names no
// actors: only a share's asker reads them.
func TestAFlushWaitsForTheOtherProcessesHeldAppend(t *testing.T) {
	pair := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	pair.heldEditOnB(t, 1, bob, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."))
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "by bob") {
		t.Fatalf("the store holds bob's edit %q while its append is held, so nothing here is tested", stored)
	}

	const held = 200 * time.Millisecond
	released := time.AfterFunc(held, pair.gateB.release)
	defer released.Stop()
	started := time.Now()
	answers, asked, err := pair.taskA.roomQueries.ask(context.Background(), pair.database.Pool, pair.artifactID, roomQueryFlush)
	waited := time.Since(started)
	if err != nil || !asked {
		t.Fatalf("task A's flush = asked %t, %v; want task B asked", asked, err)
	}
	if waited < held-20*time.Millisecond {
		t.Fatalf("task A's flush returned after %v, before task B's held append was let go at %v: it did not wait for the append", waited, held)
	}
	if waited >= roomQueryWait {
		t.Fatalf("task A's flush took %v, the whole wait: task B's answer never came", waited)
	}
	if len(answers) != 1 || answers[0].process != pair.taskB.ProcessID() || !answers[0].holds {
		t.Fatalf("task A's flush answers = %+v, want task B's one, holding the room", answers)
	}
	if len(answers[0].actors) != 0 {
		t.Fatalf("task B's flush answer names actors %+v, which no asker of a flush reads", answers[0].actors)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); !strings.Contains(stored, "by bob") {
		t.Fatalf("the store reads %q once task B answered the flush, without bob's edit", stored)
	}
}

// A share is answered at once, appending nothing, with the updates the other process's room holds
// and the store does not - here bob's paragraph, whose append is under way and held at the gate -
// and an answer that large crosses in several notifications and is put back together whole.
func TestAShareReturnsTheOtherProcessesInFlightAppend(t *testing.T) {
	pair := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	paragraph := "Bob's long paragraph " + strings.Repeat("with many words ", 1200) + "ends."
	pair.heldEditOnB(t, 1, bob, appendBlocks(t, paragraph+"\n"))

	started := time.Now()
	answers, asked, err := pair.taskA.roomQueries.ask(context.Background(), pair.database.Pool, pair.artifactID, roomQueryShare)
	waited := time.Since(started)
	if err != nil || !asked {
		t.Fatalf("task A's share = asked %t, %v; want task B asked", asked, err)
	}
	if waited >= roomQueryWait {
		t.Fatalf("task A's share took %v, the whole wait: task B's answer never came whole", waited)
	}
	if len(answers) != 1 || !answers[0].holds || answers[0].tooLarge {
		t.Fatalf("task A's share answers = %+v, want task B's one, holding the room", answers)
	}
	if len(answers[0].updates) <= roomSyncPayloadLimit {
		t.Fatalf("task B shared %d bytes, which fit one notification; the test wants an answer in parts", len(answers[0].updates))
	}
	if !reflect.DeepEqual(answers[0].actors, []model.Actor{bob}) {
		t.Fatalf("task B answered actors %+v, want bob", answers[0].actors)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "Bob's long paragraph") {
		t.Fatalf("the store holds bob's paragraph while its append is held: the share appended it")
	}
	if shared := storedDocumentWith(t, pair.database, pair.artifactID, answers[0].updates); !strings.Contains(shared, paragraph) {
		t.Fatalf("the stored document with task B's shared updates reads %q, without bob's paragraph", shared)
	}
	pair.gateB.release()
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Count(stored, "Bob's long paragraph") != 1 {
		t.Fatalf("once the gate opened the store reads %q; want bob's paragraph stored once, by task B's own append", stored)
	}
}

// What a share holds past roomShareLimit is not sent: the answer says too_large, carries no
// updates, and the asker logs a WARN and judges without them.
func TestAShareTooLargeForOneQueryAnswersTooLarge(t *testing.T) {
	pair := newQueryProcesses(t)
	pair.heldEditOnB(t, 1, model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, strings.Repeat("Bob's huge paragraph. ", 4000)+"\n"))
	logs := captureWarnings(t)

	answers, _, err := pair.taskA.roomQueries.ask(context.Background(), pair.database.Pool, pair.artifactID, roomQueryShare)
	if err != nil {
		t.Fatalf("task A's share: %v", err)
	}
	if len(answers) != 1 || !answers[0].holds || !answers[0].tooLarge || len(answers[0].updates) != 0 {
		t.Fatalf("task A's share answers = %+v, want task B's one, holding the room, too large and with no updates", answers)
	}
	if warned := logs.Lines("more not-yet-stored updates than one query shares"); !strings.Contains(warned, pair.taskB.ProcessID()) {
		t.Fatalf("task A logged no WARN naming task B for its too-large share; logged:\n%s", logs.String())
	}
}

// A member that never answers - an older build, or a process that has just died - costs a query
// the wait and no more, and a WARN naming it. The silent member here holds the listener
// connection's name, which is all membership reads, and listens to nothing.
func TestAMemberThatNeverAnswersCostsTheWaitAndAWarn(t *testing.T) {
	pair := newQueryProcesses(t)
	silent := uuid.NewString()
	conn, err := pair.database.Pool.Dedicated(context.Background(), ListenerApplicationPrefix+silent)
	if err != nil {
		t.Fatalf("connect the silent member: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	logs := captureWarnings(t)

	for _, kind := range []string{roomQueryHolds, roomQueryFlush, roomQueryShare} {
		started := time.Now()
		answers, asked, err := pair.taskA.roomQueries.ask(context.Background(), pair.database.Pool, pair.artifactID, kind)
		waited := time.Since(started)
		if err != nil || !asked {
			t.Fatalf("%s: asked %t, %v; want the members asked", kind, asked, err)
		}
		if waited < roomQueryWait || waited > roomQueryWait+300*time.Millisecond {
			t.Fatalf("%s took %v with a member that never answers, want the %v wait and no more", kind, waited, roomQueryWait)
		}
		if len(answers) != 1 || answers[0].process != pair.taskB.ProcessID() {
			t.Fatalf("%s answers = %+v, want task B's alone", kind, answers)
		}
	}
	if warned := logs.Lines("did not answer a document room query"); strings.Count(warned, silent) != 3 {
		t.Fatalf("task A logged %q; want a WARN naming the silent member for each of its three queries", warned)
	}
}

// A flush is never sent while the asker holds the document's advisory lock, which every other
// process's append waits on: it would wait out every member. The query reads the lock from the
// database, skips the flush at once and logs it at ERROR.
func TestAFlushIsNeverSentUnderTheDocumentsAdvisoryLock(t *testing.T) {
	pair := newQueryProcesses(t)
	logs := captureWarnings(t)
	ctx := context.Background()
	tx, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, locked, err := readRoomQueryMembers(ctx, tx, pair.artifactID, true); err != nil || locked {
		t.Fatalf("before the lock, the members' read says locked %t, %v; want unlocked", locked, err)
	}
	if err := lockDocumentRoom(ctx, tx, pair.artifactID); err != nil {
		t.Fatalf("take the document's advisory lock: %v", err)
	}
	if _, locked, err := readRoomQueryMembers(ctx, tx, pair.artifactID, true); err != nil || !locked {
		t.Fatalf("under the lock, the members' read says locked %t, %v; want locked", locked, err)
	}
	if _, locked, err := readRoomQueryMembers(ctx, tx, uuid.NewString(), true); err != nil || locked {
		t.Fatalf("another document's lock read says locked %t, %v; want unlocked", locked, err)
	}

	started := time.Now()
	answers, _, err := pair.taskA.roomQueries.ask(ctx, tx, pair.artifactID, roomQueryFlush)
	if err != nil || len(answers) != 0 {
		t.Fatalf("a flush under the lock = %+v, %v; want it skipped", answers, err)
	}
	if waited := time.Since(started); waited >= roomQueryWait/2 {
		t.Fatalf("a flush under the lock took %v; want it skipped at once", waited)
	}
	if logged := logs.Lines("level=ERROR"); !strings.Contains(logged, "flush was asked for while holding") {
		t.Fatalf("a flush under the lock logged no ERROR; logged:\n%s", logs.String())
	}
}

// An agent's write on task A inserts into a paragraph that bob, on task B, deletes while the
// write is open: task A's room never sees the delete, and task B's append of it waits at the gate.
// The write's version capture shares task B's unstored updates into a scratch copy of its fork and
// refuses the write as lost, naming everyone connected on either process - alice on task A, bob
// (in two tabs) on task B - once each, and not the writer, connected on task B too. The refused
// write leaves bob's delete to be stored, once the gate opens.
func TestALostWriteNamesTheParticipantsOfEveryProcess(t *testing.T) {
	pair := newQueryProcesses(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	pair.taskA.addConnection(pair.artifactID, 1, alice)
	pair.taskB.addConnection(pair.artifactID, 2, bob)
	pair.taskB.addConnection(pair.artifactID, 3, lostEditAgent)

	edit := startEditInFlight(t, pair.taskA, pair.artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
	})
	pair.heldEditOnB(t, 4, bob, withoutTopLevelBlock("Alpha paragraph."))

	_, err := pair.taskA.NamedVersion(edit.ctx, pair.artifactID, "the agent's version", lostEditAgent)
	var lost *ErrEditLost
	if !errors.As(err, &lost) {
		t.Fatalf("the agent's version after bob deleted its paragraph on task B = %v, want the write refused as lost", err)
	}
	if !reflect.DeepEqual(lost.Ops, []int{0}) {
		t.Fatalf("lost operations = %v, want [0]", lost.Ops)
	}
	if want := []model.Actor{alice, bob}; !reflect.DeepEqual(lost.Participants, want) {
		t.Fatalf("participants = %+v, want %+v: the connected actors of both processes, once each, without the writer", lost.Participants, want)
	}

	edit.ledger.Discard()
	if err := edit.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("roll back the refused write: %v", err)
	}
	pair.gateB.release()
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "Alpha") {
		t.Fatalf("the store reads %q once the refused write rolled back and task B's gate opened; want bob's delete stored", stored)
	}
}

// Before a write forks, every other process holding the document's room stores what it applied:
// bob deleted the paragraph on task B, whose append is held, and an agent's edit of that paragraph
// through task A, which holds no room, waits for the append rather than forking a store without
// the delete, and is refused as an edit whose target is gone.
func TestALiveWriteForksOnlyOnceTheOtherProcessHasStoredItsRoom(t *testing.T) {
	pair := newQueryProcesses(t)
	pair.heldEditOnB(t, 1, model.Actor{Kind: "user", ID: "bob"}, withoutTopLevelBlock("Alpha paragraph."))
	released := time.AfterFunc(150*time.Millisecond, pair.gateB.release)
	defer released.Stop()
	_, err := joinedApplyOps(pair.taskA, pair.artifactID, []model.EditOp{{Op: "replace", Find: "Alpha paragraph.", With: "Alpha edited."}}, lostEditAgent, nil)
	var gone *ErrQuoteNotFound
	if !errors.As(err, &gone) {
		t.Fatalf("the agent's edit through task A of the paragraph bob deleted on task B = %v, want ErrQuoteNotFound: the write forked a store without the delete", err)
	}
	if pair.taskA.roomHeld(pair.artifactID) {
		t.Fatal("the refused write left a room of the document on task A")
	}
}

// A write's verdict after its commit is judged on the document at the stored head: here the write
// went through task A, which holds no room, and bob deleted its paragraph on task B between the
// write's commit and its publish, with task B's append of the delete held. The second flush waits
// for that append, and the write's lost operations name the one bob removed. The write loads no
// room on task A, publishing into none.
func TestAPublishedWritesLossIsJudgedAtTheStoredHeadAfterTheOtherProcessFlushes(t *testing.T) {
	pair := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	var once sync.Once
	pair.taskA.afterLiveCommit = func(string) {
		once.Do(func() {
			pair.heldEditOnB(t, 1, bob, withoutFirstTopLevelBlock)
			time.AfterFunc(150*time.Millisecond, pair.gateB.release)
		})
	}
	lost, verdict := committedEdit(t, pair.taskA, pair.artifactID, model.EditOp{Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph, extended."})
	if !verdict || !reflect.DeepEqual(lost, []int{0}) {
		t.Fatalf("the published write's lost operations = %v (verdict %t), want [0]: bob's delete on task B removed its text", lost, verdict)
	}
	if pair.taskA.roomHeld(pair.artifactID) {
		t.Fatal("the cold write's publish loaded a room on task A")
	}
}

// committedEdit commits op as the agent's edit of artifactID through service and returns the
// published write's lost operations and whether it reached a verdict (Ledger.LostOps).
func committedEdit(t *testing.T, service *Service, artifactID string, op model.EditOp) (lost []int, verdict bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{op}, lostEditAgent, nil); err != nil {
		t.Fatalf("the agent's edit: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the agent's edit: %v", err)
	}
	return ledger.LostOps(artifactID)
}

// withoutFirstTopLevelBlock deletes the document's first top-level block, whatever its text: a
// browser's paragraph delete, made in a room that may or may not have taken in a write to it yet.
func withoutFirstTopLevelBlock(tree *pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: tree.Type, Attrs: tree.Attrs, Children: slices.Clone(tree.Children[1:])}
}

// A cold write's kept fork is brought up to the stored head once its transaction holds the
// document's lock: task B's browser stored an edit between the write's read of the store and its
// first append, and the version the write renders holds both.
func TestAColdWritesVersionHoldsWhatTheOtherProcessStoredBeforeItsLock(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, lostEditDocument)
	var once sync.Once
	taskA.afterForkRead = func(string) {
		once.Do(func() {
			editAsBrowserPeer(t, taskB, artifactID, 1, model.Actor{Kind: "user", ID: "bob"}, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."))
			waitForLandedAppends(t, taskB, artifactID)
		})
	}
	if taskA.roomHeld(artifactID) {
		t.Fatal("task A holds a room, so the write would not be cold")
	}
	edit := startEditInFlight(t, taskA, artifactID, model.EditOp{Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph, by the agent."})
	if _, err := taskA.NamedVersion(edit.ctx, artifactID, "both", lostEditAgent); err != nil {
		t.Fatalf("the agent's version: %v", err)
	}
	if err := edit.ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit the agent's write: %v", err)
	}
	markdown, cursor := latestVersionOf(t, database, artifactID)
	if !strings.Contains(markdown, "by the agent") || !strings.Contains(markdown, "by bob") {
		t.Fatalf("the cold write's version reads %q; want the agent's edit and bob's, stored before the write's lock", markdown)
	}
	if head := storedHead(t, database, artifactID); cursor != head {
		t.Fatalf("the version records update %d, the stored head is %d", cursor, head)
	}
}

// An answer whose notification would pass the payload limit is split into numbered parts, the
// actors and then the updates spread across them, the last flagged; put back together, it is the
// answer that was split.
func TestARoomAnswerTooLargeForOneNotificationIsSplitIntoParts(t *testing.T) {
	actors := make([]model.Actor, 0, 300)
	for index := range 300 {
		actors = append(actors, model.Actor{Kind: "user", ID: fmt.Sprintf("reader-with-a-long-login-%03d", index)})
	}
	updates := []byte(strings.Repeat("an update's bytes ", 2000))
	payloads, err := roomAnswerPayloads(roomAnswerPart{ID: "query", Process: "member", Holds: true}, actors, base64.StdEncoding.EncodeToString(updates))
	if err != nil {
		t.Fatalf("split the answer: %v", err)
	}
	if len(payloads) < 3 {
		t.Fatalf("the answer went in %d notifications, want several", len(payloads))
	}
	parts := &roomAnswerParts{parts: make(map[int]roomAnswerPart), last: -1}
	// Parts may arrive in any order.
	for _, payload := range slices.Backward(payloads) {
		if len(payload) > roomSyncPayloadLimit {
			t.Fatalf("a part of %d bytes passes the %d-byte limit", len(payload), roomSyncPayloadLimit)
		}
		var part roomAnswerPart
		if err := json.Unmarshal([]byte(payload), &part); err != nil {
			t.Fatalf("decode a part: %v", err)
		}
		parts.parts[part.Part] = part
		if part.Last {
			parts.last = part.Part
		}
	}
	if parts.last != len(payloads)-1 {
		t.Fatalf("the last part is numbered %d, want %d", parts.last, len(payloads)-1)
	}
	answer, err := parts.assemble()
	if err != nil {
		t.Fatalf("assemble the answer: %v", err)
	}
	if !answer.holds || answer.process != "member" || !reflect.DeepEqual(answer.actors, actors) || string(answer.updates) != string(updates) {
		t.Fatalf("the assembled answer differs from the one split: holds %t, process %q, %d actors, %d bytes of updates",
			answer.holds, answer.process, len(answer.actors), len(answer.updates))
	}
}
