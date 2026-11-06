package docs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
// (skipDeletedForTextAnchor, sjawhar/ygo v1.55.1-sami.1, crdt/ytext.go:711-754), and the
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
// struct (sjawhar/ygo v1.55.1-sami.1, crdt/yarray.go:829-846), and the document stays at the cap
// it was already at, as logPastDecodeCap's comment allows for. What no document at the cap takes
// is a write that adds a struct, and that is what a rebuild has to make room for.
func TestRebuildingADocumentAtTheDecodeCapLeavesItSmallAndReadingTheSame(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
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
			history := decodedHistory(t, state)
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

// decodedHistory is update decoded as a room's load decodes it (decodeStoredCopy), the document
// alone, failing t where ygo refuses it: what these tests read of a stored history.
func decodedHistory(t *testing.T, update []byte) *crdt.Doc {
	t.Helper()
	decoded, err := decodeStoredCopy(update, maxUpdateItems)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.doc
}

// freshStructs is the struct total of a fresh document of the text update's document holds, as
// an upload of it would seed it (encodeDocumentTree).
func freshStructs(t *testing.T, update []byte) uint64 {
	t.Helper()
	doc := decodedHistory(t, update)
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	fresh := decodedHistory(t, seed)
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
// retired, bounded and listed as every room is (peerUpdateRoom), with the retired clients its
// load read from the seed (keepReplica, retiredClientsOf).
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
	service := calibratedService()
	return service, room, service.keepReplica(room, nil, loadRendering(rendering), nil)
}

// The retired clients' list is the rebuild's alone: an update that adds an entry to it, one that
// overwrites an entry - a map entry written again, naming the entry before it as its origin -
// and one that deletes an entry are each refused and leave the room as it was, while typing and
// a whole-state message of what the room holds, the list included, are admitted.
//
// What refuses them is the allowed roots a browser's apply runs under (peerAllowedRoots): the
// fragment and the margin, and nothing else. The rule is a containment rule over the whole
// update, so it refuses an item written beside an entry the room already holds, which names no
// root on the wire, and a delete of one, which names nothing either
// (crdt.WithAllowedRoots). The close is the document's own, 4413 DOC_UPDATE_REFUSED, since the
// editor never sends such an update.
func TestAnUpdateThatOverwritesOrDeletesARetiredEntryIsRefused(t *testing.T) {
	retired := map[crdt.ClientID]uint64{123: 5, 456: 7}
	service, room, replica := rebuiltRoom(t, "# Rebuilt\n\nOne paragraph of ordinary prose.\n", retired)
	if listed := replica.retiredClients(); len(listed) != len(retired) {
		t.Fatalf("the room's load read %d retired clients, want the seed's %d", len(listed), len(retired))
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
		var refusal *websocket.PeerClose
		if !errors.Is(err, crdt.ErrRootNotAllowed) || !errors.As(err, &refusal) ||
			refusal.Code != contracts.DocumentTooLargeCloseCode || refusal.Reason != contracts.DocumentUpdateRefusedCloseReason {
			t.Fatalf("%s was answered %v (applied %t), want it refused for the root it names, closed %d %q",
				row.name, err, called, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
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
// closed with the document's own close and DOC_REBUILT; and the editor's fresh read of the
// document, as its 4413 path makes, reads the rebuilt text and saves a new edit. A document far
// from both caps with a resident room is still refused, DOCUMENT_LIVE
// (TestRebuildDocumentRefusesALiveRoom).
//
// Its fixture stands at the decode cap (longEditedAtCapState), built by about a million ygo
// writes, so a -race build leaves it out (skipAtTheBoundsUnderRace). What the detector could
// catch here the tests beside it drive over a small document in seconds: the rebuild's close
// against an apply still writing in the room (TestTheRebuildsCloseLeavesARoomAnApplyIsWriting),
// the rebuild a live room is refused for (TestRebuildDocumentRefusesALiveRoom), and a browser
// whose socket a room's close ends and which reconnects and re-syncs
// (TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip).
func TestARebuildNearACapClosesTheResidentRoomAndItsEditorsReAdmitTheRebuiltDocument(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
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
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentRebuiltCloseReason {
		t.Fatalf("the room ended the reconnecting browser with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentRebuiltCloseReason)
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

// tangledHistory is a document's history whose re-scan comes to about scans items: its first
// paragraph followed by chain one-character items, each of a client of its own so none merges
// with its neighbour, and then a ladder of scanner items, each of a client of its own, each
// sitting between the chain's first item and a chain item of its own, so each scans a stretch of
// the chain (the ladder below).
func tangledHistory(t *testing.T, chain, scans int) []byte {
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
		chained = chained.group(1, client, 0).raw(structStringRef | infoHasOrigin).uint(uint64(origin.Client)).uint(origin.Clock).str("a")
		origin = crdt.ID{Client: crdt.ClientID(client), Clock: 0}
	}
	// Each scanner is a client of its own, past the chain's, and sits between the chain's first
	// item and one of its own: its left origin is the chain's first and its right origin the
	// (chain-1-i)-th, so its integrate scans the chain from the first to that one (Item.integrate),
	// about chain-i items. They cannot share an anchor: two items naming the same left origin and
	// the same right origin are symmetric, and ygo's integrate breaks at the first of them
	// whichever order they arrive in, so scanners that all name the chain's first and no right
	// origin - as this fixture once wrote them - scan one item each in a re-scan of the room's
	// state, which integrates client groups by descending client. The ladder reaches scans in
	// total, or as near it as a chain of this length allows.
	count, ladder := 0, 0
	for count < chain-1 && ladder < scans {
		ladder += chain - count
		count++
	}
	scanning := newV1Frame(count)
	for i := range count {
		rightOrigin := uint64(firstClient + chain - 1 - i)
		scanning = scanning.group(1, uint64(scanClient+i), 0).
			raw(structStringRef | infoHasOrigin | infoHasRightOrigin).
			uint(firstClient).uint(0).uint(rightOrigin).uint(0).str("k")
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
// handoff (rescan, a count of the re-scan's transaction) and reads the same text. The
// tangle's scanners, about 2,000 scans each, are enough to pass the total less one handoff.
func TestRebuildingADocumentNearTheScanTotalLeavesItWithASmallScanTotalAndReadingTheSame(t *testing.T) {
	state := tangledHistory(t, 2_000, roomScanTotal-honestHandoffScans)
	history := decodedHistory(t, state)
	scans, err := rescan(crdt.EncodeStateAsUpdateV1(history, nil))
	if err != nil {
		t.Fatalf("the tangled history's re-scan could not be counted: %v", err)
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
	rebuilt := decodedHistory(t, loaded.Update)
	after, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if rescans, _ := rescan(crdt.EncodeStateAsUpdateV1(rebuilt, nil)); rescans >= honestHandoffScans || after != before {
		t.Fatalf("the rebuilt history re-scans %d items and reads %d bytes, want fewer than %d and the %d it read before", rescans, len(after), honestHandoffScans, len(before))
	}
}

// A browser that held a document before its rebuild still holds the history the rebuild replaced,
// and the edits it made while disconnected. When it reconnects, the rebuilt room holds none of
// that history, so the browser's answer to the room's sync hands all of it back with its unsent
// edit: the room refuses it with the document's own close, on which the editor drops what it
// sent and builds its document afresh from the room. The room's struct count stays small, the
// document reads the rebuilt text with no block twice and without the unsent edit, and a browser
// of the rebuilt history - its whole state, the retired clients' list included, handed back -
// edits it as before.
//
// The reason is DOC_UPDATE_REFUSED here rather than DOC_REBUILT, because this history stands at
// the decode cap: the message carrying it holds more structs than one update may decode, so the
// retired-client scan cannot read its header (crdt.ParseUpdateMeta refuses an update past
// maxV2Items) and refuses nothing, and ygo's own decoder is what refuses the bytes
// (refusedBeforeApply, refusedByApply). A history the scan can read is closed DOC_REBUILT, which
// TestAGroupOfARetiredClientBelowItsClockIsClosedAndAHeaderThatWillNotReadIsRefused and the row
// of TestEveryRefusalOfTheHookCarriesItsOwnCodeAndReason that hands one back drive. Either way
// the browser is closed 4413 and comes back from a fresh read.
//
// Its fixture stands at the decode cap (longEditedAtCapState), so a -race build leaves it out
// (skipAtTheBoundsUnderRace): the fixture's million instrumented writes cost the race run minutes
// and gigabytes, and under -race on the development pod a socket connect of this test timed out
// after 192 s. What the detector could catch here the tests beside it drive over a small
// document: the hook closing a browser that hands back a history a rebuild replaced
// (TestAGroupOfARetiredClientBelowItsClockIsClosedAndAHeaderThatWillNotReadIsRefused, and the
// row of TestEveryRefusalOfTheHookCarriesItsOwnCodeAndReason that hands one back), and a browser
// whose socket a room's close ends and which reconnects and re-syncs
// (TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip).
func TestABrowserHoldingTheHistoryARebuildReplacedCannotHandItBack(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
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
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentUpdateRefusedCloseReason {
		t.Fatalf("the room ended the browser holding the replaced history with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room closed with the refusal, want it open for the document's other browsers")
	}
	replica := service.replicaOf(live)
	if replica == nil || len(replica.retiredClients()) == 0 {
		t.Fatal("the rebuilt room lists no retired clients")
	}
	if structs := countRoomStructs(live); structs > maxUpdateItems/16 {
		t.Fatalf("after the reconnect the room holds %d structs, want it small", structs)
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
// below the browser
// TestABrowserHoldingAHistoryARebuildReplacedIsClosedThoughItsMessageWillNotRead
// connects with: ygo and Yjs both write a state's client groups by descending client
// (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:386-387), so the browser's own group comes before
// the history's.
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

// A browser holding the history a rebuild replaced is sent back to a fresh read even where
// nothing can read its whole-state message's header. Here the history is one client's, at the
// decode cap, and the browser's own unsent edits stand beside it, so the message carries more
// structs than one update may decode: crdt.ParseUpdateMeta refuses it (maxV2Items), the
// retired-client scan refuses nothing of its own and the apply judges the bytes, where ygo's own
// decoder refuses them and rolls the apply back whole (refusedBeforeApply, refusedByApply).
//
// What matters is that the refusal is a close the editor acts on. A plain error rather than a
// *websocket.PeerClose has ygo answer SyncStatus(0) and then close with 4205 "Reset Connection"
// (sjawhar/ygo v1.55.1-sami.1, provider/websocket/peer_update.go:142-154,
// provider/websocket/peer.go:271-290, over the Hocuspocus framing New sets); 4205 is what
// @hocuspocus/provider 2.x reconnects at once after, so the browser would hand the same message
// back on every reconnect while each later edit of it, naming the old history's items, parked.
// The room closes it with the document's own close, 4413 DOC_UPDATE_REFUSED; it reconnects from
// a fresh read of the rebuilt document, as the editor's 4413 path does, not with its old state;
// and a new edit from it is applied and stored.
//
// Its fixture stands at the decode cap (oneClientAtCapState), built by about a million ygo
// writes, so a -race build leaves it out (skipAtTheBoundsUnderRace). What the detector could
// catch here the tests beside it drive over a small document: the hook closing a browser that
// holds a history a rebuild replaced, whether or not its header reads
// (TestAGroupOfARetiredClientBelowItsClockIsClosedAndAHeaderThatWillNotReadIsRefused, and the
// row of TestEveryRefusalOfTheHookCarriesItsOwnCodeAndReason that hands one back), and a browser
// whose socket a room's close ends and which reconnects and re-syncs
// (TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip).
func TestABrowserHoldingAHistoryARebuildReplacedIsClosedThoughItsMessageWillNotRead(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	const browserClient crdt.ClientID = 0x0ce2
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
	rebuiltHistory := decodedHistory(t, stored.Update)
	if retired := retiredClientsOf(rebuiltHistory); len(retired) != 1 || retired[oneClientWriter] == 0 {
		t.Fatalf("the rebuilt history lists %v as retired, want the one writer alone", retired)
	}
	// The rebuilt room holds none of the browser's clients, so the message it hands back is its
	// whole state: the replaced history's writer from clock 0 beside the browser's own structs,
	// which together carry more structs than one update may decode. Nothing can read that
	// message's header, so the retired-client scan refuses nothing and the apply is what judges
	// the bytes.
	handback := crdt.EncodeStateAsUpdateV1(held.Doc, nil)
	if _, _, err := crdt.ParseUpdateMeta(handback); !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("the %d-byte message's header read as %v, want it refused as an update ygo will not decode", len(handback), err)
	}

	reconnected := docstest.Dial(t, url, alice, artifactID, held.Doc)
	select {
	case <-reconnected.Ended:
	case <-time.After(time.Minute):
		t.Fatal("the room left the browser holding the replaced history connected")
	}
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentUpdateRefusedCloseReason {
		t.Fatalf("the room ended the browser holding the replaced history with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
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
		history := decodedHistory(t, stored.Update)
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
// handoff of what it already holds, or no browser could reconnect to it. The room already holds
// every struct and every delete range of that state, so the handoff integrates nothing and adds
// nothing to its count. For each of the two documents a room keeps at the cap, and for the
// long-edited one typed on to one keystroke short of it, a browser that read the document hands
// its whole state back, and another reconnects holding it: both are admitted and stay connected,
// the room's state as it was. A third reconnects with an edit it typed offline, which deletes
// nothing and adds one struct with the two splits it can cause: every one of these rooms refuses
// it and is left as it was, so no handoff and no edit takes a room past the cap. The reason
// differs with what refuses it - the struct cap's DOC_TOO_LARGE where the room's own count
// leaves no room for the edit, and the state cap's DOC_NEEDS_REBUILD where it does, since a
// document at the decode cap holds more state than one room may (maxRoomStateBytes) and takes
// no insert at all until it is rebuilt. Measured on 2026-10-28, both documents at the cap load
// at 1,048,576 structs exactly, the long-edited one's handoff carrying 1,345 delete ranges and
// the agent's 32,793; the long-edited one took 33 s here, the one a keystroke short 30 s, and
// the agent's 229 s, which runs with DISPATCH_REBUILD_AGENT_REWRITES=1, as its rebuild does. The
// row a keystroke short of the cap types a million keystrokes of its own, where the rows before
// it share one fixture.
//
// Every row's fixture stands at the decode cap, each built by about a million ygo writes, so a
// -race build leaves the test out (skipAtTheBoundsUnderRace): under -race on the development pod
// it carried its shard's resident memory from 4.2 GB to 9.3 GB over 4.75 minutes. What the
// detector could catch here the tests beside it drive over a small document: a room taking a
// whole state handed back (the row of TestAnUpdateThatOverwritesOrDeletesARetiredEntryIsRefused
// that hands the room its own state), two browsers' sockets with one update refused between them
// (TestARefusedPeerUpdateLeavesTheRoomAsItsOtherPeersHoldIt), and a browser whose socket a room's
// close ends and which reconnects and re-syncs
// (TestAWriteOnTheOtherProcessClosesThisProcessesRoomWithinTheRoundTrip).
func TestAWholeStateHandoffOfADocumentAtTheDecodeCapIsAdmitted(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	const editAdds = smallestReplacementAdds
	for _, row := range []struct {
		name  string
		gate  string
		state func(t *testing.T) []byte
		// edits is whether the room's own struct count leaves room for the offline edit, and
		// reason the close that edit meets: the struct cap's where it does not, the state cap's
		// where it does.
		edits  bool
		reason string
	}{
		{"an agent's whole rewrites until the cap refused one", agentRewritesEnv, func(t *testing.T) []byte {
			state, refusal := agentRewrittenState(t)
			if !errors.Is(refusal, ErrDocumentTooLarge) {
				t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
			}
			return state
		}, false, contracts.DocumentTooLargeCloseReason},
		{"a long-edited document typed on until the cap", "", longEditedAtCapState, false, contracts.DocumentTooLargeCloseReason},
		{"a long-edited document typed on until a keystroke short of the cap", "", func(t *testing.T) []byte {
			return longEditedTypedTo(t, maxUpdateItems-editAdds)
		}, true, contracts.DocumentNeedsRebuildCloseReason},
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

			reader := connectPeer(t, server.URL, artifactID, "alice")
			awaitAnswer(t, reader, "the document near the cap")
			live := service.srv.GetDoc(artifactID)
			replica := service.replicaOf(live)
			if live == nil || replica == nil {
				t.Fatal("the document's room is not resident with a browser connected")
			}
			before := crdt.EncodeStateAsUpdateV1(live, nil)
			structs := countRoomStructs(live)
			ceiling := structCeiling(live)
			handoff := crdt.EncodeStateAsUpdateV1(reader.Doc, nil)
			t.Logf("the room holds %d structs, a ceiling of %d; a handoff carries %d bytes", structs, ceiling, len(handoff))
			if ceiling < structs || (structs+editAdds <= maxUpdateItems) != row.edits {
				t.Fatalf("the room holds %d structs with a ceiling of %d: want the ceiling no less than the count and room for an edit %t against the %d cap",
					structs, ceiling, row.edits, maxUpdateItems)
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
			if after := structCeiling(live); after > ceiling {
				t.Fatalf("after the handoffs the room's ceiling is %d, want no more than the %d it stood at", after, ceiling)
			}

			editing := holding()
			into := firstText(editing.GetXmlFragment(fragmentName))
			docstest.Transact(editing, func(txn *crdt.Transaction) { into.Insert(txn, into.Len(), "offline", nil) })
			edited := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"carol"}}, artifactID, editing)
			select {
			case <-edited.Ended:
			case <-time.After(time.Minute):
				t.Fatal("the room left connected a browser whose edit it cannot take")
			}
			if closed := edited.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != row.reason {
				t.Fatalf("the room ended the browser carrying the offline edit with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, row.reason)
			}
			unchanged("the refused edit")
		})
	}
}

// A browser's message carrying a group of a client the rebuild retired, starting below the clock
// the rebuild recorded for it, is closed as holding the replaced history and never applied,
// whatever follows that group: the hook reads the update's header alone (crdt.ParseUpdateMeta),
// which reports the lowest clock each client's structs start at, and never a struct of it. A
// group of a retired client from its retired clock on - what that client wrote after the
// rebuild, which a browser of the rebuilt document can hold - is applied.
//
// A message whose header will not read at all refuses nothing here and falls through to the
// apply, which judges the bytes itself: ygo's own decoder refuses the same bytes, rolls the
// whole apply back and the hook answers DOC_UPDATE_REFUSED. Refusing it here instead would
// close an honest whole-state handoff of a document at the decode cap, whose header carries
// more structs than one update may decode, with the wrong reason. Either way the room is left
// byte for byte as it was.
func TestAGroupOfARetiredClientBelowItsClockIsClosedAndAHeaderThatWillNotReadIsRefused(t *testing.T) {
	retired := map[crdt.ClientID]uint64{123: 5}
	service, room, replica := rebuiltRoom(t, "# Rebuilt\n\nOne paragraph of ordinary prose.\n", retired)
	// own starts a message of groups client groups with the browser's own: one GC struct of a
	// client the rebuild did not retire.
	own := func(groups int) v1Frame { return newV1Frame(groups).group(1, 1000, 0).gc(1) }
	for _, row := range []struct {
		name   string
		update []byte
		// reason is the close the hook answers with, empty for a message it applies.
		reason string
		// afterApply is whether the hook reached the apply: a header the retired-client scan
		// cannot read is judged by the apply and by nothing before it.
		afterApply bool
	}{
		{name: "the history's header past the cap", update: own(2).group(maxUpdateItems, 123, 0).done(), reason: contracts.DocumentUpdateRefusedCloseReason, afterApply: true},
		{name: "a struct of the history cut short", update: own(2).group(1, 123, 0).done(), reason: contracts.DocumentUpdateRefusedCloseReason, afterApply: true},
		{name: "the history the header reads whole", update: own(2).group(1, 123, 0).gc(5).noDeletes(), reason: contracts.DocumentRebuiltCloseReason},
		{name: "a retired client's group from its retired clock on", update: own(2).group(1, 123, 5).gc(1).noDeletes()},
		{name: "another client's header that will not read", update: own(2).group(maxUpdateItems, 456, 0).done(), reason: contracts.DocumentUpdateRefusedCloseReason, afterApply: true},
	} {
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, row.update)
		var refusal *websocket.PeerClose
		closed := errors.As(err, &refusal) && refusal.Code == contracts.DocumentTooLargeCloseCode
		switch {
		case row.reason == "" && (err != nil || !called):
			t.Errorf("%s was answered %v (the apply ran %t), want it applied", row.name, err, called)
		case row.reason != "" && (!closed || refusal.Reason != row.reason || called != row.afterApply):
			t.Errorf("%s was answered %v (the apply ran %t), want it closed %d %q with the apply run %t",
				row.name, err, called, contracts.DocumentTooLargeCloseCode, row.reason, row.afterApply)
		case row.reason != "" && !bytes.Equal(crdt.EncodeStateAsUpdateV1(room, nil), before):
			t.Errorf("%s changed the room though it was refused", row.name)
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

// longEditFixture is the long-edited document's encoded state as longEditedRoom leaves it after
// longEditKeystrokes keystrokes, gzipped. It is stored rather than made by each test that reads
// it because the simulation takes about twelve minutes on one processor: every keystroke's place
// is found by a walk of its text's items, and the items grow with the edit.
const longEditFixture = "testdata/long_edited_document.update.gz"

// longEditKeystrokes is how many keystrokes the long-edited fixture took.
const longEditKeystrokes = 1_000_000

// longEdit is what longEditedRoom did to its document: keystrokes in all, characters typed (each
// a keystroke), typos (each a wrong character typed and taken back, two keystrokes), characters
// taken back one keystroke at a time, selections deleted (each a keystroke) and the characters
// they held, and the room's residencies.
type longEdit struct {
	keystrokes, typed, typos, backspaced, selections, selected, sessions int
}

// longEditedRoom is a room's live document holding about 5 KiB of prose after one person has edited
// it for keystrokes keystrokes, deterministically: the same keystrokes always make the same bytes.
//
// The document is ten paragraphs of 512 bytes. The person places the cursor anywhere in any
// paragraph and then, while the document stays within 256 bytes of 5 KiB, types a run of about 14
// characters (55 per cent of the time; one character in 33 is a typo taken back at once), takes
// back about 5 characters one keystroke at a time (30 per cent), or deletes a selection of about 24
// characters in one keystroke (15 per cent); outside the 256 bytes they type, or delete, until
// back within them. Every keystroke is a transaction of its own on the room, with the browser's
// client id: what a room holds after applying each keystroke's update as it arrives, which
// TestALongEditsKeystrokesLeaveTheRoomWhatTheirUpdatesLeaveThroughTheHook holds. Each 4,000
// keystrokes (about twenty minutes of editing) the room is retired and loaded again from its state
// in one decode, as a room retired once unused is, and the person's next page load edits it with a
// new client id. relay, when not nil, is called with the state each residency loads from, and the
// function it returns is handed each keystroke's update in that residency.
func longEditedRoom(tb testing.TB, keystrokes int, relay func(loaded []byte) func(update []byte)) (*crdt.Doc, longEdit) {
	tb.Helper()
	const (
		paragraphs        = 10
		target            = 5 << 10
		sessionKeystrokes = 4_000
	)
	minted := 0
	pmdoc.SetBlockIDGenerator(func() string {
		minted++
		return fmt.Sprintf("long-edit-%d", minted)
	})
	defer pmdoc.SetBlockIDGenerator(nil)
	var seed strings.Builder
	for range paragraphs {
		seed.WriteString(strings.TrimSpace(strings.Repeat("word ", target/paragraphs/5)))
		seed.WriteString("\n\n")
	}
	tree, err := pmdoc.Parse(seed.String())
	if err != nil {
		tb.Fatal(err)
	}
	seeded := newDocumentCopy(crdt.WithClientID(1))
	fragment := seeded.GetXmlFragment(fragmentName)
	seeded.GetMap(marksMapName)
	if err := seeded.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		tb.Fatal(err)
	}
	state := crdt.EncodeStateAsUpdateV1(seeded, nil)

	rng := rand.New(rand.NewPCG(179, 12))
	geometric := func(mean float64) int { return 1 + int(-math.Log(1-rng.Float64())*(mean-1)) }
	const letters = "etaoinshrdlucmfwypvbgkjqxz      "
	var done longEdit
	var room *crdt.Doc
	var texts []*crdt.YXmlText
	keystroke := func(change func(txn *crdt.Transaction)) {
		room.Transact(change)
		done.keystrokes++
	}
	for done.keystrokes < keystrokes {
		if room == nil || done.keystrokes/sessionKeystrokes >= done.sessions {
			if room != nil {
				state = crdt.EncodeStateAsUpdateV1(room, nil)
			}
			// A client id of five bytes, as most random ids are (the browser's
			// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor gives why).
			room = newDocumentCopy(crdt.WithClientID(crdt.ClientID(1<<31 + rng.Uint32()>>1)))
			if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
				tb.Fatal(err)
			}
			if relay != nil {
				keystrokeUpdate := relay(state)
				room.OnUpdate(func(update []byte, _ any) { keystrokeUpdate(update) })
			}
			texts = allTexts(room.GetXmlFragment(fragmentName))
			if len(texts) != paragraphs {
				tb.Fatalf("the document holds %d texts, want %d", len(texts), paragraphs)
			}
			done.sessions++
		}
		text := texts[rng.IntN(len(texts))]
		pos := rng.IntN(text.Len() + 1)
		total := 0
		for _, text := range texts {
			total += text.Len()
		}
		roll := rng.Float64()
		switch {
		case total < target-256 || (total <= target+256 && roll < 0.55):
			for range geometric(14) {
				if rng.Float64() < 0.03 {
					keystroke(func(txn *crdt.Transaction) { text.Insert(txn, pos, "q", nil) })
					keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos, 1) })
					done.typos++
				}
				char := string(letters[rng.IntN(len(letters))])
				keystroke(func(txn *crdt.Transaction) { text.Insert(txn, pos, char, nil) })
				pos++
				done.typed++
			}
		case total > target+256 || roll < 0.85:
			for range min(geometric(5), pos) {
				keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos-1, 1) })
				pos--
				done.backspaced++
			}
		default:
			if n := min(geometric(24), text.Len()-pos); n > 0 {
				keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos, n) })
				done.selections++
				done.selected += n
			}
		}
	}
	return room, done
}

// allTexts is every text in node, in document order, however deep.
func allTexts(node any) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	var walk func(node any)
	walk = func(node any) {
		switch node := node.(type) {
		case *crdt.YXmlText:
			texts = append(texts, node)
		case *crdt.YXmlFragment:
			for _, child := range node.Children() {
				walk(child)
			}
		case *crdt.YXmlElement:
			for _, child := range node.Children() {
				walk(child)
			}
		}
	}
	walk(node)
	return texts
}

// longEditedState is the long-edited fixture's encoded state.
func longEditedState(tb testing.TB) []byte {
	tb.Helper()
	file, err := os.Open(longEditFixture)
	if err != nil {
		tb.Fatal(err)
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		tb.Fatal(err)
	}
	state, err := io.ReadAll(unzipped)
	if err != nil {
		tb.Fatal(err)
	}
	return state
}

// The long-edited fixture is a document of about 5 KiB of prose that loads and renders, whose
// state carries the history of its million keystrokes: more than ten structs for each byte its
// text renders (93 measured), since ygo keeps each character taken back one keystroke at a time a
// struct of its own (gcTxnDeleteSet replaces a deleted item's content and merges nothing,
// sjawhar/ygo v1.55.1-sami.1, crdt/snapshot.go:370-407).
func TestTheLongEditedFixtureIsAFiveKiBDocumentCarryingItsHistory(t *testing.T) {
	room := newDocumentCopy()
	state := longEditedState(t)
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	structs := countRoomStructs(room)
	t.Logf("the long-edited fixture renders %d bytes; its %d-byte state loads as %d structs", len(rendering), len(state), structs)
	if len(rendering) < 4<<10 || len(rendering) > 6<<10 || structs < 10*uint64(len(rendering)) {
		t.Fatalf("the fixture renders %d bytes in %d structs, want about 5 KiB carrying more than ten structs a byte", len(rendering), structs)
	}
}

// The fixture is what longEditedRoom makes, byte for byte. Set DISPATCH_LONG_EDIT_FIXTURE=check
// to check it, or =write to write it again after a change to the simulation; either runs the
// simulation, about twelve minutes on one processor.
func TestTheLongEditedFixtureIsWhatTheSimulationMakes(t *testing.T) {
	mode := os.Getenv("DISPATCH_LONG_EDIT_FIXTURE")
	if mode == "" {
		t.Skip("set DISPATCH_LONG_EDIT_FIXTURE=check or =write to run the twelve-minute simulation")
	}
	room, done := longEditedRoom(t, longEditKeystrokes, nil)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	t.Logf("%+v: a %d-byte state", done, len(state))
	if mode == "write" {
		var zipped bytes.Buffer
		writer, err := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(state); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(longEditFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(longEditFixture, zipped.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(state, longEditedState(t)) {
		t.Fatalf("the simulation makes a %d-byte state that differs from the fixture's: write it again with DISPATCH_LONG_EDIT_FIXTURE=write", len(state))
	}
}

// longEditedRoom applies each keystroke to the room as a transaction of its own. A room takes a
// browser's keystrokes as updates through the hook, each applied in a transaction of its own
// (weighPeerUpdate): the two leave the same struct total, so the simulation's figure is a room's.
// One residency is driven both ways here, its keystrokes relayed through the hook to a second room
// loaded from the same state.
func TestALongEditsKeystrokesLeaveTheRoomWhatTheirUpdatesLeaveThroughTheHook(t *testing.T) {
	const keystrokes = 3_000
	var through *crdt.Doc
	var replica *renderedReplica
	relayed := 0
	room, done := longEditedRoom(t, keystrokes, func(loaded []byte) func(update []byte) {
		through = newDocumentCopy()
		if err := crdt.ApplyUpdateV1(through, loaded, nil); err != nil {
			t.Fatal(err)
		}
		rendering, err := renderDocument(through)
		if err != nil {
			t.Fatal(err)
		}
		service := calibratedService()
		replica = service.keepReplica(through, nil, loadRendering(rendering), nil)
		return func(update []byte) {
			if err, called := weighInto(service, through, update); err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t) to keystroke %d, want it applied", err, called, relayed+1)
			}
			relayed++
		}
	})
	runtime.KeepAlive(replica)
	if done.sessions != 1 || relayed != done.keystrokes {
		t.Fatalf("the drive took %d residencies and relayed %d of %d keystrokes, want one residency relaying each", done.sessions, relayed, done.keystrokes)
	}
	direct, hooked := countRoomStructs(room), countRoomStructs(through)
	t.Logf("%+v: %d structs applied as transactions, %d through the hook", done, direct, hooked)
	if direct != hooked {
		t.Fatalf("the keystrokes leave %d structs applied as transactions and %d through the hook, want the same", direct, hooked)
	}
}

// agentRewrittenState is the stored state of a document an agent has rewritten whole through the
// API (ReplaceText), each write 16,384 headings of fresh text at both halves of the byte and
// element bounds, until a write is refused; and the refusal. It is built once a test process
// (atCapStates): the 28 writes take about 200 s here, and the state does not change between tests.
func agentRewrittenState(t *testing.T) ([]byte, error) {
	t.Helper()
	atCapStates.Lock()
	defer atCapStates.Unlock()
	if atCapStates.agentRewritten != nil {
		return atCapStates.agentRewritten, atCapStates.agentRefusal
	}
	service, artifactID := newTestService(t)
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	const headingBytes = 64
	headings := pmdoc.MaxDocumentBytes / headingBytes
	for write := 0; ; write++ {
		var markdown strings.Builder
		for heading := range headings {
			fmt.Fprintf(&markdown, "# %0*d\n\n", headingBytes-4, write*headings+heading)
		}
		_, err := joinedReplaceText(service, artifactID, markdown.String(), agent)
		if err != nil {
			if write == 0 {
				t.Fatalf("the first write: %v", err)
			}
			loaded, loadErr := service.persistence.Load(context.Background(), artifactID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			t.Logf("the agent's write %d was refused: %v", write+1, err)
			atCapStates.agentRewritten, atCapStates.agentRefusal = loaded.Update, err
			return loaded.Update, err
		}
	}
}

// structsAndRendering is state's struct total as a room loaded from it counts it, what it renders,
// and the struct total ygo's RunGC would leave it, which merges each run of adjacent deleted items
// one client wrote in order into one (sjawhar/ygo v1.55.1-sami.1, crdt/snapshot.go:522-606), as
// Yjs merges them at each transaction's end; no room runs it.
func structsAndRendering(t *testing.T, state []byte) (structs uint64, rendered int, merged uint64) {
	t.Helper()
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	structs = countRoomStructs(doc)
	crdt.RunGC(doc)
	return structs, len(rendering), countRoomStructs(doc)
}
