package pmdoc

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
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

// A pasted HTML table can hold cells spanning columns or rows. The header is the one drawn above
// the column the cell is written in, while column stays the cell's child index, which
// delete_column takes.
func TestBlockPathOfNamesTheHeaderAboveTheColumnASpannedCellIsDrawnIn(t *testing.T) {
	cell := func(kind, value string, colspan, rowspan int) *Node {
		return &Node{Type: kind, Attrs: Attrs{"colspan": colspan, "rowspan": rowspan}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}}}
	}
	for _, test := range []struct {
		name   string
		rows   []*Node
		row    int
		column int
		header string
	}{
		{
			// | Owner |  | Due | over | Sami | Lucas | Today |
			name: "a header cell spanning two columns",
			rows: []*Node{
				{Type: "table_header_row", Children: []*Node{cell("table_header", "Owner", 2, 1), cell("table_header", "Due", 1, 1)}},
				{Type: "table_row", Children: []*Node{cell("table_cell", "Sami", 1, 1), cell("table_cell", "Lucas", 1, 1), cell("table_cell", "Today", 1, 1)}},
			},
			row: 1, column: 1, header: "Owner",
		},
		{
			// | Line | Owner | Due | over | Backfill | Sami | Oct 1 | and |  | Lucas | Oct 2 |
			name: "a body cell spanning two rows",
			rows: []*Node{
				{Type: "table_header_row", Children: []*Node{cell("table_header", "Line", 1, 1), cell("table_header", "Owner", 1, 1), cell("table_header", "Due", 1, 1)}},
				{Type: "table_row", Children: []*Node{cell("table_cell", "Backfill", 1, 2), cell("table_cell", "Sami", 1, 1), cell("table_cell", "Oct 1", 1, 1)}},
				{Type: "table_row", Children: []*Node{cell("table_cell", "Lucas", 1, 1), cell("table_cell", "Oct 2", 1, 1)}},
			},
			row: 2, column: 0, header: "Owner",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: []*Node{{Type: "table", Children: test.rows}}}
			EnsureBlockIDs(doc)
			target := doc.Children[0].Children[test.row].Children[test.column].Children[0].Attrs[BlockIDAttr].(string)
			path, err := BlockPathOf(doc, target)
			if err != nil {
				t.Fatal(err)
			}
			if got := path.Table; *got.Row != test.row || *got.Column != test.column || *got.Header != test.header {
				t.Fatalf("spanned cell = row %d, column %d headed %q, want row %d, column %d headed %q", *got.Row, *got.Column, *got.Header, test.row, test.column, test.header)
			}
		})
	}
}

// The header is the one the renderer draws above the cell, with the table laid out whole: a header
// cell spanning 1,000 columns, beside a body row of 1,200 cells, covers the 1,000 columns after the
// first, since the widest row raises the grid's width limit past 1,000. Laid out alone, the header
// row stopped one column short of that, and its last column read as having no header.
func TestBlockPathOfNamesTheHeaderTheRendererDrawsOverAWideTable(t *testing.T) {
	cell := func(kind, value string, colspan int) *Node {
		return &Node{Type: kind, Attrs: Attrs{"colspan": colspan, "rowspan": 1}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}}}
	}
	body := make([]*Node, 1_200)
	for index := range body {
		body[index] = cell("table_cell", fmt.Sprint(index), 1)
	}
	table := &Node{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", "Key", 1), cell("table_header", "Span", 1_000)}},
		{Type: "table_row", Children: body},
	}}
	doc := &Node{Type: "doc", Children: []*Node{table}}
	EnsureBlockIDs(doc)
	grid, _ := (&renderer{spanBudget: maxSpanCells}).tableGrid(table)
	if grid[0][1] != table.Children[0].Children[1] || len(grid[0]) < 1_001 || grid[1][1_000] != body[1_000] {
		t.Fatalf("the renderer writes the header %d cells wide, %q at column 1, want Span at column 1 spanning past column 1,000", len(grid[0]), oneLineText(grid[0][1]))
	}
	if control := prefixColumnHeader(table, 1, 1_000); control != "" {
		t.Fatalf("control: the header row laid out alone heads column 1,000 %q, want none, so the layout decides this test", control)
	}
	for column, want := range map[int]string{0: "Key", 1: "Span", 1_000: "Span", 1_001: ""} {
		id := body[column].Attrs[BlockIDAttr].(string)
		path, err := BlockPathOf(doc, id)
		if err != nil {
			t.Fatal(err)
		}
		if *path.Table.Header != want {
			t.Errorf("cell %d headed %q, want %q", column, *path.Table.Header, want)
		}
	}
}

