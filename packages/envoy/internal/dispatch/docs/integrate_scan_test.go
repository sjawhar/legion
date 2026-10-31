package docs

import (
	"bytes"
	"errors"
	"maps"
	"runtime"
	"slices"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// scanFixtureSeed is the stored state of a document of a title and paragraphs of ordinary prose,
// each about a thousand characters, at least size bytes of them, as the server writes it.
func scanFixtureSeed(t *testing.T, size int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# Title\n\n" + proseAtLeast(size))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// scanReceived is the origin a scan fixture's browser applies another browser's update with, so its
// own update observer does not take that update for one of its own.
type scanReceived struct{}

// scanBrowser is one person's browser in a scan fixture: a copy of the document under a client of
// its own whose every transaction is one update it sends, as the editor sends one for each
// ProseMirror transaction, and a cursor in one paragraph's text.
type scanBrowser struct {
	t    *testing.T
	doc  *crdt.Doc
	text *crdt.YXmlText
	// cursor is anchored to the character right of it, as an editor keeps a cursor across what
	// other people insert.
	cursor crdt.RelativePosition
	// sent is every update the browser's own transactions made, in the order it made them.
	sent [][]byte
}

func newScanBrowser(t *testing.T, state []byte, client crdt.ClientID) *scanBrowser {
	t.Helper()
	b := &scanBrowser{t: t, doc: newDocumentCopy(crdt.WithClientID(client))}
	if err := crdt.ApplyUpdateV1(b.doc, state, nil); err != nil {
		t.Fatal(err)
	}
	b.doc.OnUpdate(func(update []byte, origin any) {
		if _, received := origin.(scanReceived); !received {
			b.sent = append(b.sent, bytes.Clone(update))
		}
	})
	return b
}

// receive applies another browser's update, as the room relays it.
func (b *scanBrowser) receive(update []byte) {
	b.t.Helper()
	if err := crdt.ApplyUpdateV1(b.doc, update, scanReceived{}); err != nil {
		b.t.Fatal(err)
	}
}

// at puts the cursor before character index of the text of the document's paragraph-th paragraph.
func (b *scanBrowser) at(paragraph, index int) {
	b.text = paragraphTexts(b.doc)[paragraph]
	b.cursor = crdt.CreateRelativePositionFromIndex(b.text, index, 0)
}

func (b *scanBrowser) index() int {
	b.t.Helper()
	at, ok := crdt.ToAbsolutePosition(b.doc, b.cursor)
	if !ok {
		b.t.Fatal("the cursor's anchor is gone")
	}
	return at.Index
}

// key types s at the cursor in one transaction.
func (b *scanBrowser) key(s string) {
	at := b.index()
	b.doc.Transact(func(txn *crdt.Transaction) { b.text.Insert(txn, at, s, nil) })
}

// backspace deletes the character left of the cursor in one transaction.
func (b *scanBrowser) backspace() {
	at := b.index()
	b.doc.Transact(func(txn *crdt.Transaction) { b.text.Delete(txn, at-1, 1) })
}

// The way a person composes text in scan fixtures: a typo, corrected at once with a backspace,
// every scanTypoEvery characters, and every scanReviseEvery characters a return scanReviseBack
// characters into what was just written, to add a word there, before carrying on where the person
// was. Each correction leaves a deleted item, and each revision splits an item, so the text a
// person composes at one place is many items rather than the one that unbroken typing merges into.
const (
	scanTypoEvery   = 9
	scanReviseEvery = 60
	scanReviseBack  = 12
)

// composing is the keystrokes of a person composing n characters at their cursor, each a function
// that makes one transaction, or none for a move of the cursor.
func composing(n int) []func(*scanBrowser) {
	var keystrokes []func(*scanBrowser)
	var resume crdt.RelativePosition
	for i := range n {
		if i%scanTypoEvery == scanTypoEvery-1 {
			keystrokes = append(keystrokes, func(b *scanBrowser) { b.key("q") }, (*scanBrowser).backspace)
		}
		letter := string(rune('a' + i%26))
		keystrokes = append(keystrokes, func(b *scanBrowser) { b.key(letter) })
		if i%scanReviseEvery == scanReviseEvery-1 && i >= scanReviseBack {
			keystrokes = append(keystrokes, func(b *scanBrowser) {
				resume = b.cursor
				b.cursor = crdt.CreateRelativePositionFromIndex(b.text, b.index()-scanReviseBack, 0)
			})
			for _, letter := range "word " {
				keystrokes = append(keystrokes, func(b *scanBrowser) { b.key(string(letter)) })
			}
			keystrokes = append(keystrokes, func(b *scanBrowser) { b.cursor = resume })
		}
	}
	return keystrokes
}

// compose runs keystrokes on b alone.
func (b *scanBrowser) compose(keystrokes []func(*scanBrowser)) {
	for _, keystroke := range keystrokes {
		keystroke(b)
	}
}

// scanGap is the character of a paragraph's text where the people of a scan fixture type: the
// middle of a paragraph of about a thousand characters.
const scanGap = 500

// The client of the person whose update is measured is the higher one in each fixture: a client
// that loses the tie-break against an item at the same place stops its scan there
// (sjawhar/ygo v1.54.1-sami.1, crdt/item.go:170-179), and one that wins scans on past every item at
// that place, which is the order whose count the room's scan total has to hold.
const (
	scanFirstClient  crdt.ClientID = 1_001
	scanSecondClient crdt.ClientID = 1_002
)

// tangledTypist is the client of the browser whose keystrokes tangledRoom's paragraph holds.
const tangledTypist crdt.ClientID = 5

// tangledRoom is chargedPeerRoom's room, its document bounded by its scan budget (newRoomScans),
// after a browser of client tangledTypist typed typed characters onto the end of its paragraph, one
// keystroke an update through the hook. Each keystroke stays an item of its own in the room, since
// ygo merges a run of typed characters only within one transaction (sjawhar/ygo v1.54.1-sami.1,
// crdt/transaction.go:242-247).
func tangledRoom(t *testing.T, typed int) (*Service, *crdt.Doc, *renderedReplica, *roomCharge) {
	t.Helper()
	service, room, replica, charge := chargedPeerRoom(t)
	typeOnto(t, service, room, tangledTypist, typed)
	return service, room, replica, charge
}

// typeOnto has a browser of client type typed characters onto the end of room's first paragraph,
// one keystroke an update through the hook, each admitted.
func typeOnto(t *testing.T, service *Service, room *crdt.Doc, client crdt.ClientID, typed int) {
	t.Helper()
	browser := newDocumentCopy(crdt.WithClientID(client))
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatal(err)
	}
	var sent []byte
	browser.OnUpdate(func(update []byte, _ any) { sent = update })
	text := paragraphTexts(browser)[0]
	for range typed {
		browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), "k", nil) })
		if err, called := weighInto(service, room, sent); err != nil || !called {
			t.Fatalf("the hook answered a keystroke %v (applied %t), want it admitted", err, called)
		}
	}
}

