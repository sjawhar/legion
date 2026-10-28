//go:build memory

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The real-process drives of what bounds one browser's update and the documents a server holds
// open (INFRA-179): a Dispatch server in its own process (memory_harness_test.go), driven over its
// document socket as the editor drives it, its resident memory read from /proc.

const (
	// podMemory is the memory limit of the pod Dispatch runs in (infra/gcp/dispatch_kube.go:459),
	// and podGoMemoryLimit the Go limit a server in it takes, 0.9 of it (setGoMemoryLimit), which
	// the budget drives start their servers under, so they weigh the budget and not the library.
	podMemory        = 4 << 30
	podGoMemoryLimit = 3_865_470_566

	// These mirror constants of internal/dispatch/docs, which this package cannot read:
	// roomBudgetBytes (room_budget.go:42), bytesOfHeapAnElement (:56), roomCopies (:571),
	// maxPeerInsertAtTheBound (peer_update.go:110), maxPeerByteCreep (:115) and maxMessageBytes
	// (:227). No drive takes a mirror on trust. The server names its budget and what each refused
	// document needs in the line it logs for a refusal (refuseOverRoomBudget), and the budget
	// drives fail unless those are these numbers; the byte drive fails unless a run of small
	// inserts is refused exactly once the room is the creep past the bound, a paste past the
	// insert allowance is refused, and a frame of exactly the message floor reaches the room's
	// hook while one byte more does not.
	roomBudgetBytesForTest         = 2 << 30
	bytesOfHeapAnElementForTest    = 470
	roomCopiesForTest              = 2
	maxPeerInsertAtTheBoundForTest = 8 << 10
	maxPeerByteCreepForTest        = 64 << 10
	maxMessageBytesForTest         = 54 << 20
	// maxUnrenderedBytesForTest mirrors maxUnrenderedBytes (peer_update.go:125), 9.5 MiB, and
	// heaviestHandoffBoundForTest heaviestHandoffBound (:175), F, the sum of bounds the frame
	// total refuses past. The margin flood fails unless exactly as many records are admitted as
	// fit within the first, and the heaviest handoff unless the room refuses a paste with its
	// whole state within one paste of the second.
	maxUnrenderedBytesForTest   = 9_961_472
	heaviestHandoffBoundForTest = 44_936_089
	// perFrameScanCeilingForTest mirrors perFrameScanCeiling (internal/dispatch/docs/scan_budget.go:34),
	// the most items one browser's update the room weighs may scan. The server names the allowance
	// it refused each update at in the line it logs for the refusal (scanRefusalLine), and
	// TestAScanFrameIsRefusedWholeAndTheRoomIsAsItWas fails unless the first is this.
	perFrameScanCeilingForTest = 400_000

	// roomCountIntervalForTest mirrors roomCountInterval (internal/dispatch/docs/tree.go:487), the
	// least time between two counts of one room's elements, which is how stale a room's charge
	// can be. The drives that grow rooms send a round of growth no more often than this and two
	// seconds, so each round's counts have landed before the next is sent, as they would under a
	// person's typing; a wrong mirror changes only how far growth runs past the budget before it
	// is refused, which the bound on peak resident memory catches.
	roomCountIntervalForTest = 15 * time.Second

	// bytesOfHeapAStructHeldForTest mirrors bytesOfHeapAStructHeld
	// (internal/dispatch/docs/room_budget.go:80), the heap one copy of a room holds for each struct
	// of its state, measured (TestAResidentRoomsHeapPerStructOfItsState); bytesOfHeapAStructForTest
	// mirrors bytesOfHeapAStruct (:158), what the structure part charges each struct an update adds
	// and each split it can cause, and splitsAStructCanCauseForTest splitsAStructCanCause
	// (peer_update.go:652). TestARoomFedOnlyByAPIWritesIsChargedTheStructsTheyAdd weighs a room's
	// growth against them; a wrong mirror moves only how much growth it asks for, and its log line
	// prints each figure beside the server's own.
	bytesOfHeapAStructHeldForTest = 170
	bytesOfHeapAStructForTest     = 620
	splitsAStructCanCauseForTest  = 2
)

// One browser's update is bounded by bytes, over a real document socket of a real server
// (Service.weighPeerUpdate, internal/dispatch/docs/peer_update.go:229-448): an update that would
// take a document past the bound is refused with 4413 and DOC_TOO_LARGE, and the document is kept;
// small updates carry a document over the bound only by the creep they are allowed; a document at
// or over the bound can be opened, handed back whole, edited and shortened; an edit under it
// applies; and a frame up to the message floor reaches the room's hook, while one past it is
// closed by the websocket library before any room reads it.
func TestABrowsersUpdateIsBoundedByTheDocumentBound(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	const bound = pmdoc.MaxDocumentBytes
	upload := func(t *testing.T, markdown string) (string, string) {
		t.Helper()
		stored := server.upload(t, "json", memory.issue, markdown)
		if stored.status != http.StatusCreated {
			t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
		}
		memory.waitForSettlement(t, stored)
		return stored.artifactID, server.text(t, stored.artifactID)
	}
	openFor := func(t *testing.T, artifactID string) *browser {
		t.Helper()
		opened := server.openBrowser(t, artifactID)
		t.Cleanup(opened.leave)
		return opened
	}
	underID, under := upload(t, proseMarkdown(t, bound-32<<10))
	if len(under) <= bound-64<<10 || len(under) >= bound-16<<10 {
		t.Fatalf("the document under the bound renders %d bytes, want between %d and %d", len(under), bound-64<<10, bound-16<<10)
	}

	t.Run("an update past the bound is refused and the document kept", func(t *testing.T) {
		pasting := openFor(t, underID)
		paste := strings.Repeat(" pasted", 64<<10/7)
		update := browserEdit(t, pasting.state(t), func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += paste })
		applied, closed, err := pasting.send(pasting.updateFrame(update))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("a paste of %d bytes into a %d-byte document: applied %v, closed %v", len(paste), len(under), applied, closed)
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
		memory.waitForSettled(t, underID)
		if after := server.text(t, underID); after != under {
			t.Fatalf("the document reads %d bytes after the refused paste, want the %d it held", len(after), len(under))
		}
	})

	t.Run("an edit under the bound applies", func(t *testing.T) {
		editing := openFor(t, underID)
		const word = " edited"
		update := browserEdit(t, editing.state(t), func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += word })
		requireApplied(t, editing, update, "a one-word edit of a document under the bound")
		if after := server.text(t, underID); len(after) != len(under)+len(word) || !strings.Contains(after, "word"+word) {
			t.Fatalf("the document reads %d bytes after a one-word edit of %d, want the word and %d bytes", len(after), len(under), len(under)+len(word))
		}
	})

	// Every insert of the run is under the allowance a document at the bound still takes, so
	// what stops it is the creep cap: the run carries the document over the bound, and is refused
	// once the room's rendering is the creep past it.
	runID, _ := upload(t, proseMarkdown(t, bound-40<<10))
	t.Run("a run of small updates stops at the creep past the bound", func(t *testing.T) {
		typing := openFor(t, runID)
		document := newBrowserDocument(t, typing.state(t))
		chunk := strings.Repeat(" grow", 800)
		if len(chunk) >= maxPeerInsertAtTheBoundForTest {
			t.Fatalf("each insert is %d bytes, want it under the %d a document at the bound takes", len(chunk), maxPeerInsertAtTheBoundForTest)
		}
		var lengths []int
		var refusedAt int
		for range 64 {
			before := server.text(t, runID)
			update := document.edit(t, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += chunk })
			applied, closed, err := typing.send(typing.updateFrame(update))
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				lengths = append(lengths, len(before))
				continue
			}
			requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
			memory.waitForSettled(t, runID)
			if after := server.text(t, runID); after != before {
				t.Fatalf("the document reads %d bytes after the refused insert, want the %d it held", len(after), len(before))
			}
			refusedAt = len(before)
			break
		}
		if refusedAt == 0 || len(lengths) == 0 {
			t.Fatalf("%d inserts applied before a refusal at %d bytes, want inserts applied and then the run refused at the creep past the bound", len(lengths), refusedAt)
		}
		t.Logf("%d inserts of %d bytes applied, from a %d-byte document; refused at %d bytes, %d past the bound", len(lengths), len(chunk), lengths[0], refusedAt, refusedAt-bound)
		if last := lengths[len(lengths)-1]; last >= bound+maxPeerByteCreepForTest || last < bound {
			t.Errorf("the last insert applied to a %d-byte document, want one over the bound (%d) and under the creep past it (%d)", last, bound, bound+maxPeerByteCreepForTest)
		}
		if refusedAt < bound+maxPeerByteCreepForTest || refusedAt > bound+maxPeerByteCreepForTest+len(chunk) {
			t.Errorf("the run was refused at %d bytes, want no earlier than the creep past the bound (%d) and no later than the one insert that crossed it (%d)", refusedAt, bound+maxPeerByteCreepForTest, bound+maxPeerByteCreepForTest+len(chunk))
		}
	})

	t.Run("a document at the bound opens and edits, and one over it shortens", func(t *testing.T) {
		atID, at := upload(t, proseMarkdown(t, bound))
		opening := openFor(t, atID)
		state := opening.state(t)
		applied, closed, err := opening.send(opening.stateFrame(state))
		if err != nil || !applied || closed != nil {
			t.Fatalf("handing back the whole state of a %d-byte document: applied %v, closed %v, %v; want it applied and the socket open", len(at), applied, closed, err)
		}
		update := browserEdit(t, state, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += " edited" })
		requireApplied(t, opening, update, fmt.Sprintf("a one-word edit of a %d-byte document", len(at)))

		over := server.text(t, runID)
		if len(over) <= bound {
			t.Fatalf("the run's document is %d bytes, want it over the bound", len(over))
		}
		shortening := openFor(t, runID)
		const cut, paragraphs = 1_000, 30
		update = browserEdit(t, shortening.state(t), func(tree *pmdoc.Node) {
			for _, text := range paddingTexts(t, tree)[1 : 1+paragraphs] {
				shortenPadding(t, text, cut)
			}
		})
		requireInsertsNothing(t, update)
		requireApplied(t, shortening, update, fmt.Sprintf("a cut of %d bytes from a %d-byte document", cut*paragraphs, len(over)))
		if after := server.text(t, runID); len(after) != len(over)-cut*paragraphs {
			t.Fatalf("the document reads %d bytes after a cut of %d from %d, want %d", len(after), cut*paragraphs, len(over), len(over)-cut*paragraphs)
		}
	})

	t.Run("the heaviest whole state is taken, and a frame past the floor is closed before any room", func(t *testing.T) {
		// A heading weighs four elements, so headings reach both halves of the bound together
		// where each is a sixteen-thousandth of it, as TestTheHeaviestWholeStateFrameFitsTheMessageFloor
		// builds them; an upload is weighed on its rendering, which sets a blank line between two
		// headings, so each is written a byte short of that.
		heading := strings.Repeat("a", 60)
		markdown := strings.Repeat("# "+heading+"\n", pmdoc.MaxDocumentElements/4)
		tree, err := pmdoc.Parse(markdown)
		if err != nil {
			t.Fatal(err)
		}
		if size := renderedSize(t, tree); size.Bytes > bound || size.Bytes < bound-64 || size.Elements != pmdoc.MaxDocumentElements {
			t.Fatalf("the headings render %+v, want %d bytes making %d elements", size, bound, pmdoc.MaxDocumentElements)
		}
		heaviestID, _ := upload(t, markdown)
		suggestions := fillMargin(t, server, memory.issue, heaviestID, heading)
		heaviest := openFor(t, heaviestID)
		frame := heaviest.stateFrame(heaviest.state(t))
		applied, closed, err := heaviest.send(frame)
		t.Logf("the whole state of a document at both halves of the bound with %d suggestions of 255,000 bytes is a %d-byte frame, 1/%.1f of the %d-byte floor: applied %v, closed %v",
			suggestions, len(frame), float64(maxMessageBytesForTest)/float64(len(frame)), maxMessageBytesForTest, applied, closed)
		if err != nil || !applied || closed != nil {
			t.Fatalf("handing back the heaviest whole state: applied %v, closed %v, %v; want it applied and the socket open", applied, closed, err)
		}

		floorID, floor := upload(t, "The floor.\n")
		atFloor := openFor(t, floorID)
		state := atFloor.state(t)
		applied, closed, err = atFloor.send(floorFrame(t, atFloor, state, maxMessageBytesForTest))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("a %d-byte frame, the floor: applied %v, closed %v", maxMessageBytesForTest, applied, closed)
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
		pastFloor := openFor(t, floorID)
		applied, closed, err = pastFloor.send(floorFrame(t, pastFloor, state, maxMessageBytesForTest+1))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("a %d-byte frame, one past the floor: applied %v, closed %v", maxMessageBytesForTest+1, applied, closed)
		if applied || closed == nil || closed.Code != gws.CloseMessageTooBig {
			t.Fatalf("a frame one byte past the floor: applied %v, closed %v; want it closed %d by the websocket library before the room's hook reads it", applied, closed, gws.CloseMessageTooBig)
		}
		if after := server.text(t, floorID); after != floor {
			t.Fatalf("the document reads %q after the frames at and past the floor, want %q", after, floor)
		}
	})
}

// A frame no other rule refuses but whose merge would scan past the room's per-frame scan ceiling
// (perFrameScanCeiling, internal/dispatch/docs/scan_budget.go) is refused whole, over a real
// document socket of a real server, and the room is left as it was. A paragraph is first made a
// chain of tangledChain one-character items, each of a client of its own, so a run of them is never
// merged into one item; then a frame whose items each name the chain's first item as their left
// origin and no right origin, so each scans the whole chain, after a 256 KiB string at the chain's
// end, which makes the room weigh the frame's apply and so give it the whole ceiling. It passes the
// walker, the 8 KiB rule (the document is far under the bound), the struct total and the
// apply-heap bound, and the room's bounded document refuses it at an allowance of the ceiling: its
// browser is closed with 4413 and DOC_TOO_TANGLED, the room's encoded state is byte for byte what
// it was, a browser joining after the refusal receives that state, the stored document once
// settled holds it, and the budget's figures and the room's struct total, which the server logs at
// each refusal as the refusal leaves them, are the same at a second refusal of the frame as at the
// first, so the first left them as it found them. The refused scans count toward the room's scan
// total, whose million items leave more than the ceiling after the first refusal, so the second is
// at the ceiling again, still DOC_TOO_TANGLED, with the room's total grown by what the first
// refusal's scans passed. A frame one item shorter, whose scans stay within the ceiling, applies
// once the server has started again and loaded the room afresh, whose total holds none of the
// refused scans: what refused the frame is the ceiling and nothing else.
func TestAScanFrameIsRefusedWholeAndTheRoomIsAsItWas(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	stored := server.upload(t, "json", memory.issue, "# Tangled\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	setting := server.openBrowser(t, room)
	t.Cleanup(setting.leave)
	requireApplied(t, setting, chainFrame(t, setting.state(t)), fmt.Sprintf("a chain of %d one-character items", tangledChain))
	// The room's count of what the chain added lands within roomCountIntervalForTest, and its
	// settlement after the room's settle delay; once both have, nothing moves the budget's figures
	// or the struct total but what the browsers below send.
	time.Sleep(roomCountIntervalForTest + 2*time.Second)
	memory.waitForSettled(t, room)
	before := setting.state(t)

	frame, items, scans := scanFramePast(t, before, tangledScanClient, perFrameScanCeilingForTest)
	t.Logf("a %d-byte frame of %d items scanning %d items, against the %d-byte state of the room it is sent to",
		len(frame), items, scans, len(before))
	for refusal := range 2 {
		refused := server.openBrowser(t, room)
		t.Cleanup(refused.leave)
		if state := refused.state(t); !bytes.Equal(state, before) {
			t.Fatalf("before refusal %d the room's state is %d bytes, want the %d it held before the first", refusal+1, len(state), len(before))
		}
		applied, closed, err := refused.send(refused.updateFrame(frame))
		if err != nil {
			t.Fatal(err)
		}
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooTangledCloseReason)
	}
	refusals := server.scanRefusals(t, 2)
	t.Logf("the server logged the refusals as %q and %q", refusals[0][0], refusals[1][0])
	if refusals[0][1] != room || !slices.Equal(refusals[0][2:5], refusals[1][2:5]) {
		t.Fatalf("the refusals left the room's figures at %q and then at %q, want the room's and the same at both", refusals[0][1:5], refusals[1][1:5])
	}
	if limit, err := strconv.ParseInt(refusals[0][3], 10, 64); err != nil || limit != roomBudgetBytesForTest {
		t.Fatalf("the refusal names a budget of %s bytes, want %d", refusals[0][3], roomBudgetBytesForTest)
	}
	for index, refusal := range refusals {
		if allowance, err := strconv.Atoi(refusal[5]); err != nil || allowance != perFrameScanCeilingForTest {
			t.Fatalf("refusal %d names an allowance of %s items, want the %d ceiling", index+1, refusal[5], perFrameScanCeilingForTest)
		}
	}
	first, firstErr := strconv.ParseInt(refusals[0][6], 10, 64)
	second, secondErr := strconv.ParseInt(refusals[1][6], 10, 64)
	if firstErr != nil || secondErr != nil || second <= first {
		t.Fatalf("the refusals left the room's scan total at %s and then %s, want the second refusal's past the first's", refusals[0][6], refusals[1][6])
	}

	joined := server.openBrowser(t, room)
	t.Cleanup(joined.leave)
	if state := joined.state(t); !bytes.Equal(state, before) {
		t.Fatalf("a browser joining after the refusals received a %d-byte state, want the %d-byte state from before them", len(state), len(before))
	}
	memory.waitForSettled(t, room)
	if storedState, want := canonicalState(t, memory.storedState(t, room)), canonicalState(t, before); !bytes.Equal(storedState, want) {
		t.Fatalf("the stored document is a %d-byte state, want the room's %d-byte state from before the refusals", len(storedState), len(want))
	}

	shorter := scanFrame(tangledScanClient, items-1)
	shorterScans := frameScans(t, before, shorter)
	if shorterScans > perFrameScanCeilingForTest {
		t.Fatalf("a frame one item shorter scans %d items, want within the %d ceiling", shorterScans, perFrameScanCeilingForTest)
	}
	server.stop(t)
	again := memory.start(t)
	reopened := again.openBrowser(t, room)
	t.Cleanup(reopened.leave)
	requireApplied(t, reopened, shorter, fmt.Sprintf("a frame of %d items scanning %d", items-1, shorterScans))
}

