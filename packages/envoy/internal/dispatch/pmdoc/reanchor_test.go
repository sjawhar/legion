package pmdoc

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
)

// reanchorOutcome is what re-anchoring one mark did: the range its quote resolved to, the error
// finding the quote returned, and the error writing the mark returned.
type reanchorOutcome struct {
	r       Range
	findErr error
	markErr error
}

// skipped is the upload's decision for the mark: a quote that no longer resolves, or a range that
// holds no text, drops it.
func (o reanchorOutcome) skipped() bool {
	return errors.Is(o.findErr, ErrTargetNotFound) || errors.Is(o.markErr, ErrTargetNotFound)
}

type reanchorStats struct {
	placed, reanchored, missed int
}

// textblockAt is the textblock of doc whose content holds position.
func textblockAt(doc *Node, position int) *Node {
	var found *Node
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if isTextblock(node.Type) && pos < position && position < end {
			found = node
			return false
		}
		return true
	})
	return found
}

// sameError reports whether two errors are both nil or carry one message.
func sameError(left, right error) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Error() == right.Error()
}

// checkReanchorReuse re-anchors count marks of markdown after an upload the way ReplaceText does,
// in two Y documents written alike: one asks FindQuote and MarkRange once per mark, the other asks
// one QuoteFinder and one MarkWriter for every mark, and the two must decide, write and end alike.
// The upload re-parses markdown, so it holds no anchor mark, replaces the textblock holding every
// third anchor's start, so that anchor's quote can miss, and appends " edited" to every fifth other
// paragraph. Its failures name the document, the mark's index, ranges, counts and error types,
// never the document's text or a miss's nearest blocks: the anchor corpus it also checks is
// private. corpus skips a document Parse or Update refuses rather than failing on it.
func checkReanchorReuse(t *testing.T, name, markdown string, count int, seed int64, corpus bool) reanchorStats {
	t.Helper()
	refuse := t.Fatalf
	if corpus {
		refuse = t.Skipf
	}
	marked, err := Parse(markdown)
	if err != nil {
		refuse("%s: Parse refused it (%T)", name, err)
	}
	refs := placeAnchorMarks(marked, count, seed)
	target, err := Parse(markdown)
	if err != nil {
		refuse("%s: Parse refused it (%T)", name, err)
	}

	type anchor struct {
		quote string
		near  int
		mark  Mark
	}
	anchors := make([]anchor, len(refs))
	var replacing []*Node
	for index, ref := range refs {
		r, quote, ok := FindMark(marked, ref.Type, ref.ID)
		attrs, attrsOK := MarkAttrs(marked, ref.Type, ref.ID)
		if !ok || !attrsOK {
			t.Fatalf("%s: mark %d (%s) is not in the tree it was placed in", name, index, ref.Type)
		}
		anchors[index] = anchor{quote: quote, near: r.From, mark: Mark{Type: ref.Type, Attrs: attrs}}
		if index%3 == 0 {
			// The anchors' positions are the target's too until it is edited: marks take none.
			if block := textblockAt(target, r.From); block != nil {
				replacing = append(replacing, block)
			}
		}
	}
	replaced := map[*Node]bool{}
	for index, block := range replacing {
		block.Children = []*Node{{Type: "text", Text: fmt.Sprintf("replaced block %d", index)}}
		replaced[block] = true
	}
	paragraphs := 0
	Walk(target, func(node *Node) bool {
		if node.Type != "paragraph" || replaced[node] {
			return true
		}
		if paragraphs%5 == 0 {
			if last := len(node.Children) - 1; last >= 0 && node.Children[last].Type == "text" && len(node.Children[last].Marks) == 0 {
				node.Children[last].Text += " edited"
			} else {
				node.Children = append(node.Children, &Node{Type: "text", Text: " edited"})
			}
		}
		paragraphs++
		return true
	})

	perCall, perCallFragment := writeTree(t, marked, crdt.WithClientID(7))
	reused, reusedFragment := writeTree(t, marked, crdt.WithClientID(7))

	perCallOutcomes := make([]reanchorOutcome, len(anchors))
	err = perCall.TransactE(func(txn *crdt.Transaction) error {
		if err := Update(txn, perCallFragment, target); err != nil {
			return err
		}
		for index, anchor := range anchors {
			near := anchor.near
			outcome := &perCallOutcomes[index]
			outcome.r, outcome.findErr = FindQuote(target, anchor.quote, nil, &near)
			if outcome.findErr == nil {
				outcome.markErr = MarkRange(txn, perCallFragment, outcome.r, anchor.mark)
			}
		}
		return nil
	})
	if err != nil {
		refuse("%s: Update refused the upload (%T)", name, err)
	}

	reusedOutcomes := make([]reanchorOutcome, len(anchors))
	err = reused.TransactE(func(txn *crdt.Transaction) error {
		// The writer is made before Update writes the fragment: it reads nothing of the fragment
		// until its first mark, so the texts it marks are those Update left, not those it replaced.
		writer := NewMarkWriter(reusedFragment)
		finder := NewQuoteFinder(target)
		if err := Update(txn, reusedFragment, target); err != nil {
			return err
		}
		for index, anchor := range anchors {
			near := anchor.near
			outcome := &reusedOutcomes[index]
			outcome.r, outcome.findErr = finder.FindDiscardingMiss(anchor.quote, nil, &near)
			var miss *ErrQuoteNotFound
			if errors.As(perCallOutcomes[index].findErr, &miss) {
				// A miss the upload drops is never described, but Find still describes it.
				if outcome.findErr != ErrTargetNotFound || outcome.r != (Range{}) {
					t.Errorf("%s: mark %d: FindDiscardingMiss of a miss returned %v and %T, want the bare ErrTargetNotFound", name, index, outcome.r, outcome.findErr)
				}
				if _, described := finder.Find(anchor.quote, nil, &near); !reflect.DeepEqual(described, perCallOutcomes[index].findErr) {
					t.Errorf("%s: mark %d: the finder's Find of a miss returned %T, not FindQuote's description", name, index, described)
				}
			} else if !reflect.DeepEqual(outcome.findErr, perCallOutcomes[index].findErr) {
				t.Errorf("%s: mark %d: FindDiscardingMiss returned %T, FindQuote %T", name, index, outcome.findErr, perCallOutcomes[index].findErr)
			}
			if outcome.findErr != nil {
				continue
			}
			outcome.markErr = writer.Mark(txn, outcome.r, anchor.mark)
			// Writing a mark formats texts and changes no range, so the table the writer read at
			// its first mark must still be the fragment's.
			fresh, freshErr := yTextRanges(reusedFragment)
			if !sameError(writer.err, freshErr) {
				t.Errorf("%s: after mark %d the writer holds refusal %T, the fragment reads %T", name, index, writer.err, freshErr)
			} else if differs := firstDifferentYTextRange(writer.spans, fresh); differs >= 0 {
				t.Errorf("%s: after mark %d the writer's text %d of %d is not the fragment's (%d texts)", name, index, differs, len(writer.spans), len(fresh))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: Update refused the upload in the second document only (%T)", name, err)
	}

	var stats reanchorStats
	stats.placed = len(refs)
	for index := range anchors {
		perCallOutcome, reusedOutcome := perCallOutcomes[index], reusedOutcomes[index]
		if perCallOutcome.skipped() != reusedOutcome.skipped() {
			t.Errorf("%s: mark %d: per call skipped it %t, reused %t", name, index, perCallOutcome.skipped(), reusedOutcome.skipped())
		}
		if perCallOutcome.r != reusedOutcome.r {
			t.Errorf("%s: mark %d: per call resolved %v, reused %v", name, index, perCallOutcome.r, reusedOutcome.r)
		}
		if !sameError(perCallOutcome.markErr, reusedOutcome.markErr) {
			t.Errorf("%s: mark %d: MarkRange returned %T, the writer %T", name, index, perCallOutcome.markErr, reusedOutcome.markErr)
		}
		switch {
		case perCallOutcome.skipped():
			stats.missed++
		case perCallOutcome.findErr == nil && perCallOutcome.markErr == nil:
			stats.reanchored++
		}
	}
	if !bytes.Equal(crdt.EncodeStateAsUpdateV1(perCall, nil), crdt.EncodeStateAsUpdateV1(reused, nil)) {
		t.Errorf("%s: the two documents' states differ after re-anchoring %d marks", name, len(anchors))
	}
	// MarkRange is a MarkWriter, so the two documents agreeing cannot show a mark the writer left
	// out: each mark is read back as well, one span over exactly the text its quote's range holds
	// when it was written, and absent when it was not.
	readBack, err := Read(reusedFragment)
	if err != nil {
		t.Fatalf("%s: read the re-anchored document back (%T)", name, err)
	}
	for index, ref := range refs {
		outcome := reusedOutcomes[index]
		written := outcome.findErr == nil && outcome.markErr == nil
		want, hasText := textWithin(target, outcome.r)
		spans := MarkSpans(readBack, ref.Type, ref.ID)
		if written && (!hasText || len(spans) != 1 || spans[0] != want) || !written && len(spans) != 0 {
			t.Errorf("%s: mark %d (%s): reads back as %d spans, first %v; written %t over the text at %v", name, index, ref.Type, len(spans), append(spans, Range{})[0], written, want)
		}
	}
	return stats
}

// textWithin is the part of r its text runs cover in doc: from the first text inside r to the end
// of the last. A range can start or end on a leaf or a block boundary, where no mark can lie.
func textWithin(doc *Node, r Range) (Range, bool) {
	covered, found := Range{}, false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if pos >= r.To {
			return false
		}
		if node.Type != "text" || end <= r.From || pos == end {
			return true
		}
		if !found {
			covered.From, found = max(pos, r.From), true
		}
		covered.To = min(end, r.To)
		return true
	})
	return covered, found
}

func TestReanchorReuseMatchesPerCall(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			checkReanchorReuse(t, document.name, document.markdown, 47, 47, false)
		})
	}
	for _, size := range realSizeDocuments {
		t.Run(size.name, func(t *testing.T) {
			stats := checkReanchorReuse(t, size.name, realSizeMarkdown(size.bytes, size.seed), 47, size.seed, false)
			if stats.reanchored == 0 || stats.missed == 0 {
				t.Fatalf("%d marks re-anchored and %d missed: want both", stats.reanchored, stats.missed)
			}
		})
	}
}

