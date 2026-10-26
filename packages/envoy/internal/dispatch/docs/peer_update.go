package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
// (bytesOfHeapAnElement), with the render about 160 ms (AGENTS.md:531-568, :681-693) - against a
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
// The hook has a third state, which is the room budget's rather than the document's: while the
// open rooms are past their budget and nothing can be closed (roomBudget.full), only an update
// that inserts nothing is admitted, in every room, and anything else is refused with the transient
// DocumentBusyCloseCode (refusedUnderPressure). Bytes cannot be that test. The hook does not count
// elements, and the elements are what the heap follows (bytesOfHeapAnElement): a mebibyte of
// markdown can make 786,432 of them (pmdoc/elements.go:16-29), so an update can leave a document
// as long as it was, or shorter, and still multiply what its room holds. What an update inserts is
// what can add an element, so an update that inserts nothing is the one update that cannot grow the
// rendering. That refusal is not the document's own bound and is not folded into it: 4413 is
// permanent and the editor drops the refused edit, where 4503 is transient and the editor keeps
// it, to send once the pressure passes.

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a room's measurement may creep before only an update
// that inserts nothing at all is admitted, so a document that has crossed can still be trimmed or
// split and nothing can grow it further.
const maxPeerByteCreep = 64 << 10

// maxMessageBytes is the most one websocket message a browser sends may carry, ygo's
// Server.MaxMessageBytes, whose own default is 64 MiB (sjawhar/ygo v1.51.3-sami.2,
// provider/websocket/server.go:1017-1025). It is a floor against a client that is not the editor at
// all, never the bound on a document: the bound is weighPeerUpdate's, which walks a frame without
// decoding it (walkUpdate) and refuses an oversized one before anything decodes it, so a frame at
// this floor that the hook refuses costs the pod the frame itself and nothing proportional to the
// structs it carries.
//
// The number is four times the largest frame the editor sends, which is a browser handing over the
// whole live state of the heaviest document the server's own writes can leave: its markdown at
// pmdoc.MaxDocumentBytes making pmdoc.MaxDocumentElements, its margin at maxMarginBytes and its
// anchor marks at maxAnchorBytes. That frame measured 5,360,879 bytes, well under the 64 MiB ygo
// allowed before, past which this design would have had to come back to the architect - the
// markdown alone is 3,160,622 of it, since a document at the element bound carries 16,384 blocks
// and the block id of each - so the floor is four times the measured frame rounded up to the next
// mebibyte, 6 MiB, which is 24 MiB. TestTheHeaviestWholeStateFrameFitsTheMessageFloor builds that
// document and fails once its frame passes a quarter of this floor, since a frame over the floor is
// closed by the websocket library with a code the editor reconnects on, a loop no person's edit may
// ever reach.
const maxMessageBytes = 24 << 20

