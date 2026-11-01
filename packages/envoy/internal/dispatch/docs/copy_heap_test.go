package docs

import (
	"context"
	"errors"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// copyHeapMeasurement is the environment variable that runs the copy-heap measurement's heavy
// rows (TestEveryFullStateDecodesTransientHeapIsWithinItsGrant).
const copyHeapMeasurement = "DISPATCH_MEASURE_COPY_HEAP"

// keystrokeRunStates are the states of a room that took a run of a million keystrokes, each its
// own item, after the first paragraph of a small document, and of the same room once three
// one-character frames each named a run item as its left origin and the run item a third of a
// million later as its right origin: a decode of the second scans 1,000,002 distinct items, each
// with its own scan mark, the whole of roomScanTotal, the heaviest shape a decode re-scans.
//
// The run is six keystrokes past a million so that those three frames carry the whole total
// themselves. Under sjawhar/ygo v1.51.3-sami.7 the seed document's own decode counted four scans,
// and a run of 1,000,000 reached the total with them; under v1.54.1-sami.1 that decode counts
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
			raw(byte(structString) | infoHasOrigin | infoHasRightOrigin).
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

// Every full-state decode the copy-heap bound grants holds no more heap above what it leaves behind
// than its grant (copyGrant.cover): copyHeapPerByte for each byte of the state it decodes, or the
// whole bound for one heavier than it, which then runs alone. For each shape, at GOMAXPROCS=1
// (measureLoad), it measures the room's load as the server's own work makes it (Server.Apply on a
// room no one holds, and the room's first transaction) and as a socket's admission and load make
// it; a write's fork from the store (coldFork) and from a resident room it has never forked
// (warmFork's rebuild); the replica's build from a base (renderedReplica.start); the measurer's
// copy for a server write (structsAfter); a rebuild's decode and re-scan of the history
// (decodeHistory, nearACap); and, with an empty update stored after the state, the room's load
// that folds the history first, a cold read's fold (LoadDocument) and compaction's (Compact). A
// fold is weighed by every stored update it folds, or by the state it makes where that is larger.
// Each path starts from the state as the whole stored history, since a closed room compacts what
// its own transactions stored into a state re-encoded from its decode.
//
// The fast row is a mebibyte of prose. Set DISPATCH_MEASURE_COPY_HEAP=1 for the heavy rows - the
// document an agent rewrote whole until the decode cap refused a write (agentRewrittenState, kept
// between runs in the file DISPATCH_AGENT_REWRITTEN_CACHE names), the long-edited document typed on
// until the cap (longEditedAtCapState), a run of a million keystrokes and the same run scanned
// once whole (keystrokeRunStates), each near or at the decode cap - every one of whose paths must
// also fit in the whole bound alone, since a stored document must always load. They took 597 to
// 623 s together here on 2026-10-28.
func TestEveryFullStateDecodesTransientHeapIsWithinItsGrant(t *testing.T) {
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
			measureCopyPaths(t, shape.state(t), shape.heavy)
		})
	}
}

