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
	"maps"
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
	// podMemory is the memory limit of the pod Dispatch runs in (newDispatchKubernetes),
	// and podGoMemoryLimit the Go limit a server in it takes, 0.9 of it (setGoMemoryLimit), which
	// the budget drives start their servers under, so they weigh the budget and not the library.
	podMemory        = 4 << 30
	podGoMemoryLimit = 3_865_470_566
	// podGoMemoryLimitMargin is how far past that limit a drive lets the memory the limit bounds
	// read - what the runtime has mapped less what it has handed back - before it fails the step
	// (requireUnderThePod). The limit is what the collector steers the heap goal to, not a wall:
	// an allocation of tens of MiB is mapped whole before the cycle it triggers can sweep or
	// scavenge anything back, so a step that allocates in large pieces reads past the limit by
	// megabytes, and how far past depends on how much of the two processors the collector had
	// while it did - which is why one run of a step says nothing about its worst. The margin
	// bounds that overshoot of the collector's soft limit and nothing else: no holder of heap
	// stands behind it, since every holder a heap profile names at the heaviest step's peak is
	// one a bound charges.
	//
	// Measured at the runtime's own ratio on 2026-10-29, over the drives that weigh a server
	// against the pod: the rooms typed and pasted into stayed 2.1 GiB under the limit and the
	// step that fills the budget a gibibyte under it; the burst of fifty browsers read from
	// 8,094 bytes under it to 57,442 over; the burst of struct-dense documents from 142,809,214
	// under to 1,832,882 over; and the heaviest applies beside the folds, whose frames and
	// copies run to tens of MiB each, 5,240,914, 15,501,378 and 34,453,570 over. The 128 MiB this
	// was then - three and a half times the worst of those - rested on three readings of that
	// heaviest step, whose overshoot spreads far wider. Over ten runs of it at 5f70eeee on
	// 2026-11-03, alone on this pod, it read 2,210, 137,298, 2,476,098, 2,615,362, 5,138,530,
	// 14,796,882, 48,175,186, 63,137,858, 95,455,330 and 173,549,698 bytes past the limit. The same
	// spread stands at the commits before it, so none of it is new: 653,362 at 713d2fe8, the last
	// commit before the merge with main's load that no longer replays a document's whole edit
	// history, and 28,547,106 and 90,572,882 at c6ccc5bf, the commit whose 2026-11-01 figures this
	// drive's comment quoted; a run at 713d2fe8 whose server sampled its own live heap, and so
	// collected far more often, read the worst of all, 187,308,018. Ten more runs the same day
	// at a65dac4f, after the merge with main's observer that renders only the blocks a keystroke
	// changed, read 4,221,026 to 78,747,778 past it, charging 3,675 to 3,739 MiB.
	//
	// 256 MiB is 1.55 times the worst of the ten runs at 5f70eeee and 1.43 times the worst of every
	// run measured, and still a bound on what a pod is charged: a step within it holds
	// 4,133,906,022 bytes of the pod's 4,294,967,296, leaving 161,061,274 - 153 MiB - for memory
	// outside the runtime, which these steps read at 25 to 41 MiB - their charged peak less their
	// held peak - in every one but the heaviest applies, where that difference was 22 MiB the
	// other way, the double subtraction this margin is here to bound. That undercount - a release
	// of 2 MiB or more subtracted twice, once as a page gVisor dropped from VmRSS and once as
	// released heap - runs to hundreds of MiB over a step (peakResident, memory_harness_test.go).
	// What holds Dispatch inside its pod is not this reading but the charge a pod's kernel would
	// make, which requireUnderThePod asserts against the pod itself and which no run of this drive
	// has read above 3,840 MiB of the pod's 4,096.
	podGoMemoryLimitMargin = 256 << 20
	// podMemoryOutsideTheRuntime is the least of the pod a step within the margin must leave for
	// the memory a pod is charged beyond what the Go runtime holds - the binary's mapped text and
	// data, and what the kernel charges for the process outside the runtime's own count - which
	// these steps read at 25 to 41 MiB, their charged peak less their held peak. 4,294,967,296
	// less the limit and the margin leaves 161,061,274 bytes, 153 MiB, so this holds with 25 MiB
	// to spare, and the limit and the margin can never grow into it unnoticed: the build refuses
	// the constant below once they do.
	podMemoryOutsideTheRuntime = 128 << 20
	// A uint holds no negative constant, so this does not compile once the limit, the margin and
	// the memory outside the runtime together pass the pod.
	_ = uint(podMemory - podGoMemoryLimit - podGoMemoryLimitMargin - podMemoryOutsideTheRuntime)

	// These mirror constants of internal/dispatch/docs, which this package cannot read:
	// roomBudgetBytes, bytesOfHeapAStateByte, roomCopies, maxPeerInsertAtTheBound,
	// maxPeerByteCreep, maxRoomStateBytes and maxMessageBytes. Each is the figure the cut of
	// INFRA-179 left: one price a state byte in place of the six the budget charged before, and
	// the room's state cap in place of the unrendered bound and the frame total
	// (dispatch://INFRA-179/artifact/infra179-new-design-md).
	//
	// No drive takes a mirror on trust. The server names its budget in the line it logs for a
	// refusal (refuseOverRoomBudget), and the budget drives fail unless that is this number; the
	// byte drive fails unless a run of small inserts is refused exactly once the room is the
	// creep past the bound, a paste past the insert allowance is refused, and a frame of exactly
	// the message floor reaches the room's hook while one byte more does not; and the state cap
	// drive fails unless exactly as many margin records are admitted as fit under it.
	roomBudgetBytesForTest         = 2 << 30
	bytesOfHeapAStateByteForTest   = 14
	roomCopiesForTest              = 2
	maxPeerInsertAtTheBoundForTest = 8 << 10
	maxPeerByteCreepForTest        = 64 << 10
	maxMessageBytesForTest         = 64 << 20
	// The state cap is a quarter of the message floor and is derived from it in the product, so
	// a room at the cap hands over a frame about the size of its state and four such frames fit
	// the floor; the mirror derives it the same way rather than naming a second number.
	maxRoomStateBytesForTest = maxMessageBytesForTest / 4

	// perFrameScanCeilingForTest mirrors perFrameScanCeiling, the most items one browser's
	// update may scan when it holds a load slot, and bytesOfAScanMarkForTest bytesOfAScanMark,
	// the heap a remote apply's scans hold for each distinct item they pass. Together they are
	// scanCeilingForTest, the allowance the room hands an apply of a given size, which the
	// server names in the line it logs for a refusal (scanRefusalLine) and which both scan
	// drives hold it to.
	perFrameScanCeilingForTest = 400_000
	bytesOfAScanMarkForTest    = 76

	// unslottedApplyBytesForTest mirrors unslottedApplyBytes, the update's own bytes at or under
	// which an apply of a browser's update takes no slot of the pod's load concurrency
	// (internal/dispatch/docs/copy_heap.go). A frame above it is held across its whole apply by
	// one slot, which is what makes a reconnect storm's whole-state handoffs decode k at a time;
	// a keystroke, far under it, never waits. It is also the bound on an unslotted apply's scan
	// marks (scanCeilingForTest).
	unslottedApplyBytesForTest = 2 << 20
	// loadSlotBytesForTest mirrors loadSlotBytes and loadSlotsForTest docs.MemoryBounds.LoadSlots
	// at the calibration: 600 MB a slot - one whole-document decode, one live write's fork or one
	// heavy apply - and one slot at the 3,865,470,566-byte Go limit a 4 GiB pod gives the server,
	// which TestTheBootLineNamesTheBoundsItsGoMemoryLimitGives holds the boot line to and which
	// the server names in each line it logs for a slot it gives back (loadSlotLine).
	//
	// One slot at the calibration is not only a memory bound. It is what keeps A3's "the refusals
	// begin within one room of the budget's own arithmetic" true now that an admission reserves
	// nothing for the document it is about to load: with one cold load at a time the budget sees
	// each room's weight before the next load begins, so fifty admissions at once serialise into
	// one after another rather than fifty of them reading the same headroom at the same moment.
	loadSlotBytesForTest = 600_000_000
	loadSlotsForTest     = 1

	// keystrokeUpdateBytesForTest is the most one keystroke's update holds: a one-character
	// insert is a struct's info byte, its client and clock, its two origins and the character
	// itself, under the update's own header, which ygo encodes in a few dozen bytes. It is set
	// past any of them, and only the room a person types in is sized from it.
	keystrokeUpdateBytesForTest = 64
	// keystrokeChargeForTest is the most one keystroke adds to its room's charge: its update's
	// own bytes, which the room's observer adds to the room's tracked state as it drains that
	// update, at the one price and at both copies (roomWeightForTest). Nothing else charges a
	// keystroke now: the structure part, the densest-byte price and the count that re-based them
	// all went with the cut.
	keystrokeChargeForTest = keystrokeUpdateBytesForTest * bytesOfHeapAStateByteForTest * roomCopiesForTest
)

// roomWeightForTest is what the room budget charges a room whose tracked state is stateBytes
// long, as the server charges it (docs' roomWeight): its state at one price in each of its two
// copies, 28 bytes a state byte. A room's tracked state is the stored state its load decoded
// plus the bytes of every update its own observer has drained since, so a drive weighs a room by
// what the store holds for it (memoryHarness.storedState) and what it has sent into it.
func roomWeightForTest(stateBytes int64) int64 {
	return stateBytes * bytesOfHeapAStateByteForTest * roomCopiesForTest
}

// scanCeilingForTest is the most items the scans of a browser's update of updateBytes bytes may
// pass, as the room works it out (docs' scanCeiling): the whole per-frame ceiling for an update
// over unslottedApplyBytesForTest, which applies holding a load slot, and for any other what its
// scan marks can hold in what is left of unslottedApplyBytesForTest past the update's own bytes.
//
// A frame built to tangle a room is always the second kind, and cannot be made the first. The
// slot is taken at 2 MiB and the document bound is 1 MiB, so an update large enough to hold a
// slot is large enough to trip the byte bound, whose allowance of maxPeerInsertAtTheBoundForTest
// would refuse the padding that made it large before its scans were ever weighed. So the
// allowance a tangle frame meets is (2 MiB - its own bytes) / 76, and the drives below build
// their frames against that rather than against the ceiling. The whole ceiling belongs to the
// one apply that is over 2 MiB and still integrates nothing of its own: a browser's whole-state
// handoff.
func scanCeilingForTest(updateBytes int) int {
	if updateBytes > unslottedApplyBytesForTest {
		return perFrameScanCeilingForTest
	}
	return max((unslottedApplyBytesForTest-updateBytes)/bytesOfAScanMarkForTest, 1)
}

// podServerEnv is the environment a drive that judges a server against the pod adds to its
// harness (memoryHarness.env): the pod's Go memory limit, and the collector's ratio named rather
// than inherited. Dispatch reads and sets no GOGC, so a pod runs at the runtime's default of 100,
// and every figure these drives quote - the room budget's refusals, the charged peaks and the
// overshoot podGoMemoryLimitMargin was measured from - was read there. Naming it here keeps them
// there whatever ratio the harness's own environment gives every other memory test.
func podServerEnv() []string {
	return []string{"GOMEMLIMIT=" + strconv.Itoa(podGoMemoryLimit), "GOGC=100"}
}

// One browser's update is bounded by bytes, over a real document socket of a real server
// (Service.weighPeerUpdate): an update that would
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

// A frame no other rule refuses but whose merge would scan past the allowance its room gives it
// (scanCeiling, internal/dispatch/docs/scan_budget.go) is refused whole, over a real
// document socket of a real server, and the room is left as it was. A paragraph is first made a
// chain of tangledChain one-character items, each of a client of its own, so a run of them is never
// merged into one item; then a frame whose items each name the chain's first item as their left
// origin and no right origin, so each scans the whole chain, after a 256 KiB string at the
// chain's end, where nothing follows it to scan.
//
// The allowance is the frame's own, not the per-frame ceiling. A tangle frame can never earn the
// ceiling: the ceiling belongs to an apply over unslottedApplyBytesForTest, 2 MiB, and an update
// that large is past the 1 MiB document bound, whose allowance of maxPeerInsertAtTheBoundForTest
// would refuse the padding that made it large before its scans were weighed at all. So what this
// frame meets is (2 MiB - its own bytes) / 76, and the drive builds the frame against that
// allowance rather than assuming one (scanFrameAtItsAllowance) and holds the server's own line
// to the same figure. The whole ceiling belongs to the one apply over 2 MiB that still
// integrates nothing of its own: a browser's whole-state handoff.
//
// The frame passes the byte bound (the document is far under it), the decode cap and the
// room's state cap, and the room's bounded document refuses it at that allowance: its
// browser is closed with 4413 and DOC_TOO_TANGLED, the room's encoded state is byte for byte what
// it was, a browser joining after the refusal receives that state, the stored document once
// settled holds it, and the budget's figures and the room's count of structs, which the server logs at
// each refusal as the refusal leaves them, are the same at a second refusal of the frame as at the
// first, so the first left them as it found them. The refused scans count toward the room's scan
// total, whose million items leave more than the allowance after the first refusal, so the second is
// at the same allowance, still DOC_TOO_TANGLED, with the room's total grown by what the first
// refusal's scans passed. A frame one item shorter, whose scans stay within its own allowance, applies
// once the server has started again and loaded the room afresh, whose total holds none of the
// refused scans: what refused the frame is the allowance and nothing else.
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
	// The chain's own bytes are on the room's charge the moment its drain commits (docs'
	// renderedReplica.chargeAdded), so the only thing to wait for is the settlement after the
	// room's settle delay; once it has landed, nothing moves the budget's figures or the room's
	// count of structs but what the browsers below send.
	memory.waitForSettled(t, room)
	before := setting.state(t)

	frame, items, scans, allowance := scanFrameAtItsAllowance(t, before, tangledScanClient)
	t.Logf("a %d-byte frame of %d items scanning %d items against an allowance of %d, sent to a room holding %d bytes of state",
		len(frame), items, scans, allowance, len(before))
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
		if logged, err := strconv.Atoi(refusal[5]); err != nil || logged != allowance {
			t.Fatalf("refusal %d names an allowance of %s items, want the %d the frame's own %d bytes earn it", index+1, refusal[5], allowance, len(frame))
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
	if want := scanCeilingForTest(len(shorter)); shorterScans > want {
		t.Fatalf("a frame one item shorter scans %d items, want within the %d its own %d bytes are allowed", shorterScans, want, len(shorter))
	}
	server.stop(t)
	again := memory.start(t)
	reopened := again.openBrowser(t, room)
	t.Cleanup(reopened.leave)
	requireApplied(t, reopened, shorter, fmt.Sprintf("a frame of %d items scanning %d", items-1, shorterScans))
}

// A room's scan total is re-measured by each load, over a real document socket of a real server:
// a room takes a run of browsers' frames, each the longest its own bytes are allowed to scan
// (scanCeilingForTest, about twenty-seven thousand items), until what is left of the room's
// million-item total is less than one such frame; the frame that meets that is refused at an
// allowance of what is left, which the server names in the line it logs for the refusal
// (scanRefusalLine). The server stops, which flushes the room to the store, and a new server
// loads the room from what it stored; the same frame then applies, because the reloaded room's
// total is what its stored state re-scans (docs.RescanForTest) and not the one the room's own
// applies came to. That re-scan is what the load's own decode cost, and it is what the total is
// there to bound (roomScans.rescanned).
//
// That re-scan is not the sum of what the room's own applies scanned, and at sjawhar/ygo
// v1.55.1-sami.1 it is the smaller of the two for a shape like this one: the room's browsers scan
// as their frames arrive, while a decode of the whole stored state integrates the clients' groups
// by descending client (crdt/update.go:386-387), and each frame's client is above the one before
// it, so the decode takes the frames in the reverse of the order the room applied them. What
// bounds any one apply is the per-frame ceiling, which no total changes.
// TestTheRoomScanTotalIsReMeasuredByEachLoad holds the same truth in process.
func TestTheRoomScanTotalIsReMeasuredByEachLoadOverARealSocket(t *testing.T) {
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

	// The room's scan total is a million items and no one browser's frame may scan more than
	// what its own bytes are allowed, about twenty-four thousand (scanCeilingForTest), so the
	// total is used up by a run of frames rather than by two. Each is the longest frame of a
	// client of its own that stays within its own allowance; the run stops at the frame the
	// room refuses, which is the one whose scans pass what is left of the total rather than
	// what its bytes are allowed.
	admitted, sent := 0, 0
	var third []byte
	var thirdScans int
	for client := crdt.ClientID(0); third == nil; client++ {
		if client > 200 {
			t.Fatalf("the room took %d frames scanning %d items and refused none, want the scan total used up", sent, admitted)
		}
		state := setting.state(t)
		_, items, _, _ := scanFrameAtItsAllowance(t, state, tangledScanClient+client)
		frame := scanFrame(tangledScanClient+client, items-1)
		scans := frameScans(t, state, frame)
		applied, closed, err := setting.send(setting.updateFrame(frame))
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			admitted += scans
			sent++
			continue
		}
		// Two closes are correct answers to this one frame, and which the room gives turns on
		// where the allowance it was weighed against came from. An allowance taken from the
		// frame's own bytes is the frame being too tangled for one apply, DOC_TOO_TANGLED; one
		// taken from what is left of the room's million-item scan total is the room asking to
		// be rebuilt, DOC_NEEDS_REBUILD, which the editor offers the rebuild for (docs'
		// documentNeedsRebuild, roomScans.roomUsedUp). The run of frames lands either side of
		// that line from one run to the next - each scans about twenty-seven thousand items
		// against a total of a million - so the drive takes both and names which it got, where
		// asserting one would be asserting the arithmetic of a particular run.
		refusedWith := requireRefusedEither(t, applied, closed,
			contracts.DocumentTooTangledCloseReason, contracts.DocumentNeedsRebuildCloseReason)
		t.Logf("the frame scanning %d items was refused %s, after the room's own frames had scanned %d of its total", scans, refusedWith, admitted)
		// The refusal closed this browser, so the drive reads the room through another and
		// keeps the frame that was refused: it is the one the reload is weighed against.
		setting.leave()
		setting = server.openBrowser(t, room)
		t.Cleanup(setting.leave)
		third, thirdScans = frame, scans
	}
	after := setting.state(t)
	t.Logf("the room took %d frames scanning %d items together before one scanning %d was refused", sent, admitted, thirdScans)

	// refuse sends the refused frame to the room on server again and returns the allowance the
	// refusal names and what the room's history had scanned when the frame arrived. logged is
	// how many refusals that server will have logged by then: on the first server the run above
	// left one of its own.
	refuse := func(server *memoryServer, logged int) (allowance, used int) {
		t.Helper()
		refused := server.openBrowser(t, room)
		t.Cleanup(refused.leave)
		if state := refused.state(t); !bytes.Equal(canonicalState(t, state), canonicalState(t, after)) {
			t.Fatalf("the room holds a %d-byte state, want the %d-byte state the run of frames left", len(state), len(after))
		}
		applied, closed, err := refused.send(refused.updateFrame(third))
		if err != nil {
			t.Fatal(err)
		}
		// Either close, for the reason the run above gives: the frame meets its own bytes'
		// allowance or what is left of the room's total, and both refuse it.
		requireRefusedEither(t, applied, closed,
			contracts.DocumentTooTangledCloseReason, contracts.DocumentNeedsRebuildCloseReason)
		refusal := server.scanRefusals(t, logged)[logged-1]
		t.Logf("the server logged %q", refusal[0])
		allowance, allowanceErr := strconv.Atoi(refusal[5])
		scanned, scannedErr := strconv.Atoi(refusal[6])
		left, leftErr := strconv.Atoi(refusal[7])
		if allowanceErr != nil || scannedErr != nil || leftErr != nil {
			t.Fatalf("the refusal names allowance=%s scan_total=%s scans_left=%s, want three counts",
				refusal[5], refusal[6], refusal[7])
		}
		if want := scanCeilingForTest(len(third)); allowance >= thirdScans || allowance >= want {
			t.Fatalf("the refusal names an allowance of %d items, want what is left of the room's total, under the frame's %d scans and the %d its own bytes are allowed",
				allowance, thirdScans, want)
		}
		// The line's scan_total and scans_left are the room's total and what is left of the whole
		// scan total past it once the refusal has settled, with no other apply's allowance in
		// flight, so their sum is that whole total; and the allowance, which is under the ceiling,
		// is what was left of it when the frame arrived. The total less the allowance is therefore
		// what the room's history had scanned before the refusal.
		return allowance, scanned + left - allowance
	}
	left, used := refuse(server, 2)
	memory.waitForSettled(t, room)
	server.stop(t)
	storedState := memory.storedState(t, room)
	if !bytes.Equal(canonicalState(t, storedState), canonicalState(t, after)) {
		t.Fatalf("the store holds a %d-byte state, want the %d-byte state the room held after the run of frames", len(storedState), len(after))
	}
	again := memory.start(t)
	// The reloaded room re-measures its total from the stored state rather than carrying the
	// one its own applies came to, and for this shape the re-scan is the smaller of the two: a
	// decode of the whole state integrates the clients' groups by descending client, and each
	// frame's client is above the one before it, so the decode takes the frames in the reverse
	// of the order the room applied them and scans far less. So the frame the room refused
	// before the reload applies after it, and that is what the re-measurement means. It is read
	// here rather than through a second refusal, which the reloaded room has no reason to make.
	rescanned, err := docs.RescanForTest(storedState)
	if err != nil {
		t.Fatalf("re-scan the stored state: %v", err)
	}
	reopened := again.openBrowser(t, room)
	t.Cleanup(reopened.leave)
	t.Logf("before the reload the room had scanned %d items and refused the frame at %d left; its %d-byte stored state re-scans %d items",
		used, left, len(storedState), rescanned)
	if rescanned >= int64(used) {
		t.Errorf("the stored state re-scans %d items, no fewer than the %d the room's own applies scanned: a load that started the room there would leave it no better off than the room it replaced",
			rescanned, used)
	}
	requireApplied(t, reopened, third, fmt.Sprintf("the frame of %d scans the room refused before the reload", thirdScans))
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

