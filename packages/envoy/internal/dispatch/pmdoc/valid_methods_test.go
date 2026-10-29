package pmdoc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A ValidDocument method answers what its exported counterpart answers for the same tree, the
// refusal included: the methods exist only so an edit validates each tree once, so a difference
// between the two is a difference in what an edit answers. These tests hold every method added for
// the edit path to its counterpart over the whole corpus.

// validOf is tree as a ValidDocument, which every corpus document is.
func validOf(t *testing.T, tree *Node) ValidDocument {
	t.Helper()
	valid, err := ValidateDocument(tree, "test")
	if err != nil {
		t.Fatalf("ValidateDocument: %v", err)
	}
	return valid
}

// sameAnswer reports how two answers differ, or "" when they do not: the values as %#v prints
// them, and the errors by their text, since every caller serves an error's own words.
func sameAnswer(wantValue, gotValue any, wantErr, gotErr error) string {
	if (wantErr == nil) != (gotErr == nil) {
		return fmt.Sprintf("errors: want %v, got %v", wantErr, gotErr)
	}
	if wantErr != nil {
		if wantErr.Error() != gotErr.Error() {
			return fmt.Sprintf("errors: want %q, got %q", wantErr, gotErr)
		}
		return ""
	}
	if want, got := fmt.Sprintf("%#v", wantValue), fmt.Sprintf("%#v", gotValue); want != got {
		return fmt.Sprintf("values: want %s, got %s", want, got)
	}
	return ""
}

// quoteProbes are the quotes each document is searched for: its own first words, which match; a
// single space, which is ambiguous in most documents; text no document holds; and the empty quote.
func quoteProbes(tree *Node) []string {
	probes := []string{"", " ", "no document holds this text \u2014 zzqq", "the"}
	text := TextContent(tree)
	for _, width := range []int{5, 17, 40} {
		if len(text) > width {
			probes = append(probes, text[:width])
		}
	}
	return probes
}

func TestValidDocumentQuoteFinderAnswersFindQuote(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			valid := validOf(t, tree)
			finder := valid.QuoteFinder()
			for _, quote := range quoteProbes(tree) {
				wantRange, wantErr := FindQuote(tree, quote, nil, nil)
				gotRange, gotErr := finder.Find(quote, nil, nil)
				if diff := sameAnswer(wantRange, gotRange, wantErr, gotErr); diff != "" {
					t.Fatalf("QuoteFinder().Find(%q) %s", quote, diff)
				}
			}
		})
	}
	// The zero ValidDocument holds no tree, so its finder answers FindQuote's error for one.
	_, wantErr := FindQuote(nil, "x", nil, nil)
	_, gotErr := ValidDocument{}.QuoteFinder().Find("x", nil, nil)
	if diff := sameAnswer(nil, nil, wantErr, gotErr); diff != "" {
		t.Fatalf("the zero ValidDocument's finder %s", diff)
	}
}

