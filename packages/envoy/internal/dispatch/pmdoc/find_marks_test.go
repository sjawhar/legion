package pmdoc

import (
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// findMarksAbsentKeys are identities no document under test carries: FindMarks must leave them out
// of its result, as frozenFindMark finds nothing for them.
var findMarksAbsentKeys = []MarkRef{
	{Type: "proofComment", ID: "\x00findmarks-absent"},
	{Type: "dispatchAsk", ID: ""},
}

// frozenFindMark and frozenMarkAttrs are FindMark and MarkAttrs as they stood before both became
// lookups in FindMarks (commit 59f6647d): the base implementation, one walk per identity, copied
// verbatim but for their names and kept as the oracle FindMarks must agree with. frozenFindMark
// reads the production MarkSpans, which stays as it was: rejecting a suggestion needs every span.
func frozenFindMark(doc *Node, markType, id string) (Range, string, bool) {
	spans := MarkSpans(doc, markType, id)
	if len(spans) == 0 {
		return Range{}, "", false
	}
	marked := spans[0]
	var quote strings.Builder
	last := -1
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if pos >= marked.To {
			return false
		}
		if node.Type != "text" || pos < marked.From {
			return true
		}
		if last >= 0 && pos != last {
			quote.WriteByte(' ')
		}
		quote.WriteString(node.Text)
		last = end
		return true
	})
	return marked, nulAsReplacement(quote.String()), true
}

func frozenMarkAttrs(doc *Node, markType, id string) (Attrs, bool) {
	var attrs Attrs
	found := false
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if node.Type != "text" {
			return true
		}
		for _, mark := range node.Marks {
			if markHasID(mark, markType, id) {
				attrs = cloneAttrs(mark.Attrs)
				found = true
				return false
			}
		}
		return true
	})
	return attrs, found
}

// assertFindMarksMatchesOracle asks FindMarks for keys, two absent identities and the first key a
// second time, checks each against frozenFindMark and frozenMarkAttrs asked for that key alone, as
// it does FindMark and MarkAttrs, and returns how many distinct keys were found. Its failures name
// the document, the key's index and type, ranges, quote lengths and the first byte the quotes
// differ at, never the document's text: the anchor corpus it also checks is private.
func assertFindMarksMatchesOracle(t testing.TB, name string, doc *Node, keys []MarkRef) int {
	t.Helper()
	query := make([]MarkRef, 0, len(keys)+len(findMarksAbsentKeys)+1)
	query = append(query, keys...)
	query = append(query, findMarksAbsentKeys...)
	if len(keys) > 0 {
		query = append(query, keys[0])
	}
	got := FindMarks(doc, query)
	asked := make(map[MarkRef]bool, len(query))
	found := 0
	for index, key := range query {
		r, quote, ok := frozenFindMark(doc, key.Type, key.ID)
		attrs, attrsOK := frozenMarkAttrs(doc, key.Type, key.ID)
		if ok && !asked[key] {
			found++
		}
		asked[key] = true
		if oneRange, oneQuote, oneOK := FindMark(doc, key.Type, key.ID); oneRange != r || oneQuote != quote || oneOK != ok {
			t.Errorf("%s: key %d (%s): FindMark range %v, quote of %d bytes, found %t; the frozen FindMark range %v, quote of %d bytes, found %t",
				name, index, key.Type, oneRange, len(oneQuote), oneOK, r, len(quote), ok)
		}
		if oneAttrs, oneOK := MarkAttrs(doc, key.Type, key.ID); oneOK != attrsOK || !reflect.DeepEqual(oneAttrs, attrs) {
			t.Errorf("%s: key %d (%s): MarkAttrs attributes (%d of them, found %t) are not the frozen MarkAttrs' (%d of them, found %t)",
				name, index, key.Type, len(oneAttrs), oneOK, len(attrs), attrsOK)
		}
		hit, hitOK := got[key]
		if hitOK != ok {
			t.Errorf("%s: key %d (%s): FindMarks found it %t, the frozen FindMark %t", name, index, key.Type, hitOK, ok)
			continue
		}
		if !ok {
			continue
		}
		if hit.Range != r {
			t.Errorf("%s: key %d (%s): FindMarks range %v, the frozen FindMark range %v", name, index, key.Type, hit.Range, r)
		}
		if hit.Quote != quote {
			t.Errorf("%s: key %d (%s): FindMarks quote of %d bytes, the frozen FindMark quote of %d bytes, first differing at byte %d",
				name, index, key.Type, len(hit.Quote), len(quote), firstDifferingByte(hit.Quote, quote))
		}
		if !attrsOK || !reflect.DeepEqual(hit.Attrs, attrs) {
			t.Errorf("%s: key %d (%s): FindMarks attributes (%d of them) are not the frozen MarkAttrs' (%d of them, found %t)",
				name, index, key.Type, len(hit.Attrs), len(attrs), attrsOK)
		}
	}
	for key := range got {
		if !asked[key] {
			t.Errorf("%s: FindMarks answered a %s key it was not asked for", name, key.Type)
		}
	}
	return found
}

