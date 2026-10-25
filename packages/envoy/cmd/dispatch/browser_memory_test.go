//go:build memory

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

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
	// roomBudgetBytes (room_budget.go:33), bytesOfHeapAnElement (:45), roomCopies (:180),
	// maxPeerInsertAtTheBound (peer_update.go:64), maxPeerByteCreep (:69) and maxMessageBytes
	// (:88). No drive takes a mirror on trust. The server names its budget and what each refused
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
	maxMessageBytesForTest         = 24 << 20

	// roomCountIntervalForTest mirrors roomCountInterval (internal/dispatch/docs/tree.go:434), the
	// least time between two counts of one room's elements, which is how stale a room's charge
	// can be. The drives that grow rooms send a round of growth no more often than this and two
	// seconds, so each round's counts have landed before the next is sent, as they would under a
	// person's typing; a wrong mirror changes only how far growth runs past the budget before it
	// is refused, which the bound on peak resident memory catches.
	roomCountIntervalForTest = 15 * time.Second
)

// One browser's update is bounded by bytes, over a real document socket of a real server
// (Service.weighPeerUpdate, internal/dispatch/docs/peer_update.go:117-144): an update that would
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

// The room budget refuses the browser its own arithmetic says it has no room for: documents of a
// mebibyte of prose, each edited once, opened until one is refused with 4503 and DOC_BUSY, under the
// Go limit a 4 GiB pod gives the server. A room is charged at both copies from its admission
// (roomCopies, internal/dispatch/docs/room_budget.go:180), so the refusals begin within one room of
// the budget divided by what one such room reserves - its stored version's elements at the price of
// one, at both copies - computed here from the server's constants and held to the numbers the server
// logs, both when the browsers come one after another and when fifty open at once.
//
// One after another, five of the browsers leave once they have edited. Each refusal while their
// rooms are still charged starts a sweep that closes one of them to make room for the refused
// browser (sweepRooms), and that browser's next attempt, as the editor's backoff makes it, is
// admitted. Each close is read twice before that attempt: from the line the server logs for it, in
// the budget's own figures, and from the room's absence, which the rebuild route reports (roomLive).
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
// round of headings at a time, until the counts of their elements take the charges past the limit,
// a sweep finds nothing to close, and the budget is full (roomBudget.full): the next round is
// refused.
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
	// beside the document's whole delete set (sjawhar/ygo v1.51.3-sami.2, crdt/doc.go:459-465), so
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
// part in answering: the rebuild route refuses a document live in this server with 409
// DOCUMENT_LIVE before it reads anything, and one that is not live, whose history loads, with 409
// DOCUMENT_LOADS, and rebuilds nothing either way (docs.Service.RebuildDocument,
// internal/dispatch/docs/rebuild.go:65-75). For a document that is not live, the answer costs the
// server a load of its stored history.
func (p *memoryServer) roomLive(t *testing.T, artifactID string) bool {
	t.Helper()
	answer := p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/rebuild", "application/json", strings.NewReader("{}"), http.Header{"X-Dispatch-User": {"alice"}})
	var refusal struct{ Code string }
	if answer.status == http.StatusConflict && json.Unmarshal(answer.body, &refusal) == nil {
		switch refusal.Code {
		case "DOCUMENT_LIVE":
			return true
		case "DOCUMENT_LOADS":
			return false
		}
	}
	t.Fatalf("asking whether document %s is live: %d %.300s, want 409 DOCUMENT_LIVE or DOCUMENT_LOADS", artifactID, answer.status, answer.body)
	return false
}

// requireRoomAbsent fails the test unless the server says the document is not live (roomLive).
func requireRoomAbsent(t *testing.T, server *memoryServer, artifactID, what string) {
	t.Helper()
	if server.roomLive(t, artifactID) {
		t.Fatalf("%s, %s, is live in the server, want no room for it", what, artifactID)
	}
}

// proseMarkdown is the shape TestAResidentRoomsHeapPerByteOfItsMarkdown weighs rooms in
// (internal/dispatch/docs/room_heap_test.go:185-211), whole sections to at most size bytes: a
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
// (errRoomBudgetFull, internal/dispatch/docs/room_budget.go:222-254).
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
// budget's own figures (sweepRooms, internal/dispatch/docs/room_budget.go:700-728).
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
