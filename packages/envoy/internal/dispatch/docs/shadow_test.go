package docs

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// residentShadow is the shadow room's current state holds, nil for none.
func residentShadow(service *Service, room string) *roomShadow {
	state := service.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.shadow
}

// retypeBlockID is a browser edit that writes id as the block id of the paragraph whose text is
// find: a change the document's markdown does not carry, so the room's update observer measures
// it as no content change (updateChangesMarkdown) and records the peers connected for the room's
// shadow instead (unchangedCredit).
func retypeBlockID(find, id string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type != "paragraph" || pmdoc.TextContent(node) != find {
				return true
			}
			if node.Attrs == nil {
				node.Attrs = pmdoc.Attrs{}
			}
			node.Attrs[pmdoc.BlockIDAttr] = id
			return true
		})
		return tree
	}
}

// storedContentChanges is, in version order, whether each of the document's stored updates was
// stored as a change to its content.
func storedContentChanges(t *testing.T, database *store.Store, artifactID string) []bool {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select content_changed from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read the stored updates' classes: %v", err)
	}
	defer rows.Close()
	var changes []bool
	for rows.Next() {
		var changed bool
		if err := rows.Scan(&changed); err != nil {
			t.Fatalf("read a stored update's class: %v", err)
		}
		changes = append(changes, changed)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored updates' classes: %v", err)
	}
	return changes
}

// A flush measures its classes once it has let the state's lock go, so the shadow it copied the
// window of can be dropped and released under it (closeShadow, when the write's publish returns;
// reachShadow, when the publish reached another instance). A measure on the document that drop
// let go of renders nothing and counts every class a content change, which would store an update
// the room measured as no change as one - with the peers connected as the room took it written as
// its pending authors. Bob retypes a block's id while the agent's write is open: the room measures
// no change and records those peers for the shadow (unchangedCredit), and the flush of that update
// drops and releases the shadow between its window copy and its measure (beforeFlushMeasure). The
// update is stored as the room measured it, and credits nobody.
func TestAFlushStoresTheRoomsOwnClassWhenTheShadowIsReleasedUnderIt(t *testing.T) {
	service, artifactID := storedOrderService(t)
	if residentShadow(service, artifactID) != nil {
		t.Fatal("a shadow is resident before the write under test, so the drop below is not its")
	}
	service.addConnection(artifactID, 1, storedOrderBob)
	defer service.removeConnection(artifactID, 1)
	service.afterForkRead = onFirst(artifactID, func() {
		editAsPeer(t, service, artifactID, retypeBlockID("Second.", "bob-retyped-second"))
	})
	defer func() { service.afterForkRead = nil }()
	var released atomic.Int64
	service.beforeFlushMeasure = func(room string) {
		if room != artifactID {
			return
		}
		state := service.room(artifactID)
		state.mu.Lock()
		dropped := state.shadow
		state.shadow = nil
		service.unlockState(artifactID, state)
		if dropped != nil {
			released.Add(1)
			dropped.release()
		}
	}
	defer func() { service.beforeFlushMeasure = nil }()

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "First.", With: "First, agent."},
	}, storedOrderAgent, nil); err != nil {
		t.Fatalf("the agent's edit: %v", err)
	}
	report := assertStoredOrder(t, service, artifactID)
	if released.Load() == 0 {
		t.Fatal("no flush measured a class while a shadow was resident, so nothing here is tested")
	}
	if report.UpdatesChecked < 2 {
		t.Fatalf("the stored history holds %d updates to check, want the agent's and Bob's", report.UpdatesChecked)
	}
	changes := storedContentChanges(t, service.store, artifactID)
	if last := changes[len(changes)-1]; last {
		t.Errorf("Bob's update, which the room measured as no change, is stored as a content change: the stored classes are %v", changes)
	}
	if authors := pendingAuthorRows(t, service.store, artifactID); slices.Contains(authors, storedOrderBob) {
		t.Errorf("the document's pending authors are %v, want Bob uncredited: his update changed no content", authors)
	}
}

// A live write leaves its room a shadow at its commit, and the room frees it once nothing it
// measures is left: Bob types into the paragraph the agent's replacement removes after the write's
// fork read, his update is stored as no change, and once the room has drained no shadow is
// resident.
func TestAShadowIsFreedOnceTheRoomHasStoredWhatItMeasures(t *testing.T) {
	service, artifactID := storedOrderService(t)
	var installed bool
	service.afterLiveCommit = onFirst(artifactID, func() {
		installed = residentShadow(service, artifactID) != nil
	})
	defer func() { service.afterLiveCommit = nil }()
	removeSecondUnderBob(t, service, artifactID, &service.afterForkRead)
	if !installed {
		t.Fatal("the write's commit left its room no shadow")
	}
	assertStoredOrder(t, service, artifactID)
	if residentShadow(service, artifactID) != nil {
		t.Fatal("a shadow is still resident after the room stored every update it measures")
	}
}

