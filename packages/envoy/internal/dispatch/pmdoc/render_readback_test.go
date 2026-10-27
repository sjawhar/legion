package pmdoc

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// engineReadings reads each markdown as the browser editor imports a document, with the fork's
// headless engine (gen/differential.ts), each short table body row padded as Parse pads it.
func engineReadings(t *testing.T, markdowns []string) []*Node {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("bun is not on PATH; the engine's reading is required in CI")
		}
		t.Fatalf("bun is required in CI: %v", err)
	}
	dir := t.TempDir()
	var lines []string
	for index, markdown := range markdowns {
		line, _ := json.Marshal(map[string]any{"id": index, "md": markdown})
		lines = append(lines, string(line))
	}
	in, out := filepath.Join(dir, "in.jsonl"), filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(in, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := genScript("differential.ts", in, out, "md").CombinedOutput(); err != nil {
		t.Fatalf("differential.ts: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	readings := make([]*Node, 0, len(markdowns))
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var read struct {
			MD *diffReading `json:"md"`
		}
		if err := json.Unmarshal([]byte(line), &read); err != nil {
			t.Fatal(err)
		}
		readings = append(readings, diffTree(read.MD))
	}
	return readings
}

// A document the browser editor holds is stored as its rendering, so the rendering must read back,
// in Go and in the browser editor's engine alike, as what the editor shows - where markdown has a
// form for it. Each row is a tree the editor can hold whose rendering main misreads, and the
// markdown whose reading both parsers must give the rendering back as.
func TestRenderReadsBackInBothParsersAsTheEditorShowsIt(t *testing.T) {
	parsed := func(markdown string) *Node {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q): %v", markdown, err)
		}
		return doc
	}
	all := func(doc *Node, kind string) []*Node {
		var found []*Node
		Walk(doc, func(node *Node) bool {
			if node.Type == kind {
				found = append(found, node)
			}
			return true
		})
		return found
	}
	// texts is markdown's reading with its text nodes' text replaced in order.
	texts := func(markdown string, replaced ...string) *Node {
		doc := parsed(markdown)
		for index, node := range all(doc, "text") {
			node.Text = replaced[index]
		}
		return doc
	}
	// soft is doc with each hard break one the editor draws inline, as a paste of plain text used
	// to leave it.
	soft := func(doc *Node) *Node {
		for _, node := range all(doc, "hardbreak") {
			node.Attrs["isInline"] = true
		}
		return doc
	}
	tight := func(markdown string) *Node {
		doc := parsed(markdown)
		for _, node := range append(all(doc, "bullet_list"), all(doc, "list_item")...) {
			node.Attrs["spread"] = false
		}
		return doc
	}
	// cells is markdown's first table with each listed cell's inline content replaced by the
	// reading of its markdown and its attributes set, and the cells a span covers removed.
	type cellChange struct {
		row, cell int
		inline    string
		attrs     Attrs
		remove    bool
	}
	cells := func(markdown string, changes ...cellChange) *Node {
		doc := parsed(markdown)
		table := all(doc, "table")[0]
		for _, change := range changes {
			cell := table.Children[change.row].Children[change.cell]
			if change.inline != "" {
				cell.Children[0].Children = parsed(change.inline).Children[0].Children
			}
			for key, value := range change.attrs {
				cell.Attrs[key] = value
			}
		}
		for index := len(changes) - 1; index >= 0; index-- {
			if change := changes[index]; change.remove {
				row := table.Children[change.row]
				row.Children = append(row.Children[:change.cell], row.Children[change.cell+1:]...)
			}
		}
		return doc
	}
	level := func(doc *Node, heading int) *Node {
		all(doc, "heading")[0].Attrs["level"] = heading
		return doc
	}
	cellText := func(markdown, text string) *Node {
		doc := parsed(markdown)
		all(doc, "table_cell")[0].Children[0].Children = []*Node{{Type: "text", Text: text}}
		return doc
	}
	bareURL := func(url string) *Node {
		doc := texts("https://x.com/a b\n", url, " b")
		all(doc, "text")[0].Marks[0].Attrs["href"] = url
		return doc
	}
	const table = "| h |\n| --- |\n| c |\n"
	const table2 = "| h1 | h2 |\n| --- | --- |\n| c1 | c2 |\n"
	const table3 = "| h1 | h2 | h3 |\n| --- | --- | --- |\n| c1 | c2 | c3 |\n| d1 | d2 | d3 |\n"
	rows := []struct {
		name string
		tree *Node
		want string
	}{
		// 1. A soft line break the editor draws as a space is written as one.
		{"soft break in a paragraph", soft(parsed("a\\\nb\n")), "a b\n"},
		{"soft break between marked text", soft(parsed("*a*\\\n*b*\n")), "*a* *b*\n"},
		{"soft break in a list item", soft(parsed("- a\\\n  b\n")), "- a b\n"},
		{"soft break in a heading", soft(parsed("a\\\nb\n===\n")), "# a b\n"},
		{"soft break in a table cell", soft(cells(table, cellChange{row: 1, inline: "a\\\nb\n"})), "| h |\n| --- |\n| a b |\n"},
		// 2. A block whose first line would be a row of a table before it is written after a blank
		// line, which spreads a tight item, as two paragraphs in one do; every other block keeps it
		// tight.
		{"paragraph after a table in a tight item", tight("- x\n\n  " + strings.ReplaceAll(table, "\n", "\n  ") + "\n  after\n- y\n"), "- x\n\n  | h |\n  | --- |\n  | c |\n\n  after\n- y\n"},
		{"paragraph after a table in a tight nested item", tight("- a\n  - x\n\n    " + strings.ReplaceAll(table, "\n", "\n    ") + "\n    after\n"), "- a\n  - x\n\n    | h |\n    | --- |\n    | c |\n\n    after\n"},
		{"table after a table in a tight item", tight("- x\n\n  | h |\n  | --- |\n  | c |\n\n  | h2 |\n  | --- |\n  | c2 |\n"), "- x\n\n  | h |\n  | --- |\n  | c |\n\n  | h2 |\n  | --- |\n  | c2 |\n"},
		{"ordered list not at one after a table in a tight item", tight("- x\n\n  | h |\n  | --- |\n  | c |\n\n  2. o\n"), "- x\n\n  | h |\n  | --- |\n  | c |\n\n  2. o\n"},
		{"heading after a table keeps a tight item tight", tight("- x\n\n  | h |\n  | --- |\n  | c |\n\n  # h\n"), "- x\n  | h |\n  | --- |\n  | c |\n  # h\n"},
		{"setext heading after a table in a tight item", tight("- x\n\n  | h |\n  | --- |\n  | c |\n\n  a\\\n  b\n  ===\n"), "- x\n\n  | h |\n  | --- |\n  | c |\n\n  a\\\n  b\n  ===\n"},
		{"empty list item after a table keeps a tight item tight", tight("- | h |\n  | --- |\n  | c |\n  -\n"), "- | h |\n  | --- |\n  | c |\n  -\n"},
		// 3. A delimiter run beside punctuation inside it and a letter or digit outside it opens or
		// closes nothing as written, so that character is written as a character reference.
		{"bold ending in punctuation before a letter", texts("**Which here?** Careful.\n", "Which here?", "Careful."), "**Which here?**&#67;areful.\n"},
		{"italic ending in punctuation before a letter", texts("*Which here?* Careful.\n", "Which here?", "Careful."), "*Which here?*&#67;areful.\n"},
		{"strikethrough ending in punctuation before a letter", texts("~~Which here?~~ Careful.\n", "Which here?", "Careful."), "~~Which here?~~&#67;areful.\n"},
		{"bold and italic ending in punctuation before a letter", texts("***Which here?*** Careful.\n", "Which here?", "Careful."), "***Which here?***&#67;areful.\n"},
		{"bold ending in punctuation before a digit", texts("**Note:** 1\n", "Note:", "1"), "**Note:**&#49;\n"},
		{"bold opening on punctuation after a letter", texts("Careful **x**\n", "Careful", "?Which"), "Carefu&#108;**?Which**\n"},
		{"italic opening on punctuation after a letter", texts("Careful *x*\n", "Careful", "?Which"), "Carefu&#108;*?Which*\n"},
		{"bold with punctuation at both ends between letters", texts("a **x** c\n", "a", "?b?", "c"), "&#97;**?b?**&#99;\n"},
		{"bold opening on a space after a letter", texts("a **x**\n", "a", " b"), "&#97;**&#32;b**\n"},
		// 4. A hard break in a table cell, which would end the row, is written as a space, as the
		// editor writes it.
		{"hard break in a body cell", cells(table, cellChange{row: 1, inline: "b1\\\nb2\n"}), "| h |\n| --- |\n| b1 b2 |\n"},
		{"hard break in a header cell", cells(table, cellChange{row: 0, inline: "h1\\\nh2\n"}), "| h1 h2 |\n| --- |\n| c |\n"},
		{"hard break between marked text in a cell", cells(table, cellChange{row: 1, inline: "*b1*\\\n*b2*\n"}), "| h |\n| --- |\n| *b1* *b2* |\n"},
		// 5. A spanning cell is written as the cells it covers, empty past its first, and a header
		// narrower than the table is written as wide, so no cell's text is lost.
		{"header cell spanning two columns", cells(table2, cellChange{row: 0, cell: 0, attrs: Attrs{"colspan": 2}}, cellChange{row: 0, cell: 1, remove: true}), "| h1 |  |\n| --- | --- |\n| c1 | c2 |\n"},
		{"body cell spanning two columns", cells(table2, cellChange{row: 1, cell: 0, attrs: Attrs{"colspan": 2}}, cellChange{row: 1, cell: 1, remove: true}), "| h1 | h2 |\n| --- | --- |\n| c1 |  |\n"},
		{"body cell spanning two rows", cells(table2+"| d1 | d2 |\n", cellChange{row: 1, cell: 0, attrs: Attrs{"rowspan": 2}}, cellChange{row: 2, cell: 0, remove: true}), "| h1 | h2 |\n| --- | --- |\n| c1 | c2 |\n|  | d2 |\n"},
		{"body cell spanning two columns and two rows", cells(table3, cellChange{row: 1, cell: 0, attrs: Attrs{"colspan": 2, "rowspan": 2}}, cellChange{row: 1, cell: 1, remove: true}, cellChange{row: 2, cell: 0, remove: true}, cellChange{row: 2, cell: 1, remove: true}), "| h1 | h2 | h3 |\n| --- | --- | --- |\n| c1 |  | c3 |\n|  |  | d3 |\n"},
		{"header narrower than a body row", cells(table2, cellChange{row: 0, cell: 1, remove: true}), "| h1 |  |\n| --- | --- |\n| c1 | c2 |\n"},
		// 6. A line feed in text, which the editor draws as a line break, is written as a hard break.
		{"line feed in a paragraph", texts("Before after.\n", "Beforeline1\nline2 after."), "Beforeline1\\\nline2 after.\n"},
		{"line feed in marked text", texts("*Before after.*\n", "Beforeline1\nline2 after."), "*Beforeline1*\\\n*line2 after.*\n"},
		{"line feed in a list item", texts("- Listed item.\n- Second.\n", "Listedline1\nline2 item.", "Second."), "- Listedline1\\\n  line2 item.\n- Second.\n"},
		{"line feed in a level-two heading", texts("## a\n", "a\nb"), "a\\\nb\n---\n"},
		{"line feed in a level-three heading", level(texts("## a\n", "a\nb"), 3), "### a b\n"},
		{"line feed in a table cell", cellText(table, "a\nb"), "| h |\n| --- |\n| a b |\n"},
		{"line feed ending a paragraph", texts("abc\n", "abc\n"), "abc\n"},
		// 9. A bare URL holding a backtick, which the writer escapes and linkify stops at, is written
		// as an autolink, whose text neither parser unescapes.
		{"bare URL holding a backtick", bareURL("https://x.com/a`b"), "<https://x.com/a`b> b\n"},
		{"bare URL holding a backtick and a reference", bareURL("https://x.com/a&amp;`b"), "<https://x.com/a&amp;`b> b\n"},
	}
	rendered := make([]string, len(rows))
	for index, row := range rows {
		markdown, err := Render(row.tree)
		if err != nil {
			t.Fatalf("%s: Render: %v", row.name, err)
		}
		rendered[index] = markdown
	}
	readings := engineReadings(t, rendered)
	for index, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			want := parsed(row.want)
			if back, err := Parse(rendered[index]); err != nil || !back.Equal(want) {
				got, _ := back.JSON()
				t.Errorf("Go reads the rendering %q back as %s (%v), want what %q reads as", rendered[index], got, err, row.want)
			}
			if engine := readings[index]; engine == nil || !engine.Equal(want) {
				got, _ := engine.JSON()
				t.Errorf("the engine reads the rendering %q back as %s, want what %q reads as", rendered[index], got, row.want)
			}
		})
	}
}

