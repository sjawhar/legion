package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// proofHistory writes a document's stored history as a room stores one: each write is the update
// that takes its document from what it held to markdown's tree.
type proofHistory struct {
	t   *testing.T
	doc *crdt.Doc
}

// newProofHistory is a writer of client's updates, starting from the updates in from.
func newProofHistory(t *testing.T, client crdt.ClientID, from ...[]byte) *proofHistory {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(client))
	for _, update := range from {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("open the writer: %v", err)
		}
	}
	return &proofHistory{t: t, doc: doc}
}

func (h *proofHistory) write(markdown string) []byte {
	h.t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		h.t.Fatalf("parse %q: %v", markdown, err)
	}
	before := h.doc.StateVector()
	fragment := h.doc.GetXmlFragment(fragmentName)
	if err := h.doc.TransactE(func(txn *crdt.Transaction) error {
		return pmdoc.Update(txn, fragment, tree)
	}); err != nil {
		h.t.Fatalf("write %q: %v", markdown, err)
	}
	return crdt.EncodeStateAsUpdateV1(h.doc, before)
}

func appendUpdates(t *testing.T, versioned *PgVersioned, artifactID string, updates ...[]byte) {
	t.Helper()
	for index, update := range updates {
		if _, err := versioned.AppendUpdate(context.Background(), artifactID, update); err != nil {
			t.Fatalf("append update %d: %v", index, err)
		}
	}
}

func readStamp(t *testing.T, versioned *PgVersioned, artifactID string) DocumentStamp {
	t.Helper()
	stamp, err := versioned.DocumentStamp(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document's stamp: %v", err)
	}
	return stamp
}

func loadDocument(t *testing.T, versioned *PgVersioned, artifactID string) LoadedDocument {
	t.Helper()
	loaded, err := versioned.LoadDocument(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the document: %v", err)
	}
	return loaded
}

// docMarkdown is what doc renders, or why it does not.
func docMarkdown(doc *crdt.Doc) string {
	markdown, err := renderDocument(doc)
	if err != nil {
		return "unrenderable: " + err.Error()
	}
	return markdown
}

// A prune deletes the updates past its target, and the next append takes the number the first of
// them had, with other content. The stamp names the row at the head, not only its number, so the
// stamp after the prune and the append differs from the one before them, and the load shows the
// new content under it.
func TestAStampChangesWhenAPruneAndAnAppendRepeatAVersionNumber(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	artifactID := createDocument(t, database, "one")
	if empty := readStamp(t, versioned, artifactID); empty != (DocumentStamp{}) {
		t.Fatalf("a document with no stored update has the stamp %+v, want none", empty)
	}
	writer := newProofHistory(t, 101)
	one, two, three := writer.write("one\n"), writer.write("two\n"), writer.write("three\n")
	appendUpdates(t, versioned, artifactID, one, two, three)
	before := readStamp(t, versioned, artifactID)
	if before.Version != 3 || before.Row == "" {
		t.Fatalf("stamp after three updates = %+v, want version 3 and its row", before)
	}
	if err := versioned.PruneAfter(ctx, artifactID, 2, nil); err != nil {
		t.Fatalf("prune to version 2: %v", err)
	}
	if pruned := readStamp(t, versioned, artifactID); pruned.Version != 2 || pruned.Row == "" {
		t.Fatalf("stamp after the prune = %+v, want version 2 and its row", pruned)
	}
	appendUpdates(t, versioned, artifactID, newProofHistory(t, 202, one, two).write("four\n"))
	after := readStamp(t, versioned, artifactID)
	if after.Version != 3 || after == before {
		t.Fatalf("stamp after the prune and an append = %+v, want version 3 under another row than %+v", after, before)
	}
	loaded := loadDocument(t, versioned, artifactID)
	if got := docMarkdown(loaded.Doc); got != "four\n" || loaded.Stamp != after {
		t.Fatalf("load = %q under %+v, want %q under %+v", got, loaded.Stamp, "four\n", after)
	}
}

// Compaction folds the log into the head's row, which it rewrites: the stamp moves while the
// version and the document stay as they were.
func TestACompactionChangesTheStampAndKeepsTheDocument(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	artifactID := createDocument(t, database, "one")
	writer := newProofHistory(t, 101)
	appendUpdates(t, versioned, artifactID, writer.write("one\n"), writer.write("two\n\nmore\n"), writer.write("three\n"))
	before := readStamp(t, versioned, artifactID)
	loaded := loadDocument(t, versioned, artifactID)
	want, wantMarkdown := rawReading(t, loaded.Doc), docMarkdown(loaded.Doc)
	if loaded.Stamp != before || wantMarkdown != "three\n" {
		t.Fatalf("load before compaction = %q under %+v, want three under %+v", wantMarkdown, loaded.Stamp, before)
	}
	if deleted, err := versioned.Compact(context.Background(), artifactID, compactKeep); err != nil || deleted != 2 {
		t.Fatalf("compact: deleted %d (%v), want 2", deleted, err)
	}
	after := readStamp(t, versioned, artifactID)
	if after.Version != before.Version || after.Row == before.Row {
		t.Fatalf("stamp after compaction = %+v, want version %d under another row than %q", after, before.Version, before.Row)
	}
	compacted := loadDocument(t, versioned, artifactID)
	if got := rawReading(t, compacted.Doc); got != want || docMarkdown(compacted.Doc) != wantMarkdown || compacted.Stamp != after {
		t.Fatalf("load after compaction = %s under %+v, want %s under %+v", got, compacted.Stamp, want, after)
	}
}

