package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// scanFixture is a room and the honest remote updates it takes, in the order it takes them: seed is
// the room's state as its load decodes it, unbounded, and updates are what browsers and the server
// send it after.
type scanFixture struct {
	name    string
	seed    []byte
	updates [][]byte
}

// replay is the room's state after it takes every update of f with its scans bounded at bound, or
// unbounded at 0, and the first error an apply returned.
func (f scanFixture) replay(t *testing.T, bound int) ([]byte, error) {
	t.Helper()
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, f.seed, nil); err != nil {
		t.Fatal(err)
	}
	room.SetMaxIntegrateScan(bound)
	for _, update := range f.updates {
		if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
			return nil, err
		}
	}
	return crdt.EncodeStateAsUpdateV1(room, nil), nil
}

// mostScanned is the most items any one update of f scans: the least bound under which a room
// takes every update and ends where an unbounded room does, found by bisection, each probe a
// fresh copy of the room. The end state is compared as well as the errors because a drain of
// parked items that passes the bound is taken back and dropped while its update stands
// (crdt/apply_rollback.go:20-26). A count of 0 and of 1 both read as 1.
func (f scanFixture) mostScanned(t *testing.T) int {
	t.Helper()
	want, err := f.replay(t, 0)
	if err != nil {
		t.Fatalf("an unbounded room refused the fixture: %v", err)
	}
	takes := func(bound int) bool {
		got, err := f.replay(t, bound)
		return err == nil && bytes.Equal(got, want)
	}
	low, high := 1, maxUpdateItems
	if !takes(high) {
		t.Fatalf("a room bounded at %d does not take the fixture as an unbounded one does", high)
	}
	for low < high {
		mid := low + (high-low)/2
		if takes(mid) {
			high = mid
		} else {
			low = mid + 1
		}
	}
	return low
}

// scanFixtureSeed is the stored state of a document of a title and twenty-odd paragraphs of
// ordinary prose, each about a thousand characters, as the server writes it.
func scanFixtureSeed(t *testing.T) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# Title\n\n" + proseAtLeast(24<<10))
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
// (crdt/item.go:161-170), and one that wins scans on past every item at that place, which is the
// order whose count the bound has to hold.
const (
	scanFirstClient  crdt.ClientID = 1_001
	scanSecondClient crdt.ClientID = 1_002
)

// twoEditorsTypingInOneParagraph is two people typing into the same place of one paragraph at
// once, each composing 600 characters, each keystroke one update the room takes as it is made, and
// each seeing the other's keystrokes only 15 updates late, about two seconds of typing behind.
func twoEditorsTypingInOneParagraph(t *testing.T) scanFixture {
	const (
		characters = 600
		lag        = 15
	)
	seed := scanFixtureSeed(t)
	first, second := newScanBrowser(t, seed, scanFirstClient), newScanBrowser(t, seed, scanSecondClient)
	first.at(0, scanGap)
	second.at(0, scanGap)
	firstKeys, secondKeys := composing(characters), composing(characters)
	fixture := scanFixture{name: "two editors typing in one paragraph", seed: seed}
	roomHas := [2]int{}
	received := [2]int{}
	browsers := [2]*scanBrowser{first, second}
	for i := range max(len(firstKeys), len(secondKeys)) {
		if i < len(firstKeys) {
			firstKeys[i](first)
		}
		if i < len(secondKeys) {
			secondKeys[i](second)
		}
		for j, browser := range browsers {
			fixture.updates = append(fixture.updates, browser.sent[roomHas[j]:]...)
			roomHas[j] = len(browser.sent)
		}
		for j, browser := range browsers {
			other := browsers[1-j]
			for ; received[j] < len(other.sent)-lag; received[j]++ {
				browser.receive(other.sent[received[j]])
			}
		}
	}
	return fixture
}

// anOfflineBrowsersHandoff is a browser that went offline and composed 100 characters into each
// of twenty paragraphs while another person, online, composed 300 characters at the same place of
// each of them, and then reconnected: its whole state is one update, whose scans at each of the
// twenty places pass what the other person wrote there.
func anOfflineBrowsersHandoff(t *testing.T) scanFixture {
	const (
		paragraphs = 20
		online     = 300
		offline    = 100
	)
	seed := scanFixtureSeed(t)
	present, away := newScanBrowser(t, seed, scanFirstClient), newScanBrowser(t, seed, scanSecondClient)
	for paragraph := range paragraphs {
		present.at(paragraph, scanGap)
		present.compose(composing(online))
		away.at(paragraph, scanGap)
		away.compose(composing(offline))
	}
	return scanFixture{
		name:    "an offline browser's whole-state handoff",
		seed:    seed,
		updates: append(present.sent, crdt.EncodeStateAsUpdateV1(away.doc, nil)),
	}
}

