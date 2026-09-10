package pmdoc

import (
	"errors"
	"reflect"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestFindQuoteDisambiguatesByOccurrenceAndNearest(t *testing.T) {
	doc, err := Parse("brown X brown Y brown\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindQuote(doc, "brown", nil, nil); !errors.As(err, new(*ErrTargetAmbiguous)) {
		t.Fatalf("want ambiguity, got %v", err)
	}

	one := 1
	rangeAtOccurrence, err := FindQuote(doc, "brown", &one, nil)
	if err != nil || rangeAtOccurrence.From != 1+len16("brown X ") {
		t.Fatalf("occurrence 1: %v %v", rangeAtOccurrence, err)
	}

	near := 1 + len16("brown X brown Y b")
	rangeNearHint, err := FindQuote(doc, "brown", nil, &near)
	if err != nil || rangeNearHint.From != 1+len16("brown X brown Y ") {
		t.Fatalf("nearest: %v %v", rangeNearHint, err)
	}
	if _, err := FindQuote(doc, "purple", nil, nil); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("want not found, got %v", err)
	}

	normalized, err := Parse("the quick\nbrown fox\n")
	if err != nil {
		t.Fatal(err)
	}
	normalizedRange, err := FindQuote(normalized, "quick brown", nil, nil)
	if err != nil || normalizedRange != (Range{From: 5, To: 16}) {
		t.Fatalf("normalized whitespace range = %v, %v", normalizedRange, err)
	}
}

func TestFindHeadingMatchesTitleAcrossLevels(t *testing.T) {
	doc, err := Parse("# A\n\ntext\n\n## A\n\n# B\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindHeading(doc, "A", nil); !errors.As(err, new(*ErrTargetAmbiguous)) {
		t.Fatalf("two headings titled A: err = %v, want ErrTargetAmbiguous", err)
	}
	one := 1
	r, err := FindHeading(doc, "A", &one)
	if err != nil {
		t.Fatal(err)
	}
	// doc = h1("A")[0,3) p("text")[3,9) h2("A")[9,12) h1("B")[12,15)
	if r != (Range{From: 9, To: 12}) {
		t.Fatalf("second A = %v, want [9,12)", r)
	}
	if r, err := FindHeading(doc, "B", nil); err != nil || r != (Range{From: 12, To: 15}) {
		t.Fatalf("B = %v %v", r, err)
	}
	if _, err := FindHeading(doc, "text", nil); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("paragraph text is not a heading: %v", err)
	}
	if got := Size(doc); got != 15 {
		t.Fatalf("Size = %d, want 15", got)
	}
}

func TestListMarksAndMarkAttrs(t *testing.T) {
	doc, err := FromJSON(fixtureNamed(t, "marks").PMJSON)
	if err != nil {
		t.Fatal(err)
	}
	want := []MarkRef{{"proofComment", "c1"}, {"dispatchAsk", "a1"}, {"proofSuggestion", "s1"}}
	if got := ListMarks(doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("ListMarks = %v, want %v", got, want)
	}
	attrs, ok := MarkAttrs(doc, "proofSuggestion", "s1")
	if !ok || attrs["kind"] != "replace" || attrs["by"] != "session:01a0" {
		t.Fatalf("MarkAttrs = %v %v", attrs, ok)
	}
	if _, ok := MarkAttrs(doc, "proofComment", "nope"); ok {
		t.Fatal("unknown id must not resolve")
	}
}

