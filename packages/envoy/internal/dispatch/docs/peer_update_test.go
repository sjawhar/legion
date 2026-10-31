package docs

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The hook is driven here at its own level, as ygo drives it: the room's document, the update's
// bytes, and an apply that applies the update to that document exactly as the room's own does
// (sjawhar/ygo v1.54.1-sami.1, provider/websocket/peer_update.go:84-89). What a real browser meets
// over a real socket is the real-process drive; what these rows pin is the rule itself.
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
			name:       "an update that does not decode is refused as one the walk will not read",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update:     func(*testing.T, *crdt.Doc) []byte { return []byte{0x01, 0xff, 0xff, 0xff} },
			refused:    contracts.DocumentTooLargeCloseCode,
			reason:     contracts.DocumentUpdateRefusedCloseReason,
			unapplied:  true,
		},
		{
			name:      "an update that does not decode under the bound is refused the same way",
			markdown:  ordinary,
			update:    func(*testing.T, *crdt.Doc) []byte { return []byte{0x01, 0xff, 0xff, 0xff} },
			refused:   contracts.DocumentTooLargeCloseCode,
			reason:    contracts.DocumentUpdateRefusedCloseReason,
			unapplied: true,
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
			name:       "a 9 KiB paste at the byte bound while the room budget is full is the document's own refusal",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			busy:       true,
			update: func(t *testing.T, room *crdt.Doc) []byte {
				return peerTyping(t, room, strings.Repeat("p", 9<<10))
			},
			refused: contracts.DocumentTooLargeCloseCode,
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

// peerUpdateRoom is a room holding markdown, bounded as every room's document is (newRoomScans),
// with the measurement its load publishes (keepReplica), and nothing else of the service: the hook
// reads the room's document and that measurement alone. leastBytes is what the rendering must
// measure for its caller's state, so a row written for a document at a bound fails rather than
// passing under one. The replica is returned because the service lists it weakly, and its caller is
// what holds it.
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
	service := &Service{}
	replica := service.keepReplica(room, nil, &rendering, nil)
	bytes, elements, measured := replica.size()
	if !measured || bytes < int64(leastBytes) {
		t.Fatalf("the room published %d bytes and %d elements, want at least %d bytes", bytes, elements, leastBytes)
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
	if len(after) > len(before) || countElements(after) <= countElements(before) {
		t.Fatalf("the reshaped document renders %d bytes making %d elements, from %d making %d: want no more bytes and more elements",
			len(after), countElements(after), len(before), countElements(before))
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
// interface is unexported (sjawhar/ygo v1.54.1-sami.1, crdt/yxml.go:16), so the walk takes each
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
// maxAnchorBytes - whose whole state is far more than its markdown: growth.go:14-18 records
// thirty-two suggestions of 900 KB that left the markdown as it was. What this does not weigh is
// the items a long edit history tombstones, which no rendering shows and which the decode cap
// bounds instead (maxUpdateItems: refuseUndecodable, and the peer update hook's structTotal);
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

// The floor is the larger of four times the heaviest frame the server's own writes store and a
// quarter more than F, the heaviest a browser can hand over under the hook's bounds, rounded up
// to the next mebibyte, and under the 64 MiB ygo allowed before (maxMessageBytes). The heaviest
// the server stores is a document an agent rewrote whole through the API until the decode cap
// refused a write, whose state carries every rewrite's tombstones: built here, its frame is held
// within a hundredth of serverStoredStateFrame, the figure the floor is set from. The writes'
// random client ids move it from run to run: 12,555,674 and 12,630,814 bytes were measured on
// 2026-10-26 and 2026-10-27, and four times either is still under a quarter more than F.
func TestTheHeaviestWholeStateFrameTheServerStoresFitsTheMessageFloor(t *testing.T) {
	state, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	structs, rendered, _ := structsAndRendering(t, state)
	floor := max(4*serverStoredStateFrame, heaviestHandoffBound*5/4+1)
	t.Logf("an agent's rewrites stored %d structs rendering %d bytes in a %d-byte frame; F is %d; the floor is %d, from four times %d and a quarter more than F",
		structs, rendered, len(state), heaviestHandoffBound, maxMessageBytes, serverStoredStateFrame)
	if len(state) > serverStoredStateFrame+serverStoredStateFrame/100 {
		t.Fatalf("the stored frame is %d bytes, past the %d the floor is set from by more than a hundredth: measure it again and set the floor from it", len(state), serverStoredStateFrame)
	}
	if maxMessageBytes < floor || maxMessageBytes-1<<20 >= floor || maxMessageBytes > 64<<20 {
		t.Fatalf("the floor is %d bytes, want the next mebibyte at or above %d, and at most the 64 MiB ygo allowed before", maxMessageBytes, floor)
	}
}

// roomFramesUnderTheBudget is the most bytes of whole-state frames rooms can send when the room
// budget is charged to its limit and holds rooms of them: every room's charge is at least its
// structs at bytesOfHeapAStructHeld in each of roomCopies copies (copyHeap), so the rooms share
// roomBudgetBytes / 340 structs, whose frames are densestStateFrame's bytes a struct, and each
// room adds F's other terms, its rendering at the creep cap and its unrendered bytes.
func roomFramesUnderTheBudget(rooms int64) int64 {
	structs := int64(roomBudgetBytes) / (bytesOfHeapAStructHeld * roomCopies)
	ofStructs := (structs*densestStateFrame + densestStateStructs - 1) / densestStateStructs
	return ofStructs + rooms*(pmdoc.MaxDocumentBytes+maxPeerByteCreep+maxPeerInsertAtTheBound+maxUnrenderedBytes)
}

// The per-peer write-queue budget (peerWriteQueueBytes) clears every frame a room can send one
// socket, since a frame arriving while the budget or more waits closes the socket it arrives for,
// honest or not; and twenty sockets that never read, each holding the budget and the frame that
// crossed it, stay inside the Go memory limit beside a room budget charged to its limit. The
// frames those twenty hold are bounded by that same budget (roomFramesUnderTheBudget), which is
// why twenty fit at all: twenty times F would be 898,721,780 bytes.
//
// It also holds the two figures the constant's own sizing is argued from: 64 MiB, the draft's
// budget against ygo's draft semantics, where a socket held the budget and no frame more, no
// longer fits; and fifteen sockets fit when the frame each socket's writer is in the middle of is
// counted as well, a second frame a socket, where sixteen do not.
func TestTheWriteQueueBudgetsArithmetic(t *testing.T) {
	const sockets = 20
	if peerWriteQueueBytes <= maxMessageBytes || peerWriteQueueBytes <= heaviestHandoffBound {
		t.Errorf("the write-queue budget is %d bytes, want it past the %d-byte message floor and past F, %d: a socket holding one such frame would be closed by the next frame it is sent",
			peerWriteQueueBytes, maxMessageBytes, uint64(heaviestHandoffBound))
	}
	frames := roomFramesUnderTheBudget(sockets)
	held := sockets*peerWriteQueueBytes + frames + roomBudgetBytes
	t.Logf("%d sockets hold %d x %d and the %d bytes of frames their rooms can send under a full room budget, %d beside that budget, against the %d-byte Go memory limit",
		sockets, sockets, peerWriteQueueBytes, frames, held, int64(goMemoryLimitBytes))
	if held > goMemoryLimitBytes {
		t.Errorf("%d sockets that never read hold %d bytes beside a full room budget, %d past the %d-byte Go memory limit: lower the budget",
			sockets, held, held-goMemoryLimitBytes, int64(goMemoryLimitBytes))
	}
	if draft := int64(sockets)*(64<<20) + frames + roomBudgetBytes; draft <= goMemoryLimitBytes {
		t.Errorf("%d sockets at a 64 MiB budget hold %d bytes beside a full room budget, within the %d-byte Go memory limit: the constant's account of why 64 MiB no longer fits is stale",
			sockets, draft, int64(goMemoryLimitBytes))
	}
	for _, row := range []struct {
		sockets int64
		fits    bool
	}{{15, true}, {16, false}} {
		withTheWriterFrame := row.sockets*peerWriteQueueBytes + 2*roomFramesUnderTheBudget(row.sockets) + roomBudgetBytes
		if fits := withTheWriterFrame <= goMemoryLimitBytes; fits != row.fits {
			t.Errorf("%d sockets holding the budget, the frame that crossed it and the frame their writer is in the middle of hold %d bytes beside a full room budget, which fits the %d-byte Go memory limit: %t, want %t",
				row.sockets, withTheWriterFrame, int64(goMemoryLimitBytes), fits, row.fits)
		}
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
// admits an update at the byte bound while what it inserts is under maxPeerInsertAtTheBound, until
// the room's last rendering has crept maxPeerByteCreep past pmdoc.MaxDocumentBytes. So the floor
// (maxMessageBytes) has to clear the whole-state frame of the heaviest document grown that way too,
// which a browser hands over on its next sync. This grows heaviestDocument, at both halves of the
// bound with its full margin and anchor marks, through the hook itself (weighPeerUpdate), with
// updates a browser's copy of the room makes the way the editor makes them (pmdoc.Update of its
// tree), of the densest markdown this repository measures: empty items of alternating bullet lists,
// `- \n\n* \n\n`, 2.25 elements a byte (room_budget.go:119-121), each list and item and paragraph
// carrying the block id a browser's write mints. Each update is as large as the hook admits at the
// bound, and the last is taken from a rendering under the creep cap by at most a pair, so the
// document ends as far past the cap as one admitted update can take it; one more pair is then
// refused.
// After each admitted update the room's rendering is published as its update observer publishes
// it (publishCounted rather than the observer's publishSize, whose element count waits on the
// service's measurer: the hook reads the byte half alone). It fails once four times the grown
// document's whole-state frame passes the floor.
func TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor(t *testing.T) {
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
	serverUnrendered := countRoom(room).unrendered
	service := &Service{}
	replica := service.keepReplica(room, nil, &rendering, nil)
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
	// update that makes, reporting what the update inserts and the hook's answer. An update the
	// hook admits is applied to the room, whose new rendering is then published.
	grow := func(pairs int) (inserted int64, refused error) {
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
		weighed := peerWeighing{held: room.StateVector()}
		if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
			t.Fatal(err)
		}
		if err, _ := weighInto(service, room, update); err != nil {
			return weighed.inserted, err
		}
		rendering, err := renderDocument(room)
		if err != nil {
			t.Fatal(err)
		}
		replica.publishCounted(&rendering, countElements(rendering))
		return weighed.inserted, nil
	}
	published := func() int64 {
		bytes, _, _ := replica.size()
		return bytes
	}
	// One small update first, which says what a pair inserts and renders, and so how many pairs
	// the largest update the hook admits at the bound carries.
	const calibration = 64
	loaded := published()
	inserted, err := grow(calibration)
	if err != nil {
		t.Fatalf("the hook refused %d pairs at the bound: %v", calibration, err)
	}
	insertedAPair := (inserted + calibration - 1) / calibration
	renderedAPair := (published() - loaded) / calibration
	largest := int((maxPeerInsertAtTheBound - 1) / insertedAPair)
	creepCap := int64(pmdoc.MaxDocumentBytes + maxPeerByteCreep)
	updates := 1
	for published() < creepCap {
		pairs := largest
		if fit := int((creepCap - 1 - published()) / renderedAPair); fit > 0 && fit < largest {
			pairs = fit
		}
		if inserted, err := grow(pairs); err != nil {
			t.Fatalf("the hook refused %d pairs inserting %d at %d rendered bytes: %v", pairs, inserted, published(), err)
		}
		updates++
	}
	var refusal *websocket.PeerClose
	if _, err := grow(1); !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode {
		t.Fatalf("the hook answered %v to one more pair past the creep cap, want the document's own refusal", err)
	}
	runtime.KeepAlive(replica)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	frame := len(state)
	t.Logf("%d updates of up to %d pairs, each pair inserting %d and rendering %d bytes, took the rendering from %d bytes to %d, against the %d-byte creep cap; "+
		"the whole-state frame grew from %d to %d bytes, four times it %d; the message floor is %d",
		updates, largest, insertedAPair, renderedAPair, loaded, published(), creepCap, serverWritten, frame, 4*frame, maxMessageBytes)
	t.Logf("its unrendered bytes grew from %d to %d, against the %d-byte unrendered bound", serverUnrendered, countRoom(room).unrendered, maxUnrenderedBytes)
	held, _ := applyHeapEstimate(t, room, state)
	handedOver, _ := applyHeapEstimate(t, newDocumentCopy(), state)
	t.Logf("its handoff is estimated at %d bytes into the room holding it and %d into a fresh one, against the %d-byte apply heap bound",
		held, handedOver, uint64(applyHeapBound))
	if frame*4 > maxMessageBytes {
		t.Fatalf("the heaviest whole-state frame a browser can grow is %d bytes, past a quarter of the %d-byte message floor: "+
			"measure it again and set the floor at four times it, or take it back to the design if four times it passes the 64 MiB ygo allowed before",
			frame, maxMessageBytes)
	}
}

// BenchmarkPeerUpdateInsertMeasures weighs the ways to ask what an update carries that the room
// does not already hold, on the update a keystroke sends against a document at
// pmdoc.MaxDocumentBytes: the walk the hook takes (walkUpdate with peerWeighing), against the
// decode it replaced, crdt.ContentIDsFromUpdateV1 with the same arithmetic over the clock runs it
// returns, and against re-encoding the update against the room's state vector
// (crdt.DiffUpdateV1), whose length carries the update's delete set too, so a pure delete would not
// weigh zero.
func BenchmarkPeerUpdateInsertMeasures(b *testing.B) {
	tree, err := pmdoc.Parse(proseAtLeast(pmdoc.MaxDocumentBytes))
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
	browser := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		b.Fatal(err)
	}
	text := firstText(browser.GetXmlFragment(fragmentName))
	if text == nil {
		b.Fatal("the document holds no text to type into")
	}
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "y", nil) })
	update := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
	// The state vector is taken once, as the hook takes it once: what is weighed here is the ways
	// of measuring the update against it.
	held := room.StateVector()
	b.Run("walkUpdate", func(b *testing.B) {
		for b.Loop() {
			weighed := peerWeighing{held: held}
			if _, err := walkUpdate(update, fromPeer, &weighed); err != nil || weighed.inserted != 1 {
				b.Fatalf("the walk weighed %d inserted (%v), want 1", weighed.inserted, err)
			}
		}
	})
	b.Run("ContentIDsFromUpdateV1", func(b *testing.B) {
		for b.Loop() {
			ids, err := crdt.ContentIDsFromUpdateV1(update)
			if err != nil {
				b.Fatal(err)
			}
			inserted := uint64(0)
			for _, client := range ids.Inserts.Clients() {
				for _, run := range ids.Inserts.Ranges(client) {
					if end := run.Clock + run.Len; end > held[client] {
						inserted += end - max(run.Clock, held[client])
					}
				}
			}
			if inserted != 1 {
				b.Fatalf("the decode weighed %d inserted, want 1", inserted)
			}
		}
	})
	b.Run("DiffUpdateV1", func(b *testing.B) {
		for b.Loop() {
			if _, err := crdt.DiffUpdateV1(update, held); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// weighInto hands update to the hook for room, with an apply that applies it to room as ygo's own
// does (provider/websocket/peer_update.go:84-94), and reports what the hook answered and whether it
// called the apply.
func weighInto(service *Service, room *crdt.Doc, update []byte) (err error, called bool) {
	err = service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update}, func() error {
		called = true
		return crdt.ApplyUpdateV1(room, update, nil)
	})
	return err, called
}

// requireRefusedTooLarge requires err to be the document's own refusal, 4413, made without the
// apply, and room to hold before, byte for byte.
func requireRefusedTooLarge(t *testing.T, err error, called bool, room *crdt.Doc, before []byte) {
	t.Helper()
	var refusal *websocket.PeerClose
	if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the hook answered %v, want %d %q", err, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	}
	if called {
		t.Fatal("the hook called the apply for an update it refused")
	}
	if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
		t.Fatal("the room's document changed for an update the hook refused")
	}
}

// countedStructTotal is replica's totals with their counts counted: the count of the room's state
// the hook or the measurer takes calls it once each.
func countedStructTotal(t *testing.T, replica *renderedReplica) (*structTotal, *atomic.Int64) {
	t.Helper()
	total := replica.structs
	if total == nil {
		t.Fatal("the room's load made no struct total")
	}
	counts := new(atomic.Int64)
	total.count = func(doc *crdt.Doc) roomCount {
		counts.Add(1)
		return countRoom(doc)
	}
	return total, counts
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

// An update the walk refuses is never applied: the hook refuses it with 4413
// DOC_UPDATE_REFUSED, the walk's own error behind the close (documentUpdateRefused, refusedWith),
// with which ygo closes that browser and relays nothing, and the room is as it was. ygo
// integrates each struct as it decodes it (sjawhar/ygo v1.54.1-sami.1, crdt/update.go:810-935):
// on a document with no scan bound or budget, a frame of valid structs ending in one bad byte
// leaves those structs in it, and a room's bounded document takes them back out only after it has
// decoded and integrated every one (crdt/apply_rollback.go:9-21), the work the walk keeps off the
// peer's read loop.
func TestAPeerUpdateTheWalkRefusesIsNeverApplied(t *testing.T) {
	for _, row := range []struct {
		name   string
		update []byte
		rule   error
		// halfApplies is whether ygo's own apply into an unbounded copy of the room leaves part of
		// the update in it before it fails: structs ygo integrated before it found the frame bad.
		halfApplies bool
	}{
		{
			name:        "valid GC structs and then one bad byte",
			update:      newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done(),
			rule:        errUpdateUndecodable,
			halfApplies: true,
		},
		{
			name:   "a subdocument",
			update: newV1Frame(1).group(1, 7, 0).rooted(9, marksMapName, "sub").str("guid").any(map[string]any{"autoLoad": false}).noDeletes(),
			rule:   errUpdateNotFromEditor,
		},
		{
			name:   "an unknown root",
			update: newV1Frame(1).group(1, 7, 0).rooted(4, "t", "").str("hi").noDeletes(),
			rule:   errUpdateNotFromEditor,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
			if row.halfApplies {
				copied, err := snapshotDocument(room)
				if err != nil {
					t.Fatal(err)
				}
				if err := crdt.ApplyUpdateV1(copied, row.update, nil); err == nil || copied.StateVector()[7] == 0 {
					t.Fatalf("ygo's own apply answered %v and left client 7 at clock %d, want it to fail having integrated structs", err, copied.StateVector()[7])
				}
			}
			held := room.StateVector()
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, row.update)
			runtime.KeepAlive(replica)
			var refusal *websocket.PeerClose
			if !errors.Is(err, row.rule) || !errors.As(err, &refusal) ||
				refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentUpdateRefusedCloseReason {
				t.Fatalf("the hook answered %v, want the walk's refusal (%v) closed %d %q", err, row.rule, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
			}
			if called {
				t.Fatal("the hook called the apply for an update its walk refused")
			}
			if !maps.Equal(room.StateVector(), held) || !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
				t.Fatalf("the room's state vector is %v, want %v as it was", room.StateVector(), held)
			}
		})
	}
}

