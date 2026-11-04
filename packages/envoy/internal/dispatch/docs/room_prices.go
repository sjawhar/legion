package docs

// This file holds the prices the room budget charges a loaded room at: the heap one copy of a
// document is measured to hold for each element, rendered byte, struct, split and byte of content
// it holds, and the arithmetic that turns a room's three parts into the weight the budget carries
// (roomWeight, copyHeap).

// bytesOfHeapAnElement is the heap one copy of a document holds for each element its rendering
// makes (pmdoc.MeasureDocument): about 500 bytes at 524 KiB (501 read, 510 edited) and at 1 MiB
// (493 and 502), 605 and 616 at 51 KiB, where what every document holds whatever its size is a
// larger share. A room holds two from its load on, the live document and the rendered replica its
// update observer keeps (renderedReplica), which is the copy the load decoded of the stored state
// (keepReplica); the budget charges every room at both from its admission on (roomCopies). The
// heap follows the elements and not the markdown's bytes, which misprice a dense document by
// several times: the figure before this one, 120 bytes of heap a byte of markdown, was measured
// on a shape no room can hold (room_heap_test.go).
// TestAResidentRoomsHeapPerByteOfItsMarkdown measures rooms of each size, read and edited, and
// fails once a copy's heap an element leaves 0.85x to 1.3x of this. A document whose structs weigh
// more at bytesOfHeapAStructHeld, or whose rendered bytes weigh more at bytesOfHeapARenderedByte,
// than its elements at this is priced at the heavier of those instead (copyHeap).
//
// It stood at 470 under sjawhar/ygo v1.51.3-sami.7, where the same rooms read 470 at 524 KiB and
// at 1 MiB and 578 at 51 KiB. Every element of a rendering is a shared type in the room's copies,
// and upstream reearth/ygo#295 added three fields to crdt.abstractType (see bytesOfHeapAStruct),
// which is what moved each reading by 23 to 38 bytes.
const bytesOfHeapAnElement = 500

// bytesOfHeapAStructHeld is the heap one copy of a document holds for each struct of its state
// (countRoomStructs, the count the room's struct total keeps, roomTotals). A copy is priced at its
// elements (bytesOfHeapAnElement), its rendered bytes (bytesOfHeapARenderedByte) or its structs at
// this price, whichever is the most (copyHeap), because a document's history, which no rendering
// shows, can hold far more than the document renders: the long-edited document typed on until the
// decode cap renders 5,314 bytes making 53 elements, 26,500 bytes of heap at the element price,
// from 1,048,576 structs, which hold 168,652,856 bytes in each copy.
//
// Measured on 2026-10-28 (TestAResidentRoomsHeapPerStructOfItsState), what one copy of a resident
// room holds for each struct of its state: the document an agent rewrote whole until the decode cap
// refused a write, 1,048,576 structs rendering a mebibyte, 162.8 bytes; the long-edited document
// typed on until the cap, 160.8; the long-edited fixture itself, 494,308 structs, 161.4; and a
// paragraph typed into 100,000 times before its last character and then deleted, 161.6. It is set
// past the largest. A mebibyte of prose holds 345.7 bytes for each of its 77,600 structs, which
// carry its text, and is priced by its elements, 28,800,000 bytes a copy against 13,192,000 at
// this price. A run of a million keystrokes typed one after another is no struct-dense document:
// the decode of its state merges the run into one item (squashRuns, sjawhar/ygo v1.55.1-sami.1,
// crdt/transaction.go:275-430), so a loaded copy holds 11 structs and 9 elements, priced at 4,500
// bytes, while both copies hold 20,410,464 bytes, about 10.2 bytes a character of the paragraph's
// mebibyte of text in each copy: text in one paragraph is a cost neither this price nor the
// element price follows, and the rendered-byte price does (bytesOfHeapARenderedByte). That test
// fails once a struct-dense shape holds more than this a struct, or less than 0.85 of it.
const bytesOfHeapAStructHeld = 170

