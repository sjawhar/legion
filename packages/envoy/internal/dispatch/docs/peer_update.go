package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// This file holds the peer update hook itself (weighPeerUpdate), the allowance it builds for each
// browser's update, the roots it lets one name, and the closes it refuses one with.

// A browser's update reaches its room through no check of the server's own: applyLive weighs every
// write a caller makes by the document it leaves (growth.go), and a browser's update runs through
// none of it, so a browser can carry a document past the bound every server write obeys
// (refuseGrowth). What stops that here is weighPeerUpdate, ygo's Server.OnPeerUpdate, which the
// room runs in place of its own apply for each update and whole-state message a read-write browser
// sends.
//
// It does not run applyLive's checks. Those weigh what a write leaves, which costs a fork of the
// document, a tree build and a full markdown render - about 420 ms for the fork alone at 1 MiB and
// a whole further copy of the document, with the render about 160 ms (AGENTS.md:563-602,
// :780-781) - against a keystroke on such a document that already takes between half a second and
// a second. What it does instead is hand ygo an allowance built from what it can see without
// reading the update at all - the update's own length, what the room already holds
// (crdt.Doc.StructCount), the room's last published rendering (renderedReplica.publishSize) and
// its tracked state bytes - and let the apply itself count what the update decodes, integrates
// and adds against that allowance, rolling the whole apply back where it passes
// (crdt.WithApplyBudget). For a keystroke that is a handful of comparisons; nothing here is
// proportional to the document or to the update.
//
// The hook weighs bytes and not elements. Counting a rendering's elements is a whole goldmark
// parse, 58 to 89 ms at 524 KiB, which no keystroke can pay on a one-processor pod. No cheap
// exact count exists either: a render that counts what it writes agreed with the server's count
// over 1,127 documents, but took 1,404 lines emulating goldmark and still disagreed for a live
// tree whose rendering reads back as something else, which is what a browser holds. So the
// element bound is kept where it already is: a server write carrying caller text is refused when
// it would leave a document past pmdoc.MaxDocumentElements and heavier than it was (weigh). What
// a browser can leave live past it - about 64 KiB of headings - goes on saving and syncing; the
// server's writes that would grow it are refused until someone shortens it, a write that leaves
// it no heavier passes, and a rebuild from its latest stored version restores it whatever it
// weighs (TestARebuildFromTheLatestVersionRestoresItWhateverItWeighs).
//
// The bound is not a ceiling the document cannot pass: the rendering the hook compares against is
// as recent as the room's last update observer, so a document passes it by the updates in flight
// when it crosses, and this is all that is claimed here.
//
// The document has a second bound of its own, which bytes cannot see: the decode cap. ygo decodes
// at most maxUpdateItems structs in one update, and a room's whole state is one update to the next
// load that reads it, so a room whose state holds more could never be loaded again once its
// updates were folded. A browser can get there without passing the byte bound - a frame of GC
// structs is two bytes a struct and renders nothing - so the apply's allowance of structs is what
// is left of the cap above what the room already holds, and an apply that would integrate more is
// refused with DocumentTooLargeCloseCode and taken back out whole.
//
// The document has a third bound, which neither bytes nor structs see: what a room holds that is
// not text, a value of any size in one clock and one struct. The server's own writes bound the
// margin's records at maxMarginBytes and the anchor marks' ids at maxAnchorBytes (growth.go), and
// a browser's update runs through neither: measured on 2026-10-26, eighty browser updates each
// writing a million bytes into the margin were all admitted, and took the room's whole-state frame
// from 264 bytes to 80,001,457 with its rendering at 42, past the message floor every browser of
// the document then reconnects against. What answers that now is the state cap
// (maxRoomStateBytes), which bounds the room's whole encoded state whatever the bytes of it
// render.
//
// The hook has a third state, which is the room budget's rather than the document's: while the
// open rooms are past their budget and nothing can be closed (roomBudget.full), an apply may
// integrate no struct and no byte of content in any room, so an update that only deletes is
// admitted and every other is refused with the transient DocumentBusyCloseCode. Bytes cannot be
// that test: an update can leave a document as long as it was, or shorter, and still multiply
// what its room holds. What an update integrates is what can grow the room, and an update that
// integrates nothing of its own is the one update that cannot. That refusal is not the document's
// own bound and is not folded into it: 4413 is permanent and the editor drops the refused edit,
// where 4503 is transient and the editor keeps it, to send once the pressure passes.

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a room's measurement may creep before only an update
// that inserts nothing at all is admitted, so a document that has crossed can still be trimmed or
// split and nothing can grow it further.
const maxPeerByteCreep = 64 << 10

