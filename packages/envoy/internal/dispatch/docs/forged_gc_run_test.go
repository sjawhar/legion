package docs

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// forgedGCRunDocument is the document the forged GC run's rooms load with: one paragraph long
// enough that a delete over every other character of it is a few hundred ranges, and short enough
// that each room loads in a few milliseconds.
const forgedGCRunDocument = "word word word word word word word word word word\n"

// A GC run born deleted over every clock the room holds, which the peer update hook admits
// (gcRunOverHeldClocks, TestAGCRunOverHeldClocksLeavesLaterDeletesCharged), leaves the room taking
// edits: the delete a browser sends next is admitted, the replica's rendering after it is what a
// fresh render of the room answers, and the room's text carries the delete. The forged run touches
// no block of the replica's document - it integrates one struct of a client the room holds nothing
// of, born deleted, under no block - so the rendering the room loaded with is what the delete's own
// blocks are spliced into (renderedReplica.spliceTouched), which is why the room is loaded here as
// production loads one (drainedPeerUpdateRoom, keepReplica): with the markdown and the layout of
// one render of the state its replica starts from.
func TestARoomTakesAnHonestDeleteAfterAForgedGCRun(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, forgedGCRunDocument)
	before := firstText(room.GetXmlFragment(fragmentName)).ToString()
	forged := gcRunOverHeldClocks(room)
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte GC run was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	requireRoomRendered(t, room, replica, "the forged GC run")
	honest := peerDeletingEveryOtherCharacter(t, room)
	if err, called := weighInto(service, room, honest); err != nil || !called {
		t.Fatalf("the %d-byte honest delete was answered %v (applied %t), want it admitted", len(honest), err, called)
	}
	requireRoomRendered(t, room, replica, "the honest delete")
	if got, want := firstText(room.GetXmlFragment(fragmentName)).ToString(), everyOtherCharacterDeleted(before); got != want {
		t.Fatalf("the room's text after the delete reads %q, want %q", got, want)
	}
	runtime.KeepAlive(replica)
}

// The same forged GC run, and then the smallest edit there is: one character typed on a browser's
// own copy of the room (snapshotDocument), three of them, each from a copy taken after the one
// before it. Each is admitted, each leaves the replica's rendering equal to a fresh render of the
// room, and the room's text carries all three.
func TestARoomTakesTypedCharactersAfterAForgedGCRun(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, forgedGCRunDocument)
	before := firstText(room.GetXmlFragment(fragmentName)).ToString()
	forged := gcRunOverHeldClocks(room)
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte GC run was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	requireRoomRendered(t, room, replica, "the forged GC run")
	const typed = "y"
	for keystroke := 1; keystroke <= 3; keystroke++ {
		update := browserTypingACharacter(t, room, typed)
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("keystroke %d, %d bytes, was answered %v (applied %t), want it admitted", keystroke, len(update), err, called)
		}
		requireRoomRendered(t, room, replica, fmt.Sprintf("keystroke %d", keystroke))
	}
	if got, want := firstText(room.GetXmlFragment(fragmentName)).ToString(), strings.Repeat(typed, 3)+before; got != want {
		t.Fatalf("the room's text after the three keystrokes reads %q, want %q", got, want)
	}
	runtime.KeepAlive(replica)
}

