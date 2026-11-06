package docs

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The hook is driven here at its own level, as ygo drives it: the room's document, the update's
// bytes, and an apply that applies the update to that document exactly as the room's own does
// (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:84-98). What a real browser meets
// over a real socket is the real-process drive; what these rows pin is the rule itself.
//
// Every refusal but the pressure's is the apply's own now: the hook hands ygo an allowance
// (crdt.WithApplyBudget) and ygo counts what the apply decodes, integrates and adds against it,
// rolling the whole apply back where a count passes its field. So the apply runs for all but the
// two refusals the hook makes before it, and a refused room is left byte for byte as it was.
func TestAPeerUpdateIsWeighedAgainstTheRoomsLastMeasurement(t *testing.T) {
	atTheBound := proseAtLeast(pmdoc.MaxDocumentBytes)
	pastTheCreepCap := proseAtLeast(pmdoc.MaxDocumentBytes + maxPeerByteCreep)
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	for _, row := range []struct {
		name string
		// markdown is the document the room loaded with, and leastBytes what its rendering must
		// measure for the row to drive the state it is written for.
		markdown   string
		leastBytes int
		// busy is the room budget's signal that it is full and nothing can be closed.
		busy bool
		// update is the one update the room is handed, built against the room's own document.
		update func(t *testing.T, room *crdt.Doc) []byte
		// refused is the code of the close the hook answers, zero for none, and reason its reason,
		// empty for the code's own; applied is whether the room took the update; unapplied is
		// whether the hook refused it without calling the apply at all; changes is whether the
		// room's document is any different for it, which a whole-state message of what the room
		// already holds is not.
		refused                     int
		reason                      string
		applied, unapplied, changes bool
	}{
		{
			name:     "a keystroke under the bound applies",
			markdown: ordinary,
			update:   func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") },
			applied:  true,
			changes:  true,
		},
		{
			name:       "a 9 KiB paste at the byte bound is refused",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return peerTyping(t, room, strings.Repeat("p", 9<<10))
			},
			refused: contracts.DocumentTooLargeCloseCode,
		},
		{
			name:       "a 7 KiB insert at the byte bound applies",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return peerTyping(t, room, strings.Repeat("p", 7<<10))
			},
			applied: true,
			changes: true,
		},
		{
			name:       "one byte past the creep cap is refused",
			markdown:   pastTheCreepCap,
			leastBytes: pmdoc.MaxDocumentBytes + maxPeerByteCreep,
			update:     func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") },
			refused:    contracts.DocumentTooLargeCloseCode,
		},
		{
			name:       "a delete past the creep cap applies",
			markdown:   pastTheCreepCap,
			leastBytes: pmdoc.MaxDocumentBytes + maxPeerByteCreep,
			update:     func(t *testing.T, room *crdt.Doc) []byte { return peerDeleting(t, room, 64) },
			applied:    true,
			changes:    true,
		},
		{
			name:       "the whole live state the room already holds applies",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return crdt.EncodeStateAsUpdateV1(room, nil)
			},
			applied: true,
		},
		{
			name:       "an update that does not decode is refused as one ygo will not read",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update:     func(*testing.T, *crdt.Doc) []byte { return []byte{0x01, 0xff, 0xff, 0xff} },
			refused:    contracts.DocumentTooLargeCloseCode,
			reason:     contracts.DocumentUpdateRefusedCloseReason,
		},
		{
			name:     "an update that does not decode under the bound is refused the same way",
			markdown: ordinary,
			update:   func(*testing.T, *crdt.Doc) []byte { return []byte{0x01, 0xff, 0xff, 0xff} },
			refused:  contracts.DocumentTooLargeCloseCode,
			reason:   contracts.DocumentUpdateRefusedCloseReason,
		},
		{
			name:     "a keystroke while the room budget is full is refused as transient",
			markdown: ordinary,
			busy:     true,
			update:   func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") },
			refused:  contracts.DocumentBusyCloseCode,
		},
		{
			name:     "a delete while the room budget is full applies",
			markdown: ordinary,
			busy:     true,
			update:   func(t *testing.T, room *crdt.Doc) []byte { return peerDeleting(t, room, 3) },
			applied:  true,
			changes:  true,
		},
		{
			name:     "the whole live state the room already holds applies while the room budget is full",
			markdown: ordinary,
			busy:     true,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return crdt.EncodeStateAsUpdateV1(room, nil)
			},
			applied: true,
		},
		{
			name:     "an update that leaves the document no longer but adds elements while the room budget is full is refused as transient",
			markdown: ordinary,
			busy:     true,
			update:   peerReshaping,
			refused:  contracts.DocumentBusyCloseCode,
		},
		{
			name:       "a 7 KiB insert at the byte bound while the room budget is full is refused as transient",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			busy:       true,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return peerTyping(t, room, strings.Repeat("p", 7<<10))
			},
			refused: contracts.DocumentBusyCloseCode,
		},
		{
			name:       "a delete at the byte bound while the room budget is full applies",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			busy:       true,
			update:     func(t *testing.T, room *crdt.Doc) []byte { return peerDeleting(t, room, 64) },
			applied:    true,
			changes:    true,
		},
		{
			// The pressure sets both fields of the allowance to nothing and names itself for
			// both, so while the budget is full a paste the byte bound would refuse on its own
			// is refused as transient: the editor keeps the edit and sends it once the pressure
			// passes, where it meets the bound's permanent refusal.
			name:       "a 9 KiB paste at the byte bound while the room budget is full is refused as transient",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			busy:       true,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return peerTyping(t, room, strings.Repeat("p", 9<<10))
			},
			refused: contracts.DocumentBusyCloseCode,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, row.markdown, row.leastBytes)
			service.budget.full.Store(row.busy)
			update := row.update(t, room)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			applied, called := false, false
			err := service.weighPeerUpdate(
				websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update},
				func() error {
					called = true
					if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
						return err
					}
					applied = true
					return nil
				},
			)
			// A live room's own update observer holds its replica, which the service lists weakly
			// (keepReplica); nothing else here does, so the measurement would be collectable from
			// under the hook.
			runtime.KeepAlive(replica)
			var refusal *websocket.PeerClose
			switch {
			case row.refused != 0:
				if !errors.As(err, &refusal) {
					t.Fatalf("the hook answered %v, want a close refusing the update", err)
				}
				reason := row.reason
				if reason == "" {
					reason = contracts.DocumentTooLargeCloseReason
					if row.refused == contracts.DocumentBusyCloseCode {
						reason = contracts.DocumentBusyCloseReason
					}
				}
				if refusal.Code != row.refused || refusal.Reason != reason {
					t.Fatalf("the hook closed with %d %q, want %d %q", refusal.Code, refusal.Reason, row.refused, reason)
				}
				if row.unapplied && called {
					t.Fatal("the hook called the apply for an update it refused before any apply")
				}
			default:
				if err != nil {
					t.Fatalf("the hook answered %v, want the update admitted", err)
				}
			}
			if applied != row.applied {
				t.Fatalf("the room applied the update: %t, want %t", applied, row.applied)
			}
			if changed := string(crdt.EncodeStateAsUpdateV1(room, nil)) != string(before); changed != row.changes {
				t.Fatalf("the room's document changed: %t, want %t", changed, row.changes)
			}
		})
	}
}

// peerUpdateRoom is a room holding markdown, bounded as every room's document is (newRoomApplies,
// newRoomScans), with the measurement its load publishes (keepReplica), and nothing else of the
// service: the hook reads the room's document and that measurement alone. leastBytes is what the
// rendering must measure for its caller's state, so a row written for a document at a bound fails
// rather than passing under one. The replica is returned because the service lists it weakly, and
// its caller is what holds it.
func peerUpdateRoom(t *testing.T, markdown string, leastBytes int) (*Service, *crdt.Doc, *renderedReplica) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	service := calibratedService()
	replica := service.keepReplica(room, nil, loadRendering(rendering), nil)
	bytes, measured := replica.size()
	if !measured || bytes < int64(leastBytes) {
		t.Fatalf("the room published %d bytes, want at least %d", bytes, leastBytes)
	}
	return service, room, replica
}

// peerTyping is the update a browser sends that inserts text at the head of the room's first text,
// built on a copy of the room as the browser's own document is.
func peerTyping(t *testing.T, room *crdt.Doc, text string) []byte {
	t.Helper()
	browser, into := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, text, nil) })
	return crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
}

