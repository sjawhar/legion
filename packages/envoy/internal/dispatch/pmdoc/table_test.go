package pmdoc

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// The budget pays for the cells goldmark's table transformer will pad before it runs, so
// findTableRows must read the table goldmark reads: whether one forms, where its header is, and how
// many cells it writes and holds once padded. Each shape here, and every paragraph of the corpus
// fixtures, is read by both, the transformer on a copy apart from any document.
func TestFindTableRowsReadsWhatGoldmarkReads(t *testing.T) {
	shapes := []string{
		"| a | b |\n| - | - |\n| 1 | 2 |\n",
		"| a |\n| - | - |\n| 1 | 2 |\n",
		"| a |\n| - | - | - |\n| 1 |\n",
		"| a | b | c |\n| - | - |\n| 1 |\n",
		"x\n| a |\n| - |\n| 1 |\n",
		"x\ny\n| a | b |\n| - | - |\n",
		"| a | b |\n|:-|-:|\n| 1 |\n| 1 | 2 | 3 |\n",
		"| a \\| b | c |\n| - | - |\n| `x \\| y` |\n",
		"a\n:-\nb\n",
		"a | b\n--- | ---\nc\n",
		"| a |\n |  :---:  | \n| 1 |\n",
		"|\n| - |\n| 1 |\n",
		"| a |\n| - |\n",
		"| - |\n| a |\n| - |\n",
		"| a |\n| - |\n| - |\n",
		"| a |\n| -\v |\n| 1 |\n",
		"| a | b |\n| - | - |\n=\n==\n-\n",
		"| | |\n|-|-|\n||\n|\n",
		"| a | b |   \n| - | - |  \n| 1 |\n",
		"| a |\n  | - | - |\n| 1 |\n",
		"| a |\n    | - | - |\n| 1 |\n",
		"| a |\n \t| - | - |\n| 1 |\n",
		"| é | ü |\n| - | - |\n| ñ |\n",
		"| a |\n|---|---|---|\n",
		"| a |\n| --- |\n| b | c | d |\n| e |\n",
		"| a |\n| -- :|\n",
		"| a |\n| : |\n",
		"| a |\n| :-: | ::- |\n",
		"text only\nno table here\n",
	}
	for _, shape := range shapes {
		assertTableModelMatchesGoldmark(t, shape, splitLines([]byte(shape)), []byte(shape))
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	plain := goldmark.New()
	paragraphs := 0
	for _, entry := range entries {
		source, err := os.ReadFile(filepath.Join("testdata", "corpus", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		_ = ast.Walk(plain.Parser().Parse(gmtext.NewReader(source)), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if paragraph, ok := node.(*ast.Paragraph); ok && entering && paragraph.Lines().Len() > 1 {
				paragraphs++
				assertTableModelMatchesGoldmark(t, entry.Name(), paragraph.Lines(), source)
			}
			return ast.WalkContinue, nil
		})
	}
	if paragraphs == 0 {
		t.Fatal("the corpus fixtures hold no paragraph of two lines or more")
	}
}

func assertTableModelMatchesGoldmark(t *testing.T, name string, lines *gmtext.Segments, source []byte) {
	t.Helper()
	rows, found, _ := findTableRows(lines, source, 1)
	trial := ast.NewParagraph()
	trial.SetLines(tabExpandedLines(lines, source))
	holder := ast.NewDocument()
	holder.AppendChild(holder, trial)
	tableTransformer.Transform(trial, gmtext.NewReader(source), parser.NewContext())
	var table *extensionast.Table
	for child := holder.FirstChild(); child != nil; child = child.NextSibling() {
		if formed, ok := child.(*extensionast.Table); ok {
			table = formed
		}
	}
	if found != (table != nil) {
		t.Errorf("%q: findTableRows found a table = %v, goldmark formed one = %v", name, found, table != nil)
		return
	}
	if table == nil {
		return
	}
	header := 0
	if before, ok := table.PreviousSibling().(*ast.Paragraph); ok {
		header = before.Lines().Len()
	}
	var written, implied int
	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			implied++
			if cell.Lines().Len() > 0 {
				written++
			}
		}
	}
	padding := rows.padding(lines, source)
	if rows.header != header || padding.written != written || padding.implied != implied {
		t.Errorf("%q: findTableRows read header %d, %d cells written of %d; goldmark read header %d, %d of %d",
			name, rows.header, padding.written, padding.implied, header, written, implied)
	}
}