// Every refusal the hook makes carries a code and a reason of Dispatch's own. ygo closes a peer
// whose update the hook refused before the apply ran with 4205 "Reset Connection" for any error
// that is no *websocket.PeerClose (sjawhar/ygo v1.54.1-sami.1,
// provider/websocket/peer_update.go:38-52, :139-151), and @hocuspocus/provider 2.x reconnects at
// once after 4205 and sends the refused frame again, which the same rule refuses again: a loop,
// each round a whole sync. 4413 is the band the editor reads as permanent, so it drops the
// refused edit and re-reads the document, and 4503 the transient one it keeps the edit for. The
// rows below drive every refusal through the hook and read the close it answers with.
//
// Two refusals are not here, each because the fixture that reaches it is a drive of its own: an
// apply estimated past applyTooHeavyBytes, 4413 DOC_TOO_LARGE
// (TestAnUpdateHeavierThanAnyApplyMayHoldIsNeverApplied, which builds a frame at the message
// floor), and the admission of a browser the room budget has no room for, 4503 DOC_BUSY, which
// is no refusal of an update at all (ServeHTTP).
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
		// applies is whether the hook calls the apply before it refuses: the room's two scan
		// refusals are the apply's own, made inside it.
		applies bool
		// refusal is the service and room the row drives, the update it hands the hook, and what
		// holds the room's replica while the hook reads it (the service lists it weakly).
		refusal func(t *testing.T) (*Service, *crdt.Doc, []byte, any)
	}{
		{
			name: "bytes ygo's decoder refuses", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done(), replica
			},
		},
		{
			name: "a subdocument, which the editor never sends", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				update := newV1Frame(1).group(1, 7, 0).rooted(9, marksMapName, "sub").str("guid").any(map[string]any{"autoLoad": false}).noDeletes()
				return service, room, update, replica
			},
		},
		{
			name: "a root the editor never writes", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, newV1Frame(1).group(1, 7, 0).rooted(4, "t", "").str("hi").noDeletes(), replica
			},
		},
		{
			name: "a write of the retired clients' list, which only a rebuild writes", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentUpdateRefusedCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				browser, _ := peerDocument(t, room)
				list := browser.GetMap(retiredMapName)
				held := browser.StateVector()
				browser.Transact(func(txn *crdt.Transaction) { list.Set(txn, "789", int64(9)) })
				return service, room, crdt.EncodeStateAsUpdateV1(browser, held), replica
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
			name: "a browser still holding the history a rebuild replaced", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentNeedsRebuildCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := rebuiltRoom(t, ordinary, rebuilt)
				update := newV1Frame(2).group(1, 1000, 0).gc(1).group(1, 123, 0).gc(5).noDeletes()
				return service, room, update, replica
			},
		},
		{
			name: "a 9 KiB paste at the byte bound", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, proseAtLeast(pmdoc.MaxDocumentBytes), pmdoc.MaxDocumentBytes)
				return service, room, peerTyping(t, room, strings.Repeat("p", 9<<10)), replica
			},
		},
		{
			name: "an update past the decode cap", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				held, _ := replica.structs.running()
				return service, room, gcFrameFrom(7, int((maxUpdateItems-held)/(1+splitsAStructCanCause))+1), replica
			},
		},
		{
			name: "an update past the unrendered bound", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				return service, room, marginRecord(1_000, 0, "c00", strings.Repeat("m", maxUnrenderedBytes)), replica
			},
		},
		{
			name: "an update past the heaviest whole-state frame a browser may hand over", code: contracts.DocumentTooLargeCloseCode, reason: contracts.DocumentTooLargeCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				framePadded(t, replica, room, heaviestHandoffBound-countRoom(room).frame)
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
		{
			name: "a keystroke while the room budget is full", code: contracts.DocumentBusyCloseCode, reason: contracts.DocumentBusyCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica := peerUpdateRoom(t, ordinary, 0)
				service.budget.full.Store(true)
				return service, room, peerTyping(t, room, "y"), replica
			},
		},
		{
			name: "a delete past the pressure reserve while the room budget is full", code: contracts.DocumentBusyCloseCode, reason: contracts.DocumentBusyCloseReason,
			refusal: func(t *testing.T) (*Service, *crdt.Doc, []byte, any) {
				service, room, replica, _ := chargedPeerRoom(t)
				service.budget.mu.Lock()
				service.budget.limit = service.budget.used - pressureReserveBytes - 1
				service.budget.mu.Unlock()
				service.budget.full.Store(true)
				return service, room, peerDeleting(t, room, 3), replica
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

// An update that would take the room's struct total past the decode cap is refused with the
// document's own close, whatever it inserts or deletes, and the room is left as it was; one that
// takes it to the cap exactly is admitted. The refusal is confirmed by an exact count of the room
// before it is made: the running total says the update passes the cap, and the count agrees.
func TestAnUpdatePastTheRoomsStructTotalIsRefused(t *testing.T) {
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	for _, row := range []struct {
		name string
		// update is built against the room's exact count, held.
		update  func(held uint64) []byte
		refused bool
	}{
		{
			name: "GC structs one past the cap",
			update: func(held uint64) []byte {
				return gcFrameFrom(7, int((maxUpdateItems-held)/(1+splitsAStructCanCause))+1)
			},
			refused: true,
		},
		{
			name: "GC structs to the cap",
			update: func(held uint64) []byte {
				return gcFrameFrom(7, int((maxUpdateItems-held)/(1+splitsAStructCanCause)))
			},
		},
		{
			name: "delete ranges one past the cap",
			update: func(held uint64) []byte {
				return deleteRangesFrom(7, int((maxUpdateItems-held)/splitsAStructCanCause)+1)
			},
			refused: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, ordinary, 0)
			total, counts := countedStructTotal(t, replica)
			held, stale := total.running()
			if stale || held != countRoomStructs(room) {
				t.Fatalf("the room's total is %d (stale %t), want its exact count %d", held, stale, countRoomStructs(room))
			}
			update := row.update(held)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, update)
			runtime.KeepAlive(replica)
			if row.refused {
				requireRefusedTooLarge(t, err, called, room, before)
				total.mu.Lock()
				confirmed := total.confirmed
				total.mu.Unlock()
				if counts.Load() != 1 || !confirmed {
					t.Fatalf("the hook counted the room %d times (confirmed %t) before refusing, want one confirmation", counts.Load(), confirmed)
				}
				return
			}
			if err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t), want the update admitted", err, called)
			}
			if after, _ := total.running(); after > maxUpdateItems || countRoomStructs(room) > after {
				t.Fatalf("the room's total is %d after the update, want within %d and no less than its exact count %d", after, maxUpdateItems, countRoomStructs(room))
			}
		})
	}
}

