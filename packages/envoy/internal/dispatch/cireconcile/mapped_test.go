package cireconcile

import (
	"reflect"
	"testing"
)

// unrenamedRow is a mapping that was never renamed and sorts after the renamed pair, so a read
// that answered one repository for the whole table would drop it rather than the pair.
const unrenamedRow = "zed/other"

// Every mapped repository is read, whatever any other mapping was renamed to: a pass over a
// renamed pair and a mapping of its own reads two repositories, the pair contributing the name
// it has today once. A rename is one mapping's fact, so a reader that let one mapping's
// renamed_to stand for every row would leave every repository but that one out of the pass —
// no head read, no record mended, no settlement — for as long as any rename is recorded.
func TestEveryMappingIsReadWhenAnotherNamesARename(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.mapRepository(unrenamedRow)

	repositories, err := h.reconciler.mapped(h.ctx)
	if err != nil {
		t.Fatalf("read the mapped repositories: %v", err)
	}

	want := []string{rowRepository, unrenamedRow}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("the pass reads %v, want %v: each mapped repository under the name it has now, once", repositories, want)
	}
}
