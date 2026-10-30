package docs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room is charged three parts: its latest count at both copies; the content its transactions
// have added since that count, at the densest price at both copies; and its structure part. Both
// copies are charged from its load, which keeps both (roomCopies). Each count replaces the last
// rather than adding to it, so the charge falls when the document does, and clears the content
// added up to the rendering it measured; no count touches the structure part. A count prices a
// copy at its elements, its rendered bytes or its structs, whichever is the most. A rendering
// goldmark could not finish counting is priced from its bytes at the densest price, past the
// element bound and never at nothing. A socket's reservation is replaced by its room's charge when
// the room loads, and the whole charge goes when ygo retires the room.
func TestTheRoomBudgetChargesARoomItsCountItsUncountedContentAndItsStructure(t *testing.T) {
	const densest = bytesOfHeapAByteAtTheDensest * roomCopies
	budget := &roomBudget{limit: roomBudgetBytes}
	charge := budget.load("room", copyHeap(10_000, 1_000, 0))
	counted := int64(2 * 1_000 * bytesOfHeapAnElement)
	requireCharged(t, budget, charge, counted, "a room that has taken no update is charged both copies")

	charge.adds(100)
	requireCharged(t, budget, charge, counted+100*densest, "the content the first update adds is charged at the densest price at both copies")
	charge.grew(5_000)
	requireCharged(t, budget, charge, counted+100*densest+5_000, "the structure an update adds is charged as it is")
	charge.adds(50)
	requireCharged(t, budget, charge, counted+150*densest+5_000, "a later update's content adds to the uncounted part")

	// The count of the rendering published once the room had added 100 bytes re-bases the charge
	// there: the 50 bytes added since stay beside it, and so does the structure.
	charge.measured(copyHeap(8_000, 800, 0), 100)
	requireCharged(t, budget, charge, 2*800*bytesOfHeapAnElement+50*densest+5_000, "a count of a smaller document lowers the charge")
	charge.measured(copyHeap(8_000, 800, 0), 150)
	requireCharged(t, budget, charge, 2*800*bytesOfHeapAnElement+5_000, "a count of the latest rendering leaves no uncounted content, and the structure part as it was")
	// The same rendering over a history of 100,000 structs: its structs outweigh its elements,
	// and price it; structs that weigh less than the elements leave it at its elements.
	charge.measured(copyHeap(8_000, 800, 100_000), 150)
	requireCharged(t, budget, charge, 2*100_000*bytesOfHeapAStructHeld+5_000, "a count whose structs outweigh its elements is priced at its structs")
	charge.measured(copyHeap(8_000, 800, 2_000), 150)
	requireCharged(t, budget, charge, 2*800*bytesOfHeapAnElement+5_000, "a count whose elements outweigh its structs is priced at its elements")
	// A long run of text renders many bytes making few elements from few structs: its bytes
	// outweigh both, and price it.
	charge.measured(copyHeap(800_000, 9, 11), 150)
	requireCharged(t, budget, charge, 2*800_000*bytesOfHeapARenderedByte+5_000, "a count whose rendered bytes outweigh its elements and structs is priced at its bytes")

	// A rendering MeasureDocument leaves uncounted answers no elements at all; priced as that, the
	// heaviest room in the pod would be free. It is priced from its bytes at the densest price,
	// with no ceiling at the element bound.
	const uncounted = 64 << 10
	charge.measured(copyHeap(uncounted, elementsPastCounting, 0), 150)
	requireCharged(t, budget, charge, 2*uncounted*bytesOfHeapAByteAtTheDensest+5_000, "a rendering goldmark stopped counting is priced from its bytes at the densest price")
	if atTheBound := roomWeight(copyHeap(pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements, 0), 0, 0); charge.weight <= atTheBound {
		t.Fatalf("an uncounted 64 KiB rendering is charged %d bytes, want more than the %d of a document at the element bound", charge.weight, atTheBound)
	}
	charge.measured(copyHeap(sizeUnmeasured, sizeUnmeasured, 0), 150)
	requireCharged(t, budget, charge, 5_000, "a room that renders no markdown and holds no struct is charged its uncounted content and structure alone")
	charge.measured(copyHeap(sizeUnmeasured, sizeUnmeasured, 1_000), 150)
	requireCharged(t, budget, charge, 2*1_000*bytesOfHeapAStructHeld+5_000, "a room that renders no markdown is priced at its structs")

	budget.retire("room")
	charge.adds(1_000)
	charge.grew(1_000)
	if budget.used != 0 || len(budget.rooms) != 0 {
		t.Fatalf("the budget holds %d bytes over %d rooms once the room retired, want nothing", budget.used, len(budget.rooms))
	}

	// The reservation an admission makes, its stored version's count at both copies, is replaced
	// at the room's load by the charge of the room's own document at both copies, here one an
	// update stored since that version has made smaller.
	ctx := context.Background()
	reserved := roomWeight(copyHeap(3_000, 300, 0), 0, 0)
	reservation, err := budget.reserve(ctx, time.Time{}, "other", reserved)
	if err != nil || reservation == nil || budget.used != reserved {
		t.Fatalf("a reservation held %d bytes (%v), want %d", budget.used, err, reserved)
	}
	other := budget.load("other", copyHeap(3_000, 100, 0))
	if budget.used != 2*100*bytesOfHeapAnElement || len(budget.rooms["other"].reservations) != 0 {
		t.Fatalf("the loaded room is charged %d bytes beside %d reservations, want its own %d alone",
			budget.used, len(budget.rooms["other"].reservations), 2*100*bytesOfHeapAnElement)
	}
	if other.closeOnceUnused || budget.marked.Load() != 0 {
		t.Fatal("a room loaded for an admitted socket was marked to close once unused")
	}
	budget.release(reservation)
	if joined, err := budget.reserve(ctx, time.Time{}, "other", reserved); joined != nil || err != nil || budget.used != other.weight {
		t.Fatalf("a socket joining a charged room reserved %v (%v), and the budget holds %d bytes, want nothing reserved", joined, err, budget.used)
	}

	// A budget no service built never sweeps, so an admission it has no room for is refused at
	// once, and wants nothing once it has been.
	budget.limit = other.weight + 100
	if refused, err := budget.reserve(ctx, time.Now().Add(time.Hour), "third", 101); refused != nil || !errors.Is(err, errRoomBudgetFull) || budget.used != other.weight {
		t.Fatalf("a reservation past the limit was %v (%v), and the budget holds %d bytes, want a refusal holding nothing", refused, err, budget.used)
	}
	if budget.wanted != 0 || budget.waiting != 0 {
		t.Fatalf("a refused admission left %d bytes wanted by %d waiting, want none", budget.wanted, budget.waiting)
	}
	// A sweep that leaves the budget within its limit raises nothing; one that leaves it over
	// raises full, which refuses growth in every room, and the room's retirement lowers it.
	budget.swept(false)
	if budget.full.Load() {
		t.Fatal("a sweep that left the budget within its limit raised full")
	}
	other.grew(1_000)
	budget.swept(false)
	if !budget.full.Load() {
		t.Fatal("a sweep that left the budget over did not raise full")
	}
	budget.retire("other")
	if budget.full.Load() || budget.used != 0 {
		t.Fatalf("the budget holds %d bytes and full is %t once its room retired, want nothing and full lowered", budget.used, budget.full.Load())
	}
}

