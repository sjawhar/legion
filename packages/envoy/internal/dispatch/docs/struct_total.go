package docs

import (
	"cmp"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// This file holds the three totals the peer update hook keeps of each room (roomTotals): its
// struct total against the decode cap, its unrendered bytes against maxUnrenderedBytes and its
// whole-state frame against heaviestHandoffBound, with the bounds themselves, the exact counts
// that re-base them (countRoom) and the retired clients a rebuild left in the room's history.

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
// densest document a browser builds, measured on 2026-10-26
// (TestNoAbsoluteStructCapBothKeepsTheFrameAndClearsWhatRoomsHold):
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
// on 2026-10-26 (TestNoAbsoluteStructCapBothKeepsTheFrameAndClearsWhatRoomsHold): a document of
// 16,384 headings an agent rewrote
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

// roomCount is a room's state as the hook's totals weigh it (roomTotals): its structs, which the
// decode cap bounds; its unrendered bytes (unrenderedBytes), which maxUnrenderedBytes bounds; and
// its whole-state frame's bytes with what deleting what it holds can add (deletionReserve, and
// frameBytes and deleteFrameBytes for an addition), which heaviestHandoffBound bounds. inserts is
// whether an addition carries a struct the room does not hold, the only addition the frame bound
// refuses (fitsBounds); a count's is false, and the totals do not sum it.
type roomCount struct {
	structs, unrendered, frame uint64
	inserts                    bool
}

// plus is the three totals of c and other summed, each with its own. inserts is the caller's
// question about one addition, not a total, so it is left as c's.
func (c roomCount) plus(other roomCount) roomCount {
	c.structs += other.structs
	c.unrendered += other.unrendered
	c.frame += other.frame
	return c
}

// minus is other's three totals taken off c's, each off its own. A total only grows and is never
// taken off a smaller one of its own (roomTotals), so none of the three wraps.
func (c roomCount) minus(other roomCount) roomCount {
	c.structs -= other.structs
	c.unrendered -= other.unrendered
	c.frame -= other.frame
	return c
}

// atomicRoomCount is a roomCount every holder of the hook's totals reads and writes without a lock
// (roomTotals): the three totals as three atomics, so that each operation on them names them once.
// The three are not read together: a reader sees each as it stood when it read it, which is what
// the order the totals are written in makes safe (roomTotals).
type atomicRoomCount struct {
	structs, unrendered, frame atomic.Uint64
}

// add adds each of added's three totals to its own.
func (a *atomicRoomCount) add(added roomCount) {
	a.structs.Add(added.structs)
	a.unrendered.Add(added.unrendered)
	a.frame.Add(added.frame)
}

// sub takes each of added's three totals off its own.
func (a *atomicRoomCount) sub(added roomCount) {
	a.structs.Add(-added.structs)
	a.unrendered.Add(-added.unrendered)
	a.frame.Add(-added.frame)
}

// load is the three totals as they stand, structs first.
func (a *atomicRoomCount) load() roomCount {
	return roomCount{structs: a.structs.Load(), unrendered: a.unrendered.Load(), frame: a.frame.Load()}
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

// roomTotals is what the hook keeps of one room: its three running totals (roomCount) and the
// clients the room's history lists as retired (retired, entries). The first of the three is its
// struct total, the number of structs its whole state holds as ygo
// decodes it, against which the hook refuses an update that would take the room past the decode
// cap (maxUpdateItems). Measured on this branch: a second GC-struct frame of 600,000 structs left a
// room whose 2,200,030-byte state no longer decoded, though each frame alone was within the cap
// ygo's apply checks, so the cap has to be enforced on what the room holds, not on one update.
//
// The total is an exact count plus what the hook admitted since it. The count walks the room's
// encoded state (countRoomStructs), which carries parked structs and parked delete ranges
// (crdt.EncodeStateAsUpdateV1, sjawhar/ygo v1.54.1-sami.3, crdt/update.go:236-258). It is taken at
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
// once. A move adds to admitted before it takes from inflight, and runningTotals reads inflight
// before admitted, so a move between its two reads is seen at least once. The same order holds
// for writes, so a write that landed after a count's encode leaves the total stale.
type roomTotals struct {
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

	// inflight is the sum of the additions the hook has reserved whose applies have not yet
	// returned, all three totals of each; admitted is the same sum for every addition whose apply
	// has returned, and writes the number of transactions the observer has seen that were not a
	// hook's apply. admitted and writes only grow.
	inflight, admitted atomicRoomCount
	writes             atomic.Uint64

	// mu guards the last count - counted, with the admissions and writes as they were read before
	// it - and the hook's last confirmation: whether one was taken and the admissions when it was.
	mu                sync.Mutex
	counted           roomCount
	countedAdmitted   roomCount
	countedWrites     uint64
	confirmed         bool
	confirmedAdmitted roomCount
}

// newRoomTotals is live's totals, counted once now, at the room's load, with the clients live's
// history lists as retired and where it lists them.
func newRoomTotals(live *crdt.Doc) *roomTotals {
	retired := retiredClientsOf(live)
	total := &roomTotals{live: weak.Make(live), count: countRoom, retired: retired}
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
// 2^53 as a float64 (sjawhar/ygo v1.54.1-sami.3, encoding/encoder.go:283-299,
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

// retiredClients is the clients the room's history lists as retired (roomTotals.retired), nil for
// a total no load made.
func (t *roomTotals) retiredClients() map[crdt.ClientID]uint64 {
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

// retiredEntries is where the room's history holds its retired clients' list (roomTotals.entries),
// nil for a room never rebuilt or a total no load made.
func (t *roomTotals) retiredEntries() retiredEntries {
	if t == nil {
		return nil
	}
	return t.entries
}

// observed is the total's update observer: a transaction that is not one of the hook's applies
// marks the total stale.
func (t *roomTotals) observed(_ []byte, origin any) {
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
func (t *roomTotals) hookApplies(origin any) bool {
	if t == nil {
		return false
	}
	t.originMu.Lock()
	defer t.originMu.Unlock()
	return t.origins[origin] > 0
}

// countedStructs is the struct total of the room's last exact count (countRoomStructs): the one
// its load made (newRoomTotals), then each count since - the measurer's in its turn
// (recountLive), or a confirmation of the hook's (confirm). It is the struct count the room
// budget prices one copy of the room at (copyHeap); what the hook has admitted since the count is
// in the room's structure part already. Zero for a total no load made.
func (t *roomTotals) countedStructs() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return int64(t.counted.structs)
}

// applying runs apply, which applies an update with origin, as one of the hook's own applies.
func (t *roomTotals) applying(origin any, apply func() error) error {
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
func (t *roomTotals) reserve(doc *crdt.Doc, added roomCount) bool {
	if t == nil {
		return t.admits(doc, added)
	}
	t.admissions.Lock()
	defer t.admissions.Unlock()
	if !t.admits(doc, added) {
		return false
	}
	t.inflight.add(added)
	return true
}

// landed moves added, a reservation whose apply has returned, from inflight to admitted: to
// admitted first, so a running total read between the two never misses it.
func (t *roomTotals) landed(added roomCount) {
	if t != nil {
		t.admitted.add(added)
		t.inflight.sub(added)
	}
}

// unreserve gives back added, a reservation no apply followed or whose apply failed.
func (t *roomTotals) unreserve(added roomCount) {
	if t != nil {
		t.inflight.sub(added)
	}
}

// runningTotals is the three totals as they stand - the last count, what the hook admitted since
// and what it has in flight - and whether a write the hook did not see has landed since the
// count. The reservations in flight are read before the admissions (roomTotals gives why).
func (t *roomTotals) runningTotals() (total roomCount, stale bool) {
	t.mu.Lock()
	counted, admitted, writes := t.counted, t.countedAdmitted, t.countedWrites
	t.mu.Unlock()
	inflight := t.inflight.load()
	return counted.plus(inflight).plus(t.admitted.load().minus(admitted)), t.writes.Load() != writes
}

// roomStructs is the room's struct total as it stands, which the apply heap's estimate takes as
// the most deleted runs the room's state can hold (peerWeighing.applyHeap). A total a server write
// made stale lags what the server's writes added since the last count, until the measurer's next
// count re-bases it (elementMeasurer.count). A nil total, a room no load listed, is counted
// afresh.
func (t *roomTotals) roomStructs(doc *crdt.Doc) uint64 {
	if t == nil {
		return countRoomStructs(doc)
	}
	total, _ := t.runningTotals()
	return total.structs
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
func (t *roomTotals) admits(doc *crdt.Doc, added roomCount) bool {
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
func (t *roomTotals) confirmationHolds(stale bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if stale || !t.confirmed {
		return false
	}
	since := t.admitted.load().minus(t.confirmedAdmitted)
	return since.structs < structTotalRecheck && since.unrendered < unrenderedRecheck &&
		since.frame < frameRecheck
}

// confirm re-bases the totals on an exact count of doc and records it as the hook's confirmation.
func (t *roomTotals) confirm(doc *crdt.Doc) {
	admitted := t.recount(doc)
	t.mu.Lock()
	t.confirmed, t.confirmedAdmitted = true, admitted
	t.mu.Unlock()
}

// recountLive re-bases the totals on an exact count of the room's live document, if it is still
// resident: the measurer's count, in its turn.
func (t *roomTotals) recountLive() {
	if t == nil {
		return
	}
	if live := t.live.Value(); live != nil {
		t.recount(live)
	}
}

// recount re-bases the totals on an exact count of doc, and returns the admissions it was counted
// against.
func (t *roomTotals) recount(doc *crdt.Doc) roomCount {
	admitted := t.admitted.load()
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
