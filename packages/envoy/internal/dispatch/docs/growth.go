package docs

import (
	"fmt"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write may not grow a document past what one upload of it may hold. One upload sends at most
// pmdoc.MaxDocumentBytes of markdown making at most pmdoc.MaxDocumentElements, but writes add up:
// thirty-two inserts of 900 KB of prose, each within both, grew one document to 29.5 MB, on which a
// one-word edit then held a gigabyte; eighteen inserts of a thousand headings left one whose live
// state the server could no longer load; and thirty-two edits of an ask's options grew one to
// 28.8 MB through a route that did not opt in to the bound. So no write opts in: every write a
// caller makes runs through applyLive, which refuses one outside a transaction (errUnjoined) and
// weighs every one by what it leaves (refuseGrowth) before it is appended, as SeedText weighs a new
// document:
//
//   - its rendering, the markdown GET .../text answers and an upload of it would send, measured as
//     an upload is measured (pmdoc.MeasureDocument): its bytes, and the elements the upload's parse
//     makes of it. So whatever a write leaves, its own text uploads back.
//   - the live document it leaves, which must load again with room to spare under the item cap ygo
//     loads a document under (refuseUnloadable).
//
// A write that leaves the document no bigger and no heavier than it was passes, so a document
// already past the bound - stored before it, or grown by browser edits, which no server write
// carries - can still be trimmed or split. Each refusal is pmdoc.ErrTooManyElements, served as 413
// CAP_EXCEEDED. The writes that do not run through applyLive add no caller text: settlement's
// repairs, the block-id backfill and the sweep of unrecorded marks. What the bound weighs is the
// document a write leaves, not the history its store keeps: every update stays stored with the
// content later writes delete, and a cold load builds all of it, so repeated versions and a
// comment's margin record, which each reply rewrites whole, still grow what a load costs
// (LEGION-496).

// refuseGrowth is the refusal of a write that leaves the document rendering as after where it
// rendered as before ("" for a new document), or nil. The rendering is refused past either of an
// upload's limits when it is bigger than before by that measure: longer than before past
// pmdoc.MaxDocumentBytes, heavier than before past pmdoc.MaxDocumentElements. A rendering the write
// left as it was - an anchor mark, a margin record, an attribute no rendering carries - is neither,
// so it is not measured again. fork is the transaction's copy of the live document the write
// leaves, which refuseUnloadable loads, or nil for a new document, which one writer wrote and so
// parks nothing. before is measured only when a refusal turns on its elements.
func refuseGrowth(fork *crdt.Doc, before, after string) error {
	if after == before {
		if fork == nil {
			return nil
		}
		return refuseUnloadable(fork, func() bool { return false })
	}
	size := pmdoc.MeasureDocument(after)
	if size.TooLong() && len(after) > len(before) {
		return fmt.Errorf("%w: a markdown document is at most 1 MiB (%d bytes), and this change would make the document's markdown %d bytes (it was %d); shorten the change, or split the document",
			pmdoc.ErrTooManyElements, pmdoc.MaxDocumentBytes, len(after), len(before))
	}
	var measured *pmdoc.DocumentSize
	was := func() pmdoc.DocumentSize {
		if measured == nil {
			size := pmdoc.MeasureDocument(before)
			measured = &size
		}
		return *measured
	}
	if size.TooHeavy() && heavier(was(), size) {
		return fmt.Errorf("%w: this change would make the document's markdown make %s elements, past the %d one document may hold (it made %s); shorten the change, or split the document",
			pmdoc.ErrTooManyElements, elements(size), pmdoc.MaxDocumentElements, elements(was()))
	}
	if fork == nil {
		return nil
	}
	return refuseUnloadable(fork, func() bool { return len(after) > len(before) || heavier(was(), size) })
}

// heavier reports whether after makes more elements than before, as an upload's parse counts them.
// Markdown whose parse stopped at the guard makes more than any the parse read whole, and two that
// both stopped are compared by their bytes, all either measure says of them.
func heavier(before, after pmdoc.DocumentSize) bool {
	switch {
	case after.Counted && before.Counted:
		return after.Elements > before.Elements
	case after.Counted:
		return false
	case before.Counted:
		return true
	default:
		return after.Bytes > before.Bytes
	}
}

// elements is how many elements size names: its count, or more than the limit where the parse
// stopped at the guard.
func elements(size pmdoc.DocumentSize) string {
	if size.Counted {
		return fmt.Sprint(size.Elements)
	}
	return fmt.Sprintf("more than %d", pmdoc.MaxDocumentElements)
}

// maxPendingItems is how many items ygo parks while it loads a document's state into a new
// document, items whose neighbour or parent belongs to a writer it has not read yet: ygo's
// defaultMaxPendingItems, which it does not export. Every load of a live document - its room, a
// transaction's fork, a read of a document no room holds - is such a load, and one that parks more
// is refused as an invalid update: the document is then unreadable (503) and unwritable. ygo reads
// writers in order of their ids, and a writer it reads before one it builds on has every item from
// there on parked. A transaction's writes take an id past every writer the document holds
// (writerAfter), so what parks is a browser's edits, whose ids are random, and a write that builds
// on items that park themselves.
const maxPendingItems = 100_000

// loadableItems is how many items the state a growing write leaves may park when it is loaded: the
// tenth of maxPendingItems left is the room that the writes that leave the rendering as it was - a
// comment's anchor, a margin record, settlement's repairs - and a browser's edits have before the
// document stops loading.
const loadableItems = maxPendingItems - maxPendingItems/10

// refuseUnloadable is the refusal of a write that leaves fork, its transaction's copy of the live
// document, a state that would not load parking at most loadableItems, or nil. A state that would
// still load under maxPendingItems is refused only when the write grew the document (grew, as
// refuseGrowth measures it), so a document past the margin can still be trimmed; one that would not
// load at all is always refused, since storing it would leave the document unreadable. The check is
// the load itself, on a copy: nothing short of reading the state tells how many items its writers'
// order parks. A state that holds no more items than the margin - each item spans at least one
// clock tick - is not loaded.
func refuseUnloadable(fork *crdt.Doc, grew func() bool) error {
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
	if loads && !grew() {
		return nil
	}
	return fmt.Errorf("%w: this change would leave the document too large for the server to load again with room to spare (its live copy would park more than %d of the %d items a load may wait on); shorten the change, or split the document",
		pmdoc.ErrTooManyElements, loadableItems, maxPendingItems)
}