// A room's charge follows its renderings through the measurer: a count that lands replaces the
// counted part and clears the content the room had added when the counted rendering was
// published, never the content added since, and a rendering whose bytes are published without a
// count of its own clears nothing. No count touches the structure part. A room whose rendering the
// counter cannot finish is charged from that rendering's bytes at the densest price, at its load
// and at a later count alike.
func TestTheMeasurersCountClearsOnlyTheContentAddedUpToTheRenderingItMeasured(t *testing.T) {
	const densest = bytesOfHeapAByteAtTheDensest * roomCopies
	service := &Service{budget: roomBudget{limit: roomBudgetBytes}}
	service.measurer.interval = noInterval
	// Each count the measurer starts is handed over here and held until the test releases it.
	counting, release := make(chan string, 2), make(chan struct{}, 2)
	service.measurer.beforeCount = func(markdown string) {
		counting <- markdown
		<-release
	}
	// Empty items of alternating bullet lists, each a list of its own, are the densest rendering
	// measured (bytesOfHeapAByteAtTheDensest), and a mebibyte of them makes more nodes than
	// goldmark builds before MeasureDocument stops counting.
	dense := func(kibibytes int) string { return strings.Repeat("- \n\n* \n\n", kibibytes<<7) }
	loaded, counted, later := dense(1024), headings(1), dense(1152)
	for _, rendering := range []string{loaded, later} {
		if elements := countElements(rendering); elements != elementsPastCounting {
			t.Fatalf("a dense rendering of %d bytes counts %d elements, want it past counting", len(rendering), elements)
		}
	}
	countedHeap := copyHeap(int64(len(counted)), countElements(counted), 0)
	if countedHeap <= 0 || countedHeap == int64(len(later))*bytesOfHeapAByteAtTheDensest {
		t.Fatalf("the counted rendering prices %d bytes, want a price no other rendering here has", countedHeap)
	}

	live := newDocumentCopy()
	replica := service.keepReplica(live, nil, &loaded, nil)
	charge := service.chargeLoadedRoom("room", live, replica)
	requireCharged(t, &service.budget, charge, 2*int64(len(loaded))*bytesOfHeapAByteAtTheDensest,
		"a room loaded with a rendering the counter cannot finish is priced from its bytes at the densest price")

	charge.adds(100)
	charge.grew(7_000)
	replica.publishSize(&counted)
	if markdown := <-counting; markdown != counted {
		t.Fatal("the measurer started on a rendering the room did not publish")
	}
	// The count of counted is in flight. What the room adds now, and a rendering published while
	// that count runs, which publishes its bytes at once and waits for a count of its own, leave
	// the charge as it was: the bytes alone clear nothing.
	charge.adds(50)
	replica.publishSize(&later)
	if bytes, _, _ := replica.size(); bytes != int64(len(later)) {
		t.Fatalf("the room published %d bytes, want the later rendering's %d at once", bytes, len(later))
	}
	requireCharged(t, &service.budget, charge, 2*int64(len(loaded))*bytesOfHeapAByteAtTheDensest+150*densest+7_000,
		"a rendering published without a count clears no content")

	// The count of counted lands: it holds the 100 bytes added before it was published, and not
	// the 50 added since, which stay beside it until the count of later holds them.
	release <- struct{}{}
	if markdown := <-counting; markdown != later {
		t.Fatal("the measurer's second count is not of the later rendering")
	}
	requireCharged(t, &service.budget, charge, 2*countedHeap+50*densest+7_000,
		"a count clears the content added up to the rendering it measured, and no more, and leaves the structure")
	release <- struct{}{}
	service.measurer.waitIdle()
	requireCharged(t, &service.budget, charge, 2*int64(len(later))*bytesOfHeapAByteAtTheDensest+7_000,
		"the latest rendering's count, past counting, prices its bytes at the densest price and clears all the content it holds")
	// A live room's update observer holds its replica, which the measurer holds weakly
	// (elementMeasurer); nothing else here does, so a count could otherwise land on nothing.
	runtime.KeepAlive(replica)
}

// The budget's arithmetic at the shapes its design is stated in, each room admitted against an
// empty budget, loaded and then given what browsers add to it before the next arrives. A room of a
// mebibyte of prose (57,600 elements from 77,600 structs,
// TestAResidentRoomsHeapPerByteOfItsMarkdown's shape) is charged 54 MB once counted, at its
// elements, at both copies whether it is read or edited (roomCopies), and since an admission
// reserves the same count at the same two copies (storedRoomReservation), the budget admits
// thirty-nine such documents and refuses the fortieth: a load charged at one copy would admit
// seventy-eight, whose replicas their first keystrokes would then take with nothing able to refuse
// them.
//
// What browsers add is charged beside the count. A 4 KiB paste not yet counted is its content at
// the densest price at both copies, 4,096 x 1,058 x 2 = 8,667,136 bytes, so rooms each holding one
// admit thirty-four. 2,000 keystrokes, each one struct at bytesOfHeapAStruct with its two splits at
// bytesOfHeapASplit and its content, a character and its length, as text at bytesOfHeapAByteOfText
// (typed text reads as unrendered to the walk, unrenderedContentHeap), at both copies,
// (620 + 2 x 200 + 2 x 2) x 2 = 2,048 bytes, are a structure part of 4,096,000 bytes, which no
// count clears, so rooms each typed into that much admit thirty-six; a delete range the room did
// not hold deleted is two splits, 2 x 200 x 2 = 800 bytes, and one it held deleted already nothing
// (deletedRuns). One frame of 500,000 GC structs (plan.md, departure 4: 64,645,200 bytes kept in
// one copy, the rendering unchanged) is a structure part of 500,000 x (620 + 2 x 200) x 2 =
// 1,020,000,000 bytes, so two such rooms take the whole budget. A document at the decode cap is
// priced at its 1,048,576 structs whatever it renders (bytesOfHeapAStructHeld), 1,048,576 x 170 x
// 2 = 356,515,840 bytes: the long-edited document's 53 elements and the agent-rewritten document's
// 65,536 alike, so the budget admits six of either, where their elements would have admitted over
// forty thousand of the first. A paragraph of a mebibyte typed in one transaction loads as 11
// structs making 9 elements, 4,230 bytes a copy at the element price, and is priced at its rendered
// bytes instead (bytesOfHeapARenderedByte), 1,048,576 x 12 x 2 = 25,165,824 bytes, so the budget
// admits eighty-five; the same price leaves the other shapes where their elements or structs put
// them, a mebibyte of prose at 12,582,912 bytes a copy against its elements' 27,072,000, and the
// agent-rewritten document at 12,582,900 against its structs' 178,257,920. There is no ceiling on
// one room: a mebibyte of the densest rendering, which goldmark stops counting and which is priced
// from its bytes (bytesOfHeapAByteAtTheDensest), is more than the whole budget at both copies, so
// the budget admits none, and the pressure rule is what answers a room grown that heavy
// (refusedUnderPressure).
func TestTheRoomBudgetsArithmetic(t *testing.T) {
	prose := copyHeap(pmdoc.MaxDocumentBytes, 57_600, 77_600)
	for _, shape := range []struct {
		name               string
		heap               int64
		content, structure int64
		weight, admits     int64
	}{
		{"a room of a mebibyte of prose", prose, 0, 0, 54_144_000, 39},
		{"a mebibyte of prose with a 4 KiB paste not yet counted", prose, 4 << 10, 0, 62_811_136, 34},
		{"a mebibyte of prose typed into with 2,000 keystrokes", prose, 0, 2_000 * (bytesOfHeapAStruct + splitsAStructCanCause*bytesOfHeapASplit + 2*bytesOfHeapAByteOfText) * roomCopies, 58_240_000, 36},
		{"a mebibyte of prose given a frame of 500,000 GC structs", prose, 0, 500_000 * (bytesOfHeapAStruct + splitsAStructCanCause*bytesOfHeapASplit) * roomCopies, 1_074_144_000, 2},
		{"the long-edited document typed on until the decode cap", copyHeap(5_314, 53, maxUpdateItems), 0, 0, 356_515_840, 6},
		{"an agent's whole rewrites until the decode cap refused one", copyHeap(1_048_575, 65_536, maxUpdateItems), 0, 0, 356_515_840, 6},
		{"a paragraph of a mebibyte typed in one transaction", copyHeap(pmdoc.MaxDocumentBytes, 9, 11), 0, 0, 25_165_824, 85},
		{"a mebibyte of the densest rendering", copyHeap(pmdoc.MaxDocumentBytes, elementsPastCounting, 0), 0, 0, 2_218_786_816, 0},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if weight := roomWeight(shape.heap, shape.content, shape.structure); weight != shape.weight {
				t.Fatalf("a room of %d bytes of heap a copy, %d bytes of uncounted content and a %d-byte structure part is charged %d bytes, want %d",
					shape.heap, shape.content, shape.structure, weight, shape.weight)
			}
			budget := &roomBudget{limit: roomBudgetBytes}
			admitted := int64(0)
			for {
				room := fmt.Sprintf("room-%d", admitted)
				reservation, err := budget.reserve(context.Background(), time.Time{}, room, roomWeight(shape.heap, 0, 0))
				if errors.Is(err, errRoomBudgetFull) {
					break
				}
				if err != nil || reservation == nil {
					t.Fatalf("reserve room %d: %v", admitted+1, err)
				}
				charge := budget.load(room, shape.heap)
				charge.adds(shape.content)
				charge.grew(shape.structure)
				if charge.weight != shape.weight {
					t.Fatalf("room %d is charged %d bytes, want %d", admitted+1, charge.weight, shape.weight)
				}
				admitted++
			}
			if admitted != shape.admits {
				t.Fatalf("the budget admitted %d rooms charged %d bytes each, want %d", admitted, shape.weight, shape.admits)
			}
		})
	}
}

