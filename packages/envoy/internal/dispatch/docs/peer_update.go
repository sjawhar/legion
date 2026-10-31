package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	"golang.org/x/sync/semaphore"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A browser's update reaches its room through no check of the server's own: applyLive weighs every
// write a caller makes by the document it leaves (growth.go), and a browser's update runs through
// none of it, so a browser can carry a document past the bound every server write obeys
// (growth.go:33-35). What stops that here is weighPeerUpdate, ygo's Server.OnPeerUpdate, which the
// room runs in place of its own apply for each update and whole-state message a read-write browser
// sends.
//
// It does not run applyLive's checks. Those weigh what a write leaves, which costs a fork of the
// document, a tree build and a full markdown render - about 420 ms for the fork alone at 1 MiB and
// a whole further copy of the document, about 27 MB at the 470 bytes an element a copy holds
// (bytesOfHeapAnElement), with the render about 160 ms (AGENTS.md:563-602, :780-781) - against a
// keystroke on such a document that already takes between half a second and a second. What it does
// instead is read the update itself in one pass that decodes nothing and allocates nothing per
// struct (walkUpdate), and weigh what that pass finds against the room's last published rendering
// (renderedReplica.publishSize) and the room's struct total (structTotal): work proportional to the
// update rather than to the document, which for a keystroke is a walk of a few dozen bytes.
//
// The hook weighs bytes and not elements. Counting a rendering's elements is a whole goldmark
// parse, 58 to 89 ms at 524 KiB (BenchmarkElementCountAgainstRender), which no keystroke can pay
// on a one-processor pod: weighed in an earlier build of the hook, it took the p50 of
// BenchmarkKeystrokeBesideReads up by 18 to 23 per cent across the runs taken on this branch (the
// approved spec quotes the 18) against a stop condition of ten. No cheap exact count exists either:
// a render that counts what it writes agreed with the server's count over 1,127 documents, but
// took 1,404 lines emulating goldmark and still disagreed for a live tree whose rendering reads
// back as something else, which is what a browser holds. So the element bound is kept where it
// already is: a server write carrying caller text is refused when it would leave a document past
// pmdoc.MaxDocumentElements and heavier than it was (weigh, growth.go:148-163). What a browser can
// leave live past it - about 64 KiB of headings - goes on saving and syncing; the server's writes
// that would grow it are refused until someone shortens it, a write that leaves it no heavier
// passes, and a rebuild from its latest stored version restores it whatever it weighs
// (rebuild_test.go:217-228).
//
// The bound is not a ceiling the document cannot pass: the rendering the hook compares against is
// as recent as the room's last update observer, so a document passes it by the updates in flight
// when it crosses, and this is all that is claimed here.
//
// The document has a second bound of its own, which bytes cannot see: the decode cap. ygo decodes
// at most maxUpdateItems structs in one update, and a room's whole state is one update to the next
// load that reads it, so a room whose state holds more could never be loaded again once its
// updates were folded. A browser can get there without passing the byte bound - a frame of GC
// structs is two bytes a struct and renders nothing - so the hook keeps each room's struct total
// and refuses with DocumentTooLargeCloseCode an update that would take it past the cap, whatever it
// inserts or deletes (structTotal).
//
// The document has a third bound, which neither bytes nor structs see: what a room holds that is
// not text, a value of any size in one clock and one struct. The server's own writes bound the
// margin's records at maxMarginBytes and the anchor marks' ids at maxAnchorBytes (growth.go), and a
// browser's update runs through neither: measured on 2026-10-26, eighty browser updates each
// writing a million bytes into the margin were all admitted, and took the room's whole-state frame
// from 264 bytes to 80,001,457 with its rendering at 42, past the message floor every browser of
// the document then reconnects against. So the room keeps an unrendered total beside its struct
// total, and the hook refuses with DocumentTooLargeCloseCode an update that adds unrendered bytes
// past maxUnrenderedBytes (unrenderedBytes, structTotal).
//
// The unrendered total does not see everything a frame is made of: text inside an element nested
// under the margin reads as text, which the walk cannot place, so one struct of it could carry the
// frame past what the three bounds allow. So the room keeps a fourth total, never below its
// whole-state frame: its encoded state's bytes at each count with what deleting what it holds can
// add (deletionReserve), and what each update the hook admits since can add (frameBytes,
// deleteFrameBytes). The hook refuses with DocumentTooLargeCloseCode an update that inserts
// anything and would take it past heaviestHandoffBound, F, the heaviest frame the bounds allow a
// browser to hand over; one that only deletes is never refused by it, so a room at F can be cut.
//
// The hook has a third state, which is the room budget's rather than the document's: while the
// open rooms are past their budget and nothing can be closed (roomBudget.full), an update that
// inserts anything is refused in every room with the transient DocumentBusyCloseCode, and one that
// only deletes is admitted while the budget's charges stay within the pressure reserve past its
// limit (pressureReserveBytes), and refused the same way past it (refusedUnderPressure). Bytes
// cannot be that test. The hook does not count elements, and the elements are what the heap
// follows (bytesOfHeapAnElement): a mebibyte of markdown can make 786,432 of them
// (pmdoc/elements.go:16-29), so an update can leave a document as long as it was, or shorter, and
// still multiply what its room holds. What an update inserts is what can add an element, so an
// update that inserts nothing is the one update that cannot grow the rendering; but a delete that
// cuts the middle of an item splits it, which adds structs, so even deletes are bounded, by the
// reserve. That refusal is not the document's own bound and is not folded into it: 4413 is
// permanent and the editor drops the refused edit, where 4503 is transient and the editor keeps
// it, to send once the pressure passes.
//
// The hook charges the room budget from the same walk (peerWeighing.structureHeap): each update it
// admits adds to its room's structure part what the rendering's count cannot see, which no count
// clears (roomCharge).

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a room's measurement may creep before only an update
// that inserts nothing at all is admitted, so a document that has crossed can still be trimmed or
// split and nothing can grow it further.
const maxPeerByteCreep = 64 << 10

// maxUnrenderedBytes is the most unrendered bytes (unrenderedBytes) a room may hold once an update
// that adds any has applied: the server's own margin rule, maxMarginBytes, and anchor rule,
// maxAnchorBytes, on the browser's path, with maxStructureBytes for what the walk counts beside
// them. A walk cannot tell a margin record from an element's attribute (unrenderedBytes), so the
// total counts the fragment's element names, attributes and mark values too, which every document
// carries: a bound of the margin's and the anchors' two mebibytes alone would refuse a person
// adding a paragraph to a document the server's own writes stored, whose margin and anchor marks
// are full, since its elements alone hold up to 1,692,989 bytes of them (below). It is 9.5 MiB.
const maxUnrenderedBytes = maxMarginBytes + maxAnchorBytes + maxStructureBytes

// maxStructureBytes is the unrendered bytes the fragment's own elements and marks may hold beside
// a full margin and full anchor marks: as much as the server's writes can store, and as much again
// as a browser's creep past the byte bound adds, each with room to spare, so no document the
// server stores and no browser's growth within the creep allowance is refused by the unrendered
// bound. Measured on 2026-10-26 at the element bound, the densest shapes' elements hold 25.8
// unrendered bytes each - 1,692,989 for 65,536 elements of empty list items, each with its node
// name and block id - against 18.0 for headings and 14.8 for images; pmdoc.MaxDocumentElements at
// 48 bytes, 3 MiB, also covers a document whose links run hrefs beside its elements. A browser's
// creep (maxPeerByteCreep, and maxPeerInsertAtTheBound once more past it) in the densest markdown,
// alternating empty bullet items with their minted ids, adds 448 unrendered bytes for each pair's
// 8 rendered bytes, 56 a byte (TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor:
// 3,361,925 to 6,297,669 bytes over 52,424 rendered), taken at 64 a byte, 4.5 MiB. The densest
// shape grown far past the creep, in a document a browser built past the element bound, is
// refused here before the struct total refuses it, at about 22,000 pairs where the decode cap
// would take about 52,000: a document no server write would store.
const maxStructureBytes = pmdoc.MaxDocumentElements*48 + (maxPeerByteCreep+maxPeerInsertAtTheBound)*64

// unrenderedStaleMargin is how near maxUnrenderedBytes a stale unrendered total must be for the
// hook to confirm it by an exact count before it admits an update that adds any (nearBounds): half
// the bound, more than the 3,361,925 unrendered bytes of the heaviest document one server write
// leaves (heaviestDocument), so the writes since a count would have to be several such documents
// to use the margin up unseen, and each is weighed by the server's own rules.
const unrenderedStaleMargin = maxUnrenderedBytes / 2

// unrenderedRecheck is how many unrendered bytes the hook must have admitted since its last
// confirmation before a running total past maxUnrenderedBytes is confirmed again: 1/64 of the
// bound, as structTotalRecheck is of the cap.
const unrenderedRecheck = maxUnrenderedBytes / 64

// densestStateFrame and densestStateStructs are the whole-state frame and the struct total of the
// densest document a browser builds, measured on 2026-10-26 (struct_cap_measure_test.go:35-40):
// alternating empty bullet items, each list, item and paragraph with its minted block id, pasted
// through the hook until the struct total refused one more pair, 32.28 frame bytes a struct. Each
// struct's share holds its header and the content the shape gives it - a node name, a block id -
// as well as the shape's 416,458 rendered bytes.
const (
	densestStateFrame   = 33_609_343
	densestStateStructs = 1_041_050
)

// heaviestHandoffBound is F, the heaviest whole-state frame a browser can hand over under the
// hook's three bounds, each term at its bound: maxUpdateItems structs at densestStateFrame's bytes
// a struct, 33,852,313 rounded up; a rendering at the creep cap and one admitted insert past it,
// pmdoc.MaxDocumentBytes + maxPeerByteCreep + maxPeerInsertAtTheBound, 1,122,304, each rendered
// byte at most one frame byte of text; and maxUnrenderedBytes, 9,961,472. That is 44,936,089.
// It overstates every document a browser can reach: the per-struct term already holds the
// densest shape's node names and block ids, which the unrendered term counts again, and the
// unrendered bound stops that shape well short of the decode cap.
const heaviestHandoffBound = (maxUpdateItems*densestStateFrame+densestStateStructs-1)/densestStateStructs +
	pmdoc.MaxDocumentBytes + maxPeerByteCreep + maxPeerInsertAtTheBound + maxUnrenderedBytes

// serverStoredStateFrame is the heaviest whole-state frame the server's own writes store, measured
// on 2026-10-26 (struct_cap_measure_test.go:41-45): a document of 16,384 headings an agent rewrote
// whole through ReplaceText until the decode cap refused a write, 1,048,576 structs rendering
// 1,048,575 bytes, against 5,360,879 for heaviestDocument at every bound with no history.
const serverStoredStateFrame = 12_598_044

// frameStaleMargin is how near heaviestHandoffBound a stale frame total must be for the hook to
// confirm it by an exact count before it admits an update that inserts anything (nearBounds): half
// of F, more than the 12,598,044-byte frame of the heaviest state the server's own writes store
// (serverStoredStateFrame), so the writes since a count would have to be more than such a whole
// document to use the margin up unseen, and each is weighed by the server's own rules.
const frameStaleMargin = heaviestHandoffBound / 2

// frameRecheck is how many frame bytes the hook must have admitted since its last confirmation
// before a running frame total past heaviestHandoffBound is confirmed again: 1/64 of F, as
// structTotalRecheck is of the cap.
const frameRecheck = heaviestHandoffBound / 64

// jsonReencodeGrowth is the most a byte of JSON content can grow to once ygo writes it into the
// room's state again. ygo keeps a JSON item's, a format's and an embed's values decoded and encodes
// each again as JSON text with encoding/json (fmtValToJSON, sjawhar/ygo v1.54.1-sami.1,
// crdt/update.go:488-500 and :1563-1574). A JSON item whose values read only as lib0 Any, as ygo
// up to 1.51.0 wrote them (decodeContent's wireJSON case, :1231-1263), grows the most: each false,
// one byte, is written as the five of false with its length prefix, six bytes a byte. A JSON text
// grows less: encoding/json writes a number below 10^21 in full, so the four bytes of 1e20, five
// with their length prefix, become the twenty-one of 100000000000000000000, twenty-two with
// theirs, 4.4 bytes a byte; a line separator's three bytes become its six-byte escape.
const jsonReencodeGrowth = 6

// maxMessageBytes is the most one websocket message a browser sends may carry, ygo's
// Server.MaxMessageBytes, whose own default is 64 MiB (sjawhar/ygo v1.54.1-sami.1,
// provider/websocket/server.go:150-157). It is a floor against a client that is not the editor at
// all, never the bound on a document: the bound is weighPeerUpdate's, which walks a frame without
// decoding it (walkUpdate) and refuses an oversized one before anything decodes it, so a frame at
// this floor that the hook refuses costs the pod the frame itself and nothing proportional to the
// structs it carries.
//
// The floor has to clear every whole-state frame an editor hands over, since a frame over it is
// closed by the websocket library with a code the editor reconnects on, a loop the editor's
// handoff must never enter. It is the larger of four times the heaviest frame the server's own
// writes store, 4 x serverStoredStateFrame = 50,392,176, and a quarter more than F, the heaviest a
// browser can hand over under the hook's bounds, 1.25 x heaviestHandoffBound = 56,170,112, rounded
// up to the next mebibyte: 54 MiB, under the 64 MiB ygo allowed before, past which this design
// would have had to come back to the architect. Measured frames sit well inside it: heaviestDocument
// at 5,360,879 bytes, and that document grown through the creep allowance at 9,577,606
// (TestTheHeaviestWholeStateFrameFitsTheMessageFloor,
// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor), and
// TestTheHeaviestWholeStateFrameTheServerStoresFitsTheMessageFloor holds the floor over both of
// its terms.
const maxMessageBytes = 54 << 20

