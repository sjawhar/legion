package docs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// atCapStates holds the stored documents at the decode cap the tests share, each built once a test
// process: the agent's rewrites (agentRewrittenState), the long edit's keystrokes
// (longEditedAtCapState) and one client's keystrokes (oneClientAtCapState) take minutes, and none
// changes between tests.
var atCapStates struct {
	sync.Mutex
	agentRewritten []byte
	agentRefusal   error
	longEdited     []byte
	oneClient      []byte
}

// longEditedAtCapState is the long-edited fixture (longEditedState) edited on until its struct
// total stands at the decode cap: its first paragraph typed into at its head a character at a
// time, each its own item, as a person's keystrokes at one place are, until the decoded document
// counts maxUpdateItems structs, and those characters then deleted, so it renders the fixture's
// 5 KiB again while its state holds every keystroke. Characters typed one before another never
// merge into one item, in the room or in a decode of its state.
//
// ygo anchors a text insert after the deleted items next to its left neighbour
// (skipDeletedForTextAnchor, sjawhar/ygo v1.51.3-sami.7, crdt/ytext.go:964-988), and the
// fixture's first paragraph opens with deleted characters, so each of the more than half a million
// inserts at its very head walked past all of them: 1,240 s of the 1,426 s the long-edited case of
// TestRebuildingADocumentAtTheDecodeCapLeavesItSmallAndReadingTheSame took on 2026-10-28, against
// 22 s now. One character is typed at the head first, in a transaction of its own, which lands past
// them, and every later character is typed just after it, where no deleted item lies.
func longEditedAtCapState(t *testing.T) []byte {
	t.Helper()
	atCapStates.Lock()
	defer atCapStates.Unlock()
	if atCapStates.longEdited != nil {
		return atCapStates.longEdited
	}
	state := longEditedTypedTo(t, maxUpdateItems)
	if structs, _, _ := structsAndRendering(t, state); structs+smallestReplacementAdds <= maxUpdateItems || structs > maxUpdateItems {
		t.Fatalf("the long-edited document typed on decodes to %d structs, want within %d of the %d cap", structs, smallestReplacementAdds, maxUpdateItems)
	}
	atCapStates.longEdited = state
	return state
}