// weighPeerUpdate is ygo's Server.OnPeerUpdate: the room hands it every update and whole-state
// message a read-write browser sends, with the apply that would put it in the room, and a read-only
// browser never reaches it (sjawhar/ygo v1.51.3-sami.2, provider/websocket/peer_update.go:68-110).
// An update it does not apply is neither stored nor relayed to the room's other browsers, and the
// *websocket.PeerClose it refuses with closes that one browser with that code and reason.
//
// Every update is walked first (walkUpdate, fromPeer), under the bound too. An update the walk
// refuses - bytes ygo's decoder would refuse, or content the Proof editor never sends: a
// subdocument, a root other than the fragment and the margin - is answered with the walk's error
// and never applied, so ygo answers that browser SyncStatus(0), integrates nothing and leaves it
// connected (provider/websocket/peer.go:247-263). The apply cannot be what answers such a frame:
// ygo integrates each struct as it decodes it inside a transaction it never rolls back
// (crdt/update.go:50-56), so a frame of valid structs ending in one bad byte would leave every
// struct before the byte in the room.
//
// While the room's last measured rendering plus the update's own bytes stay under
// pmdoc.MaxDocumentBytes, the bytes admit the update. At or over it, the update is admitted only
// when what it carries that the room does not already hold - the walk's clocks beyond the room's
// state vector (peerWeighing) - is under maxPeerInsertAtTheBound, or, past the creep cap, only when
// it carries nothing new at all. Then the room's struct total admits it only while what it would
// add keeps the total within maxUpdateItems (structTotal.admits). Each refusal of the three is the
// document's own, DocumentTooLargeCloseCode.
//
// While the room budget is full (roomBudget.full), an update the document's bounds admit is then
// refused with DocumentBusyCloseCode when it inserts anything (refusedUnderPressure), whatever it
// leaves the document's bytes at, since an insert of fewer bytes than it deletes can still add
// elements. An update does not say what it deletes without the document to measure it against, so
// an edit that replaces text with less of it is refused too, as the creep cap refuses it. The
// signal is one atomic load, so the peer's read loop takes neither the budget's lock nor a room's
// state lock for it.
//
// Every update's apply is weighed too, by what the walk estimates ygo's apply of it allocates into
// this room (peerWeighing.applyHeap), which is proportional to the values and structs it carries
// whether or not the room already holds them, and to the room's clients and deleted items. One
// whose estimate alone passes applyHeapBound, all the heap the pod lets its applies hold at once,
// could never apply within it: it is answered with an error before any other rule and never
// applied, as a walk's refusal is. An admitted update whose own share of the estimate passes
// unweighedApplyHeapBytes applies only once it holds its estimate out of the bound (applyHeaps), so
// a reconnect storm's whole-state handoffs decode as many at a time as the bound has room for
// rather than all at once; typing never waits on it.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema, websocket.go:175):
// the document's byte bound admits its updates, and the room's next render settles what it holds.
// An error the apply returns - one that overflows the room's parked-item queue (MaxPendingItems) -
// is returned as it is, so ygo answers that browser SyncStatus(0) and leaves it connected, exactly
// as a room without this hook does.
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	weighed := peerWeighing{held: info.Doc.StateVector()}
	if _, err := walkUpdate(info.Update, fromPeer, &weighed); err != nil {
		return err
	}
	replica := s.liveReplica(info.Doc)
	var bytes int64
	measured := false
	var structs *structTotal
	if replica != nil {
		bytes, _, measured = replica.size()
		structs = replica.structs
	}
	heap := weighed.applyHeap(len(info.Update), structs.roomStructs(info.Doc))
	if heap > applyHeapBound {
		return fmt.Errorf("%w: its apply is estimated to allocate %d bytes, past the %d bytes the pod lets every apply hold at once",
			errApplyTooHeavy, heap, uint64(applyHeapBound))
	}
	atTheBound := measured && bytes+int64(len(info.Update)) >= pmdoc.MaxDocumentBytes
	crept := measured && bytes >= pmdoc.MaxDocumentBytes+maxPeerByteCreep
	if atTheBound && (weighed.inserted >= maxPeerInsertAtTheBound || (crept && weighed.inserted > 0)) {
		return documentTooLarge()
	}
	// A heavy update waits for its share of the bound before it takes the room's admission lock,
	// so the room's other browsers do not wait behind it; what the room admits on is then weighed
	// as it stands once the wait is over.
	if weighed.ownApplyHeap(len(info.Update)) > unweighedApplyHeapBytes {
		defer s.applyHeaps.hold(heap)()
	}
	added := weighed.added()
	// What the room's struct total admits on must still hold when the apply lands: two browsers
	// of one room weighed at once could each pass the cap's check, and together pass the cap.
	structs.lockAdmissions()
	defer structs.unlockAdmissions()
	if !structs.admits(info.Doc, added) {
		return documentTooLarge()
	}
	if s.budget.full.Load() && s.refusedUnderPressure(&weighed) {
		return &websocket.PeerClose{
			Code:   contracts.DocumentBusyCloseCode,
			Reason: contracts.DocumentBusyCloseReason,
		}
	}
	err := structs.applying(info.Origin, apply)
	// Taken after the apply returns, so a recount that read the total's admissions before its
	// encode cannot miss what this apply integrated (structTotal.recount). An apply that failed
	// is taken all the same: it can have integrated structs before it failed.
	structs.took(added)
	return err
}