// maxRoomStateBytes is the most encoded state one room may hold: a quarter of the message floor,
// derived from it here rather than written as a figure of its own, because the two are one rule.
// With the frame total gone, this is what keeps a browser's whole-state handoff inside the floor
// A1 asks it to clear: a room at the cap hands over a frame about the size of its state, and
// four such frames fit the floor. Writing the two apart is what let them drift: at 13.5 MB
// against a 54 MiB floor, the branch's own long-edited document at the decode cap, 14,427,285
// bytes, was already past the cap, and four times it was past the floor - so that document could
// take no byte from any browser and could not hand its state back either, which is a person's
// document that stops working. At 16,777,216 it has 2,349,931 bytes to spare
// (TestTheHeaviestStateAtTheDecodeCapFitsTheRoomsStateCapAndTheFloor). A room at the cap is
// charged 16,777,216 x 28 = 469,762,048 bytes of the budget, so four such rooms fit the
// calibration's 2 GiB.
const maxRoomStateBytes = maxMessageBytes / 4

// maxDecodedValuesPerApply is the most values one apply of a browser's update may decode: each
// struct, each lib0 Any value at any depth, each Any map entry and each JSON member
// (crdt.ApplyBudget.Decoded). 1,200,000 is past the decode cap itself (maxUpdateItems,
// 1,048,576), so an honest whole-state handoff of a document at the cap decodes within it, and
// far under what the heaviest honest apply the pod allows holds: 25,165,779 null values in one
// struct for a client whose every clock the room held allocated 805,307,736 bytes in one apply
// though it inserted nothing, and the decode count is what refuses such a frame as it is read
// rather than once it is all in memory
// (dispatch://INFRA-179/artifact/infra179-simplify-measured-md).
const maxDecodedValuesPerApply = 1_200_000

// peerRule names the rule that set one field of a browser's update's allowance, so a refusal the
// apply's own counters report can say which rule refused it and with which close.
type peerRule uint8

const (
	// ruleDecodedValues: the apply decoded more values than any apply may (maxDecodedValuesPerApply).
	ruleDecodedValues peerRule = iota
	// ruleDecodeCap: the structs it integrated would take the room past maxUpdateItems.
	ruleDecodeCap
	// ruleByteBound: the room's rendering is at or over pmdoc.MaxDocumentBytes, so the update may
	// carry at most maxPeerInsertAtTheBound bytes of content.
	ruleByteBound
	// ruleCreepCap: the rendering has crept maxPeerByteCreep past the bound, so the update may
	// carry no content at all.
	ruleCreepCap
	// ruleStateCap: the bytes it added would take the room's state past maxRoomStateBytes.
	ruleStateCap
	// rulePressure: the open rooms are past their budget and nothing can be closed, so no apply
	// may integrate anything (roomBudget.full).
	rulePressure
)

// refusal is the close a browser is given for an update this rule refused: the document's own
// permanent close for every rule but two - the state cap, which only a rebuild clears, and the
// pressure, which passes by itself.
func (r peerRule) refusal() *websocket.PeerClose {
	switch r {
	case ruleStateCap:
		return documentNeedsRebuild()
	case rulePressure:
		return &websocket.PeerClose{Code: contracts.DocumentBusyCloseCode, Reason: contracts.DocumentBusyCloseReason}
	default:
		return documentTooLarge()
	}
}

// why is the rule in plain words, for the one line a refusal leaves (logRefusal).
func (r peerRule) why() string {
	switch r {
	case ruleDecodeCap:
		return "its apply would take the document past the items ygo decodes in one update"
	case ruleByteBound:
		return "its apply inserts into a document already at its byte bound"
	case ruleCreepCap:
		return "its apply inserts into a document that has crept past its byte bound"
	case ruleStateCap:
		return "its apply would take the document's state past what one room may hold"
	case rulePressure:
		return "the open documents are past their memory budget and nothing can be closed"
	default:
		return "its apply decodes more than the pod lets any apply decode"
	}
}