// longEditedTypedTo is the long-edited fixture typed on as longEditedAtCapState types it until the
// decoded document counts structs structs, and those characters then deleted.
func longEditedTypedTo(t *testing.T, structs uint64) []byte {
	t.Helper()
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, longEditedState(t), nil); err != nil {
		t.Fatal(err)
	}
	text := firstText(doc.GetXmlFragment(fragmentName))
	if text == nil {
		t.Fatal("the long-edited document holds no text")
	}
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	typed := 1
	for counted := countRoomStructs(doc); counted < structs; counted = countRoomStructs(doc) {
		more := int(structs - counted)
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

// storeHistory makes state the whole stored history of artifactID, as one update at the next
// version, the way a rebuild stores its seed (RebuildTx).
func storeHistory(t *testing.T, service *Service, artifactID string, state []byte) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := service.persistence.RebuildTx(ctx, tx, artifactID, state); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// closeResidentRoom closes artifactID's room if this server holds it, as an idle room is closed.
func closeResidentRoom(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if service.srv.GetDoc(artifactID) == nil {
		return
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatal(err)
	}
	waitForNoLiveDocument(t, service, artifactID)
}

// agentRewritesEnv is the environment variable that runs the rebuild of the document an agent
// rewrote whole until the decode cap refused a write, whose 28 writes take about 200 s here
// (agentRewrittenState): past what one test of the package's unit run may take.
const agentRewritesEnv = "DISPATCH_REBUILD_AGENT_REWRITES"

// A document whose history loads but stands at the decode cap can be grown by no one: a write
// that adds a paragraph is refused by the decode cap (refuseUndecodable), and a browser's edit by
// the room's struct total. A rebuild takes it, from its own text: the two documents a room keeps
// at the cap - one an agent rewrote whole through the API until the cap refused a write, and the
// long-edited 5 KiB document typed on until its state reached the cap - each rebuild to no more
// structs than a fresh document of the same text and its list of retired clients, read the same
// text, and take the write that was refused. The long-edited document's case takes about
// 22 s here; the agent's runs with DISPATCH_REBUILD_AGENT_REWRITES=1, about 215 s.
//
// The write is one that adds, not one that trims: a write trimming the long-edited document's
// last line lands on it at the cap, since a delete range splits an item only where one of its
// ends falls inside that item, so a range covering whole items splits none of them and adds no
// struct (sjawhar/ygo v1.51.3-sami.7, crdt/yarray.go:827-844), and the document stays at the cap
// it was already at, as logPastDecodeCap's comment allows for. What no document at the cap takes
// is a write that adds a struct, and that is what a rebuild has to make room for.
func TestRebuildingADocumentAtTheDecodeCapLeavesItSmallAndReadingTheSame(t *testing.T) {
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, row := range []struct {
		name  string
		gate  string
		state func(t *testing.T) []byte
	}{
		{"an agent's whole rewrites until the cap refused one", agentRewritesEnv, func(t *testing.T) []byte {
			state, refusal := agentRewrittenState(t)
			if !errors.Is(refusal, ErrDocumentTooLarge) {
				t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
			}
			return state
		}},
		{"a long-edited document typed on until the cap", "", longEditedAtCapState},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.gate != "" && os.Getenv(row.gate) == "" {
				t.Skip("set " + row.gate + "=1 to run this rebuild")
			}
			state := row.state(t)
			service, artifactID := newTestService(t)
			storeHistory(t, service, artifactID, state)
			ctx := context.Background()
			before, err := service.Text(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			structs, _, _ := structsAndRendering(t, state)
			grown := before + "\nA paragraph the write adds.\n"
			if _, err := joinedReplaceText(service, artifactID, grown, agent); !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "items ygo decodes") {
				t.Fatalf("a write adding a paragraph to the %d-struct document was answered %v, want the decode cap's refusal", structs, err)
			}
			closeResidentRoom(t, service, artifactID)

			report, _, err := rebuildDocument(t, service, artifactID, nil)
			if err != nil {
				t.Fatalf("rebuild a document at %d structs: %v", structs, err)
			}
			loaded, err := service.persistence.Load(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			rebuilt, rendered, _ := structsAndRendering(t, loaded.Update)
			after, err := service.Text(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d structs in a %d-byte state rebuilt to %d in a %d-byte state rendering %d bytes (%s)",
				structs, len(state), rebuilt, len(loaded.Update), rendered, report.ValidationError)
			if after != before {
				t.Fatalf("the rebuilt document reads %d bytes, want the %d it read before", len(after), len(before))
			}
			history, err := decodeHistory(state)
			if err != nil {
				t.Fatal(err)
			}
			fresh, retired := freshStructs(t, loaded.Update), uint64(len(historyClients(history)))
			if rebuilt > fresh+retired {
				t.Fatalf("the rebuilt document holds %d structs, want no more than the %d of a fresh document of its text and its %d retired clients", rebuilt, fresh, retired)
			}
			if _, err := joinedReplaceText(service, artifactID, grown, agent); err != nil {
				t.Fatalf("the growing write after the rebuild: %v", err)
			}
		})
	}
}

// freshStructs is the struct total of a fresh document of the text update's document holds, as
// an upload of it would seed it (encodeDocumentTree).
func freshStructs(t *testing.T, update []byte) uint64 {
	t.Helper()
	doc, err := decodeHistory(update)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := decodeHistory(seed)
	if err != nil {
		t.Fatal(err)
	}
	return countRoomStructs(fresh)
}

// A history that loads and stands further from the decode cap than a write's smallest replacement
// is refused unchanged, with or without supplied markdown: a write trims it.
func TestARebuildOfAHistoryThatLoadsFarFromTheCapIsRefusedWithOrWithoutMarkdown(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Kept\n\nA document that loads.\n")
	closeResidentRoom(t, service, artifactID)
	markdown := "# Replaced\n"
	for _, supplied := range []*string{nil, &markdown} {
		if _, _, err := rebuildDocument(t, service, artifactID, supplied); !errors.Is(err, ErrDocumentLoads) {
			t.Fatalf("rebuild with markdown %t: %v, want ErrDocumentLoads", supplied != nil, err)
		}
	}
}

// rebuiltRoom is a room loaded from a rebuild's seed of markdown that retires the clients of
// retired, bounded and listed as every room is (peerUpdateRoom), with its totals counted at the
// load as a room's are.
func rebuiltRoom(t *testing.T, markdown string, retired map[crdt.ClientID]uint64) (*Service, *crdt.Doc, *renderedReplica) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := rebuiltSeed(tree, retired)
	if err != nil {
		t.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	return service, room, service.keepReplica(room, nil, &rendering, nil)
}