func TestMarkRangeThenFindMark(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	want, err := Parse("The quick brown fox.\n")
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}

	rangeBrown, err := FindQuote(want, "brown", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, rangeBrown, Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "a1", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	got, quote, ok := FindMark(tree, "dispatchAsk", "a1")
	if !ok || quote != "brown" || got != rangeBrown {
		t.Fatalf("FindMark = %v %q %v", got, quote, ok)
	}

	doc.Transact(func(txn *crdt.Transaction) {
		err = Unmark(txn, frag, "dispatchAsk", "a1")
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err = Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := FindMark(tree, "dispatchAsk", "a1"); ok {
		t.Fatal("mark still present after Unmark")
	}
	markdown, _, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "The quick brown fox.\n" {
		t.Fatalf("text changed by marking: %q", markdown)
	}
}

func TestFindMarkSpansTextblocks(t *testing.T) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	tree, err := Parse("one\n\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, fragment, tree)
	})
	if err != nil {
		t.Fatal(err)
	}
	range_, err := FindQuote(tree, "one two", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, fragment, range_, Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	marked, err := Read(fragment)
	if err != nil {
		t.Fatal(err)
	}
	got, quote, found := FindMark(marked, "proofComment", "c1")
	if !found || quote != "one two" || got != range_ {
		t.Fatalf("FindMark = %v %q %t, want %v %q true", got, quote, found, range_, "one two")
	}
}

func TestSpliceInlineKeepsNeighbourMarks(t *testing.T) {
	doc, err := Parse("Keep **this** and change that.\n")
	if err != nil {
		t.Fatal(err)
	}
	rangeChange, err := FindQuote(doc, "change that", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("*alter* those")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, rangeChange, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(out)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Keep **this** and *alter* those.\n" {
		t.Fatalf("md = %q", markdown)
	}
}

func TestSpliceAcrossBlocksReplacesBlocks(t *testing.T) {
	doc, err := Parse("# Title\n\nOne.\n\nTwo.\n\nThree.\n")
	if err != nil {
		t.Fatal(err)
	}
	from, err := FindQuote(doc, "One.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, err := FindQuote(doc, "Two.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("- a\n- b\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, Range{From: from.From, To: to.To}, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(out)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# Title\n\n- a\n- b\n\nThree.\n" {
		t.Fatalf("md = %q", markdown)
	}
}

func TestSpliceNestedTextblocks(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		quote    string
		want     string
	}{
		{
			name:     "list item",
			markdown: "- one\n- two\n",
			quote:    "one",
			want:     "- uno\n- two\n",
		},
		{
			name:     "nested list item",
			markdown: "- outer\n  - one\n  - two\n",
			quote:    "one",
			want:     "- outer\n  - uno\n  - two\n",
		},
		{
			name:     "blockquote paragraph",
			markdown: "> one\n>\n> two\n",
			quote:    "one",
			want:     "> uno\n>\n> two\n",
		},
		{
			name:     "table cell",
			markdown: "| one |\n| :--- |\n| two |\n",
			quote:    "one",
			want:     "| uno |\n| :--- |\n| two |\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			r, err := FindQuote(doc, test.quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			with, err := Parse("uno\n")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Splice(doc, r, with)
			if err != nil {
				t.Fatal(err)
			}
			markdown, _, err := Render(got)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("Splice() = %q, want %q", markdown, test.want)
			}
		})
	}
}

