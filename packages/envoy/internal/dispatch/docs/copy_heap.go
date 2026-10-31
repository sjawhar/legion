package docs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// The copy-heap bound holds what the pod's full-state decodes hold live beside their product at
// once. These decodes of a document's whole state - a room's load (its fold of a history of
// several updates, the copy LoadDoc or a socket's admission decodes, ygo's decode into the room),
// a write's fork from the store (coldFork) or a rebuilt fork of a room (warmFork), the fold of a
// cold read, a version read or a compaction (fold), the replica's build from a base
// (renderedReplica.start), the measurer's copy for a server write (structsAfter) and a rebuild's
// decodes (rebuildSource, RebuildTx) - each hold copyHeapWeight of their encoded state out of
// copyHeapBound while they decode. What each leaves behind is its owner's: a room's two copies are
// charged by the room budget, a fork is its write's, a fold's state goes back to its caller. The
// copy a read of a resident room takes while the room's replica is busy (holdLive,
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

// copyHeapBound is the most the full-state decodes are granted at once: the third of what the Go
// memory limit leaves above the room budget, the cold reads' cache and the pressure reserve past
// the budget's limit (heapAboveTheReserve) that neither the apply-heap bound nor the slack takes,
// 1,181,116,006 - applyHeapBound 393,705,335 - heapSlackBytes 393,705,335 = 393,705,336 bytes,
// about 375 MiB. Every load of a document at or near the decode cap measured holds less above what
// it leaves than the whole bound: the heaviest load measured held 303,430,664 bytes live beside its
// two copies (copyHeapPerByte), and the heaviest decode of any path 320,762,656, 72,942,680 under
// the bound. Weighed at copyHeapPerByte, a mebibyte of prose (145,691,520 bytes) decodes two at a
// time; a run of a million keystrokes (383,220,432) one at a time; the agent-rewritten and
// long-edited documents at the decode cap weigh more than the whole bound (602,726,112 and
// 692,509,680), and each waits for all of it and runs alone, never refused, since a stored document
// must always load (copyGrant.cover).
const copyHeapBound = heapAboveTheReserve - applyHeapBound - heapSlackBytes

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
// guard stops latest (nodeGuard, pmdoc/elements.go:31-37). A mebibyte of prose held 13.0 a byte,
// the walk of its 2,957,640-byte state included. The small shapes run denser a byte because the
// guard stops the parse of a large one: it is the whole parse, not the byte, that the guard
// bounds, so a count of a whole document never holds more than storedCountHeapAtMost whatever its
// markdown spells.
const storedCountHeapPerByte = 160

// storedCountHeapAtMost caps what a count's markdown is granted (storedCountWeight), since
// goldmark stops building nodes at the guard (nodeGuard, pmdoc/elements.go:31-37) and a stored
// version's markdown is at most pmdoc.MaxDocumentBytes: no count of a mebibyte measured held more
// than 65,025,848 bytes, a mebibyte of hard breaks, where the densest markdown held 50,703,568 and
// a mebibyte of prose 13,607,960 (TestAStoredVersionsCountHoldsNoMoreThanItsGrant). It is about a
// quarter more than that, so a mebibyte is granted 81,788,928 bytes rather than the 167,737,600
// its bytes alone would ask for, and four counts of a mebibyte run at once inside copyHeapBound
// rather than two.
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

// copyHeaps is the pod's bound on the heap its full-state decodes hold (copyHeapBound). Its zero
// value is ready: the bound is made on first use. Waiters are served in the order they came
// (semaphore.Weighted), so a decode waiting for the whole bound is not passed over by lighter ones
// behind it, and every one after it waits until it has run.
//
// It cannot deadlock, because a holder of any part of it waits for nothing another holder or a
// waiter can hold: between its cover and its release it decodes, renders, counts or reads the
// rows of the transaction it already holds, takes no lock held across a wait, takes no pooled
// connection - a fold covers its grant inside the transaction it already holds, a count of a
// stored version opens its own transaction and weighs itself in it before it covers
// (countStoredVersion), and a room's load gives its grant back before onLoadDocument reads the
// store - runs no update observer, since ygo decodes a loading room's state before the room
// registers any and no holder commits on a resident room, and acquires no second grant: a decode
// under a context carrying a grant covers itself on that one (withCopyGrant), so a room load's
// fold, its decode and ygo's decode share the load's, and a grant that must grow gives back what
// it holds before it waits for the whole weight (copyGrant.cover). A waiter may hold anything - a
// write's transaction, its owner row and advisory lock, a compaction's advisory lock and
// connection, a connection of the rooms pool or of the shared pool, which is what a count holds
// while it waits, the measurer's turn, a replica's lock, a loading room's ready barrier - since
// no holder waits for any of them, and so every holder finishes and gives its part back. A waiter
// whose request ends returns its context's error holding nothing. An admission waiting for
// another admission's count of the same version holds no part of the bound either
// (storedCounts.await).
type copyHeaps struct {
	once  sync.Once
	bound *semaphore.Weighted
	// size is the bound: copyHeapBound, or a test's.
	size int64
	// held is what the grants hold of the bound now.
	held atomic.Int64
	// waiting runs, in a test, as a grant finds the bound too full for its weight and starts to
	// wait; granted runs once a grant holds its weight, with what every grant holds then.
	waiting func(weight int64)
	granted func(held int64)
}

