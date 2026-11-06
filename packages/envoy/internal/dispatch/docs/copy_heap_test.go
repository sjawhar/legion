package docs

import (
	"context"
	"errors"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// copyHeapMeasurement is the environment variable that runs the load-slot measurement's heavy
// rows (TestEveryFullStateDecodesTransientHeapIsWithinItsSlot).
const copyHeapMeasurement = "DISPATCH_MEASURE_COPY_HEAP"

// keystrokeRunStates are the states of a room that took a run of a million keystrokes, each its
// own item, after the first paragraph of a small document, and of the same room once three
// one-character frames each named a run item as its left origin and the run item a third of a
// million later as its right origin: a decode of the second scans 1,000,002 distinct items, each
// with its own scan mark, the whole of roomScanTotal, the heaviest shape a decode re-scans.
//
// The run is six keystrokes past a million so that those three frames carry the whole total
// themselves. Under sjawhar/ygo v1.51.3-sami.7 the seed document's own decode counted four scans,
// and a run of 1,000,000 reached the total with them; under v1.55.1-sami.1 that decode counts
// none, measured as the unscanned state's re-scan in
// TestACopyOfARoomAtTheScanTotalCostsACopyOfItsStateAndAboutASecondOfScans.
func keystrokeRunStates(t *testing.T) (keystrokes, scanned []byte) {
	t.Helper()
	const runner, scanner crdt.ClientID = 1_000, 4_000_000
	const run = 1_000_006
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
		t.Fatal(err)
	}
	chainedRun(t, room, lastCharacter(t, room), runner, run)
	keystrokes = crdt.EncodeStateAsUpdateV1(room, nil)
	span := uint64(run / 3)
	for k := range uint64(3) {
		frame := newV1Frame(1).group(1, uint64(scanner)+k, 0).
			raw(structStringRef | infoHasOrigin | infoHasRightOrigin).
			uint(uint64(runner)).uint(k * span).uint(uint64(runner)).uint((k + 1) * span).str("z").noDeletes()
		if err := crdt.ApplyUpdateV1(room, frame, nil); err != nil {
			t.Fatal(err)
		}
	}
	return keystrokes, crdt.EncodeStateAsUpdateV1(room, nil)
}

// emptyUpdate is a V1 update of no struct and no deletion: a stored history's second update that
// makes its fold decode the state whole, and adds nothing a document at the decode cap has no room
// for.
var emptyUpdate = []byte{0, 0}

// errCopyMeasured ends the copy measurement's joined transactions without committing them.
var errCopyMeasured = errors.New("the copy was measured")

// copyPath is one full-state decode the copy-heap bound grants, as measureCopyPaths measures it:
// bytes is the encoded state its grant is weighed by, read once prepare has run.
type copyPath struct {
	name    string
	prepare func()
	bytes   func() int
	copy    func() any
}

// copyMeasurementGCPercent is the collector's GOGC while a copy is measured: the heap may grow a
// tenth past what the last collection left live before the next one runs, so what the sampler sees
// at its peak is what the copy holds live, with at most about a tenth more of garbage. At the
// default of 100 the garbage of a load's first decode, left uncollected while its second runs,
// showed the load of a million keystrokes holding 601,928,008 bytes above what it left, against
// 253,699,168 at 10, on 2026-10-28; under the production's memory limit the collector frees
// garbage as the heap nears it, and what it cannot free is what the bound is for.
const copyMeasurementGCPercent = 10

// copyTransient is what one run of a full-state decode holds live above what it leaves behind:
// the most heap its objects held above the heap before it (loadCost.peak), measured at
// copyMeasurementGCPercent, less what it still holds once collected, which is its product - a
// room's two copies, a fork, a replica's copy - and is charged where its owner keeps it, if
// anywhere.
func copyTransient(cost loadCost) int64 {
	return int64(cost.peak) - max(cost.held, 0)
}

