package docs

import (
	"fmt"
	"sync"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write may not grow a document past what one upload of it may hold. One upload sends at most
// pmdoc.MaxDocumentBytes of markdown making at most pmdoc.MaxDocumentElements, but writes add up:
// thirty-two inserts of 900 KB of prose, each within both, grew one document to 29.5 MB, on which a
// one-word edit then held a gigabyte; eighteen inserts of a thousand headings left one whose live
// state the server could no longer load; thirty-two edits of an ask's options grew one to 28.8 MB;
// and thirty-two suggestions of 900 KB, which leave the markdown as it was, took one cold
// websocket load of their document to 296 MiB. So every write a caller makes runs through
// applyLive, which refuses one outside a transaction (errUnjoined) and weighs every one by what it
// leaves (refuseGrowth) before it is appended, as SeedText weighs a new document:
//
//   - its rendering, the markdown GET .../text answers and an upload of it would send, measured as
//     an upload is measured (pmdoc.MeasureDocument): its bytes, and the elements the upload's parse
//     makes of it. So whatever a write leaves, its own text uploads back.
//   - its margin, the comment and suggestion records a browser shows beside the document, which
//     every load builds though no rendering carries them (marginWatch).
//   - the live document it leaves, which must load again with room to spare under the item cap ygo
//     loads a document under (refuseUnloadable).
//
// A write that leaves the document no bigger and no heavier than it was passes, so a document
// already past the bound - stored before it, or grown by browser edits, which no server write
// carries - can still be trimmed or split. Each refusal is ErrDocumentTooLarge, served as 413
// CAP_EXCEEDED. The writes that do not run through applyLive add no caller text: settlement's
// repairs, the block-id backfill and the sweep of unrecorded marks. What the bound weighs is the
// document a write leaves, not the history its store keeps: every update stays stored with the
// content later writes delete, and a cold load builds all of it, so repeated versions and a
// comment's margin record, which each reply rewrites whole, still grow what a load costs
// (LEGION-496).

// growth is what one write leaves a document, as refuseGrowth weighs it.
type growth struct {
	// fork is the transaction's copy of the live document the write leaves, which
	// refuseUnloadable loads, or nil for a new document, which one writer wrote and so parks
	// nothing.
	fork *crdt.Doc
	// before and after are the document's renderings before the write ("" for a new document)
	// and after it.
	before, after string
	// margin is the margin records the write changed, or nil for a new document, which has none.
	margin *marginWatch
}

// refuseGrowth is the refusal of the write g describes, or nil. Its rendering is refused past
// either of an upload's limits when it is bigger than before by that measure: longer than before
// past pmdoc.MaxDocumentBytes, heavier than before past pmdoc.MaxDocumentElements. A rendering the
// write left as it was - an anchor mark, a margin record, an attribute no rendering carries - is
// neither, so it is not measured again. before is measured only when a refusal turns on its
// elements.
func refuseGrowth(g growth) error {
	if err := g.margin.refusal(); err != nil {
		return err
	}
	if g.after == g.before {
		if g.fork == nil {
			return nil
		}
		return refuseUnloadable(g.fork, func() bool { return false })
	}
	size := pmdoc.MeasureDocument(g.after)
	if size.TooLong() && len(g.after) > len(g.before) {
		return fmt.Errorf("%w: a markdown document is at most 1 MiB (%d bytes), and this change would make the document's markdown %d bytes (it was %d); shorten the change, or split the document",
			ErrDocumentTooLarge, pmdoc.MaxDocumentBytes, len(g.after), len(g.before))
	}
	was := sync.OnceValue(func() pmdoc.DocumentSize { return pmdoc.MeasureDocument(g.before) })
	if size.TooHeavy() && heavier(was(), size) {
		return fmt.Errorf("%w: this change would make the document's markdown make %s elements, past the %d one document may hold (it made %s); shorten the change, or split the document",
			ErrDocumentTooLarge, elements(size), pmdoc.MaxDocumentElements, elements(was()))
	}
	if g.fork == nil {
		return nil
	}
	return refuseUnloadable(g.fork, func() bool { return len(g.after) > len(g.before) || heavier(was(), size) })
}

// maxMarginBytes is the most text a document's margin may hold: the records of every comment and
// suggestion it has had, live in its marks map, which a browser shows beside the document and
// every load of it builds. That is as much as its markdown may hold (pmdoc.MaxDocumentBytes):
// thirty-two open suggestions of 900 KB took one cold websocket load of their document to 296 MiB,
// and sixty-four took four cold reads at once to 1,223 MiB, past the production task's 1,024 MiB.
const maxMarginBytes = 1 << 20

// maxMarginRecordBytes is the most text one margin record may hold: a comment's body and its
// replies, or a suggestion's and the replacement it proposes. A record is written whole every time
// it changes - a reply, an edit, a resolve, an accept - so it also bounds what each of those writes
// adds to the document's stored history.
const maxMarginRecordBytes = 256 << 10

// marginWatch is the margin records one write changes on its transaction's fork, each as it was
// before the write. A comment's projection (ProjectMark) is the only server write of one.
type marginWatch struct {
	marks *crdt.YMap
	was   map[string]any
}

// watchMargin records the margin records the writes fork takes next change, until stop.
func watchMargin(fork *crdt.Doc) (watch *marginWatch, stop func()) {
	watch = &marginWatch{marks: fork.GetMap(marksMapName)}
	stop = watch.marks.Observe(func(event crdt.YMapEvent) {
		for key, change := range event.Keys {
			if _, seen := watch.was[key]; seen {
				continue
			}
			if watch.was == nil {
				watch.was = map[string]any{}
			}
			watch.was[key] = change.OldValue
		}
	})
	return watch, stop
}

// refusal is the refusal of a write that leaves a margin record it changed holding more than
// maxMarginRecordBytes of text and more than it held, or the margin holding more than
// maxMarginBytes and more than it held, or nil. A write that changes no record weighs nothing here.
func (w *marginWatch) refusal() error {
	if w == nil {
		return nil
	}
	grew := 0
	for key, was := range w.was {
		now, _ := w.marks.Get(key)
		text, wasText := marginText(now), marginText(was)
		if text > maxMarginRecordBytes && text > wasText {
			return fmt.Errorf("%w: a comment's margin record - its body, its replies and its suggestion's replacement - holds at most 256 KiB (%d bytes) of text, and this change would make one hold %d bytes (it held %d); shorten the comment or the suggestion, or start a new thread",
				ErrDocumentTooLarge, maxMarginRecordBytes, text, wasText)
		}
		grew += text - wasText
	}
	if grew <= 0 {
		return nil
	}
	held := 0
	w.marks.ForEach(func(_ string, record any) { held += marginText(record) })
	if held > maxMarginBytes {
		return fmt.Errorf("%w: a document's margin holds at most 1 MiB (%d bytes) of comment and suggestion text, and this change would make it hold %d bytes (it held %d); shorten the comment or the suggestion. A document keeps every comment it has had, so one whose margin is full takes more once it is split",
			ErrDocumentTooLarge, maxMarginBytes, held, held-grew)
	}
	return nil
}

// marginText is how much text a margin record holds: every string in it, at any depth, but the
// state the server keeps beside a comment's text - its kind, when it was made, a suggestion's
// status (MarkRecord) - which no caller writes and which an accept or a reject rewrites.
func marginText(record any) int {
	fields, ok := record.(map[string]any)
	if !ok {
		return textIn(record)
	}
	text := 0
	for name, value := range fields {
		switch name {
		case "kind", "createdAt", "status":
		default:
			text += textIn(value)
		}
	}
	return text
}

// textIn is the length of every string in value, at any depth.
func textIn(value any) int {
	text := 0
	switch value := value.(type) {
	case string:
		text = len(value)
	case []any:
		for _, item := range value {
			text += textIn(item)
		}
	case map[string]any:
		for _, item := range value {
			text += textIn(item)
		}
	}
	return text
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
		ErrDocumentTooLarge, loadableItems, maxPendingItems)
}