// podCopyHeaps is the process's bound, shared by every service and store in it as the memory it
// bounds is; a test swaps in one of its own (useCopyHeaps).
var podCopyHeaps atomic.Pointer[copyHeaps]

func init() { podCopyHeaps.Store(&copyHeaps{}) }

// limit is the bound's size.
func (h *copyHeaps) limit() int64 {
	if h.size > 0 {
		return h.size
	}
	return copyHeapBound
}

// semaphore is the bound, made on first use.
func (h *copyHeaps) semaphore() *semaphore.Weighted {
	h.once.Do(func() { h.bound = semaphore.NewWeighted(h.limit()) })
	return h.bound
}

// copyHeapWeight is what a decode of bytes of encoded state is granted, before the bound caps it
// (copyGrant.cover).
func copyHeapWeight(bytes int) int64 {
	return int64(bytes) * copyHeapPerByte
}

// copyGrant is one holder's part of the bound: held of it, nothing once released. One holder uses
// it at a time: the goroutine whose decodes it covers, or for a room's load the load's record,
// whose lock it is given back under (roomLoad.take).
type copyGrant struct {
	heaps *copyHeaps
	held  int64
	// waited is how long the grant waited for what it holds, and since when it has held it, both
	// for its release's log line (logHeapGrant).
	waited time.Duration
	since  time.Time
}

// newCopyGrant is a grant of the pod's bound holding nothing.
func newCopyGrant() *copyGrant {
	return &copyGrant{heaps: podCopyHeaps.Load()}
}

// cover makes g hold the weight of a decode of bytes of encoded state, waiting within ctx
// (coverWeight).
func (g *copyGrant) cover(ctx context.Context, bytes int) error {
	return g.coverWeight(ctx, copyHeapWeight(bytes))
}

// coverWeight makes g hold weight of the bound, waiting within ctx: what a decode's state is
// weighed at (copyHeapWeight) or what a count of a stored version is granted (storedCountWeight).
// A grant that already holds that much holds what it holds. One that holds less gives it back
// before it waits for the whole weight, so no holder waits while it holds any part of the bound.
// A weight past the whole bound waits for the whole bound and runs alone: a stored document must
// always load, and must always be counted. A wait ctx ends returns ctx's error and leaves g
// holding nothing.
func (g *copyGrant) coverWeight(ctx context.Context, weight int64) error {
	weight = min(weight, g.heaps.limit())
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
func (g *copyGrant) release() {
	if g == nil || g.held == 0 {
		return
	}
	g.heaps.held.Add(-g.held)
	g.heaps.semaphore().Release(g.held)
	logHeapGrant("copy", g.held, g.heaps.limit(), g.waited, time.Since(g.since))
	g.held = 0
}

// copyGrantKey is the context key a grant travels under (carriedBy).
type copyGrantKey struct{}

// carriedBy is ctx carrying g, so a decode under it covers itself on g (holdCopy) rather than
// acquire a grant of its own inside g's.
func (g *copyGrant) carriedBy(ctx context.Context) context.Context {
	return context.WithValue(ctx, copyGrantKey{}, g)
}

// withCopyGrant is ctx carrying a grant, and the grant: the one ctx already carries, which its
// owner releases, with a done that releases nothing, or a new one, with a done that releases it.
// A decode under ctx covers itself on that grant (holdCopy), so the decodes one holder makes in
// turn share one grant rather than nest a second acquire inside the first.
func withCopyGrant(ctx context.Context) (context.Context, *copyGrant, func()) {
	if grant, ok := ctx.Value(copyGrantKey{}).(*copyGrant); ok {
		return ctx, grant, func() {}
	}
	grant := newCopyGrant()
	return grant.carriedBy(ctx), grant, grant.release
}

// holdCopy covers a decode of bytes of encoded state (copyGrant.cover) on the grant ctx carries,
// or on one of its own, and returns what ends the hold: nothing for a grant ctx carries, whose
// owner releases it, and the release of its own.
func holdCopy(ctx context.Context, bytes int) (func(), error) {
	ctx, grant, done := withCopyGrant(ctx)
	if err := grant.cover(ctx, bytes); err != nil {
		return nil, err
	}
	return done, nil
}

// holdServerCopy is holdCopy for a decode of the server's own that no request waits on: the
// replica's build, the measurer's copy, the copy of a room loaded without this service's
// persistence. Its wait has no deadline, and ends, since every holder gives its part back
// (copyHeaps).
func holdServerCopy(bytes int) func() {
	release, _ := holdCopy(context.Background(), bytes)
	return release
}

// heapGrantLogged is how long a grant of the apply-heap bound (applyHeaps) or of the copy-heap
// bound (copyHeaps) may have waited for its weight before its release is logged (logHeapGrant):
// a tenth of a second, past any wait on a lock or the scheduler and short of the second or more a
// heavy apply or decode takes, so the line names the turns a burst of heavy work made a holder
// wait for and stays quiet while the bounds are not crowded.
const heapGrantLogged = 100 * time.Millisecond

// logHeapGrant logs a grant of bound, "apply" or "copy", as it is given back, where it waited
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