// The densest price (bytesOfHeapAByteAtTheDensest) is the most elements a byte of rendered
// markdown makes, measured here over the dense shapes pmdoc's own tests build
// (pmdoc/elements_test.go: the shapes its refusal of a mebibyte and its spelling of every node
// type use) and over this repository's markdown files, and this fails once any shape
// prices higher at a mebibyte than the constant does. What is measured is each one's rendering,
// since that is what the budget prices: a version is stored as its rendering
// (pmdoc/parse.go:120-122), and a room's replica publishes its own.
//
// MeasureDocument stops counting at twice the element bound, so a mebibyte of a dense shape is
// counted in pieces, each a document of its own of up to 8 KiB of the shape, rendered and measured
// whole, until their renderings reach the size: the sum counts the head and the containers each
// piece opens once for each piece, which errs on the dense side. A unit that needs an id of its
// own takes its index within its piece. The densest at a mebibyte, at 64 KiB and overall - every
// shape at those sizes and as a lone unit, and every file at its own size - is logged.
func TestNoMarkdownPricesPastTheDensestPrice(t *testing.T) {
	type shape struct {
		name, head, tail string
		unit             func(index int) string
	}
	repeated := func(name, unit string) shape {
		return shape{name: name, unit: func(int) string { return unit }}
	}
	numbered := func(name, format string) shape {
		return shape{name: name, unit: func(index int) string { return fmt.Sprintf(format, index) }}
	}
	shapes := []shape{
		repeated(")_", ")_"),
		repeated("a_", "a_"),
		repeated("a_b*", "a_b*"),
		repeated("a*b*", "a*b*"),
		repeated("[a", "[a"),
		repeated("a line feed per character", "a\n"),
		repeated("paragraphs", "a\n\n"),
		repeated("list items", "- a\n"),
		repeated("empty list items", "-\n"),
		repeated("empty items of alternating bullet lists", "-\n*\n"),
		repeated("empty items of alternating ordered lists", "1.\n1)\n"),
		repeated("list items opening with a heading", "- # a\n"),
		repeated("hard breaks", "a  \n"),
		repeated("backslash hard breaks", "a\\\n"),
		repeated("headings", "# a\n"),
		repeated("empty headings", "#\n"),
		repeated("empty quotes", ">\n\n"),
		repeated("quotes", "> a\n\n"),
		repeated("empty fences", "```\n```\n"),
		repeated("fences", "```\na\n```\n"),
		repeated("rules", "***\n"),
		repeated("images", "![](u)"),
		repeated("images with alt text", "![a](u)"),
		repeated("linked images", "[![](u)](v)"),
		repeated("inline HTML tags", "<b>"),
		repeated("inline HTML spans", "<b>a</b>"),
		repeated("inline HTML around emphasis", "<b>*a*</b>"),
		{name: "HTML comments", head: "a", unit: func(int) string { return "<!---->" }},
		{name: "a two-column table", head: "| a | b |\n| - | - |\n", unit: func(int) string { return "| x | y |\n" }},
		{name: "empty table rows", head: "| a |\n| - |\n", unit: func(int) string { return "| |\n" }},
		{name: "empty four-column rows", head: "| a | b | c | d |\n| - | - | - | - |\n", unit: func(int) string { return "| | | | |\n" }},
		repeated("header-only tables", "| a |\n| - |\n\n"),
		repeated("empty three-column header-only tables", "| | | |\n| - | - | - |\n\n"),
		repeated(`\~a`, `\~a`),
		repeated(`)\_`, `)\_`),
		repeated(`\)\_`, `\)\_`),
		repeated(`\[a`, `\[a`),
		repeated("&#95;a", "&#95;a"),
		numbered("empty footnote definitions", "[^%d]:\n"),
		numbered("footnote definitions", "[^%d]: a\n"),
		{name: "footnote references", tail: "\n\n[^a]: b\n", unit: func(int) string { return "[^a]" }},
		repeated("empty callouts", ":::callout{}\n:::\n\n"),
		repeated("callouts", ":::callout{}\na\n:::\n\n"),
		numbered("empty asks", ":::ask{#ask-%d}\n:::\n\n"),
		numbered("asks", ":::ask{#ask-%d}\nq\n:::\n\n"),
	}
	type density struct {
		elements, bytes int64
		name            string
	}
	denser := func(worst *density, name string, elements, bytes int64) {
		if bytes > 0 && (worst.bytes == 0 || elements*worst.bytes > worst.elements*bytes) {
			*worst = density{elements: elements, bytes: bytes, name: name}
		}
	}
	perByte := func(d density) float64 { return float64(d.elements) / float64(d.bytes) }
	// renderingOf is markdown read and rendered as a room would hold it, or the error that says it
	// is no document's rendering at all: markdown outside the Proof schema.
	renderingOf := func(markdown string) (string, error) {
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			return "", err
		}
		return documentMarkdown(tree)
	}
	const pieceBytes = 8 << 10
	// at is what shape's renderings make, in pieces, once they reach size bytes; a piece measured
	// once is not measured again, which every piece of a repeated unit is but the last. Every piece
	// of a shape is a document in the schema, and counted whole.
	at := func(s shape, size int64) (int64, int64) {
		t.Helper()
		measured := map[string][2]int64{}
		var elements, bytes int64
		for bytes < size {
			var piece strings.Builder
			piece.WriteString(s.head)
			for index := 0; piece.Len()+len(s.tail) < pieceBytes; index++ {
				piece.WriteString(s.unit(index))
			}
			piece.WriteString(s.tail)
			got, ok := measured[piece.String()]
			if !ok {
				rendering, err := renderingOf(piece.String())
				if err != nil {
					t.Fatalf("%s: a piece of %d bytes is no document's rendering: %v", s.name, piece.Len(), err)
				}
				counted := pmdoc.MeasureDocument(rendering)
				if !counted.Counted {
					t.Fatalf("%s: a piece's rendering of %d bytes makes more nodes than MeasureDocument builds; measure it in smaller pieces", s.name, len(rendering))
				}
				got = [2]int64{int64(counted.Elements), int64(counted.Bytes)}
				measured[piece.String()] = got
			}
			elements += got[0]
			bytes += got[1]
		}
		return elements, bytes
	}
	var atAMebibyte, at64KiB, overall, densestFile density
	for _, s := range shapes {
		elements, bytes := at(s, pmdoc.MaxDocumentBytes)
		denser(&atAMebibyte, s.name, elements, bytes)
		denser(&overall, s.name+" at a mebibyte", elements, bytes)
		if price, densest := elements*bytesOfHeapAnElement, bytes*bytesOfHeapAByteAtTheDensest; price > densest {
			t.Errorf("a mebibyte of %s renders to %d bytes making %d elements, %d bytes of heap a copy, past the %d the densest price gives it",
				s.name, bytes, elements, price, densest)
		}
		elements, bytes = at(s, 64<<10)
		denser(&at64KiB, s.name, elements, bytes)
		denser(&overall, s.name+" at 64 KiB", elements, bytes)
		// A lone unit of some shapes reads as something else - a lone `<b>` is an HTML block,
		// which the schema refuses - and is no document's rendering to weigh.
		if rendering, err := renderingOf(s.head + s.unit(0) + s.tail); err == nil {
			if size := pmdoc.MeasureDocument(rendering); size.Counted {
				denser(&overall, "a lone unit of "+s.name, int64(size.Elements), int64(size.Bytes))
			}
		}
	}
	root := repositoryRoot(t)
	files, unread := 0, 0
	for _, file := range repositoryMarkdown(t, root) {
		markdown, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		rendering, err := renderingOf(string(markdown))
		if err != nil {
			unread++ // markdown outside the Proof schema is no document's rendering
			continue
		}
		size := pmdoc.MeasureDocument(rendering)
		if !size.Counted {
			t.Errorf("%s: its rendering of %d bytes makes more nodes than MeasureDocument builds", name, len(rendering))
			continue
		}
		files++
		denser(&overall, name, int64(size.Elements), int64(size.Bytes))
		denser(&densestFile, name, int64(size.Elements), int64(size.Bytes))
		if size.Bytes >= 64<<10 {
			denser(&at64KiB, name, int64(size.Elements), int64(size.Bytes))
		}
	}
	if files < 100 {
		t.Fatalf("the walk read %d markdown files in this repository (%d outside the schema), want the hundreds it holds", files, unread)
	}
	t.Logf("the densest rendering at a mebibyte is %s at %.4f elements a byte, %.1f bytes of heap a byte; the densest price is %d",
		atAMebibyte.name, perByte(atAMebibyte), perByte(atAMebibyte)*bytesOfHeapAnElement, bytesOfHeapAByteAtTheDensest)
	t.Logf("the densest rendering at 64 KiB is %s at %.4f elements a byte", at64KiB.name, perByte(at64KiB))
	t.Logf("the densest rendering overall is %s at %.4f elements a byte (%d elements in %d bytes), over %d shapes and %d of this repository's markdown files (%d outside the schema)",
		overall.name, perByte(overall), overall.elements, overall.bytes, len(shapes), files, unread)
	t.Logf("the densest rendering of this repository's markdown files is %s at %.4f elements a byte (%d elements in %d bytes)",
		densestFile.name, perByte(densestFile), densestFile.elements, densestFile.bytes)
}