// peerAllowance is what one browser's update applies under: the budget ygo holds its apply to
// (crdt.ApplyBudget) and, for each of the two fields a rule can close the browser for, the rule
// that set it and that rule's own figure, so a refusal names the rule rather than the arithmetic.
type peerAllowance struct {
	budget crdt.ApplyBudget
	// structs and bytes name the rule each of budget's two bounded fields came from, and
	// structsLimit and bytesLimit that rule's own figure, which the refusal's line carries.
	structs, bytes           peerRule
	structsLimit, bytesLimit int64
}

// peerApplyAllowance is the allowance an update of update bytes applies under in a room already
// holding structs structs and stateBytes bytes of tracked state, whose last published rendering
// measures rendering bytes (measured false for a room with none), while the budget is full or
// not.
//
// Each field is the least of the rules that apply to it, and each rule's own figure is kept so
// the refusal can name it:
//
//   - Decoded is maxDecodedValuesPerApply always: no origin decodes more.
//   - Structs is what is left of the decode cap above what the room holds (the decode cap rule).
//   - Bytes is no limit below the byte bound; maxPeerInsertAtTheBound at or over it (the bound);
//     nothing past the creep cap (the creep cap); and never more than what is left of
//     maxRoomStateBytes above the room's tracked state (the state cap).
//   - With the budget full, Structs and Bytes are both nothing (the pressure), so the one update
//     admitted is one that integrates nothing of its own: a delete, or a frame of structs the
//     room already holds.
//
// A negative figure is clamped to nothing: a room already past a bound takes no more, and ygo
// reads a field below zero as no limit at all. A room with no rendering to measure is weighed on
// the update's own bytes alone, so the caller passes a rendering of nothing for it
// (roomWeighing.size answers sizeUnmeasured, which is no length).
func peerApplyAllowance(update, rendering, stateBytes int64, structs int, measured, full bool) peerAllowance {
	if !measured {
		rendering = 0
	}
	allowance := peerAllowance{
		budget:       crdt.ApplyBudget{Decoded: maxDecodedValuesPerApply, Bytes: -1},
		structs:      ruleDecodeCap,
		bytes:        ruleStateCap,
		structsLimit: maxUpdateItems,
		bytesLimit:   maxRoomStateBytes,
	}
	allowance.budget.Structs = clampedStructs(int64(maxUpdateItems) - int64(structs))
	switch {
	case measured && rendering >= pmdoc.MaxDocumentBytes+maxPeerByteCreep:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, ruleCreepCap, maxPeerByteCreep
	case rendering+update >= pmdoc.MaxDocumentBytes:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = maxPeerInsertAtTheBound, ruleByteBound, maxPeerInsertAtTheBound
	}
	if state := clampedBytes(maxRoomStateBytes - stateBytes); allowance.budget.Bytes < 0 || state < allowance.budget.Bytes {
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = state, ruleStateCap, maxRoomStateBytes
	}
	if full {
		allowance.budget.Structs, allowance.structs, allowance.structsLimit = 0, rulePressure, 0
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, rulePressure, 0
	}
	return allowance
}

// clampedStructs is what is left of the decode cap, never below nothing and never above the cap
// itself: a room already past it may add no struct, and a field below zero would read as no
// limit at all (crdt.ApplyBudget).
func clampedStructs(left int64) int {
	if left <= 0 {
		return 0
	}
	if left > int64(maxUpdateItems) {
		return maxUpdateItems
	}
	return int(left)
}

// clampedBytes is what is left of a byte bound, never below nothing. It is a count of bytes, so
// nothing caps it but the bound it came from: capping it at the decode cap's count of structs,
// which is the smaller number, would refuse a paste of a mebibyte into an empty room in the name
// of a state cap of 13.5 MB.
func clampedBytes(left int64) int {
	if left <= 0 {
		return 0
	}
	return int(left)
}

// peerAllowedRoots is the roots a browser's update may name: the document's fragment and its
// margin, and nothing else, so a subdocument, an unknown root and a write of the retired clients'
// list a rebuild left are all refused by ygo as it decodes them (crdt.WithAllowedRoots), with no
// walk of the update and no apply of any part of it. Only a rebuild writes the retired clients'
// list, and nothing the editor runs writes or deletes an entry of it.
var peerAllowedRoots = []string{fragmentName, marksMapName}

