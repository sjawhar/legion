package docs

import "github.com/sjawhar/envoy/internal/dispatch/model"

// A room credits each content change to its authors (creditContentChange, Ledger.credit), who stay
// its pending authors until a committed version that credits them releases them. Each entry
// carries the sequence number of the change it credits, and a version takes the pending authors
// with the tree it records, through the latest credit then (takeAuthors), so its commit releases
// only the entries it took (authorCapture.release). An entry credited after the take stays
// pending for the next version that holds its change: its author edited again after the version
// took its authors, or the version's tree holds the change while its update observer had not yet
// credited it - ygo runs that observer only once the change's transaction has released the
// document, and observers wait for each other's renders (renderedReplica). A settlement that
// writes no version releases nothing, since the version it finds can hold such a change
// (LEGION-503).

// pendingAuthor is a pending author of a room's next version: the actor, and credit, the sequence
// number of the latest content change credited to them (roomState.credits).
type pendingAuthor struct {
	actor  model.Actor
	credit uint64
}

// authorCapture is whom a version credits, taken with the tree it records.
type authorCapture struct {
	// state is the room the authors were taken from, and through its credit sequence then: the
	// version holds each of its authors' pending entries credited no later.
	state   *roomState
	through uint64
	authors map[string]model.Actor
	// write is the calling transaction's write to the document, and edits how many of its content
	// changes the version holds (liveWrite.edits). A version that holds them all credits the
	// write's authors, whom its transaction's commit then leaves out of pending (Ledger.credit).
	write *liveWrite
	edits int
}

// creditAuthor makes actor a pending author of the room's next version, for a content change of
// theirs observed now. The caller holds state.mu.
func (state *roomState) creditAuthor(actor model.Actor) {
	state.credits++
	state.pending[actorKey(actor)] = pendingAuthor{actor: actor, credit: state.credits}
}

// takeAuthors captures the room's pending authors for a version of the tree just read. The caller
// holds state.mu.
func (state *roomState) takeAuthors() authorCapture {
	authors := make(map[string]model.Actor, len(state.pending)+1)
	for key, entry := range state.pending {
		authors[key] = entry.actor
	}
	return authorCapture{state: state, through: state.credits, authors: authors}
}

// release releases, once the version the capture was taken for has committed, the pending entries
// the version holds and credits: each of its authors' entries credited no later than through. Every
// release of pending authors goes through it.
func (capture authorCapture) release() {
	state := capture.state
	state.mu.Lock()
	defer state.mu.Unlock()
	for key := range capture.authors {
		if entry, pending := state.pending[key]; pending && entry.credit <= capture.through {
			delete(state.pending, key)
		}
	}
}
