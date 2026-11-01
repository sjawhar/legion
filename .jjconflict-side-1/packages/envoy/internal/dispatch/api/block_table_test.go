package api

import (
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// notesHeader is the spanned table's third header, longer than the opening words (40 characters) a
// listing names a cell by, and notesOpening those opening words, so a check that compares a listed
// header with the block route's whole header text fails.
const (
	notesHeader  = "Notes on what each owner still owes this week"
	notesOpening = "Notes on what each owner still owes this…"
)

// requestMemoryBound is the most one request may hold, as cmd/dispatch/memory_test.go's
// requestMemoryBound states it for a running server.
const requestMemoryBound = 256 << 20

// A table's entry in GET /blocks lists its rows and cells, each placed exactly where GET
// /blocks/{block_id} places that row or cell from the same live tree: a plain table written as
// markdown, and one holding cells that span columns and rows, as a pasted HTML table does, where a
// cell's column is its index in its row (the index delete_column takes) rather than the column the
// markdown writes it in. The table stays one entry: the list holds the paragraph and the two tables
// alone, and the issue route lists the same rows.
func TestBlocksListATablesRowsAndCellsWhereTheBlockRoutePlacesThem(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table rows",
		"Intro.\n\n| # | Line | Due |\n| --- | --- | --- |\n| 1 | Backfill | Oct 1 |\n| 2 | Red-team loop | Today |\n")
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, spannedCellsTable())
		return nil
	})
	peer.barrier(t)

	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if len(blocks) != 3 || blocks[0].Type != "paragraph" || blocks[1].Type != "table" || blocks[2].Type != "table" {
		t.Fatalf("GET /blocks = %#v, want the paragraph and the two tables alone", blocks)
	}
	if blocks[0].Table != nil {
		t.Fatalf("the paragraph carries table rows: %#v", blocks[0].Table)
	}
	plain, spanned := blocks[1], blocks[2]
	requirePlacedAsTheBlockRoute(t, handler, issue.PrimaryArtifactID, plain, [][]string{
		{"#", "Line", "Due"}, {"1", "Backfill", "Oct 1"}, {"2", "Red-team loop", "Today"},
	})
	requirePlacedAsTheBlockRoute(t, handler, issue.PrimaryArtifactID, spanned, [][]string{
		{"Owner", "Due", notesOpening}, {"Sami", "Oct 1", "late"}, {"Oct 2", "soon"},
	})
	// The spanned table's Due header is the second cell of its row, so delete_column takes 1 for it,
	// where the markdown writes it third, after the two columns Owner spans.
	if due := spanned.Table.Rows[0].Cells[1]; due.Text != "Due" || due.Column != 1 || due.Header != "Due" {
		t.Fatalf("spanned Due = %#v, want column 1 headed Due", due)
	}
	// The cell spanning two rows heads its column for the row below, which holds one cell fewer.
	if below := spanned.Table.Rows[2].Cells[0]; below.Text != "Oct 2" || below.Column != 0 || below.Header != "Owner" {
		t.Fatalf("the cell under a rowspan = %#v, want column 0 drawn under Owner", below)
	}

	byIssue := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/spec/blocks", nil, "alice"))
	if len(byIssue) != 3 || byIssue[2].Table == nil || len(byIssue[2].Table.Rows) != 3 || byIssue[2].Table.Rows[1].Cells[2].ID != spanned.Table.Rows[1].Cells[2].ID {
		t.Fatalf("the issue route's blocks = %#v, want the artifact route's rows", byIssue)
	}
}