// theServersPublishRacingABrowser is an API write that inserts a sentence in the middle of a
// paragraph, made on a fork of the room as the server's writes are (pmdoc.Update), while a person
// composes 400 characters at the same place; the write's update then reaches the room, which
// applies it as a remote update (publishLiveUpdate), its scan passing everything the person wrote
// there. A write that replaces the text there instead scans nothing: pmdoc.Update deletes before it
// inserts (simpleDiff), and ygo places the insert after the deleted run, between two items no
// browser typed between (measured: one item).
func theServersPublishRacingABrowser(t *testing.T) scanFixture {
	const typed = 400
	seed := scanFixtureSeed(t)
	fork := newDocumentCopy(crdt.WithClientID(scanSecondClient))
	if err := crdt.ApplyUpdateV1(fork, seed, nil); err != nil {
		t.Fatal(err)
	}
	forked := fork.StateVector()
	tree, err := treeOf(fork)
	if err != nil {
		t.Fatal(err)
	}
	paragraph := tree.Children[1]
	if paragraph.Type != "paragraph" || len(paragraph.Children) != 1 {
		t.Fatalf("the document's first paragraph is %+v, want one run of text", paragraph)
	}
	text := paragraph.Children[0].Text
	paragraph.Children[0].Text = text[:scanGap] + "The agent added this sentence of its own. " + text[scanGap:]
	fragment := fork.GetXmlFragment(fragmentName)
	if err := fork.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	browser := newScanBrowser(t, seed, scanFirstClient)
	browser.at(0, scanGap)
	browser.compose(composing(typed))
	return scanFixture{
		name:    "the server's publish racing a browser in the same paragraph",
		seed:    seed,
		updates: append(browser.sent, crdt.EncodeStateAsUpdateV1(fork, forked)),
	}
}

// anOfflinePasteIntoAParagraphEditedMeanwhile is a browser that went offline, pasted 32 KiB of
// text into a paragraph and composed 200 characters after it, while another person, online,
// composed 2,000 characters at the same place, and then reconnected with its whole state.
func anOfflinePasteIntoAParagraphEditedMeanwhile(t *testing.T) scanFixture {
	const (
		online  = 2_000
		offline = 200
	)
	seed := scanFixtureSeed(t)
	present, away := newScanBrowser(t, seed, scanFirstClient), newScanBrowser(t, seed, scanSecondClient)
	present.at(0, scanGap)
	present.compose(composing(online))
	away.at(0, scanGap)
	away.key(strings.Repeat("pasted words ", (32<<10)/13))
	away.compose(composing(offline))
	return scanFixture{
		name:    "an offline browser's large paste into a paragraph edited meanwhile",
		seed:    seed,
		updates: append(present.sent, crdt.EncodeStateAsUpdateV1(away.doc, nil)),
	}
}

// The integrate-scan bound is four times the most items any honest remote update scans: each
// fixture is a room and the updates people and the server send it, and the most any one of them
// scans, times four, has to stay within the bound, or the bound would refuse an honest edit with
// too little room to spare.
func TestTheIntegrateScanBoundHoldsEveryHonestUpdateFourTimesOver(t *testing.T) {
	for _, build := range []func(*testing.T) scanFixture{
		twoEditorsTypingInOneParagraph,
		anOfflineBrowsersHandoff,
		theServersPublishRacingABrowser,
		anOfflinePasteIntoAParagraphEditedMeanwhile,
	} {
		fixture := build(t)
		t.Run(fixture.name, func(t *testing.T) {
			scanned := fixture.mostScanned(t)
			t.Logf("%d updates; the most items one of them scans is %d, four times it %d, against the bound of %d",
				len(fixture.updates), scanned, 4*scanned, integrateScanBound)
			if 4*scanned > integrateScanBound {
				t.Errorf("an honest update scans %d items, and four times it passes the bound of %d", scanned, integrateScanBound)
			}
		})
	}
}

