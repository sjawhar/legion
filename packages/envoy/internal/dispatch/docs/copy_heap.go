package docs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/persistence"
	"golang.org/x/sync/semaphore"
)

// The pod's load concurrency holds how many whole-document copies it builds at once. It is k
// fixed slots of loadSlotBytes each (MemoryBounds.LoadSlots), one slot whatever the document
// weighs, and three kinds of work take one.
//
// The first is one whole-document decode, held while it decodes: a room's load (its fold of a
// history of several updates, the copy LoadDoc decodes, the load hook's decode into the room,
// bootstrapLoadedState), the fold of a cold read, a version read or a compaction (foldRead), the
// replica's build from a base (renderedReplica.start), the copy a server write's decode-cap count
// takes (structsAfter) and a rebuild's decodes (its check of a resident room and of the stored
// history, nearACap, rebuildSource, RebuildTx). What each leaves behind is its owner's: a room's
// two copies are charged by the room budget, a fold's state goes back to its caller.
//
// The second is a live write's fork, held for the fork's whole life rather than for a decode
// alone: from the moment before the fork is built - cloned from the room's replica (warmFork,
// Service.forkPoint) or decoded from the store or from the copy a read took of a room whose
// replica held no state (coldFork, rebuildFork, buildForkHeld) - through the trees its
// operations read off it and the markdown they render, until the fork is dropped
// (liveWrite.releaseFork). A write never gives its slot back while it holds a fork, and the
// decodes it makes inside the slot it already holds take no second one (withLoadSlot, holdLoad).
//
// The third is one apply of a browser's update over unslottedApplyBytes, held across the apply
// (Service.weighPeerUpdate): a reconnect storm's whole-state handoffs then decode k at a time
// rather than all at once, while a keystroke, far under that size, never waits for a slot.
//
// Every taker takes its slot before it takes a database connection, so a slot is never held
// behind a pool a holder of another slot could fill, and a wait a request's context ends is
// answered errWriteBusy, the API's 503 DOC_SERVICE_UNAVAILABLE. The copy a read of a resident
// room takes while the room's replica is busy (holdLive, snapshotDocument) takes no slot: its
// callers hand it no request context to bound a wait by.

// unslottedApplyBytes is the update's own bytes at or under which an apply of a browser's update
// takes no slot: 2 MiB, the figure the apply-heap bound used before the cut, over thirty times
// the editor's 64 KiB paste and far past any keystroke, so typing never waits on a reconnect
// storm's handoffs. A whole-state handoff is above it and waits
// (dispatch://INFRA-179/artifact/infra179-simplify-measured-md).
const unslottedApplyBytes = 2 << 20

// loadSlotsName is what the slots are logged as (logLoadSlot).
const loadSlotsName = "load"

// loadSlots is the pod's load concurrency: MemoryBounds.LoadSlots slots, one of which covers a
// whole-document decode, a live write's fork or a heavy apply (loadSlotBytes). Waiters are served
// in the order they came (semaphore.Weighted), so no holder is passed over by one behind it.
//
// It cannot deadlock, because every holder of a slot finishes and gives it back: it waits for
// nothing another holder or a waiter can hold for good, and it takes no second slot.
//
// A decode's slot, held between its take and its release, decodes, renders or reads the rows of
// the transaction it already holds, takes no lock held across a wait, takes no pooled connection
// - a fold covers the slot holding its own transaction's connection alone, or, once that
// transaction has ended, none (foldRead), and a room's load gives its slot back before
// onLoadDocument reads the store - and runs no update observer, since ygo decodes nothing of a
// loading room's state and no holder commits on a resident room.
//
// A live write's slot is held far longer - from before its fork is built until its fork is
// dropped - so the things it holds it across are named here too, each with what keeps that wait
// from being a cycle:
//
//   - its own transaction, its owner row and the document's advisory lock, all taken before the
//     slot, and its writer slot, also taken before it (openLiveWrite): a waiter for a writer slot
//     holds no load slot, since it has not forked yet.
//   - the room replica's lock, waited for to clone the replica (warmFork). Nothing under that
//     lock waits for a slot: the one decode there takes a free slot at once and otherwise runs
//     beside the slots (renderedReplica.start, holdFreeLoad).
//   - the publish of its own committed updates, which may load a room and so take a slot of that
//     load's own. The transaction drops every fork, and with them its slot, before it publishes
//     any of them (Ledger.publish).
//
// No holder takes a second slot: a decode under a context carrying one covers itself on that one
// (withLoadSlot), so a room load's fold, its copy's decode and the load hook's decode into the
// room share the load's, and a write's fork decode and its decode-cap copy share the write's;
// and one transaction's live writes share one slot (forkSlot), so a transaction writing two
// documents never waits for the slot its own other fork holds.
//
// A waiter may hold anything - a write's transaction, its owner row and advisory lock, a
// compaction's advisory lock and connection, a connection of the rooms pool or of the shared
// pool, a slot of a buffered fold (foldSlots), a replica's lock, a loading room's ready barrier -
// since no holder waits for any of them, and so every holder finishes and gives its slot back. A
// waiter whose request ends returns its context's error holding nothing.
type loadSlots struct {
	// size is how many slots there are, always set by whoever built it (newLoadSlots): the
	// process's MemoryBounds.LoadSlots, or a test's.
	size int64
	once sync.Once
	free *semaphore.Weighted
	// held is how many slots are held now.
	held atomic.Int64
	// waiting runs, in a test, as a taker finds no slot free and starts to wait; taken runs once
	// it holds one, with how many are held then.
	waiting func()
	taken   func(held int64)
}

