package docs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// boundRoomDocument sets on doc, a document holding the state a room's load decoded, the scan
// budget a room's load sets (newRoomScans) and returns it, for a test or benchmark that measures a
// room's document without the replica every room keeps (keepReplica, which sets the room's own):
// every remote apply on doc is then whole or not at all, as a room's is, and one of no browser's
// may scan perFrameScanCeiling items.
func boundRoomDocument(doc *crdt.Doc) *crdt.Doc {
	doc.SetIntegrateScanBudget((&roomScans{}).budget)
	return doc
}

// scanMeasurement is the environment variable that runs this file's slow measurements.
const scanMeasurement = "DISPATCH_MEASURE_SCAN_TOTAL"

// longSessionKeystrokes is how many keystrokes each of the honest long session's two editors
// makes: characters, the typos and backspaces that go with them, and the moves of the cursor a
// person makes to add a word a few characters back (composing).
const longSessionKeystrokes = 50_000

// longSessionPlaceCharacters is how many characters each editor composes at one place of the
// honest long session before both move on to the next paragraph.
const longSessionPlaceCharacters = 1_000

// anHonestLongSession is two people editing seed, scanFixtureSeed's document, for a long session,
// the session the room's scan total is fifteen times (roomScanTotal): two browsers, each making
// longSessionKeystrokes keystrokes as a person composes text (composing), both at the same place
// of one paragraph at a time and moving on to the next paragraph together every
// longSessionPlaceCharacters characters, as two people working through a document side by side
// do. Each keystroke is one update the room takes as it is made, and each browser sees the other's
// keystrokes between 5 and 20 updates late, drawn afresh at each keystroke. The client of the
// second browser is the higher, so its items scan on past the first's at the same place
// (sjawhar/ygo v1.54.1-sami.1, crdt/item.go:170-179).
//
// Each place is driven in documents of its own, fresh from seed, and handed to take as one place's
// updates in the order its room takes them. A conflict scan passes only the items between an
// item's left and right origins, which are neighbours at its place (crdt/item.go:117-192), so a
// keystroke at one place scans what the other editor typed there and nothing of any other place:
// what a room's applies scan at one place is the same whether the room holds the session's other
// places or not, and the session's total is the sum of its places'. Driving them in one document
// instead costs every keystroke an encode of the whole document's delete set and store, which ygo
// writes into each transaction's update (crdt/doc.go:597-612): the session in one document had not
// finished after twenty minutes of one processor.
func anHonestLongSession(t *testing.T, seed []byte, take func(place int, update []byte)) {
	t.Helper()
	lags := rand.New(rand.NewPCG(1, 2))
	made := 0
	for place := 0; made < longSessionKeystrokes; place++ {
		browsers := [2]*scanBrowser{newScanBrowser(t, seed, scanFirstClient), newScanBrowser(t, seed, scanSecondClient)}
		paragraph := place % len(paragraphTexts(browsers[0].doc))
		// unseen holds, for each browser, the other's updates it has not received yet, oldest
		// first.
		var unseen [2][][]byte
		var keys [2][]func(*scanBrowser)
		for j, browser := range browsers {
			browser.at(paragraph, scanGap)
			keys[j] = composing(longSessionPlaceCharacters)
		}
		for i := 0; i < len(keys[0]) && made < longSessionKeystrokes; i++ {
			made++
			for j, browser := range browsers {
				keys[j][i](browser)
			}
			for j, browser := range browsers {
				for _, update := range browser.sent {
					take(place, update)
					unseen[1-j] = append(unseen[1-j], update)
				}
				browser.sent = nil
			}
			for j, browser := range browsers {
				for lag := 5 + lags.IntN(16); len(unseen[j]) > lag; unseen[j] = unseen[j][1:] {
					browser.receive(unseen[j][0])
				}
			}
		}
	}
}

