package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/stacktest"
)

const (
	expectedMaxNesting         = 100
	expectedMaxInlineNesting   = 100
	expectedMaxTreeDepth       = 1_000
	expectedMaxNodeAttrNesting = 100
	expectedMaxMarkAttrNesting = 99
)

// A markdown document opens at most 100 blocks inside one another, however the nesting is written:
// all on one line, or one level more on each line, where the line before still holds an open
// paragraph the next level interrupts. The same parser handles a whole upload and the fragments
// written by edits and accepted suggestions.
func TestParseRefusesMarkdownNestedPastTheBound(t *testing.T) {
	parsers := blockMarkdownReaders()
	onOneLine := func(open string) func(int) string {
		return func(levels int) string { return strings.Repeat(open, levels) + "a" }
	}
	onePerLine := func(level func(int) string) func(int) string {
		return func(levels int) string {
			lines := make([]string, 0, levels)
			for depth := 1; depth <= levels; depth++ {
				lines = append(lines, level(depth))
			}
			return strings.Join(lines, "\n")
		}
	}
	firstLine := func(int) int { return 1 }
	lastLine := func(levels int) int { return levels }
	for _, test := range []struct {
		name string
		// markdown nests levels of its shape, each opening blocks blocks; the level past the bound
		// opens on line(levels).
		markdown func(levels int) string
		blocks   int
		line     func(levels int) int
	}{
		{name: "quotes", markdown: onOneLine("> "), blocks: 1, line: firstLine},
		{name: "bare quotes", markdown: onOneLine(">"), blocks: 1, line: firstLine},
		{name: "lists", markdown: onOneLine("- "), blocks: 2, line: firstLine},
		{name: "callouts", markdown: nestedCallouts, blocks: 1, line: lastLine},
		{name: "quotes one per line", markdown: onePerLine(func(depth int) string {
			return strings.Repeat("> ", depth) + "a"
		}), blocks: 1, line: lastLine},
		{name: "lists one per line", markdown: onePerLine(func(depth int) string {
			return strings.Repeat("  ", depth-1) + "- a"
		}), blocks: 2, line: lastLine},
	} {
		t.Run(test.name, func(t *testing.T) {
			levels := expectedMaxNesting / test.blocks
			deepest, over := test.markdown(levels), test.markdown(levels+1)
			refusal := fmt.Sprintf("line %d opens a block inside %d blocks", test.line(levels+1), expectedMaxNesting)
			for _, reader := range parsers {
				t.Run(reader.name, func(t *testing.T) {
					if _, err := reader.parse(deepest); err != nil {
						t.Fatalf("%d nested blocks: %v, want them read", expectedMaxNesting, err)
					}
					_, err := reader.parse(over)
					if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
						t.Fatalf("%d nested blocks: %v, want ErrSchema saying %q", expectedMaxNesting+test.blocks, err, refusal)
					}
				})
			}
		})
	}
}

// The bound counts the blocks a new block would open inside, and only blocks a parser opens: the
// next block after 100 nested quotes stands at the document's level, and text in the deepest quote
// that a container's parser is offered but does not open - `-x`, `1x`, a link, an emoji shortcode
// - reads as a paragraph there.
func TestParseReadsWhatOpensNoBlockPastTheBound(t *testing.T) {
	deepest := strings.Repeat("> ", expectedMaxNesting)
	for _, test := range []struct {
		markdown string
		// blocks is how many blocks the document holds at its own level.
		blocks int
	}{
		{markdown: deepest + "a\n- b", blocks: 2},
		{markdown: deepest + "a\n\n- b", blocks: 2},
		{markdown: deepest + "a\n# b", blocks: 2},
		{markdown: deepest + "-x", blocks: 1},
		{markdown: deepest + "1x", blocks: 1},
		{markdown: deepest + "[link](https://example.com)", blocks: 1},
		{markdown: deepest + ":smile:", blocks: 1},
		{markdown: deepest + "a\n" + deepest + "-x", blocks: 1},
	} {
		for _, reader := range blockMarkdownReaders() {
			doc, err := reader.parse(test.markdown)
			if err != nil {
				t.Fatalf("%s of %q after %d nested quotes: %v, want it read", reader.name, test.markdown[len(deepest):], expectedMaxNesting, err)
			}
			if len(doc.Children) != test.blocks {
				t.Fatalf("%s of %q after %d nested quotes read as %d blocks at the document's level, want %d", reader.name, test.markdown[len(deepest):], expectedMaxNesting, len(doc.Children), test.blocks)
			}
		}
	}
}

