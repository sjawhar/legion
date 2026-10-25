package docs

import (
	"cmp"
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"
)

// roomBudgetBytes is the most heap the open rooms are charged together (roomCharge): 2 GiB of the
// task's 4 GiB, under the 3.6 GiB Go memory limit the server takes from its container at boot
// (cmd/dispatch/main.go:119-128). Open documents are the largest thing the pod holds; the rest is
// the request path's, the cold reads' cache's (documentReadBudget, 256 MiB, which this does not
// count) and the slack that limit leaves the collector. At the measured cost of a room
// (bytesOfHeapAnElement) it holds thirty-nine documents of a mebibyte of prose (57,600 elements,
// 54 MB each at both copies, roomCopies), read or edited alike, and since an admission reserves its
// stored version's count at that same price (storedRoomReservation), an empty budget admits
// thirty-nine of them and refuses the fortieth. No room has a ceiling: a mebibyte of the densest
// markdown measured, which goldmark stops counting, is priced at more than the whole budget
// (bytesOfHeapAByteAtTheDensest), and what answers a room that heavy is the hook's refusal of
// every insert while nothing can be closed (peer_update.go:44-58), not a cap on its charge
// (TestTheRoomBudgetsArithmetic).
const roomBudgetBytes = 2 << 30

// bytesOfHeapAnElement is the heap one copy of a document holds for each element its rendering
// makes (pmdoc.MeasureDocument): about 470 bytes at 524 KiB and at 1 MiB, 578 at 51 KiB, where
// what every document holds whatever its size is a larger share. A room that is only read holds
// one copy, its live document; a room that has taken an update holds two, the live document and
// the rendered replica its update observer keeps (renderedReplica); the budget charges every room
// at both from its admission on (roomCopies). The heap follows the elements and not the markdown's
// bytes, which misprice a dense document by several times: the figure before this one, 120 bytes
// of heap a byte of markdown, was measured on a shape no room can hold (room_heap_test.go).
// TestAResidentRoomsHeapPerByteOfItsMarkdown measures rooms of each size, read and edited, and
// fails once a copy's heap an element leaves 0.85x to 1.3x of this.
const bytesOfHeapAnElement = 470

// bytesOfHeapAByteAtTheDensest is the densest price: the heap one copy of a document is charged
// for each byte of a rendering pmdoc.MeasureDocument could not finish counting (DocumentSize.Counted
// false, elementsPastCounting), which is the most elements a byte of rendered markdown makes, as
// this repository measures it, at bytesOfHeapAnElement each. That is the one thing it prices
// (copyHeap): MeasureDocument answers such a rendering with no elements at all, and charged as
// that, the heaviest room in the pod would look free. Every rendering it finishes is charged its
// count.
//
// The bytes it prices are always a rendering - a room's (renderedReplica.publishSize), or a stored
// version, which is stored as its rendering (pmdoc/parse.go:120-122) - so what it is measured on is
// the rendering of each dense shape pmdoc's own tests build and of each of this repository's
// markdown files (TestNoMarkdownPricesPastTheDensestPrice), and it is set from the figure at a
// mebibyte, the size of document the budget is there for. The densest there is empty items of
// alternating bullet lists, `- \n\n* \n\n`, each item a list of its own: 2.2501 elements a byte at
// 1 MiB and at 64 KiB alike, which at 470 bytes an element is 1,057.6 bytes of heap a byte,
// rounded up here, so a mebibyte of it is priced at 2,218,786,816 bytes for two copies, more than
// the whole budget. Measured on the markdown a caller sends rather than on its rendering, the same
// shape is 4.5 elements a byte, since its rendering is twice as long. Of the repository's 846
// files in the schema the densest rendering is a fixture of empty list items,
// pmdoc/testdata/corpus/nested-empty-items.md, at 1.37 elements a byte.
//
// A small document can be denser a byte than this: the densest measured overall is a lone empty
// list item, a list, its item and the paragraph the item is read as holding, nine elements in a
// rendering of three bytes, 3.0 a byte. That is acceptable because its absolute cost is small -
// nine elements are about 4 KB of heap whatever their density. Nor is it the most any markdown
// makes at a mebibyte: nested containers, which none of pmdoc's shapes build, render denser,
// `- - \n\n* - \n\n` at 3.0 elements a byte, and a document of them that goldmark stops counting
// is priced under what it holds.
const bytesOfHeapAByteAtTheDensest = 1_058

