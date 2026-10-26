package docs

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// What a browser's update adds to its room beyond what the rendering's count prices is charged to
// the room's structure part (roomCharge, peerWeighing.structureHeap): each new struct at
// bytesOfHeapAStruct, the splits integration can cause at the same price, and the content no
// rendering prices at its kind's heap a byte. These tests pin each price the way
// TestAResidentRoomsHeapPerByteOfItsMarkdown pins bytesOfHeapAnElement: they measure what one copy
// of a room keeps for frames ygo applies into it (heldByUpdates), and fail once a price leaves what
// it is set from by more than the tolerance each states. The room budget charges every price at
// both copies (roomCopies), since the replica takes the live document's whole state; what is
// measured here is one copy.

// heapPriceChild is the environment variable naming the test a process was started to measure
// alone (measuredAlone).
const heapPriceChild = "DISPATCH_HEAP_PRICE_CHILD"

// measuredAlone runs the calling test, or subtest, again in a process of its own and reports
// whether this is that process, the one to measure in; the caller returns when it is not. A
// collection costs as much as the heap the process holds, and a whole run of this package holds a
// gigabyte or more by the time these tests run: on this pod the three of them took 108, 75 and
// 16 s inside the package's run, against 2.5, 1.4 and 0.2 s in a process of their own, with the
// same readings. The process runs the one test of that exact name, each level of it matched whole.
// The test that started the process logs what the process printed and the process's peak resident
// memory, and fails when it failed. The peak is the kernel's for the process, which folds in the
// peak of the memory the process was started from (Go starts it sharing this process's memory
// until its exec), so it reads no less than this process's own peak at the start, and bounds the
// process's own from above.
func measuredAlone(t *testing.T) bool {
	t.Helper()
	if os.Getenv(heapPriceChild) == t.Name() {
		return true
	}
	levels := strings.Split(t.Name(), "/")
	for i, level := range levels {
		levels[i] = "^" + regexp.QuoteMeta(level) + "$"
	}
	child := exec.Command(os.Args[0], "-test.run="+strings.Join(levels, "/"), "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), heapPriceChild+"="+t.Name())
	output, err := child.CombinedOutput()
	t.Logf("measured in a process of its own:\n%s", output)
	if child.ProcessState != nil {
		if usage, ok := child.ProcessState.SysUsage().(*syscall.Rusage); ok {
			t.Logf("the process's peak resident memory, at least this process's own: %d KiB", usage.Maxrss)
		}
	}
	if err != nil {
		t.Fatalf("the measurement in a process of its own failed: %v", err)
	}
	return false
}

// measuredUpdate is one apply heldByUpdates measures: update into the document room makes.
type measuredUpdate struct {
	room   func() *crdt.Doc
	update []byte
}

// heldByUpdates is the heap one copy of a room keeps once it has applied each update, every room
// made fresh: what stays allocated once collections have freed everything else the apply made. It
// follows heldByResidentRoom (room_heap_test.go:87): each reading follows two collections, since a
// sync.Pool's contents survive the first, and the least of the passes' readings is taken, since
// what anything else allocates meanwhile only adds to a reading. Each pass makes every room before
// its first reading and keeps them all until its end, so each reading after an apply is the next
// apply's reading before it, two collections an apply rather than four; and two passes are taken
// rather than three, since the readings agree to the byte from one pass to the next. The updates
// are allocated before any reading starts, so what a room keeps of a frame itself is not counted:
// ygo copies every string and binary out of the frame, which is what the content prices are of.
// Its callers measure in a process of their own (measuredAlone), whose collections are cheap.
func heldByUpdates(t *testing.T, measured []measuredUpdate) []int64 {
	t.Helper()
	held := make([]int64, len(measured))
	for i := range held {
		held[i] = math.MaxInt64
	}
	collected := func() int64 {
		var stats runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&stats)
		return int64(stats.HeapAlloc)
	}
	for range 2 {
		rooms := make([]*crdt.Doc, len(measured))
		for i, apply := range measured {
			rooms[i] = apply.room()
		}
		before := collected()
		for i, apply := range measured {
			if err := crdt.ApplyUpdateV1(rooms[i], apply.update, nil); err != nil {
				t.Fatalf("apply update %d: %v", i, err)
			}
			after := collected()
			held[i] = min(held[i], after-before)
			before = after
		}
		runtime.KeepAlive(rooms)
	}
	return held
}

