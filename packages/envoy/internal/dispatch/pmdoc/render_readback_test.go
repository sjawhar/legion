package pmdoc

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// form for it. Each row is a tree the editor can hold whose rendering the reader before
// sjawhar/legion#1563 misreads, and the markdown whose reading both parsers must give the
// rendering back as.
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
		// 10. A link destination is written so that both parsers, which decode its backslash
		// escapes, read back its href: `<` and `>` escaped between angle brackets, and a backslash
		// escaped before ASCII punctuation or at its end.
		{"link destination holding a parenthesis and `>`", bareURL("https://x.com/(a)>"), "[https://x.com/(a)>](<https://x.com/(a)\\>>) b\n"},
		{"link destination ending in a backslash", bareURL("https://x.com/a>b\\"), "[https://x.com/a>b\\\\](https://x.com/a>b\\\\) b\n"},
		{"link destination holding a backslash before punctuation", bareURL("https://x.com/a\\@b>"), "[https://x.com/a\\\\@b>](https://x.com/a\\\\@b>) b\n"},
		{"link destinations holding escapes", parsed("[a](<x\\>y\\\\>) [b](x\\_y) [c](<\\<z>)\n"), "[a](x>y\\\\) [b](x_y) [c](\\<z)\n"},
		// A control character a destination's reference decodes to is written as that reference
		// again: raw, a tab or line feed ends the destination and a form feed is no destination
		// character to the engine.
		{"link destination holding a decoded line feed", parsed("[x](https://a.b/?q=a&#10;b)\n"), "[x](https://a.b/?q=a&#10;b)\n"},
		{"link destination holding a decoded tab", parsed("[x](a&#9;b)\n"), "[x](a&#9;b)\n"},
		{"link destination holding a decoded form feed", parsed("[x](a&#12;b)\n"), "[x](a&#12;b)\n"},
		{"image address holding a decoded tab", parsed("![a](u&#9;v)\n"), "![a](u&#9;v)\n"},
		{"cell link destination holding a decoded line feed", parsed("| h |\n| --- |\n| [x](a&#10;b) |\n"), "| h |\n| --- |\n| [x](a&#10;b) |\n"},
		// 11. Plain text whose www domain an underscore in its last two parts spoils reads as text,
		// but the backslash the writer escapes that underscore with ends the domain before it,
		// which then reads as a www URL; the dot after `www` is escaped as well, so neither reads one.
		{"heading text with a www domain spoiled by underscores", texts("# a\n", "_www.1.2.3.4__;b"), "# \\_www.1.2.3.4__;b\n"},
		{"paragraph text with a www domain spoiled by a trailing underscore", texts("a\n", "www.a.b_;x"), "www.a.b_;x\n"},
		{"cell text with a www domain spoiled by an underscore before its last dot", cellText(table, "www.a_.b"), "| h |\n| --- |\n| www.a_.b |\n"},
		// 12. A `|` in a cell's code after an odd run of backslashes has no spelling in a cell, where
		// `\\|` ends the cell, so it is written as text after the code: the cell keeps every
		// character, that one no longer code.
		{"code holding a backslash before a pipe in a body cell", cells(table, cellChange{row: 1, inline: "`a\\|b`\n"}), "| h |\n| --- |\n| `a\\`\\|`b` |\n"},
		{"code holding a backslash before a pipe in a header cell", cells(table, cellChange{row: 0, inline: "`a\\|b`\n"}), "| `a\\`\\|`b` |\n| --- |\n| c |\n"},
		{"bold code holding a backslash before a pipe", cells(table, cellChange{row: 1, inline: "**`a\\|b`**\n"}), "| h |\n| --- |\n| **`a\\`\\|`b`** |\n"},
		{"a grep alternation's code", cells(table2, cellChange{row: 1, cell: 1, inline: "`grep -n 'one\\|two' notes.txt`\n"}), "| h1 | h2 |\n| --- | --- |\n| c1 | `grep -n 'one\\`\\|`two' notes.txt` |\n"},
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

