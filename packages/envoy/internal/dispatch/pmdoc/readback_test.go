package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A document a caller writes is stored as its rendering, so one whose rendering does not read back
// as the document is refused rather than stored: the next read of what was stored would give
// another document, or refuse it. Each document here is read as the browser editor's engine reads
// it; the first two read back as another document (ErrMisreadRendering) and the third's rendering
// no reader of this package reads at all, which is refused by the shape's own name
// (ErrRenderingRefused), since that is what its author can act on.
func TestParseForWriteRefusesADocumentItsRenderingReadsBackOtherwise(t *testing.T) {
	for markdown, want := range map[string]error{
		"> [^n]: -\n> -\n":                    ErrMisreadRendering,
		"> [^n]: *\n> 10. text x[^n]\n":       ErrMisreadRendering,
		"2. [^N]:\n       12. *\n       --\n": ErrRenderingRefused,
	} {
		if _, err := Parse(markdown); err != nil {
			t.Fatalf("Parse(%q) = %v, want it read", markdown, err)
		}
		_, err := ParseForWrite(markdown, nil)
		if !errors.Is(err, want) {
			t.Errorf("ParseForWrite(%q) = %v, want %v", markdown, err, want)
		}
		if want == ErrRenderingRefused && !strings.Contains(err.Error(), "an empty list item followed by a blank line") {
			t.Errorf("ParseForWrite(%q) = %v, want it to name the shape its rendering holds", markdown, err)
		}
	}
}

// A short table row is padded to the header's width, and a padded cell takes its column's
// alignment: the rendering writes it as a cell of that column, which reads back with the column's
// alignment, so a padded cell holding another would store a table that reads back otherwise.
//
// The padding is a normalisation the document write makes, not a reading of the markdown the
// browser editor's parser shares: that parser reads a short row as the cells it writes, and
// prosemirror-tables' fixTables pads it as this reader does the moment the document reaches an
// editor (packages/envoy/AGENTS.md, "Milkdown's gfm preset"). What is stored is what both read
// alike, which is what this pins: the row is written out padded, and that markdown reads back as
// the tree it was stored from. Measured against the engine over the 99 documents of this class
// found in a 50,000-document sample: 99 of 99 of their stored renderings read alike in both
// parsers (sjawhar/legion#2507).
func TestParsePadsAShortRowAndStoresWhatReadsBackAlike(t *testing.T) {
	markdown := "| a | b | c | d |\n| :--- | :---: | ---: | --- |\n| x |\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	row := doc.Children[0].Children[1]
	var got []any
	for _, cell := range row.Children {
		got = append(got, cell.Attrs["alignment"])
	}
	if want := []any{"left", "center", "right", nil}; !slices.Equal(got, want) {
		t.Errorf("Parse(%q) pads the short row with alignments %v, want %v", markdown, got, want)
	}
	stored, err := ParseForWrite(markdown, nil)
	if err != nil {
		t.Fatalf("ParseForWrite(%q) = %v, want the table stored", markdown, err)
	}
	rendered, err := Render(stored)
	if err != nil {
		t.Fatal(err)
	}
	if want := "| a | b | c | d |\n| :--- | :---: | ---: | --- |\n| x |  |  |  |\n"; rendered != want {
		t.Errorf("the write stored %q, want the row written out padded, %q", rendered, want)
	}
	back, err := ParseRendering(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(back) {
		t.Errorf("the stored markdown %q reads back otherwise", rendered)
	}
}

// A backslash inside a bare URL is the URL's, as GFM and the browser editor's parser read it:
// `https://x.com/a\_b` links `https://x.com/a\_b` whole. The renderer writes a bare URL's bytes as
// they are, so the document is stored as written and reads back as it was read, in both parsers;
// a URL holding a parenthesis, a backtick or `>`, which no bare URL is written with, is written as
// a link both read back.
func TestRenderKeepsABackslashInsideABareURL(t *testing.T) {
	const chars = "!\"#$%&'()+,-./:;=>?@[]^_`{}~"
	var markdowns []string
	for _, char := range chars {
		markdowns = append(markdowns, "see https://x.com/a\\"+string(char)+"b c\n")
	}
	readings := engineReadings(t, markdowns)
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		url := strings.TrimSuffix(strings.TrimPrefix(markdown, "see "), " c\n")
		if got, want := inlineSummary(doc.Children[0]), fmt.Sprintf("text %q | text %q link %s | text %q", "see ", url, url, " c"); got != want {
			t.Errorf("Parse(%q) = %s, want %q linked whole", markdown, got, url)
		}
		if engine := readings[index]; engine == nil || !engine.Equal(doc) {
			got, _ := engine.JSON()
			t.Errorf("the engine reads %q as %s, not as Go does", markdown, got)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
		}
		if back, err := Parse(rendered); err != nil || !back.Equal(doc) {
			t.Errorf("Render(Parse(%q)) = %q, which reads back otherwise (%v)", markdown, rendered, err)
		}
		if !strings.ContainsRune("()`>", rune(chars[index])) && rendered != markdown {
			t.Errorf("Render(Parse(%q)) = %q, want it as written", markdown, rendered)
		}
		if _, err := ParseForWrite(markdown, nil); err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want it stored", markdown, err)
		}
	}
}