// browserScanCeiling is the per-frame ceiling of frame, a browser's update to room, as the hook
// sets it from the update's own share of its apply-heap estimate (scanCeiling).
func browserScanCeiling(t *testing.T, room *crdt.Doc, frame []byte) int {
	t.Helper()
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(frame, fromPeer, &weighed); err != nil {
		t.Fatalf("the walk refused the frame: %v", err)
	}
	return scanCeiling(weighed.ownApplyHeap(len(frame)))
}

// tangledFrame is client 9's scanningFrame in tangledRoom of the fewest items whose scans together
// pass the frame's per-frame ceiling (browserScanCeiling): the first item scans the typed-1
// characters after the typist's first, and each later one those and one item of its own frame,
// where its scan stops, so n items scan n*typed-1. Nothing else refuses it: it is a few hundred
// bytes of 1-character strings into a paragraph, which the hook does not weigh.
func tangledFrame(t *testing.T, room *crdt.Doc, typed int) []byte {
	t.Helper()
	for items := 1; ; items++ {
		frame := scanningFrame(9, crdt.ID{Client: tangledTypist}, items)
		if items*typed-1 > browserScanCeiling(t, room, frame) {
			return frame
		}
	}
}

// scanningFrame is client's frame of items 1-character strings, each naming origin as its left
// origin and no right origin, so the scan of each passes every item after origin in its list
// (sjawhar/ygo v1.54.1-sami.1, crdt/item.go:152-200) up to one of client's own.
func scanningFrame(client crdt.ClientID, origin crdt.ID, items int) []byte {
	f := newV1Frame(1).group(items, uint64(client), 0)
	for range items {
		f.raw(byte(structString) | infoHasOrigin).uint(uint64(origin.Client)).uint(origin.Clock).str("z")
	}
	return f.noDeletes()
}

