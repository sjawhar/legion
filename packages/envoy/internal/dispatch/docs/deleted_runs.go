package docs

import (
	"slices"
	"sort"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// This file holds the delete ranges a room is known to hold deleted (deletedRuns), which the peer
// update hook asks what a browser's delete range would add to the room: nothing for one the room
// holds deleted already, which is most of a reconnecting browser's whole-state handoff.

// deletedRuns is the delete ranges a room is known to hold deleted, each client's sorted and
// merged: the room's whole delete set at its load (seed, crdt.DeleteSetFromDoc), the ranges of
// every update the room has been charged the structure of since - a browser's update the peer
// hook admitted, a transaction of the server's own (roomCharge.wrote) - (record), and the clocks
// every transaction of the room integrated deleted, a GC run's and a deleted item's, read from
// the update ygo encoded of that transaction: a server write's as its charge walks it
// (integrated), a browser's as the room's update observer hands its commit over (listed,
// roomCharge.applied). A clock it lists that the room holds is deleted; one the room does not hold
// yet is in a range the room keeps parked, since ygo parks the part of a range past the clocks it
// holds (sjawhar/ygo v1.54.1-sami.3, crdt/delete_set.go:385-397, crdt/update.go:783-786) and an
// update is listed only once its apply has returned. A delete range it covers whole adds no
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
//
// It lists no clock a browser's bytes claim. A GC run or a deleted item a browser sends says
// which clocks it holds deleted, not which the room does: ygo integrates a struct from the first
// clock the room does not hold (sjawhar/ygo v1.54.1-sami.3, crdt/item.go:82-90), so a GC run over
// clocks the room holds live deletes none of them, and one whose predecessor is missing parks and
// deletes nothing yet. Listing such a run was a forgery one 22-byte frame could send: with the
// room's live text listed deleted, a later delete range over it was covered (covers) and left no
// end split (splitEnds), so the struct total, the structure part and the frame total each charged
// it nothing until the measurer's next count. Clipping the run to the clocks past the state vector
// the hook's weighing read is not enough either, since no lock is held from that read to the apply
// (weighPeerUpdate) and another browser's update or a server write can fill those clocks live in
// between. Reading the room's whole delete set after such an apply is not the answer either: a
// crafted browser sends a frame of a few bytes, one GC run of a client id of its own, as often as
// the decode cap lets it, and each of them would walk the room's whole store. So the room lists
// what its own transaction integrated instead, from the update ygo encoded of it, which no
// browser writes and no apply racing it can forge.
type deletedRuns struct {
	mu      sync.RWMutex
	clients map[uint64][]clockRun
}

// clockRun is the clocks [start, end) of one client.
type clockRun struct{ start, end uint64 }

// deletedRun is the clocks [clock, clock+length) of one client that one struct of an update
// integrated deleted, as a walk of that update read them (roomCharge.applied).
type deletedRun struct{ client, clock, length uint64 }

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
// already. A range of no clocks splits nothing (sjawhar/ygo v1.54.1-sami.3,
// crdt/delete_set.go:187-189), and is covered. Nil, a room no load charged, covers nothing.
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
// clock+length, sjawhar/ygo v1.54.1-sami.3, crdt/delete_set.go:199-201 and crdt/store.go:168-185),
// deletes whole each item between them (crdt/delete_set.go:211-237) and parks the part of the
// range past the clocks the room holds (crdt/delete_set.go:238-244). ygo sorts and merges each
// client's ranges as it decodes them (crdt/update.go:1474), so every range it applies ends where a
// range of the update ends. A range of no clocks splits nothing (crdt/delete_set.go:187-189).
//
// An end x can be left split only where the list holds neither clock x-1 nor clock x, and never at
// clock 0, where a client's first item starts. ygo keeps an item deleted whole or not at all: a
// delete cleans its ends first, and a merge joins only two items of one deleted state
// (tryMergeWithLeft, crdt/transaction.go:554). So where x-1 and x are held, one deleted and the
// other not, they lie in two items and x is clean already. Where both are held deleted, the split
// at x cuts a deleted item into two deleted halves, which ygo merges back as the transaction ends:
// splitItem records the right half (crdt/item.go:579-582) and tryMergeWithLefts merges it with its
// left (crdt/transaction.go:448-476, run at commit, crdt/doc.go:860-864), since the halves share
// client, parent, origins and deleted state, and an item that can split - a deleted run, or a run
// of string, Any or JSON content - holds content that merges (crdt/transaction.go:587-618), every
// other content being one clock long. Only an item integrated between the halves keeps them
// apart, and ygo's integrate puts one there only when it names clock x-1 as its origin or clock x
// as its right origin (crdt/item.go:103-105 and :155-201: past the left half, its scan always
// passes the right half, whose origin is the left half's last clock), which its own integrate
// cleans at x, a split its own count holds (splitsAStructCanCause).
//
// Where x is not held, no item holds it: a room holds each client's clocks from 0 up, and
// integrates a struct reaching past them from the first clock it does not hold
// (crdt/item.go:82-90), so where x-1 is held the item that brings x starts at x. Where x-1 is not
// held either, one item the same transaction integrates can hold both; what the list holds of them
// is then in ranges the room keeps parked, which ygo deletes in that transaction once their clocks
// arrive (drainParked, crdt/update.go:803). Where x-1 and x both end deleted, the halves at x
// merge back as above; where only one does, x is where a parked run starts or ends, a split
// charged to the range that first listed a clock beside x, which had its own end at x with neither
// clock listed - and the room's count holds splitsAStructCanCause structs for each parked range
// besides (structTotalOf). So each place a listed run starts or ends is split once at most, by a
// range charged for it; and the list only grows, so a range weighed against the list before its
// apply is charged no less than against the list its apply meets. Nil, a room no load charged,
// lists nothing.
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
// peer hook admitted, or a transaction of the server's own (roomCharge.wrote). It walks as a
// room's own state is walked (fromRoom), which refuses only what ygo cannot decode: the server's
// writes can carry what no browser sends, and an update the hook admitted passed the stricter
// walk already. The structs of the update are not read here: a browser's say which clocks the
// browser holds deleted, not which the room does, and a transaction's own are listed from the
// update the room encoded of it (integrated, listed). Nil records nothing.
func (d *deletedRuns) record(update []byte, deletesAt int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = walkDeleteSet(update, deletesAt, fromRoom, (*deletedRunsRecorder)(d))
}

// integrated lists the ranges of the delete set of update and the clocks of every struct of it
// that integrates deleted - a GC run, a deleted item (bornDeleted), which the delete set of every
// encode of the room's state lists from then on (buildDeleteSet, sjawhar/ygo v1.54.1-sami.3,
// crdt/update.go:665-678) though no delete range named them - from one walk of the whole update
// rather than of its delete set alone. Its update is ygo's own encode of a transaction the room
// committed, which the room's update observer hands over (roomCharge.wrote): ygo writes it from
// the state before the transaction (buildPhase2 and encodeV1Locked, crdt/doc.go:604-620 and
// crdt/update.go:366-406), so every struct in it is a struct the room's store holds, and one born
// deleted is one the room holds deleted. A browser's bytes are never read this way
// (deletedRuns). Nil lists nothing.
func (d *deletedRuns) integrated(update []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = walkUpdate(update, fromRoom, (*deletedRunsRecorder)(d))
}

// listed lists runs, the clocks the structs of one transaction of the room integrated deleted: a
// walk of the update ygo encoded of that transaction read them, and its caller holds no lock of
// this list (roomCharge.applied). Nil, a room no load charged, lists nothing.
func (d *deletedRuns) listed(runs []deletedRun) {
	if d == nil || len(runs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, run := range runs {
		d.addLocked(run.client, run.clock, run.length)
	}
}

// deletedRunsRecorder is record's and integrated's visitor, which its caller holds the lock for.
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