// An agent edits a table row by reading its markdown and writing it back, so the markdown a write
// takes must come back from the next read as it went in. Each cell here holds a bare URL next to
// punctuation, the first from a production row, read in a table cell and in a paragraph: Go must
// read it as the browser editor's engine does - GFM leaves trailing punctuation before an end out
// of a URL, so only the URL is linked, a URL after a quote is linked as one after a space, and a
// scheme or `www` reads in any case - and every write must render it as written.
func TestBareURLMarkdownComesBackFromAWriteAsWritten(t *testing.T) {
	type form struct{ name, before, after string }
	forms := []form{{"paragraph", "", "\n"}, {"table cell", "| a | b |\n| --- | --- |\n| x | ", " |\n"}}
	tests := []struct{ name, cell string }{
		{"bare URL before a parenthesis and a quote", `"HTTP 401: Bad credentials (https://api.github.com/graphql)" while`},
		{"bare URLs in quotes and before a bracket", `see "https://x.test/a"; and 'https://x.test/b'! or https://x.test/c]`},
		{"bare URLs with an uppercase scheme and www", `see HTTPS://X.TEST/Y and Www.x.test/z; or (www.a.test) and *www.b.test*`},
	}
	var markdowns []string
	for _, test := range tests {
		for _, f := range forms {
			markdowns = append(markdowns, f.before+test.cell+f.after)
		}
	}
	readings := engineReadings(t, markdowns)
	index := 0
	for _, test := range tests {
		for _, f := range forms {
			markdown, engine := markdowns[index], readings[index]
			index++
			t.Run(test.name+" in a "+f.name, func(t *testing.T) {
				doc, err := Parse(markdown)
				if err != nil {
					t.Fatalf("Parse(%q) = %v", markdown, err)
				}
				if engine == nil || !doc.Equal(engine) {
					got, _ := doc.JSON()
					reading, _ := engine.JSON()
					t.Errorf("Go reads %q as %s, the engine as %s", markdown, got, reading)
				}
				for write := 1; write <= 3; write++ {
					rendered, err := Render(doc)
					if err != nil {
						t.Fatalf("Render, write %d: %v", write, err)
					}
					if rendered != markdown {
						t.Fatalf("write %d renders %q, want %q", write, rendered, markdown)
					}
					if doc, err = Parse(rendered); err != nil {
						t.Fatalf("Parse(%q) = %v", rendered, err)
					}
				}
			})
		}
	}
}

