package docs

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// What one open document costs the pod is the figure the room budget charges a room by, and the
// budget charges one price: the heap one copy of a document holds for each byte of its encoded
// state (bytesOfHeapAStateByte), at both copies a room keeps (roomCopies). This measures the room
// itself - the live document beside the rendered replica its update observer keeps while the room
// is resident (keepReplica, renderedReplica.observe), which every room holds from its load on -
// against the bytes of state the budget charges it by.
//
// The price replaced six of them, each of a different measure of a document: its elements, its
// structs held, its rendered bytes, its bytes at the densest, a struct added and a split. The
// encoded state is the one measure every shape's heap follows within one spread, and the one the
// room can keep without reading the document, since a whole-state encode costs 18.7 ms on a
// mebibyte of prose and nothing on the keystroke path may take one. So the shapes below are what
// that spread is measured over: prose at three sizes, read and edited; a small document whose
// paragraph took 100,000 characters before its last one and then lost them all, the densest
// measured; the long-edited fixture; a run of 300,000 keystrokes, the lightest; and one and eight
// long paragraphs each typed in one transaction, which ygo merges into a few items.
//
// Each shape must hold no more than the price for each byte of its state in each copy, which is
// what keeps the budget's arithmetic honest; and the heaviest of them must hold at least
// leastOfThePrice of it, which is what keeps the price from being set so far above every shape
// that the budget charges rooms many times what they cost.
//
// Each row is measured in a process of its own (measuredAlone): every row collects twelve times,
// and inside the package's whole run each collection walks the gigabyte or more that run holds by
// then, which took the prose rows 318 s on this pod on 2026-11-03 against 11 s alone, with the
// same readings. The row's state is built in that process alone, so the run itself builds none.
//
// Set DISPATCH_MEASURE_STRUCT_HEAP=1 for the heavy rows, each at the decode cap or a million
// keystrokes long: the document an agent rewrote whole until the cap refused a write
// (agentRewrittenState, about 200 s to build, kept between runs in the file
// DISPATCH_AGENT_REWRITTEN_CACHE names), the long-edited document typed on until the cap
// (longEditedAtCapState) and a run of a million keystrokes (keystrokeRunStates).
func TestAResidentRoomsHeapPerByteOfItsState(t *testing.T) {
	skipUnderRace(t)
	const leastOfThePrice = 0.75
	heavy := os.Getenv(structHeapMeasurement) != ""
	type shape struct {
		name   string
		heavy  bool
		edited bool
		state  func(t *testing.T) []byte
	}
	shapes := []shape{
		{"a paragraph typed before its last character 100,000 times and deleted", false, false, func(t *testing.T) []byte {
			return typedBeforeState(t, 100_000)
		}},
		{"the long-edited document", false, false, func(t *testing.T) []byte { return longEditedState(t) }},
		{"a run of 300,000 keystrokes", false, false, func(t *testing.T) []byte { return typedRunState(t, 300_000) }},
		{"one paragraph of a mebibyte typed in one transaction", false, false, func(t *testing.T) []byte { return longRunState(t, 1) }},
		{"eight paragraphs of 128 KiB, each typed in one transaction", false, false, func(t *testing.T) []byte { return longRunState(t, 8) }},
		{"an agent's whole rewrites until the cap refused one", true, false, cachedAgentRewrittenState},
		{"a long-edited document typed on until the cap", true, false, longEditedAtCapState},
		{"a run of a million keystrokes", true, false, func(t *testing.T) []byte {
			keystrokes, _ := keystrokeRunStates(t)
			return keystrokes
		}},
	}
	for _, size := range []int{51 << 10, 524 << 10, 1 << 20} {
		for _, edited := range []bool{false, true} {
			name := fmt.Sprintf("%d KiB of prose, read", size>>10)
			if edited {
				name = fmt.Sprintf("%d KiB of prose, edited", size>>10)
			}
			shapes = append(shapes, shape{name, false, edited, func(t *testing.T) []byte {
				state, _ := residentRoomState(t, size)
				return state
			}})
		}
	}
	// heaviest is what the shapes held at their most, a share of the price, and heaviestShape
	// which one that is. Each shape is measured in a process of its own, so each reading is read
	// back out of what that process printed (heapReadingLine): the heaviest of them is known only
	// here, where the readings meet. A run that measured every shape it did not skip is the only
	// one that can weigh the price against the heaviest of them, so a run of one of them - the
	// process a shape is measured in, which runs this test with that shape alone - weighs none.
	heaviest, heaviestShape, read := 0.0, "", 0
	measured := 0
	for _, shape := range shapes {
		if shape.heavy && !heavy {
			t.Run(shape.name, func(t *testing.T) { t.Skip("set " + structHeapMeasurement + "=1 to run the heavy rows") })
			continue
		}
		measured++
		t.Run(shape.name, func(t *testing.T) {
			output, here := measuredAloneOutput(t)
			if !here {
				perStateByte, ok := heapReading(output)
				if !ok {
					t.Fatalf("the measurement in a process of its own printed no reading (%q)", heapReadingLine)
				}
				read++
				if perStateByte > heaviest {
					heaviest, heaviestShape = perStateByte, shape.name
				}
				return
			}
			state := shape.state(t)
			held := heldByResidentRoom(t, state, shape.edited)
			perStateByte := float64(held) / float64(int64(len(state))*roomCopies)
			t.Logf("%d B of state; heap held %d B, %.2f B a state byte for each of %d copies, %.3f of the %d-byte price",
				len(state), held, perStateByte, roomCopies, perStateByte/bytesOfHeapAStateByte, bytesOfHeapAStateByte)
			fmt.Printf("%s%.6f\n", heapReadingLine, perStateByte)
			if perStateByte > bytesOfHeapAStateByte {
				t.Errorf("the room holds %.2f B of heap a state byte for each copy, past the %d the room budget charges: measure the price again",
					perStateByte, bytesOfHeapAStateByte)
			}
		})
	}
	if read != measured {
		return
	}
	t.Logf("the heaviest of the %d shapes measured is %s at %.2f B a state byte, %.3f of the %d-byte price",
		read, heaviestShape, heaviest, heaviest/bytesOfHeapAStateByte, bytesOfHeapAStateByte)
	if heaviest < leastOfThePrice*bytesOfHeapAStateByte {
		t.Errorf("the heaviest shape, %s, holds %.2f B of heap a state byte, under %.2f of the %d the room budget charges: the price is set far past every shape",
			heaviestShape, heaviest, leastOfThePrice, bytesOfHeapAStateByte)
	}
}