func TestValidDocumentBlockLookupsAnswerTheirCounterparts(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			EnsureBlockIDs(tree)
			valid := validOf(t, tree)
			// Every document-level boundary, every position one past it, and one past the end.
			position := 0
			probes := []Range{{From: 0, To: 0}}
			for _, block := range tree.Children {
				position += Size(&Node{Type: "doc", Children: []*Node{block}})
				probes = append(probes, Range{From: position, To: position}, Range{From: position - 1, To: position})
			}
			probes = append(probes, Range{From: position + 1, To: position + 1}, Range{From: 0, To: position})
			for _, probe := range probes {
				wantIndex, wantErr := BlockIndex(tree, probe)
				gotIndex, gotErr := valid.BlockIndex(probe)
				if diff := sameAnswer(wantIndex, gotIndex, wantErr, gotErr); diff != "" {
					t.Fatalf("BlockIndex(%v) %s", probe, diff)
				}
				for _, after := range []bool{false, true} {
					wantAt, wantErr := BlockBoundary(tree, probe, after)
					gotAt, gotErr := valid.BlockBoundary(probe, after)
					if diff := sameAnswer(wantAt, gotAt, wantErr, gotErr); diff != "" {
						t.Fatalf("BlockBoundary(%v, %t) %s", probe, after, diff)
					}
				}
			}
			for _, blockID := range append(blockIDsOf(tree), "no-such-block") {
				wantRange, wantErr := BlockRange(tree, blockID)
				gotRange, gotErr := valid.BlockRange(blockID)
				if diff := sameAnswer(wantRange, gotRange, wantErr, gotErr); diff != "" {
					t.Fatalf("BlockRange(%q) %s", blockID, diff)
				}
				wantIDs, wantErr := BlockDescendantIDs(tree, blockID)
				gotIDs, gotErr := valid.BlockDescendantIDs(blockID)
				if diff := sameAnswer(wantIDs, gotIDs, wantErr, gotErr); diff != "" {
					t.Fatalf("BlockDescendantIDs(%q) %s", blockID, diff)
				}
				wantTree, wantErr := DeleteBlock(tree, blockID)
				gotTree, gotErr := valid.DeleteBlock(blockID)
				if diff := sameAnswer(treeDump(wantTree), treeDump(gotTree), wantErr, gotErr); diff != "" {
					t.Fatalf("DeleteBlock(%q) %s", blockID, diff)
				}
			}
		})
	}
	// The zero ValidDocument answers each counterpart's error for a nil tree.
	for _, test := range []struct {
		name string
		want func() error
		got  func() error
	}{
		{"BlockIndex", func() error { _, err := BlockIndex(nil, Range{}); return err }, func() error { _, err := ValidDocument{}.BlockIndex(Range{}); return err }},
		{"BlockBoundary", func() error { _, err := BlockBoundary(nil, Range{}, true); return err }, func() error { _, err := ValidDocument{}.BlockBoundary(Range{}, true); return err }},
		{"BlockRange", func() error { _, err := BlockRange(nil, "b"); return err }, func() error { _, err := ValidDocument{}.BlockRange("b"); return err }},
		{"BlockDescendantIDs", func() error { _, err := BlockDescendantIDs(nil, "b"); return err }, func() error { _, err := ValidDocument{}.BlockDescendantIDs("b"); return err }},
		{"DeleteBlock", func() error { _, err := DeleteBlock(nil, "b"); return err }, func() error { _, err := ValidDocument{}.DeleteBlock("b"); return err }},
		{"DeleteTextblock", func() error { _, _, err := DeleteTextblock(nil, Range{}); return err }, func() error { _, _, err := ValidDocument{}.DeleteTextblock(Range{}); return err }},
		{"InsertTableRows", func() error {
			_, _, err := InsertTableRows(nil, Range{}, "| a |", true, NewWriteBudget())
			return err
		}, func() error {
			_, _, err := ValidDocument{}.InsertTableRows(Range{}, "| a |", true, NewWriteBudget())
			return err
		}},
		{"Splice", func() error { _, err := Splice(nil, Range{}, nil); return err }, func() error { _, err := ValidDocument{}.Splice(Range{}, nil); return err }},
	} {
		if diff := sameAnswer(nil, nil, test.want(), test.got()); diff != "" {
			t.Fatalf("the zero ValidDocument's %s %s", test.name, diff)
		}
	}
}

