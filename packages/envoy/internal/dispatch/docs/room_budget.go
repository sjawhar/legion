package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// roomBudgetBytes is the most heap the open rooms are charged together (roomCharge): 2 GiB of the
// task's 4 GiB, under the 3.6 GiB Go memory limit the server takes from its container at boot
// (cmd/dispatch/main.go:119-128). Open documents are the largest thing the pod holds; the rest is
// the request path's, the cold reads' cache's (documentReadBudget, 256 MiB, which this does not
// count) and the slack that limit leaves the collector. At the measured cost of a room
// (bytesOfHeapAnElement) it holds thirty-nine edited documents of a mebibyte of prose (57,600
// elements, 54 MB each) and refuses the fortieth, and holds thirty-four at the element bound (62 MB
// each) (TestTheRoomBudgetsArithmeticAtTheBound).
const roomBudgetBytes = 2 << 30

// bytesOfHeapAnElement is the heap one copy of a document holds for each element its rendering
// makes (pmdoc.MeasureDocument): about 470 bytes at 524 KiB and at 1 MiB, 578 at 51 KiB, where
// what every document holds whatever its size is a larger share. A room that is only read holds
// one copy, its live document; a room that has taken an update holds two, the live document and
// the rendered replica its update observer keeps (renderedReplica). The heap follows the elements
// and not the markdown's bytes, which misprice a dense document by several times: the figure
// before this one, 120 bytes of heap a byte of markdown, was measured on a shape no room can hold
// (room_heap_test.go). TestAResidentRoomsHeapPerByteOfItsMarkdown measures rooms of each size,
// read and edited, and fails once a copy's heap an element leaves 0.85x to 1.3x of this.
const bytesOfHeapAnElement = 470

// errRoomBudgetFull is the refusal of a browser the open rooms leave no room for (reserveRoom). It
// is answered with DocumentBusyCloseCode, the transient close: the next attempt finds the bytes
// the sweep it started has freed, or the rooms open retiring by themselves.
var errRoomBudgetFull = errors.New("the open documents fill their memory budget")

// roomBudget charges every open room the heap it holds, and refuses a browser one more room past
// limit. It is written as the cold reads' cache is (documentReads): one mutex, a count against a
// budget, and a measured weight per entry, here a room's (roomCharge) and each admission's
// reservation before its room loads (roomReservation). It charges every room whoever opened it,
// and refuses only a browser's socket at its admission (reserveRoom): never Server.Apply, so the
// server's own work - an API write, which opens its room from the store (livewrite.go:303), the
// unrecorded-mark sweep, settlement's repair - opens its room over the budget, and the room is
// charged and closed once nothing holds it (sweepRooms).
//
// Over the budget, the rooms that hold nothing outlasting them are closed (sweepRooms), and when
// none is left to close, full is raised: the one signal the peer update hook reads (Service.
// weighPeerUpdate), with one atomic load, since the hook runs on the peer's read loop and takes
// neither this mutex nor a room's state lock. It is lowered once the rooms' charges are back
// within the budget, and a sweep started once a room may have become closable lowers it too.
type roomBudget struct {
	mu sync.Mutex
	// limit is roomBudgetBytes, but for a browser test's own (SetRoomBudgetForTest), and used
	// what the rooms' charges and the reservations come to.
	limit, used int64
	// rooms holds, per document, the charges of its loaded instances and the reservations of the
	// sockets being admitted to it; a document with neither has no entry.
	rooms map[string]*budgetedRoom
	// sweeping is set while a sweep runs, so one runs at a time.
	sweeping bool
	// sweep closes the rooms nothing holds until the budget is met (Service.sweepRooms), started
	// on a goroutine of its own. Nil in a budget no service built, which never sweeps.
	sweep func()
	// full is raised by a sweep that leaves the budget over, having closed every room it could.
	full atomic.Bool
	// weighing lets one admission at a time count the elements of a stored document
	// (storedRoomWeight).
	weighing chan struct{}
}

// budgetedRoom is what the budget holds for one document: charges, the instances of its room ygo
// has loaded and not yet retired, oldest first - more than one only while a retired instance's
// unload has not fired yet when its successor loads - and reservations, the sockets admitted to
// it whose room has not loaded.
type budgetedRoom struct {
	charges      []*roomCharge
	reservations []*roomReservation
}

// roomCharge is one loaded room's charge: its latest measured document at its copies, plus the
// bytes of the updates it has taken since that measurement (weight). Each measurement replaces the
// last rather than adding to it, and the update bytes beside it start again from that measurement:
// a charge that only rose would never fall while browsers stayed, since a Yjs delete tombstones
// what it deletes rather than freeing it. What no rendering shows - those tombstones - is what the
// charge does not see; the room's own retirement a minute after its last use, and the compaction
// that folds its updates as it closes, reclaim it.
type roomCharge struct {
	budget *roomBudget
	room   string
	// elements is the room's latest count (renderedReplica.elements), and copies 1 until the room
	// takes its first update and 2 from then on.
	elements, copies int64
	// updates is the bytes of every update the room has taken since it loaded, and measuredAt what
	// it was at the rendering elements counts.
	updates, measuredAt int64
	weight              int64
	retired             bool
}

