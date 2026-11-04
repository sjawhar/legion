package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// This file holds the budget the open rooms are charged against (roomBudget): what it charges
// them together, what it refuses a browser's admission for, and the sweeps and the pressure
// signal it answers a pod whose rooms no longer fit with.

// roomBudgetBytes is the most heap the open rooms are charged together (roomCharge) at the Go
// memory limit this package's figures are calibrated at: 2 GiB of a task's 4 GiB, under the
// 3.6 GiB limit a server of that container takes at boot (setGoMemoryLimit,
// cmd/dispatch/main.go). A process holding another limit charges them the same share of it
// (MemoryBounds.RoomBudget), which is what its budget's limit is (New); the figures below are
// this calibration's. Open documents are the largest thing the pod holds; the rest is the request
// path's, the cold reads' cache's (documentReadBudget, 256 MiB,
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

// pressureReserveBytes is how far past the budget's limit the charges of the delete-only updates
// admitted while the budget is full may take it at the calibration (refusedUnderPressure):
// 256 MiB, and the same share of another Go memory limit, since what fills it is a share of the
// rooms the budget holds (MemoryBounds.PressureReserve). A delete that cuts
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
// leaves (evictRoom). Eight seconds is over the 4.5 s one settlement of a mebibyte took on a
// loaded pod (TestShutdownThatCutsASettlementShortLeavesItToTheNextLoad), with the flush beside it.
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
// server's own work - an API write, which opens its room from the store (publishLiveWrite), the
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
	// limit is what the service's Go memory limit gives it (MemoryBounds.RoomBudget), but for a
	// browser test's own (SetRoomBudgetForTest), and used
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
	// waiters is waiting, for the readers that take no lock (sweepsForCloses): while an
	// admission waits, a room that becomes closable starts a sweep, as it does while full is up.
	waiters atomic.Int32
	// pending is whether the last sweep to end left a charged room that something other than a
	// connected browser holds - a settlement, a writer, an append - and that will be closable once
	// that ends; an admission still short when no sweep runs and none is pending is refused at once.
	pending bool
	// changed is closed, and replaced, by every change to the budget while an admission waits, so
	// each waiting admission weighs the budget again (await).
	changed chan struct{}
	// marked is how many loaded charges are closeOnceUnused, for the reader that takes no lock
	// (watchesForCloses).
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

// roomReservation is a socket's admission to a room that has not loaded: the count of the stored
// version at both copies (storedRoomReservation), held from the admission until the room loads,
// when the room's own charge replaces it, or until the socket's handler returns, whichever comes
// first.
type roomReservation struct {
	room    string
	weight  int64
	settled bool
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
// v1.55.1-sami.1, provider/websocket/peer.go:574-720, OnLastPeer), which is what marks the room's
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
// publish (publishLiveWrite), settlement's repair and warm-up, the mark sweep, a mark's
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

// sweepsForCloses reports, with no lock, whether a sweep may close a room something has just let
// go of: the budget is full, so the last sweep closed every room it could and this one may have
// become closable, or an admission waits on it (waiters), so a close may be what admits it.
// Neither means no sweep has anything to do with that room, and the budget is left untouched:
// a change to it starts a sweep of its own (unlock), which a kick of a room no sweep wants would
// be (Service.unlockState, kick).
func (b *roomBudget) sweepsForCloses() bool {
	return b.full.Load() || b.waiters.Load() > 0
}

// watchesForCloses reports, with no lock, whether the budget wants any room closed at all: a sweep
// may close one (sweepsForCloses), or a room is marked as opened over the budget and closed once
// nothing holds it (marked, Service.closeIfOpenedOverBudget). Where it is false, a room whose
// state has just been let go is left as it is (Service.unlockState).
func (b *roomBudget) watchesForCloses() bool {
	return b.sweepsForCloses() || b.marked.Load() > 0
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
