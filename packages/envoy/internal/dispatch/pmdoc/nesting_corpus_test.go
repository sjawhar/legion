package pmdoc

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The block bound is pinned by a pair of corpus documents for each chain that reaches it: one in
// testdata/corpus, read and stored, and one in testdata/corpus-errors, a level deeper and refused
// by every reader of caller markdown at the line of the opener past the bound. A chain of list
// markers on one line opens a list and its item at each marker, so 50 of them stand at the bound
// and 51 pass it; a chain of quote markers opens one block each, so 100 stand at it and 101 pass
// it. The browser editor's parser reads the opener past the bound as the text it is instead, which
// is what keeps the two readings of a deep chain the same (maxBlockNesting, proof-editor's
// block-nesting.ts, sjawhar/legion#2403).
func TestCorpusChainsStandAtTheBlockBoundAndOnePastItIsRefused(t *testing.T) {
	for _, test := range []struct{ read, refused string }{
		{read: "list-items-chained-50-deep", refused: "list-items-chained-51-deep"},
		{read: "quotes-nested-100-deep", refused: "quotes-nested-101-deep"},
	} {
		t.Run(test.read, func(t *testing.T) {
			deepest := corpusDocument(t, "corpus", test.read)
			over := corpusDocument(t, "corpus-errors", test.refused)
			refusal := fmt.Sprintf("line 1 opens a block inside %d blocks", expectedMaxNesting)
			for _, reader := range blockMarkdownReaders() {
				if _, err := reader.parse(deepest); err != nil {
					t.Errorf("%s of %s: %v, want it read", reader.name, test.read, err)
				}
				_, err := reader.parse(over)
				if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
					t.Errorf("%s of %s: %v, want ErrSchema saying %q", reader.name, test.refused, err, refusal)
				}
			}
		})
	}
}

// corpusDocument is the markdown of the named document in the named testdata directory.
func corpusDocument(t *testing.T, dir, name string) string {
	t.Helper()
	markdown, err := os.ReadFile("testdata/" + dir + "/" + name + ".md")
	if err != nil {
		t.Fatalf("read %s/%s: %v", dir, name, err)
	}
	return string(markdown)
}
