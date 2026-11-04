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
// merged. It has one source, the delete set ygo itself committed for the room: the room's whole
// delete set at its load (seed, crdt.DeleteSetFromDoc), and the delete set of every update the
// room's update observer drains, listed by the walk the room's charge already makes of that
// update (list, from roomCharge.applied and roomCharge.wrote). ygo writes
// buildDeleteSet(doc.store) - the ranges of every item of the room's store it holds deleted, and
// nothing else - into the update of every transaction (sjawhar/ygo v1.54.1-sami.3, encodeV1Locked,
// crdt/update.go:387 and :402, which buildPhase2 calls under the document's lock,
// crdt/doc.go:604-620; buildDeleteSet, crdt/update.go:665-678), so each drained update carries
// the room's whole delete set as it stood at that commit, and the list is their union: what ygo
// holds deleted as of the room's last drain, and nothing besides.
//
// A delete range it covers whole adds no struct, and one that overlaps what it lists none at an
// end inside it (splitEnds), so neither the structure part nor the struct total charges a
// browser's whole-state handoff for the delete set it carries. Without it the structure part,
// which no count clears, would charge the room's whole delete set again on every browser's sync,
// whose whole-state handoff carries it, and on every transaction, whose update ygo encodes with
// the room's whole delete set (crdt/doc.go:597-612): an edited room held open across reconnects
// would grow its charge with nothing added to its heap. And the struct total would count two
// structs for every range of it on every connect, which refused every browser reconnecting to a
// room at the decode cap before it had edited anything.
//
// Nothing else lists a clock. Not a browser's delete ranges, which the hook once listed as it
// admitted an update: they say which clocks the browser holds deleted, not which the room does.
// ygo integrates a struct from the first clock the room does not hold (crdt/item.go:82-90), so a
// GC run over clocks the room holds live deletes none of them, and it parks the part of a delete
// range past the clocks the room holds (crdt/delete_set.go:238-244, crdt/update.go:783-786),
// which deletes nothing until those clocks arrive and which no delete set ygo builds holds. And
// not the structs of an update, which the room once read from its own encode of a transaction it
// had committed: a struct that renders nothing is not always one ygo holds deleted. A GC struct
// is, since ygo decodes one as an item marked deleted (crdt/update.go:447-456), which
// buildDeleteSet lists. A deleted item - content reference 1, what a browser sends for a
// tombstone of its own - is not: ygo decodes one as an item carrying ContentDeleted whose Deleted
// stays false (crdt/update.go:1183-1191, decodeContent :1222-1229), and Item.integrate never
// marks it deleted (crdt/item.go:81-420), where Yjs marks it as it integrates its content (yjs
// 13.6.32, src/structs/ContentDeleted.js:65-68). buildDeleteSet lists item.Deleted alone
// (crdt/update.go:665-672), so the room's delete set holds no range of such an item - though
// encodeItem writes it back under the deleted reference (crdt/update.go:488-491), so a walk of
// the room's own encode reads it as a struct born deleted all the same. Listed from that walk, as
// the room once listed it, one 26-byte frame - a browser's rooted deleted item of length 1,000,
// whose length is a varuint that can claim up to 2^53 clocks - took 1,000 clocks of a client of
// its own off the books: covers answered true over them where crdt.DeleteSetFromDoc held no range
// of that client, and a delete of every other one of them was then charged nothing at all, where
// against the room's own delete set it is charged 999 structs, 999 splits and 81,360 frame bytes
// (TestADeletedItemTheRoomHoldsLiveLeavesLaterDeletesCharged).
//
// A range is listed only from a transaction that committed: a failed apply logs no update, and
// the room's bounded document took what it integrated back out whole (newRoomScans). Two updates
// weighed at once - two browsers', or a browser's and a server write committed meanwhile - can
// each find the same range unlisted and both be charged it, and so can an update weighed between
// the apply of a delete and the drain that lists it. Each of those over-charges and none
// under-charges: the list is a subset of what ygo holds deleted at every moment, and a range
// weighed against a subset is charged no less than against the whole. The list holds sixteen
// bytes for each run of the room's delete set, beside the tombstone of at least one struct, 129
// bytes or more (bytesOfHeapAStruct), that each run deletes, and it goes with the room.
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
// Where x is not held, the list holds neither x-1 nor x, so the end is charged a split: a room
// holds each client's clocks from 0 up, and integrates a struct reaching past them from the first
// clock it does not hold (crdt/item.go:82-90), and the list holds only clocks of items the room's
// store holds deleted (deletedRuns), which are clocks the room holds. Where x-1 and x are both
// held and both deleted, the halves at x merge back as above; the list holds no clock that is not
// deleted, so the two paragraphs above cover every end it answers for, and each place a listed
// run starts or ends is split once at most, by a range charged for it. The list only grows and
// never holds a clock ygo holds live, so a range weighed against it is charged no less than
// against the room's own delete set as its apply meets it. Nil, a room no load charged, lists
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

// list lists the clocks [clock, clock+length) of client of one range of the delete set of an
// update the room's update observer drained, as the walk the room's charge makes of that update
// reads it (roomCharge.applied, roomCharge.wrote): the one place a run is listed from an update,
// beside the room's delete set at its load (seed). The room's delete set only grows, so a range
// the list covers already adds nothing, and costs the read lock and a binary search alone -
// neither the write lock nor an allocation. That is what a keystroke into a room of thousands of
// deleted runs costs here: ygo writes the room's whole delete set into every transaction's
// update, and the list holds every range of it already. Nil, a room no load charged, lists
// nothing.
func (d *deletedRuns) list(client, clock, length uint64) {
	if d == nil || length == 0 || d.covers(client, clock, length) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addLocked(client, clock, length)
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