// weighedFresh is the hook's walk of update as a browser's update to a room holding none of it.
func weighedFresh(t *testing.T, update []byte) peerWeighing {
	t.Helper()
	weighed := peerWeighing{held: crdt.StateVector{}}
	if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
		t.Fatalf("walk the update: %v", err)
	}
	return weighed
}

// Every kind of struct a browser's update can carry keeps no more than bytesOfHeapAStruct in one
// copy of its room, and the heaviest keeps at least 0.8 of it, so the price neither under-charges
// any kind nor drifts far past the one it is set from. Each kind is measured in a frame of
// structsAKind structs of the least content it can carry, directly under the fragment where it has
// a parent, and as entries of keys of their own under the margin; the walk refuses subdocuments and
// moves, and never integrates a skip, so those are not here.
func TestTheRoomBudgetsPricePerStructHoldsEveryKind(t *testing.T) {
	if !measuredAlone(t) {
		return
	}
	const structsAKind, least = 20_000, 0.8
	fresh := func() *crdt.Doc { return newDocumentCopy() }
	item := func(kind structKind, content func(v1Frame) v1Frame) []byte {
		return fragmentItems(structsAKind, kind, content)
	}
	typed := func(ref byte, name string) []byte {
		return item(structType, func(f v1Frame) v1Frame {
			f = f.raw(ref)
			if name != "" {
				f = f.str(name)
			}
			return f
		})
	}
	// keyed is structsAKind entries of keys of their own under the margin, content writing each:
	// what an XML element's attribute is to its element, under a root the walk names.
	keyed := func(kind structKind, content func(v1Frame) v1Frame) []byte {
		f := newV1Frame(1).group(structsAKind, 7, 0)
		for i := range structsAKind {
			f = content(f.rooted(byte(kind), marksMapName, fmt.Sprintf("k%d", i)))
		}
		return f.noDeletes()
	}
	rows := []struct {
		kind   string
		update []byte
	}{
		{"a GC run", gcFrameFrom(7, structsAKind)},
		{"a deleted item", item(structDeleted, func(f v1Frame) v1Frame { return f.uint(1) })},
		{"a string", item(structString, func(f v1Frame) v1Frame { return f.str("a") })},
		{"a binary", item(structBinary, func(f v1Frame) v1Frame { return f.str("") })},
		{"an embed", item(structEmbed, func(f v1Frame) v1Frame { return f.str("0") })},
		{"a format", item(structFormat, func(f v1Frame) v1Frame { return f.str("b").str("true") })},
		{"an Any value", item(structAny, func(f v1Frame) v1Frame { return f.uint(1).any(nil) })},
		{"a JSON value", item(structJSON, func(f v1Frame) v1Frame { return f.uint(1).str("0") })},
		{"an array", typed(0, "")},
		{"a map", typed(1, "")},
		{"a text", typed(2, "")},
		{"an XML element", typed(3, "p")},
		{"an XML fragment", typed(4, "")},
		{"an XML hook", typed(5, "h")},
		{"an XML text", typed(6, "")},
		{"an Any value as an entry of its own key", keyed(structAny, func(f v1Frame) v1Frame { return f.uint(1).any(nil) })},
		{"a string as an entry of its own key", keyed(structString, func(f v1Frame) v1Frame { return f.str("a") })},
		{"a map as an entry of its own key", keyed(structType, func(f v1Frame) v1Frame { return f.raw(1) })},
		{"an XML element as an entry of its own key", keyed(structType, func(f v1Frame) v1Frame { return f.raw(3).str("p") })},
	}
	measured := make([]measuredUpdate, len(rows))
	for i, row := range rows {
		if weighed := weighedFresh(t, row.update); weighed.structs != structsAKind {
			t.Fatalf("%s: the walk found %d new structs, want %d", row.kind, weighed.structs, structsAKind)
		}
		measured[i] = measuredUpdate{fresh, row.update}
	}
	heaviest, heaviestKind := 0.0, ""
	for i, held := range heldByUpdates(t, measured) {
		kind := rows[i].kind
		perStruct := float64(held) / structsAKind
		t.Logf("%s: %.1f bytes of heap a struct", kind, perStruct)
		if perStruct > bytesOfHeapAStruct {
			t.Errorf("%s keeps %.1f bytes of heap a struct, past the %d the room budget charges", kind, perStruct, bytesOfHeapAStruct)
		}
		if perStruct > heaviest {
			heaviest, heaviestKind = perStruct, kind
		}
	}
	t.Logf("the heaviest struct is %s, %.1f bytes, %.2f of bytesOfHeapAStruct", heaviestKind, heaviest, heaviest/bytesOfHeapAStruct)
	if heaviest < least*bytesOfHeapAStruct {
		t.Errorf("the heaviest struct, %s, keeps %.1f bytes of heap, under %.1f of the %d the room budget charges each", heaviestKind, heaviest, least, bytesOfHeapAStruct)
	}
}