// weighPeerUpdate is ygo's Server.OnPeerUpdate: the room hands it every update and whole-state
// message a read-write browser sends, with the apply that would put it in the room, and a read-only
// browser never reaches it (sjawhar/ygo v1.54.1-sami.1, provider/websocket/peer_update.go:84-151,
// provider/websocket/peer.go:263-270).
// An update it does not apply is neither stored nor relayed to the room's other browsers, and the
// *websocket.PeerClose it refuses with closes that one browser with that code and reason.
//
// Every update is walked first (walkUpdate, fromPeer), under the bound too. An update the walk
// refuses - bytes ygo's decoder would refuse, or content the Proof editor never sends: a
// subdocument, a root other than the fragment and the margin - is refused, unless the walk
// reached a client group of the history a rebuild replaced before it refused (below), with
// DocumentTooLargeCloseCode and DocumentUpdateRefusedCloseReason (documentUpdateRefused,
// refusedWith, which keeps the walk's own error behind the close) and never applied, so ygo
// integrates nothing of it and closes that browser with that code and reason
// (sjawhar/ygo v1.54.1-sami.1, provider/websocket/peer_update.go:38-52, :139-146). Under
// v1.51.3-sami.7 the browser was answered SyncStatus(0) and left connected; upstream
// reearth/ygo#304 was merged in the form that closes it, with 4205 "Reset Connection" for a hook
// error that is no *websocket.PeerClose, and a hook can keep a refused browser connected only by
// letting the room's own apply refuse the update. 4205 is what @hocuspocus/provider 2.x
// reconnects at once after, and the client would send the refused frame again, and the walk
// refuse it again, a loop of full syncs; 4413 is the permanent band, which the editor drops the
// refused edit for, so every refusal of the hook made before the apply carries it. The apply is
// not what answers such a frame either: though a room's bounded document takes a failed apply
// back out whole (newRoomScans), ygo would first decode and integrate every struct before the bad
// byte, at the cost the walk is there to keep off the peer's read loop, and the room would have
// to be left to refuse what the editor never sends.
//
// While the room's last measured rendering plus the update's own bytes stay under
// pmdoc.MaxDocumentBytes, the bytes admit the update. At or over it, the update is admitted only
// when what it carries that the room does not already hold - the walk's clocks beyond the room's
// state vector (peerWeighing) - is under maxPeerInsertAtTheBound, or, past the creep cap, only when
// it carries nothing new at all. Then the room's totals admit it only while what it would add keeps
// its struct total within maxUpdateItems, its unrendered bytes, when it adds any, within
// maxUnrenderedBytes, and its frame total, when it inserts anything, within heaviestHandoffBound
// (structTotal.admits). Each refusal of the five is the document's own, DocumentTooLargeCloseCode.
//
// The unrendered bytes are what the byte and clock rules cannot see (unrenderedBytes): a value of
// any size is one clock, and one in the margin or in an element's attributes renders nothing, so a
// browser could otherwise add mebibytes of them in each update, under the bound and at it, and
// carry the room's whole-state frame past the message floor (maxMessageBytes). An update that adds
// none - typing, deleting, a whole-state message of what the room holds - is never refused by them.
// The frame total keeps the frame under F, short of what deletes add at F, whatever the walk can
// place; an update that inserts nothing - a delete, the room's state handed back - it never refuses.
//
// An update carrying a client group of the history a rebuild replaced - a group of a client the
// rebuilt history lists as retired, starting below the clock it had reached (retiredMapName) -
// comes from a browser that held the document before the rebuild and still holds that history
// (walkedGroup gives why no other browser sends one, whatever the walk reads of it): applied, its
// whole-state message would put the old document beside the rebuilt one, every block twice, and
// carry the room back toward the cap the rebuild trimmed. It is refused before any other rule, the
// walk's refusal included, with DocumentTooLargeCloseCode and DocumentNeedsRebuildCloseReason, so
// the editor drops what it sent and builds its document afresh from the rebuilt room, and drops its
// saved rows typed against the old history as rows of a rebuilt document (pending-sync.ts,
// addsParking). An update that adds an entry to the retired clients' list, overwrites one or
// deletes one is refused as the editor's never-sent content is: only a rebuild writes the list,
// and nothing the editor runs writes or deletes an entry of it (update_walk.go's search).
//
// While the room budget is full (roomBudget.full), an update the document's bounds admit is then
// refused with DocumentBusyCloseCode when it inserts anything (refusedUnderPressure), whatever it
// leaves the document's bytes at, since an insert of fewer bytes than it deletes can still add
// elements. An update does not say what it deletes without the document to measure it against, so
// an edit that replaces text with less of it is refused too, as the creep cap refuses it. One that
// only deletes is admitted while what it adds to the charges keeps them within the budget's limit
// and the pressure reserve beyond it (pressureReserveBytes), and refused the same way past it. The
// signal is one atomic load, so the peer's read loop takes neither the budget's lock nor a room's
// state lock for it while the budget is not full.
//
// An update it admits is charged to its room's structure part (roomCharge.grew,
// peerWeighing.structureHeap) once its apply has succeeded. One admitted under pressure is charged
// before its apply, in the same look at the budget as the reserve's check. A delete range the room
// is known to hold deleted already is charged nothing (deletedRuns), so a browser's whole-state
// handoff is not charged the room's whole delete set. The content it adds that a rendering can show
// is charged by the room's update observer, from the transaction the apply commits (contentAdded),
// not here; and every transaction the hook did not apply, the server's own writes, is charged the
// same structure by that observer (roomCharge.wrote), which the hook never refuses or weighs.
//
// Every update's apply is weighed too, by what the walk estimates ygo's apply of it allocates into
// this room (peerWeighing.applyHeap), which is proportional to the values and structs it carries
// whether or not the room already holds them, and to the room's clients and deleted items. One
// whose estimate passes applyTooHeavyBytes, all of what the Go memory limit leaves above the room
// budget, the cold reads' cache and the pressure reserve, could not apply even alone: it is
// refused with DocumentTooLargeCloseCode and DocumentTooLargeCloseReason before any other rule
// and never applied (errApplyTooHeavy), as a walk's refusal is: no document of the editor's
// reaches it (TestAnHonestHandoffAtTheHeaviestFrameIsNeverRefusedAsTooHeavy), and a client that
// sends such a frame would send it again on every reconnect. An
// admitted update whose own share of the estimate passes unweighedApplyHeapBytes applies only once
// it holds its estimate, the marks its scans can make included, out of the bound (applyHeaps), or
// the whole bound for an estimate past it, which then applies alone, so a reconnect storm's
// whole-state handoffs decode as many at a time as the bound has room for rather than all at once,
// and no honest handoff is refused for its weight; typing never waits on it.
//
// Every apply's scans are held to an allowance (roomScans): the per-frame ceiling for its heap
// share (scanCeiling) or what is left of the room's scan total, whichever is less, reserved before
// the apply and settled from what its scans passed once it returns, and usedUpScanBudget once the
// room's total is used up.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema, websocket.go:235),
// or one no load listed: the byte bound weighs its updates on their own bytes, as if the room
// rendered nothing, and the creep cap, which needs a rendering to measure, never applies to it.
//
// An apply that fails leaves no trace but its scans. The room's bounded document takes every
// remote update it refuses back out whole, whatever the error, and fires no observer for it
// (crdt/apply_rollback.go:9-21), so the hook takes back what it booked for the update: its
// reservation of the struct total (structTotal.unreserve), a structure charge taken before the
// apply under pressure (roomCharge.gaveBack), and it charges no structure and lists none of its
// delete ranges as the room's; what its scans passed before the refusal it reports
// (crdt.RefusedUpdateError), and the room's scan total keeps it (roomScans.settle), since the work
// was done. An update whose merge would scan past its allowance (crdt.ErrIntegrateScanBudget,
// which wraps crdt.ErrInvalidUpdate as every apply error does) is refused with
// DocumentTooLargeCloseCode and DocumentTooTangledCloseReason, or DocumentNeedsRebuildCloseReason
// when the room's scan total was used up (documentNeedsRebuild): permanent, as the document's other
// refusals are, so the editor drops the edit, since the same frame would be refused again on every
// reconnect. Any other error the apply returns - one that would park structs or delete ranges past
// the room's pending cap (MaxPendingItems, which the room holds from the end of its load,
// TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad) - is returned as it is, the one error the
// hook answers that is no *websocket.PeerClose, so ygo answers that browser SyncStatus(0) and
// leaves it connected, exactly as a room without this hook does: the
// apply ran, which is what keeps a refused browser connected where a refusal before the apply
// closes it (provider/websocket/peer_update.go:136-151). The pending cap refuses such an update
// before the apply scans or changes anything, so it costs the room neither charge nor scans
// (TestAPendingCapRefusalCostsTheRoomNeitherChargeNorScans). The struct total refuses such an
// update first: it counts each parked struct and parked delete range splitsAStructCanCause times
// within maxUpdateItems, the cap itself.
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	replica := s.liveReplica(info.Doc)
	charge := replica.roomCharge()
	var structs *structTotal
	var scans *roomScans
	if replica != nil {
		structs, scans = replica.structs, replica.scans
	}
	weighed := peerWeighing{held: info.Doc.StateVector(), known: charge.deletes(), retired: structs.retiredClients(), entries: structs.retiredEntries()}
	tally, err := walkUpdate(info.Update, fromPeer, &weighed)
	if s.refusesNextPeerUpdateForTest(info.Room) && err == nil && !weighed.former {
		err = &updateRefusal{rule: errUpdateNotFromEditor, reason: "the test hook armed this document's next browser update to be refused"}
	}
	// A replaced history is closed before any other rule, its walk's refusal included: a
	// browser's whole-state message of a history the rebuild found at the decode cap, with an
	// edit it made since, is past the cap and refused by the walk; left connected, it would hand
	// it back on every reconnect while each later edit, naming the old history's items, parked.
	// What the walk read before it refused is the update's, the header of the group it refused in
	// included (walkedGroup). It is closed before its entries are weighed as forged too: a
	// history an earlier rebuild seeded carries that rebuild's list, which the room, seeded
	// afresh, does not hold.
	if weighed.former {
		slog.Info("dispatch: refused a browser's update carrying the history its document's rebuild replaced",
			"room", info.Room, "update_bytes", len(info.Update))
		return documentNeedsRebuild()
	}
	if err != nil {
		return refusedWith(documentUpdateRefused(), err)
	}
	if weighed.forged {
		return refusedWith(documentUpdateRefused(),
			&updateRefusal{rule: errUpdateNotFromEditor, reason: "a change to the retired clients' list, which only a rebuild writes"})
	}
	var bytes int64
	measured := false
	if replica != nil {
		bytes, _, measured = replica.size()
	}
	if !measured {
		bytes = 0
	}
	own := weighed.ownApplyHeap(len(info.Update))
	weighs := own > unweighedApplyHeapBytes
	heap := own + weighed.roomApplyHeap(structs.roomStructs(info.Doc))
	if heap > applyTooHeavyBytes {
		return refusedWith(documentTooLarge(), fmt.Errorf("%w: its apply is estimated to allocate %d bytes, past the %d bytes the pod lets any apply hold, even alone",
			errApplyTooHeavy, heap, uint64(applyTooHeavyBytes)))
	}
	atTheBound := bytes+int64(len(info.Update)) >= pmdoc.MaxDocumentBytes
	crept := measured && bytes >= pmdoc.MaxDocumentBytes+maxPeerByteCreep
	if atTheBound && (weighed.inserted >= maxPeerInsertAtTheBound || (crept && weighed.inserted > 0)) {
		return documentTooLarge()
	}
	// A heavy update waits for its share of the bound before it reserves its addition to the
	// room's totals, so what the room admits on is weighed as it stands once the wait is over,
	// and the room's other browsers are not weighed against a reservation that waits.
	if weighs {
		defer s.applyHeaps.hold(heap)()
	}
	added := weighed.addition()
	// The reservation is what keeps two browsers of one room weighed at once from each passing
	// a bound's check and together passing the bound: each counts the other's addition while it
	// is in flight (structTotal.reserve). No lock is held across the apply, so another browser
	// of the room is weighed and applied while this apply runs, its observers included.
	if !structs.reserve(info.Doc, added) {
		return documentTooLarge()
	}
	charged := false
	if s.budget.full.Load() {
		if s.refusedUnderPressure(&weighed, charge) {
			structs.unreserve(added)
			return &websocket.PeerClose{
				Code:   contracts.DocumentBusyCloseCode,
				Reason: contracts.DocumentBusyCloseReason,
			}
		}
		charged = true
	}
	// The allowance is reserved last, so no refusal above leaves one in flight, and settled as soon
	// as the apply returns, with what its scans passed.
	allowance := scans.reserve(info.Origin, scanCeiling(own))
	err = structs.applying(info.Origin, apply)
	scans.settle(allowance, err)
	if err != nil {
		// The room's bounded document took the update back out whole (newRoomScans), so nothing
		// of it is the room's: what the hook booked for it goes back.
		structs.unreserve(added)
		if charged {
			charge.gaveBack(weighed.structureHeap())
		}
		if errors.Is(err, crdt.ErrIntegrateScanBudget) {
			// The honest long session's updates scan 12 items at most, so a refusal is worth a
			// line, with the budget's figures, the room's struct total and its scan total as the
			// refusal leaves them, and the allowance the update was refused at.
			used, limit, _ := s.budget.figures()
			scanned, left := scans.figures()
			slog.Warn("dispatch: refused a browser's update its room could not merge within its scan allowance",
				"room", info.Room, "update_bytes", len(info.Update), "charged", used, "limit", limit,
				"struct_total", structs.roomStructs(info.Doc), "allowance", allowance.limit(),
				"scan_total", scanned, "scans_left", left)
			if allowance.roomUsedUp() {
				return documentNeedsRebuild()
			}
			return documentTooTangled()
		}
		return err
	}
	// Moved to what the hook admitted only after the apply returns, so a recount that read the
	// total's admissions before its encode cannot miss what this apply integrated
	// (structTotal.recount). Its delete ranges are listed as the room's, so a later update
	// carrying them again is charged nothing for them (deletedRuns).
	structs.landed(added)
	if !charged {
		charge.grew(weighed.structureHeap())
	}
	charge.deletes().record(info.Update, tally.deletesAt, weighed.born)
	return nil
}