// A fused delimiter run that reads back is written as the writer before sjawhar/legion#1464
// writes it: the parser splits it as written, and the browser editor's parser reads the same
// bytes as the same tree.
func TestRenderKeepsAFusedRunThatReadsBack(t *testing.T) {
	assertRenders(t, []struct{ markdown, want string }{
		{"*x**b***\n", "*x****b***\n"},
		{"_p x**b**_ q\n", "*p x****b*** q\n"},
		{"> *x**b***\n", "> *x****b***\n"},
		{"- *x**b***\n", "- *x****b***\n"},
		{"*a **b** c*\n", "*a&#32;****b****&#32;c*\n"},
		{"*a **b** c **d** e*\n", "*a&#32;****b****&#32;c&#32;****d****&#32;e*\n"},
		{"| *a **b** c* |\n| --- |\n", "| *a&#32;****b****&#32;c* |\n| --- |\n"},
		{"> _&#42; ~ it's **`` x ``**_\n", "> *\\* ~ it's&#32;****`x`***\n"},
	})
}

// A fused run that does not read back is written again in the plain respellings, as the writer
// before sjawhar/legion#1464 writes it, before any spelling that keeps marks open: that writer's
// escaped bytes read back in both parsers, so a keep-open spelling tried first changed a document
// that writer stores correctly.
func TestRenderRespellsAFusedRunPlainlyBeforeKeepingMarksOpen(t *testing.T) {
	assertRenders(t, []struct{ markdown, want string }{
		{"- ****1**a__1***\n", "- \\*\\*\\**1****a\\_\\_1***\n"},
		{"****b.**(c)***(c)\n", "\\*\\*\\**b.****(c)***(c)\n"},
		{"# ****a___(c)**(c)*** *\n", "# \\*\\*\\**a\\_\\_\\_(c)****(c)*** \\*\n"},
		{"> ****1**x's***b.*b.**\n", "> \\*\\**1****x's***b.*b.*\n"},
		{"_(c)****a**a_Note:*** __\n", "\\_(c)\\*\\*\\**a****a\\_Note:*** \\_\\_\n"},
	})
}

// A fused run that does not read back is written with no mark the next text still carries closed
// and opened again between them: open marks stay open, and a text opens first the marks the text
// after it carries.
func TestRenderKeepsAMarkOpenWhereAFusedRunDoesNotReadBack(t *testing.T) {
	assertRenders(t, []struct{ markdown, want string }{
		{"*a **b [l](https://e.com/u) c** d*\n", "*a **b [l](https://e.com/u) c** d*\n"},
		{"*a **b***\n", "*a **b***\n"},
		{"***a** b*\n", "***a** b*\n"},
		{"_**Note:** see below_\n", "***Note:** see below*\n"},
		{"x ***a** b* y\n", "x ***a** b* y\n"},
		{"[***a** b*](https://e.com/u)\n", "[***a** b*](https://e.com/u)\n"},
	})
}

// unreadableRun is a paragraph's text that reads back in no spelling runSpellings lists and calls
// for every family of them (runNeeds): a tilde, a bare URL, a scheme's colon, a `www.` and an `@` in
// text, bold whose punctuation inside it stands beside a letter outside it, a delimiter run fused
// between nested marks, and an email link.
const unreadableRun = "ax@y.zz</b>#!<b>_.~~a_**]-**@*&#x40;* **Which here?**Careful. http: www.q z@ http://a.bc ~x *a _b **a.**&#98; awww.q *a **b*** "