// bytesOfHeapARenderedByte is the heap one copy of a document holds at most for each byte of its
// rendering: the floor of its price (copyHeap), which is its elements at bytesOfHeapAnElement, its
// structs at bytesOfHeapAStructHeld or its rendered bytes at this, whichever is the most. Text in
// long runs is what neither other price follows: ygo merges a run typed in one transaction into
// one item as the transaction commits, and the decode of a state merges a run of keystrokes stored
// each as an item of its own the same way (squashRuns, sjawhar/ygo v1.55.1-sami.1,
// crdt/transaction.go:275-430), so a paragraph of a mebibyte loads as 11 structs making 9
// elements, 4,500 bytes a copy at the element price, while each copy holds its text.
//
// Measured on 2026-10-28 (TestAResidentRoomsHeapPerStructOfItsState), what one copy of a resident
// room holds for each byte of its rendering: a run of a million keystrokes, each stored as an item
// of its own, 10.2 bytes, 20,410,464 for both copies of 1,000,042 rendered bytes; runs of 50,000
// to 500,000 keystrokes 9.2 to 11.1, the most at 300,000. The decode appends each keystroke's item
// to its client's slice of the store before the merge, and the merge keeps that slice's array
// (StructStore.Append, crdt/store.go:64-73, and squashRuns's compaction,
// crdt/transaction.go:377-395): eight bytes a keystroke and the slack append grew the array by,
// beside the character's own byte, so the figure moves with the run's length. A
// paragraph of a mebibyte typed in one transaction, which no decode merges, holds 1.0 byte a byte,
// and so do eight paragraphs of 128 KiB. It is set past the most measured, 11.1, rounded up to the
// next whole byte. It stays under the other two prices of the shapes they price: a mebibyte of
// prose holds 25.6 bytes a rendered byte and is priced by its 57,600 elements, 28,800,000 bytes a
// copy against 12,580,308 at this price, and the agent-rewritten document at the decode cap by its
// structs, 178,257,920 bytes a copy against 12,582,900 for its mebibyte of rendering. That test
// fails once a long run holds more than this a byte, or a run of keystrokes less than 0.75 of it.
const bytesOfHeapARenderedByte = 12

// bytesOfHeapAByteAtTheDensest is the densest price: the heap one copy of a document is charged
// for each byte of a rendering pmdoc.MeasureDocument could not finish counting (DocumentSize.Counted
// false, elementsPastCounting), which is the most elements a byte of rendered markdown makes, as
// this repository measures it, at bytesOfHeapAnElement each. That is the one thing it prices
// (copyHeap): MeasureDocument answers such a rendering with no elements at all, and charged as
// that, the heaviest room in the pod would look free. Every rendering it finishes is charged its
// count, or its bytes at bytesOfHeapARenderedByte where they weigh more.
//
// The bytes it prices are always a rendering - a room's (renderedReplica.publishSize), or a stored
// version, which is stored as its rendering (writeVersionTx) - so what it is measured on is
// the rendering of each dense shape pmdoc's own tests build and of each of this repository's
// markdown files (TestNoMarkdownPricesPastTheDensestPrice), and it is set from the figure at a
// mebibyte, the size of document the budget is there for. The densest there is empty items of
// alternating bullet lists, `- \n\n* \n\n`, each item a list of its own: 2.2501 elements a byte at
// 1 MiB and at 64 KiB alike, which at 500 bytes an element is 1,125.1 bytes of heap a byte,
// rounded up here, so a mebibyte of it is priced at 2,361,393,152 bytes for two copies, more than
// the whole budget. Measured on the markdown a caller sends rather than on its rendering, the same
// shape is 4.5 elements a byte, since its rendering is twice as long. Of the repository's 1,474
// files in the schema the densest rendering is a fixture of empty list items,
// pmdoc/testdata/corpus/nested-empty-items.md, at 1.37 elements a byte.
//
// A small document can be denser a byte than this: the densest measured overall is a lone empty
// list item, a list, its item and the paragraph the item is read as holding, nine elements in a
// rendering of three bytes, 3.0 a byte. That is acceptable because its absolute cost is small -
// nine elements are about 4.5 KB of heap whatever their density. Nor is it the most any markdown
// makes at a mebibyte: nested containers, which none of pmdoc's shapes build, render denser,
// `- - \n\n* - \n\n` at 3.0 elements a byte, and a document of them that goldmark stops counting
// is priced under what it holds.
const bytesOfHeapAByteAtTheDensest = 1_126

