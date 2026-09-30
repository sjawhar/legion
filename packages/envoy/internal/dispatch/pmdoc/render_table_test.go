package pmdoc

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A span comes from the live tree unchecked, and each position it covers is written as a cell, so
// each bound on those cells is held by a case below that goes red when that bound alone is removed,
// its comment naming the check that does. Every case is held to the cells maxSpanCells allows a
// render to write that no one wrote: twice the budget, and one for each cell the table holds. Each
// such cell costs a render some 600 bytes, so the case over many tables, which writes all the
// budget allows, is held to a kibibyte for each as well, a backstop for a cost the count does not
// see. maxColspan is held by TestRenderWritesAColspanAsWideAsTheEditorDraws.
func TestRenderBoundsAbsurdTableSpans(t *testing.T) {
	oneCellRows := func(rows, colspan int, rowspan func(index int) int) *Node {
		spans := make([][][2]int, rows)
		for index := range spans {
			spans[index] = [][2]int{{colspan, rowspan(index)}}
		}
		return spanTable(spans...)
	}
	toTheEnd := func(rows int) func(int) int { return func(index int) int { return rows - index } }
	one := func(int) int { return 1 }
	for _, test := range []struct {
		name   string
		tables func() []*Node
		// width is the header's width in cells the case pins, or 0.
		width    int
		allocate bool
	}{
		// The rowspan clamp: without it a rowspan past the last row indexes past the grid's rows,
		// and the recovered panic fails the render.
		{name: "rowspan past the last row", tables: func() []*Node {
			return []*Node{spanTable([][2]int{{1, 1}, {1, 1}}, [][2]int{{1, 2_000_000}, {1, 1}})}
		}},
		// The budget as each colspan spends it: without that, a colspan on each of 1,000 rows adds
		// 999 cells to every row, 999,000 in all.
		{name: "a colspan on every row", tables: func() []*Node { return []*Node{oneCellRows(1000, 1000, one)} }},
		// The budget as a rowspan spends it on the rows below: without that, rowspans stacked to
		// the end of 1,000 rows cover half a million positions.
		{name: "rowspans stacked to the end", tables: func() []*Node { return []*Node{oneCellRows(1000, 1, toTheEnd(1000))} }},
		// The budget as a rowspan spends it for each position it covers in a row below, not once
		// for the row: charged once a row, a cell spanning 1,000 columns and 1,000 rows covers all
		// 999,000 positions below it, where it covers 99,001 and the table writes 100,001 empty
		// cells.
		{name: "a cell spanning 1,000 columns and rows", tables: func() []*Node {
			spans := [][][2]int{{{1000, 1000}}}
			for range 999 {
				spans = append(spans, [][2]int{{1, 1}})
			}
			return []*Node{spanTable(spans...)}
		}},
		// The budget as each gap filled up to a rowspan spends it: without that, 1,000 rows each
		// fill 998 empty cells up to the column a header cell spans all of them in.
		{name: "gaps up to a rowspan", tables: func() []*Node {
			spans := [][][2]int{{{999, 1}, {1, 1001}}}
			for range 1000 {
				spans = append(spans, [][2]int{{1, 1}})
			}
			return []*Node{spanTable(spans...)}
		}},
		// One budget for each render, not each table: with one for each table, 500 tables of a
		// one-cell header over a cell spanning 2,000,000 columns add 999 cells to every body row
		// and widen every header by 999 more, 999,000 in all. This case writes all the budget
		// allows: 100,000 cells the spans add and as many the headers are widened by.
		{name: "many tables", allocate: true, tables: func() []*Node {
			tables := make([]*Node, 500)
			for index := range tables {
				tables[index] = spanTable([][2]int{{1, 1}}, [][2]int{{2_000_000, 1}})
			}
			return tables
		}},
		// The grid's limit: under a one-cell header, 20 one-cell rows and a row of 100 cells each
		// spanning 1,000 columns, only the first span is written across 1,000 columns, so the
		// header, which every row reads back as wide as, is 1,099 cells wide. Without the limit
		// it is 100,000, and the table reads back as 2,200,000 cells.
		{name: "a row of wide spans under a one-cell header", width: 1099, tables: func() []*Node {
			spans := [][][2]int{{{1, 1}}}
			for range 20 {
				spans = append(spans, [][2]int{{1, 1}})
			}
			wide := make([][2]int, 100)
			for index := range wide {
				wide[index] = [2]int{1000, 1}
			}
			return []*Node{spanTable(append(spans, wide)...)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := &Node{Type: "doc", Children: test.tables()}
			held := 0
			Walk(doc, func(node *Node) bool {
				if node.Type == "table_cell" || node.Type == "table_header" {
					held++
				}
				return true
			})
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			markdown, err := Render(doc)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			if empty, most := emptyCells(markdown), 2*maxSpanCells+held; empty > most {
				t.Errorf("Render wrote %d empty cells, want at most %d", empty, most)
			}
			if got := headerWidth(markdown); test.width != 0 && got != test.width {
				t.Errorf("the header is written %d cells wide, want %d", got, test.width)
			}
			if allocated, most := after.TotalAlloc-before.TotalAlloc, uint64(2*maxSpanCells+held)<<10; test.allocate && allocated > most {
				t.Errorf("Render allocated %d MiB, want at most %d", allocated>>20, most>>20)
			}
		})
	}
}

// The checks that read one document-level block at a time (BlockReadError, BlockShapeError) run
// once for each block a write changed, and a suggestion accepted across many tables changes each of
// them, so a check writes a table spanless: were each of its renders to spend a budget of span
// cells, the checks over four tables of 100 one-character rows under a cell spanning 1,000 columns
// and every row would allocate some 1,500 MiB, where the document's own render spends one budget.
func TestBlockChecksSpendNoSpanBudgetForEachBlock(t *testing.T) {
	var blocks []*Node
	for range 4 {
		spans := [][][2]int{{{1000, 100}}}
		for range 99 {
			spans = append(spans, [][2]int{{1, 1}})
		}
		blocks = append(blocks, spanTable(spans...))
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, block := range blocks {
		if err := BlockReadError(block); err != nil {
			t.Fatal(err)
		}
		if err := BlockShapeError(block); err != nil && !errors.Is(err, ErrSchema) {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("the checks over %d tables allocated %d MiB, want at most 64", len(blocks), allocated>>20)
	}
}

// BlockShapeError writes a table spanless, so a table reads back in its shape exactly when every
// row holds as many cells: a header cell spanning two columns over a body cell spanning two, a cell
// in each row, does, where written with its spans each row would read back two cells wide; a
// two-cell header over one body cell spanning both does not.
func TestBlockShapeErrorReadsATableInItsShapeWhenItsRowsHoldAsManyCells(t *testing.T) {
	if err := BlockShapeError(spanTable([][2]int{{2, 1}}, [][2]int{{2, 1}})); err != nil {
		t.Errorf("a header cell over a body cell, each spanning two columns: %v, want the table's shape", err)
	}
	if err := BlockShapeError(spanTable([][2]int{{1, 1}, {1, 1}}, [][2]int{{2, 1}})); !errors.Is(err, ErrSchema) {
		t.Errorf("a two-cell header over one body cell spanning both: %v, want it read back as another shape", err)
	}
}

// spanTable is a table of rows, the first its header, each cell holding `x` and spanning the
// columns and rows its pair names.
func spanTable(rows ...[][2]int) *Node {
	table := &Node{Type: "table"}
	for index, spans := range rows {
		kind, rowType := "table_cell", "table_row"
		if index == 0 {
			kind, rowType = "table_header", "table_header_row"
		}
		row := &Node{Type: rowType}
		for _, span := range spans {
			row.Children = append(row.Children, spanCell(kind, span[0], span[1]))
		}
		table.Children = append(table.Children, row)
	}
	return table
}

// spanCell is a table cell of kind holding `x`, spanning colspan columns and rowspan rows.
func spanCell(kind string, colspan, rowspan int) *Node {
	return &Node{
		Type:     kind,
		Attrs:    Attrs{"alignment": nil, "colspan": colspan, "colwidth": nil, "rowspan": rowspan},
		Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "x"}}}},
	}
}

