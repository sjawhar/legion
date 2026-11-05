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

// The copy-heap bound holds what the pod's whole-document copies hold live at once. Two kinds
// take it.
//
// The first is one full-state decode, held while it decodes: a room's load (its fold of a history
// of several updates, the copy LoadDoc or a socket's admission decodes, the load hook's decode
// into the room, bootstrapLoadedState), the fold of a cold read, a version read or a compaction
// (foldRead), the replica's build from a base (renderedReplica.start), the measurer's copy for a
// server write (structsAfter) and a rebuild's decodes (its check of a resident room and of the
// stored history, nearACap, rebuildSource, RebuildTx). Each holds copyHeapWeight of its encoded
// state out of the bound (MemoryBounds.CopyHeap, podCopyHeaps) while it decodes. What each leaves
// behind is its owner's: a room's two copies are charged by the room budget, a fold's state goes
// back to its caller.
//
// The second is a live write's fork, held for the fork's whole life rather than for a decode
// alone: from the moment before the fork is built - cloned from the room's replica (warmFork,
// Service.forkPoint) or decoded from the store or from the copy a read took of a room whose
// replica held no state (coldFork, rebuildFork, buildForkHeld) - through the trees its
// operations read off it and the markdown they render, until the fork is dropped
// (liveWrite.releaseFork). It holds liveWriteWeight of the state it was built from, which is
// more than any decode inside it is granted, so the fork's own decode and the decode-cap copy of
// the room the fork was read from (structsAfter) cover that one grant and ask for nothing more
// (withCopyGrant, holdCopy): a write never gives its part of the bound back while it holds a
// fork. Before this the grant covered the decode alone and the fork a write kept through its
// tree build and its render was bounded by nothing, and the clone that replaced that decode for
// the usual warm fork took no grant at all (INFRA-179).
//
// The copy a read of a resident room takes while the room's replica is busy (holdLive,
// snapshotDocument) takes no grant: its callers hand it no request context to bound a wait by.
//
// One holder of the bound decodes nothing: the count of an admission's stored version
// (countStoredVersion), whose goldmark parse of the version's markdown and walk of the stored
// history hold heap of the same pod while they run, and which takes its grant at
// storedCountWeight of the bytes it reads rather than at copyHeapWeight (storedCountHeapPerByte).

// copyHeapPerByte is the heap one full-state decode is granted for each byte of the encoded state
// it decodes (copyHeapWeight). It is what the densest shapes a decode can meet hold live above
// what each decode leaves behind, a fifth more than the most measured. Measured on 2026-10-28 by
// TestEveryFullStateDecodesTransientHeapIsWithinItsGrant (GOMAXPROCS=1, the collector at GOGC=10
// so the sampled peak is what is live; the heavy shapes in two runs, the prose in four), the most
// a byte of state held was 40.2 bytes, 320,762,656 for a cold read's fold of the 7,983,814-byte
// state of a run of a million keystrokes scanned whole, whose room load held 303,430,664, 38.0 a
// byte; a run of a million keystrokes not scanned held up to 35.7 a byte (its room load,
// 285,393,720 bytes). Such a run is the densest a byte: each keystroke is an item of its own as it
// decodes, with its scan mark, until the decode's transaction merges the run into one item, so
// what the decode held goes and almost nothing stays.
// At the decode cap the document an agent rewrote whole (12,556,794 bytes) held up to 25.0 a byte
// in the measurer's copy, 314,476,736 bytes, and 13.2 in a socket's load, and the long-edited
// document (14,427,285 bytes) 19.6 and 9.9; a mebibyte of prose (3,035,240 bytes) held up to 17.6,
// in the replica's build. One path's figure moved by up to a fifth from one run to the other.
const copyHeapPerByte = 48

// copyHeapName is what the copy-heap bound's grants are logged as (logHeapGrant, weighedHeaps).
// The bound itself is the pod's (podCopyHeaps), sized from MemoryBounds.CopyHeap, and what every
// load of a document at or near the decode cap measured holds above what it leaves fits inside
// it: see that field.
const copyHeapName = "copy"