// documentTooLarge is the document's own refusal of an update, permanent: the editor drops the
// refused edit and rebuilds from a fresh read of the document.
func documentTooLarge() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooLargeCloseReason,
	}
}

// documentTooTangled is the refusal of an update the room's document could not merge within its
// scan allowance while the room's scan total was not used up (roomScans.reserve), permanent as
// documentTooLarge is and under the same code, with a reason of its own so the editor can say why
// the edit was dropped.
func documentTooTangled() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooTangledCloseReason,
	}
}

// documentNeedsRebuild is the refusal of an update the document cannot take until it is rebuilt -
// one carrying the history a rebuild replaced - permanent as documentTooLarge is and under the same
// code, with a reason of its own.
func documentNeedsRebuild() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentNeedsRebuildCloseReason,
	}
}

// documentUpdateRefused is the refusal of an update the room will not read: one the walk refuses -
// bytes ygo's decoder would refuse, or content the Proof editor never sends - and one that writes
// the retired clients' list only a rebuild writes. Permanent as documentTooLarge is and under the
// same code, with a reason of its own: the client that sent it sends it again on every reconnect
// (@hocuspocus/provider 2.x reconnects at once after a close it does not read as permanent), so
// the one answer that ends the exchange is the one the editor drops the edit for.
func documentUpdateRefused() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentUpdateRefusedCloseReason,
	}
}

// refusedWith is refusal, the close ygo answers the browser with - it reads it through errors.As
// (sjawhar/ygo v1.54.1-sami.1, provider/websocket/peer_update.go:139-146) - with cause behind it,
// which no close frame carries: the rule that refused the update, its reason and the byte it
// refused at, for the line ygo logs and for this package's tests.
func refusedWith(refusal *websocket.PeerClose, cause error) error {
	return fmt.Errorf("%w: %w", refusal, cause)
}

// refusedUnderPressure is whether the room budget's pressure, while the budget is full and nothing
// can be closed (roomBudget.full), refuses weighed, an update the document's own bounds admit:
// one that carries anything the room does not already hold, and one that only deletes once its
// structure would take the charges and reservations past the limit and pressureReserveBytes
// beyond it. A delete it admits is charged to charge, the room's, in the same look at the budget
// (roomBudget.growWithin), so the hook does not charge it again after its apply.
func (s *Service) refusedUnderPressure(weighed *peerWeighing, charge *roomCharge) bool {
	if weighed.inserted > 0 {
		return true
	}
	structure := weighed.structureHeap()
	return structure > 0 && !s.budget.growWithin(charge, structure, pressureReserveBytes)
}

// liveReplica is the replica of the room holding live (keepReplica), or nil for a document no
// room's load listed. It is a load off the listing the room's reads already go through (holdLive),
// so the peer's read loop takes neither the replica's lock, which every browser in the room waits
// behind (AGENTS.md:559-562), nor the room's state lock.
func (s *Service) liveReplica(live *crdt.Doc) *renderedReplica {
	listed, ok := s.replicas.Load(weak.Make(live))
	if !ok {
		return nil
	}
	return listed.(weak.Pointer[renderedReplica]).Value()
}

// peerWeighing is what one walk of a browser's update finds against held, the room's state vector
// when the walk began (walkUpdate's visitor). It replaces a decode of the update
// (crdt.ContentIDsFromUpdateV1, which allocates an item for every struct, up to maxUpdateItems
// of them for one frame) with the same arithmetic over the walk's structs: a struct, or the part of
// one, past its client's clock in held is what the room does not already hold. Skips are clocks an
// update withholds and are never integrated, so they count for nothing. From the same walk it
// estimates what ygo's apply of the update allocates (applyHeap).
type peerWeighing struct {
	held crdt.StateVector
	// inserted is how much content the update carries that the room does not already hold,
	// counted in the units a Yjs item's content is measured in: a character of text, an element,
	// an attribute an element gains. It is zero for an update that only deletes, and zero for a
	// whole-state message of a document the room already holds, which every browser sends on its
	// first sync and which measured literally would be the whole document.
	inserted int64
	// structs is the structs reaching past the room's state, each of which integration adds to the
	// room's store; deletes is the update's delete ranges, and splits the items at their two ends
	// that their applies can leave split for good (deletedRuns.splitEnds): none for a range the
	// room is known to hold deleted already (known), and for one that overlaps what it is known to
	// hold, none at an end inside that. deleteStructs is what the ranges can add to the room's
	// struct total: their splits, with the last end of a range that reaches past the room's clock
	// for its client (held) counted splitsAStructCanCause rather than one, since the range's tail
	// can park there as a parked range of its own, which the room's count holds
	// splitsAStructCanCause structs for (structTotalOf). known is nil for a room no load charged,
	// in which every end but a client's first clock can split.
	structs, deletes, splits, deleteStructs uint64
	known                                   *deletedRuns
	// decoded is every walked struct's share of the apply's estimate, since ygo's apply decodes
	// each before it asks whether the room holds it; integrated is the share of the structs past
	// the room's state alone, which only they cost (structApplyHeap).
	decoded, integrated uint64
	// content is the heap one copy of the room keeps for the content of the structs past its
	// state that no rendering prices (unrenderedContentHeap).
	content uint64
	// unrendered is the unrendered bytes of the structs past the room's state (unrenderedBytes),
	// what the update adds to the room's unrendered total, and frame the bytes its new structs and
	// the delete ranges the room is not known to hold deleted can add to the room's whole-state
	// frame (frameBytes, deleteFrameBytes), what it adds to the room's frame total.
	unrendered, frame uint64
	// born is whether any struct past the room's state integrates deleted (bornDeleted), whose
	// clocks the room's delete set holds from then on (deletedRuns.record).
	born bool
	// retired is the clients the room's history lists as retired by a rebuild, each with the clock
	// it had reached (structTotal.retired), nil for a room never rebuilt or no load listed, and
	// entries where that list's own entries sit (structTotal.entries). former is whether the update
	// carries a client group of a retired client starting below that clock (walkedGroup), which
	// only a browser holding the replaced history sends; forged whether it changes the list - an
	// entry past the room's state, a struct naming an entry as its origin, which is how an
	// overwrite of a map key is written, or a delete range over an entry - which no browser does.
	retired        map[crdt.ClientID]uint64
	entries        retiredEntries
	former, forged bool
}

// walkedGroup marks the update former when the group is a retired client's and starts below the
// clock that client had reached: the group's first struct is then the replaced history's. Every
// struct of a group starts at or past the group's clock, so this sees every struct of a retired
// client below its clock.
//
// It reads each group's header - its client and the clock its first struct starts at - which the
// walk hands over before it reads the group's structs or weighs the group's count against
// maxUpdateItems (groupVisitor), so it answers whatever the walk read before a refusal. That is
// what a browser holding a history at the decode cap needs: its whole-state message, the history
// with an edit made since, is past the cap, and the walk refuses it at the header of the group
// that passes it; when the browser's own client sorts before the history's (ygo writes groups by
// ascending client, sjawhar/ygo v1.54.1-sami.1, crdt/update.go:386; Yjs by descending, yjs 13.6.32
// src/utils/encoding.js:99), and a history one client wrote is one group, no struct of the history
// is read before that refusal, and only the header shows it. Every rule the walk refuses a
// history's handoff by - its count, its content, its delete set - comes at that header or after
// it: the groups before it are the browser's own clients', carrying only what its editor wrote
// since, which the walk refuses only for more structs than the cap or content no editor sends.
//
// It never closes an honest browser. A retired client's clocks below the one it had reached are
// the replaced history's structs: a client numbers its structs from zero up, the rebuild reads
// that clock from the history it replaces (rebuiltSeed), and the rebuilt room holds none of them -
// its seed is a fresh client's, and the hook refuses every update carrying them - so a browser
// that took the document from the rebuilt room holds none either, and writes a group of a retired
// client, if any, only from that client's structs at or past that clock. The editor's 4413 path
// builds its document afresh on a new connection, a new Y.Doc with a client id of its own
// (packages/dispatch/web/src/features/doc/connection.ts:153, :222-231), so what it writes next is
// no retired client's. And no encoder begins a client group with anything but a struct it holds of
// that client: ygo starts each group inside its first item (encodeV1Locked, crdt/update.go:231-271;
// encodeStructStoreV1, crdt/merge.go:162-188, which writes a skip only between two items), and Yjs
// at the first struct of its store, which holds no skips (writeStructs, src/utils/encoding.js:57-71),
// at the first struct of a merge, whose readers drop skips (mergeUpdatesV2, src/utils/updates.js:338,
// writing a skip only between two structs of one client, :405-417), and never with a skip in a
// diff (diffUpdateV2, :477-481). So a header of a retired client below that clock is a struct of
// the replaced history, short of a fresh browser drawing a retired client's random 32-bit id.
func (w *peerWeighing) walkedGroup(client, clock, structs uint64) {
	if limit, listed := w.retired[crdt.ClientID(client)]; listed && structs > 0 && clock < limit {
		w.former = true
	}
}

func (w *peerWeighing) walkedStruct(walked updateStruct) {
	w.weighStruct(walked, structApplyHeap(walked))
}

// weighStruct weighs walked, whose share of the apply's estimate is cost. It is walkedStruct's
// body with that share passed in, so that a weighing no reader of the estimate holds pays no walk
// for it (writtenWeighing.walkedStruct).
func (w *peerWeighing) weighStruct(walked updateStruct, cost applyCost) {
	w.decoded += cost.decode
	if walked.kind == structSkip {
		return
	}
	known := w.held[crdt.ClientID(walked.client)]
	end := walked.clock + walked.length
	// A struct of no clocks at the room's clock or past it is a struct all the same: it spans
	// nothing a state vector can see, and integration still stores it.
	if end <= known && (walked.length != 0 || walked.clock < known) {
		return
	}
	w.inserted += int64(end - max(walked.clock, known))
	w.structs++
	w.integrated += cost.integrate
	w.content += unrenderedContentHeap(walked)
	w.unrendered += unrenderedBytes(walked)
	w.frame += frameBytes(walked)
	w.born = w.born || bornDeleted(walked)
	w.forged = w.forged || walked.parent == parentRetired ||
		(walked.hasOrigin && w.entries.overlap(walked.origin.client, walked.origin.clock, 1)) ||
		(walked.hasRightOrigin && w.entries.overlap(walked.rightOrigin.client, walked.rightOrigin.clock, 1))
}

func (w *peerWeighing) walkedDelete(client, clock, length uint64) {
	w.deletes++
	var splits uint64
	start, end := w.known.splitEnds(client, clock, length)
	if start {
		splits++
	}
	if end {
		splits++
	}
	w.splits += splits
	w.deleteStructs += splits
	if end && clock+length > w.held[crdt.ClientID(client)] {
		w.deleteStructs += splitsAStructCanCause - 1
	}
	if !w.known.covers(client, clock, length) {
		w.frame += deleteFrameBytes(client, clock, length, splits)
	}
	w.forged = w.forged || w.entries.overlap(client, clock, length)
}

