package docs

import (
	"fmt"
	"log/slog"
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
// leaves (refuseGrowth) before it is appended, as SeedText weighs a new document's rendering
// (weighRendering):
//
//   - its rendering, the markdown GET .../text answers and an upload of it would send, measured as
//     an upload is measured (pmdoc.MeasureDocument): its bytes, and the elements the upload's parse
//     makes of it. So whatever a write leaves, its own text uploads back.
//   - its margin, the comment and suggestion records a browser shows beside the document, which
//     every load builds though no rendering carries them (marginWatch).
//   - the actor ids its anchor marks carry, which no rendering carries either (anchorWatch).
//   - the live state it leaves, whose complete update ygo must still decode within its
//     1,048,576-struct cap (refuseUndecodable).
//
// A write that leaves the document no bigger and no heavier than it was passes, so a document
// already past the bound - stored before it, or grown by browser edits, which no server write
// carries - can still be trimmed or split. Each refusal is ErrDocumentTooLarge, served as 413
// CAP_EXCEEDED. An answer is caller text the server writes twice: the answer route writes it into
// its ask's block, and settlement writes it back into a block that returns to the document. Where
// the document has no room for it, each records who answered and when and leaves the answer out of
// the block (SetBlockAttributes, withholdAnswers), and the ask keeps it. Of the other writes that
// do not run through applyLive, a rebuild weighs the markdown a caller supplies as SeedText weighs
// a new document's, and restores a latest version whatever it weighs, since that is the server's
// own text (RebuildDocument). Three more add no caller text and call no refuseGrowth, so they never
// reach refuseUndecodable either: the block-id stamp of settlement and of the backfill
// (stampBlockIDs), the sweep of unrecorded marks (unmarkExpired), and settlement's own repair of a
// returning answer (withholdAnswers, returnAnswersWithRoom), which is weighed for size the way a
// caller's answer is. Each writes into the live document itself rather than a fork it could drop,
// so each asks before it writes whether the document its write would leave stays within the cap
// (Service.leavesPastDecodeCap), and leaves one it would carry past as it was (LEGION-545):
// settlement withholds the answer as it withholds one a full document has no room for, so the ask
// keeps it and the settlement counts no failure; the stamp and the sweep write nothing, settlement
// carries on without them, the backfill reports the document skipped, and each logs it once a room
// rather than on every pass (errPastDecodeCap, logPastDecodeCap). Asking of the document as it
// stands would not do: one at exactly the cap decodes, and each of the three would push it past.
// The question is asked first of the clock sum, which costs no encode, and only where that cannot
// clear the write is the document counted (leavesPastDecodeCap gives the steps). The sum clears
// nothing past the cap, because it counts characters and the cap counts structs: ygo's state
// vector is each client's last item's clock plus that item's length (crdt/store.go:190-200 in
// sjawhar/ygo v1.51.3-sami.2), a string's length is its UTF-16 length (crdt/content.go:109), and
// one struct holds a whole run of typing. Measured on 2026-10-25, the shape room_heap_test.go
// builds, typed over as benchmarkLiveDocument types, sums to 52,575 clocks at 51 KiB, 538,306 at
// 524 KiB and 1,050,470 at a mebibyte, past the 1,048,576 cap, where it holds 77,600 structs
// (BenchmarkDecodeCapConfirmation); a 256 KiB document after a mebibyte of typing and deleting
// sums to 1,310,720, and 1.5 MiB written in 1 KiB inserts to 1,572,864; and every one of them
// decodes. A document's clocks never fall, so refusing on the sum alone would stop the stamp, the
// sweep and the returning answers for good on the documents people work in hardest, while
// applyLive, which counts to confirm, went on writing to them.
// Settlement's repair was the one of the three with a plausible route to the cap - two rounds of
// a 50,000-byte answer added 24 clocks, so tens of thousands of repair cycles on one document -
// and the other two had narrower triggers, not cheaper ones: the stamp runs only for a duplicate
// or missing id a browser edit left (LEGION-487's gap), and the sweep only for a mark a browser
// race left with no comment, ask or suggestion row to match. A rebuild is not asked: it replaces
// the document's history with one fresh update of the server's own text rather than adding to it,
// and the store refuses that update past the cap itself (itemCapError).
// What the bound weighs is the document a write leaves, not the history its store keeps - that
// is now compaction's (LEGION-496), which folds every update into the document's state as a
// room closes, as the server shuts down, and daily; what grows between those is bounded by how
// long a room stays open, not by how many versions or replies it has had.

// growth is what one write leaves a document, as refuseGrowth weighs it. The live state it leaves
// is checked apart (refuseUndecodable), once the write has let go of the tree before it.
type growth struct {
	// before and after are the document's renderings before the write and after it, and unchanged
	// whether they are the same.
	before, after string
	unchanged     bool
	// margin is the margin records the write changed, and anchors the actor ids its anchor marks
	// gained.
	margin  *marginWatch
	anchors anchorWatch
	// serverState reports whether the write changed the document's tree only in the state the
	// server keeps on its typed blocks, an answer apart (pmdoc.EqualOutsideServerState). Such a
	// write is weighed as one that left the rendering as it was, so an ask on a document already
	// past the bound can still be answered, resolved or retracted, while an answer's own words and
	// the options it selects are measured as any caller text is.
	serverState func() bool
}

// refuseGrowth is the refusal of the write g describes, or nil. Its rendering is weighed by
// weighRendering. A rendering the write left as it was - an anchor mark, a margin record, an
// attribute no rendering carries - is not measured again, and one that changed only the server's
// state of a typed block (serverState) is taken whatever the measures say of it. Every write is
// then still refused when the live state it leaves could not be decoded (refuseUndecodable, which
// applyLive runs next).
func refuseGrowth(g growth) error {
	if err := g.margin.refusal(); err != nil {
		return err
	}
	if err := g.anchors.refusal(); err != nil {
		return err
	}
	if !g.unchanged && !g.serverState() {
		if err := weighRendering(g.before, g.after); err != nil {
			return err
		}
	}
	return nil
}

// weighRendering is the refusal of after, a document's rendering once a write has run, against
// before, its rendering until then (weigh).
func weighRendering(before, after string) error {
	return weigh(renderingOf(before), renderingOf(after))
}

// rendering is a document's rendering as weigh judges it: its length, and what an upload's parse
// measures of it (pmdoc.MeasureDocument), which is measured only when a judgement turns on it.
type rendering struct {
	bytes    int
	measured func() pmdoc.DocumentSize
}

func renderingOf(markdown string) rendering {
	return rendering{bytes: len(markdown), measured: sync.OnceValue(func() pmdoc.DocumentSize { return pmdoc.MeasureDocument(markdown) })}
}

// size is what an upload's parse measures of r.
func (r rendering) size() pmdoc.DocumentSize {
	size := r.measured()
	size.Bytes = r.bytes
	return size
}

// longer is r with n bytes more that make no element: text an ask block's attributes carry, which
// its directive writes quoted on its one opening line (pmdoc.IsAnswerAttribute).
func (r rendering) longer(n int) rendering {
	return rendering{bytes: r.bytes + n, measured: r.measured}
}

// weigh is the refusal of after against before: after is refused past either of an upload's
// limits when it is bigger than before by that measure - longer than before past
// pmdoc.MaxDocumentBytes, heavier than before past pmdoc.MaxDocumentElements. A rendering refused by
// its bytes is not parsed, and before is measured only when a refusal turns on its elements.
func weigh(before, after rendering) error {
	if after.bytes > pmdoc.MaxDocumentBytes && after.bytes > before.bytes {
		return fmt.Errorf("%w: a markdown document is at most %s (%d bytes), and this change would make the document's markdown %d bytes (it was %d); shorten the change, or split the document",
			ErrDocumentTooLarge, binarySize(pmdoc.MaxDocumentBytes), pmdoc.MaxDocumentBytes, after.bytes, before.bytes)
	}
	size := after.size()
	if size.TooHeavy() && heavier(before.size(), size) {
		return fmt.Errorf("%w: this change would make the document's markdown make %s elements, past the %d one document may hold (it made %s); shorten the change, or split the document",
			ErrDocumentTooLarge, elements(size), pmdoc.MaxDocumentElements, elements(before.size()))
	}
	return nil
}

// maxMarginBytes is the most text a document's margin may hold: the records of every comment and
// suggestion it has had, live in its marks map, which a browser shows beside the document and
// every load of it builds. That is as much as its markdown may hold: thirty-two open suggestions
// of 900 KB took one cold websocket load of their document to 296 MiB, and sixty-four took four
// cold reads at once to 1,223 MiB, past the production task's 1,024 MiB.
const maxMarginBytes = pmdoc.MaxDocumentBytes

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
	grew := 0
	for key, was := range w.was {
		now, _ := w.marks.Get(key)
		text, wasText := marginText(now), marginText(was)
		if text > maxMarginRecordBytes && text > wasText {
			return fmt.Errorf("%w: a comment's margin record - its body, its replies and its suggestion's replacement - holds at most %s (%d bytes) of text, and this change would make one hold %d bytes (it held %d); shorten the comment or the suggestion, or start a new thread",
				ErrDocumentTooLarge, binarySize(maxMarginRecordBytes), maxMarginRecordBytes, text, wasText)
		}
		grew += text - wasText
	}
	if grew <= 0 {
		return nil
	}
	held := 0
	w.marks.ForEach(func(_ string, record any) { held += marginText(record) })
	if held > maxMarginBytes {
		return fmt.Errorf("%w: a document's margin holds at most %s (%d bytes) of comment and suggestion text, and this change would make it hold %d bytes (it held %d); shorten the comment or the suggestion. A document keeps every comment it has had, so one whose margin is full takes more once it is split",
			ErrDocumentTooLarge, binarySize(maxMarginBytes), maxMarginBytes, held, held-grew)
	}
	return nil
}

