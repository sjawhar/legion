package docs

import (
	"errors"
	"fmt"
	"log/slog"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// This file holds the peer update hook itself (weighPeerUpdate), the bounds it admits a
// browser's update on and the closes it refuses one with. What one walk weighs an update at is
// peerWeighing, what a room's totals admit it against is roomTotals, and what its apply is
// estimated to allocate is applyHeap.

// A browser's update reaches its room through no check of the server's own: applyLive weighs every
// write a caller makes by the document it leaves (growth.go), and a browser's update runs through
// none of it, so a browser can carry a document past the bound every server write obeys
// (refuseGrowth). What stops that here is weighPeerUpdate, ygo's Server.OnPeerUpdate, which the
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
// (renderedReplica.publishSize) and the room's struct total (roomTotals): work proportional to the
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
// pmdoc.MaxDocumentElements and heavier than it was (weigh). What a browser can
// leave live past it - about 64 KiB of headings - goes on saving and syncing; the server's writes
// that would grow it are refused until someone shortens it, a write that leaves it no heavier
// passes, and a rebuild from its latest stored version restores it whatever it weighs
// (TestARebuildFromTheLatestVersionRestoresItWhateverItWeighs).
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
// inserts or deletes (roomTotals).
//
// The document has a third bound, which neither bytes nor structs see: what a room holds that is
// not text, a value of any size in one clock and one struct. The server's own writes bound the
// margin's records at maxMarginBytes and the anchor marks' ids at maxAnchorBytes (growth.go), and a
// browser's update runs through neither: measured on 2026-10-26, eighty browser updates each
// writing a million bytes into the margin were all admitted, and took the room's whole-state frame
// from 264 bytes to 80,001,457 with its rendering at 42, past the message floor every browser of
// the document then reconnects against. So the room keeps an unrendered total beside its struct
// total, and the hook refuses with DocumentTooLargeCloseCode an update that adds unrendered bytes
// past maxUnrenderedBytes (unrenderedBytes, roomTotals).
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
// (pmdoc.MaxDocumentElements), so an update can leave a document as long as it was, or shorter, and
// still multiply what its room holds. What an update inserts is what can add an element, so an
// update that inserts nothing is the one update that cannot grow the rendering; but a delete that
// cuts the middle of an item splits it, which adds structs, so even deletes are bounded, by the
// reserve. A struct born deleted - a GC run, a deleted item - renders nothing either and inserts
// nothing here (peerWeighing.weighStruct), and a flood of them is bounded by that same reserve,
// which its structs are charged against. That refusal is not the document's own bound and is not
// folded into it: 4413 is
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
// when what it carries that the room does not already hold and can render - the walk's clocks
// beyond the room's state vector, born deleted ones apart (peerWeighing.weighStruct) - is under
// maxPeerInsertAtTheBound, or, past the creep cap, only when
// it inserts nothing at all. Then the room's totals admit it only while what it would add keeps
// its struct total within maxUpdateItems, its unrendered bytes, when it adds any, within
// maxUnrenderedBytes, and its frame total, when it inserts anything, within heaviestHandoffBound
// (roomTotals.admits). Each refusal of the five is the document's own, DocumentTooLargeCloseCode.
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
// walk's refusal included, with DocumentTooLargeCloseCode and DocumentRebuiltCloseReason, a reason
// of its own rather than the one a used-up scan total is refused with: the document was just
// rebuilt, so no rebuild of it would help, and the editor tells the person it was rebuilt, builds
// its document afresh from the rebuilt room and drops its saved rows typed against the old history
// as rows of a rebuilt document (connection.ts refusedEditNotice, pending-sync.ts addsParking).
// An update that adds an entry to the retired clients' list, overwrites one or
// deletes one is refused as the editor's never-sent content is: only a rebuild writes the list,
// and nothing the editor runs writes or deletes an entry of it (update_walk.go's search).
//
// While the room budget is full (roomBudget.full), an update the document's bounds admit is then
// refused with DocumentBusyCloseCode when it inserts anything (refusedUnderPressure), whatever it
// leaves the document's bytes at, since an insert of fewer bytes than it deletes can still add
// elements. An update does not say what it deletes without the document to measure it against, so
// an edit that replaces text with less of it is refused too, as the creep cap refuses it. One that
// only deletes, or carries nothing but structs born deleted, is admitted while what it adds to
// the charges keeps them within the budget's limit
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
// it holds its estimate, the marks its scans can make included, out of the bound
// (Service.applyHeaps), or
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
// which the socket path refuses to open in the first place (refuseOutsideSchema),
// or one no load listed: the byte bound weighs its updates on their own bytes, as if the room
// rendered nothing, and the creep cap, which needs a rendering to measure, never applies to it.
//
// An apply that fails leaves no trace but its scans. The room's bounded document takes every
// remote update it refuses back out whole, whatever the error, and fires no observer for it
// (crdt/apply_rollback.go:9-21), so the hook takes back what it booked for the update: its
// reservation of the struct total (roomTotals.unreserve), a structure charge taken before the
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
	weighing := s.roomWeighingOf(info.Doc)
	charge := weighing.roomCharge()
	structs, scans := weighing.totals()
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
		return documentRebuilt()
	}
	if err != nil {
		return refusedWith(documentUpdateRefused(), err)
	}
	if weighed.forged {
		return refusedWith(documentUpdateRefused(),
			&updateRefusal{rule: errUpdateNotFromEditor, reason: "a change to the retired clients' list, which only a rebuild writes"})
	}
	bytes, _, measured := weighing.size()
	if !measured {
		bytes = 0
	}
	own := weighed.ownApplyHeap(len(info.Update))
	weighs := own > unweighedApplyHeapBytes
	heap := own + weighed.roomApplyHeap(structs.roomStructs(info.Doc))
	if tooHeavy := s.memory.ApplyTooHeavy; heap > uint64(tooHeavy) {
		return refusedWith(documentTooLarge(), fmt.Errorf("%w: its apply is estimated to allocate %d bytes, past the %d bytes the pod lets any apply hold, even alone",
			errApplyTooHeavy, heap, tooHeavy))
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
	// is in flight (roomTotals.reserve). No lock is held across the apply, so another browser
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
	// (roomTotals.recount). Its delete ranges are listed as the room's, so a later update
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
// one its room's used-up scan total refuses - permanent as documentTooLarge is and under the same
// code, with a reason of its own: the editor offers the rebuild that would take the edit.
func documentNeedsRebuild() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentNeedsRebuildCloseReason,
	}
}

