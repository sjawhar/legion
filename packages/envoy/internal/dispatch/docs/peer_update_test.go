package docs

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The hook is driven here at its own level, as ygo drives it: the room's document, the update's
// bytes, and an apply that applies the update to that document exactly as the room's own does
// (sjawhar/ygo v1.51.3-sami.2, provider/websocket/peer_update.go:75-84). What a real browser meets
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
		// refused is the code of the close the hook answers, zero for none; applied is whether the
		// room took the update; failed is whether the hook returned the apply's own error instead;
		// changes is whether the room's document is any different for it, which a whole-state
		// message of what the room already holds is not.
		refused                  int
		applied, failed, changes bool
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
			name:       "an update that does not decode is answered without a close",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update:     func(*testing.T, *crdt.Doc) []byte { return []byte{0x01, 0xff, 0xff, 0xff} },
			failed:     true,
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
			applied := false
			err := service.weighPeerUpdate(
				websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update},
				func() error {
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
				reason := contracts.DocumentTooLargeCloseReason
				if row.refused == contracts.DocumentBusyCloseCode {
					reason = contracts.DocumentBusyCloseReason
				}
				if refusal.Code != row.refused || refusal.Reason != reason {
					t.Fatalf("the hook closed with %d %q, want %d %q", refusal.Code, refusal.Reason, row.refused, reason)
				}
			case row.failed:
				if err == nil || errors.As(err, &refusal) {
					t.Fatalf("the hook answered %v, want the apply's own error and no close", err)
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

// peerUpdateRoom is a room holding markdown, with the measurement its load publishes (keepReplica),
// and nothing else of the service: the hook reads the room's document and that measurement alone.
// leastBytes is what the rendering must measure for its caller's state, so a row written for a
// document at a bound fails rather than passing under one. The replica is returned because the
// service lists it weakly, and its caller is what holds it.
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
	replica := service.keepReplica(room, &rendering)
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
	browser, from := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) { from.Delete(txn, 0, characters) })
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
// interface is unexported (sjawhar/ygo v1.51.3-sami.2, crdt/yxml.go:16), so the walk takes each
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
// bounds instead (maxUpdateItems, refuseUndecodable).
func TestTheHeaviestWholeStateFrameFitsTheMessageFloor(t *testing.T) {
	frame := len(crdt.EncodeStateAsUpdateV1(heaviestDocument(t), nil))
	t.Logf("the heaviest whole-state frame is %d bytes; the message floor is %d", frame, maxMessageBytes)
	if frame*4 > maxMessageBytes {
		t.Fatalf("the heaviest whole-state frame is %d bytes, past a quarter of the %d-byte message floor: "+
			"measure it again and set the floor at four times it, or take a frame past 16 MiB back to the design",
			frame, maxMessageBytes)
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

// BenchmarkPeerUpdateInsertMeasures weighs the two ways to ask what an update carries that the room
// does not already hold, on the update a keystroke sends against a document at pmdoc.MaxDocumentBytes,
// which is where the hook decodes at all: one decode and arithmetic over the clock runs the update
// inserts (insertedBeyond, through crdt.ContentIDsFromUpdateV1), against re-encoding the update
// against the room's state vector (crdt.DiffUpdateV1). The hook takes the first, which is both
// cheaper and zero for an update that only deletes, where the second's length carries the update's
// delete set too.
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
	// The state vector is taken once, as the hook takes it once: what is weighed here is the two
	// ways of measuring the update against it.
	held := room.StateVector()
	b.Run("ContentIDsFromUpdateV1", func(b *testing.B) {
		for b.Loop() {
			if _, err := insertedBeyond(update, held); err != nil {
				b.Fatal(err)
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