// heapReadingLine is how a process measuring one shape's heap reports its reading to the process
// that started it (measuredAloneOutput), which is where the shapes' readings meet.
const heapReadingLine = "heap-bytes-a-state-byte: "

// heapReading is the reading a measuring process printed, and whether it printed one.
func heapReading(output string) (float64, bool) {
	for line := range strings.SplitSeq(output, "\n") {
		printed, found := strings.CutPrefix(strings.TrimSpace(line), heapReadingLine)
		if !found {
			continue
		}
		reading, err := strconv.ParseFloat(printed, 64)
		if err != nil {
			continue
		}
		return reading, true
	}
	return 0, false
}

// structHeapMeasurement is the environment variable that runs the heavy rows of
// TestAResidentRoomsHeapPerByteOfItsState, each a document at or near the decode cap.
const structHeapMeasurement = "DISPATCH_MEASURE_STRUCT_HEAP"

// typedRoomHeapMeasurement is the environment variable that runs
// TestATypedAndPastedRoomIsChargedAtLeastItsHeapAndWithinTheFactor, about thirteen minutes.
const typedRoomHeapMeasurement = "DISPATCH_MEASURE_TYPED_ROOM_HEAP"

// A room typed into is charged no less than the heap it holds, and no more than the factor the
// shapes' own measurement allows. The budget charges a room its tracked state, which its drain
// raises by the bytes of every update the room commits (renderedReplica.chargeAdded), and an
// update's bytes carry what its transaction deleted beside what it added, where the state itself
// merges a run of typed characters into one item: so a room typed into is tracked above the state
// it holds, which is the safe side for a budget and is what this weighs. The shapes' measurement
// holds the heaviest of them to leastOfThePrice of the price (TestAResidentRoomsHeapPerByteOfItsState),
// so a priced shape may be charged up to 1/0.75 times its heap, and that is the factor here: the
// room's charge is at least its heap and its heap at least leastHeldOfCharge of its charge.
//
// The room is a mebibyte of prose (residentRoomState), loaded as a room's load leaves it - the
// live document, and the load's own decode of the stored state as the replica, its update log
// open and its update observer registered (onLoadDocument) - and typed into as a person types:
// 2,000 one-character keystrokes in a hundred runs of twenty, each run in the middle of a
// paragraph of its own across the document, and a paste of 4 KiB after every tenth run, each in a
// paragraph of its own, every one a browser's update the peer hook weighs
// (Service.weighPeerUpdate) and the observer renders and charges (renderedReplica.observe).
//
// The room is then weighed as the shapes' measurement weighs one (heldByResidentRoom): what stays
// allocated after two collections, a first build of the same state discarded beforehand, both
// copies and everything the room holds besides. It takes one reading rather than the least of
// three, since each build is the 2,010 updates' run; the readings of a room's heap agree to the
// byte from one build to the next in a process of its own, and this one is measured in one
// (measuredAlone). One difference of HeapAlloc can still carry garbage the build left behind, so
// the reading is cross-checked against what a collection frees once every reference to the room
// is dropped (runtime.KeepAlive, placed so the room lives through the reading of its charge and
// no longer), which what else the process allocates or frees meanwhile does not move: the two
// must agree within a hundredth, and the heap the factor is weighed against is not trusted
// otherwise. The state and the updates, allocated before the first reading, are kept past the
// last; freed between the readings they would be taken off the room's heap.
func TestATypedAndPastedRoomIsChargedAtLeastItsHeapAndWithinTheFactor(t *testing.T) {
	const keystrokes, runs, pastes, pasteBytes, leastHeldOfCharge = 2_000, 100, 10, 4 << 10, 0.75
	if os.Getenv(typedRoomHeapMeasurement) == "" {
		t.Skip("set " + typedRoomHeapMeasurement + "=1 to run it, about thirteen minutes")
	}
	if !measuredAlone(t) {
		return
	}
	state, rendering := residentRoomState(t, 1<<20)
	updates := typedAndPastedUpdates(t, state, keystrokes, runs, pastes, pasteBytes)
	tracked := int64(len(state))
	for _, update := range updates {
		tracked += int64(len(update))
	}

	// The loaded room, untyped, weighed as the shapes' measurement weighs it; its first build is
	// the discarded one that keeps the decoder's one-time setup out of the typed room's reading.
	loaded := heldByResidentRoom(t, state, false)
	collected := func() int64 {
		var stats runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&stats)
		return int64(stats.HeapAlloc)
	}
	before := collected()
	started := time.Now()
	room := loadAndType(t, state, updates)
	took := time.Since(started)
	after := collected()
	weight := chargedWeight(room.charge)
	bytes, _ := room.replica.size()
	// The room lives through the readings of its charge and no longer, so what the collection
	// below frees is the room's own heap; the state and the updates outlive that collection, so
	// neither is freed into the figure the reading is cross-checked against.
	runtime.KeepAlive(room)
	dropped := collected()
	runtime.KeepAlive(state)
	runtime.KeepAlive(updates)
	held, freed := after-before, after-dropped

	t.Logf("%d B of markdown making %d elements; the loaded room holds %d B of heap", rendering.Bytes, rendering.Elements, loaded)
	t.Logf("%d updates (%d keystrokes in %d runs, %d pastes of %d B) in %s; the room renders %d B",
		len(updates), keystrokes, runs, pastes, pasteBytes, took.Round(time.Second), bytes)
	t.Logf("the typed room holds %d B of heap, %d more than loaded, and its drop frees %d, %d from that reading; it is tracked at %d B of state and charged %d B",
		held, held-loaded, freed, held-freed, tracked, weight)
	t.Logf("the charge is %.3fx the heap, want 1.000x to %.3fx (1/%.2f)", float64(weight)/float64(held), 1/leastHeldOfCharge, leastHeldOfCharge)
	if diff := held - freed; diff > held/100 || -diff > held/100 {
		t.Fatalf("the room reads %d B of heap and its drop frees %d, more than a hundredth apart: the two readings disagree, so the heap the factor is weighed against is not trustworthy",
			held, freed)
	}
	if weight != roomWeight(tracked) {
		t.Fatalf("the room is charged %d B, want the %d its %d tracked bytes of state come to", weight, roomWeight(tracked), tracked)
	}
	if weight < held {
		t.Errorf("the room is charged %d B for the %d B of heap it holds, under it", weight, held)
	}
	if float64(held) < leastHeldOfCharge*float64(weight) {
		t.Errorf("the room holds %d B of heap for the %d B it is charged, %.3f of it, want at least %.2f: the tracked state over-charges typing",
			held, weight, float64(held)/float64(weight), leastHeldOfCharge)
	}
}