// storedCountHeapPerByte is the heap a count of a stored version is granted for each byte of the
// version's markdown (storedCountWeight): the bytes themselves, read into a string the count holds
// until it returns, and goldmark's tree over them, which is what a count of elements costs
// (countElements, pmdoc.MeasureDocument). It is a fifth more than the most measured.
//
// Measured on 2026-10-29 by TestAStoredVersionsCountHoldsNoMoreThanItsGrant (GOMAXPROCS=1, the
// collector at GOGC=10 so the sampled peak is what is live), what one count of a document whose
// stored history is one small update held: 134.1 bytes a byte of its markdown for 4 KiB of the
// densest markdown this repository measures, empty items of alternating bullet lists
// (bytesOfHeapAByteAtTheDensest), 549,224 bytes in all; 117.4 for 64 KiB of it, 7,692,712 bytes;
// 48.4 for a mebibyte of it; and 62.0 for a mebibyte of hard breaks, the shape goldmark's node
// guard stops latest (pmdoc's nodeGuard). A mebibyte of prose held 13.0 a byte,
// the walk of its 2,957,640-byte state included. The small shapes run denser a byte because the
// guard stops the parse of a large one: it is the whole parse, not the byte, that the guard
// bounds, so a count of a whole document never holds more than storedCountHeapAtMost whatever its
// markdown spells.
const storedCountHeapPerByte = 160

// storedCountHeapAtMost caps what a count's markdown is granted (storedCountWeight), since
// goldmark stops building nodes at the guard (pmdoc's nodeGuard) and a stored
// version's markdown is at most pmdoc.MaxDocumentBytes: no count of a mebibyte measured held more
// than 65,025,848 bytes, a mebibyte of hard breaks, where the densest markdown held 50,703,568 and
// a mebibyte of prose 13,607,960 (TestAStoredVersionsCountHoldsNoMoreThanItsGrant). It is about a
// quarter more than that, so a mebibyte is granted 81,788,928 bytes rather than the 167,737,600
// its bytes alone would ask for, and four counts of a mebibyte run at once inside the copy-heap
// bound (MemoryBounds.CopyHeap) rather than two.
const storedCountHeapAtMost = 78 << 20

// storedCountHeapPerStoredRowByte is the heap a count is granted for each byte of the largest
// update row of the stored history it walks (storedCountWeight, storedStructs). The walk reads one
// row at a time and allocates nothing for the structs it counts (walkUpdate), so what it holds is
// the row pgx scanned and the buffer pgx read it into. Measured the same way, a walk of one
// 3,035,240-byte row, the state of a mebibyte of prose, peaked at 2.39 bytes a byte of it, and one
// of 1,183,765 bytes, a paragraph typed into 100,000 times before its last character, at 2.78. It
// is rounded up past a fifth more than the most of those.
const storedCountHeapPerStoredRowByte = 4

// storedCountWeight is what a count of a stored version is granted of the copy-heap bound
// (countStoredVersion): its markdown's bytes at storedCountHeapPerByte, which the parse's own
// ceiling caps (storedCountHeapAtMost), and the largest row of the history it walks at
// storedCountHeapPerStoredRowByte. The two are added rather than taken at the larger, because the
// count holds its markdown while it walks.
func storedCountWeight(markdown, row int64) int64 {
	return min(markdown*storedCountHeapPerByte, storedCountHeapAtMost) + row*storedCountHeapPerStoredRowByte
}