// documentRebuilt is the refusal of an update carrying the history a rebuild of the document
// replaced (weighPeerUpdate), permanent as documentTooLarge is and under the same code, with a
// reason of its own: the document has just been rebuilt, so the editor says so and offers no
// rebuild, where documentNeedsRebuild's does.
func documentRebuilt() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentRebuiltCloseReason,
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
// one that inserts content the room does not already hold (peerWeighing.inserted), and one that
// inserts none - a delete, a frame of structs born deleted - once its
// structure would take the charges and reservations past the limit and the service's pressure
// reserve beyond it (MemoryBounds.PressureReserve). An update it admits is charged to charge, the
// room's, in the same look at the budget
// (roomBudget.growWithin), so the hook does not charge it again after its apply.
func (s *Service) refusedUnderPressure(weighed *peerWeighing, charge *roomCharge) bool {
	if weighed.inserted > 0 {
		return true
	}
	structure := weighed.structureHeap()
	return structure > 0 && !s.budget.growWithin(charge, structure, s.memory.PressureReserve)
}

// roomWeighingOf is what the hook reads of the room holding live (keepReplica), or nil for a
// document no room's load listed. It is a load off the listing the room's reads already go through
// (holdLive), so the peer's read loop takes neither the replica's lock, which every browser in the
// room waits behind (AGENTS.md:559-562), nor the room's state lock; and the listing holds the
// weighing strongly, so the read makes no weak pointer strong, which the collector can block for
// the rest of a mark phase (roomWeighing).
func (s *Service) roomWeighingOf(live *crdt.Doc) *roomWeighing {
	listed, ok := s.replicas.Load(weak.Make(live))
	if !ok {
		return nil
	}
	return listed.(*roomListing).weighing
}