// chargedWeight is what the budget charges charge now, read under the budget's lock.
func chargedWeight(charge *roomCharge) int64 {
	charge.budget.mu.Lock()
	defer charge.budget.mu.Unlock()
	return charge.weight
}

// typedResidentRoom is a room typed into (loadAndType): the room as heldByResidentRoom holds one,
// with its charge in the room budget.
type typedResidentRoom struct {
	residentRoom
	charge *roomCharge
}

// loadAndType loads state as a room's load does (onLoadDocument): the
// live document decoded from state, the load's own decode of it as the replica, the room's update
// log opened, the replica kept and its scan total set from that decode, the room charged at the
// bytes of the state it loaded in a budget of roomBudgetBytes, and an update observer on live
// that moves the replica through each commit, adding each committed update's bytes to the room's
// tracked state (renderedReplica.chargeAdded). It then hands each of updates to the peer hook as
// one browser's, applied as the room applies a peer's update.
// The room's update log is left open, as a room's load leaves it (onLoadDocument): it goes with
// the replica that drains it, which the room holds, so nothing outside the room holds the log and
// a room dropped is collected whole, which the heap reading's cross-check rests on.
func loadAndType(t *testing.T, state []byte, updates [][]byte) *typedResidentRoom {
	t.Helper()
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		t.Fatalf("decode the stored state: %v", err)
	}
	decoded, err := decodeStoredCopy(state, maxUpdateItems)
	if err != nil {
		t.Fatalf("decode the stored state's copy: %v", err)
	}
	service := calibratedService()
	service.budget.limit = roomBudgetBytes
	replica := service.keepReplica(live, live.OpenUpdateLog(), liveRendering(t, live), decoded.doc)
	replica.scans.rescanned(decoded.scans)
	charge := service.chargeLoadedRoom("room", replica, int64(len(state)))
	live.OnUpdate(func([]byte, any) { replica.observe("room") })
	browser := new(int)
	for index, update := range updates {
		info := websocket.PeerUpdateInfo{Room: "room", Doc: live, Update: update, Origin: browser}
		if err := service.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(live, update, browser) }); err != nil {
			t.Fatalf("update %d of %d: the hook answered %v, want it admitted", index+1, len(updates), err)
		}
	}

	return &typedResidentRoom{residentRoom: residentRoom{service: service, live: live, replica: replica}, charge: charge}
}

