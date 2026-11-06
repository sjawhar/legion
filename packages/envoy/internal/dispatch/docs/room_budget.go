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
// path's, the cold reads' cache's (documentReadBudget, 256 MiB, which this does not count) and
// the collector's slack. At the measured cost of a room (bytesOfHeapAStateByte, both copies) it
// holds twenty-five documents of a mebibyte of prose, whose 3,035,240-byte state is charged
// 84,986,720, and a document at the state cap (maxRoomStateBytes) is charged 378,000,000, so five
// of those fill it. No room has a ceiling: a room is charged what its state comes to, however
// heavy, and what answers a room that heavy is the hook's refusal of every apply that integrates
// anything while nothing can be closed (roomBudget.full), not a cap on its charge.
//
// Nothing is reserved before a room loads. What keeps fifty admissions at once from each reading
// the same headroom is the pod's load concurrency, one slot at the calibration
// (MemoryBounds.LoadSlots): each cold load holds a slot while it decodes, so the budget sees
// each room's charge before the next load begins.
const roomBudgetBytes = 2 << 30

// admissionFlushAllowance is what an admission that finds the budget short waits for its sweep
// beyond the settle delay (Service.admissionWait): a room whose browser has just gone is not
// closable until a settlement has committed what it owes - the one its browser's leaving runs at
// once (settleLastPeer), or the one the settle delay after its last write arms - and the close
// that follows flushes the room's updates to the store and compacts them before the room's charge
// leaves (evictRoom). Eight seconds is over the 4.5 s one settlement of a mebibyte took on a
// loaded pod (TestShutdownThatCutsASettlementShortLeavesItToTheNextLoad), with the flush beside it.
const admissionFlushAllowance = 8 * time.Second

// errRoomBudgetFull is the refusal of a browser the open rooms leave no room for (ServeHTTP). It
// is answered with DocumentBusyCloseCode, the transient close, and only once the admission has
// waited for the sweeps it started to close what they could (roomBudget.await): the next attempt
// finds the rooms open retiring by themselves, or the rooms whose browsers leave meanwhile.
var errRoomBudgetFull = errors.New("the open documents fill their memory budget")

// roomBudget charges every open room the heap it holds, and refuses a browser one more room past
// limit. It is written as the cold reads' cache is (documentReads): one mutex, a count against a
// budget, and a measured weight per entry, here a room's (roomCharge). It charges every room
// whoever opened it, and refuses only a browser's socket at its admission (ServeHTTP): never
// Server.Apply, so the server's own work - an API write's publish, which opens its room from the
// store (publishLiveWrite), settlement's warm-up and repair, the unrecorded-mark sweep - opens
// its room over the budget, and a room so opened is closed once nothing holds it
// (closeOnceUnused, Service.closeIfOpenedOverBudget).
//
// What bounds that work is not a refusal but an opening (openServerRoom): it loads a room the
// budget does not charge only while the charges stand under the limit and no other such load is
// under way, so the charges can stand no further past the limit than the one room an opening
// makes. Without it a burst of that work - three hundred API writes to documents no browser
// holds, each publishing into a room of its own, none of them closable until the settlement its
// write arms has run - took the pod 713,885,218 bytes past its Go memory limit over a full
// budget (cmd/dispatch, TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory). A publish the
// opening refuses leaves its room closed, which loses nothing (publishedWithoutARoom); a
// settlement waits for one, and is armed again where none came (settleNoRoom).
//
// Over its limit, the rooms that hold nothing outlasting them are closed (sweepRooms), and an
// admission that finds the budget over it waits for those closes before it is refused (await).
// When the budget is still over its limit with none left to close, full is raised: the one signal
// the peer update hook reads (Service.weighPeerUpdate), with one atomic load, since the hook runs
// on the peer's read loop and takes neither this mutex nor a room's state lock. It is lowered
// once the rooms' charges are back within the budget, and a sweep started once a room may have
// become closable lowers it too.
type roomBudget struct {
	mu sync.Mutex
	// limit is what the service's Go memory limit gives it (MemoryBounds.RoomBudget), but for a
	// browser test's own (SetRoomBudgetForTest), and used what the loaded rooms come to.
	limit, used int64
	// rooms holds, per document, the charges of its loaded instances; a document with none has
	// no entry.
	rooms map[string]*budgetedRoom
	// sweeping is set while a sweep runs, so one runs at a time, and again is set by anything that
	// would have started one meanwhile - a room that may have become closable, an admission that
	// began to wait - so that the sweep runs once more if the budget is still over its limit when
	// it ends (swept): the running sweep may have looked at that room, or weighed the budget,
	// before.
	sweeping, again bool
	// sweep closes the rooms nothing holds until the budget is met (Service.sweepRooms), started
	// on a goroutine of its own. Nil in a budget no service built, which never sweeps.
	sweep func()
	// waiting is how many admissions wait on the budget, and waiters the same for the readers
	// that take no lock (sweepsForCloses): while an admission waits, a room that becomes closable
	// starts a sweep, as it does while full is up.
	waiting int
	waiters atomic.Int32
	// wanted is what the admissions waiting on the budget need together: each one's room, which
	// the budget does not charge yet, at roomWeight of the stored state that admission read
	// (refusal). A sweep closes rooms for it as well as for the limit (short), so an admission
	// whose room would cross the limit waits for a close rather than being refused while a room
	// nothing holds is still open.
	wanted int64
	// reserved is what the admissions already weighed hold for rooms whose loads have not run
	// yet (roomReservation), which the budget weighs beside the charges: a room's reservation
	// is released when its load charges the room, or when the socket that holds it goes.
	reserved int64
	// pending is whether the last sweep to end left a charged room that something other than a
	// connected browser holds - a settlement, a writer, an append - and that will be closable once
	// that ends; an admission still refused when no sweep runs and none is pending is refused at
	// once.
	pending bool
	// changed is closed, and replaced, by every change to the budget while an admission waits, so
	// each waiting admission weighs the budget again (await).
	changed chan struct{}
	// marked is how many loaded charges are closeOnceUnused, for the reader that takes no lock
	// (watchesForCloses).
	marked atomic.Int32
	// full is raised by a sweep that leaves the budget over its limit, having closed every room it
	// could (swept).
	full atomic.Bool
	// opening is the one room the server's own work is loading that the budget does not yet
	// charge, named so that the charge its load makes is the one marked closeOnceUnused
	// (openServerRoom, load), and empty while none is. Another such load waits for it, or gives
	// up and leaves its room closed.
	opening string
}