// splitsAStructCanCause is the most structs integrating one struct can add to a store beyond
// itself, and the most applying one delete range can: each splits at most the item it starts in
// and the item it ends in (an item's integrate splits the items holding its origin and its right
// origin, sjawhar/ygo v1.54.1-sami.1, crdt/update.go:928-935 and crdt/item.go:98-110, through
// crdt/store.go:136-185; a delete range splits the items at its two ends,
// crdt/delete_set.go:199-201).
const splitsAStructCanCause = 2

// added is what the walked update can add to the room's struct total: each new struct with the
// splits its integration can cause, and what its delete ranges can add (deleteStructs). A
// whole-state handoff of the document the room holds adds nothing: it carries no struct past the
// room's state, and every one of its delete ranges the room holds deleted already, whose splits
// ygo merges back at commit (deletedRuns.splitEnds). So a browser reconnecting to a room at the
// decode cap, which hands its whole state over on connecting as every browser does, is admitted.
func (w *peerWeighing) added() uint64 {
	return w.structs*(1+splitsAStructCanCause) + w.deleteStructs
}

// addition is what the walked update can add to the room's totals (structTotal): its structs
// (added), its unrendered bytes and its frame bytes, and whether it inserts a struct.
func (w *peerWeighing) addition() roomCount {
	return roomCount{structs: w.added(), unrendered: w.unrendered, frame: w.frame, inserts: w.structs > 0}
}

// structureHeap is what the walked update adds to its room that a count of the rendering cannot
// see, at both copies (roomCopies), which the room budget charges to the room's structure part
// (roomCharge): each new struct at bytesOfHeapAStruct, the splits its integration can cause and
// the splits its delete ranges can leave (splits) at bytesOfHeapASplit, which a split keeps where
// the kind the split divides does not, and the content of its new structs that no rendering prices
// (content). A whole-state handoff of a document the room already holds adds no struct and carries
// no delete range the room is not known to hold deleted, and is charged nothing.
func (w *peerWeighing) structureHeap() int64 {
	splits := w.structs*splitsAStructCanCause + w.splits
	return int64((w.structs*bytesOfHeapAStruct + splits*bytesOfHeapASplit + w.content) * roomCopies)
}

// unrenderedContentHeap is the heap one copy of a room keeps for walked's content that no count of
// the rendering prices: a map key's bytes, as text, and the content itself at its kind's heap a
// byte - text for a string or an XML type's name (bytesOfHeapAByteOfText), values for Any, JSON or
// a format (bytesOfHeapAByteOfValues). Binary content and embeds the walk refuses from a browser
// (errUpdateNotFromEditor) before any weighing, so the room budget never charges them; they are
// priced here as a binary's bytes and an embed's value would weigh, as text and as values, only so
// that an estimate of an update a walk of a room's own state reads (fromRoom) holds them too
// (TestEveryApplysTransientHeapIsWithinItsEstimate). The rendering prices what it shows: a string,
// an embed, a format or an XML type directly under the fragment. Everything else is charged here,
// whether or not it is shown, since the walk cannot tell for a struct whose parent is an item or
// its neighbour's (parentItem, parentOfNeighbour) where it sits: text typed into a paragraph, and
// a later entry for a margin key, both read that way. Charged as unshown, typing costs its text's
// price a byte beside the per-struct price, a few bytes a keystroke, against content in the margin
// that no count would ever see were it taken as shown. A deleted item's or a GC run's content is a
// length, which no heap follows.
func unrenderedContentHeap(walked updateStruct) uint64 {
	heap := uint64(len(walked.key)) * bytesOfHeapAByteOfText
	content := uint64(len(walked.content))
	switch walked.kind {
	case structString, structType, structEmbed, structFormat:
		if walked.parent == parentFragment {
			return heap
		}
	}
	switch walked.kind {
	case structString, structBinary, structType:
		return heap + content*bytesOfHeapAByteOfText
	case structAny, structJSON, structEmbed, structFormat:
		return heap + content*bytesOfHeapAByteOfValues
	}
	return heap
}

// unrenderedBytes is walked's unrendered bytes: the encoded bytes of its map key and of its
// content that are not text, which are what the hook's byte and clock rules cannot see. A value
// of an Any or JSON item, a binary, an embed or a format value, and an XML element's name, are one
// clock each whatever their size, and a map key is no clock at all; none of them is weighed by the
// rendering where it renders nothing - the margin's records, an element's attributes the schema
// does not render. A text's characters are not counted: each is a clock, which the bound's rules
// weigh where the room renders them (inserted, maxPeerInsertAtTheBound), and each renders where
// the editor writes it. A deleted item's, a GC run's and a skip's content is a length, which
// takes no bytes past its header; a deleted map entry's key stays in the state, and is counted.
//
// It is measured wherever the struct sits, because the walk cannot place a value whose parent is
// an item or its neighbour's (parentItem, parentOfNeighbour): a later record for a margin key and a
// later value for an element's attribute read alike. So it counts what the server's own margin
// rule (maxMarginBytes) and anchor rule (maxAnchorBytes) weigh - each record's whole encoding,
// keys and the server's own fields with its text, and each anchor mark's format value, written
// again at each text it covers - and with them the names and attributes of the fragment's
// elements and the values of its other marks, which the server weighs through its rendering and
// element bounds. maxUnrenderedBytes bounds the sum of all of them.
func unrenderedBytes(walked updateStruct) uint64 {
	switch walked.kind {
	case structString, structDeleted, structGC, structSkip:
		return uint64(len(walked.key))
	}
	return uint64(len(walked.key) + len(walked.content))
}

// frameBytes is what walked, a struct past the room's state, can add to the room's frame total:
// to the whole-state frame (crdt.EncodeStateAsUpdateV1, sjawhar/ygo v1.54.1-sami.1,
// crdt/update.go:94-98 and :236-258, and encodeItem, :461-588), now and through whatever happens
// to it later -
//
//   - its own bytes in the update (updateStruct.size), with what ygo's writing of its content
//     again can grow them by. ygo keeps a JSON item's, a format's and an embed's values decoded
//     and writes them as JSON text (jsonReencodeGrowth), and an Any item's values decoded and
//     written by lib0's rules, which read an integer of more than 32 bits from as few as six bytes,
//     a tag and a five-byte varint, and write it as a nine-byte float64 (writeAnyInt,
//     encoding/encoder.go:283-299; internal/anycodec/decoder.go:276-284): half as much again.
//     ygo writes every other content as it read it, and names a struct's origins and parent as
//     the update did, or drops an origin it holds as a GC run for a parent named in no more bytes
//     (encodeItem, :349-361 and :392-421);
//   - its place among the room's structs (placeBytes);
//   - its deletion, by its own delete range or by whatever deletes it later - a block deleted
//     around it, an entry written over it, a parent deleted before it arrives (deleteRangeBytes);
//   - the splits its integration can cause: one in the item holding its origin and one in the item
//     holding its right origin, for each it names (splitsAStructCanCause gives where), so a GC run
//     or an item naming only its parent splits nothing (splitFrameBytes).
//
// A delete range's share is deleteFrameBytes.
func frameBytes(walked updateStruct) uint64 {
	written := uint64(walked.size)
	switch walked.kind {
	case structJSON, structFormat, structEmbed:
		written += (jsonReencodeGrowth - 1) * uint64(len(walked.content))
	case structAny:
		written += uint64(len(walked.content)) / 2
	}
	written += placeBytes(walked) + deleteRangeBytes(walked.client, walked.clock, walked.length)
	if walked.hasOrigin {
		written += splitFrameBytes
	}
	if walked.hasRightOrigin {
		written += splitFrameBytes
	}
	return written
}

// deleteFrameBytes is what a delete range the room is not known to hold deleted (deletedRuns) can
// add to the room's frame total: its range in the room's delete set, which a range of clocks the
// room does not hold yet keeps there parked (deleteRangeBytes), and splitFrameBytes for each of
// the splits its apply can leave for good in the items at its two ends, splits of them
// (deletedRuns.splitEnds). The items it deletes are already priced for their deletion: by the
// count (deletionReserve) or by the update that brought them (frameBytes). A range the room is
// known to hold deleted adds nothing to the frame: ygo's encode writes the room's delete set from
// its deleted items, merged where their clocks touch (buildDeleteSet, crdt/update.go:502-515), and
// the room's parked ranges merged the same way (crdt/delete_set.go:65-104), so the clocks it names
// are written as they were, and it leaves no split.
func deleteFrameBytes(client, clock, length, splits uint64) uint64 {
	return deleteRangeBytes(client, clock, length) + splits*splitFrameBytes
}

// maxVarintBytes is the most bytes a lib0 varint takes for any client or clock a room holds: the
// walk refuses a varint past 53 bits from a browser (update_walk.go), a server write's client is 32
// bits (crdt.NewClientID, crdt/doc.go:414-421) and its clocks count its own structs, and 53 bits
// take eight bytes of seven.
const maxVarintBytes = 8

// varintBytes is the bytes lib0 writes v in as a varint: seven bits a byte.
func varintBytes(v uint64) uint64 {
	n := uint64(1)
	for ; v >= 0x80; v >>= 7 {
		n++
	}
	return n
}

// placeBytes is the most walked's place among the room's structs adds beside its own bytes: a
// group of its own, should the room hold nothing of its client - the group's struct count, client
// and first clock, and a byte more of the count of groups - or, should it park beyond a gap in its
// client's clocks, the skip ygo writes before it, a byte and the gap's length, no more than its
// clock (encodeStructStoreV1, crdt/merge.go:162-200), with a byte more of its group's struct count.
func placeBytes(walked updateStruct) uint64 {
	return 2 + varintBytes(walked.client) + varintBytes(walked.clock)
}

// deleteRangeBytes is the most deleting the clocks [clock, clock+length) of client adds to the
// room's delete set as ygo writes it (encodeDeleteSet, crdt/update.go:517-533): the range's clock
// and length, and should the delete set hold no range of client, its client and a byte of its
// count of ranges; and a byte more of a count, of ranges or of clients. A range that touches one
// the delete set holds is merged with it (buildDeleteSet, :502-515) and adds less.
func deleteRangeBytes(client, clock, length uint64) uint64 {
	return 2 + varintBytes(client) + varintBytes(clock) + varintBytes(length)
}

// maxDeleteRangeBytes is deleteRangeBytes for a client, a clock and a length of maxVarintBytes
// each.
const maxDeleteRangeBytes = 2 + 3*maxVarintBytes

// splitFrameBytes is the most one split of an item the room holds can add to the room's frame
// total (splitItem, crdt/item.go:515). The right part becomes an item of its own, written with an
// info byte, its origin - the left part's last clock - and the item's right origin, each a client
// and a clock, and its content's length (a string's bytes, a run's values or clocks), whose bytes
// are the item's own, moved and not added; an item with an origin names no parent; and its group's
// struct count may take a byte more: 2 + 5 x maxVarintBytes. The right part can be deleted later,
// a range of its own (maxDeleteRangeBytes), and the deletion of the two parts can add more than
// the deletion of the item was priced at (deletionReserve) by the right part's length, which its
// content no longer gives back (maxVarintBytes).
const splitFrameBytes = 2 + 5*maxVarintBytes + maxDeleteRangeBytes + maxVarintBytes

// deletionReserve is the most deleting walked, a struct of the room's encoded state, can add to
// the room's frame: the range it adds to the delete set (deleteRangeBytes), less what ygo gives
// back of its content, which it writes as a deleted run's length once the item is deleted
// (gcTxnDeleteSet, crdt/snapshot.go:284-313, in every room, whose documents keep ygo's GC on). A
// GC run, a deleted run and a skip are deleted already.
func deletionReserve(walked updateStruct) uint64 {
	switch walked.kind {
	case structGC, structDeleted, structSkip:
		return 0
	}
	added := deleteRangeBytes(walked.client, walked.clock, walked.length)
	givenBack := uint64(len(walked.content)) - min(uint64(len(walked.content)), varintBytes(walked.length))
	return added - min(added, givenBack)
}

// roomCount is a room's state as the hook's totals weigh it (structTotal): its structs, which the
// decode cap bounds; its unrendered bytes (unrenderedBytes), which maxUnrenderedBytes bounds; and
// its whole-state frame's bytes with what deleting what it holds can add (deletionReserve, and
// frameBytes and deleteFrameBytes for an addition), which heaviestHandoffBound bounds. inserts is
// whether an addition carries a struct the room does not hold, the only addition the frame bound
// refuses (fitsBounds); a count's is false, and the totals do not sum it.
type roomCount struct {
	structs, unrendered, frame uint64
	inserts                    bool
}

// fitsBounds reports whether a room holding held can take added within the decode cap; when added
// carries any unrendered bytes, within maxUnrenderedBytes; and when it inserts a struct the room
// does not hold, within heaviestHandoffBound. An update that adds no unrendered bytes is never
// refused by the unrendered bound, and one that inserts nothing never by the frame total, though it
// adds to it, so a room past either - a stored document heavier than the bound, or one a write the
// hook does not see took past it - can still be trimmed and handed back whole, and past the
// unrendered bound typed in too.
func fitsBounds(held, added roomCount) bool {
	return held.structs+added.structs <= maxUpdateItems &&
		(added.unrendered == 0 || held.unrendered+added.unrendered <= maxUnrenderedBytes) &&
		(!added.inserts || held.frame+added.frame <= heaviestHandoffBound)
}