// The room's scan total holds fifteen honest long sessions: two editors, each making 50,000
// keystrokes at the same places, each seeing the other's 5 to 20 updates late, whose updates the
// room takes one at a time, scan so few items between them that fifteen such sessions stay within
// roomScanTotal; and no one update scans past usedUpScanBudget, so a room whose total is used up
// still takes every keystroke of such a session. Each place's updates go to a room's bounded
// document of its own, fresh from the seed (anHonestLongSession says why the sum is the session's).
// It logs what the session's states re-scan past the seed's own re-scan, the count a reload starts
// the room's total at (roomScans.rescanned), beside what the rooms' applies scanned. Set
// DISPATCH_MEASURE_SCAN_TOTAL=1 to run it: 26 s here. Run on 2026-10-30: 97,600 updates at 38
// places scanned 42,401 items, at most 9 in one, re-scanning as 42,587; fifteen times 42,401 is
// 636,015, within the 1,000,000 of roomScanTotal. It read the same scans and the same ceiling on
// 2026-10-28 under sjawhar/ygo v1.51.3-sami.7, re-scanning as 42,611.
func TestTheRoomScanTotalHoldsFifteenHonestLongSessions(t *testing.T) {
	if os.Getenv(scanMeasurement) == "" {
		t.Skip("set " + scanMeasurement + "=1 to run the long session's measurement")
	}
	seed := scanFixtureSeed(t, 24<<10)
	seedScans, err := rescan(seed)
	if err != nil {
		t.Fatal(err)
	}
	var admitted, most, updates int
	var rescanned uint64
	var room *crdt.Doc
	roomPlace := -1
	finishPlace := func() {
		if room == nil {
			return
		}
		scans, err := rescan(crdt.EncodeStateAsUpdateV1(room, nil))
		if err != nil {
			t.Fatal(err)
		}
		rescanned += scans - seedScans
	}
	started := time.Now()
	anHonestLongSession(t, seed, func(place int, update []byte) {
		if place != roomPlace {
			finishPlace()
			room, roomPlace = newDocumentCopy(), place
			if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
				t.Fatal(err)
			}
			boundRoomDocument(room)
			room.OnAfterTransaction(func(txn *crdt.Transaction) {
				admitted += txn.IntegrateScanned()
				most = max(most, txn.IntegrateScanned())
			})
		}
		updates++
		if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
			t.Fatalf("the room refused update %d: %v", updates, err)
		}
	})
	finishPlace()
	t.Logf("%d updates at %d places in %s scanned %d items, at most %d in one, fifteen times %d against the %d total; the session's states re-scan %d",
		updates, roomPlace+1, time.Since(started).Round(time.Second), admitted, most, 15*admitted, roomScanTotal, rescanned)
	if 15*admitted > roomScanTotal {
		t.Errorf("the long session scanned %d items, and fifteen times it passes the %d scan total", admitted, roomScanTotal)
	}
	if most > usedUpScanBudget {
		t.Errorf("one update of the long session scanned %d items, past the %d a used-up room takes", most, usedUpScanBudget)
	}
}

// No document the server's own writes and people's editing store loads with half its room's scan
// total used: the re-scan each load starts the room's total at (rescan, roomScans.rescanned) of
// the heaviest document the server's writes store (heaviestDocument), of the document an agent
// rewrote whole until the decode cap refused a write (agentRewrittenState), and of the long-edited
// 5 KiB document typed on until its state reached the decode cap (longEditedAtCapState) is each at
// most half of roomScanTotal, so each leaves people at least half the total to edit it with. Set
// DISPATCH_MEASURE_SCAN_TOTAL=1 to run it: 159 s here with the store, the agent's rewrites 150 s
// of them. Run on 2026-10-30: the heaviest document's 5,360,879-byte state, the long-edited one's
// 14,427,285-byte state and the agent-rewritten document's 12,630,814-byte state, 28 whole
// rewrites, re-scan no item at all, each far within half the 1,000,000 total, 500,000. On
// 2026-10-28, under sjawhar/ygo v1.51.3-sami.7, the same three states re-scanned 49,156, 10 and
// 475,136 items; the figure this test holds to is the half, not the reading.
func TestNoStoredHeavyDocumentLoadsWithHalfItsScanTotalUsed(t *testing.T) {
	if os.Getenv(scanMeasurement) == "" {
		t.Skip("set " + scanMeasurement + "=1 to run the heavy documents' re-scans")
	}
	for _, row := range []struct {
		name  string
		state func(t *testing.T) []byte
	}{
		{"the heaviest document the server's writes store", func(t *testing.T) []byte {
			return crdt.EncodeStateAsUpdateV1(heaviestDocument(t), nil)
		}},
		{"an agent's whole rewrites until the cap refused one", func(t *testing.T) []byte {
			state, refusal := agentRewrittenState(t)
			if !errors.Is(refusal, ErrDocumentTooLarge) {
				t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
			}
			return state
		}},
		{"a long-edited document typed on until the cap", longEditedAtCapState},
	} {
		t.Run(row.name, func(t *testing.T) {
			state := row.state(t)
			started := time.Now()
			scans, err := rescan(state)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("a %d-byte state re-scans %d items in %s, against half the %d scan total, %d",
				len(state), scans, time.Since(started).Round(time.Millisecond), roomScanTotal, roomScanTotal/2)
			if scans > roomScanTotal/2 {
				t.Errorf("the document's load re-scans %d items, past half the %d scan total", scans, roomScanTotal)
			}
		})
	}
}