// roomPicture is what a refused update must leave a room's bookkeeping as: its document's encoded
// state, the budget's used bytes, the room's structure part, its struct total, and the delete
// ranges the room is known to hold deleted.
type roomPicture struct {
	state     []byte
	used      int64
	structure int64
	total     uint64
	stale     bool
	deletes   map[uint64][]clockRun
}

func pictureRoom(service *Service, room *crdt.Doc, replica *renderedReplica, charge *roomCharge) roomPicture {
	picture := roomPicture{state: crdt.EncodeStateAsUpdateV1(room, nil), used: budgetUsed(service)}
	picture.structure, _, _, _ = chargeParts(charge)
	picture.total, picture.stale = replica.structs.running()
	charge.deleted.mu.RLock()
	picture.deletes = make(map[uint64][]clockRun, len(charge.deleted.clients))
	for client, runs := range charge.deleted.clients {
		picture.deletes[client] = slices.Clone(runs)
	}
	charge.deleted.mu.RUnlock()
	return picture
}

func requireRoomAsItWas(t *testing.T, before, after roomPicture) {
	t.Helper()
	if !bytes.Equal(after.state, before.state) {
		t.Error("the room's document changed")
	}
	if after.used != before.used || after.structure != before.structure {
		t.Errorf("the budget holds %d bytes and the room's structure part %d, want %d and %d as they were",
			after.used, after.structure, before.used, before.structure)
	}
	if after.total != before.total || after.stale != before.stale {
		t.Errorf("the room's struct total is %d (stale %t), want %d (stale %t) as it was", after.total, after.stale, before.total, before.stale)
	}
	if !maps.EqualFunc(after.deletes, before.deletes, slices.Equal) {
		t.Errorf("the room's recorded delete ranges are %v, want %v as they were", after.deletes, before.deletes)
	}
}

// An update the room's bounded document refuses leaves no trace in the room's bookkeeping but what
// its scans cost, as it leaves none in the document (sjawhar/ygo v1.54.1-sami.1,
// crdt/apply_rollback.go:9-21): a frame whose scans pass its allowance, which every other rule of
// the hook admits and an unbounded copy of the room applies, is refused with 4413 DOC_TOO_TANGLED,
// and the room's document, the budget's used bytes, the room's structure part, its struct total and
// its recorded delete ranges are as they were, while its scan total holds the items the refused
// apply's scans passed, exactly its allowance. A delete admitted under pressure is charged before
// its apply (refusedUnderPressure), and one whose apply fails takes that charge back
// (roomCharge.gaveBack) and is answered with the apply's own error, its scan total as it was.
func TestAnIntegrateScanRefusalLeavesTheRoomsChargeAndTotalAsTheyWere(t *testing.T) {
	const typed = 2_000
	service, room, replica, charge := tangledRoom(t, typed)
	frame := tangledFrame(t, room, typed)
	weighed := peerWeighing{held: room.StateVector(), known: charge.deletes()}
	if _, err := walkUpdate(frame, fromPeer, &weighed); err != nil {
		t.Fatalf("the walk refused the frame: %v", err)
	}
	if weighed.inserted >= maxPeerInsertAtTheBound || weighed.ownApplyHeap(len(frame)) > unweighedApplyHeapBytes {
		t.Fatalf("the frame inserts %d and its apply's own share is %d bytes, want a small frame no other rule weighs", weighed.inserted, weighed.ownApplyHeap(len(frame)))
	}
	copied, err := snapshotDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdateV1(copied, frame, nil); err != nil {
		t.Fatalf("an unbounded copy of the room refused the frame: %v", err)
	}

	before := pictureRoom(service, room, replica, charge)
	scannedBefore, _ := replica.scans.figures()
	err, called := weighInto(service, room, frame)
	var refusal *websocket.PeerClose
	if !called || !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentTooTangledCloseReason {
		t.Fatalf("the hook answered %v (applied %t), want the apply's refusal closed %d %q", err, called,
			contracts.DocumentTooLargeCloseCode, contracts.DocumentTooTangledCloseReason)
	}
	requireRoomAsItWas(t, before, pictureRoom(service, room, replica, charge))
	scannedAfter, _ := replica.scans.figures()
	if allowance := int64(browserScanCeiling(t, room, frame)); scannedAfter-scannedBefore != allowance {
		t.Errorf("the refusal added %d items to the room's scan total, want the %d of its allowance", scannedAfter-scannedBefore, allowance)
	}

	used := budgetUsed(service)
	service.budget.mu.Lock()
	service.budget.limit = used - 1
	service.budget.mu.Unlock()
	service.budget.full.Store(true)
	deletion := peerDeleting(t, room, 3)
	before = pictureRoom(service, room, replica, charge)
	scannedBefore, _ = replica.scans.figures()
	refused := errors.New("the room's document refused the update")
	var chargedBeforeApply int64
	err = service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: deletion}, func() error {
		chargedBeforeApply, _, _, _ = chargeParts(charge)
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("the hook answered %v, want the apply's own error", err)
	}
	if chargedBeforeApply <= before.structure {
		t.Fatalf("the room's structure part was %d bytes during the apply, want the deletion charged past %d before it", chargedBeforeApply, before.structure)
	}
	requireRoomAsItWas(t, before, pictureRoom(service, room, replica, charge))
	if scannedAfter, _ = replica.scans.figures(); scannedAfter != scannedBefore {
		t.Errorf("an apply that scanned nothing left the room's scan total at %d, want the %d it was", scannedAfter, scannedBefore)
	}
	runtime.KeepAlive(replica)
}