// roomReservation is a socket's admission to a room that has not loaded: the weight of its stored
// document, held from the admission until the room loads, when the room's own charge replaces it,
// or until the socket's handler returns, whichever comes first.
type roomReservation struct {
	room    string
	weight  int64
	settled bool
}

// roomWeight is what a room of elements elements at copies copies, which has taken updates bytes
// since they were counted, is charged. A count past the element bound - a rendering goldmark
// stopped counting (elementsPastCounting), or a document a browser carried past the bound, which
// every server write then refuses to grow (growth.go:139-150) - is charged at the bound, so no room
// is charged more than about 62 MB at two copies, beside its update bytes. A room that renders no
// markdown (sizeUnmeasured) is charged its update bytes alone.
func roomWeight(elements, copies, updates int64) int64 {
	return min(max(elements, 0), pmdoc.MaxDocumentElements)*bytesOfHeapAnElement*copies + updates
}

// join reports whether room has a loaded instance the budget charges, which a socket joins with
// nothing reserved; over the limit it starts a sweep, since the socket's room may be one of many
// that something has stopped using.
func (b *roomBudget) join(room string) bool {
	b.mu.Lock()
	defer b.unlock()
	entry := b.rooms[room]
	if entry == nil || len(entry.charges) == 0 {
		return false
	}
	if b.used > b.limit {
		b.startSweepLocked()
	}
	return true
}

// refusal is a budget already past its limit refusing an admission before it weighs anything, and
// starting a sweep; nil within the limit.
func (b *roomBudget) refusal() error {
	b.mu.Lock()
	defer b.unlock()
	if b.used <= b.limit {
		return nil
	}
	b.startSweepLocked()
	return fmt.Errorf("%w: %d of %d bytes are charged", errRoomBudgetFull, b.used, b.limit)
}