// peerDeleting is the update a browser sends that deletes the first characters of the room's first
// text, which inserts nothing.
func peerDeleting(t *testing.T, room *crdt.Doc, characters int) []byte {
	t.Helper()
	return peerDeletingAt(t, room, 0, characters)
}

// peerDeletingAt is the update a browser sends that deletes characters of the room's first text
// from index at, which inserts nothing.
func peerDeletingAt(t *testing.T, room *crdt.Doc, at, characters int) []byte {
	t.Helper()
	browser, from := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) { from.Delete(txn, at, characters) })
	return crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
}

// peerReshaping is the update a browser sends that cuts the room's first paragraph short and adds
// one-word paragraphs after it, leaving the document's rendering no longer than it was while it
// makes more elements: the update a test of its bytes would admit, and which grows what the room
// holds all the same.
func peerReshaping(t *testing.T, room *crdt.Doc) []byte {
	t.Helper()
	browser, _ := peerDocument(t, room)
	before, err := renderDocument(browser)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := treeOf(browser)
	if err != nil {
		t.Fatal(err)
	}
	first := slices.IndexFunc(tree.Children, func(block *pmdoc.Node) bool { return block.Type == "paragraph" })
	if first < 0 {
		t.Fatal("the document holds no paragraph to cut short")
	}
	tree.Children[first].Children = []*pmdoc.Node{{Type: "text", Text: "One."}}
	var added []*pmdoc.Node
	for range 8 {
		added = append(added, &pmdoc.Node{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "a"}}})
	}
	tree.Children = slices.Insert(tree.Children, first+1, added...)
	pmdoc.EnsureBlockIDs(tree)
	fragment := browser.GetXmlFragment(fragmentName)
	if err := browser.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	after, err := renderDocument(browser)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before) || pmdoc.MeasureDocument(after).Elements <= pmdoc.MeasureDocument(before).Elements {
		t.Fatalf("the reshaped document renders %d bytes making %d elements, from %d making %d: want no more bytes and more elements",
			len(after), pmdoc.MeasureDocument(after).Elements, len(before), pmdoc.MeasureDocument(before).Elements)
	}
	return crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
}

// peerDocument is a copy of the room as a browser holds it, and the first text in it, the one a
// browser's keystroke lands in.
func peerDocument(t *testing.T, room *crdt.Doc) (*crdt.Doc, *crdt.YXmlText) {
	t.Helper()
	browser := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatal(err)
	}
	text := firstText(browser.GetXmlFragment(fragmentName))
	if text == nil {
		t.Fatal("the document holds no text to type into")
	}
	return browser, text
}

// firstText is the first text in node's document order, however deep it is nested. ygo's own node
// interface is unexported (sjawhar/ygo v1.55.1-sami.1, crdt/yxml.go:16), so the walk takes each
// child as it comes.
func firstText(node any) *crdt.YXmlText {
	var children []any
	switch node := node.(type) {
	case *crdt.YXmlText:
		return node
	case *crdt.YXmlFragment:
		for _, child := range node.Children() {
			children = append(children, child)
		}
	case *crdt.YXmlElement:
		for _, child := range node.Children() {
			children = append(children, child)
		}
	}
	for _, child := range children {
		if text := firstText(child); text != nil {
			return text
		}
	}
	return nil
}

// proseAtLeast is markdown of ordinary paragraphs whose rendering is at least bytes bytes. Its
// paragraphs end in a word rather than a space, which the renderer would drop, so what it builds
// and what the room renders of it are the same length.
func proseAtLeast(bytes int) string {
	paragraph := strings.Repeat("word ", 199) + "word\n\n"
	var markdown strings.Builder
	for markdown.Len() < bytes+len(paragraph) {
		markdown.WriteString(paragraph)
	}
	return markdown.String()
}

// The floor on one websocket message (maxMessageBytes) has to clear the largest frame the editor
// itself sends, which is a browser handing over a document's whole live state on its first sync:
// updates are never split across frames, so a floor under that frame would close an editor of a
// large document in a reconnect loop no person's edit could leave. The frame this measures is the
// heaviest document the server's own writes can leave - its markdown at pmdoc.MaxDocumentBytes
// making pmdoc.MaxDocumentElements, its margin at maxMarginBytes and its anchor marks at
// maxAnchorBytes - whose whole state is far more than its markdown: growth.go records thirty-two
// suggestions of 900 KB that left the markdown as it was. What this does not weigh is the items a
// long edit history tombstones, which no rendering shows and which the state cap
// (maxRoomStateBytes) and the decode cap (maxUpdateItems) bound instead;
// TestTheHeaviestWholeStateFrameTheServerStoresFitsTheMessageFloor weighs those.
func TestTheHeaviestWholeStateFrameFitsTheMessageFloor(t *testing.T) {
	frame := len(crdt.EncodeStateAsUpdateV1(heaviestDocument(t), nil))
	t.Logf("the heaviest whole-state frame is %d bytes; the message floor is %d", frame, maxMessageBytes)
	if frame*4 > maxMessageBytes {
		t.Fatalf("the heaviest whole-state frame is %d bytes, past a quarter of the %d-byte message floor: "+
			"measure it again and set the floor at four times it, or take it back to the design if four times it passes the 64 MiB ygo allowed before",
			frame, maxMessageBytes)
	}
}

// serverStoredStateFrame is the heaviest whole-state frame the server's own writes store, measured
// on 2026-10-26 and 2026-10-27 at 12,555,674 and 12,630,814 bytes: a document an agent rewrote
// whole through the API until the decode cap refused a write, whose state carries every rewrite's
// tombstones. It is one of the two terms maxMessageBytes is set from (write_queue.go), and the
// writes' random client ids move it from run to run, so the test holds the built frame within a
// hundredth of it.
const serverStoredStateFrame = 12_598_044

// The floor clears the larger of four times the heaviest frame the server's own writes store and
// four times the most state one room may hold (maxRoomStateBytes), and stays under the 64 MiB ygo
// allowed before (maxMessageBytes). A room at the state cap hands over a frame about the size of
// its state, so four such handoffs fit the floor. The heaviest the server stores is a document an
// agent rewrote whole through the API until the decode cap refused a write: built here, its frame
// is held within a hundredth of serverStoredStateFrame, the figure the floor is set from.
func TestTheHeaviestWholeStateFrameTheServerStoresFitsTheMessageFloor(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	state, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	structs, rendered, _ := structsAndRendering(t, state)
	floor := max(4*serverStoredStateFrame, 4*maxRoomStateBytes)
	t.Logf("an agent's rewrites stored %d structs rendering %d bytes in a %d-byte frame; the state cap is %d; the floor is %d, from four times %d and four times the state cap",
		structs, rendered, len(state), maxRoomStateBytes, maxMessageBytes, serverStoredStateFrame)
	if len(state) > serverStoredStateFrame+serverStoredStateFrame/100 {
		t.Fatalf("the stored frame is %d bytes, past the %d the floor is set from by more than a hundredth: measure it again and set the floor from it", len(state), serverStoredStateFrame)
	}
	// The floor is ygo's own old default, 64 MiB, and the state cap is a quarter of it, so the
	// second term is the floor exactly and this holds the first: four times the heaviest frame
	// the server's own writes store fits inside it.
	if maxMessageBytes < floor || maxMessageBytes > 64<<20 {
		t.Fatalf("the floor is %d bytes, want it at or above the %d the larger term asks, and at most the 64 MiB ygo allowed before", maxMessageBytes, floor)
	}
}

// heaviestStateAtTheDecodeCap is the heaviest state measured of a document a person typed to the
// decode cap: the branch's long-edited fixture, 14,427,285 bytes on 2026-11-06. It is the figure
// the state cap and the message floor have to clear, and the one they did not clear when they
// were written apart - a 13.5 MB cap under a 54 MiB floor left this document unable to take a
// byte from any browser and unable to hand its state back, because four times it was over the
// floor meant to clear it (write_queue.go, maxRoomStateBytes).
const heaviestStateAtTheDecodeCap = 14_427_285