// A running total past the cap overstates the room, since it counts every split an update could
// cause: the hook confirms it by an exact count before it refuses, admits the update the count
// leaves room for, and re-bases the total on the count.
func TestARunningStructTotalPastTheCapIsConfirmedBeforeItRefuses(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	total, counts := countedStructTotal(t, replica)
	total.admitted.Add(maxUpdateItems)
	update := peerTyping(t, room, "y")
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
		t.Fatal(err)
	}
	exact := countRoomStructs(room)
	err, called := weighInto(service, room, update)
	runtime.KeepAlive(replica)
	if err != nil || !called {
		t.Fatalf("the hook answered %v (applied %t), want the keystroke admitted once the count confirmed room for it", err, called)
	}
	if counts.Load() != 1 {
		t.Fatalf("the hook counted the room %d times, want one confirmation", counts.Load())
	}
	if after, _ := total.running(); after != exact+weighed.added() {
		t.Fatalf("the room's total is %d after the confirmation, want the count's %d and the keystroke's %d", after, exact, weighed.added())
	}
}

// A total a server write has made stale is confirmed by an exact count before the hook admits on
// it once it is within an eighth of the cap; further from the cap, or fresh, it answers alone.
func TestAStaleStructTotalNearTheCapIsConfirmedBeforeItAdmits(t *testing.T) {
	for _, row := range []struct {
		name string
		// short is how far short of the cap the running total stands.
		short     uint64
		stale     bool
		confirmed bool
	}{
		{name: "a stale total near the cap is confirmed", short: structTotalStaleMargin / 2, stale: true, confirmed: true},
		{name: "a stale total far from the cap is not", short: 2 * structTotalStaleMargin, stale: true},
		{name: "a fresh total near the cap is not", short: structTotalStaleMargin / 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
			total, counts := countedStructTotal(t, replica)
			held, _ := total.running()
			total.admitted.Add(maxUpdateItems - row.short - held)
			if row.stale {
				// A server write: a transaction on the room of an origin that is not a hook's
				// apply, as Server.Apply's are.
				text := firstText(room.GetXmlFragment(fragmentName))
				room.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "s", nil) }, "server")
			}
			if _, stale := total.running(); stale != row.stale {
				t.Fatalf("the total reads stale %t, want %t", stale, row.stale)
			}
			err, called := weighInto(service, room, peerTyping(t, room, "y"))
			runtime.KeepAlive(replica)
			if err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t), want the keystroke admitted", err, called)
			}
			if confirmed := counts.Load() == 1; confirmed != row.confirmed || counts.Load() > 1 {
				t.Fatalf("the hook counted the room %d times, want a confirmation: %t", counts.Load(), row.confirmed)
			}
			if _, stale := total.running(); stale != (row.stale && !row.confirmed) {
				t.Fatalf("the total reads stale %t after the keystroke, want %t", stale, row.stale && !row.confirmed)
			}
		})
	}
}

// A confirmation that the room is at the cap answers for it until the hook has admitted another
// 1/64 of the cap, so a room at the cap is not counted again for every update it refuses; past
// that the hook counts it again.
func TestTheStructTotalsConfirmationIsReusedWithinASixtyFourthOfTheCap(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	total, counts := countedStructTotal(t, replica)
	held, _ := total.running()
	// What the room cannot take is the same frame each time, since nothing refused moves the room.
	pastTheCap := gcFrameFrom(7, int((maxUpdateItems-held)/(1+splitsAStructCanCause))+1)
	refuse := func(wantCounts int64) {
		t.Helper()
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, pastTheCap)
		requireRefusedTooLarge(t, err, called, room, before)
		if counts.Load() != wantCounts {
			t.Fatalf("the hook has counted the room %d times, want %d", counts.Load(), wantCounts)
		}
	}
	admit := func(update []byte) {
		t.Helper()
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("the hook answered %v (applied %t), want the update admitted", err, called)
		}
	}
	refuse(1)
	refuse(1)
	// A keystroke's few structs are well within the sixtieth-fourth.
	admit(peerTyping(t, room, "y"))
	refuse(1)
	// GC structs of another client, just enough that what the hook admitted since the
	// confirmation reaches structTotalRecheck.
	admit(gcFrameFrom(8, structTotalRecheck/(1+splitsAStructCanCause)+1))
	refuse(2)
	refuse(2)
	runtime.KeepAlive(replica)
}