// tangledTypist is the client of the browser whose keystrokes tangledRoom's paragraph holds.
const tangledTypist crdt.ClientID = 5

// tangledRoom is chargedPeerRoom's room, its document bounded (boundRoomDocument), after a browser
// of client tangledTypist typed typed characters onto the end of its paragraph, one keystroke an
// update through the hook. Each keystroke stays an item of its own in the room, since ygo merges a
// run of typed characters only within one transaction (crdt/transaction.go:172).
func tangledRoom(t *testing.T, typed int) (*Service, *crdt.Doc, *renderedReplica, *roomCharge) {
	t.Helper()
	service, room, replica, charge := chargedPeerRoom(t)
	browser := newDocumentCopy(crdt.WithClientID(tangledTypist))
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
	return service, room, replica, charge
}

// tangledFrame is client 9's scanningFrame of enough items that their scans together pass
// integrateScanBound in tangledRoom by about one item's scan. Nothing else refuses it: it is a few
// hundred bytes of 1-character strings into a paragraph.
func tangledFrame(typed int) []byte {
	return scanningFrame(9, integrateScanBound/(typed-1)+1)
}

// scanningFrame is client's frame of items 1-character strings, each naming the first character
// tangledTypist typed as its left origin and no right origin, so the scan of each passes every item
// after that character in its paragraph (crdt/item.go:150-192) up to one of client's own.
func scanningFrame(client crdt.ClientID, items int) []byte {
	f := newV1Frame(1).group(items, uint64(client), 0)
	for range items {
		f.raw(byte(structString) | infoHasOrigin).uint(uint64(tangledTypist)).uint(0).str("z")
	}
	return f.noDeletes()
}

// tangledDocument is a room's bounded document holding a title and a paragraph, onto whose end
// tangledTypist typed typed characters, one remote transaction each, as tangledRoom's are.
func tangledDocument(tb testing.TB, typed int) *crdt.Doc {
	tb.Helper()
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		tb.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		tb.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
		tb.Fatal(err)
	}
	boundRoomDocument(room)
	browser := newDocumentCopy(crdt.WithClientID(tangledTypist))
	if err := crdt.ApplyUpdateV1(browser, seed, nil); err != nil {
		tb.Fatal(err)
	}
	text := paragraphTexts(browser)[0]
	browser.OnUpdate(func(update []byte, _ any) {
		if err := crdt.ApplyUpdateV1(room, update, nil); err != nil {
			tb.Fatal(err)
		}
	})
	for range typed {
		browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), "k", nil) })
	}
	return room
}