// maxAnchorBytes is the most text the anchor marks on a document's text may hold. The mark of each
// comment, suggestion and ask carries its id and who made it (MarkSpec), which no rendering carries
// and every load of the document builds, and an ask's mark has no margin record to weigh it: thirty
// asks anchored by a session whose id was 200 KB left a 210-byte rendering over six megabytes of
// live document. It is as much as the margin may hold.
const maxAnchorBytes = maxMarginBytes

// anchorWatch is a write's document tree before it and after it, whose anchor marks refusal
// weighs. before is nil for a write that repairs a tree outside the schema (applyLive), whose marks
// are weighed as a new document's would be: against none.
type anchorWatch struct{ before, after *pmdoc.Node }

// refusal is the refusal of a write that leaves the anchor marks of the document holding more than
// maxAnchorBytes of text and more than they held, or nil.
func (w anchorWatch) refusal() error {
	held := anchorText(w.after)
	if held <= maxAnchorBytes {
		return nil
	}
	was := anchorText(w.before)
	if held <= was {
		return nil
	}
	return fmt.Errorf("%w: the marks a document's comments, suggestions and asks hold on its text carry at most %s (%d bytes) of ids and authors, and this change would make them carry %d bytes (they carried %d); anchor fewer, or split the document",
		ErrDocumentTooLarge, binarySize(maxAnchorBytes), maxAnchorBytes, held, was)
}