// nearBounds reports whether held, a stale total, is near enough a bound that added would take it
// past should the writes since the count have used up the margin: within structTotalStaleMargin
// of the decode cap; for an addition of unrendered bytes, within unrenderedStaleMargin of
// maxUnrenderedBytes; and for an addition that inserts, within frameStaleMargin of
// heaviestHandoffBound.
func nearBounds(held, added roomCount) bool {
	return held.structs+added.structs > maxUpdateItems-structTotalStaleMargin ||
		(added.unrendered > 0 && held.unrendered+added.unrendered > maxUnrenderedBytes-unrenderedStaleMargin) ||
		(added.inserts && held.frame+added.frame > heaviestHandoffBound-frameStaleMargin)
}

// structTotalStaleMargin is how near the cap a stale struct total must be for the hook to confirm
// it by an exact count before it admits on it: an eighth of the cap, about as many structs as the
// heaviest document one upload admits encodes to (maxUpdateItems). The server's writes that leave
// the total stale each ask the decode cap of what they leave themselves (refuseUndecodable,
// leavesPastDecodeCap in growth.go).
const structTotalStaleMargin = maxUpdateItems / 8

// structTotalRecheck is how much the hook must have admitted since its last confirmation before a
// running total past the cap is confirmed again: 1/64 of the cap, so a room typed into at the cap
// re-encodes its state once in every few thousand keystrokes rather than on each.
const structTotalRecheck = maxUpdateItems / 64

// structTotal is a room's struct total, the number of structs its whole state holds as ygo
// decodes it, against which the hook refuses an update that would take the room past the decode
// cap (maxUpdateItems). Measured on this branch: a second GC-struct frame of 600,000 structs left a
// room whose 2,200,030-byte state no longer decoded, though each frame alone was within the cap
// ygo's apply checks, so the cap has to be enforced on what the room holds, not on one update.
//
// The total is an exact count plus what the hook admitted since it. The count walks the room's
// encoded state (countRoomStructs), which carries parked structs and parked delete ranges
// (crdt.EncodeStateAsUpdateV1, sjawhar/ygo v1.54.1-sami.1, crdt/update.go:236-258). It is taken at
// the room's load, beside the load's rendering (keepReplica), with each count the service's
// measurer makes of the room (elementMeasurer.count), and by the hook to confirm a total near or
// past the cap. What the hook admits after it is each update's addition (peerWeighing.added): its
// new structs, each with the two splits its integration can cause, and for its delete ranges the
// splits each can leave at its two ends - none at an end either of whose two clocks the room is
// known to hold deleted - with what a tail parked past the room's clock adds to the count
// (peerWeighing.deleteStructs; deletedRuns.splitEnds gives why). That is an upper bound, so the
// running total is never below what the room holds, short of a write the hook does not see.
//
// Beside it the total keeps the room's unrendered bytes (unrenderedBytes), against which the hook
// refuses an update that adds any past maxUnrenderedBytes, the same way: exact at each count, the
// same walk of the same encode summing each struct's (countRoom), plus each admitted update's
// (peerWeighing.unrendered), which is all of its new structs' and so never less than what they
// add. A delete takes nothing off it until the next count, so the running total overstates a room
// that deleted values since, and a refusal on it is confirmed by a count as one on the struct
// total is. Its stale margin and its recheck are unrenderedStaleMargin and unrenderedRecheck.
//
// And it keeps the room's frame total, against which the hook refuses an update that inserts
// anything past heaviestHandoffBound: the length of the same encode at each count with what
// deleting what it holds can add (countRoom, deletionReserve), plus what each update admitted
// since can add to it (peerWeighing.frame: frameBytes for each new struct, deleteFrameBytes for
// each delete range the room is not known to hold deleted), so it is never below the frame. A
// delete adds to it what it can add to the frame and takes nothing off it until the next count.
// Its stale margin and its recheck are frameStaleMargin and frameRecheck.
//
// Those writes are every transaction on the room that is not a hook's apply: the server's own
// writes (Server.Apply - an API write's publish, settlement and its repairs, the mark sweep, the
// block-id stamp - and an update injected through ygo, allowInject), which ask the decode cap in
// their own way (refuseUndecodable, leavesPastDecodeCap) and are bounded there, not here. The one
// place that sees each of them, whoever wrote it, is the document's update observer: ygo fires it
// for every transaction it commits, even one that writes nothing, with the transaction's origin
// (crdt/doc.go:606-608, :688-703), on the committing goroutine. So the total registers an
// observer of its own (observed), and the hook names the origin of each apply it is running while
// it runs it (applying): ygo's peer, the origin ygo applies a browser's update with
// (provider/websocket/peer_update.go:84-97). Several browsers' applies can run at once, each its
// own peer, so the origins are a multiset. A transaction of any other origin marks the total
// stale. A list of the server's writes would miss the next one added; the observer cannot. A stale
// total within structTotalStaleMargin of the cap is confirmed by an exact count before the hook
// admits on it; further from the cap the server's writes, each bounded by its own check, are
// trusted not to have used the margin up.
//
// The hook reserves an update's addition before its apply and holds no lock across the apply:
// ygo fires a transaction's update observers after it has released the document's lock, on the
// committing goroutine (crdt/doc.go:795-798), so a lock held from the check through the apply
// would hold every other browser of the room while one apply sat in an observer
// (TestARoomWhosePeersUpdatesAreStoredOutOfOrderSettlesBoth holds one there). Under a
// short lock (admissions) the hook checks the running total, which counts every reservation in
// flight, with the update's addition against the cap, confirming it by a count where admits says
// so, and adds the addition to inflight; it then applies with no lock, and once the apply returns
// moves the addition from inflight to admitted (landed), or, for an apply that failed, which the
// room's bounded document took back out whole, out of inflight alone (unreserve). Two browsers
// weighed at once therefore each count the other's addition, and cannot together pass the cap.
//
// The running total is counted + (admitted - countedAdmitted) + inflight, and a count racing an
// apply never makes it undercount. A count reads admitted and writes before it encodes the room
// (recount). An apply that moved to admitted before that read had returned, so it landed before
// the encode, and the count holds it while countedAdmitted already takes it out: counted once.
// An apply in flight at the read and landing before the encode is in the count, and still in
// inflight, or in admitted past countedAdmitted once it moves: counted twice, which overstates the
// total. One landing after the encode is in inflight or moves to admitted after the read: counted
// once. A move adds to admitted before it takes from inflight, and running reads inflight before
// admitted, so a move between running's two reads is seen at least once. The same order holds
// for writes, so a write that landed after a count's encode leaves the total stale.
type structTotal struct {
	live weak.Pointer[crdt.Doc]
	// count is countRoom, but for a test's, which counts the counts.
	count func(doc *crdt.Doc) roomCount
	// retired is the clients the room's history lists as retired by the rebuild that seeded it,
	// each with the clock it had reached (retiredClientsOf), and entries where those entries sit in
	// the history (retiredEntriesOf), both read once at the room's load: only a rebuild writes
	// them, a rebuild replaces a history no room holds (RebuildDocument), and the hook refuses any
	// change to them (peerWeighing.forged).
	retired map[crdt.ClientID]uint64
	entries retiredEntries

	// admissions is held by the hook from its check of the total to its reservation (reserve),
	// never across an apply, so two reservations are checked one after the other.
	admissions sync.Mutex

	// originMu guards origins, the origin of each apply the hook is running and how many of its
	// applies are running, which the observer reads on whatever goroutine commits a transaction.
	originMu sync.Mutex
	origins  map[any]int

	// inflight is the sum of the struct additions the hook has reserved whose applies have not yet
	// returned, and inflightUnrendered and inflightFrame of their unrendered and frame bytes.
	// admitted, admittedUnrendered and admittedFrame are the same sums for every addition whose
	// apply has returned, and writes the number of transactions the observer has seen that were
	// not a hook's apply; those four only grow.
	inflight, admitted, writes             atomic.Uint64
	inflightUnrendered, admittedUnrendered atomic.Uint64
	inflightFrame, admittedFrame           atomic.Uint64

	// mu guards the last count - counted, with the admissions and writes as they were read before
	// it - and the hook's last confirmation: whether one was taken and the admissions when it was.
	mu                sync.Mutex
	counted           roomCount
	countedAdmitted   roomCount
	countedWrites     uint64
	confirmed         bool
	confirmedAdmitted roomCount
}

// newStructTotal is live's totals, counted once now, at the room's load, with the clients live's
// history lists as retired and where it lists them.
func newStructTotal(live *crdt.Doc) *structTotal {
	retired := retiredClientsOf(live)
	total := &structTotal{live: weak.Make(live), count: countRoom, retired: retired}
	if retired != nil {
		total.entries = retiredEntriesOf(live)
	}
	live.OnUpdate(total.observed)
	total.recount(live)
	return total
}

// retiredClientsOf is the clients doc's history lists under retiredMapName, each with the clock it
// had reached when a rebuild replaced the history it belonged to (rebuiltSeed), or nil for a
// history that lists none. An entry that does not read as a client and a clock is not the
// rebuild's, and is skipped.
func retiredClientsOf(doc *crdt.Doc) map[crdt.ClientID]uint64 {
	var retired map[crdt.ClientID]uint64
	doc.GetMap(retiredMapName).ForEach(func(key string, value any) {
		client, err := strconv.ParseUint(key, 10, 64)
		clock, isClock := retiredClock(value)
		if err != nil || !isClock {
			return
		}
		if retired == nil {
			retired = make(map[crdt.ClientID]uint64)
		}
		retired[crdt.ClientID(client)] = clock
	})
	return retired
}

// retiredClock reads a retired client's clock as ygo decodes the int64 a rebuild wrote: lib0 Any
// writes an integer within 32 bits as a varint, which decodes as an int64, and a larger one within
// 2^53 as a float64 (sjawhar/ygo v1.54.1-sami.1, encoding/encoder.go:283-299,
// internal/anycodec/decoder.go:276-296). A clock of zero retires nothing.
func retiredClock(value any) (uint64, bool) {
	switch value := value.(type) {
	case int64:
		return uint64(value), value >= 1 && value <= maxEditorClock
	case float64:
		return uint64(value), value >= 1 && value <= maxEditorClock
	}
	return 0, false
}

// retiredClients is the clients the room's history lists as retired (structTotal.retired), nil for
// a total no load made.
func (t *structTotal) retiredClients() map[crdt.ClientID]uint64 {
	if t == nil {
		return nil
	}
	return t.retired
}

// retiredEntries is the entries of the retired clients' list (retiredMapName) a room's history
// holds: the clocks of each, by the client that wrote it, as runs sorted by clock and merged where
// they touch. A rebuild writes them in its seed's one transaction (rebuiltSeed), so they are one
// run of one client.
type retiredEntries map[crdt.ClientID][]crdt.IDRange

// retiredEntriesOf is where doc's history holds the entries of its retired clients' list: each
// struct a walk of its encoded state finds directly under retiredMapName.
func retiredEntriesOf(doc *crdt.Doc) retiredEntries {
	var found retiredEntryReader
	_, _ = walkUpdate(crdt.EncodeStateAsUpdateV1(doc, nil), fromRoom, &found)
	for client, runs := range found.entries {
		slices.SortFunc(runs, func(a, b crdt.IDRange) int { return cmp.Compare(a.Clock, b.Clock) })
		merged := runs[:1]
		for _, run := range runs[1:] {
			last := &merged[len(merged)-1]
			if run.Clock <= last.Clock+last.Len {
				last.Len = max(last.Len, run.Clock+run.Len-last.Clock)
				continue
			}
			merged = append(merged, run)
		}
		found.entries[client] = merged
	}
	return found.entries
}

// retiredEntryReader is retiredEntriesOf's visitor.
type retiredEntryReader struct{ entries retiredEntries }

func (r *retiredEntryReader) walkedStruct(walked updateStruct) {
	if walked.parent != parentRetired || walked.length == 0 {
		return
	}
	if r.entries == nil {
		r.entries = make(retiredEntries)
	}
	client := crdt.ClientID(walked.client)
	r.entries[client] = append(r.entries[client], crdt.IDRange{Clock: walked.clock, Len: walked.length})
}

func (*retiredEntryReader) walkedDelete(_, _, _ uint64) {}

// overlap reports whether the clocks [clock, clock+length) of client hold any entry.
func (e retiredEntries) overlap(client, clock, length uint64) bool {
	runs := e[crdt.ClientID(client)]
	// The first run that ends past clock is the only one that can start before clock+length.
	at, _ := slices.BinarySearchFunc(runs, clock, func(run crdt.IDRange, clock uint64) int {
		if run.Clock+run.Len <= clock {
			return -1
		}
		return 1
	})
	return at < len(runs) && runs[at].Clock < clock+length
}

// retiredEntries is where the room's history holds its retired clients' list (structTotal.entries),
// nil for a room never rebuilt or a total no load made.
func (t *structTotal) retiredEntries() retiredEntries {
	if t == nil {
		return nil
	}
	return t.entries
}

// observed is the total's update observer: a transaction that is not one of the hook's applies
// marks the total stale.
func (t *structTotal) observed(_ []byte, origin any) {
	if !t.hookApplies(origin) {
		t.writes.Add(1)
	}
}