// A www link is written as its bare text wherever the browser editor's engine and Go both read it
// back from that text: after a line's head, whitespace, punctuation or a symbol
// (renderer.wwwMayFollow) - a quote, a code span's fence and inline HTML among them - where its
// text opens no email and the text written after it does not run on into it. It is written as an
// explicit link, which both read back as the tree, after a letter or a digit, where neither reads a
// www URL; where its text opens with an email, which the tokenizer tries first (isBareWWWLink); and
// in a run whose bare writing does not read back as written, because what follows it continues it -
// a letter outside ASCII, a backslash, another www link's label - where the run is written again
// with every www link explicit (runSpelling.angleURLs). Where what follows it is plain text opening
// with an address only the tokenizer reads (`http://localhost`), the run is written again with
// every bare URL explicit and that address's colon escaped, a scheme's URL as an autolink.
func TestAWWWLinkTheBareTextWouldLoseIsWrittenAsALink(t *testing.T) {
	link := func(text string) *Node {
		return &Node{Type: "text", Text: text, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": "http://" + text, "title": nil}}}}
	}
	urlLink := func(url string) *Node {
		return &Node{Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url, "title": nil}}}}
	}
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	code := func(value string) *Node { return &Node{Type: "text", Text: value, Marks: []Mark{{Type: "inlineCode"}}} }
	html := func(value string) *Node { return &Node{Type: "html", Attrs: Attrs{"value": value}} }
	tests := []struct {
		name   string
		inline []*Node
		want   string
	}{
		{"after a quote and after a space, both bare", []*Node{text(`see "`), link("www.a.test"), text(`" and `), link("Www.b.test"), text(".")},
			"see \"www.a.test\" and Www.b.test.\n"},
		{"after a letter", []*Node{text("ab"), link("www.a.test"), text(" c")},
			"ab[www.a.test](http://www.a.test) c\n"},
		{"text opening with an email", []*Node{text("see "), link("www.x@y.com"), text(" b")},
			"see [www.x@y.com](http://www.x@y.com) b\n"},
		{"after inline HTML, before a letter outside ASCII it runs on into", []*Node{html("<b>"), link("www.a.test"), text("é")},
			"<b>[www.a.test](http://www.a.test)é\n"},
		{"after a code span, before a backslash it runs on into", []*Node{code("c"), link("www.a.test"), text(`\b`)},
			"`c`[www.a.test](http://www.a.test)\\b\n"},
		{"before another www link's label it runs on into", []*Node{link("www.a.test"), link("www.b.test"), text("|x")},
			"[www.a.test](http://www.a.test)[www.b.test](http://www.b.test)|x\n"},
		{"before plain text opening with a URL only the tokenizer reads", []*Node{link("www.a.test"), text("http://localhost x")},
			"[www.a.test](http://www.a.test)http\\://localhost x\n"},
		{"a scheme's URL before plain text opening with a URL only the tokenizer reads", []*Node{urlLink("https://a.test"), text("http://localhost x")},
			"<https://a.test>http\\://localhost x\n"},
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

// Go reads every bare web address as the browser editor's engine reads it: remark-gfm's tokenizer
// where the tokenizer starts an address in the markdown, and its transform over the decoded text
// it leaves - after an escape or a character reference, after any punctuation, a letter outside
// ASCII or a digit, at a line or a table cell opening with punctuation. Each row is markdown both
// must read alike, the rows where the two engine readers end an address differently among them;
// a row marked writtenAsIs is also written back exactly as it is, with no backslash before a
// scheme's colon. The plain texts a person types in the browser editor, which has no autolink
// input rule, are written and read back alike by both too.
func TestURLReadingsAgreeWithTheEngine(t *testing.T) {
	tests := []struct {
		markdown    string
		writtenAsIs bool
	}{
		{"see www\\.x.test b\n", false},
		{"see HTTP\\://x.test b\n", false},
		{"see http\\://x.test b\n", false},
		{"x;https://x.test b\n", true},
		{"x#https://x.test b\n", false},
		{"x&https://x.test b\n", false},
		{"\\*https://x.test b\n", false},
		{"\\(www.x.test b\n", false},
		{"\"www.a.test/?u=https://b.test\"\n", false},
		{"\"https://x.test\" b\n", false},
		{"see \"https://x.test\" and 'https://y.test'.\n", true},
		{"see www.x.test b\n", true},
		{"see HTTP://x.test b\n", true},
		// The tokenizer and the transform end these addresses differently.
		{"x;https://x.test/a_ b\n", false},
		{"x;https://x.test/a<b c\n", false},
		{"x;https://a.test/*b* c\n", false},
		{"see http\\://x.test/a_ b\n", false},
		{"see http\\://x.test/a<b c\n", false},
		// Each position the engine starts an address at.
		{"éhttps://x.test b\n", false},
		{"x—https://x.test b\n", false},
		{"x;www.x.test b\n", false},
		{"x]https://x.test b\n", false},
		{"x|https://x.test b\n", false},
		{"x!https://x.test b\n", false},
		{"&amp;https://x.test b\n", false},
		{"x\"https://x.test\" b\n", false},
		{"[https://x.test b\n", false},
		{"x1https://x.test b\n", false},
		{"| a | b |\n| --- | --- |\n| x | \"https://x.test\" b |\n", false},
		{"`c`;https://x.test b\n", false},
		{"foo;\nhttps://x.test b\n", false},
		// INFRA-172's wider comparison.
		{"See <http://e.test/a\\\\b now.\n", false},
		{"www.e.test*b*\n", false},
		{"`www.e.test_\n", false},
		// A www address inside a link label that never forms a link (INFRA-534's corpus).
		{"y [.a www.a.com/x]\\(u) z\n", false},
		// An explicit link's destination, its escapes and character references decoded.
		{"[a](<x\\>y\\\\>) [b](x\\_y) [c](<\\<z>) d\n", false},
		{"[x](https://x.test/one&amp;amp;two) [y](u&copy;v) [z](u\\&amp;v)\n", false},
		// A www address in a link label that does not close where the label first seems to.
		{"[[www.a.com/~x[\\![ *.](u)\n", false},
		{"[_~www.a.com/~x\\![**www.a.com/~x](u)\n", false},
		// A name that is a character reference only with its `;` (`&copy`, `&amp`) is text to the
		// engine, inside an address the transform reads as anywhere else (INFRA-531 testing).
		{"see http\\://x.test/?a=1&copy=2 b\n", false},
		{"x;www.x.test/?a=1&amp=2 b\n", false},
	}
	plain := func(text string) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}}}
	}
	typed := []string{"see www.x.test b", "see HTTP://x.test b", "see http://x.test b"}
	markdowns := make([]string, 0, len(tests)+len(typed))
	for _, test := range tests {
		markdowns = append(markdowns, test.markdown)
	}
	for _, text := range typed {
		markdown, err := Render(plain(text))
		if err != nil {
			t.Fatal(err)
		}
		markdowns = append(markdowns, markdown)
	}
	readings := engineReadings(t, markdowns)
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		if readings[index] == nil || !doc.Equal(readings[index]) {
			t.Errorf("%q: Go reads %s, the engine %s", markdown, readingSummary(doc), readingSummary(readings[index]))
		}
		if index < len(tests) && tests[index].writtenAsIs {
			if rendered, err := Render(doc); err != nil || rendered != markdown {
				t.Errorf("%q is written back as %q (%v), want it as it is", markdown, rendered, err)
			}
		}
	}
}