// scanFrameAtItsAllowance is the shortest frame of client's whose scans pass the allowance the
// room gives an apply of that frame's own size, with the scans it makes and that allowance. The
// two depend on each other - a longer frame leaves less of unslottedApplyBytesForTest for its
// marks, so it is given a smaller allowance (scanCeilingForTest) - so the frame is taken to a
// fixed point: built against the allowance the last frame's size gave, until it scans past its
// own allowance and one item shorter does not. The feedback is weak, since an item adds about a
// byte to the frame and about tangledChain to its scans, so it settles in a pass or two.
func scanFrameAtItsAllowance(t *testing.T, state []byte, client crdt.ClientID) (frame []byte, items, scans, allowance int) {
	t.Helper()
	allowance = scanCeilingForTest(len(scanFrame(client, 1)))
	for range 8 {
		frame, items, scans = scanFramePast(t, state, client, allowance)
		allowance = scanCeilingForTest(len(frame))
		if scans <= allowance {
			continue
		}
		if items == 1 || frameScans(t, state, scanFrame(client, items-1)) <= allowance {
			return frame, items, scans, allowance
		}
	}
	t.Fatalf("no frame of client %d settles at the allowance its own size gives it", client)
	return nil, 0, 0, 0
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
	// (sjawhar/ygo v1.55.1-sami.1, crdt/item.go:189-202).
	tangledScanClient = 4_000_000
	// scanFramePadding is the bytes of the string scanFrame opens with: it stands at the chain's
	// end, where nothing follows it to scan, and makes the frame a paste's size rather than a
	// few hundred bytes. It is part of what the room weighs the frame's scan allowance from,
	// since an apply under unslottedApplyBytesForTest is given what its marks can hold in what
	// is left of that figure past the frame's own bytes (scanCeilingForTest), so the drives
	// compute the allowance from the frame they built rather than assuming one.
	//
	// 8 KiB and not the 256 KiB of before, because the padding is text the room renders. A room
	// takes about forty of these frames to use up its million-item scan total
	// (TestTheRoomScanTotalIsReMeasuredByEachLoadOverARealSocket), and forty paddings of 256 KiB
	// would carry the rendering past pmdoc.MaxDocumentBytes long before then, so the byte bound
	// would refuse the fifth frame with DOC_TOO_LARGE and the scan total would never be reached.
	// Forty of 8 KiB are 320 KiB, well inside the bound.
	scanFramePadding = 8 << 10
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
// there to its end (sjawhar/ygo v1.55.1-sami.1, crdt/item.go:168-232).
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
// allowance, with the budget's figures, the structs the room's document holds and its scan total
// as the refusal leaves them, and the allowance it was refused at (Service.refusedByApply,
// internal/dispatch/docs/peer_update.go). The count of structs is ygo's own
// (crdt.Doc.StructCount) where it was the room's running struct total before the cut.
var scanRefusalLine = regexp.MustCompile(`msg="dispatch: refused a browser's update its room could not merge within its scan allowance" room=(\S+) update_bytes=\d+ charged=(\d+) limit=(\d+) structs=(\d+) allowance=(\d+) scan_total=(\d+) scans_left=(-?\d+)`)

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

// unboundedRoomBudget is a room budget no drive of one document's bounds comes near: a room of a
// mebibyte of prose is charged about 84 MB of it (roomWeightForTest), and a drive that writes
// tens of megabytes into one room would fill the real budget.
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

// The flood the room's state cap closes, over a real document socket of a real server: a browser
// that is not the editor sends eighty updates of a million margin bytes each, every one a single
// clock and a single struct of a client of its own, which before any such cap were all admitted
// and took a room's whole-state frame from 264 bytes to 80,001,457, past the message floor every
// browser of the document then reconnects against. Each is admitted while the room's tracked
// state has room under maxRoomStateBytes for the content it carries, and the first that would
// pass the cap and every one after it is refused with 4413 and DOC_NEEDS_REBUILD: the cap is the
// one bound of the document's that only a rebuild clears, and the editor offers that rebuild.
// The stored document, once settled, is a whole-state frame within the cap and one record, four
// of which fit the message floor, and a browser opening it receives that state and hands it back
// whole without its socket being closed. The room budget is set out of reach first, so what
// refuses is the document's own bound and nothing else.
//
// internal/dispatch/docs holds the same cap in process
// (TestBrowserUpdatesOfMarginBytesStopAtTheRoomsStateCap). What this drive adds is what the cap
// is for: that the whole state a real room is left holding crosses a real socket within the
// floor A1 asks it to clear, and that a room that reached the cap opens, hands over and takes a
// delete afterwards.
//
// How many are admitted is the cap's own arithmetic, taken from the bytes themselves rather than
// from a figure of the drive's. A room's tracked state starts at the stored state its load
// decoded - which is why the drive restarts the server before it floods, so that state is the
// one it measured - and each admitted update raises it by that update's own bytes (docs'
// renderedReplica.chargeAdded). An update is admitted while the content it carries fits under
// the cap above that tracked state, so the admitted are the first (cap - state - content)/update
// + 1 of them.
func TestMarginFloodUpdatesStopAtTheRoomsStateCapAndTheFrameStaysUnderTheFloor(t *testing.T) {
	memory := newMemoryHarness(t)
	uploading := memory.start(t)
	stored := uploading.upload(t, "json", memory.issue, "# Flooded\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	uploading.stop(t)
	server := memory.start(t)
	server.setRoomBudget(t, unboundedRoomBudget)
	state0 := int64(len(memory.storedState(t, room)))

	const updates, size = 80, 1_000_000
	text := strings.Repeat("m", size)
	each := int64(len(marginFloodRecord(5_000_000, "c00", text)))
	want := int((maxRoomStateBytesForTest-state0-size)/each) + 1
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
		requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
		refused++
		flooding.leave()
		flooding = server.openBrowser(t, room)
	}
	flooding.leave()
	memory.waitForSettled(t, room)
	frame := int64(len(canonicalState(t, memory.storedState(t, room))))
	t.Logf("%d of %d updates carrying %d bytes of content in %d bytes of update were admitted into a room starting at %d bytes of state, against the %d-byte state cap, %d refused; the stored whole-state frame is %d bytes, four of which are weighed against the %d-byte floor",
		admitted, updates, size, each, state0, maxRoomStateBytesForTest, refused, frame, maxMessageBytesForTest)
	if admitted != want {
		t.Fatalf("%d updates were admitted, want the %d that fit under the %d-byte state cap", admitted, want, maxRoomStateBytesForTest)
	}
	if frame > maxRoomStateBytesForTest+each || 4*frame >= maxMessageBytesForTest {
		t.Fatalf("the stored whole-state frame is %d bytes, want it within the %d-byte state cap and one %d-byte record, and four of them under the %d-byte floor",
			frame, maxRoomStateBytesForTest, each, maxMessageBytesForTest)
	}
	handing := server.openBrowser(t, room)
	t.Cleanup(handing.leave)
	state := handing.state(t)
	if applied, closed, err := handing.send(handing.stateFrame(state)); err != nil || !applied || closed != nil {
		t.Fatalf("handing back the flooded document's %d-byte whole state: applied %v, closed %v, %v; want it applied and the socket open", len(state), applied, closed, err)
	}
	// A delete integrates nothing, so the cap admits it: a room that has reached the cap can
	// still be trimmed, which is what keeps the rebuild the close offers from being the only way
	// out of it.
	cutting := server.openBrowser(t, room)
	t.Cleanup(cutting.leave)
	cutDoc := newBrowserDocument(t, cutting.state(t)).doc
	margin := cutDoc.GetMap("marks")
	if !margin.Has("c00") {
		t.Fatalf("the flooded margin holds no record under c00, want the one the first update wrote: %v", margin.Keys())
	}
	held := cutDoc.StateVector()
	cutDoc.Transact(func(txn *crdt.Transaction) { margin.Delete(txn, "c00") })
	cut := crdt.EncodeStateAsUpdateV1(cutDoc, held)
	requireInsertsNothing(t, cut)
	requireApplied(t, cutting, cut, "a delete of a margin record from a room at the state cap")
}

// The room budget refuses the browser its own arithmetic says it has no room for: documents of a
// mebibyte of prose, each edited once, opened until one is refused with 4503 and DOC_BUSY, under
// the Go limit a 4 GiB pod gives the server. A room is charged its stored state at one price in
// each of its two copies, 28 bytes a state byte, from the moment its load decodes that state
// (roomWeightForTest, docs' chargeLoadedRoom). Nothing is reserved for a room before it loads,
// so an admission is weighed against what the open rooms already hold and nothing else: the room
// that crosses the limit is the last one admitted, not the next one refused, and the refusals
// begin within one room of the budget divided by one room's weight, and one more. That
// arithmetic is computed here from the server's constants (mebibyteFleet) and held to the
// numbers the server logs, both when the browsers come one after another and when fifty open at
// once.
//
// With nothing reserved, what keeps "within one room" true of fifty at once is the pod's load
// concurrency: k is 1 at the calibration (loadSlotsForTest), so the cold loads take their slot
// one at a time and the budget sees each room's weight before the next load begins. Fifty
// admissions therefore serialise into one after another, each weighed against the rooms the ones
// before it left charged, where k admissions at once would read the same headroom at the same
// moment and the refusals would begin up to k rooms late.
//
// One after another, five of the browsers leave once they have edited, the last of them just before
// the next browser asks, and no browser is refused while a room whose browser went is still
// charged: an admission the budget is short for waits for the sweep it starts (roomBudget.await),
// which closes such a room once its settlement has committed (admissionFlushAllowance), and the
// admission is then made into the room the close freed. Each close is read from the line the server
// logs for it, in the budget's own figures, and from the room's absence, which a test-only read
// reports (roomLive). The first refusal comes only once every one of those rooms is closed, read the
// same two ways, with the rooms of the browsers that stayed within one of the arithmetic. Then a
// browser that stayed leaves just before the next asks: that browser is admitted into the room the
// leaving freed, closed for it, and the one after it is refused. A browser that stayed keeps its
// room, each step holds both the memory a pod's kernel would charge and the memory the server's
// own Go limit bounds within what bounds them, and once the rest have left, a browser is
// admitted again.
//
// Fifty at once, each browser hands back its whole state as soon as it has the room's, as the
// editor does on every connect, and then edits: the budget admits within one room of the same
// number and refuses the rest at admission, every browser one or the other and none answered 503,
// though their rooms' loads share the pod's one load slot and the last of them waits for every
// one before it; and no admitted browser's handoff or first edit is refused, since a handoff of
// the state the room holds integrates nothing and so passes whatever the budget says, and the
// room's whole weight was charged by its load before the edit arrived.
//
// The figures below were measured before the cut, when a mebibyte of prose reserved 57,600,000
// bytes at admission by its elements' price and the 2,147,483,648-byte budget held 37 such
// rooms. The one price charges such a room its stored state - about 3 MB - at 28 bytes a byte,
// some 84 MB, so the budget now holds about 25 of them; the memory those rooms cost the pod is
// what the readings below bound and what requireUnderThePod reads afresh on every run.
//
// Measured on 2026-11-03 under sjawhar/ygo v1.54.1-sami.3 on this two-processor pod, beside
// another role's tests on it, the server at the pod's Go memory limit and the collector at the
// runtime's own ratio, the only one Dispatch runs at: one after another, the first refusal came
// with 37 rooms held by browsers that stayed, once the five whose browsers went were closed for
// it, and so did the last; the peak a pod's kernel would charge was 3,638 MiB and the runtime
// held 3,759,040,168 bytes of the 3,865,470,566 its limit bounds, VmRSS 3,656 MiB. Fifty at
// once, 37 handed their state back and edited, the slowest answered after 2m17.234s, 13 were
// refused at admission, the slowest after 26.439s, and none was answered 503; the burst logged
// 121 grants of the copy bound the slots replaced, the longest wait 54.141 s and the longest
// hold 7.865 s; it charged 3,721 MiB and held 3,848,718,024 bytes, VmRSS 3,770 MiB, the whole
// drive in 443 s. The same drive on 2026-11-01, with the pod to itself, refused the thirteen by
// 10.471s and answered the last of the thirty-seven at 1m15.215s, charging 3,629 and 3,730 MiB
// by step and holding 3,760,039,608 and 3,859,998,408 bytes, in 338 s.
//
// Those runs' peaks were read as VmRSS besides, which on this sandbox counts the pages the
// runtime has released as well (peakResident). Read as a pod's kernel would charge it and
// against the Go memory limit, both from the server's own trace (requireUnderThePod), a run of
// 2026-10-29 charged 3,616 MiB with the browsers coming one after another and 3,727 MiB over the
// burst, against VmRSS of 3,623 and 3,768 MiB, and held 3,755,968,168 and 3,865,462,472 bytes of
// what the 3,865,470,566-byte limit bounds, 104 MiB and 8,094 bytes under it.
func TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays(t *testing.T) {
	const burst, gone = 50, 5
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	fleet := memory.mebibyteFleet(t, func(int64) int { return burst + 4 })
	// The budget's own arithmetic. A room is charged its stored state at one price over both
	// copies the moment its load decodes it (roomWeightForTest, docs' chargeLoadedRoom), and an
	// admission carries the weight of the room it is about to load - read from the stored state
	// under a slot of its own, released before the weighing - and holds it as a reservation
	// until that room is charged (docs' roomBudget.refusal). So an admission is weighed against
	// what the open rooms hold, plus what the admissions in flight have reserved, plus its own
	// weight, and no admission can take the budget past its limit: the rooms open at the first
	// refusal are the budget divided by one room's weight.
	//
	// The reservation is what makes that true of fifty at once as of one after another. Without
	// it, fifty admissions weighed before any of their rooms had charged anything would each
	// read the same headroom and all pass: measured before it, twenty-four browsers arriving
	// together were all admitted and their rooms charged 5,976,639,564 bytes against this
	// 2,147,483,648-byte budget, with the pod's kernel charging 7,225 MiB of its 4,096.
	rooms := int(roomBudgetBytesForTest / fleet.weight)
	t.Logf("a mebibyte of prose makes %d elements and a room of it is charged %d bytes: the %d-byte budget holds %d such rooms, and no admission takes it past its limit", fleet.elements, fleet.weight, roomBudgetBytesForTest, rooms)
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
		var firstRefusal int
		var final *gws.CloseError
		// leftClosed is the closes the budget logged of rooms whose browsers went (closesOf), and
		// closedWithin the same once it holds want of them. A sweep with none of those left to close
		// goes on to any other room nothing holds, such as one the server opened for its own work,
		// heaviest first.
		leftClosed := func() []budgetClose { return closesOf(t, server, left, 0) }
		closedWithin := func(want int) []budgetClose { return closesOf(t, server, left, want) }
		peak := server.peakResident(t, func() {
			// Those who stay, then those who go, one room short of the budget's arithmetic: the
			// rooms of those who went are the budget's to close once a browser needs their room. The
			// last of them leaves just before the next browser asks.
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
			// Each of those rooms is the budget's to close only once the server has forgotten its
			// browser, which the drive waits for before it asks the budget for a room one of them
			// has to be closed for (requireBrowserForgotten): the admissions below are the ones the
			// budget is short for, and an admission that meets a departure still settling waits for
			// it and is refused where it outlasts the admission's own wait.
			for _, room := range left {
				requireBrowserForgotten(t, server, room, 1)
			}
			// Then more who stay, until one is refused, which none is while a room whose browser
			// went is still charged: each admission the budget is short for waits for the close of
			// one of them.
			for {
				opened, refused := admit(t)
				if opened != nil {
					stayed = append(stayed, opened)
					continue
				}
				if closes := closedWithin(len(left)); len(closes) != len(left) {
					t.Fatalf("a browser was refused (%v) with %d rooms held by browsers that stayed, while %d of the %d rooms whose browsers went were still charged", refused, len(stayed), len(left)-len(closes), len(left))
				}
				for _, room := range left {
					requireRoomAbsent(t, server, room, "a room whose browser went, at the first refusal")
				}
				firstRefusal = len(stayed)
				t.Logf("the first refusal came with %d rooms held by browsers that stayed, once the %d rooms whose browsers went were closed: %v", firstRefusal, len(left), refused)
				break
			}
			// A browser that stayed leaves just before the next asks: that admission waits for the
			// room it left to be closed, and is admitted into it. A socket's close is not the end
			// of its browser's departure. ygo reports the departure at the end of the peer's
			// teardown - after it has taken the peer out of the room and flushed the room's
			// pending updates to the store (sjawhar/ygo v1.55.1-sami.1,
			// provider/websocket/peer.go:574-720, OnLastPeer) - that report runs the settlement
			// the room owes inside the teardown (settleLastPeer), and only once it returns does
			// the socket's handler forget the browser (removeConnection), which is what empties
			// the room's connected set and leaves the room unused (unusedLocked) for the budget to
			// close. Until then the room is charged and no sweep can close it, so an admission
			// placed inside that window waits for it and is refused where the window outlasts the
			// wait the admission has (admissionWait, the settle delay and
			// admissionFlushAllowance): the settlement of a mebibyte of prose took 10 to 20 s here
			// with the pod's rooms full and two processors to share, against the 10 s of that
			// wait. The drive therefore waits for the server's own report that the socket has
			// ended (requireBrowserForgotten) rather than for a hold of its own, and asks only
			// then. The wait costs the admission nothing of the room it waits for: while no
			// admission waits on the budget and the charges are within its limit, nothing closes a
			// room (Service.unlockState), so the room the browser left is still the budget's to
			// close when the next browser asks for one.
			leaving := stayed[len(stayed)-1]
			stayed = stayed[:len(stayed)-1]
			left = append(left, leaving.room)
			leaving.leave()
			requireBrowserForgotten(t, server, leaving.room, 1)
			opened, refused := admit(t)
			if refused != nil {
				t.Fatalf("the browser asking just after another left was refused (%v), want it admitted once the room that browser left was closed:\n%s", refused, server.output.tail())
			}
			stayed = append(stayed, opened)
			if closes := closedWithin(len(left)); len(closes) != len(left) || closes[len(closes)-1].room != leaving.room {
				t.Fatalf("the browser asking just after another left was admitted, and the budget logged %d closes of the %d rooms whose browsers went, the last of them not the room just left", len(closes), len(left))
			}
			requireRoomAbsent(t, server, leaving.room, "the room a browser left just before the next asked")
			// And the next is refused: no room whose browser went is left to close.
			again, refused := admit(t)
			if again != nil {
				stayed = append(stayed, again)
				t.Fatalf("a browser was admitted with %d rooms held by browsers that stayed and none whose browser went, want it refused", len(stayed)-1)
			}
			final = refused
		})
		closes := leftClosed()
		for _, made := range closes {
			t.Logf("the budget closed %s, a room whose browser went, %d -> %d of %d bytes charged", made.room, made.chargedBefore, made.charged, made.limit)
			if made.limit != roomBudgetBytesForTest || made.chargedBefore-made.charged < fleet.weight {
				t.Errorf("the budget closed a room at %d -> %d of %d bytes charged, giving back %d, want a close that gave back at least the %d a room of this document is charged",
					made.chargedBefore, made.charged, made.limit, made.chargedBefore-made.charged, fleet.weight)
			}
		}
		t.Logf("the first refusal came with %d rooms held by browsers that stayed, and the last, after one of them left and the next browser took its room, with %d (%v)", firstRefusal, len(stayed), final)
		for what, open := range map[string]int{"first": firstRefusal, "last": len(stayed)} {
			if open < rooms-1 || open > rooms+1 {
				t.Errorf("the %s refusal came with %d rooms held by browsers that stayed, want within one of the budget's %d", what, open, rooms)
			}
		}
		refusals := requireBudgetRefusals(t, server)
		if len(refusals) != 2 {
			t.Errorf("the server logged %d refusals of the budget, want the two this drive asked for", len(refusals))
		}
		for index, refusal := range refusals {
			if refusal.charged >= int64(len(stayed)+1)*fleet.weight {
				t.Errorf("at refusal %d the budget charged %d bytes, %d rooms' worth for the %d browsers that stayed: a room whose browser had gone was still charged", index+1, refusal.charged, refusal.charged/fleet.weight, len(stayed))
			}
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
		requireUnderThePod(t, peak, "the rooms opened one after another")

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
			opened              *browser
			handedBack, applied bool
			closed              *gws.CloseError
			err                 error
			// answered is how long the socket's upgrade took to be answered, admitted or not.
			answered time.Duration
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
					asked := time.Now()
					result.opened, result.err = server.tryOpenBrowser(fleet.copies[index])
					result.answered = time.Since(asked)
					if errors.As(result.err, &result.closed) {
						result.err = nil
						return
					}
					// Every copy holds the stored state, so the room's state is what this browser
					// received and hands back whole.
					if result.err == nil {
						result.handedBack, result.closed, result.err = result.opened.send(result.opened.stateFrame(fleet.state))
					}
					if result.err == nil && result.handedBack {
						result.applied, result.closed, result.err = result.opened.send(result.opened.updateFrame(edit))
					}
				})
			}
			group.Wait()
		})
		// What the browsers waited for: each room's load takes one slot of the pod's load
		// concurrency, of which the calibration has one, so the loads run one after another and
		// the line says how long the longest waiter waited and the longest holder held it.
		logLoadSlots(t, "the burst", loadSlotHolds(t, server.output.String()))
		applied, atAdmission, atHandoff, atEdit, unavailable := 0, 0, 0, 0, 0
		var slowestAdmitted, slowestRefused time.Duration
		for index, outcome := range outcomes {
			switch refused := (*upgradeRefused)(nil); {
			case errors.As(outcome.err, &refused) && refused.status == http.StatusServiceUnavailable:
				t.Errorf("browser %d was answered 503 after %s, want it admitted or refused by the budget with %d", index, outcome.answered.Round(time.Millisecond), contracts.DocumentBusyCloseCode)
				unavailable++
			case outcome.err != nil:
				t.Errorf("browser %d, answered after %s: %v", index, outcome.answered.Round(time.Millisecond), outcome.err)
			case outcome.applied:
				applied++
				slowestAdmitted = max(slowestAdmitted, outcome.answered)
			case outcome.closed == nil:
				t.Errorf("browser %d's handoff or edit was not applied and its socket stayed open, want both applied or the browser refused with %d", index, contracts.DocumentBusyCloseCode)
			case outcome.opened == nil:
				requireClose(t, outcome.closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, fmt.Sprintf("browser %d", index))
				atAdmission++
				slowestRefused = max(slowestRefused, outcome.answered)
				requireRoomAbsent(t, server, fleet.copies[index], fmt.Sprintf("browser %d's document, refused at admission", index))
			case !outcome.handedBack:
				t.Errorf("browser %d's handoff of the state its room holds was refused with %v, want it applied", index, outcome.closed)
				atHandoff++
			default:
				t.Errorf("browser %d's first edit was refused with %v, want it applied", index, outcome.closed)
				atEdit++
			}
		}
		t.Logf("%d browsers at once: %d handed their state back and edited their rooms, the slowest answered after %s; %d refused at admission, the slowest after %s; %d answered 503; %d refused their handoff, %d their edit",
			burst, applied, slowestAdmitted.Round(time.Millisecond), atAdmission, slowestRefused.Round(time.Millisecond), unavailable, atHandoff, atEdit)
		requireBudgetRefusals(t, server)
		if applied+atAdmission != burst {
			t.Errorf("%d browsers were admitted and %d refused by the budget, want every one of the %d one or the other", applied, atAdmission, burst)
		}
		if applied < rooms-1 || applied > rooms+1 {
			t.Errorf("%d rooms took their browser's handoff and edit, want within one of the budget's %d", applied, rooms)
		}
		if atHandoff+atEdit != 0 {
			t.Errorf("%d admitted browsers had their handoff refused and %d their first edit, want none: a handoff adds nothing, and a room's second copy is charged from its admission", atHandoff, atEdit)
		}
		for index, outcome := range outcomes {
			if outcome.applied {
				if err := outcome.opened.ping(); err != nil {
					t.Errorf("browser %d lost its room: %v", index, err)
				}
			}
		}
		requireUnderThePod(t, peak, "the burst of fifty browsers")
	})
}