// A room's scan total survives a reload, over a real document socket of a real server: a room
// takes two browsers' frames, each scanning just under the per-frame ceiling, which leave about a
// fifth of the room's scan total; a third frame scanning more than is left, and less than the
// ceiling, is refused DOC_TOO_TANGLED at an allowance of what is left. The server stops, which
// flushes the room to the store, and a new server loads the room from what it stored; the same
// third frame is refused there at an allowance within a few hundred items of the first: the
// reloaded room's total starts at what its stored state re-scans, which is what the first room had
// come to, short of the scans of the refusal, which the store never held. Each frame's client is
// above the one before it, so a decode of the stored state, which takes each client's structs in
// the order of their clients (sjawhar/ygo v1.51.3-sami.7, crdt/update.go:251), integrates the
// frames in the order the room applied them.
func TestTheRoomScanTotalSurvivesAReloadOverARealSocket(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	stored := server.upload(t, "json", memory.issue, "# Tangled\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	setting := server.openBrowser(t, room)
	t.Cleanup(setting.leave)
	requireApplied(t, setting, chainFrame(t, setting.state(t)), fmt.Sprintf("a chain of %d one-character items", tangledChain))

	admitted := 0
	for client := range crdt.ClientID(2) {
		_, items, _ := scanFramePast(t, setting.state(t), tangledScanClient+client, perFrameScanCeilingForTest)
		frame := scanFrame(tangledScanClient+client, items-1)
		scans := frameScans(t, setting.state(t), frame)
		requireApplied(t, setting, frame, fmt.Sprintf("a frame of %d items scanning %d", items-1, scans))
		admitted += scans
	}
	after := setting.state(t)
	third, thirdItems, thirdScans := scanFramePast(t, after, tangledScanClient+2, 300_000)
	t.Logf("the room took two frames scanning %d items together; the third frame, of %d items, scans %d", admitted, thirdItems, thirdScans)

	refuse := func(server *memoryServer) int {
		t.Helper()
		refused := server.openBrowser(t, room)
		t.Cleanup(refused.leave)
		if state := refused.state(t); !bytes.Equal(canonicalState(t, state), canonicalState(t, after)) {
			t.Fatalf("the room holds a %d-byte state, want the %d-byte state after the first two frames", len(state), len(after))
		}
		applied, closed, err := refused.send(refused.updateFrame(third))
		if err != nil {
			t.Fatal(err)
		}
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooTangledCloseReason)
		refusal := server.scanRefusals(t, 1)[0]
		t.Logf("the server logged %q", refusal[0])
		allowance, err := strconv.Atoi(refusal[5])
		if err != nil || allowance >= thirdScans || allowance >= perFrameScanCeilingForTest {
			t.Fatalf("the refusal names an allowance of %s items, want what is left of the room's total, under the frame's %d scans and the %d ceiling",
				refusal[5], thirdScans, perFrameScanCeilingForTest)
		}
		return allowance
	}
	left := refuse(server)
	memory.waitForSettled(t, room)
	server.stop(t)
	again := memory.start(t)
	reloaded := refuse(again)
	t.Logf("the room was refused at %d items left, and reloaded at %d", left, reloaded)
	if difference := reloaded - left; difference < -500 || difference > 500 {
		t.Fatalf("the reloaded room refused the frame at an allowance of %d, want within 500 of the %d the room had left before the reload", reloaded, left)
	}
}

// scanFramePast is scanFrame of client with the fewest items whose scans pass past in a document
// holding state (frameScans), its items and its scans. Each item scans about as many items as the
// next - the chain, and the items of frames of lower clients already past its origin - so a first
// reading says about how many pass past, and the count then moves an item at a time to the fewest
// that do.
func scanFramePast(t *testing.T, state []byte, client crdt.ClientID, past int) (frame []byte, items, scans int) {
	t.Helper()
	items = max(past/tangledChain-1, 1)
	if scans = frameScans(t, state, scanFrame(client, items)); scans > 0 {
		items = max(past*items/scans, 1)
	}
	for ; ; items++ {
		frame = scanFrame(client, items)
		if scans = frameScans(t, state, frame); scans > past {
			break
		}
	}
	for items > 1 {
		shorter := scanFrame(client, items-1)
		shorterScans := frameScans(t, state, shorter)
		if shorterScans <= past {
			break
		}
		frame, items, scans = shorter, items-1, shorterScans
	}
	return frame, items, scans
}

// frameScans is how many items ygo's apply of frame into a document holding state scans, read from
// the apply's transaction (crdt.Transaction.IntegrateScanned): what the room's apply of it scans,
// since the room holds the chain as a decode of state does, each item of a client of its own.
func frameScans(t *testing.T, state, frame []byte) int {
	t.Helper()
	doc := newBrowserDocument(t, state).doc
	scanned := 0
	doc.OnAfterTransaction(func(txn *crdt.Transaction) { scanned = txn.IntegrateScanned() })
	if err := crdt.ApplyUpdateV1(doc, frame, nil); err != nil {
		t.Fatal(err)
	}
	return scanned
}

// scanRefusals waits until the server has logged at least n scan refusals (scanRefusalLine) and
// returns them. The server's output reaches the test through a pipe the process copies on its own
// goroutine, so a line the server logged before it closed the socket can still be on its way.
func (p *memoryServer) scanRefusals(t *testing.T, n int) [][]string {
	t.Helper()
	var refusals [][]string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		refusals = scanRefusalLine.FindAllStringSubmatch(p.output.String(), -1)
		if len(refusals) >= n || time.Now().After(deadline) {
			break
		}
	}
	if len(refusals) != n {
		t.Fatalf("the server logged %d scan refusals, want %d:\n%s", len(refusals), n, p.output.tail())
	}
	return refusals
}

// tangledChain is how many one-character items chainFrame chains onto a paragraph, and
// tangledChainClient the client of the first of them, each next item's client one more.
const (
	tangledChain       = 2_000
	tangledChainClient = 1_000
	// tangledScanClient is the client of scanFrame's items, above every client of the chain, so
	// each of its items wins the tie-break against each item of the chain and scans on past it
	// (sjawhar/ygo v1.51.3-sami.7, crdt/item.go:160-169).
	tangledScanClient = 4_000_000
	// scanFramePadding is the bytes of the string scanFrame opens with: its 256 KiB take the
	// frame's own share of its apply-heap estimate past the 2 MiB under which the room does not
	// weigh an apply (unweighedApplyHeapBytes, internal/dispatch/docs/peer_update.go), about nine
	// bytes of estimate a frame byte, so the frame's allowance is the whole per-frame ceiling.
	scanFramePadding = 256 << 10
)

// chainFrame is a frame of tangledChain one-character strings after the last character of the
// first paragraph of state, each of a client of its own, from tangledChainClient up, naming the
// one before it as its left origin and no right origin: every one lands at the paragraph's end,
// scanning nothing, and none merges with its neighbour, since ygo merges only one client's run.
func chainFrame(t *testing.T, state []byte) []byte {
	t.Helper()
	document := newBrowserDocument(t, state)
	var text *crdt.YXmlText
	for _, child := range document.fragment.Children() {
		if element, ok := child.(*crdt.YXmlElement); ok && element.NodeName == "paragraph" {
			for _, inner := range element.Children() {
				if found, ok := inner.(*crdt.YXmlText); ok && text == nil {
					text = found
				}
			}
			break
		}
	}
	if text == nil || text.Len() == 0 {
		t.Fatal("the document holds no paragraph text to chain onto")
	}
	last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
	if last == nil {
		t.Fatal("the paragraph's last character has no item")
	}
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(tangledChain)
		origin := *last
		for i := range tangledChain {
			client := crdt.ClientID(tangledChainClient + i)
			encoder.WriteVarUint(1)
			encoder.WriteVarUint(uint64(client))
			encoder.WriteVarUint(0)
			writeOriginString(encoder, origin)
			origin = crdt.ID{Client: client, Clock: 0}
		}
		encoder.WriteVarUint(0)
	})
}

// scanFrame is a frame of client's: a string of scanFramePadding bytes after the chain's last item,
// where nothing follows it to scan, and then items one-character strings, each naming the first
// item chainFrame chained as its left origin and no right origin, so each scans the chain from
// there to its end (sjawhar/ygo v1.51.3-sami.7, crdt/item.go:151-191).
func scanFrame(client crdt.ClientID, items int) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(uint64(items + 1))
		encoder.WriteVarUint(uint64(client))
		encoder.WriteVarUint(0)
		writeOriginText(encoder, crdt.ID{Client: tangledChainClient + tangledChain - 1, Clock: 0}, strings.Repeat("p", scanFramePadding))
		for range items {
			writeOriginString(encoder, crdt.ID{Client: tangledChainClient, Clock: 0})
		}
		encoder.WriteVarUint(0)
	})
}

// writeOriginString writes one V1 struct: a string "k" naming origin as its left origin and no right
// origin (writeOriginText).
func writeOriginString(encoder *encoding.Encoder, origin crdt.ID) {
	writeOriginText(encoder, origin, "k")
}

// writeOriginText writes one V1 struct: the string text naming origin as its left origin and no
// right origin, whose parent is its origin's (Yjs's info byte: the content's reference, 4 for a
// string, with bit 0x80 for a left origin).
func writeOriginText(encoder *encoding.Encoder, origin crdt.ID, text string) {
	encoder.WriteUint8(4 | 0x80)
	encoder.WriteVarUint(uint64(origin.Client))
	encoder.WriteVarUint(origin.Clock)
	encoder.WriteVarString(text)
}

// canonicalState is state decoded into a document of its own and encoded again, so two states that
// hold the same structs and deletions compare equal however their updates were folded.
func canonicalState(t *testing.T, state []byte) []byte {
	t.Helper()
	return crdt.EncodeStateAsUpdateV1(newBrowserDocument(t, state).doc, nil)
}

// scanRefusalLine is the line the server logs for each update its room refuses past its scan
// allowance, with the budget's figures, the room's struct total and its scan total as the refusal
// leaves them, and the allowance it was refused at (Service.weighPeerUpdate,
// internal/dispatch/docs/peer_update.go).
var scanRefusalLine = regexp.MustCompile(`msg="dispatch: refused a browser's update its room could not merge within its scan allowance" room=(\S+) update_bytes=\d+ charged=(\d+) limit=(\d+) struct_total=(\d+) allowance=(\d+) scan_total=(\d+) scans_left=(-?\d+)`)

// setRoomBudget sets the server's room budget to bytes through its test-only route, so a drive of
// the document's own bounds is not refused by the budget first; the drive puts the real one back
// when it ends.
func (p *memoryServer) setRoomBudget(t *testing.T, bytes int64) {
	t.Helper()
	set := func(bytes int64) error {
		body, err := json.Marshal(map[string]int64{"bytes": bytes})
		if err != nil {
			return err
		}
		answer, err := p.trySend(http.MethodPost, "/api/v1/artifacts/_test/room-budget", "application/json", strings.NewReader(string(body)), http.Header{"X-Dispatch-User": {"alice"}})
		if err == nil && answer.status != http.StatusOK {
			err = fmt.Errorf("status %d body %.300s", answer.status, answer.body)
		}
		return err
	}
	if err := set(bytes); err != nil {
		t.Fatalf("set the room budget to %d bytes: %v", bytes, err)
	}
	t.Cleanup(func() { _ = set(0) })
}

// unboundedRoomBudget is a room budget no drive of one document's bounds comes near: the budget
// charges a byte of content the measurer has not yet counted at the densest price, 2,116 bytes at
// both copies (bytesOfHeapAByteAtTheDensest, internal/dispatch/docs/room_budget.go:135), so a
// mebibyte written in one update would fill the real budget until the room's next count.
const unboundedRoomBudget = 1 << 50