func TestReanchorReuseMatchesPerCallOnAnchorCorpus(t *testing.T) {
	for _, document := range anchorCorpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			var total reanchorStats
			for _, count := range []int{12, 47, 80} {
				stats := checkReanchorReuse(t, fmt.Sprintf("%s at %d marks", document.name, count), document.markdown, count, int64(count), true)
				total.placed += stats.placed
				total.reanchored += stats.reanchored
				total.missed += stats.missed
			}
			t.Logf("%s: %d marks placed, %d re-anchored, %d missed", document.name, total.placed, total.reanchored, total.missed)
		})
	}
}

// One finder answers every query as FindQuote answers it alone, however many queries it answered
// before, and FindDiscardingMiss differs from Find only where Find describes a miss.
func TestQuoteFinderAnswersLikeFindQuote(t *testing.T) {
	doc, err := Parse(strings.Join([]string{
		"# Title of the spec",
		"The quick brown fox jumps over the lazy dog. The quick brown fox sleeps.",
		"## Design notes",
		"A paragraph with **bold words** and *emphasis* and `code` and a [link](https://example.com).",
		"- the quick item\n- another item with spaces",
		"| Name | Value |\n| --- | --- |\n| café | 😀 |",
		"```go\nfmt.Println(\"quick\")\n```",
	}, "\n\n") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	at := func(value int) *int { return &value }
	queries := []struct {
		quote      string
		occurrence *int
		near       *int
	}{
		{quote: "quick brown fox"},
		{quote: "quick brown fox", occurrence: at(0)},
		{quote: "quick brown fox", occurrence: at(1)},
		{quote: "quick brown fox", occurrence: at(2)},
		{quote: "quick brown fox", occurrence: at(-1)},
		{quote: "quick brown fox", near: at(60)},
		{quote: "lazy dog"},
		{quote: "lazy \n  dog"},
		{quote: "  another   item\twith spaces "},
		{quote: "**bold words** and *emphasis*"},
		{quote: "and `code` and a [link](https://example.com)"},
		{quote: "## Design notes"},
		{quote: "### Title of the spec"},
		{quote: "## Missing heading"},
		{quote: "nothing like this"},
		{quote: "quick", near: at(200)},
		{quote: "quick", occurrence: at(3)},
		{quote: "quick", occurrence: at(9)},
		{quote: "café 😀"},
		{quote: "fmt.Println"},
		{quote: ""},
		{quote: "# "},
	}
	finder := NewQuoteFinder(doc)
	var hits, misses, ambiguous int
	for round := range 2 {
		for index, query := range queries {
			wantRange, wantErr := FindQuote(doc, query.quote, query.occurrence, query.near)
			gotRange, gotErr := finder.Find(query.quote, query.occurrence, query.near)
			if gotRange != wantRange || !reflect.DeepEqual(gotErr, wantErr) {
				t.Errorf("round %d, query %d (%q): Find = %v, %v; FindQuote = %v, %v", round, index, query.quote, gotRange, gotErr, wantRange, wantErr)
			}
			discardedRange, discardedErr := finder.FindDiscardingMiss(query.quote, query.occurrence, query.near)
			var miss *ErrQuoteNotFound
			var choice *ErrTargetAmbiguous
			switch {
			case errors.As(wantErr, &miss):
				misses++
				if discardedErr != ErrTargetNotFound || discardedRange != (Range{}) {
					t.Errorf("round %d, query %d (%q): FindDiscardingMiss of a miss = %v, %v; want the bare ErrTargetNotFound", round, index, query.quote, discardedRange, discardedErr)
				}
				continue
			case errors.As(wantErr, &choice):
				ambiguous++
			case wantErr == nil:
				hits++
			}
			if discardedRange != wantRange || !reflect.DeepEqual(discardedErr, wantErr) {
				t.Errorf("round %d, query %d (%q): FindDiscardingMiss = %v, %v; FindQuote = %v, %v", round, index, query.quote, discardedRange, discardedErr, wantRange, wantErr)
			}
		}
	}
	if hits == 0 || misses == 0 || ambiguous == 0 {
		t.Fatalf("%d hits, %d misses, %d ambiguous answers: want each", hits, misses, ambiguous)
	}

	for _, test := range []struct {
		name string
		doc  *Node
	}{
		{name: "a nil document"},
		{name: "an invalid document", doc: &Node{Type: "doc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			finder := NewQuoteFinder(test.doc)
			for round := range 2 {
				_, want := FindQuote(test.doc, "quick", nil, nil)
				if want == nil {
					t.Fatal("FindQuote answered; want a refusal")
				}
				if _, got := finder.Find("quick", nil, nil); !reflect.DeepEqual(got, want) {
					t.Errorf("round %d: Find refused with %v, FindQuote with %v", round, got, want)
				}
				if _, got := finder.FindDiscardingMiss("quick", nil, nil); !reflect.DeepEqual(got, want) {
					t.Errorf("round %d: FindDiscardingMiss refused with %v, FindQuote with %v", round, got, want)
				}
			}
		})
	}
}

// A writer refuses a mark with MarkRange's error, checked in MarkRange's order: the range, the
// mark's type, its attributes' nesting, then the fragment's text ranges, read only once those
// checks pass.
func TestMarkWriterRefusesInMarkRangesOrder(t *testing.T) {
	comment := Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice"}}
	nested := Attrs{"id": "c1", "nested": crafted.NestedValue(expectedMaxMarkAttrNesting + 1)}
	// misaligned writes a paragraph "abc" ending in a hard break that holds text, which
	// yTextRanges refuses, and returns the hard break.
	misaligned := func(t *testing.T) (*crdt.Doc, *crdt.YXmlFragment, *crdt.YXmlElement) {
		t.Helper()
		doc := crdt.New()
		frag := doc.GetXmlFragment("prosemirror")
		leaf := crdt.NewYXmlElement("hardbreak")
		doc.Transact(func(txn *crdt.Transaction) {
			paragraph := crdt.NewYXmlElement("paragraph")
			frag.InsertElement(txn, 0, paragraph)
			run := crdt.NewYXmlText()
			paragraph.InsertText(txn, 0, run)
			run.Insert(txn, 0, "abc", nil)
			paragraph.InsertElement(txn, 1, leaf)
			held := crdt.NewYXmlText()
			leaf.InsertText(txn, 0, held)
			held.Insert(txn, 0, "zz", nil)
		})
		if _, err := yTextRanges(frag); !errors.Is(err, ErrSchema) {
			t.Fatalf("the misaligned fragment reads with %v, want a refusal", err)
		}
		return doc, frag, leaf
	}
	checkSame := func(t *testing.T, what string, got, want error, wantText string) {
		t.Helper()
		if want == nil || !strings.Contains(want.Error(), wantText) {
			t.Fatalf("%s: MarkRange refused with %v, want %q", what, want, wantText)
		}
		if !sameError(got, want) || errors.Is(got, ErrSchema) != errors.Is(want, ErrSchema) {
			t.Errorf("%s: the writer refused with %v, MarkRange with %v", what, got, want)
		}
	}

	t.Run("the checks precede the fragment's read", func(t *testing.T) {
		doc, frag, leaf := misaligned(t)
		writer := NewMarkWriter(frag)
		doc.Transact(func(txn *crdt.Transaction) {
			for _, check := range []struct {
				what  string
				txn   *crdt.Transaction
				r     Range
				mark  Mark
				error string
			}{
				{what: "no transaction", r: Range{From: 1, To: 2}, mark: Mark{Type: "bogus", Attrs: nested}, error: "invalid mark range"},
				{what: "an empty range", txn: txn, r: Range{From: 2, To: 2}, mark: Mark{Type: "bogus", Attrs: nested}, error: "invalid mark range"},
				{what: "an unknown type", txn: txn, r: Range{From: 1, To: 2}, mark: Mark{Type: "bogus", Attrs: nested}, error: `mark "bogus"`},
				{what: "nested attributes", txn: txn, r: Range{From: 1, To: 2}, mark: Mark{Type: "proofComment", Attrs: nested}, error: "nests more than"},
			} {
				checkSame(t, check.what, writer.Mark(check.txn, check.r, check.mark), MarkRange(check.txn, frag, check.r, check.mark), check.error)
			}
			// The hard break no longer holds text, so the fragment reads; a writer that had read
			// it during the refused marks would still hold its refusal.
			leaf.Delete(txn, 0, 1)
			if err := writer.Mark(txn, Range{From: 1, To: 3}, comment); err != nil {
				t.Fatalf("the first mark that passes the checks: %v", err)
			}
		})
		tree, err := Read(frag)
		if err != nil {
			t.Fatal(err)
		}
		if r, quote, ok := FindMark(tree, "proofComment", "c1"); !ok || r != (Range{From: 1, To: 3}) || quote != "ab" {
			t.Fatalf("FindMark = %v %q %t, want {1 3} \"ab\" true", r, quote, ok)
		}
	})

	t.Run("a refused read refuses every mark", func(t *testing.T) {
		doc, frag, _ := misaligned(t)
		writer := NewMarkWriter(frag)
		doc.Transact(func(txn *crdt.Transaction) {
			want := MarkRange(txn, frag, Range{From: 1, To: 2}, comment)
			checkSame(t, "the first mark", writer.Mark(txn, Range{From: 1, To: 2}, comment), want, "has no ProseMirror text node")
			checkSame(t, "a later mark", writer.Mark(txn, Range{From: 2, To: 4}, comment), want, "has no ProseMirror text node")
			checkSame(t, "a later empty range", writer.Mark(txn, Range{From: 3, To: 3}, comment), MarkRange(txn, frag, Range{From: 3, To: 3}, comment), "invalid mark range")
		})
	})

	t.Run("refusals after the read", func(t *testing.T) {
		tree, err := Parse("Text\n\n```\ncode\n```\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag := writeTree(t, tree)
		code, err := FindQuote(tree, "code", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ask := Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "q1"}}
		writer := NewMarkWriter(frag)
		doc.Transact(func(txn *crdt.Transaction) {
			checkSame(t, "a mark the code block does not allow", writer.Mark(txn, code, ask), MarkRange(txn, frag, code, ask), "not allowed in code_block")
			past := Range{From: Size(tree) + 1, To: Size(tree) + 3}
			if got, want := writer.Mark(txn, past, comment), MarkRange(txn, frag, past, comment); got != ErrTargetNotFound || want != ErrTargetNotFound {
				t.Errorf("a range past the text: the writer refused with %v, MarkRange with %v; want ErrTargetNotFound", got, want)
			}
		})
	})

	t.Run("a nil fragment", func(t *testing.T) {
		doc := crdt.New()
		doc.Transact(func(txn *crdt.Transaction) {
			checkSame(t, "a nil fragment", NewMarkWriter(nil).Mark(txn, Range{From: 1, To: 2}, comment), MarkRange(txn, nil, Range{From: 1, To: 2}, comment), "invalid mark range")
		})
	})
}