func firstDifferingByte(left, right string) int {
	limit := min(len(left), len(right))
	for index := range limit {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

// The trees here are built by hand to reach what a parse rarely makes: overlapping marks of one
// type, one identity twice on a run, non-string ids, leaf nodes and empty runs inside a span, and
// spans across list and table boundaries. Each states the quote it expects as well as asking the
// oracle, so a change to the frozen copies cannot pass unnoticed either.
func TestFindMarksMatchesFindMarkOnHandBuiltTrees(t *testing.T) {
	comment := func(id string) Mark { return Mark{Type: "proofComment", Attrs: Attrs{"id": id, "by": "user:alice"}} }
	run := func(text string, marks ...Mark) *Node { return &Node{Type: "text", Text: text, Marks: marks} }
	node := func(nodeType string, children ...*Node) *Node { return &Node{Type: nodeType, Children: children} }
	paragraph := func(children ...*Node) *Node { return node("paragraph", children...) }
	doc := func(children ...*Node) *Node { return node("doc", children...) }
	key := func(markType, id string) MarkRef { return MarkRef{Type: markType, ID: id} }
	c1, c2 := comment("c1"), comment("c2")

	type want struct {
		key   MarkRef
		quote string
		by    string
	}
	for _, test := range []struct {
		name string
		doc  *Node
		want []want
	}{
		{
			name: "two marks of one type overlap",
			doc:  doc(paragraph(run("quick ", c1), run("brown", c1, c2), run(" fox", c2))),
			want: []want{{key: key("proofComment", "c1"), quote: "quick brown"}, {key: key("proofComment", "c2"), quote: "brown fox"}},
		},
		{
			// A run carrying one identity twice counts once, and the attributes are the first's;
			// an identity listed behind another of its type on a run is still found there.
			name: "one identity twice on a run and one behind another of its type",
			doc: doc(paragraph(
				run("one", Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice"}}, Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:bob"}}),
				run(" two", comment("c0"), comment("c3")),
			)),
			want: []want{
				{key: key("proofComment", "c1"), quote: "one", by: "user:alice"},
				{key: key("proofComment", "c3"), quote: " two"},
				{key: key("proofComment", "c0"), quote: " two"},
			},
		},
		{
			name: "a mark split into several spans",
			doc:  doc(paragraph(run("a", c1), run("b"), run("c", c1)), paragraph(run("d", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "a"}},
		},
		{
			// An id that is not a string names no identity, so its run ends the span.
			name: "a non-string id",
			doc: doc(paragraph(
				run("x", comment("7")),
				run("y", Mark{Type: "proofComment", Attrs: Attrs{"id": float64(7), "by": "user:alice"}}),
				run("z", comment("7")),
			)),
			want: []want{{key: key("proofComment", "7"), quote: "x"}},
		},
		{
			name: "leaf nodes between runs",
			doc: doc(
				paragraph(run("a", c1), node("hardbreak"), run("b", c1), node("image"), run("c", c1), node("html"), run("d", c1), node("footnote_reference"), run("e", c1)),
				node("hr"),
				paragraph(run("f", c1)),
			),
			want: []want{{key: key("proofComment", "c1"), quote: "a b c d e f"}},
		},
		{
			name: "an empty marked run in another block",
			doc:  doc(paragraph(run("a", c1)), paragraph(run("", c1)), paragraph(run("b", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "a  b"}},
		},
		{
			name: "an empty marked run inside one block",
			doc:  doc(paragraph(run("a", c1), run("", c1), run("b", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "ab"}},
		},
		{
			name: "an empty unmarked run ends the span",
			doc:  doc(paragraph(run("a", c1), run(""), run("b", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "a"}},
		},
		{
			// The span runs to the empty run's position, which FindMark's quote walk stops at, so
			// no separator follows the text.
			name: "an empty marked run ending the span",
			doc:  doc(paragraph(run("a", c1)), paragraph(run("", c1)), paragraph(run("z"))),
			want: []want{{key: key("proofComment", "c1"), quote: "a"}},
		},
		{
			name: "an empty marked run ending the document",
			doc:  doc(paragraph(run("a", c1)), paragraph(run("", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "a"}},
		},
		{
			name: "across list items",
			doc: doc(node("bullet_list",
				node("list_item", paragraph(run("one", c1))),
				node("list_item", paragraph(run("two", c1)), node("ordered_list", node("list_item", paragraph(run("three", c1))))),
			)),
			want: []want{{key: key("proofComment", "c1"), quote: "one two three"}},
		},
		{
			name: "across table cells",
			doc: doc(node("table",
				node("table_header_row", node("table_header", paragraph(run("h1", c2))), node("table_header", paragraph(run("h2", c2)))),
				node("table_row", node("table_cell", paragraph(run("c1", c2))), node("table_cell", paragraph(run("x")))),
			)),
			want: []want{{key: key("proofComment", "c2"), quote: "h1 h2 c1"}},
		},
		{
			name: "a NUL",
			doc:  doc(paragraph(run("a\x00b", c1))),
			want: []want{{key: key("proofComment", "c1"), quote: "a\uFFFDb"}},
		},
		{
			name: "astral characters",
			doc:  doc(paragraph(run("pre 😀 "), run("😀𝔸", c1), run(" 𝔸 post", c2))),
			want: []want{{key: key("proofComment", "c1"), quote: "😀𝔸"}, {key: key("proofComment", "c2"), quote: " 𝔸 post"}},
		},
		{
			// Identities of every anchor type, asked out of document order, open and close at
			// different runs.
			name: "identities of every type",
			doc: doc(
				paragraph(
					run("a", Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "q1"}}),
					run("b", Mark{Type: "dispatchAsk", Attrs: Attrs{"id": "q1"}}, c1),
					run("c", c1, Mark{Type: "proofSuggestion", Attrs: Attrs{"id": "s1", "kind": "replace"}}),
				),
				paragraph(run("d", Mark{Type: "proofSuggestion", Attrs: Attrs{"id": "s1", "kind": "replace"}})),
			),
			want: []want{
				{key: key("proofSuggestion", "s1"), quote: "c d"},
				{key: key("proofComment", "c1"), quote: "bc"},
				{key: key("dispatchAsk", "q1"), quote: "ab"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			keys := make([]MarkRef, len(test.want))
			for index, want := range test.want {
				keys[index] = want.key
			}
			if found := assertFindMarksMatchesOracle(t, test.name, test.doc, keys); found != len(keys) {
				t.Fatalf("found %d of %d keys", found, len(keys))
			}
			got := FindMarks(test.doc, keys)
			for _, want := range test.want {
				hit := got[want.key]
				if hit.Quote != want.quote {
					t.Errorf("%s %s: quote %q, want %q", want.key.Type, want.key.ID, hit.Quote, want.quote)
				}
				if want.by != "" && hit.Attrs["by"] != want.by {
					t.Errorf("%s %s: by %v, want %q", want.key.Type, want.key.ID, hit.Attrs["by"], want.by)
				}
			}
		})
	}

	// MarkAttrs hands its caller attributes it may change, while FindMarks shares the tree's own:
	// a change to MarkAttrs' result must reach neither the tree nor a later read.
	t.Run("MarkAttrs returns a copy", func(t *testing.T) {
		tree := doc(paragraph(run("a", comment("c1"))))
		attrs, ok := MarkAttrs(tree, "proofComment", "c1")
		if !ok {
			t.Fatal("MarkAttrs found no c1")
		}
		attrs["by"] = "user:mallory"
		if hit := FindMarks(tree, []MarkRef{key("proofComment", "c1")})[key("proofComment", "c1")]; hit.Attrs["by"] != "user:alice" {
			t.Fatalf("changing MarkAttrs' result changed FindMarks' read of the tree's mark to %v", hit.Attrs)
		}
		if again, _ := MarkAttrs(tree, "proofComment", "c1"); again["by"] != "user:alice" {
			t.Fatalf("changing MarkAttrs' result changed a later MarkAttrs read to %v", again)
		}
	})

	t.Run("no keys", func(t *testing.T) {
		if got := FindMarks(doc(paragraph(run("a", c1))), nil); got == nil || len(got) != 0 {
			t.Fatalf("FindMarks(nil keys) = %#v, want an empty map", got)
		}
	})
}

// namedMarkdown is one markdown document a test reads, under the name its failures give it.
type namedMarkdown struct {
	name     string
	markdown string
}

// corpusMarkdown is every testdata/corpus document.
func corpusMarkdown(t testing.TB) []namedMarkdown {
	t.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	documents := make([]namedMarkdown, 0, len(paths))
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, namedMarkdown{name: filepath.Base(path), markdown: string(markdown)})
	}
	return documents
}

// anchorCorpusMarkdown is every *.md under PMDOC_ANCHOR_CORPUS, a directory of private documents
// (real specs) that a test checks without ever printing their text, or skips the test when it is
// unset.
func anchorCorpusMarkdown(t *testing.T) []namedMarkdown {
	t.Helper()
	root := os.Getenv("PMDOC_ANCHOR_CORPUS")
	if root == "" {
		t.Skip("PMDOC_ANCHOR_CORPUS unset; point it at a directory of markdown documents")
	}
	var documents []namedMarkdown
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		markdown, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		documents = append(documents, namedMarkdown{name: name, markdown: string(markdown)})
		return nil
	})
	if err != nil {
		t.Fatalf("read PMDOC_ANCHOR_CORPUS: %v", err)
	}
	if len(documents) == 0 {
		t.Fatal("PMDOC_ANCHOR_CORPUS holds no *.md document")
	}
	return documents
}