// reserve holds weight for a socket admitted to room, or reserves nothing for a room already
// charged, which the socket joins. Past the limit it reserves nothing, starts a sweep, and refuses.
func (b *roomBudget) reserve(room string, weight int64) (*roomReservation, error) {
	b.mu.Lock()
	defer b.unlock()
	entry := b.entry(room)
	if len(entry.charges) > 0 {
		b.dropIfEmptyLocked(room, entry)
		return nil, nil
	}
	if b.used+weight > b.limit {
		b.dropIfEmptyLocked(room, entry)
		b.startSweepLocked()
		return nil, fmt.Errorf("%w: %d of %d bytes are charged, and this document needs %d", errRoomBudgetFull, b.used, b.limit, weight)
	}
	reservation := &roomReservation{room: room, weight: weight}
	entry.reservations = append(entry.reservations, reservation)
	b.used += weight
	return reservation, nil
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

// load charges room's newly loaded instance, of elements elements at one copy, in place of every
// reservation its admissions hold.
func (b *roomBudget) load(room string, elements int64) *roomCharge {
	b.mu.Lock()
	defer b.unlock()
	entry := b.entry(room)
	for _, reservation := range entry.reservations {
		b.settleLocked(reservation)
	}
	entry.reservations = nil
	charge := &roomCharge{budget: b, room: room, elements: elements, copies: 1}
	entry.charges = append(entry.charges, charge)
	b.reweighLocked(charge)
	return charge
}

// took charges the room update bytes more, at two copies once copied: the update observer takes the
// replica, the room's second copy, on the room's first update (renderedReplica.catchUp).
func (c *roomCharge) took(update int, copied bool) {
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.updates += int64(update)
	if copied {
		c.copies = 2
	}
	c.budget.reweighLocked(c)
}

// updatesTaken is the bytes of every update the room has taken since it loaded, which a rendering
// published now holds, so its count, once it lands, re-bases the charge from there (measured).
// Zero for a replica no room's load charged (a test's).
func (c *roomCharge) updatesTaken() int64 {
	if c == nil {
		return 0
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.updates
}

// measured re-bases the room's charge on a count of elements for the rendering published when the
// room had taken updates bytes of updates: the update bytes taken before that rendering are in the
// count, and only those taken since stay beside it. A replica no room's load charged has nothing to
// re-base.
func (c *roomCharge) measured(elements, updates int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.elements = elements
	c.measuredAt = updates
	c.budget.reweighLocked(c)
}

// retire returns the charge of room's oldest loaded instance, which ygo has retired
// (releaseUnloadedRoom). ygo fires one unload for each instance it loaded, and loads the next only
// once the last is out of its map, so the oldest charge is the retired instance's - or, where two
// retire before either unload fires, the other's, which is the same total.
func (b *roomBudget) retire(room string) {
	b.mu.Lock()
	defer b.unlock()
	entry := b.rooms[room]
	if entry == nil || len(entry.charges) == 0 {
		return
	}
	charge := entry.charges[0]
	entry.charges = slices.Delete(entry.charges, 0, 1)
	charge.retired = true
	b.used -= charge.weight
	b.dropIfEmptyLocked(room, entry)
}

// over is whether the charges and reservations pass the limit.
func (b *roomBudget) over() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used > b.limit
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

// kick starts a sweep while the budget is over, unless one runs: an admission past the limit, a
// room that may have become closable while the budget is full (unlockState), a test's new limit.
func (b *roomBudget) kick() {
	b.mu.Lock()
	defer b.unlock()
	if b.used > b.limit {
		b.startSweepLocked()
	}
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
// (unlockState).
func (b *roomBudget) unlock() {
	if b.used <= b.limit {
		b.full.Store(false)
	} else if !b.full.Load() {
		b.startSweepLocked()
	}
	b.mu.Unlock()
}

// startSweepLocked starts a sweep unless one runs or the budget has none. Its caller holds mu.
func (b *roomBudget) startSweepLocked() {
	if b.sweeping || b.sweep == nil {
		return
	}
	b.sweeping = true
	go b.sweep()
}

// swept ends a sweep, raising full when it left the budget over: it closed every room it could.
func (b *roomBudget) swept() {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	weight := roomWeight(charge.elements, charge.copies, charge.updates-charge.measuredAt)
	b.used += weight - charge.weight
	charge.weight = weight
}

// reserveRoom admits a browser's socket to room's document against the room budget, before anything
// loads the document (ServeHTTP). A room already charged admits it with nothing reserved: the
// browser joins a room whose heap is counted. Otherwise it reserves what the stored document would
// cost at both copies, since a browser's first keystroke takes the room's second copy in its update
// observer, where nothing can refuse it. The reservation is made before the load, not when the room
// has loaded: fifty browsers could otherwise pass an unreserved check and each build a whole cold
// copy before any of their rooms exists.
//
// A budget already past its limit refuses at once, without reading the document, and so does one
// this document's reservation would pass; either starts a sweep on a goroutine of its own
// (sweepRooms), never on this one, an HTTP handler whose load already has a documented wedge
// against a writer holding a pooled connection (onLoadDocument), and the next attempt finds what
// the sweep freed. The caller releases the reservation on every way out of its handler.
func (s *Service) reserveRoom(ctx context.Context, room string) (*roomReservation, error) {
	if s.budget.join(room) {
		return nil, nil
	}
	if err := s.budget.refusal(); err != nil {
		return nil, err
	}
	weight, err := s.storedRoomWeight(ctx, room)
	if err != nil {
		return nil, err
	}
	return s.budget.reserve(room, weight)
}

// storedRoomWeight is what room's stored document costs at both copies: its latest version's
// markdown, counted (pmdoc.MeasureDocument). The store holds no count of a document's elements, so
// this is one parse for each socket admitted to a room not yet open, never one for each keystroke.
// One admission counts at a time: the parse holds up to about 130 MiB for a mebibyte of the heaviest
// shape (pmdoc/elements.go:31-37), and a burst of fifty admissions parsing at once would hold the
// heap the budget is there to keep. A document with no version yet weighs nothing.
func (s *Service) storedRoomWeight(ctx context.Context, room string) (int64, error) {
	var markdown string
	err := s.queryFrom(ctx).QueryRow(ctx, `
		select coalesce(markdown, '') from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, room).Scan(&markdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the document's latest version: %w", err)
	}
	select {
	case s.budget.weighing <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-s.budget.weighing }()
	return roomWeight(countElements(markdown), 2, 0), nil
}

// chargeLoadedRoom charges room's newly loaded instance, whose replica has just published its
// load's count (keepReplica), in place of the reservations of the sockets admitted to it, and
// hands the replica the charge its counts re-base from then on (renderedReplica.storeCounted).
func (s *Service) chargeLoadedRoom(room string, replica *renderedReplica) *roomCharge {
	_, elements, _ := replica.size()
	charge := s.budget.load(room, elements)
	replica.sizeMu.Lock()
	replica.charge = charge
	replica.sizeMu.Unlock()
	return charge
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is over, it closes the
// charged rooms that hold nothing outlasting them, the heaviest first, through the eviction a
// failed room takes (evictRoom), whose close flushes the room before it goes. Unused is the
// service's own test (unusedLocked), not the weaker "no browser connected": a room with no browser
// can still hold an open writer, an armed settlement or an update its persistence has not
// appended. The close's unload returns the room's charge (releaseUnloadedRoom). Once nothing more
// can be closed it raises full if the budget is still over (roomBudget.swept).
//
// It runs on a goroutine of its own, as failRoomLocked's eviction does, which Shutdown joins
// (evictWG); a service stopping sweeps nothing, since its Shutdown closes every room itself.
func (s *Service) sweepRooms() {
	defer s.budget.swept()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	defer s.evictWG.Done()
	for _, room := range s.budget.heaviest() {
		if !s.budget.over() {
			return
		}
		state := s.lockExistingState(room)
		if state == nil {
			continue
		}
		if !s.unusedLocked(state) {
			s.unlockState(room, state)
			continue
		}
		// As Evict does: a settlement read under the generation before the close is one the close
		// has invalidated.
		state.roomGeneration++
		s.unlockState(room, state)
		_ = s.evictRoom(room, state)
	}
}