// repositoryRoot is the directory this repository's Go workspace sits in, found by walking up from
// the package the test runs in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.work above this package: the repository root is what the walk starts from")
		}
		root = parent
	}
}

// repositoryMarkdown is every markdown file under root, skipping the trees nothing in this
// repository wrote - its dependencies, its version control, the caches each leaves. It is this
// test's own walk: no other package walks the repository's markdown.
func repositoryMarkdown(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func requireCharged(t *testing.T, budget *roomBudget, charge *roomCharge, want int64, what string) {
	t.Helper()
	if charge.weight != want || budget.used != want {
		t.Fatalf("%s: the room is charged %d bytes and the budget holds %d, want %d", what, charge.weight, budget.used, want)
	}
}

// Every way out of the document socket's handler returns the reservation its admission made, and
// the one way that keeps the socket hands it to the room's own charge when the room loads: a socket
// refused outside the schema, one whose admission load fails, one the budget refuses, and one that
// joins its room. The reservation is the stored version's count at both copies, held while the
// handler loads the document, and the room's load replaces it with the count of the rendering it
// loaded with, at both copies too (roomCopies): the same document here, so the reservation itself.
func TestTheDocumentSocketReturnsItsReservationOnEveryWayOut(t *testing.T) {
	database := storetest.Open(t)
	watch := &admissionWatchStore{VersionedStore: NewPgVersioned(database), held: make(map[string][]int64)}
	service := newRoomBudgetService(t, database, watch)
	watch.service.Store(service)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 4)
	outside, failing, refused, joined := ids[0], ids[1], ids[2], ids[3]
	stored := latestVersionMarkdown(t, service, joined)
	reserved := roomWeight(copyHeap(int64(len(stored)), countElements(stored), storedHistoryStructs(t, database, joined)), 0, 0)
	if reserved <= 0 {
		t.Fatalf("the stored document reserves %d bytes, want some", reserved)
	}

	t.Run("refused outside the schema", func(t *testing.T) {
		if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, outside); err != nil {
			t.Fatal(err)
		}
		requireClosedAtUpgrade(t, httpServer.URL, outside, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
		watch.requireHeldAtLoad(t, outside, reserved)
		requireNothingReserved(t, service)
	})
	t.Run("its admission load fails", func(t *testing.T) {
		watch.failing.Store(failing)
		if connection, err := dialDocument(httpServer.URL, failing); err == nil {
			_ = connection.Close()
			t.Fatal("the socket opened, want the failed load answered 503")
		} else if !strings.Contains(err.Error(), "503") {
			t.Fatalf("the socket was refused with %v, want 503", err)
		}
		watch.requireHeldAtLoad(t, failing, reserved)
		requireNothingReserved(t, service)
	})
	t.Run("the budget has no room for it", func(t *testing.T) {
		service.SetRoomBudgetForTest(reserved - 1)
		t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
		requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
		if service.srv.GetDoc(refused) != nil || len(watch.loads(refused)) != 0 {
			t.Fatal("the refused socket's document was loaded, want it refused before any load")
		}
		requireNothingReserved(t, service)
	})
	t.Run("it joins its room", func(t *testing.T) {
		connection, err := dialDocument(httpServer.URL, joined)
		if err != nil {
			t.Fatalf("open the document: %v", err)
		}
		waitFor(t, 10*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(joined) != nil })
		watch.requireHeldAtLoad(t, joined, reserved)
		rendering := renderedLive(t, service, joined)
		loaded := roomWeight(copyHeap(int64(len(rendering)), countElements(rendering), liveStructs(t, service, joined)), 0, 0)
		requireNothingReserved(t, service)
		if used := budgetUsed(service); used != loaded {
			t.Fatalf("the budget holds %d bytes for the open room, want its own charge at both copies, %d", used, loaded)
		}
		if loaded != reserved {
			t.Fatalf("the room's count charges %d bytes at both copies, want the %d its admission reserved from the same document at both", loaded, reserved)
		}
		leaveDocument(connection)
		if err := service.Evict(context.Background(), joined); err != nil {
			t.Fatal(err)
		}
		if used := budgetUsed(service); used != 0 {
			t.Fatalf("the budget holds %d bytes once the room retired, want none", used)
		}
	})
}