// The state cap clears the heaviest state a document at the decode cap holds, and the message
// floor clears four of them, so a room that reaches the cap can still hand its whole state over
// and still take an edit. This is the arithmetic alone, so it runs everywhere and in no time;
// TestTheHeaviestStateAtTheDecodeCapIsStillTheFigureBelow builds the document and holds the
// figure itself to what it measures.
func TestTheHeaviestStateAtTheDecodeCapFitsTheRoomsStateCapAndTheFloor(t *testing.T) {
	if heaviestStateAtTheDecodeCap >= maxRoomStateBytes {
		t.Fatalf("the heaviest state at the decode cap is %d bytes and the room's state cap %d: a document at the cap could take no byte from any browser",
			heaviestStateAtTheDecodeCap, maxRoomStateBytes)
	}
	if 4*heaviestStateAtTheDecodeCap >= maxMessageBytes {
		t.Fatalf("four times the heaviest state at the decode cap is %d bytes and the message floor %d: that document's handoff would be closed by the floor meant to clear it",
			4*heaviestStateAtTheDecodeCap, maxMessageBytes)
	}
	if maxRoomStateBytes != maxMessageBytes/4 {
		t.Fatalf("the state cap is %d and a quarter of the floor is %d: the two are one rule and are derived from one figure so they cannot drift",
			maxRoomStateBytes, maxMessageBytes/4)
	}
}

// The figure above is still what a document typed to the decode cap holds. It builds the
// fixture, which takes about a million writes, so it carries the bounds' race skip as the other
// fixtures at the cap do; the arithmetic it feeds is held by the test above at every build.
func TestTheHeaviestStateAtTheDecodeCapIsStillTheFigureBelow(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	state := longEditedAtCapState(t)
	t.Logf("a document typed to the decode cap holds %d bytes of state against the %d recorded; the state cap is %d and the floor %d",
		len(state), heaviestStateAtTheDecodeCap, maxRoomStateBytes, maxMessageBytes)
	if len(state) > heaviestStateAtTheDecodeCap+heaviestStateAtTheDecodeCap/100 {
		t.Fatalf("the state is %d bytes, past the %d recorded by more than a hundredth: measure it again and set heaviestStateAtTheDecodeCap from it",
			len(state), heaviestStateAtTheDecodeCap)
	}
}

// roomFramesUnderTheBudget is the most bytes of whole-state frames the rooms of a room budget of
// budget bytes can send when the budget is charged to its limit: a room is charged its tracked
// state at bytesOfHeapAStateByte in each of roomCopies copies (roomWeight), and a room's
// whole-state frame is about the size of its state, so the rooms share budget / 28 bytes of state
// between them whatever their shapes.
func roomFramesUnderTheBudget(budget int64) int64 {
	return budget / (bytesOfHeapAStateByte * roomCopies)
}

// The per-peer write-queue budget (peerWriteQueueBytes) clears every frame a room can send one
// socket, since a frame arriving while the budget or more waits closes the socket it arrives for,
// honest or not; and twenty sockets that never read, each holding the budget and the frame that
// crossed it, stay inside the Go memory limit beside a room budget charged to its limit. The
// frames those twenty hold are bounded by that same budget (roomFramesUnderTheBudget), which is
// why twenty fit at all: twenty frames at the message floor would be 1,132,462,080 bytes.
//
// The budget is the one bound that does not follow the Go memory limit, so the arithmetic is
// weighed at the calibration and at the production pod's limit, where the room budget and the
// frames its rooms can send are eight times larger and these twenty queues are not. The
// calibration's own figures, which the constant's comment quotes, are held here exactly, and so
// is the one the comment makes of 64 MiB: under the one price it would fit the limit too, and the
// budget stays at 60 MiB because that is where the measurements were taken.
func TestTheWriteQueueBudgetsArithmetic(t *testing.T) {
	const sockets = 20
	if peerWriteQueueBytes <= maxMessageBytes {
		t.Errorf("the write-queue budget is %d bytes, want it past the %d-byte message floor: a socket holding one such frame would be closed by the next frame it is sent",
			peerWriteQueueBytes, maxMessageBytes)
	}
	for _, row := range []struct {
		name  string
		limit int64
	}{
		{"the calibration", calibratedGoMemoryLimitBytes},
		{"the production pod's limit", productionGoMemoryLimitBytes},
	} {
		t.Run(row.name, func(t *testing.T) {
			bounds := BoundsForLimit(row.limit)
			frames := roomFramesUnderTheBudget(bounds.RoomBudget)
			held := sockets*bounds.PeerWriteQueue + frames + bounds.RoomBudget
			t.Logf("%d sockets hold %d x %d and the %d bytes of frames their rooms can send under a full room budget of %d, %d in all, against the %d-byte Go memory limit",
				sockets, sockets, bounds.PeerWriteQueue, frames, bounds.RoomBudget, held, bounds.Limit)
			if held > bounds.Limit {
				t.Errorf("%d sockets that never read hold %d bytes beside a full room budget, %d past the %d-byte Go memory limit: lower the budget",
					sockets, held, held-bounds.Limit, bounds.Limit)
			}
		})
	}
	calibrated := BoundsForLimit(calibratedGoMemoryLimitBytes)
	frames := roomFramesUnderTheBudget(calibrated.RoomBudget)
	held := sockets*calibrated.PeerWriteQueue + frames + calibrated.RoomBudget
	if frames != 76_695_844 || held != 3_692_185_892 || calibrated.Limit-held != 173_284_674 {
		t.Errorf("at the calibration the rooms can send %d bytes of frames and the twenty sockets hold %d, leaving %d: the figures peerWriteQueueBytes quotes are 76,695,844, 3,692,185,892 and 173,284,674",
			frames, held, calibrated.Limit-held)
	}
	// The budget is the message floor plus the lag it allows beyond one frame at the floor, so
	// a socket is never closed for a frame it could not have avoided (write_queue.go), and the
	// two move together whenever the floor does.
	if int64(peerWriteQueueBytes) != int64(maxMessageBytes)+6_291_456 {
		t.Errorf("the write-queue budget is %d bytes and the message floor %d: the budget is the floor plus the 6,291,456 bytes of lag it allows beyond one frame at the floor",
			peerWriteQueueBytes, maxMessageBytes)
	}
}

