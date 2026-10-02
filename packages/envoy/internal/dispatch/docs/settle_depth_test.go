package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// A crafted client can write an over-deep tree through the room's CRDT without passing through
// pmdoc.Update. Settlement skips that tree as it does every tree outside the schema: it writes no
// version and does not fail the live room.
func TestSettlementSkipsATreeOverTheDepthBound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			writeOverDeepCRDTTree(txn, fragment)
		})
	}); err != nil {
		t.Fatalf("write crafted CRDT tree: %v", err)
	}

	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("settlement wrote %d versions for an over-deep document, want 1", versions)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("read over-deep document: %v, want ErrDocSchema", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.failed != nil {
		t.Fatalf("settlement failed the live room: %v", state.failed)
	}
	if state.settleFailures != 0 {
		t.Fatalf("settlement recorded %d failures for an over-deep document", state.settleFailures)
	}
}

// writeOverDeepCRDTTree writes a tree one level deeper than pmdoc's bound: its text stands 1,001
// levels below the document, under 999 blockquotes and a paragraph.
func writeOverDeepCRDTTree(txn *crdt.Transaction, fragment *crdt.YXmlFragment) {
	parent := crdt.NewYXmlElement("blockquote")
	fragment.InsertElement(txn, 0, parent)
	for range 998 {
		child := crdt.NewYXmlElement("blockquote")
		parent.InsertElement(txn, 0, child)
		parent = child
	}
	paragraph := crdt.NewYXmlElement("paragraph")
	parent.InsertElement(txn, 0, paragraph)
	text := crdt.NewYXmlText()
	paragraph.InsertText(txn, 0, text)
	text.Insert(txn, 0, "a", nil)
}