// A replica that could not place an update holds no document, no fragment and no rendering, and
// waits for the base the room's next commit records (renderedReplica.recover): the edit after that
// commit starts the replica from that base, rendering it whole, and splices its own block into that
// rendering. So a restart leaves the room taking edits too, and nothing on the path reads the
// document, the fragment or the layout the replica dropped.
func TestARestartedReplicaRendersItsBaseAndSplicesTheEditAfterIt(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, "# Title\n\nParagraph one of the room.\n\nParagraph two of the room.\n")
	// The room's first commit, which hands the replica the log's first base and leaves it
	// following the room (renderedReplica.start): what a restart after it starts again from is
	// the fresh base the log records, which no load rendered.
	if err, called := weighInto(service, room, browserTypingACharacter(t, room, "a")); err != nil || !called {
		t.Fatalf("the room's first edit was answered %v (applied %t), want it admitted", err, called)
	}
	requireRoomRendered(t, room, replica, "the room's first edit")
	// The replica cannot place the next update, as one whose apply failed cannot (takeLocked).
	forceRecover(replica)
	before := readRenderPaths(service)
	unplaced := browserTypingACharacter(t, room, "b")
	if err, called := weighInto(service, room, unplaced); err != nil || !called {
		t.Fatalf("the update the replica could not place was answered %v (applied %t), want it admitted", err, called)
	}
	doc, fragment, rendering, layout := replicaHolds(replica)
	if doc || fragment || rendering || layout {
		t.Fatalf("between the drop and the room's next commit the replica holds a document %t, a fragment %t, a rendering %t and a layout %t, want none of them",
			doc, fragment, rendering, layout)
	}
	if took := readRenderPaths(service).minus(before); took != (renderPaths{}) {
		t.Fatalf("the update it could not place took the paths %+v, want none", took)
	}
	before = readRenderPaths(service)
	edit := browserTypingACharacter(t, room, "c")
	if err, called := weighInto(service, room, edit); err != nil || !called {
		t.Fatalf("the edit after the restart was answered %v (applied %t), want it admitted", err, called)
	}
	if took, want := readRenderPaths(service).minus(before), (renderPaths{catchUpRendered: 1, observerIncremental: 1}); took != want {
		t.Fatalf("the edit after the restart took the paths %+v, want %+v", took, want)
	}
	requireRoomRendered(t, room, replica, "the edit after the restart")
	if got := paragraphTexts(room)[0].ToString(); !strings.HasPrefix(got, "cba") {
		t.Fatalf("the room's first paragraph reads %q, want it to carry the three characters typed", got)
	}
	runtime.KeepAlive(replica)
}

// requireRoomRendered holds the rendering the replica keeps after an update to a fresh render of
// the room: whichever path the update took - spliced, rendered whole, or taken from a published
// write - the replica's markdown is the room's own.
func requireRoomRendered(t *testing.T, room *crdt.Doc, replica *renderedReplica, after string) {
	t.Helper()
	want, err := renderDocument(room)
	if err != nil {
		t.Fatalf("after %s, render the room: %v", after, err)
	}
	held := markdownAt(heldRendering(replica))
	if held == nil {
		t.Fatalf("after %s the replica holds no rendering, want the room's", after)
	}
	if *held != want {
		t.Fatalf("after %s the replica's rendering is %q and a fresh render of the room %q", after, *held, want)
	}
}

// replicaHolds is whether the replica holds a document, a fragment, a rendering and a layout with
// that rendering, read under its lock.
func replicaHolds(replica *renderedReplica) (doc, fragment, rendering, layout bool) {
	replica.mu.Lock()
	defer replica.mu.Unlock()
	return replica.doc != nil, replica.fragment != nil, replica.rendering != nil,
		replica.rendering != nil && replica.rendering.layout != nil
}

// browserTypingACharacter is the update a browser sends that types character at the start of the
// first paragraph of its own copy of the room (snapshotDocument): the smallest edit the editor
// makes, from a copy of the room as it stands now.
func browserTypingACharacter(t *testing.T, room *crdt.Doc, character string) []byte {
	t.Helper()
	browser, err := snapshotDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	texts := paragraphTexts(browser)
	if len(texts) == 0 {
		t.Fatal("the browser's copy of the room holds no paragraph")
	}
	var sent []byte
	browser.OnUpdate(func(update []byte, _ any) { sent = update })
	browser.Transact(func(txn *crdt.Transaction) { texts[0].Insert(txn, 0, character, nil) })
	if sent == nil {
		t.Fatal("the browser's keystroke made no update")
	}
	return sent
}

// everyOtherCharacterDeleted is text with the characters peerDeletingEveryOtherCharacter deletes
// taken out of it: every other one from its last, none of them the first.
func everyOtherCharacterDeleted(text string) string {
	runes := []rune(text)
	for at := len(runes) - 1; at > 0; at -= 2 {
		runes = append(runes[:at], runes[at+1:]...)
	}
	return string(runes)
}