// newLoadSlots is a load concurrency of slots slots, at least one: a pod with no slot could load
// no document at all.
func newLoadSlots(slots int) *loadSlots {
	return &loadSlots{size: int64(max(slots, 1))}
}

// podLoadSlots is the process's load concurrency, shared by every service and store in it as the
// memory it bounds is: the calibration's until a service built at another Go memory limit sizes
// it from that limit (New), and a test swaps in one of its own (useLoadSlots).
var podLoadSlots atomic.Pointer[loadSlots]

func init() { podLoadSlots.Store(newLoadSlots(calibratedBounds.LoadSlots)) }

// semaphore is the slots themselves, made on first use.
func (h *loadSlots) semaphore() *semaphore.Weighted {
	h.once.Do(func() { h.free = semaphore.NewWeighted(h.size) })
	return h.free
}

// loadSlot is one holder's slot: held, or nothing once released. One holder uses it at a time:
// the goroutine whose decodes it covers, or for a room's load the load's record, whose lock it is
// given back under (roomLoad.take).
type loadSlot struct {
	slots *loadSlots
	held  bool
	// waited is how long it waited for its slot, and since when it has held it, both for its
	// release's log line (logLoadSlot).
	waited time.Duration
	since  time.Time
}

// newLoadSlot is a holder of the pod's load concurrency holding nothing.
func newLoadSlot() *loadSlot {
	return &loadSlot{slots: podLoadSlots.Load()}
}

// take makes g hold a slot, waiting within ctx. A holder that already holds one holds it. A wait
// ctx ends returns ctx's error and leaves g holding nothing.
func (g *loadSlot) take(ctx context.Context) error {
	if g.held {
		return nil
	}
	slots := g.slots.semaphore()
	started := time.Now()
	if !slots.TryAcquire(1) {
		if g.slots.waiting != nil {
			g.slots.waiting()
		}
		if err := slots.Acquire(ctx, 1); err != nil {
			return err
		}
	}
	g.held, g.since = true, time.Now()
	g.waited = g.since.Sub(started)
	held := g.slots.held.Add(1)
	if g.slots.taken != nil {
		g.slots.taken(held)
	}
	return nil
}

// release gives g's slot back, and logs it where it waited long (logLoadSlot). A holder of
// nothing gives back nothing.
func (g *loadSlot) release() {
	if g == nil || !g.held {
		return
	}
	g.slots.held.Add(-1)
	g.slots.semaphore().Release(1)
	logLoadSlot(g.slots.size, g.waited, time.Since(g.since))
	g.held = false
}

// tryTake makes g, which holds nothing, hold a slot when one is free at once, and reports whether
// it does. It never waits, so a decode that may not wait still runs under a slot wherever one is
// free (holdFreeLoad), and holds none when it reports false.
func (g *loadSlot) tryTake() bool {
	if g.held {
		return true
	}
	if !g.slots.semaphore().TryAcquire(1) {
		return false
	}
	g.held, g.since, g.waited = true, time.Now(), 0
	held := g.slots.held.Add(1)
	if g.slots.taken != nil {
		g.slots.taken(held)
	}
	return true
}

// loadSlotKey is the context key a slot travels under (carriedBy).
type loadSlotKey struct{}

// carriedBy is ctx carrying g, so a decode under it covers itself on g (holdLoad) rather than
// take a slot of its own inside g's.
func (g *loadSlot) carriedBy(ctx context.Context) context.Context {
	return context.WithValue(ctx, loadSlotKey{}, g)
}

// withLoadSlot is ctx carrying a slot, and the slot: the one ctx already carries, which its owner
// releases, with a done that releases nothing, or a new one, with a done that releases it. A
// decode under ctx covers itself on that slot (holdLoad), so the decodes one holder makes in turn
// share one slot rather than wait for a second inside the first.
func withLoadSlot(ctx context.Context) (context.Context, *loadSlot, func()) {
	if slot, ok := ctx.Value(loadSlotKey{}).(*loadSlot); ok {
		return ctx, slot, func() {}
	}
	slot := newLoadSlot()
	return slot.carriedBy(ctx), slot, slot.release
}

// holdLoad covers a whole-document decode on the slot ctx carries, or on one of its own, and
// returns what ends the hold: nothing for a slot ctx carries, whose owner releases it, and the
// release of its own.
func holdLoad(ctx context.Context) (func(), error) {
	ctx, slot, done := withLoadSlot(ctx)
	if err := slot.take(ctx); err != nil {
		return nil, err
	}
	return done, nil
}