// heaviestDocument is a document at every bound the server's own writes obey: markdown of
// pmdoc.MaxDocumentBytes making pmdoc.MaxDocumentElements, the margin records of maxMarginBytes
// that a browser shows beside it, and anchor marks carrying maxAnchorBytes of ids and authors.
func heaviestDocument(t *testing.T) *crdt.Doc {
	t.Helper()
	author := model.Actor{Kind: "user", ID: "alice"}
	// A heading weighs four elements, so headings of 64 bytes reach both halves of the bound at
	// once (TestAWriteAtTheElementLimitIsReadAndOneElementMoreIsRefused).
	const headingBytes = 64
	heading := "# " + strings.Repeat("a", headingBytes-3) + "\n"
	markdown := strings.Repeat(heading, pmdoc.MaxDocumentBytes/headingBytes)
	size := pmdoc.MeasureDocument(markdown)
	if size.Bytes != pmdoc.MaxDocumentBytes || size.Elements != pmdoc.MaxDocumentElements {
		t.Fatalf("the markdown measures %+v, want %d bytes making %d elements",
			size, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements)
	}
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("markdown alone, at both halves of the bound: %d bytes of whole state", len(crdt.EncodeStateAsUpdateV1(doc, nil)))
	// The margin is the comment and suggestion records the document keeps in its marks map, each
	// within maxMarginRecordBytes and together within maxMarginBytes.
	const records = maxMarginBytes/maxMarginRecordBytes + 1
	marks := doc.GetMap(marksMapName)
	doc.Transact(func(txn *crdt.Transaction) {
		for record := range records {
			marks.Set(txn, fmt.Sprintf("c%d", record), map[string]any{
				"kind": "comment",
				"by":   ActorRef(author),
				"text": strings.Repeat("m", maxMarginBytes/records),
			})
		}
	})
	held := 0
	marks.ForEach(func(_ string, record any) { held += marginText(record) })
	if held < maxMarginBytes-records {
		t.Fatalf("the margin holds %d bytes, want the %d one document may hold", held, maxMarginBytes)
	}
	t.Logf("with its margin at %d bytes: %d bytes of whole state", held, len(crdt.EncodeStateAsUpdateV1(doc, nil)))
	// Each anchor mark carries an id and an author no rendering carries, counted once however many
	// texts it covers (anchorText), and written into the document by the same tree diff a server
	// write makes of one (pmdoc.Update).
	const anchors = 1 << 10
	anchorID := strings.Repeat("c", maxAnchorBytes/anchors)
	anchored := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "text" || anchored == anchors {
			return true
		}
		node.Marks = append(node.Marks, MarkSpec{
			Kind: MarkComment,
			ID:   fmt.Sprintf("%s%d", anchorID, anchored),
			By:   author,
		}.pmMark())
		anchored++
		return true
	})
	if anchored != anchors {
		t.Fatalf("the document took %d anchor marks, want %d", anchored, anchors)
	}
	if held := anchorText(tree); held < maxAnchorBytes {
		t.Fatalf("the anchor marks carry %d bytes, want the %d one document may hold", held, maxAnchorBytes)
	}
	fragment := doc.GetXmlFragment(fragmentName)
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A browser can leave a live document heavier than any the server's own writes leave: the hook
// admits an update at the byte bound while what it integrates is under maxPeerInsertAtTheBound,
// until the room's last rendering has crept maxPeerByteCreep past pmdoc.MaxDocumentBytes. So the
// floor (maxMessageBytes) has to clear the whole-state frame of the heaviest document grown that
// way too, which a browser hands over on its next sync. This grows heaviestDocument, at both
// halves of the bound with its full margin and anchor marks, through the hook itself
// (weighPeerUpdate), with updates a browser's copy of the room makes the way the editor makes them
// (pmdoc.Update of its tree), of the densest markdown this repository measures: empty items of
// alternating bullet lists, `- \n\n* \n\n`, each list and item and paragraph carrying the block id
// a browser's write mints.
//
// Each update is sized by its own bytes rather than by what ygo will count as content, which no
// caller can read before the apply: an update whose whole frame is under the insert allowance can
// integrate no more content than that, so every such update is admitted at the bound. The last is
// taken from a rendering under the creep cap by at most a pair, so the document ends as far past
// the cap as one admitted update can take it; one more pair is then refused. After each admitted
// update the room's rendering is published as its update observer publishes it (publishSize). It
// fails once four times the grown document's whole-state frame passes the floor.
func TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	const pair = "- \n\n* \n\n"
	room := heaviestDocument(t)
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	if len(rendering) < pmdoc.MaxDocumentBytes {
		t.Fatalf("the heaviest document renders %d bytes, want at least %d", len(rendering), pmdoc.MaxDocumentBytes)
	}
	serverWritten := len(crdt.EncodeStateAsUpdateV1(room, nil))
	service := calibratedService()
	replica := service.keepReplica(room, nil, loadRendering(rendering), nil)
	// A client id of five bytes, the most a lib0 varint of one takes, which most random ids are:
	// every origin of every struct the browser writes names it, so a smaller one would measure a
	// lighter frame than most browsers send.
	browser := newDocumentCopy(crdt.WithClientID(math.MaxUint32))
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatal(err)
	}
	tree, err := treeOf(browser)
	if err != nil {
		t.Fatal(err)
	}
	fragment := browser.GetXmlFragment(fragmentName)
	// grow appends pairs of the densest shape to the browser's document and hands the room the
	// update that makes, reporting the update's own bytes and the hook's answer. An update the
	// hook admits is applied to the room, whose new rendering is then published.
	grow := func(pairs int) (frame int, refused error) {
		t.Helper()
		added, err := pmdoc.Parse(strings.Repeat(pair, pairs))
		if err != nil {
			t.Fatal(err)
		}
		tree.Children = append(tree.Children, added.Children...)
		if err := browser.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
			t.Fatal(err)
		}
		update := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
		if err, _ := weighInto(service, room, update); err != nil {
			return len(update), err
		}
		rendering, err := renderDocument(room)
		if err != nil {
			t.Fatal(err)
		}
		replica.publishSize(&rendering)
		return len(update), nil
	}
	published := func() int64 {
		bytes, _ := replica.size()
		return bytes
	}
	// One small update first, which says what a pair costs the frame and the rendering, and so
	// how many pairs the largest update that is certainly under the insert allowance carries. It
	// carries few enough pairs to be under that allowance whatever a pair costs, since a pair
	// costs the frame well over a hundred bytes: its two lists, two items and two paragraphs each
	// carry the block id a browser mints.
	const calibration = 8
	loaded := published()
	frame, err := grow(calibration)
	if err != nil {
		t.Fatalf("the hook refused %d pairs at the bound: %v", calibration, err)
	}
	framedAPair := int64((frame + calibration - 1) / calibration)
	renderedAPair := (published() - loaded) / calibration
	largest := int((maxPeerInsertAtTheBound - 1) / framedAPair)
	if largest < 1 {
		t.Fatalf("a pair costs %d bytes of frame, more than the %d-byte insert allowance", framedAPair, maxPeerInsertAtTheBound)
	}
	creepCap := int64(pmdoc.MaxDocumentBytes + maxPeerByteCreep)
	updates := 1
	for published() < creepCap {
		pairs := largest
		if fit := int((creepCap - 1 - published()) / renderedAPair); fit > 0 && fit < largest {
			pairs = fit
		}
		if frame, err := grow(pairs); err != nil {
			t.Fatalf("the hook refused %d pairs in a %d-byte frame at %d rendered bytes: %v", pairs, frame, published(), err)
		}
		updates++
	}
	var refusal *websocket.PeerClose
	if _, err := grow(1); !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode {
		t.Fatalf("the hook answered %v to one more pair past the creep cap, want the document's own refusal", err)
	}
	runtime.KeepAlive(replica)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	grown := len(state)
	t.Logf("%d updates of up to %d pairs, each pair costing %d frame bytes and rendering %d, took the rendering from %d bytes to %d, against the %d-byte creep cap; "+
		"the whole-state frame grew from %d to %d bytes, four times it %d; the message floor is %d and the state cap %d",
		updates, largest, framedAPair, renderedAPair, loaded, published(), creepCap, serverWritten, grown, 4*grown, maxMessageBytes, maxRoomStateBytes)
	if grown*4 > maxMessageBytes {
		t.Fatalf("the heaviest whole-state frame a browser can grow is %d bytes, past a quarter of the %d-byte message floor: "+
			"measure it again and set the floor at four times it, or take it back to the design if four times it passes the 64 MiB ygo allowed before",
			grown, maxMessageBytes)
	}
}

// weighInto hands update to the hook for room, with an apply that applies it to room as ygo's own
// does (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:112-127), and reports what
// the hook answered and whether it called the apply.
func weighInto(service *Service, room *crdt.Doc, update []byte) (err error, called bool) {
	err = service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update}, func() error {
		called = true
		return crdt.ApplyUpdateV1(room, update, nil)
	})
	return err, called
}

// requireRefusedTooLarge requires err to be the document's own refusal, 4413 DOC_TOO_LARGE, and
// room to hold before, byte for byte: ygo takes a refused apply back out whole, so a refusal the
// apply itself made leaves the room exactly as the one the hook made before any apply does.
func requireRefusedTooLarge(t *testing.T, err error, room *crdt.Doc, before []byte) {
	t.Helper()
	var refusal *websocket.PeerClose
	if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the hook answered %v, want %d %q", err, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	}
	if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
		t.Fatal("the room's document changed for an update the hook refused")
	}
}

// gcFrameFrom is one client's frame of n GC structs of one clock each, from clock 0.
func gcFrameFrom(client uint64, n int) []byte {
	f := newV1Frame(1).group(n, client, 0)
	for range n {
		f.gc(1)
	}
	return f.noDeletes()
}

// deleteRangesFrom is a frame of no structs whose delete set holds n ranges of one clock each of
// client's, none adjacent to the next.
func deleteRangesFrom(client uint64, n int) []byte {
	f := newV1Frame(0).uint(1).uint(client).uint(uint64(n))
	for i := range n {
		f.uint(uint64(2 * i)).uint(1)
	}
	return f.done()
}

// marginValues is client 7's frame of one item under the margin's key "k" holding n copies of the
// lib0 Any value v.
func marginValues(n int, v any) []byte {
	f := newV1Frame(1).group(1, 7, 0).rooted(byte(structAnyRef), marksMapName, "k").uint(uint64(n))
	for range n {
		f.any(v)
	}
	return f.noDeletes()
}

// marginRecord is client's frame of one margin record under key at clock: a map whose one field
// holds text, as the server writes a comment's record (marginWatch weighs its text).
func marginRecord(client, clock uint64, key, text string) []byte {
	return newV1Frame(1).group(1, client, clock).rooted(byte(structAnyRef), marksMapName, key).
		uint(1).any(map[string]any{"text": text}).noDeletes()
}