// A list opens with its first item, so a list whose item would stand inside 100 blocks is refused
// with it, naming the list's line, rather than opened without the item: goldmark's list parser
// panics on an itemless list at the list's next line. A list with room for its item reads.
func TestParseRefusesAListWithNoRoomForItsItem(t *testing.T) {
	quotes := func(depth int) string { return strings.Repeat("> ", depth) }
	for _, test := range []struct {
		name string
		// markdown is a list inside depth quotes, followed by a line it reads.
		markdown func(depth int) string
	}{
		{name: "next item", markdown: func(depth int) string { return quotes(depth) + "- a\n" + quotes(depth) + "- b\n" }},
		{name: "continued item", markdown: func(depth int) string { return quotes(depth) + "- a\n" + quotes(depth) + "  b\n" }},
		{name: "ordered list", markdown: func(depth int) string { return quotes(depth) + "1. a\n" + quotes(depth) + "2. b\n" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, reader := range blockMarkdownReaders() {
				t.Run(reader.name, func(t *testing.T) {
					if _, err := reader.parse(test.markdown(expectedMaxNesting - 2)); err != nil {
						t.Fatalf("a list and its item inside %d quotes: %v, want them read", expectedMaxNesting-2, err)
					}
					_, err := reader.parse(test.markdown(expectedMaxNesting - 1))
					refusal := fmt.Sprintf("line 1 opens a block inside %d blocks", expectedMaxNesting)
					if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
						t.Fatalf("a list and its item inside %d quotes: %v, want ErrSchema saying %q", expectedMaxNesting-1, err, refusal)
					}
				})
			}
		})
	}
}

// An unsupported directive opens a leaf, which the block bound does not count: in the deepest quote
// the bound allows it is refused for what it is, as it is anywhere else.
func TestParseRefusesAnUnsupportedDirectiveInTheDeepestQuoteForWhatItIs(t *testing.T) {
	deepest := strings.Repeat("> ", expectedMaxNesting)
	for _, test := range []struct{ directive, reason string }{
		{directive: ":::bogus", reason: malformedDirectiveReason},
		{directive: "::leaf", reason: "leaf directives (::name) are not supported"},
		{directive: ":text{a}", reason: "text directives (:name{...}) are not supported"},
	} {
		for _, reader := range blockMarkdownReaders() {
			_, err := reader.parse(deepest + test.directive)
			if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), test.reason) {
				t.Errorf("%s of %q in the %dth quote: %v, want ErrSchema saying %q", reader.name, test.directive, expectedMaxNesting, err, test.reason)
			}
		}
	}
}

// The renderer reads a run back with footnote definitions after it (parseInlineWithDefinitions),
// through a parser whose definition parser holds the block bound. Definitions nested past it are
// refused there, as every other reader refuses them, rather than read as the paragraph's text.
func TestInlineReadBackRefusesDefinitionsNestedPastTheBound(t *testing.T) {
	definitions := func(depth int) string { return "a\n\n" + strings.Repeat("[^a]: ", depth) + "x" }
	if _, err := parseInlineWithDefinitions(definitions(expectedMaxNesting), []string{"a"}, inlineMarkdown); err != nil {
		t.Fatalf("a run before %d nested definitions: %v, want it read", expectedMaxNesting, err)
	}
	_, err := parseInlineWithDefinitions(definitions(expectedMaxNesting+1), []string{"a"}, inlineMarkdown)
	refusal := fmt.Sprintf("line 3 opens a block inside %d blocks", expectedMaxNesting)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("a run before %d nested definitions: %v, want ErrSchema saying %q", expectedMaxNesting+1, err, refusal)
	}
}