// A www link is written as its bare text only where the browser editor's tokenizer and Go's guard
// both read it from that text: right after a line's head, a space, a tab, `(`, `*`, `_`, `[`, `]`
// or `~`, and only when its text opens with no email address. Anywhere else - after a quote, a code
// span, inline HTML or a link - it is written as an explicit link, which both parsers read back as
// the tree, where the bare text would read in the editor as a longer link or an email.
func TestAWWWLinkTheBareTextWouldLoseIsWrittenAsALink(t *testing.T) {
	link := func(text string) *Node {
		return &Node{Type: "text", Text: text, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": "http://" + text, "title": nil}}}}
	}
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	code := func(value string) *Node { return &Node{Type: "text", Text: value, Marks: []Mark{{Type: "inlineCode"}}} }
	html := func(value string) *Node { return &Node{Type: "html", Attrs: Attrs{"value": value}} }
	tests := []struct {
		name   string
		inline []*Node
		want   string
	}{
		{"after a quote, and after a space", []*Node{text(`see "`), link("www.a.test"), text(`" and `), link("Www.b.test"), text(".")},
			"see \"[www.a.test](http://www.a.test)\" and Www.b.test.\n"},
		{"text opening with an email", []*Node{text("see "), link("www.x@y.com"), text(" b")},
			"see [www.x@y.com](http://www.x@y.com) b\n"},
		{"after inline HTML, before a letter outside ASCII", []*Node{html("<b>"), link("www.a.test"), text("é")},
			"<b>[www.a.test](http://www.a.test)é\n"},
		{"after a code span, before a backslash", []*Node{code("c"), link("www.a.test"), text(`\b`)},
			"`c`[www.a.test](http://www.a.test)\\b\n"},
		{"after a link", []*Node{link("www.a.test"), link("www.b.test"), text("|x")},
			"[www.a.test](http://www.a.test)[www.b.test](http://www.b.test)|x\n"},
	}
	rendered := make([]string, len(tests))
	docs := make([]*Node, len(tests))
	for index, test := range tests {
		docs[index] = &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: test.inline}}}
		markdown, err := Render(docs[index])
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		rendered[index] = markdown
	}
	readings := engineReadings(t, rendered)
	for index, test := range tests {
		if rendered[index] != test.want {
			t.Errorf("%s: Render = %q, want %q", test.name, rendered[index], test.want)
		}
		back, err := Parse(rendered[index])
		if err != nil || !back.Equal(StripAnchorMarks(docs[index])) || readings[index] == nil || !readings[index].Equal(back) {
			got, _ := back.JSON()
			engine, _ := readings[index].JSON()
			t.Errorf("%s: %q reads back in Go as %s (%v), in the engine as %s", test.name, rendered[index], got, err, engine)
		}
	}
}