// The retired clients' list is the rebuild's alone: an update that adds an entry to it, one that
// overwrites an entry - a map entry written again, naming the entry before it as its origin - and
// one that deletes an entry are each refused as what the editor never sends, never applied, and
// leave the room as it was, while typing and a whole-state message of what the room holds, the
// list included, are admitted.
func TestAnUpdateThatOverwritesOrDeletesARetiredEntryIsRefused(t *testing.T) {
	retired := map[crdt.ClientID]uint64{123: 5, 456: 7}
	service, room, replica := rebuiltRoom(t, "# Rebuilt\n\nOne paragraph of ordinary prose.\n", retired)
	if entries := replica.structs.retiredEntries(); len(entries) != 1 {
		t.Fatalf("the room found its list's entries under %d clients, want the seed's one", len(entries))
	}
	// changed is the update a browser holding the room sends for change to the list.
	changed := func(change func(txn *crdt.Transaction, list *crdt.YMap)) []byte {
		browser, _ := peerDocument(t, room)
		list := browser.GetMap(retiredMapName)
		held := browser.StateVector()
		browser.Transact(func(txn *crdt.Transaction) { change(txn, list) })
		return crdt.EncodeStateAsUpdateV1(browser, held)
	}
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"a new entry", changed(func(txn *crdt.Transaction, list *crdt.YMap) { list.Set(txn, "789", int64(9)) })},
		{"an overwritten entry", changed(func(txn *crdt.Transaction, list *crdt.YMap) { list.Set(txn, "123", int64(1)) })},
		{"a deleted entry", changed(func(txn *crdt.Transaction, list *crdt.YMap) { list.Delete(txn, "456") })},
	} {
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, row.update)
		if !errors.Is(err, errUpdateNotFromEditor) || called {
			t.Fatalf("%s was answered %v (applied %t), want it refused as not from the editor and never applied", row.name, err, called)
		}
		if !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before) {
			t.Fatalf("the room changed for %s, which the hook refused", row.name)
		}
	}
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"a keystroke", peerTyping(t, room, "y")},
		{"the room's whole state handed back", crdt.EncodeStateAsUpdateV1(room, nil)},
	} {
		if err, called := weighInto(service, room, row.update); err != nil || !called {
			t.Fatalf("%s was answered %v (applied %t), want it admitted", row.name, err, called)
		}
	}
	if listed := retiredClientsOf(room); len(listed) != len(retired) || listed[123] != 5 || listed[456] != 7 {
		t.Fatalf("the room lists %v as retired, want the seed's %v", listed, retired)
	}
}

// A document at the decode cap is one people are typing into when they are refused, so its room
// is resident when someone rebuilds it. The rebuild closes the room rather than refusing it: the
// connected browser's socket ends; when it reconnects still holding the replaced history it is
// closed with the document's own close and DOC_NEEDS_REBUILD; and the editor's fresh read of the
// document, as its 4413 path makes, reads the rebuilt text and saves a new edit. A document far
// from both caps with a resident room is still refused, DOCUMENT_LIVE
// (TestRebuildDocumentRefusesALiveRoom).
func TestARebuildNearACapClosesTheResidentRoomAndItsEditorsReAdmitTheRebuiltDocument(t *testing.T) {
	state := longEditedAtCapState(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, state)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	ctx := context.Background()
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	held := connectPeer(t, server.URL, artifactID, "alice")
	awaitAnswer(t, held, "the document at the cap")
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the document's room is not resident with a browser connected")
	}

	report, _, err := rebuildDocument(t, service, artifactID, nil)
	if err != nil {
		t.Fatalf("rebuild a document at the cap with its room resident: %v", err)
	}
	t.Logf("rebuilt with its room resident: %s", report.ValidationError)
	select {
	case <-held.Ended:
	case <-time.After(time.Minute):
		t.Fatal("the browser's socket stayed open after its room was closed for the rebuild")
	}

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	reconnected := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID, held.Doc)
	select {
	case <-reconnected.Ended:
	case <-time.After(time.Minute):
		t.Fatal("the room never answered the browser holding the replaced history")
	}
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentNeedsRebuildCloseReason {
		t.Fatalf("the room ended the reconnecting browser with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
	}

	readmitted := connectPeer(t, server.URL, artifactID, "alice")
	awaitAnswer(t, readmitted, "the rebuilt document")
	if rendering, err := renderDocument(readmitted.Doc); err != nil || rendering != before {
		t.Fatalf("the re-admitted browser reads a %d-byte rendering (%v), want the document's %d", len(rendering), err, len(before))
	}
	// The text is taken before the transaction: ygo's fragment lookup takes the document's lock,
	// which the transaction holds.
	readmittedText := firstText(readmitted.Doc.GetXmlFragment(fragmentName))
	if _, err := readmitted.Send(func(txn *crdt.Transaction) {
		readmittedText.Insert(txn, 0, "readmitted ", nil)
	}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		text, err := service.Text(ctx, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "readmitted ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the re-admitted browser typed, and the document still reads %d bytes without it", len(text))
		}
	}
}