// LoadDocument hands out the document the fold's read-back decoded, or, for a log of one update,
// the update decoded once: the same document a load's state decodes into, for a log of many
// updates (each pmdoc fixture rewritten into the next and back), a log of one, and a log whose
// fold parks an update and a delete, which the loaded document parks again.
func TestALoadedDocumentIsTheLoadedStateDecoded(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	sameAsLoad := func(t *testing.T, artifactID string) *crdt.Doc {
		t.Helper()
		loaded := loadDocument(t, versioned, artifactID)
		state, err := versioned.Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("load the state: %v", err)
		}
		decoded := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(decoded, state.Update, nil); err != nil {
			t.Fatalf("decode the loaded state: %v", err)
		}
		if loaded.Stamp.Version != int64(state.Version) {
			t.Fatalf("loaded under version %d, the state is at %d", loaded.Stamp.Version, state.Version)
		}
		for what, pair := range map[string][2]string{
			"tree":     {rawReading(t, loaded.Doc), rawReading(t, decoded)},
			"markdown": {docMarkdown(loaded.Doc), docMarkdown(decoded)},
			"text":     {loaded.Doc.GetText("t").ToString(), decoded.GetText("t").ToString()},
			"pending":  {fmt.Sprint(loaded.Doc.PendingStats()), fmt.Sprint(decoded.PendingStats())},
		} {
			if pair[0] != pair[1] {
				t.Fatalf("the loaded document's %s is\n%s\nthe decoded state's\n%s", what, pair[0], pair[1])
			}
		}
		if !maps.Equal(loaded.Doc.StateVector(), decoded.StateVector()) {
			t.Fatalf("the loaded document's state vector is %v, the decoded state's %v", loaded.Doc.StateVector(), decoded.StateVector())
		}
		return loaded.Doc
	}

	fixtures := readHistoryFixtures(t)
	for index, fixture := range fixtures {
		t.Run("many updates/"+fixture.Name, func(t *testing.T) {
			artifactID := createDocument(t, database, "history")
			appendUpdates(t, versioned, artifactID, documentHistory(t, fixture.update, fixtures[(index+1)%len(fixtures)].update)...)
			sameAsLoad(t, artifactID)
		})
	}
	t.Run("one update", func(t *testing.T) {
		artifactID := createDocument(t, database, "history")
		appendUpdates(t, versioned, artifactID, fixtures[0].update)
		sameAsLoad(t, artifactID)
	})
	t.Run("parked updates", func(t *testing.T) {
		writer := crdt.New(crdt.WithClientID(7))
		text := writer.GetText("t")
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "base", nil) })
		base := crdt.EncodeStateAsUpdateV1(writer, nil)
		afterBase := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 4, " lost", nil) })
		lost := crdt.EncodeStateAsUpdateV1(writer, afterBase)
		afterLost := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 9, " kept", nil) })
		parkedInsert := crdt.EncodeStateAsUpdateV1(writer, afterLost)
		other := crdt.New(crdt.WithClientID(8))
		if err := crdt.ApplyUpdateV1(other, base, nil); err != nil {
			t.Fatalf("open the second writer: %v", err)
		}
		otherText := other.GetText("t")
		other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "gone ", nil) })
		gone := crdt.EncodeStateAsUpdateV1(other, afterBase)
		afterGone := other.StateVector()
		other.Transact(func(txn *crdt.Transaction) { otherText.Delete(txn, 0, 5) })
		parkedDelete := crdt.EncodeStateAsUpdateV1(other, afterGone)

		artifactID := createDocument(t, database, "parked")
		appendUpdates(t, versioned, artifactID, base, parkedInsert, parkedDelete)
		doc := sameAsLoad(t, artifactID)
		if parked := doc.PendingStats(); parked.Items == 0 && parked.DeleteRanges == 0 {
			t.Fatal("the loaded document parks nothing, want the insert and the delete whose dependencies never arrived")
		}
		for _, update := range [][]byte{lost, gone} {
			if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatalf("apply a missing update: %v", err)
			}
		}
		if got := doc.GetText("t").ToString(); got != "base lost kept" {
			t.Fatalf("the loaded document, once the missing updates arrive, reads %q, want %q", got, "base lost kept")
		}
	})
}

// A history whose updates merge past ygo's item cap does not decode: LoadDocument names it as the
// one state a rebuild repairs (ErrDocumentUnloadable), and so does a read of it, not wrapped in
// the unavailable service any failed room or store answers.
func TestAnUndecodableHistoryLoadsAsDocumentUnloadable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	versioned := NewPgVersioned(database)
	appendUpdates(t, versioned, artifactID, oversizedHistory(t)...)
	if _, err := versioned.LoadDocument(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) {
		t.Fatalf("load an over-cap history: %v, want ErrDocumentUnloadable", err)
	}
	service := newSchemaRepairService(database)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("read an over-cap history: %v, want ErrDocumentUnloadable and not ErrServiceUnavailable", err)
	}
}