// realSizeDocuments are the generated documents of a real spec's size the tests and benchmarks
// read: an anchor's cost shows only on a document this large.
var realSizeDocuments = []struct {
	name  string
	bytes int
	seed  int64
}{
	{name: "150KB", bytes: 150_000, seed: 1},
	{name: "400KB", bytes: 400_000, seed: 2},
}

func parseRealSize(t testing.TB, bytes int, seed int64) *Node {
	t.Helper()
	doc, err := Parse(realSizeMarkdown(bytes, seed))
	if err != nil {
		t.Fatalf("parse the %d-byte document of seed %d: %v", bytes, seed, err)
	}
	return doc
}

var realSizeWords = []string{
	"anchor", "quote", "naïve", "café", "𝔸lpha", "😀", "document", "reader", "writes", "comment",
	"between", "blocks", "every", "run", "mark", "server", "browser", "table", "list", "heading",
	"suggestion", "ask", "walk", "position", "text", "fragment", "update", "version", "über",
	"résumé", "the", "of", "a", "and", "keeps", "reads", "once",
}

// realSizeMarkdown is a deterministic markdown document of at least targetBytes bytes shaped like a
// real spec: sections and subsections, paragraphs of sentences carrying inline code, strong,
// emphasis and links, bullet lists with nested items, ordered lists, tables and Go code blocks, in
// words that include accented, astral and emoji characters.
func realSizeMarkdown(targetBytes int, seed int64) string {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
	word := func() string { return realSizeWords[rng.IntN(len(realSizeWords))] }
	words := func(count int) string {
		parts := make([]string, count)
		for index := range parts {
			parts[index] = word()
		}
		return strings.Join(parts, " ")
	}
	link := 0
	sentence := func() string {
		count := 6 + rng.IntN(11)
		parts := make([]string, 0, count)
		for len(parts) < count {
			switch rng.IntN(14) {
			case 0:
				parts = append(parts, "`"+word()+"`")
			case 1:
				parts = append(parts, "**"+words(1+rng.IntN(3))+"**")
			case 2:
				parts = append(parts, "*"+words(1+rng.IntN(2))+"*")
			case 3:
				link++
				parts = append(parts, fmt.Sprintf("[%s](https://example.com/p/%d)", words(1+rng.IntN(3)), link))
			default:
				parts = append(parts, word())
			}
		}
		text := strings.Join(parts, " ")
		// The first rune, not the first byte: a word can open with a multi-byte character.
		first, width := utf8.DecodeRuneInString(text)
		return string(unicode.ToUpper(first)) + text[width:] + "."
	}
	paragraph := func() string {
		sentences := make([]string, 2+rng.IntN(4))
		for index := range sentences {
			sentences[index] = sentence()
		}
		return strings.Join(sentences, " ")
	}
	bulletList := func() string {
		var lines []string
		for range 2 + rng.IntN(4) {
			lines = append(lines, "- "+sentence())
			if rng.IntN(3) == 0 {
				for range 1 + rng.IntN(3) {
					lines = append(lines, "  - "+sentence())
				}
			}
		}
		return strings.Join(lines, "\n")
	}
	orderedList := func() string {
		var lines []string
		for index := range 2 + rng.IntN(4) {
			lines = append(lines, fmt.Sprintf("%d. %s", index+1, sentence()))
			if rng.IntN(4) == 0 {
				lines = append(lines, "   - "+sentence())
			}
		}
		return strings.Join(lines, "\n")
	}
	table := func() string {
		lines := []string{"| Name | Value | Note |", "| --- | --- | --- |"}
		for range 2 + rng.IntN(4) {
			lines = append(lines, fmt.Sprintf("| %s | %d | %s `%s` |", word(), rng.IntN(1000), words(2+rng.IntN(4)), word()))
		}
		return strings.Join(lines, "\n")
	}
	codeBlock := func(section int) string {
		return fmt.Sprintf("```go\nfunc section%d(reader string) string {\n\t// %s\n\treturn reader + %q\n}\n```", section, words(4), words(3))
	}

	var blocks []string
	size := 0
	add := func(block string) {
		blocks = append(blocks, block)
		size += len(block) + 2
	}
	for section := 1; size < targetBytes; section++ {
		add(fmt.Sprintf("## Section %d", section))
		for range 1 + rng.IntN(3) {
			add(paragraph())
		}
		if rng.IntN(2) == 0 {
			add(bulletList())
		}
		if rng.IntN(3) == 0 {
			add(orderedList())
		}
		for subsection := 1; subsection <= 1+rng.IntN(2); subsection++ {
			add(fmt.Sprintf("### Subsection %d.%d", section, subsection))
			add(paragraph())
			switch rng.IntN(3) {
			case 0:
				add(table())
			case 1:
				add(codeBlock(section))
			}
			add(paragraph())
		}
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// anchorTextblock is a textblock placeAnchorMarks may mark: its text runs hold length code points.
type anchorTextblock struct {
	node    *Node
	inTable bool
	length  int
}

func anchorTextblocks(doc *Node) []anchorTextblock {
	var blocks []anchorTextblock
	var visit func(node *Node, inTable bool)
	visit = func(node *Node, inTable bool) {
		if isTextblock(node.Type) {
			length := 0
			for _, child := range node.Children {
				if child.Type == "text" {
					length += utf8.RuneCountInString(child.Text)
				}
			}
			if length > 0 {
				blocks = append(blocks, anchorTextblock{node: node, inTable: inTable, length: length})
			}
			return
		}
		if node.Type == "table_cell" || node.Type == "table_header" {
			inTable = true
		}
		for _, child := range node.Children {
			visit(child, inTable)
		}
	}
	visit(doc, false)
	return blocks
}

// markTextblock adds mark to the code points from..to of block's text runs, splitting the runs it
// starts or ends inside, so a mark can start and end mid-run. Each run keeps its marks sorted.
func markTextblock(block *Node, from, to int, mark Mark) {
	children := make([]*Node, 0, len(block.Children)+2)
	piece := func(source *Node, text string) *Node {
		out := &Node{Type: "text", Text: text, Marks: make([]Mark, len(source.Marks))}
		for index, existing := range source.Marks {
			out.Marks[index] = Mark{Type: existing.Type, Attrs: cloneAttrs(existing.Attrs)}
		}
		return out
	}
	offset := 0
	for _, child := range block.Children {
		if child.Type != "text" {
			children = append(children, child)
			continue
		}
		length := utf8.RuneCountInString(child.Text)
		start, end := max(from, offset), min(to, offset+length)
		if start >= end {
			children = append(children, child)
			offset += length
			continue
		}
		startByte, endByte := codePointByte(child.Text, start-offset), codePointByte(child.Text, end-offset)
		if startByte > 0 {
			children = append(children, piece(child, child.Text[:startByte]))
		}
		marked := piece(child, child.Text[startByte:endByte])
		marked.Marks = append(marked.Marks, Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)})
		sortMarks(marked.Marks)
		children = append(children, marked)
		if endByte < len(child.Text) {
			children = append(children, piece(child, child.Text[endByte:]))
		}
		offset += length
	}
	block.Children = children
}