// documentTooLarge is the document's own refusal of an update, permanent: the editor drops the
// refused edit and rebuilds from a fresh read of the document.
func documentTooLarge() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooLargeCloseReason,
	}
}

// refusedUnderPressure is whether the room budget's pressure, while the budget is full and nothing
// can be closed (roomBudget.full), refuses weighed, an update the document's own bounds admit: one
// that carries anything the room does not already hold. One that only deletes is admitted.
func (s *Service) refusedUnderPressure(weighed *peerWeighing) bool {
	return weighed.inserted > 0
}

// liveReplica is the replica of the room holding live (keepReplica), or nil for a document no
// room's load listed. It is a load off the listing the room's reads already go through (holdLive),
// so the peer's read loop takes neither the replica's lock, which every browser in the room waits
// behind (AGENTS.md:527-530), nor the room's state lock.
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
	// room's store; deletes is the update's delete ranges.
	structs, deletes uint64
	// decoded is every walked struct's share of the apply's estimate, since ygo's apply decodes
	// each before it asks whether the room holds it; integrated is the share of the structs past
	// the room's state alone, which only they cost (structApplyHeap).
	decoded, integrated uint64
}

func (w *peerWeighing) walkedStruct(walked updateStruct) {
	cost := structApplyHeap(walked)
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
}

func (w *peerWeighing) walkedDelete(_, _, _ uint64) { w.deletes++ }

// splitsAStructCanCause is the most structs integrating one struct can add to a store beyond
// itself, and the most applying one delete range can: each splits at most the item it starts in
// and the item it ends in (an item's integrate splits the items holding its origin and its right
// origin, sjawhar/ygo v1.51.3-sami.2, crdt/update.go:659-661 and crdt/item.go:83-106, through
// crdt/store.go:91-140; a delete range splits the items at its two ends,
// crdt/delete_set.go:170-171).
const splitsAStructCanCause = 2