// Rooms admitted small grow under their browsers, and the budget follows them up, closing the rooms
// whose browsers went before it refuses anything. Each room is a copy of one 5 KiB document of
// prose (proseMarkdown), held by a socket that sends nothing, so a room whose growth is refused is
// still held, and grown through a socket of its own in pastes of 64 KiB of the same prose
// (pasteRounds), the same update bytes in every room, toward the document bound, as fast as the
// client can: each paste is sent once the room has answered the last, room after room. A grower
// the budget refuses reconnects into its room and sends its paste again on the next pass, as the
// editor keeps a refused edit and sends it once room frees; the growth ends once a pass moves no
// room. Every fourth room's browsers both leave once it renders more than 256 KiB, a quarter of
// the document bound: early enough that every room, those that leave included, reaches that
// point within the budget (the nine tenths below).
//
// How many rooms grow is computed from the server's own figures: a room of a copy the drive does
// not grow takes the same pastes on a server of its own, and what the budget charges it is read
// at the paste after which every fourth browser leaves and at the last (roomChargesAfter). The
// drive grows the fewest rooms whose held rooms at the last paste are charged past the budget by
// a twentieth, so the budget must refuse, and fails unless that many rooms are charged under
// nine tenths of it at the leaving paste, so those browsers reach it.
//
// A paste is on its room's charge the moment the room's observer drains it, at the paste's own
// bytes over both copies (docs' renderedReplica.chargeAdded, roomWeightForTest), so a grower
// that does not wait is weighed against what it has already written and nothing is outstanding
// between a paste and the next. A room's tracked state therefore runs a little above the state
// it holds - an update's bytes carry what its transaction deleted beside what it added - which
// is the safe side for a budget and is why the drive reads each room's charge from the server
// rather than pricing the pastes itself.
//
// What this proves is that the budget closes the rooms whose browsers went before it refuses any
// growth: at the first refusal every fourth browser has gone and the budget has closed each of
// their rooms, read from the line it logs for each close (budgetCloseLine) and from the room's
// absence (roomLive), and at each refusal after it, every such room whose settlement has committed,
// which makes it closable (unusedLocked), has been
// closed the same way; that once the held rooms are past the budget the next browser is refused
// with 4503 and DOC_BUSY at admission; that each held room's insert is then refused the same way
// while a delete applies; and that the memory a pod's kernel would charge the server stays under
// the pod's throughout, with what its Go memory limit bounds within that limit
// (requireUnderThePod). The documents hold every paste
// their rooms took, none they were refused, the delete, and nowhere the text of the probe every
// held room refused.
func TestTheRoomBudgetFollowsGrowingRoomsAndClosesTheGoneOnesFirst(t *testing.T) {
	const pasteBytes, leaveAt, probeText, pasted = 64 << 10, 256 << 10, " Probe.", "## Pasted section 0\n"
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	// proseMarkdown writes whole sections of about 1,740 bytes, so three of them are the 5 KiB.
	source, state := memory.storedDocument(t, proseMarkdown(t, 6<<10))
	small := memory.storedShape(t, "the 5 KiB document", source)
	if small.rendered < 5<<10-512 || small.rendered > 5<<10+512 {
		t.Fatalf("the document to grow renders %d bytes, want about 5 KiB", small.rendered)
	}
	paste := strings.ReplaceAll(proseMarkdown(t, pasteBytes), "## Section ", "## Pasted section ")
	rounds := pasteRounds(t, state, paste)
	leave := slices.IndexFunc(rounds, func(round pasteRound) bool { return round.size.Bytes > leaveAt })
	if leave < 0 {
		t.Fatalf("no paste takes the document past %d bytes", leaveAt)
	}
	copies := memory.cloneDocuments(t, source, 2)
	charges := memory.roomChargesAfter(t, copies[0], copies[1], rounds, leave, len(rounds)-1)
	atLeave, final := charges[0], charges[1]
	rooms := 4
	for int64(rooms-rooms/4)*final <= roomBudgetBytesForTest*21/20 {
		rooms++
	}
	last := rounds[len(rounds)-1]
	t.Logf("%d pastes of %d bytes take the %d-byte document to %d bytes making %d elements, its room charged %d bytes once counted; every fourth browser leaves after paste %d, at %d bytes and %d charged; %d rooms grow, %d of them held",
		len(rounds), len(paste), small.rendered, last.size.Bytes, last.size.Elements, final, leave+1, rounds[leave].size.Bytes, atLeave, rooms, rooms-rooms/4)
	if charged := int64(rooms) * atLeave; charged >= roomBudgetBytesForTest*9/10 {
		t.Fatalf("%d rooms are charged %d bytes when their every fourth browser leaves, past nine tenths of the %d-byte budget: those browsers might never reach the point they leave at", rooms, charged, roomBudgetBytesForTest)
	}
	copies = memory.cloneDocuments(t, source, rooms+1)
	// The shrink deletes the first half of the first paste, which every held room holds, from a
	// copy of the document that holds that paste alone, so its bytes apply wherever the pastes
	// stopped.
	shrinker := newBrowserDocument(t, state)
	before := len(shrinker.tree(t).Children)
	if err := crdt.ApplyUpdateV1(shrinker.doc, rounds[0].update, nil); err != nil {
		t.Fatal(err)
	}
	shrink := shrinker.edit(t, func(tree *pmdoc.Node) {
		added := len(tree.Children) - before
		tree.Children = append(tree.Children[:before], tree.Children[before+added/2:]...)
	})
	requireInsertsNothing(t, shrink)
	probe := browserEdit(t, state, func(tree *pmdoc.Node) { paddingTexts(t, tree)[0].Text += probeText })

	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	holders, growers := make([]*browser, rooms), make([]*browser, rooms)
	t.Cleanup(func() {
		for _, opened := range append(holders, growers...) {
			if opened != nil {
				opened.leave()
			}
		}
	})
	for index, room := range copies[:rooms] {
		holders[index], growers[index] = server.openBrowser(t, room), server.openBrowser(t, room)
	}
	took := make([]int, rooms)
	gone := make([]bool, rooms)
	var left []string
	leftAt := map[string]time.Time{}
	type refusal struct{ pass, room, took, left int }
	var refusals []refusal
	peak := server.peakResident(t, func() {
		for pass, idle := 1, false; ; pass++ {
			moved := false
			for index := range copies[:rooms] {
				if gone[index] || took[index] == len(rounds) {
					continue
				}
				if growers[index] == nil {
					growers[index] = server.openBrowser(t, copies[index])
				}
				applied, closed, err := growers[index].send(growers[index].updateFrame(rounds[took[index]].update))
				if err != nil {
					t.Fatalf("pass %d, room %d: %v", pass, index, err)
				}
				if applied {
					took[index]++
					moved = true
					if index%4 == 3 && rounds[took[index]-1].size.Bytes > leaveAt {
						holders[index].leave()
						growers[index].leave()
						gone[index] = true
						left = append(left, copies[index])
						// When each room's browsers went, so the sweep's own lines for that
						// room can be read against it afterwards (sweepHoldsAfter).
						leftAt[copies[index]] = time.Now()
						t.Logf("pass %d: room %d's browsers left %s at %s", pass, index, copies[index], leftAt[copies[index]].UTC().Format(time.RFC3339Nano))
					}
					continue
				}
				requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
				growers[index] = nil
				refusals = append(refusals, refusal{pass: pass, room: index, took: took[index], left: len(left)})
				if len(refusals) == 1 {
					// What the budget itself charges each room whose browsers went, at the
					// first refusal: a room the budget has given a charge back for is absent
					// from the read, not zero (roomCharges), so this says whether the ten
					// rooms are still the budget's to close or were closed by the departure's
					// own path before any sweep looked (INFRA-179).
					charges, used, limit := server.roomCharges(t)
					var held int64
					for _, room := range left {
						_, carried := charges[room]
						held += charges[room]
						t.Logf("at the first refusal the budget charges room %s, whose browsers went, %d bytes (carried %v)", room, charges[room], carried)
					}
					t.Logf("at the first refusal the budget charges %d bytes for the %d rooms whose browsers went, of %d used of %d, over the %d rooms it carries",
						held, len(left), used, limit, len(charges))
				}
				// A refusal may come while every room whose browsers have gone is still
				// charged, and that is the teardown window rather than a fault: ygo takes a
				// leaving peer out of its room, flushes the room's pending updates to the
				// store and only then reports the departure, so until it does the budget still
				// sees that room as held (docs' roomBudget.await). An admission waits through
				// that window; an update's refusal reads the budget's full flag with one
				// atomic load, by design, because it runs on the peer's read loop and may not
				// wait. So the drive asks nothing of the instant of the refusal - the peer's
				// read loop was never going to give that ordering - and holds what the design
				// does promise, once the growth has ended: every room whose browsers went is
				// closed, read from the line the budget logs for it and from the room's
				// absence, and a grower the budget refused takes its paste on a later pass, as
				// the editor's retry does.
				if len(refusals) == 1 {
					t.Logf("pass %d: the budget refused room %d's paste %d, its first refusal, with %d rooms whose browsers went; %d MiB resident", pass, index, took[index]+1, len(left), server.resident(t, "VmRSS")>>20)
					if len(left) != rooms/4 {
						t.Fatalf("the budget refused growth with %d of the %d rooms whose every fourth browser leaves left, want it to refuse none before they have", len(left), rooms/4)
					}
				}
			}
			taken := 0
			for _, count := range took {
				taken += count
			}
			t.Logf("pass %d: the rooms have taken %d pastes, %d refused so far; %d MiB resident", pass, taken, len(refusals), server.resident(t, "VmRSS")>>20)
			if moved {
				idle = false
				continue
			}
			if idle {
				break
			}
			// Nothing of a paste is outstanding once its room has answered it, so the only wait
			// left before the last pass is the sweep's: a room whose browsers went is closable
			// only once its settlement has committed (unusedLocked), and the pass below reads
			// what the budget did with it.
			idle = true
			time.Sleep(5 * time.Second)
		}
		if len(refusals) == 0 {
			t.Fatalf("every room took all %d pastes, want the budget to refuse growth once the held rooms pass it", len(rounds))
		}
		_, err := server.tryOpenBrowser(copies[rooms])
		var closed *gws.CloseError
		if !errors.As(err, &closed) {
			t.Fatalf("the next browser, once the held rooms passed the budget: %v, want its socket closed", err)
		}
		requireClose(t, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, "the next browser, once the held rooms passed the budget")
		requireRoomAbsent(t, server, copies[rooms], "the next browser's document")
		for index := range copies[:rooms] {
			if gone[index] {
				continue
			}
			asking := server.openBrowser(t, copies[index])
			applied, closed, err := asking.send(asking.updateFrame(probe))
			if err != nil {
				t.Fatalf("room %d: %v", index, err)
			}
			requireRefused(t, applied, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
		}
		for index := range copies[:rooms] {
			if !gone[index] {
				requireApplied(t, holders[index], shrink, fmt.Sprintf("room %d's delete", index))
			}
		}
	})
	early := 0
	for _, refused := range refusals {
		if refused.left == 0 {
			early++
		}
	}
	t.Logf("%d pastes refused, the first on pass %d to room %d after it took %d, %d of them before any browser left; the next browser and every held room's insert refused, every held room's delete applied",
		len(refusals), refusals[0].pass, refusals[0].room, refusals[0].took, early)
	requireBudgetRefusals(t, server)
	// What the sweep said of every charged room it left open, and for how long after its
	// browsers went: the architect's reading of why a room whose browsers have gone is not
	// closable (sweepHoldsAfter). It changes nothing the drive asserts.
	sweepHoldsAfter(t, server, left, leftAt)
	// Once the growth has ended, the budget holds no charge for any room whose browsers went:
	// that, and not a line in the log, is what "the budget closed it" means. The close is read
	// from the budget itself (roomCharges), because a departure closes a room by its own path -
	// the handler's unlockState, which closes a room opened over the budget - as well as by a
	// sweep, and only the sweep's path logs a close. Measured 2026-11-06: at the first refusal
	// and at the end of the growth the budget carried none of the ten rooms, while the sweep
	// logged no close of any of them. It is read where the teardown window is behind rather
	// than at the instant of a refusal (above), which the peer's read loop was never going to
	// give.
	carrying, used, limit := server.roomCharges(t)
	for _, room := range left {
		if weight, carried := carrying[room]; carried {
			t.Errorf("the budget still charges room %s, whose browsers went, %d bytes of its %d used of %d, want no charge for a room nothing holds:\n%s",
				room, weight, used, limit, server.output.tail())
		}
	}
	t.Logf("once the growth had ended the budget carried none of the %d rooms whose browsers went, charging %d of %d bytes over the %d rooms it still carries",
		len(left), used, limit, len(carrying))
	if len(left) != rooms/4 {
		t.Errorf("%d browsers left, want the %d rooms of every fourth browser", len(left), rooms/4)
	}
	for _, room := range left {
		requireRoomAbsent(t, server, room, "a room whose browsers went, once the growth had ended")
	}
	for index, room := range copies[:rooms] {
		if gone[index] {
			continue
		}
		text := server.text(t, room)
		if got := strings.Count(text, pasted); got != took[index]-1 {
			t.Errorf("room %d reads %d pastes after taking %d and the delete of the first's first half, want %d: a refused paste reached it, or a taken one or the delete did not", index, got, took[index], took[index]-1)
		}
		if strings.Contains(text, probeText) {
			t.Errorf("room %d reads the probe its room refused", index)
		}
	}
	requireUnderThePod(t, peak, "the growing rooms")
}

// holdsMark is whether tree carries the mark ref names. pmdoc.ListMarks lists each mark once with
// the author its first run names, which these checks do not read.
func holdsMark(tree *pmdoc.Node, ref pmdoc.MarkRef) bool {
	return slices.ContainsFunc(pmdoc.ListMarks(tree), func(mark pmdoc.Marked) bool { return mark.MarkRef == ref })
}

// A server whose open documents fill the real budget - no test hook shrinks it - with every room
// held by a socket that stays, so none can be closed, answers four things as the budget says: the
// next browser is closed with 4503 and DOC_BUSY at its socket's upgrade, before any room is made
// for it; an update that would grow a document already open is refused the same way, and the
// document never gains it; so is one that adds elements and leaves the document no larger in bytes,
// which a test of bytes would let through; and a hundred updates that shrink documents are applied
// and stored. Meanwhile an API write to a document no room holds is taken,
// and the unrecorded-mark sweep writes, in a room a browser holds and in one no room held when the
// budget filled: each such room the server's own work opened over the budget is closed once its
// settlement has ended (closeIfOpenedOverBudget), read from its absence (roomLive) and
// from the line the server logs for the close. The memory a pod's kernel would charge the server
// stays under the pod's throughout, and what its Go memory limit bounds within that limit
// (requireUnderThePod).
//
// Why the shrinks apply where everything else is refused: while the budget is full the allowance
// the hook builds for a browser's update lets it integrate no item of its own and no byte of
// content (docs' peerApplyAllowance, rulePressure), and a delete integrates neither. The splits
// a delete makes in the items the document already held are not the update's own and are not
// counted against it (crdt.ApplyBudget.Structs), so a cut that leaves a document shorter applies
// where an insert of one character does not. Nothing is held in reserve for those deletes: the
// pressure reserve the budget kept past its limit for them went with the cut, and what a delete
// adds to its room is the bytes of the update that carried it, at the one price.
//
// The budget is filled the way the server reaches a full one. A room is charged its stored state
// at the one price when its load decodes it and an admission reserves nothing of its own, so the
// room that crosses the limit is the last one admitted: browsers open documents of a mebibyte
// until the budget refuses one, which leaves it just past its limit or just under. Then small
// rooms opened first grow under their browsers, a round of headings at a time, until what the
// rounds' own bytes add to their rooms' charges (docs' renderedReplica.chargeAdded) takes the
// charges past the limit, a sweep finds nothing to close, and the budget is full
// (roomBudget.full): the next round is refused.
//
// Each refusal is read twice: an update's from its close frame and from its document, through the
// server's text and through the store, never holding it; a browser's from its close frame and from
// its document's absence (roomLive).
func TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks(t *testing.T) {
	// proofComment is the mark a comment anchors with (docs.MarkComment).
	const growers, markID, unheldMarkID, commentMark = 4, "memory-unrecorded-mark", "memory-unrecorded-mark-unheld", "proofComment"
	// A hundred shrinks are one browser's five cuts of fifty bytes each, sent to twenty
	// documents in the order it made them: ygo's update observer encodes a transaction's structs
	// beside what that transaction deleted (sjawhar/ygo v1.55.1-sami.1, collectAndEncodeLocked,
	// crdt/doc.go:844-873, reearth/ygo#350), so each carries its own cut alone, where at
	// v1.54.1-sami.3 each carried the cuts before it too, which a document that has taken them
	// holds.
	//
	// Twenty documents and not twenty-five because the one price holds fewer rooms of a mebibyte
	// than the six prices did: the budget admits twenty-six, and the drive needs four of them
	// for the writes it refuses and the mark it leaves. The hundred is the acceptance's own
	// figure and is kept whole by cutting five times in each.
	const shrunk, cutsEach, cutBytes = 20, 5, 50
	// markAuthor is the reader whose abandoned composer left both unrecorded marks behind, and is
	// nobody this drive connects as: the sweep keeps an unrecorded mark whose author still has a
	// browser on its room, the composer still open in front of them
	// (sweepUnrecordedMarks), and every browser here connects as alice
	// (memoryServer.openBrowser), one of them holding the room of the
	// first mark until the drive ends, so a mark of alice's would never age.
	const markAuthor = "user:bob"
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	// The budget admits what it holds and one more, the room that crosses the limit, since an
	// admission reserves nothing for the room it is about to load; the drive needs one past that
	// for the refusal, and three it keeps for the writes at the end.
	fleet := memory.mebibyteFleet(t, func(weight int64) int { return int(roomBudgetBytesForTest/weight) + 6 })
	small, smallState := memory.smallDocuments(t, growers)
	// Rounds a growing room can take while it stays under the element bound every server write
	// obeys, so its settlement keeps writing: about seven, which across four rooms is several times
	// what one mebibyte room's reservation leaves of the budget.
	rounds := growthRounds(t, smallState, func(size pmdoc.DocumentSize) bool {
		return size.Elements+4*headingsARound < pmdoc.MaxDocumentElements
	})
	// unheld is a document whose stored history holds an unrecorded comment mark and that no room of
	// the drive's server holds: a server of its own takes the mark from a browser and stores it, then
	// stops, well inside the minute after which the sweep removes an unrecorded mark
	// (sweepUnrecordedMarks), which the drive's server counts
	// from when its own settlement first finds the mark.
	unheld := fleet.copies[len(fleet.copies)-1]
	unheldMark := pmdoc.MarkRef{Type: commentMark, ID: unheldMarkID}
	marking := memory.start(t)
	setting := marking.openBrowser(t, unheld)
	requireApplied(t, setting, browserEdit(t, fleet.state, func(tree *pmdoc.Node) {
		text := paddingTexts(t, tree)[0]
		text.Marks = append(text.Marks, pmdoc.Mark{Type: commentMark, Attrs: pmdoc.Attrs{"id": unheldMarkID, "by": markAuthor}})
	}), "an unrecorded comment mark")
	setting.leave()
	memory.waitForSettled(t, unheld)
	marking.stop(t)
	if !holdsMark(newBrowserDocument(t, memory.storedState(t, unheld)).tree(t), unheldMark) {
		t.Fatalf("the document %s does not store the unrecorded mark its browser applied", unheld)
	}
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
		text.Marks = append(text.Marks, pmdoc.Mark{Type: commentMark, Attrs: pmdoc.Attrs{"id": markID, "by": markAuthor}})
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
			if next >= len(fleet.copies)-3 {
				t.Fatalf("all %d copies but the three kept for the end were admitted, want the budget to refuse one", next)
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
		// which inserts, applies. It has no comment recorded for it, and no browser of its author
		// is on the room, so the sweep removes it once it is a minute old
		// (sweepUnrecordedMarks), which is after the budget is full.
		marking := server.openBrowser(t, held[0].room)
		requireApplied(t, marking, mark, "an unrecorded comment mark on a budget that refused a browser and is within its limit")
		marking.leave()

		took := make([]int, growers)
		refusedRound := 0
		for number, round := range rounds {
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
			// A round is on its room's charge as soon as the room has answered it (docs'
			// renderedReplica.chargeAdded), and the sweep that looks for something to close runs
			// on the admission that needs it, so the next round follows at once.
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
		if last := requireBudgetRefusals(t, server); last[len(last)-1].charged+last[len(last)-1].reserved+last[len(last)-1].asks <= last[len(last)-1].limit {
			t.Errorf("the full server refused the next browser at %d charged and %d reserved of %d bytes asking %d, which fit: want a refusal the budget's arithmetic needed",
				last[len(last)-1].charged, last[len(last)-1].reserved, last[len(last)-1].limit, last[len(last)-1].asks)
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
		// What the shrinks added to the budget is their own bytes at the one price: a delete
		// carries no content, but the update that carried it is bytes the room's observer adds
		// to the room's tracked state as it drains (docs' renderedReplica.chargeAdded). Nothing
		// is held back for them - the pressure reserve the budget kept for this went with the
		// cut - and the server is full the whole way through, which is what the refusal below
		// reads.
		if _, err := server.tryOpenBrowser(fleet.copies[next]); !errors.As(err, &closed) {
			t.Fatalf("the next browser after the shrinks: %v, want its socket closed", err)
		}
		requireClose(t, closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, "the next browser after the shrinks")
		refusals := requireBudgetRefusals(t, server)
		before, after := refusals[len(refusals)-2], refusals[len(refusals)-1]
		shrinkBytes := 0
		for _, update := range shrinking {
			shrinkBytes += len(update)
		}
		t.Logf("the budget charged %d of %d bytes before the shrinks and %d after them, %d more, against the %d their own bytes are priced at over %d documents",
			before.charged, before.limit, after.charged, after.charged-before.charged,
			roomWeightForTest(int64(shrunk*shrinkBytes)), shrunk)
		if after.charged+after.reserved+after.asks <= after.limit {
			t.Errorf("after the shrinks a browser was refused at %d charged and %d reserved of %d bytes asking %d, which fit: the server stopped being full, so what it refused above it would now admit",
				after.charged, after.reserved, after.limit, after.asks)
		}

		// An API write to a document no room holds is applied on the full server and opens no
		// room for it: the budget has no opening for one while its charges stand past the limit
		// (docs' openServerRoom), so the publish writes into no room (docs'
		// publishedWithoutARoom). The settlement the write leaves owed is the one that cannot do
		// without a room, so it waits for the budget's one opening whatever the charges read
		// (docs' awaitServerRoom), warms the room over the full budget, and the budget closes it
		// once that settlement has ended.
		written := fleet.copies[next+1]
		for _, room := range []string{written, unheld} {
			if server.roomLive(t, room) {
				t.Fatalf("document %s is live before the API write, want one no room holds", room)
			}
		}
		if answer := server.edit(t, written, map[string]any{"op": "replace", "find": "Paragraph 3 holds", "with": "Paragraph 3 now holds"}); answer.status != http.StatusOK {
			t.Errorf("an API write to a document no room holds, on the full server: %d %.300s, want 200", answer.status, answer.body)
		} else if !strings.Contains(server.text(t, written), "Paragraph 3 now holds") {
			t.Error("an API write on the full server answered 200, and the document does not read it")
		}
		requireClosedOnceSettled(t, memory, server, written, "the room an API write's settlement opened on the full server")
		// So does one to the document whose stored mark no room has seen: its settlement finds the
		// mark, which the sweep then removes a minute later, in a room the server opens for it.
		if answer := server.edit(t, unheld, map[string]any{"op": "replace", "find": "Paragraph 5 holds", "with": "Paragraph 5 now holds"}); answer.status != http.StatusOK {
			t.Errorf("an API write to the unheld document, on the full server: %d %.300s, want 200", answer.status, answer.body)
		}

		markRef := pmdoc.MarkRef{Type: commentMark, ID: markID}
		deadline := time.Now().Add(3 * time.Minute)
		for holdsMark(newBrowserDocument(t, held[0].state(t)).tree(t), markRef) {
			if time.Now().After(deadline) {
				t.Fatalf("the unrecorded mark %s was still in its document three minutes after it was made, want the sweep to remove it", markID)
			}
			time.Sleep(5 * time.Second)
		}
		memory.waitForStored(t, held[0].room, "the unrecorded mark's removal", func(state []byte) bool {
			return !holdsMark(newBrowserDocument(t, state).tree(t), markRef)
		})
		requireBusyAndUnchanged(t, memory, server, held[len(held)-3].room, grow, "an update that grows a document once the mark sweep has written")
		t.Log("the sweep removed the unrecorded mark, and the server still refuses growth")
		memory.waitForStored(t, unheld, "the sweep's removal of the unrecorded mark no room held", func(state []byte) bool {
			return !holdsMark(newBrowserDocument(t, state).tree(t), unheldMark)
		})
		requireClosedOnceSettled(t, memory, server, unheld, "the room the mark sweep wrote in on the full server")
		t.Log("the sweep removed the unrecorded mark from the document no room held, and its room was closed once its settlement ended")
	})
	requireUnderThePod(t, peak, "the full server")
}

// gcFloodClient is the client of the first GC-struct update the A1 drives send, each later update
// of a drive a client of its own above it, none of them a client any stored document holds.
const gcFloodClient = 5_000_000

// A browser cannot push a document past the decode cap, over a real document socket of a real
// server. A room is brought near the cap by an update of GC structs its allowance admits, and
// one that would take it one struct past - sized from the structs the room's own document holds,
// which is what the apply's allowance is built from (crdt.Doc.StructCount, docs'
// peerApplyAllowance) - is refused with 4413 and DOC_TOO_LARGE and taken back out whole, leaving
// the room as it was. The frame that takes the room to the cap exactly is admitted, and so is a
// frame of delete ranges, which integrates nothing: the splits a delete makes in items the
// document already held are not the update's own and are not counted against it
// (crdt.ApplyBudget.Structs), so a document at the cap can still be trimmed. The stored document
// then loads in a new room of a new server, within the cap.
//
// Before the cut the room kept a running struct total of its own and charged each struct of an
// update with the two splits it could cause, which is why a frame of a third of the cap's
// structs was refused where this one takes it; ygo counts what the apply integrates now, so the
// drive asks for the exact frame the cap refuses. internal/dispatch/docs holds the same cap in
// process (TestAnUpdateThatWouldTakeTheRoomPastTheDecodeCapIsRefused); what this drive adds is
// that the room a real socket drove is left byte for byte as it was and that its stored history
// loads again.
func TestABrowserCannotPushADocumentPastTheDecodeCap(t *testing.T) {
	const nearStructs = 340_000
	memory := newMemoryHarness(t)
	server := memory.start(t)
	stored := server.upload(t, "json", memory.issue, "# Capped\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	filling := server.openBrowser(t, room)
	t.Cleanup(filling.leave)
	requireApplied(t, filling, gcFrame(gcFloodClient, nearStructs, 1), fmt.Sprintf("an update of %d GC structs", nearStructs))
	before := canonicalState(t, filling.state(t))
	held, err := docs.StateStructsForTest(before)
	if err != nil {
		t.Fatalf("count the room's structs: %v", err)
	}
	left := int(maxUpdateItemsForTest - held)
	t.Logf("the room holds %d structs after an update of %d GC structs, %d short of the %d-struct decode cap", held, nearStructs, left, maxUpdateItemsForTest)
	if left < 2 {
		t.Fatalf("the room is %d structs short of the cap, want room for the frame that reaches it and the one that passes it", left)
	}
	refusing := server.openBrowser(t, room)
	t.Cleanup(refusing.leave)
	past := gcFrame(gcFloodClient+1, left+1, 1)
	applied, closed, err := refusing.send(refusing.updateFrame(past))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a %d-byte update of %d GC structs, one past the cap: applied %v, closed %v", len(past), left+1, applied, closed)
	requireRefused(t, applied, closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	if state := canonicalState(t, filling.state(t)); !bytes.Equal(state, before) {
		t.Fatalf("the room holds a %d-byte state after the refused update, want the %d-byte state it held", len(state), len(before))
	}
	// What the cap admits: the frame that reaches it exactly, and a frame of delete ranges,
	// which integrates nothing of its own whatever the room already holds.
	requireApplied(t, filling, gcFrame(gcFloodClient+2, left, 1), fmt.Sprintf("an update of %d GC structs, to the cap exactly", left))
	requireApplied(t, filling, deleteRangesFrame(gcFloodClient+3, 64), "an update of 64 delete ranges into a room at the cap")
	atTheCap := canonicalState(t, filling.state(t))
	capped, err := docs.StateStructsForTest(atTheCap)
	if err != nil {
		t.Fatalf("count the room's structs at the cap: %v", err)
	}
	t.Logf("the room holds %d structs once the frame that reaches the cap and a frame of delete ranges have applied", capped)
	if capped > maxUpdateItemsForTest {
		t.Fatalf("the room holds %d structs, past the %d-struct cap: the allowance admitted more than what was left of it", capped, maxUpdateItemsForTest)
	}
	memory.waitForSettled(t, room)
	server.stop(t)
	again := memory.start(t)
	reopened := again.openBrowser(t, room)
	t.Cleanup(reopened.leave)
	state := canonicalState(t, reopened.state(t))
	loaded, err := docs.StateStructsForTest(state)
	if err != nil {
		t.Fatalf("count the reloaded room's structs: %v", err)
	}
	t.Logf("a new server's room loads the stored document as a %d-byte state of %d structs", len(state), loaded)
	if !bytes.Equal(state, atTheCap) || loaded > maxUpdateItemsForTest {
		t.Fatalf("a new room loads a %d-byte state of %d structs, want the %d-byte state the room held, within the %d-struct cap", len(state), loaded, len(atTheCap), maxUpdateItemsForTest)
	}
}

// reconnectRounds is how many times the refused browser reconnects and sends the refused frame
// again in the drive below, and reconnectRoundsMargin how much more the server may hold after
// the last round than after the first: 16 MiB, above the garbage a round's socket, its sync of
// the room's state and its refusal leave until the collector runs, and far under what nineteen
// rounds would add if a refusal kept anything of the frame or of the browser it closed.
// Measured on 2026-10-31, the runtime held 13,007,112 bytes after the first round and
// 13,736,200 after the twentieth, 729,088 bytes more.
const (
	reconnectRounds       = 20
	reconnectRoundsMargin = 16 << 20
)

// An update the apply refuses is not applied, over a real document socket of a real server: the
// room closes the browser that sent it with 4413 DOC_UPDATE_REFUSED, read from the close frame,
// and the room's state vector and state are what they were, for a frame of valid GC structs
// ending in one bad byte, which ygo's own apply refuses, leaving the document holding none of it
// (sjawhar/ygo v1.55.1-sami.6, crdt/update.go:822-996, reearth/ygo#340), for a subdocument, which
// ygo refuses as bytes it will not read, and for a struct under a root the Proof editor never
// writes, which ygo refuses by name as it decodes it (crdt.WithAllowedRoots, docs'
// peerAllowedRoots). Only that browser is closed: another reading the room goes on reading it.
// The stored document, once settled, holds none of them.
//
// The pre-apply walk that refused these three before the cut is gone; what refuses them now is
// the apply itself, which rolls back whole, and internal/dispatch/docs holds each refusal in
// process (TestAPeerUpdateYgoRefusesIsNeverApplied). What this drive adds is the loop the code
// is there to end: the browser that was refused reconnects and sends the same frame again,
// twenty rounds of it, as @hocuspocus/provider 2.x does after a close it does not read as
// permanent. Each round is closed with the same code and reason, the room's state is what it was
// after every one, and the server holds no more memory at the end than at the start, read from
// its own trace (heldNow), so a client that does not read the close costs the pod nothing that
// accumulates.
//
// Under sjawhar/ygo v1.51.3-sami.7 the room answered such a frame SyncStatus(0) and kept the
// browser connected. Upstream reearth/ygo#304 was merged in the form that closes a browser whose
// update the hook refused, with 4205 "Reset Connection" for a refusal that
// is no *websocket.PeerClose, which the provider reconnects at once after
// (provider/websocket/peer_update.go:38-52, :139-151): every refusal the hook makes carries a
// code and reason of Dispatch's own instead.
func TestAnUpdateTheApplyRefusesIsNotApplied(t *testing.T) {
	const client = gcFloodClient
	memory := newMemoryHarness(t)
	server := memory.start(t)
	stored := server.upload(t, "json", memory.issue, "# Walked\n\nOne paragraph of ordinary prose.\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	// The walk's refusal closes the browser that sent the frame, so each row sends from a browser
	// of its own and this one only reads.
	reader := server.openBrowser(t, room)
	t.Cleanup(reader.leave)
	before := reader.state(t)
	vector := newBrowserDocument(t, before).doc.StateVector()
	badByte := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(4)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(0)
		for range 3 {
			encoder.WriteUint8(0)
			encoder.WriteVarUint(2)
		}
		encoder.WriteUint8(0xff)
	})
	if err := crdt.ApplyUpdateV1(newBrowserDocument(t, before).doc, badByte, nil); err == nil {
		t.Fatal("ygo's own apply took the frame ending in a bad byte, want it refused")
	}
	// ygo refuses the frame and leaves the document holding none of it (reearth/ygo#340): what
	// the walk keeps off the room is the decode and the integration of the structs before the
	// bad byte, and the rollback after them, not a document left holding them.
	refused := newBrowserDocument(t, before).doc
	_ = crdt.ApplyUpdateV1(refused, badByte, nil)
	if refused.StateVector()[crdt.ClientID(client)] != 0 {
		t.Fatalf("ygo's own apply of the frame ending in a bad byte left client %d at clock %d, want a document holding none of it", client, refused.StateVector()[crdt.ClientID(client)])
	}
	// rooted is one struct of client's at clock 0: content of reference tag directly under root, as
	// the entry key of a map when key is not empty (info bit 0x20).
	rooted := func(tag byte, root, key string, content func(encoder *encoding.Encoder)) []byte {
		return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
			encoder.WriteVarUint(1)
			encoder.WriteVarUint(1)
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(0)
			if key == "" {
				encoder.WriteRaw([]byte{tag, 1})
				encoder.WriteVarString(root)
			} else {
				encoder.WriteRaw([]byte{tag | 0x20, 1})
				encoder.WriteVarString(root)
				encoder.WriteVarString(key)
			}
			content(encoder)
			encoder.WriteVarUint(0)
		})
	}
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"three valid GC structs and then one bad byte", badByte},
		// A subdocument (content reference 9) as an entry of the margin (marksMapName):
		// its guid and its options.
		{"a subdocument", rooted(9, "marks", "sub", func(encoder *encoding.Encoder) {
			encoder.WriteVarString("guid")
			encoder.WriteAny(map[string]any{"autoLoad": false})
		})},
		// A string (content reference 4) under a root named t.
		{"a struct under an unknown root", rooted(4, "t", "", func(encoder *encoding.Encoder) { encoder.WriteVarString("hi") })},
	} {
		sending := server.openBrowser(t, room)
		applied, closed, err := sending.send(sending.updateFrame(row.update))
		t.Logf("%s, %d bytes: applied %v, closed %v, %v", row.name, len(row.update), applied, closed, err)
		if err != nil || applied || closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentUpdateRefusedCloseReason {
			t.Fatalf("%s: applied %v, closed %v, %v; want the frame refused and that browser closed %d %q",
				row.name, applied, closed, err, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
		}
		sending.leave()
		if err := reader.ping(); err != nil {
			t.Fatalf("%s: the room closed the browser that only reads it: %v", row.name, err)
		}
		after := reader.state(t)
		if got := newBrowserDocument(t, after).doc.StateVector(); !maps.Equal(got, vector) || !bytes.Equal(after, before) {
			t.Fatalf("%s: the room's state vector is %v and its state %d bytes, want %v and the %d bytes it held", row.name, got, len(after), vector, len(before))
		}
	}
	// The loop a client that does not read the close makes: the same frame again on a socket of
	// its own, round after round, each one closed the same way with the room's state unchanged
	// after it. What the runtime holds after the last round is within reconnectRoundsMargin of
	// what it held after the first, read from the server's own trace (heldNow, tracePeaks): a
	// client that reconnects and resends for ever costs the pod nothing that accumulates.
	resend := func(round int) {
		sending := server.openBrowser(t, room)
		applied, closed, err := sending.send(sending.updateFrame(badByte))
		if err != nil || applied || closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentUpdateRefusedCloseReason {
			t.Fatalf("round %d of the reconnect loop: applied %v, closed %v, %v; want the frame refused and that browser closed %d %q",
				round, applied, closed, err, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
		}
		sending.leave()
		if after := reader.state(t); !bytes.Equal(after, before) {
			t.Fatalf("round %d of the reconnect loop left the room a %d-byte state, want the %d bytes it held", round, len(after), len(before))
		}
	}
	resend(1)
	afterFirst := heldNow(t, server, "after the first refused round")
	for round := 2; round <= reconnectRounds; round++ {
		resend(round)
	}
	afterRounds := heldNow(t, server, fmt.Sprintf("after all %d refused rounds", reconnectRounds))
	t.Logf("%d reconnect-and-resend rounds: the runtime holds %d bytes after the last, against %d after the first (%+d), within the %d-byte margin",
		reconnectRounds, afterRounds, afterFirst, afterRounds-afterFirst, int64(reconnectRoundsMargin))
	if afterRounds > afterFirst+reconnectRoundsMargin {
		t.Fatalf("the %d rounds left the runtime holding %d bytes, past the %d it held after the first by more than the %d-byte margin",
			reconnectRounds, afterRounds, afterFirst, int64(reconnectRoundsMargin))
	}
	memory.waitForSettled(t, room)
	if storedState, want := canonicalState(t, memory.storedState(t, room)), canonicalState(t, before); !bytes.Equal(storedState, want) {
		t.Fatalf("the stored document is a %d-byte state, want the room's %d-byte state from before the refused updates", len(storedState), len(want))
	}
}

// gcFrame is one client's update of n GC runs of length clocks each, from clock 0, with an empty
// delete set, written byte for byte as Yjs writes a GC struct: its info byte 0, then its length.
func gcFrame(client uint64, n int, length uint64) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(uint64(n))
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(0)
		for range n {
			encoder.WriteUint8(0)
			encoder.WriteVarUint(length)
		}
		encoder.WriteVarUint(0)
	})
}