func TestSpliceCrossListItemsReplacesOnlySelectedItems(t *testing.T) {
	doc, err := Parse("- one\n- two\n- three\n")
	if err != nil {
		t.Fatal(err)
	}
	from, err := FindQuote(doc, "one", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, err := FindQuote(doc, "two", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("- uno\n- dos\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Splice(doc, Range{From: from.From, To: to.To}, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "- uno\n- dos\n- three\n" {
		t.Fatalf("Splice() = %q", markdown)
	}
}

func TestSpliceAcrossTextblocksKeepsPartialBoundaries(t *testing.T) {
	doc, err := Parse("a b c\n\nd e f\n")
	if err != nil {
		t.Fatal(err)
	}
	from, err := FindQuote(doc, "b c", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, err := FindQuote(doc, "d e", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("X\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Splice(doc, Range{From: from.From, To: to.To}, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "a X f\n" {
		t.Fatalf("Splice() = %q, want %q", markdown, "a X f\n")
	}
}

func spliceRange(t *testing.T, doc *Node, fx spliceFixture) Range {
	t.Helper()
	if fx.Point == "" {
		from, err := FindQuote(doc, fx.From, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		to, err := FindQuote(doc, fx.To, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return Range{From: from.From, To: to.To}
	}
	var pos int
	switch fx.Point {
	case "doc-start":
		pos = 0
	case "doc-end":
		pos = Size(doc)
	default:
		at, err := FindQuote(doc, fx.At, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		block := textblockContaining(doc, at.From)
		if block.To == 0 {
			t.Fatalf("point %q is not in a textblock", fx.Point)
		}
		switch fx.Point {
		case "after":
			pos = at.To
		case "before":
			pos = at.From
		case "after-textblock":
			pos = block.To
		case "before-textblock":
			pos = block.From
		default:
			t.Fatalf("unknown point %q", fx.Point)
		}
	}
	return Range{From: pos, To: pos}
}

func textblockContaining(doc *Node, position int) Range {
	var block Range
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if isTextblock(node.Type) && pos+1 <= position && position <= end-1 {
			block = Range{From: pos, To: end}
			return false
		}
		return true
	})
	return block
}

func TestSpliceMatchesEngineReplaceRange(t *testing.T) {
	for _, fx := range loadSpliceFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc, err := Parse(fx.Markdown)
			if err != nil {
				t.Fatal(err)
			}
			r := spliceRange(t, doc, fx)
			with, err := Parse(fx.Replacement)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Splice(doc, r, with)
			if err != nil {
				t.Fatal(err)
			}
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				actual, _ := got.JSON()
				t.Fatalf("Splice() differs from engine replaceRange\n got: %s\nwant: %s", actual, fx.PMJSON)
			}
		})
	}
}

func TestSpliceBlockReplacementDoesNotFlattenIntoParagraph(t *testing.T) {
	doc, err := Parse("a b c\n")
	if err != nil {
		t.Fatal(err)
	}
	r, err := FindQuote(doc, "b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("```\ncode\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Splice(doc, r, with)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != 3 || got.Children[0].Type != "paragraph" || got.Children[1].Type != "code_block" || got.Children[2].Type != "paragraph" {
		t.Fatalf("Splice() blocks = %#v, want paragraph/code_block/paragraph", got.Children)
	}
	if got.Children[0].Children[0].Text != "a " || got.Children[1].Children[0].Text != "code" || got.Children[2].Children[0].Text != " c" {
		t.Fatalf("Splice() did not preserve paragraph boundaries: %#v", got.Children)
	}
}

func TestSpliceDoesNotJoinTextblocksWithDifferentAttrs(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		from     string
		to       string
		want     []string
	}{
		{
			name:     "heading levels",
			markdown: "# a b\n\n## c d\n",
			from:     "b",
			to:       "c",
			want:     []string{"heading", "paragraph", "heading"},
		},
		{
			name:     "code languages",
			markdown: "```go\na b\n```\n\n```js\nc d\n```\n",
			from:     "b",
			to:       "c",
			want:     []string{"code_block", "paragraph", "code_block"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			from, err := FindQuote(doc, test.from, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			to, err := FindQuote(doc, test.to, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			with, err := Parse("X\n")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Splice(doc, Range{From: from.From, To: to.To}, with)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Children) != len(test.want) {
				t.Fatalf("Splice() has %d blocks, want %d", len(got.Children), len(test.want))
			}
			for index, nodeType := range test.want {
				if got.Children[index].Type != nodeType {
					t.Fatalf("Splice() block %d = %q, want %q", index, got.Children[index].Type, nodeType)
				}
			}
		})
	}
}

func TestSpliceAtBlockBoundariesAndDeletes(t *testing.T) {
	doc, err := Parse("One.\n\nTwo.\n")
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("Replacement.\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Splice(doc, Range{From: 0, To: 6}, with)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Replacement.\n\nTwo.\n" {
		t.Fatalf("Splice(block boundary) = %q", markdown)
	}

	empty, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	got, err = Splice(doc, Range{From: 0, To: 6}, empty)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _, err = Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Two.\n" {
		t.Fatalf("Splice(delete block) = %q", markdown)
	}

	only, err := Parse("One.\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err = Splice(only, Range{From: 0, To: 6}, empty)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(empty) {
		t.Fatalf("Splice(delete all blocks) = %#v, want one empty paragraph", got)
	}
}

func TestMarkRangeSupportsCodeBlocksAndMultipleBlocks(t *testing.T) {
	doc, err := Parse("```\ncode\n```\n\none\n\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}

	state := crdt.New()
	frag := state.GetXmlFragment("prosemirror")
	state.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := FindQuote(doc, "code", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, code, Mark{Type: "proofComment", Attrs: Attrs{"id": "code", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	one, err := FindQuote(doc, "one", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := FindQuote(doc, "two", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.Transact(func(txn *crdt.Transaction) {
		err = MarkRange(txn, frag, Range{From: one.From, To: two.To}, Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "both", "by": "session:x"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMark(got, "proofComment") || !hasMark(got, "dispatchAsk") {
		t.Fatal("marks were not applied across every selected text span")
	}
}