// The first spelling of a run, the one inlineWithEscapes falls back to when no respelling reads
// back, closes a code span before a www link it writes as an explicit link: rolling back the bare
// writing of the link restores the open code fence its marks closed.
func TestTheFirstSpellingClosesACodeSpanBeforeAWWWLinkAfterIt(t *testing.T) {
	nodes := []*Node{
		{Type: "text", Text: "c", Marks: []Mark{{Type: "inlineCode"}}},
		{Type: "text", Text: "www.a.test", Marks: []Mark{{Type: "link", Attrs: Attrs{"href": "http://www.a.test", "title": nil}}}},
	}
	r := &renderer{}
	r.writeInlineRun(nodes, "", inlineContext{startsLine: true}, runSpelling{})
	if got, want := r.b.String(), "`c`[www.a.test](http://www.a.test)"; got != want {
		t.Fatalf("writeInlineRun = %q, want %q", got, want)
	}
}

// The browser editor's parser links a URL in decoded text, after an escape or a character
// reference, which Go's parser, reading markdown, does not. Each row is markdown and whether Go and
// the engine read it alike, so a change to either reading shows here: the divergences that remain
// (a URL after a letter and `;`, `#` or `&`, after an escaped character, at a line opening with a
// quote, a lowercase scheme written `http\:`, and a URL inside a www URL the transform reads) are
// recorded as differing, and the writer keeps a plain-text `www.` and an uppercase scheme
// unescaped, so they read as links in both, where an escape would leave Go reading text and the
// engine a link.
func TestURLReadingsBesideEscapesAgreeWhereRecorded(t *testing.T) {
	tests := []struct {
		markdown string
		agree    bool
	}{
		{"see www\\.x.test b\n", false},
		{"see HTTP\\://x.test b\n", false},
		{"see http\\://x.test b\n", false},
		{"x;https://x.test b\n", false},
		{"x#https://x.test b\n", false},
		{"x&https://x.test b\n", false},
		{"\\*https://x.test b\n", false},
		{"\\(www.x.test b\n", false},
		{"\"www.a.test/?u=https://b.test\"\n", false},
		{"\"https://x.test\" b\n", false},
		{"see \"https://x.test\" and 'https://y.test'.\n", true},
		{"see www.x.test b\n", true},
		{"see HTTP://x.test b\n", true},
	}
	plain := func(text string) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}}}
	}
	written := map[string]string{}
	for _, text := range []string{"see www.x.test b", "see HTTP://x.test b"} {
		markdown, err := Render(plain(text))
		if err != nil {
			t.Fatal(err)
		}
		written[text] = markdown
	}
	markdowns := make([]string, 0, len(tests)+len(written))
	for _, test := range tests {
		markdowns = append(markdowns, test.markdown)
	}
	for _, text := range []string{"see www.x.test b", "see HTTP://x.test b"} {
		markdowns = append(markdowns, written[text])
	}
	readings := engineReadings(t, markdowns)
	for index, markdown := range markdowns {
		agree := true
		if index < len(tests) {
			agree = tests[index].agree
		}
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		if got := readings[index] != nil && doc.Equal(readings[index]); got != agree {
			t.Errorf("%q: Go reads %s, the engine %s; recorded as agreeing: %v", markdown, inlineSummary(doc.Children[0]), inlineSummary(readings[index].Children[0]), agree)
		}
	}
}