// What integration splits is charged at bytesOfHeapAStruct a split, splitsAStructCanCause of them
// for each new struct and for each delete range (peerWeighing.added). A one-character insert whose
// origins fall inside the room's string splits it, and keeps no more than a struct and its splits
// are charged; a delete range inside the string splits it at both ends, and keeps no more than its
// two splits are charged. A split itself, what such a delete range keeps for each of its two,
// keeps no more than a struct's price and at least a fifth of it: past that the margin charges
// splits so far past what they cost that it is to be measured again.
func TestTheRoomBudgetsSplitMarginHoldsWhatSplitsAdd(t *testing.T) {
	if !measuredAlone(t) {
		return
	}
	const n, least = 20_000, 0.2
	text := fragmentItems(1, structString, func(f v1Frame) v1Frame { return f.str(strings.Repeat("a", 2*n)) })
	holdingText := func() *crdt.Doc {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, text, nil); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	between := newV1Frame(1).group(n, 8, 0)
	for i := range n {
		between.raw(byte(structString) | infoHasOrigin | infoHasRightOrigin).uint(7).uint(uint64(2 * i)).uint(7).uint(uint64(2*i + 1)).str("b")
	}
	inserts := between.noDeletes()
	ranges := deleteRangesFrom(7, n)

	held := heldByUpdates(t, []measuredUpdate{{holdingText, inserts}, {holdingText, ranges}})
	perInsert, perRange := float64(held[0])/n, float64(held[1])/n
	perSplit := perRange / splitsAStructCanCause
	t.Logf("an insert splitting the room's string keeps %.1f bytes, a delete range splitting it at both ends %.1f, %.1f a split, %.2f of bytesOfHeapAStruct",
		perInsert, perRange, perSplit, perSplit/bytesOfHeapAStruct)
	if charged := float64((1 + splitsAStructCanCause) * bytesOfHeapAStruct); perInsert > charged {
		t.Errorf("an insert splitting the room's string keeps %.1f bytes, past the %.0f a struct and its splits are charged", perInsert, charged)
	}
	if charged := float64(splitsAStructCanCause * bytesOfHeapAStruct); perRange > charged {
		t.Errorf("a delete range splitting the room's string keeps %.1f bytes, past the %.0f its splits are charged", perRange, charged)
	}
	if perSplit > bytesOfHeapAStruct || perSplit < least*bytesOfHeapAStruct {
		t.Errorf("a split keeps %.1f bytes of heap, want %.0f to %d", perSplit, least*bytesOfHeapAStruct, bytesOfHeapAStruct)
	}
}