// callerReader is one reader of caller markdown, by name.
type callerReader struct {
	name  string
	parse func(string) (*Node, error)
}

// blockMarkdownReaders is every reader of caller markdown that opens blocks: a whole upload, and
// the fragments written by edits and accepted suggestions.
func blockMarkdownReaders() []callerReader {
	return []callerReader{
		{name: "document", parse: Parse},
		{name: "upload", parse: func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
		{name: "fragment", parse: func(markdown string) (*Node, error) {
			return ParseFragment(markdown, true, NewWriteBudget())
		}},
	}
}

// A textblock's inline markdown opens at most 100 marks inside one another. Every reader of
// caller markdown refuses the same run: an upload, an edit fragment and an accepted suggestion
// through the document readers, and a replacement's inline markdown through ParseInline. A link
// and an image are marks too, and an image whose own marks pass the bound, which goldmark's parse
// refuses as it makes the image, is refused for the same line as any other run: the first one past
// the bound, wherever goldmark meets it first.
func TestParseRefusesInlineMarksNestedPastTheBound(t *testing.T) {
	// Two `*` either side open one mark.
	nested := func(marks int) string { return strings.Repeat("**", marks) + "x" + strings.Repeat("**", marks) }
	imageInLink := func(marks int) string { return "[![" + nested(marks) + "](u)](v)" }
	for _, reader := range inlineMarkReaders() {
		t.Run(reader.name, func(t *testing.T) {
			for _, markdown := range []string{nested(expectedMaxInlineNesting), imageInLink(expectedMaxInlineNesting - 2)} {
				if _, err := reader.parse(markdown); err != nil {
					t.Fatalf("%.40q…, nested %d marks deep: %v, want it read", markdown, expectedMaxInlineNesting, err)
				}
			}
			for _, test := range []struct {
				markdown string
				line     int
			}{
				{markdown: nested(expectedMaxInlineNesting + 1), line: 1},
				{markdown: imageInLink(expectedMaxInlineNesting - 1), line: 1},
				{markdown: imageInLink(expectedMaxInlineNesting), line: 1},
				{markdown: "a\n\n" + imageInLink(10_000), line: 3},
				{markdown: imageInLink(10_000) + "\n\n" + nested(expectedMaxInlineNesting+1), line: 1},
				{markdown: nested(expectedMaxInlineNesting+1) + "\n\n" + imageInLink(10_000), line: 1},
				// goldmark reads a footnote definition's text before the rest.
				{markdown: nested(expectedMaxInlineNesting+1) + "\n\n[^a]: " + imageInLink(10_000), line: 1},
				{markdown: imageInLink(10_000) + "\n\n[^a]: " + imageInLink(10_000), line: 1},
			} {
				_, err := reader.parse(test.markdown)
				if want := fmt.Sprintf("line %d starts text nested inside more than 100 inline marks", test.line); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), want) {
					t.Errorf("%.40q…: %v, want ErrSchema saying %q", test.markdown, err, want)
				}
			}
		})
	}
}

// The inline bound is what keeps the readers' recursion off a stack that grows with the caller's
// nesting: every walk through a textblock's marks takes a frame per level, the one through a table
// cell's code spans among them, and so does goldmark's own walk through a link's label while it
// parses, which enters every image the label holds. Under a stack far below the default, a run
// that nests ten thousand times past the bound is refused rather than recursed into: in a
// paragraph, in a cell holding an escaped pipe after a backtick, and in an image inside a link.
func TestInlineMarksNestNoDeeperThanTheBoundUnderASmallStack(t *testing.T) {
	stacktest.Under(t, 16<<20, func(t *testing.T) {
		run := strings.Repeat("*", 2_000_000)
		for _, shape := range []struct{ name, markdown string }{
			{name: "paragraph", markdown: run + "x" + run},
			{name: "table cell", markdown: "| a |\n| --- |\n| `x\\| " + run + "x" + run + " |\n"},
			{name: "image in a link", markdown: "[![" + run + "x" + run + "](u)](v)"},
		} {
			for _, reader := range inlineMarkReaders() {
				t.Run(shape.name+"/"+reader.name, func(t *testing.T) {
					if _, err := reader.parse(shape.markdown); !errors.Is(err, ErrSchema) {
						t.Fatalf("a million nested inline marks: %v, want ErrSchema", err)
					}
				})
			}
		}
	})
}