// hookApplies reports whether origin is the origin of an apply the hook is running (applying): a
// browser's update, whose structure the hook charges itself (roomCharge.grew). Every transaction
// commits inside the apply that made it, whose update observers ygo fires before the apply returns
// (crdt/doc.go:795-798), so the room's update observer, which drains the room's update log there,
// finds each of the hook's updates while its apply is still running; any other origin is the
// server's own write, whose structure that observer charges (renderedReplica.chargeAdded). Nil, a
// total no load made, runs no apply of the hook's.
func (t *structTotal) hookApplies(origin any) bool {
	if t == nil {
		return false
	}
	t.originMu.Lock()
	defer t.originMu.Unlock()
	return t.origins[origin] > 0
}

// countedStructs is the struct total of the room's last exact count (countRoomStructs): the one
// its load made (newStructTotal), then each count since - the measurer's in its turn
// (recountLive), or a confirmation of the hook's (confirm). It is the struct count the room
// budget prices one copy of the room at (copyHeap); what the hook has admitted since the count is
// in the room's structure part already. Zero for a total no load made.
func (t *structTotal) countedStructs() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return int64(t.counted.structs)
}

// applying runs apply, which applies an update with origin, as one of the hook's own applies.
func (t *structTotal) applying(origin any, apply func() error) error {
	if t == nil {
		return apply()
	}
	t.originMu.Lock()
	if t.origins == nil {
		t.origins = make(map[any]int)
	}
	t.origins[origin]++
	t.originMu.Unlock()
	defer func() {
		t.originMu.Lock()
		if t.origins[origin]--; t.origins[origin] == 0 {
			delete(t.origins, origin)
		}
		t.originMu.Unlock()
	}()
	return apply()
}

// reserve reports whether doc, the room's live document, can take added within the decode cap, the
// unrendered bound and F beside every reservation in flight (admits), and if it can, reserves added
// for an apply the caller then makes and ends with landed, or abandons with unreserve.
func (t *structTotal) reserve(doc *crdt.Doc, added roomCount) bool {
	if t == nil {
		return t.admits(doc, added)
	}
	t.admissions.Lock()
	defer t.admissions.Unlock()
	if !t.admits(doc, added) {
		return false
	}
	t.inflight.Add(added.structs)
	t.inflightUnrendered.Add(added.unrendered)
	t.inflightFrame.Add(added.frame)
	return true
}

// landed moves added, a reservation whose apply has returned, from inflight to admitted: to
// admitted first, so a running total read between the two never misses it.
func (t *structTotal) landed(added roomCount) {
	if t != nil {
		t.admitted.Add(added.structs)
		t.admittedUnrendered.Add(added.unrendered)
		t.admittedFrame.Add(added.frame)
		t.inflight.Add(-added.structs)
		t.inflightUnrendered.Add(-added.unrendered)
		t.inflightFrame.Add(-added.frame)
	}
}

// unreserve gives back added, a reservation no apply followed or whose apply failed.
func (t *structTotal) unreserve(added roomCount) {
	if t != nil {
		t.inflight.Add(-added.structs)
		t.inflightUnrendered.Add(-added.unrendered)
		t.inflightFrame.Add(-added.frame)
	}
}

// running is the struct total as it stands - the last count, what the hook admitted since and
// what it has in flight - and whether a write the hook did not see has landed since the count.
func (t *structTotal) running() (total uint64, stale bool) {
	totals, stale := t.runningTotals()
	return totals.structs, stale
}

// runningTotals is the three totals as they stand, and whether a write the hook did not see has
// landed since the count. Each inflight is read before its admitted (structTotal gives why).
func (t *structTotal) runningTotals() (total roomCount, stale bool) {
	t.mu.Lock()
	counted, admitted, writes := t.counted, t.countedAdmitted, t.countedWrites
	t.mu.Unlock()
	inflight := roomCount{structs: t.inflight.Load(), unrendered: t.inflightUnrendered.Load(), frame: t.inflightFrame.Load()}
	now := roomCount{structs: t.admitted.Load(), unrendered: t.admittedUnrendered.Load(), frame: t.admittedFrame.Load()}
	return roomCount{
		structs:    counted.structs + inflight.structs + now.structs - admitted.structs,
		unrendered: counted.unrendered + inflight.unrendered + now.unrendered - admitted.unrendered,
		frame:      counted.frame + inflight.frame + now.frame - admitted.frame,
	}, t.writes.Load() != writes
}

// roomStructs is the room's struct total as it stands, which the apply heap's estimate takes as
// the most deleted runs the room's state can hold (peerWeighing.applyHeap). A total a server write
// made stale lags what the server's writes added since the last count, until the measurer's next
// count re-bases it (elementMeasurer.count). A nil total, a room no load listed, is counted
// afresh.
func (t *structTotal) roomStructs(doc *crdt.Doc) uint64 {
	if t == nil {
		return countRoomStructs(doc)
	}
	total, _ := t.running()
	return total
}

// admits reports whether doc, the room's live document, can take added within the decode cap,
// when added carries unrendered bytes within maxUnrenderedBytes, and when it carries frame bytes
// within heaviestHandoffBound (fitsBounds), beside every reservation in flight, which the running
// totals count; reserve calls it under the admissions lock. The running totals answer when they
// fit and are either fresh or short of every bound by more than their stale margins (nearBounds).
// Totals past a bound are confirmed by an exact count before they refuse, unless a confirmation
// was taken while they were fresh and less than structTotalRecheck structs, unrenderedRecheck
// unrendered bytes and frameRecheck frame bytes have been admitted since, which answers on the
// running totals instead; stale totals near a bound are always confirmed. A confirmation re-bases
// the totals and nothing else: the reservations in flight stay counted beside them.
//
// The count runs on the browser's read loop, so that browser's next frame waits for it, and under
// the admissions lock, so the room's other browsers' reservations wait for it too, though none of
// their applies does: an encode of the room's state and a walk of it, whose walk allocates
// nothing per struct and whose encode allocates its growing buffer. Measured on 2026-10-26 at a
// mebibyte of a room's shape, a 3,035,240-byte state: 71 to 80 ms and 9.5 to 10.6 MB in about
// 21,660 allocations a count (BenchmarkStructTotalConfirmation). It does not take the measurer's
// turn, which would put a keystroke behind another room's element count or a server write's
// decode.
//
// A nil total, a room no load listed, is counted afresh for each update.
func (t *structTotal) admits(doc *crdt.Doc, added roomCount) bool {
	if t == nil {
		return fitsBounds(countRoom(doc), added)
	}
	total, stale := t.runningTotals()
	switch {
	case !fitsBounds(total, added):
		if t.confirmationHolds(stale) {
			return false
		}
	case stale && nearBounds(total, added):
	default:
		return true
	}
	t.confirm(doc)
	total, _ = t.runningTotals()
	return fitsBounds(total, added)
}

// confirmationHolds reports whether the hook's last confirmation still answers for the totals: it
// was taken, nothing the hook did not see has landed since (stale), and the hook has admitted less
// than structTotalRecheck structs, unrenderedRecheck unrendered bytes and frameRecheck frame bytes
// since it.
func (t *structTotal) confirmationHolds(stale bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !stale && t.confirmed &&
		t.admitted.Load()-t.confirmedAdmitted.structs < structTotalRecheck &&
		t.admittedUnrendered.Load()-t.confirmedAdmitted.unrendered < unrenderedRecheck &&
		t.admittedFrame.Load()-t.confirmedAdmitted.frame < frameRecheck
}

// confirm re-bases the totals on an exact count of doc and records it as the hook's confirmation.
func (t *structTotal) confirm(doc *crdt.Doc) {
	admitted := t.recount(doc)
	t.mu.Lock()
	t.confirmed, t.confirmedAdmitted = true, admitted
	t.mu.Unlock()
}

// recountLive re-bases the totals on an exact count of the room's live document, if it is still
// resident: the measurer's count, in its turn.
func (t *structTotal) recountLive() {
	if t == nil {
		return
	}
	if live := t.live.Value(); live != nil {
		t.recount(live)
	}
}

// recount re-bases the totals on an exact count of doc, and returns the admissions it was counted
// against.
func (t *structTotal) recount(doc *crdt.Doc) roomCount {
	admitted := roomCount{structs: t.admitted.Load(), unrendered: t.admittedUnrendered.Load(), frame: t.admittedFrame.Load()}
	writes := t.writes.Load()
	counted := t.count(doc)
	t.mu.Lock()
	t.counted, t.countedAdmitted, t.countedWrites = counted, admitted, writes
	t.mu.Unlock()
	return admitted
}

// countRoom is the exact count of doc's three totals: its struct total (countRoomStructs), the
// unrendered bytes of every struct of its encoded state (unrenderedBytes), parked structs
// included, and that state's length, its whole-state frame, with what deleting each struct of it
// can add (deletionReserve), from one encode and one walk.
func countRoom(doc *crdt.Doc) roomCount {
	pending := doc.PendingStats()
	var held roomTally
	state := crdt.EncodeStateAsUpdateV1(doc, nil)
	tally, err := walkUpdate(state, fromRoom, &held)
	return roomCount{structs: structTotalOf(tally, err, pending), unrendered: held.unrendered, frame: uint64(len(state)) + held.reserve}
}

// roomTally is countRoom's visitor, summing each walked struct's unrendered bytes and its
// deletion reserve.
type roomTally struct{ unrendered, reserve uint64 }

func (r *roomTally) walkedStruct(walked updateStruct) {
	r.unrendered += unrenderedBytes(walked)
	r.reserve += deletionReserve(walked)
}

func (*roomTally) walkedDelete(_, _, _ uint64) {}

// countRoomStructs is the exact count of doc's struct total: the structs of its encoded state as
// ygo would decode them on the next load (walkUpdate, fromRoom), parked structs included, plus the
// splits each parked struct and each parked delete range can still cause once what it waits for
// arrives (crdt.Doc.PendingStats). The hook charged those splits when it admitted the update that
// parked them, and a count that dropped them would let them land uncounted.
//
// A state the walk refuses would not load again. One past the cap counts what the walk had read
// when it refused, which is past it; one refused for any other reason is a defect in the room or
// the walk, logged, and counted past the cap, so the room takes nothing more until it is rebuilt.
func countRoomStructs(doc *crdt.Doc) uint64 {
	pending := doc.PendingStats()
	tally, err := walkUpdate(crdt.EncodeStateAsUpdateV1(doc, nil), fromRoom, nil)
	return structTotalOf(tally, err, pending)
}

// structTotalOf is the struct total of a room whose encoded state walked to tally, refused with
// err or not, while it held pending (countRoomStructs).
func structTotalOf(tally updateTally, err error, pending crdt.PendingStats) uint64 {
	if err != nil && tally.structs <= maxUpdateItems {
		slog.Error("dispatch: count the structs of a room's own state", "error", err)
		tally.structs = maxUpdateItems + 1
	}
	return tally.structs + splitsAStructCanCause*uint64(pending.Items+pending.DeleteRanges)
}

// errApplyTooHeavy is why the hook refuses an update whose apply alone is estimated to allocate
// more than applyTooHeavyBytes: no turn could hold it, even alone. It is never applied, and the
// hook answers it with the document's own close, DocumentTooLargeCloseCode and
// DocumentTooLargeCloseReason, this error behind it (refusedWith), as it answers a frame the walk
// refuses.
var errApplyTooHeavy = errors.New("the update is heavier to apply than the pod can hold")

// goMemoryLimitBytes is the Go runtime's soft memory limit on the production pod: 90 per cent of
// the container's 4 GiB, which the server sets at boot (setGoMemoryLimit, cmd/dispatch/main.go:121-156).
const goMemoryLimitBytes = 4 << 30 * 9 / 10

// heapAboveTheReserve is what the Go memory limit leaves above the room budget, the cold reads'
// cache and the pressure reserve the budget's charges may pass its limit by: 3,865,470,566 -
// roomBudgetBytes 2,147,483,648 - documentReadBudget 268,435,456 - pressureReserveBytes
// 268,435,456 = 1,181,116,006 bytes. The architect's ruling splits it three ways: the apply-heap
// bound (applyHeapBound), the copy-heap bound (copyHeapBound) and slack named for what neither
// bound weighs (heapSlackBytes), a third each. The ruling's words are "a quarter of what the limit
// leaves above the reserve to each bound, and a quarter as named slack", which come to three
// quarters and leave the last quarter to nothing; a third each is taken as what "the three-way
// split" means, so that every byte of it has a named holder.
const heapAboveTheReserve = goMemoryLimitBytes - roomBudgetBytes - documentReadBudget - pressureReserveBytes