// weighedHeaps is a bound on the estimated heap of one kind of work, held in weighed parts by its
// holders (heapGrant). There are two: the pod's bound on what its full-state decodes hold
// (podCopyHeaps, MemoryBounds.CopyHeap) and the service's on the estimated heap of its weighed
// applies of browsers' updates (Service.applyHeaps, MemoryBounds.ApplyHeap). Waiters are served
// in the order they came (semaphore.Weighted), so a holder waiting for the whole bound is not
// passed over by lighter ones behind it, and every one after it waits until it has run; a weight
// past the whole bound waits for all of it and runs alone, refused by neither bound
// (heapGrant.coverWeight).
//
// The copy-heap bound cannot deadlock, because every holder of a part of it finishes and gives
// that part back: it waits for nothing another holder or a waiter can hold for good, and it
// acquires no second grant.
//
// A decode's grant, held between its cover and its release, decodes, renders, counts or reads
// the rows of the transaction it already holds, takes no lock held across a wait, takes no
// pooled connection - a fold covers its grant holding its own transaction's connection alone,
// or, once that transaction has ended, none (foldRead), a count of a stored version opens its
// own transaction and weighs itself in it before it covers (countStoredVersion), and a room's
// load gives its grant back before onLoadDocument reads the store - and runs no update
// observer, since ygo decodes nothing of a loading room's state and no holder commits on a
// resident room.
//
// A live write's grant is held far longer - from before its fork is built until its fork is
// dropped (liveWriteWeight) - so the things it holds it across are named here too, each with
// what keeps that wait from being a cycle:
//
//   - its own transaction, its owner row and the document's advisory lock, all taken before the
//     grant, and its writer slot, also taken before it (openLiveWrite): a waiter for a slot
//     holds no grant, since it has not forked yet.
//   - the measurer's turn, waited for to count what the write would leave the room near the
//     decode cap (leavesPastDecodeCap). No holder of that turn waits for the bound: a count of
//     a server write's own covers its grant before it takes the turn, and a live write's covers
//     the grant the write already holds (structsAfter says why before).
//   - the room replica's lock, waited for to clone the replica (warmFork). Nothing under that
//     lock waits for the bound: the one decode there takes what is free of the bound at once
//     and otherwise runs beside it (renderedReplica.start, holdFreeCopy).
//   - the publish of its own committed updates, which may load a room and so take a grant of
//     that load's own. The transaction drops every fork, and with them its grant, before it
//     publishes any of them (Ledger.publish).
//
// No holder acquires a second grant: a decode under a context carrying one covers itself on that
// one (withCopyGrant), so a room load's fold, its copy's decode and the load hook's decode into
// the room share the load's, and a write's fork decode and decode-cap copy share the write's;
// and one transaction's live writes share one grant, covered at what all their forks weigh
// together (forkGrant), so a transaction writing two documents never waits for a part of the
// bound its own other grant holds. A grant that must grow gives back what it holds before it
// waits for the whole weight (heapGrant.cover).
//
// A waiter may hold anything - a write's transaction, its owner row and advisory lock, a
// compaction's advisory lock and connection, a connection of the rooms pool or of the shared
// pool, which is what a count holds while it waits, a slot of a buffered fold (foldSlots), the
// measurer's turn, a replica's lock, a loading room's ready barrier - since no holder waits for
// any of them, and so every holder finishes and gives its part back. A waiter whose request ends
// returns its context's error holding nothing. An admission waiting for another admission's
// count of the same version holds no part of the bound either (storedCounts.await).
type weighedHeaps struct {
	// name is what its grants are logged as (logHeapGrant): copyHeapName or applyHeapName.
	name string
	// size is the bound, always set by whoever built it (newWeighedHeaps): the process's
	// MemoryBounds.CopyHeap, the service's MemoryBounds.ApplyHeap, or a test's.
	size  int64
	once  sync.Once
	bound *semaphore.Weighted
	// held is what the grants hold of the bound now.
	held atomic.Int64
	// waiting runs, in a test, as a grant finds the bound too full for its weight and starts to
	// wait; granted runs once a grant holds its weight, with what every grant holds then.
	waiting func(weight int64)
	granted func(held int64)
}

// newWeighedHeaps is a bound of size bytes whose grants are logged as name.
func newWeighedHeaps(name string, size int64) *weighedHeaps {
	return &weighedHeaps{name: name, size: size}
}

// podCopyHeaps is the process's copy-heap bound, shared by every service and store in it as the
// memory it bounds is: the calibration's until a service built at another Go memory limit sizes
// it from that limit (New), and a test swaps in one of its own (useCopyHeaps).
var podCopyHeaps atomic.Pointer[weighedHeaps]

func init() { podCopyHeaps.Store(newWeighedHeaps(copyHeapName, calibratedBounds.CopyHeap)) }

// semaphore is the bound, made on first use.
func (h *weighedHeaps) semaphore() *semaphore.Weighted {
	h.once.Do(func() { h.bound = semaphore.NewWeighted(h.size) })
	return h.bound
}

// hold waits until it holds weight of the bound, or the whole bound for a weight past it, and
// returns what gives it back: a grant of its own, covered and released (heapGrant), which logs it
// where it waited long or ran alone (logHeapGrant). With a context that is never done the wait
// returns only once it holds what it asked for, so it cannot fail. It is how the apply-heap bound
// is held, for one apply from its weighing to its return (Service.weighPeerUpdate), where the
// copy-heap bound's holders cover a grant they carry from decode to decode.
func (h *weighedHeaps) hold(weight uint64) (release func()) {
	grant := &heapGrant{heaps: h}
	_ = grant.coverWeight(context.Background(), int64(min(weight, uint64(h.size))))
	return grant.release
}