// referenceRows are escapes and character references at the edges of what CommonMark decodes: an
// escaped `&` and an escaped backslash before a reference, a reference missing its `;`, the most
// digits a numeric one takes and one past them, code points the parser replaces (NUL, controls,
// surrogates, noncharacters, past U+10FFFF) beside their neighbours it keeps, a line feed in each
// spelling, named ones HTML decodes without a `;` (`&notit;`), in other cases or not at all, ones
// Go's HTML table lacks (`&nGt;`, `&nLt;`), and empty ones.
var referenceRows = []string{
	`Plain \&amp; end.`, `x\&#65; y`, `AT&T &copy 2024`, `x &amp y`, `x &#38 y`, `&#00000065;`,
	`&#x0000041;`, `&#128;`, `&#127;`, `&#xFFFF;`, `&notit;`, `\\&amp;`, `&#0000065;`, `&#x10FFFF;`,
	`&#x;`, `&#;`, `&;`, `&#1;`, `&#13;`, `&#159;`, `&#160;`, `&#xFDD0;`, `&#x1FFFE;`, `&semi;`,
	`&Abreve;`, `&AMP;`, `&Amp;`, `&ngE;`, `&foo;`, `&#0;`, `&#xD800;`, `&#x110000;`, `&#9;`,
	`&#10;`, `&#xA;`, `&NewLine;`, `a&#10;b`, `&nGt;`, `&nLt;`,
}

