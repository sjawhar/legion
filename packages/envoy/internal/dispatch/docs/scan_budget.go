package docs

import (
	"errors"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// Integrating an item scans the items between its left and right origins (Item.integrate,
// sjawhar/ygo v1.51.3-sami.7, crdt/item.go:117-192). One the editor sends scans only what other
// people inserted concurrently at the same place, but an item naming a left origin early in a list
// and no right origin scans to the list's end, so an update of n such items scans n times the list:
// ygo's release notes record 2,000 of them against a list of 20,000 taking 20.8 s. Every room
// therefore gives each remote apply an allowance of scans, asked of the room as the apply begins,
// under the document's lock and with the apply's origin (roomScans.budget,
// Doc.SetIntegrateScanBudget, crdt/doc.go:175-212); an apply whose scans, the drain of the parked
// queue it ends with included, would pass it is refused and taken back out whole, and a drain the
// allowance cannot hold is taken back with the parked queue kept while the update stands
// (crdt/apply_rollback.go:9-34, :221-309).
// A browser's update scans at most the per-frame ceiling or what is left of the room's scan total,
// whichever is less; the server's own remote applies scan at most the ceiling.

// perFrameScanCeiling is the most items the scans of one remote apply may pass in a room, for a
// browser's update the peer update hook weighs against applyHeapBound and for every remote apply
// of the server's own. Measured on this pod (BenchmarkAnApplyJustUnderTheScanCeiling, GOMAXPROCS=1,
// 2026-10-28, three runs of three to five applies each): a frame scanning 399,999 items, each a
// distinct item of a run of 400,000 keystrokes, held the room's lock 213 to 320 ms, about 496 ns
// an item past the 16 ms an apply that scans nothing holds it; a frame scanning a run of 2,000
// items 200 times, 399,999 items again, 55 to 58 ms, 145 ns an item, its marks one for each
// distinct item. Both under the 500 ms past which this would have gone back to the architect. A
// browser's update the hook does not weigh gets less, so that its scan marks stay within what it
// may hold unweighed (scanCeiling).
const perFrameScanCeiling = 400_000

// bytesOfAScanMark is the heap a remote apply's scans hold for each distinct item they pass: the
// mark ygo keeps for it in one map for the whole transaction (Transaction.scanMarks,
// crdt/item.go:126-158), with the map's growth: 40 to 48 bytes measured on this pod for a frame
// scanning 399,999 distinct items (BenchmarkAnApplyJustUnderTheScanCeiling, 2026-10-28), and the
// ruling's 76, the most measured on the development machine, is kept as the price.
const bytesOfAScanMark = 76

// usedUpScanBudget is the allowance of a browser's apply in a room whose scan total is used up -
// what is left of it is this or less: 64 items, past the 9 the most of any update of the honest
// long session scanned (TestTheRoomScanTotalHoldsFifteenHonestLongSessions), so typing in place
// goes on while every larger apply is refused as needing a rebuild (documentNeedsRebuild).
const usedUpScanBudget = 64

// roomScanTotal is the most items the conflict scans of a room's history may come to, counted from
// a re-scan of its stored state at the room's load and from what each transaction the room commits
// scans after it: 1,000,000, the architect's ruling. It is at least twice the re-scan of the
// heaviest document the server's own writes store, the document an agent rewrote whole until the
// decode cap refused a write, 475,136 items under sjawhar/ygo v1.51.3-sami.7, twice 950,272,
// where under v1.54.1-sami.3 that state re-scans no item at all, measured 2026-10-31
// (TestNoStoredHeavyDocumentLoadsWithHalfItsScanTotalUsed); and at least fifteen times what an
// honest long session's updates scan, 42,401 items on this pod, fifteen times 636,015
// (TestTheRoomScanTotalHoldsFifteenHonestLongSessions). A copy of a room at the total costs at most
// a copy at the decode cap and about a second of scans, measured on 2026-10-28
// (TestACopyOfARoomAtTheScanTotalCostsACopyOfItsStateAndAboutASecondOfScans) on a run of a million
// keystrokes whose state re-scans 1,000,000 items: its decode took 2.22 s and allocated 457,822,248
// bytes, against 1.46 s and 383,011,168 for the same run unscanned, 0.76 s and 74,811,080 bytes
// more (an earlier run of the same decode took 2.47 s, against 1.25 to 1.36 s unscanned). Copies
// take turns under the copy-heap bound (copyHeapBound). A rebuild is the way out of a room that
// reaches it (RebuildDocument).
const roomScanTotal = 1_000_000

// honestHandoffScans is what one honest offline handoff scans: a browser that typed into a hundred
// paragraphs while another person typed at the same places hands over an update scanning 35,800
// items, every place's scans summed in one update (7,160 at twenty paragraphs). A history within
// it of roomScanTotal is near the scan total, as one within smallestReplacementAdds of
// maxUpdateItems is near the decode cap.
const honestHandoffScans = 35_800

// scanCeiling is the most items the scans of a browser's update may pass whose own share of its
// apply-heap estimate is own (peerWeighing.ownApplyHeap): perFrameScanCeiling for an update the
// hook weighs (own past unweighedApplyHeapBytes), whose estimate prices the marks its scans can
// make (peerWeighing.applyHeap) and which applies only once it holds that estimate out of
// applyHeapBound, or the whole bound for an estimate past it, and for any other what its marks can
// hold in what is left of unweighedApplyHeapBytes past its own share, (2 MiB - own) / 76: 27,594
// for an update of no share, so typing never waits on the heap bound for its scans either. It is
// at least 1, since an allowance of 0 would leave the apply unbounded (crdt/doc.go:362-370).
func scanCeiling(own uint64) int {
	if own > unweighedApplyHeapBytes {
		return perFrameScanCeiling
	}
	return max(int((unweighedApplyHeapBytes-own)/bytesOfAScanMark), 1)
}

// roomScans is a room's scan total and the allowances of the browsers' applies it has handed out.
// used is what the room's history has scanned: the re-scan of the stored state its load decoded
// (rescanned), every transaction the room has committed since (observed) - a browser's apply, the
// server's publish, an injected update, a repair, a local write - and every browser apply the room
// refused, for what its scans passed before the refusal (settle). A copy of the room re-scans
// about what the room's own applies scanned - the long session's 42,401 admitted scans re-scan as
// 42,587, measured 2026-10-31 under sjawhar/ygo v1.54.1-sami.3
// (TestTheRoomScanTotalHoldsFifteenHonestLongSessions) - so the total a reload counts from
// the stored state is the total the room had, give or take a few hundred items, and the scans of
// applies the room refused, which the stored state does not hold. Only a rebuild, which stores a
// fresh history, starts it again.
//
// A browser's apply is reserved its allowance before it applies and settled after (reserve,
// settle), as the struct total reserves an update's addition: two browsers applying at once each
// see the other's allowance taken out of what is left, so together they cannot pass the total by
// more than a used-up room's allowance each. ygo asks the budget once per remote apply, under the
// document's lock (crdt/apply_rollback.go:134-151), so the budget reads the allowance reserved for
// the apply's origin, ygo's peer for a browser's update (provider/websocket/peer_update.go:84-94),
// which applies one update at a time; an apply of any other origin is the server's own, and gets
// perFrameScanCeiling whatever is left, so a server write never fails its room for the total.
//
// What an apply that commits scanned is read in the room's OnAfterTransaction observer, which ygo
// fires after the transaction has released the document's lock and before ApplyUpdate returns, on
// the committing goroutine (crdt/doc.go:729-826, :705-707), from Transaction.IntegrateScanned,
// which counts the drain's scans too, a drain taken back included (crdt/transaction.go:95-108). A
// refused apply commits nothing an observer sees, and reports its scans in the
// *crdt.RefusedUpdateError it returns (crdt/update.go:47-74).
type roomScans struct {
	mu sync.Mutex
	// used is the room's scan total so far, and reserved the allowances of the browsers' applies
	// in flight.
	used, reserved int64
	// applies is the allowance of each browser's apply in flight, by its origin.
	applies map[any]*scanAllowance
}

// scanAllowance is one browser's apply's allowance: the items its scans may pass, whether the room
// was used up when it was reserved, and, once its transaction has committed, what its scans
// passed.
type scanAllowance struct {
	origin    any
	scans     int
	usedUp    bool
	scanned   int
	committed bool
}

// newRoomScans is the scan total of live, a room's document, with nothing used yet, set as live's
// scan budget and observer: from here live applies every remote update whole or not at all, and
// refuses one carrying a subdocument or a move (crdt/doc.go:197-201), as every room the peer update
// hook guards does. The room's load records what the stored state's re-scan came to (rescanned)
// before ygo hands the room to any peer. What decodes a document the server already holds stays
// unbounded: a write's fork, the fold, growth's and a cold load's copies, the measurer's copies
// (structsAfter), validateUpdate, snapshotDocument and the room's replica integrate state no
// browser chose, and a bounded document's apply keeps a journal to take itself back out
// (beginRemoteApply, crdt/apply_rollback.go:129-151) where theirs need not.
func newRoomScans(live *crdt.Doc) *roomScans {
	scans := &roomScans{}
	live.SetIntegrateScanBudget(scans.budget)
	live.OnAfterTransaction(scans.observed)
	return scans
}

// rescanned records scans, what a re-scan of the stored state the room loaded came to (rescan), as
// what the room's history has scanned before its first transaction.
func (r *roomScans) rescanned(scans uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.used += int64(scans)
}

// budget is the room's scan budget, which ygo calls under the document's lock as each remote apply
// begins: the allowance reserved for a browser's apply of origin, and perFrameScanCeiling for any
// other. It takes nothing but r.mu, which nothing holds across a call into the document.
func (r *roomScans) budget(origin any) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if allowance, reserved := r.applies[origin]; reserved {
		return allowance.scans
	}
	return perFrameScanCeiling
}

