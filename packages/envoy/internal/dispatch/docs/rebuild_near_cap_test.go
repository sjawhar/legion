package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// atCapStates holds the two stored documents at the decode cap the rebuild tests share, each built
// once a test process: the agent's rewrites take minutes, and neither changes between tests.
var atCapStates struct {
	sync.Mutex
	longEdited []byte
}

// longEditedAtCapState is the long-edited fixture (longEditedState) edited on until its struct
// total stands at the decode cap: its first paragraph typed into at its head a character at a
// time, each its own item, as a person's keystrokes at one place are, until the decoded document
// counts maxUpdateItems structs, and those characters then deleted, so it renders the fixture's
// 5 KiB again while its state holds every keystroke. Characters typed one before another never
// merge into one item, in the room or in a decode of its state.
func longEditedAtCapState(t *testing.T) []byte {
	t.Helper()
	atCapStates.Lock()
	defer atCapStates.Unlock()
	if atCapStates.longEdited != nil {
		return atCapStates.longEdited
	}
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, longEditedState(t), nil); err != nil {
		t.Fatal(err)
	}
	text := firstText(doc.GetXmlFragment(fragmentName))
	if text == nil {
		t.Fatal("the long-edited document holds no text")
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
		t.Fatalf("the long-edited document typed on by %d characters decodes to %d structs, want within %d of the %d cap", typed, structs, smallestReplacementAdds, maxUpdateItems)
	}
	atCapStates.longEdited = state
	return state
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

// A document whose history loads but stands at the decode cap can be changed by no one: a write
// that trims it is refused by the decode cap (refuseUndecodable), and a browser's edit by the
// room's struct total. A rebuild takes it, from its own text: the two documents a room keeps at
// the cap - one an agent rewrote whole through the API until the cap refused a write, and the
// long-edited 5 KiB document typed on until its state reached the cap - each rebuild to a small
// struct total, read the same text, and take the trimming write that was refused.
func TestRebuildingADocumentAtTheDecodeCapLeavesItSmallAndReadingTheSame(t *testing.T) {
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, row := range []struct {
		name  string
		state func(t *testing.T) []byte
	}{
		{"an agent's whole rewrites until the cap refused one", func(t *testing.T) []byte {
			state, refusal := agentRewrittenState(t)
			if !errors.Is(refusal, ErrDocumentTooLarge) {
				t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
			}
			return state
		}},
		{"a long-edited document typed on until the cap", longEditedAtCapState},
	} {
		t.Run(row.name, func(t *testing.T) {
			state := row.state(t)
			service, artifactID := newTestService(t)
			storeHistory(t, service, artifactID, state)
			ctx := context.Background()
			before, err := service.Text(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			structs, _, _ := structsAndRendering(t, state)
			trimmed := before[:strings.LastIndex(strings.TrimRight(before, "\n"), "\n")+1]
			if _, err := joinedReplaceText(service, artifactID, trimmed, agent); !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "items ygo decodes") {
				t.Fatalf("a write trimming the %d-struct document was answered %v, want the decode cap's refusal", structs, err)
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
			if rebuilt > maxUpdateItems/16 {
				t.Fatalf("the rebuilt document holds %d structs, want a small fraction of the %d cap", rebuilt, maxUpdateItems)
			}
			if _, err := joinedReplaceText(service, artifactID, trimmed, agent); err != nil {
				t.Fatalf("the trimming write after the rebuild: %v", err)
			}
		})
	}
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

// A browser that held a document before its rebuild still holds the history the rebuild replaced,
// and the edits it made while disconnected. When it reconnects, the rebuilt room holds none of
// that history, so the browser's answer to the room's sync hands all of it back with its unsent
// edit: the room refuses it with the document's own close, on which the editor drops what it sent
// and builds its document afresh from the room, and drops its saved rows typed against the old
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
	docstest.Transact(held.Doc, func(txn *crdt.Transaction) {
		firstText(held.Doc.GetXmlFragment(fragmentName)).Insert(txn, 0, "unsent ", nil)
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
	if closed := reconnected.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the room ended the browser holding the replaced history with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
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
	if _, err := fresh.Send(func(txn *crdt.Transaction) {
		firstText(fresh.Doc.GetXmlFragment(fragmentName)).Insert(txn, 0, "fresh ", nil)
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