// typedAndPastedUpdates is the updates one browser sends typing into state as a person types:
// keystrokes one-character keystrokes in runs runs, each run at the middle of a paragraph of its
// own, one character after another, and pastes pastes of pasteBytes bytes, one after each
// runs/pastes runs, each at a third of a paragraph of its own, the paragraphs evenly spaced across
// the document. Each keystroke and each paste is a transaction of the browser's own copy, and its
// update is the one ygo encodes for it.
func typedAndPastedUpdates(t *testing.T, state []byte, keystrokes, runs, pastes, pasteBytes int) [][]byte {
	t.Helper()
	browser := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(browser, state, nil); err != nil {
		t.Fatal(err)
	}
	texts := paragraphTexts(browser)
	if len(texts) < runs+pastes {
		t.Fatalf("the document holds %d paragraphs, want at least the %d runs and pastes go to", len(texts), runs+pastes)
	}
	var updates [][]byte
	stop := browser.OnUpdate(func(update []byte, _ any) { updates = append(updates, update) })
	defer stop()
	paste := strings.Repeat("pasted ", pasteBytes/7) + strings.Repeat("x", pasteBytes%7)
	places := 0
	next := func() *crdt.YXmlText {
		text := texts[places*len(texts)/(runs+pastes)]
		places++
		return text
	}
	for run := range runs {
		text := next()
		at := text.Len() / 2
		for typed := range keystrokes / runs {
			browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, at+typed, "k", nil) })
		}
		if (run+1)%(runs/pastes) == 0 {
			pasted := next()
			browser.Transact(func(txn *crdt.Transaction) { pasted.Insert(txn, pasted.Len()/3, paste, nil) })
		}
	}
	if len(updates) != keystrokes+pastes {
		t.Fatalf("the browser made %d updates, want %d", len(updates), keystrokes+pastes)
	}
	return updates
}