// A room's bounded document refuses a frame whose scans pass integrateScanBound, but takes one
// just under it, and the items such a frame leaves name the origins they named: every copy of the
// room decodes its whole state into an unbounded document (snapshotDocument, a write's fork, the
// next load), which integrates each of those items again and re-pays its scan. This feeds a room
// typed into as tangledRoom is frames number of frames, each of clients of its own, ascending, so
// each frame's items scan past every earlier frame's too, each frame the most items the room's
// bound takes, and times a copy of the room against an ordinary copy of a mebibyte of a room's
// shape (residentRoomDocument).
func BenchmarkACopyOfARoomFedFramesJustUnderTheScanBound(b *testing.B) {
	const typed = 2_000
	for _, frames := range []int{0, 1, 4, 16, 64} {
		b.Run(fmt.Sprintf("frames-%d", frames), func(b *testing.B) {
			room := tangledDocument(b, typed)
			// after is how many items follow the typist's first character in the paragraph.
			after, scanned := typed-1, 0
			for frame := range frames {
				client := crdt.ClientID(100 + frame)
				// Each item scans the after items and, past the first, one of its own frame's.
				items := (integrateScanBound+1)/(after+1) + 1
				for ; ; items-- {
					err := crdt.ApplyUpdateV1(room, scanningFrame(client, items), nil)
					if err == nil {
						break
					}
					if !errors.Is(err, crdt.ErrIntegrateScanBudget) {
						b.Fatal(err)
					}
				}
				scanned += items*(after+1) - 1
				after += items
			}
			state := crdt.EncodeStateAsUpdateV1(room, nil)
			for b.Loop() {
				if err := crdt.ApplyUpdateV1(newDocumentCopy(), state, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(state)), "state-bytes")
			b.ReportMetric(float64(scanned), "scanned-items")
		})
	}
	b.Run("an ordinary copy of a mebibyte", func(b *testing.B) {
		state := crdt.EncodeStateAsUpdateV1(residentRoomDocument(b, 1<<20), nil)
		for b.Loop() {
			if err := crdt.ApplyUpdateV1(newDocumentCopy(), state, nil); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(len(state)), "state-bytes")
	})
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

// An update the room's bounded document refuses leaves no trace in the room's bookkeeping, as it
// leaves none in the document (crdt/apply_rollback.go:9-18): a frame whose scans pass
// integrateScanBound, which every other rule of the hook admits and an unbounded copy of the room
// applies, is refused with 4413 DOC_TOO_TANGLED, and the room's document, the budget's used bytes,
// the room's structure part, its struct total and its recorded delete ranges are as they were. A
// delete admitted under pressure is charged before its apply (refusedUnderPressure), and one whose
// apply fails takes that charge back (roomCharge.gaveBack) and is answered with the apply's own
// error.
func TestAnIntegrateScanRefusalLeavesTheRoomsChargeAndTotalAsTheyWere(t *testing.T) {
	const typed = 2_000
	service, room, replica, charge := tangledRoom(t, typed)
	frame := tangledFrame(typed)
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
	err, called := weighInto(service, room, frame)
	var refusal *websocket.PeerClose
	if !called || !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentTooTangledCloseReason {
		t.Fatalf("the hook answered %v (applied %t), want the apply's refusal closed %d %q", err, called,
			contracts.DocumentTooLargeCloseCode, contracts.DocumentTooTangledCloseReason)
	}
	requireRoomAsItWas(t, before, pictureRoom(service, room, replica, charge))

	used := budgetUsed(service)
	service.budget.mu.Lock()
	service.budget.limit = used - 1
	service.budget.mu.Unlock()
	service.budget.full.Store(true)
	deletion := peerDeleting(t, room, 3)
	before = pictureRoom(service, room, replica, charge)
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
	runtime.KeepAlive(replica)
}

// The server's publish of a write (publishLiveUpdate) reaches a room's bounded document as a remote
// update, and a publish the bound refused would fail the room (publishLiveWrite). An agent's edit
// that inserts words in the middle of a paragraph, published after a person composed 400 characters
// at the same place of the room's paragraph, scans past every item they typed there, and applies:
// the room is not failed, and holds both, the agent's words after the person's, where its scan
// placed them.
func TestTheServersPublishFitsTheIntegrateScanBound(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha inserted words paragraph.",
	})
	if _, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent); err != nil {
		t.Fatalf("snapshot the edit: %v", err)
	}
	if err := edit.ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the document's room is not loaded")
	}
	// The person's client is the least there is, so the agent's write, whose client is random,
	// wins every tie-break at that place and its scan passes everything the person typed.
	before := crdt.EncodeStateAsUpdateV1(live, nil)
	person := newScanBrowser(t, before, 1)
	person.at(0, len("Alpha "))
	person.compose(composing(400))
	for _, update := range person.sent {
		if err := crdt.ApplyUpdateV1(live, update, person); err != nil {
			t.Fatalf("the room refused the person's keystroke: %v", err)
		}
	}
	typedText := person.text.ToString()
	typedText = typedText[len("Alpha ") : len(typedText)-len("paragraph.")]

	// Measured on copies that take the person's keystrokes one transaction each, as the room did:
	// a copy decoded from the room's state in one transaction would merge each run of them into
	// one item (squashRuns), and scan less than the room does.
	published := scanFixture{name: "the publish", seed: before, updates: append(slices.Clone(person.sent), edit.ledger.live[artifactID].updates...)}
	scanned := published.mostScanned(t)
	t.Logf("the publish scans %d items, against the bound of %d", scanned, integrateScanBound)
	if scanned < 400 {
		t.Fatalf("the publish scans %d items, want past every one of the 400 the person typed at its place", scanned)
	}
	edit.ledger.publish()

	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the publish failed the room: %v", err)
	}
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if want := "Alpha " + typedText + "inserted words paragraph."; !strings.Contains(text, want) {
		t.Fatalf("the document reads %q, want it to hold %q: the person's text and then the agent's words", text, want)
	}
}