// added is what the walked update can add to the room's struct total: each new struct with the
// splits its integration can cause, and the splits each delete range can cause.
func (w *peerWeighing) added() uint64 {
	return w.structs*(1+splitsAStructCanCause) + w.deletes*splitsAStructCanCause
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
// (crdt.EncodeStateAsUpdateV1, sjawhar/ygo v1.51.3-sami.2, crdt/update.go:69-91). It is taken at
// the room's load, beside the load's rendering (keepReplica), with each count the service's
// measurer makes of the room (elementMeasurer.count), and by the hook to confirm a total near or
// past the cap. What the hook admits after it is each update's addition (peerWeighing.added): its
// new structs, each with the two splits its integration can cause, and two for each delete range.
// That is an upper bound, so the running total is never below what the room holds, short of a
// write the hook does not see.
//
// Those writes are every transaction on the room that is not a hook's apply: the server's own
// writes (Server.Apply - an API write's publish, settlement and its repairs, the mark sweep, the
// block-id stamp - and an update injected through ygo, allowInject), which ask the decode cap in
// their own way (refuseUndecodable, leavesPastDecodeCap) and are bounded there, not here. The one
// place that sees each of them, whoever wrote it, is the document's update observer: ygo fires it
// for every transaction it commits, even one that writes nothing, with the transaction's origin
// (crdt/doc.go:462-464, :541-556), on the committing goroutine. So the total registers an
// observer of its own (observed), and the hook names the origin of the apply it is running while
// it runs it (applying): ygo's peer, the origin ygo applies a browser's update with
// (provider/websocket/peer_update.go:84-97). A transaction of any other origin marks the total
// stale. A list of the server's writes would miss the next one added; the observer cannot. A stale
// total within structTotalStaleMargin of the cap is confirmed by an exact count before the hook
// admits on it; further from the cap the server's writes, each bounded by its own check, are
// trusted not to have used the margin up.
//
// The total's three counters order a count against the hook's admissions without a lock across
// the walk: a count reads admitted and writes before it encodes, and the hook adds an apply's
// addition only once the apply has returned, its observer included. An apply that landed before the
// encode and was added after the read is counted twice, which overstates the total; none can be
// missed. The same holds for writes, so a write that landed after a count's encode leaves the
// total stale.
type structTotal struct {
	live weak.Pointer[crdt.Doc]
	// count is countRoomStructs, but for a test's, which counts the counts.
	count func(doc *crdt.Doc) uint64

	// admissions is held by the hook from its check of the total through the apply and the
	// addition that follow, so the total it admits on is the total the apply lands on.
	admissions sync.Mutex

	// originMu guards origin and inApply, the apply the hook is running, which the observer reads
	// on whatever goroutine commits a transaction.
	originMu sync.Mutex
	origin   any
	inApply  bool

	// admitted is the sum of every addition the hook has admitted, and writes the number of
	// transactions the observer has seen that were not a hook's apply; both only grow.
	admitted, writes atomic.Uint64

	// mu guards the last count - counted, with admitted and writes as they were read before it -
	// and the hook's last confirmation: whether one was taken and admitted when it was.
	mu                sync.Mutex
	counted           uint64
	countedAdmitted   uint64
	countedWrites     uint64
	confirmed         bool
	confirmedAdmitted uint64
}

// newStructTotal is live's struct total, counted once now, at the room's load.
func newStructTotal(live *crdt.Doc) *structTotal {
	total := &structTotal{live: weak.Make(live), count: countRoomStructs}
	live.OnUpdate(total.observed)
	total.recount(live)
	return total
}

// observed is the total's update observer: a transaction that is not the hook's apply marks the
// total stale.
func (t *structTotal) observed(_ []byte, origin any) {
	t.originMu.Lock()
	hook := t.inApply && origin == t.origin
	t.originMu.Unlock()
	if !hook {
		t.writes.Add(1)
	}
}

// applying runs apply, which applies an update with origin, as the hook's own apply.
func (t *structTotal) applying(origin any, apply func() error) error {
	if t == nil {
		return apply()
	}
	t.originMu.Lock()
	t.origin, t.inApply = origin, true
	t.originMu.Unlock()
	defer func() {
		t.originMu.Lock()
		t.origin, t.inApply = nil, false
		t.originMu.Unlock()
	}()
	return apply()
}

func (t *structTotal) lockAdmissions() {
	if t != nil {
		t.admissions.Lock()
	}
}

func (t *structTotal) unlockAdmissions() {
	if t != nil {
		t.admissions.Unlock()
	}
}

// took adds added, an admitted update's addition, to the total.
func (t *structTotal) took(added uint64) {
	if t != nil {
		t.admitted.Add(added)
	}
}

// running is the total as it stands - the last count and what the hook admitted since - and
// whether a write the hook did not see has landed since the count.
func (t *structTotal) running() (total uint64, stale bool) {
	t.mu.Lock()
	counted, admitted, writes := t.counted, t.countedAdmitted, t.countedWrites
	t.mu.Unlock()
	return counted + t.admitted.Load() - admitted, t.writes.Load() != writes
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

// admits reports whether doc, the room's live document, can take added more structs within the
// decode cap. The running total answers when it is within the cap and either fresh or more than
// structTotalStaleMargin short of it. A total past the cap is confirmed by an exact count before
// it refuses, unless a confirmation was taken while the total was fresh and less than
// structTotalRecheck has been admitted since, which answers on the running total instead; a stale
// total near the cap is always confirmed. A confirmation re-bases the total and nothing else.
//
// The count runs on the browser's read loop, so that browser's next frame waits for it: an encode
// of the room's state and a walk of it, whose walk allocates nothing per struct and whose encode
// allocates its growing buffer. Measured on 2026-10-26 at a mebibyte of a room's shape, a
// 3,035,240-byte state: 71 to 80 ms and 9.5 to 10.6 MB in about 21,660 allocations a count
// (BenchmarkStructTotalConfirmation). It does not take the measurer's turn, which would put a
// keystroke behind another room's element count or a server write's decode.
//
// A nil total, a room no load listed, is counted afresh for each update.
func (t *structTotal) admits(doc *crdt.Doc, added uint64) bool {
	if t == nil {
		return countRoomStructs(doc)+added <= maxUpdateItems
	}
	total, stale := t.running()
	switch {
	case total+added > maxUpdateItems:
		if t.confirmationHolds(stale) {
			return false
		}
	case stale && total+added > maxUpdateItems-structTotalStaleMargin:
	default:
		return true
	}
	t.confirm(doc)
	total, _ = t.running()
	return total+added <= maxUpdateItems
}

// confirmationHolds reports whether the hook's last confirmation still answers for the total: it
// was taken, nothing the hook did not see has landed since (stale), and the hook has admitted less
// than structTotalRecheck since it.
func (t *structTotal) confirmationHolds(stale bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !stale && t.confirmed && t.admitted.Load()-t.confirmedAdmitted < structTotalRecheck
}

// confirm re-bases the total on an exact count of doc and records it as the hook's confirmation.
func (t *structTotal) confirm(doc *crdt.Doc) {
	admitted := t.recount(doc)
	t.mu.Lock()
	t.confirmed, t.confirmedAdmitted = true, admitted
	t.mu.Unlock()
}

// recountLive re-bases the total on an exact count of the room's live document, if it is still
// resident: the measurer's count, in its turn.
func (t *structTotal) recountLive() {
	if t == nil {
		return
	}
	if live := t.live.Value(); live != nil {
		t.recount(live)
	}
}

// recount re-bases the total on an exact count of doc, and returns the admissions it was counted
// against.
func (t *structTotal) recount(doc *crdt.Doc) uint64 {
	admitted, writes := t.admitted.Load(), t.writes.Load()
	counted := t.count(doc)
	t.mu.Lock()
	t.counted, t.countedAdmitted, t.countedWrites = counted, admitted, writes
	t.mu.Unlock()
	return admitted
}

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
	if err != nil && tally.structs <= maxUpdateItems {
		slog.Error("dispatch: count the structs of a room's own state", "error", err)
		tally.structs = maxUpdateItems + 1
	}
	return tally.structs + splitsAStructCanCause*uint64(pending.Items+pending.DeleteRanges)
}

// errApplyTooHeavy is the hook's answer to an update whose apply alone is estimated to allocate
// more than applyHeapBound, all the heap the pod lets every browser's apply hold at once: no turn
// could ever apply it within the bound, so it is never applied. ygo answers the browser
// SyncStatus(0) and leaves it connected, as it answers a walk's refusal.
var errApplyTooHeavy = errors.New("the update is heavier to apply than the pod can hold")

// goMemoryLimitBytes is the Go runtime's soft memory limit on the production pod: 90 per cent of
// the container's 4 GiB, which the server sets at boot (cmd/dispatch/main.go:116-128).
const goMemoryLimitBytes = 4 << 30 * 9 / 10

// applyHeapBound is the most heap, by their estimates (peerWeighing.applyHeap), that the pod's
// weighed applies of browsers' updates hold at once (applyHeaps): half of what the Go memory limit
// leaves above the room budget and the cold reads' cache, (3,865,470,566 - roomBudgetBytes
// 2,147,483,648 - documentReadBudget 268,435,456) / 2 = 1,449,551,462 / 2 = 724,775,731 bytes,
// about 691 MiB. The other half is the request path's and the collector's slack, and it is also
// where the applies under unweighedApplyHeapBytes run.
//
// It is not in the spec: it is the plan's departure 8, large applies taking turns, weighed by each
// apply's estimate rather than counted in slots. Every browser's first sync hands over its whole
// state, and ygo's apply decodes every struct of it, and every value of each, before decodeAndPark
// drops what the room already holds (sjawhar/ygo v1.51.3-sami.2, crdt/update.go:571-593, the
// values at :1064-1078), so fifty browsers reconnecting at once to rooms of heavy documents would
// otherwise decode fifty handoffs at the same moment. A count of slots sized for the heaviest
// handoff could not bound an update the editor never sends, whose apply allocates in proportion
// to the values it carries whatever the room holds: 25,165,779 bytes of null values in one struct
// for a client whose every clock the room held allocated 805,307,736 bytes in one apply, though it
// inserted nothing and added no struct. Measured on 2026-10-26
// (TestEveryApplysTransientHeapIsWithinItsEstimate), the 5,360,879-byte handoff of the heaviest
// document the message floor is measured on is estimated at 71,356,658 bytes into the room
// holding it, against the 33,408,936 it allocates there, so ten such handoffs apply at once, and
// at 212,118,103 into a fresh room, against 98,003,608; that frame of nulls is estimated at
// 1,660,944,870 bytes, past the whole bound, and refused (errApplyTooHeavy).
const applyHeapBound = (goMemoryLimitBytes - roomBudgetBytes - documentReadBudget) / 2

// unweighedApplyHeapBytes is the update's own share of its estimate (peerWeighing.ownApplyHeap)
// at or under which an admitted update applies without waiting on applyHeapBound: 2 MiB, over
// three times the own share of the editor's 64 KiB paste (the creep cap's size, far past a
// keystroke) into the heaviest document and over four hundred times a keystroke's, so typing never
// waits on a reconnect storm's handoffs (TestEveryApplysTransientHeapIsWithinItsEstimate). The
// room's share is left out of the comparison because it is the room's, not the update's: every
// transaction on a room pays for the room's clients and deleted items, the server's own writes
// included, and ygo runs one transaction at a time in a room (its document lock), so that share is
// paid at most once a room at any moment. An unweighed apply runs beside the bound, one per
// browser's read loop at most, in the half the bound leaves to the request path.
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

// hold waits until it holds heap of the bound and returns what gives it back. heap is never over
// applyHeapBound, which the hook refuses before it gets here, and with a context that is never
// done Acquire returns only once it holds heap, so it cannot fail.
func (h *applyHeaps) hold(heap uint64) (release func()) {
	h.once.Do(func() { h.bound = semaphore.NewWeighted(applyHeapBound) })
	_ = h.bound.Acquire(context.Background(), int64(heap))
	return func() { h.bound.Release(int64(heap)) }
}

// applyHeap is the estimate of what ygo's apply of the walked update allocates, frame its bytes,
// into a room of roomStructs structs (structTotal.roomStructs): an upper bound, every factor below
// set from TestEveryApplysTransientHeapIsWithinItsEstimate's measurement of ygo's apply at scale
// (runtime.MemStats.TotalAlloc across one apply, which bounds what the apply holds at its peak
// from above), into a document with an update observer, as every room has. It is the update's own
// share (ownApplyHeap) and the room's:
//
//   - the room's state vector, which ygo's apply copies into maps before it reads a struct
//     (crdt/update.go:497) and its transaction twice more (crdt/doc.go:595, :662), each a slot
//     for every client the room holds;
//   - the update the transaction made, which ygo encodes for the room's update observers at every
//     commit, one that changed nothing included (buildPhase2, crdt/doc.go:459-465, called at
//     :625), by walking the room's whole store and building the delete set of all of it, a range
//     for every deleted item before it compacts them (encodeV1Locked and buildDeleteSet,
//     crdt/update.go:177-217 and :448-461): a price for every deleted item of the room, of which
//     it holds no more than its structs and the structs the update can add (peerWeighing.added).
//     11,319,808 bytes measured for a one-character keystroke into a string of 100,000 deleted
//     items, against 2,240 into one of none.
//
// What it leaves out is what depends on the shape of the room's lists, which no walk of the update
// can see: an item naming an origin inside a run and no right origin makes ygo's integrate scan the
// rest of its parent's list into two maps (crdt/item.go:120-172), so a frame of such items
// allocates in proportion to their number times the list's length; and an item or a delete range
// splitting a room's Any or JSON item copies the values right of the split
// (ContentAny.Splice, crdt/content.go:170-174).
func (w *peerWeighing) applyHeap(frame int, roomStructs uint64) uint64 {
	return w.ownApplyHeap(frame) + applyHeapPerRoomClient*uint64(len(w.held)) +
		applyHeapPerRoomStruct*(roomStructs+w.added())
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
// into a document with an update observer, per struct of a frame of 200,000 unless said, decode
// and integration together unless said); that test holds the estimate over every measurement.
// What each covers is what ygo's decode allocates (decodeItem, crdt/update.go:794-966;
// decodeContent, :968-1110; decodeTypeContent, :1112-1177; decodeDeleteSet, :1179-1217), and
// what integrating and encoding again for the room's observers allocates (encodeItem, :231-371).
const (
	// applyHeapPerApply is the transaction, the decode's own state and the observers' encode of a
	// small update: 2,336 bytes measured for the whole apply of a keystroke into the heaviest
	// document, 3,224 for a deletion of one character from it.
	applyHeapPerApply = 4 << 10
	// applyHeapPerRoomClient is each client in the room's state vector, which ygo's apply copies
	// into maps sized for it. A map takes slots for its size past 8/7 rounded up to a power of
	// two, so a room of 57,345 clients, the least that rounds to 131,072 slots, pays the most a
	// client: 19,220,952 bytes measured for a keystroke into such a room, each client holding one
	// GC struct, 335 a client with the observers' encode of that struct as a deleted item
	// (applyHeapPerRoomStruct), against 263 a client in a room of 200,000.
	applyHeapPerRoomClient = 160
	// applyHeapPerRoomStruct is each deleted item of the room, bounded by each struct the room
	// holds, at every apply: 11,319,808 bytes measured for a keystroke into a string of 100,000
	// deleted items among 100,000 live ones, 113 a deleted item.
	applyHeapPerRoomStruct = 256
	// applyHeapPerFrameByte is each byte of the frame: a string ygo copies out of it is one
	// allocation of its bytes rounded up to a size class, at most twice them for a short one (a
	// string of 9 bytes takes 16), and JSON text is copied twice more (applyHeapPerJSONByte).
	applyHeapPerFrameByte = 2
	// applyHeapPerIntegratedFrameByte is each byte of the frame of an update that integrates a
	// struct, for the observers' encode of what it integrated: a buffer grown by appending, whose
	// allocations sum to about five times what it holds once it grows by a quarter at a time, and
	// the copy handed to the observers. 28,356,168 bytes measured for 2,200,018 bytes of Any
	// strings of 9 bytes in one struct, 12.9 a byte, and 12,602,120 for one string of 4 MiB, 3.
	applyHeapPerIntegratedFrameByte = 7
	// applyHeapPerRun is a GC struct or a skip: an Item and its content. 120 bytes measured for
	// each of maxUpdateItems GC structs into a room holding them, which decodes them and
	// integrates nothing, and for each skip.
	applyHeapPerRun = 192
	// applyHeapPerItem is an item of any content: the Item, its origins and its content's own
	// struct. 184 bytes measured for a margin entry of one null value into a room holding it.
	applyHeapPerItem = 192
	// applyHeapPerKey is an item that names a map key: the key.
	applyHeapPerKey = 64
	// applyHeapPerType is an item of a nested type: the type, its maps and, for an XML element,
	// its name.
	applyHeapPerType = 192
	// applyHeapPerNewStruct is a struct the update integrates: its place in the store and its
	// parent, the splits of the items its origins fall inside (crdt/update.go:659-661,
	// crdt/item.go:83-106), and the observers' encode of it. 218 bytes measured for a GC struct,
	// 357 for a one-character string naming both origins, 661 for one splitting the room's string
	// at both, 538 for a margin entry of one null value.
	applyHeapPerNewStruct = 384
	// applyHeapPerNewType is an integrated item of a nested type beyond applyHeapPerNewStruct: the
	// observers' encode of its type and name. 769 bytes measured for an XML element, 614 for an
	// XML text.
	applyHeapPerNewType = 160
	// applyHeapPerDeleteRange is a range of the delete set: its decoded range, the splits of the
	// items it starts and ends inside, and its place in the transaction's delete set. 786 bytes
	// measured for each of 100,000 ranges splitting the room's string, the room's deleted items
	// included, and 138 for a range of a client the room has not seen.
	applyHeapPerDeleteRange = 768
	// applyHeapPerAnyValue is each lib0 Any value, at every depth (anyValuesApplyHeap): its slot in
	// the item's values and their normalised copy, a boxed scalar or string header, and the copy a
	// partly held item's split makes of its values. 38,266,456 bytes measured for a million null
	// values in one struct, 38 a value.
	applyHeapPerAnyValue = 64
	// applyHeapPerNewAnyValue is each Any value of an integrated item, as the observers' encode
	// writes it.
	applyHeapPerNewAnyValue = 8
	// applyHeapPerAnyArray is each Any array beyond its values: its boxed slice header.
	applyHeapPerAnyArray = 32
	// applyHeapPerAnyMap is each Any map: its header and its first group of slots, which a map of
	// one entry allocates whole. 393 bytes measured for a map of one entry with its null value,
	// the densest Any there is.
	applyHeapPerAnyMap = 384
	// applyHeapPerAnyMapEntry is each entry of an Any map past the map's first group of eight: its
	// slots, at a load as low as a map just past a growth leaves them. 123 bytes an entry
	// measured for maps of 100,000 entries, with each entry's key and value.
	applyHeapPerAnyMapEntry = 64
	// applyHeapPerJSONText is each JSON text ygo decodes with encoding/json (jsonTextApplyHeap):
	// its slot in the item's values and what encoding/json allocates for any text. 79 bytes
	// measured for a null text.
	applyHeapPerJSONText = 64
	// applyHeapPerNewJSONText is each JSON value of an integrated item, which the observers'
	// encode writes as JSON text again with an encoder of its own (fmtValToJSON,
	// crdt/update.go:1243-1264). 300 bytes measured for a number, 711 for a map read from legacy
	// lib0 Any.
	applyHeapPerNewJSONText = 384
	// applyHeapPerJSONContainer is each JSON object or array: an object's map with its first
	// group. 760 bytes measured for an object of one entry, 842 for an object of one empty object.
	applyHeapPerJSONContainer = 384
	// applyHeapPerJSONMember is each separator of a JSON text, a ',' or a ':', which bounds its
	// members: an array's element in a slice grown by appending, an object's entry in a map grown
	// by inserting. 134 bytes measured for each element of arrays of 100,000 numbers.
	applyHeapPerJSONMember = 128
	// applyHeapPerNewJSONMember is each separator of an integrated JSON text, a member the
	// observers' encode writes again, keys sorted and each written through encoding/json. 272
	// bytes measured for each entry of objects of 100,000 entries.
	applyHeapPerNewJSONMember = 128
	// applyHeapPerJSONByte is each byte of JSON text beyond applyHeapPerFrameByte: ygo copies the
	// text into a string and again into the bytes encoding/json reads (fmtValFromJSON,
	// crdt/update.go:1296-1305), and the strings it decodes copy it once more.
	applyHeapPerJSONByte = 2
	// applyHeapPerNewJSONByte is each byte of an integrated JSON text beyond
	// applyHeapPerIntegratedFrameByte, which the observers' encode escapes and copies again. 12.2
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
// all again as lib0 Any (decodeContent's wireJSON case, crdt/update.go:977-1008), so such an item
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