// longRunState is the state of a small document whose paragraphs, paragraphs of them, together
// hold about a mebibyte of text, each typed in one transaction of its own a kibibyte at a time at
// its end: ygo merges each paragraph's run into one item as its transaction commits (squashRuns),
// so the document holds a few structs and a few elements whatever its length, and renders about
// its text's bytes.
func longRunState(t *testing.T, paragraphs int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# Title\n\n" + strings.Repeat("A paragraph.\n\n", paragraphs))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, seed, nil); err != nil {
		t.Fatal(err)
	}
	texts := paragraphTexts(doc)
	if len(texts) != paragraphs {
		t.Fatalf("the document holds %d paragraphs, want %d", len(texts), paragraphs)
	}
	chunk := strings.Repeat("words of one long paragraph ", 1<<10/28) + "word"
	each := (pmdoc.MaxDocumentBytes - 4<<10) / paragraphs
	for _, text := range texts {
		at := text.Len()
		doc.Transact(func(txn *crdt.Transaction) {
			for typed := 0; typed+len(chunk) <= each; typed += len(chunk) {
				text.Insert(txn, at, chunk, nil)
				at += len(chunk)
			}
		})
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// typedRunState is the state of a room that took a run of keystrokes keystrokes after the first
// paragraph of a small document, each its own item (chainedRun), as keystrokeRunStates builds its
// first.
func typedRunState(t *testing.T, keystrokes int) []byte {
	t.Helper()
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
	chainedRun(t, room, lastCharacter(t, room), 1_000, keystrokes)
	return crdt.EncodeStateAsUpdateV1(room, nil)
}

// typedBeforeState is the state of a small document whose first paragraph took typed characters,
// each typed before the one typed last, as longEditedAtCapState types them, and then lost them all
// again: a room holding typed structs, each its own item, that renders the document it began as.
func typedBeforeState(t *testing.T, typed int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, seed, nil); err != nil {
		t.Fatal(err)
	}
	text := paragraphTexts(doc)[0]
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	doc.Transact(func(txn *crdt.Transaction) {
		for range typed - 1 {
			text.Insert(txn, 1, "x", nil)
		}
	})
	doc.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, typed) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// residentRoom is what one open document holds in memory: the room's live document, and the
// replica its update observer renders and keeps from the room's load on (renderedReplica). The
// service is held because it is what lists the replica, and it lists it weakly (keepReplica), so
// nothing else here would keep that listing alive.
type residentRoom struct {
	service *Service
	live    *crdt.Doc
	replica *renderedReplica
}

// heldByResidentRoom is the heap a room loading state holds: what stays allocated once collections
// have freed everything else its decode, its copy and its render made. It follows the read cache's
// own measurement of what a structure holds (heldByDocumentRead): a
// first build of the same state, discarded, keeps the decoder's one-time setup out of the count;
// each reading follows two collections, since a sync.Pool's contents survive the first; and the
// least of three readings is taken, since what anything else allocates meanwhile only adds to a
// reading.
func heldByResidentRoom(t *testing.T, state []byte, edited bool) int64 {
	t.Helper()
	buildResidentRoom(t, state, edited)
	var room *residentRoom
	held := int64(math.MaxInt64)
	for range 3 {
		room = nil
		var before, after runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&before)
		room = buildResidentRoom(t, state, edited)
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&after)
		held = min(held, int64(after.HeapAlloc)-int64(before.HeapAlloc))
	}
	runtime.KeepAlive(room)
	return held
}

// buildResidentRoom loads state as a room's load decodes it twice, into the room's document and
// into the copy the load keeps as the replica (newDocumentCopy, decodeStoredCopy, keepReplica),
// and, for a room that has had an update, takes the replica as the room's observer moves it
// through that update instead: one drain of the room's update log, which copies the document from
// the log's base and renders it (observedReplica), a replica holding the same state.
func buildResidentRoom(t *testing.T, state []byte, edited bool) *residentRoom {
	t.Helper()
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		t.Fatalf("decode the stored state: %v", err)
	}
	room := &residentRoom{service: calibratedService(), live: live}
	if !edited {
		decoded, err := decodeStoredCopy(state, maxUpdateItems)
		if err != nil {
			t.Fatalf("decode the stored state's copy: %v", err)
		}
		room.replica = room.service.keepReplica(live, nil, nil, decoded.doc)
		return room
	}
	room.replica = observedReplica(t, room.service, live, nil)
	if _, measured := room.replica.size(); !measured {
		t.Fatal("the room's observer published no measurement, so it rendered no markdown and the replica this weighs is not a room's")
	}
	return room
}