// BlockPathsAndTableRows places every block in one walk exactly as BlockPathOf places it alone:
// over the placement documents (placementDocuments). An id no block carries is in neither.
func TestBlockPathsPlacesEveryBlockAsBlockPathOfDoes(t *testing.T) {
	for name, doc := range placementDocuments(t) {
		t.Run(name, func(t *testing.T) {
			paths, _, err := BlockPathsAndTableRows(doc)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			Walk(doc, func(node *Node) bool {
				if id, ok := node.Attrs[BlockIDAttr].(string); ok && node.Type != "doc" && !isInlineNodeType(node.Type) {
					ids[id] = true
				}
				return true
			})
			if len(ids) == 0 {
				t.Fatal("the document carries no block id")
			}
			for id := range ids {
				want, err := BlockPathOf(doc, id)
				if err != nil {
					t.Fatalf("BlockPathOf(%q): %v", id, err)
				}
				if got, ok := paths[id]; !ok || !reflect.DeepEqual(got, want) {
					t.Fatalf("paths[%q] = %#v (present %t), want BlockPathOf's %#v", id, got, ok, want)
				}
			}
			for id := range paths {
				if !ids[id] {
					t.Fatalf("the paths hold %q, which no block carries", id)
				}
			}
			if _, err := BlockPathOf(doc, "missing"); !errors.Is(err, ErrTargetNotFound) {
				t.Fatalf("BlockPathOf of an id no block carries: %v, want ErrTargetNotFound", err)
			}
		})
	}
}

// placementDocuments are the documents the placement tests read: the pmdoc fixtures, a list whose
// ancestors carry no block id, an id two blocks carry (the first in document order keeps it, as
// BlockPathOf finds it), tables with column and row spans, and a body row of more than 1,000 cells.
func placementDocuments(t *testing.T) map[string]*Node {
	t.Helper()
	documents := map[string]*Node{}
	for _, fx := range loadFixtures(t) {
		doc, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("fixture %s: %v", fx.Name, err)
		}
		documents["fixture "+fx.Name] = doc
	}
	text := func(value string) []*Node { return []*Node{{Type: "text", Text: value}} }
	paragraph := func(id, value string) *Node {
		return &Node{Type: "paragraph", Attrs: Attrs{BlockIDAttr: id}, Children: text(value)}
	}
	documents["unstamped ancestors"] = &Node{Type: "doc", Children: []*Node{{Type: "bullet_list", Children: []*Node{
		{Type: "list_item", Children: []*Node{paragraph("item", "item"), {Type: "blockquote", Children: []*Node{paragraph("quoted", "quoted")}}}},
	}}}}
	documents["a repeated id"] = &Node{Type: "doc", Children: []*Node{
		paragraph("same", "first"),
		{Type: "blockquote", Attrs: Attrs{BlockIDAttr: "quote"}, Children: []*Node{paragraph("same", "second")}},
		paragraph("other", "third"),
	}}
	cell := func(kind, value string, colspan, rowspan int) *Node {
		return &Node{Type: kind, Attrs: Attrs{"colspan": colspan, "rowspan": rowspan}, Children: []*Node{{Type: "paragraph", Children: text(value)}}}
	}
	spans := &Node{Type: "doc", Children: []*Node{{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", "Owner", 2, 1), cell("table_header", "Due", 1, 1), cell("table_header", "Notes on what each owner still owes this week", 3, 1)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "Backfill", 1, 3), cell("table_cell", "Sami", 2, 1), cell("table_cell", "Oct 1", 1, 2), cell("table_cell", "x", 1, 1)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "Lucas", 1, 1), cell("table_cell", "Oct 2", 1, 1)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "Wide", 4, 1), cell("table_cell", "past", 1, 1), cell("table_cell", "more", 1, 1)}},
		{Type: "table_row", Children: nil},
		{Type: "table_row", Children: []*Node{cell("table_cell", "last", 1, 9)}},
	}}}}
	EnsureBlockIDs(spans)
	documents["column and row spans"] = spans
	wideRow := make([]*Node, 1_200)
	for index := range wideRow {
		wideRow[index] = cell("table_cell", fmt.Sprint(index), 1, 1)
	}
	wide := &Node{Type: "doc", Children: []*Node{{Type: "blockquote", Children: []*Node{{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", "Spans", 1_000, 1), cell("table_header", "Tail", 1, 1)}},
		{Type: "table_row", Children: wideRow},
		{Type: "table_row", Children: []*Node{cell("table_cell", "short", 1, 1)}},
	}}}}}}
	EnsureBlockIDs(wide)
	documents["a row of more than 1,000 cells"] = wide
	return documents
}