// A pending cap refuses a browser's update before the apply begins: ygo reads the update against
// the cap before it starts the journal or asks the document's scan budget (sjawhar/ygo
// v1.54.1-sami.1, crdt/update.go:743-760, crdt/apply_rollback.go:140-157), so the refusal reports
// no scans at all, and the room's scan total neither takes any for it nor has any to give back.
// The hook returns the error as it is, so ygo answers that browser SyncStatus(0) and leaves it
// connected (weighPeerUpdate), and the room's charge, its structure part, its struct total and its
// recorded delete ranges are as they were: nothing of the update was booked, so nothing is
// refunded either.
//
// Only ygo's apply can make this refusal: the struct total counts each parked delete range
// splitsAStructCanCause times within maxUpdateItems and refuses a frame that would pass the room's
// own cap of maxUpdateItems first (TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad), so the
// room here is given a cap as small as the frames this test sends.
func TestAPendingCapRefusalCostsTheRoomNeitherChargeNorScans(t *testing.T) {
	const parked = 4
	// What ygo answers, which the hook's accounting rests on.
	asked := 0
	bounded := newDocumentCopy(crdt.WithClientID(1))
	bounded.SetMaxPendingItems(parked)
	bounded.SetIntegrateScanBudget(func(any) int {
		asked++
		return perFrameScanCeiling
	})
	if err := crdt.ApplyUpdateV1(bounded, deleteRangesFrom(11, parked), nil); err != nil {
		t.Fatalf("the document refused %d parked delete ranges, its whole cap: %v", parked, err)
	}
	if asked != 1 {
		t.Fatalf("the admitted apply asked the budget %d times, want once", asked)
	}
	err := crdt.ApplyUpdateV1(bounded, deleteRangesFrom(12, 1), nil)
	var refused *crdt.RefusedUpdateError
	if !errors.As(err, &refused) || !errors.Is(err, crdt.ErrInvalidUpdate) || errors.Is(err, crdt.ErrIntegrateScanBudget) {
		t.Fatalf("the cap refused one range past it with %v, want a RefusedUpdateError wrapping ErrInvalidUpdate", err)
	}
	if refused.Scanned != 0 || asked != 1 {
		t.Fatalf("the refusal reports %d items scanned, and the budget was asked %d times in all, want none and the admitted apply's one",
			refused.Scanned, asked)
	}

	// And the same refusal through the hook, in a room the budget charges.
	service, room, replica, charge := chargedPeerRoom(t)
	room.SetMaxPendingItems(parked)
	if err, called := weighInto(service, room, deleteRangesFrom(11, parked)); err != nil || !called {
		t.Fatalf("the hook answered a frame parking the room's whole cap %v (applied %t), want it admitted", err, called)
	}
	before := pictureRoom(service, room, replica, charge)
	scannedBefore, leftBefore := replica.scans.figures()
	err, called := weighInto(service, room, deleteRangesFrom(12, 1))
	var closed *websocket.PeerClose
	if !called || errors.As(err, &closed) || !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("the hook answered %v (applied %t), want the apply's own error, which closes nothing", err, called)
	}
	requireRoomAsItWas(t, before, pictureRoom(service, room, replica, charge))
	if scannedAfter, leftAfter := replica.scans.figures(); scannedAfter != scannedBefore || leftAfter != leftBefore {
		t.Errorf("the refusal left the room's scan total at %d with %d left, want the %d and %d it was",
			scannedAfter, leftAfter, scannedBefore, leftBefore)
	}
	runtime.KeepAlive(replica)
}