// observed is the room's OnAfterTransaction observer: what a committed transaction's scans passed
// is a browser's apply's, kept for its settle, or the server's, added to the total at once. The
// format cleanup ygo commits after an apply into a text that carries formatting is a transaction
// of its own, local and of no origin (crdt/ytext_format_cleanup.go:50-74): it reaches here as the
// server's, scans nothing because it integrates nothing, and so leaves every figure as it was.
func (r *roomScans) observed(txn *crdt.Transaction) {
	scanned := txn.IntegrateScanned()
	r.mu.Lock()
	defer r.mu.Unlock()
	if allowance, reserved := r.applies[txn.Origin]; reserved {
		allowance.scanned, allowance.committed = scanned, true
		return
	}
	r.used += int64(scanned)
}

// reserve reserves the allowance of a browser's apply with origin whose ceiling is ceiling
// (scanCeiling): the ceiling or what is left of the total, whichever is less, or usedUpScanBudget
// once what is left is no more than that. Nil, a room no load listed, reserves nothing.
func (r *roomScans) reserve(origin any, ceiling int) *scanAllowance {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	allowance := &scanAllowance{origin: origin}
	switch left := roomScanTotal - r.used - r.reserved; {
	case left <= usedUpScanBudget:
		allowance.scans, allowance.usedUp = usedUpScanBudget, true
	default:
		allowance.scans = int(min(int64(ceiling), left))
	}
	if r.applies == nil {
		r.applies = make(map[any]*scanAllowance)
	}
	r.applies[origin] = allowance
	r.reserved += int64(allowance.scans)
	return allowance
}