// TableRows lists every row and cell of every table carrying a block id, each stamped one placed
// exactly as BlockPathOf places it alone: a row's index is its Row, and a cell's row and column are
// its Row and Column (the child indexes delete_row and delete_column take, a spanning cell counted
// once) and its header the opening words of its Header, over the placement documents, the tables
// with column and row spans among them, one of whose headers runs past the opening words. Every row
// and cell is listed in child order with its opening words, one the live document has not stamped
// yet with no id, since its indexes still address it.
func TestTableRowsPlacesEveryRowAndCellAsBlockPathOfDoes(t *testing.T) {
	tables := 0
	for name, doc := range placementDocuments(t) {
		listed, err := TableRows(doc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The cached read takes the rows from the walk that places blocks, which must list what
		// TableRows lists alone.
		_, rows, err := BlockPathsAndTableRows(doc)
		if err != nil || !reflect.DeepEqual(rows, listed) {
			t.Fatalf("%s: BlockPathsAndTableRows lists other rows than TableRows (err=%v)", name, err)
		}
		Walk(doc, func(node *Node) bool {
			tableID, _ := node.Attrs[BlockIDAttr].(string)
			if node.Type != "table" || tableID == "" {
				return true
			}
			tables++
			want := make([]TableRow, len(node.Children))
			for rowIndex, row := range node.Children {
				rowID, _ := row.Attrs[BlockIDAttr].(string)
				if rowID != "" {
					path, err := BlockPathOf(doc, rowID)
					if err != nil || *path.Table.Row != rowIndex {
						t.Fatalf("%s: BlockPathOf(row %q) = %#v err=%v, want row %d", name, rowID, path.Table, err, rowIndex)
					}
				}
				want[rowIndex] = TableRow{ID: rowID, Index: rowIndex, Cells: make([]TableCell, len(row.Children))}
				for column, cell := range row.Children {
					cellID, _ := cell.Attrs[BlockIDAttr].(string)
					entry := TableCell{ID: cellID, Row: rowIndex, Column: column, Text: openingWords(oneLineText(cell))}
					if cellID != "" {
						place, err := BlockPathOf(doc, cellID)
						if err != nil {
							t.Fatalf("%s: BlockPathOf(cell %q): %v", name, cellID, err)
						}
						entry.Row, entry.Column, entry.Header = *place.Table.Row, *place.Table.Column, openingWords(*place.Table.Header)
					} else {
						// BlockPathOf's own placement (tablePosition) of a cell no id reaches.
						entry.Header = openingWords(*tablePosition(doc, node, tableID, []int{rowIndex, column}).Header)
					}
					want[rowIndex].Cells[column] = entry
				}
			}
			if got := listed[tableID]; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: TableRows[%q] = %#v, want BlockPathOf's %#v", name, tableID, got, want)
			}
			return true
		})
		for id := range listed {
			if path, err := BlockPathOf(doc, id); err != nil || path.Type != "table" {
				t.Fatalf("%s: TableRows lists %q, which no table carries (%v)", name, id, err)
			}
		}
	}
	if tables < 3 {
		t.Fatalf("the placement documents hold %d tables with a block id, want the spanned and the wide ones at least", tables)
	}
}