// marginFloodRecord is the update a browser that is not the editor sends to write one margin
// record of text under key, as a client of its own: one struct, one clock, a map entry whose
// value is a map holding the text, as the server writes a comment's record.
func marginFloodRecord(client uint64, key, text string) []byte {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(client)))
	margin := doc.GetMap("marks")
	doc.Transact(func(txn *crdt.Transaction) { margin.Set(txn, key, map[string]any{"text": text}) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// marginFloodUnrendered is the unrendered bytes one marginFloodRecord carries, worked out from the
// wire as the server's walk counts them (unrenderedBytes, internal/dispatch/docs/peer_update.go):
// its key as encoded, and its content, the count of values and the one lib0 Any value.
func marginFloodUnrendered(key, text string) int {
	encoded := encoding.NewEncoder()
	encoded.WriteVarString(key)
	encoded.WriteVarUint(1)
	encoded.WriteAny(map[string]any{"text": text})
	return len(encoded.Bytes())
}

// The flood the unrendered bound closes, over a real document socket of a real server: a browser
// that is not the editor sends eighty updates of a million margin bytes each, every one a single
// clock and a single struct of a client of its own, which before the bound were all admitted and
// took a room's whole-state frame from 264 bytes to 80,001,457, past the message floor every
// browser of the document then reconnects against. Each is admitted while the room's unrendered
// bytes stay within maxUnrenderedBytes - nine, for a document whose own are a few hundred - and the
// first past it and every one after it is refused with 4413 and DOC_TOO_LARGE. The stored
// document, once settled, is a whole-state frame within F and under the floor, and a browser
// opening it receives that state and hands it back whole without its socket being closed. The
// room budget is set out of reach first, so what refuses is the document's bound and nothing else.
func TestEightyMarginFloodUpdatesStopAtTheUnrenderedBoundAndTheFrameStaysUnderTheFloor(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	server.setRoomBudget(t, unboundedRoomBudget)
	stored := server.upload(t, "json", memory.issue, "# Flooded\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	const updates, size = 80, 1_000_000
	text := strings.Repeat("m", size)
	each := marginFloodUnrendered("c00", text)
	want := maxUnrenderedBytesForTest / each
	admitted, refused := 0, 0
	flooding := server.openBrowser(t, room)
	for i := range updates {
		applied, closed, err := flooding.send(flooding.updateFrame(marginFloodRecord(uint64(5_000_000+i), fmt.Sprintf("c%02d", i), text)))
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			if refused > 0 {
				t.Fatalf("update %d was applied after %d refusals, want every update after the first refusal refused", i, refused)
			}
			admitted++
			continue
		}
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
		refused++
		flooding.leave()
		flooding = server.openBrowser(t, room)
	}
	flooding.leave()
	memory.waitForSettled(t, room)
	frame := len(canonicalState(t, memory.storedState(t, room)))
	t.Logf("%d of %d updates of %d unrendered bytes admitted against the %d-byte bound, %d refused; the stored whole-state frame is %d bytes, against F %d and the %d-byte floor",
		admitted, updates, each, maxUnrenderedBytesForTest, refused, frame, heaviestHandoffBoundForTest, maxMessageBytesForTest)
	if admitted != want {
		t.Fatalf("%d updates were admitted, want the %d that fit within the %d-byte unrendered bound", admitted, want, maxUnrenderedBytesForTest)
	}
	if frame > heaviestHandoffBoundForTest || frame >= maxMessageBytesForTest {
		t.Fatalf("the stored whole-state frame is %d bytes, want it within F (%d) and under the %d-byte floor", frame, heaviestHandoffBoundForTest, maxMessageBytesForTest)
	}
	handing := server.openBrowser(t, room)
	t.Cleanup(handing.leave)
	state := handing.state(t)
	if applied, closed, err := handing.send(handing.stateFrame(state)); err != nil || !applied || closed != nil {
		t.Fatalf("handing back the flooded document's %d-byte whole state: applied %v, closed %v, %v; want it applied and the socket open", len(state), applied, closed, err)
	}
}

// The heaviest whole state a browser can hand over under the bounds is admitted, over a real
// document socket of a real server, where the floor before them would have closed it. F
// (heaviestHandoffBound) is a sum of bounds no test can build term by term, so the drive builds its
// frame total instead, through what only that total sees: text nested under the margin, which
// renders nothing and adds no unrendered bytes, pasted a million bytes an update until the room
// refuses a paste with 4413 and DOC_TOO_LARGE because it would take the frame total past F. The
// room's whole state then stands within one paste of F; a delete-only update at F is admitted; and
// a browser opening the document receives that state and hands it back whole, a frame far past
// the 37 MiB floor of before and under maxMessageBytes, without its socket being closed. The room
// budget is set out of reach first, so what refuses is the document's bound and nothing else.
func TestTheHeaviestHandoffTheBoundsAllowIsNotClosedForSize(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	server.setRoomBudget(t, unboundedRoomBudget)
	stored := server.upload(t, "json", memory.issue, "# Heavy\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	const nestedKey = "nested"
	// nestedText is a browser's copy of the room's state and the text nested under its margin.
	nestedText := func(t *testing.T, state []byte) (*crdt.Doc, *crdt.YText) {
		t.Helper()
		doc := newBrowserDocument(t, state).doc
		value, ok := doc.GetMap("marks").Get(nestedKey)
		text, isText := value.(*crdt.YText)
		if !ok || !isText {
			t.Fatalf("the margin holds %T under %q, want the nested text", value, nestedKey)
		}
		return doc, text
	}

	pasting := server.openBrowser(t, room)
	doc := newBrowserDocument(t, pasting.state(t)).doc
	margin := doc.GetMap("marks")
	held := doc.StateVector()
	doc.Transact(func(txn *crdt.Transaction) { margin.Set(txn, nestedKey, crdt.NewTextPrelim()) })
	requireApplied(t, pasting, crdt.EncodeStateAsUpdateV1(doc, held), "a text nested under the margin")
	value, _ := margin.Get(nestedKey)
	text, isText := value.(*crdt.YText)
	if !isText {
		t.Fatalf("the browser's margin holds %T under %q, want the text it nested", value, nestedKey)
	}
	paste := strings.Repeat("t", 1_000_000)
	pastes := 0
	for ; ; pastes++ {
		if pastes > heaviestHandoffBoundForTest/len(paste)+1 {
			t.Fatalf("%d pastes of %d bytes were all applied, want one refused before the frame total passes F (%d)", pastes, len(paste), heaviestHandoffBoundForTest)
		}
		held := doc.StateVector()
		doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), paste, nil) })
		applied, closed, err := pasting.send(pasting.updateFrame(crdt.EncodeStateAsUpdateV1(doc, held)))
		if err != nil {
			t.Fatal(err)
		}
		if !applied {
			requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
			break
		}
	}
	pasting.leave()

	cutting := server.openBrowser(t, room)
	t.Cleanup(cutting.leave)
	atF := cutting.state(t)
	t.Logf("%d pastes of %d bytes under the margin were applied before one was refused; the room's whole state is %d bytes, F %d",
		pastes, len(paste), len(atF), heaviestHandoffBoundForTest)
	// The refused paste adds its text and its struct's header, a few dozen bytes, to the frame.
	if len(atF) > heaviestHandoffBoundForTest || len(atF)+len(paste)+64 <= heaviestHandoffBoundForTest {
		t.Fatalf("the room's whole state is %d bytes after the refusal, want it within F (%d) and short of it by less than the refused %d-byte paste",
			len(atF), heaviestHandoffBoundForTest, len(paste))
	}
	cutDoc, cutText := nestedText(t, atF)
	held = cutDoc.StateVector()
	cutDoc.Transact(func(txn *crdt.Transaction) { cutText.Delete(txn, 0, 1_000) })
	cut := crdt.EncodeStateAsUpdateV1(cutDoc, held)
	requireInsertsNothing(t, cut)
	requireApplied(t, cutting, cut, "a cut of 1,000 bytes from a room at F")

	handing := server.openBrowser(t, room)
	t.Cleanup(handing.leave)
	state := handing.state(t)
	frame := handing.stateFrame(state)
	applied, closed, err := handing.send(frame)
	t.Logf("the whole state of the room at F is a %d-byte frame, against the 37 MiB floor of before (%d) and the %d-byte floor: applied %v, closed %v",
		len(frame), 37<<20, maxMessageBytesForTest, applied, closed)
	if err != nil || !applied || closed != nil {
		t.Fatalf("handing back the %d-byte whole state of a room at F: applied %v, closed %v, %v; want it applied and the socket open", len(frame), applied, closed, err)
	}
	if len(frame) <= 37<<20 || len(frame) >= maxMessageBytesForTest {
		t.Fatalf("the handoff is a %d-byte frame, want one past the 37 MiB floor of before and under the %d-byte floor", len(frame), maxMessageBytesForTest)
	}
}

// The room budget refuses the browser its own arithmetic says it has no room for: documents of a
// mebibyte of prose, each edited once, opened until one is refused with 4503 and DOC_BUSY, under the
// Go limit a 4 GiB pod gives the server. A room is charged at both copies from its admission
// (roomCopies, internal/dispatch/docs/room_budget.go:571), so the refusals begin within one room of
// the budget divided by what one such room reserves - its stored version's elements at the price of
// one, at both copies - computed here from the server's constants and held to the numbers the server
// logs, both when the browsers come one after another and when fifty open at once.
//
// One after another, five of the browsers leave once they have edited. Each refusal while their
// rooms are still charged starts a sweep that closes one of them to make room for the refused
// browser (sweepRooms), and that browser's next attempt, as the editor's backoff makes it, is
// admitted. Each close is read twice before that attempt: from the line the server logs for it, in
// the budget's own figures, and from the room's absence, which a test-only read reports (roomLive).
// Each refusal is read from its close frame and from its document's absence. A browser that stayed
// keeps its room, the server's peak resident memory stays under the pod's, and once the rest have
// left, a browser is admitted again.
//
// Fifty at once, the budget admits within one room of the same number and refuses the rest at
// admission, and none of the admitted browsers' first edits is refused: each room's second copy was
// charged before the edit made it.
func TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays(t *testing.T) {
	const burst, gone = 50, 5
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	fleet := memory.mebibyteFleet(t, func(int64) int { return burst + 4 })
	rooms := int(roomBudgetBytesForTest / fleet.reserved)
	t.Logf("a mebibyte of prose makes %d elements and reserves %d bytes at admission: the %d-byte budget holds %d such rooms", fleet.elements, fleet.reserved, roomBudgetBytesForTest, rooms)
	if rooms+gone+3 > len(fleet.copies) || rooms+2 > burst {
		t.Fatalf("the budget holds %d rooms, too many for the %d copies and the burst of %d this drive opens", rooms, len(fleet.copies), burst)
	}

	t.Run("one after another", func(t *testing.T) {
		server := memory.start(t)
		requirePodGoMemoryLimit(t, server)
		edit := browserEdit(t, fleet.state, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += " Edited once." })
		var stayed []*browser
		t.Cleanup(func() {
			for _, held := range stayed {
				held.leave()
			}
		})
		next := 0
		// admit opens a browser on the next copy and has it edit once. A browser the budget refuses
		// leaves next on its copy, for its next attempt, and its refusal is read twice: from the
		// close frame, and from its document's absence.
		admit := func(t *testing.T) (*browser, *gws.CloseError) {
			t.Helper()
			if next >= len(fleet.copies)-1 {
				t.Fatalf("the drive opened %d copies, all but the last, which it keeps for the end", next)
			}
			opened, err := server.tryOpenBrowser(fleet.copies[next])
			if closed := (*gws.CloseError)(nil); errors.As(err, &closed) {
				requireClose(t, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, "a refused browser")
				requireRoomAbsent(t, server, fleet.copies[next], "a refused browser's document")
				return nil, closed
			} else if err != nil {
				t.Fatal(err)
			}
			next++
			requireApplied(t, opened, edit, "a browser's first edit")
			return opened, nil
		}
		var left []string
		var firstRefusalOpen int
		var final *gws.CloseError
		// leftClosed is the closes the budget logged of rooms whose browsers went. A sweep with
		// none of those left to close goes on to any other room nothing holds, such as one the
		// server opened for its own work, heaviest first.
		leftClosed := func() []budgetClose {
			return slices.DeleteFunc(budgetCloses(t, server), func(made budgetClose) bool { return !slices.Contains(left, made.room) })
		}
		peak := server.peakResident(t, func() {
			// Those who stay, then those who go, one room short of the budget's arithmetic: the
			// rooms of those who went are the budget's to close once a browser needs their room.
			for len(stayed)+len(left) < rooms-1 {
				opened, refused := admit(t)
				if refused != nil {
					t.Fatalf("the budget refused a browser with %d rooms open, want it to admit at least %d", len(stayed)+len(left), rooms-1)
				}
				if len(stayed) < rooms-1-gone {
					stayed = append(stayed, opened)
					continue
				}
				left = append(left, opened.room)
				opened.leave()
			}
			for _, room := range left {
				memory.waitForSettled(t, room)
			}
			// Then more who stay. Each refusal while a room whose browser went is charged is
			// answered by a close, and the refused browser's next attempt is admitted.
			closed, justClosed := 0, false
			for {
				opened, refused := admit(t)
				if opened != nil {
					stayed = append(stayed, opened)
					justClosed = false
					continue
				}
				if justClosed {
					t.Fatalf("a refused browser's next attempt was refused again (%v) after the budget closed a room for it", refused)
				}
				if firstRefusalOpen == 0 {
					firstRefusalOpen = len(stayed) + len(left)
				}
				if closed == len(left) {
					final = refused
					return
				}
				// The refusal started a sweep. A room whose browser went may still have been
				// settling when it looked, and closable only after, so the browser tries again, as
				// the editor's backoff does, until a close of one is logged; no attempt may be
				// admitted before one is.
				deadline := time.Now().Add(45 * time.Second)
				for len(leftClosed()) == closed {
					if time.Now().After(deadline) {
						t.Fatalf("no room whose browser went was closed in the 45 s after a refusal, with %d of the %d closed", closed, len(left))
					}
					time.Sleep(3 * time.Second)
					if len(leftClosed()) > closed {
						break
					}
					if again, _ := admit(t); again != nil {
						t.Fatalf("a refused browser was admitted with no room closed for it, %d of the %d rooms whose browsers went closed", closed, len(left))
					}
				}
				made := leftClosed()[closed]
				closed++
				t.Logf("refused with %d rooms held by browsers that stayed; the budget closed %s, %d -> %d of %d bytes charged, to make room for %d", len(stayed), made.room, made.chargedBefore, made.charged, made.limit, made.makingRoomFor)
				if made.limit != roomBudgetBytesForTest || made.makingRoomFor != fleet.reserved ||
					made.chargedBefore+made.makingRoomFor <= made.limit || made.charged+made.makingRoomFor > made.limit {
					t.Errorf("the budget closed a room at %d -> %d of %d bytes to make room for %d, want a close that a reservation of %d needed and that made room for it", made.chargedBefore, made.charged, made.limit, made.makingRoomFor, fleet.reserved)
				}
				requireRoomAbsent(t, server, made.room, "a room the budget logged as closed")
				justClosed = true
			}
		})
		t.Logf("the first refusal came with %d rooms open, %d of them left by their browsers; the budget closed those one refusal at a time and refused again with %d rooms held by browsers that stayed (%v); peak resident memory %d MiB", firstRefusalOpen, len(left), len(stayed), final, peak>>20)
		if firstRefusalOpen < rooms-1 || firstRefusalOpen > rooms+1 {
			t.Errorf("the first refusal came with %d rooms open, want within one of the budget's %d", firstRefusalOpen, rooms)
		}
		if len(stayed) < rooms-1 || len(stayed) > rooms+1 {
			t.Errorf("the last refusal came with %d rooms held by browsers that stayed, want within one of the budget's %d", len(stayed), rooms)
		}
		refusals := requireBudgetRefusals(t, server, fleet.reserved)
		if charged := refusals[len(refusals)-1].charged; charged >= int64(len(stayed)+1)*fleet.reserved {
			t.Errorf("at the last refusal the budget charged %d bytes, %d rooms' worth for the %d browsers that stayed: a room whose browser had gone was still charged", charged, charged/fleet.reserved, len(stayed))
		}
		// The last refusal's sweep finds no room whose browser went, and closes no room a browser
		// holds. What else it closes is a room nothing holds, which the server opened for work of
		// its own; it is logged by name.
		time.Sleep(5 * time.Second)
		if got := len(leftClosed()); got != len(left) {
			t.Errorf("the budget closed %d rooms whose browsers went, want each of the %d once", got, len(left))
		}
		for _, made := range budgetCloses(t, server) {
			if slices.Contains(left, made.room) {
				continue
			}
			if slices.ContainsFunc(stayed, func(held *browser) bool { return held.room == made.room }) {
				t.Errorf("the budget closed %s, a room a browser that stayed holds", made.room)
				continue
			}
			t.Logf("the budget also closed %s, %s, a room no browser of this drive opened, %d -> %d bytes charged", made.room, memory.artifactName(t, made.room), made.chargedBefore, made.charged)
		}
		for _, held := range stayed {
			if err := held.ping(); err != nil {
				t.Errorf("a browser that stayed lost its room: %v", err)
			} else if !server.roomLive(t, held.room) {
				t.Errorf("a browser that stayed answers in its room, and the server says document %s is not live", held.room)
			}
		}
		if peak >= podMemory {
			t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
		}

		for _, held := range stayed {
			held.leave()
		}
		started := time.Now()
		for {
			admitted, err := server.tryOpenBrowser(fleet.copies[len(fleet.copies)-1])
			if err == nil {
				admitted.leave()
				t.Logf("a browser was admitted again %s after the others left", time.Since(started).Round(time.Second))
				break
			}
			if closed := (*gws.CloseError)(nil); !errors.As(err, &closed) || closed.Code != contracts.DocumentBusyCloseCode {
				t.Fatal(err)
			}
			if time.Since(started) > 3*time.Minute {
				t.Fatalf("no browser was admitted in the %s after the others left, want one once their rooms were closed", time.Since(started).Round(time.Second))
			}
			time.Sleep(5 * time.Second)
		}
	})

	t.Run("fifty at once", func(t *testing.T) {
		server := memory.start(t)
		requirePodGoMemoryLimit(t, server)
		edit := browserEdit(t, fleet.state, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += " Edited in a burst." })
		type outcome struct {
			opened  *browser
			applied bool
			closed  *gws.CloseError
			err     error
		}
		outcomes := make([]outcome, burst)
		t.Cleanup(func() {
			for _, outcome := range outcomes {
				if outcome.opened != nil {
					outcome.opened.leave()
				}
			}
		})
		peak := server.peakResident(t, func() {
			var group sync.WaitGroup
			for index := range outcomes {
				group.Go(func() {
					result := &outcomes[index]
					result.opened, result.err = server.tryOpenBrowser(fleet.copies[index])
					if errors.As(result.err, &result.closed) {
						result.err = nil
						return
					}
					if result.err == nil {
						result.applied, result.closed, result.err = result.opened.send(result.opened.updateFrame(edit))
					}
				})
			}
			group.Wait()
		})
		applied, atAdmission, atEdit := 0, 0, 0
		for index, outcome := range outcomes {
			switch {
			case outcome.err != nil:
				t.Errorf("browser %d: %v", index, outcome.err)
			case outcome.applied:
				applied++
			case outcome.closed == nil:
				t.Errorf("browser %d's edit was not applied and its socket stayed open, want it applied or refused with %d", index, contracts.DocumentBusyCloseCode)
			default:
				requireClose(t, outcome.closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, fmt.Sprintf("browser %d", index))
				if outcome.opened != nil {
					atEdit++
					continue
				}
				atAdmission++
				requireRoomAbsent(t, server, fleet.copies[index], fmt.Sprintf("browser %d's document, refused at admission", index))
			}
		}
		t.Logf("%d browsers at once: %d edited their rooms, %d refused at admission, %d refused their edit; peak resident memory %d MiB", burst, applied, atAdmission, atEdit, peak>>20)
		requireBudgetRefusals(t, server, fleet.reserved)
		if applied < rooms-1 || applied > rooms+1 {
			t.Errorf("%d rooms took their browser's edit, want within one of the budget's %d", applied, rooms)
		}
		if atEdit != 0 {
			t.Errorf("%d admitted browsers had their first edit refused, want none: a room's second copy is charged from its admission", atEdit)
		}
		for index, outcome := range outcomes {
			if outcome.applied {
				if err := outcome.opened.ping(); err != nil {
					t.Errorf("browser %d lost its room: %v", index, err)
				}
			}
		}
		if peak >= podMemory {
			t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
		}
	})
}