// copyHeapWeight is what a decode of bytes of encoded state is granted, before the bound caps it
// (heapGrant.cover).
func copyHeapWeight(bytes int) int64 {
	return int64(bytes) * copyHeapPerByte
}

// liveWriteKeepPerByte is the heap a live write keeps for each byte of the state its fork was
// built from, beside what building the fork holds: the fork itself, the tree the operation
// started from, the tree it leaves, the markdown and offsets rendered of each, and the update and
// version it appends, all of which the write holds at once while it weighs what it changed
// (applyLive) and keeps until the fork is dropped (liveWrite.releaseFork). It is a fifth more
// than the most measured.
//
// Measured on 2026-11-05 by TestALiveWritesPeakHeapIsWithinItsGrant (GOMAXPROCS=1, the collector
// at GOGC=10 so the sampled peak is what is live), one replacement of ten characters in one
// heading: a write on the 3,035,240-byte state of a mebibyte of a room's shape kept 41,902,296
// bytes where its fork was decoded, 13.8 a byte, and 32,590,064 where it was cloned, 10.7; on the
// 782,880-byte state of 256 KiB of it, 10,759,296 and 8,371,064, 13.7 and 10.7.
const liveWriteKeepPerByte = 17

// liveWriteHeapPerByte is the heap a live write's fork is granted for each byte of the state it
// was built from, for the fork's whole life (liveWriteWeight): what building the fork holds above
// what it leaves, which for a decoded fork is what any full-state decode of that state holds
// (copyHeapPerByte), and what the write then keeps beside it (liveWriteKeepPerByte).
//
// A cloned fork, which is the usual one (Service.forkPoint), holds far less than a decode while
// it is built - it decodes nothing, so it holds no encoded state and no decoder - and is granted
// the decode's figure all the same, for two reasons. The grant must cover every hold the write
// makes inside it, and the heaviest is the decode-cap copy of the room the fork was read from
// (structsAfter), a whole second copy granted copyHeapWeight of that room's state; a weight under
// that would make the write give its grant back and wait for a larger one while it held its fork,
// which is the unbounded hold this bound exists to stop. And one weight for both ways a fork is
// built means a write is weighed before the replica is asked whether it holds the state to clone,
// which is only known under the replica's lock (Service.forkPoint).
//
// What a whole write holds at its peak is well inside it: measured the same day, the mebibyte
// write peaked at 67,103,336 bytes cloned and 69,465,856 decoded, 22.1 and 22.9 a byte of its
// state, and the 256 KiB write at 18,276,856 and 20,861,840, 23.3 and 26.6 a byte. Those are a
// room's shape, whose decode is far from the densest a decode can meet (copyHeapPerByte names
// the shapes that are): the sum is what holds for those too.
const liveWriteHeapPerByte = copyHeapPerByte + liveWriteKeepPerByte

// liveWriteWeight is what a live write's fork is granted for its whole life, before the bound
// caps it (heapGrant.coverWeight): the state it was built from at liveWriteHeapPerByte. A write
// whose state weighs more than the whole bound waits for all of it and runs alone, refused by
// nothing, as a load of the same document does.
func liveWriteWeight(bytes int) int64 {
	return int64(bytes) * liveWriteHeapPerByte
}

// heapGrant is one holder's part of a bound: held of it, nothing once released. One holder uses it
// at a time: the goroutine whose decodes it covers, or for a room's load the load's record, whose
// lock it is given back under (roomLoad.take), or the one apply a grant of the apply-heap bound
// covers (weighedHeaps.hold).
type heapGrant struct {
	heaps *weighedHeaps
	held  int64
	// waited is how long the grant waited for what it holds, and since when it has held it, both
	// for its release's log line (logHeapGrant).
	waited time.Duration
	since  time.Time
}

// newCopyGrant is a grant of the pod's copy-heap bound holding nothing.
func newCopyGrant() *heapGrant {
	return &heapGrant{heaps: podCopyHeaps.Load()}
}

// cover makes g hold the weight of a decode of bytes of encoded state, waiting within ctx
// (coverWeight).
func (g *heapGrant) cover(ctx context.Context, bytes int) error {
	return g.coverWeight(ctx, copyHeapWeight(bytes))
}