// The nesting an ordinary document reaches - emphasis, strikethrough and a code span inside
// strong, and marks inside a link - is far below the bound, and reads and writes back unchanged.
func TestOrdinaryNestedInlineMarksReadAndRenderUnchanged(t *testing.T) {
	for _, markdown := range []string{
		"**strong *and emphasis*, ~~struck~~ and `code`**\n",
		"[**a *b***](https://example.com)\n",
		"> **strong *and emphasis* and ~~struck~~**\n",
	} {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("read %q: %v", markdown, err)
		}
		written, err := Render(doc)
		if err != nil {
			t.Fatalf("write %q: %v", markdown, err)
		}
		if written != markdown {
			t.Fatalf("%q wrote back as %q", markdown, written)
		}
	}
}

// inlineMarkReaders is every reader of caller markdown that reaches the inline parser: the block
// readers, and a replacement's inline markdown through ParseInline, which reads no document.
func inlineMarkReaders() []callerReader {
	return append(blockMarkdownReaders(), callerReader{name: "inline", parse: func(markdown string) (*Node, error) {
		_, err := ParseInline(markdown, NewWriteBudget())
		return nil, err
	}})
}

// nestedCallouts is depth callouts inside one another, with the outermost fenced with the most colons.
func nestedCallouts(depth int) string {
	var markdown strings.Builder
	for level := depth; level >= 1; level-- {
		markdown.WriteString(strings.Repeat(":", level+2) + `callout{kind="note" title="T"}` + "\n")
	}
	markdown.WriteString("a\n")
	for level := 1; level <= depth; level++ {
		markdown.WriteString(strings.Repeat(":", level+2) + "\n")
	}
	return markdown.String()
}

// A megabyte of bare quote markers is a million nested quotes, a tree deep enough to overflow the
// process stack in the document pipeline's recursive walks. It is refused in bounded time, before
// the parser builds that tree for them to enter.
func TestParseRefusesAMebibyteOfQuotesInBoundedTime(t *testing.T) {
	started := time.Now()
	_, err := Parse(strings.Repeat(">", 1<<20))
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("1 MiB of `>`: %v, want ErrSchema", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("refusing 1 MiB of `>` took %s, want under 10 s", elapsed)
	}
}