// A room's load counts its struct total from the room's encoded state, parked structs and parked
// delete ranges included, each parked one with the splits it can still cause.
func TestTheStructTotalALoadCountsIsTheWalkOfTheRoomsEncodedState(t *testing.T) {
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
	integrated, err := walkUpdate(crdt.EncodeStateAsUpdateV1(room, nil), fromRoom, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Structs of client 9 from clock 10, whose clocks before 10 the room has never seen, and a
	// delete range of client 11's, which the room has never seen at all: both park.
	parked := newV1Frame(1).group(3, 9, 10).gc(1).gc(1).gc(1).noDeletes()
	if err := crdt.ApplyUpdateV1(room, parked, nil); err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdateV1(room, deleteRangesFrom(11, 2), nil); err != nil {
		t.Fatal(err)
	}
	pending := room.PendingStats()
	if pending.Items == 0 || pending.DeleteRanges == 0 {
		t.Fatalf("the room parked %+v, want parked structs and parked delete ranges", pending)
	}
	walked, err := walkUpdate(crdt.EncodeStateAsUpdateV1(room, nil), fromRoom, nil)
	if err != nil {
		t.Fatal(err)
	}
	if walked.structs < integrated.structs+uint64(pending.Items) {
		t.Fatalf("the room's encoded state walks %d structs, want its %d integrated and its %d parked", walked.structs, integrated.structs, pending.Items)
	}
	service := &Service{}
	rendering := "# Title\n\nOne paragraph of ordinary prose.\n"
	replica := service.keepReplica(room, nil, &rendering, nil)
	want := walked.structs + splitsAStructCanCause*uint64(pending.Items+pending.DeleteRanges)
	if total, stale := replica.structs.running(); total != want || stale {
		t.Fatalf("the load counted a total of %d (stale %t), want the walk's %d structs and the splits of %d parked structs and %d parked ranges, %d",
			total, stale, walked.structs, pending.Items, pending.DeleteRanges, want)
	}
}

// A delete range the room already holds deleted adds nothing to its struct total or its frame
// total: ygo's apply splits the deleted items at its ends and merges the halves back as the
// transaction ends (deletedRuns.splitEnds). A new range in the middle of a run of text adds its
// two splits; one that overlaps the range the room holds, or runs on from it, adds a split only at
// an end outside it; and one of a client the room holds nothing of, which parks, counts its end
// splitsAStructCanCause, the structs the room's count holds for the range it parks
// (structTotalOf), and its start, a client's first clock, nothing. Each is weighed and applied
// through the hook in a room of its own, which holds ten characters of a run deleted: the room's
// exact count grows by no more than the addition, not at all for a range it holds, and its total
// stays at or above the count.
func TestADeleteRangeTheRoomHoldsAddsNothingToTheStructTotal(t *testing.T) {
	const stranger = 11
	for _, row := range []struct {
		name string
		// from and length are the range's clocks, from the run's first, or from a stranger's 0.
		from, length uint64
		// held is whether the room holds the range deleted, and stranger whether the range is of
		// a client the room holds nothing of.
		held, stranger bool
		// splits is the ends the range can leave split, and added what it adds to the total.
		splits, added uint64
	}{
		{name: "the range the room holds", from: 10, length: 10, held: true},
		{name: "a range within the range the room holds", from: 12, length: 3, held: true},
		{name: "a new range in the run", from: 40, length: 5, splits: 2, added: 2},
		{name: "a range over the end of the range the room holds", from: 15, length: 10, splits: 1, added: 1},
		{name: "a range over the start of the range the room holds", from: 5, length: 10, splits: 1, added: 1},
		{name: "a range over the whole range the room holds", from: 5, length: 20, splits: 2, added: 2},
		{name: "a range running on from the range the room holds", from: 20, length: 2, splits: 1, added: 1},
		{name: "a range of a client the room holds nothing of", length: 4, stranger: true, splits: 1, added: splitsAStructCanCause},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, strings.Repeat("word ", 20)+"\n", 0)
			service.budget.limit = roomBudgetBytes
			text := firstText(room.GetXmlFragment(fragmentName))
			first := crdt.CreateRelativePositionFromIndex(text, 0, 0).Item
			last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
			if first == nil || last == nil || last.Client != first.Client || last.Clock != first.Clock+uint64(text.Len())-1 {
				t.Fatalf("the paragraph's %d characters run from %v to %v, want one run of one client's clocks", text.Len(), first, last)
			}
			room.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 10, 10) })
			charge := service.chargeLoadedRoom("room", room, replica)
			replica.structs.recount(room)

			client, from := uint64(first.Client), first.Clock+row.from
			if row.stranger {
				client, from = stranger, row.from
			}
			update := newV1Frame(0).uint(1).uint(client).uint(1).uint(from).uint(row.length).done()
			weighed := peerWeighing{held: room.StateVector(), known: charge.deletes()}
			if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
				t.Fatal(err)
			}
			wantFrame := deleteFrameBytes(client, from, row.length, row.splits)
			if row.held {
				wantFrame = 0
			}
			if weighed.splits != row.splits || weighed.added() != row.added || weighed.frame != wantFrame {
				t.Fatalf("the range adds %d structs with %d splits and %d frame bytes, want %d structs with %d splits and %d frame bytes",
					weighed.added(), weighed.splits, weighed.frame, row.added, row.splits, wantFrame)
			}

			before := countRoomStructs(room)
			if err, called := weighInto(service, room, update); err != nil || !called {
				t.Fatalf("the range was answered %v (applied %t), want it admitted", err, called)
			}
			after := countRoomStructs(room)
			if after-before > row.added || (row.added == 0 && after != before) {
				t.Fatalf("the range took the room's count from %d to %d, want it to grow by no more than the %d it added", before, after, row.added)
			}
			if total, stale := replica.structs.running(); total < after || stale {
				t.Fatalf("the room's total is %d (stale %t), want no less than its exact count %d", total, stale, after)
			}
			runtime.KeepAlive(replica)
		})
	}
}