// GET /blocks of a document a room holds lays its tables out on the one budget of the empty cells
// spans add that a render of the document spends, in document order (pmdoc.TableRows), never a
// budget per table: a browser can paste tables whose spans each fill a budget, and a listing that
// gave each its own laid six of them out at several times the bound one request may hold. The
// document holds the small spanned table, six tables whose header cell spans 1,000 columns and the
// 100 rows below it, and the small table again. One GET /blocks allocates at most that bound and
// lists every table with its rows; both small tables are placed where GET /blocks/{block_id}
// places them; the second, past the budget, is listed under the headers GET /text draws it with,
// Owner spanning nothing, so its Oct 1 is under Due where the first one's is under Owner; and
// delete_row on one of its listed rows removes that row.
func TestBlocksOfAHeldDocumentShareOneSpanBudget(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Spanned tables", "Intro.\n")
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, spannedCellsTable())
		for range 6 {
			tree.Children = append(tree.Children, budgetSpendingTable())
		}
		tree.Children = append(tree.Children, spannedCellsTable())
		return nil
	})
	peer.barrier(t)

	blocksPath := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/blocks"
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	listed := dispatchRequest(t, handler, http.MethodGet, blocksPath, nil, "alice")
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("one GET /blocks of the held document allocated %d MiB", allocated>>20)
	if allocated > requestMemoryBound {
		t.Fatalf("one GET /blocks of the held document allocated %d MiB, want at most %d MiB", allocated>>20, requestMemoryBound>>20)
	}
	tables := tablesOf(decodeBody[[]model.ArtifactBlock](t, listed))
	if len(tables) != 8 {
		t.Fatalf("GET /blocks lists %d tables, want 8", len(tables))
	}
	for _, spending := range tables[1:7] {
		if spending.Table == nil || len(spending.Table.Rows) != 101 {
			t.Fatalf("a budget-spending table is listed as %#v, want its 101 rows", spending.Table)
		}
	}
	covered, past := tables[0], tables[7]
	texts := [][]string{{"Owner", "Due", notesOpening}, {"Sami", "Oct 1", "late"}, {"Oct 2", "soon"}}
	requirePlacedAsTheBlockRoute(t, handler, issue.PrimaryArtifactID, covered, texts)
	requirePlacedAsTheBlockRoute(t, handler, issue.PrimaryArtifactID, past, texts)
	if oct1 := covered.Table.Rows[1].Cells[1]; oct1.Header != "Owner" {
		t.Fatalf("the covered table's Oct 1 is listed under %q, want Owner, whose span covers its column", oct1.Header)
	}
	if oct1 := past.Table.Rows[1].Cells[1]; oct1.Header != "Due" {
		t.Fatalf("the table past the budget lists Oct 1 under %q, want Due, as GET /text draws it", oct1.Header)
	}
	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	spannedLine, wideLine, unspannedLine := strings.Index(markdown, "| Owner |  | Due | "+notesHeader+" |"), strings.Index(markdown, "| Wide |"), strings.LastIndex(markdown, "| Owner | Due | "+notesHeader+" |")
	if spannedLine < 0 || wideLine < spannedLine || unspannedLine < wideLine {
		t.Fatalf("GET /text draws the small tables at %d and %d around the wide ones at %d, want the first with Owner's span and the last without it", spannedLine, unspannedLine, wideLine)
	}

	sami := past.Table.Rows[1]
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_row", "block": past.ID, "index": sami.Index}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("delete_row %d: status=%d body=%s", sami.Index, edited.Code, edited.Body.String())
	}
	for _, table := range tablesOf(decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, blocksPath, nil, "alice"))) {
		switch table.ID {
		case past.ID:
			if got, want := cellTexts(table.Table), [][]string{{"Owner", "Due", notesOpening}, {"Oct 2", "soon"}}; !equalRows(got, want) {
				t.Fatalf("after delete_row %d the table past the budget holds %q, want %q", sami.Index, got, want)
			}
		case covered.ID:
			if got := cellTexts(table.Table); !equalRows(got, texts) {
				t.Fatalf("delete_row on the last table changed the first to %q", got)
			}
		}
	}
}