// Rooms admitted small grow under browsers that stay, and the budget follows them up. Each room
// holds two sockets that never leave: one only holds the room, so nothing can close it, and one
// grows it in rounds of 2,048 one-letter headings (growthRounds), each four elements in five bytes
// of rendering, the same update bytes sent to every room, a round no more often than a room's
// elements are counted (roomCountIntervalForTest). The heap follows the elements and not the bytes
// (bytesOfHeapAnElement), so the rooms pass the budget while each document and each update stay far
// under the document bound, and what refuses them can only be the budget. With every room held
// there is no unused room to close and no browser asking for a new document; what this proves is
// that the charge follows the rooms up - no growth is refused until what the rooms hold, at the
// server's price of an element and both copies, is past the budget - that once it is, every update
// that would grow a room is refused with 4503 and DOC_BUSY while one that shrinks it still applies,
// and that the server's peak resident memory stays under the pod's throughout. Each refusal is read
// from its close frame and from the documents, which hold every round their rooms took, none they
// were refused, and nowhere the text of the probe every room refused.
func TestTheRoomBudgetFollowsRoomsThatGrowWhileTheirBrowsersStay(t *testing.T) {
	const rooms, probeText = 56, " More."
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	copies, state := memory.smallDocuments(t, rooms)
	charge := func(elements int) int64 { return roomCopiesForTest * int64(elements) * bytesOfHeapAnElementForTest }
	rounds := growthRounds(t, state, func(size pmdoc.DocumentSize) bool {
		return int64(rooms)*charge(size.Elements) <= roomBudgetBytesForTest*3/2
	})
	// The shrink deletes the first round's first half, which every room holds, from a copy of the
	// document that holds that round alone, so its bytes apply wherever the rounds stopped.
	shrinker := newBrowserDocument(t, state)
	if err := crdt.ApplyUpdateV1(shrinker.doc, rounds[0].update, nil); err != nil {
		t.Fatal(err)
	}
	shrink := shrinker.edit(t, func(tree *pmdoc.Node) {
		tree.Children = append(tree.Children[:2], tree.Children[2+headingsARound/2:]...)
	})
	requireInsertsNothing(t, shrink)
	probe := browserEdit(t, state, func(tree *pmdoc.Node) { tree.Children[1].Children[0].Text += probeText })

	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	holders, growers := make([]*browser, rooms), make([]*browser, rooms)
	t.Cleanup(func() {
		for _, held := range append(holders, growers...) {
			if held != nil {
				held.leave()
			}
		}
	})
	for index, room := range copies {
		holders[index], growers[index] = server.openBrowser(t, room), server.openBrowser(t, room)
	}
	took := make([]int, rooms)
	held := func() int64 {
		total := int64(0)
		for _, taken := range took {
			total += charge(rounds[taken-1].size.Elements)
		}
		return total
	}
	type refusal struct {
		round, room int
		held        int64
	}
	var refusals []refusal
	peak := server.peakResident(t, func() {
		for number, round := range rounds {
			started := time.Now()
			for index := range copies {
				if growers[index] == nil {
					continue
				}
				applied, closed, err := growers[index].send(growers[index].updateFrame(round.update))
				if err != nil {
					t.Fatalf("round %d, room %d: %v", number+1, index, err)
				}
				if applied {
					took[index] = number + 1
					continue
				}
				if number == 0 {
					t.Fatalf("room %d refused the first round, %v; want every room to take it", index, closed)
				}
				requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
				refusals = append(refusals, refusal{round: number + 1, room: index, held: held()})
				growers[index] = nil
			}
			taking := 0
			for _, grower := range growers {
				if grower != nil {
					taking++
				}
			}
			t.Logf("round %d, %d elements a room: %d rooms grew, the rooms hold %d bytes at the server's price, %d MiB resident", number+1, round.size.Elements, taking, held(), server.resident(t, "VmRSS")>>20)
			// Each room's count of this round lands within the measurer's interval
			// (roomCountIntervalForTest), one room at a time, before the next round is sent.
			time.Sleep(time.Until(started.Add(roomCountIntervalForTest + 2*time.Second)))
			if taking == 0 {
				break
			}
		}
		for index, room := range copies {
			asking := server.openBrowser(t, room)
			applied, closed, err := asking.send(asking.updateFrame(probe))
			if err != nil {
				t.Fatalf("room %d: %v", index, err)
			}
			requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
		}
		for index := range copies {
			requireApplied(t, holders[index], shrink, fmt.Sprintf("room %d's shrink", index))
		}
	})
	if len(refusals) == 0 {
		t.Fatal("no growth was refused, want the budget to refuse it once the rooms pass it")
	}
	t.Logf("%d growth updates refused, the first in round %d with the rooms holding %d bytes at the server's price; peak resident memory %d MiB", len(refusals), refusals[0].round, refusals[0].held, peak>>20)
	// What a room is charged is at most what it holds at the server's price: its count of
	// elements trails its rendering, and only the update bytes taken since stand beside it.
	if slack := int64(rooms) * int64(len(rounds[0].update)); refusals[0].held+slack <= roomBudgetBytesForTest {
		t.Errorf("growth was first refused with the rooms holding %d bytes, under the %d-byte budget: the budget charged more than the rooms hold", refusals[0].held, roomBudgetBytesForTest)
	}
	for index, room := range copies {
		text := server.text(t, room)
		if got, want := strings.Count(text, growthHeading), headingsARound*took[index]-headingsARound/2; got != want {
			t.Errorf("room %d reads %d grown headings after %d rounds and its shrink, want %d: a refused round reached it, or a taken one did not", index, got, took[index], want)
		}
		if strings.Contains(text, probeText) {
			t.Errorf("room %d reads the probe its room refused", index)
		}
	}
	if peak >= podMemory {
		t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
	}
}

// A server whose open documents fill the real budget - no test hook shrinks it - with every room
// held by a socket that stays, so none can be closed, answers four things as the budget says: the
// next browser is closed with 4503 and DOC_BUSY at its socket's upgrade, before any room is made
// for it; an update that would grow a document already open is refused the same way, and the
// document never gains it; so is one that adds elements and leaves the document no larger in bytes,
// which a test of bytes would let through; and a hundred updates that shrink documents are applied
// and stored. Meanwhile an API write to a document no room holds is taken, and the unrecorded-mark
// sweep writes. The server's peak resident memory stays under the pod's throughout.
//
// The budget is filled the way the server reaches a full one. An admission never takes it past its
// limit, since a room is charged at both copies from its reservation on (roomCopies), so browsers
// open documents of a mebibyte until the budget refuses one, which leaves less than one such room's
// worth of it free and raises nothing; then small rooms opened first grow under their browsers, a
// round of headings at a time, until what a round adds to its room's charge - the structure the
// peer hook charges for its structs and the count of its elements (internal/dispatch/docs,
// roomCharge) - takes the charges past the limit, a sweep finds nothing to close, and the budget is
// full (roomBudget.full): the next round is refused.
//
// Each refusal is read twice: an update's from its close frame and from its document, through the
// server's text and through the store, never holding it; a browser's from its close frame and from
// its document's absence (roomLive).
func TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks(t *testing.T) {
	// proofComment is the mark a comment anchors with (docs.MarkComment,
	// internal/dispatch/docs/marks.go:26).
	const growers, markID, commentMark = 4, "memory-unrecorded-mark", "proofComment"
	// A hundred shrinks are one browser's four cuts of fifty bytes each, sent to twenty-five
	// documents in the order it made them: ygo's update observer encodes a transaction's structs
	// beside the document's whole delete set (sjawhar/ygo v1.51.3-sami.7, crdt/doc.go:597-612), so
	// each carries the deletes of the cuts before it too, which a document that has taken them
	// holds.
	const shrunk, cutsEach, cutBytes = 25, 4, 50
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	fleet := memory.mebibyteFleet(t, func(reserved int64) int { return int(roomBudgetBytesForTest/reserved) + 4 })
	small, smallState := memory.smallDocuments(t, growers)
	// Rounds a growing room can take while it stays under the element bound every server write
	// obeys, so its settlement keeps writing: about seven, which across four rooms is several times
	// what one mebibyte room's reservation leaves of the budget.
	rounds := growthRounds(t, smallState, func(size pmdoc.DocumentSize) bool {
		return size.Elements+4*headingsARound < pmdoc.MaxDocumentElements
	})
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	var opened []*browser
	t.Cleanup(func() {
		for _, held := range opened {
			held.leave()
		}
	})
	open := func(t *testing.T, room string) *browser {
		t.Helper()
		holding := server.openBrowser(t, room)
		opened = append(opened, holding)
		return holding
	}
	paragraph := func(tree *pmdoc.Node, text *pmdoc.Node) int {
		for index, child := range tree.Children {
			for _, inline := range child.Children {
				if inline == text {
					return index
				}
			}
		}
		t.Fatal("no paragraph holds the text")
		return 0
	}
	mark := browserEdit(t, fleet.state, func(tree *pmdoc.Node) {
		text := paddingTexts(t, tree)[0]
		text.Marks = append(text.Marks, pmdoc.Mark{Type: commentMark, Attrs: pmdoc.Attrs{"id": markID, "by": "user:alice"}})
	})
	grow := browserEdit(t, fleet.state, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += " This sentence grows the document." })
	denser := browserEdit(t, fleet.state, func(tree *pmdoc.Node) {
		before := renderedSize(t, tree)
		text := paddingTexts(t, tree)[1]
		shortenPadding(t, text, 300)
		at := paragraph(tree, text) + 1
		var added []*pmdoc.Node
		for range 20 {
			added = append(added, &pmdoc.Node{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "a"}}})
		}
		tree.Children = append(tree.Children[:at], append(added, tree.Children[at:]...)...)
		pmdoc.EnsureBlockIDs(tree)
		if after := renderedSize(t, tree); after.Bytes > before.Bytes || after.Elements <= before.Elements {
			t.Fatalf("the denser edit leaves %+v of %+v, want no more bytes and more elements", after, before)
		}
	})
	shrinker := newBrowserDocument(t, fleet.state)
	var shrinking [][]byte
	for index := range cutsEach {
		shrinking = append(shrinking, shrinker.edit(t, func(tree *pmdoc.Node) { shortenPadding(t, paddingTexts(t, tree)[2+index], cutBytes) }))
		requireInsertsNothing(t, shrinking[index])
	}

	var held []*browser
	peak := server.peakResident(t, func() {
		growing := make([]*browser, growers)
		for index, room := range small {
			open(t, room) // holds the room once its grower is refused
			growing[index] = open(t, room)
		}
		next := 0
		var refused *gws.CloseError
		for {
			if next >= len(fleet.copies)-2 {
				t.Fatalf("all %d copies but the two kept for the end were admitted, want the budget to refuse one", next)
			}
			admitted, err := server.tryOpenBrowser(fleet.copies[next])
			if errors.As(err, &refused) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			opened = append(opened, admitted)
			held = append(held, admitted)
			next++
		}
		requireClose(t, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, "the browser past the budget")
		requireRoomAbsent(t, server, fleet.copies[next], "the browser past the budget")
		t.Logf("%d browsers hold documents of a mebibyte open, unedited, before the budget refused one: %v; %d MiB resident", len(held), refused, server.resident(t, "VmRSS")>>20)
		if len(held) < shrunk+4 {
			t.Fatalf("%d documents of a mebibyte are open, want at least %d", len(held), shrunk+4)
		}
		// The refused browser left the budget within its limit, so nothing is full: the mark,
		// which inserts, applies. It has no comment recorded for it, so the sweep removes it once it
		// is a minute old (sweepUnrecordedMarks, internal/dispatch/docs/marks.go:782-838), which is
		// after the budget is full.
		marking := server.openBrowser(t, held[0].room)
		requireApplied(t, marking, mark, "an unrecorded comment mark on a budget that refused a browser and is within its limit")
		marking.leave()

		took := make([]int, growers)
		refusedRound := 0
		for number, round := range rounds {
			started := time.Now()
			for index, grower := range growing {
				if grower == nil {
					continue
				}
				applied, closed, err := grower.send(grower.updateFrame(round.update))
				if err != nil {
					t.Fatalf("round %d, growing room %d: %v", number+1, index, err)
				}
				if applied {
					took[index] = number + 1
					continue
				}
				requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
				growing[index] = nil
				refusedRound = number + 1
			}
			if refusedRound != 0 {
				break
			}
			t.Logf("round %d, %d elements a growing room: every growing room took it, %d MiB resident", number+1, round.size.Elements, server.resident(t, "VmRSS")>>20)
			// Each growing room's count of this round lands within the measurer's interval
			// (roomCountIntervalForTest) before the next round is sent.
			time.Sleep(time.Until(started.Add(roomCountIntervalForTest + 2*time.Second)))
		}
		if refusedRound == 0 {
			t.Fatalf("all %d rounds of growth applied, want the budget to refuse growth once the rooms' counts take it past its limit", len(rounds))
		}
		t.Logf("growth was refused in round %d: the server is full, %d MiB resident", refusedRound, server.resident(t, "VmRSS")>>20)
		for index, room := range small {
			want := headingsARound * took[index]
			if got := strings.Count(server.text(t, room), growthHeading); got != want {
				t.Errorf("growing room %d reads %d grown headings after taking %d rounds, want %d", index, got, took[index], want)
			}
			// Every round the room took is stored once its settlement has run; the refused one never is.
			memory.waitForStoredText(t, room, func(text string) bool { return strings.Count(text, growthHeading) >= want })
			memory.waitForSettled(t, room)
			if got := strings.Count(memory.storedText(t, room), growthHeading); got != want {
				t.Errorf("growing room %d stores %d grown headings after taking %d rounds, want %d", index, got, took[index], want)
			}
		}

		_, err := server.tryOpenBrowser(fleet.copies[next])
		var closed *gws.CloseError
		if !errors.As(err, &closed) {
			t.Fatalf("the next browser on the full server: %v, want its socket closed", err)
		}
		requireClose(t, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, "the next browser on the full server")
		requireRoomAbsent(t, server, fleet.copies[next], "the next browser on the full server")
		if refusals := requireBudgetRefusals(t, server, fleet.reserved); refusals[len(refusals)-1].charged <= refusals[len(refusals)-1].limit {
			t.Errorf("the full server refused the next browser at %d of %d bytes charged, want the budget past its limit", refusals[len(refusals)-1].charged, refusals[len(refusals)-1].limit)
		}

		requireBusyAndUnchanged(t, memory, server, held[len(held)-1].room, grow, "an update that grows a document")
		requireBusyAndUnchanged(t, memory, server, held[len(held)-2].room, denser, "an update that adds elements and no bytes")

		// The shrinks go to the documents after the first, whose mark the sweep is to remove
		// meanwhile.
		shrinkingRooms := held[1 : 1+shrunk]
		lengths := make([]int, shrunk)
		for index, room := range shrinkingRooms {
			lengths[index] = len(server.text(t, room.room))
		}
		for _, update := range shrinking {
			for index, room := range shrinkingRooms {
				requireApplied(t, room, update, fmt.Sprintf("a shrink of document %d on the full server", index))
			}
		}
		for index, room := range shrinkingRooms {
			want := lengths[index] - cutBytes*len(shrinking)
			if after := len(server.text(t, room.room)); after != want {
				t.Errorf("document %d reads %d bytes after %d shrinks of %d from %d, want %d", index, after, len(shrinking), cutBytes, lengths[index], want)
			}
			memory.waitForStoredText(t, room.room, func(text string) bool { return len(text) == want })
		}
		t.Logf("%d shrinks applied to %d documents on the full server, and stored; %d MiB resident", len(shrinking)*shrunk, shrunk, server.resident(t, "VmRSS")>>20)

		written := fleet.copies[next+1]
		if server.roomLive(t, written) {
			t.Fatalf("document %s is live before the API write, want one no room holds", written)
		}
		if answer := server.edit(t, written, map[string]any{"op": "replace", "find": "Paragraph 3 holds", "with": "Paragraph 3 now holds"}); answer.status != http.StatusOK {
			t.Errorf("an API write to a document no room holds, on the full server: %d %.300s, want 200", answer.status, answer.body)
		} else if !strings.Contains(server.text(t, written), "Paragraph 3 now holds") {
			t.Error("an API write on the full server answered 200, and the document does not read it")
		}

		markRef := pmdoc.MarkRef{Type: commentMark, ID: markID}
		deadline := time.Now().Add(3 * time.Minute)
		for slices.Contains(pmdoc.ListMarks(newBrowserDocument(t, held[0].state(t)).tree(t)), markRef) {
			if time.Now().After(deadline) {
				t.Fatalf("the unrecorded mark %s was still in its document three minutes after it was made, want the sweep to remove it", markID)
			}
			time.Sleep(5 * time.Second)
		}
		memory.waitForStored(t, held[0].room, "the unrecorded mark's removal", func(state []byte) bool {
			return !slices.Contains(pmdoc.ListMarks(newBrowserDocument(t, state).tree(t)), markRef)
		})
		requireBusyAndUnchanged(t, memory, server, held[len(held)-3].room, grow, "an update that grows a document once the mark sweep has written")
		t.Log("the sweep removed the unrecorded mark, and the server still refuses growth")
	})
	t.Logf("peak resident memory %d MiB", peak>>20)
	if peak >= podMemory {
		t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
	}
}