// An update ygo refuses is never put in the room: the hook answers 4413 DOC_UPDATE_REFUSED, ygo's
// own refusal behind the close (documentUpdateRefused, refusedWith), with which ygo closes that
// browser and relays nothing, and the room is as it was. The apply runs for every one of these -
// the refusal is the apply's - and a bounded document takes a refused apply back out whole,
// firing no observer (sjawhar/ygo v1.55.1-sami.1, crdt/apply_rollback.go:9-21), so a frame of
// valid structs ending in one bad byte leaves no document holding them.
//
// The roots are the containment rule's: a browser may name the document's fragment and its margin
// and nothing else (peerAllowedRoots), so a subdocument, an unknown root, and both a write and a
// delete of an entry of the retired clients' list a rebuild left are refused. Only a rebuild
// writes that list, and nothing the editor runs writes or deletes an entry of it.
func TestAPeerUpdateYgoRefusesIsNeverApplied(t *testing.T) {
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	rebuilt := map[crdt.ClientID]uint64{123: 5, 456: 7}
	for _, row := range []struct {
		name string
		// rule is the error of ygo's the refusal must carry.
		rule error
		// room is the service and room the row drives, the update it hands the hook, and what
		// holds the room's replica while the hook reads it (the service lists it weakly).
		room func(t *testing.T) (*Service, *crdt.Doc, []byte, any)
	}{
		{
			name: "valid GC structs and then one bad byte",
			rule: crdt.ErrInvalidUpdate,
			room: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done(), replica
			},
		},
		{
			name: "a subdocument",
			rule: crdt.ErrInvalidUpdate,
			room: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				update := newV1Frame(1).group(1, 7, 0).rooted(byte(structDocumentRef), marksMapName, "sub").
					str("guid").any(map[string]any{"autoLoad": false}).noDeletes()
				return service, room, update, replica
			},
		},
		{
			name: "an unknown root",
			rule: crdt.ErrRootNotAllowed,
			room: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(1, 7, 0).rooted(byte(structStringRef), "t", "").str("hi").noDeletes(), replica
			},
		},
		{
			name: "a write of an entry of the retired clients' list",
			rule: crdt.ErrRootNotAllowed,
			room: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				return service, room, retiredListEdit(t, room, func(txn *crdt.Transaction, list *crdt.YMap) {
					list.Set(txn, "123", int64(9))
				}), replica
			},
		},
		{
			name: "a delete of an entry of the retired clients' list",
			rule: crdt.ErrRootNotAllowed,
			room: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				return service, room, retiredListEdit(t, room, func(txn *crdt.Transaction, list *crdt.YMap) {
					list.Delete(txn, "123")
				}), replica
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, update, holder := row.room(t)
			held := room.StateVector()
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, update)
			runtime.KeepAlive(holder)
			var refusal *websocket.PeerClose
			if !errors.Is(err, row.rule) || !errors.As(err, &refusal) ||
				refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentUpdateRefusedCloseReason {
				t.Fatalf("the hook answered %v, want ygo's refusal (%v) closed %d %q", err, row.rule, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
			}
			if !called {
				t.Fatal("the hook refused the update before the apply, want ygo's own refusal inside it")
			}
			if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
				t.Fatalf("the room's state vector is %v, want %v as it was", room.StateVector(), held)
			}
		})
	}
}

// retiredListEdit is the update a browser sends for edit to the retired clients' list of its copy
// of the room: the list only a rebuild writes, which the editor never touches.
func retiredListEdit(t *testing.T, room *crdt.Doc, edit func(txn *crdt.Transaction, list *crdt.YMap)) []byte {
	t.Helper()
	browser, _ := peerDocument(t, room)
	list := browser.GetMap(retiredMapName)
	if len(list.Keys()) == 0 {
		t.Fatal("the browser's copy holds no retired clients' list to edit")
	}
	held := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { edit(txn, list) })
	update := crdt.EncodeStateAsUpdateV1(browser, held)
	if len(update) == 0 {
		t.Fatal("the browser's edit of the retired clients' list made no update")
	}
	return update
}

// Every refusal the hook makes carries a code and a reason of Dispatch's own. ygo closes a peer
// whose update the hook refused with 4205 "Reset Connection" for any error that is no
// *websocket.PeerClose (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:38-52,
// :142-154), and @hocuspocus/provider 2.x reconnects at once after 4205 and sends the refused
// frame again, which the same rule refuses again: a loop, each round a whole sync. 4413 is the
// band the editor reads as permanent, so it drops the refused edit and re-reads the document, and
// 4503 the transient one it keeps the edit for. The rows below drive every refusal through the
// hook and read the close it answers with.
//
// One refusal is not here, because the fixture that reaches it is a drive of its own: the
// admission of a browser the room budget has no room for, 4503 DOC_BUSY, which is no refusal of
// an update at all (ServeHTTP).
//
// The one error the hook answers that is no close is the room's own refusal of an apply that ran
// - a frame past the room's pending cap - which ygo answers SyncStatus(0), leaving that browser
// connected exactly as a room without the hook does
// (TestAPendingCapRefusalCostsTheRoomNeitherChargeNorScans, weighPeerUpdate).
func TestEveryRefusalOfTheHookCarriesItsOwnCodeAndReason(t *testing.T) {
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	rebuilt := map[crdt.ClientID]uint64{123: 5, 456: 7}
	// What the typist types into the tangled rows' room, as the scan-refusal test types: enough
	// that a frame of a few items at that place scans past what one apply of a browser's is
	// allowed (tangledRoom, tangledFrame).
	const tangledTyping = 2_000
	for _, row := range []struct {
		name   string
		code   int
		reason string
		// applies is whether the hook calls the apply before it refuses: every refusal but the
		// replaced history's and the test hook's is the apply's own, made inside it.
		applies bool
		// refusal is the service and room the row drives, the update it hands the hook, and what
		// holds the room's replica while the hook reads it (the service lists it weakly).
		refusal func(t *testing.T) (*Service, *crdt.Doc, []byte, any)
	}{
		{
			name: "bytes ygo's decoder refuses", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done(), replica
			},
		},
		{
			name: "a subdocument, which the editor never sends", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				update := newV1Frame(1).group(1, 7, 0).rooted(byte(structDocumentRef), marksMapName, "sub").
					str("guid").any(map[string]any{"autoLoad": false}).noDeletes()
				return service, room, update, replica
			},
		},
		{
			name: "a root the editor never writes", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(1, 7, 0).rooted(byte(structStringRef), "t", "").str("hi").noDeletes(), replica
			},
		},
		{
			name: "a write of the retired clients' list, which only a rebuild writes", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				return service, room, retiredListEdit(t, room, func(txn *crdt.Transaction, list *crdt.YMap) {
					list.Set(txn, "123", int64(9))
				}), replica
			},
		},
		{
			name: "a delete of an entry of the retired clients' list", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				return service, room, retiredListEdit(t, room, func(txn *crdt.Transaction, list *crdt.YMap) {
					list.Delete(txn, "123")
				}), replica
			},
		},
		{
			name: "the update a browser test's hook arms", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.RefuseNextPeerUpdateForTest("room")
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
		{
			name: "a browser still holding the history a rebuild replaced", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentRebuiltCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				update := newV1Frame(2).group(1, 1000, 0).gc(1).group(1, 123, 0).gc(5).noDeletes()
				return service, room, update, replica
			},
		},
		{
			name: "a 9 KiB paste at the byte bound", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, proseAtLeast(pmdoc.MaxDocumentBytes), pmdoc.MaxDocumentBytes)
				return service, room, peerTyping(t, room, strings.Repeat("p", 9<<10)), replica
			},
		},
		{
			name: "an update past the decode cap", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, gcFrameFrom(7, maxUpdateItems-room.StructCount()+1), replica
			},
		},
		{
			name: "an update past what the pod lets any apply decode", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, marginValues(maxDecodedValuesPerApply+1, nil), replica
			},
		},
		{
			name: "an update past the room's state cap", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentNeedsRebuildCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.chargeLoadedRoom("room", replica, maxRoomStateBytes-1<<10)
				return service, room, peerTyping(t, room, strings.Repeat("p", 4<<10)), replica
			},
		},
		{
			name: "a keystroke while the room budget is full", code: contracts.DocumentBusyCloseCode, reason: contracts.DocumentBusyCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.budget.full.Store(true)
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
		{
			name: "a merge past what one apply of the room may scan", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooTangledCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica, _ := tangledRoom(t, tangledTyping)
				return service, room, tangledFrame(t, room, tangledTyping), replica
			},
		},
		{
			name: "a merge in a room whose scan total is used up", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentNeedsRebuildCloseReason, applies: true,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica, _ := tangledRoom(t, tangledTyping)
				frame := tangledFrame(t, room, tangledTyping)
				replica.scans.rescanned(roomScanTotal - usedUpScanBudget)
				return service, room, frame, replica
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, update, holder := row.refusal(t)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, update)
			runtime.KeepAlive(holder)
			var refusal *websocket.PeerClose
			if !errors.As(err, &refusal) || refusal.Code != row.code || refusal.Reason != row.reason {
				t.Fatalf("the hook answered %v, want the browser closed %d %q", err, row.code, row.reason)
			}
			if called != row.applies {
				t.Fatalf("the hook called the apply %t, want %t", called, row.applies)
			}
			if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
				t.Fatal("the room's document changed for an update the hook refused")
			}
		})
	}
}

