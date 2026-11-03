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
// merged: the room's whole delete set at its load (seed, crdt.DeleteSetFromDoc), and every range of
// every update the room has been charged the structure of since - a browser's update the peer hook
// admitted, a transaction of the server's own (roomCharge.wrote) - with the GC runs and deleted
// items it integrated (record). A clock it lists that the room holds is deleted; one the room does
// not hold yet is in a range the room keeps parked, since ygo parks the part of a range past the
// clocks it holds (sjawhar/ygo v1.54.1-sami.3, crdt/delete_set.go:385-397, crdt/update.go:783-786)
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