// Each count the measurer makes of a room's rendering counts the room's struct total afresh, in
// the measurer's turn, re-basing it on what the room holds.
func TestTheMeasurersCountReBasesTheStructTotal(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	service.measurer.interval = noInterval
	total, counts := countedStructTotal(t, replica)
	total.admitted.Add(maxUpdateItems / 2)
	rendering := headings(1)
	replica.publishSize(&rendering)
	service.measurer.waitIdle()
	runtime.KeepAlive(room)
	if got, stale := total.running(); counts.Load() != 1 || got != countRoomStructs(room) || stale {
		t.Fatalf("after the measurer's count the total is %d (stale %t, %d counts), want the room's exact count %d", got, stale, counts.Load(), countRoomStructs(room))
	}
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
// apply. The struct total counts the held apply's addition while it is in flight, so it is never
// below what the room holds, and neither apply marks it stale, though the two ran at once.
func TestAPeerUpdateIsAppliedWhileAnotherBrowsersApplyIsStillRunning(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	total, counts := countedStructTotal(t, replica)
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
	if running, stale := total.running(); running < countRoomStructs(room) || stale {
		t.Fatalf("with the first apply in flight the total is %d (stale %t), want no less than the room's exact count %d and fresh", running, stale, countRoomStructs(room))
	}

	releaseHeld()
	if err := <-firstAnswer; err != nil {
		t.Fatalf("the hook answered %v for the first browser's keystroke, want it admitted", err)
	}
	running, stale := total.running()
	if running < countRoomStructs(room) || stale {
		t.Fatalf("after both applies the total is %d (stale %t), want no less than the room's exact count %d and fresh", running, stale, countRoomStructs(room))
	}
	if counts.Load() != 0 {
		t.Fatalf("the hook counted the room %d times, want none for two keystrokes far short of the cap", counts.Load())
	}
	runtime.KeepAlive(replica)
}

// Two browsers' updates weighed at once near the cap cannot both pass it: the first's addition is
// reserved while its apply runs, and the second is weighed against the total with it. A
// confirmation the second takes re-bases the count and leaves the reservation in flight counted.
// Reservations racing each other admit exactly as many additions as the cap has room for.
func TestTwoReservationsOfTheStructTotalNearTheCapCannotBothPassIt(t *testing.T) {
	const n = 10
	added := uint64(n * (1 + splitsAStructCanCause))

	t.Run("one held inside its apply", func(t *testing.T) {
		service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
		total := replica.structs
		// The room's count stands where one addition fits and two do not.
		near := maxUpdateItems - added - added/2
		var counts atomic.Int64
		total.count = func(*crdt.Doc) roomCount {
			counts.Add(1)
			return roomCount{structs: near}
		}
		total.recount(room)
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseHeld)
		firstAnswer := heldApply(t, service, room, gcFrameFrom(7, n), new(int), release)

		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, gcFrameFrom(8, n))
		requireRefusedTooLarge(t, err, called, room, before)
		if counts.Load() != 2 {
			t.Fatalf("the room was counted %d times, want its re-basing and the second update's confirmation", counts.Load())
		}
		releaseHeld()
		if err := <-firstAnswer; err != nil {
			t.Fatalf("the hook answered %v for the first update, want it admitted", err)
		}
		if running, _ := total.running(); running != near+added {
			t.Fatalf("the total is %d once the first update landed, want the count's %d and its %d", running, near, added)
		}
		runtime.KeepAlive(replica)
	})

	t.Run("racing reservations", func(t *testing.T) {
		_, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
		total := replica.structs
		const fit, racers = 3, 8
		near := maxUpdateItems - fit*added - added/2
		total.count = func(*crdt.Doc) roomCount { return roomCount{structs: near} }
		total.recount(room)
		start := make(chan struct{})
		var passed atomic.Int64
		var wg sync.WaitGroup
		for range racers {
			wg.Go(func() {
				<-start
				if total.reserve(room, roomCount{structs: added}) {
					passed.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		if passed.Load() != fit {
			t.Fatalf("%d of %d racing reservations passed, want the %d the cap has room for", passed.Load(), racers, fit)
		}
		if running, _ := total.running(); running != near+fit*added || running > maxUpdateItems {
			t.Fatalf("the total is %d with the reservations in flight, want %d and within the %d cap", running, near+fit*added, maxUpdateItems)
		}
		runtime.KeepAlive(replica)
	})
}

// applyHeapEstimate is the estimate the hook computes for update arriving at room, a room no load
// listed (peerWeighing.applyHeap, structTotal.roomStructs), and the update's own share of it
// (peerWeighing.ownApplyHeap). The update is walked as a room's own state is (fromRoom), which
// refuses only what ygo cannot decode: the source decides what the walk refuses and nothing of
// what it hands the weighing, so the estimate of an update the hook admits is the hook's, and the
// content the hook refuses from a browser before it estimates anything - binary content and embeds
// (weighPeerUpdate) - is held to the same estimate should a walk ever admit it.
func applyHeapEstimate(t *testing.T, room *crdt.Doc, update []byte) (estimate, own uint64) {
	t.Helper()
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(update, fromRoom, &weighed); err != nil {
		t.Fatal(err)
	}
	var unlisted *structTotal
	return weighed.applyHeap(len(update), unlisted.roomStructs(room)), weighed.ownApplyHeap(len(update))
}

// marginValues is client 7's frame of one item under the margin's key "k" holding n copies of the
// lib0 Any value v.
func marginValues(n int, v any) []byte {
	f := newV1Frame(1).group(1, 7, 0).rooted(byte(structAny), marksMapName, "k").uint(uint64(n))
	for range n {
		f.any(v)
	}
	return f.noDeletes()
}

// marginEntries is client 7's frame of n items under the margin, each the entry of a key of its
// own holding the one lib0 Any value v.
func marginEntries(n int, v any) []byte {
	f := newV1Frame(1).group(n, 7, 0)
	for i := range n {
		f.rooted(byte(structAny), marksMapName, fmt.Sprintf("k%d", i)).uint(1).any(v)
	}
	return f.noDeletes()
}

// marginJSON is client 7's frame of one ContentJSON item under the margin's key "k" holding n
// copies of the JSON text js.
func marginJSON(n int, js string) []byte {
	f := newV1Frame(1).group(1, 7, 0).rooted(byte(structJSON), marksMapName, "k").uint(uint64(n))
	for range n {
		f.str(js)
	}
	return f.noDeletes()
}

// fragmentItems is client 7's frame of n items of content reference kind directly under the
// fragment, content writing each one's content.
func fragmentItems(n int, kind structKind, content func(v1Frame) v1Frame) []byte {
	f := newV1Frame(1).group(n, 7, 0)
	for range n {
		f = content(f.rooted(byte(kind), fragmentName, ""))
	}
	return f.noDeletes()
}

// chainedCharacters is client 7's frame of n one-character strings under the fragment, each after
// the last, and each but the first naming both its origins, the heaviest header an item carries.
func chainedCharacters(n int) []byte {
	f := newV1Frame(1).group(n, 7, 0).rooted(byte(structString), fragmentName, "").str("a")
	for i := 1; i < n; i++ {
		f.raw(byte(structString) | infoHasOrigin | infoHasRightOrigin).uint(7).uint(uint64(i - 1)).uint(7).uint(0).str("a")
	}
	return f.noDeletes()
}

// applyAllocation is what ygo's apply of update into a document into makes allocates, the least of
// three runs, each into a fresh document from into: an upper bound on the heap the apply adds at
// its peak, since no byte it holds can be one it did not allocate. The document has an update
// observer, as every room does (ygo's own, and the room's, websocket.go:617), so the apply
// encodes the update its transaction made for the observers (sjawhar/ygo v1.54.1-sami.1,
// crdt/doc.go:591-595) as a room's does. Two collections before each run empty the pool ygo's
// encoders come from (encoding/pool.go:17-19), whose primary and victim caches each survive one,
// so the encode grows its buffer from nothing, as it does in a pod whose pool a collection emptied.
// scanRefused is whether the apply is to be refused by the document's scan budget
// (crdt.ErrIntegrateScanBudget), which then measures what the refused apply allocates up to its
// refusal and in taking itself back out.
func applyAllocation(t *testing.T, into func() *crdt.Doc, update []byte, scanRefused bool) uint64 {
	t.Helper()
	least := uint64(math.MaxUint64)
	for range 3 {
		doc := into()
		doc.OnUpdate(func([]byte, any) {})
		runtime.GC()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := crdt.ApplyUpdateV1(doc, update, nil)
		runtime.ReadMemStats(&after)
		if scanRefused != errors.Is(err, crdt.ErrIntegrateScanBudget) || (!scanRefused && err != nil) {
			t.Fatalf("the apply answered %v, want the scan bound's refusal: %t", err, scanRefused)
		}
		least = min(least, after.TotalAlloc-before.TotalAlloc)
		runtime.KeepAlive(doc)
	}
	return least
}

// ygo's apply of each update, of every kind of content a browser can send and at scale, allocates
// no more than the hook's estimate of it (peerWeighing.applyHeap), so the bound the estimates are
// held to (applyHeapBound) bounds what the applies hold. Each row is measured as ygo's apply into
// a room's bounded document allocates it (applyAllocation, boundRoomDocument), into a fresh room,
// or into one that already holds every clock the update carries, where ygo still decodes every
// struct and value before it drops them, or into one whose items the update splits, whose deleted
// items the observers' encode walks, or which holds parked structs or parked delete ranges, or
// into one whose scan budget refuses it, measured up to its refusal and its taking itself back out.
// The estimate does not overstate what the editor itself sends past three times, so that the bound
// does not turn away or hold up a real editor: the whole-state handoffs of the heaviest document
// and of a mebibyte of prose into the room holding them, by the whole estimate; and a 64 KiB paste,
// a keystroke and a deletion, by the update's own share, which is what decides whether they wait
// (unweighedApplyHeapBytes). The figures the comments of applyHeapBound and unweighedApplyHeapBytes
// quote are held here too.
//
// Each row is measured in a process of its own (measuredAlone), which builds only what that row
// reads (builtOnce), so no row's collections pay for the heap another row's inputs or the rest of
// the package hold, and the processes run as many at a time as the binary runs parallel tests: run
// in one process beside the whole package, this test took 490 s and the package's peak resident
// memory reached about 11.8 GB, where alone on this pod's two processors it took 20 s, and no
// row's process passed 300 MiB resident (each logs its peak).
func TestEveryApplysTransientHeapIsWithinItsEstimate(t *testing.T) {
	// The rows of many structs carry structs of them, of many Any values values, and of large maps,
	// objects and arrays entries an element; the long string and binary are blob bytes. Each makes
	// the estimate's fixed part (applyHeapPerApply) under a hundredth of the row's measurement, so
	// the per-unit factors decide every comparison, and makes every slice the apply grows by
	// appending long past the 256 elements under which Go doubles one rather than growing it by
	// about a quarter, so each unit costs what it costs at scale. The rows of GC structs stay at
	// maxUpdateItems, the most one update decodes: 1.4 s and about 250 MiB in a process of their
	// own.
	const (
		structs = 20_000
		values  = 100_000
		entries = 10_000
		blob    = 512 << 10
	)
	type (
		room  = func(t *testing.T) *crdt.Doc
		frame = func(t *testing.T) []byte
	)
	built := func(build func() []byte) frame { return builtOnce(func(*testing.T) []byte { return build() }) }
	// Every room is a room's bounded document (boundRoomDocument), holding what it holds before it is
	// bounded, as a room's load decodes its stored state unbounded.
	fresh := func(*testing.T) *crdt.Doc { return boundRoomDocument(newDocumentCopy()) }
	holding := func(updates ...frame) room {
		return func(t *testing.T) *crdt.Doc {
			doc := newDocumentCopy()
			for _, update := range updates {
				if err := crdt.ApplyUpdateV1(doc, update(t), nil); err != nil {
					t.Fatal(err)
				}
			}
			return boundRoomDocument(doc)
		}
	}
	// holdingClocks is a room that already holds client's first length clocks, as one GC run.
	holdingClocks := func(client, length uint64) room {
		return holding(built(func() []byte { return newV1Frame(1).group(1, client, 0).gc(length).noDeletes() }))
	}
	heaviest := builtOnce(heaviestDocument)
	heaviestState := builtOnce(func(t *testing.T) []byte { return crdt.EncodeStateAsUpdateV1(heaviest(t), nil) })
	prose := builtOnce(func(t *testing.T) []byte { return crdt.EncodeStateAsUpdateV1(residentRoomDocument(t, 1<<20), nil) })
	paste := builtOnce(func(t *testing.T) []byte {
		return peerTyping(t, heaviest(t), strings.Repeat("pasted words ", (64<<10)/13))
	})
	keystroke := builtOnce(func(t *testing.T) []byte { return peerTyping(t, heaviest(t), "y") })
	deletion := builtOnce(func(t *testing.T) []byte { return peerDeleting(t, heaviest(t), 1) })
	manyClients := func(clients int) room {
		return holding(built(func() []byte {
			f := newV1Frame(clients)
			for c := range clients {
				f = f.group(1, uint64(100+c), 0).gc(1)
			}
			return f.noDeletes()
		}))
	}
	oneCharacter := built(func() []byte {
		return fragmentItems(1, structString, func(f v1Frame) v1Frame { return f.str("a") })
	})
	record := map[string]any{"kind": "comment", "by": "user:alice", "text": strings.Repeat("m", 200), "at": 1.5e12, "replies": []any{"r", int64(3)}}
	bigMap := func() map[string]any {
		big := map[string]any{}
		for i := range entries {
			big[fmt.Sprint(i)] = nil
		}
		return big
	}
	bigObject := func() string {
		var big strings.Builder
		big.WriteString("{")
		for i := range entries {
			if i > 0 {
				big.WriteString(",")
			}
			fmt.Fprintf(&big, `"%d":0`, i)
		}
		big.WriteString("}")
		return big.String()
	}
	bigArray := func() string { return "[" + strings.TrimSuffix(strings.Repeat("0,", entries), ",") + "]" }
	legacyJSON := func(n int) []byte {
		f := newV1Frame(1).group(1, 7, 0).rooted(byte(structJSON), marksMapName, "k").uint(uint64(n))
		for range n {
			f.any(map[string]any{"": nil})
		}
		return f.noDeletes()
	}
	skips := func(n int) []byte {
		f := newV1Frame(1).group(n, 7, 0)
		for range n {
			f.skip(1)
		}
		return f.noDeletes()
	}
	// A room holding client 7's string of structs characters, and the same with every other
	// character deleted, structs/2 deleted items; client 8's characters each inserted between two
	// of client 7's, splitting the string at both origins; and client 8's keystroke at the
	// string's end.
	text := built(func() []byte {
		return fragmentItems(1, structString, func(f v1Frame) v1Frame { return f.str(strings.Repeat("a", structs)) })
	})
	everyOther := built(func() []byte { return deleteRangesFrom(7, structs/2) })
	between := built(func() []byte {
		f := newV1Frame(1).group(structs/2, 8, 0)
		for i := range structs / 2 {
			f.raw(byte(structString) | infoHasOrigin | infoHasRightOrigin).uint(7).uint(uint64(2 * i)).uint(7).uint(uint64(2*i + 1)).str("b")
		}
		return f.noDeletes()
	})
	atTheEnd := built(func() []byte {
		return newV1Frame(1).group(1, 8, 0).raw(byte(structString) | infoHasOrigin).uint(7).uint(structs - 1).str("y").noDeletes()
	})
	tombstoned := builtOnce(func(t *testing.T) []byte { return crdt.EncodeStateAsUpdateV1(holding(text, everyOther)(t), nil) })
	// Rooms holding within eight of the most parked structs and of the most parked delete ranges
	// their struct total admits, each parked struct counting three and each parked range two
	// (countRoomStructs): GC structs of client 8 from clock 1, whose clock 0 the room has never
	// seen, and ranges of client 11, which it has never seen at all. A bounded document's apply
	// keeps them by reference, so a keystroke pays nothing for them; client 11's own first
	// character moves its clock past the first parked range, and the apply retries every range of
	// it, re-parking all but the first. The estimate prices none of it apart from the room's
	// struct total, which counts every parked struct and range.
	parkedStructs := built(func() []byte {
		n := maxUpdateItems/(1+splitsAStructCanCause) - 8
		f := newV1Frame(1).group(n, 8, 1)
		for range n {
			f.gc(1)
		}
		return f.noDeletes()
	})
	parkedRanges := built(func() []byte { return deleteRangesFrom(11, maxUpdateItems/splitsAStructCanCause-8) })
	parkedRangesClient := built(func() []byte {
		return newV1Frame(1).group(1, 11, 0).rooted(byte(structString), fragmentName, "").str("a").noDeletes()
	})
	// A room holding a run of 2,000 characters client 8 typed one keystroke at a time after client
	// 7's one character, each an item of its own (chainedRun), and a frame whose items each name
	// the run's first character and no right origin, enough of them that their scans pass the
	// per-frame ceiling, the allowance of an apply of no browser's (roomScans.budget): the room's
	// scan budget refuses it once its scans reach the ceiling.
	const run = 2_000
	keystrokeRun := func(t *testing.T) *crdt.Doc {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, oneCharacter(t), nil); err != nil {
			t.Fatal(err)
		}
		chainedRun(t, doc, crdt.ID{Client: 7}, 8, run)
		return boundRoomDocument(doc)
	}
	pastTheCeiling := built(func() []byte { return scanningFrame(9, crdt.ID{Client: 8}, perFrameScanCeiling/run+1) })
	// editor is whether the update is one the editor sends, which the estimate may not overstate
	// past three times: by the whole estimate, or by the update's own share. A whole-state handoff
	// of a document into a room holding none of it is no update the editor sends a room, which
	// loads the document before it takes any browser; a fresh room's budget gives it the per-frame
	// ceiling, past the 34,200 items its decode into a document holding nothing scans for the
	// mebibyte of prose and the 49,156 for the heaviest document, so it applies, and the estimate
	// holds over what the whole apply allocates.
	const (
		notTheEditors = iota
		editorsWhole
		editorsOwn
		scanRefused
	)
	for _, row := range []struct {
		name   string
		into   room
		update frame
		editor int
	}{
		{"100,000 null Any values in one struct", fresh, built(func() []byte { return marginValues(values, nil) }), notTheEditors},
		{"100,000 null Any values in one struct, into a room holding them", holdingClocks(7, values), built(func() []byte { return marginValues(values, nil) }), notTheEditors},
		{"maps of one entry, the densest Any, in one struct", fresh, built(func() []byte { return marginValues(structs, map[string]any{"": nil}) }), notTheEditors},
		{"maps of one empty map in one struct", fresh, built(func() []byte { return marginValues(structs, map[string]any{"": map[string]any{}}) }), notTheEditors},
		{"arrays of one map in one struct", fresh, built(func() []byte { return marginValues(structs, []any{map[string]any{"": nil}}) }), notTheEditors},
		{"strings of nine bytes in one struct", fresh, built(func() []byte { return marginValues(structs, "123456789") }), notTheEditors},
		{"maps of 10,000 entries", fresh, built(func() []byte { return marginValues(10, bigMap()) }), notTheEditors},
		{"margin entries of one null each", fresh, built(func() []byte { return marginEntries(structs, nil) }), notTheEditors},
		{"margin entries of one null each, into a room holding them", holdingClocks(7, structs), built(func() []byte { return marginEntries(structs, nil) }), notTheEditors},
		{"margin entries of one record each", fresh, built(func() []byte { return marginEntries(structs/10, record) }), notTheEditors},
		{"null JSON texts in one struct", fresh, built(func() []byte { return marginJSON(structs, "null") }), notTheEditors},
		{"number JSON texts in one struct", fresh, built(func() []byte { return marginJSON(structs, "0") }), notTheEditors},
		{"JSON objects of one entry in one struct", fresh, built(func() []byte { return marginJSON(structs, `{"":0}`) }), notTheEditors},
		{"JSON objects of one empty object in one struct", fresh, built(func() []byte { return marginJSON(structs, `{"":{}}`) }), notTheEditors},
		{"JSON arrays of 10,000 numbers", fresh, built(func() []byte { return marginJSON(20, bigArray()) }), notTheEditors},
		{"JSON objects of 10,000 entries", fresh, built(func() []byte { return marginJSON(20, bigObject()) }), notTheEditors},
		{"JSON strings of 1,000 escapes", fresh, built(func() []byte { return marginJSON(200, `"`+strings.Repeat(`\n`, 500)+`"`) }), notTheEditors},
		{"legacy Any JSON values in one struct", fresh, built(func() []byte { return legacyJSON(structs) }), notTheEditors},
		{"a string of 512 KiB", fresh, built(func() []byte {
			return fragmentItems(1, structString, func(f v1Frame) v1Frame { return f.str(strings.Repeat("a", blob)) })
		}), notTheEditors},
		{"one-character strings each naming both origins", fresh, built(func() []byte { return chainedCharacters(structs) }), notTheEditors},
		{"one-character strings each splitting the room's string", holding(text), between, notTheEditors},
		{"strings of seventeen bytes", fresh, built(func() []byte {
			return fragmentItems(structs, structString, func(f v1Frame) v1Frame { return f.str("12345678901234567") })
		}), notTheEditors},
		{"binary of 512 KiB", fresh, built(func() []byte {
			return fragmentItems(1, structBinary, func(f v1Frame) v1Frame { return f.str(strings.Repeat("a", blob)) })
		}), notTheEditors},
		{"empty binaries", fresh, built(func() []byte {
			return fragmentItems(structs, structBinary, func(f v1Frame) v1Frame { return f.str("") })
		}), notTheEditors},
		{"embeds of an empty object", fresh, built(func() []byte {
			return fragmentItems(structs, structEmbed, func(f v1Frame) v1Frame { return f.str("{}") })
		}), notTheEditors},
		{"formats of an empty object", fresh, built(func() []byte {
			return fragmentItems(structs, structFormat, func(f v1Frame) v1Frame { return f.str("bold").str("{}") })
		}), notTheEditors},
		{"XML elements", fresh, built(func() []byte {
			return fragmentItems(structs, structType, func(f v1Frame) v1Frame { return f.raw(3).str("paragraph") })
		}), notTheEditors},
		{"XML texts", fresh, built(func() []byte {
			return fragmentItems(structs, structType, func(f v1Frame) v1Frame { return f.raw(6) })
		}), notTheEditors},
		{"deleted items", fresh, built(func() []byte {
			return fragmentItems(structs, structDeleted, func(f v1Frame) v1Frame { return f.uint(1) })
		}), notTheEditors},
		{"maxUpdateItems GC structs", fresh, built(func() []byte { return gcFrameFrom(7, maxUpdateItems) }), notTheEditors},
		{"maxUpdateItems GC structs, into a room holding them", holdingClocks(7, maxUpdateItems), built(func() []byte { return gcFrameFrom(7, maxUpdateItems) }), notTheEditors},
		{"skips", fresh, built(func() []byte { return skips(structs) }), notTheEditors},
		{"delete ranges of a client the room has not seen", fresh, built(func() []byte { return deleteRangesFrom(7, structs) }), notTheEditors},
		{"delete ranges each splitting the room's string", holding(text), everyOther, notTheEditors},
		{"one character into a room of 20,000 clients", manyClients(structs), oneCharacter, notTheEditors},
		{"one character into a room of 57,345 clients", manyClients(57_345), oneCharacter, notTheEditors},
		{"one character into a string of 10,000 deleted items", holding(text, everyOther), atTheEnd, notTheEditors},
		{"one character into a room holding the most parked structs", holding(parkedStructs), oneCharacter, notTheEditors},
		{"one character into a room holding the most parked delete ranges", holding(parkedRanges), oneCharacter, notTheEditors},
		{"one character of the client whose parked delete ranges it starts to apply", holding(parkedRanges), parkedRangesClient, notTheEditors},
		{"the whole state of a string of 10,000 deleted items into the room holding it", holding(text, everyOther), tombstoned, notTheEditors},
		{"the heaviest handoff into the room holding it", holding(heaviestState), heaviestState, editorsWhole},
		{"the heaviest handoff into a fresh room", fresh, heaviestState, notTheEditors},
		{"a mebibyte of prose handed off into the room holding it", holding(prose), prose, editorsWhole},
		{"a mebibyte of prose handed off into a fresh room", fresh, prose, notTheEditors},
		{"a frame refused at the scan ceiling, into a room holding a run of keystrokes", keystrokeRun, pastTheCeiling, scanRefused},
		{"a 64 KiB paste into the heaviest document", holding(heaviestState), paste, editorsOwn},
		{"a keystroke into the heaviest document", holding(heaviestState), keystroke, editorsOwn},
		{"a deletion of one character from the heaviest document", holding(heaviestState), deletion, editorsOwn},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if !measuredAlone(t) {
				return
			}
			update := row.update(t)
			into := func() *crdt.Doc { return row.into(t) }
			estimate, own := applyHeapEstimate(t, into(), update)
			allocated := applyAllocation(t, into, update, row.editor == scanRefused)
			t.Logf("a %d-byte update allocates %d bytes, estimated at %d (%.2f times), its own share %d (%.2f times)",
				len(update), allocated, estimate, float64(estimate)/float64(allocated), own, float64(own)/float64(allocated))
			if allocated > estimate {
				t.Errorf("the apply allocates %d bytes, past its estimate of %d", allocated, estimate)
			}
			if row.editor == editorsWhole && estimate > 3*allocated {
				t.Errorf("the estimate of %d bytes is more than three times the %d the editor's update allocates", estimate, allocated)
			}
			if row.editor == editorsOwn && own > 3*allocated {
				t.Errorf("the update's own share of %d bytes is more than three times the %d it allocates", own, allocated)
			}
		})
	}
	t.Run("the heaviest document's handoffs, paste and keystroke against the bounds", func(t *testing.T) {
		t.Parallel()
		if !measuredAlone(t) {
			return
		}
		held, _ := applyHeapEstimate(t, holding(heaviestState)(t), heaviestState(t))
		handedOver, _ := applyHeapEstimate(t, fresh(t), heaviestState(t))
		_, pasted := applyHeapEstimate(t, holding(heaviestState)(t), paste(t))
		_, typed := applyHeapEstimate(t, holding(heaviestState)(t), keystroke(t))
		t.Logf("the heaviest handoff is estimated at %d bytes into the room holding it, %d into a fresh one; a 64 KiB paste's own share is %d, a keystroke's %d",
			held, handedOver, pasted, typed)
		if 5*held > applyHeapBound || handedOver > applyHeapBound {
			t.Errorf("the heaviest handoff is estimated at %d bytes into the room holding it and %d into a fresh one, past a fifth and the whole of the %d-byte bound",
				held, handedOver, uint64(applyHeapBound))
		}
		if 3*pasted > unweighedApplyHeapBytes || 400*typed > unweighedApplyHeapBytes {
			t.Errorf("a 64 KiB paste's own share is %d bytes and a keystroke's %d, past a third and a four-hundredth of the %d unweighed",
				pasted, typed, unweighedApplyHeapBytes)
		}
	})
}

