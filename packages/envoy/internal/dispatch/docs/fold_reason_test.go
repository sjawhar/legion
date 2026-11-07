package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A stored log whose later row does not decode is refused naming the decoder's refusal once, as
// every other decode of stored state refuses it (decodeStoredState): the fold a room's load, a cold
// read and a compaction make of a document's rows reports such a row as an invalid update carrying
// ygo's own refusal of it, whatever reason ygo gives, stated once. The compaction and the load log
// that refusal (recordUnfoldable, "serving them merged"), so the error must be the line an
// operator can act on.
func TestAStoredLogThatDoesNotFoldIsRefusedNamingItsReasonOnce(t *testing.T) {
	t.Parallel()
	writer := newProofHistory(t, 101)
	first := writer.write("A first paragraph.\n")
	second := writer.write("A first paragraph.\n\nA second paragraph, longer than the first.\n")
	cut := second[:8]
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatalf("apply the first row: %v", err)
	}
	refused := crdt.ApplyUpdateV1(doc, cut, nil)
	if refused == nil {
		t.Fatal("the cut second row decoded, so nothing here is tested")
	}

	_, _, misread, err := fold(memoryUpdates(context.Background(), [][]byte{first, cut}), maxUpdateItems)
	if err != nil {
		t.Fatalf("fold the stored rows: %v", err)
	}
	if misread == nil {
		t.Fatal("the fold of a log whose second row does not decode reported no misread")
	}
	want := errUnfoldable.Error() + ": " + refused.Error()
	if misread.Error() != want || strings.Count(misread.Error(), crdt.ErrInvalidUpdate.Error()) != 1 || !errors.Is(misread, crdt.ErrInvalidUpdate) {
		t.Fatalf("the fold's refusal = %q, want %q, an invalid update stated once", misread, want)
	}
}
