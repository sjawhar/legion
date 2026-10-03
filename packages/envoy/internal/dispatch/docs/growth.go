package docs

import (
	"context"
	"fmt"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write may not grow a document past what one upload of it may hold. One write's markdown makes
// at most pmdoc's element limit, but writes add up: thirty-two edits of 900 KiB each grew one
// document to 29.5 MB, and eighteen inserts of a thousand headings left one whose live state the
// server could no longer load. So every write that adds caller content - an upload, an edit batch,
// an accepted suggestion - is weighed by what it leaves, twice:
//
//   - its Proof tree, with its marks, against pmdoc.MaxDocumentElements (refuseGrowth), before the
//     write reaches the live document;
//   - the live document a joined write leaves, which must load again with room to spare under the
//     item cap ygo loads a document under (refuseUnloadable), before the write is appended.
//
// A write that leaves the document no heavier than it was passes, so a document already past the
// bound - stored before it, or grown by browser edits, which no server write carries - can still be
// trimmed or split. Each refusal is pmdoc.ErrTooManyElements, served as 413 CAP_EXCEEDED.

// refuseGrowth is the refusal of a write that leaves after weighing more than a document may and
// more than before did, or nil. before is nil for a new document.
func refuseGrowth(before, after *pmdoc.Node) error {
	weight := pmdoc.Weight(after)
	if weight <= pmdoc.MaxDocumentElements {
		return nil
	}
	was := 0
	if before != nil {
		was = pmdoc.Weight(before)
	}
	if weight <= was {
		return nil
	}
	return fmt.Errorf("%w: this change would make the document weigh %d elements, past the %d one document may hold (it weighed %d); shorten the change, or split the document",
		pmdoc.ErrTooManyElements, weight, pmdoc.MaxDocumentElements, was)
}

// maxPendingItems is how many items ygo parks while it loads a document's state into a new
// document, items whose neighbour or parent belongs to a writer it has not read yet: ygo's
// defaultMaxPendingItems, which it does not export. Every load of a live document - its room, a
// transaction's fork, a read of a document no room holds - is such a load, and one that parks more
// is refused as an invalid update: the document is then unreadable (503) and unwritable. A state
// written by one writer parks nothing, but every edit is a writer of its own, and ygo reads writers
// in order of their ids, which are random, so a document of several writers can park all but the
// first one's items.
const maxPendingItems = 100_000

// loadableItems is how many items the state a growth write leaves may park when it is loaded: the
// tenth of maxPendingItems left is the room that the writes no bound covers - settlement's, a
// comment's anchor, a browser's edit - have before the document stops loading.
const loadableItems = maxPendingItems - maxPendingItems/10

// growthBoundKey marks the context of a write refuseUnloadable weighs (growthBound).
type growthBoundKey struct{}

// growthBound is ctx for a write that adds caller content, whose joined live write applyJoined
// refuses when it leaves the document unloadable (refuseUnloadable).
func growthBound(ctx context.Context) context.Context {
	return context.WithValue(ctx, growthBoundKey{}, true)
}

func isGrowthBound(ctx context.Context) bool {
	bound, _ := ctx.Value(growthBoundKey{}).(bool)
	return bound
}

// refuseUnloadable is the refusal of a write that leaves fork, its transaction's copy of the live
// document, a state that would not load parking at most loadableItems, or nil. A state that would
// still load under maxPendingItems is refused only when the write made the document heavier
// (before and after, its Proof tree on either side), so a document past the margin can still be
// trimmed; one that would not load at all is always refused, since storing it would leave the
// document unreadable. The check is the load itself, on a copy: nothing short of reading the state
// tells how many items its writers' order parks. A state that holds no more items than the margin
// - each item spans at least one clock tick - is not loaded.
func refuseUnloadable(fork *crdt.Doc, before, after *pmdoc.Node) error {
	var clocks uint64
	for _, clock := range fork.StateVector() {
		clocks += clock
	}
	if clocks <= loadableItems {
		return nil
	}
	state := crdt.EncodeStateAsUpdateV1(fork, nil)
	if crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(loadableItems)), state, nil) == nil {
		return nil
	}
	loads := crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(maxPendingItems)), state, nil) == nil
	if loads && pmdoc.Weight(after) <= pmdoc.Weight(before) {
		return nil
	}
	return fmt.Errorf("%w: this change would leave the document too large for the server to load again with room to spare (its live copy would park more than %d of the %d items a load may wait on); shorten the change, or split the document",
		pmdoc.ErrTooManyElements, loadableItems, maxPendingItems)
}