// Content no rendering prices keeps no more than the structure part charges for it: each frame's
// heap in one copy is within its structs at bytesOfHeapAStruct and its content at its kind's heap
// a byte (peerWeighing.content). And each factor is tight: the heaviest shape charged at it keeps,
// beyond the structs' price, at least a stated share of the factor for each byte. Text - a string,
// a binary, an XML type's name and a map key - is a copy of its bytes in an allocation Go rounds up
// to its size class, or past the largest class to whole 8 KiB pages, the heaviest a text just past
// 32 KiB, 1.24 bytes a byte; the factor is the next whole byte, and the heaviest is held to half
// of it. Values - Any, JSON, an embed, a format - decode into Go maps, slices and interfaces, the
// heaviest a map for the fewest bytes: lib0 Any maps of one entry nested in each other, which a
// JSON item read as legacy Any weighs the same as; JSON text is lighter, its objects nested the
// same way the heaviest of it. The heaviest is held to 0.8 of the factor. The kinds the rendering
// prices directly under the fragment are measured as entries under the margin, where the walk
// charges them.
func TestTheRoomBudgetsContentFactorsHoldEveryKind(t *testing.T) {
	if !measuredAlone(t) {
		return
	}
	fresh := func() *crdt.Doc { return newDocumentCopy() }
	big := strings.Repeat("a", 64<<10)
	pastAPage := strings.Repeat("a", 32<<10+1)
	margin := func(n int, kind structKind, content func(v1Frame) v1Frame) []byte {
		f := newV1Frame(1).group(n, 7, 0)
		for i := range n {
			f = content(f.rooted(byte(kind), marksMapName, fmt.Sprintf("k%d", i)))
		}
		return f.noDeletes()
	}
	nestedMaps := func(depth int) any {
		var v any
		for range depth {
			v = map[string]any{"": v}
		}
		return v
	}
	nestedJSON := strings.Repeat(`{"":`, 99) + "0" + strings.Repeat("}", 99)
	legacyJSON := newV1Frame(1).group(1, 7, 0).rooted(byte(structJSON), marksMapName, "k").uint(500)
	for range 500 {
		legacyJSON.any(nestedMaps(99))
	}
	longKeys := newV1Frame(1).group(1024, 7, 0)
	for i := range 1024 {
		longKeys.rooted(byte(structAny), marksMapName, fmt.Sprintf("%08d", i)+strings.Repeat("k", 1016)).uint(1).any(nil)
	}
	const (
		text = iota
		values
		mixed
	)
	least := map[int]float64{text: 0.5, values: 0.8}
	tightest := map[int]float64{}
	rows := []struct {
		name   string
		factor int
		update []byte
	}{
		{"strings of 64 KiB under the margin", text, margin(16, structString, func(f v1Frame) v1Frame { return f.str(big) })},
		{"strings of 32,769 bytes under the margin", text, margin(31, structString, func(f v1Frame) v1Frame { return f.str(pastAPage) })},
		{"binaries of 64 KiB", text, fragmentItems(16, structBinary, func(f v1Frame) v1Frame { return f.str(big) })},
		{"XML elements named in 64 KiB under the margin", text, margin(16, structType, func(f v1Frame) v1Frame { return f.raw(3).str(big) })},
		{"margin entries of 1 KiB keys", mixed, longKeys.noDeletes()},
		{"Any maps of one entry nested 99 deep", values, marginValues(500, nestedMaps(99))},
		{"Any maps of one null entry", values, marginValues(25_000, map[string]any{"": nil})},
		{"Any nulls", values, marginValues(100_000, nil)},
		{"Any strings of 64 KiB", values, marginValues(16, big)},
		{"JSON objects of one entry nested 99 deep", values, marginJSON(500, nestedJSON)},
		{"JSON read as legacy Any maps nested 99 deep", values, legacyJSON.noDeletes()},
		{"embeds of JSON objects nested 99 deep under the margin", values, margin(500, structEmbed, func(f v1Frame) v1Frame { return f.str(nestedJSON) })},
		{"formats of JSON objects nested 99 deep under the margin", values, margin(500, structFormat, func(f v1Frame) v1Frame { return f.str("b").str(nestedJSON) })},
	}
	measured := make([]measuredUpdate, len(rows))
	for i, row := range rows {
		measured[i] = measuredUpdate{fresh, row.update}
	}
	for i, held := range heldByUpdates(t, measured) {
		row := rows[i]
		weighed := weighedFresh(t, row.update)
		charged := int64(weighed.structs*bytesOfHeapAStruct + weighed.content)
		structs := int64(weighed.structs * bytesOfHeapAStruct)
		var priced int64
		switch row.factor {
		case text:
			priced = int64(weighed.content / bytesOfHeapAByteOfText)
		case values:
			priced = int64(weighed.content / bytesOfHeapAByteOfValues)
		}
		t.Logf("%s: %d bytes of heap for %d structs, charged %d a copy (%.2f of it)", row.name, held, weighed.structs, charged, float64(held)/float64(charged))
		if held > charged {
			t.Errorf("%s keep %d bytes of heap in one copy, past the %d the structure part charges a copy", row.name, held, charged)
		}
		if priced > 0 {
			perByte := float64(held-structs) / float64(priced)
			t.Logf("%s: %.2f bytes of heap a byte beyond the structs' price", row.name, perByte)
			tightest[row.factor] = max(tightest[row.factor], perByte)
		}
	}
	for factor, set := range map[int]float64{text: bytesOfHeapAByteOfText, values: bytesOfHeapAByteOfValues} {
		if tightest[factor] < least[factor]*set {
			t.Errorf("the heaviest shape charged at %v bytes a byte keeps %.2f a byte beyond its structs, under %.1f of it", set, tightest[factor], least[factor])
		}
	}
}