// Every refusal of the hook closes the browser's socket, and ygo logs a refused peer update at
// Debug alone (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:142-143,
// p.server.log().Debug("peer update refused", ...)), which the pod's level drops: without a line
// of the hook's own, an operator sees an editor vanish and nothing says why. Each row below
// drives one refusal and reads the one line it leaves: the room, the close that browser was
// given, and the figures that refused it, and never a byte of the person's own text. The two
// refusals that logged a line already are not here - the replaced history's and the scan
// allowance's (weighPeerUpdate) - nor the room's own refusal of an apply that ran, which closes
// nothing.
func TestEveryRefusalThatClosesABrowserLogsWhyItRefusedIt(t *testing.T) {
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	for _, row := range []struct {
		name    string
		code    int
		reason  string
		message string
		// figures are what the line must carry of the rule that refused the update, beside the
		// room, the update's bytes and the close.
		figures []string
		// refusal is the service and room the row drives, the update it hands the hook, and what
		// holds the room's replica while the hook reads it (the service lists it weakly).
		refusal func(t *testing.T) (*Service, *crdt.Doc, []byte, any)
	}{
		{
			name: "a root the editor never writes",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			message: "dispatch: refused a browser's update naming a root the editor never writes",
			figures: []string{`root=t`},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(1, 7, 0).rooted(byte(structStringRef), "t", "").str("hi").noDeletes(), replica
			},
		},
		{
			name: "a write of the retired clients' list",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			message: "dispatch: refused a browser's update naming a root the editor never writes",
			figures: []string{"root=" + retiredMapName},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, map[crdt.ClientID]uint64{123: 5, 456: 7})
				return service, room, retiredListEdit(t, room, func(txn *crdt.Transaction, list *crdt.YMap) {
					list.Set(txn, "123", int64(9))
				}), replica
			},
		},
		{
			name: "bytes ygo will not read",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			message: "dispatch: refused a browser's update ygo will not read",
			figures: []string{"error="},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done(), replica
			},
		},
		{
			name: "the update a browser test's hook arms",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			message: "dispatch: refused a browser's update the test hook armed this document for",
			figures: []string{`rule="the test hook"`},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.RefuseNextPeerUpdateForTest("room")
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
		{
			name: "a 9 KiB paste at the byte bound",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			message: "dispatch: refused a browser's update its apply may not take",
			figures: []string{
				`rule="its apply inserts into a document already at its byte bound"`,
				"content_bytes=", fmt.Sprintf("content_limit=%d", maxPeerInsertAtTheBound),
				fmt.Sprintf("byte_rule_limit=%d", maxPeerInsertAtTheBound), "split=",
			},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, proseAtLeast(pmdoc.MaxDocumentBytes), pmdoc.MaxDocumentBytes)
				return service, room, peerTyping(t, room, strings.Repeat("p", 9<<10)), replica
			},
		},
		{
			name: "an update past the room's decode cap",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			message: "dispatch: refused a browser's update its apply may not take",
			figures: []string{
				`rule="its apply would take the document past the items ygo decodes in one update"`,
				"structs=", "structs_limit=", fmt.Sprintf("struct_rule_limit=%d", maxUpdateItems),
			},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, gcFrameFrom(7, maxUpdateItems-room.StructCount()+1), replica
			},
		},
		{
			name: "an update past the room's state cap",
			code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentNeedsRebuildCloseReason,
			message: "dispatch: refused a browser's update its apply may not take",
			figures: []string{
				`rule="its apply would take the document's state past what one room may hold"`,
				fmt.Sprintf("byte_rule_limit=%d", maxRoomStateBytes),
			},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.chargeLoadedRoom("room", replica, maxRoomStateBytes-1<<10)
				return service, room, peerTyping(t, room, strings.Repeat("p", 4<<10)), replica
			},
		},
		{
			name: "a keystroke while the room budget is full",
			code: contracts.DocumentBusyCloseCode, reason: contracts.DocumentBusyCloseReason,
			message: "dispatch: refused a browser's update its apply may not take",
			figures: []string{
				`rule="the open documents are past their memory budget and nothing can be closed"`,
				"structs_limit=0", "content_limit=0",
			},
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.budget.full.Store(true)
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureLogs(t)
			service, room, update, holder := row.refusal(t)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, _ := weighInto(service, room, update)
			runtime.KeepAlive(holder)
			var refusal *websocket.PeerClose
			if !errors.As(err, &refusal) || refusal.Code != row.code || refusal.Reason != row.reason {
				t.Fatalf("the hook answered %v, want the browser closed %d %q", err, row.code, row.reason)
			}
			if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
				t.Fatal("the room's document changed for an update the hook refused")
			}
			line := logs.Lines(row.message)
			if strings.Count(line, "\n") != 1 {
				t.Fatalf("the refusal logged %d lines holding %q, want the one line that says why:\n%s",
					strings.Count(line, "\n"), row.message, logs)
			}
			for _, want := range append([]string{
				"level=WARN", "room=room", fmt.Sprintf("update_bytes=%d", len(update)),
				fmt.Sprintf("close_code=%d", row.code), "close_reason=" + row.reason,
			}, row.figures...) {
				if !strings.Contains(line, want) {
					t.Fatalf("the refusal logged\n%swant that line to hold %q", line, want)
				}
			}
			t.Logf("%s", line)
		})
	}
}

// A browser cannot take its room's own state past the decode cap: ygo decodes at most
// maxUpdateItems structs in one update, and a room's whole state is one update to the next load
// that reads it, so a room whose state held more could never be loaded again. The apply's
// allowance of structs is what is left of the cap above what the room already holds
// (crdt.Doc.StructCount), so a frame that would take the room one struct past it is refused with
// the document's own close, 4413 DOC_TOO_LARGE, and taken back out whole, while the frame that
// takes the room to the cap exactly is admitted. A delete-only frame, which integrates nothing, is
// admitted at the cap.
func TestAnUpdateThatWouldTakeTheRoomPastTheDecodeCapIsRefused(t *testing.T) {
	for _, row := range []struct {
		name string
		// update is built against the room's own exact count of structs.
		update  func(held int) []byte
		refused bool
	}{
		{
			name:    "GC structs one past the cap",
			update:  func(held int) []byte { return gcFrameFrom(7, maxUpdateItems-held+1) },
			refused: true,
		},
		{
			name:   "GC structs to the cap",
			update: func(held int) []byte { return gcFrameFrom(7, maxUpdateItems-held) },
		},
		{
			name:   "delete ranges, which integrate nothing",
			update: func(int) []byte { return deleteRangesFrom(7, 64) },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
			held := room.StructCount()
			update := row.update(held)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, update)
			runtime.KeepAlive(replica)
			if row.refused {
				if !called {
					t.Fatal("the hook refused the frame before the apply, want the allowance's refusal inside it")
				}
				requireRefusedTooLarge(t, err, room, before)
				return
			}
			if err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t), want the update admitted", err, called)
			}
			if after := room.StructCount(); after > maxUpdateItems {
				t.Fatalf("the room holds %d structs after the update, want within the %d cap", after, maxUpdateItems)
			}
		})
	}
}