// builtOnce is build's result, built by the first call with that call's test and kept for every
// call after it. TestEveryApplysTransientHeapIsWithinItsEstimate runs each row in a process of
// its own, so the first call is that row's, and its test is the one a failed build fails.
func builtOnce[V any](build func(t *testing.T) V) func(t *testing.T) V {
	var (
		done  bool
		value V
	)
	return func(t *testing.T) V {
		t.Helper()
		if !done {
			value, done = build(t), true
		}
		return value
	}
}

// Admitted updates whose own share of their estimate passes unweighedApplyHeapBytes apply only
// while their estimates together stay within applyHeapBound, for the whole service and in rooms of
// their own: with as many applying as the bound has room for, the next waits until one ends. A
// keystroke, under the threshold, applies at once, even in the room whose heavy update is waiting,
// since a waiter holds nothing of its room.
//
// Each heavy update is what a reconnect storm's handoffs are, a frame of what its room already
// holds: 4,000,000 values for clocks of client 7 each room holds (heldClocks), so it inserts
// nothing and adds no struct, and none of the document's rules refuses it - the byte rule weighs a
// room no load measured by the update's own bytes, and a 4 MB frame that inserted its values would
// be refused there before its apply - while ygo's apply decodes every value of it all the same,
// which is what its estimate weighs. Every wait is bounded, so a heavy update that never applies
// fails the test rather than hanging it.
func TestTheApplyHeapBoundHoldsConcurrentApplies(t *testing.T) {
	const heavyValues = 4_000_000
	service := &Service{}
	heavy := marginValues(heavyValues, nil)
	heldClocks := func(room *crdt.Doc) *crdt.Doc {
		t.Helper()
		if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, 7, 0).gc(heavyValues).noDeletes(), nil); err != nil {
			t.Fatal(err)
		}
		return boundRoomDocument(room)
	}
	estimate, own := applyHeapEstimate(t, heldClocks(newDocumentCopy()), heavy)
	fits := int(applyHeapBound / estimate)
	if own <= unweighedApplyHeapBytes || fits < 1 || fits > 8 {
		t.Fatalf("the heavy update is estimated at %d bytes, its own share %d, %d to the bound, want its share past %d and a few to the bound",
			estimate, own, fits, unweighedApplyHeapBytes)
	}
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	typedInto := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(typedInto, seed, nil); err != nil {
		t.Fatal(err)
	}
	heldClocks(typedInto)
	keystroke := peerTyping(t, typedInto, "y")
	if _, typed := applyHeapEstimate(t, typedInto, keystroke); typed > unweighedApplyHeapBytes {
		t.Fatalf("the keystroke's own share is %d bytes, past the %d unweighed", typed, unweighedApplyHeapBytes)
	}
	waiting, _ := applyHeapEstimate(t, typedInto, heavy)
	var holding, most atomic.Uint64
	var running atomic.Int32
	release := make(chan struct{})
	var released sync.Once
	releaseAll := func() { released.Do(func() { close(release) }) }
	started := make(chan int, fits+1)
	refused := make(chan error, fits+1)
	var applies sync.WaitGroup
	// awaitApplies releases every apply and waits a bounded time for each heavy update to return.
	awaitApplies := func() bool {
		releaseAll()
		returned := make(chan struct{})
		go func() {
			applies.Wait()
			close(returned)
		}()
		select {
		case <-returned:
			return true
		case <-time.After(time.Minute):
			return false
		}
	}
	// A test that fails while heavy updates are applying or waiting releases them before it ends.
	t.Cleanup(func() {
		if !awaitApplies() {
			t.Error("the heavy updates had not returned a minute after their release")
		}
	})
	weigh := func(i int, room *crdt.Doc, weight uint64) {
		applies.Go(func() {
			err := service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: heavy}, func() error {
				now := holding.Add(weight)
				for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
				}
				running.Add(1)
				started <- i
				<-release
				running.Add(-1)
				holding.Add(-weight)
				return nil
			})
			if err != nil {
				refused <- fmt.Errorf("heavy update %d: %w", i, err)
			}
		})
	}
	for i := range fits {
		weigh(i, heldClocks(newDocumentCopy()), estimate)
	}
	for range fits {
		select {
		case <-started:
		case err := <-refused:
			t.Fatalf("the hook refused a heavy update the bound has room for: %v", err)
		case <-time.After(time.Minute):
			t.Fatalf("%d heavy updates the bound has room for had not all started their applies after a minute", fits)
		}
	}
	// The bound has no room for another: the heavy update left over waits, in the room the
	// keystroke goes to, and the keystroke does not wait.
	weigh(fits, typedInto, waiting)
	select {
	case i := <-started:
		t.Fatalf("heavy update %d applied with %d holding the bound", i, fits)
	case err := <-refused:
		t.Fatalf("the hook refused the heavy update the bound has no room for, want it waiting: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	typed := make(chan error, 1)
	go func() {
		typed <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: typedInto, Update: keystroke}, func() error { return nil })
	}()
	select {
	case err := <-typed:
		if err != nil {
			t.Fatalf("the hook refused the keystroke: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the keystroke waited behind the heavy update waiting on the apply heap bound")
	}
	if got := running.Load(); got != int32(fits) {
		t.Fatalf("%d heavy updates are applying, want the %d the bound has room for", got, fits)
	}
	if !awaitApplies() {
		t.Fatal("the heavy updates had not returned a minute after their release")
	}
	close(refused)
	for err := range refused {
		t.Errorf("the hook refused a heavy update: %v", err)
	}
	if got := most.Load(); got > applyHeapBound || got < uint64(fits)*estimate {
		t.Fatalf("the heavy applies held %d bytes of estimate at most, want at least the %d of %d at once and never past the %d-byte bound",
			got, uint64(fits)*estimate, fits, applyHeapBound)
	}
}