// bytesOfHeapAStruct is the heap one copy of a document holds for each struct a browser's update
// adds to it: the price of the structure part of a room's charge (roomCharge), which a count of the
// rendering cannot see. One update of 500,000 GC structs, a million bytes, keeps 64,645,200 bytes
// of heap in its room with the rendering unchanged (plan.md, departure 4), where the rendering's
// count prices nothing for it. Measured on 2026-10-30
// (TestTheRoomBudgetsPricePerStructHoldsEveryKind), what one copy keeps for each struct, in frames
// of 20,000 of one kind: a GC run 129 bytes, a deleted item 130, a string 146, an Any value 162, a
// format 170, a JSON value 178, an XML hook 434, an array, a map, a text, an XML fragment or an XML
// text 498, an XML element 562; and as the entry of a key of its own, as an XML element's attribute
// is and a margin record is, a string 219, an Any value 235, a map 571 and an XML element 635. The
// walk refuses binary content and embeds from a browser (errUpdateNotFromEditor), so none is here.
// Every struct is charged at the next fifty bytes past the largest, since the walk that charges a
// struct (peerWeighing) does not decide which kind of a browser's frame is the expensive one; that
// test fails once any kind keeps more than this, or once the largest falls under 0.8 of it.
//
// Every kind holding a shared type - the hook, the array, the map, the text, the XML fragment, the
// XML text and the XML element, keyed or not - weighs 64 to 80 bytes more than it did under
// sjawhar/ygo v1.51.3-sami.7, where the same frames read 354, 418, 498, 491 and 571: upstream
// reearth/ygo#295 added cleanFormatting, fmtGen and attrCache to crdt.abstractType (sjawhar/ygo
// v1.55.1-sami.1, crdt/abstract_type.go:105-115), which takes each of those allocations into the
// next size class.
// No kind that holds no shared type moved. The largest went from 570.7 to 634.7 bytes, past the
// 620 this charged before.
//
// What a struct's integration splits is charged at bytesOfHeapASplit, not at this price: a split
// allocates an item, and the content it divides stays where it is.
const bytesOfHeapAStruct = 650

// bytesOfHeapASplit is the heap one copy of a document holds for each item a struct's integration
// or a delete range's apply splits off (splitsAStructCanCause, two for each new struct, and for
// each delete range one for each of its two ends its apply can leave split,
// deletedRuns.splitEnds). A split allocates one item, and what it does with the content it divides
// follows that content's kind rather than the kind of struct the split lands in: a string's text
// and a deleted length are divided where they stand, the text as a slice of the bytes the item
// already held (sjawhar/ygo v1.55.1-sami.1, crdt/content.go:56-74, :85-89, :118-124); a run of Any
// or JSON values is divided into a slice of its own, sixteen bytes an interface header for each
// value past the split (:174-178, :192-196), which the content price of the struct that carried
// the run covered when the hook admitted it, 120 bytes for each of its bytes against 16 for each
// of its values (bytesOfHeapAByteOfValues). So a split is priced from the item, not from
// bytesOfHeapAStruct, the heaviest kind's price. Measured on 2026-10-29 and read again unchanged
// on 2026-10-30 (TestTheRoomBudgetsSplitMarginHoldsWhatSplitsAdd) in frames of 20,000 splits of
// one shape: a delete range splitting a run at both ends keeps 307.2 bytes, 153.6 a split, and a
// one-character insert splitting a run keeps 338.8 bytes with its split, 191.3 of that beyond the
// 145.5 its string struct keeps alone (TestTheRoomBudgetsPricePerStructHoldsEveryKind). Charged at
// the next fifty bytes past the larger; that test fails once a split keeps more than this, or once
// the range's falls under 0.7 of it.
//
// Typing is what the difference is for. A keystroke adds one struct and can split one item, and
// charged at the heaviest kind's price its two splits come to 1,300 bytes a copy against the 338.8
// the insert and its split keep together: the room budget would charge a mebibyte of prose typed
// with 2,000 keystrokes and ten pastes of 4 KiB a structure part of 8,018,920 bytes, taking its
// charge to 65,618,920, 1.117 times the 58,740,176 bytes of heap the cross-checked reading of
// 2026-11-03 under sjawhar/ygo v1.54.1-sami.3 finds the room holding, where this price charges
// 4,400,920 and 1.056 times it
// (TestATypedAndPastedRoomIsChargedAtLeastItsHeapAndWithinTheFactor). Under sjawhar/ygo
// v1.51.3-sami.7 the same two prices charged 1.112 and 1.051 times a reading of 55,591,872 bytes,
// and the single reading of 2026-10-29, 3.2 MB short before the test cross-checked it against what
// dropping the room frees, put the heaviest kind's price at 1.179 times 52,421,760 bytes, which is
// what that price was measured again for. The price stands on the per-struct measurement above,
// not on that factor.
const bytesOfHeapASplit = 200