// codePointByte is the byte offset of text's count-th code point: a split there never parts a
// surrogate pair's two UTF-16 units, nor the bytes of an invalid sequence utf8 reads as one.
func codePointByte(text string, count int) int {
	offset := 0
	for range count {
		_, width := utf8.DecodeRuneInString(text[offset:])
		offset += width
	}
	return offset
}

// anchorPlacement is one mark placeAnchorMarks wrote within one textblock.
type anchorPlacement struct {
	block    int
	from, to int
	mark     Mark
}

// placeAnchorMarks writes count anchor marks into doc, deterministically for seed, and returns the
// distinct identities written, in the order first written. The marks are proofComment,
// proofSuggestion and dispatchAsk marks with an id and an author, starting and ending mid-run, in
// shapes taken in turn: a paragraph's sentence, the end of one textblock through the start of the
// next, a table cell, a code block, a heading, an overlap of an earlier mark (every other one of
// its type) and an earlier identity written again elsewhere, splitting it into two spans. A shape
// the document has no place for falls back to a paragraph's sentence. A mark touching a code block
// takes a type the code block allows (markAllowedInTextblock), so Update can write the result.
func placeAnchorMarks(doc *Node, count int, seed int64) []MarkRef {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x6a09e667f3bcc909))
	blocks := anchorTextblocks(doc)
	if len(blocks) == 0 {
		return nil
	}
	types := []string{"proofComment", "proofSuggestion", "dispatchAsk"}
	authors := []string{"user:alice", "user:bob", "session:01a0"}
	fit := func(markType string, indexes ...int) string {
		for _, index := range indexes {
			if !markAllowedInTextblock(markType, blocks[index].node.Type) {
				return "proofComment"
			}
		}
		return markType
	}
	pick := func(keep func(anchorTextblock) bool) int {
		var candidates []int
		for index, block := range blocks {
			if keep(block) {
				candidates = append(candidates, index)
			}
		}
		if len(candidates) == 0 {
			return -1
		}
		return candidates[rng.IntN(len(candidates))]
	}
	within := func(length int) (int, int) {
		if length == 1 {
			return 0, 1
		}
		from := rng.IntN(length - 1)
		return from, min(length, from+1+rng.IntN(40))
	}

	var refs []MarkRef
	var placements []anchorPlacement
	// written holds every identity's mark and the textblocks it lies in, for writing it again.
	type writtenMark struct {
		mark   Mark
		blocks []int
	}
	var written []writtenMark
	overlaps := 0
	newMark := func(markType string, index int) Mark {
		mark := Mark{Type: markType, Attrs: Attrs{"id": fmt.Sprintf("m%04x-%d", rng.IntN(1<<16), index), "by": authors[rng.IntN(len(authors))]}}
		refs = append(refs, MarkRef{Type: markType, ID: mark.Attrs["id"].(string)})
		return mark
	}
	place := func(block, from, to int, mark Mark) {
		markTextblock(blocks[block].node, from, to, mark)
		placements = append(placements, anchorPlacement{block: block, from: from, to: to, mark: mark})
	}
	inOne := func(index, block int, markType string) {
		from, to := within(blocks[block].length)
		mark := newMark(fit(markType, block), index)
		place(block, from, to, mark)
		written = append(written, writtenMark{mark: mark, blocks: []int{block}})
	}

	for index := range count {
		markType := types[rng.IntN(len(types))]
		block := -1
		switch index % 7 {
		case 1:
			if len(blocks) >= 2 {
				first := rng.IntN(len(blocks) - 1)
				second := first + 1
				mark := newMark(fit(markType, first, second), index)
				from := max(0, blocks[first].length-1-rng.IntN(min(blocks[first].length, 20)))
				markTextblock(blocks[first].node, from, blocks[first].length, mark)
				markTextblock(blocks[second].node, 0, 1+rng.IntN(min(blocks[second].length, 20)), mark)
				written = append(written, writtenMark{mark: mark, blocks: []int{first, second}})
				continue
			}
		case 2:
			block = pick(func(block anchorTextblock) bool { return block.inTable })
		case 3:
			block = pick(func(block anchorTextblock) bool { return block.node.Type == "code_block" })
		case 4:
			block = pick(func(block anchorTextblock) bool { return block.node.Type == "heading" })
		case 5:
			if len(placements) > 0 {
				earlier := placements[rng.IntN(len(placements))]
				overlapType := earlier.mark.Type
				if overlaps%2 == 1 {
					for _, other := range types {
						if other != earlier.mark.Type && markAllowedInTextblock(other, blocks[earlier.block].node.Type) {
							overlapType = other
							break
						}
					}
				}
				overlaps++
				from := earlier.from + (earlier.to-earlier.from)/2
				to := min(blocks[earlier.block].length, earlier.to+1+rng.IntN(8))
				mark := newMark(overlapType, index)
				place(earlier.block, from, to, mark)
				written = append(written, writtenMark{mark: mark, blocks: []int{earlier.block}})
				continue
			}
		case 6:
			if len(written) > 0 {
				again := written[rng.IntN(len(written))]
				block = pick(func(candidate anchorTextblock) bool {
					for _, used := range again.blocks {
						if blocks[used].node == candidate.node {
							return false
						}
					}
					return markAllowedInTextblock(again.mark.Type, candidate.node.Type)
				})
				if block >= 0 {
					from, to := within(blocks[block].length)
					place(block, from, to, again.mark)
					continue
				}
			}
		}
		if block < 0 {
			block = pick(func(block anchorTextblock) bool { return block.node.Type == "paragraph" && !block.inTable })
		}
		if block < 0 {
			block = rng.IntN(len(blocks))
		}
		inOne(index, block, markType)
	}
	return refs
}