// anchorText is the text the anchor marks on tree's text carry: every string in each comment's,
// suggestion's and ask's mark, counted once however many texts the mark covers. No tree carries
// none.
func anchorText(tree *pmdoc.Node) int {
	seen := map[string]bool{}
	text := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		for _, mark := range node.Marks {
			switch MarkKind(mark.Type) {
			case MarkAsk, MarkComment, MarkSuggestion:
			default:
				continue
			}
			id, _ := mark.Attrs["id"].(string)
			if key := mark.Type + "\x00" + id; !seen[key] {
				seen[key] = true
				text += textIn(map[string]any(mark.Attrs))
			}
		}
		return true
	})
	return text
}

// binarySize is bytes as a refusal names a bound: in mebibytes or kibibytes where it is a whole
// number of them.
func binarySize(bytes int) string {
	switch {
	case bytes%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", bytes>>20)
	case bytes%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", bytes>>10)
	}
	return fmt.Sprintf("%d bytes", bytes)
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

// refuseUndecodable is the refusal of a write that leaves fork, its transaction's copy of the live
// document, past the decode cap, or nil: a struct total (countRoomStructs) past the maxUpdateItems
// structs ygo decodes in one update. The bound is asked first without an encode
// (structCeiling), which clears every fork short of the cap; past it the ceiling says nothing,
// since the clock sum in it counts characters rather than structs (this file's doc comment gives
// the figures), and the fork's structs are counted to find out: an encode of its state and a walk
// of it, which allocates nothing per struct, where a decode of that state into a fresh document
// would hold a second copy of it. The count is never below what a decode of the state reads,
// since it adds the splits parked structs can still cause, so it refuses every state a decode
// would. A state past the cap is refused whether or not the write grew the document: storing it
// would leave every read unable to load it.
func refuseUndecodable(fork *crdt.Doc) error {
	if structCeiling(fork) <= maxUpdateItems || countRoomStructs(fork) <= maxUpdateItems {
		return nil
	}
	return fmt.Errorf("%w: this change would leave the document's live state past the %d items ygo decodes in one update; shorten the change, or split the document",
		ErrDocumentTooLarge, maxUpdateItems)
}

// clockSum is the sum of doc's state vector. It bounds the structs doc has integrated from above,
// since every one takes at least one clock, and says nothing past that: a struct of typed text
// takes a clock for each character (crdt/store.go:190-200, crdt/content.go:109 in sjawhar/ygo
// v1.51.3-sami.2).
func clockSum(doc *crdt.Doc) uint64 {
	var clocks uint64
	for _, clock := range doc.StateVector() {
		clocks += clock
	}
	return clocks
}

// structCeiling bounds doc's struct total (countRoomStructs) from above without an encode: the
// one measure of a document against the decode cap that costs nothing. The struct total is the
// structs doc's encoded state carries plus the splits its parked structs and parked delete ranges
// can still cause. Of those structs, every one doc has integrated takes at least one clock of the
// state vector (clockSum), but a parked one takes none: the state vector stops at the gap a parked
// struct waits behind, and the encoded state carries each parked struct, with at most a skip over
// the gap before it (withParked, crdt/update.go:68-95 in sjawhar/ygo v1.51.3-sami.2). So the
// encoded state holds at most the clock sum and two structs for each parked one, and the splits
// are splitsAStructCanCause for each parked struct and each parked delete range
// (crdt.Doc.PendingStats, crdt/doc.go:984-1003). A room may park up to maxUpdateItems structs
// (Server.MaxPendingItems, service.go:575), none of which the clock sum sees.
func structCeiling(doc *crdt.Doc) uint64 {
	parked := doc.PendingStats()
	items, ranges := uint64(parked.Items), uint64(parked.DeleteRanges)
	return clockSum(doc) + 2*items + splitsAStructCanCause*(items+ranges)
}

// serverWrite is one of the server's writes into a room's live document outside applyLive - the
// block-id stamp (stampBlockIDs), the sweep of unrecorded marks (unmarkExpired) and settlement's
// repair of a returning answer (withholdAnswers) - as leavesPastDecodeCap asks the decode cap of
// it.
type serverWrite struct {
	// adds is the most structs the write can add to the document, derived by its writer from what
	// it writes and stated where it is derived; bounded is false for a write no such count bounds,
	// which is always counted.
	adds    uint64
	bounded bool
	// apply makes the write on doc, a copy of the room's document, as the writer would make it on
	// the room's own. Whatever it leaves there, refused or not, is what is counted, since that is
	// what the same write would leave on the room's.
	apply func(doc *crdt.Doc)
}

// leavesPastDecodeCap reports whether write would leave doc, a room's live document, with a
// struct total (countRoomStructs) past the decode cap: the question each server write outside
// applyLive asks before it writes (errPastDecodeCap). Those write into the live document itself,
// where the sweep's single transaction (unmarkExpired) cannot be rolled back once it has begun,
// so the question is answered before the write rather than of what it left. Each step answers
// only where the one before cannot:
//
//   - the ceiling of doc's struct total (structCeiling) and what the write adds at most: within
//     the cap, the write fits. It costs no encode and clears every document well short of the cap.
//   - in the measurer's turn (elementMeasurer.structsAfter), doc's exact struct total, an encode
//     of its state and a walk of it: past the cap, the write is refused, and a copy could not even
//     be made, since its decode would refuse the state; with what the write adds at most and still
//     within the cap, the write fits.
//   - in the same turn, the write made on a copy of doc, whose struct total it raises by as much
//     as it would raise doc's at least: within the cap with that added, the write fits.
//
// The copy cannot be counted for doc's own total: it is a decode of doc's state, and ygo merges
// into one struct each run of typing a decode delivers at once (squashRuns, crdt/transaction.go:
// 134-289), which the room took keystroke by keystroke and holds a struct a keystroke - a
// thousand one-character transactions counted 1,000 structs on the document they were typed into
// and 1 on its copy. What the write adds to the copy bounds what it adds to doc instead. The copy
// holds doc's structs with some runs merged, so every boundary between two of its structs is one
// of doc's: the write inserts the same items into both, and each split it makes in doc, at a
// point no boundary of doc's holds, it makes in the copy too.
//
// No answer is kept between writes. The question turns on the write, so one write's answer does
// not answer another's, and a document's struct total can grow without its clocks: a deletion
// inside a struct splits it and adds no clock, so a count kept until the clocks had grown some
// way could be passed unseen. The count of doc is paid by each such write on a document whose
// ceiling has passed the cap, and the copy only by one within what it adds of the cap; either is
// paid only by a write that has something to write - an id to mint, an expired mark, an answer to
// return. Measured alone on a two-processor pod on 2026-10-26 (BenchmarkDecodeCapConfirmation),
// the count of doc took 31 to 48 ms and allocated 7 to 18 MB on a mebibyte of typed-over markdown
// (a 3,035,240-byte state of 77,600 structs), and 397 to 412 ms and 68 to 102 MB on a document of
// exactly maxUpdateItems structs (19,527,448 bytes); the count with the copy took 302 to 387 ms and
// 72 to 95 MB, and 2.65 to 3.58 s and 573 to 755 MB. The encode and decode into a fresh document
// it replaced, measured beside it, took 162 to 278 ms and 46 to 63 MB, and 1.45 to 1.59 s and 300
// to 448 MB: the count with the copy costs up to about twice that, since it is that decode with
// three encodes and walks beside it, and the count alone a fifth to a quarter of it.
//
// A count holds an encode of doc's whole state, and one near the cap a whole copy of the document,
// which is the heap the room budget exists to bound, so the service runs one at a time, in the
// measurer's turn, which waits for an element count or another room's count to finish: two rooms
// never hold a copy each for it at once. Each of the three writes can wait for it, on its own
// goroutine and never a peer's read loop: they are the server's own background work, and the
// sweep and the backfill already leave a document and run again on the next pass. What the wait
// and the count hold meanwhile is the writer's own: the stamp's repair holds its suppression slot,
// so the room's persistence worker waits at the room's next update, and its close gate
// (applySuppressed); settlement's answer check holds the settlement's database transaction. The
// copy lives for the one count and is dropped, so it is charged to no room's budget entry
// (roomCharge), and the budget's arithmetic does not count it.
//
// The caller holds no transaction on doc, since the encode takes doc's lock, and a count in the
// turn waiting on that lock while the transaction waited on the turn would hold both for good.
// The answer is of doc as it is when the turn counts it: a browser's edit landing between it and
// the write is the peer update hook's to bound (structTotal). The stamp and the sweep keep what
// they write within the bound they were answered on whatever such an edit leaves (stampBlockIDs,
// unmarkExpired), and each of settlement's repairs lands on the one block its id names
// (repairLive).
func (s *Service) leavesPastDecodeCap(doc *crdt.Doc, write serverWrite) bool {
	if write.bounded && structCeiling(doc)+write.adds <= maxUpdateItems {
		return false
	}
	return s.measurer.structsAfter(doc, write) > maxUpdateItems
}

// errPastDecodeCap is the refusal of a server write outside applyLive - the block-id stamp
// (stampBlockIDs) and the sweep of unrecorded marks (unmarkExpired) - that would leave its live
// document past the decode cap (leavesPastDecodeCap), made before the write so it leaves the
// document as it was. Neither write is a caller's, so neither answers it to one: settlement carries
// on without the write, the backfill reports the document skipped, and each logs it once a room
// (logPastDecodeCap). Settlement's repair of a returning answer asks the same question and
// withholds the answer instead (withholdAnswers).
var errPastDecodeCap = fmt.Errorf("the write would leave the document's live state holding more than the %d items ygo decodes in one update", maxUpdateItems)

// logPastDecodeCap logs that write, a server write outside applyLive, left room's document alone
// because the write would carry it past the decode cap (errPastDecodeCap). A document at the cap
// stays there, short of a rebuild or a deletion ygo folds into fewer structs, and the sweep and
// settlement's stamp would otherwise log it on every settlement's pass: each write logs it once
// while the room's state lasts (roomState.pastDecodeCap), and again only once a release or a
// restart has dropped that state.
func (s *Service) logPastDecodeCap(room, write string) {
	state := s.lockState(room)
	_, logged := state.pastDecodeCap[write]
	if !logged {
		if state.pastDecodeCap == nil {
			state.pastDecodeCap = make(map[string]struct{})
		}
		state.pastDecodeCap[write] = struct{}{}
	}
	s.unlockState(room, state)
	if !logged {
		slog.Warn("dispatch: leave a document the write would carry past the decode cap as it is", "room", room, "write", write, "reason", errPastDecodeCap)
	}
}
