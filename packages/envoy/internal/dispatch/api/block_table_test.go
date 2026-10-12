package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

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
		{"Owner", "Due", "Notes"}, {"Sami", "Oct 1", "late"}, {"Oct 2", "soon"},
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
	if literal := [][]string{{"Owner", "Notes"}, {"Sami", "late"}, {"Oct 2"}}; !equalRows(want, literal) {
		t.Fatalf("the listing without column %d = %q, want %q", due.Column, want, literal)
	}
	if got := cellTexts(after.Table); !equalRows(got, want) {
		t.Fatalf("rows after delete_column %d = %q, want %q: each row without the cell listed at that column", due.Column, got, want)
	}
}

// spannedCellsTable is a table a pasted HTML table makes, its blocks stamped with ids as the
// browser editor stamps them: a header cell spanning two columns and a body cell spanning two rows,
// so the row below it holds one cell fewer.
func spannedCellsTable() *pmdoc.Node {
	cell := func(kind, value string, colspan, rowspan int) *pmdoc.Node {
		return &pmdoc.Node{
			Type:     kind,
			Attrs:    pmdoc.Attrs{"alignment": nil, "colspan": colspan, "colwidth": nil, "rowspan": rowspan},
			Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: value}}}},
		}
	}
	table := &pmdoc.Node{Type: "table", Children: []*pmdoc.Node{
		{Type: "table_header_row", Children: []*pmdoc.Node{cell("table_header", "Owner", 2, 1), cell("table_header", "Due", 1, 1), cell("table_header", "Notes", 1, 1)}},
		{Type: "table_row", Children: []*pmdoc.Node{cell("table_cell", "Sami", 1, 2), cell("table_cell", "Oct 1", 1, 1), cell("table_cell", "late", 1, 1)}},
		{Type: "table_row", Children: []*pmdoc.Node{cell("table_cell", "Oct 2", 1, 1), cell("table_cell", "soon", 1, 1)}},
	}}
	pmdoc.EnsureBlockIDs(table)
	return table
}

// requirePlacedAsTheBlockRoute fails t unless table's entry lists rows holding texts, every row and
// cell with a block id the block route places at the index, row and column (and header) listed.
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
	for _, row := range table.Table.Rows {
		if path := place(row.ID); path.Type != "table_row" && path.Type != "table_header_row" || *path.Table.Row != row.Index || path.Table.Column != nil {
			t.Fatalf("row %s listed at index %d, the block route places it %#v", row.ID, row.Index, path.Table)
		}
		for _, cell := range row.Cells {
			path := place(cell.ID)
			if *path.Table.Row != cell.Row || cell.Row != row.Index || path.Table.Column == nil || *path.Table.Column != cell.Column ||
				path.Table.Header == nil || *path.Table.Header != cell.Header {
				t.Fatalf("cell %q listed at row %d, column %d headed %q; the block route places it %#v", cell.Text, cell.Row, cell.Column, cell.Header, path.Table)
			}
		}
	}
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
