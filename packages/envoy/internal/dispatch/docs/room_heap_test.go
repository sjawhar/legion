package docs

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// What one open document costs the pod is the figure the room budget charges a room by
// (bytesOfHeapAnElement), and it was first read off a benchmark of a read's second replica rather
// than measured of a room: about 120 bytes of heap for every byte of markdown for one copy, and
// two copies for a room under edit (AGENTS.md:590-591). This measures the room itself: the live
// document beside the rendered replica its update observer keeps while the room is resident
// (keepReplica, renderedReplica.observe), which every room holds from its load on - the replica is
// the copy the load decoded of the stored state (decodeStoredState) - whether it is only read or
// has taken an update since. Each measures about two copies, the budget's roomCopies.
//
// Both are weighed against the room's rendering, the markdown its stored version holds. A byte of
// markdown is not what the heap follows, though: what it follows is the elements that markdown
// makes, about 470 bytes of heap for each one for each copy at 524 KiB and at 1 MiB, and 578 at 51
// KiB, where what a document holds whatever its size is a larger share of the whole. So the same
// bytes in a denser shape cost more, and the 120 bytes a byte the figure above states were measured
// on a shape four times denser than this one: the live benchmarks' own document
// (benchmarkLiveDocument), 0.28 elements a byte, which weighs 130 bytes of heap a byte at 51 KiB
// - the figure reproduced - and which no room could hold at 524 KiB or at 1 MiB, since a
// document may hold pmdoc.MaxDocumentElements and that shape passes them at about 490 KiB. The
// shape here (residentRoomMarkdown) is that one with longer paragraphs, 0.055 elements a byte,
// which a room holds at every size weighed. The per-byte bounds are wide enough for that spread and
// are not a number to the byte; the per-element bound is the budget's own, and fails once a copy's
// heap an element leaves leastPerElement to mostPerElement of bytesOfHeapAnElement.
func TestAResidentRoomsHeapPerByteOfItsMarkdown(t *testing.T) {
	const leastPerElement, mostPerElement = 0.85, 1.3
	for _, size := range []int{51 << 10, 524 << 10, 1 << 20} {
		state, rendering := residentRoomState(t, size)
		for _, room := range []struct {
			name                      string
			edited                    bool
			copies                    int
			leastPerByte, mostPerByte float64
		}{
			{"read", false, 2, 30, 120},
			{"edited", true, 2, 30, 120},
		} {
			t.Run(fmt.Sprintf("%dKiB/%s", size>>10, room.name), func(t *testing.T) {
				held := heldByResidentRoom(t, state, room.edited)
				perByte := float64(held) / float64(rendering.Bytes)
				perElement := float64(held) / float64(rendering.Elements*room.copies)
				t.Logf("%d B of markdown making %d elements, heap held %d B (%.1f B of heap a byte, %.0f B an element for each of %d copies)",
					rendering.Bytes, rendering.Elements, held, perByte, perElement, room.copies)
				if perByte < room.leastPerByte || perByte > room.mostPerByte {
					t.Errorf("the room holds %d B of heap for %d B of markdown, %.1f B a byte, want %.0f to %.0f a byte",
						held, rendering.Bytes, perByte, room.leastPerByte, room.mostPerByte)
				}
				if ratio := perElement / bytesOfHeapAnElement; ratio < leastPerElement || ratio > mostPerElement {
					t.Errorf("the room holds %.0f B of heap an element for each copy, %.2fx the %d B the room budget charges, want %.2fx to %.2fx",
						perElement, ratio, bytesOfHeapAnElement, leastPerElement, mostPerElement)
				}
			})
		}
	}
}

// structHeapMeasurement is the environment variable that runs the heavy rows of
// TestAResidentRoomsHeapPerStructOfItsState, each a document at or near the decode cap.
const structHeapMeasurement = "DISPATCH_MEASURE_STRUCT_HEAP"

// copyPrice is the term of copyHeap that prices a shape's copy: its elements, its structs or its
// rendered bytes.
type copyPrice int

const (
	pricedByElements copyPrice = iota
	pricedByStructs
	pricedByBytes
)