// The finding the state cap closes, as a browser makes it: updates of a million margin bytes
// each, every one a single clock and a single struct, which no rendering shows and which the byte
// bound therefore never sees. Measured on 2026-10-26, eighty such updates were all admitted and
// took a room's whole-state frame from 264 bytes to 80,001,457, past the message floor every
// browser of the document then reconnects against. Each is now admitted while the room's tracked
// state, which its drain raises by every update's own bytes (renderedReplica.chargeAdded), has
// room for it under maxRoomStateBytes; the first that would pass it is refused with
// DOC_NEEDS_REBUILD - the editor offers the rebuild that would take the edit - and leaves the room
// as it was, and so is every one after it. A delete, which integrates nothing, is admitted at the
// cap, so a document that has reached it can still be trimmed.
func TestBrowserUpdatesOfMarginBytesStopAtTheRoomsStateCap(t *testing.T) {
	const updates, size = 20, 1_000_000
	service, room, replica, _ := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
	text := strings.Repeat("m", size)
	startFrame := len(crdt.EncodeStateAsUpdateV1(room, nil))
	admitted := 0
	for i := range updates {
		// A client of its own for each update, so a refused one leaves no clock gap the next
		// would park behind.
		update := marginRecord(uint64(1000+i), 0, fmt.Sprintf("c%02d", i), text)
		tracked := replica.roomStateBytes()
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, update)
		if tracked+int64(len(update)) <= maxRoomStateBytes {
			if err != nil || !called {
				t.Fatalf("update %d, with the room tracked at %d bytes of state, was answered %v (applied %t), want it admitted", i, tracked, err, called)
			}
			admitted++
			continue
		}
		var refusal *websocket.PeerClose
		if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentNeedsRebuildCloseReason {
			t.Fatalf("update %d, with the room tracked at %d bytes of state, was answered %v, want %d %q",
				i, tracked, err, contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
		}
		if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
			t.Fatalf("update %d changed the room though it was refused", i)
		}
	}
	tracked := replica.roomStateBytes()
	frame := len(crdt.EncodeStateAsUpdateV1(room, nil))
	t.Logf("%d of %d updates of %d margin bytes admitted, leaving the room tracked at %d bytes of state against the %d-byte cap; the whole-state frame went from %d bytes to %d, against the %d-byte floor",
		admitted, updates, size, tracked, maxRoomStateBytes, startFrame, frame, maxMessageBytes)
	if admitted == 0 || admitted == updates || tracked > maxRoomStateBytes {
		t.Fatalf("%d updates admitted leaving the room tracked at %d bytes, want them stopped at the %d-byte cap", admitted, tracked, maxRoomStateBytes)
	}
	if frame >= maxMessageBytes {
		t.Fatalf("the room's whole-state frame is %d bytes, want it under the %d-byte floor", frame, maxMessageBytes)
	}
	// A delete integrates nothing, so the state cap's allowance of nothing takes it.
	if err, called := weighInto(service, room, peerDeleting(t, room, 3)); err != nil || !called {
		t.Fatalf("a delete at the state cap was answered %v (applied %t), want it admitted", err, called)
	}
	runtime.KeepAlive(replica)
}

// A room with no measurement - a document outside the schema - weighs an update's bytes on their
// own, as if it rendered nothing: a paste of a mebibyte is refused with the document's own close,
// and a paste well under the bound applies.
func TestAnUnmeasuredRoomWeighsAPeerUpdateOnItsOwnBytes(t *testing.T) {
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
	service := calibratedService()
	replica := service.keepReplica(room, nil, nil, nil)
	defer runtime.KeepAlive(replica)
	if _, measured := replica.size(); measured {
		t.Fatal("the room's load published a measurement, want none")
	}
	paste := peerTyping(t, room, strings.Repeat("pasted ", pmdoc.MaxDocumentBytes/7+1))
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	err, _ = weighInto(service, room, paste)
	requireRefusedTooLarge(t, err, room, before)
	if err, called := weighInto(service, room, peerTyping(t, room, strings.Repeat("typed ", 64<<10/6))); err != nil || !called {
		t.Fatalf("a 64 KiB paste was answered %v (applied %t), want it admitted", err, called)
	}
}

// An update whose apply would decode more values than the pod lets any apply decode is refused
// with the document's own close, 4413 DOC_TOO_LARGE, before it is all in memory: the count is
// taken as the bytes are read, so a frame of 25,165,779 null values for a client whose every clock
// the room already holds - which inserts nothing, adds no struct and passes every other rule - is
// refused as it is read rather than once it has allocated the 805,307,736 bytes one such apply
// measured (maxDecodedValuesPerApply). The room is left as it was.
func TestAnUpdateDecodingMoreThanAnyApplyMayIsNeverApplied(t *testing.T) {
	const values = maxDecodedValuesPerApply * 2
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, 7, 0).gc(values).noDeletes(), nil); err != nil {
		t.Fatal(err)
	}
	update := marginValues(values, nil)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	err, called := weighInto(service, room, update)
	runtime.KeepAlive(replica)
	if !called {
		t.Fatal("the hook refused the frame before the apply, want the decode count's refusal inside it")
	}
	if !errors.Is(err, crdt.ErrApplyBudget) {
		t.Fatalf("the hook answered %v, want the apply budget's refusal behind the close", err)
	}
	requireRefusedTooLarge(t, err, room, before)
	t.Logf("a %d-byte update of %d null values is refused against the %d the pod lets one apply decode", len(update), values, maxDecodedValuesPerApply)
}

// gcForgeryClient is the client of the one struct of the forged frame below that the room holds
// nothing of, past the 32 bits crdt.NewClientID draws, so the room integrates that struct and the
// update is one whose structs render nothing.
const gcForgeryClient = 1 << 40

// peerDeletingEveryOtherCharacter is the update a browser sends that deletes every other character
// of the room's first text, from its last: a delete range for each, no two of them touching, and
// the room holds none of them deleted.
func peerDeletingEveryOtherCharacter(t *testing.T, room *crdt.Doc) []byte {
	t.Helper()
	browser, from := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) {
		for at := from.Len() - 1; at > 0; at -= 2 {
			from.Delete(txn, at, 1)
		}
	})
	return crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
}

// gcRunOverHeldClocks is the forged frame: one GC struct over every clock the room holds of each of
// its clients, which ygo integrates nothing of, and one GC struct of a client the room holds
// nothing of (gcForgeryClient), which it appends as a struct born deleted. Its groups are
// written by ascending client, which the decode reads in the order they are written, whatever
// order ygo would write them in (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:386-387 writes a
// state's groups by descending client).
func gcRunOverHeldClocks(room *crdt.Doc) []byte {
	held := room.StateVector()
	frame := newV1Frame(len(held) + 1)
	for _, client := range slices.Sorted(maps.Keys(held)) {
		frame = frame.group(1, uint64(client), 0).gc(held[client])
	}
	return frame.group(1, gcForgeryClient, 0).gc(1).noDeletes()
}

// drainedPeerUpdateRoom is peerUpdateRoom's room with its update log open and an observer moving
// the replica through every commit, as a room's load leaves it (keepReplica, onLoadDocument), and
// charged in a budget of roomBudgetBytes at the bytes of the state it loaded with: so each update
// the hook admits is drained, added to the room's tracked state and charged where a loaded room's
// is (renderedReplica.chargeAdded). Its replica takes the load's rendering as a load makes it
// (liveRendering, documentRenderingWithLayout): the markdown and the layout of one render of the
// state the replica starts from, which is what each admitted update is measured and spliced
// against (renderedReplica.spliceTouched).
func drainedPeerUpdateRoom(t *testing.T, markdown string) (*Service, *crdt.Doc, *renderedReplica, *roomCharge) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	update, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
		t.Fatal(err)
	}
	service := calibratedService()
	service.budget.limit = roomBudgetBytes
	log := room.OpenUpdateLog()
	t.Cleanup(log.Close)
	replica := service.keepReplica(room, log, liveRendering(t, room), nil)
	if _, measured := replica.size(); !measured {
		t.Fatal("the room published no measurement for the hook to weigh against")
	}
	charge := service.chargeLoadedRoom("room", replica, int64(len(crdt.EncodeStateAsUpdateV1(room, nil))))
	room.OnUpdate(func([]byte, any) { replica.observe("room") })
	return service, room, replica, charge
}

// heldApply starts the hook on update for room from a browser whose origin is origin, with an
// apply that applies update and then waits inside the apply until release is closed, as an apply
// whose update observer is still running does. It returns once the apply holds, and the channel
// the hook's answer arrives on.
func heldApply(t *testing.T, service *Service, room *crdt.Doc, update []byte, origin any, release <-chan struct{}) <-chan error {
	t.Helper()
	inside, answered := make(chan struct{}), make(chan error, 1)
	go func() {
		answered <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update, Origin: origin}, func() error {
			err := crdt.ApplyUpdateV1(room, update, origin)
			close(inside)
			<-release
			return err
		})
	}()
	select {
	case <-inside:
	case err := <-answered:
		t.Fatalf("the hook answered %v without holding inside the apply", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never reached the apply")
	}
	return answered
}