// A column GET /blocks lists for a cell is the one delete_column removes it with: delete_column
// removes the cell at that index from every row, so each row loses exactly the cell the listing
// names at that column - Due in the header, and under the rowspan the second cell of the shorter
// row, which the listing shows is soon. A column taken from the markdown (2, after Owner's two
// columns) would remove Notes from the header instead.
func TestDeleteColumnWithAListedColumnRemovesThatCell(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Listed column", "Intro.\n")
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, spannedCellsTable())
		return nil
	})
	peer.barrier(t)

	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	table := tableBlockOf(t, blocks)
	if table.Table == nil {
		t.Fatalf("the table's entry lists no rows: %#v", table)
	}
	due := table.Table.Rows[0].Cells[1]
	if due.Text != "Due" {
		t.Fatalf("header cell 1 = %#v, want Due", due)
	}
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": table.ID, "index": due.Column}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("delete_column %d: status=%d body=%s", due.Column, edited.Code, edited.Body.String())
	}
	after := tableBlockOf(t, decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")))
	if after.ID != table.ID {
		t.Fatalf("table id after delete_column = %q, want %q", after.ID, table.ID)
	}
	var want [][]string
	for _, row := range table.Table.Rows {
		var kept []string
		for _, cell := range row.Cells {
			if cell.Column != due.Column {
				kept = append(kept, cell.Text)
			}
		}
		want = append(want, kept)
	}
	if literal := [][]string{{"Owner", notesOpening}, {"Sami", "late"}, {"Oct 2"}}; !equalRows(want, literal) {
		t.Fatalf("the listing without column %d = %q, want %q", due.Column, want, literal)
	}
	if got := cellTexts(after.Table); !equalRows(got, want) {
		t.Fatalf("rows after delete_column %d = %q, want %q: each row without the cell listed at that column", due.Column, got, want)
	}
}

// spannedCellsTable is a table a pasted HTML table makes, its blocks stamped with ids as the
// browser editor stamps them: a header cell spanning two columns and a body cell spanning two rows,
// so the row below it holds one cell fewer, and a header longer than a cell's opening words.
func spannedCellsTable() *pmdoc.Node {
	table := &pmdoc.Node{Type: "table", Children: []*pmdoc.Node{
		{Type: "table_header_row", Children: []*pmdoc.Node{pastedCell("table_header", "Owner", 2, 1), pastedCell("table_header", "Due", 1, 1), pastedCell("table_header", notesHeader, 1, 1)}},
		{Type: "table_row", Children: []*pmdoc.Node{pastedCell("table_cell", "Sami", 1, 2), pastedCell("table_cell", "Oct 1", 1, 1), pastedCell("table_cell", "late", 1, 1)}},
		{Type: "table_row", Children: []*pmdoc.Node{pastedCell("table_cell", "Oct 2", 1, 1), pastedCell("table_cell", "soon", 1, 1)}},
	}}
	pmdoc.EnsureBlockIDs(table)
	return table
}

// budgetSpendingTable is a table a pasted HTML table can make whose spans alone fill a document's
// whole budget of span cells (100,000): a header cell spanning 1,000 columns and the 100 rows below
// it, each holding one cell of its own.
func budgetSpendingTable() *pmdoc.Node {
	table := &pmdoc.Node{Type: "table", Children: []*pmdoc.Node{{Type: "table_header_row", Children: []*pmdoc.Node{pastedCell("table_header", "Wide", 1_000, 101)}}}}
	for row := range 100 {
		table.Children = append(table.Children, &pmdoc.Node{Type: "table_row", Children: []*pmdoc.Node{pastedCell("table_cell", strconv.Itoa(row), 1, 1)}})
	}
	pmdoc.EnsureBlockIDs(table)
	return table
}

// pastedCell is a cell of kind holding value and spanning colspan columns and rowspan rows, with
// the attributes the browser editor gives a cell.
func pastedCell(kind, value string, colspan, rowspan int) *pmdoc.Node {
	return &pmdoc.Node{
		Type:     kind,
		Attrs:    pmdoc.Attrs{"alignment": nil, "colspan": colspan, "colwidth": nil, "rowspan": rowspan},
		Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: value}}}},
	}
}