// errRoomBudgetFull is the refusal of a browser the open rooms leave no room for (reserveRoom). It
// is answered with DocumentBusyCloseCode, the transient close: the next attempt finds the bytes
// the sweep it started has freed for it (wanted), or the rooms open retiring by themselves.
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
// Over the budget, or short of what an admission it refused needs (wanted), the rooms that hold
// nothing outlasting them are closed (sweepRooms). When the budget is still over its limit with
// none left to close, full is raised: the one signal the peer update hook reads (Service.
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
	// wanted is the most an admission the budget refused needed since the last sweep ended
	// (reserve), which the next sweep closes rooms to make room for beside the limit itself
	// (short). Without it a sweep would stop as soon as the budget is back within its limit, and
	// a reservation never takes it past: the sweep a refused admission starts would close nothing,
	// and the next attempt would be refused while unused rooms stayed charged until each retired
	// on its own a minute later.
	wanted int64
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

// roomCharge is one loaded room's charge: its latest measured document at both copies, plus the
// bytes of the updates it has taken since that measurement (weight). Each measurement replaces the
// last rather than adding to it, and the update bytes beside it start again from that measurement:
// a charge that only rose would never fall while browsers stayed, since a Yjs delete tombstones
// what it deletes rather than freeing it. What no rendering shows - those tombstones - is what the
// charge does not see; the room's own retirement a minute after its last use, and the compaction
// that folds its updates as it closes, reclaim it.
//
// What a room is charged has a lifecycle. Its admission reserves the count of its stored version
// at both copies (roomReservation); its load, once the rendering it loaded with is counted
// (keepReplica), replaces that reservation with this charge (load); and each count the measurer
// makes on its cadence after that replaces the charge again (measured). A count that lands clears
// only the update bytes charged up to the rendering it measured, never those charged since, and a
// rendering published without a count of its own (renderedReplica.publishSize) clears none.
type roomCharge struct {
	budget *roomBudget
	room   string
	// heap is what one copy of the room's latest measured document is priced at (copyHeap).
	heap int64
	// updates is the bytes of every update the room has taken since it loaded, and measuredAt what
	// it was at the rendering heap prices.
	updates, measuredAt int64
	weight              int64
	retired             bool
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

// roomCopies is the copies of its document a room is charged at, from the reservation its
// admission makes (storedRoomReservation) through its load (load) to its retirement (retire): its
// live document and the rendered replica its update observer keeps (renderedReplica). A room only
// read holds the first alone, and both are charged before the second exists, because the replica
// is taken by the room's own update observer the first time anyone types (renderedReplica.catchUp),
// inside ygo's apply of an update the peer hook has already admitted, and nothing at that point can
// refuse it. Charged one copy until then, rooms only read would be admitted at half their price:
// an admission of a mebibyte of prose reserves 54,144,000 bytes, its room would then be charged
// 27,072,000, and an empty budget would admit seventy-eight of them rather than thirty-nine; once
// they were typed into, each would take its replica with nothing able to refuse it, and the open
// rooms would come to about twice the 2 GiB budget in a 4 GiB pod.
const roomCopies = 2

// roomWeight is what a room is charged whose one copy holds heap (copyHeap), at roomCopies, and
// which has taken updates bytes of updates since heap was measured.
func roomWeight(heap, updates int64) int64 {
	return heap*roomCopies + updates
}

// copyHeap is the heap one copy of a document is priced at whose rendering is bytes long and makes
// elements elements (renderedReplica.size): its count at bytesOfHeapAnElement, or, for a rendering
// goldmark stopped counting (elementsPastCounting), its bytes at the densest price. Nothing caps
// it: a room is charged what its document holds, however heavy. A room that renders no markdown
// (sizeUnmeasured) is priced at nothing, and charged its update bytes alone.
func copyHeap(bytes, elements int64) int64 {
	switch {
	case bytes == sizeUnmeasured || elements == sizeUnmeasured:
		return 0
	case elements == elementsPastCounting:
		return bytes * bytesOfHeapAByteAtTheDensest
	default:
		return elements * bytesOfHeapAnElement
	}
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
// charged, which the socket joins. Where weight would take the budget past its limit it reserves
// nothing, records weight as what the sweep it starts is to make room for (wanted), and refuses:
// the admission that found the budget full is refused at once, and the sweep frees the bytes for
// its next attempt, which the editor's backoff on DocumentBusyCloseCode retries into. A weight past
// the limit itself is not recorded, since no sweep can make room for it, and closing every unused
// room to try would only cost their next readers a load.
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
		if weight <= b.limit {
			b.wanted = max(b.wanted, weight)
		}
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

// load charges room's newly loaded instance, whose one copy is priced at heap (copyHeap) from the
// count of the rendering it loaded with, at roomCopies, in place of every reservation its
// admissions hold: the room's own document replaces its stored version at the same two copies, and
// the two documents differ only by the updates stored since that version was cut.
func (b *roomBudget) load(room string, heap int64) *roomCharge {
	b.mu.Lock()
	defer b.unlock()
	entry := b.entry(room)
	for _, reservation := range entry.reservations {
		b.settleLocked(reservation)
	}
	entry.reservations = nil
	charge := &roomCharge{budget: b, room: room, heap: heap}
	entry.charges = append(entry.charges, charge)
	b.reweighLocked(charge)
	return charge
}

// took charges the room update bytes more. Its second copy is charged from its load on
// (roomCopies), so the update that makes the replica adds only its own bytes.
func (c *roomCharge) took(update int) {
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.updates += int64(update)
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

// measured re-bases the room's charge on heap (copyHeap), the price of a count of the rendering
// published when the room had taken updates bytes of updates: the update bytes taken before that
// rendering are in the count, and only those taken since stay beside it. Only a count lands here
// (renderedReplica.storeCounted); a rendering whose bytes alone are published re-bases nothing. A
// replica no room's load charged has nothing to re-base.
func (c *roomCharge) measured(heap, updates int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.heap = heap
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

// short is whether the charges and reservations leave no room for wanted, the most an admission
// the budget refused needed since the last sweep ended: past the limit, or within it but without
// room for that admission. A sweep closes rooms while it holds.
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

// swept ends a sweep, raising full when it left the budget over its limit: it closed every room
// it could. A sweep that leaves the budget within its limit but short of wanted lowers full
// rather than raising it. Full refuses every insert in every room (Service.weighPeerUpdate), and
// a single oversized document refused on an almost empty pod must not freeze every room's growth;
// a refused admission is answered by its own refusal, and by whatever room the sweep could make
// for it.
//
// wanted starts again from nothing: the admissions refused since this sweep started have had what
// room it could make for the most any of them needed, and one refused after its last look at the
// budget, where nothing else has freed room by its next attempt, is refused again then, which
// records its weight once more and starts a sweep of its own.
func (b *roomBudget) swept() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweeping = false
	b.wanted = 0
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
	weight := roomWeight(charge.heap, charge.updates-charge.measuredAt)
	b.used += weight - charge.weight
	charge.weight = weight
}

// reserveRoom admits a browser's socket to room's document against the room budget, before anything
// loads the document (ServeHTTP). A room already charged admits it with nothing reserved: the
// browser joins a room whose heap is counted. Otherwise it reserves what the stored document will
// cost at both copies (storedRoomReservation, roomCopies), since a browser's first keystroke takes
// the room's second copy in its update observer, where nothing can refuse it. The reservation is
// made before the load, not when the room has loaded: fifty browsers could otherwise pass an
// unreserved check and each build a whole cold copy before any of their rooms exists.
//
// A budget already past its limit refuses at once, without reading the document, and so does one
// this document's reservation would pass; either starts a sweep on a goroutine of its own
// (sweepRooms), never on this one, an HTTP handler whose load already has a documented wedge
// against a writer holding a pooled connection (onLoadDocument), and the next attempt finds what
// the sweep freed: room for this document's reservation where unused rooms can make it (reserve).
// The caller releases the reservation on every way out of its handler.
func (s *Service) reserveRoom(ctx context.Context, room string) (*roomReservation, error) {
	if s.budget.join(room) {
		return nil, nil
	}
	if err := s.budget.refusal(); err != nil {
		return nil, err
	}
	weight, err := s.storedRoomReservation(ctx, room)
	if err != nil {
		return nil, err
	}
	return s.budget.reserve(room, weight)
}

// storedRoomReservation is what an admission to room reserves: its latest version's stored
// markdown, counted (pmdoc.MeasureDocument) and priced as a loaded room's rendering is (copyHeap),
// at both copies. A version is stored as its rendering (pmdoc/parse.go:120-122), so this is the
// count the room's load makes of the rendering it loads with (keepReplica) wherever no update has
// been stored since that version, and that count replaces this reservation (roomBudget.load). So
// a mebibyte of prose reserves about the 54 MB it costs once counted
// (TestAnAdmissionReservesTheCountOfItsStoredVersion), where its bytes at the densest price would
// reserve 2.2 GB, past the whole budget, and refuse it on an empty pod. Only markdown
// MeasureDocument cannot finish is priced from its bytes (bytesOfHeapAByteAtTheDensest). A
// document with no version yet reserves nothing.
//
// The count is a whole goldmark parse: about 89 ms for a mebibyte, and up to about 130 MiB of heap
// for a mebibyte of the heaviest shape (pmdoc/elements.go:31-37). It is paid on the path a person
// reaches when they open a document, once for each version in each process, since the count is
// kept (storedCounts) - never on a keystroke, and never inside a room's update observer. The
// admissions that miss one version at once share one count, the others waiting for the one that
// counts, and counts of different documents run one at a time (storedCounts.counting), so a burst
// of admissions holds one parse's heap rather than one for each. Only the latest version's number
// is read on every admission; its markdown is read only to count it.
func (s *Service) storedRoomReservation(ctx context.Context, room string) (int64, error) {
	var number int64
	err := s.queryFrom(ctx).QueryRow(ctx, `
		select number from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, room).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the number of the document's latest version: %w", err)
	}
	if heap, ok := s.budget.stored.get(room, number); ok {
		return roomWeight(heap, 0), nil
	}
	flight := s.budget.stored.flights.DoChan(room+"@"+strconv.FormatInt(number, 10), func() (any, error) {
		return s.countStoredVersion(ctx, room, number)
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			return 0, result.Err
		}
		return roomWeight(result.Val.(int64), 0), nil
	}
}

// countStoredVersion is the price of one copy of room's version number (copyHeap), counted from its
// stored markdown and kept (storedCounts.put), unless an admission that started its count before
// this one's miss has kept it already. It runs once for each version however many admissions miss
// it at once (storedRoomReservation), so it belongs to the version rather than to the admission
// that started it: it reads on a context no admission's end cancels, bounded as a cold read's fold
// is (coldReadTimeout), and through the pool rather than any caller's transaction, and an
// admission whose request ends returns at once and leaves the count to the others. Its error is
// the read's, or a panic recovered from the parse: singleflight re-panics an unrecovered panic of
// a call it has waiters for in a goroutine nothing can recover, as foldDocument says.
func (s *Service) countStoredVersion(ctx context.Context, room string, number int64) (heap int64, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("counting the stored document panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: counting a stored document for its admission panicked", "room", room, "error", err)
		}
	}()
	counts := &s.budget.stored
	if heap, ok := counts.get(room, number); ok {
		return heap, nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coldReadTimeout)
	defer cancel()
	var markdown string
	if err := s.store.Pool.QueryRow(readCtx, `
		select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2
	`, room, number).Scan(&markdown); err != nil {
		return 0, fmt.Errorf("read the document's version %d: %w", number, err)
	}
	counts.counting.Lock()
	defer counts.counting.Unlock()
	if counts.beforeCount != nil {
		counts.beforeCount(room, number)
	}
	heap = copyHeap(int64(len(markdown)), countElements(markdown))
	counts.put(&storedCount{room: room, number: number, heap: heap})
	return heap, nil
}

// storedCountsHeld is the most documents whose stored version's count is kept (storedCounts). An
// entry is an id and two integers, a few hundred bytes with its list element and its map slot, so
// 4,096 of them hold about a mebibyte, and counting entries bounds their weight: the cold reads'
// cache weighs each of its entries (documentRead.weight) because they run from bytes to
// megabytes, and these do not vary. A document the bound has dropped costs its next admission one
// parse again, nothing more.
const storedCountsHeld = 4_096

// storedCounts keeps, for each document an admission counted, the price of one copy of its latest
// stored version (countStoredVersion), the most recently admitted first, for at most
// storedCountsHeld documents. It is shaped as the cold reads' cache is (documentReads,
// read_cache.go:82-142): one mutex, a map into a list kept in the order of use, one entry for a
// document, a newer version's count replacing an older one's, and a singleflight group, so that
// the admissions missing one version at once share one count rather than each parsing it.
//
// Unlike that cache's renderings, a count never goes stale, and needs no stamp: a version's
// markdown is written once, by the insert that makes the version (api/artifacts.go:296,
// api/issue_create.go:349), and no running server rewrites it - the other writes to
// artifact_versions move a file's content (files/backfill.go:177,215) or are migrations, which run
// before it serves - so the count of a document's version number is that version's for as long as
// it stands. An entry is used only for the version the store names latest at the admission.
type storedCounts struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	// flights counts each (document, version) once however many admissions miss it at once.
	flights singleflight.Group
	// counting lets one count run at a time, of whichever documents.
	counting sync.Mutex
	// beforeCount runs in a count once it has read the version's markdown and before it parses it.
	// Nil outside tests; tests use it to count the parses and to hold one while admissions arrive.
	beforeCount func(room string, number int64)
}

// storedCount is the price of one copy of room's version number (copyHeap).
type storedCount struct {
	room         string
	number, heap int64
}

// get is the price of one copy of room's version number when it is kept, which it marks the most
// recently used.
func (c *storedCounts) get(room string, number int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[room]
	if !ok {
		return 0, false
	}
	count := element.Value.(*storedCount)
	if count.number != number {
		return 0, false
	}
	c.order.MoveToFront(element)
	return count.heap, true
}

// put keeps count as its document's, in place of a count of an older version, then drops the least
// recently used past storedCountsHeld. A count of an older version than the one kept is dropped
// instead: a slow count must not put back a version the store has moved past.
func (c *storedCounts) put(count *storedCount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries, c.order = make(map[string]*list.Element), list.New()
	}
	if element, ok := c.entries[count.room]; ok {
		if element.Value.(*storedCount).number > count.number {
			return
		}
		c.order.Remove(element)
	}
	c.entries[count.room] = c.order.PushFront(count)
	if c.order.Len() > storedCountsHeld {
		delete(c.entries, c.order.Remove(c.order.Back()).(*storedCount).room)
	}
}

// chargeLoadedRoom charges room's newly loaded instance, whose replica has just published its
// load's count (keepReplica), in place of the reservations of the sockets admitted to it, and
// hands the replica the charge its counts re-base from then on (renderedReplica.storeCounted).
func (s *Service) chargeLoadedRoom(room string, replica *renderedReplica) *roomCharge {
	bytes, elements, _ := replica.size()
	charge := s.budget.load(room, copyHeap(bytes, elements))
	replica.sizeMu.Lock()
	replica.charge = charge
	replica.sizeMu.Unlock()
	return charge
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is short (roomBudget.short)
// - over its limit, or without room for the most an admission it refused needed since the last
// sweep (wanted) - it closes the charged rooms that hold nothing outlasting them, the heaviest
// first, so as few as it can, through the eviction a failed room takes (evictRoom), whose close
// flushes the room before it goes. Unused is the service's own test (unusedLocked), not the weaker
// "no browser connected": a room with no browser can still hold an open writer, an armed
// settlement or an update its persistence has not appended. The close's unload returns the room's
// charge (releaseUnloadedRoom), and ygo's CloseRoom fires that unload before it returns wherever
// it is the one that removed the room (sjawhar/ygo v1.51.3-sami.2,
// provider/websocket/inject.go:604-634), so the next look at the budget sees the charge gone. Each
// close is logged with the budget's own figures before and after it, which is how an operator, or
// a test of the real process, tells a room the budget closed from one that retired on its own.
// Once nothing more can be closed it raises full if the budget is still over its limit
// (roomBudget.swept).
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
		if !s.budget.short() {
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
		before, _, _ := s.budget.figures()
		_ = s.evictRoom(room, state)
		after, limit, wanted := s.budget.figures()
		slog.Info("dispatch: the room budget closed an unused document",
			"room", room, "charged_before", before, "charged", after, "limit", limit, "making_room_for", wanted)
	}
}