// budgetedRoom is what the budget holds for one document: the charges of the instances of its
// room ygo has loaded and not yet retired, oldest first - more than one only while a retired
// instance's unload has not fired yet when its successor loads.
type budgetedRoom struct {
	charges []*roomCharge
	// reservations is what the sockets admitted to this room hold for the load that has not
	// run yet (roomReservation): the first admission's weight, and nothing for each later one,
	// since one load serves them all. The room's load settles every one of them.
	reservations []*roomReservation
}

// join reports whether room has a loaded instance the budget charges, which a socket joins
// (joinedLocked); over the limit it starts a sweep, since the socket's room may be one of many
// that something has stopped using.
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

// roomReservation is the weight a socket's admission holds for the room it is about to load,
// from the weighing until that room's load charges it (roomBudget.load) or the socket goes
// (release). It is what keeps two admissions weighed at the same moment from each reading the
// same headroom.
type roomReservation struct {
	room    string
	weight  int64
	settled bool
}

// refusal weighs an admission and holds what it needs until its room has loaded: it answers a
// reservation, or a budget with no room for the admission refuses it, once the sweeps the
// admission starts have closed what they could before deadline or ctx ends (await).
//
// weight is the room the admission is about to load, which the budget does not charge yet: the
// bytes of the stored state the admission has already read and decoded for it (ServeHTTP's
// preload), at roomWeight. So the admission is weighed as the budget will stand once its room
// has loaded, which is what keeps it from taking the budget past its limit at all. That matters
// beyond the arithmetic: the budget standing over its limit raises full, and full refuses every
// insert in EVERY room of the pod with 4503 until a sweep can close something, which it cannot
// while browsers hold those rooms - so an admission that crossed the limit made the whole pod
// read-only. Measured before this: fifty browsers at once, twenty-seven admitted, and every one
// of the twenty-seven refused on its first edit (INFRA-179).
//
// The reservation is what makes it hold for a burst rather than for one: every admission in
// flight is weighed against the others' rooms as well as against the rooms already open. Two
// admissions to the SAME document reserve its weight once - the second shares the first's, at a
// weight of nothing - since one load serves both. It is not the reservation the cut deleted:
// nothing is cached, no count is read for it and no flight is held, only a length the admission
// already holds. While an admission waits, what it needs is wanted, so the sweeps close rooms
// for it as well as for the limit (short).
func (b *roomBudget) refusal(ctx context.Context, deadline time.Time, room string, weight int64) (*roomReservation, error) {
	b.mu.Lock()
	defer b.unlock()
	if entry := b.rooms[room]; entry != nil && len(entry.reservations) > 0 {
		weight = 0
	}
	b.wanted += weight
	defer func() { b.wanted -= weight }()
	if !b.await(ctx, deadline, func() bool { return b.used+b.reserved+weight <= b.limit }) {
		return nil, fmt.Errorf("%w: %d charged and %d reserved of %d bytes, and the room asks %d",
			errRoomBudgetFull, b.used, b.reserved, b.limit, weight)
	}
	reservation := &roomReservation{room: room, weight: weight}
	entry := b.entry(room)
	entry.reservations = append(entry.reservations, reservation)
	b.reserved += weight
	return reservation, nil
}

