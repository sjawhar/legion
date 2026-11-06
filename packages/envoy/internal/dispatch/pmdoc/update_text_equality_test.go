package pmdoc

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

// equalYTextPText decides whether a write rewrites a text or leaves it alone. It used to build the
// text's delta (yTextDeltaInTransaction, which the write itself still builds) and compare that
// delta with the nodes; it now walks the items and compares each run as it grows. This test holds
// the walk to the delta it replaced, over every text of every corpus document and over nodes
// changed in the ways a write changes them, so a disagreement - a text rewritten that holds what
// the write wants, or one left alone that does not - fails here.

// equalYTextPTextByDelta is the predicate as it was: the whole delta, then a positional compare.
func equalYTextPTextByDelta(text *crdt.YXmlText, nodes []*Node) bool {
	delta, err := yTextDeltaInTransaction(text)
	if err != nil {
		return false
	}
	if len(delta) != len(nodes) {
		return false
	}
	for index, operation := range delta {
		if operation.Op != crdt.DeltaOpInsert {
			return false
		}
		value, ok := operation.Insert.(string)
		if !ok || value != nodes[index].Text || !equalYMarks(operation.Attributes, nodes[index].Marks) {
			return false
		}
	}
	return true
}

func TestEqualYTextPTextAnswersTheDeltaComparison(t *testing.T) {
	// variants are the node runs each text is compared against: the run the document holds, and
	// the ways a write changes one.
	variants := func(nodes []*Node) [][]*Node {
		out := [][]*Node{nodes, {}}
		if len(nodes) == 0 {
			return out
		}
		clone := func(change func([]*Node)) []*Node {
			copied := make([]*Node, len(nodes))
			for index, node := range nodes {
				copy := *node
				copy.Marks = append([]Mark(nil), node.Marks...)
				copied[index] = &copy
			}
			change(copied)
			return copied
		}
		out = append(out,
			clone(func(run []*Node) { run[0].Text += " more" }),
			clone(func(run []*Node) { run[0].Text = "" }),
			clone(func(run []*Node) {
				if len(run[0].Text) > 1 {
					run[0].Text = run[0].Text[1:]
				}
			}),
			clone(func(run []*Node) { run[len(run)-1].Marks = append(run[len(run)-1].Marks, Mark{Type: "strong"}) }),
			clone(func(run []*Node) { run[0].Marks = nil }),
			nodes[:len(nodes)-1],
			append(append([]*Node{}, nodes...), &Node{Type: "text", Text: "tail"}),
		)
		return out
	}
	texts := 0
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			EnsureBlockIDs(tree)
			doc := crdt.New(crdt.WithClientID(3))
			frag := doc.GetXmlFragment("prosemirror")
			var writeErr error
			doc.Transact(func(txn *crdt.Transaction) { writeErr = Update(txn, frag, tree) })
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			back, err := Read(frag)
			if err != nil {
				t.Fatal(err)
			}
			// Each text of the written document beside the nodes it was written from.
			var visit func(children []any, nodes []*Node)
			visit = func(children []any, nodes []*Node) {
				child, index := 0, 0
				for index < len(nodes) && child < len(children) {
					if nodes[index].Type != "text" {
						if element, ok := children[child].(*crdt.YXmlElement); ok {
							inner := make([]any, 0)
							for _, next := range element.Children() {
								inner = append(inner, next)
							}
							visit(inner, nodes[index].Children)
						}
						index++
						child++
						continue
					}
					run := index
					for run < len(nodes) && nodes[run].Type == "text" {
						run++
					}
					if text, ok := children[child].(*crdt.YXmlText); ok {
						texts++
						for _, variant := range variants(nodes[index:run]) {
							want := equalYTextPTextByDelta(text, variant)
							if got := equalYTextPText(text, variant); got != want {
								t.Fatalf("equalYTextPText = %t, the delta comparison says %t, for %d nodes", got, want, len(variant))
							}
						}
					}
					index = run
					child++
				}
			}
			children := make([]any, 0)
			for _, next := range frag.Children() {
				children = append(children, next)
			}
			visit(children, back.Children)
		})
	}
	t.Logf("compared %d texts", texts)
	if texts == 0 {
		t.Fatal("no text was compared")
	}
}