// requirePlacedAsTheBlockRoute fails t unless table's entry lists rows holding texts, every row and
// cell with a block id the block route places at the index, row and column listed, and under the
// header listed. The listing names a header by the header row's opening words (pmdoc.TableCell),
// the block route by its whole text, so a cell's listed header must be the listed text of the
// header cell whose whole text the block route names; a header cell is drawn above its own column,
// so the block route names each header cell's own whole text for it.
func requirePlacedAsTheBlockRoute(t *testing.T, handler http.Handler, artifactID string, table model.ArtifactBlock, texts [][]string) {
	t.Helper()
	if table.Table == nil {
		t.Fatalf("table %s lists no rows", table.ID)
	}
	if got := cellTexts(table.Table); !equalRows(got, texts) {
		t.Fatalf("table %s rows = %q, want %q", table.ID, got, texts)
	}
	place := func(id string) model.BlockPath {
		t.Helper()
		if id == "" {
			t.Fatalf("table %s lists a row or cell with no block id: %#v", table.ID, table.Table)
		}
		routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks/"+id, nil, "alice")
		if routed.Code != http.StatusOK {
			t.Fatalf("GET /blocks/%s: status=%d body=%s", id, routed.Code, routed.Body.String())
		}
		path := decodeBody[model.BlockPath](t, routed)
		if path.Table == nil || path.Table.ID != table.ID || path.Table.Row == nil {
			t.Fatalf("GET /blocks/%s = %#v, want a place in table %s", id, path, table.ID)
		}
		return path
	}
	headers := map[string]string{"": ""}
	for _, header := range table.Table.Rows[0].Cells {
		path := place(header.ID)
		if path.Table.Header == nil {
			t.Fatalf("GET /blocks/%s names no header for a header cell", header.ID)
		}
		headers[*path.Table.Header] = header.Text
	}
	for _, row := range table.Table.Rows {
		if path := place(row.ID); path.Type != "table_row" && path.Type != "table_header_row" || *path.Table.Row != row.Index || path.Table.Column != nil {
			t.Fatalf("row %s listed at index %d, the block route places it %#v", row.ID, row.Index, path.Table)
		}
		for _, cell := range row.Cells {
			path := place(cell.ID)
			if *path.Table.Row != cell.Row || cell.Row != row.Index || path.Table.Column == nil || *path.Table.Column != cell.Column || path.Table.Header == nil {
				t.Fatalf("cell %q listed at row %d, column %d; the block route places it %#v", cell.Text, cell.Row, cell.Column, path.Table)
			}
			if want, named := headers[*path.Table.Header]; !named || cell.Header != want {
				t.Fatalf("cell %q listed under %q; the block route draws it under %q, which the header row lists as %q", cell.Text, cell.Header, *path.Table.Header, want)
			}
		}
	}
}

// tablesOf is the table blocks of blocks, in document order.
func tablesOf(blocks []model.ArtifactBlock) []model.ArtifactBlock {
	var tables []model.ArtifactBlock
	for _, block := range blocks {
		if block.Type == "table" {
			tables = append(tables, block)
		}
	}
	return tables
}

func tableBlockOf(t *testing.T, blocks []model.ArtifactBlock) model.ArtifactBlock {
	t.Helper()
	for _, block := range blocks {
		if block.Type == "table" {
			return block
		}
	}
	t.Fatalf("blocks = %#v, want a table", blocks)
	return model.ArtifactBlock{}
}

func cellTexts(table *model.BlockTable) [][]string {
	if table == nil {
		return nil
	}
	rows := make([][]string, len(table.Rows))
	for index, row := range table.Rows {
		rows[index] = make([]string, len(row.Cells))
		for column, cell := range row.Cells {
			rows[index][column] = cell.Text
		}
	}
	return rows
}

func equalRows(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if strings.Join(got[index], "\x00") != strings.Join(want[index], "\x00") || len(got[index]) != len(want[index]) {
			return false
		}
	}
	return true
}
