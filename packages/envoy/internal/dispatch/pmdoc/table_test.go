package pmdoc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
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
// in a container too, naming what it holds and how to write the pipe; written `\|`, the same row
// is read whole.
func TestParseRefusesARowHoldingMoreCellsThanItsTable(t *testing.T) {
	const row = "| `runtime.ts` | Hold `x: Promise<void> | undefined`; every caller waits. |\n"
	for _, markdown := range []string{
		"| File | Rule |\n| --- | --- |\n" + row,
		"> | File | Rule |\n> | --- | --- |\n> " + row,
		"- | File | Rule |\n  | --- | --- |\n  " + row,
	} {
		_, err := ParseForWrite(markdown, nil)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "3 cells where its table has 2") || !strings.Contains(err.Error(), `written \|`) {
			t.Errorf("ParseForWrite(%q) = %v, want a schema refusal naming the row's 3 cells and how to write the pipe", markdown, err)
		}
	}
	doc, err := ParseForWrite("| File | Rule |\n| --- | --- |\n"+strings.Replace(row, "> | undefined", "> \\| undefined", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := textContent(doc.Children[0].Children[1].Children[1]); got != "Hold x: Promise<void> | undefined; every caller waits." {
		t.Errorf("the row written with \\| holds %q in its second cell, want the whole text", got)
	}
}