// readingDiff is the two trees of one markdown, for a failure saying where Go and the engine part.
func readingDiff(doc, engine *Node) string {
	got, _ := doc.JSON()
	reading, _ := engine.JSON()
	return "Go reads " + string(got) + "\nthe engine reads " + string(reading)
}

// parseAsTheEngine reads each markdown with Parse and with the engine (engineReadings) and reports
// each that Parse fails on, with the engine's tree, or reads otherwise, with both trees. It returns
// Go's trees, nil where Go's reading failed or parted from the engine's.
func parseAsTheEngine(t *testing.T, markdowns []string) []*Node {
	t.Helper()
	readings := engineReadings(t, markdowns)
	trees := make([]*Node, len(markdowns))
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			engine, _ := readings[index].JSON()
			t.Errorf("Parse(%q) = %v; the engine reads %s", markdown, err, engine)
			continue
		}
		if readings[index] == nil || !doc.Equal(readings[index]) {
			t.Errorf("%q:\n%s", markdown, readingDiff(doc, readings[index]))
			continue
		}
		trees[index] = doc
	}
	return trees
}

// An agent reads a document as Go reads its markdown, and the user sees it as the browser editor's
// engine reads it, so where Go decodes an escape or a character reference otherwise the agent
// quotes and writes back text the user never wrote: Go read `Plain \&amp; end.` as `Plain & end.`
// where the editor shows `Plain &amp; end.`. Each row is read at every place markdown decodes one
// - paragraph text, a link's and an image's title, an image's address and alt text, a footnote
// label and a table cell - and Go must read it as the engine does.
func TestEveryEscapeAndReferenceReadsAsTheEngineReadsIt(t *testing.T) {
	sites := []struct {
		name  string
		place func(row string) (string, bool)
	}{
		{"paragraph", func(row string) (string, bool) { return row + "\n", true }},
		{"link title", func(row string) (string, bool) { return `[a](u "` + row + "\")\n", true }},
		{"image title", func(row string) (string, bool) { return `![a](u "` + row + "\")\n", true }},
		{"image address", func(row string) (string, bool) {
			if strings.Contains(row, " ") {
				return "![a](<u " + row + " v>)\n", true
			}
			return "![a](u" + row + "v)\n", true
		}},
		{"image alt", func(row string) (string, bool) { return "![" + row + "](u)\n", true }},
		// A footnote label ends at a space or a `]`, so a row holding one makes no footnote.
		{"footnote label", func(row string) (string, bool) {
			return "x[^" + row + "]\n\n[^" + row + "]: d\n", !strings.ContainsAny(row, " ]")
		}},
		{"table cell", func(row string) (string, bool) { return "| h |\n| - |\n| " + row + " |\n", true }},
	}
	type placed struct{ site, row, markdown string }
	var cases []placed
	for _, row := range referenceRows {
		for _, site := range sites {
			if markdown, ok := site.place(row); ok {
				cases = append(cases, placed{site.name, row, markdown})
			}
		}
	}
	markdowns := make([]string, len(cases))
	for index, c := range cases {
		markdowns[index] = c.markdown
	}
	readings := engineReadings(t, markdowns)
	for index, c := range cases {
		doc, err := Parse(c.markdown)
		if err != nil {
			engine, _ := readings[index].JSON()
			t.Errorf("%s %q: Parse(%q) = %v; the engine reads %s", c.site, c.row, c.markdown, err, engine)
			continue
		}
		if readings[index] == nil || !doc.Equal(readings[index]) {
			t.Errorf("%s %q in %q:\n%s", c.site, c.row, c.markdown, readingDiff(doc, readings[index]))
		}
		if c.site == "paragraph" && c.row == `Plain \&amp; end.` {
			if text := doc.Children[0].Children; len(text) != 1 || text[0].Text != "Plain &amp; end." {
				t.Errorf("Go reads %q as %s, want the text %q", c.markdown, inlineSummary(doc.Children[0]), "Plain &amp; end.")
			}
		}
	}
	t.Logf("%d rows at %d sites: %d markdowns", len(referenceRows), len(sites), len(cases))
}