// deleteRangesFrame is an update of no structs whose delete set holds n ranges of one clock each of
// client's, at the odd clocks from 1 up, so none starts at a client's first clock or touches the
// next.
func deleteRangesFrame(client uint64, n int) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(0)
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(uint64(n))
		for index := range n {
			encoder.WriteVarUint(uint64(2*index + 1))
			encoder.WriteVarUint(1)
		}
	})
}

// gcRunFrame is one client's update of a single GC run of length clocks starting at clock, with
// an empty delete set: gcFrame's struct at a clock of the caller's choosing, so the run can be
// appended past the clocks a room holds of that client rather than skipped as clocks it holds
// already (sjawhar/ygo v1.55.1-sami.6, integrateGroup steps over a struct ending at or before
// the client's clock, crdt/update.go:899-917, and appends one past them without integrating it,
// :938-949).
func gcRunFrame(client, clock, length uint64) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteUint8(0)
		encoder.WriteVarUint(length)
		encoder.WriteVarUint(0)
	})
}

const (
	// hugeSpanAnsweredWithin is the bound the drive below holds the frame's answer to. A clock
	// length in a V1 update is a varuint, so a frame of a few bytes can claim 2^40 clocks: a
	// second is three orders of magnitude past the milliseconds a frame of one struct costs,
	// and three orders under what any work in the clocks the frame claims would take - 2^40
	// clocks at a nanosecond each is 18 minutes. The second is not a figure to relax: a room
	// that misses it is doing work in the clocks a frame claimed rather than in the bytes it
	// carried, which is the whole of what this drive is here to refuse.
	hugeSpanAnsweredWithin = time.Second
	// hugeSpanEditWithin is the bound it holds the keystroke after the frame and the reconnect
	// after that to: ten seconds, which each of them is three orders of magnitude under on this
	// pod and which no path linear in a claimed span could meet.
	hugeSpanEditWithin = 10 * time.Second
	// hugeSpanNewClient is the client of the frame below, above every client a stored document
	// holds and above the 32 bits a browser's own id is drawn from (crdt.New).
	hugeSpanNewClient = 1<<40 + 211
)

// A frame costs the server what its bytes carry, never what its clocks claim, over real document
// sockets of a real server: a GC run of 2^40 clocks for a client the room has never seen, which
// a browser writes in a handful of bytes because a clock length in a V1 update is a varuint.
// A hostile browser can send it today, so what it meets is observable behaviour whatever answers
// it; what changed with INFRA-179 is only which code does. The pre-apply walk that read a struct
// and a delete range at a time, and the running totals it fed, are gone: the frame now goes
// straight into ygo's bounded apply, where the hook's allowance counts what the apply decodes,
// integrates and adds (crdt.ApplyBudget, docs' peerApplyAllowance) and nothing counts clocks.
// ygo integrates a GC struct by appending it, never over its clocks (crdt/update.go:938-949), so
// the apply costs the frame's bytes and the allowance's three comparisons.
//
// What the drive holds, against the room that took the frame:
//
//   - the frame is answered within hugeSpanAnsweredWithin, admitted or refused;
//   - the answer is named here rather than left open: the frame carries one struct, no content
//     and no root a browser may not name, so the allowance admits it and the socket stays open;
//   - the room goes on serving its other browsers: the browser that only reads it answers a
//     ping, a second browser's keystroke applies within hugeSpanEditWithin, the document's text
//     carries that keystroke, and a browser reconnecting is given the whole document;
//   - the pod's heap does not jump, by both figures every drive of this file is weighed by
//     (requireUnderThePod): what a pod's kernel would charge, under the pod's 4 GiB, and what
//     the Go memory limit bounds, within that limit and its margin.
//
// Then the stored history: the room settles what it took, the server is restarted so no room is
// resident, and the document is loaded again and takes a keystroke. A load that walked the
// clocks a stored GC run claims could not answer at all.
//
// Measured on 2026-11-06, the first run of this drive after the cut: the 17-byte frame was
// applied with the socket left open and answered in 1.383 ms, three orders of magnitude inside
// the second the architect set; the keystroke after it applied in 1.367 ms, a reconnect read
// the room's 2,288-byte state in 12.92 ms, and after the restart the stored history loaded in
// 47.385 ms and took a keystroke in 1.026 ms.
func TestAFrameClaimingHugeClockSpansCostsOnlyItsBytes(t *testing.T) {
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	stored := server.upload(t, "json", memory.issue, "# Spans\n\n"+strings.Repeat("word ", 400)+"\n")
	if stored.status != http.StatusCreated {
		t.Fatalf("upload: status %d body %.300s, want 201", stored.status, stored.body)
	}
	memory.waitForSettlement(t, stored)
	room := stored.artifactID
	frame := gcRunFrame(hugeSpanNewClient, 0, 1<<40)

	var answered, typed, resynced time.Duration
	var applied bool
	var closed *gws.CloseError
	var state []byte
	peak := server.peakResident(t, func() {
		// A browser that only reads the room, which the frame must leave alone.
		reader := server.openBrowser(t, room)
		defer reader.leave()
		sending := server.openBrowser(t, room)
		defer sending.leave()
		start := time.Now()
		var err error
		applied, closed, err = sending.send(sending.updateFrame(frame))
		answered = time.Since(start)
		if err != nil {
			t.Fatalf("the %d-byte frame claiming 2^40 clocks was answered %v, want the room to answer it", len(frame), err)
		}
		if err := reader.ping(); err != nil {
			t.Fatalf("the room stopped answering the browser that only reads it: %v", err)
		}
		typed = typedThrough(t, server, room, " typed-after-the-frame")
		resynced, state = resyncedThrough(t, server, room)
		// The room's settlement falls inside the step too. It is the heaviest thing the frame
		// leaves the server to do, and it is what makes the step long enough for the trace's
		// 50 ms samples to fall inside it: the socket work above is over in a few milliseconds,
		// and a step the trace holds no sample of is read neither way (requireUnderThePod).
		memory.waitForSettled(t, room)
	})
	t.Logf("a %d-byte frame claiming 2^40 clocks of a client the room has never seen: applied %v, closed %v, answered in %s; the keystroke after it applied in %s and a reconnect read the room's %d-byte state in %s",
		len(frame), applied, closed, answered.Round(time.Microsecond), typed.Round(time.Microsecond), len(state), resynced.Round(time.Microsecond))
	if answered > hugeSpanAnsweredWithin {
		t.Fatalf("the room took %s to answer a %d-byte frame claiming 2^40 clocks, past the %s a frame costing only its bytes takes: something on the path is walking the clocks the frame claimed",
			answered, len(frame), hugeSpanAnsweredWithin)
	}
	if !applied || closed != nil {
		t.Fatalf("the frame was answered applied %v, closed %v; want it applied and the socket open: it carries one struct, no content and no root a browser may not name, so the apply's allowance admits it",
			applied, closed)
	}
	if typed > hugeSpanEditWithin || resynced > hugeSpanEditWithin {
		t.Fatalf("after the frame the keystroke took %s and the reconnect %s, past the %s each takes in a room no frame has made slow", typed, resynced, hugeSpanEditWithin)
	}
	if text := server.text(t, room); !strings.Contains(text, "typed-after-the-frame") {
		t.Fatalf("the document's text is %d bytes and carries none of the keystroke after the frame", len(text))
	}
	requireUnderThePod(t, peak, "the frame claiming 2^40 clocks")

	memory.waitForSettled(t, room)
	server.stop(t)
	restarted := memory.start(t)
	requireRoomAbsent(t, restarted, room, "the room the frame was sent to")
	load, reloaded := resyncedThrough(t, restarted, room)
	after := typedThrough(t, restarted, room, " typed-after-the-reload")
	t.Logf("after the restart the room loaded its %d-byte stored history in %s and took a keystroke in %s",
		len(reloaded), load.Round(time.Microsecond), after.Round(time.Microsecond))
	if load > hugeSpanEditWithin || after > hugeSpanEditWithin {
		t.Fatalf("after the restart the room loaded in %s and took a keystroke in %s, past the %s each takes for a stored history no frame has made slow", load, after, hugeSpanEditWithin)
	}
}