// A cell is counted once in its row however many columns it spans, so its listed column is its
// child index, the index delete_column takes: deleting it removes that cell and no other. Negative
// control: the markdown the table is written as places that cell at another column, so a column
// read from the markdown would delete a different one.
func TestTableRowsColumnIsTheIndexDeleteTableColumnTakes(t *testing.T) {
	cell := func(kind, value string, colspan, rowspan int) *Node {
		return &Node{Type: kind, Attrs: Attrs{"colspan": colspan, "rowspan": rowspan}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}}}
	}
	doc := &Node{Type: "doc", Children: []*Node{{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", "Owner", 2, 1), cell("table_header", "Due", 1, 1), cell("table_header", "Notes", 1, 1)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "Sami", 1, 1), cell("table_cell", "Oct 1", 1, 1), cell("table_cell", "late", 1, 1)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", "Lucas", 1, 1), cell("table_cell", "Oct 2", 1, 1), cell("table_cell", "soon", 1, 1)}},
	}}}}
	EnsureBlockIDs(doc)
	table := doc.Children[0]
	tableID := table.Attrs[BlockIDAttr].(string)
	listed, err := TableRows(doc)
	if err != nil {
		t.Fatal(err)
	}
	var due TableCell
	for _, row := range listed[tableID] {
		for _, listedCell := range row.Cells {
			if listedCell.Text == "Due" {
				due = listedCell
			}
		}
	}
	if due.ID == "" || due.Row != 0 || due.Column != 1 || due.Header != "Due" {
		t.Fatalf("Due = %#v, want row 0, column 1 headed Due", due)
	}
	grid, _ := (&renderer{spanBudget: maxSpanCells}).tableGrid(table)
	if written := slices.Index(grid[0], table.Children[0].Children[1]); written == due.Column {
		t.Fatalf("control: the markdown writes Due at column %d too, so this table cannot tell the two indexes apart", written)
	}
	deleted, err := DeleteTableColumn(doc, tableID, due.Column)
	if err != nil {
		t.Fatal(err)
	}
	if header := cellOpenings(deleted.Children[0].Children[0]); !reflect.DeepEqual(header, []string{"Owner", "Notes"}) {
		t.Fatalf("after delete_column %d the header row is %q, want Owner · Notes: the Due cell alone removed", due.Column, header)
	}
}

// The document's tables share one budget of the empty cells spans add (maxSpanCells), spent in
// document order as the renderer spends it, so a cell is listed and placed under the header GET
// /text draws above it. Two identical spanned tables stand either side of one spending the rest of
// the budget: the renderer draws the first with Owner's second column and the last without it, and
// the last's Oct 1 is under Due, where on a budget of its own it sat under Owner. A table the
// listing skips - one with no block id, or a second table under an earlier one's id - still spends
// what the renderer spends on it.
func TestTablesShareOneSpanBudgetInDocumentOrder(t *testing.T) {
	cell := func(kind, value string, colspan, rowspan int) *Node {
		return &Node{Type: kind, Attrs: Attrs{"colspan": colspan, "rowspan": rowspan}, Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}}}
	}
	owners := func() *Node {
		return &Node{Type: "table", Children: []*Node{
			{Type: "table_header_row", Children: []*Node{cell("table_header", "Owner", 2, 1), cell("table_header", "Due", 1, 1), cell("table_header", "Notes", 1, 1)}},
			{Type: "table_row", Children: []*Node{cell("table_cell", "Sami", 1, 1), cell("table_cell", "Oct 1", 1, 1), cell("table_cell", "late", 1, 1)}},
		}}
	}
	// spending covers 1,000 columns of each of its 100 body rows, past what the budget holds.
	spending := func() *Node {
		table := &Node{Type: "table", Children: []*Node{{Type: "table_header_row", Children: []*Node{cell("table_header", "Wide", 1_000, 101)}}}}
		for row := range 100 {
			table.Children = append(table.Children, &Node{Type: "table_row", Children: []*Node{cell("table_cell", fmt.Sprint(row), 1, 1)}})
		}
		return table
	}
	for _, test := range []struct {
		name string
		// unlisted marks the spending table the listing skips: its id removed, or set to before's.
		unlisted func(before, spent *Node)
	}{
		{name: "every table listed"},
		{name: "the spending table has no block id", unlisted: func(_, spent *Node) { delete(spent.Attrs, BlockIDAttr) }},
		{name: "the spending table repeats an earlier table's id", unlisted: func(before, spent *Node) { spent.Attrs[BlockIDAttr] = before.Attrs[BlockIDAttr] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, spent, after := owners(), spending(), owners()
			doc := &Node{Type: "doc", Children: []*Node{before, spent, after}}
			EnsureBlockIDs(doc)
			if test.unlisted != nil {
				test.unlisted(before, spent)
			}
			markdown, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			spanned, wide, unspanned := strings.Index(markdown, "| Owner |  | Due | Notes |"), strings.Index(markdown, "| Wide |"), strings.Index(markdown, "| Owner | Due | Notes |")
			if spanned < 0 || wide < spanned || unspanned < wide {
				t.Fatalf("the renderer draws the tables at %d, %d and %d of %.300q, want the first with Owner's second column, then the spending table, then the last without it", spanned, wide, unspanned, markdown)
			}
			listed, err := TableRows(doc)
			if err != nil {
				t.Fatal(err)
			}
			paths, walked, err := BlockPathsAndTableRows(doc)
			if err != nil || !reflect.DeepEqual(walked, listed) {
				t.Fatalf("BlockPathsAndTableRows lists other rows than TableRows (err=%v)", err)
			}
			for _, check := range []struct {
				name   string
				table  *Node
				header string
			}{{"the table before the budget is spent", before, "Owner"}, {"the table after it", after, "Due"}} {
				oct1 := check.table.Children[1].Children[1]
				id := oct1.Attrs[BlockIDAttr].(string)
				path, err := BlockPathOf(doc, id)
				if err != nil {
					t.Fatal(err)
				}
				if *path.Table.Header != check.header || *paths[id].Table.Header != check.header {
					t.Errorf("%s: Oct 1 placed under %q, and under %q by the walk, want %q as the renderer draws it", check.name, *path.Table.Header, *paths[id].Table.Header, check.header)
				}
				if rows := listed[check.table.Attrs[BlockIDAttr].(string)]; len(rows) != 2 || rows[1].Cells[1].Header != check.header {
					t.Errorf("%s: Oct 1 listed as %#v, want under %q", check.name, rows, check.header)
				}
			}
		})
	}
}