// A copy of a room at its scan total costs a copy of the same state with nothing to scan and about
// a second of scans more, the architect's rule for the total: the run of a million keystrokes and
// the same run once three frames scanned it whole (keystrokeRunStates), whose state re-scans
// 1,000,002 distinct items, the whole of roomScanTotal, each decoded as a room's load decodes its
// stored state (decodeStoredState) at GOMAXPROCS=1 (measureLoad). The scanned copy takes at most
// 1.5 s more than the unscanned one, and allocates at most twice bytesOfAScanMark more for each
// item its scans pass, which is what ygo keeps a mark for (crdt/item.go:126-158). Set
// DISPATCH_MEASURE_SCAN_TOTAL=1 to run it: 11 s here. Run on 2026-10-30: the scanned copy
// re-scanned 1,000,002 items in 2.217 s, allocating 456,926,192 bytes with a peak of 433,741,040
// above the heap before it; the unscanned one, no item, 1.419 s, 382,005,224 and 353,177,568:
// 0.798 s and 74,920,968 bytes more, 74.9 bytes a scan.
func TestACopyOfARoomAtTheScanTotalCostsACopyOfItsStateAndAboutASecondOfScans(t *testing.T) {
	if os.Getenv(scanMeasurement) == "" {
		t.Skip("set " + scanMeasurement + "=1 to run the copy measurement")
	}
	keystrokes, scanned := keystrokeRunStates(t)
	copyOf := func(state []byte) (loadCost, uint64) {
		var scans uint64
		cost := measureLoad(t, func() any {
			decoded, err := decodeStoredState(state)
			if err != nil {
				t.Fatal(err)
			}
			scans = decoded.scans
			return decoded.doc
		})
		return cost, scans
	}
	plain, plainScans := copyOf(keystrokes)
	atTotal, totalScans := copyOf(scanned)
	for _, copied := range []struct {
		name  string
		state []byte
		cost  loadCost
		scans uint64
	}{{"unscanned", keystrokes, plain, plainScans}, {"scanned", scanned, atTotal, totalScans}} {
		t.Logf("the %s copy of a %d-byte state re-scans %d items: %s, %d bytes allocated, %d peak, %d held",
			copied.name, len(copied.state), copied.scans, copied.cost.took.Round(time.Millisecond), copied.cost.allocated, copied.cost.peak, copied.cost.held)
	}
	if totalScans < roomScanTotal || plainScans >= honestHandoffScans {
		t.Fatalf("the scanned state re-scans %d items and the unscanned one %d, want the first at the %d total and the second under %d",
			totalScans, plainScans, roomScanTotal, honestHandoffScans)
	}
	if more := atTotal.took - plain.took; more > 1500*time.Millisecond {
		t.Errorf("the copy at the total took %s more than the unscanned one, past 1.5 s", more.Round(time.Millisecond))
	}
	if more := int64(atTotal.allocated) - int64(plain.allocated); more > int64(2*bytesOfAScanMark*totalScans) {
		t.Errorf("the copy at the total allocated %d bytes more than the unscanned one, past twice %d for each of its %d scans", more, bytesOfAScanMark, totalScans)
	}
}