// A tree from any source is outside the schema when a node stands more than 1,000 levels below the
// document. The CRDT chains below use the same Yjs element insertion a crafted browser client can
// send, not Update, because Update validates the server-authored tree before it writes it.
//
// The bound is one every reader serves: at it, with the deepest node's attribute nested as far as
// the schema allows, the document token is JSON encoding/json still reads, which it is not past
// 10,000 nested arrays and objects.
func TestTreesDeeperThanTheBoundAreOutsideTheSchema(t *testing.T) {
	// Both halves count a tree's depth as its text's level, the unit MaxTreeDepth bounds and
	// docstest.WriteDeepChain writes: the document is level 0, the blockquotes levels 1 through
	// textLevel-2, and the paragraph level textLevel-1.
	chain := func(textLevel int) *Node {
		text := &Node{Type: "text", Text: "a", Marks: []Mark{{
			Type: "proofComment", Attrs: Attrs{"id": "c1", "nested": nestedValue(expectedMaxMarkAttrNesting)},
		}}}
		node := &Node{Type: "paragraph", Children: []*Node{text}}
		for range textLevel - 2 {
			node = &Node{Type: "blockquote", Children: []*Node{node}}
		}
		return &Node{Type: "doc", Children: []*Node{node}}
	}

	deepest := chain(expectedMaxTreeDepth)
	if err := deepest.Validate(); err != nil {
		t.Fatalf("a tree %d deep: %v, want it valid", expectedMaxTreeDepth, err)
	}
	if _, err := Render(deepest); err != nil {
		t.Fatalf("render a tree %d deep: %v", expectedMaxTreeDepth, err)
	}
	token, err := deepest.TokenJSON()
	if err != nil || !json.Valid(token) {
		t.Fatalf("token of a tree %d deep: valid JSON %t, %v; want JSON encoding/json reads", expectedMaxTreeDepth, json.Valid(token), err)
	}
	over := chain(expectedMaxTreeDepth + 1)
	if err := over.Validate(); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), fmt.Sprint(expectedMaxTreeDepth)) {
		t.Fatalf("a tree %d deep: %v, want ErrSchema naming the bound", expectedMaxTreeDepth+1, err)
	}

	for _, test := range []struct {
		name      string
		textLevel int
		want      error
	}{
		{name: "at the bound", textLevel: expectedMaxTreeDepth},
		{name: "over the bound", textLevel: expectedMaxTreeDepth + 1, want: ErrSchema},
	} {
		t.Run(test.name, func(t *testing.T) {
			ydoc := crdt.New()
			fragment := ydoc.GetXmlFragment("prosemirror")
			ydoc.Transact(func(txn *crdt.Transaction) {
				docstest.WriteDeepChain(txn, fragment, test.textLevel, "a")
			})
			_, err := Read(fragment)
			if test.want == nil && err != nil {
				t.Fatalf("read back a chain %d deep: %v, want it valid", test.textLevel, err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("read back a chain %d deep: %v, want %v", test.textLevel, err, test.want)
			}
		})
	}
}

// A CRDT update can contain far more nodes than a markdown document. Read stops at the schema
// bound, rather than recursing through a million-element chain and overflowing the process stack.
func TestReadRefusesAMillionLevelCRDTTreeWithoutOverflow(t *testing.T) {
	ydoc := crdt.New()
	fragment := ydoc.GetXmlFragment("prosemirror")
	ydoc.Transact(func(txn *crdt.Transaction) {
		docstest.WriteDeepChain(txn, fragment, 1_000_000, "a")
	})

	_, err := Read(fragment)
	if past := fmt.Sprintf("a node %d levels deep", expectedMaxTreeDepth+1); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), past) {
		t.Fatalf("read a million-level CRDT tree: %v, want ErrSchema at level %d", err, expectedMaxTreeDepth+1)
	}
}

// The walks over a live tree that do not read it through Read first - the text a mark covers
// (MarkRange, Unmark) and the text a client wrote (AuthoredTextRuns) - meet the depth bound where
// Read does: a tree at the bound is walked, one whose text alone stands one level past it is
// refused, and under a stack far below the default, a tree a peer wrote far past it is refused
// rather than recursed through.
func TestLiveTreeWalksStopAtTheDepthBoundUnderASmallStack(t *testing.T) {
	stacktest.Under(t, 16<<20, func(t *testing.T) {
		for _, test := range []struct {
			name      string
			textLevel int
			refused   bool
		}{
			{name: "at the bound", textLevel: expectedMaxTreeDepth},
			{name: "its text one past the bound", textLevel: expectedMaxTreeDepth + 1, refused: true},
			{name: "far past the bound", textLevel: 300_000, refused: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				ydoc := crdt.New()
				fragment := ydoc.GetXmlFragment("prosemirror")
				ydoc.Transact(func(txn *crdt.Transaction) {
					docstest.WriteDeepChain(txn, fragment, test.textLevel, "a")
				})
				// The chain's text follows the openings of the textLevel-1 elements holding it.
				text := Range{From: test.textLevel - 1, To: test.textLevel}
				comment := Mark{Type: "proofComment", Attrs: Attrs{"id": "c1"}}
				var marked, unmarked error
				ydoc.Transact(func(txn *crdt.Transaction) {
					marked = MarkRange(txn, fragment, text, comment)
					unmarked = Unmark(txn, fragment, comment.Type, "c1")
				})
				_, authored := AuthoredTextRuns(fragment, ydoc.ClientID(), 0, nil)
				_, read := Read(fragment)
				past := fmt.Sprintf("a node %d levels deep", expectedMaxTreeDepth+1)
				for _, walk := range []struct {
					name string
					err  error
				}{{"Read", read}, {"MarkRange", marked}, {"Unmark", unmarked}, {"AuthoredTextRuns", authored}} {
					if test.refused && (!errors.Is(walk.err, ErrSchema) || !strings.Contains(walk.err.Error(), past)) {
						t.Errorf("%s of a chain %d deep: %v, want ErrSchema saying %q", walk.name, test.textLevel, walk.err, past)
					}
					if !test.refused && walk.err != nil {
						t.Errorf("%s of a chain %d deep: %v, want it walked", walk.name, test.textLevel, walk.err)
					}
				}
			})
		}
	})
}