// tangledHistory is a document's history whose re-scan passes near roomScanTotal: its first
// paragraph followed by chain one-character items, each of a client of its own so none merges
// with its neighbour, and then scanners items each naming the chain's first as its left origin and
// no right origin, so each scans the chain to its end, about chain items each.
func tangledHistory(t *testing.T, chain, scanners int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse("# Tangled\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, seed, nil); err != nil {
		t.Fatal(err)
	}
	var text *crdt.YXmlText
	for _, candidate := range allTexts(doc.GetXmlFragment(fragmentName)) {
		if candidate.Len() > 0 {
			text = candidate
		}
	}
	if text == nil {
		t.Fatal("the document holds no text to chain onto")
	}
	last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
	if last == nil {
		t.Fatal("the paragraph's last character has no item")
	}
	const firstClient, scanClient = 1_000, 4_000_000
	chained := newV1Frame(chain)
	origin := *last
	for i := range chain {
		client := uint64(firstClient + i)
		chained = chained.group(1, client, 0).raw(byte(structString) | infoHasOrigin).uint(uint64(origin.Client)).uint(origin.Clock).str("a")
		origin = crdt.ID{Client: crdt.ClientID(client), Clock: 0}
	}
	scanning := newV1Frame(1).group(scanners, scanClient, 0)
	for range scanners {
		scanning = scanning.raw(byte(structString) | infoHasOrigin).uint(firstClient).uint(0).str("k")
	}
	for _, update := range [][]byte{chained.noDeletes(), scanning.noDeletes()} {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatal(err)
		}
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// A history whose re-scan stands within one honest offline handoff of the room's scan total
// (roomScanTotal, honestHandoffScans) is near the scan total, which leaves every larger merge
// refused, and a rebuild takes it: the rebuilt history re-scans fewer items than one honest
// handoff (historyScans, a count of the re-scan's transaction) and reads the same text. The
// tangle's scanners, about 2,000 scans each, are enough to pass the total less one handoff.
func TestRebuildingADocumentNearTheScanTotalLeavesItWithASmallScanTotalAndReadingTheSame(t *testing.T) {
	state := tangledHistory(t, 2_000, (roomScanTotal-honestHandoffScans)/2_000+2)
	history, err := decodeHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	scans, counted := historyScans(crdt.EncodeStateAsUpdateV1(history, nil))
	if !counted {
		t.Fatal("the tangled history's re-scan could not be counted")
	}
	if scans+honestHandoffScans < roomScanTotal {
		t.Fatalf("the tangled history re-scans %d items, want it within %d of the %d scan total: tangle it further", scans, honestHandoffScans, roomScanTotal)
	}
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, state)
	ctx := context.Background()
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	closeResidentRoom(t, service, artifactID)
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatalf("rebuild a history re-scanning %d items: %v", scans, err)
	}
	loaded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := decodeHistory(loaded.Update)
	if err != nil {
		t.Fatal(err)
	}
	after, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if rescans, _ := historyScans(crdt.EncodeStateAsUpdateV1(rebuilt, nil)); rescans >= honestHandoffScans || after != before {
		t.Fatalf("the rebuilt history re-scans %d items and reads %d bytes, want fewer than %d and the %d it read before", rescans, len(after), honestHandoffScans, len(before))
	}
}