// A cold write's room loads while the write runs: the replacement reads the store, with no room
// behind it, and Bob opens the document after that read and types into the paragraph the
// replacement removes. The room loaded without the write, so it measures his update as a change;
// stored after the write, it changes nothing, which the shadow the write leaves the loaded room,
// from its first commit on, measures.
func TestAColdWritesShadowMeasuresTheRoomThatLoadedUnderIt(t *testing.T) {
	service, artifactID := storedOrderService(t)
	closeDocumentRoom(t, service, artifactID)
	opened := false
	service.afterForkRead = func(room string) {
		if room != artifactID || opened || service.srv.GetDoc(artifactID) != nil {
			return
		}
		opened = true
		editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	}
	defer func() { service.afterForkRead = nil }()
	if _, err := joinedReplaceText(service, artifactID, "First.\n", storedOrderAgent); err != nil {
		t.Fatalf("agent's replacement: %v", err)
	}
	if !opened {
		t.Fatal("the write found a room at its read, so it was not cold")
	}
	assertStoredOrder(t, service, artifactID)
	if residentShadow(service, artifactID) != nil {
		t.Fatal("a shadow is still resident after the room stored every update it measures")
	}
}

// A shadow whose provisional update is still unstored is freed when its room fails: Bob's append
// is held before the store, so his update is left for the shadow after the write's publish, and
// the failure, then its eviction's hand-over, leave no shadow on any record of the room.
func TestAShadowIsFreedWhenItsRoomFails(t *testing.T) {
	service, artifactID, entered, release := heldAppendService(t)
	removeSecondUnderBob(t, service, artifactID, &service.afterLiveCommit)
	awaitHeldAppend(t, entered)
	failed := service.room(artifactID)
	failed.mu.Lock()
	resident := failed.shadow != nil
	failed.mu.Unlock()
	if !resident {
		t.Fatal("no shadow is resident while bob's update is unstored, so nothing here is tested")
	}
	service.failRoom(artifactID, errors.New("injected room failure"))
	failed.mu.Lock()
	resident = failed.shadow != nil
	failed.mu.Unlock()
	if resident {
		t.Fatal("the failed room still holds its shadow")
	}
	release()
	waitFor(t, 10*time.Second, "the failed room's eviction to settle its record", func() bool {
		return service.roomFailure(artifactID) == nil
	})
	if residentShadow(service, artifactID) != nil {
		t.Fatal("the record that replaced the failed room's holds a shadow")
	}
}

// documentOf is a document holding markdown.
func documentOf(t *testing.T, markdown string) *crdt.Doc {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	doc := newDocumentCopy()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write the document: %v", err)
		}
	})
	return doc
}

// The shadow measures only the classes of its own room instance inside its window, each once, and
// each stored class carries the credit that class calls for: a class its observer measured as no
// change that the shadow measures as one takes the authors the room recorded for it, and one it
// measures as no change writes no author.
func TestAShadowMeasuresTheClassesInItsWindowAndCreditsWhatItMeasures(t *testing.T) {
	base := documentOf(t, "First.\n")
	fork, err := snapshotDocument(base)
	if err != nil {
		t.Fatalf("copy the document: %v", err)
	}
	_, added := peerEdit(t, base, appendBlocks(t, "Second.\n"))
	instance, other := &roomCursor{}, &roomCursor{}
	shadow := &roomShadow{doc: fork, cursor: instance, from: 1}
	inflight, recorded, otherInflight := &UpdateCredit{}, &UpdateCredit{}, &UpdateCredit{}
	class := func(seq uint64, cursor *roomCursor, changed bool, credit, roomCredit *UpdateCredit) *queuedClass {
		return &queuedClass{documentUpdateClass: documentUpdateClass{
			update: added, seq: seq, contentChanged: changed, durable: true, credit: credit, roomCredit: roomCredit, cursor: cursor,
		}}
	}
	atRead := class(1, instance, false, nil, nil)
	nowChanged := class(2, instance, false, nil, recorded)
	nowUnchanged := class(3, instance, true, inflight, nil)
	elsewhere := class(2, other, true, otherInflight, nil)

	window := shadowWindow{shadow: shadow, cursor: instance, from: shadow.from}
	stored := storedClasses("room", window, []*queuedClass{atRead, nowChanged, nowUnchanged, elsewhere})
	want := []storedClass{
		{changed: false, credit: nil},
		{changed: true, credit: recorded},
		{changed: false, credit: nil},
		{changed: true, credit: otherInflight},
	}
	for index := range want {
		if stored[index] != want[index] {
			t.Errorf("class %d is stored as %+v, want %+v", index, stored[index], want[index])
		}
	}
	// A second flush of a class measured already, its first rolled back, stores the same class.
	if again := storedClasses("room", window, []*queuedClass{nowChanged}); again[0] != want[1] {
		t.Errorf("the measured class is stored again as %+v, want %+v", again[0], want[1])
	}
	// Past the write's last published update the observer's class stands.
	window.until = 3
	if closed := storedClasses("room", window, []*queuedClass{class(3, instance, true, inflight, nil)}); closed[0] != (storedClass{changed: true, credit: inflight}) {
		t.Errorf("a class past the window is stored as %+v, want the class its observer recorded", closed[0])
	}
}
