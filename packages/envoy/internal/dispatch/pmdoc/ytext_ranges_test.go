package pmdoc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// frozenYTextRanges and frozenYFragmentShapeChildren are yTextRanges and yFragmentShapeChildren as
// they stood before yTextRanges located its texts in the traversal that finds them (commit
// 59f6647d), copied verbatim but for their names and for walking with referenceWalk (walk_test.go): the
// oracle the one-pass version must agree with, entry for entry and refusal for refusal.
// yFragmentLocations shares walk's position rules (isOpaqueNodeType, opaqueNodeSize,
// boundaryTokens), so an oracle reading the production walk would move with a change to them.
func frozenYTextRanges(frag *crdt.YXmlFragment) ([]yTextRange, error) {
	var texts []yTextNode
	children, err := frozenYFragmentShapeChildren(frag, "", 1, &texts)
	if err != nil {
		return nil, err
	}
	doc := &Node{Type: "doc", Children: children}
	var locations []textLocation
	referenceWalk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type == "text" {
			locations = append(locations, textLocation{pos: pos, end: end})
		}
		return true
	})

	ranges := make([]yTextRange, 0, len(texts))
	location := 0
	for _, current := range texts {
		length := current.text.Len()
		if length == 0 {
			continue
		}
		if location == len(locations) {
			return nil, fmt.Errorf("%w: Yjs text has no ProseMirror text node", ErrSchema)
		}
		from := locations[location].pos
		to := from
		for remaining := length; remaining > 0; location++ {
			if location == len(locations) || locations[location].pos != to {
				return nil, fmt.Errorf("%w: Yjs text does not align with ProseMirror text runs", ErrSchema)
			}
			width := locations[location].end - locations[location].pos
			if width > remaining {
				return nil, fmt.Errorf("%w: Yjs text splits a ProseMirror text run", ErrSchema)
			}
			to = locations[location].end
			remaining -= width
		}
		ranges = append(ranges, yTextRange{
			text:      current.text,
			textblock: current.textblock,
			Range:     Range{From: from, To: to},
		})
	}
	if location != len(locations) {
		return nil, fmt.Errorf("%w: ProseMirror text has no Yjs text", ErrSchema)
	}
	return ranges, nil
}

// The hand-built tree reaches shapes no parse makes, which a fragment a peer wrote can read as and
// walkTestTrees does not build: an element named doc below the document, which adds no positions
// of its own, beside leaves and a text holding children and an empty text.
func TestWalkMatchesTheReferenceWalkOnHandBuiltShapes(t *testing.T) {
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	node := func(nodeType string, children ...*Node) *Node { return &Node{Type: nodeType, Children: children} }
	tree := node("doc",
		node("paragraph", text("a😀"), node("hardbreak"), node("image"), text(""), node("footnote_reference")),
		node("hr", node("paragraph", text("inside a leaf"))),
		node("html"),
		node("blockquote", node("doc", node("paragraph", text("x"))), node("paragraph")),
		&Node{Type: "text", Children: []*Node{node("paragraph", text("inside a text"))}},
		node("table", node("table_row", node("table_cell", node("paragraph", text("cell"))))),
	)
	want, wantPanicked := recordWalk(referenceWalk, tree, 0)
	got, gotPanicked := recordWalk(walk, tree, 0)
	compareVisits(t, "hand-built", got, want, gotPanicked, wantPanicked)
	if got, want := nodeSize(tree), referenceNodeSize(tree); got != want {
		t.Errorf("hand-built: nodeSize %d, the reference %d", got, want)
	}
}