// chainedRun applies to doc count one-character items of client, each after the one before and
// the first after after, each in a remote transaction of its own, as a room takes each of a
// person's keystrokes: ygo merges a run of characters only within one transaction (sjawhar/ygo
// v1.54.1-sami.1, crdt/transaction.go:242-247), so each stays an item of its own, and a scan from the
// run's first item passes count-1 distinct items. doc must have no observer yet, which would pay
// for every transaction.
func chainedRun(tb testing.TB, doc *crdt.Doc, after crdt.ID, client crdt.ClientID, count int) {
	tb.Helper()
	for clock := range count {
		origin := after
		if clock > 0 {
			origin = crdt.ID{Client: client, Clock: uint64(clock - 1)}
		}
		frame := newV1Frame(1).group(1, uint64(client), uint64(clock)).
			raw(byte(structString) | infoHasOrigin).uint(uint64(origin.Client)).uint(origin.Clock).str("a").noDeletes()
		if err := crdt.ApplyUpdateV1(doc, frame, nil); err != nil {
			tb.Fatal(err)
		}
	}
}

// lastCharacter is the ID of the last character of doc's first paragraph.
func lastCharacter(tb testing.TB, doc *crdt.Doc) crdt.ID {
	tb.Helper()
	text := paragraphTexts(doc)[0]
	last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
	if last == nil {
		tb.Fatal("the paragraph's last character has no item")
	}
	return *last
}

// An apply just under the per-frame ceiling holds its room's lock under 500 ms on one processor,
// the bound past which the ceiling would have gone back to the architect: a frame of scanningFrame
// items each naming the first item of a run the room took a keystroke at a time and no right
// origin, applied to a room's bounded document with its replica, as a room keeps it
// (observedReplica) and its update observer moves it, by the budget's allowance for an apply of no
// browser's, perFrameScanCeiling. One frame scans a run of 400,000 distinct items once; the other a
// run of 2,000 items two hundred times. lock-ms is the time from the apply's start to the room's
// first update observer, which ygo runs once the transaction has released the document's lock
// (sjawhar/ygo v1.54.1-sami.1, crdt/doc.go:805-813); ns/scan and the bytes a scanned item adds,
// against a frame of one item that scans nothing, are the per-item costs perFrameScanCeiling and
// bytesOfAScanMark quote. Run it with -benchtime=1x: each iteration builds its room afresh, a
// keystroke at a time.
func BenchmarkAnApplyJustUnderTheScanCeiling(b *testing.B) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, shape := range []struct {
		name         string
		run, scanned int
	}{
		{"a run of 400,000 distinct items scanned once", perFrameScanCeiling, 1},
		{"a run of 2,000 items scanned 200 times", 2_000, perFrameScanCeiling / 2_000},
	} {
		b.Run(shape.name, func(b *testing.B) {
			const scanner, runner crdt.ClientID = 4_000_000, 1_000
			var lockMost, quietMost, perScanMost time.Duration
			var scans int
			var scanAlloc, quietAlloc uint64
			for range b.N {
				b.StopTimer()
				live, replica, unlocked := scanCeilingRoom(b, runner, shape.run)
				frame := scanningFrame(scanner, crdt.ID{Client: runner}, shape.scanned)
				scanned := 0
				live.OnAfterTransaction(func(txn *crdt.Transaction) { scanned = txn.IntegrateScanned() })
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				b.StartTimer()
				started := time.Now()
				err := crdt.ApplyUpdateV1(live, frame, nil)
				b.StopTimer()
				runtime.ReadMemStats(&after)
				if err != nil {
					b.Fatalf("the room refused a frame just under the ceiling: %v", err)
				}
				locked := unlocked.Sub(started)
				lockMost, scans = max(lockMost, locked), scanned
				scanAlloc = after.TotalAlloc - before.TotalAlloc
				// The same apply of a frame of one item at the run's end, which scans nothing: what
				// the room's lock costs any apply, its observers' encode of the room's whole store
				// included, which the scans' share is told from.
				quiet := newV1Frame(1).group(1, uint64(scanner+1), 0).
					raw(byte(structString) | infoHasOrigin).uint(uint64(runner)).uint(uint64(shape.run - 1)).str("q").noDeletes()
				runtime.GC()
				runtime.ReadMemStats(&before)
				*unlocked = time.Time{}
				started = time.Now()
				if err := crdt.ApplyUpdateV1(live, quiet, nil); err != nil {
					b.Fatal(err)
				}
				quietLocked := unlocked.Sub(started)
				runtime.ReadMemStats(&after)
				quietAlloc = after.TotalAlloc - before.TotalAlloc
				quietMost = max(quietMost, quietLocked)
				perScanMost = max(perScanMost, (locked-min(quietLocked, locked))/time.Duration(max(scans, 1)))
				runtime.KeepAlive(replica)
				b.StartTimer()
			}
			if scans > perFrameScanCeiling || scans < perFrameScanCeiling-shape.run {
				b.Fatalf("the frame scanned %d items, want just under the %d ceiling", scans, perFrameScanCeiling)
			}
			b.ReportMetric(float64(lockMost.Microseconds())/1000, "lock-ms")
			b.ReportMetric(float64(quietMost.Microseconds())/1000, "quiet-lock-ms")
			b.ReportMetric(float64(scans), "scanned-items")
			b.ReportMetric(float64(perScanMost.Nanoseconds()), "ns/scan")
			b.ReportMetric(float64(scanAlloc-min(quietAlloc, scanAlloc))/float64(scans), "B/scanned-item")
			if lockMost >= 500*time.Millisecond {
				b.Errorf("an apply just under the ceiling held its room's lock %s, past the 500 ms bound", lockMost)
			}
		})
	}
}