// roomApplies is the allowance of each of the hook's applies in flight in one room, by the origin
// ygo applies it with (provider/websocket/peer_update.go:84-98), and the room's answer to ygo's
// two per-apply questions: what this apply may decode, integrate and add (crdt.WithApplyBudget)
// and which roots it may name (crdt.WithAllowedRoots). ygo asks both under the document's lock as
// each remote apply begins, so neither may call the document; both take nothing but mu, which
// nothing holds across a call into the document.
//
// An apply of any other origin is the server's own - an API write's publish, an injected update,
// settlement's repair - and is bounded by nothing here and allowed every root: each of those
// weighs what it leaves by its own rules (growth.go), and a repair that could not write the
// margin would leave the document broken.
type roomApplies struct {
	mu        sync.Mutex
	allowance map[any]crdt.ApplyBudget
}

// newRoomApplies sets live, a room's document, to apply every browser's update under the
// allowance the hook reserved for it and to refuse one naming any root but the fragment and the
// margin, as newRoomScans sets its scan budget. From here live applies every remote update whole
// or not at all.
func newRoomApplies(live *crdt.Doc) *roomApplies {
	applies := &roomApplies{}
	live.SetApplyBudget(applies.budget)
	live.SetAllowedRoots(applies.roots)
	return applies
}

// budget is ygo's per-apply allowance: the one reserved for the hook's apply with this origin,
// and no limit for every other origin.
func (a *roomApplies) budget(origin any) crdt.ApplyBudget {
	a.mu.Lock()
	defer a.mu.Unlock()
	if budget, reserved := a.allowance[origin]; reserved {
		return budget
	}
	return crdt.ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1}
}

// roots is ygo's per-apply root list: the browser's two for the hook's applies, and nil - every
// root - for every other origin.
func (a *roomApplies) roots(origin any) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, reserved := a.allowance[origin]; reserved {
		return peerAllowedRoots
	}
	return nil
}

// applying runs apply, one of the hook's own applies with origin, under budget: reserved before
// the apply and dropped as it returns, so ygo's two questions are answered from it for exactly
// this apply and every other origin's apply is unbounded. Nil, a room no load listed, runs the
// apply under whatever the document itself was built with.
func (a *roomApplies) applying(origin any, budget crdt.ApplyBudget, apply func() error) error {
	if a == nil {
		return apply()
	}
	a.mu.Lock()
	if a.allowance == nil {
		a.allowance = make(map[any]crdt.ApplyBudget)
	}
	a.allowance[origin] = budget
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.allowance, origin)
		a.mu.Unlock()
	}()
	return apply()
}