// yFragmentShapeChildren is the shape of frag's children, which stand depth levels below the
// document, and appends each Yjs text among them to texts with the textblock holding it. It
// refuses a node past MaxTreeDepth as Read does: an element, or a text holding anything, which Read
// makes a text node of.
func frozenYFragmentShapeChildren(frag *crdt.YXmlFragment, textblock string, depth int, texts *[]yTextNode) ([]*Node, error) {
	var children []*Node
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			if current.Len() > 0 {
				if err := treeDepthError(depth); err != nil {
					return nil, err
				}
			}
			*texts = append(*texts, yTextNode{text: current, textblock: textblock})
			delta, err := yTextDeltaInTransaction(current)
			if err != nil {
				return nil, err
			}
			for _, operation := range delta {
				value, ok := operation.Insert.(string)
				if !ok {
					return nil, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
				}
				children = append(children, &Node{Type: "text", Text: value})
			}
		case *crdt.YXmlElement:
			if err := treeDepthError(depth); err != nil {
				return nil, err
			}
			inner := textblock
			switch current.NodeName {
			case "paragraph", "heading", "code_block":
				inner = current.NodeName
			}
			grandchildren, err := frozenYFragmentShapeChildren(&current.YXmlFragment, inner, depth+1, texts)
			if err != nil {
				return nil, err
			}
			children = append(children, &Node{Type: current.NodeName, Children: grandchildren})
		default:
			return nil, fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return children, nil
}

// firstDifferentYTextRange is the index of the first entry got and want disagree on - its Yjs
// text, its textblock or its range - or -1 when they agree on every entry and on their count.
func firstDifferentYTextRange(got, want []yTextRange) int {
	for index := range min(len(got), len(want)) {
		if got[index].text != want[index].text || got[index].textblock != want[index].textblock || got[index].Range != want[index].Range {
			return index
		}
	}
	if len(got) != len(want) {
		return min(len(got), len(want))
	}
	return -1
}

// assertYTextRangesMatchFrozen checks yTextRanges(frag) against the frozen walk and returns its
// result. Its failures carry error strings, textblock names and ranges, none of which holds the
// document's text.
func assertYTextRangesMatchFrozen(t testing.TB, name string, frag *crdt.YXmlFragment) ([]yTextRange, error) {
	t.Helper()
	got, gotErr := yTextRanges(frag)
	want, wantErr := frozenYTextRanges(frag)
	if (gotErr == nil) != (wantErr == nil) || gotErr != nil && (gotErr.Error() != wantErr.Error() || errors.Is(gotErr, ErrSchema) != errors.Is(wantErr, ErrSchema)) {
		t.Errorf("%s: yTextRanges refused with %v, the frozen walk with %v", name, gotErr, wantErr)
	}
	if index := firstDifferentYTextRange(got, want); index >= 0 {
		describe := func(ranges []yTextRange) string {
			if index >= len(ranges) {
				return fmt.Sprintf("no entry (%d in all)", len(ranges))
			}
			return fmt.Sprintf("%s %v (text %p)", ranges[index].textblock, ranges[index].Range, ranges[index].text)
		}
		t.Errorf("%s: entry %d: yTextRanges %s, the frozen walk %s", name, index, describe(got), describe(want))
	}
	return got, gotErr
}

// writeTree writes tree into a new Y document's prosemirror fragment with Update, in one
// transaction.
func writeTree(t testing.TB, tree *Node, options ...crdt.DocOption) (*crdt.Doc, *crdt.YXmlFragment) {
	t.Helper()
	doc := crdt.New(options...)
	frag := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, frag, tree) }); err != nil {
		t.Fatalf("write the tree: %v", err)
	}
	return doc, frag
}

func TestYTextRangesMatchesFrozenOnDocuments(t *testing.T) {
	type document struct {
		name     string
		markdown string
		realSize bool
	}
	var documents []document
	for _, corpus := range corpusMarkdown(t) {
		documents = append(documents, document{name: corpus.name, markdown: corpus.markdown})
	}
	for _, size := range realSizeDocuments {
		documents = append(documents, document{name: size.name, markdown: realSizeMarkdown(size.bytes, size.seed), realSize: true})
	}
	for _, document := range documents {
		for _, marked := range []bool{false, true} {
			name := document.name
			if marked {
				name += " marked"
			}
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if marked {
				placeAnchorMarks(tree, 47, 47)
			}
			_, frag := writeTree(t, tree)
			ranges, err := assertYTextRangesMatchFrozen(t, name, frag)
			if err != nil {
				t.Fatalf("%s: a document Update wrote is refused: %v", name, err)
			}
			if !document.realSize || !marked {
				continue
			}
			// A marked text is several delta operations, the case where a Yjs text spans several
			// located runs.
			formatted := false
			for _, current := range ranges {
				delta, err := yTextDeltaInTransaction(current.text)
				if err != nil {
					t.Fatal(err)
				}
				if len(delta) >= 2 {
					formatted = true
					break
				}
			}
			if !formatted {
				t.Fatalf("%s: no Yjs text holds two delta operations", name)
			}
		}
	}
}

