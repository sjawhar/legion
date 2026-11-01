package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A stored log whose later row does not decode is refused naming the decoder's reason once, as every
// other decode of stored state refuses it (decodeStoredState): the fold a room's load, a cold read
// and a compaction make of a document's rows reports such a row as an invalid update, never as
// ygo's doubled "crdt: invalid update: crdt: invalid update". The compaction and the load log that
// refusal (recordUnfoldable, "serving them merged"), so the doubled text would be the line an operator
// reads for a history that does not fold.
func TestAStoredLogThatDoesNotFoldIsRefusedNamingItsReasonOnce(t *testing.T) {
	writer := newProofHistory(t, 101)
	first := writer.write("A first paragraph.\n")
	second := writer.write("A first paragraph.\n\nA second paragraph, longer than the first.\n")
	// A cut of the second row that ygo's apply refuses with no reason of its own, onto a document
	// that holds the first row, as the fold applies it.
	var cut []byte
	for length := len(second) - 1; length > 0 && cut == nil; length-- {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, first, nil); err != nil {
			t.Fatalf("apply the first row: %v", err)
		}
		if err := crdt.ApplyUpdateV1(doc, second[:length], nil); err != nil && err.Error() == doubledInvalidUpdate {
			cut = second[:length]
		}
	}
	if cut == nil {
		t.Fatal("no cut of the second row is refused with no reason by ygo's apply, so nothing here is tested")
	}

	_, _, misread, err := fold(memoryUpdates(context.Background(), [][]byte{first, cut}), maxUpdateItems)
	if err != nil {
		t.Fatalf("fold the stored rows: %v", err)
	}
	if misread == nil {
		t.Fatal("the fold of a log whose second row does not decode reported no misread")
	}
	if strings.Contains(misread.Error(), doubledInvalidUpdate) || !strings.HasSuffix(misread.Error(), ": crdt: invalid update") || !errors.Is(misread, crdt.ErrInvalidUpdate) {
		t.Fatalf("the fold's refusal = %q, want an invalid update ending \": crdt: invalid update\", stated once", misread)
	}
}
