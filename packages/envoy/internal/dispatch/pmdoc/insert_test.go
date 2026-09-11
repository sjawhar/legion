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
	assertRenderedMarkdown(t, out, "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n\n## Added\n\nNext.\n")
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
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 | new |\n", true)
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
	assertRenderedMarkdown(t, out, "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 | new |\n")
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
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 | new |\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("row fragment was not inserted into the table")
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 | new |\n| A12 | later |\n")
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
	_, _, err = InsertTableRows(doc, anchor, "| A11 | new | extra |\n", true)
	if !errors.Is(err, ErrTableWidth) {
		t.Fatalf("InsertTableRows() error = %v, want ErrTableWidth", err)
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
	out, inserted, err := InsertTableRows(doc, anchor, "| A11 |\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("short row fragment was not inserted into the table")
	}
	assertRenderedMarkdown(t, out, "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 |  |\n")
}

func renderedMarkdown(t *testing.T, doc *Node) string {
	t.Helper()
	markdown, _, err := Render(doc)
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