// A browser's update is weighed and applied while another browser's apply to the same room is
// still running, as it is while that apply sits in one of the room's update observers
// (TestARoomWhosePeersUpdatesAreStoredOutOfOrderSettlesBoth): the hook holds no lock across an
// apply, and the allowance it reserves for one apply's origin is the one ygo asks for under that
// apply's own lock, so neither apply reads the other's (roomApplies.applying).
func TestAPeerUpdateIsAppliedWhileAnotherBrowsersApplyIsStillRunning(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	first, second := peerTyping(t, room, "a"), peerTyping(t, room, "b")
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHeld)
	alpha, beta := new(int), new(int)
	firstAnswer := heldApply(t, service, room, first, alpha, release)

	secondAnswer := make(chan error, 1)
	var secondApplied atomic.Bool
	go func() {
		secondAnswer <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: second, Origin: beta}, func() error {
			secondApplied.Store(true)
			return crdt.ApplyUpdateV1(room, second, beta)
		})
	}()
	select {
	case err := <-secondAnswer:
		if err != nil || !secondApplied.Load() {
			t.Fatalf("the hook answered %v (applied %t) for the second browser's keystroke, want it admitted", err, secondApplied.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second browser's keystroke waited on the first browser's apply")
	}

	releaseHeld()
	if err := <-firstAnswer; err != nil {
		t.Fatalf("the hook answered %v for the first browser's keystroke, want it admitted", err)
	}
	runtime.KeepAlive(replica)
}

// An apply of an update over unslottedApplyBytes takes a slot of the pod's load concurrency and
// holds it across the apply, so a reconnect storm's whole-state handoffs decode as many at a time
// as the pod has slots rather than all at once; a keystroke, far under that size, takes none and
// never waits for one. With the pod's one slot held, the heavy update waits before its apply and
// the keystroke beside it applies at once; once the slot is given back the heavy update applies
// too, and gives its slot back.
func TestAHeavyApplyTakesASlotOfTheLoadConcurrencyAndAKeystrokeTakesNone(t *testing.T) {
	const values, each = 1_000, 4_000
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	// The room already holds every clock the heavy update names, so its apply integrates no
	// struct and no byte of content and no rule of the document's refuses it, while ygo decodes
	// every value of it: what this weighs is the slot, not the allowance.
	if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, 7, 0).gc(values).noDeletes(), nil); err != nil {
		t.Fatal(err)
	}
	heavy := marginValues(values, strings.Repeat("a", each))
	keystroke := peerTyping(t, room, "y")
	if len(heavy) <= unslottedApplyBytes || len(keystroke) > unslottedApplyBytes {
		t.Fatalf("the heavy update is %d bytes and the keystroke %d, want the heavy one past the %d a slot is taken over and the keystroke under it",
			len(heavy), len(keystroke), unslottedApplyBytes)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }

	applied := make(chan error, 1)
	var reached atomic.Bool
	go func() {
		applied <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: heavy}, func() error {
			reached.Store(true)
			return crdt.ApplyUpdateV1(room, heavy, nil)
		})
	}()
	select {
	case <-waiting:
	case err := <-applied:
		t.Fatalf("the heavy update was answered %v while the only slot was held, want it waiting for one", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the heavy update neither waited for a slot nor answered in 30 s")
	}
	if reached.Load() {
		t.Fatal("the heavy update reached its apply while the only slot was held")
	}
	// The keystroke takes no slot, so it applies beside the heavy update's wait.
	typed := make(chan error, 1)
	go func() {
		err, called := weighInto(service, room, keystroke)
		if err == nil && !called {
			err = errors.New("the hook admitted the keystroke without applying it")
		}
		typed <- err
	}()
	select {
	case err := <-typed:
		if err != nil {
			t.Fatalf("the keystroke was answered %v, want it applied while the heavy update waits for a slot", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the keystroke waited behind the heavy update's slot")
	}

	holder.release()
	select {
	case err := <-applied:
		if err != nil || !reached.Load() {
			t.Fatalf("the heavy update was answered %v (applied %t) once a slot was free, want it applied", err, reached.Load())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the heavy update did not apply in 30 s once a slot was free")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("%d slots are held once the heavy apply has returned, want none", held)
	}
	runtime.KeepAlive(replica)
}

// marginTextKey is the margin key under which nestedMarginText nests a text.
const marginTextKey = "nested"

// nestedMarginText is a browser's copy of room with a text nested under the margin
// (marksMapName), the update that nests it, and the text: a value whose characters no rendering
// of the document shows, so neither the byte bound nor the creep cap sees them, and only the
// state cap bounds what a browser may put there.
func nestedMarginText(t *testing.T, room *crdt.Doc) (*crdt.Doc, []byte, *crdt.YText) {
	t.Helper()
	browser, _ := peerDocument(t, room)
	held := browser.StateVector()
	margin := browser.GetMap(marksMapName)
	browser.Transact(func(txn *crdt.Transaction) { margin.Set(txn, marginTextKey, crdt.NewTextPrelim()) })
	nested, ok := margin.Get(marginTextKey)
	text, isText := nested.(*crdt.YText)
	if !ok || !isText {
		t.Fatalf("the margin holds %T under %q, want a text", nested, marginTextKey)
	}
	return browser, crdt.EncodeStateAsUpdateV1(browser, held), text
}

// typedInto is the update browser sends for typing typed at the end of text.
func typedInto(browser *crdt.Doc, text *crdt.YText, typed string) []byte {
	held := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), typed, nil) })
	return crdt.EncodeStateAsUpdateV1(browser, held)
}

// The wire tags of the struct kinds the hand-written frames below carry, as ygo's V1 encoder
// writes them (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:17-37). The package itself names only
// the three testhooks.go writes a chain with (structStringRef, infoHasOrigin,
// infoHasRightOrigin), now that no product code walks an update: a frame these tests write by
// hand is the browser's bytes, and a change to ygo's tags must fail here rather than be followed
// silently.
const (
	structGCRef       byte = 0
	structDeletedRef  byte = 1
	structJSONRef     byte = 2
	structFormatRef   byte = 6
	structTypeRef     byte = 7
	structAnyRef      byte = 8
	structDocumentRef byte = 9
	structSkipRef     byte = 10

	// infoHasParentSub says an item's tag carries the key of the map entry it writes.
	infoHasParentSub byte = 0x20
)

// varuint is v as a lib0 variable-length unsigned integer, the encoding every count and clock of
// a frame is written in.
func varuint(v uint64) []byte {
	e := encoding.NewEncoder()
	e.WriteVarUint(v)
	return e.Bytes()
}

// v1Frame lays out a V1 update by hand, byte for byte as a Yjs encoder does, for the frames ygo's
// API cannot write: JSON and binary content, legacy lib0 Any JSON, GC and skip runs, roots the
// editor never names, and malformed bytes.
type v1Frame struct{ *encoding.Encoder }

// newV1Frame starts a frame of clients groups of structs.
func newV1Frame(clients int) v1Frame {
	f := v1Frame{encoding.NewEncoder()}
	f.WriteVarUint(uint64(clients))
	return f
}

// group starts one client's group of structs structs from clock.
func (f v1Frame) group(structs int, client, clock uint64) v1Frame {
	return f.uint(uint64(structs)).uint(client).uint(clock)
}

func (f v1Frame) gc(length uint64) v1Frame   { return f.raw(structGCRef).uint(length) }
func (f v1Frame) skip(length uint64) v1Frame { return f.raw(structSkipRef).uint(length) }

// rooted starts an item of content reference tag directly under root, a map entry under key when
// key is not empty.
func (f v1Frame) rooted(tag byte, root, key string) v1Frame {
	if key == "" {
		return f.raw(tag, 1).str(root)
	}
	return f.raw(tag|infoHasParentSub, 1).str(root).str(key)
}

func (f v1Frame) uint(v uint64) v1Frame {
	f.WriteVarUint(v)
	return f
}

func (f v1Frame) str(s string) v1Frame {
	f.WriteVarBytes([]byte(s))
	return f
}

func (f v1Frame) any(v any) v1Frame {
	f.WriteAny(v)
	return f
}

func (f v1Frame) raw(b ...byte) v1Frame {
	f.WriteRaw(b)
	return f
}

// noDeletes ends the frame with an empty delete set.
func (f v1Frame) noDeletes() []byte { return f.uint(0).done() }

func (f v1Frame) done() []byte { return slices.Clone(f.Bytes()) }