// A browser that held a document before its rebuild still holds the history the rebuild replaced,
// and the edits it made while disconnected. When it reconnects, the rebuilt room holds none of
// that history, so the browser's answer to the room's sync hands all of it back with its unsent
// edit: the room refuses it with the document's own close and the reason that the document needed
// rebuilding (DOC_NEEDS_REBUILD), on which the editor drops what it sent and builds its document
// afresh from the room, and drops its saved rows typed against the old
// history as rows of a rebuilt document. The room's struct total stays small, the document reads
// the rebuilt text with no block twice and without the unsent edit, and a browser of the rebuilt
// history - its whole state, the retired clients' list included, handed back - edits it as before.
func TestABrowserHoldingTheHistoryARebuildReplacedCannotHandItBack(t *testing.T) {
	state := longEditedAtCapState(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, state)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	ctx := context.Background()
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	closeResidentRoom(t, service, artifactID)

	held := connectPeer(t, server.URL, artifactID, "alice")
	awaitAnswer(t, held, "the document at the cap")
	if rendering, err := renderDocument(held.Doc); err != nil || rendering != before {
		t.Fatalf("the browser holds a %d-byte rendering (%v), want the document's %d", len(rendering), err, len(before))
	}
	held.Close()
	unsentInto := firstText(held.Doc.GetXmlFragment(fragmentName))
	docstest.Transact(held.Doc, func(txn *crdt.Transaction) {
		unsentInto.Insert(txn, 0, "unsent ", nil)
	})
	closeResidentRoom(t, service, artifactID)
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	reconnected := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID, held.Doc)
	select {
	case <-reconnected.Ended:
	case <-time.After(time.Minute):
		t.Fatal("the room never answered the browser holding the replaced history")
	}
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentNeedsRebuildCloseReason {
		t.Fatalf("the room ended the browser holding the replaced history with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room closed with the refusal, want it open for the document's other browsers")
	}
	replica := service.liveReplica(live)
	if replica == nil || len(replica.structs.retiredClients()) == 0 {
		t.Fatal("the rebuilt room lists no retired clients")
	}
	if total, _ := replica.structs.running(); total > maxUpdateItems/16 || countRoomStructs(live) > maxUpdateItems/16 {
		t.Fatalf("after the reconnect the room's struct total is %d and its count %d, want both small", total, countRoomStructs(live))
	}
	after, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if after != rebuilt || after != before || strings.Contains(after, "unsent") {
		t.Fatalf("after the reconnect the document reads %d bytes (unsent edit %t), want the rebuilt %d", len(after), strings.Contains(after, "unsent"), len(rebuilt))
	}

	fresh := connectPeer(t, server.URL, artifactID, "bob")
	awaitAnswer(t, fresh, "the rebuilt document")
	if err := fresh.Write(ygsync.EncodeUpdate(crdt.EncodeStateAsUpdateV1(fresh.Doc, nil))); err != nil {
		t.Fatal(err)
	}
	freshInto := firstText(fresh.Doc.GetXmlFragment(fragmentName))
	if _, err := fresh.Send(func(txn *crdt.Transaction) {
		freshInto.Insert(txn, 0, "fresh ", nil)
	}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		text, err := service.Text(ctx, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "fresh ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a browser of the rebuilt history typed, and the document still reads %d bytes without it", len(text))
		}
	}
	select {
	case <-fresh.Ended:
		t.Fatalf("the room ended the rebuilt history's browser with %v", fresh.Closed())
	default:
	}
}

// oneClientWriter is the client that writes the whole of oneClientAtCapState's history, numbered
// above the browser
// TestABrowserHoldingAHistoryARebuildReplacedIsClosedThoughTheWalkRefusesItBeforeReadingAnyOfIt
// connects with, so ygo writes the browser's own group before the history's.
const oneClientWriter crdt.ClientID = 0x0ce1

// oneClientAtCapState is a document's history one client wrote, standing at the decode cap: a
// short document that client seeded, its first text typed into at its head a character at a time,
// each its own item, until the decoded document counts maxUpdateItems structs, and those
// characters then deleted, as longEditedAtCapState types the long-edited fixture. Its encoded state
// is one client group.
func oneClientAtCapState(t *testing.T) []byte {
	t.Helper()
	atCapStates.Lock()
	defer atCapStates.Unlock()
	if atCapStates.oneClient != nil {
		return atCapStates.oneClient
	}
	tree, err := pmdoc.Parse("# One writer\n\nA paragraph its only writer typed into a character at a time.\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy(crdt.WithClientID(oneClientWriter))
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	text := firstText(fragment)
	if text == nil {
		t.Fatal("the seeded document holds no text")
	}
	typed := 0
	for structs := countRoomStructs(doc); structs < maxUpdateItems; structs = countRoomStructs(doc) {
		more := int(maxUpdateItems - structs)
		doc.Transact(func(txn *crdt.Transaction) {
			for range more {
				text.Insert(txn, 0, "x", nil)
			}
		})
		typed += more
	}
	doc.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, typed) })
	state := crdt.EncodeStateAsUpdateV1(doc, nil)
	if structs, _, _ := structsAndRendering(t, state); structs+smallestReplacementAdds <= maxUpdateItems || structs > maxUpdateItems {
		t.Fatalf("the one writer's document typed on by %d characters decodes to %d structs, want within %d of the %d cap", typed, structs, smallestReplacementAdds, maxUpdateItems)
	}
	if clients := doc.StateVector(); len(clients) != 1 {
		t.Fatalf("the one writer's history holds the clocks of %d clients, want its writer's alone", len(clients))
	}
	atCapStates.oneClient = state
	return state
}