// The room budget charges a room the structs the server's own writes add to it, over a real
// server: a room fed only by API writes, with one browser connected that sends nothing but its
// first sync, has its charge grow by at least the price of what a room's copy holds for a struct
// (bytesOfHeapAStructHeld) for each struct those writes add to its stored state, and its structure
// part by at least each write's one new struct with the splits its integration can cause, at both
// copies (roomCharge.wrote, internal/dispatch/docs/room_budget.go). None of the writes is refused,
// though the budget is past its limit throughout. Each write replaces one word of the room's
// paragraph with another of the same length, so the rendering, and what its elements are charged,
// stay as they were, and what grows is the structure part and the struct total each count prices.
//
// The charge is read the way the budget drives read the budget's figures, from the line the server
// logs for a browser it refuses (budgetRefusalLine): on a budget of one byte, a browser opening a
// document no room holds is refused with what the open rooms are charged, which is the fed room's
// charge alone once the sweeps have closed every room nothing holds. It is read once the room's
// count of its rendering has landed, before the writes and after them, so neither reading holds
// the uncounted part the writes' content adds until a count.
func TestARoomFedOnlyByAPIWritesIsChargedTheStructsTheyAdd(t *testing.T) {
	const writes = 200
	memory := newMemoryHarness(t)
	server := memory.start(t)
	var words strings.Builder
	for word := range writes {
		fmt.Fprintf(&words, " w%03d", word)
	}
	fed := server.upload(t, "json", memory.issue, "# Fed\n\nThe words"+words.String()+".\n")
	probe := server.upload(t, "json", memory.issue, "# Probe\n\nA document no browser is admitted to.\n")
	for _, stored := range []uploadResult{fed, probe} {
		if stored.status != http.StatusCreated {
			t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
		}
		memory.waitForSettlement(t, stored)
	}
	held := server.openBrowser(t, fed.artifactID)
	t.Cleanup(held.leave)
	// The fed room alone passes a budget of one byte: the sweep that follows closes every room
	// nothing holds, the probe's among them if the upload left it open, and leaves the browser's.
	server.setRoomBudget(t, 1)
	for deadline := time.Now().Add(2 * time.Minute); server.roomLive(t, probe.artifactID); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("the probe's room was never closed on a budget of one byte")
		}
	}
	charged := func(when string) int64 {
		t.Helper()
		// The room's count of what was written lands within roomCountIntervalForTest, and its
		// settlement after the room's settle delay.
		time.Sleep(roomCountIntervalForTest + 2*time.Second)
		memory.waitForSettled(t, fed.artifactID)
		seen := len(budgetRefusalLine.FindAllStringSubmatch(server.output.String(), -1))
		refused, err := server.tryOpenBrowser(probe.artifactID)
		if err == nil {
			refused.leave()
			t.Fatalf("%s: a browser was admitted to the probe on a budget of one byte", when)
		}
		var closed *gws.CloseError
		if !errors.As(err, &closed) {
			t.Fatalf("%s: the probe's socket failed with %v, want the budget's close", when, err)
		}
		requireClose(t, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, when+", the probe's socket")
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if refusals := budgetRefusalLine.FindAllStringSubmatch(server.output.String(), -1); len(refusals) > seen {
				last := refusals[len(refusals)-1]
				charged, chargedErr := strconv.ParseInt(last[1], 10, 64)
				limit, limitErr := strconv.ParseInt(last[2], 10, 64)
				if chargedErr != nil || limitErr != nil || limit != 1 {
					t.Fatalf("%s: the server refused the probe with %q, want what is charged against a budget of one byte", when, last[0])
				}
				return charged
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the server logged no refusal of the probe:\n%s", when, server.output.tail())
			}
		}
	}
	structs := func() int64 {
		t.Helper()
		count, err := docs.StateStructsForTest(memory.storedState(t, fed.artifactID))
		if err != nil {
			t.Fatalf("count the stored state's structs: %v", err)
		}
		return count
	}

	before, structsBefore := charged("before the writes"), structs()
	for word := range writes {
		find, with := fmt.Sprintf("w%03d", word), fmt.Sprintf("v%03d", word)
		if answer := server.edit(t, fed.artifactID, map[string]any{"op": "replace", "find": find, "with": with}); answer.status != http.StatusOK {
			t.Fatalf("API write %d answered %d %.300s on a budget of one byte, want 200", word+1, answer.status, answer.body)
		}
	}
	after := charged("after the writes")
	added := structs() - structsBefore
	grew := after - before
	byStructs := added * bytesOfHeapAStructHeldForTest
	byStructure := int64(writes * (1 + splitsAStructCanCauseForTest) * bytesOfHeapAStructForTest * roomCopiesForTest)
	t.Logf("%d API writes added %d structs to the stored state (%d -> %d); the room was charged %d bytes before them and %d after, %d more, against %d for the structs at %d bytes each and %d for a struct each in the structure part",
		writes, added, structsBefore, structsBefore+added, before, after, grew, byStructs, bytesOfHeapAStructHeldForTest, byStructure)
	if added < writes {
		t.Fatalf("%d API writes added %d structs to the stored state, want at least one each", writes, added)
	}
	if grew < byStructs {
		t.Errorf("the room's charge grew by %d bytes for %d structs its API writes added, want at least %d, %d bytes a struct", grew, added, byStructs, bytesOfHeapAStructHeldForTest)
	}
	if grew < byStructure {
		t.Errorf("the room's charge grew by %d bytes for %d API writes, want at least %d, one struct each with its splits at both copies in its structure part", grew, writes, byStructure)
	}
	text := server.text(t, fed.artifactID)
	for word := range writes {
		if !strings.Contains(text, fmt.Sprintf(" v%03d", word)) || strings.Contains(text, fmt.Sprintf(" w%03d", word)) {
			t.Fatalf("the room reads %q, want word %d replaced", text, word)
		}
	}
}

const (
	// maxUpdateItemsForTest mirrors maxUpdateItems (internal/dispatch/docs/persistence.go:875), the
	// decode cap: the most structs a stored document's state may hold. The struct-dense drives
	// build documents at it and fail unless their stored histories hold within 4,096 of it
	// (requireAtTheDecodeCap).
	maxUpdateItemsForTest = 1 << 20
	// bytesOfHeapARenderedByteForTest mirrors bytesOfHeapARenderedByte
	// (internal/dispatch/docs/room_budget.go:105), the third price of a copy beside its elements
	// and its structs (copyHeap, :592-602), from which storedShape computes what an admission
	// reserves; the struct-dense drives fail unless the server's refusals name that reservation.
	bytesOfHeapARenderedByteForTest = 12
	// applyHeapBoundForTest mirrors applyHeapBound (internal/dispatch/docs/peer_update.go:1465)
	// and copyHeapBoundForTest copyHeapBound (copy_heap.go:54): a third each of what the Go limit
	// leaves above the room budget, the cold reads' cache and the pressure reserve. The server
	// names its bound in the line it logs for a grant (heapGrantLine), and the heaviest-apply drive
	// fails unless each is this number.
	applyHeapBoundForTest = 393_705_335
	copyHeapBoundForTest  = 393_705_336
	// copyHeapPerByteForTest mirrors copyHeapPerByte (copy_heap.go:40), what a full-state decode
	// is granted for each byte of the state it decodes, so a state of more than 8,202,195 bytes is
	// granted the whole copy bound and decodes alone among the copies.
	copyHeapPerByteForTest = 48
	// heaviestHandoffEstimate is the hook's estimate of the heaviest honest handoff at F, measured
	// into the room holding it (internal/dispatch/docs/peer_update.go:1485-1489): the long-edited
	// document typed on until 4,096 structs short of the decode cap and padded to F, the shape the
	// heaviest-apply drive hands back. The server logs no estimate, only that each handoff held the
	// whole apply bound (heapGrantLine), which an estimate past applyHeapBoundForTest does; the
	// drive quotes it in its arithmetic.
	heaviestHandoffEstimate = 904_378_600
)

// longEditFixturePath is the long-edited fixture (internal/dispatch/docs/struct_cap_measure_test.go:53-57),
// the 5 KiB document a million keystrokes left, from this package's directory.
const longEditFixturePath = "../../internal/dispatch/docs/testdata/long_edited_document.update.gz"

// A burst of browsers opening struct-dense documents that render little, all at once, under the
// Go limit a 4 GiB pod gives the server and at the collector's default pace, keeps the server's
// peak resident memory, read from /proc, under the pod's 4 GiB. The burst is eight copies each of
// the two struct-dense shapes a room keeps - the long-edited 5 KiB document typed on until its
// state stands at the decode cap (longEditedTypedTo), and the document an agent rewrote whole
// through the API until the decode cap refused a write (agentRewrittenDocument), about a million
// structs each - and eight documents of a mebibyte of prose, the ordinary document A3
// (TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays) weighs. A struct-dense document is priced at
// its structs (bytesOfHeapAStructHeldForTest), about 356 MB at both copies, where its elements
// would price it at kilobytes, so the budget admits at most six of them, and the ordinary ones
// the rest of it has room for.
//
// Every refusal is 4503 and DOC_BUSY at admission, with the document left unopened, and within the
// budget's arithmetic: the line the server logs for it names what the open rooms were charged and
// what the refused document needs, which must be that document's price as computed here from the
// server's constants and its stored history (storedShape), and the two together must pass the
// budget. An ordinary document is refused only so, needing what A3 reserves for it, so none is
// refused earlier than A3 allows. Every admitted browser receives its document's whole state and
// answers a ping.
func TestABurstOfStructDenseDocumentsStaysUnderThePodsMemory(t *testing.T) {
	const copiesOfEach, ordinary = 8, "a mebibyte of prose"
	building := time.Now()
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	longEdited := memory.storedShape(t, "the long-edited document at the decode cap",
		memory.storedStates(t, longEditedTypedTo(t, maxUpdateItemsForTest))[0])
	agent := memory.storedShape(t, "the agent-rewritten document", memory.agentRewrittenDocument(t))
	fleet := memory.mebibyteFleet(t, func(int64) int { return copiesOfEach })
	type opening struct {
		shape    string
		room     string
		reserved int64
		opened   *browser
		closed   *gws.CloseError
		err      error
	}
	var burst []opening
	for _, shape := range []storedShape{longEdited, agent} {
		requireAtTheDecodeCap(t, shape)
		for _, room := range memory.cloneDocuments(t, shape.id, copiesOfEach) {
			burst = append(burst, opening{shape: shape.name, room: room, reserved: shape.reserved})
		}
	}
	for _, room := range fleet.copies {
		burst = append(burst, opening{shape: ordinary, room: room, reserved: fleet.reserved})
	}
	t.Logf("the %d-byte budget holds %d struct-dense rooms of the heavier price and %d rooms of a mebibyte of prose, which reserves %d bytes (%d elements), as in A3; the documents took %s to build",
		roomBudgetBytesForTest, roomBudgetBytesForTest/max(longEdited.reserved, agent.reserved), roomBudgetBytesForTest/fleet.reserved,
		fleet.reserved, fleet.elements, time.Since(building).Round(time.Second))

	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	t.Cleanup(func() {
		for _, opened := range burst {
			if opened.opened != nil {
				opened.opened.leave()
			}
		}
	})
	burstStarted := time.Now()
	peak := server.peakResident(t, func() {
		var group sync.WaitGroup
		for index := range burst {
			group.Go(func() {
				opened := &burst[index]
				opened.opened, opened.err = server.tryOpenBrowser(opened.room)
				if errors.As(opened.err, &opened.closed) {
					opened.err = nil
				}
			})
		}
		group.Wait()
	})
	took := time.Since(burstStarted)

	refusals := budgetRefusalsByRoom(t, server)
	admitted, refused := map[string]int{}, map[string]int{}
	var admittedReserved int64
	for index, opened := range burst {
		what := fmt.Sprintf("browser %d, of %s", index, opened.shape)
		switch {
		case opened.err != nil:
			t.Errorf("%s: %v", what, opened.err)
		case opened.opened != nil:
			admitted[opened.shape]++
			admittedReserved += opened.reserved
			if err := opened.opened.ping(); err != nil {
				t.Errorf("%s was admitted and lost its room: %v", what, err)
			}
		default:
			refused[opened.shape]++
			requireClose(t, opened.closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, what)
			requireRoomAbsent(t, server, opened.room, what+"'s document")
			refusal, logged := refusals[opened.room]
			t.Logf("%s was refused at %d of %d bytes charged, needing %d against the %d computed for it", what, refusal.charged, refusal.limit, refusal.needs, opened.reserved)
			switch {
			case !logged:
				t.Errorf("%s was refused and the server logged no refusal of its document %s", what, opened.room)
			case refusal.limit != roomBudgetBytesForTest:
				t.Errorf("%s was refused against a budget of %d bytes, want %d", what, refusal.limit, roomBudgetBytesForTest)
			case refusal.needs == 0 && refusal.charged <= refusal.limit:
				t.Errorf("%s was refused at %d of %d bytes charged before its document was weighed, want a refusal only past the limit", what, refusal.charged, refusal.limit)
			case refusal.needs != 0 && refusal.needs != opened.reserved:
				t.Errorf("%s was refused needing %d bytes, want the %d its stored history and rendering are priced at", what, refusal.needs, opened.reserved)
			case refusal.needs != 0 && refusal.charged+refusal.needs <= refusal.limit:
				t.Errorf("%s was refused at %d of %d bytes charged needing %d, which fit: refused earlier than the budget's arithmetic allows", what, refusal.charged, refusal.limit, refusal.needs)
			}
		}
	}
	for _, shape := range []string{longEdited.name, agent.name, ordinary} {
		t.Logf("%s: %d admitted, %d refused", shape, admitted[shape], refused[shape])
	}
	dense := admitted[longEdited.name] + admitted[agent.name]
	t.Logf("%d browsers at once: %d admitted to struct-dense rooms and %d to rooms of prose, %d bytes reserved at admission against the %d-byte budget, in %s; peak resident memory %d MiB, against the pod's %d MiB",
		len(burst), dense, admitted[ordinary], admittedReserved, roomBudgetBytesForTest, took.Round(time.Second), peak>>20, podMemory>>20)
	logHeapGrants(t, "the burst", heapGrants(t, server.output.String()))
	if most := roomBudgetBytesForTest / min(longEdited.reserved, agent.reserved); dense == 0 || int64(dense) > most {
		t.Errorf("%d struct-dense rooms were admitted, want at least one and no more than the %d their structs' price fits in the budget", dense, most)
	}
	if peak >= podMemory {
		t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
	}
}