// residentRoomState is the state a room holding about size bytes of markdown loads, with what
// that room's rendering measures (pmdoc.MeasureDocument, which is how an upload of the same
// markdown is weighed). Each reading decodes that state rather than building the document again,
// as the read cache's own measurement builds its read from a document decoded from its stored
// state, which holds strings of its own rather than the markdown's
// (buildTestDocumentRead).
func residentRoomState(t *testing.T, size int) (state []byte, rendering pmdoc.DocumentSize) {
	t.Helper()
	live := residentRoomDocument(t, size)
	markdown, err := renderDocument(live)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return crdt.EncodeStateAsUpdateV1(live, nil), pmdoc.MeasureDocument(markdown)
}

// residentRoomDocument is a live document of about size bytes of markdown a room can hold
// (residentRoomMarkdown), typed over as the live benchmarks' document is (benchmarkLiveDocument):
// 2,000 keystrokes typed and deleted across it, so its delete set is a document's that people
// have edited. It is bounded as a room's document is (boundRoomDocument).
func residentRoomDocument(tb testing.TB, size int) *crdt.Doc {
	tb.Helper()
	tree, err := pmdoc.Parse(residentRoomMarkdown(tb, size))
	if err != nil {
		tb.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		tb.Fatal(err)
	}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
		tb.Fatal(err)
	}
	texts := paragraphTexts(live)
	for keystroke := range 2_000 {
		text := texts[keystroke*7%len(texts)]
		live.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
		live.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, 1) })
	}
	return boundRoomDocument(live)
}

// residentRoomMarkdown is markdown of at least size bytes in the shape this package's live
// benchmarks type into - headings, marked paragraphs, lists, tables and code
// (benchmarkLiveDocument) - with paragraphs long enough that a room can hold a megabyte of it.
// That shape as the benchmarks build it makes 0.28 elements a byte, where a document at
// pmdoc.MaxDocumentBytes may hold pmdoc.MaxDocumentElements, one element per 16 bytes: 524 KiB of
// it measures 147,552 elements, which no room renders at all (renderDocument refuses it), so such
// a room would publish no measurement and its replica would keep no rendering. Lengthening the
// paragraphs leaves the shape and the elements it makes as they are and takes the density to
// 0.055 an element a byte, under a fifth of the bound's own; what the markdown measures is
// checked here rather than assumed, since a shape past either half of the bound would measure a
// room no document could fill.
func residentRoomMarkdown(tb testing.TB, size int) string {
	tb.Helper()
	padding := " " + strings.Repeat("word ", 279) + "word"
	var markdown, next strings.Builder
	for section := 0; ; section++ {
		next.Reset()
		fmt.Fprintf(&next, "## Section %d\n\n", section)
		fmt.Fprintf(&next, "Paragraph %d holds **bold text**, `inline code`, *emphasis* and a "+
			"[link](https://example.com/%d) among the ordinary words a spec's paragraph runs to.%s\n\n",
			section, section, padding)
		next.WriteString("- the first item\n- the second item with `code`\n- the third item\n\n")
		fmt.Fprintf(&next, "| Column | Value |\n| --- | --- |\n| row %d | cell |\n| other | cell |\n\n", section)
		next.WriteString("```go\nfunc example() int {\n\treturn 1\n}\n```\n\n")
		// The last section is left out rather than taken past size, since a document of
		// pmdoc.MaxDocumentBytes is the largest a room can hold and 1 MiB is one of the sizes
		// weighed here.
		if markdown.Len()+next.Len() > size {
			break
		}
		markdown.WriteString(next.String())
	}
	measured := pmdoc.MeasureDocument(markdown.String())
	if !measured.Counted || measured.Bytes > pmdoc.MaxDocumentBytes || measured.Elements > pmdoc.MaxDocumentElements {
		tb.Fatalf("%d bytes of this shape measure %+v, past a bound a room's own document obeys", size, measured)
	}
	return markdown.String()
}
