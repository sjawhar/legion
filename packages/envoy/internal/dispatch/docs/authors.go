package docs

import "github.com/sjawhar/envoy/internal/dispatch/model"

// A room credits each content change to its authors (creditContentChange, Ledger.credit), who stay
// its pending authors until a committed version releases them. Each entry carries the sequence
// number of the latest change it credits (roomState.creditSeq). A version captures the sequence no
// later than it reads the tree it records, and its commit releases every entry credited through
// that sequence (authorCapture.release): those changes are in the version's tree.
//
// That holds because a change is credited only once the room holds it. A settlement and a version
// read straight from the room take their authors under the room's state lock before they copy the
// tree; a transaction's version takes its authors before its fork is brought up to date with the
// room (captureLiveTextAndAuthors, forkLive). The update observer that credits a browser's or the
// service's change runs only once the change is in the room. A committed transaction's write is
// credited before its publish, while the transaction still holds the writer slot, and a settlement
// that finds the slot held, or published since its read, writes no version (settleRoomWithin).
//
// An entry credited after the capture stays pending for the next version that holds its change:
// its author edited again after the capture, or the version's tree holds the change while its
// update observer had not yet credited it - ygo runs that observer only once the change's
// transaction has released the document, and observers wait for each other's renders
// (renderedReplica). A settlement that writes no version releases nothing, since the version it
// finds can hold such a change (LEGION-503).
//
// An upload's version is the exception to "holds": an upload that changes the document clears
// every entry credited before its write last read the room, whether its replacement removed that
// change or kept it, and credits its uploader alone (Ledger.WroteVersion).

// pendingAuthor is a pending author of a room's next version: the actor, and seq, the sequence
// number of the latest content change credited to them (roomState.creditSeq).
type pendingAuthor struct {
	actor model.Actor
	seq   uint64
}

// authorCapture is whom a version credits, and the credit sequence it was taken through.
type authorCapture struct {
	// state is the room the capture was taken from, and through its credit sequence then: the
	// version's commit releases every pending entry numbered no later.
	state   *roomState
	through uint64
	authors map[string]model.Actor
}

// creditAuthor makes actor a pending author of the room's next version, for a content change of
// theirs the room now holds. The caller holds state.mu.
func (state *roomState) creditAuthor(actor model.Actor) {
	state.creditSeq++
	state.pending[actorKey(actor)] = pendingAuthor{actor: actor, seq: state.creditSeq}
}

// captureThrough is a capture, crediting no one yet, whose commit releases the entries the room
// credited through through. Every capture is made here.
func (state *roomState) captureThrough(through uint64, size int) authorCapture {
	return authorCapture{state: state, through: through, authors: make(map[string]model.Actor, size)}
}

// takeAuthors captures the room's pending authors for a version whose tree is read under this
// lock or after it. The caller holds state.mu.
func (state *roomState) takeAuthors() authorCapture {
	capture := state.captureThrough(state.creditSeq, len(state.pending)+1)
	for key, entry := range state.pending {
		capture.authors[key] = entry.actor
	}
	return capture
}

// credit adds actor to the authors the version credits.
func (capture authorCapture) credit(actor model.Actor) {
	capture.authors[actorKey(actor)] = actor
}

// has reports whether the version credits actor.
func (capture authorCapture) has(actor model.Actor) bool {
	_, found := capture.authors[actorKey(actor)]
	return found
}

// release releases, once the version the capture was taken for has committed, every pending entry
// credited no later than through. A capture of the room's pending authors took each of those
// entries, so its version credits them; an upload's clears them (Ledger.WroteVersion). Every
// release of pending authors goes through it.
func (capture authorCapture) release() {
	state := capture.state
	state.mu.Lock()
	defer state.mu.Unlock()
	for key, entry := range state.pending {
		if entry.seq <= capture.through {
			delete(state.pending, key)
		}
	}
}