// The worst case the room budget and the two heap bounds leave the pod, run at once under the Go
// limit a 4 GiB pod gives the server and at the collector's default pace: the budget full, the cold
// reads' cache full, honest handoffs at F applying - each estimated past the apply-heap bound, so
// each holds the whole bound and applies alone among the applies - and beside them struct-dense
// documents loading and folding, each decode weighed past the copy-heap bound and holding all of
// it. An apply past its bound runs alone among applies but not among copies, so this is the sum
// the bounds allow: 2 GiB of rooms, 256 MiB of cache, a handoff estimated at 904 MB beside a copy
// at its whole 394 MB bound, 3,714,003,040 bytes by the estimates, inside the 3,865,470,566-byte Go
// limit. The server's peak resident memory, read from /proc, stays under the pod's 4 GiB through
// every step, and every honest handoff, load and fold completes.
//
// On one server: first the budget is filled by browsers that stay - two rooms of the long-edited
// document typed on until 4,096 structs short of the decode cap and padded to F by text nested
// under the margin (paddedToTheHeaviestFrame), whose browsers later hand back, then copies of the
// long-edited document at the decode cap - leaving room, by the budget's arithmetic computed here
// from the server's constants, for one more document an agent rewrote to the decode cap and not
// two. Then the cache is filled by cold reads of documents of the heaviest headings the element
// bound admits, one more than its budget holds. Then, all at once, three browsers of the rooms at F
// hand their whole state back with a few words typed, two browsers open two copies of the
// agent-rewritten document, of which the budget admits one and refuses the other with 4503 within
// its arithmetic, and four struct-dense documents no room holds are read cold.
//
// The server logs each grant of either bound that waited or held its whole bound (heapGrantLine),
// from which the drive reads that a handoff holding the whole apply bound and a copy holding the
// whole copy bound were held at the same moment, so the worst case did happen, and the longest wait
// either bound made. It logs those beside each step's peak and duration.
func TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory(t *testing.T) {
	const folds = 4
	building := time.Now()
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	stored := memory.storedStates(t, longEditedTypedTo(t, maxUpdateItemsForTest),
		paddedToTheHeaviestFrame(t, longEditedTypedTo(t, maxUpdateItemsForTest-4_096)))
	longEdited := memory.storedShape(t, "the long-edited document at the decode cap", stored[0])
	heaviest := memory.storedShape(t, "the long-edited document 4,096 structs short of the decode cap, padded to F", stored[1])
	agent := memory.storedShape(t, "the agent-rewritten document", memory.agentRewrittenDocument(t))
	requireAtTheDecodeCap(t, longEdited)
	requireAtTheDecodeCap(t, agent)
	if heaviest.stateBytes > heaviestHandoffBoundForTest || heaviest.stateBytes+64<<10 < heaviestHandoffBoundForTest {
		t.Fatalf("the padded document stores %d bytes of state, want within 64 KiB under F (%d)", heaviest.stateBytes, heaviestHandoffBoundForTest)
	}
	cached, cacheWeight := memory.readCacheFill(t)
	heavyRooms := memory.cloneDocuments(t, heaviest.id, 2)
	fill := (roomBudgetBytesForTest - 2*heaviest.reserved - agent.reserved) / longEdited.reserved
	free := roomBudgetBytesForTest - 2*heaviest.reserved - fill*longEdited.reserved
	if fill < 1 || free < agent.reserved || free >= 2*agent.reserved {
		t.Fatalf("two rooms at F and %d at the decode cap leave %d of the %d-byte budget, want room for one agent-rewritten document (%d bytes) and not two",
			fill, free, roomBudgetBytesForTest, agent.reserved)
	}
	filling := memory.cloneDocuments(t, longEdited.id, int(fill))
	lastRooms := memory.cloneDocuments(t, agent.id, 2)
	folded := append(memory.cloneDocuments(t, agent.id, folds/2), memory.cloneDocuments(t, longEdited.id, folds/2)...)
	worst := int64(roomBudgetBytesForTest) + documentReadBudgetForTest + heaviestHandoffEstimate + copyHeapBoundForTest
	t.Logf("the budget: two rooms at F reserve %d bytes each and %d at the decode cap %d each, which leaves %d, room for one agent-rewritten document (%d) and not two",
		heaviest.reserved, fill, longEdited.reserved, free, agent.reserved)
	t.Logf("the bounds: a handoff at F is estimated at %d bytes, past the %d-byte apply bound, so it holds all of it; a fold or load of a struct-dense state is weighed at %d bytes a byte, %d for the agent's %d bytes and %d for the long-edited document's %d, past the %d-byte copy bound, so it holds all of it",
		heaviestHandoffEstimate, applyHeapBoundForTest, copyHeapPerByteForTest, agent.stateBytes*copyHeapPerByteForTest, agent.stateBytes,
		longEdited.stateBytes*copyHeapPerByteForTest, longEdited.stateBytes, copyHeapBoundForTest)
	t.Logf("the worst case: the %d-byte budget, the %d-byte cache (%d documents of %d bytes each), the handoff's %d and the copy bound's %d come to %d bytes, against the %d-byte Go limit and the pod's %d; the documents took %s to build",
		roomBudgetBytesForTest, documentReadBudgetForTest, len(cached), cacheWeight, heaviestHandoffEstimate, copyHeapBoundForTest, worst,
		podGoMemoryLimit, podMemory, time.Since(building).Round(time.Second))

	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	var held []*browser
	t.Cleanup(func() {
		for _, opened := range held {
			opened.leave()
		}
	})
	open := func(t *testing.T, room string) *browser {
		t.Helper()
		opened := server.openBrowser(t, room)
		held = append(held, opened)
		return opened
	}

	step := time.Now()
	var handing []*browser
	filledPeak := server.peakResident(t, func() {
		handing = append(handing, open(t, heavyRooms[0]), open(t, heavyRooms[0]), open(t, heavyRooms[1]))
		for _, room := range filling {
			open(t, room)
		}
	})
	t.Logf("the budget is filled: two rooms at F held by %d browsers and %d rooms at the decode cap, in %s; peak resident memory %d MiB, %d MiB resident now",
		len(handing), len(filling), time.Since(step).Round(time.Second), filledPeak>>20, server.resident(t, "VmRSS")>>20)

	step = time.Now()
	before := server.resident(t, "VmRSS")
	cachePeak := server.peakResident(t, func() {
		for _, document := range cached {
			if text := server.text(t, document); text == "" {
				t.Fatalf("the cold read of document %s answered no text", document)
			}
		}
	})
	retained := server.resident(t, "VmRSS") - before
	t.Logf("the cache is filled by %d cold reads, in %s: %d MiB more resident than before them; peak resident memory %d MiB",
		len(cached), time.Since(step).Round(time.Second), retained>>20, cachePeak>>20)
	if retained < documentReadBudgetForTest/2 {
		t.Errorf("filling the cache left %d MiB more resident, want at least %d MiB, so the cache holds what the worst case counts", retained>>20, (documentReadBudgetForTest/2)>>20)
	}

	handoffs := make([][]byte, len(handing))
	for index, handingBack := range handing {
		handoffs[index] = typedHandoff(t, handingBack.state(t))
		if len(handoffs[index]) > heaviestHandoffBoundForTest || len(handoffs[index])+64<<10 < heaviestHandoffBoundForTest {
			t.Fatalf("handoff %d is %d bytes, want within 64 KiB under F (%d)", index, len(handoffs[index]), heaviestHandoffBoundForTest)
		}
	}
	type outcome struct {
		took    time.Duration
		applied bool
		opened  *browser
		closed  *gws.CloseError
		err     error
	}
	handed := make([]outcome, len(handing))
	loaded := make([]outcome, len(lastRooms))
	read := make([]outcome, len(folded))
	t.Cleanup(func() {
		for _, last := range loaded {
			if last.opened != nil {
				last.opened.leave()
			}
		}
	})
	mark := len(server.output.String())
	step = time.Now()
	worstPeak := server.peakResident(t, func() {
		var group sync.WaitGroup
		for index, handingBack := range handing {
			group.Go(func() {
				result := &handed[index]
				result.applied, result.closed, result.err = handingBack.send(handingBack.stateFrame(handoffs[index]))
				result.took = time.Since(step)
			})
		}
		for index, room := range lastRooms {
			group.Go(func() {
				result := &loaded[index]
				result.opened, result.err = server.tryOpenBrowser(room)
				if errors.As(result.err, &result.closed) {
					result.err = nil
				}
				result.took = time.Since(step)
			})
		}
		for index, room := range folded {
			group.Go(func() {
				result := &read[index]
				var answer response
				answer, result.err = server.trySend(http.MethodGet, "/api/v1/artifacts/"+room+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
				var text struct{ Markdown string }
				if result.err == nil && (answer.status != http.StatusOK || json.Unmarshal(answer.body, &text) != nil || text.Markdown == "") {
					result.err = fmt.Errorf("status %d body %.300s, want 200 and the document's text", answer.status, answer.body)
				}
				result.took = time.Since(step)
			})
		}
		group.Wait()
		// The handoffs' typed words are settled once the settle delay after them has passed, a
		// render and a version of each room at F, which the peak counts too.
		for _, room := range heavyRooms {
			memory.waitForSettled(t, room)
		}
	})
	took := time.Since(step)
	window := server.output.String()[mark:]

	for index, result := range handed {
		t.Logf("handoff %d of %d bytes: applied %v, closed %v, answered in %s", index, len(handoffs[index]), result.applied, result.closed, result.took.Round(time.Millisecond))
		if result.err != nil || !result.applied || result.closed != nil {
			t.Errorf("handoff %d of %d bytes at F: applied %v, closed %v, %v; want it applied and the socket open", index, len(handoffs[index]), result.applied, result.closed, result.err)
		}
	}
	refusals := budgetRefusalsByRoom(t, server)
	admittedLast := 0
	for index, result := range loaded {
		switch {
		case result.err != nil:
			t.Errorf("the browser of agent-rewritten document %d: %v", index, result.err)
		case result.opened != nil:
			admittedLast++
			t.Logf("the browser of agent-rewritten document %d was admitted and received its document in %s", index, result.took.Round(time.Millisecond))
			if err := result.opened.ping(); err != nil {
				t.Errorf("the admitted browser of agent-rewritten document %d lost its room: %v", index, err)
			}
		default:
			requireClose(t, result.closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, fmt.Sprintf("the browser of agent-rewritten document %d", index))
			requireRoomAbsent(t, server, lastRooms[index], "a refused agent-rewritten document")
			refusal, logged := refusals[lastRooms[index]]
			t.Logf("the browser of agent-rewritten document %d was refused in %s at %d of %d bytes charged, needing %d", index, result.took.Round(time.Millisecond), refusal.charged, refusal.limit, refusal.needs)
			if !logged || refusal.needs != agent.reserved || refusal.charged+refusal.needs <= refusal.limit || refusal.charged > refusal.limit {
				t.Errorf("the browser of agent-rewritten document %d was refused with %+v (logged %v), want a refusal at no more than the %d-byte limit needing the %d its structs are priced at, past the limit together",
					index, refusal, logged, roomBudgetBytesForTest, agent.reserved)
			}
		}
	}
	if admittedLast != 1 {
		t.Errorf("%d of the two agent-rewritten documents were admitted, want the one the budget's arithmetic has room for", admittedLast)
	}
	for index, result := range read {
		t.Logf("cold read %d answered in %s (%v)", index, result.took.Round(time.Millisecond), result.err)
		if result.err != nil {
			t.Errorf("cold read %d of a struct-dense document: %v", index, result.err)
		}
	}

	grants := heapGrants(t, window)
	var applyAlone, copyAlone []heapGrant
	for _, grant := range grants {
		switch {
		case grant.bound == "apply" && grant.size != applyHeapBoundForTest, grant.bound == "copy" && grant.size != copyHeapBoundForTest:
			t.Errorf("the server logged a grant of the %s bound of %d bytes, want the %s bound this package mirrors", grant.bound, grant.size, grant.bound)
		case grant.alone && grant.bound == "apply":
			applyAlone = append(applyAlone, grant)
		case grant.alone:
			copyAlone = append(copyAlone, grant)
		}
	}
	logHeapGrants(t, "the heaviest applies beside full copy bounds", grants)
	logHeapGrants(t, "the whole run", heapGrants(t, server.output.String()))
	overlap, pairs := longestOverlap(applyAlone, copyAlone)
	t.Logf("the heaviest applies beside full copy bounds, in %s: %d handoffs held the whole apply bound and %d decodes the whole copy bound; %d pairs of them were held at once, the longest for %s; peak resident memory %d MiB, against the pod's %d MiB",
		took.Round(time.Second), len(applyAlone), len(copyAlone), pairs, overlap.Round(time.Millisecond), worstPeak>>20, podMemory>>20)
	if len(applyAlone) < len(handing) {
		t.Errorf("%d grants held the whole apply bound, want one for each of the %d handoffs at F", len(applyAlone), len(handing))
	}
	if len(copyAlone) < folds+1 {
		t.Errorf("%d grants held the whole copy bound, want one for each of the %d cold reads and the load", len(copyAlone), folds)
	}
	if pairs == 0 {
		t.Error("no handoff held the whole apply bound while a decode held the whole copy bound: the worst case did not happen")
	}
	t.Logf("peak resident memory by step: the budget filled %d MiB, the cache filled %d MiB, the heaviest applies beside full copy bounds %d MiB", filledPeak>>20, cachePeak>>20, worstPeak>>20)
	for _, peak := range []int64{filledPeak, cachePeak, worstPeak} {
		if peak >= podMemory {
			t.Errorf("the server's resident memory peaked at %d MiB, want under the pod's %d MiB", peak>>20, podMemory>>20)
		}
	}
}

// requireBusyAndUnchanged sends update to room from a browser of its own and fails the test unless
// the room refuses it with 4503 and DOC_BUSY, and the document is as it was before, read through
// the server's text and through the store.
func requireBusyAndUnchanged(t *testing.T, memory *memoryHarness, server *memoryServer, room string, update []byte, what string) {
	t.Helper()
	memory.waitForSettled(t, room)
	text, stored := server.text(t, room), memory.storedText(t, room)
	editing := server.openBrowser(t, room)
	applied, closed, err := editing.send(editing.updateFrame(update))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s on the full server: applied %v, closed %v", what, applied, closed)
	requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	memory.waitForSettled(t, room)
	if after := server.text(t, room); after != text {
		t.Errorf("%s was refused, and the document then read %d bytes, want the %d it held", what, len(after), len(text))
	}
	if after := memory.storedText(t, room); after != stored {
		t.Errorf("%s was refused, and the stored document then rendered %d bytes, want the %d it stored", what, len(after), len(stored))
	}
}

// headingsARound is how many one-letter headings a round of growth appends (growthRounds), each
// four elements in five bytes of rendering, so a round grows what a room is charged many times
// faster than its bytes.
const headingsARound = 2_048

// growthHeading is the heading a round of growth appends, as its rendering spells it.
const growthHeading = "# g\n"

// growthRound is one update that appends headingsARound headings to a document, and the size of
// the rendering it leaves.
type growthRound struct {
	update []byte
	size   pmdoc.DocumentSize
}

// growthRounds is the rounds of growth one browser makes from state, each on the last, for as long
// as more says the rendering the last leaves wants another. A round sent to copies of state that
// took the rounds before it applies to each alike. Every round stays far under the document bound
// in its rendering and in its update, so what refuses one can only be the room budget.
func growthRounds(t *testing.T, state []byte, more func(pmdoc.DocumentSize) bool) []growthRound {
	t.Helper()
	grower := newBrowserDocument(t, state)
	var rounds []growthRound
	for more(grower.size(t)) {
		added, err := pmdoc.Parse(strings.Repeat(growthHeading, headingsARound))
		if err != nil {
			t.Fatal(err)
		}
		update := grower.edit(t, func(tree *pmdoc.Node) {
			tree.Children = append(tree.Children, added.Children...)
			pmdoc.EnsureBlockIDs(tree)
		})
		size := grower.size(t)
		if size.Bytes+len(update) >= pmdoc.MaxDocumentBytes/2 {
			t.Fatalf("round %d leaves %d bytes of rendering and sends %d, want both far under the %d-byte bound", len(rounds)+1, size.Bytes, len(update), pmdoc.MaxDocumentBytes)
		}
		rounds = append(rounds, growthRound{update: update, size: size})
	}
	if len(rounds) == 0 {
		t.Fatal("no round of growth was wanted")
	}
	return rounds
}

// smallDocuments is count copies of one stored document of a heading and a sentence
// (cloneDocuments), and its stored state, for a drive to grow from small.
func (h *memoryHarness) smallDocuments(t *testing.T, count int) ([]string, []byte) {
	t.Helper()
	setup := h.start(t)
	source := setup.upload(t, "json", h.issue, "# Growth\n\nA small document its browser grows.\n")
	if source.status != http.StatusCreated {
		t.Fatalf("upload the small document: status %d body %.300s, want 201", source.status, source.body)
	}
	h.waitForSettlement(t, source)
	setup.stop(t)
	return h.cloneDocuments(t, source.artifactID, count), h.storedState(t, source.artifactID)
}

// mebibyteFleet is copies of one stored document of a mebibyte of prose (cloneDocuments), its
// stored state for browsers to edit, its stored version's count of elements, and what the room
// budget reserves to admit a browser to one: that count at the server's price of an element, at
// both copies (storedRoomReservation, roomCopiesForTest). copies is how many a drive needs, given
// that reservation.
type mebibyteFleet struct {
	copies   []string
	state    []byte
	elements int
	reserved int64
}

func (h *memoryHarness) mebibyteFleet(t *testing.T, copies func(reserved int64) int) mebibyteFleet {
	t.Helper()
	setup := h.start(t)
	source := setup.upload(t, "json", h.issue, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	if source.status != http.StatusCreated {
		t.Fatalf("upload a mebibyte of prose: status %d body %.300s, want 201", source.status, source.body)
	}
	h.waitForSettlement(t, source)
	setup.stop(t)
	var stored string
	if err := h.database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`, source.artifactID,
	).Scan(&stored); err != nil {
		t.Fatalf("read the stored version: %v", err)
	}
	size := pmdoc.MeasureDocument(stored)
	if !size.Counted {
		t.Fatalf("the stored version measures %+v, want it counted", size)
	}
	reserved := roomCopiesForTest * int64(size.Elements) * bytesOfHeapAnElementForTest
	return mebibyteFleet{
		copies:   h.cloneDocuments(t, source.artifactID, copies(reserved)),
		state:    h.storedState(t, source.artifactID),
		elements: size.Elements,
		reserved: reserved,
	}
}

// cloneDocuments copies the stored document sourceID, which no server holds open, into count new
// documents of the same issue, row for row: its artifact, its stored updates, its checkpoint and its
// versions. Each copy holds the same Yjs history, client ids and clocks included, so an update a
// browser encodes once against the source applies to every copy alike, and each copy's admission
// counts its own stored version as an upload's would.
func (h *memoryHarness) cloneDocuments(t *testing.T, sourceID string, count int) []string {
	t.Helper()
	ctx := context.Background()
	copies := make([]string, count)
	for index := range copies {
		tx, err := h.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin copy %d: %v", index, err)
		}
		slug := fmt.Sprintf("copy-%d-%03d", time.Now().UnixNano(), index)
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select issue_key, project_key, $2, $2 || '.md', kind, false, created_by from artifacts where id = $1
			returning id::text
		`, sourceID, slug).Scan(&copies[index]); err != nil {
			t.Fatalf("copy artifact %d: %v", index, err)
		}
		for _, statement := range []string{
			`insert into doc_updates (artifact_id, version, update, content_changed)
				select $1, version, update, content_changed from doc_updates where artifact_id = $2`,
			`insert into doc_checkpoints (artifact_id, ceiling, state)
				select $1, ceiling, state from doc_checkpoints where artifact_id = $2`,
			`insert into artifact_versions (artifact_id, number, markdown, content, mime, size, sha256, authors, named, summary, doc_update_version)
				select $1, number, markdown, content, mime, size, sha256, authors, named, summary, doc_update_version
				from artifact_versions where artifact_id = $2`,
		} {
			if _, err := tx.Exec(ctx, statement, copies[index], sourceID); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("copy %d: %v", index, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit copy %d: %v", index, err)
		}
	}
	return copies
}

// artifactName names a document for a log line: its name, and whether it is its issue's primary
// document, as the store holds them.
func (h *memoryHarness) artifactName(t *testing.T, artifactID string) string {
	t.Helper()
	var name string
	var primary bool
	if err := h.database.Pool.QueryRow(context.Background(),
		`select name, is_primary from artifacts where id = $1`, artifactID,
	).Scan(&name, &primary); err != nil {
		t.Fatalf("read document %s's name: %v", artifactID, err)
	}
	if primary {
		return fmt.Sprintf("%q, its issue's primary document", name)
	}
	return fmt.Sprintf("%q", name)
}

// storedState is the document's whole stored Yjs state.
func (h *memoryHarness) storedState(t *testing.T, artifactID string) []byte {
	t.Helper()
	loaded, err := docs.NewPgVersioned(h.database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	return loaded.Update
}

// storedText is the document's rendering as the store holds it: its whole stored Yjs state, read
// and rendered by the test as the server renders a room's (pmdoc.Render). Nothing of it passes
// through the server, so it is a second source beside the server's text route.
func (h *memoryHarness) storedText(t *testing.T, artifactID string) string {
	t.Helper()
	markdown, err := pmdoc.Render(newBrowserDocument(t, h.storedState(t, artifactID)).tree(t))
	if err != nil {
		t.Fatalf("render the stored document: %v", err)
	}
	return markdown
}

// waitForStoredText waits until the document's stored rendering satisfies holds: an update a room
// applied reaches the store when the room's persistence appends it, a moment after the room
// answers the browser that sent it.
func (h *memoryHarness) waitForStoredText(t *testing.T, artifactID string, holds func(markdown string) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		h.waitForSettled(t, artifactID)
		markdown := h.storedText(t, artifactID)
		if holds(markdown) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s's stored rendering, %d bytes, never held what was wanted of it", artifactID, len(markdown))
		}
		time.Sleep(time.Second)
	}
}