func TestValidDocumentSpliceAndDeleteAnswerTheirCounterparts(t *testing.T) {
	replacement, err := Parse("Replacement text.\n")
	if err != nil {
		t.Fatal(err)
	}
	invalid := &Node{Type: "doc", Children: []*Node{{Type: "no-such-node"}}}
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			valid := validOf(t, tree)
			size := Size(tree)
			probes := []Range{{From: 0, To: 0}, {From: 1, To: 1}, {From: 1, To: 2}, {From: 0, To: size}, {From: 3, To: 1}, {From: size + 5, To: size + 9}}
			if size > 8 {
				probes = append(probes, Range{From: 2, To: 7}, Range{From: size / 2, To: size/2 + 2})
			}
			for _, probe := range probes {
				for name, with := range map[string]*Node{"text": replacement, "invalid": invalid, "nil": nil} {
					wantTree, wantErr := Splice(tree, probe, with)
					gotTree, gotErr := valid.Splice(probe, with)
					if diff := sameAnswer(treeDump(wantTree), treeDump(gotTree), wantErr, gotErr); diff != "" {
						t.Fatalf("Splice(%v, %s) %s", probe, name, diff)
					}
				}
				wantTree, wantRemoved, wantErr := DeleteTextblock(tree, probe)
				gotTree, gotRemoved, gotErr := valid.DeleteTextblock(probe)
				if diff := sameAnswer([]any{treeDump(wantTree), wantRemoved}, []any{treeDump(gotTree), gotRemoved}, wantErr, gotErr); diff != "" {
					t.Fatalf("DeleteTextblock(%v) %s", probe, diff)
				}
				// The rows an insert parses carry freshly minted block ids, so the two answers are
				// compared with every block id replaced by its order of appearance.
				wantTree, wantInserted, wantErr := InsertTableRows(tree, probe, "| one | two |\n", true, NewWriteBudget())
				gotTree, gotInserted, gotErr := valid.InsertTableRows(probe, "| one | two |\n", true, NewWriteBudget())
				if diff := sameAnswer([]any{numberedIDs(wantTree), wantInserted}, []any{numberedIDs(gotTree), gotInserted}, wantErr, gotErr); diff != "" {
					t.Fatalf("InsertTableRows(%v) %s", probe, diff)
				}
			}
		})
	}
}

// UpdateDocument writes what Update writes: the state the two leave behind is byte-equal, so the
// update a write appends to a document is unchanged by taking the validated tree.
func TestUpdateDocumentWritesWhatUpdateWrites(t *testing.T) {
	documents := corpusMarkdown(t)
	for index, document := range documents {
		t.Run(document.name, func(t *testing.T) {
			over := documents[(index+1)%len(documents)]
			first, err := Parse(over.markdown)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			EnsureBlockIDs(first)
			EnsureBlockIDs(second)
			write := func(update func(txn *crdt.Transaction, frag *crdt.YXmlFragment) error) []byte {
				doc := crdt.New(crdt.WithClientID(7))
				frag := doc.GetXmlFragment("prosemirror")
				var writeErr error
				doc.Transact(func(txn *crdt.Transaction) { writeErr = Update(txn, frag, first) })
				if writeErr != nil {
					t.Fatal(writeErr)
				}
				doc.Transact(func(txn *crdt.Transaction) { writeErr = update(txn, frag) })
				if writeErr != nil {
					t.Fatal(writeErr)
				}
				return crdt.EncodeStateAsUpdateV1(doc, nil)
			}
			want := write(func(txn *crdt.Transaction, frag *crdt.YXmlFragment) error { return Update(txn, frag, second) })
			got := write(func(txn *crdt.Transaction, frag *crdt.YXmlFragment) error {
				return UpdateDocument(txn, frag, validOf(t, second))
			})
			if !bytes.Equal(want, got) {
				t.Fatalf("UpdateDocument left %d bytes of state, Update %d, and they differ", len(got), len(want))
			}
		})
	}
	// The zero ValidDocument is Update's refusal of a nil document.
	doc := crdt.New(crdt.WithClientID(7))
	frag := doc.GetXmlFragment("prosemirror")
	var wantErr, gotErr error
	doc.Transact(func(txn *crdt.Transaction) {
		wantErr = Update(txn, frag, nil)
		gotErr = UpdateDocument(txn, frag, ValidDocument{})
	})
	if diff := sameAnswer(nil, nil, wantErr, gotErr); diff != "" {
		t.Fatalf("UpdateDocument of the zero ValidDocument %s", diff)
	}
}

// blockIDsOf is every block id tree carries, in pre-order.
func blockIDsOf(tree *Node) []string {
	var ids []string
	Walk(tree, func(node *Node) bool {
		if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

// treeDump is a tree as text two equal trees print alike, "nil" for no tree.
func treeDump(tree *Node) string {
	if tree == nil {
		return "nil"
	}
	return readerDump(tree)
}

// numberedIDs is treeDump with every block id replaced by its order of appearance, for an answer
// whose ids are minted fresh at each call.
func numberedIDs(tree *Node) string {
	if tree == nil {
		return "nil"
	}
	dump := treeDump(tree)
	for index, id := range blockIDsOf(tree) {
		dump = strings.ReplaceAll(dump, id, fmt.Sprintf("block-%d", index))
	}
	return dump
}
