package docs

import (
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// residentShadow is the shadow room's current state holds, nil for none.
func residentShadow(service *Service, room string) *roomShadow {
	state := service.room(room)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.shadow
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