// settle ends allowance, whose apply returned err: it gives the reservation back and adds what the
// apply's scans passed to the total - its committed transaction's count, or for a refused apply
// the count its refusal reports. Nil settles nothing.
func (r *roomScans) settle(allowance *scanAllowance, err error) {
	if r == nil || allowance == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.applies, allowance.origin)
	r.reserved -= int64(allowance.scans)
	var refused *crdt.RefusedUpdateError
	switch {
	case errors.As(err, &refused):
		r.used += int64(refused.Scanned)
	case allowance.committed:
		r.used += int64(allowance.scanned)
	}
}

// limit is the items allowance's apply may scan, 0 for no allowance: an apply in a room no load
// listed.
func (a *scanAllowance) limit() int {
	if a == nil {
		return 0
	}
	return a.scans
}

// roomUsedUp is whether allowance was reserved in a room whose scan total was used up.
func (a *scanAllowance) roomUsedUp() bool {
	return a != nil && a.usedUp
}

// figures is the room's scan total so far and what is left of roomScanTotal past it and the
// allowances in flight. Zero and the whole total for a room no load listed.
func (r *roomScans) figures() (used, left int64) {
	if r == nil {
		return 0, roomScanTotal
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.used, roomScanTotal - r.used - r.reserved
}

// rescan decodes update as a room decodes its stored state - into a document with the server's
// pending queue (newDocumentCopy) - and returns how many items that decode's conflict scans
// passed, ygo's count of them over every transaction the apply commits (decodeCountingScans),
// with ygo's refusal of update if it refused it. The room's own load decodes the same bytes into
// a document of its own the same way (provider/websocket/server.go:1765-1773), so the count is
// what that load scanned.
func rescan(update []byte) (uint64, error) {
	return decodeCountingScans(newDocumentCopy(), update)
}

// decodedState is a stored state decoded as a room decodes it (decodeStoredState): doc, the copy
// it decoded into, and scans, what that decode's conflict scans passed, the room's scan total at
// its load (roomScans.rescanned). doc is nil for an empty state, which decodes into nothing.
type decodedState struct {
	doc   *crdt.Doc
	scans uint64
}

// decodeStoredState is rescan keeping the copy it decoded: the one decode of the stored state a
// room's load makes of its own, shared by the socket admission's schema check (loadTree), the
// room's scan count (LoadDoc) and the room's replica, which takes the copy as its own
// (keepReplica). The scans are counted over the transactions that decode the copy, so they are the
// copy's. A state that does not decode leaves no copy: what a refused decode left in its document
// is no state the store holds.
func decodeStoredState(update []byte) (decodedState, error) {
	if len(update) == 0 {
		return decodedState{}, nil
	}
	doc := newDocumentCopy()
	scans, err := decodeCountingScans(doc, update)
	if err != nil {
		return decodedState{scans: scans}, err
	}
	return decodedState{doc: doc, scans: scans}, nil
}

// decodeCountingScans applies update to doc, a fresh unbounded copy, and returns what the conflict
// scans of that apply passed: what every transaction the apply commits scanned, summed, and not
// what the last of them scanned. An apply that changes a text whose type carries formatting
// commits a second transaction after the remote one, ygo's format cleanup, which integrates
// nothing and so scans nothing (Doc.afterRemoteObservers and Doc.formattingCleanupLocked,
// sjawhar/ygo v1.54.1-sami.1, crdt/ytext_format_cleanup.go:50-74 and :83-119, run for a type a
// ContentFormat item's integrate marked, crdt/item.go:340-344). Reading the last transaction's
// count left the decode of a document whose text carries a mark at zero - the tangle the browser
// row of packages/dispatch/e2e/document-refusals.e2e.ts builds re-scans 40,001 items and was read
// as none - and with it the scan total a room's load starts from (roomScans.rescanned). An
// observer of the transactions is all this adds to the decode: ygo encodes no update for an
// observer of a transaction (crdt/update_log.go:153-156).
func decodeCountingScans(doc *crdt.Doc, update []byte) (uint64, error) {
	if len(update) == 0 {
		return 0, nil
	}
	var scanned int
	stop := doc.OnAfterTransaction(func(txn *crdt.Transaction) { scanned += txn.IntegrateScanned() })
	defer stop()
	err := crdt.ApplyUpdateV1(doc, update, nil)
	return uint64(scanned), err
}