// Every full-state decode holds no more heap above what it leaves behind than the slot it takes
// of the pod's load concurrency allows it (loadSlotBytes, 600 MB): one whole-document decode, one
// fork or one heavy apply to a slot, whatever the document weighs. For each shape, at
// GOMAXPROCS=1 (measureLoad), it measures the room's load as the server's own work makes it
// (Server.Apply on a room no one holds, and the room's first transaction) and as a socket's
// admission and load make it; a write's fork from the store (coldFork) and from a resident room
// it has never forked (warmFork's rebuild); the replica's build from a base
// (renderedReplica.start); the copy a server write's decode-cap count takes (structsAfter); a
// rebuild's decode and re-scan of the history (loadDecodedHistory, nearACap); and, with an empty
// update stored after the state, the room's load that folds the history first, a cold read's
// fold (LoadDocument) and compaction's (Compact). Each path starts from the state as the whole
// stored history, since a closed room compacts what its own transactions stored into a state
// re-encoded from its decode.
//
// The slot is one figure for every shape, where the bound it replaced was weighed per byte of
// the state: what this holds is that the heaviest decode of the heaviest stored document still
// fits one slot, which is what lets the pod run MemoryBounds.LoadSlots of them at once.
//
// The fast row is a mebibyte of prose. Set DISPATCH_MEASURE_COPY_HEAP=1 for the heavy rows - the
// document an agent rewrote whole until the decode cap refused a write (agentRewrittenState, kept
// between runs in the file DISPATCH_AGENT_REWRITTEN_CACHE names), the long-edited document typed
// on until the cap (longEditedAtCapState), a run of a million keystrokes and the same run scanned
// once whole (keystrokeRunStates), each near or at the decode cap, every one of whose paths must
// also fit one slot, since a stored document must always load. They took 597 to 623 s together
// here on 2026-10-28.
func TestEveryFullStateDecodesTransientHeapIsWithinItsSlot(t *testing.T) {
	skipUnderRace(t)
	heavy := os.Getenv(copyHeapMeasurement) != ""
	var keystrokes, scanned []byte
	runStates := func(t *testing.T) {
		if keystrokes == nil {
			keystrokes, scanned = keystrokeRunStates(t)
		}
	}
	for _, shape := range []struct {
		name  string
		heavy bool
		state func(t *testing.T) []byte
	}{
		{"a mebibyte of prose", false, func(t *testing.T) []byte {
			state, _ := residentRoomState(t, 1<<20)
			return state
		}},
		{"an agent's whole rewrites until the cap refused one", true, cachedAgentRewrittenState},
		{"a long-edited document typed on until the cap", true, longEditedAtCapState},
		{"a run of a million keystrokes", true, func(t *testing.T) []byte { runStates(t); return keystrokes }},
		{"a run of a million keystrokes scanned whole", true, func(t *testing.T) []byte { runStates(t); return scanned }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if shape.heavy && !heavy {
				t.Skip("set " + copyHeapMeasurement + "=1 to run the heavy rows")
			}
			measureCopyPaths(t, shape.state(t))
		})
	}
}

