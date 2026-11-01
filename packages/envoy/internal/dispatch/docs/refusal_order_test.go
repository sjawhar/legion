package docs

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write's refusals all read the document-level blocks it changed. The write now finds those
// blocks once and passes the answer on (matchedBlocks), where each refusal found them for itself,
// so these tests hold the one walk to the two lookups it replaced and the refusals to the words
// they are given in. Every expected string here is what this package answered before the change;
// the file runs unchanged at the base revision.

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
