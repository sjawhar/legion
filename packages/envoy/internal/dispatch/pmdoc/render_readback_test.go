package pmdoc

import (
	"encoding/json"
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
	cmd := exec.Command("bun", "run", "differential.ts", in, out, "md")
	cmd.Dir = "gen"
	if output, err := cmd.CombinedOutput(); err != nil {
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