// measureCopyPaths measures every path TestEveryFullStateDecodesTransientHeapIsWithinItsSlot
// names on a document storing state, and fails a path whose transient passes one load slot.
func measureCopyPaths(t *testing.T, state []byte) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs in the room while it is measured
	closed := func() { closeResidentRoom(t, service, artifactID) }
	// fresh leaves the room closed and state the whole stored history again: a closed room's
	// persistence compacts what its transactions stored into a state of its own.
	fresh := func() {
		closed()
		storeHistory(t, service, artifactID, state)
	}
	resident := func() *crdt.Doc {
		if err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatalf("load the room: %v", err)
		}
		return roomDocument(t, service, artifactID)
	}
	load := func() any {
		live := resident()
		live.Transact(func(*crdt.Transaction) {})
		return live
	}
	fork := func() any {
		var fork *crdt.Doc
		err := joinedWrite(service, func(ctx context.Context) error {
			write, err := service.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID)
			if err != nil {
				return err
			}
			if fork, err = service.forkLive(ctx, write); err != nil {
				return err
			}
			return errCopyMeasured
		})
		if !errors.Is(err, errCopyMeasured) {
			t.Fatalf("fork the document: %v", err)
		}
		return fork
	}
	stored := func() int { return len(state) }
	var live *crdt.Doc
	liveState := func() int { return len(crdt.EncodeStateAsUpdateV1(live, nil)) }
	var markdown string
	// appended is fresh with an empty update stored after the state, so the next read of the
	// history folds two updates.
	appended := func() {
		fresh()
		if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
			t.Fatal(err)
		}
		if stored := storedUpdateCount(t, service.store, artifactID); stored != 2 {
			t.Fatalf("the history holds %d updates, want the state and the update after it", stored)
		}
	}
	// history is the stored updates' bytes, or the state their fold makes where that is larger.
	history := func() int {
		loaded, err := service.persistence.Load(ctx, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		return max(storedUpdateBytes(t, service.store, artifactID), len(loaded.Update))
	}
	paths := []copyPath{
		{"the server's load and first transaction", fresh, stored, load},
		{"a socket's admission, load and first transaction", fresh, stored, func() any {
			tree, preload, err := service.loadTree(ctx, artifactID)
			if err != nil {
				t.Fatalf("the admission's schema check: %v", err)
			}
			if _, err := documentMarkdown(tree); err != nil {
				t.Fatalf("the admission's rendering: %v", err)
			}
			defer service.leavePreload(artifactID, preload)()
			return load()
		}},
		{"a write's fork of the stored history", fresh, stored, fork},
		{"a write's first fork of the resident room", func() { fresh(); live = resident() }, liveState, fork},
		{"the replica's build from a base", func() {
			doc := newDocumentCopy()
			if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
				t.Fatal(err)
			}
			rendered, err := renderDocument(doc)
			if err != nil {
				t.Fatal(err)
			}
			markdown = rendered
		}, stored, func() any {
			replica := &renderedReplica{rendering: &replicaRendering{markdown: markdown}, roomWeighing: &roomWeighing{}, paths: &service.renderPaths}
			replica.start(artifactID, state)
			if replica.doc == nil {
				t.Fatal("the replica took no copy of the base")
			}
			return replica
		}},
		{"the copy a server write's decode-cap count takes", func() { fresh(); live = resident() }, liveState, func() any {
			return structsAfter(ctx, live, serverWrite{apply: func(*crdt.Doc) {}})
		}},
		{"a rebuild's decode and re-scan of the history", func() {}, stored, func() any {
			history := decodedHistory(t, state)
			if _, _, err := nearACap(ctx, history); err != nil {
				t.Fatal(err)
			}
			return history
		}},
		{"the server's load of a history it folds first", appended, history, load},
		{"a cold read's fold of the history", appended, history, func() any {
			loaded, err := service.persistence.LoadDocument(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			return loaded.Doc
		}},
		// The cold read loaded no room, so the history it folded is still the two updates. The
		// keep-1 rule folds them now: the tail rule would pass over rows this young (compactTail).
		{"compaction's fold of the history", func() {}, history, func() any {
			folded, err := service.persistence.compactRoom(ctx, artifactID, compactRule{keep: 1}, true)
			if err != nil || folded.deleted != 1 {
				t.Fatalf("compaction folded %d updates (%v), want 1", folded.deleted, err)
			}
			return nil
		}},
	}
	for _, path := range paths {
		path.prepare()
		bytes := path.bytes()
		cost := func() loadCost {
			defer debug.SetGCPercent(debug.SetGCPercent(copyMeasurementGCPercent))
			return measureLoad(t, path.copy)
		}()
		live = nil
		transient := copyTransient(cost)
		t.Logf("%d-byte state, %s: %s, %d bytes allocated, %d peak, %d held; %d transient, %.1f a byte of %d, against a %d-byte slot",
			len(state), path.name, cost.took.Round(time.Millisecond), cost.allocated, cost.peak, cost.held,
			transient, float64(transient)/float64(bytes), bytes, loadSlotBytes)
		if transient > loadSlotBytes {
			t.Errorf("%s holds %d bytes above what it leaves, %.1f a byte of its %d, past the %d bytes one load slot allows",
				path.name, transient, float64(transient)/float64(bytes), bytes, loadSlotBytes)
		}
	}
}

// useLoadSlots makes slots the process's load concurrency for the rest of t (podLoadSlots).
func useLoadSlots(t *testing.T, slots *loadSlots) *loadSlots {
	t.Helper()
	previous := podLoadSlots.Swap(slots)
	t.Cleanup(func() { podLoadSlots.Store(previous) })
	return slots
}

// proseState is the state of a fresh document of at least bytes of prose (proseAtLeast).
func proseState(t *testing.T, bytes int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse(proseAtLeast(bytes))
	if err != nil {
		t.Fatal(err)
	}
	state, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// loadRoomOf loads artifactID's room as the server's own work does, a Server.Apply that changes
// nothing.
func loadRoomOf(service *Service, artifactID string) error {
	err := service.srv.Apply(context.Background(), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	if errors.Is(err, websocket.ErrNoChanges) {
		return nil
	}
	return err
}

// slotsHeld records how many slots were held as each one was taken (loadSlots.taken).
type slotsHeld struct {
	mu   sync.Mutex
	held []int64
}

func (g *slotsHeld) record(held int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = append(g.held, held)
}

func (g *slotsHeld) all() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.held)
}