// emptyCells counts the cells markdown's table rows hold nothing in.
func emptyCells(markdown string) int {
	empty := 0
	for _, line := range strings.Split(markdown, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		for _, cell := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ") {
			if cell == "" {
				empty++
			}
		}
	}
	return empty
}

// headerWidth is how many cells markdown's first line, a table's header row, is written with.
func headerWidth(markdown string) int {
	first, _, _ := strings.Cut(markdown, "\n")
	return strings.Count(first, "|") - 1
}

// The browser editor draws a colspan past 1000 as 1000, as HTML caps it, so a cell claiming more
// is written across 1000 columns, even in a table whose widest row would let the grid reach past.
func TestRenderWritesAColspanAsWideAsTheEditorDraws(t *testing.T) {
	header := &Node{Type: "table_header_row"}
	for range 1500 {
		header.Children = append(header.Children, spanCell("table_header", 1, 1))
	}
	body := &Node{Type: "table_row", Children: []*Node{spanCell("table_cell", 1500, 1)}}
	markdown, err := Render(&Node{Type: "doc", Children: []*Node{{Type: "table", Children: []*Node{header, body}}}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(markdown, "\n"), "\n")
	if cells := strings.Count(lines[len(lines)-1], "|") - 1; cells != maxColspan {
		t.Fatalf("the body row is written %d cells wide, want %d", cells, maxColspan)
	}
}

// A span reaches the renderer as the live Yjs tree holds it, where a peer's update decodes an
// integer as an int64, so a colspan read from one is written across the columns it spans.
func TestRenderWritesASpanAsAPeersUpdateCarriesIt(t *testing.T) {
	source := crdt.New()
	fragment := source.GetXmlFragment("prosemirror")
	tree := &Node{Type: "doc", Children: []*Node{spanTable([][2]int{{1, 1}, {1, 1}}, [][2]int{{2, 1}})}}
	if err := source.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, fragment, tree)
	}); err != nil {
		t.Fatal(err)
	}
	peer := crdt.New()
	if err := crdt.ApplyUpdateV1(peer, crdt.EncodeStateAsUpdateV1(source, nil), nil); err != nil {
		t.Fatal(err)
	}
	read, err := Read(peer.GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatal(err)
	}
	if colspan := read.Children[0].Children[1].Children[0].Attrs["colspan"]; colspan != int64(2) {
		t.Fatalf("the update carries colspan %#v, want int64(2), the type this case is for", colspan)
	}
	markdown, err := Render(read)
	if err != nil {
		t.Fatal(err)
	}
	if want := "| x | x |\n| --- | --- |\n| x |  |\n"; markdown != want {
		t.Fatalf("Render() = %q, want %q", markdown, want)
	}
}

// The table-cell-pipes fixture's tree is what the browser editor's parser reads its markdown as, so
// a renderer that writes the tree as that markdown writes cells the browser editor reads back as
// they were: a `|` in a cell's text, code, link or image, and one after a backslash. A backslash
// before a pipe ends the cell in that parser unless it is one of an odd run, so an image's alt
// written `one\\|two`, which Parse reads back as `one\|two`, is two cells there.
func TestRenderWritesTableCellPipesAsTheBrowserEditorReadsThem(t *testing.T) {
	fx := fixtureNamed(t, "table-cell-pipes")
	tree, err := FromJSON(fx.PMJSON)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != fx.Markdown {
		t.Fatalf("Render() = %q, want the markdown the browser editor read the tree from, %q", markdown, fx.Markdown)
	}
}