// An admission reserves its stored version counted, at both copies, as the room it opens is priced
// once loaded (storedRoomReservation). A mebibyte of prose, the shape
// TestAResidentRoomsHeapPerByteOfItsMarkdown weighs, is admitted to an empty budget, and what it
// reserves is within 1.05x of what its room is charged once loaded, at both copies. The
// two are not one figure by construction: the admission counts the markdown the version stored,
// and the room counts the rendering of the document it decoded from the stored updates, so they
// agree only as far as that rendering spells the document as the stored one does - here, with no
// update stored since the version, exactly; past that, by the updates stored since.
//
// The count is kept for the version: a second admission and the socket's own parse nothing, and
// a new version is counted again. A version MeasureDocument cannot finish counting is priced from
// its bytes at the densest price, and a mebibyte of it is refused at admission.
func TestAnAdmissionReservesTheCountOfItsStoredVersion(t *testing.T) {
	const factor = 1.05
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	var parses atomic.Int64
	service.budget.stored.beforeCount = func(string, int64) { parses.Add(1) }
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	room := createDocumentsHolding(t, database, 1, residentRoomMarkdown(t, pmdoc.MaxDocumentBytes))[0]
	stored := latestVersionMarkdown(t, service, room)
	started := time.Now()
	elements := countElements(stored)
	t.Logf("the stored version is %d bytes of prose making %d elements, counted in %s", len(stored), elements, time.Since(started))
	if elements == elementsPastCounting {
		t.Fatal("a mebibyte of prose is past counting, want it counted")
	}

	reservation, err := service.reserveRoom(ctx, room)
	if err != nil || reservation == nil {
		t.Fatalf("a mebibyte of prose was refused at admission to an empty budget: %v", err)
	}
	if want := roomWeight(copyHeap(int64(len(stored)), elements, storedHistoryStructs(t, database, room)), 0, 0); reservation.weight != want || parses.Load() != 1 {
		t.Fatalf("the admission reserved %d bytes after %d parses, want its count at both copies, %d, after one", reservation.weight, parses.Load(), want)
	}
	service.budget.release(reservation)
	again, err := service.reserveRoom(ctx, room)
	if err != nil || again == nil || again.weight != reservation.weight || parses.Load() != 1 {
		t.Fatalf("a second admission reserved %v (%v) after %d parses, want the same %d bytes and no second parse", again, err, parses.Load(), reservation.weight)
	}
	service.budget.release(again)

	connection, err := dialDocument(httpServer.URL, room)
	if err != nil {
		t.Fatalf("open the document: %v", err)
	}
	waitFor(t, 30*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(room) != nil })
	requireNothingReserved(t, service)
	loaded := budgetUsed(service)
	ratio := float64(reservation.weight) / float64(loaded)
	t.Logf("a mebibyte of prose reserves %d bytes at admission; its room is charged %d bytes loaded, at both copies (%.4fx); its bytes at the densest price would reserve %d",
		reservation.weight, loaded, ratio, roomWeight(int64(len(stored))*bytesOfHeapAByteAtTheDensest, 0, 0))
	if ratio < 1/factor || ratio > factor {
		t.Fatalf("the admission reserved %d bytes, %.4fx the %d its room costs once counted, want within %.2fx", reservation.weight, ratio, loaded, factor)
	}
	if parses.Load() != 1 {
		t.Fatalf("the socket's admission parsed the stored version again (%d parses), want the kept count", parses.Load())
	}
	leaveDocument(connection)
	if err := service.Evict(ctx, room); err != nil {
		t.Fatal(err)
	}

	appendVersion := func(markdown string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select $1, coalesce(max(number), 0) + 1, $2, '[]' from artifact_versions where artifact_id = $1
		`, room, markdown); err != nil {
			t.Fatalf("store a new version: %v", err)
		}
	}
	const second = "A second version.\n"
	appendVersion(second)
	next, err := service.reserveRoom(ctx, room)
	if want := roomWeight(copyHeap(int64(len(second)), countElements(second), storedHistoryStructs(t, database, room)), 0, 0); err != nil || next == nil || next.weight != want || parses.Load() != 2 {
		t.Fatalf("an admission to a new version reserved %v (%v) after %d parses, want its own count, %d, after a second parse", next, err, parses.Load(), want)
	}
	service.budget.release(next)

	// Empty items of alternating bullet lists, each a list of its own, are the densest rendering
	// measured, and a mebibyte of them makes more nodes than goldmark builds before
	// MeasureDocument stops counting.
	uncountable := strings.Repeat("- \n\n* \n\n", 1024<<7)
	if countElements(uncountable) != elementsPastCounting {
		t.Fatal("a mebibyte of the densest rendering was counted, want it past counting")
	}
	appendVersion(uncountable)
	densest := roomWeight(int64(len(uncountable))*bytesOfHeapAByteAtTheDensest, 0, 0)
	if weight, err := service.storedRoomReservation(ctx, room); err != nil || weight != densest || parses.Load() != 3 {
		t.Fatalf("a version past counting reserves %d bytes (%v) after %d parses, want its bytes at the densest price at both copies, %d, after a third", weight, err, parses.Load(), densest)
	}
	if refused, err := service.reserveRoom(ctx, room); refused != nil || !errors.Is(err, errRoomBudgetFull) || parses.Load() != 3 {
		t.Fatalf("an admission to a mebibyte past counting reserved %v (%v) after %d parses, want a refusal from the kept count", refused, err, parses.Load())
	}
	requireNothingReserved(t, service)
}

// Two admissions to one document no room holds, at the same moment, parse its stored version once:
// the second waits for the count the first started rather than starting its own
// (storedRoomReservation), and both reserve that count.
func TestTwoAdmissionsAtOnceCountTheStoredVersionOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	room := createDocuments(t, database, 1)[0]
	stored := latestVersionMarkdown(t, service, room)
	var parses atomic.Int64
	counting, release := make(chan struct{}, 2), make(chan struct{})
	service.budget.stored.beforeCount = func(string, int64) {
		parses.Add(1)
		counting <- struct{}{}
		<-release
	}
	type admission struct {
		reservation *roomReservation
		err         error
	}
	admissions := make(chan admission, 2)
	admit := func() {
		reservation, err := service.reserveRoom(ctx, room)
		admissions <- admission{reservation, err}
	}
	go admit()
	select {
	case <-counting:
	case <-time.After(10 * time.Second):
		t.Fatal("the first admission never started its count")
	}
	go admit()
	// Give the second admission a chance to reach the count the first holds, as the cold reads'
	// test gives its followers (TestCancelledColdReaderLeavesTheFoldForOtherReaders).
	time.Sleep(20 * time.Millisecond)
	select {
	case early := <-admissions:
		t.Fatalf("an admission returned %v (%v) while the count was held, want it waiting for that count", early.reservation, early.err)
	default:
	}
	close(release)
	want := roomWeight(copyHeap(int64(len(stored)), countElements(stored), storedHistoryStructs(t, database, room)), 0, 0)
	for range 2 {
		select {
		case got := <-admissions:
			if got.err != nil || got.reservation == nil || got.reservation.weight != want {
				t.Fatalf("an admission reserved %v (%v), want the stored version's count at both copies, %d", got.reservation, got.err, want)
			}
			defer service.budget.release(got.reservation)
		case <-time.After(10 * time.Second):
			t.Fatal("an admission never returned once the count was released")
		}
	}
	if parses.Load() != 1 {
		t.Fatalf("two admissions at once parsed the stored version %d times, want once", parses.Load())
	}
	if used := budgetUsed(service); used != 2*want {
		t.Fatalf("the budget holds %d bytes, want both admissions' %d", used, 2*want)
	}
}

// The counts of distinct documents each wait for a grant of the copy-heap bound, in the order they
// asked for it (copyHeaps), so in a burst that fills the bound the last of them waits for every
// count before it. Each count's read bound (coldReadTimeout) starts once it holds that grant, not
// when its admission asks: against a bound one count fills, n admissions of n documents whose
// counts each take twice the bound over n, the burst together twice the bound, all complete, none
// with a count that timed out, each document counted once and no two counts at the same time.
func TestABurstOfAdmissionsToDistinctDocumentsCountsEachWithinItsOwnReadBound(t *testing.T) {
	const n = 6
	const bound = 600 * time.Millisecond
	const slow = 2 * bound / n
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	rooms := createDocuments(t, database, n)
	useCopyHeaps(t, &copyHeaps{size: storedCountGrant(t, database, rooms[0], 1)})
	oldTimeout := coldReadTimeout
	coldReadTimeout = bound
	t.Cleanup(func() { coldReadTimeout = oldTimeout })
	var parses, running atomic.Int64
	var overlapped atomic.Bool
	service.budget.stored.beforeCount = func(string, int64) {
		if running.Add(1) > 1 {
			overlapped.Store(true)
		}
		parses.Add(1)
		time.Sleep(slow)
		running.Add(-1)
	}
	type admission struct {
		room        string
		reservation *roomReservation
		err         error
	}
	admissions := make(chan admission, n)
	started := time.Now()
	for _, room := range rooms {
		go func() {
			reservation, err := service.reserveRoom(ctx, room)
			admissions <- admission{room, reservation, err}
		}()
	}
	admitted := 0
	for range n {
		select {
		case got := <-admissions:
			switch {
			case got.err == nil && got.reservation != nil:
				admitted++
				defer service.budget.release(got.reservation)
			case errors.Is(got.err, errRoomBudgetFull):
			default:
				t.Errorf("the admission to %s answered %v (%v) %s into the burst, want it admitted or refused by the budget", got.room, got.reservation, got.err, time.Since(started).Round(time.Millisecond))
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an admission of the burst never returned")
		}
	}
	elapsed := time.Since(started)
	t.Logf("%d admissions of %d documents, each count %s against a read bound of %s: %d admitted in %s", n, n, slow, bound, admitted, elapsed.Round(time.Millisecond))
	if elapsed <= bound {
		t.Fatalf("the burst took %s, want it past one read bound, %s, so its last counts waited past it", elapsed, bound)
	}
	if parses.Load() != n || overlapped.Load() {
		t.Fatalf("the burst parsed %d times, overlapping %v, want each of the %d documents counted once and one count at a time", parses.Load(), overlapped.Load(), n)
	}
}

// An admission whose request ends while the count it waits for still waits for its grant of the
// copy-heap bound returns at once with its request's error, and leaves the count to the admissions
// still waiting for it, which are admitted at that count, the document parsed once. A count every
// admission has left behind ends its wait for that grant and never parses: nobody is waiting for
// it. The bound here holds one count, so the count holding it keeps the others waiting.
func TestAnAdmissionWhoseRequestEndsWhileItsCountWaitsForItsGrantLeavesTheCountToTheOthers(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	ids := createDocuments(t, database, 3)
	held, shared, abandoned := ids[0], ids[1], ids[2]
	useCopyHeaps(t, &copyHeaps{size: storedCountGrant(t, database, held, 1)})
	var mu sync.Mutex
	parsed := map[string]int{}
	counting, release := make(chan struct{}, 1), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	service.budget.stored.beforeCount = func(room string, _ int64) {
		mu.Lock()
		parsed[room]++
		mu.Unlock()
		if room == held {
			counting <- struct{}{}
			<-release
		}
	}
	type admission struct {
		reservation *roomReservation
		err         error
	}
	admit := func(ctx context.Context, room string) chan admission {
		answered := make(chan admission, 1)
		go func() {
			reservation, err := service.reserveRoom(ctx, room)
			answered <- admission{reservation, err}
		}()
		return answered
	}
	answer := func(answered chan admission, what string) admission {
		t.Helper()
		select {
		case got := <-answered:
			return got
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never returned", what)
			return admission{}
		}
	}
	waitingFor := func(room string) (flights, waiting int) {
		service.budget.stored.mu.Lock()
		defer service.budget.stored.mu.Unlock()
		for key, flight := range service.budget.stored.flights {
			if strings.HasPrefix(key, room+"@") {
				flights++
				waiting += flight.waiting
			}
		}
		return flights, waiting
	}

	holding := admit(ctx, held)
	select {
	case <-counting:
	case <-time.After(10 * time.Second):
		t.Fatal("the first admission's count never took its turn")
	}
	leaving, leave := context.WithCancel(ctx)
	defer leave()
	left := admit(leaving, shared)
	staying := admit(ctx, shared)
	gone, goAway := context.WithCancel(ctx)
	defer goAway()
	alone := admit(gone, abandoned)
	waitFor(t, 5*time.Second, "two admissions waiting for one count of the shared document, one for the abandoned", func() bool {
		sharedFlights, sharedWaiting := waitingFor(shared)
		abandonedFlights, abandonedWaiting := waitingFor(abandoned)
		return sharedFlights == 1 && sharedWaiting == 2 && abandonedFlights == 1 && abandonedWaiting == 1
	})

	leave()
	if got := answer(left, "the admission whose request ended"); got.reservation != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("the admission whose request ended answered %v (%v), want its request's end", got.reservation, got.err)
	}
	if flights, waiting := waitingFor(shared); flights != 1 || waiting != 1 {
		t.Fatalf("once one of its admissions left, the shared document has %d counts with %d admissions waiting, want its one count with the other still waiting", flights, waiting)
	}
	goAway()
	if got := answer(alone, "the abandoned document's only admission"); got.reservation != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("the abandoned document's only admission answered %v (%v), want its request's end", got.reservation, got.err)
	}
	if flights, _ := waitingFor(abandoned); flights != 0 {
		t.Fatalf("the abandoned document still has %d counts in flight once its only admission left, want none", flights)
	}

	released.Do(func() { close(release) })
	for what, answered := range map[string]chan admission{"the first admission": holding, "the admission that stayed": staying} {
		got := answer(answered, what)
		if got.err != nil || got.reservation == nil {
			t.Fatalf("%s answered %v (%v), want it admitted", what, got.reservation, got.err)
		}
		defer service.budget.release(got.reservation)
	}
	stored := latestVersionMarkdown(t, service, shared)
	if want := roomWeight(copyHeap(int64(len(stored)), countElements(stored), storedHistoryStructs(t, database, shared)), 0, 0); budgetUsed(service) != 2*want {
		t.Fatalf("the budget holds %d bytes, want the two admissions' %d each", budgetUsed(service), want)
	}
	mu.Lock()
	defer mu.Unlock()
	if parsed[held] != 1 || parsed[shared] != 1 || parsed[abandoned] != 0 {
		t.Fatalf("the documents were parsed %v times, want the held and the shared once each and the abandoned never", parsed)
	}
}

// Counts of distinct documents run beside each other, as many at once as the copy-heap bound has
// room for and no more (storedCountWeight, countStoredVersion): against a bound of three counts,
// six admissions to six documents, each count held where it would parse, run three at once; the
// fourth waits, and the bound never holds more than its size. Once they are released every
// document is counted once and every admission reserves that count.
func TestCountsOfStoredVersionsRunBesideEachOtherWithinTheCopyHeapBound(t *testing.T) {
	const n, together = 6, 3
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	rooms := createDocuments(t, database, n)
	weight := storedCountGrant(t, database, rooms[0], 1)
	heaps := useCopyHeaps(t, &copyHeaps{size: together * weight})
	var most atomic.Int64
	heaps.granted = func(held int64) {
		for seen := most.Load(); held > seen && !most.CompareAndSwap(seen, held); seen = most.Load() {
		}
	}
	var parses atomic.Int64
	counting, release := make(chan string, n), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	service.budget.stored.beforeCount = func(room string, _ int64) {
		parses.Add(1)
		counting <- room
		<-release
	}
	type admission struct {
		reservation *roomReservation
		err         error
	}
	admissions := make(chan admission, n)
	for _, room := range rooms {
		go func() {
			reservation, err := service.reserveRoom(ctx, room)
			admissions <- admission{reservation, err}
		}()
	}
	started := map[string]bool{}
	for range together {
		select {
		case room := <-counting:
			started[room] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the %d counts the bound has room for started", len(started), together)
		}
	}
	// The fourth count waits for its grant: nothing else gives one back while these three are held.
	select {
	case room := <-counting:
		t.Fatalf("a fourth count of %s started with the %d-byte bound held by %d counts of %d bytes each", room, heaps.limit(), together, weight)
	case <-time.After(250 * time.Millisecond):
	}
	if parses.Load() != together {
		t.Fatalf("%d counts parsed with the bound held by %d, want only those", parses.Load(), together)
	}
	released.Do(func() { close(release) })
	for range n - together {
		select {
		case <-counting:
		case <-time.After(10 * time.Second):
			t.Fatal("a count left waiting for its grant never ran once the first counts gave theirs back")
		}
	}
	for range n {
		select {
		case got := <-admissions:
			if got.err != nil || got.reservation == nil {
				t.Fatalf("an admission of the burst answered %v (%v), want it admitted", got.reservation, got.err)
			}
			defer service.budget.release(got.reservation)
		case <-time.After(30 * time.Second):
			t.Fatal("an admission of the burst never returned")
		}
	}
	t.Logf("%d counts of %d bytes each against a %d-byte bound: %d ran at once, the bound holding %d at its most",
		n, weight, heaps.limit(), together, most.Load())
	if parses.Load() != n {
		t.Fatalf("the burst parsed %d times, want each of the %d documents counted once", parses.Load(), n)
	}
	if most.Load() != heaps.limit() {
		t.Fatalf("the bound held %d bytes at its most, want all %d of it: %d counts of %d bytes ran at once", most.Load(), heaps.limit(), together, weight)
	}
}

// A document whose history outgrows its rendering reserves its structs and is charged them once
// loaded: a small document whose first paragraph took 100,000 characters, each typed before the
// last and then deleted (typedBeforeState), renders the paragraph it began as, a few elements, from
// over 100,000 structs. Its admission reads the struct count from a walk of its stored history,
// which decodes nothing (storedStructs), and reserves it at bytesOfHeapAStructHeld at both copies;
// its room's load prices the struct total it counts the same way (chargeLoadedRoom), within 1.05x
// of the reservation.
func TestAStructDenseDocumentReservesAndIsChargedItsStructs(t *testing.T) {
	const factor = 1.05
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.settle = time.Hour // no settlement writes into the room while it is weighed
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	room := createDocumentsHolding(t, database, 1, "# Title\n\nOne paragraph of ordinary prose.\n")[0]
	storeHistory(t, service, room, typedBeforeState(t, 100_000))
	stored := latestVersionMarkdown(t, service, room)
	structs := storedHistoryStructs(t, database, room)
	elements := countElements(stored)
	if structs < 100_000 || structs*bytesOfHeapAStructHeld <= elements*bytesOfHeapAnElement {
		t.Fatalf("the stored history holds %d structs beside a rendering of %d elements, want over 100,000 structs that outweigh the elements", structs, elements)
	}

	reservation, err := service.reserveRoom(ctx, room)
	if err != nil || reservation == nil {
		t.Fatalf("the admission was refused: %v", err)
	}
	if want := int64(structs * bytesOfHeapAStructHeld * roomCopies); reservation.weight != want {
		t.Fatalf("the admission reserved %d bytes, want its %d structs at both copies, %d, where its %d elements would reserve %d",
			reservation.weight, structs, want, elements, elements*bytesOfHeapAnElement*roomCopies)
	}
	service.budget.release(reservation)

	connection, err := dialDocument(httpServer.URL, room)
	if err != nil {
		t.Fatalf("open the document: %v", err)
	}
	waitFor(t, 30*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(room) != nil })
	requireNothingReserved(t, service)
	counted := liveStructs(t, service, room)
	rendering := renderedLive(t, service, room)
	loaded := budgetUsed(service)
	if want := counted * bytesOfHeapAStructHeld * roomCopies; loaded != want || counted*bytesOfHeapAStructHeld <= countElements(rendering)*bytesOfHeapAnElement {
		t.Fatalf("the loaded room is charged %d bytes for %d structs and %d elements, want its structs at both copies, %d",
			loaded, counted, countElements(rendering), want)
	}
	ratio := float64(reservation.weight) / float64(loaded)
	t.Logf("%d stored structs reserve %d bytes; the loaded room counts %d and is charged %d (%.4fx)", structs, reservation.weight, counted, loaded, ratio)
	if ratio < 1/factor || ratio > factor {
		t.Fatalf("the admission reserved %d bytes, %.4fx the %d its room is charged once loaded, want within %.2fx", reservation.weight, ratio, loaded, factor)
	}
	leaveDocument(connection)
}

// A document of one long run of text reserves its rendered bytes and is charged them once loaded:
// a paragraph of a mebibyte, its text one item, renders about a mebibyte making a few elements
// from a few structs, which would price it at a few kilobytes while each copy holds its text. Its
// admission reserves the stored version's bytes at bytesOfHeapARenderedByte at both copies, past
// what its elements and the structs of its stored history would reserve (copyHeap); its room's
// load prices its rendering the same way (chargeLoadedRoom), within 1.05x of the reservation.
func TestAParagraphOfAMebibyteReservesAndIsChargedItsRenderedBytes(t *testing.T) {
	const factor = 1.05
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.settle = time.Hour // no settlement writes into the room while it is weighed
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	const words = "words of one long paragraph "
	paragraph := strings.Repeat(words, (pmdoc.MaxDocumentBytes-4<<10)/len(words)) + "end"
	room := createDocumentsHolding(t, database, 1, "# Title\n\n"+paragraph+"\n")[0]
	stored := latestVersionMarkdown(t, service, room)
	structs := storedHistoryStructs(t, database, room)
	bytes, elements := int64(len(stored)), countElements(stored)
	byElements, byStructs := elements*bytesOfHeapAnElement, structs*bytesOfHeapAStructHeld
	if bytes < pmdoc.MaxDocumentBytes-8<<10 || bytes*bytesOfHeapARenderedByte <= max(byElements, byStructs) {
		t.Fatalf("the stored version is %d bytes making %d elements beside %d stored structs, want about a mebibyte whose bytes outweigh both", bytes, elements, structs)
	}

	reservation, err := service.reserveRoom(ctx, room)
	if err != nil || reservation == nil {
		t.Fatalf("the admission was refused: %v", err)
	}
	if want := bytes * bytesOfHeapARenderedByte * roomCopies; reservation.weight != want {
		t.Fatalf("the admission reserved %d bytes, want its %d rendered bytes at both copies, %d, where its %d elements would reserve %d and its %d structs %d",
			reservation.weight, bytes, want, elements, byElements*roomCopies, structs, byStructs*roomCopies)
	}
	service.budget.release(reservation)

	connection, err := dialDocument(httpServer.URL, room)
	if err != nil {
		t.Fatalf("open the document: %v", err)
	}
	waitFor(t, 30*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(room) != nil })
	requireNothingReserved(t, service)
	rendering := renderedLive(t, service, room)
	loaded := budgetUsed(service)
	if want := int64(len(rendering)) * bytesOfHeapARenderedByte * roomCopies; loaded != want {
		t.Fatalf("the loaded room is charged %d bytes for a %d-byte rendering making %d elements from %d structs, want its rendered bytes at both copies, %d",
			loaded, len(rendering), countElements(rendering), liveStructs(t, service, room), want)
	}
	ratio := float64(reservation.weight) / float64(loaded)
	t.Logf("a %d-byte version of %d elements and %d stored structs reserves %d bytes; the loaded room renders %d bytes and is charged %d (%.4fx)",
		bytes, elements, structs, reservation.weight, len(rendering), loaded, ratio)
	if ratio < 1/factor || ratio > factor {
		t.Fatalf("the admission reserved %d bytes, %.4fx the %d its room is charged once loaded, want within %.2fx", reservation.weight, ratio, loaded, factor)
	}
	leaveDocument(connection)
}

// Over the budget, the sweep closes the rooms nothing is still using and leaves the ones something
// is, then raises the signal the hook reads; the next browser is refused with the transient close
// before any room is made for it, while the server's own write still lands and its room is closed
// once it has; and once the last room in use is left, the budget closes it and lowers the signal.
func TestTheRoomBudgetClosesUnusedRoomsAndRefusesWhenNoneIsLeft(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 3)
	used, written, refused := ids[0], ids[1], ids[2]

	connection, err := dialDocument(httpServer.URL, used)
	if err != nil {
		t.Fatalf("open the document in use: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the room in use to go live", func() bool { return service.srv.GetDoc(used) != nil })
	appendThroughAPI(t, service, written)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, written) })

	service.SetRoomBudgetForTest(1)
	waitFor(t, 10*time.Second, "the sweep to close the unused room", func() bool { return service.srv.GetDoc(written) == nil })
	waitFor(t, 10*time.Second, "the budget to report itself full", service.budget.full.Load)
	if service.srv.GetDoc(used) == nil {
		t.Fatal("the sweep closed the room a browser holds open, want it kept")
	}

	requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	if service.srv.GetDoc(refused) != nil {
		t.Fatal("the refused browser's room was made, want none")
	}
	requireNothingReserved(t, service)

	// The server's own write is never refused: it opens its room over the budget, lands, and the
	// room goes once nothing holds it, the signal staying up for the room still in use.
	appendThroughAPI(t, service, refused)
	if text, err := service.Text(context.Background(), refused); err != nil || strings.Count(text, "Room cap probe.") != 2 {
		t.Fatalf("the document the server wrote over a full budget reads %q (%v), want the write", text, err)
	}
	waitFor(t, 10*time.Second, "the server's room to close once its write lands", func() bool { return service.srv.GetDoc(refused) == nil })
	if !service.budget.full.Load() {
		t.Fatal("the budget lowered its signal with the room in use still over it")
	}

	leaveDocument(connection)
	waitFor(t, 10*time.Second, "the left room to close", func() bool { return service.srv.GetDoc(used) == nil })
	waitFor(t, 10*time.Second, "the budget to lower its signal", func() bool { return !service.budget.full.Load() })
	if used := budgetUsed(service); used != 0 {
		t.Fatalf("the budget holds %d bytes with no room open, want none", used)
	}
}

// An admission the budget has no room for while it is within its limit starts a sweep that makes
// room for that admission, and waits for it: a reservation never takes the budget past its limit,
// so a sweep that stopped once the budget was within it would close nothing, and the admission
// would be refused while unused rooms stayed charged. The sweep closes unused rooms, the heaviest
// first, until the admission's weight fits and no further, and the admission is admitted into the
// room it made, its first attempt. Nothing in this raises full, which refuses growth in every room.
func TestTheRoomBudgetAdmissionWaitsForItsSweepAndIsAdmittedIntoTheRoomItMakes(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	// Each write's rendering is counted as it is published, so the rooms' charges are settled
	// before the limit is set against them.
	service.measurer.interval = noInterval
	ids := createDocuments(t, database, 4)
	unused, waiting := ids[:3], ids[3]
	for _, room := range unused {
		appendThroughAPI(t, service, room)
		waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, room) })
	}
	service.measurer.waitIdle()
	needs, err := service.storedRoomReservation(ctx, waiting)
	if err != nil {
		t.Fatal(err)
	}
	service.budget.mu.Lock()
	charged, lightest := service.budget.used, int64(0)
	for _, room := range unused {
		weight := service.budget.rooms[room].charges[0].weight
		if lightest == 0 || weight < lightest {
			lightest = weight
		}
	}
	service.budget.mu.Unlock()
	if needs <= 0 || needs > lightest {
		t.Fatalf("the waiting document reserves %d bytes and the lightest unused room is charged %d, want a reservation one room's close makes room for", needs, lightest)
	}

	service.SetRoomBudgetForTest(charged)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
	reservation, err := service.reserveRoom(ctx, waiting)
	if err != nil || reservation == nil {
		t.Fatalf("an admission to a budget its unused rooms fill exactly reserved %v (%v), want it admitted once its sweep made room", reservation, err)
	}
	defer service.budget.release(reservation)
	closed := 0
	for _, room := range unused {
		if service.srv.GetDoc(room) == nil {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("the sweep closed %d of the %d unused rooms, want the one that makes room for the admission", closed, len(unused))
	}
	if used := budgetUsed(service); used > charged || service.budget.full.Load() {
		t.Fatalf("after the admission the budget holds %d of %d bytes with full %t, want it within and full down", used, charged, service.budget.full.Load())
	}
	service.budget.mu.Lock()
	defer service.budget.mu.Unlock()
	if service.budget.wanted != 0 || service.budget.waiting != 0 {
		t.Fatalf("the admitted admission left %d bytes wanted by %d waiting, want none", service.budget.wanted, service.budget.waiting)
	}
}

// newRoomBudgetService is a document service over persist whose rooms stay resident a minute after
// their last use, as production's do, so what closes one in these tests is the budget's sweep.
func newRoomBudgetService(t *testing.T, database *store.Store, persist VersionedStore) *Service {
	t.Helper()
	service := New(Deps{
		Store:       database,
		Persistence: persist,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service
}

// admissionWatchStore records, at each load of a document's stored state (the socket's admission
// check, loadTree, and the room's own), the reservations the room budget holds for that document,
// and fails the loads of the document failing names.
type admissionWatchStore struct {
	VersionedStore
	service atomic.Pointer[Service]
	failing atomic.Value
	mu      sync.Mutex
	held    map[string][]int64
}

func (s *admissionWatchStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	budget := &s.service.Load().budget
	budget.mu.Lock()
	reserved := int64(0)
	if entry := budget.rooms[room]; entry != nil {
		for _, reservation := range entry.reservations {
			reserved += reservation.weight
		}
	}
	budget.mu.Unlock()
	s.mu.Lock()
	s.held[room] = append(s.held[room], reserved)
	s.mu.Unlock()
	if failing, _ := s.failing.Load().(string); failing == room {
		return persistence.LoadResult{}, errors.New("injected load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *admissionWatchStore) loads(room string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.held[room]...)
}

// requireHeldAtLoad requires the admission check's load of room, its first, to have found the
// socket's reservation of want bytes held.
func (s *admissionWatchStore) requireHeldAtLoad(t *testing.T, room string, want int64) {
	t.Helper()
	loads := s.loads(room)
	if len(loads) == 0 || loads[0] != want {
		t.Fatalf("the admission's load found %v bytes reserved, want %d first", loads, want)
	}
}

// requireClosedAtUpgrade opens room's socket and requires the server to complete the upgrade and
// close it at once with code and reason, before any sync.
func requireClosedAtUpgrade(t *testing.T, serverURL, room string, code int, reason string) {
	t.Helper()
	connection, err := dialDocument(serverURL, room)
	if err != nil {
		t.Fatalf("open the document: %v, want an upgrade the server closes", err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, message, err := connection.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != code || closed.Text != reason {
		t.Fatalf("first frame = %q (%v), want close %d %q before any sync", message, err, code, reason)
	}
}

// requireNothingReserved requires the budget to hold no reservation once the handlers the test
// drove have returned: a refusing handler returns once the client answers its close, a moment
// after the client has read it.
func requireNothingReserved(t *testing.T, service *Service) {
	t.Helper()
	waitFor(t, 5*time.Second, "the budget to hold no reservation", func() bool {
		service.budget.mu.Lock()
		defer service.budget.mu.Unlock()
		for _, entry := range service.budget.rooms {
			if len(entry.reservations) > 0 {
				return false
			}
		}
		return true
	})
}

func budgetUsed(service *Service) int64 {
	service.budget.mu.Lock()
	defer service.budget.mu.Unlock()
	return service.budget.used
}

// roomUnused is whether room's state holds nothing that outlasts the room (unusedLocked).
func roomUnused(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return service.unusedLocked(state)
}

// renderedLive is the rendering of room's resident document, which its load counted.
func renderedLive(t *testing.T, service *Service, room string) string {
	t.Helper()
	markdown, err := renderDocument(service.srv.GetDoc(room))
	if err != nil {
		t.Fatal(err)
	}
	return markdown
}

// storedHistoryStructs is the structs of every update room's history stores, each walked as a
// room's own state is (walkUpdate, fromRoom), summed: what an admission prices the stored history
// at (storedStructs), read here row by row.
func storedHistoryStructs(t *testing.T, database *store.Store, room string) int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select update from doc_updates where artifact_id = $1`, room)
	if err != nil {
		t.Fatalf("read the stored history: %v", err)
	}
	defer rows.Close()
	var structs uint64
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("read a stored update: %v", err)
		}
		tally, err := walkUpdate(update, fromRoom, nil)
		if err != nil {
			t.Fatalf("walk a stored update: %v", err)
		}
		structs += tally.structs
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored history: %v", err)
	}
	return int64(structs)
}

// liveStructs is the struct total room's resident document counted last (structTotal), which its
// load charged it by.
func liveStructs(t *testing.T, service *Service, room string) int64 {
	t.Helper()
	replica := service.liveReplica(service.srv.GetDoc(room))
	if replica == nil {
		t.Fatal("the room lists no replica")
	}
	return replica.structs.countedStructs()
}