// release gives a reservation back, unless its room's load has already settled it. A nil
// reservation releases nothing, which is what a socket that joined a charged room holds.
func (b *roomBudget) release(reservation *roomReservation) {
	if reservation == nil {
		return
	}
	b.mu.Lock()
	defer b.unlock()
	b.settleLocked(reservation)
}

// settleLocked drops reservation from its room and from the reserved total, once. Its caller
// holds mu.
func (b *roomBudget) settleLocked(reservation *roomReservation) {
	if reservation.settled {
		return
	}
	reservation.settled = true
	b.reserved -= reservation.weight
	entry := b.rooms[reservation.room]
	if entry == nil {
		return
	}
	entry.reservations = slices.DeleteFunc(entry.reservations, func(held *roomReservation) bool {
		return held == reservation
	})
	b.dropIfEmptyLocked(reservation.room, entry)
}

// awaitRoom waits for the budget's sweep to bring the charges back within the limit, and reports
// whether they are: what an admission waits for, with a weight of nothing. A browser's update
// about to be refused for the pressure waits for it too, so the rooms nothing is still using are
// closed before anything is refused (Service.underPressure, the spec's R3 and A3).
func (b *roomBudget) awaitRoom(ctx context.Context, deadline time.Time) bool {
	b.mu.Lock()
	defer b.unlock()
	return b.await(ctx, deadline, func() bool { return b.used+b.reserved <= b.limit })
}