// splitLines is source's lines, each a segment holding its line ending, as a paragraph holds them.
func splitLines(source []byte) *gmtext.Segments {
	lines := gmtext.NewSegments()
	for start := 0; start < len(source); {
		end := start + bytes.IndexByte(source[start:], '\n') + 1
		if end <= start {
			end = len(source)
		}
		lines.Append(gmtext.NewSegment(start, end))
		start = end
	}
	return lines
}

// Every table goldmark reads is paid for before its cells are built, however it is written: a
// header narrower than its delimiter row, which goldmark pads with the rest; a setext underline
// under the table, which goldmark's setext parser asks the table transformer about first; and
// rows of `=`, each such a question.
func TestParseChargesEveryTableGoldmarkReads(t *testing.T) {
	const maximumAllocation = 8 << 20
	for _, shape := range []struct {
		name, markdown string
	}{
		{
			name:     "a one-cell header over a 500-column delimiter row",
			markdown: "| header |\n" + strings.Repeat("| --- ", 500) + "|\n" + strings.Repeat("| body |\n", 500),
		},
		{
			name:     "a setext underline under the table",
			markdown: tablePaddingBomb(500) + "===\n",
		},
		{
			name:     "400 rows of = under a 300-column header",
			markdown: strings.Repeat("| h ", 300) + "|\n" + strings.Repeat("| - ", 300) + "|\n" + strings.Repeat("=\n", 400),
		},
	} {
		for _, reader := range []struct {
			name string
			read func(string) (*Node, error)
		}{
			{name: "Parse", read: Parse},
			{name: "ParseForWrite", read: func(markdown string) (*Node, error) { return ParseForWrite(markdown, nil) }},
		} {
			t.Run(shape.name+"/"+reader.name, func(t *testing.T) {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				_, err := reader.read(shape.markdown)
				runtime.ReadMemStats(&after)
				allocated := after.TotalAlloc - before.TotalAlloc
				t.Logf("allocated=%d", allocated)
				if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "limit 10000") {
					t.Errorf("error = %v, want the padding refused naming limit 10000", err)
				}
				if allocated > maximumAllocation {
					t.Errorf("allocated %d bytes, want at most %d", allocated, maximumAllocation)
				}
			})
		}
	}
}

// Goldmark's setext parser asks about every line opening with `=` or `-` under an open paragraph,
// and each asks whether the paragraph's lines make a table: read once each, a paragraph of such
// lines costs what its lines do, where reading them all for every question cost their square.
func TestSetextLookAheadReadsEachLineOnce(t *testing.T) {
	const maximumAllocation = 32 << 20
	for _, shape := range []struct {
		name, markdown string
	}{
		{name: "8,000 lines of =a under a paragraph", markdown: "a\n" + strings.Repeat("=a\n", 8000)},
		{name: "2,000 lines of =a under a table", markdown: "| a | b |\n| - | - |\n" + strings.Repeat("=a\n", 2000)},
	} {
		t.Run(shape.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := Parse(shape.markdown)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated=%d", allocated)
			if err != nil {
				t.Fatalf("Parse = %v, want it read", err)
			}
			if allocated > maximumAllocation {
				t.Errorf("allocated %d bytes, want at most %d", allocated, maximumAllocation)
			}
		})
	}
}

