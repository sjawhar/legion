package pmdoc

import (
	"encoding/json"
	"fmt"
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

// A www link is written as its bare text wherever the browser editor's engine and Go both read it
// back from that text: after a line's head, whitespace, punctuation or a symbol
// (renderer.wwwMayFollow) - a quote, a code span's fence and inline HTML among them - where its
// text opens no email and the text written after it does not run on into it. It is written as an
// explicit link, which both read back as the tree, after a letter or a digit, where neither reads a
// www URL; where its text opens with an email, which the tokenizer tries first (isBareWWWLink); and
// in a run whose bare writing does not read back as written, because what follows it continues it -
// a letter outside ASCII, a backslash, another www link's label - where the run is written again
// with every www link explicit (runSpelling.angleURLs).
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
			t.Errorf("%q: Go reads %s, the engine %s; recorded as agreeing: true", markdown, readingSummary(doc), readingSummary(readings[index]))
		}
		if index < len(tests) && tests[index].writtenAsIs {
			if rendered, err := Render(doc); err != nil || rendered != markdown {
				t.Errorf("%q is written back as %q (%v), want it as it is", markdown, rendered, err)
			}
		}
	}
}

// readingSummary names each textblock of doc and its inline nodes, with every mark they carry.
func readingSummary(doc *Node) string {
	var blocks []string
	Walk(doc, func(node *Node) bool {
		if len(node.Children) == 0 || len(node.Children[0].Children) != 0 {
			return true
		}
		var parts []string
		for _, child := range node.Children {
			part := fmt.Sprintf("%s %q", child.Type, child.Text)
			for _, mark := range child.Marks {
				part += " " + mark.Type
				if href, ok := mark.Attrs["href"].(string); ok {
					part += " " + href
				}
			}
			parts = append(parts, part)
		}
		blocks = append(blocks, node.Type+": "+strings.Join(parts, " | "))
		return true
	})
	if len(blocks) == 0 {
		return "no text"
	}
	return strings.Join(blocks, " / ")
}