// scanCeilingRoom is BenchmarkAnApplyJustUnderTheScanCeiling's room: a title and a paragraph with
// run characters of runner chained onto its end a keystroke at a time (chainedRun), its replica
// kept as a room keeps it and moved by an update observer as a room's is, after a first observer
// that records when it ran, the moment the apply released the room's lock.
func scanCeilingRoom(b *testing.B, runner crdt.ClientID, run int) (*crdt.Doc, *renderedReplica, *time.Time) {
	b.Helper()
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		b.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		b.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
		b.Fatal(err)
	}
	chainedRun(b, room, lastCharacter(b, room), runner, run)
	rendering, err := renderDocument(room)
	if err != nil {
		b.Fatal(err)
	}
	replica := observedReplica(b, &Service{}, room, &rendering)
	unlocked := new(time.Time)
	room.OnUpdate(func([]byte, any) {
		if unlocked.IsZero() {
			*unlocked = time.Now()
		}
	})
	room.OnUpdate(func([]byte, any) { replica.observe("bench", nil) })
	return room, replica, unlocked
}

// An apply's scans never pass its allowance, the drain of the parked queue included: a browser's
// update that fills the gap parked items wait on, whose own item scans nothing but whose drain
// would scan past what is left of the update's allowance, applies with the drain taken back, the
// parked items parked as they were, and the room's scan total grown by what the apply's scans
// passed, no more than its allowance.
func TestAnApplyNeverScansMoreThanItsAllowanceDrainIncluded(t *testing.T) {
	const typed, waiting = 2_000, 20
	service, room, replica, _ := tangledRoom(t, typed)
	// waiting items of client 9 from clock 1, each naming the typist's first character as its
	// left origin and no right origin, so each would scan the typed run; client 9's clock 0 is
	// missing, so the room parks them.
	parked := newV1Frame(1).group(waiting, 9, 1)
	for range waiting {
		parked.raw(byte(structString) | infoHasOrigin).uint(uint64(tangledTypist)).uint(0).str("z")
	}
	if err, called := weighInto(service, room, parked.noDeletes()); err != nil || !called {
		t.Fatalf("the hook answered the parked items %v (applied %t), want them admitted", err, called)
	}
	if pending := room.PendingStats(); pending.Items != waiting {
		t.Fatalf("the room parked %d items, want %d", pending.Items, waiting)
	}
	// Client 9's clock 0, after the typist's last character, where nothing follows it to scan.
	filler := newV1Frame(1).group(1, 9, 0).
		raw(byte(structString) | infoHasOrigin).uint(uint64(tangledTypist)).uint(typed - 1).str("f").noDeletes()
	allowance := int64(browserScanCeiling(t, room, filler))
	if waiting*(typed-1) <= int(allowance) {
		t.Fatalf("the parked items would scan %d items, want past the filler's %d allowance", waiting*(typed-1), allowance)
	}
	before, _ := replica.scans.figures()
	if err, called := weighInto(service, room, filler); err != nil || !called {
		t.Fatalf("the hook answered the filler %v (applied %t), want it admitted with its drain taken back", err, called)
	}
	after, _ := replica.scans.figures()
	t.Logf("the filler's apply scanned %d items against its allowance of %d", after-before, allowance)
	if after-before > allowance || after == before {
		t.Errorf("the filler's apply added %d items to the room's scan total, want some and no more than its %d allowance", after-before, allowance)
	}
	if held := room.StateVector()[9]; held != 1 {
		t.Errorf("the room holds client 9 to clock %d, want the filler's 1", held)
	}
	if pending := room.PendingStats(); pending.Items != waiting {
		t.Errorf("the room parks %d items after the drain was taken back, want the %d it parked", pending.Items, waiting)
	}
	runtime.KeepAlive(replica)
}