// await waits until fits reports room for an admission, and reports whether it does. Its caller
// holds mu, which it gives up while it waits and holds again whenever it looks and when it
// returns. While it waits, a room that becomes closable starts a sweep of its own (waiters,
// Service.unlockState). It starts a sweep, or has the running one run again once it ends, since
// that one may have weighed the budget before this admission arrived (sweepAgainLocked).
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
func (b *roomBudget) await(ctx context.Context, deadline time.Time, fits func() bool) bool {
	if fits() {
		return true
	}
	b.waiting++
	b.waiters.Store(int32(b.waiting))
	defer func() {
		b.waiting--
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

// openServerRoom books the budget's one opening, the one room the server's own work may be
// loading that the budget does not yet charge (roomBudget), and reports whether it booked it:
// the charges stand under the limit and no other opening stands. The caller gives it back once
// it is done opening the room (closeServerRoom), by when the budget charges the room it made and
// the next opening is weighed against that charge.
//
// Those two conditions together are what bounds the server's own work, and neither does it
// alone. The limit alone would not: a room the budget has not weighed cannot be refused by
// arithmetic - the work that opens it holds no measure of the stored document, and reading one
// would cost it a pooled connection (publishLiveUpdate) - so three
// hundred of them would read the same headroom at the same moment and all pass. One at a time
// alone would not either, for a caller that gives the opening back while its room stands: a room
// outlives the load that opened it, by the settlement the write armed in it and then by the
// minute ygo leaves it idle, so openings taken in turn would still pile rooms up. With both, the
// charges can stand no further past the limit than the one room an opening makes, since the next
// opening is weighed once that room's charge is in.
//
// A caller refused here opens no room, which is the one thing a publish of a committed write can
// do without: its update is durable before it publishes, no browser holds a document no room
// holds, and the room that loads next reads the store, which holds the write
// (publishLiveWrite). A caller that cannot do without one waits instead (awaitServerRoom). A
// budget with no limit is a budget no service built, a test's, which bounds nothing and opens
// whatever it is asked for, as its admissions are never refused (await).
func (b *roomBudget) openServerRoom(room string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.opening != "" || !(b.limit <= 0 || b.used < b.limit) {
		return false
	}
	b.opening = room
	return true
}

// awaitServerRoom books that opening for a caller that cannot do without its room - a
// settlement's warm-up (settleRoomWithin) - waiting until no other opening stands, and reports
// whether it booked one. It waits until deadline passes or ctx ends; the opening before it being
// given back wakes it, as does every other change to the budget (unlock).
//
// The limit is no condition here, where it is one for a publish. A settlement is the one thing
// that must still happen on a budget its browsers have filled past its limit: it is what writes
// the document's version, reconciles its ask blocks and clears its pending-settlement row, and a
// document whose settlement waited for a charge to fall would wait as long as those browsers
// stayed (TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks drives exactly that server). What
// bounds it instead is the opening alone, held from before the warm-up until the settlement has
// let the room go, so one such room stands at a time whatever the charges read.
//
// It counts as an admission waiting on the budget while it waits (waiters) and starts a sweep,
// wanting nothing of its own: the room an opening before it loaded is closed once the work that
// opened it is done with it (Service.closeIfOpenedOverBudget), and a sweep started here is what
// closes the rooms a burst of this work has left behind. A caller it refuses loses nothing and
// gives nothing up for good: the settlement is armed again (settleNoRoom), and the document's
// pending-settlement row outlives every process.
func (b *roomBudget) awaitServerRoom(ctx context.Context, deadline time.Time, room string) bool {
	b.mu.Lock()
	defer b.unlock()
	for {
		if b.opening == "" {
			b.opening = room
			return true
		}
		wait := time.Until(deadline)
		if wait <= 0 || ctx.Err() != nil {
			return false
		}
		b.waiting++
		b.waiters.Store(int32(b.waiting))
		b.sweepAgainLocked()
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		b.mu.Lock()
		b.waiting--
		b.waiters.Store(int32(b.waiting))
	}
}

// closeServerRoom gives the opening back, whether its load made a room or not, and wakes whatever
// waits for one (unlock).
func (b *roomBudget) closeServerRoom() {
	b.mu.Lock()
	defer b.unlock()
	b.opening = ""
}

// load charges room's newly loaded instance at stateBytes, the bytes of the stored state the
// load decoded, at both copies (roomWeight): the room's two documents hold that state, and what
// the room commits after it the room's update observer adds as it drains
// (renderedReplica.chargeAdded). A room the budget's opening named is the server's own work's -
// an API write's publish (publishLiveWrite), settlement's repair and warm-up, the mark sweep, a
// mark's verification, a snapshot read, a live tree's read, settlement resumed at boot: every
// opener of Server.Apply (openServerRoom) - and where its charge leaves the budget over its
// limit it is marked closeOnceUnused, to be closed once that work no longer holds it
// (Service.closeIfOpenedOverBudget) rather than staying charged until it retires a minute later.
func (b *roomBudget) load(room string, stateBytes int64) *roomCharge {
	b.mu.Lock()
	defer b.unlock()
	entry := b.entry(room)
	charge := &roomCharge{budget: b, room: room}
	entry.charges = append(entry.charges, charge)
	// The room is charged now, so every admission waiting for this load stops holding its
	// weight: the charge is the real figure and the reservation was its estimate.
	for _, reservation := range slices.Clone(entry.reservations) {
		b.settleLocked(reservation)
	}
	b.reweighLocked(charge, roomWeight(stateBytes))
	if b.opening == room && b.used > b.limit {
		charge.closeOnceUnused = true
		b.marked.Add(1)
	}
	return charge
}

// retire returns the charge of room's oldest loaded instance, which ygo has retired
// (releaseUnloadedRoom). ygo fires one unload for each instance it loaded, and loads the next
// only once the last is out of its map, so the oldest charge is the retired instance's - or,
// where two retire before either unload fires, the other's, which is the same weight.
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

// short is whether the charges leave no room for the limit or for what the admissions waiting on
// the budget need together (wanted). A sweep closes rooms while it holds.
func (b *roomBudget) short() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used+b.reserved+b.wanted > b.limit
}

// figures is what the rooms' charges come to and the limit, for a sweep to log what each room it
// closes left.
func (b *roomBudget) figures() (used, limit int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.limit
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
	if entry := b.rooms[room]; entry == nil || len(entry.charges) == 0 || b.used <= b.limit {
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
// (unlockState). Whatever waits on the budget is woken to weigh it again: the admissions
// (await), and the server's own work waiting for an opening (awaitServerRoom). Only a waiter
// leaves a channel to close (wakeLocked), so this costs a change nobody waits on nothing.
func (b *roomBudget) unlock() {
	if b.used <= b.limit {
		b.full.Store(false)
	} else if !b.full.Load() {
		b.startSweepLocked()
	}
	b.wakeLocked()
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
// for the sweep again while it ran and the budget is still over its limit, it runs again at
// once, on a goroutine of its own. Otherwise it raises full when it left the budget over its
// limit: it closed every room it could. Full refuses every apply that integrates anything in
// every room (Service.weighPeerUpdate), and a single oversized document refused on an almost
// empty pod must not freeze every room's growth; an admission still refused is answered by its
// own refusal once it stops waiting. Every waiting admission is woken to weigh the budget again.
func (b *roomBudget) swept(pending bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = pending
	b.wakeLocked()
	if b.again && b.used > b.limit {
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

// reweighLocked charges charge weight in place of what it carried, unless its room has retired.
// Its caller holds mu.
func (b *roomBudget) reweighLocked(charge *roomCharge, weight int64) {
	if charge.retired {
		return
	}
	b.used += weight - charge.weight
	charge.weight = weight
}