// typedThrough sends one browser's keystroke into the room over a socket of its own, appending
// typed to the document's first paragraph of plain words, and returns how long the room took to
// answer it applied.
func typedThrough(t *testing.T, server *memoryServer, room, typed string) time.Duration {
	t.Helper()
	typist := server.openBrowser(t, room)
	defer typist.leave()
	keystroke := browserEdit(t, typist.state(t), func(tree *pmdoc.Node) {
		paddingTexts(t, tree)[0].Text += typed
	})
	start := time.Now()
	applied, closed, err := typist.send(typist.updateFrame(keystroke))
	took := time.Since(start)
	if err != nil || !applied || closed != nil {
		t.Fatalf("the keystroke into %s was answered applied %v, closed %v, %v; want it applied and the socket open", room, applied, closed, err)
	}
	return took
}

// resyncedThrough opens a browser of its own on the room and asks it for the whole document, as a
// browser reconnecting does, and returns how long the two took with the state it was given.
func resyncedThrough(t *testing.T, server *memoryServer, room string) (time.Duration, []byte) {
	t.Helper()
	start := time.Now()
	browser := server.openBrowser(t, room)
	defer browser.leave()
	state := browser.state(t)
	return time.Since(start), state
}

const (
	// maxUpdateItemsForTest mirrors docs' maxUpdateItems, the
	// decode cap: the most structs a stored document's state may hold. The struct-dense drives
	// build documents at it and fail unless their stored histories hold within 4,096 of it
	// (requireAtTheDecodeCap).
	maxUpdateItemsForTest = 1 << 20
	// worstCaseLoadSlotBytesForTest is what the pod's load concurrency may hold at once:
	// loadSlotsForTest slots of loadSlotBytesForTest each, the sum the heaviest-apply drive adds
	// to the room budget and the cold reads' cache to weigh its worst case against the Go limit.
	// One slot covers one whole-document decode, one live write's fork or one apply of an update
	// over unslottedApplyBytesForTest, whichever takes it.
	worstCaseLoadSlotBytesForTest = loadSlotsForTest * loadSlotBytesForTest
)

// longEditFixturePath is the long-edited fixture (docs' longEditFixture),
// the 5 KiB document a million keystrokes left, from this package's directory.
const longEditFixturePath = "../../internal/dispatch/docs/testdata/long_edited_document.update.gz"

// A burst of browsers opening struct-dense documents that render little, all at once, under the
// Go limit a 4 GiB pod gives the server and at the runtime's own collector ratio, which the
// server leaves alone, keeps the memory a pod's kernel would charge it under the pod's 4 GiB and
// what the Go limit bounds within that limit (requireUnderThePod): 3,675 MiB charged on
// 2026-11-03 under sjawhar/ygo v1.54.1-sami.3, of 3,692 MiB of VmRSS, with 3,809,254,920 bytes
// held of what the 3,865,470,566-byte limit bounds, 54 MiB under it, in a drive of 287 s; the
// run of 2026-11-01 charged 3,658 MiB, of 3,665 MiB of VmRSS, with 3,808,812,536 bytes held. A
// run of the same drive on 2026-10-29 charged 3,584 MiB, of 3,604 MiB of VmRSS, with
// 3,722,661,352 bytes held, 136 MiB under the limit, another that day 3,708 MiB and 14 MiB under
// it, and one before the held reading was asserted read
// 1,832,882 bytes past it (podGoMemoryLimitMargin). The 2,660 to 3,727 MiB earlier runs recorded
// were VmRSS alone, which on this sandbox counts the pages the runtime has released as well
// (peakResident). The burst is eight copies each
// of the two struct-dense shapes a room keeps - the long-edited 5 KiB document typed on until
// its state stands at the decode cap
// (longEditedTypedTo), and the document an agent rewrote whole through the API until the decode
// cap refused a write (agentRewrittenDocument), about a million structs each - and eight documents
// of a mebibyte of prose, the ordinary document A3 weighs
// (TestTheRoomBudgetRefusesWhereItsOwnArithmeticSays). Every room is charged its stored state at
// one price over both copies (roomWeightForTest), and a struct-dense document's state is several
// times a mebibyte of prose's, so the budget admits few of them and the ordinary ones what is
// left.
//
// Every refusal is 4503 and DOC_BUSY at admission, with the document left unopened, and within
// the budget's arithmetic: the line the server logs for it names what the open rooms were
// charged against the budget, and that charge must already be past the limit, since an
// admission reserves nothing for the room it is about to load and is refused only once what the
// open rooms hold passes it. Every admitted browser receives its document's whole state and
// answers a ping.
func TestABurstOfStructDenseDocumentsStaysUnderThePodsMemory(t *testing.T) {
	const copiesOfEach, ordinary = 8, "a mebibyte of prose"
	building := time.Now()
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	longEdited := memory.storedShape(t, "the long-edited document at the decode cap",
		memory.storedStates(t, longEditedTypedTo(t, maxUpdateItemsForTest))[0])
	agent := memory.storedShape(t, "the agent-rewritten document", memory.agentRewrittenDocument(t))
	fleet := memory.mebibyteFleet(t, func(int64) int { return copiesOfEach })
	type opening struct {
		shape  string
		room   string
		weight int64
		opened *browser
		closed *gws.CloseError
		err    error
	}
	var burst []opening
	for _, shape := range []storedShape{longEdited, agent} {
		requireAtTheDecodeCap(t, shape)
		for _, room := range memory.cloneDocuments(t, shape.id, copiesOfEach) {
			burst = append(burst, opening{shape: shape.name, room: room, weight: shape.weight})
		}
	}
	for _, room := range fleet.copies {
		burst = append(burst, opening{shape: ordinary, room: room, weight: fleet.weight})
	}
	t.Logf("the %d-byte budget holds %d struct-dense rooms of the heavier state and %d rooms of a mebibyte of prose, which is charged %d bytes (%d elements), as in A3; the documents took %s to build",
		roomBudgetBytesForTest, roomBudgetBytesForTest/max(longEdited.weight, agent.weight), roomBudgetBytesForTest/fleet.weight,
		fleet.weight, fleet.elements, time.Since(building).Round(time.Second))
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
	byBudget, bySlot := map[string]int{}, map[string]int{}
	var admittedWeight int64
	for index, opened := range burst {
		what := fmt.Sprintf("browser %d, of %s", index, opened.shape)
		switch {
		case opened.err != nil:
			t.Errorf("%s: %v", what, opened.err)
		case opened.opened != nil:
			admitted[opened.shape]++
			admittedWeight += opened.weight
			if err := opened.opened.ping(); err != nil {
				t.Errorf("%s was admitted and lost its room: %v", what, err)
			}
		default:
			refused[opened.shape]++
			requireClose(t, opened.closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, what)
			requireRoomAbsent(t, server, opened.room, what+"'s document")
			// Two things refuse a browser 4503 here and the drive names which. A refusal with
			// the budget's own figures is the budget's (budgetRefusalsByRoom). A refusal with no
			// such line is the pod's load concurrency: at the calibration's one slot an
			// admission behind a heavy handoff or a struct-dense load waits for a slot until its
			// own request ends and is answered then (docs' loadSlots, errWriteBusy). Both are
			// the design working, and the second is far rarer at the sixteen slots the
			// production pod's limit gives (MemoryBounds.LoadSlots).
			refusal, logged := refusals[opened.room]
			if !logged {
				bySlot[opened.shape]++
				t.Logf("%s was refused with no budget line, which is the pod's %d load slots: its admission waited for one until its own request ended", what, loadSlotsForTest)
				continue
			}
			byBudget[opened.shape]++
			t.Logf("%s was refused by the budget at %d charged and %d reserved of %d bytes, asking %d, its own room weighed at %d", what, refusal.charged, refusal.reserved, refusal.limit, refusal.asks, opened.weight)
			switch {
			case refusal.limit != roomBudgetBytesForTest:
				t.Errorf("%s was refused against a budget of %d bytes, want %d", what, refusal.limit, roomBudgetBytesForTest)
			case refusal.charged+refusal.reserved+refusal.asks <= refusal.limit:
				t.Errorf("%s was refused at %d charged and %d reserved of %d bytes asking %d, which fit: refused earlier than the budget's arithmetic allows", what, refusal.charged, refusal.reserved, refusal.limit, refusal.asks)
			}
		}
	}
	for _, shape := range []string{longEdited.name, agent.name, ordinary} {
		t.Logf("%s: %d admitted, %d refused - %d by the room budget and %d by the pod's load slots",
			shape, admitted[shape], refused[shape], byBudget[shape], bySlot[shape])
	}
	dense := admitted[longEdited.name] + admitted[agent.name]
	t.Logf("%d browsers at once: %d admitted to struct-dense rooms and %d to rooms of prose, %d bytes charged by what was admitted against the %d-byte budget, in %s",
		len(burst), dense, admitted[ordinary], admittedWeight, roomBudgetBytesForTest, took.Round(time.Second))
	logLoadSlots(t, "the burst", loadSlotHolds(t, server.output.String()))
	// Every admitted room but the last was weighed against a budget within its limit, so what
	// the budget admits is what it holds and one more, the room that crossed it.
	if most := roomBudgetBytesForTest/min(longEdited.weight, agent.weight) + 1; dense == 0 || int64(dense) > most {
		t.Errorf("%d struct-dense rooms were admitted, want at least one and no more than the %d their states' weight fits in the budget, and the one that crosses it", dense, most)
	}
	requireUnderThePod(t, peak, "the burst of struct-dense documents")
}

// The worst case the room budget and the pod's load concurrency leave the pod, run at once under
// the Go limit a 4 GiB pod gives the server: the budget full, the cold reads' cache full, honest
// handoffs of the heaviest state a room may hold applying - each far over
// unslottedApplyBytesForTest, so each takes a slot of the pod's load concurrency and holds it
// across its whole apply - and beside them struct-dense documents loading and folding, each
// decode taking a slot of its own. The calibration has one slot (loadSlotsForTest), so the three
// kinds of heavy work queue for it one at a time, and this is the sum the bounds allow: 2 GiB of
// rooms, 256 MiB of cache and one 600 MB slot, 3,015,919,104 bytes, inside the
// 3,865,470,566-byte Go limit. The memory a pod's kernel would charge the server stays under the
// pod's 4 GiB through every step, and what the Go limit bounds stays within that limit and its
// margin (requireUnderThePod), and every honest handoff, load and fold completes.
//
// What the garbage of all that work does to that memory is the collector's, at the runtime's own
// ratio: the server reads and sets no GOGC, and the boot line this drive reads at the start is
// the Go memory limit it took from its container (requirePodGoMemoryLimit). At that default the
// heap goal of a server whose budget is full is the limit itself, so the collector keeps every
// byte of garbage the limit allows - and the limit bounds exactly what a pod's kernel charges,
// what the runtime holds less what it has released. Measured on 2026-11-03 under sjawhar/ygo
// v1.54.1-sami.3 on this two-processor pod, over ten runs of 344 to 372 s, the three steps
// below charged 2,724 to 2,955, 3,608 to 3,661 and 3,670 to 3,840 MiB of the pod's 4,096, 256
// MiB under it at their worst, and the keystrokes a person typed through the two heavy steps
// were answered in 99th percentiles of 57 to 125 ms and 121 to 300 ms. Of what the Go limit
// bounds the heaviest step held 3,865,472,776 to 4,039,020,264 bytes, 2,210 to 173,549,698 past
// the limit, which is the step that reads past it: an allocation of a frame or a copy of tens
// of MiB is mapped before the cycle it triggers can hand anything back, by however much the
// collector was behind, which is what the margin bounds (podGoMemoryLimitMargin). That
// overshoot is no younger than the drive: measured the same day, the same step read 653,362
// bytes past the limit at the last commit before the merge with main's load that no longer
// replays a document's whole edit history, and 28,547,106 and 90,572,882 at the commit whose
// 2026-11-01 run this comment once quoted at 2,856, 3,620 and 3,237 MiB - figures that commit
// does not read again on this pod, where it charges 2,706 to 2,714, 3,621 to 3,624 and 3,684 to
// 3,776 MiB. VmRSS over the ten runs read 2,727 to 2,963, 3,610 to
// 3,663 and 4,279 to 4,639 MiB, each above what the pod would be
// charged: the difference
// is the pages the runtime released and gVisor keeps resident, which no unsandboxed pod is
// charged (peakResident; the GKE Autopilot deployment names no runtimeClassName or GKE Sandbox
// node pool, newDispatchKubernetes). The 4,302 and 4,424 MiB this drive once
// recorded were that same VmRSS, and holding the collector to the ratio the room budget's charge
// left it, which a change since removed, moved the charged peak no lower: 3,691 to 3,695 MiB in
// 539 and 547 s.
//
// On one server: first the budget is filled by browsers that stay - a browser of an ordinary small
// document, two rooms of the long-edited document typed on until 4,096 structs short of the decode
// cap and padded to the room's state cap by text nested under the margin
// (paddedToTheRoomsStateCap), whose browsers later hand back, then copies of the long-edited
// document at the decode cap - leaving room, by the budget's arithmetic computed here from the
// server's constants, for what typing adds to the small document's room and one more document an
// agent rewrote to the decode cap, and not two.
// Then the cache is filled by cold reads of documents of the heaviest headings the element bound
// admits, one more than its budget holds. Then, all at once, three browsers of the rooms at the
// state cap hand their whole state back with a few words typed, two browsers open two copies of
// the agent-rewritten document, of which the budget admits one and refuses the other with 4503
// within its arithmetic, and four struct-dense documents no room holds are read cold.
//
// The state cap is what bounds the heaviest handoff now that the frame total is gone: a room may
// hold maxRoomStateBytes of state and no more, a browser hands that state back as one frame, and
// four such frames fit the message floor (docs' maxRoomStateBytes). Nothing a browser sends can
// take a room there, so the drive stores the state itself, as a rebuild does (storedStates).
//
// Every one of those documents is built before the server starts and owes it no block index
// (oweNoBlockIndex), so the only reads of them are the drive's own: a server left to index them
// renders each whole within seconds of its boot, which both caches that rendering under the
// document's stored stamp, so a cold read of it is answered from the cache and folds nothing, and
// folds documents of a million structs beside the work the steps weigh. Each of the four cold
// reads is then the first read of its document, and must fold its stored history under a slot of
// the pod's load concurrency.
//
// Through the cache's fill and the heavy step a person types in the small document's room, a
// keystroke every 200 ms, each sent once the room has answered the last applied (startTyping).
// No slot holds a keystroke - a one-character insert is far under unslottedApplyBytesForTest and
// loads nothing - so every keystroke applies, in each step answered within 2 s at the 99th
// percentile and none past 5 s: the person does not wait behind the heavy work.
//
// The server logs each slot a holder waited a tenth of a second or more for (loadSlotLine), from
// which the drive reads that the handoffs and the folds did queue for the pod's one slot, so the
// worst case did happen, and the longest wait it made. It logs those beside each step's
// duration, each step's three readings of what it left the server holding (requireUnderThePod),
// and the keystrokes' latencies at their 50th, 90th and 99th percentiles and their most.
func TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory(t *testing.T) {
	const folds = 4
	building := time.Now()
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	// The padding starts from the long-edited document 4,096 structs short of the decode cap,
	// the heaviest struct-dense state this package can build: it stores 14,427,285 bytes, which
	// the room's state cap - a quarter of the 64 MiB message floor, 16,777,216 - has 2,349,931
	// bytes of room above, so the padding takes it to 16 KiB under the cap. That is the heaviest
	// state a room may hold, and so the heaviest whole-state handoff a browser of such a room
	// can send; four of those frames fit the floor, which is what the cap is derived to make
	// true.
	stored := memory.storedStates(t, longEditedTypedTo(t, maxUpdateItemsForTest),
		paddedToTheRoomsStateCap(t, longEditedTypedTo(t, maxUpdateItemsForTest-4_096)))
	longEdited := memory.storedShape(t, "the long-edited document at the decode cap", stored[0])
	heaviest := memory.storedShape(t, "the long-edited document 4,096 structs short of the decode cap, padded to the room's state cap", stored[1])
	agent := memory.storedShape(t, "the agent-rewritten document", memory.agentRewrittenDocument(t))
	requireAtTheDecodeCap(t, longEdited)
	requireAtTheDecodeCap(t, agent)
	if heaviest.stateBytes > maxRoomStateBytesForTest || heaviest.stateBytes+64<<10 < maxRoomStateBytesForTest {
		t.Fatalf("the padded document stores %d bytes of state, want within 64 KiB under the room's state cap (%d)", heaviest.stateBytes, maxRoomStateBytesForTest)
	}
	cached, cacheWeight := memory.readCacheFill(t)
	heavyRooms := memory.cloneDocuments(t, heaviest.id, 2)
	typedIDs, _ := memory.smallDocuments(t, 1)
	typed := memory.storedShape(t, "the ordinary document a person types in", typedIDs[0])
	typedGrowth := int64(typingKeystrokes * keystrokeChargeForTest)
	fill := (roomBudgetBytesForTest - 2*heaviest.weight - typed.weight - typedGrowth - agent.weight) / longEdited.weight
	free := roomBudgetBytesForTest - 2*heaviest.weight - typed.weight - fill*longEdited.weight
	if fill < 1 || free-typedGrowth < agent.weight || free >= 2*agent.weight {
		t.Fatalf("two rooms at the state cap, the typed room and %d at the decode cap leave %d of the %d-byte budget, want room for one agent-rewritten document (%d bytes) beside the %d its typing can add, and not for two before any typing",
			fill, free, roomBudgetBytesForTest, agent.weight, typedGrowth)
	}
	filling := memory.cloneDocuments(t, longEdited.id, int(fill))
	lastRooms := memory.cloneDocuments(t, agent.id, 2)
	folded := append(memory.cloneDocuments(t, agent.id, folds/2), memory.cloneDocuments(t, longEdited.id, folds/2)...)
	worst := int64(roomBudgetBytesForTest) + documentReadBudgetForTest + worstCaseLoadSlotBytesForTest
	t.Logf("the budget: two rooms at the state cap are charged %d bytes each, the room a person types in %d and up to %d more for its %d keystrokes, and %d rooms at the decode cap %d each, which leaves %d, room for one agent-rewritten document (%d) beside the typing and not two",
		heaviest.weight, typed.weight, typedGrowth, typingKeystrokes, fill, longEdited.weight, free, agent.weight)
	t.Logf("the slots: a handoff of the %d bytes a room at the state cap holds is far over the %d-byte figure at which an apply takes a slot, and so is a fold of the agent's %d bytes or the long-edited document's %d, so each of them holds one of the pod's %d slots for its whole run",
		heaviest.stateBytes, unslottedApplyBytesForTest, agent.stateBytes, longEdited.stateBytes, loadSlotsForTest)
	t.Logf("the worst case: the %d-byte budget, the %d-byte cache (%d documents of %d bytes each) and the %d bytes the pod's %d load slots may hold come to %d bytes, against the %d-byte Go limit and the pod's %d; the documents took %s to build",
		roomBudgetBytesForTest, documentReadBudgetForTest, len(cached), cacheWeight, int64(worstCaseLoadSlotBytesForTest), loadSlotsForTest, worst,
		podGoMemoryLimit, podMemory, time.Since(building).Round(time.Second))
	t.Logf("the drive's %d documents owe the server no block index, so the only reads of them are the drive's own", memory.oweNoBlockIndex(t))

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
	var typing *browser
	filledPeak := server.peakResident(t, func() {
		typing = open(t, typedIDs[0])
		handing = append(handing, open(t, heavyRooms[0]), open(t, heavyRooms[0]), open(t, heavyRooms[1]))
		for _, room := range filling {
			open(t, room)
		}
	})
	t.Logf("the budget is filled: the room a person types in, two rooms at F held by %d browsers and %d rooms at the decode cap, in %s; %d MiB resident now",
		len(handing), len(filling), time.Since(step).Round(time.Second), server.resident(t, "VmRSS")>>20)

	// From here through the heaviest applies a person types in the room they opened first.
	typist := startTyping(t, typing)
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
	t.Logf("the cache is filled by %d cold reads, in %s: %d MiB more resident than before them",
		len(cached), time.Since(step).Round(time.Second), retained>>20)
	if retained < documentReadBudgetForTest/2 {
		t.Errorf("filling the cache left %d MiB more resident, want at least %d MiB, so the cache holds what the worst case counts", retained>>20, (documentReadBudgetForTest/2)>>20)
	}

	handoffs := make([][]byte, len(handing))
	for index, handingBack := range handing {
		handoffs[index] = typedHandoff(t, handingBack.state(t))
		if len(handoffs[index]) > maxRoomStateBytesForTest || len(handoffs[index])+64<<10 < maxRoomStateBytesForTest {
			t.Fatalf("handoff %d is %d bytes, want within 64 KiB under the room's state cap (%d)", index, len(handoffs[index]), maxRoomStateBytesForTest)
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
	keys, typingErr := typist.finish()
	if typingErr != nil {
		t.Errorf("the person typing in an ordinary room through the cache's fill and the heaviest applies stopped before them: %v", typingErr)
	}
	for _, during := range []struct {
		what string
		keys []keystroke
	}{
		{"the cache's fill", slices.DeleteFunc(slices.Clone(keys), func(key keystroke) bool { return !key.sent.Before(step) })},
		{"the heaviest applies beside the folds", slices.DeleteFunc(slices.Clone(keys), func(key keystroke) bool { return key.sent.Before(step) })},
		{"both", keys},
	} {
		latency := keystrokeLatencies(during.keys)
		t.Logf("keystrokes in the ordinary room during %s: %d, each answered applied in p50 %s, p90 %s, p99 %s, at most %s",
			during.what, len(during.keys), latency.p50, latency.p90, latency.p99, latency.max)
		if len(during.keys) == 0 {
			t.Errorf("the person typed nothing during %s, want a keystroke every %s", during.what, typingInterval)
			continue
		}
		if latency.p99 >= 2*time.Second || latency.max >= 5*time.Second {
			t.Errorf("keystrokes in an ordinary room during %s were answered in p99 %s and at most %s, want p99 under 2 s and none over 5 s: typing waited behind the heavy work",
				during.what, latency.p99, latency.max)
		}
	}

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
			// The budget and the pod's load concurrency both refuse a browser 4503 here. A
			// refusal with the budget's figures is the budget's; one with no such line is a
			// wait for a slot that outlasted the request (docs' errWriteBusy), which at the
			// calibration's one slot a browser behind three handoffs of the heaviest state
			// meets, and which the production pod's sixteen make far rarer.
			refusal, logged := refusals[lastRooms[index]]
			if !logged {
				t.Logf("the browser of agent-rewritten document %d was refused in %s with no budget line, which is the pod's %d load slots", index, result.took.Round(time.Millisecond), loadSlotsForTest)
				continue
			}
			t.Logf("the browser of agent-rewritten document %d was refused by the budget in %s at %d charged and %d reserved of %d bytes, asking %d, its own room weighed at %d",
				index, result.took.Round(time.Millisecond), refusal.charged, refusal.reserved, refusal.limit, refusal.asks, agent.weight)
			if refusal.charged+refusal.reserved+refusal.asks <= refusal.limit {
				t.Errorf("the browser of agent-rewritten document %d was refused with %+v, which fits the %d-byte limit: refused earlier than the budget's arithmetic allows",
					index, refusal, roomBudgetBytesForTest)
			}
		}
	}
	if admittedLast != 1 {
		t.Errorf("%d of the two agent-rewritten documents were admitted, want the one the budget's arithmetic has room for", admittedLast)
	}
	// A cold read behind the handoffs waits for the pod's one slot and is answered 503
	// DOC_SERVICE_UNAVAILABLE at its own deadline where the wait outlasts it (docs' loadSlots,
	// errWriteBusy). That is the load concurrency working, not a failure: one slot is what makes
	// A3's arithmetic true at a 4 GiB pod, and the production pod's limit gives sixteen, where
	// a read waits behind at most a sixteenth of this. So the drive counts the two answers and
	// holds only that no read failed any other way.
	coldRead, coldBusy := 0, 0
	for index, result := range read {
		t.Logf("cold read %d answered in %s (%v)", index, result.took.Round(time.Millisecond), result.err)
		switch {
		case result.err == nil:
			coldRead++
		case strings.Contains(result.err.Error(), "DOC_SERVICE_UNAVAILABLE"):
			coldBusy++
		default:
			t.Errorf("cold read %d of a struct-dense document: %v", index, result.err)
		}
	}
	t.Logf("%d of the %d cold reads folded their document and %d waited for one of the pod's %d load slots until their own deadline and were answered 503",
		coldRead, len(read), coldBusy, loadSlotsForTest)

	holds := loadSlotHolds(t, window)
	var longestWait, longestHold time.Duration
	for _, hold := range holds {
		longestWait, longestHold = max(longestWait, hold.waited), max(longestHold, hold.held)
	}
	logLoadSlots(t, "the heaviest applies beside the folds", holds)
	logLoadSlots(t, "the whole run", loadSlotHolds(t, server.output.String()))
	t.Logf("the heaviest applies beside the folds, in %s: %d holders of the pod's %d load slots waited for one, the longest for %s, and the longest hold was %s",
		took.Round(time.Second), len(holds), loadSlotsForTest, longestWait.Round(time.Millisecond), longestHold.Round(time.Millisecond))
	// The handoffs and the folds are each an apply over unslottedApplyBytesForTest or a
	// whole-document decode, so each takes a slot, and the calibration has one: the heavy work
	// queues. What the drive reads of that queue is the line itself, and the line is written
	// only for a holder that waited loadSlotLogged - a tenth of a second - or more
	// (docs' logLoadSlot), so its count is a floor on the takers and not their number: a taker
	// that found the slot free leaves none, and so does one that waited a moment. So the
	// assertion is on what a line means rather than on how many there are. One holder waiting
	// while another holds is the worst case happening: the heaviest apply and the heaviest
	// decode asked for the pod's load concurrency at the same moment and took it in turn.
	// Measured on 2026-11-06: four lines, one holder waiting 1 m 9.831 s while another held the
	// slot 59.946 s.
	if len(holds) == 0 || longestWait <= 0 {
		t.Errorf("the server logged %d holders of a load slot that waited a tenth of a second or more, the longest %s: the %d handoffs and %d folds did not queue for the pod's %d slots, so the worst case did not happen",
			len(holds), longestWait, len(handing), folds, loadSlotsForTest)
	}
	for _, measured := range []struct {
		step string
		peak podPeak
	}{
		{"the budget filled", filledPeak},
		{"the cache filled", cachePeak},
		{"the heaviest applies beside the folds", worstPeak},
	} {
		requireUnderThePod(t, measured.peak, measured.step)
	}
}