// waitForStored waits until the document's stored Yjs state satisfies holds, as waitForStoredText
// does for its rendering.
func (h *memoryHarness) waitForStored(t *testing.T, artifactID, what string, holds func(state []byte) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !holds(h.storedState(t, artifactID)) {
		if time.Now().After(deadline) {
			t.Fatalf("document %s never stored %s", artifactID, what)
		}
		time.Sleep(time.Second)
	}
}

// roomLive asks the server whether the document is live in it, a question the room budget plays no
// part in answering: the test-only route reads whether this server holds the document's room and
// loads, rebuilds and charges nothing to answer (docs.Service.RoomLiveForTest).
func (p *memoryServer) roomLive(t *testing.T, artifactID string) bool {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/_test/live", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var read struct {
		Live *bool `json:"live"`
	}
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &read) != nil || read.Live == nil {
		t.Fatalf("asking whether document %s is live: %d %.300s, want 200 and {live}", artifactID, answer.status, answer.body)
	}
	return *read.Live
}

// requireRoomAbsent fails the test unless the server says the document is not live (roomLive).
func requireRoomAbsent(t *testing.T, server *memoryServer, artifactID, what string) {
	t.Helper()
	if server.roomLive(t, artifactID) {
		t.Fatalf("%s, %s, is live in the server, want no room for it", what, artifactID)
	}
}

// proseMarkdown is the shape TestAResidentRoomsHeapPerByteOfItsMarkdown weighs rooms in
// (internal/dispatch/docs/room_heap_test.go:399-425), whole sections to at most size bytes: a
// heading, a paragraph of marks and plain words, a list, a table and a code block. Each paragraph
// ends in about 1,400 bytes of plain words (paddingTexts), which a drive can lengthen or shorten
// without touching a mark.
func proseMarkdown(t *testing.T, size int) string {
	t.Helper()
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
		if markdown.Len()+next.Len() > size {
			break
		}
		markdown.WriteString(next.String())
	}
	if measured := pmdoc.MeasureDocument(markdown.String()); !measured.Counted || measured.Bytes > pmdoc.MaxDocumentBytes || measured.Elements > pmdoc.MaxDocumentElements {
		t.Fatalf("%d bytes of prose measure %+v, past a bound an upload obeys", size, measured)
	}
	return markdown.String()
}

// paddingTexts are the text nodes of tree that end in proseMarkdown's plain words, in document
// order.
func paddingTexts(t *testing.T, tree *pmdoc.Node) []*pmdoc.Node {
	t.Helper()
	var texts []*pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" && len(node.Marks) == 0 && strings.Contains(node.Text, "word word") {
			texts = append(texts, node)
		}
		return true
	})
	if len(texts) == 0 {
		t.Fatal("the document holds no paragraph of plain words")
	}
	return texts
}

// shortenPadding cuts bytes, whole words of five bytes, from text's plain words.
func shortenPadding(t *testing.T, text *pmdoc.Node, bytes int) {
	t.Helper()
	at := strings.Index(text.Text, "word word")
	if bytes%5 != 0 || at < 0 || at+bytes > len(text.Text)-4 {
		t.Fatalf("cannot cut %d bytes of whole words from %d bytes of text", bytes, len(text.Text))
	}
	text.Text = text.Text[:at] + text.Text[at+bytes:]
}

// renderedSize is tree's rendering measured as the server measures a room's.
func renderedSize(t *testing.T, tree *pmdoc.Node) pmdoc.DocumentSize {
	t.Helper()
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return pmdoc.MeasureDocument(markdown)
}

// fillMargin adds suggestions of 255,000 bytes to the document, each anchored on a heading of text,
// until the margin's bound of 1 MiB refuses one, and returns how many it took.
func fillMargin(t *testing.T, server *memoryServer, issue, artifactID, text string) int {
	t.Helper()
	replacement := strings.Repeat("s", 255_000)
	for taken := 0; ; taken++ {
		body, err := json.Marshal(map[string]any{
			"body":       "a suggestion",
			"anchor":     map[string]any{"artifact": artifactID, "quote": text, "occurrence": taken},
			"suggestion": map[string]string{"replace_with": replacement},
		})
		if err != nil {
			t.Fatalf("encode the suggestion: %v", err)
		}
		answer := server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
		if answer.status == http.StatusCreated && taken < 8 {
			continue
		}
		if answer.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(answer.body), "a document's margin holds at most 1 MiB") {
			t.Fatalf("suggestion %d answered %d %.300s, want 201 until the margin's bound refuses one", taken+1, answer.status, answer.body)
		}
		return taken
	}
}

// floorFrame is a frame of exactly size bytes carrying an update that appends to the document's
// first text, as one browser's paste would arrive.
func floorFrame(t *testing.T, sending *browser, state []byte, size int) []byte {
	t.Helper()
	build := func(appended int) []byte {
		return sending.updateFrame(browserEdit(t, state, func(tree *pmdoc.Node) {
			tree.Children[0].Children[0].Text += strings.Repeat("x", appended)
		}))
	}
	appended := size - 4096
	for range 4 {
		frame := build(appended)
		if len(frame) == size {
			return frame
		}
		appended += size - len(frame)
	}
	t.Fatalf("no paste makes a frame of exactly %d bytes", size)
	return nil
}

// requireApplied sends update and fails the test unless the room applies it.
func requireApplied(t *testing.T, sending *browser, update []byte, what string) {
	t.Helper()
	applied, closed, err := sending.send(sending.updateFrame(update))
	if err != nil || !applied {
		t.Fatalf("%s was not applied: closed %v, %v", what, closed, err)
	}
}

// requireRefused fails the test unless the room refused an update and closed its socket with code
// and reason.
func requireRefused(t *testing.T, applied bool, closed *gws.CloseError, code int, reason string) {
	t.Helper()
	if applied {
		t.Fatalf("the update was applied, want it refused with %d %s", code, reason)
	}
	requireClose(t, closed, code, reason, "the refused update's socket")
}

// requireClose fails the test unless closed is a close frame of code and reason, read off the
// frame itself.
func requireClose(t *testing.T, closed *gws.CloseError, code int, reason, what string) {
	t.Helper()
	if closed == nil || closed.Code != code || closed.Text != reason {
		t.Fatalf("%s was closed with %v, want %d %s", what, closed, code, reason)
	}
}

// requireInsertsNothing fails the test unless update only deletes, which is what the update hook
// admits from a room at the creep past the bound or on a full server.
func requireInsertsNothing(t *testing.T, update []byte) {
	t.Helper()
	ids, err := crdt.ContentIDsFromUpdateV1(update)
	if err != nil {
		t.Fatalf("decode the update: %v", err)
	}
	if clients := ids.Inserts.Clients(); len(clients) != 0 {
		t.Fatalf("the shortening update inserts content of %d clients, want it to only delete", len(clients))
	}
}

// requirePodGoMemoryLimit fails the test unless the server took the Go limit a 4 GiB pod gives it.
func requirePodGoMemoryLimit(t *testing.T, server *memoryServer) {
	t.Helper()
	if want := "go_memory_limit_bytes=" + strconv.Itoa(podGoMemoryLimit); !strings.Contains(server.output.String(), want) {
		t.Fatalf("the server's boot did not log %s:\n%s", want, server.output.tail())
	}
}

// budgetRefusalLine is the reason the room budget gives a refused browser, as the server logs it
// (errRoomBudgetFull, internal/dispatch/docs/room_budget.go:637-674).
var budgetRefusalLine = regexp.MustCompile(`the open documents fill their memory budget: (\d+) of (\d+) bytes are charged(?:, and this document needs (\d+))?`)

// budgetRefusal is one refusal the server logged: what its rooms were charged, its budget, and
// what the refused document needed, or zero where the budget was past its limit before it weighed
// the document.
type budgetRefusal struct {
	charged, limit, needs int64
}

// requireBudgetRefusals is every refusal the server logged, failing the test unless there is one,
// and unless each names this package's mirror of the budget and, where it weighed the document,
// reserved for it what the drive computed.
func requireBudgetRefusals(t *testing.T, server *memoryServer, reserved int64) []budgetRefusal {
	t.Helper()
	var refusals []budgetRefusal
	for _, match := range budgetRefusalLine.FindAllStringSubmatch(server.output.String(), -1) {
		var refusal budgetRefusal
		for index, field := range []*int64{&refusal.charged, &refusal.limit, &refusal.needs} {
			if match[index+1] != "" {
				value, err := strconv.ParseInt(match[index+1], 10, 64)
				if err != nil {
					t.Fatalf("parse %q: %v", match[0], err)
				}
				*field = value
			}
		}
		if refusal.limit != roomBudgetBytesForTest {
			t.Errorf("the server refused against a budget of %d bytes, want %d", refusal.limit, roomBudgetBytesForTest)
		}
		if refusal.needs != 0 && refusal.needs != reserved {
			t.Errorf("the server reserved %d bytes for a document of %d bytes at %d bytes an element, want %d", refusal.needs, pmdoc.MaxDocumentBytes, bytesOfHeapAnElementForTest, reserved)
		}
		refusals = append(refusals, refusal)
	}
	if len(refusals) == 0 {
		t.Fatalf("the server logged no refusal of the room budget:\n%s", server.output.tail())
	}
	return refusals
}