// anchorShapes counts doc's anchor identities whose mark lies in two or more spans (MarkSpans) and
// those whose first span crosses a textblock boundary, and the text runs carrying two marks of one
// anchor type. It reads them in one pass: asking MarkSpans and FindMark per identity would walk a
// real-size document twice per mark.
func anchorShapes(doc *Node) (split, crossing, sameTypeOverlaps int) {
	type progress struct {
		lastRun         int
		textblock       *Node
		split, crossing bool
	}
	identities := map[MarkRef]*progress{}
	run := 0
	var visit func(node, textblock *Node)
	visit = func(node, textblock *Node) {
		if isTextblock(node.Type) {
			textblock = node
		}
		if node.Type != "text" {
			for _, child := range node.Children {
				visit(child, textblock)
			}
			return
		}
		// Every text run counts, so a run without an identity parts its spans as MarkSpans has it.
		run++
		types := map[string]bool{}
		overlapping := false
		for _, mark := range node.Marks {
			if !anchorMarkTypes[mark.Type] {
				continue
			}
			overlapping = overlapping || types[mark.Type]
			types[mark.Type] = true
			id, ok := mark.Attrs["id"].(string)
			if !ok {
				continue
			}
			ref := MarkRef{Type: mark.Type, ID: id}
			seen := identities[ref]
			switch {
			case seen == nil:
				identities[ref] = &progress{lastRun: run, textblock: textblock}
			case seen.lastRun == run:
			case seen.lastRun == run-1:
				seen.crossing = seen.crossing || !seen.split && seen.textblock != textblock
				seen.lastRun = run
			default:
				seen.split = true
				seen.lastRun = run
			}
		}
		if overlapping {
			sameTypeOverlaps++
		}
	}
	visit(doc, nil)
	for _, seen := range identities {
		if seen.split {
			split++
		}
		if seen.crossing {
			crossing++
		}
	}
	return split, crossing, sameTypeOverlaps
}

