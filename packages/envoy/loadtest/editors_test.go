package main

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygows "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// seededDocument is a document holding a seeded spec, written as Dispatch writes a document it
// creates from markdown (internal/dispatch/docs encodeDocumentTree): the parsed tree through
// pmdoc.Update into the prosemirror fragment, with the marks map, in one transaction.
func seededDocument(t *testing.T) *crdt.Doc {
	t.Helper()
	markdown, _ := specMarkdown(rand.New(rand.NewPCG(1, 1)), "LGA-0001", 4000, false)
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the seeded spec: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment(proofFragment)
	doc.GetMap("marks")
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatalf("write the seeded spec: %v", err)
	}
	return doc
}

// render reads doc's tree and renders it as Dispatch does, failing the test when either refuses
// it: a tree outside the Proof schema.
func render(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	tree, err := pmdoc.Read(doc.GetXmlFragment(proofFragment))
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return markdown
}

func TestEditParagraphStaysInsideTheProofSchema(t *testing.T) {
	doc := seededDocument(t)
	r := rand.New(rand.NewPCG(7, 7))
	for i := range 300 {
		text := " " + words[r.IntN(len(words))]
		if i%editorMarkerEvery == 0 {
			text = " " + editorMarker(int64(i))
		}
		if editParagraph(doc, r, text) == nil {
			t.Fatalf("edit %d made no update", i)
		}
	}
	markdown := render(t, doc)
	if !strings.Contains(markdown, "\n\nRevision token r0\n\n") {
		t.Fatalf("the edits changed the revision token line:\n%s", markdown)
	}
	if !strings.Contains(markdown, editorMarker(250)) {
		t.Fatalf("the document lacks the marker %s whole:\n%s", editorMarker(250), markdown)
	}
	for _, block := range strings.Split(markdown, "\n\n") {
		if block != strings.TrimSpace(block) && !strings.HasPrefix(block, "|") {
			t.Fatalf("an edit left a paragraph that starts or ends with a space: %q", block)
		}
	}
	// The same edits applied to a copy that only received the updates render the same.
	replica := crdt.New()
	if err := crdt.ApplyUpdateV1(replica, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatalf("apply the edited document to a replica: %v", err)
	}
	if got := render(t, replica); got != markdown {
		t.Fatal("a replica of the edited document renders differently")
	}
}

func TestEditorUpdateAppliesOnARoomAndRelays(t *testing.T) {
	room := "0b6a2c8e-7f4d-4c1a-9e7b-3d2f1a0c9b8e"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	seeded := crdt.EncodeStateAsUpdateV1(seededDocument(t), nil)
	// A room as Dispatch's document server runs one: ygo's websocket server on Hocuspocus
	// framing, which loads the document from its stored state and admits a token equal to the
	// server's schema version read-write.
	srv := ygows.NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(_, token string) (ygows.ConnectionConfig, error) {
		return ygows.ConnectionConfig{ReadOnly: token != schema}, nil
	}
	srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
		return crdt.ApplyUpdateV1(doc, seeded, nil)
	}
	mux := http.NewServeMux()
	mux.Handle("/ws/doc/{room}", srv)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + schema
	d := &editorDoc{key: "LGA-0001", artifactID: room, editors: 2}
	stats := newEditorStats()
	ctx := context.Background()
	a, err := dialEditor(ctx, url, "person-0", schema, d, stats)
	if err != nil {
		t.Fatalf("connect editor a: %v", err)
	}
	t.Cleanup(a.close)
	b, err := dialEditor(ctx, url, "person-1", schema, d, stats)
	if err != nil {
		t.Fatalf("connect editor b: %v", err)
	}
	t.Cleanup(b.close)
	waitUntil(t, "both editors to sync", func() bool { return a.synced.Load() && b.synced.Load() })

	marker := editorMarker(1)
	var sentAt time.Time
	if err := a.send(rand.New(rand.NewPCG(3, 3)), " "+marker, true, func(at time.Time) { sentAt = at }); err != nil {
		t.Fatalf("send the edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the edit and b to receive it", func() bool {
		stats.mu.Lock()
		defer stats.mu.Unlock()
		return stats.acked == 1 && len(stats.relayMs) == 1
	})
	stats.mu.Lock()
	refused, readOnly, closes, relay := stats.refused, stats.readOnly, len(stats.closes), stats.relayMs[0]
	stats.mu.Unlock()
	if refused != 0 || readOnly != 0 || closes != 0 {
		t.Fatalf("refused %d, read-only %d, closes %d; want none", refused, readOnly, closes)
	}
	if relay < 0 || relay > float64(time.Since(sentAt))/1e6 {
		t.Fatalf("relay latency %.3f ms is outside the send-to-now interval", relay)
	}
	roomDoc := srv.GetDoc(room)
	if roomDoc == nil {
		t.Fatal("the room is not resident while its editors are connected")
	}
	markdown := render(t, roomDoc)
	if !strings.Contains(markdown, marker) {
		t.Fatalf("the room's document lacks the marker:\n%s", markdown)
	}
	if got := render(t, b.ydoc); got != markdown {
		t.Fatal("editor b's copy renders differently from the room's")
	}
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