// A room's scan total survives a reload: a room takes browsers' updates whose scans add to its
// total, flushes them to the store and closes; the room its next load makes starts its total at
// what the stored state re-scans, which is what the room had come to, give or take the few
// hundred items a decode of a state merges where the room took keystrokes one at a time.
func TestTheRoomScanTotalSurvivesAReload(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Title\n\nOne paragraph of ordinary prose.\n")
	ctx := context.Background()
	load := func() (*crdt.Doc, *renderedReplica) {
		t.Helper()
		if err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatalf("load the document's room: %v", err)
		}
		room := roomDocument(t, service, artifactID)
		replica := service.liveReplica(room)
		if replica == nil {
			t.Fatal("the loaded room keeps no replica")
		}
		return room, replica
	}
	room, replica := load()
	loaded, _ := replica.scans.figures()
	const typed = 300
	typeOnto(t, service, room, tangledTypist, typed)
	// Four frames of fifty items, each of a client of its own, each item naming the typist's first
	// character: each scans the typed run and the items earlier frames left after it, within the
	// allowance of a frame the hook does not weigh.
	for client := crdt.ClientID(9); client < 13; client++ {
		if err, called := weighInto(service, room, scanningFrame(client, crdt.ID{Client: tangledTypist}, 50)); err != nil || !called {
			t.Fatalf("the hook answered client %d's frame %v (applied %t), want it admitted", client, err, called)
		}
	}
	live, _ := replica.scans.figures()
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	stored, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualCanonical(t, stored.Update, state) {
		t.Fatal("the store does not hold what the room held when it closed")
	}
	reloadedRoom, reloadedReplica := load()
	reloaded, _ := reloadedReplica.scans.figures()
	t.Logf("the room loaded at %d scans, came to %d, and reloaded at %d", loaded, live, reloaded)
	if live-loaded < 50_000 {
		t.Fatalf("the room's browsers scanned %d items, want enough to tell a reload that kept them from one that did not", live-loaded)
	}
	if difference := reloaded - live; difference < -500 || difference > 500 {
		t.Errorf("the reloaded room's scan total is %d, want within 500 of the %d the room had come to", reloaded, live)
	}
	runtime.KeepAlive(reloadedRoom)
}