func TestFindMarksMatchesFindMarkOnCorpus(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		for _, count := range []int{1, 6, 20} {
			doc, err := Parse(document.markdown)
			if err != nil {
				t.Fatalf("%s: %v", document.name, err)
			}
			refs := placeAnchorMarks(doc, count, int64(count))
			assertFindMarksMatchesOracle(t, fmt.Sprintf("%s at %d marks", document.name, count), doc, refs)
		}
	}
}

func TestFindMarksMatchesFindMarkOnRealSizeDocuments(t *testing.T) {
	for _, size := range realSizeDocuments {
		for _, count := range []int{12, 47, 80} {
			t.Run(fmt.Sprintf("%s/%d", size.name, count), func(t *testing.T) {
				doc := parseRealSize(t, size.bytes, size.seed)
				refs := placeAnchorMarks(doc, count, size.seed*1000+int64(count))
				if found := assertFindMarksMatchesOracle(t, size.name, doc, refs); found != len(refs) {
					t.Fatalf("found %d of the %d marks placed", found, len(refs))
				}
				split, crossing, overlaps := anchorShapes(doc)
				if split == 0 || crossing == 0 || overlaps == 0 {
					t.Fatalf("%d split, %d cross-block and %d same-type overlapping marks: want each shape present", split, crossing, overlaps)
				}
			})
		}
	}
}

func TestFindMarksMatchesFindMarkOnAnchorCorpus(t *testing.T) {
	for _, document := range anchorCorpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			checked := 0
			for _, count := range []int{12, 47, 80} {
				doc, err := Parse(document.markdown)
				if err != nil {
					t.Logf("%s: skipped, Parse refused it (%T)", document.name, err)
					return
				}
				refs := placeAnchorMarks(doc, count, int64(count))
				checked += assertFindMarksMatchesOracle(t, fmt.Sprintf("%s at %d marks", document.name, count), doc, refs)
			}
			t.Logf("%s: %d marks checked", document.name, checked)
		})
	}
}

func FuzzFindMarks(f *testing.F) {
	for index, document := range corpusMarkdown(f) {
		if len(document.markdown) <= 16<<10 {
			f.Add(int64(index), 6, document.markdown)
		}
	}
	f.Fuzz(func(t *testing.T, seed int64, count int, markdown string) {
		if len(markdown) > 16<<10 {
			return
		}
		doc, err := Parse(markdown)
		if err != nil {
			return
		}
		count = int(uint(count) % 41)
		assertFindMarksMatchesOracle(t, "fuzzed document", doc, placeAnchorMarks(doc, count, seed))
	})
}