// An attribute's value nests at most 100 arrays and objects on a node and 99 on a mark, whose
// attributes ygo stores as one more map. A crafted client writes a mark's attributes as JSON, which
// ygo decodes as deep as encoding/json reads, so Read meets the bound on the live document as
// Validate does on a tree. That document is built from the update such a client sends: from
// v1.51.1 ygo's own YText writers panic on a value past its 100 levels, so only an update can put
// one there.
func TestAttributesNestedPastTheBoundAreOutsideTheSchema(t *testing.T) {
	check := func(t *testing.T, what string, nesting, bound int, err error) {
		t.Helper()
		if nesting <= bound && err != nil {
			t.Fatalf("%s nesting %d: %v, want it valid", what, nesting, err)
		}
		if nesting > bound && (!errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), fmt.Sprintf("more than %d arrays", bound))) {
			t.Fatalf("%s nesting %d: %v, want ErrSchema naming the bound", what, nesting, err)
		}
	}
	for _, nesting := range []int{expectedMaxNodeAttrNesting, expectedMaxNodeAttrNesting + 1} {
		t.Run(fmt.Sprintf("a node attribute nesting %d", nesting), func(t *testing.T) {
			node := &Node{Type: "doc", Children: []*Node{{Type: "heading", Attrs: Attrs{"level": float64(1), "nested": nestedValue(nesting)}, Children: []*Node{{Type: "text", Text: "a"}}}}}
			check(t, "a node attribute", nesting, expectedMaxNodeAttrNesting, node.Validate())
		})
	}
	for _, nesting := range []int{expectedMaxMarkAttrNesting, expectedMaxMarkAttrNesting + 1} {
		t.Run(fmt.Sprintf("a mark attribute nesting %d", nesting), func(t *testing.T) {
			check(t, "a mark attribute", nesting, expectedMaxMarkAttrNesting, linkedText(nestedValue(nesting)).Validate())
			_, err := Read(liveLinkedText(t, nestedValue(nesting)).GetXmlFragment("prosemirror"))
			check(t, "a live mark attribute", nesting, expectedMaxMarkAttrNesting, err)
		})
	}
}

// linkedText is a document of one paragraph whose text carries a link holding nested as an
// attribute.
func linkedText(nested any) *Node {
	return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type: "text", Text: "a", Marks: []Mark{{Type: "link", Attrs: linkAttrs(nested)}},
	}}}}}
}

func linkAttrs(nested any) Attrs {
	return Attrs{"href": "https://example.com", "nested": nested}
}

// liveLinkedText is the live document a crafted client makes of linkedText(nested): a paragraph's
// text, then the update that links it with nested, which ygo's own writers would refuse past their
// bound.
func liveLinkedText(t *testing.T, nested any) *crdt.Doc {
	t.Helper()
	writer := crdt.New()
	fragment := writer.GetXmlFragment("prosemirror")
	writer.Transact(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, 0, paragraph)
		text := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "a", nil)
	})
	received := crdt.New()
	if err := crdt.ApplyUpdateV1(received, crdt.EncodeStateAsUpdateV1(writer, nil), nil); err != nil {
		t.Fatalf("apply the client's text: %v", err)
	}
	if err := crdt.ApplyUpdateV1(received, docstest.NestedLinkUpdate(t, writer, nested), nil); err != nil {
		t.Fatalf("apply the client's link: %v", err)
	}
	return received
}