// A browser holding the history a rebuild replaced is sent back to a fresh read whatever its
// whole-state message lets the walk read before the walk refuses it. Here the history is one
// client's, at the decode cap, and the browser's own client sorts before it, so the message the
// browser hands back - its own unsent edits, then the history - is refused by the walk at the
// history's header, the group that passes the cap, with no struct of the history read. Left
// connected on that refusal the browser would be answered SyncStatus(0) and keep every later
// edit of it, naming the old history's items, parked: the walk's refusal is a plain error rather
// than a *websocket.PeerClose, and ygo closes a peer whose update the hook refused only for a
// PeerClose (sjawhar/ygo v1.51.3-sami.7, provider/websocket/peer.go:254-260, over the
// Hocuspocus framing service.go:659 sets). The room closes it with the document's own close and
// DOC_NEEDS_REBUILD; it reconnects from a fresh read of the rebuilt document, as the editor's
// 4413 path does, not with its old state; and a new edit from it is applied and stored.
func TestABrowserHoldingAHistoryARebuildReplacedIsClosedThoughTheWalkRefusesItBeforeReadingAnyOfIt(t *testing.T) {
	const browserClient crdt.ClientID = 1
	state := oneClientAtCapState(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, state)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	ctx := context.Background()
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	closeResidentRoom(t, service, artifactID)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	alice := http.Header{"X-Dispatch-User": []string{"alice"}}
	held := docstest.Dial(t, url, alice, artifactID, crdt.New(crdt.WithClientID(browserClient)))
	awaitAnswer(t, held, "the document at the cap")
	if rendering, err := renderDocument(held.Doc); err != nil || rendering != before {
		t.Fatalf("the browser holds a %d-byte rendering (%v), want the document's %d", len(rendering), err, len(before))
	}
	if clients := held.Doc.StateVector(); len(clients) != 1 || clients.Clock(oneClientWriter) == 0 {
		t.Fatalf("the browser holds the clocks of %v, want the one writer's alone", clients)
	}
	held.Close()
	// Each edit inserts at the text's head, so none merges with another: smallestReplacementAdds
	// structs of the browser's own, which take its message past the cap the history stands within
	// that many structs of.
	unsentInto := firstText(held.Doc.GetXmlFragment(fragmentName))
	for range smallestReplacementAdds {
		docstest.Transact(held.Doc, func(txn *crdt.Transaction) { unsentInto.Insert(txn, 0, "unsent ", nil) })
	}
	closeResidentRoom(t, service, artifactID)
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != before {
		t.Fatalf("the rebuilt document reads %d bytes, want the %d it read before", len(rebuilt), len(before))
	}
	stored, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltHistory, err := decodeHistory(stored.Update)
	if err != nil {
		t.Fatal(err)
	}
	if retired := retiredClientsOf(rebuiltHistory); len(retired) != 1 || retired[oneClientWriter] == 0 {
		t.Fatalf("the rebuilt history lists %v as retired, want the one writer alone", retired)
	}
	// The rebuilt room holds none of the browser's clients, so the message it hands back is its
	// whole state, which the walk refuses having read the browser's own structs alone.
	read, _, err := walkRecorded(crdt.EncodeStateAsUpdateV1(held.Doc, nil), fromPeer)
	if !errors.Is(err, errUpdateUndecodable) || len(read.structs) != smallestReplacementAdds {
		t.Fatalf("the walk answered the browser's message %v having read %d structs, want it refused past the cap after the browser's %d",
			err, len(read.structs), smallestReplacementAdds)
	}
	for _, walked := range read.structs {
		if crdt.ClientID(walked.client) != browserClient {
			t.Fatalf("the walk read a struct of client %d before it refused, want the browser's alone", walked.client)
		}
	}

	reconnected := docstest.Dial(t, url, alice, artifactID, held.Doc)
	select {
	case <-reconnected.Ended:
	case <-time.After(time.Minute):
		t.Fatal("the room left the browser holding the replaced history connected")
	}
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentNeedsRebuildCloseReason {
		t.Fatalf("the room ended the browser holding the replaced history with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentNeedsRebuildCloseReason)
	}

	fresh := docstest.Dial(t, url, alice, artifactID, crdt.New())
	awaitAnswer(t, fresh, "the rebuilt document")
	if rendering, err := renderDocument(fresh.Doc); err != nil || rendering != rebuilt {
		t.Fatalf("the fresh read holds a %d-byte rendering (%v), want the rebuilt %d", len(rendering), err, len(rebuilt))
	}
	freshInto := firstText(fresh.Doc.GetXmlFragment(fragmentName))
	if _, err := fresh.Send(func(txn *crdt.Transaction) { freshInto.Insert(txn, 0, "fresh ", nil) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the fresh read's edit to be stored", func() bool {
		stored, err := service.persistence.Load(ctx, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		history, err := decodeHistory(stored.Update)
		if err != nil {
			t.Fatal(err)
		}
		rendering, err := renderDocument(history)
		return err == nil && strings.Contains(rendering, "fresh ") && !strings.Contains(rendering, "unsent")
	})
	select {
	case <-fresh.Ended:
		t.Fatalf("the room ended the fresh read's connection with %v", fresh.Closed())
	default:
	}
}

// A browser hands its whole state over whenever it connects - its structs and its whole delete
// set, in its answer to the room's sync step 1 - so a document at the decode cap must take a
// handoff of what it already holds, or no browser could reconnect to it. The room holds every
// delete range of that state deleted already, and the struct total counts nothing for them
// (deletedRuns.splitEnds), where two splits a range would take it past the cap. For each of the
// two documents a room keeps at the cap, and for the long-edited one typed on to one keystroke
// short of it, a browser that read the document hands its whole state back, and another
// reconnects holding it: both are admitted and stay connected, the room's state as it was. A third
// reconnects with an edit it typed offline, which deletes nothing and adds one struct with the two
// splits it can cause: admitted and stored where the room's total leaves room for those, and
// refused DOC_TOO_LARGE where it does not, the room as it was, so no handoff takes the room past
// the cap. Measured on 2026-10-28, both documents at the cap load at 1,048,576 structs exactly,
// the long-edited one's handoff carrying 1,345 delete ranges and the agent's 32,793; the
// long-edited one took 33 s here, the one a keystroke short 30 s, and the agent's 229 s, which
// runs with DISPATCH_REBUILD_AGENT_REWRITES=1, as its rebuild does.
func TestAWholeStateHandoffOfADocumentAtTheDecodeCapIsAdmitted(t *testing.T) {
	const editAdds = 1 + splitsAStructCanCause
	for _, row := range []struct {
		name  string
		gate  string
		state func(t *testing.T) []byte
		// edits is whether the room's total leaves room for the offline edit.
		edits bool
	}{
		{"an agent's whole rewrites until the cap refused one", agentRewritesEnv, func(t *testing.T) []byte {
			state, refusal := agentRewrittenState(t)
			if !errors.Is(refusal, ErrDocumentTooLarge) {
				t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
			}
			return state
		}, false},
		{"a long-edited document typed on until the cap", "", longEditedAtCapState, false},
		{"a long-edited document typed on until a keystroke short of the cap", "", func(t *testing.T) []byte {
			return longEditedTypedTo(t, maxUpdateItems-editAdds)
		}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.gate != "" && os.Getenv(row.gate) == "" {
				t.Skip("set " + row.gate + "=1 to run this document")
			}
			state := row.state(t)
			service, artifactID := newTestService(t)
			storeHistory(t, service, artifactID, state)
			closeResidentRoom(t, service, artifactID)
			server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(server.Close)
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
			ctx := context.Background()

			reader := connectPeer(t, server.URL, artifactID, "alice")
			awaitAnswer(t, reader, "the document near the cap")
			live := service.srv.GetDoc(artifactID)
			replica := service.liveReplica(live)
			if live == nil || replica == nil {
				t.Fatal("the document's room is not resident with a browser connected")
			}
			before := crdt.EncodeStateAsUpdateV1(live, nil)
			structs := countRoomStructs(live)
			total, _ := replica.structs.running()
			handoff := crdt.EncodeStateAsUpdateV1(reader.Doc, nil)
			tally, err := walkUpdate(handoff, fromPeer, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the room holds %d structs, a struct total of %d; a handoff carries %d structs and %d delete ranges",
				structs, total, tally.structs, tally.deleteRanges)
			if total < structs || total+splitsAStructCanCause*tally.deleteRanges <= maxUpdateItems || (total+editAdds <= maxUpdateItems) != row.edits {
				t.Fatalf("the room's total is %d for %d structs, and a handoff carries %d delete ranges: want it no less than the count, two splits a range past the %d cap, and room for an edit %t",
					total, structs, tally.deleteRanges, maxUpdateItems, row.edits)
			}

			// The room answers a request for the document once it has applied what the browser sent
			// before it, since it reads one connection's messages in order.
			confirmed := func(peer *docstest.Peer, what string) {
				t.Helper()
				if err := peer.AskForDocument(); err != nil {
					t.Fatal(err)
				}
				awaitAnswer(t, peer, what)
				select {
				case <-peer.Ended:
					t.Fatalf("the room ended %s with %v", what, peer.Closed())
				default:
				}
			}
			holding := func() *crdt.Doc {
				doc := crdt.New()
				if err := crdt.ApplyUpdateV1(doc, handoff, nil); err != nil {
					t.Fatal(err)
				}
				return doc
			}
			unchanged := func(what string) {
				t.Helper()
				if after := crdt.EncodeStateAsUpdateV1(live, nil); !bytes.Equal(after, before) || countRoomStructs(live) != structs {
					t.Fatalf("after %s the room's state is %d bytes in %d structs, want the %d bytes in %d structs it held", what, len(after), countRoomStructs(live), len(before), structs)
				}
			}
			if err := reader.Write(ygsync.EncodeUpdate(handoff)); err != nil {
				t.Fatal(err)
			}
			confirmed(reader, "the browser that handed its whole state back")
			reconnected := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"bob"}}, artifactID, holding())
			awaitAnswer(t, reconnected, "the document to a browser holding it")
			confirmed(reconnected, "the browser that reconnected holding the document")
			unchanged("the handoffs")
			if after, _ := replica.structs.running(); after > total {
				t.Fatalf("after the handoffs the room's struct total is %d, want no more than the %d it stood at", after, total)
			}

			editing := holding()
			into := firstText(editing.GetXmlFragment(fragmentName))
			docstest.Transact(editing, func(txn *crdt.Transaction) { into.Insert(txn, into.Len(), "offline", nil) })
			edited := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"carol"}}, artifactID, editing)
			if !row.edits {
				select {
				case <-edited.Ended:
				case <-time.After(time.Minute):
					t.Fatal("the room left connected a browser whose edit would take it past the cap")
				}
				if closed := edited.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentTooLargeCloseReason {
					t.Fatalf("the room ended the browser whose edit would take it past the cap with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
				}
				unchanged("the refused edit")
				return
			}
			awaitAnswer(t, edited, "the document to a browser with an edit")
			confirmed(edited, "the browser that reconnected with an edit")
			waitFor(t, 30*time.Second, "the offline edit to reach the document", func() bool {
				text, err := service.Text(ctx, artifactID)
				return err == nil && strings.Contains(text, "offline")
			})
			if after := countRoomStructs(live); after > maxUpdateItems {
				t.Fatalf("the edit left the room at %d structs, past the %d cap", after, maxUpdateItems)
			}
		})
	}
}