// liveWriteDeadline is the deadline each write of the drive below runs under, which is the one a
// real caller's write has. No deadline is the server's: its http.Server names ReadHeaderTimeout
// and IdleTimeout and leaves WriteTimeout zero, deliberately, because the event stream is
// long-lived (newHTTPServer, conn_age.go:46-53), and no handler of the API wraps a request's
// context in a timeout of its own, so a write waits for a load slot exactly as long as its
// client keeps the request (docs' liveWrite.coverFork). The client is the one every
// agent's `dispatch` command goes through, which bounds each HTTP call at
// DISPATCH_TOOL_DEADLINE_MS, 60 seconds (packages/envoy-client/src/dispatch-http.ts:291,
// requestSignal).
const liveWriteDeadline = 60 * time.Second

// A burst of concurrent API live writes to large documents, over a server whose room budget their
// rooms charge, keeps the pod inside its memory, because every live write's fork holds one slot
// of the pod's load concurrency for the fork's whole life: taken before the fork is built -
// before the clone of a warm one and before the decode of a cold one - and given back only when
// the fork is dropped (docs' liveWrite.coverFork, loadSlots). The calibration has one slot
// (loadSlotsForTest), so however many writes a swarm of agents sends at once, one fork stands at
// a time and every other write waits for a slot inside its own request, holding no copy of a
// document and no database connection: a slot is taken before any connection is
// (docs' copy_heap.go). Before INFRA-179 a write's fork took no grant at all for the usual warm
// path and a decode's weighed grant alone for the cold one, and nothing pod-wide made two
// hundred forks take turns.
//
// The four findings this drive is here for, each of which it asserts:
//
//   - A write to a document no room holds is counted against the room budget or waits for it. A
//     cold write's publish opens the room it publishes into only while the budget has an opening
//     for one (docs' roomBudget.openServerRoom); over this fill it has none, so those writes
//     publish into no room (docs' publishedWithoutARoom) and the settlements they leave owed
//     take the budget's one opening in turn once a browser's room is closed, each such room
//     charged to the budget while it stands and closed as soon as that settlement is done with
//     it (closeIfOpenedOverBudget). The drive fails unless the budget closed rooms the server's
//     own work opened over it, and unless every written document settled.
//   - A person's keystroke is not starved by a burst of API writes. A keystroke takes no slot -
//     a one-character insert is far under unslottedApplyBytesForTest and loads nothing - so the
//     drive measures the 99th percentile and the worst, holds the percentile under 5 s and
//     records the worst, which is the pod's own work and no bound of this package's.
//   - A busy request answers 503 and not 500. A write whose wait for a slot ends with its
//     request is refused errWriteBusy, which the API answers 503 DOC_SERVICE_UNAVAILABLE (docs'
//     errWriteBusy, api's documentErrorCode); and a request whose context ends anywhere else -
//     opening a transaction while every pooled connection is held, taking the owner's row,
//     reading the stored history - is answered the same 503 with the same reason rather than the
//     500 it was before (api's endedWithACancelledContext). The drive fails on any other status.
//   - A write is not applied after its client gave up. The server's own access log says when it
//     answered each write; the drive fails if any write it answered 200 was answered past its
//     caller's deadline, so no edit lands behind a caller that has already read a timeout and
//     may retry.
//
// Measured on 2026-11-05 on a two-processor pod, before the cut, 320 writes to documents of a
// mebibyte of prose over a budget filled to its brim, all 36 of the rooms it then held resident:
// the fill charged 3,461 MiB of the pod's 4,096, holding 3,559,696,008 bytes of what the
// 3,865,470,566-byte Go limit bounds, 305,774,558 under it, and the burst charged 3,657, holding
// 3,763,361,464 bytes, 102,109,102 under it. The burst took 2 m 0 s: 16 writes applied, 62 were
// refused 503, and 242 were answered where their caller's deadline found them. Of the applied,
// the slowest took 1 m 11.2 s and the median 38.8 s. The bound's holders waited up to 56.367 s
// and held up to 9.184 s, and the room budget closed the 4 rooms the server's own work had
// opened over it. Three runs of it read 3,657 to 3,681 MiB charged and 75,476,910 to
// 102,109,102 bytes under the limit. The one price charges a room of a mebibyte of prose its
// stored state at 28 bytes a byte, so the budget holds about 25 of them now rather than 36, and
// the fill is that many: the shape of the burst is unchanged and its figures are read afresh.
//
// The same fill before the budget booked an opening for the server's own rooms (docs'
// roomBudget.openServerRoom) took the pod past its memory: each applied cold write's publish
// loaded the room it published into, which no sweep could close while the settlement the write
// armed held it, and the burst charged 4,435 MiB of the pod's 4,096 and held 713,885,218 bytes
// past the Go limit, with no room closed over the whole window. The same burst over a fill that
// left the budget 24 rooms free - as this drive filled it until the opening was added - charged
// 3,854 and held 104,124,530 past the limit, which is why the fill was short of the brim: with
// nothing refusing the server's own rooms, only a fill that kept room for them stayed inside the
// pod.
//
// The burst is what a swarm of agents makes: writes to documents of many owners, all at once. Two
// things of the pod bound how far such a burst gets before the slots are reached at all, and the
// drive measures both rather than assuming either:
//
//   - The room budget bounds how many of these documents can be resident, since each is charged
//     its stored state at the one price over both copies (storedShape, roomWeightForTest). The
//     drive opens browsers of every one of them, so the budget is full
//     before the burst begins and has no room for any room the burst would open, and writes to
//     the rest cold - a write to a document no room holds reads the stored history and decodes it
//     into its fork (docs' coldFork), which is the path that holds those bytes while it waits for
//     its slot.
//   - The shared pool bounds how many of them are live writes at any moment, since each write's
//     handler holds one of its connections from the transaction it opens to the commit or rollback
//     that ends it (api's begin, editArtifact), so a write past sharedPoolSizeForTest waits for a
//     connection and not for a slot. A burst deeper than that pool therefore costs the pod no
//     more memory than one the size of it, and only takes longer to drain.
//
// Every API write to a document takes its owner's row for the length of its transaction, so the
// owner cannot close under the write (api's requireOpenOwnerStatus, ownerRowLock, "for no key
// update"). For a document of an issue that row is the issue's, so a burst of writes to many
// documents of one issue runs strictly one write at a time whatever any bound would have done with
// it - which is why this drive writes to documents no issue owns (projectDocumentCopies), each of
// which takes its own artifact row, as writes to documents of different issues take different
// issue rows.
//
// What the drive holds besides the four findings:
//
//   - Both figures a pod is weighed by stay within their bounds through the fill of the budget and
//     through the burst (requireUnderThePod): what a pod's kernel would charge, under the pod's 4
//     GiB, and what the Go memory limit bounds, within the margin of that limit.
//   - No write is lost and none is half-made: a document whose write the server answered 200 holds
//     that write's text in its latest stored version, and one whose write it answered anything else
//     holds exactly the text and the history it was stored with.
//   - The slots are what the writes waited for: the server logs each slot a holder waited a
//     tenth of a second or more for (loadSlotLine, logLoadSlots), and the burst leaves such
//     lines.
//
// What it records rather than asserts, because each is the system working as it is written to:
//
//   - A caller's deadline is not what ends a write's wait at the moment the caller gives up.
//     net/http ends a request's context when the server reads the end of the connection its
//     client closed, and it starts the read that would see that end only once the handler has
//     read the request's body to its end, so a write may wait a little past its caller before
//     its refusal lands. What the drive holds is the answer: no write the server answered 200
//     was answered past the deadline.
//   - While the charges stand past the limit every update that inserts anything is refused in
//     every room with the transient 4503 DOC_BUSY, the person's keystrokes among them (docs'
//     rulePressure, and TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks). The drive
//     records how far the person got and that what stopped them, where anything did, was that
//     refusal and nothing else.
//   - One keystroke of that person's waits far longer than the percentile, behind the pod's own
//     work. The drive logs every percentile and the worst of them.
func TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory(t *testing.T) {
	const writes, find, with = 320, "Section 0", "Section Zero"
	building := time.Now()
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	sourceID, state := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	source := memory.storedShape(t, "a mebibyte of prose", sourceID)
	typedIDs, _ := memory.smallDocuments(t, 1)
	typed := memory.storedShape(t, "the ordinary document a person types in", typedIDs[0])
	typedGrowth := int64(typingKeystrokes * keystrokeChargeForTest)
	// Every room the budget holds of these documents beside the room a person types in and what
	// their keystrokes can add to it: the brim, with nothing left over for a room the burst would
	// open. A cold write's publish loads its document's room, from the store, which by then holds
	// the write (docs' coldFork says why it loads then rather than before), and that room is
	// charged to this same budget and cannot be closed while the settlement the write arms still
	// holds it (closeIfOpenedOverBudget). So this is the fill that asks whether the budget bounds
	// the server's own rooms at all, and the answer is the opening it books for them (docs'
	// roomBudget.openServerRoom): a publish that cannot have one publishes into no room.
	full := int((roomBudgetBytesForTest-typed.weight-typedGrowth)/source.weight) - 1
	if full < 2 || full >= writes {
		t.Fatalf("the %d-byte budget holds %d documents charged %d bytes each beside the typed room's %d and its typing's %d; want at least two and fewer than the %d documents written",
			roomBudgetBytesForTest, full, source.weight, typed.weight, typedGrowth, writes)
	}
	copies := memory.projectDocumentCopies(t, sourceID, writes)
	t.Logf("%d documents of a mebibyte of prose, %d bytes of stored state each: the budget holds %d of them resident and every one of them is made resident here, which fills it to the brim; the fill charges %d of the %d-byte budget beside the typed room's %d and its typing's %d, and %d documents are written cold; they took %s to build",
		writes, len(state), full, int64(full)*source.weight, roomBudgetBytesForTest, typed.weight, typedGrowth, writes-full, time.Since(building).Round(time.Second))
	t.Logf("the slots: a fork of a document of %d bytes of state holds one of the pod's %d load slots for its whole life, so %d of these forks stand at once; at most %d of the %d writes are live writes at any moment, one for each connection of the shared pool, and each cold fork waiting for its slot holds the %d stored bytes it read, %d bytes for all of them",
		len(state), loadSlotsForTest, loadSlotsForTest, sharedPoolSizeForTest, writes, len(state), sharedPoolSizeForTest*len(state))
	t.Logf("the drive's %d documents owe the server no block index, so the only reads of them are the drive's own", memory.oweNoBlockIndex(t))

	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	var held []*browser
	t.Cleanup(func() {
		for _, opened := range held {
			opened.leave()
		}
	})
	step := time.Now()
	var typing *browser
	filledPeak := server.peakResident(t, func() {
		typing = server.openBrowser(t, typedIDs[0])
		held = append(held, typing)
		for _, room := range copies[:full] {
			held = append(held, server.openBrowser(t, room))
		}
	})
	for _, room := range copies[:full] {
		if !server.roomLive(t, room) {
			t.Fatalf("document %s is not resident after its browser opened it, so the burst's writes to it would not fork warm", room)
		}
	}
	t.Logf("the budget is filled to its brim: the room a person types in and %d rooms of a mebibyte of prose, in %s; %d MiB resident now",
		full, time.Since(step).Round(time.Second), server.resident(t, "VmRSS")>>20)
	if refusals := budgetRefusalsByRoom(t, server); len(refusals) != 0 {
		t.Errorf("the room budget refused %d of the browsers that fill it (%+v), want room for every one of them", len(refusals), refusals)
	}

	// From here through the burst a person types in the room they opened first.
	typist := startTyping(t, typing)
	type outcome struct {
		status int
		took   time.Duration
		err    error
	}
	outcomes := make([]outcome, writes)
	mark := len(server.output.String())
	step = time.Now()
	burstPeak := server.peakResident(t, func() {
		start := make(chan struct{})
		var group sync.WaitGroup
		for index, room := range copies {
			group.Go(func() {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), liveWriteDeadline)
				defer cancel()
				answered, err := server.editWithin(ctx, room, find, with)
				outcomes[index] = outcome{status: answered.status, took: time.Since(step), err: err}
			})
		}
		close(start)
		group.Wait()
		// The writes that applied are settled once the settle delay after them has passed, a
		// render and a version of each, and a load of every room the burst wrote cold, which the
		// peak counts too. Over this fill each of those loads waits for the budget's one opening
		// and for the room before it to close (docs' awaitServerRoom, settleNoRoom), so they
		// take their turns rather than standing at once, which is the whole of what this fill
		// asks of them.
		for _, room := range copies {
			memory.waitForSettled(t, room)
		}
	})
	took := time.Since(step)
	window := server.output.String()[mark:]
	keys, typingErr := typist.finish()
	latency := keystrokeLatencies(keys)
	t.Logf("keystrokes in the ordinary room during the burst: %d, each answered applied in p50 %s, p90 %s, p99 %s, at most %s",
		len(keys), latency.p50, latency.p90, latency.p99, latency.max)
	switch {
	case typingErr == nil:
	case strings.Contains(typingErr.Error(), contracts.DocumentBusyCloseReason):
		// A cold write's publish opens no room while the budget has no opening for one, but the
		// settlements those writes leave owed each open one in turn, and a room opened over a
		// budget its browsers already fill takes the charges past the limit while it stands.
		// While they are past it every update that inserts anything is refused in every room with
		// the transient close, the person's keystroke among them (docs' refusedUnderPressure, and
		// TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks, which holds that refusal of a
		// browser's own growth). The editor shows it and retries; the drive records how far the
		// person got before the burst's own pressure refused them.
		t.Logf("the person's typing was refused after %d keystrokes with the transient %d %s every insert meets while the open rooms are past their budget: %v",
			len(keys), contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, typingErr)
	default:
		t.Errorf("the person typing in an ordinary room through the burst stopped before it ended: %v", typingErr)
	}
	if len(keys) == 0 {
		t.Errorf("the person typed nothing during the burst, want a keystroke every %s", typingInterval)
	}
	// Most keystrokes are answered as promptly as an unloaded room answers them, and one is not.
	// What the worst one waits on is the pod's own work, and none of the bounds: the keystroke's
	// path takes no part of the copy bound (the one holder in it, the replica's build from a
	// base, takes only what is free at once and never waits, docs' holdFreeCopy), no part of the
	// apply bound (an insert of one character is far under docs' unweighedApplyHeapBytes, so it
	// is never held, docs' weighPeerUpdate) and no pooled connection (its durable append is the
	// room's own persistence worker's, handed over a buffered channel, and that worker takes its
	// connection off this goroutine, docs' withRoomLock).
	//
	// The worst is not a repeatable figure and the drive does not hold it to one. Measured on
	// 2026-11-05 over three runs of this fill, all of them with the heap inside the Go limit:
	// p50 19 to 76 ms, p90 109 to 379 ms, p99 382 ms to 1.37 s, and a worst keystroke of
	// 11.015 s, 12.87 s and 1 m 1.286 s. Over five runs of the earlier fill that kept rooms free
	// for the burst's own it was 10.9 to 11.3 s, with the fork's grant patched out 17.022 s, and
	// over the brim fill before the budget booked an opening for the server's own rooms - whose
	// heap stood 713,885,218 bytes past the Go limit - 1 m 5.795 s. So the heap's overshoot is
	// not what sets it: two processors carrying 64 writers' forks, renders and collections are,
	// and nothing in this drive bounds that. All of them are against the p99 under 2 s and none
	// over 5 s an ordinary room answers in beside the heaviest applies
	// (TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory). So the drive holds the
	// percentile a person feels as typing and records the worst; a keystroke the room never
	// answers is the typist's own error above (browserAnswerWithin).
	if latency.p99 >= 5*time.Second {
		t.Errorf("keystrokes in an ordinary room during the burst were answered in p99 %s, at most %s, want a 99th percentile under 5 s: typing waited behind the writes further than any burst has made it",
			latency.p99, latency.max)
	}

	// What the server answered each write, read from its own access log beside what its caller
	// saw: a write still waiting for its grant when its caller's deadline passes is left waiting,
	// since net/http ends a request's context only once the server reads the end of the
	// connection its client closed, so the answer the caller never read is the server's own line.
	answers := editAnswers(t, window)
	rewritten, updates := memory.rewrittenDocuments(t, append(slices.Clone(copies), sourceID), "## "+with)
	stored := updates[sourceID]
	applied, refused, elsewhere, late, gaveUp := 0, 0, 0, 0, 0
	statuses := map[int]int{}
	var appliedTook []time.Duration
	var elsewhereSample []string
	folded := 0
	var firstRefusal time.Duration
	for index, room := range copies {
		answered, logged := answers[room]
		statuses[answered.status]++
		if outcomes[index].err != nil {
			gaveUp++
		}
		switch {
		case !logged:
			t.Errorf("write %d to document %s is in no line of the server's access log, so the server never served it (its caller read %d, %v)", index, room, outcomes[index].status, outcomes[index].err)
		case answered.status == http.StatusOK:
			applied++
			appliedTook = append(appliedTook, answered.elapsed)
			if answered.elapsed > liveWriteDeadline {
				late++
				// The two clocks on a write the server answered past its caller's deadline,
				// for the one question its fix turns on: whether the overrun is before the
				// commit or after it. The server's own is the access log's elapsed, which runs
				// from the request's first byte to the answer written; the caller's is what
				// its own context measured, which ends at liveWriteDeadline. A commit inside
				// the deadline with the answer after it says the write was durable in time and
				// what overran is the publish the answer waits behind.
				t.Logf("write %d to document %s was answered 200 after %s by the server's own log, against its caller's %s deadline, which read %v after %s: the overrun is %s",
					index, room, answered.elapsed.Round(time.Millisecond), liveWriteDeadline,
					outcomes[index].err, outcomes[index].took.Round(time.Millisecond),
					(answered.elapsed - liveWriteDeadline).Round(time.Millisecond))
			}
			if !rewritten[room] || updates[room] < stored {
				t.Errorf("write %d to document %s was answered 200, and the document holds the written text %v with %d stored updates, want the text in its latest version and no fewer than the %d updates it was stored with",
					index, room, rewritten[room], updates[room], stored)
			}
			if updates[room] != stored+1 {
				// A compaction of the document, which folds its history into one row, is the
				// only thing that leaves an applied write's update anywhere but in a row of its
				// own (PgVersioned's compaction); it is counted so that a write landing in no
				// row at all cannot hide behind it.
				folded++
			}
		case answered.status == http.StatusServiceUnavailable:
			refused++
			if firstRefusal == 0 || answered.elapsed < firstRefusal {
				firstRefusal = answered.elapsed
			}
			if rewritten[room] || updates[room] != stored {
				t.Errorf("write %d to document %s was refused 503, and the document holds the written text %v with %d stored updates, want it exactly as it was stored, with %d",
					index, room, rewritten[room], updates[room], stored)
			}
		default:
			elsewhere++
			if len(elsewhereSample) < 3 {
				elsewhereSample = append(elsewhereSample, fmt.Sprintf("write %d to document %s was answered %d in %s, its caller reading %v",
					index, room, answered.status, answered.elapsed.Round(time.Millisecond), outcomes[index].err))
			}
			if rewritten[room] || updates[room] != stored {
				t.Errorf("write %d to document %s was answered %d, and the document holds the written text %v with %d stored updates, want it exactly as it was stored, with %d",
					index, room, answered.status, rewritten[room], updates[room], stored)
			}
		}
	}
	before := 0
	for _, elapsed := range appliedTook {
		if firstRefusal == 0 || elapsed < firstRefusal {
			before++
		}
	}
	slices.Sort(appliedTook)
	var median, slowest time.Duration
	if len(appliedTook) > 0 {
		median, slowest = appliedTook[len(appliedTook)/2], appliedTook[len(appliedTook)-1]
	}
	t.Logf("%d concurrent writes (%d warm, %d cold), each under its caller's %s deadline, in %s: %d applied, %d refused 503 DOC_SERVICE_UNAVAILABLE, %d answered otherwise; %d applied before the first refusal, the slowest applied in %s and the median in %s",
		writes, full, writes-full, liveWriteDeadline, took.Round(time.Second), applied, refused, elsewhere, before, slowest, median)
	t.Logf("the statuses the server answered, by count: %v; %d of the writes it applied it answered past their caller's %s deadline, %d callers read an error of their own rather than an answer, and a compaction folded the history of %d of the documents written",
		statuses, late, liveWriteDeadline, gaveUp, folded)
	if elsewhere > 0 {
		t.Logf("the %d writes answered neither 200 nor 503 were answered where their caller's deadline found them, which is not the bound's wait: %s; the server logged %d handler failures, %s",
			elsewhere, strings.Join(elsewhereSample, "; "), len(handlerFailureLine.FindAllString(window, -1)), handlerFailures(window))
	}
	if applied+refused+elsewhere != writes {
		t.Errorf("%d writes were applied, %d refused and %d answered otherwise, which is not the %d sent", applied, refused, elsewhere, writes)
	}
	if applied == 0 {
		t.Errorf("none of the %d writes applied, want the burst to make the writes the slots have room for", writes)
	}
	if elsewhere != 0 {
		t.Errorf("%d writes were answered neither 200 nor 503, want every write the burst could not make answered 503 DOC_SERVICE_UNAVAILABLE: a request whose context ends anywhere a burst ends one is the service unable to serve it in time, not a fault of the server's (api's endedWithACancelledContext)", elsewhere)
	}
	// A write is not committed after its caller has gone. What the server guarantees is the
	// commit itself: the ledger reads the request's context once, immediately after everything
	// the transaction waited for and immediately before the commit, and refuses rather than
	// committing (docs' commitConsuming, "the caller ended before its write committed"), which
	// the API answers 503. So a write answered 200 began its commit while its caller was live,
	// and the writes counted late above are ones whose commit landed in time and whose answer
	// did not: the caller reads a timeout, the edit is durable, and its retry carries an
	// idempotency key. That is the shape the architect settled on.
	//
	// The drive records the overrun rather than failing on it, because it cannot read the
	// commit's own time: the server logs no commit duration today, so an assertion on "the
	// commit landed inside the deadline" would be an inference from the answer and not a
	// reading. What it holds instead is beside this: every write the server answered 200 is in
	// the store, and every write it answered otherwise left the document exactly as stored.
	if late != 0 {
		t.Logf("%d of the writes the server answered 200 it answered past their caller's %s deadline; the line above gives each one's two clocks", late, liveWriteDeadline)
	}

	// The slots the writes' forks held, which is what made them wait, and what the budget did
	// with the rooms of the documents written cold: a publish over this fill opens none (docs'
	// publishedWithoutARoom), and the settlement each such write leaves owed opens one in its
	// turn and gives it back, which the budget closes as soon as that settlement is done with it
	// since the browsers of the fill hold it full (closeIfOpenedOverBudget, sweepRooms).
	holds := loadSlotHolds(t, window)
	logLoadSlots(t, "the burst of concurrent live writes", holds)
	opened := len(serverOpenedCloseLine.FindAllString(window, -1))
	t.Logf("the room budget closed %d rooms the server's own work opened over it and swept %d unused rooms during the burst",
		opened, len(budgetCloseLine.FindAllString(window, -1)))
	if len(holds) == 0 {
		t.Errorf("the server logged no holder of a load slot that waited for one, want the forks of the writes that queued behind the pod's %d", loadSlotsForTest)
	}
	if opened == 0 {
		t.Errorf("the room budget closed no room the server's own work opened over it, want the rooms the cold writes' settlements each took the budget's one opening for: either no write applied cold or those rooms were never charged to the budget")
	}
	for _, measured := range []struct {
		step string
		peak podPeak
	}{
		{"the room budget filled", filledPeak},
		{"the burst of concurrent live writes", burstPeak},
	} {
		requireUnderThePod(t, measured.peak, measured.step)
	}
}