// A run that reads back in no spelling is written in every one, and many write the same bytes: a
// flag can change nothing in the run, and a writing that fails under a reader fails under it at
// each later step too. Each markdown is read back under each reader at most once, so a write that
// fails every spelling costs a read for each distinct writing, not for each spelling and step.
func TestRenderReadsEachWritingBackUnderAReaderOnce(t *testing.T) {
	const markdown = unreadableRun + "lorem ipsum\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse(%q) = %v", markdown, err)
	}
	type read struct {
		markdown string
		reader   inlineReader
	}
	reads := map[read]int{}
	readBackObserver = func(markdown string, reader inlineReader) { reads[read{markdown, reader}]++ }
	defer func() { readBackObserver = nil }()
	if _, err := Render(doc); err != nil {
		t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
	}
	if len(reads) <= readBackReaderCount {
		t.Fatalf("Render(Parse(%q)) read back %d writings under a reader, want it to try its respellings", markdown, len(reads))
	}
	repeated, total := 0, 0
	for _, count := range reads {
		total += count
		if count > 1 {
			repeated++
		}
	}
	if repeated > 0 {
		t.Errorf("Render(Parse(%q)) made %d read-backs of %d writings under a reader, %d of them read more than once", markdown, total, len(reads), repeated)
	}
}

// assertRenders requires each markdown to render as want and to be stored by ParseForWrite.
func assertRenders(t *testing.T, cases []struct{ markdown, want string }) {
	t.Helper()
	for _, c := range cases {
		doc, err := Parse(c.markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", c.markdown, err)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", c.markdown, err)
		}
		if rendered != c.want {
			t.Errorf("Render(Parse(%q)) = %q, want %q", c.markdown, rendered, c.want)
		}
		if _, err := ParseForWrite(c.markdown, nil); err != nil {
			t.Errorf("ParseForWrite(%q) = %v, want it stored", c.markdown, err)
		}
	}
}

// A write into a document reads it back before and after with the tables' spans unwritten
// (RefuseMisreadWrite), so a stored row whose cell spans columns is written short and padded on
// each read-back. Those cells are the stored document's, not the write's, so each read-back has a
// budget of its own: beside a stored table whose spans cover 5,940 cells a paragraph is stored and
// a table whose body row is shorter than its header is still refused as reading back otherwise,
// and beside one whose read-back would pad more than a read-back's budget the write is refused,
// naming the limit, rather than stored without the check.
func TestRefuseMisreadWriteReadsStoredSpansBackOnABudgetOfTheirOwn(t *testing.T) {
	paragraph := &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "New."}}}
	shortRow := spanTable([][2]int{{1, 1}, {1, 1}}, [][2]int{{1, 1}})
	spanned := documentBesideSpannedTable(100, 60)
	if err := RefuseMisreadWrite(spanned, documentAdding(spanned, paragraph)); err != nil {
		t.Errorf("a paragraph beside a table spanning 5,940 cells: %v, want it stored", err)
	}
	if err := RefuseMisreadWrite(spanned, documentAdding(spanned, shortRow)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "reads back otherwise") {
		t.Errorf("a short-rowed table beside a table spanning 5,940 cells: %v, want it refused as reading back otherwise", err)
	}
	past := documentBesideSpannedTable(1000, 101)
	if err := RefuseMisreadWrite(past, documentAdding(past, paragraph)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "limit 100000") {
		t.Errorf("a paragraph beside a table spanning 100,899 cells: %v, want it refused naming the read-back limit", err)
	}
}

// documentBesideSpannedTable is a paragraph and a table under a header of width cells, each of
// its rows one cell spanning the header.
func documentBesideSpannedTable(width, rows int) *Node {
	header := make([][2]int, width)
	for index := range header {
		header[index] = [2]int{1, 1}
	}
	spans := [][][2]int{header}
	for range rows {
		spans = append(spans, [][2]int{{width, 1}})
	}
	doc := &Node{Type: "doc", Children: []*Node{
		{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Before."}}},
		spanTable(spans...),
	}}
	EnsureBlockIDs(doc)
	return doc
}

// documentAdding is doc with block written after its last.
func documentAdding(doc, block *Node) *Node {
	return &Node{Type: "doc", Children: append(slices.Clone(doc.Children), block)}
}