// bytesEqualCanonical reports whether two states hold the same structs and deletions, however
// their updates were folded: each decoded into a copy of its own and encoded again.
func bytesEqualCanonical(t *testing.T, a, b []byte) bool {
	t.Helper()
	canonical := func(state []byte) string {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
		return string(crdt.EncodeStateAsUpdateV1(doc, nil))
	}
	return canonical(a) == canonical(b)
}

// A server write is never refused by the room's scan total: in a room whose total is used up, the
// server's publish of a write (publishLiveUpdate) - an agent's edit inserting words where a person
// composed 400 characters meanwhile, so its scan passes every item they typed there, past the
// allowance a browser's apply gets in a used-up room - applies with the per-frame ceiling, the
// room is not failed and holds both, the agent's words after the person's, and the publish's scans
// count toward the room's total.
func TestAServerWriteIsNeverRefusedByTheRoomsScanTotal(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha inserted words paragraph.",
	})
	if _, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent); err != nil {
		t.Fatalf("snapshot the edit: %v", err)
	}
	if err := edit.ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	live := roomDocument(t, service, artifactID)
	replica := service.liveReplica(live)
	if replica == nil {
		t.Fatal("the room keeps no replica")
	}
	// The room's load re-scanned a history that used its whole total, as a room past fifteen long
	// sessions loads.
	replica.scans.rescanned(roomScanTotal)
	// The person's client is the least there is, so the agent's write, whose client is random,
	// wins every tie-break at that place and its scan passes everything the person typed.
	person := newScanBrowser(t, crdt.EncodeStateAsUpdateV1(live, nil), 1)
	person.at(0, len("Alpha "))
	person.compose(composing(400))
	for _, update := range person.sent {
		if err := crdt.ApplyUpdateV1(live, update, person); err != nil {
			t.Fatalf("the room refused the person's keystroke: %v", err)
		}
	}
	typedText := person.text.ToString()
	typedText = typedText[len("Alpha ") : len(typedText)-len("paragraph.")]
	before, left := replica.scans.figures()
	if left > usedUpScanBudget {
		t.Fatalf("the room has %d scans left, want its total used up", left)
	}
	edit.ledger.publish()
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the publish failed the room: %v", err)
	}
	after, _ := replica.scans.figures()
	t.Logf("the publish and what ran beside it scanned %d items in a room with %d left", after-before, left)
	if after-before < 400 {
		t.Errorf("the room's scan total grew by %d with the publish, want past every one of the 400 items the person typed at its place", after-before)
	}
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if want := "Alpha " + typedText + "inserted words paragraph."; !strings.Contains(text, want) {
		t.Fatalf("the document reads %q, want it to hold %q: the person's text and then the agent's words", text, want)
	}
}