// sharedPoolSizeForTest mirrors store's sharedPoolSize, the connections every request reads and
// writes through. A write's handler holds one from the transaction it opens to the commit or
// rollback that ends it, so no more than this many API writes are live writes at one moment and a
// burst past it waits for a connection rather than for a memory bound. A wrong mirror moves only
// what TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory logs of its burst's arithmetic.
const sharedPoolSizeForTest = 64

// editAnswerLine is the line the server logs for each edit it served (accesslog.Handler,
// accesslog.Message): the document in the path, the status it answered and how long the request
// took. It is how a drive reads the answer to a write whose own client gave up first, which the
// server writes into a connection that has already closed.
var editAnswerLine = regexp.MustCompile(`msg="dispatch: request" method=POST route=/api/v1/artifacts/\{id\}/edits path=/api/v1/artifacts/([0-9a-fA-F-]+)/edits [^\n]*status=(\d+) elapsed_ms=(\d+)`)

// editAnswer is what the server answered one write, as its line records it.
type editAnswer struct {
	status  int
	elapsed time.Duration
}

// editAnswers is the server's own answer to every edit served in output, by document.
func editAnswers(t *testing.T, output string) map[string]editAnswer {
	t.Helper()
	answers := map[string]editAnswer{}
	for _, match := range editAnswerLine.FindAllStringSubmatch(output, -1) {
		status, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("read the status of %q: %v", match[0], err)
		}
		elapsed, err := strconv.Atoi(match[3])
		if err != nil {
			t.Fatalf("read the elapsed time of %q: %v", match[0], err)
		}
		answers[match[1]] = editAnswer{status: status, elapsed: time.Duration(elapsed) * time.Millisecond}
	}
	return answers
}

// handlerFailureLine is the line the server logs for a request whose error names no status of its
// own, which it answers 500 INTERNAL (api's writeHandlerError).
var handlerFailureLine = regexp.MustCompile(`msg="dispatch: API handler failed" error="([^"]*)"`)

// handlerFailures is what those failures were in output: each distinct error with how many
// requests met it, so a drive says where a request its caller ended was when it ended rather than
// guessing.
func handlerFailures(output string) string {
	counted := map[string]int{}
	for _, match := range handlerFailureLine.FindAllStringSubmatch(output, -1) {
		counted[match[1]]++
	}
	failures := make([]string, 0, len(counted))
	for failure, count := range counted {
		failures = append(failures, fmt.Sprintf("%d of %q", count, failure))
	}
	slices.Sort(failures)
	return strings.Join(failures, ", ")
}

// editWithin sends one quote-anchored edit of artifactID, replacing find with with, as an agent's
// `dispatch doc-edit` does, bounded by ctx: the deadline a drive gives it is its client's, since
// the server sets none (liveWriteDeadline). A request its deadline ends answers the error the
// client raises, and the answer the server wrote for it is in the server's log (editAnswers).
func (p *memoryServer) editWithin(ctx context.Context, artifactID, find, with string) (response, error) {
	body, err := json.Marshal(map[string]any{"ops": []map[string]string{{"op": "replace", "find": find, "with": with}}})
	if err != nil {
		return response{}, fmt.Errorf("encode the edit: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/v1/artifacts/"+artifactID+"/edits", bytes.NewReader(body))
	if err != nil {
		return response{}, fmt.Errorf("build the edit: %w", err)
	}
	request.Header = http.Header{"Content-Type": {"application/json"}, "X-Dispatch-User": {"alice"}}
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		return response{}, fmt.Errorf("edit document %s: %w", artifactID, err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, fmt.Errorf("read the answer of the edit of document %s: %w", artifactID, err)
	}
	return response{status: answer.StatusCode, body: read}, nil
}

// projectDocumentCopies is cloneDocuments into documents of the source's project that no issue
// owns, so that a write to each takes its own artifact row and not one row every write of the
// burst waits for (api's ownerForArtifact, documentOwner, ownerRowLock). It leaves none of them
// owing the server a block index, which each copy's versions mark it owed, as oweNoBlockIndex
// does for the issue's own documents.
func (h *memoryHarness) projectDocumentCopies(t *testing.T, sourceID string, count int) []string {
	t.Helper()
	ctx := context.Background()
	copies := make([]string, count)
	for index := range copies {
		tx, err := h.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin copy %d: %v", index, err)
		}
		slug := fmt.Sprintf("project-copy-%d-%03d", time.Now().UnixNano(), index)
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select null, project_key, $2, $2 || '.md', kind, false, created_by from artifacts where id = $1
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
		// Each copy's versions mark its blocks owed (0122's trigger), which this clears as
		// oweNoBlockIndex clears the issue's own.
		if _, err := tx.Exec(ctx, `delete from document_block_versions where artifact_id = $1`, copies[index]); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("clear the block index copy %d owes: %v", index, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit copy %d: %v", index, err)
		}
	}
	return copies
}

// rewrittenDocuments reads, for each of documents and for the source they were copied from, how
// many updates its stored history holds and whether its latest stored version holds heading, the
// text a write of the drive leaves: one query of the store rather than a read of the server for
// each document, and a second source beside the answers the server logged.
func (h *memoryHarness) rewrittenDocuments(t *testing.T, documents []string, heading string) (map[string]bool, map[string]int) {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(), `
		select a.id::text,
			(select count(*) from doc_updates u where u.artifact_id = a.id),
			coalesce((select v.markdown like $2 from artifact_versions v
				where v.artifact_id = a.id order by v.number desc limit 1), false)
		from artifacts a where a.id = any($1)
	`, documents, "%"+heading+"%")
	if err != nil {
		t.Fatalf("read what the writes left the documents: %v", err)
	}
	defer rows.Close()
	rewritten, updates := map[string]bool{}, map[string]int{}
	for rows.Next() {
		var id string
		var stored int
		var holds bool
		if err := rows.Scan(&id, &stored, &holds); err != nil {
			t.Fatalf("read a document's stored history: %v", err)
		}
		rewritten[id], updates[id] = holds, stored
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the writes left the documents: %v", err)
	}
	return rewritten, updates
}

// typingInterval is how often the person typing in an ordinary room presses a key (startTyping),
// and typingKeystrokes the most keys they press: 3 min 25 s of typing, past the minute the cache's
// fill and the heavy step took together (29 s and 32 s in one run, 34 s and 33 s in another), and
// what the budget's arithmetic leaves their room room for (keystrokeChargeForTest).
const (
	typingInterval   = 200 * time.Millisecond
	typingKeystrokes = 1_024
)

// keystroke is one key a person pressed in a room: when its update was sent, and how long the room
// took to answer that it applied it.
type keystroke struct {
	sent time.Time
	took time.Duration
}

// typist is a person typing in a room they hold open (startTyping), until finish.
type typist struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	keys     []keystroke
	err      error
}

// startTyping has a person type in typing's room: a keystroke every typingInterval, each the
// one-character insert the editor sends for it, typed after the last at the head of the room's
// first text, and sent once the room has answered the last as applied, so a keystroke the room is
// slow to answer delays the next rather than queueing beside it. The typing stops at the first
// keystroke the room does not apply, when the test ends, and once the person has pressed
// typingKeystrokes keys, which finish reports as the person having run out.
func startTyping(t *testing.T, typing *browser) *typist {
	t.Helper()
	document := newBrowserDocument(t, typing.state(t))
	text := firstXMLText(document.fragment)
	if text == nil {
		t.Fatal("the typed room's state holds no text")
	}
	person := &typist{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(person.done)
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		for typed := 0; ; typed++ {
			if typed == typingKeystrokes {
				person.mu.Lock()
				person.err = fmt.Errorf("the person pressed all %d keys before the typing was to stop", typingKeystrokes)
				person.mu.Unlock()
				return
			}
			select {
			case <-person.stop:
				return
			case <-ticker.C:
			}
			var update []byte
			stop := document.doc.OnUpdate(func(made []byte, _ any) { update = made })
			document.doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, typed, "k", nil) })
			stop()
			sent := time.Now()
			applied, closed, err := typing.send(typing.updateFrame(update))
			person.mu.Lock()
			person.keys = append(person.keys, keystroke{sent: sent, took: time.Since(sent)})
			if err != nil || !applied {
				person.err = fmt.Errorf("keystroke %d was answered applied %v, closed %v, %v", typed, applied, closed, err)
			}
			failed := person.err != nil
			person.mu.Unlock()
			if failed {
				return
			}
		}
	}()
	t.Cleanup(func() { person.finish() })
	return person
}

// finish stops the typing, and is every keystroke it sent and why the last was not applied, if it
// was not.
func (p *typist) finish() ([]keystroke, error) {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.keys), p.err
}

// latencies is the 50th, 90th and 99th percentiles and the most of how long keystrokes took.
type latencies struct{ p50, p90, p99, max time.Duration }

// keystrokeLatencies is the percentiles of keys' times to be answered, nearest-rank, rounded to the
// millisecond; zero for no keys.
func keystrokeLatencies(keys []keystroke) latencies {
	if len(keys) == 0 {
		return latencies{}
	}
	took := make([]time.Duration, len(keys))
	for index, key := range keys {
		took[index] = key.took
	}
	slices.Sort(took)
	rank := func(percent int) time.Duration {
		return took[max((len(took)*percent+99)/100-1, 0)].Round(time.Millisecond)
	}
	return latencies{p50: rank(50), p90: rank(90), p99: rank(99), max: took[len(took)-1].Round(time.Millisecond)}
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
// (documentCopies), and its stored state, for a drive to grow from small.
func (h *memoryHarness) smallDocuments(t *testing.T, count int) ([]string, []byte) {
	t.Helper()
	return h.documentCopies(t, "# Growth\n\nA small document its browser grows.\n", count)
}

// documentCopies is count copies of one stored document of markdown (storedDocument,
// cloneDocuments), and its stored state.
func (h *memoryHarness) documentCopies(t *testing.T, markdown string, count int) ([]string, []byte) {
	t.Helper()
	source, state := h.storedDocument(t, markdown)
	return h.cloneDocuments(t, source, count), state
}

// storedDocument is a document of markdown uploaded and settled on a server of its own, which no
// server holds open once it returns, and its stored state.
func (h *memoryHarness) storedDocument(t *testing.T, markdown string) (string, []byte) {
	t.Helper()
	setup := h.start(t)
	source := setup.upload(t, "json", h.issue, markdown)
	if source.status != http.StatusCreated {
		t.Fatalf("upload the document: status %d body %.300s, want 201", source.status, source.body)
	}
	h.waitForSettlement(t, source)
	setup.stop(t)
	return source.artifactID, h.storedState(t, source.artifactID)
}

// pasteRound is one paste a browser makes into a document, and the size of the rendering it leaves.
type pasteRound struct {
	update []byte
	size   pmdoc.DocumentSize
}

// pasteRounds is the pastes of markdown one browser makes from state, each on the last, for as long
// as the byte bound admits the next: the rendering before a paste and the paste's own bytes stay
// under pmdoc.MaxDocumentBytes (weighPeerUpdate). A paste sent to copies of state that took
// the pastes before it applies to each alike.
func pasteRounds(t *testing.T, state []byte, markdown string) []pasteRound {
	t.Helper()
	grower := newBrowserDocument(t, state)
	var rounds []pasteRound
	for {
		before := grower.size(t)
		added, err := pmdoc.Parse(markdown)
		if err != nil {
			t.Fatal(err)
		}
		update := grower.edit(t, func(tree *pmdoc.Node) {
			tree.Children = append(tree.Children, added.Children...)
			pmdoc.EnsureBlockIDs(tree)
		})
		if before.Bytes+len(update) >= pmdoc.MaxDocumentBytes {
			break
		}
		rounds = append(rounds, pasteRound{update: update, size: grower.size(t)})
	}
	if len(rounds) == 0 {
		t.Fatal("the byte bound admits no paste")
	}
	return rounds
}

// roomChargesAfter is what the room budget charges a room of room, a copy of the document rounds
// grow, after each paste whose index after lists: a server of its own takes the pastes from a
// browser that holds the room, and after each listed one reads the room's charge on a budget of
// one byte (chargedAgainstOneByte), refusing a browser of probe, a document no room holds, then
// puts the real budget back. A paste's charge is on the room the moment its drain commits - the
// room's observer adds the update's bytes to the room's tracked state and weighs the room again
// (docs' renderedReplica.chargeAdded) - so there is nothing to wait for between the paste and
// the reading.
func (h *memoryHarness) roomChargesAfter(t *testing.T, room, probe string, rounds []pasteRound, after ...int) []int64 {
	t.Helper()
	server := h.start(t)
	defer server.stop(t)
	growing := server.openBrowser(t, room)
	defer growing.leave()
	var charges []int64
	for index, round := range rounds {
		requireApplied(t, growing, round.update, fmt.Sprintf("paste %d into the room the charges are read from", index+1))
		if !slices.Contains(after, index) {
			continue
		}
		server.setRoomBudget(t, 1)
		charges = append(charges, server.chargedAgainstOneByte(t, probe, fmt.Sprintf("after paste %d", index+1)))
		server.setRoomBudget(t, 0)
	}
	return charges
}

// chargedAgainstOneByte is what the open rooms are charged, read the way the budget drives read the
// budget's figures, from the line the server logs for a browser it refuses (budgetRefusalLine): on
// a budget of one byte, which the caller has set, a browser opening probe, a document no room holds,
// is refused with what the rooms are charged.
func (p *memoryServer) chargedAgainstOneByte(t *testing.T, probe, when string) int64 {
	t.Helper()
	seen := len(budgetRefusalLine.FindAllStringSubmatch(p.output.String(), -1))
	refused, err := p.tryOpenBrowser(probe)
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
		if refusals := budgetRefusalLine.FindAllStringSubmatch(p.output.String(), -1); len(refusals) > seen {
			last := refusals[len(refusals)-1]
			charged, chargedErr := strconv.ParseInt(last[1], 10, 64)
			limit, limitErr := strconv.ParseInt(last[3], 10, 64)
			if chargedErr != nil || limitErr != nil || limit != 1 {
				t.Fatalf("%s: the server refused the probe with %q, want what is charged against a budget of one byte", when, last[0])
			}
			return charged
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the server logged no refusal of the probe:\n%s", when, p.output.tail())
		}
	}
}

// mebibyteFleet is copies of one stored document of a mebibyte of prose (cloneDocuments), its
// stored state for browsers to edit, its stored version's count of elements, and what the room
// budget charges a room that loads it, computed as the server computes it (storedShape): its
// stored state at one price over both copies (roomWeightForTest). copies is how many a drive
// needs, given that weight.
type mebibyteFleet struct {
	copies   []string
	state    []byte
	elements int
	weight   int64
}