// The hook reads a retired client's group by its header, so it closes a browser holding the
// replaced history wherever the walk refuses that browser's message: at the header of the
// history's group, past the cap, or in a struct of the group, after the browser's own group either
// way, as well as when the walk refuses nothing. A group of a retired client from its retired clock
// on - what that client wrote after the rebuild, which a browser of the rebuilt document can hold -
// is not closed, and nor is a message of clients the rebuild did not retire that the walk refuses,
// which is answered with the walk's own refusal.
func TestAGroupOfARetiredClientBelowItsClockIsClosedWhereverTheWalkRefuses(t *testing.T) {
	retired := map[crdt.ClientID]uint64{123: 5}
	service, room, replica := rebuiltRoom(t, "# Rebuilt\n\nOne paragraph of ordinary prose.\n", retired)
	// own starts a message of groups client groups with the browser's own: one GC struct of a
	// client the rebuild did not retire.
	own := func(groups int) v1Frame { return newV1Frame(groups).group(1, 1000, 0).gc(1) }
	for _, row := range []struct {
		name   string
		update []byte
		// closed is whether the hook closes the browser for a rebuild, never applying the
		// message; otherwise refused is the error the hook answers with, nil for one it applies.
		closed  bool
		refused error
	}{
		{name: "the history's header past the cap", update: own(2).group(maxUpdateItems, 123, 0).done(), closed: true},
		{name: "a struct of the history cut short", update: own(2).group(1, 123, 0).done(), closed: true},
		{name: "the history the walk reads whole", update: own(2).group(1, 123, 0).gc(5).noDeletes(), closed: true},
		{name: "a retired client's group from its retired clock on", update: own(2).group(1, 123, 5).gc(1).noDeletes()},
		{name: "another client's header past the cap", update: own(2).group(maxUpdateItems, 456, 0).done(), refused: errUpdateUndecodable},
	} {
		err, called := weighInto(service, room, row.update)
		var refusal *websocket.PeerClose
		rebuildClose := errors.As(err, &refusal) && refusal.Code == contracts.DocumentTooLargeCloseCode &&
			refusal.Reason == contracts.DocumentNeedsRebuildCloseReason
		switch {
		case row.closed && (!rebuildClose || called):
			t.Errorf("%s was answered %v (applied %t), want it closed for a rebuild and never applied", row.name, err, called)
		case !row.closed && row.refused == nil && (err != nil || !called):
			t.Errorf("%s was answered %v (applied %t), want it applied", row.name, err, called)
		case !row.closed && row.refused != nil && (!errors.Is(err, row.refused) || rebuildClose || called):
			t.Errorf("%s was answered %v (applied %t), want %v and never applied", row.name, err, called, row.refused)
		}
	}
	runtime.KeepAlive(replica)
}

// awaitAnswer waits for the room's sync step 2 to peer, its whole document.
func awaitAnswer(t *testing.T, peer *docstest.Peer, what string) {
	t.Helper()
	select {
	case <-peer.Answers:
	case <-peer.Ended:
		t.Fatalf("the room ended the connection before sending %s: %v", what, peer.Closed())
	case <-time.After(time.Minute):
		t.Fatalf("the room never sent %s", what)
	}
}