// A room's load folds a history of several updates on the slot it took for the fold, and decodes
// and hands ygo the state on that same slot: with every other slot taken the moment the load
// holds its own, before its fold decodes anything, the load finishes - no part of it takes a
// second slot (withLoadSlot) - and gives its slot back.
func TestALoadWhoseFoldRunsWhileTheSlotsAreFullFoldsOnTheLoadsOwnSlot(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 64<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	if stored := storedUpdateCount(t, service.store, artifactID); stored != 2 {
		t.Fatalf("the history holds %d updates, want the state and the update after it", stored)
	}
	slots := useLoadSlots(t, newLoadSlots(2))
	var taken slotsHeld
	var filled bool
	slots.taken = func(held int64) {
		taken.record(held)
		if !filled {
			filled = true
			if !slots.semaphore().TryAcquire(slots.size - held) {
				t.Errorf("the concurrency had no %d slots left beside the load's %d held", slots.size-held, held)
			}
		}
	}
	loaded := make(chan error, 1)
	go func() { loaded <- loadRoomOf(service, artifactID) }()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("the load failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		slots.semaphore().Release(slots.size - 1)
		t.Fatal("the load did not finish in 30 s while every other slot was held: it waits for a second slot inside its own")
	}
	defer slots.semaphore().Release(slots.size - 1)
	if got := taken.all(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("the load took slots leaving %v held, want one slot of its own for its fold and its decode together", got)
	}
	if roomDocument(t, service, artifactID) == nil {
		t.Fatal("the room did not load")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d slots once loaded", held)
	}
}

// A load whose slots are every one held waits for one rather than decoding beside their holders,
// and is never refused, since a stored document must always load: it waits without a deadline
// (holdServerLoad), takes the slot the holder gives back, and gives it back itself once the room
// is loaded.
func TestALoadWaitsForASlotWhileEveryOneIsHeldAndThenTakesIt(t *testing.T) {
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	var taken slotsHeld
	slots.taken = taken.record
	loaded := make(chan error, 1)
	go func() { loaded <- loadRoomOf(service, artifactID) }()
	select {
	case <-waiting:
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the load neither waited for a slot nor finished in 30 s")
	}
	select {
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while the only slot was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("the load failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the load did not finish in 30 s once the slot was free")
	}
	if got := taken.all(); len(got) == 0 {
		t.Fatal("the load took no slot")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d slots once loaded", held)
	}
}

// Loads of six documents at once, against a concurrency of two slots, never hold more slots
// together than it has, and every one of them loads: the third waits for one of the two to give
// its slot back. Each slot is held a tenth of a second past its decode, so the loads overlap.
func TestConcurrentLoadsNeverHoldMoreSlotsThanThePodHas(t *testing.T) {
	const together = 2
	service, first := newTestService(t)
	state := proseState(t, 16<<10)
	rooms := append([]string{first}, createDocuments(t, service.store, 5)...)
	for _, room := range rooms {
		storeHistory(t, service, room, state)
	}
	slots := useLoadSlots(t, newLoadSlots(together))
	var most atomic.Int64
	slots.taken = func(held int64) {
		for seen := most.Load(); held > seen && !most.CompareAndSwap(seen, held); seen = most.Load() {
		}
		time.Sleep(100 * time.Millisecond)
	}
	errs := make(chan error, len(rooms))
	for _, room := range rooms {
		go func() { errs <- loadRoomOf(service, room) }()
	}
	for range rooms {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("a load failed: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the loads did not all finish in 60 s")
		}
	}
	if got := most.Load(); got > slots.size || got < together {
		t.Fatalf("the loads held %d slots at most, want the %d the concurrency has and never more", got, together)
	}
	for _, room := range rooms {
		if roomDocument(t, service, room) == nil {
			t.Fatalf("room %s did not load", room)
		}
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the loads still hold %d slots once loaded", held)
	}
}