// The browser editor's import reads every line feed in text as a space, a decoded `&#10;` as much
// as a soft break, but keeps one in code and in an image's alt text, which it reads as written.
// Go read `a&#10;b` as `a\nb` where the editor shows `a b`, and took a decoded line feed, or two
// decoded spaces, before a break for the line's own ending. Each row places a reference to a line
// feed beside a break, at a paragraph's end, in marked, linked, heading and table text, in code and
// in an alt text, and Go must read it as the engine does.
func TestALineFeedAReferenceNamesReadsAsTheEngineReadsIt(t *testing.T) {
	markdowns := []string{
		"a&#10;b\n",
		"a&#10;\nb\n",
		"a&#10;  \nb\n",
		"a&#10;\\\nb\n",
		"a&#10;&#32;&#32;\\\nb\n",
		"a&#32;&#32;\\\nb\n",
		"a&#10;\n",
		"*a&#10;b*\n",
		"*a&#10;*\nb\n",
		"[a&#10;b](u)\n",
		"# a&#10;b\n",
		"| h |\n| - |\n| a&#10;b |\n",
		"`a&#10;b`\n",
		"![a&#10;b](u)\n",
		"![a&#10;\nb](u)\n",
		"a&#xA;b &NewLine;c\n",
	}
	parseAsTheEngine(t, markdowns)
}

