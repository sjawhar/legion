package docs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write's refusals all read the document-level blocks it changed. The write now finds those
// blocks once and passes the answer on (matchedBlocks), where each refusal found them for itself,
// so these tests hold the one walk to the two lookups it replaced and the refusals to the words
// they are given in. Every expected string here is what this package answered before the change;
// the file runs unchanged at the base revision.

// TestMatchedBlocksAnswersTwoBlockIndexLookups holds the one walk to pmdoc.BlockIndex, the lookup
// it replaced, for every document of the corpus at every position in it: the first block holding
// the range's start, the last holding its end, and a refusal when either endpoint lies in none.
func TestMatchedBlocksAnswersTwoBlockIndexLookups(t *testing.T) {
	for _, document := range corpusTrees(t) {
		t.Run(document.name, func(t *testing.T) {
			tree := document.tree
			valid, err := pmdoc.ValidateDocument(tree, "BlockIndex")
			if err != nil {
				t.Fatal(err)
			}
			size := pmdoc.Size(tree)
			probes := []pmdoc.Range{{From: 0, To: size}, {From: size + 5, To: size + 9}}
			for from := range size + 3 {
				probes = append(probes, pmdoc.Range{From: from, To: from}, pmdoc.Range{From: from, To: from + 3})
			}
			for _, probe := range probes {
				wantFirst, firstErr := pmdoc.BlockIndex(tree, pmdoc.Range{From: probe.From, To: probe.From})
				wantLast, lastErr := pmdoc.BlockIndex(tree, pmdoc.Range{From: probe.To, To: probe.To})
				found, err := matchedBlocks(valid, probe)
				if firstErr != nil || lastErr != nil {
					if err == nil {
						t.Fatalf("matchedBlocks(%v) = %d..%d, want a refusal", probe, found.first, found.last)
					}
					if !errors.Is(err, pmdoc.ErrTargetNotFound) {
						t.Fatalf("matchedBlocks(%v) error = %v, want ErrTargetNotFound", probe, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("matchedBlocks(%v) error = %v, want blocks %d..%d", probe, err, wantFirst, wantLast)
				}
				if found.first != wantFirst || found.last != wantLast {
					t.Fatalf("matchedBlocks(%v) = %d..%d, want %d..%d", probe, found.first, found.last, wantFirst, wantLast)
				}
				// lastAfter counts the blocks the write added, as it did when every refusal
				// computed it from the tree it held.
				for _, gained := range []int{-2, -1, 0, 1, 5} {
					blocks := len(tree.Children) + gained
					if blocks < 0 {
						continue
					}
					after := &pmdoc.Node{Type: "doc", Children: make([]*pmdoc.Node, blocks)}
					want := max(found.first, found.last+blocks-len(tree.Children))
					if got := found.span(after).lastAfter; got != want {
						t.Fatalf("span of %d blocks has lastAfter %d, want %d", blocks, got, want)
					}
				}
			}
		})
	}
}

// TestEditRefusalsKeepTheirWords pins the refusals that read a write's changed blocks: each case
// trips one, and the whole message is the one this package gives. A write whose changed blocks
// were found over the wrong tree, or over the wrong range of it, waves one of these through or
// names another block.
func TestEditRefusalsKeepTheirWords(t *testing.T) {
	for _, test := range []struct {
		name     string
		document string
		tableID  string
		op       model.EditOp
		field    string
		reason   string
	}{
		{
			name:     "html that opens a block where it lands",
			document: "Intro.\n\nBody.\n",
			op:       model.EditOp{Op: "replace", Find: "Body.", With: "<div>x</div>"},
			field:    "with",
			reason:   `with "<div>x</div>" is HTML that opens a block where it lands, and the Proof schema carries no block HTML; keep the HTML inside a line, where it opens no block`,
		},
		{
			name:     "a line break that reshapes the block it lands in",
			document: "# Body.\n",
			op:       model.EditOp{Op: "replace", Find: "Body.", With: "a `x\ny` b"},
			field:    "with",
			reason:   "with \"a `x\\ny` b\" is text the document reads back as another block where it lands (the document's end reads back as a paragraph); write it inside a line of text",
		},
		{
			name:     "code holding a pipe in a table cell",
			document: "| h |\n| --- |\n| Body. |\n",
			tableID:  "table-1",
			op:       model.EditOp{Op: "replace", Find: "Body.", With: "`a\\|b`"},
			field:    "with",
			reason:   "with \"`a\\\\|b`\" leaves code the document cannot store where it lands: table \"table-1\" holds code in row 1, column 0 (under the header \"h\") whose text has a | right after a backslash, \"a\\|b\", which a table cell's markdown cannot write: in a cell a | is written \\|, in code too, \\\\| ends the cell, and a backslash escapes nothing in code; drop the backslash before the |, or end the code before the | and write the | as text",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := refusalTree(t, test.document, test.tableID)
			_, err := applyAlone(tree, test.op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) {
				t.Fatalf("%s = %v, want an *ErrInvalidOp", test.op.Op, err)
			}
			if invalid.Field != test.field || invalid.Reason != test.reason {
				t.Fatalf("refusal = {%q, %q}, want {%q, %q}", invalid.Field, invalid.Reason, test.field, test.reason)
			}
		})
	}
}

// The positive control for those refusals: a replacement that leaves the block readable, in its
// own shape, and its code writable in a cell is applied, so none of them refuses a write by
// reading blocks the write never changed.
func TestEditRefusalsKeepAWriteTheyDoNotJudge(t *testing.T) {
	for _, test := range []struct{ name, document, with, want string }{
		{"html inside a line", "Intro.\n\nfoo Body. bar\n", "<div>x</div>", "Intro.\n\nfoo <div>x</div> bar\n"},
		{"code without a pipe in a cell", "| h |\n| --- |\n| Body. |\n", "`a b`", "| h |\n| --- |\n| `a b` |\n"},
		{"plain text over a heading", "# Body.\n", "Other", "# Other\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := refusalTree(t, test.document, "")
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace: %v", err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("markdown = %q, want %q", markdown, test.want)
			}
		})
	}
}

// refusalTree parses document and, with tableID given, names its first block, so a refusal that
// quotes that block's id reads the same at every run.
func refusalTree(t *testing.T, document, tableID string) *pmdoc.Node {
	t.Helper()
	tree, err := parseInput(document)
	if err != nil {
		t.Fatal(err)
	}
	if tableID != "" {
		if tree.Children[0].Attrs == nil {
			tree.Children[0].Attrs = pmdoc.Attrs{}
		}
		tree.Children[0].Attrs[pmdoc.BlockIDAttr] = tableID
	}
	return tree
}

// namedTree is one corpus document parsed, under the name its failures give it.
type namedTree struct {
	name string
	tree *pmdoc.Node
}

// corpusTrees is pmdoc's own corpus parsed, the documents the block walk is held over.
func corpusTrees(t *testing.T) []namedTree {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "pmdoc", "testdata", "corpus", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	trees := make([]namedTree, 0, len(paths))
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := parseInput(string(markdown))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		trees = append(trees, namedTree{name: filepath.Base(path), tree: tree})
	}
	return trees
}