// A socket's admission waits for its slot within its request: one whose request ends while its
// fold waits for a slot answers the request's error, fails no room, and holds neither a slot nor
// a connection of the store, so the next admission reads and decodes the history once a slot is
// free. The concurrency here holds one slot, held by the test.
func TestAnAdmissionWhoseRequestEndsWhileItWaitsForASlotAnswersItsErrorAndHoldsNothing(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(ctx)
	type admission struct {
		preload *preloadedDocument
		err     error
	}
	admitted := make(chan admission, 1)
	go func() {
		_, preload, err := service.loadTree(request, artifactID)
		admitted <- admission{preload, err}
	}()
	select {
	case <-waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the admission did not wait for a slot in 30 s")
	}
	cancel()
	var ended admission
	select {
	case ended = <-admitted:
	case <-time.After(30 * time.Second):
		t.Fatal("the admission did not answer in 30 s once its request ended")
	}
	if !errors.Is(ended.err, context.Canceled) || ended.preload != nil {
		t.Fatalf("the admission answered %v with preload %v, want the request's context.Canceled and none", ended.err, ended.preload)
	}
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the ended request failed the room: %v", err)
	}
	if held := slots.held.Load(); held != 1 {
		t.Fatalf("%d slots are held, want the one the test holds and none of the ended admission's", held)
	}
	holder.release()
	_, preload, err := service.loadTree(ctx, artifactID)
	if err != nil || preload == nil || preload.decoded.doc == nil {
		t.Fatalf("the next admission answered %v with preload %v, want the history decoded", err, preload)
	}
	service.leavePreload(artifactID, preload)()
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("%d slots are still held once both admissions have answered", held)
	}
}

// A rebuild's check of a resident room decodes the room's whole state before the rebuild decides
// anything (nearACap), so that decode takes a slot of the pod's load concurrency like every other
// full-state decode: with the one slot held, the rebuild waits rather than decoding beside the
// holder; once that slot is given back it runs and answers that the document is live and far from
// both caps (ErrDocumentLive), holding no slot after it.
func TestARebuildOfAResidentDocumentTakesASlotBeforeItWeighsIt(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	// No settlement of the room takes a slot of its own while the rebuild waits.
	service.settle = time.Hour
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	var taken slotsHeld
	slots.taken = taken.record
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	answered := make(chan error, 1)
	go func() {
		_, _, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
		answered <- err
	}()
	select {
	case <-waiting:
	case err := <-answered:
		t.Fatalf("the rebuild answered %v while the only slot was held: its check decoded the resident room's state outside the concurrency", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild's check neither waited for a slot nor answered in 30 s")
	}
	select {
	case err := <-answered:
		t.Fatalf("the rebuild answered %v while the only slot was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrDocumentLive) {
			t.Fatalf("the rebuild of a resident document far from both caps answered %v, want ErrDocumentLive", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not answer in 30 s once the slot was free")
	}
	if got := taken.all(); len(got) == 0 {
		t.Fatal("the rebuild's check took no slot")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the refused rebuild still holds %d slots", held)
	}
}

// agentRewrittenCacheEnv names a file the heavy measurements keep the agent-rewritten state in
// (agentRewrittenState), which takes about 200 s to build: read when the file exists, written
// once built when it does not.
const agentRewrittenCacheEnv = "DISPATCH_AGENT_REWRITTEN_CACHE"

// cachedAgentRewrittenState is agentRewrittenState, kept in the file agentRewrittenCacheEnv names.
func cachedAgentRewrittenState(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv(agentRewrittenCacheEnv)
	if path != "" {
		if state, err := os.ReadFile(path); err == nil {
			return state
		}
	}
	state, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	if path != "" {
		if err := os.WriteFile(path, state, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// loadCost is what one run of a load cost at GOMAXPROCS=1: how long it took, what it allocated
// (runtime.MemStats.TotalAlloc across it), the most heap its objects held above what the heap held
// before it, sampled every millisecond the scheduler lets the sampler run
// (/memory/classes/heap/objects:bytes), and what it still holds once collected.
type loadCost struct {
	took      time.Duration
	allocated uint64
	peak      uint64
	held      int64
}

// measureLoad runs load once at GOMAXPROCS=1 and returns its cost (loadCost).
func measureLoad(t *testing.T, load func() any) loadCost {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	heapObjects := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	base := heapObjects()
	// The sampler is stopped whichever way this returns, since load fails the test (t.Fatal) from
	// this goroutine: one left running would read the heap every millisecond for the rest of the
	// run, under every measurement after it, and its reading, which no one would take, is left
	// in peaked rather than blocking it there forever.
	stop, peaked := make(chan struct{}), make(chan uint64, 1)
	stopSampling := sync.OnceFunc(func() { close(stop) })
	defer stopSampling()
	go func() {
		peak := base
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				peaked <- max(peak, heapObjects())
				return
			case <-ticker.C:
				peak = max(peak, heapObjects())
			}
		}
	}()
	started := time.Now()
	kept := load()
	took := time.Since(started)
	stopSampling()
	peak := <-peaked
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(kept)
	return loadCost{took: took, allocated: allocated, peak: peak - base, held: int64(after.HeapAlloc) - int64(before.HeapAlloc)}
}
