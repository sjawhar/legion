package pmdoc

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func pathString(path BlockPath) string {
	parts := make([]string, len(path.Path))
	for i, entry := range path.Path {
		parts[i] = fmt.Sprintf("%s[%d]", entry.Type, entry.Index)
	}
	return strings.Join(parts, " › ")
}

func TestBlockPathOfPlacesBlocksInTablesAndContainers(t *testing.T) {
	sequentialBlockIDs(t)
	doc := parseFixture(t, "Intro.\n\n| # | Line | Due |\n| --- | --- | --- |\n| 1 | GDM backfill | Oct 1 |\n| 2 | Red-teamer loop | Today |\n\n- item\n  - nested\n")
	blockOf := func(quote string) string {
		t.Helper()
		r, err := FindQuote(doc, quote, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := BlockIDForRange(doc, r)
		if err != nil || id == "" {
			t.Fatalf("block for %q: id=%q err=%v", quote, id, err)
		}
		return id
	}
	cell, err := BlockPathOf(doc, blockOf("Today"))
	if err != nil {
		t.Fatal(err)
	}
	if cell.Type != "paragraph" || pathString(cell) != "table[1] › table_row[2] › table_cell[2] › paragraph[0]" {
		t.Fatalf("cell path = %s (%s)", pathString(cell), cell.Type)
	}
	for i, entry := range cell.Path {
		if entry.ID == "" {
			t.Fatalf("path entry %d carries no block id: %#v", i, cell)
		}
	}
	table := cell.Table
	if table == nil || table.ID != cell.Path[0].ID || table.Row == nil || *table.Row != 2 || table.Column == nil || *table.Column != 2 ||
		table.Header == nil || *table.Header != "Due" || !reflect.DeepEqual(table.Cells, []string{"2", "Red-teamer loop", "Today"}) {
		t.Fatalf("cell table position = %#v, want row 2, column 2 headed Due, cells 2 · Red-teamer loop · Today", table)
	}
	header, err := BlockPathOf(doc, blockOf("Due"))
	if err != nil || *header.Table.Row != 0 || *header.Table.Column != 2 || *header.Table.Header != "Due" ||
		!reflect.DeepEqual(header.Table.Cells, []string{"#", "Line", "Due"}) {
		t.Fatalf("header cell = %#v err=%v, want row 0, column 2, cells # · Line · Due", header.Table, err)
	}
	row, err := BlockPathOf(doc, cell.Path[1].ID)
	if err != nil || row.Type != "table_row" || *row.Table.Row != 2 || row.Table.Column != nil || row.Table.Header != nil ||
		!reflect.DeepEqual(row.Table.Cells, table.Cells) {
		t.Fatalf("row block = %#v err=%v, want row 2 with no column", row, err)
	}
	whole, err := BlockPathOf(doc, cell.Path[0].ID)
	if err != nil || whole.Type != "table" || len(whole.Path) != 1 || whole.Table == nil || whole.Table.Row != nil || whole.Table.Column != nil ||
		whole.Table.Header != nil || whole.Table.Cells != nil {
		t.Fatalf("table block = %#v err=%v, want a table position with no row", whole, err)
	}
	nested, err := BlockPathOf(doc, blockOf("nested"))
	if err != nil || nested.Table != nil || pathString(nested) != "bullet_list[2] › list_item[0] › bullet_list[1] › list_item[0] › paragraph[0]" {
		t.Fatalf("nested paragraph = %s table=%#v err=%v", pathString(nested), nested.Table, err)
	}
	if _, err := BlockPathOf(doc, "missing"); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
}

// A live tree can hold a row wider than its header (the browser pads later); the column past
// the header has an empty header, not a missing one.
func TestBlockPathOfNamesAnEmptyHeaderWhereTheHeaderRowIsShort(t *testing.T) {
	cell := func(kind, value string) *Node {
		return &Node{Type: kind, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}}}
	}
	doc := &Node{Type: "doc", Children: []*Node{{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", "Key")}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "A10"), cell("table_cell", "wide")}},
	}}}}
	EnsureBlockIDs(doc)
	wide := doc.Children[0].Children[1].Children[1].Children[0].Attrs[BlockIDAttr].(string)
	path, err := BlockPathOf(doc, wide)
	if err != nil || *path.Table.Column != 1 || path.Table.Header == nil || *path.Table.Header != "" ||
		!reflect.DeepEqual(path.Table.Cells, []string{"A10", "wide"}) {
		t.Fatalf("wide cell = %#v err=%v, want column 1 with an empty header", path.Table, err)
	}
}