// coverWeight makes g hold weight of its bound, waiting within ctx: what a decode's state is
// weighed at (copyHeapWeight), what a count of a stored version is granted (storedCountWeight),
// or what an apply's estimate asks of the apply-heap bound (weighedHeaps.hold).
// A grant that already holds that much holds what it holds. One that holds less gives it back
// before it waits for the whole weight, so no holder waits while it holds any part of the bound.
// A weight past the whole bound waits for the whole bound and runs alone: a stored document must
// always load, and must always be counted. A wait ctx ends returns ctx's error and leaves g
// holding nothing.
func (g *heapGrant) coverWeight(ctx context.Context, weight int64) error {
	weight = min(weight, g.heaps.size)
	if weight <= g.held {
		return nil
	}
	g.release()
	bound := g.heaps.semaphore()
	started := time.Now()
	if !bound.TryAcquire(weight) {
		if g.heaps.waiting != nil {
			g.heaps.waiting(weight)
		}
		if err := bound.Acquire(ctx, weight); err != nil {
			return err
		}
	}
	g.held, g.since = weight, time.Now()
	g.waited = g.since.Sub(started)
	held := g.heaps.held.Add(weight)
	if g.heaps.granted != nil {
		g.heaps.granted(held)
	}
	return nil
}

// release gives back what g holds, and logs it where it waited long or held the whole bound
// (logHeapGrant). A grant holding nothing gives back nothing.
func (g *heapGrant) release() {
	if g == nil || g.held == 0 {
		return
	}
	g.heaps.held.Add(-g.held)
	g.heaps.semaphore().Release(g.held)
	logHeapGrant(g.heaps.name, g.held, g.heaps.size, g.waited, time.Since(g.since))
	g.held = 0
}

// tryCover makes g, which holds nothing, hold weight of its bound, or the whole bound for a
// weight past it, when that much is free at once, and reports whether it does. It never waits,
// so a decode that may not wait for the bound still runs under a grant whenever the bound has
// room for it (holdFreeCopy), and holds nothing when it reports false.
func (g *heapGrant) tryCover(weight int64) bool {
	weight = min(weight, g.heaps.size)
	if !g.heaps.semaphore().TryAcquire(weight) {
		return false
	}
	g.held, g.since, g.waited = weight, time.Now(), 0
	held := g.heaps.held.Add(weight)
	if g.heaps.granted != nil {
		g.heaps.granted(held)
	}
	return true
}

// copyGrantKey is the context key a grant travels under (carriedBy).
type copyGrantKey struct{}

// carriedBy is ctx carrying g, so a decode under it covers itself on g (holdCopy) rather than
// acquire a grant of its own inside g's.
func (g *heapGrant) carriedBy(ctx context.Context) context.Context {
	return context.WithValue(ctx, copyGrantKey{}, g)
}

// withCopyGrant is ctx carrying a grant, and the grant: the one ctx already carries, which its
// owner releases, with a done that releases nothing, or a new one, with a done that releases it.
// A decode under ctx covers itself on that grant (holdCopy), so the decodes one holder makes in
// turn share one grant rather than nest a second acquire inside the first.
func withCopyGrant(ctx context.Context) (context.Context, *heapGrant, func()) {
	if grant, ok := ctx.Value(copyGrantKey{}).(*heapGrant); ok {
		return ctx, grant, func() {}
	}
	grant := newCopyGrant()
	return grant.carriedBy(ctx), grant, grant.release
}

// holdCopy covers a decode of bytes of encoded state (heapGrant.cover) on the grant ctx carries,
// or on one of its own, and returns what ends the hold: nothing for a grant ctx carries, whose
// owner releases it, and the release of its own.
func holdCopy(ctx context.Context, bytes int) (func(), error) {
	ctx, grant, done := withCopyGrant(ctx)
	if err := grant.cover(ctx, bytes); err != nil {
		return nil, err
	}
	return done, nil
}

