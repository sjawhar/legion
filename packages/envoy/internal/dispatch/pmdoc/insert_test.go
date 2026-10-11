package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

func TestBlockInsertAtParagraphAnchorFollowsEnclosingParagraph(t *testing.T) {
	doc, err := Parse("Before anchor after.\n\nNext.\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "anchor", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	position, err := BlockBoundary(doc, anchor, true)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("## Added\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, Range{From: position, To: position}, with)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedMarkdown(t, out, "Before anchor after.\n\n## Added\n\nNext.\n")
}

func TestBlockInsertAtTableCellAnchorFollowsEnclosingTable(t *testing.T) {
	doc, err := Parse("| Key | Value |\n| --- | --- |\n| A10 | old |\n\nNext.\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A10", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	position, err := BlockBoundary(doc, anchor, true)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Parse("## Added\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Splice(doc, Range{From: position, To: position}, with)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| --- | --- |\n| A10 | old |\n\n## Added\n\nNext.\n")
}

func TestTableRowInsertAfterCellAnchorExtendsContainingTable(t *testing.T) {
	doc, err := Parse("| Key | Value |\n| --- | --- |\n| A10 | old |\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A10", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 | new |\n", true, NewWriteBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("row fragment was not inserted into the table")
	}
	markdown := renderedMarkdown(t, out)
	if strings.Contains(markdown, "||") || strings.Contains(markdown, "\\| A11") {
		t.Fatalf("table row rendered as inline text: %q", markdown)
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n")
}

func TestTableRowInsertBeforeCellAnchorExtendsContainingTable(t *testing.T) {
	doc, err := Parse("| Key | Value |\n| --- | --- |\n| A10 | old |\n| A12 | later |\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A12", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 | new |\n", false, NewWriteBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("row fragment was not inserted into the table")
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n| A12 | later |\n")
}

func TestTableRowInsertRejectsRowsWiderThanContainingTable(t *testing.T) {
	doc, err := Parse("| Key | Value |\n| --- | --- |\n| A10 | old |\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A10", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = InsertTableRows(doc, anchor, "| A11 | new | extra |\n", true, NewWriteBudget())
	if !errors.Is(err, ErrTableWidth) {
		t.Fatalf("InsertTableRows() error = %v, want ErrTableWidth", err)
	}
}

// A table-row insert answers as a whole-document write of the same rows does, wherever in the
// fragment a row stands: both drop a blank cell past the table's width, refuse a cell there
// holding text, refuse a line the two parsers read as another block, and store an accepted row as
// the same markdown. Both count the cells as goldmark splits them, so a `|` after an even run of
// backslashes ends no cell on either path, and a line opening a block that interrupts a paragraph
// is no table row to either.
func TestTableRowInsertAnswersAsTheDocumentWrite(t *testing.T) {
	const table = "| Key | Value |\n| --- | --- |\n| A10 | old |\n"
	for _, test := range []struct {
		row string
		// stored is the row as both paths store it, where both accept it as rows.
		stored string
		// refusal is the insert's refusal where both paths refuse the markdown: ErrTableWidth for a
		// row too wide, ErrSchema for one the two parsers read as different blocks. The document
		// write refuses either as ErrSchema.
		refusal error
		// blocks marks markdown that is no table rows: the insert reports it is not rows, for the
		// edit route to write as blocks, and the document write stores it as a block after the table.
		blocks bool
	}{
		{row: "| A11 | new | |", stored: "| A11 | new |"},
		{row: "| A11 | new |   |  |", stored: "| A11 | new |"},
		{row: "| A11 | new | extra |", refusal: ErrTableWidth},
		{row: "| A11 | `x | y` |", refusal: ErrTableWidth},
		{row: `| A11 | new | z \|`, refusal: ErrTableWidth},
		{row: `| A11 | new \| extra |`, stored: `| A11 | new \| extra |`},
		{row: `| A11 | new \\| extra |`, stored: `| A11 | new \\\| extra |`},
		// A space outside ASCII at the fragment's first or last edge is a cell's text to goldmark and
		// to the browser editor's parser, as it is on the fragment's other lines: the browser reads
		// `| A11 | new |` then U+00A0 as three cells, and a U+00A0 before the row's first `|` moves
		// A11 into the second column.
		{row: "| A11 | new |\u00a0", refusal: ErrTableWidth},
		{row: "\u00a0| A11 | new |", refusal: ErrTableWidth},
		{row: "| A11 | x |\n| A12 | y |\u3000", refusal: ErrTableWidth},
		{row: "| A11 | x |\u00a0\n| A12 | y |", refusal: ErrTableWidth},
		// So are a vertical tab and a form feed, which goldmark's table transformer trims from no row.
		{row: "| A11 | new |\v", refusal: ErrTableWidth},
		{row: "| A11 | new |\f", refusal: ErrTableWidth},
		{row: "\f| A11 | new |", refusal: ErrTableWidth},
		// Beside a lone `|` or a line of hyphen cells, such a space is a cell too: the line yields a
		// cell and an unescaped `|`, and is not delimiter-shaped, so the fragment is rows alone.
		{row: "| A11 | x |\n\u00a0|", stored: "| A11 | x |\n| \u00a0 |  |"},
		{row: "| A11 | x | y |\n\u00a0|", refusal: ErrTableWidth},
		{row: "| A11 | x | y |\n|\f", refusal: ErrTableWidth},
		{row: "| --- | --- |\u00a0", refusal: ErrTableWidth},
		{row: "| \u00a0--- | --- |", stored: "| \u00a0--- | --- |"},
		// The first row's indentation is read as any row's: three spaces are nothing, four or a tab
		// open indented code, which the browser editor's parser reads after the table.
		{row: "   | A11 | new |", stored: "| A11 | new |"},
		{row: "    | A11 | new |", refusal: ErrSchema},
		{row: "\t| A11 | new |", refusal: ErrSchema},
		{row: "| A11 | x |\n    | A12 | y |", refusal: ErrSchema},
		// A line markBlockRows refuses is refused as the upload refuses it, however many cells it
		// holds: a list item that cannot interrupt a paragraph, here also too wide, is refused as
		// that block, the refusal walk reading blocks before widths.
		{row: "2. | a | b |", refusal: ErrSchema},
		// A line that opens a list or a quote is no table row to either parser, so its marker is no
		// cell and the insert is not a table-row insert: `- | a |` is written as a list after the
		// table, and `- | a | b |` must be as well.
		{row: "- | a | b |", blocks: true},
		{row: "> | a | b |", blocks: true},
		{row: "- | a |", blocks: true},
	} {
		doc, err := Parse(table)
		if err != nil {
			t.Fatal(err)
		}
		anchor, err := FindQuote(doc, "A10", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		inserted, rows, insertErr := InsertTableRows(doc, anchor, test.row+"\n", true, NewWriteBudget())
		written, writeErr := ParseForWrite(table+test.row+"\n", nil)
		switch {
		case test.blocks:
			if insertErr != nil || rows || writeErr != nil {
				t.Errorf("%q: insert = %v with rows %v, document write = %v, want no table rows and both accepted", test.row, insertErr, rows, writeErr)
			}
		case test.refusal != nil:
			// A too-wide row is TABLE_WIDTH alone: an insert refusal that also read as a schema
			// refusal would answer INVALID_OP on the edit route (docs.invalidSchemaOp).
			if !errors.Is(insertErr, test.refusal) || (test.refusal == ErrTableWidth && errors.Is(insertErr, ErrSchema)) || !errors.Is(writeErr, ErrSchema) {
				t.Errorf("%q: insert = %v, document write = %v, want both refused, the insert with %v", test.row, insertErr, writeErr, test.refusal)
			}
		default:
			if insertErr != nil || !rows || writeErr != nil {
				t.Errorf("%q: insert = %v with rows %v, document write = %v, want both accepted as rows", test.row, insertErr, rows, writeErr)
				continue
			}
			for _, path := range []struct {
				name string
				doc  *Node
			}{{"insert", inserted}, {"document write", written}} {
				got, err := Render(path.doc)
				if err != nil {
					t.Fatal(err)
				}
				if want := table + test.stored + "\n"; got != want {
					t.Errorf("%q: the %s stored %q, want %q", test.row, path.name, got, want)
				}
			}
		}
	}
}

func TestTableRowInsertPadsRowsToContainingTableWidth(t *testing.T) {
	doc, err := Parse("| Key | Value |\n| --- | --- |\n| A10 | old |\n")
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := FindQuote(doc, "A10", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 |\n", true, NewWriteBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("short row fragment was not inserted into the table")
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 |  |\n")
}

func renderedMarkdown(t *testing.T, doc *Node) string {
	t.Helper()
	markdown, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	return markdown
}

func assertRenderedMarkdown(t *testing.T, doc *Node, want string) {
	t.Helper()
	if got := renderedMarkdown(t, doc); got != want {
		t.Fatalf("rendered markdown = %q, want %q", got, want)
	}
}