// applyHeapBound is the most heap, by their estimates (peerWeighing.applyHeap), that the pod's
// weighed applies of browsers' updates hold at once (applyHeaps): a third of heapAboveTheReserve,
// 1,181,116,006 / 3 = 393,705,335 bytes, about 375 MiB. The copy-heap bound (copyHeapBound), which
// holds the full-state decodes of loads, forks and folds, takes another third, and the slack
// (heapSlackBytes) the last, where the applies under unweighedApplyHeapBytes run. A weighed apply
// whose estimate passes the bound is never refused for it: it waits for the whole bound and then
// holds all of it, so it runs alone (applyHeaps.hold), as a copy past the copy-heap bound does.
// Only an estimate past applyTooHeavyBytes is refused (errApplyTooHeavy).
//
// It is not in the spec: it is the plan's departure 8, large applies taking turns, weighed by each
// apply's estimate rather than counted in slots. Every browser's first sync hands over its whole
// state, and ygo's apply decodes every struct of it, and every value of each, before decodeAndPark
// drops what the room already holds (sjawhar/ygo v1.54.1-sami.1, crdt/update.go:807-942, the
// values at :1222-1263), so fifty browsers reconnecting at once to rooms of heavy documents would
// otherwise decode fifty handoffs at the same moment. A count of slots sized for the heaviest
// handoff could not bound an update the editor never sends, whose apply allocates in proportion
// to the values it carries whatever the room holds: 25,165,779 bytes of null values in one struct
// for a client whose every clock the room held allocated 805,307,736 bytes in one apply, though it
// inserted nothing and added no struct. Measured on 2026-10-28
// (TestEveryApplysTransientHeapIsWithinItsEstimate), the 5,360,879-byte handoff of the heaviest
// document the server's own writes can leave is estimated at 71,356,658 bytes into the room
// holding it, against the 33,410,168 it allocates there, so five such handoffs apply at once, and
// at 234,999,499 into a fresh room, whose budget gives it the per-frame ceiling
// (perFrameScanCeiling) for the 49,156 items decoding a whole document into one holding nothing
// scans, against the 113,178,024 its apply allocates there, so one applies at a time. The
// 9,577,606-byte handoff of that document grown through the creep allowance (maxMessageBytes,
// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor) is estimated at 157,797,184
// into the room holding it, so two apply at once, and at 482,930,134 into a fresh room, past the
// bound, so it runs alone.
const applyHeapBound = heapAboveTheReserve / 3

// heapSlackBytes is the third of heapAboveTheReserve that neither bound grants, 1,181,116,006 / 3 =
// 393,705,335 bytes, about 375 MiB: the slack for what no bound weighs - the request path, the
// applies of browsers' updates under unweighedApplyHeapBytes, one for each browser's read loop at
// most, the measurer's parse of a rendering, up to about 130 MiB for a mebibyte of the heaviest
// shape (pmdoc/elements.go:31-37), and the collector, which needs headroom under the Go memory
// limit to free garbage before it reaches it. An apply estimated past applyHeapBound, which runs
// alone, may reach into it too, up to applyTooHeavyBytes.
//
// Nothing in Dispatch reads or sets GOGC: the collector runs at the runtime's own ratio, and what
// holds the pod inside its memory is the Go memory limit (goMemoryLimitBytes). At that default the
// heap goal is twice the live heap, which past half the limit is the limit itself, so a server
// whose budget is full keeps every byte of garbage the limit allows - the budget's unused room,
// both heap bounds and this slack together - and the memory it holds parks under the limit. That
// memory is what a pod's kernel charges it: what the runtime holds less what it has released,
// which is the figure the limit bounds. Measured on 2026-10-29 with the real-process drive of the
// worst case the bounds allow (cmd/dispatch,
// TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory, three steps: the budget filled,
// the cold reads' cache filled, the heaviest handoffs applying beside full copy bounds), it peaked
// by step at 2,563, 3,605 and 3,665 MiB of the pod's 4,096, 431 MiB under it, the drive taking
// 402 s and the keystrokes a person typed through both heavy steps answered in a 99th percentile
// of 185 ms.
//
// The VmRSS that run read was 2,601, 3,615 and 4,171 MiB, the last past the pod, and is not what a
// pod is charged: this repository's sandboxes run under gVisor, whose VmRSS keeps every page the
// runtime has handed back unless the madvise(MADV_DONTNEED) that released it covered 2 MiB or
// more, and the scavenger releases in smaller pieces (cmd/dispatch, peakResident, which reads the
// charged figure from the server's own memory trace). Dispatch's own pods are not sandboxed - the
// GKE Autopilot deployment names no runtimeClassName and no GKE Sandbox node pool
// (infra/gcp/dispatch_kube.go:492-501) - so what is charged there is what the limit bounds. The
// 4,302 and 4,424 MiB earlier runs of that drive recorded were VmRSS read on this sandbox, not
// memory any pod was charged.
const heapSlackBytes = heapAboveTheReserve / 3

// applyTooHeavyBytes is the estimate (peerWeighing.applyHeap) past which the hook refuses an
// update as too heavy to apply (errApplyTooHeavy): all of heapAboveTheReserve, 1,181,116,006
// bytes, both bounds and the slack together, so that no apply is refused that could run with every
// other weighed heap at rest. An update estimated past applyHeapBound and within it is never
// refused: it waits for the whole apply bound and then runs alone (applyHeaps.hold). The estimate
// bounds what an apply allocates from above: the heaviest server-written document's handoff is
// estimated at 71,356,658 bytes and allocates 33,410,168, and no handoff of the editor's is
// estimated past three times what it allocates (TestEveryApplysTransientHeapIsWithinItsEstimate).
//
// It stays above the estimate of every honest whole-state handoff at F, the heaviest frame the
// hook lets a browser hand over (heaviestHandoffBound), measured on 2026-10-28 into the room
// holding each (TestAnHonestHandoffAtTheHeaviestFrameIsNeverRefusedAsTooHeavy): 904,378,600 bytes
// for the heaviest, the long-edited document typed on until 4,096 structs short of the decode cap
// and padded to F by text nested under the margin, 276,737,406 under this line; 657,900,131 for
// the long-edited fixture padded the same way, and 404,291,526 for a paragraph. A frame of null
// values at the message floor, 56,623,059 bytes, which the editor never sends, is estimated at
// 3,737,127,910 (TestAnUpdateHeavierThanAnyApplyMayHoldIsNeverApplied), past it, and refused.
const applyTooHeavyBytes = heapAboveTheReserve

// unweighedApplyHeapBytes is the update's own share of its estimate (peerWeighing.ownApplyHeap)
// at or under which an admitted update applies without waiting on applyHeapBound: 2 MiB, over
// three times the own share of the editor's 64 KiB paste (the creep cap's size, far past a
// keystroke) into the heaviest document and over four hundred times a keystroke's, so typing never
// waits on a reconnect storm's handoffs (TestEveryApplysTransientHeapIsWithinItsEstimate). The
// room's share is left out of the comparison because it is the room's, not the update's: every
// transaction on a room pays for the room's clients and deleted items, the server's own writes
// included, and ygo runs one transaction at a time in a room (its document lock), so that share is
// paid at most once a room at any moment. An unweighed apply runs beside the bound, one per
// browser's read loop at most, in the slack neither bound grants (heapSlackBytes).
const unweighedApplyHeapBytes = 2 << 20

// applyHeaps is the service's bound on the estimated heap of its weighed applies, applyHeapBound
// of it. Its zero value is ready: the bound is made on first use. A waiter has no timeout - it is
// a browser's read loop waiting its turn, and the browser's next frame waits with it - and holds
// nothing while it waits, neither part of the bound nor its room's admission lock. Waiters are
// served in the order they came (semaphore.Weighted), so a heavy handoff is not passed over
// forever by lighter ones behind it.
type applyHeaps struct {
	once  sync.Once
	bound *semaphore.Weighted
}

// hold waits until it holds heap of the bound, or the whole bound for a heap past it, and returns
// what gives it back, which logs the grant where it waited long or held the whole bound
// (logHeapGrant). With a context that is never done Acquire returns only once it holds what it
// asked for, so it cannot fail.
func (h *applyHeaps) hold(heap uint64) (release func()) {
	h.once.Do(func() { h.bound = semaphore.NewWeighted(applyHeapBound) })
	held := int64(min(heap, applyHeapBound))
	started := time.Now()
	_ = h.bound.Acquire(context.Background(), held)
	granted := time.Now()
	return func() {
		h.bound.Release(held)
		logHeapGrant("apply", held, applyHeapBound, granted.Sub(started), time.Since(granted))
	}
}

// applyHeap is the estimate of what ygo's apply of the walked update allocates, frame its bytes,
// into a room of roomStructs structs (structTotal.roomStructs): an upper bound, every factor below
// set from TestEveryApplysTransientHeapIsWithinItsEstimate's measurement of ygo's apply at scale
// (runtime.MemStats.TotalAlloc across one apply, which bounds what the apply holds at its peak
// from above), into a room's bounded document (newRoomScans) with an update observer, as every
// room is. It is the update's own share (ownApplyHeap) and the room's (roomApplyHeap):
//
//   - the room's state vector, which ygo's apply copies into maps before it reads a struct
//     (crdt/update.go:552) and its transaction twice more (crdt/doc.go:742, :818), each a slot
//     for every client the room holds;
//   - the update the transaction made, which ygo encodes for the room's update observers at every
//     commit, one that changed nothing included (buildPhase2, crdt/doc.go:597-612, called at
//     :772), by walking the room's whole store and building the delete set of all of it, a range
//     for every deleted item before it compacts them (encodeV1Locked and buildDeleteSet,
//     crdt/update.go:231-271 and :502-515): a price for every deleted item of the room, of which
//     it holds no more than its structs and the structs the apply can hold (applyStructs).
//     875,008 bytes measured for a one-character keystroke into a string of 10,000 deleted items
//     among 10,000 live ones, against 2,328 into one of 20,000 live items and none deleted;
//   - the marks its scans make, one for each distinct item they pass (crdt/item.go:126-158), at
//     bytesOfAScanMark each: the items they can pass are the room's and those the apply makes,
//     no more than the room's structs and the structs the apply can hold, and never more than
//     the per-frame ceiling, the most any allowance gives an apply (roomScans). An update that
//     integrates no struct scans nothing, its drain included, since no clock of its moves.
//
// The room's parked queue costs an apply nothing in proportion to it unless the apply reaches it:
// a bounded document's apply keeps the parked structs and parked delete ranges by reference rather
// than copying them (beginRemoteApply, crdt/apply_rollback.go:129-151), and a client's parked
// delete ranges are retried only once an apply moves that client's clock past the first of them
// (retryParkedDeletes, crdt/delete_set.go:221-257). Measured on 2026-10-27, a keystroke allocates
// 4,240 bytes into a room holding the most parked structs its struct total admits, 349,517, and
// 4,240 into one holding the most parked delete ranges, 524,280. A retry an apply does start
// re-parks what it cannot apply yet, a copy of that client's parked ranges: 44,739,304 bytes for
// the first character of the client whose 524,280 ranges the room holds parked, 85.3 a range.
// Each parked range is splitsAStructCanCause structs of the room's struct total, which the room's
// share prices at applyHeapPerRoomStruct each, 512 bytes a range, so no term of its own prices it.
//
// What it leaves out is what depends on the shape of the room's lists, which no walk of the update
// can see: an item or a delete range splitting a room's Any or JSON item copies the values right of
// the split (ContentAny.Splice, crdt/content.go:170-174).
func (w *peerWeighing) applyHeap(frame int, roomStructs uint64) uint64 {
	return w.ownApplyHeap(frame) + w.roomApplyHeap(roomStructs)
}

// roomApplyHeap is the room's share of applyHeap: the three terms above that the room the update
// reaches decides - its clients, the structs reachable in it, and the marks the update's scans
// can make. The hook weighs the update's own share on its own (unweighedApplyHeapBytes) and adds
// this to it, rather than estimating the apply twice (weighPeerUpdate).
func (w *peerWeighing) roomApplyHeap(roomStructs uint64) uint64 {
	reachable := roomStructs + w.applyStructs()
	var marks uint64
	if w.structs > 0 {
		marks = bytesOfAScanMark * min(reachable, perFrameScanCeiling)
	}
	return applyHeapPerRoomClient*uint64(len(w.held)) + applyHeapPerRoomStruct*reachable + marks
}

// applyStructs is the most structs the walked update's apply can hold in the room's store before
// its transaction ends: each new struct with the splits its integration can cause, and the two
// splits of every delete range, one the room holds deleted already included, since ygo makes those
// splits as it applies the range and merges them back only as the transaction ends
// (deletedRuns.splitEnds).
func (w *peerWeighing) applyStructs() uint64 {
	return w.structs*(1+splitsAStructCanCause) + w.deletes*splitsAStructCanCause
}

// ownApplyHeap is the update's own share of applyHeap, what it costs whatever room it reaches:
// the apply itself, each byte of the frame, which bounds every string ygo copies out of it (root
// names, map keys, text, binary, type names, format keys, Any strings and JSON text), each delete
// range, each struct ygo decodes, and each struct it integrates with each byte of the frame
// again, which bounds what the observers' encode writes of them. It is the share that decides
// whether the update waits on the bound (unweighedApplyHeapBytes).
func (w *peerWeighing) ownApplyHeap(frame int) uint64 {
	heap := applyHeapPerApply + applyHeapPerFrameByte*uint64(frame) + applyHeapPerDeleteRange*w.deletes +
		w.decoded + w.integrated
	if w.structs > 0 {
		heap += applyHeapPerIntegratedFrameByte * uint64(frame)
	}
	return heap
}