// Every writer that hands ygo a value it checks for depth - Update's ApplyDelta for new text, its
// Format for marks on text it keeps, its element attributes, and MarkRange's Format - writes a
// value at the schema's bound and refuses one past it as outside the schema, before ygo sees it.
// From v1.51.1 ygo panics on a value past its own bound, which a mark at 100 levels inside its
// attributes map was, and an element attribute past it is one ygo writes but no peer can read back.
func TestEveryWriterKeepsAttributeValuesWithinYgosDepth(t *testing.T) {
	heading := func(text *Node, attrs Attrs) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "heading", Attrs: attrs, Children: []*Node{text}}}}
	}
	plainText := func() *Node { return &Node{Type: "text", Text: "a"} }
	linked := func(nesting int) *Node {
		return &Node{Type: "text", Text: "a", Marks: []Mark{{Type: "link", Attrs: linkAttrs(nestedValue(nesting))}}}
	}
	level := func(nested any) Attrs { return Attrs{"level": float64(1), "nested": nested} }
	for _, test := range []struct {
		name  string
		bound int
		// write writes a value nesting nesting deep into a fresh document, in one transaction.
		write func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error
	}{
		{"Update inserting marked text", expectedMaxMarkAttrNesting, func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error {
			return Update(txn, fragment, heading(linked(nesting), level(nil)))
		}},
		{"Update marking text it keeps", expectedMaxMarkAttrNesting, func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error {
			if err := Update(txn, fragment, heading(plainText(), level(nil))); err != nil {
				t.Fatalf("write the unmarked text: %v", err)
			}
			return Update(txn, fragment, heading(linked(nesting), level(nil)))
		}},
		{"Update writing a node attribute", expectedMaxNodeAttrNesting, func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error {
			return Update(txn, fragment, heading(plainText(), level(nestedValue(nesting))))
		}},
		{"Update changing a node attribute", expectedMaxNodeAttrNesting, func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error {
			if err := Update(txn, fragment, heading(plainText(), level(nil))); err != nil {
				t.Fatalf("write the heading: %v", err)
			}
			return Update(txn, fragment, heading(plainText(), level(nestedValue(nesting))))
		}},
		{"MarkRange", expectedMaxMarkAttrNesting, func(t *testing.T, txn *crdt.Transaction, fragment *crdt.YXmlFragment, nesting int) error {
			if err := Update(txn, fragment, heading(plainText(), level(nil))); err != nil {
				t.Fatalf("write the text: %v", err)
			}
			return MarkRange(txn, fragment, Range{From: 1, To: 2}, Mark{Type: "link", Attrs: linkAttrs(nestedValue(nesting))})
		}},
	} {
		for _, nesting := range []int{test.bound, test.bound + 1} {
			t.Run(fmt.Sprintf("%s nesting %d", test.name, nesting), func(t *testing.T) {
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				var err error
				doc.Transact(func(txn *crdt.Transaction) { err = test.write(t, txn, fragment, nesting) })
				if nesting > test.bound {
					if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), fmt.Sprintf("more than %d arrays", test.bound)) {
						t.Fatalf("write nesting %d: %v, want ErrSchema naming the bound", nesting, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("write nesting %d: %v, want it written", nesting, err)
				}
				// What ygo wrote is what a peer reads back and what the document holds.
				received := crdt.New()
				if err := crdt.ApplyUpdateV1(received, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
					t.Fatalf("a peer applies the write: %v", err)
				}
				for name, document := range map[string]*crdt.Doc{"the writer": doc, "a peer": received} {
					if _, err := Read(document.GetXmlFragment("prosemirror")); err != nil {
						t.Fatalf("%s reads the write back: %v", name, err)
					}
				}
			})
		}
	}
}

// nestedValue is a string inside depth arrays nested one inside another.
func nestedValue(depth int) any {
	var value any = "x"
	for range depth {
		value = []any{value}
	}
	return value
}