// A cell ends at every `|` not written `\|`, inside code and links too, and goldmark drops the cells
// a body row holds past its delimiter row's, which the browser editor's parser keeps: a row whose
// code span holds a bare `|` lost the rest of its text when it was stored. Such a row is refused,
// in a container and after text in its paragraph too, naming what it holds and both ways to write
// it; written `\|`, the same row is read whole.
func TestParseRefusesARowHoldingMoreCellsThanItsTable(t *testing.T) {
	const row = "| `runtime.ts` | Hold `x: Promise<void> | undefined`; every caller waits. |\n"
	for _, markdown := range []string{
		"| File | Rule |\n| --- | --- |\n" + row,
		"> | File | Rule |\n> | --- | --- |\n> " + row,
		"- | File | Rule |\n  | --- | --- |\n  " + row,
		"text\n| File | Rule |\n| --- | --- |\n" + row,
	} {
		_, err := ParseForWrite(markdown, nil)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "3 cells where its table has 2") || !strings.Contains(err.Error(), `as \|`) || !strings.Contains(err.Error(), "as many cells as the row") {
			t.Errorf("ParseForWrite(%q) = %v, want a schema refusal naming the row's 3 cells and both ways to write it", markdown, err)
		}
	}
	if _, err := ParseForWrite("| a | b |\n| - | - |\n| 1 | 2 | 3 |\n", nil); !errors.Is(err, ErrSchema) {
		t.Errorf("a row of three plain cells under two = %v, want a schema refusal", err)
	}
	// The refusal quotes the row as its author wrote it, so its backslashes are not doubled.
	if _, err := ParseForWrite("| a | b |\n| - | - |\n| p | q | z \\|\n", nil); err == nil || !strings.Contains(err.Error(), `written "| p | q | z \|"`) {
		t.Errorf("a row holding a backslash is refused with %v, want it quoted as written", err)
	}
	// Past the table's width goldmark drops only blank cells here, and the text is all kept.
	blank, err := ParseForWrite("| a | b |\n| - | - |\n| x | y | |\n", nil)
	if err != nil {
		t.Fatalf("a row whose third cell is blank = %v, want it read", err)
	}
	if cells := len(blank.Children[0].Children[1].Children); cells != 2 {
		t.Errorf("a row whose third cell is blank reads as %d cells, want the table's 2", cells)
	}
	doc, err := ParseForWrite("| File | Rule |\n| --- | --- |\n"+strings.Replace(row, "> | undefined", "> \\| undefined", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TextContent(doc.Children[0].Children[1].Children[1]); got != "Hold x: Promise<void> | undefined; every caller waits." {
		t.Errorf("the row written with \\| holds %q in its second cell, want the whole text", got)
	}
}

// Goldmark drops a row's closing pipe whatever stands before it, so `| x | y \|` read `y \`, where
// the browser editor's parser reads a closing pipe an odd run of backslashes escapes as the last
// cell's text. After an even run the pipe closes the row in both.
func TestParseKeepsAnEscapedClosingPipeInTheLastCell(t *testing.T) {
	for markdown, want := range map[string][2]string{
		"| a | b |\n| - | - |\n| x | y \\|\n":     {"b", "y |"},
		"| a | b \\|\n| - | - |\n| x | y |\n":     {"b |", "y"},
		"| a | b |\n| - | - |\n| x | y \\\\\\|\n": {"b", "y \\|"},
		"| a | b |\n| - | - |\n| x | y \\\\|\n":   {"b", "y \\"},
	} {
		doc, err := Parse(markdown)
		if err != nil {
			t.Errorf("Parse(%q) = %v", markdown, err)
			continue
		}
		table := doc.Children[0]
		if got := [2]string{TextContent(table.Children[0].Children[1]), TextContent(table.Children[1].Children[1])}; got != want {
			t.Errorf("Parse(%q) second cells = %q, want %q", markdown, got, want)
		}
	}
}

// unescapeTablePipes leaves the tree goldmark's table AST transformer, which pmdoc no longer
// registers, leaves: on every corpus fixture, and on seeded random tables of code spans, escaped
// and doubly escaped pipes, backslashes and marks, in a quote or a list or not, each followed by a
// paragraph of the same, the two make the same nodes over the same bytes.
func TestUnescapeTablePipesLeavesTheTreeGoldmarksTransformerDoes(t *testing.T) {
	read := func(source []byte) (ast.Node, gmtext.Reader, parser.Context) {
		reader := gmtext.NewReader(source)
		pc := parser.NewContext()
		pc.Set(writeBudgetKey, NewWriteBudget())
		return blockReader.md.Parser().Parse(reader, parser.WithContext(pc)), reader, pc
	}
	tables, unescaped := 0, 0
	compare := func(name string, source []byte) {
		t.Helper()
		ours, _, _ := read(source)
		unescapeTablePipes(ours, source)
		theirs, reader, pc := read(source)
		extension.NewTableASTTransformer().Transform(theirs.(*ast.Document), reader, pc)
		got, want := goldmarkTree(ours, source), goldmarkTree(theirs, source)
		if got != want {
			t.Errorf("%s, %q:\nunescapeTablePipes left\n%s\ngoldmark's transformer left\n%s", name, source, got, want)
		}
		if strings.Contains(want, "TableCell") {
			tables++
		}
		if untouched, _, _ := read(source); want != goldmarkTree(untouched, source) {
			unescaped++
		}
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		source, err := os.ReadFile(filepath.Join("testdata", "corpus", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		compare(entry.Name(), source)
	}
	tokens := []string{"`", "``", "\\|", "\\\\|", "\\", "a", " ", "*", "**", "_", "~~", "[", "](u)", "<b>", "|", "`a\\|b`", "`\\\\|`", "` \\| `"}
	random := rand.New(rand.NewPCG(465, 2))
	cell := func() string {
		var text strings.Builder
		for range 1 + random.IntN(8) {
			text.WriteString(tokens[random.IntN(len(tokens))])
		}
		return text.String()
	}
	for index := range 5000 {
		prefix := []string{"", "> ", "- "}[random.IntN(3)]
		var source strings.Builder
		source.WriteString(prefix + "| h | h |\n")
		indent := strings.Repeat(" ", len(prefix))
		if prefix == "> " {
			indent = prefix
		}
		source.WriteString(indent + "| - | - |\n")
		for range 1 + random.IntN(4) {
			source.WriteString(indent + "|")
			for range 1 + random.IntN(3) {
				source.WriteString(" " + cell() + " |")
			}
			source.WriteString("\n")
		}
		// A code span outside any table keeps its backslash.
		source.WriteString("\n" + cell() + "\n")
		compare(fmt.Sprintf("random table %d", index), []byte(source.String()))
	}
	if tables == 0 || unescaped == 0 {
		t.Fatalf("%d inputs read a table and %d had a pipe unescaped; want both", tables, unescaped)
	}
	t.Logf("%d inputs read a table, %d had a pipe unescaped", tables, unescaped)
}

// goldmarkTree is root's nodes, one a line, indented by depth: each node's kind, and a text's bytes
// in source with its segment's bounds.
func goldmarkTree(root ast.Node, source []byte) string {
	var tree strings.Builder
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		depth := 0
		for parent := node.Parent(); parent != nil; parent = parent.Parent() {
			depth++
		}
		fmt.Fprintf(&tree, "%s%s", strings.Repeat("  ", depth), node.Kind())
		if text, ok := node.(*ast.Text); ok {
			fmt.Fprintf(&tree, " [%d,%d) %q", text.Segment.Start, text.Segment.Stop, text.Segment.Value(source))
		}
		tree.WriteByte('\n')
		return ast.WalkContinue, nil
	})
	return tree.String()
}
