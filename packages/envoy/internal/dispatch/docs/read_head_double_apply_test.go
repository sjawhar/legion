package docs

import (
	"maps"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A read of a room behind the stored head is the stored document with that room's unstored share
// applied onto a fresh copy of it (storedWithUpdates). The two overlap: the share is taken from
// the room's update log before the read loads the store (unstoredSnapshot), and the room's own
// appends commit while that load runs, so the copy can already hold some of the updates the share
// carries. Applying a committed update onto a document that already holds it changes nothing, so
// the read answers the same document either way.
//
// ygo makes that a no-op. A struct whose end is at or below the clock the document already holds
// for its client is skipped rather than integrated again (sjawhar/ygo v1.55.1-sami.2,
// crdt/update.go:914-924, integrateGroup), and an item the document has already deleted is passed
// over by the delete cascade (crdt/item.go:505-510, Item.delete). The test guards the first skip
// alone: with update.go's skip defeated the second apply fails during integration, while with
// item.go's skip defeated it still passes, so it does not show that the delete-side skip is needed.
//
// The test applies one committed update - inserts, a deletion and a mark - once and twice onto
// copies of the same stored document and compares the rendered document and the state vector.
// That the comparison can fail is shown by the control: the same edit made on a second fork, by a
// client the stored document knows nothing of, is new content, so it renders differently. Were a
// second apply to duplicate what the first integrated, it would read like that control.
func TestApplyingACommittedUpdateTwiceOntoAStoredDocumentChangesNothing(t *testing.T) {
	stored := documentOf(t, "First.\n\nSecond.\n\nThird.\n")
	storedState := crdt.EncodeStateAsUpdateV1(stored, nil)

	// A browser's edit on a fork of the stored document: it deletes a paragraph, adds another and
	// marks a run, so the committed update carries insertions, deletions and formatting.
	const edited = "First.\n\n**Second**, edited.\n\nFourth.\n"
	fork := copyOf(t, storedState)
	before := fork.StateVector()
	writeMarkdown(t, fork, edited)
	committed := crdt.EncodeStateAsUpdateV1(fork, before)

	once := copyOf(t, storedState)
	applyCommitted(t, once, committed)
	twice := copyOf(t, storedState)
	applyCommitted(t, twice, committed)
	applyCommitted(t, twice, committed)

	onceRendered, twiceRendered := renderedBy(t, once), renderedBy(t, twice)
	if onceRendered != edited {
		t.Fatalf("one apply of the committed update rendered %q, want %q", onceRendered, edited)
	}
	if twiceRendered != onceRendered {
		t.Errorf("two applies rendered %q, one %q", twiceRendered, onceRendered)
	}
	if onceVector, twiceVector := once.StateVector(), twice.StateVector(); !maps.Equal(onceVector, twiceVector) {
		t.Errorf("two applies leave the state vector %v, one leaves %v", twiceVector, onceVector)
	}

	// The control: the same edit from a client the stored document knows nothing of is content of
	// its own, which the rendering shows. So the comparison above is one duplicated content fails.
	other := copyOf(t, storedState)
	otherBefore := other.StateVector()
	writeMarkdown(t, other, edited)
	control := copyOf(t, storedState)
	applyCommitted(t, control, committed)
	applyCommitted(t, control, crdt.EncodeStateAsUpdateV1(other, otherBefore))
	if rendered := renderedBy(t, control); rendered == onceRendered {
		t.Errorf("a second client's copy of the same edit rendered %q, the same as one apply of the committed update: this comparison would not catch duplicated content", rendered)
	}
}

// copyOf is a fresh document holding state, as storedWithUpdates builds the copy it merges into.
func copyOf(t *testing.T, state []byte) *crdt.Doc {
	t.Helper()
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("copy the stored document: %v", err)
	}
	return doc
}

// writeMarkdown writes markdown into doc's fragment in one transaction, as a browser's keystroke
// does.
func writeMarkdown(t *testing.T, doc *crdt.Doc, markdown string) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	fragment := doc.GetXmlFragment(fragmentName)
	var writeErr error
	doc.Transact(func(txn *crdt.Transaction) { writeErr = pmdoc.Update(txn, fragment, tree) })
	if writeErr != nil {
		t.Fatalf("write %q: %v", markdown, writeErr)
	}
}

// applyCommitted applies one committed update to doc, as storedWithUpdates applies a room's share.
func applyCommitted(t *testing.T, doc *crdt.Doc, update []byte) {
	t.Helper()
	if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatalf("apply a committed update: %v", err)
	}
}

func renderedBy(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	rendered, err := renderDocument(doc)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return rendered
}