// Laying a table out whole, as the renderer writes it, leaves the column header of every cell of
// the pmdoc fixtures where laying it out through the anchored row alone put it: the grid of the
// rows above a cell and the header row alone (prefixColumnHeader, the layout BlockPathOf used).
func TestBlockPathOfHeadersOfTheFixturesAreThoseOfTheRowsAboveThem(t *testing.T) {
	checked := 0
	for _, fx := range loadFixtures(t) {
		doc, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("fixture %s: %v", fx.Name, err)
		}
		Walk(doc, func(table *Node) bool {
			if table.Type != "table" {
				return true
			}
			for row, rowNode := range table.Children {
				for column, cellNode := range rowNode.Children {
					id, _ := cellNode.Attrs[BlockIDAttr].(string)
					path, err := BlockPathOf(doc, id)
					if err != nil {
						t.Fatalf("fixture %s: BlockPathOf(%q): %v", fx.Name, id, err)
					}
					if want := prefixColumnHeader(table, row, column); *path.Table.Header != want {
						t.Fatalf("fixture %s row %d column %d: header %q, want %q", fx.Name, row, column, *path.Table.Header, want)
					}
					checked++
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no fixture holds a table cell")
	}
}

// prefixColumnHeader is the header BlockPathOf named before it laid tables out whole: the grid of
// the rows through the cell's, and the header row's grid laid out alone.
func prefixColumnHeader(table *Node, row, column int) string {
	rows, _ := (&renderer{spanBudget: maxSpanCells}).tableGrid(&Node{Type: table.Type, Children: table.Children[:row+1]})
	at := slices.Index(rows[row], table.Children[row].Children[column])
	headers := table.Children[0].Children
	header, _ := (&renderer{spanBudget: maxSpanCells}).tableGrid(&Node{Type: table.Type, Children: table.Children[:1]})
	grid := header[0]
	if at >= len(grid) {
		return ""
	}
	var covering *Node
	next := 0
	for _, cell := range grid[:at+1] {
		if next < len(headers) && cell == headers[next] {
			covering, next = cell, next+1
		}
	}
	return oneLineText(covering)
}