// weighPeerUpdate is ygo's Server.OnPeerUpdate: the room hands it every update and whole-state
// message a read-write browser sends, with the apply that would put it in the room, and a
// read-only browser never reaches it (sjawhar/ygo v1.55.1-sami.1,
// provider/websocket/peer_update.go:84-154, provider/websocket/peer.go:271-277).
// An update it does not apply is neither stored nor relayed to the room's other browsers, and the
// *websocket.PeerClose it refuses with closes that one browser with that code and reason.
//
// It refuses one update before the apply, and every other refusal is the apply's own. The one is
// an update carrying a client group of the history a rebuild replaced - a group of a client the
// rebuilt history lists as retired, starting below the clock it had reached (retiredMapName),
// read from the update's own header (crdt.ParseUpdateMeta, which decodes no content). It comes
// from a browser that held the document before the rebuild and still holds that history:
// applied, its whole-state message would put the old document beside the rebuilt one, every
// block twice, and carry the room back toward the cap the rebuild trimmed. It is refused with
// DocumentTooLargeCloseCode and DocumentRebuiltCloseReason, a reason of its own rather than the
// one a used-up scan total is refused with: the document was just rebuilt, so no rebuild of it
// would help, and the editor tells the person it was rebuilt, builds its document afresh from
// the rebuilt room and drops its saved rows typed against the old history as rows of a rebuilt
// document (connection.ts refusedEditNotice, pending-sync.ts addsParking). An update whose
// header does not read at all is refused DocumentUpdateRefusedCloseReason there and then: ygo
// would refuse the same bytes.
//
// Everything else is the apply's. The hook builds the allowance this update applies under
// (peerApplyAllowance) and the roots it may name (peerAllowedRoots), hands both to the room's
// document for this one apply (roomApplies.applying) and runs it. ygo counts what the apply
// decodes, integrates and adds as it goes and refuses the whole apply where any count passes its
// field of the allowance, leaving the document exactly as it was, firing no observer and
// reporting what it reached (crdt.RefusedUpdateError); the dimension at or past its limit names
// the rule that set that field, and the rule names the close (peerRule.refusal). So the walk that
// read every browser's update before it applied - a second decode of the same bytes on the peer's
// read loop - is gone, and with it the three running totals the room kept to weigh that walk
// against.
//
// Every refusal before or inside the apply closes the browser. Under sjawhar/ygo v1.51.3-sami.7
// a hook's refusal answered the browser SyncStatus(0) and left it connected; upstream
// reearth/ygo#304 was merged in the form that closes it, with 4205 "Reset Connection" for a hook
// error that is no *websocket.PeerClose. 4205 is what @hocuspocus/provider 2.x reconnects at once
// after, and the client would send the refused frame again, a loop of full syncs; 4413 is the
// permanent band, which the editor drops the refused edit for, so every refusal of a rule the
// browser cannot pass carries it.
//
// Every apply's scans are held to an allowance besides (roomScans): the per-frame ceiling for an
// update that holds a load slot or what is left of the room's scan total, whichever is less,
// reserved before the apply and settled from what its scans passed once it returns, and
// usedUpScanBudget once the room's total is used up. An update whose merge would scan past it is
// refused DocumentTooTangledCloseReason, or DocumentNeedsRebuildCloseReason on a used-up room
// total.
//
// An update over unslottedApplyBytes takes a slot of the pod's load concurrency before it applies
// (loadSlots), so a reconnect storm's whole-state handoffs decode as many at a time as the pod
// has slots rather than all at once; typing never waits on it.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema), or one no load
// listed: the byte bound weighs its updates on their own bytes, as if the room rendered nothing,
// and the creep cap, which needs a rendering to measure, never applies to it.
//
// An apply that fails leaves no trace but its scans. The room's bounded document takes every
// remote update it refuses back out whole, whatever the error, and fires no observer for it
// (crdt/apply_rollback.go:9-21), so the hook has nothing to give back: what its scans passed
// before the refusal it reports (crdt.RefusedUpdateError), and the room's scan total keeps it
// (roomScans.settle), since the work was done. Any other error the apply returns - one that would
// park structs or delete ranges past the room's pending cap (MaxPendingItems, which the room
// holds from the end of its load) - is returned as it is, the one error the hook answers that is
// no *websocket.PeerClose, so ygo answers that browser SyncStatus(0) and leaves it connected,
// exactly as a room without this hook does: the apply ran, which is what keeps a refused browser
// connected where a refusal before the apply closes it
// (provider/websocket/peer_update.go:139-154).
//
// A successful apply charges nothing here: the room's update observer drains the update inside
// the apply that committed it, adds its bytes to the room's tracked state and weighs the room
// again from the sum (renderedReplica.chargeAdded, roomCharge).
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	weighing := s.roomWeighingOf(info.Doc)
	if refusal := s.refusedBeforeApply(info, weighing); refusal != nil {
		return refusal
	}
	rendering, measured := weighing.size()
	allowance := peerApplyAllowance(int64(len(info.Update)), rendering, weighing.roomStateBytes(),
		info.Doc.StructCount(), measured, s.underPressure(info.Room))
	// A heavy update takes its slot before the apply and gives it back once the apply has
	// returned, so the whole decode runs inside one slot; a keystroke takes none. The wait has
	// no deadline, since the peer's read loop carries none and every holder gives its slot back
	// (loadSlots), so it cannot fail.
	if len(info.Update) > unslottedApplyBytes {
		slot := newLoadSlot()
		_ = slot.take(context.Background())
		defer slot.release()
	}
	// The scan allowance is reserved last, so no refusal above leaves one in flight, and settled
	// as soon as the apply returns, with what its scans passed.
	scans := weighing.roomScans()
	scanning := scans.reserve(info.Origin, scanCeiling(len(info.Update)))
	err := weighing.applies().applying(info.Origin, allowance.budget, apply)
	scans.settle(scanning, err)
	if err == nil {
		return nil
	}
	return s.refusedByApply(info, allowance, scanning, err)
}

// peerPressureSweepWait is how long a browser's update waits for the budget's sweep before it is
// refused for the pressure: the sweep closes the rooms nothing is still using, which is what R3
// asks of Dispatch before it refuses anything and what A3 asks of it before any refusal. It is
// two seconds, which is long enough for a sweep that has rooms to close - a close flushes the
// room's updates and compacts them - and short enough that a person whose edit is going to be
// refused is told within the time a keystroke's round trip already allows. It costs nothing on
// an ordinary edit: the wait is reached only while the budget is full, which is one atomic read
// away on every other update.
const peerPressureSweepWait = 2 * time.Second