// loadedHistory is a document's stored history, read and decoded under one slot of the pod's load
// concurrency (loadDecodedHistory).
type loadedHistory struct {
	// loaded is what the read returned: the history's bytes and the version they stand at.
	loaded persistence.LoadResult
	// decoded is the decode of those bytes, as a room's load decodes them (decodeStoredCopy),
	// and nothing where refused is not nil. Its doc is nil for an empty history, which decodes
	// into nothing.
	decoded decodedState
	// refused is ygo's refusal of the history, the document's own failure rather than the
	// service's, and nil for one that decoded: a history no room can load, which each reader
	// answers its own way (loadTree's ErrDocumentUnloadable, rebuildSource's reason to rebuild,
	// LoadDoc's failure of the load).
	refused error
}

// loadDecodedHistory reads artifactID's stored history through load and decodes it, both covered
// by one slot of the pod's load concurrency: the slot ctx carries, whose owner gives it back, or
// one of its own, which it gives back before it returns, so no caller releases a slot by hand
// (withLoadSlot). It is the one way the service reads a history it decodes - a socket's
// admission (loadTree), a room's load (loadDecoded) and a rebuild's source (rebuildSource) - so
// that no new way out of one of them can leave a slot held. It decodes under maxPending, the
// pending queue the rooms of the service that reads decode under (Service.maxPendingItems).
//
// refuse, where it is not nil, is read from the history before the slot is taken and before
// anything is decoded (loadableHistory), for a history no decode should be paid for.
//
// The read's fold, when the history holds several updates, covers the slot where it reads them
// (foldRead), and the decode runs inside it. The error is the read's, the refusal's or the
// slot's, a wait ctx ended included, and leaves nothing decoded; ygo's refusal of the bytes
// themselves is the history's refused.
func loadDecodedHistory(ctx context.Context, load VersionedStore, artifactID string, maxPending int, refuse func(update []byte) error) (loadedHistory, error) {
	ctx, slot, done := withLoadSlot(ctx)
	defer done()
	loaded, err := load.Load(ctx, artifactID)
	if err == nil && refuse != nil {
		err = refuse(loaded.Update)
	}
	if err == nil {
		err = slot.take(ctx)
	}
	if err != nil {
		return loadedHistory{}, err
	}
	decoded, refused := decodeStoredCopy(loaded.Update, maxPending)
	return loadedHistory{loaded: loaded, decoded: decoded, refused: refused}, nil
}

// holdServerLoad is holdLoad for a decode of the server's own that no request waits on: a write's
// fork read outside a transaction, a server write's decode-cap copy, the copy of a room loaded
// without this service's persistence. Its wait has no deadline, and ends, since every holder
// gives its slot back (loadSlots).
//
// It takes ctx for the slot ctx carries alone, never for its deadline: a decode made inside a
// live write - its fork's own (buildForkHeld) and the decode-cap copy of the room it was read
// from (structsAfter) - covers the transaction's slot, so it waits for nothing and the
// transaction holds one slot from its first fork's build to its last fork's release. A caller
// that carries none - the three server writes outside applyLive, a socket's own copy - takes a
// slot of its own and waits for it without a deadline, as this always did, and is never refused.
func holdServerLoad(ctx context.Context) func() {
	release, _ := holdLoad(context.WithoutCancel(ctx))
	return release
}

// holdFreeLoad covers a whole-document decode on a slot that is free at once, and runs beside the
// slots' holders where none is. It is what the one decode that may not wait takes: the replica's
// build from a base (renderedReplica.start), which runs under the replica's lock. A live write
// holds a slot for its fork's whole life and waits for that lock to clone the replica (warmFork),
// so a wait here would hold that write and that replica against each other, each waiting for what
// the other holds.
//
// What such a decode holds outside the slots is one room's own second copy and the decoder above
// it (roomCopies), which the room budget charges where the replica keeps it; and a room builds
// its replica from a base only after a recover or a load that handed it no copy, never while it
// is typed into.
func holdFreeLoad() func() {
	slot := newLoadSlot()
	if !slot.tryTake() {
		return func() {}
	}
	return slot.release
}

// loadSlotLogged is how long a holder of a load slot may have waited for it before its release is
// logged (logLoadSlot): a tenth of a second, past any wait on a lock or the scheduler and short
// of the second or more a heavy apply or decode takes, so the line names the turns a burst of
// heavy work made a holder wait for and stays quiet while the slots are not crowded.
const loadSlotLogged = 100 * time.Millisecond

// logLoadSlot logs a slot of the pod's load concurrency as it is given back, where it waited
// loadSlotLogged or longer for it: how long it waited, how long it held it, and how many slots
// there are, the line's time being when it gave the slot back. An operator reads from it how long
// a burst of heavy work made each holder wait its turn; a real-process drive reads from it which
// holders ran at once and the longest wait (cmd/dispatch,
// TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory).
func logLoadSlot(slots int64, waited, held time.Duration) {
	if waited < loadSlotLogged {
		return
	}
	slog.Info("dispatch: a load slot was given back",
		"bound", loadSlotsName, "slots", slots, "waited", waited, "held", held)
}