func (h *memoryHarness) mebibyteFleet(t *testing.T, copies func(weight int64) int) mebibyteFleet {
	t.Helper()
	setup := h.start(t)
	source := setup.upload(t, "json", h.issue, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	if source.status != http.StatusCreated {
		t.Fatalf("upload a mebibyte of prose: status %d body %.300s, want 201", source.status, source.body)
	}
	h.waitForSettlement(t, source)
	setup.stop(t)
	shape := h.storedShape(t, "a mebibyte of prose", source.artifactID)
	state := h.storedState(t, source.artifactID)
	if want := roomWeightForTest(int64(len(state))); shape.weight != want {
		t.Fatalf("a mebibyte of prose is charged %d bytes, want its %d bytes of stored state at %d a byte, %d", shape.weight, len(state), bytesOfHeapAStateByteForTest*roomCopiesForTest, want)
	}
	return mebibyteFleet{
		copies:   h.cloneDocuments(t, source.artifactID, copies(shape.weight)),
		state:    state,
		elements: shape.elements,
		weight:   shape.weight,
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

// oweNoBlockIndex clears the block index every document of the harness's issue owes, and returns
// how many it cleared. Each document a drive builds owes one: every insert into artifact_versions
// of markdown marks its document owed (migration
// 0122_artifact_versions_block_index_trigger.up.sql), which an upload does and which
// cloneDocuments does again for each copy's versions.
//
// A server reads each owed document whole on its own within seconds of its boot: its block-index
// reconciliation lists twenty at a time, two seconds apart while a batch is full
// (blockIndexDocumentBatch, blockIndexStep), and
// renders each of them through the reads a browser's would take (indexOwedDocument).
// Such a read stores its rendering in the cold reads' cache under the document's stored stamp
// (foldDocument), and a drive's own cold read of
// that same stamp is then served from the cache and folds nothing (coldRead) - which is
// what answered two of the four cold reads of
// TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory in 230 and 425 ms here on
// 2026-10-31, against the 12.8 and 19.8 s the two that folded took, and left that drive a grant
// of the copy-heap bound short of the one it means each cold read to hold. It also folds
// documents of a million structs beside the work each step of a drive weighs.
func (h *memoryHarness) oweNoBlockIndex(t *testing.T) int64 {
	t.Helper()
	cleared, err := h.database.Pool.Exec(context.Background(), `
		delete from document_block_versions b
		using artifacts a where a.id = b.artifact_id and a.issue_key = $1
	`, h.issue)
	if err != nil {
		t.Fatalf("clear the block index the drive's documents owe: %v", err)
	}
	return cleared.RowsAffected()
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

// roomCharges asks the server what its room budget charges each room it holds a charge for, with
// the budget's own used and limit, read from the budget itself rather than from the lines a close
// logs (docs.Service.RoomChargesForTest). A room the budget has given a charge back for is absent
// from the map, not zero.
func (p *memoryServer) roomCharges(t *testing.T) (charges map[string]int64, used, limit int64) {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/_test/room-charges", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var read struct {
		Charges map[string]int64 `json:"charges"`
		Used    *int64           `json:"used"`
		Limit   *int64           `json:"limit"`
	}
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &read) != nil || read.Used == nil || read.Limit == nil {
		t.Fatalf("asking the room budget what it charges each room: %d %.300s, want 200 and {charges, used, limit}", answer.status, answer.body)
	}
	return read.Charges, *read.Used, *read.Limit
}

// requireRoomAbsent fails the test unless the server says the document is not live (roomLive).
func requireRoomAbsent(t *testing.T, server *memoryServer, artifactID, what string) {
	t.Helper()
	if server.roomLive(t, artifactID) {
		t.Fatalf("%s, %s, is live in the server, want no room for it", what, artifactID)
	}
}

// settled reports whether a document has settled every write it has had, as waitForSettled waits
// for, without waiting.
func (h *memoryHarness) settled(t *testing.T, artifactID string) bool {
	t.Helper()
	var owed bool
	if err := h.database.Pool.QueryRow(context.Background(),
		`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID,
	).Scan(&owed); err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	return !owed
}

// serverOpenedCloseLine is the line the server logs as it closes a room its own work opened over
// the budget, once nothing holds it (closeIfOpenedOverBudget).
var serverOpenedCloseLine = regexp.MustCompile(`msg="dispatch: the room budget closed a document the server opened over it" room=(\S+) charged_before=\d+ charged=\d+ limit=\d+`)

// requireClosedOnceSettled fails the test unless the room of artifactID, which the server's own work
// opened over a full budget, is gone within thirty seconds of the document's settlement ending: not
// live (roomLive), and logged as closed by the budget (serverOpenedCloseLine).
func requireClosedOnceSettled(t *testing.T, memory *memoryHarness, server *memoryServer, artifactID, what string) {
	t.Helper()
	memory.waitForSettled(t, artifactID)
	settled := time.Now()
	for server.roomLive(t, artifactID) {
		if time.Since(settled) > 30*time.Second {
			t.Fatalf("%s, %s, was still live %s after its settlement ended, want it closed", what, artifactID, time.Since(settled).Round(time.Second))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("%s, %s, was gone %s after its settlement ended", what, artifactID, time.Since(settled).Round(100*time.Millisecond))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if slices.ContainsFunc(serverOpenedCloseLine.FindAllStringSubmatch(server.output.String(), -1), func(match []string) bool { return match[1] == artifactID }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s, %s, is gone, and the server logged no close of it by the budget", what, artifactID)
		}
	}
}

// proseMarkdown is the shape TestAResidentRoomsHeapPerByteOfItsMarkdown weighs rooms in,
// whole sections to at most size bytes: a
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

// requireRefusedEither is requireRefused where two reasons are both correct answers to the same
// update, under the document's own close code: it fails the test unless the update was refused
// with one of them, and is the one it was refused with, so the drive names what it got rather
// than asserting the arithmetic of a particular run.
func requireRefusedEither(t *testing.T, applied bool, closed *gws.CloseError, reasons ...string) string {
	t.Helper()
	if applied {
		t.Fatalf("the update was applied, want it refused with %d and one of %v", contracts.DocumentTooLargeCloseCode, reasons)
	}
	if closed == nil {
		t.Fatalf("the update was not applied and its socket stayed open, want it closed %d with one of %v", contracts.DocumentTooLargeCloseCode, reasons)
	}
	if closed.Code != contracts.DocumentTooLargeCloseCode || !slices.Contains(reasons, closed.Text) {
		t.Fatalf("the refused update's socket was closed with %v, want %d and one of %v", closed, contracts.DocumentTooLargeCloseCode, reasons)
	}
	return closed.Text
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

// requirePodGoMemoryLimit fails the test unless the server took the Go limit a 4 GiB pod gives
// it, which is what bounds the memory the drives weigh against the pod: what the runtime holds
// less what it has released (peakResident). The server sets no collector ratio and the runtime's
// default stands.
func requirePodGoMemoryLimit(t *testing.T, server *memoryServer) {
	t.Helper()
	want := "go_memory_limit_bytes=" + strconv.Itoa(podGoMemoryLimit)
	if !strings.Contains(server.output.String(), want) {
		t.Fatalf("the server's boot did not log %s:\n%s", want, server.output.tail())
	}
}

// requireUnderThePod logs what the step called step left the server holding, and fails the drive
// unless both readings of it stayed within their bound (podPeak, memory_harness_test.go): what a
// pod's kernel would charge, under the pod's memory, and what the runtime had mapped and not
// handed back, within podGoMemoryLimitMargin of the Go memory limit the server ran under.
//
// Neither reading alone proves the step. The charged one subtracts the released heap from VmRSS,
// which under gVisor is right for the scattered releases it keeps resident and wrong, by the
// amount released, for one of 2 MiB or more, which it drops from VmRSS as well; the held one is
// the runtime's own accounting, which no kernel touches, and the limit it is weighed against is
// 0.9 of the pod's memory, so a charge the first undercounts by enough to matter is a step the
// second fails. A step the server's trace holds no sample of was read neither way and fails here
// rather than passing on VmRSS alone.
func requireUnderThePod(t *testing.T, peak podPeak, step string) {
	t.Helper()
	if peak.samples == 0 {
		t.Errorf("%s: the server's memory trace holds no sample of the step, so neither what a pod's kernel would charge nor what its Go memory limit bounds was read of it; VmRSS peaked at %d MiB", step, peak.resident>>20)
		return
	}
	t.Logf("%s: a pod's kernel would charge %d MiB at most, against the pod's %d MiB; the runtime held %d bytes at most of what its %d-byte limit bounds (%+d); VmRSS peaked at %d MiB; %d trace samples",
		step, peak.charged>>20, podMemory>>20, peak.held, peak.limit, peak.held-peak.limit, peak.resident>>20, peak.samples)
	if peak.limit != podGoMemoryLimit {
		t.Errorf("%s: the server ran under a Go memory limit of %d bytes, want the %d a 4 GiB pod gives it: nothing the drive can weigh bounded what the runtime held", step, peak.limit, podGoMemoryLimit)
	}
	if peak.charged >= podMemory {
		t.Errorf("%s: the server's memory peaked at %d MiB as a pod's kernel would charge it, want under the pod's %d MiB", step, peak.charged>>20, podMemory>>20)
	}
	if peak.held > peak.limit+podGoMemoryLimitMargin {
		t.Errorf("%s: the runtime held %d bytes of what its memory limit bounds, %d past the %d-byte limit, want no more than the %d bytes of margin a limit's overshoot takes: VmRSS less the released heap, %d bytes, read the pod's charge too low",
			step, peak.held, peak.held-peak.limit, peak.limit, podGoMemoryLimitMargin, peak.charged)
	}
}

// budgetRefusalLine is the reason the room budget gives a refused browser, as the server logs it
// (errRoomBudgetFull): what the open rooms are charged, what the admissions in flight hold
// reserved, the budget, and the weight of the room this admission would load. An admission
// carries the weight of its own room - read from the stored state under a slot of its own,
// released before the weighing - and holds it as a reservation until that room is charged, so
// what it is weighed against is charged plus reserved plus its own (docs' roomBudget.refusal).
var budgetRefusalLine = regexp.MustCompile(`the open documents fill their memory budget: (\d+) charged and (\d+) reserved of (\d+) bytes, and the room asks (\d+)`)

// budgetRefusal is one refusal the server logged: what its rooms were charged, what its
// admissions in flight held reserved, its budget, and the weight the refused admission asked
// for.
type budgetRefusal struct {
	charged, reserved, limit, asks int64
}

// requireBudgetRefusals is every refusal the server logged, failing the test unless there is one
// and unless each names this package's mirror of the budget.
func requireBudgetRefusals(t *testing.T, server *memoryServer) []budgetRefusal {
	t.Helper()
	var refusals []budgetRefusal
	for _, match := range budgetRefusalLine.FindAllStringSubmatch(server.output.String(), -1) {
		var refusal budgetRefusal
		for index, field := range []*int64{&refusal.charged, &refusal.reserved, &refusal.limit, &refusal.asks} {
			value, err := strconv.ParseInt(match[index+1], 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
			*field = value
		}
		if refusal.limit != roomBudgetBytesForTest {
			t.Errorf("the server refused against a budget of %d bytes, want %d", refusal.limit, roomBudgetBytesForTest)
		}
		refusals = append(refusals, refusal)
	}
	if len(refusals) == 0 {
		t.Fatalf("the server logged no refusal of the room budget:\n%s", server.output.tail())
	}
	return refusals
}

// budgetCloseLine is the line the room budget's sweep logs for each unused room it closes, in the
// budget's own figures (sweepRooms). It names no figure for the admission the sweep was making
// room for: nothing is reserved for a room before it loads, so what an admission waits for is
// the charges coming back within the limit and nothing of its own (docs' roomBudget.refusal).
var budgetCloseLine = regexp.MustCompile(`msg="dispatch: the room budget closed an unused document" room=(\S+) charged_before=(\d+) charged=(\d+) limit=(\d+)`)

// budgetClose is one close the server logged: the room, what the budget charged before and after
// the close, and its limit.
type budgetClose struct {
	room                          string
	chargedBefore, charged, limit int64
}

// sweepLeftOpenLine is the line the budget's sweep logs for each charged room it leaves open,
// and sweepNoStateLine the one it logs where the budget holds a charge for a room no state
// answers (sweepRooms). held_by is the first thing unusedLocked reads that is not clear.
var (
	sweepLeftOpenLine = regexp.MustCompile(`time=(\S+) [^\n]*msg="dispatch: the room budget's sweep left a charged room open" room=(\S+) held_by="?([^"\n]*?)"? pending=(\S+)`)
	sweepNoStateLine  = regexp.MustCompile(`msg="dispatch: the room budget's sweep found no state for a charged room" room=(\S+)`)
)

// sweepHoldsAfter logs what the budget's sweep said of the charged rooms it left open: every
// distinct held_by with a count, and for each room whose browsers went, the holds the sweep
// named for it and how long after its sockets closed it was still naming one. It asserts
// nothing. It is the reading that says whether a room nothing holds is held by its own work -
// a settlement, an append, an apply in flight - which a sweep could drive, or by a browser the
// handler has not forgotten, which wants the departure read from Dispatch's own socket close
// (INFRA-179).
func sweepHoldsAfter(t *testing.T, server *memoryServer, rooms []string, leftAt map[string]time.Time) {
	t.Helper()
	output := server.output.String()
	counts := map[string]int{}
	type hold struct {
		at   time.Time
		held string
	}
	byRoom := map[string][]hold{}
	for _, match := range sweepLeftOpenLine.FindAllStringSubmatch(output, -1) {
		counts[match[3]]++
		at, err := time.Parse(time.RFC3339Nano, match[1])
		if err != nil {
			continue
		}
		byRoom[match[2]] = append(byRoom[match[2]], hold{at: at, held: match[3]})
	}
	if len(counts) == 0 {
		t.Logf("the budget's sweep left no charged room open, or logged none: %d rooms whose browsers went", len(rooms))
	}
	for held, count := range counts {
		t.Logf("the sweep left a charged room open %d times held by %q", count, held)
	}
	if missing := len(sweepNoStateLine.FindAllString(output, -1)); missing > 0 {
		t.Logf("the sweep found no state for a charged room %d times", missing)
	}
	for _, room := range rooms {
		holds := byRoom[room]
		if len(holds) == 0 {
			t.Logf("the sweep never left room %s open after its browsers went at %s: it was closable whenever it looked",
				room, leftAt[room].UTC().Format(time.RFC3339Nano))
			continue
		}
		reasons := map[string]int{}
		for _, one := range holds {
			reasons[one.held]++
		}
		last := holds[len(holds)-1]
		t.Logf("room %s, whose browsers went at %s: the sweep left it open %d times, %v, the last %s after they went, held by %q",
			room, leftAt[room].UTC().Format(time.RFC3339Nano), len(holds), reasons,
			last.at.Sub(leftAt[room]).Round(time.Millisecond), last.held)
	}
}

// budgetCloses is every close of an unused room the server has logged, in the order it logged them.
func budgetCloses(t *testing.T, server *memoryServer) []budgetClose {
	t.Helper()
	var closes []budgetClose
	for _, match := range budgetCloseLine.FindAllStringSubmatch(server.output.String(), -1) {
		closed := budgetClose{room: match[1]}
		for index, field := range []*int64{&closed.chargedBefore, &closed.charged, &closed.limit} {
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

// closesOf is the closes the server has logged of rooms (budgetCloses), once they number want or
// the patience below has passed: ten seconds and one for each room wanted, since a sweep closes
// the rooms one at a time and each close flushes a room's updates to the store and compacts them
// before the room's charge leaves (evictRoom) - the thirty-nine rooms of a mebibyte
// TestTheRoomBudgetDoesNotOverchargeTypingAndPasting left at the element price of 470 took 25 s to
// close here on 2026-10-29,
// where sixteen had closed within the ten. A sweep logs a close once its eviction returns, after
// the budget has taken the room's charge back and any admission waiting on it has gone on, and the
// line reaches the test through the pipe the server's output is copied through. A caller wanting
// none takes what has been logged by now.
func closesOf(t *testing.T, server *memoryServer, rooms []string, want int) []budgetClose {
	t.Helper()
	for deadline := time.Now().Add(10*time.Second + time.Duration(want)*time.Second); ; time.Sleep(100 * time.Millisecond) {
		closes := slices.DeleteFunc(budgetCloses(t, server), func(made budgetClose) bool { return !slices.Contains(rooms, made.room) })
		if len(closes) >= want || time.Now().After(deadline) {
			return closes
		}
	}
}

// documentSocketsEnded is how many of room's document sockets the server has reported ended: its
// access log holds one line for each (accesslog.Message), written in the wrapper's defer once the
// handler it wraps has returned (accesslog.Handler), which is after that handler has forgotten the
// browser (docs.Service.ServeHTTP, removeConnection). A refused browser's socket is one of them,
// since the refusal completes the upgrade only to close it (refuseOverRoomBudget).
func documentSocketsEnded(server *memoryServer, room string) int {
	return strings.Count(server.output.String(), `msg="dispatch: request" method=GET route=/ws/doc/{room} path=/ws/doc/`+room+" ")
}

// requireBrowserForgotten waits for the server's report that want of room's document sockets have
// ended (documentSocketsEnded), which is the end of their browsers' departures and not the socket's
// close: ygo reports a departure at the end of the peer's teardown, the report runs the room's
// settlement there (settleLastPeer), and the handler forgets the browser once that returns
// (removeConnection), which is what leaves the room unused for the budget to close (unusedLocked).
// A drive that means "the room this browser left is the budget's to close" waits for this rather
// than for a hold of its own: a mebibyte's settlement took 10 to 20 s here with the pod's rooms
// full, where the budget's own wait for it is 10 s (admissionWait), so an admission placed inside
// that window is refused however long the hold before it was.
func requireBrowserForgotten(t *testing.T, server *memoryServer, room string, want int) {
	t.Helper()
	for deadline := time.Now().Add(browserAnswerWithin); ; time.Sleep(100 * time.Millisecond) {
		if ended := documentSocketsEnded(server, room); ended >= want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("the server reported %d of %s's %d document sockets ended within %s, want its browsers' departures over:\n%s", ended, room, want, browserAnswerWithin, server.output.tail())
		}
	}
}

// storedShape is a stored document a struct-dense drive copies (cloneDocuments), and what the
// room budget charges a room that loads it, computed here as the server computes it: the bytes
// of the stored state a load decodes, at the one price and at both copies (roomWeightForTest).
// Those bytes are the whole stored history as the store hands it over, folded where it holds
// several rows, which is exactly what a load reads and decodes (docs' loadDecodedHistory,
// chargeLoadedRoom) - so the drive reads it the way a load does rather than summing the rows.
//
// It carries the structs of that state besides (docs.StateStructsForTest), which the drives at
// the decode cap weigh themselves against (requireAtTheDecodeCap), and its latest version's
// markdown, counted and measured in bytes, which no price reads now and which the drives log.
type storedShape struct {
	name       string
	id         string
	structs    int64
	stateBytes int
	rendered   int
	elements   int
	weight     int64
}

func (h *memoryHarness) storedShape(t *testing.T, name, artifactID string) storedShape {
	t.Helper()
	ctx := context.Background()
	shape := storedShape{name: name, id: artifactID}
	state := h.storedState(t, artifactID)
	structs, err := docs.StateStructsForTest(state)
	if err != nil {
		t.Fatalf("walk the stored state of %s: %v", name, err)
	}
	shape.structs, shape.stateBytes = structs, len(state)
	var rows int
	if err := h.database.Pool.QueryRow(ctx,
		`select count(*) from doc_updates where artifact_id = $1`, artifactID,
	).Scan(&rows); err != nil {
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
	shape.weight = roomWeightForTest(int64(shape.stateBytes))
	t.Logf("%s: %d bytes of stored state in %d rows holding %d structs, its latest version %d bytes making %d elements; a room that loads it is charged %d bytes, its state at %d bytes a byte over both copies",
		name, shape.stateBytes, rows, shape.structs, shape.rendered, shape.elements, shape.weight,
		bytesOfHeapAStateByteForTest*roomCopiesForTest)
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
// agentRewrittenState rewrites one:
// each write a new version of the same document of 16,384 headings of fresh text at both halves of
// the byte and element bounds, until the decode cap refuses a write. Its history then stands at
// the cap, about a million structs rendering a mebibyte.
//
// The rows those writes store are then re-stored as the one state they make, as a rebuild stores
// its seed (storedStates, docs' PgVersioned.RebuildTx). The rows do not sum to the structs of
// the state they make: integration splits items that no row holds, and a compaction's fold of
// them carries those splits where the rows do not. Measured on 2026-11-03, the writes' 27 rows
// held 950,272 structs and the 14 rows the fold of the room's close left held 1,012,416, against
// the 1,048,576 of the state at the decode cap. A room is charged the structs of what it loads
// (docs' storedRoomReservation), so the re-stored state is both the heavier room and the one
// this fixture is built to stand at the cap with (requireAtTheDecodeCap), as the long-edited
// document stored beside it does.
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
	state := h.storedState(t, artifactID)
	ctx := context.Background()
	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin re-storing the agent-rewritten document's state: %v", err)
	}
	if _, err := docs.NewPgVersioned(h.database).RebuildTx(ctx, tx, artifactID, state); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-store the agent-rewritten document's %d-byte state: %v", len(state), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the agent-rewritten document's state: %v", err)
	}
	return artifactID
}

// longEditedTypedTo is the long-edited fixture typed on as its namesake in the docs package types
// it (longEditedAtCapState): its first paragraph typed into at its
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

// paddedToTheRoomsStateCap is state with a text nested under its margin, pasted in one
// transaction until the whole state stands 16 KiB short of maxRoomStateBytes: the heaviest state
// a room may hold under the cut, and so the heaviest whole-state handoff a browser of such a
// room can send. Nothing a browser does can take a room past the cap (peerApplyAllowance's state
// cap), so a drive that wants a room there stores the state itself (storedStates), as a rebuild
// does.
func paddedToTheRoomsStateCap(t *testing.T, state []byte) []byte {
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
	left := maxRoomStateBytesForTest - len(crdt.EncodeStateAsUpdateV1(doc, nil)) - 16<<10
	if left < 0 {
		t.Fatalf("the state is %d bytes past the room's state cap less 16 KiB, want it under", -left)
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
// does.
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
// document, and the budget's reason (refuseOverRoomBudget; budgetRefusalLine).
var roomRefusalLine = regexp.MustCompile(`msg="dispatch: refuse a document socket; the open documents fill their memory budget" room=(\S+) error="the open documents fill their memory budget: (\d+) charged and (\d+) reserved of (\d+) bytes, and the room asks (\d+)"`)

// budgetRefusalsByRoom is the last refusal the server logged for each document it refused a
// browser of.
func budgetRefusalsByRoom(t *testing.T, server *memoryServer) map[string]budgetRefusal {
	t.Helper()
	refusals := map[string]budgetRefusal{}
	for _, match := range roomRefusalLine.FindAllStringSubmatch(server.output.String(), -1) {
		var refusal budgetRefusal
		for index, field := range []*int64{&refusal.charged, &refusal.reserved, &refusal.limit, &refusal.asks} {
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

// loadSlotLine is the line the server logs as it gives back a slot of the pod's load concurrency
// that waited a tenth of a second or more for it (logLoadSlot,
// internal/dispatch/docs/copy_heap.go): how many slots the pod has, how long this holder waited
// and how long it held one, the line's time being when it gave it back. A slot no holder waited
// for leaves no line, so what a drive reads from these is the queue the slots made and nothing
// else.
var loadSlotLine = regexp.MustCompile(`time=(\S+) level=INFO msg="dispatch: a load slot was given back" bound=load slots=(\d+) waited=(\S+) held=(\S+)`)

// loadSlotHold is one slot the server logged (loadSlotLine), held from from to to.
type loadSlotHold struct {
	slots        int64
	waited, held time.Duration
	from, to     time.Time
}

// loadSlotHolds is every slot logged in output, in the order the server gave them back.
func loadSlotHolds(t *testing.T, output string) []loadSlotHold {
	t.Helper()
	var holds []loadSlotHold
	for _, match := range loadSlotLine.FindAllStringSubmatch(output, -1) {
		var hold loadSlotHold
		var err error
		if hold.to, err = time.Parse(time.RFC3339Nano, match[1]); err != nil {
			t.Fatalf("parse %q: %v", match[0], err)
		}
		if hold.slots, err = strconv.ParseInt(match[2], 10, 64); err != nil {
			t.Fatalf("parse %q: %v", match[0], err)
		}
		for index, field := range []*time.Duration{&hold.waited, &hold.held} {
			if *field, err = time.ParseDuration(match[index+3]); err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
		}
		hold.from = hold.to.Add(-hold.held)
		holds = append(holds, hold)
	}
	return holds
}

// logLoadSlots logs how many holders of the pod's load concurrency waited for a slot under what,
// the longest any waited and the longest any held one, and fails the drive where a line names a
// pod of other than the slots this package mirrors.
func logLoadSlots(t *testing.T, what string, holds []loadSlotHold) {
	t.Helper()
	var waited, held time.Duration
	for _, hold := range holds {
		if hold.slots != loadSlotsForTest {
			t.Errorf("the server logged a slot of a pod of %d slots, want the %d this package mirrors", hold.slots, loadSlotsForTest)
		}
		waited, held = max(waited, hold.waited), max(held, hold.held)
	}
	t.Logf("%s: %d holders of the pod's %d load slots waited for one; the longest wait %s, the longest hold %s",
		what, len(holds), loadSlotsForTest, waited.Round(time.Millisecond), held.Round(time.Millisecond))
}
