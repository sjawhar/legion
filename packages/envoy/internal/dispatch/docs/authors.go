package docs

import "github.com/sjawhar/envoy/internal/dispatch/model"

// A room credits each content change to its authors (creditContentChange, Ledger.credit), who stay
// its pending authors until a committed version that credits them releases them. Each entry
// carries the sequence number of the latest change it credits (roomState.creditSeq), and a
// version's commit releases only the entries it took (authorCapture.release).
//
// That is sound because a version takes its authors no later than it reads the tree it records,
// and a change is credited only once the room holds it. A settlement takes its authors under the
// room's state lock before it copies the tree; a version read straight from the room holds that
// lock across its copy; and a transaction's version takes its authors before its fork is brought
// up to date with the room (captureLiveTextAndAuthors, forkLive). The update observer that credits
// service's change runs only once the change is in the room; a committed transaction's write is
// credited before its publish, but while the transaction still holds the writer slot, and a
// settlement that finds the slot held, or published since its read, writes no version
// (settleRoomWithin). So the tree holds every change whose entry the version takes.
//
// An entry credited after the take stays pending for the next version that holds its change: its
// author edited again after the take, or the version's tree holds the change while its update
// observer had not yet credited it - ygo runs that observer only once the change's transaction
// has released the document, and observers wait for each other's renders (renderedReplica). A
// settlement that writes no version releases nothing, since the version it finds can hold such a
// change (LEGION-503).

// pendingAuthor is a pending author of a room's next version: the actor, and seq, the sequence
// number of the latest content change credited to them (roomState.creditSeq).
type pendingAuthor struct {
	actor model.Actor
	seq   uint64
}

// authorCapture is whom a version credits, taken no later than the tree it records.
type authorCapture struct {
	// state is the room the authors were taken from, and through its credit sequence then: the
	// version holds each change of its authors' pending entries numbered no later.
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

// release releases, once the version the capture was taken for has committed, the pending entries
// the version holds and credits: each of its authors' entries credited no later than through. Every
// release of pending authors goes through it.
func (capture authorCapture) release() {
	state := capture.state
	state.mu.Lock()
	defer state.mu.Unlock()
	for key := range capture.authors {
		if entry, pending := state.pending[key]; pending && entry.seq <= capture.through {
			delete(state.pending, key)
		}
	}
}