// measureCopyPaths measures every path TestEveryFullStateDecodesTransientHeapIsWithinItsGrant
// names on a document storing state, and fails a path whose transient passes its grant, or, where
// atCap, one whose transient passes the whole bound.
func measureCopyPaths(t *testing.T, state []byte, atCap bool) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs in the room while it is measured
	service.measurer.interval = time.Hour
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
			replica := &renderedReplica{measurer: &service.measurer, markdown: &markdown}
			replica.start(artifactID, state)
			if replica.doc == nil {
				t.Fatal("the replica took no copy of the base")
			}
			return replica
		}},
		{"the measurer's copy for a server write", func() { fresh(); live = resident() }, liveState, func() any {
			return service.measurer.structsAfter(live, serverWrite{apply: func(*crdt.Doc) {}})
		}},
		{"a rebuild's decode and re-scan of the history", func() {}, stored, func() any {
			history, err := decodeHistory(state)
			if err != nil {
				t.Fatal(err)
			}
			nearACap(history)
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
		// The cold read loaded no room, so the history it folded is still the two updates.
		{"compaction's fold of the history", func() {}, history, func() any {
			if folded, err := service.persistence.Compact(ctx, artifactID, compactKeep); err != nil || folded != 1 {
				t.Fatalf("compaction folded %d updates (%v), want 1", folded, err)
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
		grant := min(copyHeapWeight(bytes), copyHeapBound)
		t.Logf("%d-byte state, %s: %s, %d bytes allocated, %d peak, %d held; %d transient, %.1f a byte of %d, against a %d-byte grant",
			len(state), path.name, cost.took.Round(time.Millisecond), cost.allocated, cost.peak, cost.held,
			transient, float64(transient)/float64(bytes), bytes, grant)
		if transient > grant {
			t.Errorf("%s holds %d bytes above what it leaves, %.1f a byte of its %d, past its %d-byte grant",
				path.name, transient, float64(transient)/float64(bytes), bytes, grant)
		}
		if atCap && transient > copyHeapBound {
			t.Errorf("%s of a document at the decode cap holds %d bytes above what it leaves, past the %d-byte bound",
				path.name, transient, int64(copyHeapBound))
		}
	}
}

// useCopyHeaps makes heaps the process's copy-heap bound for the rest of t (podCopyHeaps).
func useCopyHeaps(t *testing.T, heaps *copyHeaps) *copyHeaps {
	t.Helper()
	previous := podCopyHeaps.Swap(heaps)
	t.Cleanup(func() { podCopyHeaps.Store(previous) })
	return heaps
}

// densestMarkdownOf is bytes of the densest markdown this repository measures, empty items of
// alternating bullet lists, each item a list of its own (bytesOfHeapAByteAtTheDensest).
func densestMarkdownOf(bytes int) string {
	const pair = "- \n\n* \n\n"
	return strings.Repeat(pair, bytes/len(pair))
}

// storeVersionMarkdown stores markdown as room's next version, as an upload's insert stores one
// (api/artifacts.go:296), leaving the document's stored history as it is: what a count of that
// version parses is the markdown, whatever the history holds.
func storeVersionMarkdown(t *testing.T, database *store.Store, room, markdown string) int64 {
	t.Helper()
	var number int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, coalesce(max(number), 0) + 1, $2, '[]' from artifact_versions where artifact_id = $1
		returning number
	`, room, markdown).Scan(&number); err != nil {
		t.Fatalf("store a version: %v", err)
	}
	return number
}

// storedCountGrant is what a count of room's latest version is granted of the copy-heap bound
// (storedCountWeight), read as the count weighs itself (weighStoredCount).
func storedCountGrant(t *testing.T, database *store.Store, room string, number int64) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, markdown, row, err := weighStoredCount(ctx, tx, room, number)
	if err != nil {
		t.Fatal(err)
	}
	return storedCountWeight(markdown, row)
}

// A count of an admission's stored version holds no more heap while it runs than the grant it
// takes of the copy-heap bound (storedCountWeight): its markdown's bytes at storedCountHeapPerByte
// up to storedCountHeapAtMost, and the largest row of the history it walks at
// storedCountHeapPerStoredRowByte. The count leaves nothing behind but two integers it keeps
// (storedCounts.put), so what it held above the heap before it is what the grant must cover.
//
// It measures, at GOMAXPROCS=1 with the collector at copyMeasurementGCPercent so the sampled peak
// is what is live (measureLoad), a mebibyte of prose, the densest markdown measured
// (bytesOfHeapAByteAtTheDensest) at a mebibyte, 64 KiB and 4 KiB, where the small shapes run
// denser a byte because goldmark's guard stops the parse of a large one (nodeGuard), a mebibyte of
// hard breaks, the shape that guard stops latest, and a paragraph typed into 100,000 times, whose
// history is one row of over a mebibyte against a rendering of a few hundred bytes: the shape the
// walk, not the parse, is weighed for.
func TestAStoredVersionsCountHoldsNoMoreThanItsGrant(t *testing.T) {
	skipUnderRace(t)
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	shapes := []struct {
		name     string
		markdown string
		history  func() []byte
	}{
		{"a mebibyte of prose", residentRoomMarkdown(t, 1<<20), func() []byte {
			state, _ := residentRoomState(t, 1<<20)
			return state
		}},
		{"a mebibyte of the densest markdown", densestMarkdownOf(1 << 20), nil},
		{"64 KiB of the densest markdown", densestMarkdownOf(64 << 10), nil},
		{"4 KiB of the densest markdown", densestMarkdownOf(4 << 10), nil},
		{"a mebibyte of hard breaks", strings.Repeat("a\\\nb\\\n", 1<<20/6), nil},
		{"a paragraph typed into 100,000 times", "# Title\n\nOne paragraph of ordinary prose.\n", func() []byte {
			return typedBeforeState(t, 100_000)
		}},
	}
	rooms := createDocuments(t, database, len(shapes))
	for index, shape := range shapes {
		room := rooms[index]
		if shape.history != nil {
			storeHistory(t, service, room, shape.history())
		}
		number := storeVersionMarkdown(t, database, room, shape.markdown)
		grant := storedCountGrant(t, database, room, number)
		cost := func() loadCost {
			defer debug.SetGCPercent(debug.SetGCPercent(copyMeasurementGCPercent))
			return measureLoad(t, func() any {
				heap, err := service.countStoredVersion(context.Background(), room, number)
				if err != nil {
					t.Fatalf("count %s: %v", shape.name, err)
				}
				return heap
			})
		}()
		held := int64(cost.peak)
		t.Logf("%s, %d bytes of markdown: %s, %d allocated, %d peak, %.1f a byte of its markdown, against a %d-byte grant",
			shape.name, len(shape.markdown), cost.took.Round(time.Millisecond), cost.allocated, held,
			float64(held)/float64(len(shape.markdown)), grant)
		if held > grant {
			t.Errorf("the count of %s held %d bytes, %.1f a byte of its %d of markdown, past its %d-byte grant",
				shape.name, held, float64(held)/float64(len(shape.markdown)), len(shape.markdown), grant)
		}
	}
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

// heldGrants records what every grant of a copy-heap bound holds as it is granted (granted).
type heldGrants struct {
	mu   sync.Mutex
	held []int64
}

func (g *heldGrants) record(held int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = append(g.held, held)
}

func (g *heldGrants) all() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.held)
}

// A room's load folds a history of several updates on the grant it covers for the fold, and
// decodes and hands ygo the state on that same grant: with the rest of the bound taken the moment
// the load holds its grant, before its fold decodes anything, the load finishes - no part of it
// acquires again - and gives its grant back, leaving the bound to the holder that filled it.
func TestALoadWhoseFoldRunsWhileTheCopyBoundIsFullFoldsOnTheLoadsOwnGrant(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 64<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	history := storedUpdateBytes(t, service.store, artifactID)
	folded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if len(folded.Update) > history {
		t.Fatalf("the %d-byte history folds into a %d-byte state, past it: the load's decode would weigh more than its fold", history, len(folded.Update))
	}
	weight := copyHeapWeight(history)
	heaps := useCopyHeaps(t, &copyHeaps{size: 2 * weight})
	var grants heldGrants
	var filled int64
	heaps.granted = func(held int64) {
		grants.record(held)
		if filled == 0 {
			filled = heaps.limit() - held
			if !heaps.semaphore().TryAcquire(filled) {
				t.Errorf("the bound had no %d bytes left beside the load's %d", filled, held)
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
		heaps.semaphore().Release(filled)
		t.Fatal("the load did not finish in 30 s while the rest of the bound was held: it waits for a second grant inside its own")
	}
	defer heaps.semaphore().Release(filled)
	if got := grants.all(); len(got) != 1 || got[0] != weight {
		t.Fatalf("the load was granted %v, want one grant of the %d bytes its %d-byte history weighs", got, weight, history)
	}
	if filled != weight {
		t.Fatalf("the rest of the bound held %d bytes beside the load, want the %d it left", filled, weight)
	}
	if roomDocument(t, service, artifactID) == nil {
		t.Fatal("the room did not load")
	}
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d bytes of the bound once loaded", held)
	}
}

// A copy whose weight passes the whole bound is never refused, since a stored document must always
// load: it waits while any part of the bound is held, then holds all of it, so it runs alone.
func TestACopyHeavierThanTheWholeCopyBoundWaitsForItToEmptyAndThenHoldsAllOfIt(t *testing.T) {
	service, artifactID := newTestService(t)
	state := proseState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	heaps := useCopyHeaps(t, &copyHeaps{size: copyHeapWeight(len(state)) / 2})
	if !heaps.semaphore().TryAcquire(1) {
		t.Fatal("the empty bound refused a byte")
	}
	waited := make(chan int64, 1)
	var waitOnce sync.Once
	heaps.waiting = func(weight int64) { waitOnce.Do(func() { waited <- weight }) }
	var grants heldGrants
	heaps.granted = grants.record
	loaded := make(chan error, 1)
	go func() { loaded <- loadRoomOf(service, artifactID) }()
	select {
	case weight := <-waited:
		if weight != heaps.limit() {
			t.Errorf("the load waits for %d bytes, want the whole %d-byte bound", weight, heaps.limit())
		}
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while a byte of the bound was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the load neither waited nor finished in 30 s")
	}
	select {
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while a byte of the bound was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	heaps.semaphore().Release(1)
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("the load failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the load did not finish in 30 s once the bound was empty")
	}
	if got := grants.all(); len(got) != 1 || got[0] != heaps.limit() {
		t.Fatalf("the load was granted %v, want the whole %d-byte bound once", got, heaps.limit())
	}
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d bytes of the bound once loaded", held)
	}
}

// Loads of six documents at once, against a bound with room for two of them, never hold more of
// it together than it has, and every one of them loads: the third waits for one of the two to
// give its grant back. Each grant is held a tenth of a second past its cover, so the loads
// overlap.
func TestConcurrentLoadsNeverHoldMoreOfTheCopyBoundThanItHas(t *testing.T) {
	service, first := newTestService(t)
	state := proseState(t, 16<<10)
	rooms := append([]string{first}, createDocuments(t, service.store, 5)...)
	for _, room := range rooms {
		storeHistory(t, service, room, state)
	}
	weight := copyHeapWeight(len(state))
	heaps := useCopyHeaps(t, &copyHeaps{size: 2*weight + weight/2})
	var most atomic.Int64
	heaps.granted = func(held int64) {
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
	if got := most.Load(); got > heaps.limit() || got < 2*weight {
		t.Fatalf("the loads held %d bytes of the bound at most, want two %d-byte grants at once and never past the %d-byte bound",
			got, weight, heaps.limit())
	}
	for _, room := range rooms {
		if roomDocument(t, service, room) == nil {
			t.Fatalf("room %s did not load", room)
		}
	}
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the loads still hold %d bytes of the bound once loaded", held)
	}
}

// A socket's admission waits for the bound within its request: one whose request ends while its
// fold waits inside the read's transaction answers the request's error, fails no room, and holds
// nothing of the bound or of the store's connections, so the next admission reads and decodes the
// history once the bound has room.
func TestAnAdmissionWhoseRequestEndsWhileItWaitsForTheCopyBoundAnswersItsErrorAndHoldsNothing(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	heaps := useCopyHeaps(t, &copyHeaps{size: 4 * copyHeapWeight(storedUpdateBytes(t, service.store, artifactID))})
	if !heaps.semaphore().TryAcquire(heaps.limit()) {
		t.Fatal("the empty bound refused all of itself")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	heaps.waiting = func(int64) { waitOnce.Do(func() { close(waiting) }) }
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
		t.Fatal("the admission did not wait for the full bound in 30 s")
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
	if held := heaps.held.Load(); held != 0 {
		t.Fatalf("the ended admission holds %d bytes of the bound", held)
	}
	heaps.semaphore().Release(heaps.limit())
	_, preload, err := service.loadTree(ctx, artifactID)
	if err != nil || preload == nil || preload.decoded.doc == nil {
		t.Fatalf("the next admission answered %v with preload %v, want the history decoded", err, preload)
	}
	service.leavePreload(artifactID, preload)()
	if !heaps.semaphore().TryAcquire(heaps.limit()) {
		t.Fatal("part of the bound is still held once both admissions have answered")
	}
	heaps.semaphore().Release(heaps.limit())
}