// bytesOfHeapAByteOfText is the heap one copy of a document holds for each byte of text a
// browser's update carries as content no rendering prices (peerWeighing.walkedStruct): a string,
// an XML type's name, and the key of a map entry. ygo copies the bytes, 1.00 bytes of heap a byte
// where the copy fills its allocation, but Go rounds an allocation up to its size class, and one
// past the largest class to whole 8 KiB pages, so a text of 32,769 bytes keeps 1.24 a byte.
// Measured on 2026-10-26 (TestTheRoomBudgetsContentFactorsHoldEveryKind), and charged at the next
// whole byte; that test fails once any text keeps more than this, or the heaviest under half of it.
const bytesOfHeapAByteOfText = 2

// bytesOfHeapAByteOfValues is the heap one copy of a document holds for each byte of Any, JSON or
// format content a browser's update carries where no rendering prices it. Values decode into Go
// maps, slices and interfaces, so they weigh many times their bytes, and the most is the
// shape that allocates a map for the fewest bytes: lib0 Any maps of one entry nested in each
// other, 111.7 bytes a byte at a depth of 99, 88.0 for one map of one null entry; a JSON item ygo
// reads as legacy Any weighs the same, and JSON text at most 66.9, as JSON objects of one entry
// nested in each other. Measured on 2026-10-26 (TestTheRoomBudgetsContentFactorsHoldEveryKind),
// which fails once any of those shapes weighs more than this, or once the heaviest falls under 0.8
// of it.
const bytesOfHeapAByteOfValues = 120

// roomCopies is the copies of its document a room is charged at, from the reservation its
// admission makes (storedRoomReservation) through its load (load) to its retirement (retire): its
// live document and the rendered replica its update observer keeps (renderedReplica), which the
// room holds from its load on, the copy its load decoded of the stored state (keepReplica). Both
// are charged before the load holds either, since an admission reserves the room as it will stand:
// charged one copy, rooms would be admitted at half their price, an admission of a mebibyte of
// prose reserving 28,800,000 bytes rather than 57,600,000, and an empty budget admitting
// seventy-four of them rather than thirty-seven, whose open rooms would come to about twice the
// budget: the figures here are the calibration's, a 2 GiB budget in a 4 GiB pod, and the rooms
// double at any limit. Every part of a charge is at both copies: the replica holds the
// live document's whole state and then takes every update the room commits
// (renderedReplica.observe), so each struct, split and byte of content the live document takes,
// the replica takes too.
const roomCopies = 2

// roomWeight is what a room is charged whose one copy's latest count is priced at heap (copyHeap),
// which has added content bytes of content since the rendering that count measured, and whose
// structure part is structure: heap and content at roomCopies, content at the densest price, and
// structure as it is, already at roomCopies. A mebibyte of prose typed into with a paste of 4 KiB
// and counted since weighs 57,600,000 bytes and the paste's structure; uncounted, the paste weighs
// 4,096 x 1,126 x 2 = 9,224,192 bytes more until the count lands (TestTheRoomBudgetsArithmetic).
func roomWeight(heap, content, structure int64) int64 {
	return (heap+content*bytesOfHeapAByteAtTheDensest)*roomCopies + structure
}

// copyHeap is the heap one copy of a document is priced at whose rendering is bytes long and makes
// elements elements (renderedReplica.size), and whose state holds structs structs (roomTotals, or
// a stored history's walk, storedStructs): its elements at bytesOfHeapAnElement, its rendered bytes
// at bytesOfHeapARenderedByte or its structs at bytesOfHeapAStructHeld, whichever is the most. A
// rendering goldmark stopped counting (elementsPastCounting) is priced from its bytes at the
// densest price in place of its elements and its bytes at the rendered-byte price, which is far
// under the densest, and one the room could not render (sizeUnmeasured) at nothing in place of
// them, so such a room is priced at its structs alone. Nothing caps it: a room is charged what its
// document holds, however heavy.
func copyHeap(bytes, elements, structs int64) int64 {
	rendered := int64(0)
	switch {
	case bytes == sizeUnmeasured || elements == sizeUnmeasured:
	case elements == elementsPastCounting:
		rendered = bytes * bytesOfHeapAByteAtTheDensest
	default:
		rendered = max(elements*bytesOfHeapAnElement, bytes*bytesOfHeapARenderedByte)
	}
	return max(rendered, structs*bytesOfHeapAStructHeld)
}