// A room's copy is priced at whichever of its three terms is the most (copyHeap), and each term's
// price neither under-charges the shapes it prices nor drifts far past what they hold: a copy holds
// bytesOfHeapAStructHeld or less for each struct of its state, and a struct-dense document at least
// 0.85 of it; and bytesOfHeapARenderedByte or less for each byte of its rendering, and a run of
// keystrokes at least 0.75 of it. It is measured as TestAResidentRoomsHeapPerByteOfItsMarkdown
// measures a room (heldByResidentRoom): a room as its load leaves it, the live document and the
// replica its load decoded, read and not edited, against the struct total its load counts
// (countRoomStructs) and its rendering, each shape in a process of its own (measuredAlone). Each
// shape is priced by the term it is listed under, which passes the other two: the struct-dense
// shapes at their structs; the long runs at their rendered bytes - runs of 300,000 and of a million
// keystrokes, which the decode of their state merges into a few structs (squashRuns), so they are
// struct-dense only in the room that typed them, and one paragraph of a mebibyte and eight of 128
// KiB, each typed in one transaction, which hold about a byte a byte and are checked only against
// the price's top; and a mebibyte of prose, whose structs carry its text, at its elements, which
// both other prices leave as they were. A run of keystrokes holds 9.2 to 11.1 bytes a rendered
// byte for each copy as its length moves (bytesOfHeapARenderedByte says why), so its least is
// 0.75 of the price.
//
// The fast rows are a mebibyte of prose, a small document typed into 100,000 times before its last
// character and then deleted (typedBeforeState), the long-edited fixture (longEditedState), a run
// of 300,000 keystrokes (typedRunState) and the two long paragraphs' shapes (longRunState), which
// took 4.0, 1.6, 8.8, 6.1 and 0.2 s each here on 2026-10-28, each in its own process. Set
// DISPATCH_MEASURE_STRUCT_HEAP=1 for the heavy rows, each at the decode cap or a million keystrokes
// long: the document an agent rewrote whole until the cap refused a write (agentRewrittenState,
// about 200 s to build, kept between runs in the file DISPATCH_AGENT_REWRITTEN_CACHE names), the
// long-edited document typed on until the cap (longEditedAtCapState) and a run of a million
// keystrokes (keystrokeRunStates), 26.9 to 29.4, 26.6 to 38.5 and 20.5 to 27.0 s with the agent's
// state cached, about 100 s together.
func TestAResidentRoomsHeapPerStructOfItsState(t *testing.T) {
	const dense, run, most = 0.85, 0.75, 1.0
	heavy := os.Getenv(structHeapMeasurement) != ""
	for _, shape := range []struct {
		name   string
		heavy  bool
		priced copyPrice
		// least is the least of its price the shape may hold for each copy.
		least float64
		state func(t *testing.T) []byte
	}{
		{"a mebibyte of prose", false, pricedByElements, 0, func(t *testing.T) []byte {
			state, _ := residentRoomState(t, 1<<20)
			return state
		}},
		{"a paragraph typed before its last character 100,000 times and deleted", false, pricedByStructs, dense, func(t *testing.T) []byte {
			return typedBeforeState(t, 100_000)
		}},
		{"the long-edited document", false, pricedByStructs, dense, func(t *testing.T) []byte { return longEditedState(t) }},
		{"a run of 300,000 keystrokes", false, pricedByBytes, run, func(t *testing.T) []byte { return typedRunState(t, 300_000) }},
		{"one paragraph of a mebibyte typed in one transaction", false, pricedByBytes, 0, func(t *testing.T) []byte { return longRunState(t, 1) }},
		{"eight paragraphs of 128 KiB, each typed in one transaction", false, pricedByBytes, 0, func(t *testing.T) []byte { return longRunState(t, 8) }},
		{"an agent's whole rewrites until the cap refused one", true, pricedByStructs, dense, cachedAgentRewrittenState},
		{"a long-edited document typed on until the cap", true, pricedByStructs, dense, longEditedAtCapState},
		{"a run of a million keystrokes", true, pricedByBytes, run, func(t *testing.T) []byte {
			keystrokes, _ := keystrokeRunStates(t)
			return keystrokes
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if shape.heavy && !heavy {
				t.Skip("set " + structHeapMeasurement + "=1 to run the heavy rows")
			}
			if !measuredAlone(t) {
				return
			}
			state := shape.state(t)
			held := heldByResidentRoom(t, state, false)
			live := newDocumentCopy()
			if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
				t.Fatal(err)
			}
			structs := int64(countRoomStructs(live))
			markdown, err := renderDocument(live)
			if err != nil {
				t.Fatal(err)
			}
			bytes, elements := int64(len(markdown)), countElements(markdown)
			perStruct := float64(held) / float64(structs*roomCopies)
			perByte := float64(held) / float64(bytes*roomCopies)
			terms := [...]int64{
				pricedByElements: elements * bytesOfHeapAnElement,
				pricedByStructs:  structs * bytesOfHeapAStructHeld,
				pricedByBytes:    bytes * bytesOfHeapARenderedByte,
			}
			t.Logf("%d B of state, %d structs, %d B of markdown making %d elements; heap held %d B, for each of %d copies %.1f B a struct (%.2fx the %d B price) and %.1f B a rendered byte (%.2fx the %d B price); a copy is priced %d B at its elements, %d at its structs, %d at its bytes",
				len(state), structs, bytes, elements, held, roomCopies, perStruct, perStruct/bytesOfHeapAStructHeld, bytesOfHeapAStructHeld,
				perByte, perByte/bytesOfHeapARenderedByte, bytesOfHeapARenderedByte, terms[pricedByElements], terms[pricedByStructs], terms[pricedByBytes])
			if copyHeap(bytes, elements, structs) != terms[shape.priced] {
				t.Fatalf("the copy is priced %d B, want its %d B term, %v", copyHeap(bytes, elements, structs), terms[shape.priced], terms)
			}
			for priced, term := range terms {
				if copyPrice(priced) != shape.priced && term >= terms[shape.priced] {
					t.Errorf("term %d prices the copy at %d B, no less than the %d B of the term that should price it, %v", priced, term, terms[shape.priced], terms)
				}
			}
			var ratio float64
			switch shape.priced {
			case pricedByStructs:
				ratio = perStruct / bytesOfHeapAStructHeld
			case pricedByBytes:
				ratio = perByte / bytesOfHeapARenderedByte
			default:
				return
			}
			if ratio < shape.least || ratio > most {
				t.Errorf("the room holds %.2fx the price that prices it for each copy, want %.2fx to %.2fx", ratio, shape.least, most)
			}
		})
	}
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
// own measurement of what a structure holds (heldByDocumentRead, read_cache_weight_test.go:48): a
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
// into the copy the load keeps as the replica (newDocumentCopy, decodeStoredState, keepReplica),
// and, for a room that has had an update, takes the replica as the room's observer moves it
// through that update instead: one drain of the room's update log, which copies the document from
// the log's base and renders it (observedReplica), a replica holding the same state.
func buildResidentRoom(t *testing.T, state []byte, edited bool) *residentRoom {
	t.Helper()
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		t.Fatalf("decode the stored state: %v", err)
	}
	room := &residentRoom{service: &Service{}, live: live}
	if !edited {
		decoded, err := decodeStoredState(state)
		if err != nil {
			t.Fatalf("decode the stored state's copy: %v", err)
		}
		room.replica = room.service.keepReplica(live, nil, nil, decoded.doc)
		return room
	}
	room.replica = observedReplica(t, room.service, live, nil)
	if _, _, measured := room.replica.size(); !measured {
		t.Fatal("the room's observer published no measurement, so it rendered no markdown and the replica this weighs is not a room's")
	}
	return room
}

// residentRoomState is the state a room holding about size bytes of markdown loads, with what
// that room's rendering measures (pmdoc.MeasureDocument, which is how an upload of the same
// markdown is weighed). Each reading decodes that state rather than building the document again,
// as the read cache's own measurement builds its read from a document decoded from its stored
// state, which holds strings of its own rather than the markdown's (buildTestDocumentRead,
// read_cache_weight_test.go:68).
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