// An admitted update whose estimate passes applyHeapBound, and not applyTooHeavyBytes, is never
// refused for its weight: it waits while any part of the bound is held, then holds all of it, so
// it applies alone, and gives it all back once it has. Each update is a frame of null values for a
// client whose every clock its room holds, which inserts nothing and adds no struct, so that no
// rule of the document's refuses it while ygo's apply would decode every value: one under the
// bound applies first and holds its part until released, and one past the bound waits until then.
func TestAnApplyPastTheApplyHeapBoundWaitsForAllOfItAndRunsAlone(t *testing.T) {
	const lightValues, heavyValues = 4_000_000, 7_000_000
	service := &Service{}
	holding := func(values int) *crdt.Doc {
		room := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, 7, 0).gc(uint64(values)).noDeletes(), nil); err != nil {
			t.Fatal(err)
		}
		return boundRoomDocument(room)
	}
	lightRoom, heavyRoom := holding(lightValues), holding(heavyValues)
	light, heavy := marginValues(lightValues, nil), marginValues(heavyValues, nil)
	lightEstimate, _ := applyHeapEstimate(t, lightRoom, light)
	heavyEstimate, own := applyHeapEstimate(t, heavyRoom, heavy)
	t.Logf("the light update is estimated at %d bytes and the heavy one at %d, against the %d-byte bound and the %d-byte line",
		lightEstimate, heavyEstimate, uint64(applyHeapBound), uint64(applyTooHeavyBytes))
	if lightEstimate >= applyHeapBound || heavyEstimate <= applyHeapBound || heavyEstimate > applyTooHeavyBytes || own <= unweighedApplyHeapBytes {
		t.Fatalf("the light update is estimated at %d bytes and the heavy one at %d, its own share %d, want the light under the %d-byte bound and the heavy past it, within the %d-byte line and weighed",
			lightEstimate, heavyEstimate, own, uint64(applyHeapBound), uint64(applyTooHeavyBytes))
	}
	release := make(chan struct{})
	lightStarted, heavyStarted := make(chan struct{}), make(chan bool, 1)
	answers := make(chan error, 2)
	var applies sync.WaitGroup
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		applies.Wait()
	})
	applies.Go(func() {
		answers <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "light", Doc: lightRoom, Update: light}, func() error {
			close(lightStarted)
			<-release
			return nil
		})
	})
	select {
	case <-lightStarted:
	case err := <-answers:
		t.Fatalf("the hook answered the light update %v before applying it", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the light update had not started its apply after ten seconds")
	}
	applies.Go(func() {
		answers <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "heavy", Doc: heavyRoom, Update: heavy}, func() error {
			bound := service.applyHeaps.bound
			alone := !bound.TryAcquire(1)
			if !alone {
				bound.Release(1)
			}
			heavyStarted <- alone
			return nil
		})
	})
	select {
	case <-heavyStarted:
		t.Fatal("the heavy update applied while the light one held part of the bound")
	case err := <-answers:
		t.Fatalf("the hook answered %v to an update past the bound while the light one applied, want it waiting", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case alone := <-heavyStarted:
		if !alone {
			t.Error("the heavy update applied with part of the bound free, want it holding all of it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the heavy update had not applied ten seconds after the light one gave its part back")
	}
	applies.Wait()
	close(answers)
	for err := range answers {
		if err != nil {
			t.Errorf("the hook refused an update: %v", err)
		}
	}
	if !service.applyHeaps.bound.TryAcquire(applyHeapBound) {
		t.Fatal("the applies left part of the bound held once both had returned")
	}
}

// An update whose estimate alone passes applyTooHeavyBytes could not apply even alone, so the
// hook refuses it with the document's own close, 4413 DOC_TOO_LARGE, errApplyTooHeavy behind it
// (refusedWith), as it refuses a frame the walk refuses, and never applies
// it: a frame at the message floor of null values for a client whose every clock the room already
// holds, which inserts nothing and adds no struct, so that every other rule admits it. One such
// frame of 25,165,779 bytes, at the floor before it rose to 37 MiB, measured to allocate
// 805,307,736 bytes in one apply. The room holds a clock for every byte of the floor, more than
// the frame has values.
func TestAnUpdateHeavierThanAnyApplyMayHoldIsNeverApplied(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, 7, 0).gc(maxMessageBytes).noDeletes(), nil); err != nil {
		t.Fatal(err)
	}
	update := marginValues(maxMessageBytes-64, nil)
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
		t.Fatal(err)
	}
	if weighed.inserted != 0 || weighed.added() != 0 || len(update) > maxMessageBytes {
		t.Fatalf("the %d-byte update inserts %d and adds %d structs, want a frame within the floor that inserts and adds nothing", len(update), weighed.inserted, weighed.added())
	}
	estimate := weighed.applyHeap(len(update), replica.structs.roomStructs(room))
	if estimate <= applyTooHeavyBytes {
		t.Fatalf("the %d-byte update is estimated at %d bytes, within the %d-byte line", len(update), estimate, uint64(applyTooHeavyBytes))
	}
	t.Logf("a %d-byte update of null values is estimated at %d bytes, against the %d-byte line", len(update), estimate, uint64(applyTooHeavyBytes))
	held := room.StateVector()
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	err, called := weighInto(service, room, update)
	runtime.KeepAlive(replica)
	var refusal *websocket.PeerClose
	if !errors.Is(err, errApplyTooHeavy) || !errors.As(err, &refusal) ||
		refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the hook answered %v, want the apply heap's refusal closed %d %q", err, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	}
	if called {
		t.Fatal("the hook called the apply for an update heavier than the line")
	}
	if !maps.Equal(room.StateVector(), held) || !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
		t.Fatal("the room changed for an update the hook refused")
	}
}