// underPressure is whether room's next update is weighed under the budget's pressure: the budget
// is full and a sweep could not make room within peerPressureSweepWait. The sweep runs here, on
// the peer's read loop, because R3 puts closing the rooms nothing is still using before refusing
// anything, and an admission already waits for exactly this sweep (roomBudget.refusal); an
// update that would be refused for the budget waits for it too, and applies where it frees a
// room. The line names how long the wait took, so what the sweep costs this path can be read
// off a loaded pod.
func (s *Service) underPressure(room string) bool {
	if !s.budget.full.Load() {
		return false
	}
	started := time.Now()
	freed := s.budget.awaitRoom(context.Background(), started.Add(peerPressureSweepWait))
	used, limit := s.budget.figures()
	slog.Info("dispatch: a browser's update waited for the room budget's sweep before the pressure weighed it",
		"room", room, "freed", freed, "waited_ms", time.Since(started).Milliseconds(), "charged", used, "limit", limit)
	return !freed
}

// refusedBeforeApply is the one refusal the hook makes without applying: an update carrying the
// history its document's rebuild replaced (weighPeerUpdate). A room whose history retired no
// client reads nothing of the update. It carries the test hook's armed refusal too
// (refusesNextPeerUpdateForTest), which stands for a refusal of the rules below it.
//
// An update whose header the scan cannot read refuses nothing here and falls through to the
// apply, which judges the update itself: ygo's own decoder refuses the same bytes, and the hook
// answers that refusal DOC_UPDATE_REFUSED below. The scan has one bound the decoder's own apply
// does not share - it reads no update carrying more structs than one update may decode
// (maxV2Items) - and refusing such an update here would close an honest whole-state handoff of
// a rebuilt document at the decode cap before the apply could weigh it at all.
func (s *Service) refusedBeforeApply(info websocket.PeerUpdateInfo, weighing *roomWeighing) error {
	if s.refusesNextPeerUpdateForTest(info.Room) {
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update the test hook armed this document for",
			"rule", "the test hook")
		return refusal
	}
	retired := weighing.retiredClients()
	if len(retired) == 0 {
		return nil
	}
	from, _, err := crdt.ParseUpdateMeta(info.Update)
	if err != nil {
		slog.Info("dispatch: a browser's update the retired-client scan could not read; the apply will judge it",
			"room", info.Room, "update_bytes", len(info.Update), "error", err.Error())
		return nil
	}
	for client, clock := range from {
		if at, listed := retired[client]; listed && clock < at {
			slog.Info("dispatch: refused a browser's update carrying the history its document's rebuild replaced",
				"room", info.Room, "update_bytes", len(info.Update), "client", uint64(client),
				"client_clock", clock, "retired_at", at)
			return documentRebuilt()
		}
	}
	return nil
}

