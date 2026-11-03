package pmdoc

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The two bodies that froze a reader's tab in the browser engine (INFRA-581) are refused by a
// document write in a few milliseconds: Go reads a bare `:::a` as a malformed directive, whatever
// it is nested in. Each refusal is held to 1 s of this process's CPU time (cpuTimeOf), hundreds of
// times what it takes, so neither a loaded runner nor -race flakes it.
func TestParseForWriteRefusesDeeplyNestedBareDirectivesQuickly(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{name: "2,000 unclosed openers then a long line", markdown: strings.Repeat(":::a\n", 2000) + strings.Repeat("x", 21000)},
		{name: "1,000 openers then 1,000 closing lines", markdown: strings.Repeat(":::a\n", 1000) + strings.Repeat(":::\n", 1000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			took, timing := cpuTimeOf(again(func() { _, err = ParseForWrite(test.markdown, nil) }))
			if timing != nil {
				t.Fatal(timing)
			}
			if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), malformedDirectiveReason) {
				t.Fatalf("ParseForWrite: %v, want ErrSchema saying %q", err, malformedDirectiveReason)
			}
			if took >= time.Second {
				t.Fatalf("ParseForWrite took %s of CPU to refuse it, want under 1 s", took)
			}
		})
	}
}

// A document write reads 32 typed blocks nested inside one another. The blocks are closed with
// fences that shorten inwards (nestedCallouts), since a closing fence closes the outermost block
// it satisfies.
func TestParseForWriteReads32NestedTypedBlocks(t *testing.T) {
	doc, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting), nil)
	if err != nil {
		t.Fatalf("%d nested callouts: %v, want them read", expectedMaxTypedNesting, err)
	}
	if depth := typedDepth(doc); depth != expectedMaxTypedNesting {
		t.Fatalf("%d nested callouts read %d deep, want %d", expectedMaxTypedNesting, depth, expectedMaxTypedNesting)
	}
}

// A document write refuses a 33rd typed block nested inside 32, at its opening line, as the
// browser's default mode does; the read-only engine draws that opener as text.
func TestParseForWriteRefusesA33rdNestedTypedBlock(t *testing.T) {
	_, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting+1), nil)
	refusal := fmt.Sprintf("line %d opens a typed block inside %d typed blocks; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting, expectedMaxTypedNesting)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("%d nested callouts: %v, want ErrSchema saying %q", expectedMaxTypedNesting+1, err, refusal)
	}
}

// The corpus's 33 nested typed blocks are refused by every reader of caller markdown at the 33rd
// opener's line.
func TestCorpusOf33NestedTypedBlocksIsRefused(t *testing.T) {
	markdown, err := os.ReadFile("testdata/corpus-errors/typed-blocks-nested-33-deep.md")
	if err != nil {
		t.Fatalf("read 33 nested typed blocks corpus: %v", err)
	}
	refusal := fmt.Sprintf("line %d opens a typed block inside %d typed blocks; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting, expectedMaxTypedNesting)
	for _, reader := range blockMarkdownReaders() {
		if _, err := reader.parse(string(markdown)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s of 33 nested typed blocks: %v, want ErrSchema saying %q", reader.name, err, refusal)
		}
	}
}

// A tree whose typed blocks nest one past the bound, as a write can leave one by landing a typed
// block inside 32 others, is refused on read-back for how deeply its typed blocks nest, not for a
// line of a rendering nobody wrote.
func TestReadBackRefusalNamesTheTypedBlockNesting(t *testing.T) {
	doc, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting), nil)
	if err != nil {
		t.Fatalf("%d nested callouts: %v", expectedMaxTypedNesting, err)
	}
	block := &Node{Type: "callout", Attrs: Attrs{"blockId": "outer", "kind": "note", "title": "T"}, Children: doc.Children}
	refusal := fmt.Sprintf("the result nests %d typed blocks inside one another; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting)
	back := ReadBackBlock(block)
	for name, verdict := range map[string]error{"Read": back.Read, "Shape": back.Shape} {
		if !errors.Is(verdict, ErrSchema) || !strings.Contains(verdict.Error(), refusal) {
			t.Errorf("the read-back's %s of %d nested callouts: %v, want ErrSchema saying %q", name, expectedMaxTypedNesting+1, verdict, refusal)
		}
	}
}

// typedDepth is how many callouts stand inside one another on node's deepest path.
func typedDepth(node *Node) int {
	deepest := 0
	for _, child := range node.Children {
		deepest = max(deepest, typedDepth(child))
	}
	if node.Type == "callout" {
		return deepest + 1
	}
	return deepest
}