// heaviestHandoffMeasurement is the environment variable that runs the heavy rows of
// TestAnHonestHandoffAtTheHeaviestFrameIsNeverRefusedAsTooHeavy.
const heaviestHandoffMeasurement = "DISPATCH_MEASURE_HEAVIEST_HANDOFF"

// An honest whole-state handoff at F (heaviestHandoffBound), the heaviest frame the hook lets a
// browser hand over, is never refused as too heavy (errApplyTooHeavy): its estimate stays under
// applyTooHeavyBytes, and the hook applies it, holding the whole apply-heap bound where its
// estimate passes the bound, so that it runs alone. Each row is a room holding a document whose
// frame text nested under the margin (handoffAtTheHeaviestFrame), which grows the frame and
// nothing else the hook weighs, takes to 16 KiB short of F, and the handoff of a browser holding
// that room that has typed a few words since, a frame within a few kibibytes of F; a document at
// the decode cap, to which nothing can be added, is handed back as it stands, and the room's
// struct total may refuse it as too large, never the apply heap as too heavy. Each handoff is
// estimated into the room holding it, which its load charged, as a browser's sync finds it; a
// fresh room loads its document before it takes any browser. Each row runs in a process of its
// own (measuredAlone).
//
// Measured on 2026-10-28, the estimates into the room holding each: a paragraph padded to F,
// 404,291,526 bytes, past the 393,705,335-byte bound, so it runs alone; the long-edited fixture,
// 494,310 structs, padded to F, 657,900,131; that document typed on until 4,096 structs short of
// the decode cap and padded to F, 904,378,600, the heaviest; and as they stand, the long-edited
// document at the decode cap 500,387,754 and the agent-rewritten one 549,442,004. Every one of
// them is under the 1,181,116,006-byte line, the heaviest by 276,737,406. The fast rows are the
// paragraph and the long-edited fixture, 2.7 and 5.8 s here, their processes' peaks 404 and 636
// MiB; set DISPATCH_MEASURE_HEAVIEST_HANDOFF=1 for the others, which reach 905 MiB, about 30 s
// together with the agent's state cached (DISPATCH_AGENT_REWRITTEN_CACHE).
func TestAnHonestHandoffAtTheHeaviestFrameIsNeverRefusedAsTooHeavy(t *testing.T) {
	heavy := os.Getenv(heaviestHandoffMeasurement) != ""
	for _, row := range []struct {
		name  string
		heavy bool
		// atCap is whether the document stands at the decode cap and is handed back as it stands.
		atCap bool
		state func(t *testing.T) []byte
	}{
		{"a paragraph", false, false, func(t *testing.T) []byte {
			tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
			if err != nil {
				t.Fatal(err)
			}
			seed, err := encodeDocumentTree(tree)
			if err != nil {
				t.Fatal(err)
			}
			return seed
		}},
		{"the long-edited document", false, false, func(t *testing.T) []byte { return longEditedState(t) }},
		{"the long-edited document typed on until 4,096 structs short of the decode cap", true, false, func(t *testing.T) []byte {
			return longEditedTypedTo(t, maxUpdateItems-4_096)
		}},
		{"the long-edited document typed on until the decode cap", true, true, longEditedAtCapState},
		{"an agent's whole rewrites until the decode cap refused one", true, true, cachedAgentRewrittenState},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.heavy && !heavy {
				t.Skip("set " + heaviestHandoffMeasurement + "=1 to run the heavy rows")
			}
			if !measuredAlone(t) {
				return
			}
			room, handoff := handoffAtTheHeaviestFrame(t, row.state(t), !row.atCap)
			estimate, own := applyHeapEstimate(t, room, handoff)
			t.Logf("a %d-byte handoff, against F's %d, into a room of %d structs is estimated at %d bytes, its own share %d; the apply-heap bound is %d and the too-heavy line %d",
				len(handoff), uint64(heaviestHandoffBound), countRoomStructs(room), estimate, own, uint64(applyHeapBound), uint64(applyTooHeavyBytes))
			if !row.atCap && len(handoff) < heaviestHandoffBound-64<<10 {
				t.Fatalf("the handoff is %d bytes, want it within 64 KiB of F's %d", len(handoff), uint64(heaviestHandoffBound))
			}
			if estimate > applyTooHeavyBytes {
				t.Errorf("the handoff is estimated at %d bytes, past the %d-byte line past which an apply is refused as too heavy", estimate, uint64(applyTooHeavyBytes))
			}
			rendering, err := renderDocument(room)
			if err != nil {
				t.Fatal(err)
			}
			// The room's load charges it, which lists its delete set as the ranges it holds
			// deleted (deletedRuns), so a handoff carrying them is weighed as adding none.
			service := &Service{budget: roomBudget{limit: roomBudgetBytes}}
			replica := service.keepReplica(room, nil, &rendering, nil)
			service.chargeLoadedRoom("room", room, replica)
			called, alone := false, false
			err = service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: handoff}, func() error {
				called = true
				if bound := service.applyHeaps.bound; bound != nil {
					alone = !bound.TryAcquire(1)
					if !alone {
						bound.Release(1)
					}
				}
				return crdt.ApplyUpdateV1(room, handoff, nil)
			})
			runtime.KeepAlive(replica)
			t.Logf("the hook answered %v, applied %t, holding the whole bound %t", err, called, alone)
			if errors.Is(err, errApplyTooHeavy) {
				t.Fatalf("the hook refused the handoff as too heavy: %v", err)
			}
			if row.atCap {
				return
			}
			if err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t), want the handoff applied", err, called)
			}
			if past := estimate > applyHeapBound; alone != past {
				t.Errorf("the handoff held the whole bound %t while it applied, want %t for an estimate of %d against the %d-byte bound", alone, past, estimate, uint64(applyHeapBound))
			}
		})
	}
}

// handoffAtTheHeaviestFrame is a room holding state, and the whole-state handoff of a browser
// holding it. Where pad says, the room first takes a text nested under the margin
// (nestedMarginText), pasted in one transaction until the room's whole-state frame stands 16 KiB
// short of F (heaviestHandoffBound), and the browser types a few words into its first text before
// it hands over: a frame within a few kibibytes of F, which the frame total admits.
func handoffAtTheHeaviestFrame(t *testing.T, state []byte, pad bool) (room *crdt.Doc, handoff []byte) {
	t.Helper()
	const paste, typed = 256 << 10, "an honest edit "
	room = newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatal(err)
	}
	if !pad {
		return room, state
	}
	margin := room.GetMap(marksMapName)
	room.Transact(func(txn *crdt.Transaction) { margin.Set(txn, marginTextKey, crdt.NewTextPrelim()) })
	nested, _ := margin.Get(marginTextKey)
	text, ok := nested.(*crdt.YText)
	if !ok {
		t.Fatalf("the margin holds %T under %q, want a text", nested, marginTextKey)
	}
	left := int(heaviestHandoffBound) - len(crdt.EncodeStateAsUpdateV1(room, nil)) - 16<<10
	if left < 0 {
		t.Fatalf("the state is %d bytes past F less 16 KiB, want it under", -left)
	}
	chunk := strings.Repeat("p", paste)
	room.Transact(func(txn *crdt.Transaction) {
		for at := 0; at < left; at += paste {
			text.Insert(txn, at, chunk[:min(paste, left-at)], nil)
		}
	})
	browser, into := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, typed, nil) })
	return room, crdt.EncodeStateAsUpdateV1(browser, nil)
}

// BenchmarkStructTotalConfirmation measures what the hook's confirmation of a room's struct total
// costs the browser whose update it weighs (structTotal.confirm): an encode of the room's whole
// state and a walk of it (countRoomStructs), on a mebibyte of a room's shape. It reports the
// encoded state's bytes beside the time and the allocation.
func BenchmarkStructTotalConfirmation(b *testing.B) {
	live := residentRoomDocument(b, 1<<20)
	b.ReportAllocs()
	for b.Loop() {
		if countRoomStructs(live) == 0 {
			b.Fatal("the room counted no structs")
		}
	}
	b.ReportMetric(float64(len(crdt.EncodeStateAsUpdateV1(live, nil))), "state-bytes")
}
