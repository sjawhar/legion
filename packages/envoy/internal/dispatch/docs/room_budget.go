package docs

import (
	"cmp"
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// roomBudgetBytes is the most heap the open rooms are charged together (roomCharge): 2 GiB of the
// task's 4 GiB, under the 3.6 GiB Go memory limit the server takes from its container at boot
// (setGoMemoryLimit, cmd/dispatch/main.go:121-156). Open documents are the largest thing the pod
// holds; the rest is the request path's, the cold reads' cache's (documentReadBudget, 256 MiB,
// which this does not count) and the collector's slack. At the measured cost of a room
// (bytesOfHeapAnElement) it holds thirty-seven documents of a mebibyte of prose (57,600 elements,
// 58 MB each at both copies, roomCopies) as they load, and since an admission reserves its stored
// version's count at that same price (storedRoomReservation), an empty budget admits thirty-seven
// of them and refuses the thirty-eighth; what browsers add to a room after its load is charged
// beside that count (roomCharge), so rooms typed into hold fewer. A document whose history
// outgrows its rendering is priced at its structs instead (bytesOfHeapAStructHeld): six documents
// at the decode cap, 356,515,840 bytes each at both copies, whatever they render. A document of
// long runs of text, which makes few elements from few structs, is priced at its rendered bytes
// (bytesOfHeapARenderedByte): a paragraph of a mebibyte about 25 MB at both copies, where its 9
// elements would price it at 9,000 bytes. No room has a ceiling: a mebibyte of the densest
// markdown measured is priced at more than the whole budget (bytesOfHeapAByteAtTheDensest), and
// what answers a room that heavy is the hook's refusal of every insert while nothing can be closed
// (refusedUnderPressure), not a cap on its charge (TestTheRoomBudgetsArithmetic).
const roomBudgetBytes = 2 << 30

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
// (countRoomStructs, the count the room's struct total keeps, structTotal). A copy is priced at its
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
// the decode of its state merges the run into one item (squashRuns, sjawhar/ygo v1.54.1-sami.1,
// crdt/transaction.go:242-397), so a loaded copy holds 11 structs and 9 elements, priced at 4,500
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
// each as an item of its own the same way (squashRuns, sjawhar/ygo v1.54.1-sami.1,
// crdt/transaction.go:242-397), so a paragraph of a mebibyte loads as 11 structs making 9
// elements, 4,500 bytes a copy at the element price, while each copy holds its text.
//
// Measured on 2026-10-28 (TestAResidentRoomsHeapPerStructOfItsState), what one copy of a resident
// room holds for each byte of its rendering: a run of a million keystrokes, each stored as an item
// of its own, 10.2 bytes, 20,410,464 for both copies of 1,000,042 rendered bytes; runs of 50,000
// to 500,000 keystrokes 9.2 to 11.1, the most at 300,000. The decode appends each keystroke's item
// to its client's slice of the store before the merge, and the merge keeps that slice's array
// (crdt/store.go:56-58, crdt/transaction.go:300-321): eight bytes a keystroke and the slack append
// grew the array by, beside the character's own byte, so the figure moves with the run's length. A
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
// version, which is stored as its rendering (pmdoc/parse.go:120-122) - so what it is measured on is
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
// v1.54.1-sami.1, crdt/abstract_type.go:105-110), which takes each of those allocations into the
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
// already held (sjawhar/ygo v1.54.1-sami.1, crdt/content.go:52-70, :81-85, :114-120); a run of Any
// or JSON values is divided into a slice of its own, sixteen bytes an interface header for each
// value past the split (:170-174, :188-192), which the content price of the struct that carried
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
// charge to 65,618,920, 1.117 times the 58,735,880 bytes of heap the cross-checked reading of
// 2026-10-31 under sjawhar/ygo v1.54.1-sami.3 finds the room holding, where this price charges
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

// earlyCountCharge is the uncounted part of a room's charge (roomCharge) past which the room's next
// rendering is counted at once rather than once its last count is roomCountInterval old: 16 MiB of
// charge, about 7,900 bytes of added content at the densest price at both copies. Typing never
// reaches it - a person typing ten characters a second adds 150 bytes in the interval, 317,400
// bytes of charge - while a paste of 8 KiB does, and its room is counted within one count
// of the measurer's queue rather than up to fifteen seconds later. It is still one count at a time
// for the pod (elementMeasurer).
const earlyCountCharge = 16 << 20

// pressureReserveBytes is how far past the budget's limit the charges of the delete-only updates
// admitted while the budget is full may take it (refusedUnderPressure): 256 MiB. A delete that cuts
// the middle of an item splits it and adds structs, so unbounded deletes would grow the heap while
// nothing can be closed. A delete range is charged a split at both copies, 800 bytes, for each of
// its ends its apply can leave split (deletedRuns.splitEnds), two at most, where a split keeps
// about 154 bytes in each copy (TestTheRoomBudgetsSplitMarginHoldsWhatSplitsAdd), so deletes that
// fill the reserve hold about three quarters of it, 206 MB, in the heap.
//
// It is set from what A4 (cmd/dispatch, TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks) asks a
// full server to take, measured on its shapes
// (TestTheRoomBudgetsPressureReserveHoldsWhatAFullServerIsAskedToTake), and not left at the 64 MiB
// the plan proposed. What raises full is a sweep that finds the charges past the limit with
// nothing to close, and the update that took them past was admitted whole: one of A4's rounds of
// 2,048 headings is a structure part of 47,423,488 bytes and a count of 7,700,480 more, 55,123,968
// past the limit at most once its early count lands, close to the whole of 64 MiB by itself, and a
// second grower's round can be admitted before the sweep raises full. Its hundred shrinking
// updates - four cuts of fifty bytes from one browser's copy, each sent to twenty-five rooms, each
// carrying the cuts before it, of which only its own range is new to the room (deletedRuns) -
// charge 80,000 bytes. Three crossing rounds and the shrinks come to 165,451,904 bytes, inside the
// reserve with 103 MB to spare.
const pressureReserveBytes = 256 << 20

// admissionFlushAllowance is what an admission that finds the budget short waits for its sweep
// beyond the settle delay (Service.admissionWait): a room whose browser has just gone is not
// closable until a settlement has committed what it owes - the one its browser's leaving runs at
// once (settleLastPeer), or the one the settle delay after its last write arms - and the close
// that follows flushes the room's updates to the store and compacts them before the room's charge
// leaves (evictRoom). Eight seconds is over the 4.5 s one settlement of a mebibyte took on a loaded
// pod (service_test.go:2293), with the flush beside it.
const admissionFlushAllowance = 8 * time.Second

// errRoomBudgetFull is the refusal of a browser the open rooms leave no room for (reserveRoom). It
// is answered with DocumentBusyCloseCode, the transient close, and only once the admission has
// waited for the sweeps it started to close what they could (roomBudget.await): the next attempt
// finds the rooms open retiring by themselves, or the rooms whose browsers leave meanwhile.
var errRoomBudgetFull = errors.New("the open documents fill their memory budget")

// roomBudget charges every open room the heap it holds, and refuses a browser one more room past
// limit. It is written as the cold reads' cache is (documentReads): one mutex, a count against a
// budget, and a measured weight per entry, here a room's (roomCharge) and each admission's
// reservation before its room loads (roomReservation). It charges every room whoever opened it,
// and refuses only a browser's socket at its admission (reserveRoom): never Server.Apply, so the
// server's own work - an API write, which opens its room from the store (livewrite.go:327), the
// unrecorded-mark sweep, settlement's repair - opens its room over the budget, and a room so
// opened is closed once nothing holds it (closeOnceUnused, Service.closeIfOpenedOverBudget).
//
// Over the budget, or short of what the admissions waiting on it need (wanted), the rooms that
// hold nothing outlasting them are closed (sweepRooms), and an admission that finds the budget
// short waits for those closes before it is refused (await). When the budget is still over its
// limit with none left to close, full is raised: the one signal the peer update hook reads
// (Service.weighPeerUpdate), with one atomic load, since the hook runs on the peer's read loop and
// takes neither this mutex nor a room's state lock. It is lowered once the rooms' charges are back
// within the budget, and a sweep started once a room may have become closable lowers it too.
type roomBudget struct {
	mu sync.Mutex
	// limit is roomBudgetBytes, but for a browser test's own (SetRoomBudgetForTest), and used
	// what the rooms' charges and the reservations come to.
	limit, used int64
	// rooms holds, per document, the charges of its loaded instances and the reservations of the
	// sockets being admitted to it; a document with neither has no entry.
	rooms map[string]*budgetedRoom
	// sweeping is set while a sweep runs, so one runs at a time, and again is set by anything that
	// would have started one meanwhile - a room that may have become closable, an admission that
	// began to wait - so that the sweep runs once more if the budget is still short when it ends
	// (swept): the running sweep may have looked at that room, or weighed the budget, before.
	sweeping, again bool
	// sweep closes the rooms nothing holds until the budget is met (Service.sweepRooms), started
	// on a goroutine of its own. Nil in a budget no service built, which never sweeps.
	sweep func()
	// wanted is what the admissions waiting on the budget need together (await): what each would
	// reserve, beside the limit itself, which the sweeps close rooms to make room for (short), and
	// waiting how many there are. Without it a sweep would stop as soon as the budget is back
	// within its limit, and a reservation never takes it past: the sweep a short admission starts
	// would close nothing, and the admission would be refused while unused rooms stayed charged
	// until each retired on its own a minute later. Each admission takes its own weight back when
	// it stops waiting, admitted or refused; no sweep's end clears it while one still waits.
	wanted  int64
	waiting int
	// waiters is waiting, for the reader that takes no lock (Service.unlockState): while an
	// admission waits, a room that becomes closable starts a sweep, as it does while full is up.
	waiters atomic.Int32
	// pending is whether the last sweep to end left a charged room that something other than a
	// connected browser holds - a settlement, a writer, an append - and that will be closable once
	// that ends; an admission still short when no sweep runs and none is pending is refused at once.
	pending bool
	// changed is closed, and replaced, by every change to the budget while an admission waits, so
	// each waiting admission weighs the budget again (await).
	changed chan struct{}
	// marked is how many loaded charges are closeOnceUnused, for the reader that takes no lock.
	marked atomic.Int32
	// full is raised by a sweep that leaves the budget over its limit, having closed every room it
	// could; never by one that leaves it within its limit but short of wanted (swept).
	full atomic.Bool
	// stored holds the counts admissions made of the stored versions (storedRoomReservation).
	stored storedCounts
}

// budgetedRoom is what the budget holds for one document: charges, the instances of its room ygo
// has loaded and not yet retired, oldest first - more than one only while a retired instance's
// unload has not fired yet when its successor loads - and reservations, the sockets admitted to
// it whose room has not loaded.
type budgetedRoom struct {
	charges      []*roomCharge
	reservations []*roomReservation
}

// roomCharge is one loaded room's charge (weight), the sum of three parts (roomWeight):
//
//   - the counted part: its latest measured document, priced from the count of its rendering, the
//     rendering's bytes and the struct total of its state (copyHeap), at both copies. Each count
//     replaces the last rather than adding to it, so a document that shrinks is charged less.
//   - the uncounted part: the content bytes the room's transactions have added since the rendering
//     that count measured (contentAdded), each at the densest price at both copies, since what
//     elements they make is unknown until a count. Every transaction is charged it once, whoever
//     wrote it - a browser's admitted update, the server's own write, an identity-closure repair -
//     by the update observer whose drain of the room's update log hands it over (chargeAdded). A
//     count re-bases it: it clears the bytes added up to the rendering it measured, never those
//     added since. A whole-state handoff of a document the room already holds adds nothing here.
//   - the structure part: what each update the room integrates adds that a count of the rendering
//     cannot see (peerWeighing.structureHeap) - each new struct at bytesOfHeapAStruct, the splits
//     its integration can cause and the splits each delete range can leave at its ends, none at an
//     end the room already held deleted (deletedRuns.splitEnds), at bytesOfHeapASplit, and the
//     content no rendering prices at its kind's heap a byte - at both copies. A browser's update is
//     charged it by the peer hook that admitted it (grew); every other transaction - an API write's
//     publish, an injected update, settlement's repair, any of the server's own writes - by the
//     update observer whose drain of the room's update log hands it over (wrote), at the same
//     prices, as each of their scans counts toward the room's scan total (roomScans.observed). The
//     server's writes are never refused for it: they only raise the charge, so the budget refuses
//     browsers or closes unused rooms sooner. A count never clears it: one update of GC structs
//     keeps tens of megabytes with the rendering unchanged. Only the room's retirement drops it,
//     with the rest of the charge.
//
// Two parts can charge one struct: the structure part charges what each update adds, and a count
// prices the struct total of the whole state once its structs outweigh its elements (copyHeap),
// which every struct the structure part charged is part of. The structure part over-charges what
// merges back too: ygo squashes a run of typed characters into one item and re-merges a split
// nothing came between (sjawhar/ygo v1.54.1-sami.1, crdt/transaction.go:26-31), while each struct
// is charged as one of its own with both its splits; and it charges every struct at the heaviest
// kind's price, rendered or not, so a rendered struct is charged here beside its count's price of
// the elements it makes. What deletes leave behind - tombstones a rendering does not show - is
// priced between counts only as far as the structure part charged their ranges' splits, and by
// the next count with every other struct of the state.
//
// What a room is charged has a lifecycle. Its admission reserves the count of its stored version
// and the structs of its stored history at both copies (roomReservation); its load, once the
// rendering it loaded with is counted (keepReplica), replaces that reservation with this charge
// (load); the room's update observer charges each transaction's added content (adds) and the
// structure of each transaction the hook did not apply (wrote), the hook each admitted update's
// structure (grew); and each count the measurer makes after that replaces the counted part
// (measured), on its cadence or sooner once the uncounted part passes earlyCountCharge. A rendering
// published without a count of its own (renderedReplica.publishSize) re-bases nothing.
type roomCharge struct {
	budget *roomBudget
	room   string
	// heap is what one copy of the room's latest measured document is priced at (copyHeap).
	heap int64
	// content is the bytes of content the room's transactions have added since it loaded, and
	// measuredAt what it was at the rendering heap prices.
	content, measuredAt int64
	// structure is the structure part, at both copies already.
	structure int64
	weight    int64
	retired   bool
	// closeOnceUnused is set on a room loaded with no socket's reservation while the budget is
	// over its limit, which the server's own work opened over it; it is closed once unused
	// (Service.closeIfOpenedOverBudget) unless a browser joins it first (join).
	closeOnceUnused bool
	// deleted is the delete ranges the room is known to hold deleted, which the structure part
	// and the struct total charge nothing for again (deletedRuns). It has its own lock, never the
	// budget's.
	deleted deletedRuns
}

// deletedRuns is the delete ranges a room is known to hold deleted, each client's sorted and
// merged: the room's whole delete set at its load (seed, crdt.DeleteSetFromDoc), and every range of
// every update the room has been charged the structure of since - a browser's update the peer hook
// admitted, a transaction of the server's own (roomCharge.wrote) - with the GC runs and deleted
// items it integrated (record). A clock it lists that the room holds is deleted; one the room does
// not hold yet is in a range the room keeps parked, since ygo parks the part of a range past the
// clocks it holds (sjawhar/ygo v1.54.1-sami.1, crdt/delete_set.go:385-397, crdt/update.go:783-786)
// and an update is listed only once its apply has returned. A delete range it covers whole adds no
// struct, and one that overlaps what it lists none at an end inside it (splitEnds), so neither the
// structure part nor the struct total charges a browser's whole-state handoff for the delete set it
// carries. Without it the structure part, which no count clears, would charge the room's whole
// delete set again on every browser's sync, whose whole-state handoff carries it, and on every
// transaction, whose update ygo encodes with the room's whole delete set (crdt/doc.go:597-612): an
// edited room held open across reconnects would grow its charge with nothing added to its heap.
// And the struct total would count two structs for every range of it on every connect, which
// refused every browser reconnecting to a room at the decode cap before it had edited anything.
//
// A range of an update whose apply failed is neither listed nor charged, since the room's bounded
// document took that update back out whole (newRoomScans). Two updates weighed at once - two
// browsers', or a browser's whose ranges the hook lists only once its apply returns and a server
// write committed meanwhile - can each find the same new range unlisted, and both are charged it,
// which over-charges and never under-charges. The list holds sixteen bytes for each run of the
// room's delete set, beside the tombstone of at least one struct, 129 bytes or more
// (bytesOfHeapAStruct), that each run deletes, and it goes with the room.
type deletedRuns struct {
	mu      sync.RWMutex
	clients map[uint64][]clockRun
}

// clockRun is the clocks [start, end) of one client.
type clockRun struct{ start, end uint64 }

// seed lists every range of set, the room's delete set at its load.
func (d *deletedRuns) seed(set *crdt.IDSet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, client := range set.Clients() {
		for _, run := range set.Ranges(client) {
			d.addLocked(uint64(client), run.Clock, run.Len)
		}
	}
}

// covers reports whether the room is known to hold clocks [clock, clock+length) of client deleted
// already. A range of no clocks splits nothing (crdt/delete_set.go:170), and is covered. Nil, a
// room no load charged, covers nothing.
func (d *deletedRuns) covers(client, clock, length uint64) bool {
	if length == 0 {
		return true
	}
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	runs := d.clients[client]
	at := sort.Search(len(runs), func(i int) bool { return runs[i].start > clock }) - 1
	return at >= 0 && runs[at].end >= clock+length
}

// splitEnds reports whether applying a delete range of clocks [clock, clock+length) of client can
// leave split for good the item holding its start and the item holding its end, the only items
// ygo's apply splits for it: it cleans the range's two ends (getItemCleanStart at clock and at
// clock+length, crdt/delete_set.go:170-172 and crdt/store.go:123-140), deletes whole each item
// between them (:182-208) and parks the part of the range past the clocks the room holds
// (:209-215). ygo sorts and merges each client's ranges as it decodes them (crdt/update.go:1324),
// so every range it applies ends where a range of the update ends. A range of no clocks splits
// nothing (crdt/delete_set.go:170).
//
// An end x can be left split only where the list holds neither clock x-1 nor clock x, and never at
// clock 0, where a client's first item starts. ygo keeps an item deleted whole or not at all: a
// delete cleans its ends first, and a merge joins only two items of one deleted state
// (tryMergeWithLeft, crdt/transaction.go:480). So where x-1 and x are held, one deleted and the
// other not, they lie in two items and x is clean already. Where both are held deleted, the split
// at x cuts a deleted item into two deleted halves, which ygo merges back as the transaction ends:
// splitItem records the right half (crdt/item.go:556-559) and tryMergeWithLefts merges it with its
// left (crdt/transaction.go:374-402, run at commit, crdt/doc.go:820-824), since the halves share
// client, parent, origins and deleted state, and an item that can split - a deleted run, or a run
// of string, Any or JSON content - holds content that merges (crdt/transaction.go:513-544), every
// other content being one clock long. Only an item integrated between the halves keeps them
// apart, and ygo's integrate puts one there only when it names clock x-1 as its origin or clock x
// as its right origin (crdt/item.go:104-106 and :152-191: past the left half, its scan always
// passes the right half, whose origin is the left half's last clock), which its own integrate
// cleans at x, a split its own count holds (splitsAStructCanCause).
//
// Where x is not held, no item holds it: a room holds each client's clocks from 0 up, and
// integrates a struct reaching past them from the first clock it does not hold (crdt/item.go:
// 83-91), so where x-1 is held the item that brings x starts at x. Where x-1 is not held either,
// one item the same transaction integrates can hold both; what the list holds of them is then in
// ranges the room keeps parked, which ygo deletes in that transaction once their clocks arrive
// (drainParked, crdt/update.go:588). Where x-1 and x both end deleted, the halves at x merge back
// as above; where only one does, x is where a parked run starts or ends, a split charged to the
// range that first listed a clock beside x, which had its own end at x with neither clock listed -
// and the room's count holds splitsAStructCanCause structs for each parked range besides
// (structTotalOf). So each place a listed run starts or ends is split once at most, by a range
// charged for it; and the list only grows, so a range weighed against the list before its apply is
// charged no less than against the list its apply meets. Nil, a room no load charged, lists
// nothing.
func (d *deletedRuns) splitEnds(client, clock, length uint64) (start, end bool) {
	if length == 0 {
		return false, false
	}
	if d == nil {
		return clock > 0, true
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	runs := d.clients[client]
	// beside is whether the list holds clock x-1 or clock x: a run starting at or before x that
	// ends at or after it, of which only the last run starting at or before x can be one.
	beside := func(x uint64) bool {
		at := sort.Search(len(runs), func(i int) bool { return runs[i].start > x }) - 1
		return at >= 0 && runs[at].end >= x
	}
	return clock > 0 && !beside(clock), !beside(clock + length)
}

// record lists every range of the delete set of update, which a walk found at deletesAt
// (updateTally.deletesAt), once the room has been charged its structure - a browser's update the
// peer hook admitted, or a transaction of the server's own (roomCharge.wrote); and, where born says
// the update carries structs that integrate deleted - a GC run, a deleted item, which the delete
// set of every encode of the room's state lists from then on (buildDeleteSet,
// crdt/update.go:502-515) though no delete range named them - their clocks too, from a walk of the
// whole update rather than of its delete set alone. It walks as a room's own state is walked
// (fromRoom), which refuses only what ygo cannot decode: the server's writes can carry what no
// browser sends, and an update the hook admitted passed the stricter walk already. Nil records
// nothing.
func (d *deletedRuns) record(update []byte, deletesAt int, born bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if born {
		_, _ = walkUpdate(update, fromRoom, (*deletedRunsRecorder)(d))
		return
	}
	_ = walkDeleteSet(update, deletesAt, fromRoom, (*deletedRunsRecorder)(d))
}

// deletedRunsRecorder is record's visitor, which its caller holds the lock for.
type deletedRunsRecorder deletedRuns

func (r *deletedRunsRecorder) walkedStruct(walked updateStruct) {
	if bornDeleted(walked) {
		(*deletedRuns)(r).addLocked(walked.client, walked.clock, walked.length)
	}
}

func (r *deletedRunsRecorder) walkedDelete(client, clock, length uint64) {
	(*deletedRuns)(r).addLocked(client, clock, length)
}

// bornDeleted is whether walked integrates as a deleted struct: a GC run or a deleted item.
func bornDeleted(walked updateStruct) bool {
	return walked.kind == structGC || walked.kind == structDeleted
}

// addLocked lists clocks [clock, clock+length) of client, merged into the runs it overlaps or
// touches. Its caller holds mu.
func (d *deletedRuns) addLocked(client, clock, length uint64) {
	if length == 0 {
		return
	}
	if d.clients == nil {
		d.clients = make(map[uint64][]clockRun)
	}
	start, end := clock, clock+length
	runs := d.clients[client]
	first := sort.Search(len(runs), func(i int) bool { return runs[i].end >= start })
	last := first
	for last < len(runs) && runs[last].start <= end {
		start, end = min(start, runs[last].start), max(end, runs[last].end)
		last++
	}
	d.clients[client] = slices.Replace(runs, first, last, clockRun{start, end})
}

// deletes is the delete ranges the room is known to hold deleted (deletedRuns); nil for a nil
// charge, a room no load charged.
func (c *roomCharge) deletes() *deletedRuns {
	if c == nil {
		return nil
	}
	return &c.deleted
}

// roomReservation is a socket's admission to a room that has not loaded: the count of the stored
// version at both copies (storedRoomReservation), held from the admission until the room loads,
// when the room's own charge replaces it, or until the socket's handler returns, whichever comes
// first.
type roomReservation struct {
	room    string
	weight  int64
	settled bool
}

// roomCopies is the copies of its document a room is charged at, from the reservation its
// admission makes (storedRoomReservation) through its load (load) to its retirement (retire): its
// live document and the rendered replica its update observer keeps (renderedReplica), which the
// room holds from its load on, the copy its load decoded of the stored state (keepReplica). Both
// are charged before the load holds either, since an admission reserves the room as it will stand:
// charged one copy, rooms would be admitted at half their price, an admission of a mebibyte of
// prose reserving 28,800,000 bytes rather than 57,600,000, and an empty budget admitting
// seventy-four of them rather than thirty-seven, whose open rooms would come to about twice the
// 2 GiB budget in a 4 GiB pod. Every part of a charge is at both copies: the replica holds the
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
// elements elements (renderedReplica.size), and whose state holds structs structs (structTotal, or
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

// join reports whether room has a loaded instance the budget charges, which a socket joins with
// nothing reserved (joinedLocked); over the limit it starts a sweep, since the socket's room may be
// one of many that something has stopped using.
func (b *roomBudget) join(room string) bool {
	b.mu.Lock()
	defer b.unlock()
	if !b.joinedLocked(room) {
		return false
	}
	if b.used > b.limit {
		b.startSweepLocked()
	}
	return true
}

// joinedLocked reports whether room has a loaded instance the budget charges, and if it has, hands
// it to the socket joining it: a room the server opened over the budget is a browser's from here
// on, closed by the sweep or its own retirement like any other, never because the server's work in
// it ended (closeOnceUnused). Its caller holds mu.
func (b *roomBudget) joinedLocked(room string) bool {
	entry := b.rooms[room]
	if entry == nil || len(entry.charges) == 0 {
		return false
	}
	for _, charge := range entry.charges {
		b.unmarkLocked(charge)
	}
	return true
}

// refusal is a budget past its limit refusing an admission before it weighs anything, once the
// sweeps the admission starts have closed what they could before deadline or ctx ends (await);
// nil within the limit, at once or by then.
func (b *roomBudget) refusal(ctx context.Context, deadline time.Time) error {
	b.mu.Lock()
	defer b.unlock()
	if b.await(ctx, deadline, 0, func() bool { return b.used <= b.limit }) {
		return nil
	}
	return fmt.Errorf("%w: %d of %d bytes are charged", errRoomBudgetFull, b.used, b.limit)
}

// reserve holds weight for a socket admitted to room, or reserves nothing for a room already
// charged, which the socket joins (joinedLocked). Where weight would take the budget past its
// limit, the admission waits on the budget (await): weight is what the sweeps it starts are to make
// room for (wanted), and the admission is refused only once they have closed what they could, the
// rooms whose browsers have just gone among them, or once deadline or ctx ends, reserving nothing.
// A weight past the limit itself is refused at once, since no sweep can make room for it, and
// closing every unused room to try would only cost their next readers a load.
func (b *roomBudget) reserve(ctx context.Context, deadline time.Time, room string, weight int64) (*roomReservation, error) {
	b.mu.Lock()
	defer b.unlock()
	charged := func() bool {
		entry := b.rooms[room]
		return entry != nil && len(entry.charges) > 0
	}
	if weight <= b.limit {
		b.await(ctx, deadline, weight, func() bool { return charged() || b.used+weight <= b.limit })
	}
	if b.joinedLocked(room) {
		return nil, nil
	}
	if b.used+weight > b.limit {
		return nil, fmt.Errorf("%w: %d of %d bytes are charged, and this document needs %d", errRoomBudgetFull, b.used, b.limit, weight)
	}
	entry := b.entry(room)
	reservation := &roomReservation{room: room, weight: weight}
	entry.reservations = append(entry.reservations, reservation)
	b.used += weight
	return reservation, nil
}

// await waits until fits reports room for an admission needing weight, and reports whether it
// does. Its caller holds mu, which it gives up while it waits and holds again whenever it looks
// and when it returns. While it waits, weight is wanted, so the sweeps close rooms for it as well
// as for the limit (short), and a room that becomes closable meanwhile starts a sweep of its own
// (waiters, Service.unlockState). It starts a sweep, or has the running one run again once it
// ends, since that one may have weighed the budget before this weight was wanted (sweepAgainLocked).
//
// It stops waiting, answering what fits says then, once deadline passes or ctx ends; and at once
// when no sweep runs and the last one to end left nothing pending (pending): the rooms still
// charged are held by browsers or by nothing at all, and nothing will free them in time. Every
// change to the budget wakes it to look again (unlock, swept). A budget no service built never
// sweeps, so it answers at once.
//
// What the sweeps know of the browsers is the room states' (unusedLocked, onlyDepartedLocked),
// and those lag the sockets by ygo's teardown: ygo takes a leaving peer out of its room, flushes
// the room's pending updates to the store and only then reports the departure (sjawhar/ygo
// v1.54.1-sami.3, provider/websocket/peer.go:517-663, OnLastPeer), which is what marks the room's
// browsers departed (settleLastPeer). An admission that weighs the budget inside that window
// finds every charged room held by a browser, nothing pending, and is refused at once though the
// room it needs is already on its way out; its retry, which the transient close asks the editor
// for (errRoomBudgetFull), meets the room pending and waits for it. Nothing the service can read
// closes that window - ygo offers no peer count and no per-peer leave - so the drive that opens a
// browser just after another left holds its socket until the report has landed
// (cmd/dispatch, TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays).
func (b *roomBudget) await(ctx context.Context, deadline time.Time, weight int64, fits func() bool) bool {
	if fits() {
		return true
	}
	b.waiting++
	b.wanted += weight
	b.waiters.Store(int32(b.waiting))
	defer func() {
		b.waiting--
		b.wanted -= weight
		b.waiters.Store(int32(b.waiting))
	}()
	b.sweepAgainLocked()
	wait := time.Until(deadline)
	if wait <= 0 {
		return fits()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for !fits() {
		if !b.sweeping && !b.pending {
			return false
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
			b.mu.Lock()
		case <-timer.C:
			b.mu.Lock()
			return fits()
		case <-ctx.Done():
			b.mu.Lock()
			return fits()
		}
	}
	return true
}

// release returns reservation, unless its room's charge already replaced it. Nil releases nothing.
func (b *roomBudget) release(reservation *roomReservation) {
	if reservation == nil {
		return
	}
	b.mu.Lock()
	defer b.unlock()
	if reservation.settled {
		return
	}
	entry := b.rooms[reservation.room]
	entry.reservations = slices.DeleteFunc(entry.reservations, func(held *roomReservation) bool { return held == reservation })
	b.settleLocked(reservation)
	b.dropIfEmptyLocked(reservation.room, entry)
}

// load charges room's newly loaded instance, whose one copy is priced at heap (copyHeap) from the
// count of the rendering it loaded with and the struct total of its state, at roomCopies, in place
// of every reservation its admissions hold: the room's own document replaces its stored version at
// the same two copies, and the two documents differ only by the updates stored since that version
// was cut. A room loaded with no socket's reservation is the server's own work's - an API write's
// publish (livewrite.go:327), settlement's repair and warm-up, the mark sweep, a mark's
// verification, a snapshot read, a warm-up, a live tree's read, settlement resumed at boot: every
// opener of Server.Apply - and where its charge leaves the budget over its limit it is marked
// closeOnceUnused, to be closed once that work no longer holds it (Service.closeIfOpenedOverBudget)
// rather than staying charged until it retires a minute later.
func (b *roomBudget) load(room string, heap int64) *roomCharge {
	b.mu.Lock()
	defer b.unlock()
	entry := b.entry(room)
	reserved := len(entry.reservations) > 0
	for _, reservation := range entry.reservations {
		b.settleLocked(reservation)
	}
	entry.reservations = nil
	charge := &roomCharge{budget: b, room: room, heap: heap}
	entry.charges = append(entry.charges, charge)
	b.reweighLocked(charge)
	if !reserved && b.used > b.limit {
		charge.closeOnceUnused = true
		b.marked.Add(1)
	}
	return charge
}

// charged moves the room's charge by content in its uncounted content part and by structure in
// its structure part, and re-weighs its entry under the budget's lock: the one body the four
// charges below share. A nil charge, a replica no room's load charged (a test's), charges
// nothing, and so does a move of nothing in either part.
func (c *roomCharge) charged(content, structure int64) {
	if c == nil || (content == 0 && structure == 0) {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.content += content
	c.structure += structure
	c.budget.reweighLocked(c)
}

// adds charges the room content bytes of content a transaction added (contentAdded) to its
// uncounted part, at the densest price at both copies until a count of a rendering holding them
// re-bases it (measured). Nil, a replica no room's load charged (a test's), charges nothing.
func (c *roomCharge) adds(content int64) {
	c.charged(content, 0)
}

// grew charges the room structure more in its structure part, which no count clears: what an
// update the peer hook admitted adds that the rendering does not price (peerWeighing.
// structureHeap). Nil charges nothing.
func (c *roomCharge) grew(structure int64) {
	c.charged(0, structure)
}

// wrote charges the room what update added, one transaction's update the peer hook did not apply -
// an API write's publish, an injected update, settlement's repair, any of the server's own writes:
// its content a rendering can show to the uncounted part, as contentAdded counts it, and its
// structure to the structure part, priced as the hook prices a browser's update
// (peerWeighing.structureHeap). Every struct the update carries is new to the room, since ygo
// encodes a transaction's update from the state before it (sjawhar/ygo v1.54.1-sami.1,
// buildPhase2, crdt/doc.go:591-595), so each is charged with the splits its integration can cause,
// with the splits each delete range can leave at its ends (deletedRuns.splitEnds) and
// the content no rendering prices. It refuses nothing: the write has committed, and its charge only
// raises the budget's figures, so the budget refuses browsers and closes unused rooms sooner. The
// update's delete ranges, and the structs it integrated deleted, are listed as the room's
// (deletedRuns.record), once the charge has given the budget's lock back. One walk reads the
// content and the structure both. An update the walk cannot read is charged its bytes as content,
// as contentAdded charges it, and the structure the walk read before it refused. Nil, a replica no
// room's load charged (a test's), charges nothing and walks nothing.
func (c *roomCharge) wrote(update []byte) {
	if c == nil {
		return
	}
	weighed := writtenWeighing{structure: peerWeighing{known: c.deletes()}}
	tally, err := walkUpdate(update, fromRoom, &weighed)
	content := weighed.content.bytes
	if err != nil {
		content = int64(len(update))
	}
	c.charged(content, weighed.structure.structureHeap())
	if err == nil {
		c.deleted.record(update, tally.deletesAt, weighed.structure.born)
	}
}

// writtenWeighing is wrote's visitor: the content a rendering can show (contentAdded's
// renderedContent) and the structure (peerWeighing) of one walk of a server write's update. Its
// peerWeighing holds no state vector, so every struct counts as past the room's state, as every
// struct of a transaction's update is.
type writtenWeighing struct {
	content   renderedContent
	structure peerWeighing
}

// walkedStruct weighs walked as the peer hook's weighing does, but for the apply-heap estimate
// (structApplyHeap), which wrote never reads: it reads the structure (structureHeap) and whether
// a struct integrated deleted (born) alone, so the estimate's re-walk of Any and JSON content is
// not paid for a server write.
func (w *writtenWeighing) walkedStruct(walked updateStruct) {
	w.content.walkedStruct(walked)
	w.structure.weighStruct(walked, applyCost{})
}

func (w *writtenWeighing) walkedDelete(client, clock, length uint64) {
	w.structure.walkedDelete(client, clock, length)
}

// gaveBack takes structure back out of the room's structure part: what growWithin charged before
// an apply under pressure for an update whose apply then failed, which the room's bounded document
// took back out whole (newRoomScans), so the room holds none of what it was charged for. Nil
// gives back nothing.
func (c *roomCharge) gaveBack(structure int64) {
	c.charged(0, -structure)
}

// growWithin charges charge structure more, as grew does, where the charges and reservations then
// stay within the limit and reserve beyond it, and reports whether it did: the pressure reserve's
// look at the budget (refusedUnderPressure), which books the charge in the same look so that two
// browsers' deletes weighed at once cannot each pass the check and together pass the reserve. A
// nil charge, a room no load charged, is weighed and charges nothing.
func (b *roomBudget) growWithin(charge *roomCharge, structure, reserve int64) bool {
	b.mu.Lock()
	defer b.unlock()
	if b.used+structure > b.limit+reserve {
		return false
	}
	if charge != nil {
		charge.structure += structure
		b.reweighLocked(charge)
	}
	return true
}

// contentTaken is the content the room's transactions have added since it loaded, which a
// rendering published now holds, so its count, once it lands, re-bases the uncounted part from
// there (measured); and whether the uncounted part has passed earlyCountCharge, so the rendering is
// to be counted at once. Zero and false for a replica no room's load charged (a test's).
func (c *roomCharge) contentTaken() (content int64, early bool) {
	if c == nil {
		return 0, false
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.content, (c.content-c.measuredAt)*bytesOfHeapAByteAtTheDensest*roomCopies >= earlyCountCharge
}

// measured re-bases the room's counted part on heap (copyHeap), the price of a count of the
// rendering published when the room had added content bytes of content: the content added before
// that rendering is in the count, and only what was added since stays uncounted beside it. The
// structure part stays as it is. Only a count lands here (renderedReplica.storeCounted); a
// rendering whose bytes alone are published re-bases nothing. A replica no room's load charged has
// nothing to re-base.
func (c *roomCharge) measured(heap, content int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.heap = heap
	c.measuredAt = content
	c.budget.reweighLocked(c)
}

// retire returns the charge of room's oldest loaded instance, which ygo has retired
// (releaseUnloadedRoom), all three of its parts. ygo fires one unload for each instance it loaded,
// and loads the next only once the last is out of its map, so the oldest charge is the retired
// instance's - or, where two retire before either unload fires, the other's, which is the same
// total.
func (b *roomBudget) retire(room string) {
	b.mu.Lock()
	defer b.unlock()
	entry := b.rooms[room]
	if entry == nil || len(entry.charges) == 0 {
		return
	}
	charge := entry.charges[0]
	entry.charges = slices.Delete(entry.charges, 0, 1)
	b.unmarkLocked(charge)
	charge.retired = true
	b.used -= charge.weight
	b.dropIfEmptyLocked(room, entry)
}

// takeMark reports whether room has a loaded charge marked closeOnceUnused, and unmarks it: the
// one close the mark asks for is the caller's to make (Service.closeIfOpenedOverBudget).
func (b *roomBudget) takeMark(room string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.rooms[room]
	if entry == nil {
		return false
	}
	marked := false
	for _, charge := range entry.charges {
		marked = charge.closeOnceUnused || marked
		b.unmarkLocked(charge)
	}
	return marked
}

// isMarked reports whether room has a loaded charge marked closeOnceUnused.
func (b *roomBudget) isMarked(room string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.rooms[room]
	return entry != nil && slices.ContainsFunc(entry.charges, func(charge *roomCharge) bool { return charge.closeOnceUnused })
}

// markAgain marks room's loaded charges closeOnceUnused again, for a close takeMark unmarked them
// for that could not be made: an Apply was inside its callback on the room (closeUnused). A room
// whose charge has retired meanwhile takes no mark, having no charge to close. Only
// Service.closeIfOpenedOverBudget calls it, with the mark it took.
func (b *roomBudget) markAgain(room string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.rooms[room]
	if entry == nil {
		return
	}
	for _, charge := range entry.charges {
		if !charge.closeOnceUnused && !charge.retired {
			charge.closeOnceUnused = true
			b.marked.Add(1)
		}
	}
}

func (b *roomBudget) unmarkLocked(charge *roomCharge) {
	if charge.closeOnceUnused {
		charge.closeOnceUnused = false
		b.marked.Add(-1)
	}
}

// short is whether the charges and reservations leave no room for wanted, what the admissions
// waiting on the budget need together: past the limit, or within it but without room for them. A
// sweep closes rooms while it holds.
func (b *roomBudget) short() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used+b.wanted > b.limit
}

// figures is what the rooms' charges and the reservations come to, the limit, and wanted, for a
// sweep to log what each room it closes left.
func (b *roomBudget) figures() (used, limit, wanted int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.limit, b.wanted
}

// heaviest lists the charged rooms, the heaviest first, for a sweep to close in that order.
func (b *roomBudget) heaviest() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	weights := make(map[string]int64, len(b.rooms))
	for room, entry := range b.rooms {
		for _, charge := range entry.charges {
			weights[room] += charge.weight
		}
	}
	rooms := make([]string, 0, len(weights))
	for room := range weights {
		rooms = append(rooms, room)
	}
	slices.SortFunc(rooms, func(a, b string) int { return cmp.Compare(weights[b], weights[a]) })
	return rooms
}

// kick is a report that room may have become closable: its state holds nothing more while the
// budget is full or an admission waits on it (Service.unlockState), or a Server.Apply on it has
// returned, which the budget's closes give up in front of (Service.roomApplied, closeUnused).
// While the budget is short and room is charged, it starts a sweep, or has the running one run
// again once it ends (sweepAgainLocked), since that one may have passed the room while something
// still held it.
// A state, or an apply, on a document the budget does not charge has no room to close, and starts
// nothing.
func (b *roomBudget) kick(room string) {
	b.mu.Lock()
	defer b.unlock()
	if entry := b.rooms[room]; entry == nil || len(entry.charges) == 0 || b.used+b.wanted <= b.limit {
		return
	}
	b.sweepAgainLocked()
}

// setLimit replaces the budget's limit, and sweeps or lowers full against the new one.
func (b *roomBudget) setLimit(limit int64) {
	b.mu.Lock()
	defer b.unlock()
	b.limit = limit
	if b.used > b.limit {
		b.startSweepLocked()
	}
}

// unlock ends a change to the budget: within the limit, full is lowered, since a room the hook
// refuses growth to can now grow; over it, with full down, a sweep is started, since a charge has
// just passed the limit. With full up a change does not sweep: the last sweep found nothing to
// close, and what can change that is a room becoming closable, which starts a sweep of its own
// (unlockState). The admissions waiting on the budget are woken to weigh it again (await).
func (b *roomBudget) unlock() {
	if b.used <= b.limit {
		b.full.Store(false)
	} else if !b.full.Load() {
		b.startSweepLocked()
	}
	if b.waiting > 0 {
		b.wakeLocked()
	}
	b.mu.Unlock()
}

// wakeLocked wakes every admission waiting on the budget (await). Its caller holds mu.
func (b *roomBudget) wakeLocked() {
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
}

// startSweepLocked starts a sweep unless one runs or the budget has none. Its caller holds mu.
func (b *roomBudget) startSweepLocked() {
	if b.sweeping || b.sweep == nil {
		return
	}
	b.sweeping = true
	go b.sweep()
}

// sweepAgainLocked starts a sweep, or, while one runs, has it run once more when it ends (again).
// Its caller holds mu.
func (b *roomBudget) sweepAgainLocked() {
	if b.sweeping {
		b.again = true
		return
	}
	b.startSweepLocked()
}

// swept ends a sweep, which leaves pending what pending says (sweepRooms). Where something asked
// for the sweep again while it ran and the budget is still short, it runs again at once, on a
// goroutine of its own. Otherwise it raises full when it left the budget over its limit: it closed
// every room it could. A sweep that leaves the budget within its limit but short of wanted lowers
// full rather than raising it. Full refuses every insert in every room (Service.weighPeerUpdate),
// and a single oversized document refused on an almost empty pod must not freeze every room's
// growth; an admission still short is answered by its own refusal once it stops waiting.
//
// wanted is left as it is: each admission waiting takes its own weight back when it stops waiting
// (await), so a sweep's end never drops what another admission still waits for. Every waiting
// admission is woken to weigh the budget again.
func (b *roomBudget) swept(pending bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = pending
	b.wakeLocked()
	if b.again && b.used+b.wanted > b.limit {
		b.again = false
		go b.sweep()
		return
	}
	b.again = false
	b.sweeping = false
	b.full.Store(b.used > b.limit)
}

func (b *roomBudget) entry(room string) *budgetedRoom {
	if b.rooms == nil {
		b.rooms = make(map[string]*budgetedRoom)
	}
	entry := b.rooms[room]
	if entry == nil {
		entry = &budgetedRoom{}
		b.rooms[room] = entry
	}
	return entry
}

func (b *roomBudget) dropIfEmptyLocked(room string, entry *budgetedRoom) {
	if len(entry.charges) == 0 && len(entry.reservations) == 0 {
		delete(b.rooms, room)
	}
}

func (b *roomBudget) settleLocked(reservation *roomReservation) {
	reservation.settled = true
	b.used -= reservation.weight
}

// reweighLocked charges charge its weight now, unless its room has retired. Its caller holds mu.
func (b *roomBudget) reweighLocked(charge *roomCharge) {
	if charge.retired {
		return
	}
	weight := roomWeight(charge.heap, charge.content-charge.measuredAt, charge.structure)
	b.used += weight - charge.weight
	charge.weight = weight
}

// contentAdded is the bytes of content update, one transaction's update as the room's update
// observer is handed it, added that a rendering can show: the content of each struct it carries,
// every one of them new to the room, since ygo encodes a transaction's update from the state
// before it (sjawhar/ygo v1.54.1-sami.1, buildPhase2, crdt/doc.go:591-595), whoever wrote it - a
// browser's update the peer hook admitted, or the server's own write. Its delete set, which ygo
// fills with every deleted item of the room on every transaction, adds nothing and is not counted:
// charging the update's bytes, as the budget did, charged a heavily edited room its whole delete
// set again on every keystroke. So a whole-state handoff of a document the room already holds,
// which integrates nothing, adds nothing; and a struct parked until what it waits for arrives
// counts in the transaction that integrates it.
//
// What no rendering shows is left out: a struct directly under the margin, under a rebuilt
// history's retired clients (parentRetired) or under any other named root, a GC run or a skip, and
// JSON, binary, deleted and GC content, which the Proof schema renders nowhere. Every
// transaction's are charged in the structure part instead (peerWeighing.structureHeap): a
// browser's by the peer hook, the server's own by the update observer (roomCharge.wrote). Any
// content under an item or beside a neighbour may be an XML element's attribute, which renders,
// and is counted. An update the walk cannot read is counted whole, which is never less than its
// content.
func contentAdded(update []byte) int64 {
	var added renderedContent
	if _, err := walkUpdate(update, fromRoom, &added); err != nil {
		return int64(len(update))
	}
	return added.bytes
}

// renderedContent is contentAdded's visitor.
type renderedContent struct{ bytes int64 }

func (c *renderedContent) walkedStruct(walked updateStruct) {
	switch walked.parent {
	case parentMargin, parentRetired, parentOtherRoot, parentUnnamed:
		return
	}
	switch walked.kind {
	case structString, structEmbed, structFormat, structType, structAny:
		c.bytes += int64(len(walked.content))
	}
}

func (*renderedContent) walkedDelete(_, _, _ uint64) {}

// reserveRoom admits a browser's socket to room's document against the room budget, before anything
// loads the document (ServeHTTP). A room already charged admits it with nothing reserved: the
// browser joins a room whose heap is counted. Otherwise it reserves what the stored document will
// cost at both copies (storedRoomReservation, roomCopies), the room's live document and the replica
// its load keeps, both made before anything can refuse them: the admission's own decode, which
// becomes the replica (loadTree, keepReplica), and ygo's decode into the room. The reservation is
// made before the load, not when the room has loaded: fifty browsers could otherwise pass an
// unreserved check and each build a whole cold copy before any of their rooms exists.
//
// A budget already past its limit, or one this document's reservation would pass, starts a sweep
// on a goroutine of its own (sweepRooms), never on this one, an HTTP handler whose load already has
// a documented wedge against a writer holding a pooled connection (onLoadDocument), and waits for
// what the sweeps can close - the rooms whose browsers have just gone, once their settlements have
// run and their closes flushed them - before it refuses (roomBudget.await), bounded by ctx and by
// admissionWait, which covers the settle delay and the close's flush, from the admission's start
// for both waits together. The refusal it then answers with is the transient close, which the
// editor's backoff retries. The caller releases the reservation on every way out of its handler.
func (s *Service) reserveRoom(ctx context.Context, room string) (*roomReservation, error) {
	if s.budget.join(room) {
		return nil, nil
	}
	deadline := time.Now().Add(s.admissionWait)
	if err := s.budget.refusal(ctx, deadline); err != nil {
		return nil, err
	}
	weight, err := s.storedRoomReservation(ctx, room)
	if err != nil {
		return nil, err
	}
	return s.budget.reserve(ctx, deadline, room, weight)
}

// storedRoomReservation is what an admission to room reserves: its latest version's stored
// markdown, counted (pmdoc.MeasureDocument) and measured in bytes, and the structs of its stored
// history (storedStructs), priced as a loaded room's rendering and struct total are (copyHeap), at
// both copies. A version is stored as its rendering (pmdoc/parse.go:120-122), so the element count
// and the bytes are what the room's load counts of the rendering it loads with (keepReplica)
// wherever no update has been stored since that version, and that count replaces this reservation
// (roomBudget.load). So a mebibyte of prose reserves about the 54 MB it costs once counted
// (TestAnAdmissionReservesTheCountOfItsStoredVersion), where its bytes at the densest price would
// reserve 2.2 GB, past the whole budget, and refuse it on an empty pod; a document whose history
// outgrows its rendering reserves its structs, as its load is charged them
// (TestAStructDenseDocumentReservesAndIsChargedItsStructs); and a paragraph of a mebibyte reserves
// its rendered bytes, as its load is charged them
// (TestAParagraphOfAMebibyteReservesAndIsChargedItsRenderedBytes). Only markdown MeasureDocument
// cannot finish is priced from its bytes at the densest price (bytesOfHeapAByteAtTheDensest). A
// document with no version yet reserves nothing.
//
// The count is a whole goldmark parse: about 89 ms for a mebibyte, and up to 65,024,480 bytes of
// heap above its markdown for a mebibyte of the heaviest shape measured
// (storedCountHeapAtMost), and a walk of the stored history, which decodes nothing. It is paid on
// the path a person reaches when they open a document, once for each version in each process,
// since the count is kept (storedCounts) - never on a keystroke, and never inside a room's update
// observer. The admissions that miss one version at once share one count, the others waiting for
// the one that counts (storedCounts.await), and the counts of different documents run beside each
// other, each holding a grant of the copy-heap bound for what it reads and parses
// (storedCountWeight), so a burst of admissions holds no more of the pod's heap in its counts than
// that bound. Only the latest version's number is read on every admission; its markdown and the
// stored history are read only to count them.
func (s *Service) storedRoomReservation(ctx context.Context, room string) (int64, error) {
	var number int64
	err := s.queryFrom(ctx).QueryRow(ctx, `
		select number from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, room).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the number of the document's latest version: %w", err)
	}
	heap, kept := s.budget.stored.get(room, number)
	if !kept {
		heap, err = s.budget.stored.await(ctx, room, number, func(wanted context.Context) (int64, error) {
			return s.countStoredVersion(wanted, room, number)
		})
		if err != nil {
			return 0, err
		}
	}
	return roomWeight(heap, 0, 0), nil
}

// countStoredVersion is the price of one copy of room's version number (copyHeap), counted from its
// stored markdown and the structs of the history stored when it counts (storedStructs) and kept
// (storedCounts.put), unless a count that held its grant before this one has kept it already. It
// runs once for each version however many admissions miss it at once (storedCounts.await), so it
// belongs to the version rather than to the admission that started it.
//
// It opens one read-only snapshot of its own, through the pool rather than any caller's
// transaction, and weighs itself in it - the version's markdown and the largest row of the history
// it will walk (weighStoredCount) - before it covers a grant of the copy-heap bound for what those
// hold (storedCountWeight). So counts of different documents run beside each other, as many as the
// bound has room for, in the order they asked for their weight (copyHeaps), rather than one at a
// time: fifty browsers opening fifty documents of a mebibyte at once, their rooms loading beside
// the counts, were refused in two minutes while the counts took one turn at a time, and in 16.1
// to 20.5 s once each count held a grant instead, the last of the thirty-nine the element price of
// 470 admitted answered at 1m12s to 1m23s against 1m49s before; measured again on 2026-10-29
// beside the other work that sandbox was carrying, the eleven were refused in 23.2 s and the last
// admitted at 1m27.7s, the decodes of those rooms' loads most of it, which ask for six times the
// bound's time the counts do. At the price of 500 the budget holds thirty-seven such rooms, and
// the run of 2026-10-31 under sjawhar/ygo v1.54.1-sami.3 refused the thirteen in 11.2 s and
// admitted the last at 1m13.5s
// (TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays). The grant is covered inside the
// transaction the count already holds, so no holder of the bound waits for a pooled connection
// (copyHeaps).
//
// The wait for that grant lasts as long as wanted holds, which is for as long as any admission
// still waits for this count. Only once the grant is held does the count's own read bound start,
// on a context no admission's end cancels, bounded as a cold read's fold is (coldReadTimeout): a
// count that had to wait within that bound saw it pass while it waited, which answered 22 of those
// fifty browsers 503 at 60.3 s. What it counted is kept for the next admission even if every
// admission waiting for it has gone. Its error is the end of wanted, a read's, or a panic
// recovered from the parse or the walk: the count runs on a goroutine of its own
// (storedCounts.await), where nothing else could recover it.
func (s *Service) countStoredVersion(wanted context.Context, room string, number int64) (heap int64, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("counting the stored document panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: counting a stored document for its admission panicked", "room", room, "error", err)
		}
	}()
	counts := &s.budget.stored
	// The weighing read is bound as the count's own read is, and is the admissions' to cancel,
	// since nothing is counted yet. The rollback that returns the connection on every way out is
	// given a bound of its own, no admission's end cancels and no earlier wait has spent: a
	// rollback on a context already done closes the pooled connection rather than returning it,
	// and a count that waited long for its grant is exactly when the pool can least afford that.
	weighing, stopWeighing := context.WithTimeout(wanted, coldReadTimeout)
	defer stopWeighing()
	tx, err := s.store.Pool.BeginTx(weighing, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, fmt.Errorf("begin the read of the document's stored version: %w", err)
	}
	defer func() {
		stopping, stop := context.WithTimeout(context.WithoutCancel(wanted), coldReadTimeout)
		defer stop()
		_ = tx.Rollback(stopping)
	}()
	head, markdownBytes, rowBytes, err := weighStoredCount(weighing, tx, room, number)
	if err != nil {
		return 0, err
	}
	grant := newCopyGrant()
	if err := grant.coverWeight(wanted, storedCountWeight(markdownBytes, rowBytes)); err != nil {
		return 0, err
	}
	defer grant.release()
	if heap, ok := counts.get(room, number); ok {
		return heap, nil
	}
	reading, stopReading := context.WithTimeout(context.WithoutCancel(wanted), coldReadTimeout)
	defer stopReading()
	var markdown string
	if err := tx.QueryRow(reading, `
		select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2
	`, room, number).Scan(&markdown); err != nil {
		return 0, fmt.Errorf("read the document's version %d: %w", number, err)
	}
	if counts.beforeCount != nil {
		counts.beforeCount(room, number)
	}
	structs, err := storedStructs(reading, tx, room, head)
	if err != nil {
		return 0, err
	}
	heap = copyHeap(int64(len(markdown)), countElements(markdown), structs)
	counts.put(&storedCount{room: room, number: number, heap: heap})
	return heap, nil
}

// weighStoredCount is what a count of room's version number holds at its peak, read in tx's one
// snapshot before the count covers its grant (storedCountWeight): the head its walk reads through
// (headVersion), the bytes of the version's markdown, which the count reads whole and parses, and
// the largest update row of that history, which is what the walk holds at once (storedStructs). A
// document with no stored update is weighed at a head of 0 and no row.
func weighStoredCount(ctx context.Context, tx pgx.Tx, room string, number int64) (head persistence.Version, markdown, row int64, err error) {
	var version int64
	if err := tx.QueryRow(ctx, `
		select h.version,
			coalesce((select octet_length(markdown) from artifact_versions where artifact_id = $1 and number = $2), 0),
			coalesce((select max(octet_length(update)) from doc_updates where artifact_id = $1 and version <= h.version), 0)
		from (select `+headVersion+` as version) h
	`, room, number).Scan(&version, &markdown, &row); err != nil {
		return 0, 0, 0, fmt.Errorf("weigh the document's stored version %d: %w", number, err)
	}
	return persistence.Version(version), markdown, row, nil
}

// storedStructs is the structs room's stored history holds, as a room's load would read it: every
// stored update through head, read one row at a time in the count's snapshot (eachUpdateThrough,
// as a load's fold reads them), each walked as a room's own state is (walkUpdate, fromRoom), which
// reads the update's headers and the lengths of its contents, decodes nothing and allocates
// nothing per struct, its structs summed. A compacted history is one update, the state a closed
// room's compaction encoded from its decode (compactKeep), whose count is the state's own as its
// room's load counts it (countRoomStructs), but for the splits that parked structs can still
// cause. A history with updates appended since its last compaction counts each update's structs as
// stored: a run typed one transaction at a time is a struct for each keystroke here, where a
// decode merges what it can (squashRuns), and the splits a delete range makes are not here. Either
// way the room's load replaces the reservation with the count of the room it decoded
// (chargeLoadedRoom). An update the walk refuses would fail the room's load; it counts what the
// walk read before refusing.
func storedStructs(ctx context.Context, tx pgx.Tx, room string, head persistence.Version) (int64, error) {
	var structs uint64
	if err := eachUpdateThrough(ctx, tx, room, head, func(update []byte) error {
		tally, _ := walkUpdate(update, fromRoom, nil)
		structs += tally.structs
		return nil
	}); err != nil {
		return 0, err
	}
	return int64(structs), nil
}

// storedCountsHeld is the most documents whose stored version's count is kept (storedCounts). An
// entry is an id and two integers, a few hundred bytes with its list element and its map slot, so
// 4,096 of them hold about a mebibyte, and counting entries bounds their weight: the cold reads'
// cache weighs each of its entries (documentRead.weight) because they run from bytes to
// megabytes, and these do not vary. A document the bound has dropped costs its next admission one
// parse and one walk of its stored history again, nothing more.
const storedCountsHeld = 4_096

// storedCounts keeps, for each document an admission counted, the price of one copy of its latest
// stored version (countStoredVersion), the most recently admitted first, for at most
// storedCountsHeld documents. It is shaped as the cold reads' cache is (documentReads,
// read_cache.go:82-142): one mutex, a map into a list kept in the order of use, one entry for a
// document, a newer version's count replacing an older one's, and the counts in flight, so that
// the admissions missing one version at once share one count rather than each parsing it (await).
// Unlike that cache's singleflight group, the counts in flight know how many admissions wait for
// each, so a count every admission has left ends its wait for its grant rather than parse for
// nobody.
//
// Unlike that cache's renderings, an element count never goes stale, and needs no stamp: a
// version's markdown is written once, by the insert that makes the version
// (api/artifacts.go:296, api/issue_create.go:349), and no running server rewrites it - the other
// writes to artifact_versions move a file's content (files/backfill.go:177,215) or are
// migrations, which run before it serves - so the count of a document's version number is that
// version's for as long as it stands. The struct count is kept beside it, for the version: it is
// of the history as stored when the version was first counted, and updates stored after that move
// it only once a later version is counted, which costs nothing more than the reservation's
// staleness, since an admission's reservation stands only until the room loads and its own count
// replaces it (roomBudget.load). An entry is used only for the version the store names latest at
// the admission.
type storedCounts struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	// flights is the counts running or waiting for their grant of the copy-heap bound, one for
	// each (document, version) however many admissions miss it at once (await).
	flights map[string]*countFlight
	// beforeCount runs in a count once it has read the version's markdown and before it parses it.
	// Nil outside tests; tests use it to count the parses and to hold one while admissions arrive.
	beforeCount func(room string, number int64)
}

// storedCount is the price of one copy of room's version number (copyHeap): its elements, its
// bytes or the structs of its stored history, whichever is the most.
type storedCount struct {
	room         string
	number, heap int64
}

// get is the price of one copy of room's version number when it is kept, which it marks the most
// recently used.
func (c *storedCounts) get(room string, number int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[room]
	if !ok {
		return 0, false
	}
	count := element.Value.(*storedCount)
	if count.number != number {
		return 0, false
	}
	c.order.MoveToFront(element)
	return count.heap, true
}

// put keeps count as its document's, in place of a count of an older version, then drops the least
// recently used past storedCountsHeld. A count of an older version than the one kept is dropped
// instead: a slow count must not put back a version the store has moved past.
func (c *storedCounts) put(count *storedCount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries, c.order = make(map[string]*list.Element), list.New()
	}
	if element, ok := c.entries[count.room]; ok {
		if element.Value.(*storedCount).number > count.number {
			return
		}
		c.order.Remove(element)
	}
	c.entries[count.room] = c.order.PushFront(count)
	if c.order.Len() > storedCountsHeld {
		delete(c.entries, c.order.Remove(c.order.Back()).(*storedCount).room)
	}
}

// countFlight is one count of a stored version (countStoredVersion) and the admissions waiting for
// it.
type countFlight struct {
	// done is closed once the count has returned heap and err.
	done chan struct{}
	heap int64
	err  error
	// waiting is how many admissions wait for the count. storedCounts.mu guards it.
	waiting int
	// abandon ends the count's wait for its grant of the copy-heap bound once no admission waits
	// for it.
	abandon context.CancelFunc
}

// await is the price count gives of one copy of room's version number, counted once however many
// admissions miss it at once: the first starts count on a goroutine of its own, and the rest wait
// for that one. The count is the version's, not an admission's: ctx bounds only this admission's
// wait, so an admission whose request ends returns at once with ctx's error and leaves the count to
// the others, and count's context, wanted, ends only once every admission waiting for it has gone,
// which ends a count still waiting for its grant of the copy-heap bound (countStoredVersion). An
// admission that misses the version after that starts a count of its own. An admission waiting
// here holds nothing of that bound, so the counts ahead of it are never waiting for what it holds.
func (c *storedCounts) await(ctx context.Context, room string, number int64, count func(wanted context.Context) (int64, error)) (int64, error) {
	key := room + "@" + strconv.FormatInt(number, 10)
	c.mu.Lock()
	flight := c.flights[key]
	if flight == nil {
		wanted, abandon := context.WithCancel(context.WithoutCancel(ctx))
		flight = &countFlight{done: make(chan struct{}), abandon: abandon}
		if c.flights == nil {
			c.flights = make(map[string]*countFlight)
		}
		c.flights[key] = flight
		go func() {
			defer abandon()
			heap, err := count(wanted)
			c.mu.Lock()
			c.forgetLocked(key, flight)
			c.mu.Unlock()
			flight.heap, flight.err = heap, err
			close(flight.done)
		}()
	}
	flight.waiting++
	c.mu.Unlock()
	select {
	case <-flight.done:
		return flight.heap, flight.err
	case <-ctx.Done():
		c.mu.Lock()
		defer c.mu.Unlock()
		if flight.waiting--; flight.waiting == 0 {
			flight.abandon()
			c.forgetLocked(key, flight)
		}
		return 0, ctx.Err()
	}
}

// forgetLocked drops flight as key's count in flight, unless a later count has replaced it. Its
// caller holds mu.
func (c *storedCounts) forgetLocked(key string, flight *countFlight) {
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
}

// chargeLoadedRoom charges room's newly loaded instance, live, whose replica has just published
// its load's count (keepReplica), in place of the reservations of the sockets admitted to it, and
// hands the replica the charge its counts re-base from then on (renderedReplica.storeCounted). The
// load is priced from that count, the rendering's bytes and the struct total the replica counted
// at the load (newStructTotal), whichever is the most (copyHeap). The charge lists live's delete
// set as the ranges the room holds deleted (deletedRuns) before the replica hands it to the peer
// hook: a walk of the document's whole store, beside the load's rendering of it, which is not a
// keystroke.
func (s *Service) chargeLoadedRoom(room string, live *crdt.Doc, replica *renderedReplica) *roomCharge {
	bytes, elements, _ := replica.size()
	charge := s.budget.load(room, copyHeap(bytes, elements, replica.structs.countedStructs()))
	charge.deleted.seed(crdt.DeleteSetFromDoc(live))
	replica.sizeMu.Lock()
	replica.charge = charge
	replica.sizeMu.Unlock()
	return charge
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is short (roomBudget.short)
// - over its limit, or without room for what the admissions waiting on it need (wanted) - it
// closes the charged rooms that hold nothing outlasting them, the heaviest first, so as few as it
// can, through the eviction a failed room takes (evictRoom), whose close flushes the room before
// it goes. Unused is the service's own test (unusedLocked), not the weaker "no browser
// connected": a room with no browser can still hold an open writer, an armed settlement or an
// update its persistence has not appended. A room still loading is not closed either: its load
// has not returned, and the opener waiting on it would find it gone. The close's unload returns
// the room's charge (releaseUnloadedRoom), and ygo's CloseRoom fires that unload before it returns
// wherever it is the one that removed the room (sjawhar/ygo v1.54.1-sami.1,
// provider/websocket/inject.go:625-641), so the next look at the budget sees the charge gone. Each
// close is logged with the budget's own figures before and after it, which is how an operator, or
// a test of the real process, tells a room the budget closed from one that retired on its own.
//
// A room it leaves because something other than a connected browser holds it - a settlement, a
// writer, an append, a load - is pending: it will be closable once that ends, which starts a sweep
// of its own (Service.unlockState), so the admissions waiting on the budget wait for it (await).
// So is a room whose browsers have all gone though their handlers have not yet forgotten them
// (roomState.departedPeers): its last browser's leaving runs the settlement it owes before they do.
// Once nothing more can be closed it raises full if the budget is still over its limit
// (roomBudget.swept).
//
// It runs on a goroutine of its own, as failRoomLocked's eviction does, which Shutdown joins
// (evictWG); a service stopping sweeps nothing, since its Shutdown closes every room itself.
func (s *Service) sweepRooms() {
	pending := false
	defer func() { s.budget.swept(pending) }()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	defer s.evictWG.Done()
	for _, room := range s.budget.heaviest() {
		if !s.budget.short() {
			return
		}
		state := s.lockExistingState(room)
		if state == nil {
			pending = true
			continue
		}
		if s.srv.GetDoc(room) == nil || !s.unusedLocked(state) {
			// Left as it is: unlockState's look at the budget would only ask for this sweep again.
			pending = pending || state.onlyDepartedLocked()
			state.mu.Unlock()
			continue
		}
		// As Evict does: a settlement read under the generation before the close is one the close
		// has invalidated. The state is unlocked without unlockState's look at the budget, which
		// would take this room, unused, for one to sweep or close again.
		state.roomGeneration++
		state.mu.Unlock()
		before, _, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			// An Apply is inside its callback on the room (closeUnused): the room will be
			// closable once that apply ends, which hands it to closeIfOpenedOverBudget and kicks
			// the budget, so the admissions waiting on it wait for that rather than being refused.
			pending = true
			continue
		}
		after, limit, wanted := s.budget.figures()
		slog.Info("dispatch: the room budget closed an unused document",
			"room", room, "charged_before", before, "charged", after, "limit", limit, "making_room_for", wanted)
	}
}

// closeIfOpenedOverBudget closes room once it is unused (unusedLocked) when its charge is marked
// closeOnceUnused: a room the server's own work loaded while the budget was over its limit
// (roomBudget.load), which no browser has joined since (roomBudget.joinedLocked). It is called
// whenever room's state may have stopped holding anything (Service.unlockState) and once each
// Server.Apply on it has returned (roomServer.Apply), which between them see the end of every
// opener's use: a writer's, a settlement's and an append's end in the state, a read's in its
// Apply. A room still loading, or one a browser holds, is left: a browser connected is a holder
// unusedLocked names, so a room a browser holds is never closed this way. The close is the
// eviction the sweep makes (evictUnusedRoom), on a goroutine of its own, which Shutdown joins
// (evictWG), and is logged with the budget's figures before and after it as the sweep's are. A
// room an Apply is inside its callback on is left marked, for the end of that Apply to look at
// again.
func (s *Service) closeIfOpenedOverBudget(room string) {
	if s.budget.marked.Load() == 0 || !s.budget.isMarked(room) {
		return
	}
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.released || s.srv.GetDoc(room) == nil || !s.unusedLocked(state) || !s.budget.takeMark(room) {
		state.mu.Unlock()
		return
	}
	state.roomGeneration++
	state.mu.Unlock()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	go func() {
		defer s.evictWG.Done()
		before, _, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			s.budget.markAgain(room)
			return
		}
		after, limit, _ := s.budget.figures()
		slog.Info("dispatch: the room budget closed a document the server opened over it",
			"room", room, "charged_before", before, "charged", after, "limit", limit)
	}()
}

// roomApplied is what the service does with a room once a Server.Apply on it has returned
// (roomServer.applied). An apply holds the room against the budget's closes while it is inside
// its callback and holds nothing in the room's state, so those closes give up in front of it
// (closeUnused) and nothing else reports its end: this is where a room the sweep passed over, and
// one the server opened over the budget, are looked at again. A room the budget does not charge
// starts no sweep (roomBudget.kick) and carries no mark.
func (s *Service) roomApplied(room string) {
	s.budget.kick(room)
	s.closeIfOpenedOverBudget(room)
}