// A link Go reads from a bare address at a position the engine starts one is written back as Go
// read it and the engine reads that writing alike, so a document holding it is stored rather than
// refused (INFRA-531 testing): an address holding `>` after a closing parenthesis, one ending in a
// backslash before an escaped character, and one between a text `<` and a later `>`. A text `<`
// a task item's text opens with is escaped only where the engine opens an HTML block at it, and
// those rows are also written back exactly as they are.
func TestALinkReadFromABareAddressAfterPunctuationIsWrittenBack(t *testing.T) {
	markdowns := []string{
		"x;https://x.test/(a)> b\n",
		"\\(http://x.test/a>b\\_\n",
		// A text `<` whose `>` comes after an address in a later text node is escaped, so the two
		// never form one autolink around the address's link.
		"&lt;mailto:x;a@b.cc>\n",
		"&lt;javascript:alert(1);'a@b.cc'>\n",
		"&lt;javascript:void(0);www.a.b>\n",
		"&lt;javascript:void(a@b.cc)>\n",
		// So is one whose later markdown closes it into inline raw HTML: an open tag, a comment
		// or a processing instruction around an address's link.
		"Set \\<input value=me@x.cc> in the form.\n",
		"Use \\<a href=https://x.cc/> here.\n",
		"x &lt;!-- see x;https://a.bc/ --> y\n",
		"x &lt;?php x;a@b.cc ?> y\n",
		"x &lt;!-- a@b.cc --> y\n",
		// So is one that only CommonMark and the engine close into raw HTML, not goldmark: a
		// declaration after a lowercase letter, and a tag whose unquoted value holds a control
		// character.
		"Set \\<!doctype x;a@b.cc> now\n",
		"Set \\<!x;a@b.cc> now\n",
		"\\<!x;a@b.cc>\n",
		"\\<a b=\x01;a@b.cc>\n",
		// At a block's first character the engine also opens an HTML block (micromark's html-flow),
		// whose start conditions accept more than inline raw HTML: a complete tag alone on its line
		// whose unquoted value ends at `=` and reads another one, and a declaration, a comment, a
		// processing instruction, a CDATA section, a raw or a block tag name with no closer at all.
		"\\<span title=a=b;a@b.cc>\n",
		"\\<a href=x?a=1&b=me@x.cc>\n",
		"\\<a b=x=*y*>\n",
		"\\<script x;a@b.cc\n",
		"\\<!-- x;a@b.cc\n",
		"\\<?x x;a@b.cc\n",
		"\\<!x x;a@b.cc\n",
		"\\<![CDATA[ x;a@b.cc\n",
		"\\<div x;a@b.cc\n",
		"> \\<span title=a=b;a@b.cc>\n",
		"- \\<span title=a=b;a@b.cc>\n",
	}
	asIs := []string{
		// A continuation line of a task item, and a later paragraph of one, start a block, where
		// the engine opens an HTML block as at any other.
		"- [ ] a\\\n  \\<!-- todo later\n",
		"- [ ] a\n\n  \\<div class=x\n",
		// Text written right after a task item's checkbox is read inside the item's paragraph,
		// where no HTML block opens: its `<` is written as it is.
		"- [ ] <!-- todo later\n",
		"- [ ] <div class=x\n",
		"- [x] <!doctype html\n",
		"1. [ ] <script x\n",
		"> - [ ] </div\n",
	}
	markdowns = append(markdowns, asIs...)
	docs := make([]*Node, len(markdowns))
	rendered := make([]string, len(markdowns))
	for index, markdown := range markdowns {
		doc, err := Parse(markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		docs[index] = doc
		if rendered[index], err = Render(doc); err != nil {
			t.Fatalf("Render(Parse(%q)) = %v", markdown, err)
		}
	}
	readings := engineReadings(t, rendered)
	for index, markdown := range markdowns {
		back, err := Parse(rendered[index])
		if err != nil || !back.Equal(docs[index]) || readings[index] == nil || !readings[index].Equal(back) {
			t.Errorf("%q reads in Go as %s, is written as %q, which reads back in Go as %s (%v) and in the engine as %s",
				markdown, readingSummary(docs[index]), rendered[index], readingSummary(back), err, readingSummary(readings[index]))
		}
		if index >= len(markdowns)-len(asIs) && rendered[index] != markdown {
			t.Errorf("%q is written back as %q, want it as it is", markdown, rendered[index])
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
			got, _ := doc.JSON()
			engine, _ := readings[index].JSON()
			t.Errorf("%q:\nGo reads %s\nthe engine reads %s", markdown, got, engine)
			continue
		}
		trees[index] = doc
	}
	return trees
}

// rendersBackInBothParsers renders each tree and reads each rendering back with Parse and with the
// engine (engineReadings), and reports, under the tree's name, each that does not read back as the
// tree in either, with the rendering and both trees.
func rendersBackInBothParsers(t *testing.T, names []string, trees []*Node) {
	t.Helper()
	rendered := make([]string, len(trees))
	for index, doc := range trees {
		markdown, err := Render(doc)
		if err != nil {
			t.Fatalf("%s: Render: %v", names[index], err)
		}
		rendered[index] = markdown
	}
	readings := engineReadings(t, rendered)
	for index, doc := range trees {
		if back, err := Parse(rendered[index]); err != nil || !back.Equal(doc) {
			got, _ := back.JSON()
			want, _ := doc.JSON()
			t.Errorf("%s: Go reads the rendering %q back as %s (%v), want %s", names[index], rendered[index], got, err, want)
		}
		if engine := readings[index]; engine == nil || !engine.Equal(doc) {
			got, _ := engine.JSON()
			want, _ := doc.JSON()
			t.Errorf("%s: the engine reads the rendering %q back as %s, want %s", names[index], rendered[index], got, want)
		}
	}
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
	var markdowns []string
	for _, row := range referenceRows {
		for _, site := range sites {
			if markdown, ok := site.place(row); ok {
				markdowns = append(markdowns, markdown)
			}
		}
	}
	trees := parseAsTheEngine(t, markdowns)
	const plain = "Plain \\&amp; end.\n"
	index := slices.Index(markdowns, plain)
	if index < 0 {
		t.Fatalf("no row places the paragraph %q", plain)
	}
	if doc := trees[index]; doc != nil {
		if text := doc.Children[0].Children; len(text) != 1 || text[0].Text != "Plain &amp; end." {
			t.Errorf("Go reads %q as %s, want the text %q", plain, inlineSummary(doc.Children[0]), "Plain &amp; end.")
		}
	}
	t.Logf("%d rows at %d sites: %d markdowns", len(referenceRows), len(sites), len(markdowns))
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
	var names []string
	var trees []*Node
	for _, holder := range holders {
		for _, value := range values {
			// A footnote label ends at a space, so markdown has no label holding one.
			if holder.name == "footnote label" && strings.Contains(value, " ") {
				continue
			}
			for _, form := range []struct{ name, before, after string }{{"", "", "\n"}, {" in a table cell", "| h |\n| --- |\n| ", " |\n"}} {
				doc := parsed(form.before + holder.content + form.after + holder.after)
				holder.set(doc, value)
				names = append(names, holder.name+form.name+" holding "+value)
				trees = append(trees, doc)
			}
		}
	}
	addressWithASpace := parsed("![a](u)\n")
	addressWithASpace.Children[0].Children[0].Attrs["src"] = "u &amp; v"
	names = append(names, "image address holding a space and &amp;")
	trees = append(trees, addressWithASpace)
	for _, markdown := range []string{"[a](u \"\\&amp;\")\n", "![a](u\\&amp;v)\n"} {
		names = append(names, "the reading of "+markdown)
		trees = append(trees, parsed(markdown))
	}
	rendersBackInBothParsers(t, names, trees)
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

// An image's address that ends in a backslash is written with that backslash doubled, so it does
// not escape the `)` or `>` that closes the address. Each markdown holds such an address, outside a
// table and in a cell, in the plain and the angle form; Go must read it as the engine does, a write
// of it must be accepted, and its rendering must read back as its reading in both parsers.
func TestAnImageAddressEndingInABackslashRendersAndReadsBack(t *testing.T) {
	markdowns := []string{
		"![a](u\\\\)\n",
		"![a](u\\\\\\\\)\n",
		"![a](u\\\\ \"t\")\n",
		"![a](<u v\\\\>)\n",
		"| h |\n| --- |\n| ![a](u\\\\) |\n",
		"| h |\n| --- |\n| ![a](<u v\\\\>) |\n",
	}
	trees := parseAsTheEngine(t, markdowns)
	var names []string
	var read []*Node
	for index, doc := range trees {
		if doc == nil {
			continue
		}
		if err := RefuseMisreadDocument(doc); err != nil {
			t.Errorf("a write of %q is refused: %v", markdowns[index], err)
		}
		names = append(names, "the reading of "+markdowns[index])
		read = append(read, doc)
	}
	rendersBackInBothParsers(t, names, read)
}