// refusedByApply is the close a failed apply of a browser's update becomes.
//
// An apply the allowance refused (crdt.ErrApplyBudget) reports what it reached of each of the
// three counts, and the first of them at or past its own field names the rule that set that
// field, which names the close: the decode count is every origin's and is DOC_TOO_LARGE, and the
// structs and the bytes carry whichever rule set them (peerRule). One ygo refused for a root the
// browser may not name (crdt.ErrRootNotAllowed) is DOC_UPDATE_REFUSED: the editor never sends
// one. One whose merge would scan past its allowance (crdt.ErrIntegrateScanBudget) is
// DOC_TOO_TANGLED, or DOC_NEEDS_REBUILD where the room's scan total was used up. One the room's
// pending cap refused (crdt.ErrPendingCap) is returned as it is, the one error the hook answers
// that is no *websocket.PeerClose, so ygo answers that browser SyncStatus(0) and leaves it
// connected, exactly as a room without this hook does: the update parks past the cap only
// because the structs it depends on have not arrived, and the browser that sends it again once
// its gap fills is right to stay. Any other refusal of the bytes themselves
// (crdt.ErrInvalidUpdate) is DOC_UPDATE_REFUSED: ygo will not read them, and the client that
// sent them sends them again on every reconnect.
func (s *Service) refusedByApply(info websocket.PeerUpdateInfo, allowance peerAllowance, scanning *scanAllowance, err error) error {
	var refused *crdt.RefusedUpdateError
	errors.As(err, &refused)
	switch {
	case errors.Is(err, crdt.ErrApplyBudget):
		rule := allowance.rule(refused)
		refusal := rule.refusal()
		logRefusal(info, refusal, "dispatch: refused a browser's update its apply may not take", "rule", rule.why(),
			"decoded", refused.Decoded, "decode_limit", allowance.budget.Decoded,
			"structs", refused.Structs, "structs_limit", allowance.budget.Structs, "struct_rule_limit", allowance.structsLimit,
			"content_bytes", refused.Bytes, "content_limit", allowance.budget.Bytes, "byte_rule_limit", allowance.bytesLimit,
			"split", refused.Split)
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrRootNotAllowed):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update naming a root the editor never writes",
			"root", refused.Root)
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrIntegrateScanBudget):
		// The honest long session's updates scan 12 items at most, so a refusal is worth a line,
		// with the budget's figures and the room's scan total as the refusal leaves them.
		used, limit := s.budget.figures()
		scanned, left := s.roomWeighingOf(info.Doc).roomScans().figures()
		slog.Warn("dispatch: refused a browser's update its room could not merge within its scan allowance",
			"room", info.Room, "update_bytes", len(info.Update), "charged", used, "limit", limit,
			"structs", info.Doc.StructCount(), "allowance", scanning.limit(),
			"scan_total", scanned, "scans_left", left)
		if scanning.roomUsedUp() {
			return documentNeedsRebuild()
		}
		return documentTooTangled()
	case errors.Is(err, crdt.ErrPendingCap):
		// No line: a browser whose gap has not filled meets this on every frame it sends until
		// the gap does fill, and the socket stays open, so one line here would be a flood where
		// every other refusal here closes the socket and so logs at most once.
		return err
	case errors.Is(err, crdt.ErrInvalidUpdate):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update ygo will not read", "error", err.Error())
		return refusedWith(refusal, err)
	}
	return err
}

// rule is the rule a refused apply broke: the first count at or past its own field of the
// allowance, read in the order the fields bound the apply in - the decode as the bytes are read,
// then the structs and the bytes as they integrate. A field of no limit (below zero) bounds
// nothing and names nothing. A refusal with no counters at all, which ygo does not make for this
// error, falls to the decode rule, whose close is the document's own.
func (a peerAllowance) rule(refused *crdt.RefusedUpdateError) peerRule {
	switch {
	case refused == nil:
		return ruleDecodedValues
	case a.budget.Decoded >= 0 && refused.Decoded >= a.budget.Decoded:
		return ruleDecodedValues
	case a.budget.Structs >= 0 && refused.Structs >= a.budget.Structs:
		return a.structs
	case a.budget.Bytes >= 0 && refused.Bytes >= a.budget.Bytes:
		return a.bytes
	}
	return ruleDecodedValues
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
// one its room's used-up scan total refuses, and one that would take its state past
// maxRoomStateBytes - permanent as documentTooLarge is and under the same code, with a reason of
// its own: the editor offers the rebuild that would take the edit.
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

// documentUpdateRefused is the refusal of an update the room will not read: one whose bytes ygo
// refuses, and one naming a root the editor never writes - a subdocument, an unknown root, the
// retired clients' list only a rebuild writes. Permanent as documentTooLarge is and under the
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
// (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:142-149) - with cause behind it,
// which no close frame carries: ygo's own refusal of the update, for this package's tests. ygo
// logs that cause at Debug alone (provider/websocket/peer_update.go:142-143), which the pod's
// level drops, so what an operator reads of a refusal is the hook's own line (logRefusal) and
// never this error.
func refusedWith(refusal *websocket.PeerClose, cause error) error {
	return fmt.Errorf("%w: %w", refusal, cause)
}

// logRefusal is the one line a refusal of a browser's update leaves. ygo logs a refused peer
// update at Debug alone (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:142-143),
// which the pod's level drops, so without this line an operator sees an editor's socket close and
// nothing says why. message names the rule in plain words and figures are what refused it, beside
// the room, the update's bytes and the close that browser is given. Neither the document's
// content nor the update's bytes are ever logged: both are the person's own text.
func logRefusal(info websocket.PeerUpdateInfo, refusal *websocket.PeerClose, message string, figures ...any) {
	slog.Warn(message, append([]any{
		"room", info.Room, "update_bytes", len(info.Update),
		"close_code", refusal.Code, "close_reason", refusal.Reason,
	}, figures...)...)
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