// budgetCloseLine is the line the room budget's sweep logs for each unused room it closes, in the
// budget's own figures (sweepRooms, internal/dispatch/docs/room_budget.go:1462-1495).
var budgetCloseLine = regexp.MustCompile(`msg="dispatch: the room budget closed an unused document" room=(\S+) charged_before=(\d+) charged=(\d+) limit=(\d+) making_room_for=(\d+)`)

// budgetClose is one close the server logged: the room, what the budget charged before and after
// the close, its limit, and the most an admission it refused needed since its last sweep, which
// the sweep was making room for (roomBudget.wanted).
type budgetClose struct {
	room                                         string
	chargedBefore, charged, limit, makingRoomFor int64
}

// budgetCloses is every close of an unused room the server has logged, in the order it logged them.
func budgetCloses(t *testing.T, server *memoryServer) []budgetClose {
	t.Helper()
	var closes []budgetClose
	for _, match := range budgetCloseLine.FindAllStringSubmatch(server.output.String(), -1) {
		closed := budgetClose{room: match[1]}
		for index, field := range []*int64{&closed.chargedBefore, &closed.charged, &closed.limit, &closed.makingRoomFor} {
			value, err := strconv.ParseInt(match[index+2], 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
			*field = value
		}
		closes = append(closes, closed)
	}
	return closes
}

// storedShape is a stored document a struct-dense drive copies (cloneDocuments), and what an
// admission to a copy of it reserves, computed here as the server computes it
// (storedRoomReservation, internal/dispatch/docs/room_budget.go:1231-1300): the structs of its
// stored history, each stored update walked as a room's own state is (docs.StateStructsForTest),
// and its latest version's markdown, counted and measured in bytes, priced at the heaviest of a
// struct's, an element's and a rendered byte's price (copyHeap, :592-602), at both copies.
type storedShape struct {
	name       string
	id         string
	structs    int64
	stateBytes int
	rendered   int
	elements   int
	reserved   int64
}

func (h *memoryHarness) storedShape(t *testing.T, name, artifactID string) storedShape {
	t.Helper()
	ctx := context.Background()
	shape := storedShape{name: name, id: artifactID}
	rows, err := h.database.Pool.Query(ctx, `select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored history of %s: %v", name, err)
	}
	updates := 0
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("read an update of %s: %v", name, err)
		}
		structs, err := docs.StateStructsForTest(update)
		if err != nil {
			t.Fatalf("walk an update of %s: %v", name, err)
		}
		shape.structs += structs
		shape.stateBytes += len(update)
		updates++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored history of %s: %v", name, err)
	}
	var markdown string
	if err := h.database.Pool.QueryRow(ctx,
		`select coalesce(markdown, '') from artifact_versions where artifact_id = $1 order by number desc limit 1`, artifactID,
	).Scan(&markdown); err != nil {
		t.Fatalf("read the latest version of %s: %v", name, err)
	}
	size := pmdoc.MeasureDocument(markdown)
	if !size.Counted {
		t.Fatalf("the latest version of %s measures %+v, want it counted", name, size)
	}
	shape.rendered, shape.elements = len(markdown), size.Elements
	byElements := int64(size.Elements) * bytesOfHeapAnElementForTest
	shape.reserved = roomCopiesForTest * max(byElements, int64(len(markdown))*bytesOfHeapARenderedByteForTest, shape.structs*bytesOfHeapAStructHeldForTest)
	t.Logf("%s: %d bytes of stored state in %d updates holding %d structs, its latest version %d bytes making %d elements; an admission reserves %d bytes at both copies, where its elements alone would reserve %d",
		name, shape.stateBytes, updates, shape.structs, shape.rendered, shape.elements, shape.reserved, roomCopiesForTest*byElements)
	return shape
}

// requireAtTheDecodeCap fails the test unless shape's stored history holds the decode cap's
// structs, or within 4,096 of them.
func requireAtTheDecodeCap(t *testing.T, shape storedShape) {
	t.Helper()
	if shape.structs < maxUpdateItemsForTest-4_096 || shape.structs > maxUpdateItemsForTest {
		t.Fatalf("%s holds %d structs, want within 4,096 under the %d-struct decode cap", shape.name, shape.structs, maxUpdateItemsForTest)
	}
}

// storedStates stores each of states as the whole history of a new document of the harness's
// issue, as a rebuild stores its seed (docs.PgVersioned.RebuildTx): a document of a line of prose is
// uploaded and settled, and once no server holds it its history is replaced by the state. Its
// latest version stays the uploaded prose, which no read of the drives takes as the document's
// text: a cold read folds the stored history (coldRead), and a room loads it.
func (h *memoryHarness) storedStates(t *testing.T, states ...[]byte) []string {
	t.Helper()
	setup := h.start(t)
	ids := make([]string, len(states))
	for index := range states {
		seed := setup.upload(t, "json", h.issue, fmt.Sprintf("# Seed %d\n\nA document whose history the drive replaces.\n", index))
		if seed.status != http.StatusCreated {
			t.Fatalf("upload seed %d: status %d body %.300s, want 201", index, seed.status, seed.body)
		}
		h.waitForSettlement(t, seed)
		ids[index] = seed.artifactID
	}
	setup.stop(t)
	ctx := context.Background()
	versioned := docs.NewPgVersioned(h.database)
	for index, state := range states {
		tx, err := h.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin storing state %d: %v", index, err)
		}
		if _, err := versioned.RebuildTx(ctx, tx, ids[index], state); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("store state %d, %d bytes: %v", index, len(state), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit state %d: %v", index, err)
		}
	}
	return ids
}

// agentRewrittenDocument is a document an agent rewrote whole through the API, as
// agentRewrittenState rewrites one (internal/dispatch/docs/struct_cap_measure_test.go:419-453):
// each write a new version of the same document of 16,384 headings of fresh text at both halves of
// the byte and element bounds, until the decode cap refuses a write. Its history then stands at
// the cap, about a million structs rendering a mebibyte.
func (h *memoryHarness) agentRewrittenDocument(t *testing.T) string {
	t.Helper()
	const headingBytes = 64
	headings := pmdoc.MaxDocumentBytes / headingBytes
	setup := h.start(t)
	name := fmt.Sprintf("agent-%d.md", time.Now().UnixNano())
	started := time.Now()
	artifactID := ""
	for write := 0; ; write++ {
		var markdown strings.Builder
		for heading := range headings {
			fmt.Fprintf(&markdown, "# %0*d\n\n", headingBytes-4, write*headings+heading)
		}
		answer, err := setup.tryUploadNamed("json", h.issue, name, markdown.String())
		if err != nil {
			t.Fatal(err)
		}
		if answer.status == http.StatusCreated {
			artifactID = answer.artifactID
			continue
		}
		if write < 20 || answer.status/100 != 4 {
			t.Fatalf("the agent's write %d answered %d %.300s, want at least twenty written and then one refused", write+1, answer.status, answer.body)
		}
		t.Logf("the agent's write %d was refused with %d %.200s, %s after the first", write+1, answer.status, answer.body, time.Since(started).Round(time.Second))
		break
	}
	h.waitForSettled(t, artifactID)
	setup.stop(t)
	return artifactID
}

// longEditedTypedTo is the long-edited fixture typed on as its namesake in the docs package types
// it (internal/dispatch/docs/rebuild_near_cap_test.go:39-93): its first paragraph typed into at its
// head a character at a time, each its own item, until its state walks to structs structs, and
// those characters then deleted, so it renders the fixture's 5 KiB again while its state holds
// every keystroke. One character is typed at the head first, in a transaction of its own, and every
// later one just after it, where no deleted item lies.
func longEditedTypedTo(t *testing.T, structs int64) []byte {
	t.Helper()
	file, err := os.Open(longEditFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := io.ReadAll(unzipped)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, fixture, nil); err != nil {
		t.Fatal(err)
	}
	text := firstXMLText(doc.GetXmlFragment(proofFragment))
	if text == nil {
		t.Fatal("the long-edited document holds no text")
	}
	counted := func() int64 {
		count, err := docs.StateStructsForTest(crdt.EncodeStateAsUpdateV1(doc, nil))
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	typed := 1
	for count := counted(); count < structs; count = counted() {
		more := int(structs - count)
		doc.Transact(func(txn *crdt.Transaction) {
			for range more {
				text.Insert(txn, 1, "x", nil)
			}
		})
		typed += more
	}
	doc.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, typed) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// paddedToTheHeaviestFrame is state with a text nested under its margin, pasted in one transaction
// until the whole state stands 16 KiB short of F, as handoffAtTheHeaviestFrame pads its room
// (internal/dispatch/docs/peer_update_test.go:2083-2109): what grows the frame and nothing else
// the hook weighs.
func paddedToTheHeaviestFrame(t *testing.T, state []byte) []byte {
	t.Helper()
	const paste, nestedKey = 256 << 10, "nested"
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	margin := doc.GetMap("marks")
	doc.Transact(func(txn *crdt.Transaction) { margin.Set(txn, nestedKey, crdt.NewTextPrelim()) })
	value, _ := margin.Get(nestedKey)
	text, isText := value.(*crdt.YText)
	if !isText {
		t.Fatalf("the margin holds %T under %q, want the nested text", value, nestedKey)
	}
	left := heaviestHandoffBoundForTest - len(crdt.EncodeStateAsUpdateV1(doc, nil)) - 16<<10
	if left < 0 {
		t.Fatalf("the state is %d bytes past F less 16 KiB, want it under", -left)
	}
	chunk := strings.Repeat("p", paste)
	doc.Transact(func(txn *crdt.Transaction) {
		for at := 0; at < left; at += paste {
			text.Insert(txn, at, chunk[:min(paste, left-at)], nil)
		}
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// typedHandoff is the whole-state handoff of a browser that received state from its room and has
// typed a few words at the head of its first text since, as handoffAtTheHeaviestFrame's browser
// does (internal/dispatch/docs/peer_update_test.go:2110-2112).
func typedHandoff(t *testing.T, state []byte) []byte {
	t.Helper()
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	text := firstXMLText(doc.GetXmlFragment(proofFragment))
	if text == nil {
		t.Fatal("the room's state holds no text")
	}
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "an honest edit ", nil) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// firstXMLText is the first text under node in document order, however deep, or nil.
func firstXMLText(node any) *crdt.YXmlText {
	switch node := node.(type) {
	case *crdt.YXmlText:
		return node
	case *crdt.YXmlFragment:
		for _, child := range node.Children() {
			if text := firstXMLText(child); text != nil {
				return text
			}
		}
	case *crdt.YXmlElement:
		for _, child := range node.Children() {
			if text := firstXMLText(child); text != nil {
				return text
			}
		}
	}
	return nil
}

// readCacheFill is documents whose cold reads fill the cold reads' cache (documentReadBudget), and
// the weight each read takes of it: copies of one stored document of the heaviest headings the
// element bound admits, as TestColdReadCacheEvictionPlateaus fills the cache, one more than its
// budget holds at that weight.
func (h *memoryHarness) readCacheFill(t *testing.T) ([]string, int64) {
	t.Helper()
	markdown := heaviestAdmitted(t, func(units int) string { return strings.Repeat("# a\n", units) })
	weight, err := docs.DocumentReadWeightForTest(markdown)
	if err != nil {
		t.Fatalf("weigh the cache-fill document: %v", err)
	}
	if weight > documentReadBudgetForTest/8 {
		t.Fatalf("the cache-fill document weighs %d bytes, past the eighth of the %d-byte budget an entry may weigh", weight, documentReadBudgetForTest)
	}
	setup := h.start(t)
	source := setup.upload(t, "json", h.issue, markdown)
	if source.status != http.StatusCreated {
		t.Fatalf("upload the cache-fill document: status %d body %.300s, want 201", source.status, source.body)
	}
	h.waitForSettlement(t, source)
	setup.stop(t)
	return h.cloneDocuments(t, source.artifactID, int(documentReadBudgetForTest/weight)+1), weight
}

// roomRefusalLine is the line the server logs for each browser the room budget refuses: its
// document, and the budget's reason (refuseOverRoomBudget, internal/dispatch/docs/websocket.go:245-248;
// budgetRefusalLine).
var roomRefusalLine = regexp.MustCompile(`msg="dispatch: refuse a document socket; the open documents fill their memory budget" room=(\S+) error="the open documents fill their memory budget: (\d+) of (\d+) bytes are charged(?:, and this document needs (\d+))?"`)

// budgetRefusalsByRoom is the last refusal the server logged for each document it refused a
// browser of.
func budgetRefusalsByRoom(t *testing.T, server *memoryServer) map[string]budgetRefusal {
	t.Helper()
	refusals := map[string]budgetRefusal{}
	for _, match := range roomRefusalLine.FindAllStringSubmatch(server.output.String(), -1) {
		var refusal budgetRefusal
		for index, field := range []*int64{&refusal.charged, &refusal.limit, &refusal.needs} {
			if match[index+2] == "" {
				continue
			}
			value, err := strconv.ParseInt(match[index+2], 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
			*field = value
		}
		refusals[match[1]] = refusal
	}
	return refusals
}

// heapGrantLine is the line the server logs as it gives back a grant of the apply-heap or the
// copy-heap bound that waited a tenth of a second or more for its weight, or held its whole bound
// (logHeapGrant, internal/dispatch/docs/copy_heap.go): the bound, the weight, the bound's size,
// whether the grant held all of it, how long it waited and how long it held it, the line's time
// being when it gave it back.
var heapGrantLine = regexp.MustCompile(`time=(\S+) level=INFO msg="dispatch: a heap bound's grant was given back" bound=(apply|copy) weight=(\d+) bound_bytes=(\d+) alone=(true|false) waited=(\S+) held=(\S+)`)

// heapGrant is one grant the server logged (heapGrantLine), held from from to to.
type heapGrant struct {
	bound        string
	weight, size int64
	alone        bool
	waited, held time.Duration
	from, to     time.Time
}

// heapGrants is every grant logged in output, in the order the server gave them back.
func heapGrants(t *testing.T, output string) []heapGrant {
	t.Helper()
	var grants []heapGrant
	for _, match := range heapGrantLine.FindAllStringSubmatch(output, -1) {
		grant := heapGrant{bound: match[2], alone: match[5] == "true"}
		var err error
		if grant.to, err = time.Parse(time.RFC3339Nano, match[1]); err != nil {
			t.Fatalf("parse %q: %v", match[0], err)
		}
		for index, field := range []*int64{&grant.weight, &grant.size} {
			if *field, err = strconv.ParseInt(match[index+3], 10, 64); err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
		}
		for index, field := range []*time.Duration{&grant.waited, &grant.held} {
			if *field, err = time.ParseDuration(match[index+6]); err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
		}
		grant.from = grant.to.Add(-grant.held)
		grants = append(grants, grant)
	}
	return grants
}

// logHeapGrants logs, for each bound, how many of grants held it whole, the longest any waited
// for its weight and the longest any held it, under what.
func logHeapGrants(t *testing.T, what string, grants []heapGrant) {
	t.Helper()
	for _, bound := range []string{"apply", "copy"} {
		logged, alone := 0, 0
		var waited, held time.Duration
		for _, grant := range grants {
			if grant.bound != bound {
				continue
			}
			logged++
			if grant.alone {
				alone++
			}
			waited, held = max(waited, grant.waited), max(held, grant.held)
		}
		t.Logf("%s: %d grants of the %s bound logged, %d of them holding all of it; the longest wait for it %s, the longest hold %s",
			what, logged, bound, alone, waited.Round(time.Millisecond), held.Round(time.Millisecond))
	}
}

// longestOverlap is how many pairs of a grant of applies and one of copies were held at the same
// moment, and the longest time any such pair was.
func longestOverlap(applies, copies []heapGrant) (longest time.Duration, pairs int) {
	for _, apply := range applies {
		for _, copied := range copies {
			from, to := apply.from, apply.to
			if copied.from.After(from) {
				from = copied.from
			}
			if copied.to.Before(to) {
				to = copied.to
			}
			if to.After(from) {
				pairs++
				longest = max(longest, to.Sub(from))
			}
		}
	}
	return longest, pairs
}