// The estimate's factors, in bytes, each set with headroom from what
// TestEveryApplysTransientHeapIsWithinItsEstimate measures (2026-10-26: bytes one apply allocates
// into a document with an update observer, per struct of a frame of 20,000 unless said, decode
// and integration together unless said); that test holds the estimate over every measurement.
// What each covers is what ygo's decode allocates (decodeItem, crdt/update.go:865-1040;
// decodeContent, :1042-1184; decodeTypeContent, :1186-1251; decodeDeleteSet, :1253-1291), and
// what integrating and encoding again for the room's observers allocates (encodeItem, :249-389).
const (
	// applyHeapPerApply is the transaction, the decode's own state and the observers' encode of a
	// small update: 2,336 bytes measured for the whole apply of a keystroke into the heaviest
	// document, 3,224 for a deletion of one character from it.
	applyHeapPerApply = 4 << 10
	// applyHeapPerRoomClient is each client in the room's state vector, which ygo's apply copies
	// into maps sized for it. A map takes slots for its size past 8/7 rounded up to a power of
	// two, so a room of 57,345 clients, the least that rounds to 131,072 slots, pays the most a
	// client: 19,356,120 bytes measured for a keystroke into such a room, each client holding one
	// GC struct, 338 a client with the observers' encode of that struct as a deleted item
	// (applyHeapPerRoomStruct), against 299 a client in a room of 20,000.
	applyHeapPerRoomClient = 160
	// applyHeapPerRoomStruct is each deleted item of the room, bounded by each struct the room
	// holds, at every apply: 875,008 bytes measured for a keystroke into a string of 10,000
	// deleted items among 10,000 live ones, 88 a deleted item.
	applyHeapPerRoomStruct = 256
	// applyHeapPerFrameByte is each byte of the frame: a string ygo copies out of it is one
	// allocation of its bytes rounded up to a size class, at most twice them for a short one (a
	// string of 9 bytes takes 16), and JSON text is copied twice more (applyHeapPerJSONByte).
	applyHeapPerFrameByte = 2
	// applyHeapPerIntegratedFrameByte is each byte of the frame of an update that integrates a
	// struct, for the observers' encode of what it integrated: a buffer grown by appending, whose
	// allocations sum to about five times what it holds once it grows by a quarter at a time, and
	// the copy handed to the observers. 2,427,728 bytes measured for 220,018 bytes of Any strings
	// of 9 bytes in one struct, 11.0 a byte, and 1,592,072 for one string of 512 KiB, 3.
	applyHeapPerIntegratedFrameByte = 7
	// applyHeapPerRun is a GC struct or a skip: an Item and its content. 120 bytes measured for
	// each of maxUpdateItems GC structs into a room holding them, which decodes them and
	// integrates nothing, and for each skip.
	applyHeapPerRun = 192
	// applyHeapPerItem is an item of any content: the Item, its origins and its content's own
	// struct. 182 bytes measured for a margin entry of one null value into a room holding it.
	applyHeapPerItem = 192
	// applyHeapPerKey is an item that names a map key: the key.
	applyHeapPerKey = 64
	// applyHeapPerType is an item of a nested type: the type, its maps and, for an XML element,
	// its name.
	applyHeapPerType = 192
	// applyHeapPerNewStruct is a struct the update integrates: its place in the store and its
	// parent, the splits of the items its origins fall inside (crdt/update.go:682-684,
	// crdt/item.go:83-106), and the observers' encode of it. 218 bytes measured for a GC struct,
	// 327 for a one-character string naming both origins, 563 for one splitting the room's string
	// at both, 526 for a margin entry of one null value.
	applyHeapPerNewStruct = 384
	// applyHeapPerNewType is an integrated item of a nested type beyond applyHeapPerNewStruct: the
	// observers' encode of its type and name. 749 bytes measured for an XML element, 592 for an
	// XML text.
	applyHeapPerNewType = 160
	// applyHeapPerDeleteRange is a range of the delete set: its decoded range, the splits of the
	// items it starts and ends inside, and its place in the transaction's delete set. 689 bytes
	// measured for each of 10,000 ranges splitting the room's string, the room's deleted items
	// included, and 129 for a range of a client the room has not seen.
	applyHeapPerDeleteRange = 768
	// applyHeapPerAnyValue is each lib0 Any value, at every depth (anyValuesApplyHeap): its slot in
	// the item's values and their normalised copy, a boxed scalar or string header, and the copy a
	// partly held item's split makes of its values. 3,835,480 bytes measured for 100,000 null
	// values in one struct, 38 a value.
	applyHeapPerAnyValue = 64
	// applyHeapPerNewAnyValue is each Any value of an integrated item, as the observers' encode
	// writes it.
	applyHeapPerNewAnyValue = 8
	// applyHeapPerAnyArray is each Any array beyond its values: its boxed slice header.
	applyHeapPerAnyArray = 32
	// applyHeapPerAnyMap is each Any map: its header and its first group of slots, which a map of
	// one entry allocates whole. 392 bytes measured for a map of one entry with its null value,
	// the densest Any there is.
	applyHeapPerAnyMap = 384
	// applyHeapPerAnyMapEntry is each entry of an Any map past the map's first group of eight: its
	// slots, at a load as low as a map just past a growth leaves them. 124 bytes an entry
	// measured for maps of 10,000 entries, with each entry's key and value.
	applyHeapPerAnyMapEntry = 64
	// applyHeapPerJSONText is each JSON text ygo decodes with encoding/json (jsonTextApplyHeap):
	// its slot in the item's values and what encoding/json allocates for any text. 80 bytes
	// measured for a null text.
	applyHeapPerJSONText = 64
	// applyHeapPerNewJSONText is each JSON value of an integrated item, which the observers'
	// encode writes as JSON text again with an encoder of its own (fmtValToJSON,
	// crdt/update.go:1327-1348). 298 bytes measured for a number, 705 for a map read from legacy
	// lib0 Any.
	applyHeapPerNewJSONText = 384
	// applyHeapPerJSONContainer is each JSON object or array: an object's map with its first
	// group. 763 bytes measured for an object of one entry, 835 for an object of one empty object.
	applyHeapPerJSONContainer = 384
	// applyHeapPerJSONMember is each separator of a JSON text, a ',' or a ':', which bounds its
	// members: an array's element in a slice grown by appending, an object's entry in a map grown
	// by inserting. 112 bytes measured for each element of arrays of 10,000 numbers.
	applyHeapPerJSONMember = 128
	// applyHeapPerNewJSONMember is each separator of an integrated JSON text, a member the
	// observers' encode writes again, keys sorted and each written through encoding/json. 290
	// bytes measured for each entry of objects of 10,000 entries.
	applyHeapPerNewJSONMember = 128
	// applyHeapPerJSONByte is each byte of JSON text beyond applyHeapPerFrameByte: ygo copies the
	// text into a string and again into the bytes encoding/json reads (fmtValFromJSON,
	// crdt/update.go:1380-1389), and the strings it decodes copy it once more.
	applyHeapPerJSONByte = 2
	// applyHeapPerNewJSONByte is each byte of an integrated JSON text beyond
	// applyHeapPerIntegratedFrameByte, which the observers' encode escapes and copies again. 11.4
	// bytes a byte measured for strings of 1,000 escapes.
	applyHeapPerNewJSONByte = 6
)

// applyCost is a struct's share of the apply heap's estimate beyond its bytes, which
// applyHeapPerFrameByte prices with the frame's: decode is what decoding it costs, which every
// struct of an update does, and integrate is what integrating it and encoding it for the room's
// observers costs, which only a struct past the room's state does.
type applyCost struct {
	decode, integrate uint64
}

func (c *applyCost) add(other applyCost) {
	c.decode += other.decode
	c.integrate += other.integrate
}

// structApplyHeap is walked's applyCost. walkUpdate has read walked's content already and refuses
// an update it cannot, so the readers below read what they know is there.
func structApplyHeap(walked updateStruct) applyCost {
	switch walked.kind {
	case structGC, structSkip:
		return applyCost{decode: applyHeapPerRun, integrate: applyHeapPerNewStruct}
	}
	cost := applyCost{decode: applyHeapPerItem, integrate: applyHeapPerNewStruct}
	if walked.keyed {
		cost.decode += applyHeapPerKey
	}
	content, pos := walked.content, 0
	switch walked.kind {
	case structType:
		cost.add(applyCost{decode: applyHeapPerType, integrate: applyHeapPerNewType})
	case structAny:
		n, _ := readVarUint(content, &pos)
		cost.add(anyValuesApplyHeap(content, &pos, n))
	case structJSON:
		cost.add(jsonContentApplyHeap(content))
	case structEmbed:
		text, _ := readVarBytes(content, &pos)
		cost.add(jsonTextApplyHeap(text))
	case structFormat:
		readVarBytes(content, &pos)
		text, _ := readVarBytes(content, &pos)
		cost.add(jsonTextApplyHeap(text))
	}
	return cost
}

// jsonContentApplyHeap is the applyCost of a ContentJSON item's content. ygo reads its values as
// JSON text, and when the first starts with a lib0 Any tag and some text does not read, reads them
// all again as lib0 Any (decodeContent's wireJSON case, crdt/update.go:1051-1082), so such an item
// is charged both readings, the values slice the failed reading made included; integrated, every
// value is written as JSON text again, taken at four bytes of text for each byte of lib0 Any.
func jsonContentApplyHeap(content []byte) applyCost {
	pos := 0
	n, _ := readVarUint(content, &pos)
	legacy := n > 0 && pos < len(content) && isAnyTag(content[pos])
	start := pos
	var cost applyCost
	for range n {
		text, ok := readVarBytes(content, &pos)
		if ok {
			cost.add(jsonTextApplyHeap(text))
		}
		if legacy && (!ok || !jsonUnmarshals(text)) {
			pos = start
			values := anyValuesApplyHeap(content, &pos, n)
			return applyCost{
				decode:    cost.decode + n*applyHeapPerJSONText + values.decode,
				integrate: n*applyHeapPerNewJSONText + 4*applyHeapPerNewJSONByte*uint64(len(content)),
			}
		}
		if !ok {
			break
		}
	}
	return cost
}

// jsonTextApplyHeap is the applyCost of one JSON text ygo decodes with encoding/json: the text, its
// containers and its separators, counted outside its strings, and its bytes beyond the frame's
// price.
func jsonTextApplyHeap(text []byte) applyCost {
	containers, separators := uint64(0), uint64(0)
	for j := 0; j < len(text); j++ {
		switch text[j] {
		case '"':
			for j++; j < len(text) && text[j] != '"'; j++ {
				if text[j] == '\\' {
					j++
				}
			}
		case '{', '[':
			containers++
		case ',', ':':
			separators++
		}
	}
	return applyCost{
		decode: applyHeapPerJSONText + containers*applyHeapPerJSONContainer + separators*applyHeapPerJSONMember +
			uint64(len(text))*applyHeapPerJSONByte,
		integrate: applyHeapPerNewJSONText + separators*applyHeapPerNewJSONMember +
			uint64(len(text))*applyHeapPerNewJSONByte,
	}
}

// anyValuesApplyHeap is the applyCost of the n lib0 Any values at *pos in buf, each value at every
// depth with its arrays and maps, read as anycodec's readAny reads them
// (internal/anycodec/decoder.go:261-393).
func anyValuesApplyHeap(buf []byte, pos *int, n uint64) applyCost {
	var shape anyShape
	for range n {
		if !shape.value(buf, pos, 0) {
			break
		}
	}
	return applyCost{
		decode: shape.values*applyHeapPerAnyValue + shape.arrays*applyHeapPerAnyArray +
			shape.maps*applyHeapPerAnyMap + shape.entries*applyHeapPerAnyMapEntry,
		integrate: shape.values * applyHeapPerNewAnyValue,
	}
}

// anyShape counts the parts of lib0 Any values ygo's decode allocates for: values at every depth,
// arrays, maps, and map entries past each map's first group of eight slots.
type anyShape struct {
	values, arrays, maps, entries uint64
}

// value reads one Any value at *pos into the shape and reports whether it read.
func (s *anyShape) value(buf []byte, pos *int, depth int) bool {
	if depth > maxAnyDepth || *pos >= len(buf) {
		return false
	}
	tag := buf[*pos]
	*pos++
	s.values++
	switch tag {
	case 125: // integer
		return readVarInt(buf, pos)
	case 124: // float32
		*pos += 4
	case 123, 122: // float64, bigint64
		*pos += 8
	case 119, 116: // string, binary
		_, ok := readVarBytes(buf, pos)
		return ok
	case 117, 118: // array, object
		n, ok := readVarUint(buf, pos)
		if !ok {
			return false
		}
		if tag == 117 {
			s.arrays++
		} else {
			s.maps++
			s.entries += n - min(n, 8)
		}
		for range n {
			if tag == 118 {
				if _, ok := readVarBytes(buf, pos); !ok {
					return false
				}
			}
			if !s.value(buf, pos, depth+1) {
				return false
			}
		}
	}
	return *pos <= len(buf)
}