// loadedHistory is a document's stored history, read and decoded under one grant of the copy-heap
// bound (loadDecodedHistory).
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
// by one grant of the copy-heap bound: the grant ctx carries, whose owner gives it back, or one of
// its own, which it gives back before it returns, so no caller releases a grant by hand
// (withCopyGrant). It is the one way the service reads a history it decodes - a socket's
// admission (loadTree), a room's load (loadDecoded) and a rebuild's source (rebuildSource) - so
// that no new way out of one of them can leave a grant held. It decodes under maxPending, the
// pending queue the rooms of the service that reads decode under (Service.maxPendingItems).
//
// copies is how many copies of the history its caller holds live at once, which the grant is
// weighed for: one for a caller that keeps the decode alone, two for one that decodes the state
// again beside it (rebuildSource). refuse, where it is not nil, is read from the history before
// the grant is covered and before anything is decoded (loadableHistory), for a history no decode
// should be paid for.
//
// The read's fold, when the history holds several updates, covers the grant where it reads them
// (foldRead), and the decode covers it for the state the read returned. The error is the
// read's, the refusal's or the grant's, a wait ctx ended included, and leaves nothing decoded;
// ygo's refusal of the bytes themselves is the history's refused.
func loadDecodedHistory(ctx context.Context, load VersionedStore, artifactID string, copies, maxPending int, refuse func(update []byte) error) (loadedHistory, error) {
	ctx, grant, done := withCopyGrant(ctx)
	defer done()
	loaded, err := load.Load(ctx, artifactID)
	if err == nil && refuse != nil {
		err = refuse(loaded.Update)
	}
	if err == nil {
		err = grant.cover(ctx, copies*len(loaded.Update))
	}
	if err != nil {
		return loadedHistory{}, err
	}
	decoded, refused := decodeStoredCopy(loaded.Update, maxPending)
	return loadedHistory{loaded: loaded, decoded: decoded, refused: refused}, nil
}

// holdServerCopy is holdCopy for a decode of the server's own that no request waits on: a write's
// fork read outside a transaction, the measurer's copy, the copy of a room loaded without this
// service's persistence. Its wait has no deadline, and ends, since every holder gives its part
// back (weighedHeaps).
//
// It takes ctx for the grant ctx carries alone, never for its deadline: a decode made inside a
// live write - its fork's own (buildForkHeld) and the decode-cap copy of the room it was read
// from (structsAfter) - covers the transaction's grant, which already holds more than either
// would ask for (liveWriteWeight), so it waits for nothing and the transaction holds one grant
// from its first fork's build to its last fork's release. A caller that carries none - the
// three server writes outside applyLive, a socket's own copy - takes a grant of its own and
// waits for it without a deadline, as this always did, and is never refused.
func holdServerCopy(ctx context.Context, bytes int) func() {
	release, _ := holdCopy(context.WithoutCancel(ctx), bytes)
	return release
}

// holdFreeCopy covers a decode of bytes of encoded state on what is free of the copy-heap bound
// at once, and runs beside the bound's holders rather than wait where it is not. It is what the
// one decode that may not wait takes: the replica's build from a base (renderedReplica.start),
// which runs under the replica's lock. A live write holds a grant of the bound for its fork's
// whole life and waits for that lock to clone the replica (warmFork, liveWriteWeight), so a
// wait here would hold that write and that replica against each other, each waiting for what
// the other holds.
//
// What such a decode holds outside the bound is one room's own second copy and the decoder above
// it (roomCopies), which the room budget charges where the replica keeps it and the named slack
// allows for while it is built (MemoryBounds.HeapSlack); and a room builds its replica from a
// base only after a recover or a load that handed it no copy, never while it is typed into.
func holdFreeCopy(bytes int) func() {
	grant := newCopyGrant()
	if !grant.tryCover(copyHeapWeight(bytes)) {
		return func() {}
	}
	return grant.release
}

// heapGrantLogged is how long a grant of a weighed heap bound (weighedHeaps) may have waited for
// its weight before its release is logged (logHeapGrant):
// a tenth of a second, past any wait on a lock or the scheduler and short of the second or more a
// heavy apply or decode takes, so the line names the turns a burst of heavy work made a holder
// wait for and stays quiet while the bounds are not crowded.
const heapGrantLogged = 100 * time.Millisecond

// logHeapGrant logs a grant of bound, applyHeapName or copyHeapName, as it is given back, where
// it waited
// heapGrantLogged or longer for its weight, or held all size bytes of its bound and so ran alone
// among that bound's holders: its weight, how long it waited, and how long it held it, the line's
// time being when it gave it back. An operator reads from it how long a burst of heavy work made
// each holder wait its turn; a real-process drive reads from it which holders ran at once and the
// longest wait (cmd/dispatch, TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory).
func logHeapGrant(bound string, weight, size int64, waited, held time.Duration) {
	if waited < heapGrantLogged && weight < size {
		return
	}
	slog.Info("dispatch: a heap bound's grant was given back",
		"bound", bound, "weight", weight, "bound_bytes", size, "alone", weight >= size, "waited", waited, "held", held)
}