// The rows above are the cases someone thought of; a reading that parts from the engine's on a
// sequence nobody listed shows the same wrong text to an agent. Each generated paragraph strings
// together backslashes, `&`, `&#`, `&#x`, `;`, digits, entity names the engine knows, ones it does
// not and ones HTML decodes without their `;`, and words and punctuation that open no link and no
// block, and Go must read it as the engine does. The seeds are fixed, so a failure reproduces.
func TestGeneratedEscapesAndReferencesReadAsTheEngineReadsThem(t *testing.T) {
	words := []string{"a", "x", "Plain", "end", "AT", "T", "2024", " ", " "}
	pieces := [][]string{
		{`\`, `\\`, "&", "&#", "&#x", "&#X", ";"},
		{`\`, "&", "&#", "&#x", ";", ";"},
		{"0", "1", "6", "9", "00000", "65", "128", "1114111"},
		{"a", "f", "A", "F", "41", "FFFF", "10FFFF", "D800", "110000"},
		{"amp", "copy", "not", "Abreve", "semi", "AMP", "ngE", "foo", "ampx", "notit", "copyx"},
		words,
		{",", ".", "!", "?", "'", `"`, "(", ")", "/", "-", "+", "%"},
	}
	random := rand.New(rand.NewPCG(20261027, 536))
	markdowns := make([]string, 2000)
	for index := range markdowns {
		var paragraph strings.Builder
		// A paragraph opens on a word, so no list, heading or other block opens on its first line.
		paragraph.WriteString(words[random.IntN(4)])
		for range 2 + random.IntN(18) {
			group := pieces[random.IntN(len(pieces))]
			paragraph.WriteString(group[random.IntN(len(group))])
		}
		paragraph.WriteString("\n")
		markdowns[index] = paragraph.String()
	}
	parseAsTheEngine(t, markdowns)
	t.Logf("%d generated paragraphs", len(markdowns))
}

// Go decodes a named reference from HTML's table and the engine from the `character-entities`
// package, so a name one knows and the other does not would read in the agent's copy as written
// and in the editor as its character. Every name the engine itself loads (gen/entity-names.ts) is
// read as a paragraph of its own, and Go must read it as the engine does, which decodes each.
func TestEveryNameTheEngineKnowsReadsAsTheEngineReadsIt(t *testing.T) {
	raw := genResult(t, "entity-names.ts")
	names := strings.Fields(string(raw))
	if len(names) <= 2000 {
		t.Fatalf("entity-names.ts wrote %d names, want the more than 2,000 of character-entities", len(names))
	}
	markdowns := make([]string, len(names))
	for index, name := range names {
		markdowns[index] = "&" + name + ";\n"
	}
	for index, doc := range parseAsTheEngine(t, markdowns) {
		if doc == nil {
			continue
		}
		markdown := markdowns[index]
		if text := doc.Children[0].Children; len(text) == 1 && text[0].Text == strings.TrimSuffix(markdown, "\n") {
			t.Errorf("%q reads as written in both, where the engine knows the name", markdown)
		}
	}
	t.Logf("%d names", len(names))
}

// A value holding `&` or a backslash must be written so that it reads back as itself: a title or
// an image's address written as it stands would read back decoded, `&amp;` as `&`, in the editor
// and in Go alike, and each write would decode it again. Each value is held in paragraph text, a
// link's and an image's title, an image's address and alt text and a footnote label, outside a
// table and in a cell; its rendering must read back as the tree in both parsers. Markdown whose
// title and address hold an escaped reference must likewise render to markdown that reads back as
// its own reading.
func TestReferencesInEveryValueRenderAndReadBackInBothParsers(t *testing.T) {
	values := []string{"&amp;", "&copy x", "&#38 y", "&#128;", `a\&amp;b`, "&#00000065;", "&foo;"}
	parsed := func(markdown string) *Node {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q): %v", markdown, err)
		}
		return doc
	}
	// inline is the paragraph holding a template's content: its table's body cell's, or its first.
	inline := func(doc *Node) *Node {
		if cell := firstNodeOfType(doc, "table_row"); cell != nil {
			return cell.Children[0].Children[0]
		}
		return doc.Children[0]
	}
	holders := []struct {
		name, content, after string
		set                  func(doc *Node, value string)
	}{
		{"paragraph text", "a", "", func(doc *Node, value string) { inline(doc).Children[0].Text = value }},
		{"link title", `[a](u "t")`, "", func(doc *Node, value string) { inline(doc).Children[0].Marks[0].Attrs["title"] = value }},
		{"image title", `![a](u "t")`, "", func(doc *Node, value string) { inline(doc).Children[0].Attrs["title"] = value }},
		{"image address", "![a](u)", "", func(doc *Node, value string) { inline(doc).Children[0].Attrs["src"] = value }},
		{"image alt", "![a](u)", "", func(doc *Node, value string) { inline(doc).Children[0].Attrs["alt"] = value }},
		{"footnote label", "x[^l]", "\n[^l]: d\n", func(doc *Node, value string) {
			firstNodeOfType(inline(doc), "footnote_reference").Attrs["label"] = value
			doc.Children[len(doc.Children)-1].Attrs["label"] = value
		}},
	}
	type tree struct {
		name string
		doc  *Node
	}
	var trees []tree
	for _, holder := range holders {
		for _, value := range values {
			// A footnote label ends at a space, so markdown has no label holding one.
			if holder.name == "footnote label" && strings.Contains(value, " ") {
				continue
			}
			for _, form := range []struct{ name, before, after string }{{"", "", "\n"}, {" in a table cell", "| h |\n| --- |\n| ", " |\n"}} {
				doc := parsed(form.before + holder.content + form.after + holder.after)
				holder.set(doc, value)
				trees = append(trees, tree{holder.name + form.name + " holding " + value, doc})
			}
		}
	}
	addressWithASpace := parsed("![a](u)\n")
	addressWithASpace.Children[0].Children[0].Attrs["src"] = "u &amp; v"
	trees = append(trees, tree{"image address holding a space and &amp;", addressWithASpace})
	for _, markdown := range []string{"[a](u \"\\&amp;\")\n", "![a](u\\&amp;v)\n"} {
		trees = append(trees, tree{"the reading of " + markdown, parsed(markdown)})
	}
	rendered := make([]string, len(trees))
	for index, tree := range trees {
		markdown, err := Render(tree.doc)
		if err != nil {
			t.Fatalf("%s: Render: %v", tree.name, err)
		}
		rendered[index] = markdown
	}
	readings := engineReadings(t, rendered)
	for index, tree := range trees {
		if back, err := Parse(rendered[index]); err != nil || !back.Equal(tree.doc) {
			got, _ := back.JSON()
			want, _ := tree.doc.JSON()
			t.Errorf("%s: Go reads the rendering %q back as %s (%v), want %s", tree.name, rendered[index], got, err, want)
		}
		if engine := readings[index]; engine == nil || !engine.Equal(tree.doc) {
			got, _ := engine.JSON()
			want, _ := tree.doc.JSON()
			t.Errorf("%s: the engine reads the rendering %q back as %s, want %s", tree.name, rendered[index], got, want)
		}
	}
	t.Logf("%d trees rendered and read back", len(trees))
}

// A stored document whose markdown already reads back as its tree must render to the same bytes,
// so a write that changes nothing changes no byte: a name the engine does not know in a title and
// an address, an `&` opening no reference in a title, and text whose `&amp;` spells `&amp;` keep
// their spelling.
func TestReferenceSpellingsThatReadBackRenderUnchanged(t *testing.T) {
	for _, markdown := range []string{"[a](u \"&foo;\")\n", "![a](u&foo;)\n", "[a](u \"AT&T\")\n", "Plain &amp;amp; end.\n"} {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q): %v", markdown, err)
		}
		if rendered, err := Render(doc); err != nil || rendered != markdown {
			t.Errorf("Render(Parse(%q)) = %q (%v), want it unchanged", markdown, rendered, err)
		}
	}
}