// Each fragment here is one a peer's update can leave behind but Update never writes: the
// one-pass walk must refuse it with the frozen walk's error, met in the same order, or read it the
// same way.
func TestYTextRangesMatchesFrozenOnRefusals(t *testing.T) {
	element := func(txn *crdt.Transaction, parent *crdt.YXmlFragment, name string) *crdt.YXmlElement {
		child := crdt.NewYXmlElement(name)
		parent.InsertElement(txn, len(parent.Children()), child)
		return child
	}
	text := func(txn *crdt.Transaction, parent *crdt.YXmlFragment, value string) *crdt.YXmlText {
		child := crdt.NewYXmlText()
		parent.InsertText(txn, len(parent.Children()), child)
		if value != "" {
			child.Insert(txn, 0, value, nil)
		}
		return child
	}
	embedded := func(txn *crdt.Transaction, parent *crdt.YXmlFragment) {
		child := text(txn, parent, "ab")
		child.InsertEmbed(txn, 1, map[string]any{"src": "x.png"}, nil)
	}
	// chain appends elements nested levels deep under parent, which stands at level depth, and
	// returns the innermost, at level depth+levels.
	chain := func(txn *crdt.Transaction, parent *crdt.YXmlFragment, levels int) *crdt.YXmlFragment {
		for range levels {
			parent = &element(txn, parent, "blockquote").YXmlFragment
		}
		return parent
	}
	// misaligned is a paragraph whose rule holds text: the text has no located run, a refusal
	// only aligning texts with runs meets, after the traversal.
	misaligned := func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
		paragraph := element(txn, frag, "paragraph")
		text(txn, &paragraph.YXmlFragment, "a")
		text(txn, &element(txn, &paragraph.YXmlFragment, "hr").YXmlFragment, "zz")
	}

	type refusal struct {
		name    string
		write   func(txn *crdt.Transaction, frag *crdt.YXmlFragment)
		refused bool
	}
	refusals := []refusal{
		{name: "an element past the depth bound", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			chain(txn, frag, MaxTreeDepth+1)
		}},
		{name: "a text past the depth bound", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			text(txn, chain(txn, frag, MaxTreeDepth), "a")
		}},
		{name: "an empty text past the depth bound", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			text(txn, &element(txn, frag, "paragraph").YXmlFragment, "a")
			text(txn, chain(txn, frag, MaxTreeDepth), "")
		}},
		{name: "past the depth bound inside an element named text", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, chain(txn, &element(txn, &paragraph.YXmlFragment, "text").YXmlFragment, MaxTreeDepth-2), "a")
		}},
		{name: "an embed in a text", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			embedded(txn, &element(txn, frag, "paragraph").YXmlFragment)
		}},
		{name: "an embed in a leaf", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, &paragraph.YXmlFragment, "a")
			embedded(txn, &element(txn, &paragraph.YXmlFragment, "image").YXmlFragment)
		}},
		{name: "an embed after an alignment problem", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			misaligned(txn, frag)
			embedded(txn, &element(txn, frag, "paragraph").YXmlFragment)
		}},
		{name: "a depth refusal after an alignment problem", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			misaligned(txn, frag)
			text(txn, chain(txn, frag, MaxTreeDepth), "a")
		}},
		{name: "an empty element named text between texts", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, &paragraph.YXmlFragment, "ab")
			element(txn, &paragraph.YXmlFragment, "text")
			text(txn, &paragraph.YXmlFragment, "cd")
		}},
		{name: "an element named text holding text", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, &paragraph.YXmlFragment, "ab")
			text(txn, &element(txn, &paragraph.YXmlFragment, "text").YXmlFragment, "cd")
			text(txn, &paragraph.YXmlFragment, "ef")
		}},
		{name: "an element named doc", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			text(txn, &element(txn, frag, "paragraph").YXmlFragment, "ab")
			inner := element(txn, &element(txn, frag, "blockquote").YXmlFragment, "doc")
			text(txn, &element(txn, &inner.YXmlFragment, "paragraph").YXmlFragment, "cd")
			text(txn, &element(txn, frag, "paragraph").YXmlFragment, "ef")
		}},
		{name: "empty texts leading, trailing and between", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, &paragraph.YXmlFragment, "")
			text(txn, &paragraph.YXmlFragment, "ab")
			text(txn, &paragraph.YXmlFragment, "")
			element(txn, &paragraph.YXmlFragment, "hardbreak")
			text(txn, &paragraph.YXmlFragment, "")
			text(txn, &paragraph.YXmlFragment, "cd")
			text(txn, &paragraph.YXmlFragment, "")
		}},
		{name: "formatted text", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			code := element(txn, frag, "code_block")
			text(txn, &code.YXmlFragment, "x := 1")
			formatted := text(txn, &element(txn, frag, "heading").YXmlFragment, "")
			formatted.Insert(txn, 0, "ab", crdt.Attributes{"strong": map[string]any{}})
			formatted.Insert(txn, 2, "😀d", nil)
			formatted.Insert(txn, 5, "ef", crdt.Attributes{"emphasis": map[string]any{}})
		}},
		{name: "text directly in the document", write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			text(txn, frag, "ab")
			text(txn, &element(txn, frag, "paragraph").YXmlFragment, "cd")
		}},
	}
	for _, leaf := range []string{"hr", "hardbreak", "image", "html", "footnote_reference"} {
		refusals = append(refusals, refusal{name: "leaf " + leaf + " holding text", refused: true, write: func(txn *crdt.Transaction, frag *crdt.YXmlFragment) {
			paragraph := element(txn, frag, "paragraph")
			text(txn, &paragraph.YXmlFragment, "a")
			text(txn, &element(txn, &paragraph.YXmlFragment, leaf).YXmlFragment, "zz")
			text(txn, &paragraph.YXmlFragment, "b")
		}})
	}
	for _, test := range refusals {
		t.Run(test.name, func(t *testing.T) {
			doc := crdt.New()
			frag := doc.GetXmlFragment("prosemirror")
			doc.Transact(func(txn *crdt.Transaction) { test.write(txn, frag) })
			ranges, err := assertYTextRangesMatchFrozen(t, test.name, frag)
			if test.refused && !errors.Is(err, ErrSchema) {
				t.Fatalf("read %d ranges and refused with %v, want ErrSchema", len(ranges), err)
			}
			if !test.refused && (err != nil || len(ranges) == 0) {
				t.Fatalf("read %d ranges and refused with %v, want ranges read", len(ranges), err)
			}
		})
	}

	// A Yjs child that is neither text nor element: an element in no document lists the children
	// inserted into it as they were given, so a bare fragment among them reaches the walk.
	t.Run("an unexpected Yjs child", func(t *testing.T) {
		outer := crdt.NewYXmlElement("blockquote")
		inner := crdt.NewYXmlElement("paragraph")
		inner.Insert(nil, 0, &crdt.YXmlFragment{})
		outer.Insert(nil, 0, inner)
		if _, err := assertYTextRangesMatchFrozen(t, "an unexpected Yjs child", &outer.YXmlFragment); !errors.Is(err, ErrSchema) {
			t.Fatalf("refused with %v, want ErrSchema", err)
		}
	})
}

func TestYTextRangesMatchesFrozenOnAnchorCorpus(t *testing.T) {
	for _, document := range anchorCorpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			checked := 0
			for _, count := range []int{12, 47, 80} {
				tree, err := Parse(document.markdown)
				if err != nil {
					t.Skipf("%s: Parse refused it (%T)", document.name, err)
				}
				placeAnchorMarks(tree, count, int64(count))
				doc := crdt.New()
				frag := doc.GetXmlFragment("prosemirror")
				if err := doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, frag, tree) }); err != nil {
					t.Skipf("%s at %d marks: Update refused it (%T)", document.name, count, err)
				}
				ranges, _ := assertYTextRangesMatchFrozen(t, fmt.Sprintf("%s at %d marks", document.name, count), frag)
				checked += len(ranges)
			}
			t.Logf("%s: %d Yjs texts checked", document.name, checked)
		})
	}
}