// A room whose scan total is used up still takes typing, and refuses a larger apply as needing a
// rebuild: a room loads a history whose re-scan came to the whole total (tangledHistory, its
// scanners about 2,000 scans each), and its total starts there as every load's does
// (roomScans.rescanned); a browser then types thirty characters onto the paragraph's end, each
// scanning nothing, and another browser's keystroke at the same place scans past them, under
// usedUpScanBudget, and both are admitted; a frame of three items at that place, whose scans pass
// usedUpScanBudget, is refused 4413 DOC_NEEDS_REBUILD and leaves the room as it was.
func TestAUsedUpRoomStillTakesTypingAndRefusesALargerApplyAsNeedingARebuild(t *testing.T) {
	state := tangledHistory(t, 2_000, roomScanTotal/2_000+2)
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	replica := service.keepReplica(room, nil, &rendering, nil)
	loaded, err := rescan(state)
	if err != nil {
		t.Fatal(err)
	}
	replica.scans.rescanned(loaded)
	if _, left := replica.scans.figures(); left > usedUpScanBudget {
		t.Fatalf("the tangled history re-scans %d items, leaving %d of the %d total: tangle it further", loaded, left, roomScanTotal)
	}

	const typist crdt.ClientID = 5
	const typed = 30
	typeOnto(t, service, room, typist, typed)
	keystroke := scanningFrame(9, crdt.ID{Client: typist}, 1)
	if err, called := weighInto(service, room, keystroke); err != nil || !called {
		t.Fatalf("the hook answered a keystroke at the typed place %v (applied %t), want it admitted", err, called)
	}

	larger := scanningFrame(10, crdt.ID{Client: typist}, 3)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	scannedBefore, _ := replica.scans.figures()
	err, called := weighInto(service, room, larger)
	var refusal *websocket.PeerClose
	if !called || !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentNeedsRebuildCloseReason {
		t.Fatalf("the hook answered the larger frame %v (applied %t), want it refused %d %q", err, called,
			contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
	}
	if string(crdt.EncodeStateAsUpdateV1(room, nil)) != string(before) {
		t.Error("the refused frame changed the room's document")
	}
	if scannedAfter, _ := replica.scans.figures(); scannedAfter-scannedBefore != usedUpScanBudget {
		t.Errorf("the refusal added %d items to the room's scan total, want the %d of a used-up room's allowance", scannedAfter-scannedBefore, usedUpScanBudget)
	}
	runtime.KeepAlive(replica)
}

// The budget answers each remote apply by its origin: a browser's apply its reserved allowance -
// the per-frame ceiling while what is left of the total exceeds it, what is left once that is
// less, and usedUpScanBudget once what is left is no more than that - and any other apply, the
// server's own, the per-frame ceiling whatever is left. Two allowances reserved at once each see
// the other's taken out of what is left.
func TestEachApplysAllowanceIsTheCeilingOrWhatIsLeftOfTheScanTotal(t *testing.T) {
	for _, row := range []struct {
		name               string
		used, ceiling      int64
		allowance, server  int
		neededRebuildFirst bool
	}{
		{"a room far from its total", 0, perFrameScanCeiling, perFrameScanCeiling, perFrameScanCeiling, false},
		{"a room nearer its total than the ceiling", roomScanTotal - 1_000, perFrameScanCeiling, 1_000, perFrameScanCeiling, false},
		{"an unweighed update in a room far from its total", 0, 27_000, 27_000, perFrameScanCeiling, false},
		{"a room with a used-up room's allowance left", roomScanTotal - usedUpScanBudget, perFrameScanCeiling, usedUpScanBudget, perFrameScanCeiling, true},
		{"a room past its total", roomScanTotal + 5_000, perFrameScanCeiling, usedUpScanBudget, perFrameScanCeiling, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			scans := newRoomScans(newDocumentCopy())
			scans.rescanned(uint64(row.used))
			browser := new(int)
			allowance := scans.reserve(browser, int(row.ceiling))
			if got := scans.budget(browser); got != row.allowance || allowance.roomUsedUp() != row.neededRebuildFirst {
				t.Errorf("the browser's apply is allowed %d (used up %t), want %d (%t)", got, allowance.roomUsedUp(), row.allowance, row.neededRebuildFirst)
			}
			if got := scans.budget(fmt.Sprintf("server %s", row.name)); got != row.server {
				t.Errorf("the server's apply is allowed %d, want %d", got, row.server)
			}
			scans.settle(allowance, nil)
			if used, _ := scans.figures(); used != row.used {
				t.Errorf("an apply that committed nothing left the total at %d, want %d", used, row.used)
			}
		})
	}
	t.Run("two allowances at once", func(t *testing.T) {
		scans := newRoomScans(newDocumentCopy())
		scans.rescanned(roomScanTotal - 300_000)
		first, second := new(int), new(int)
		a := scans.reserve(first, perFrameScanCeiling)
		b := scans.reserve(second, perFrameScanCeiling)
		if a.limit() != 300_000 || b.limit() != usedUpScanBudget {
			t.Errorf("the allowances are %d and %d, want the 300,000 left and then a used-up room's %d", a.limit(), b.limit(), usedUpScanBudget)
		}
		scans.settle(a, &crdt.RefusedUpdateError{Err: crdt.ErrIntegrateScanBudget, Scanned: a.limit()})
		scans.settle(b, nil)
		if used, left := scans.figures(); used != roomScanTotal || left != 0 {
			t.Errorf("the total is %d with %d left, want the refused apply's scans counted: %d and none", used, left, roomScanTotal)
		}
	})
}
