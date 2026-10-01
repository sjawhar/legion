package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestBlockPathReadsPlaceAnAnchoredCellByRowAndColumn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table position",
		"Intro.\n\n| # | Line | Due |\n| --- | --- | --- |\n| 1 | GDM backfill | Oct 1 |\n| 2 | Red-teamer loop | Today |\n")
	comment := decodeBody[struct {
		ID     string       `json:"id"`
		Anchor model.Anchor `json:"anchor"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Today"},
		"body":   "I want this done today",
	}, "alice"))
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "quote": "Today"},
		"question": "Which day is meant?",
	}, "alice"))
	if comment.Anchor.BlockID == nil {
		t.Fatal("the comment anchor records no block")
	}

	routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/"+*comment.Anchor.BlockID, nil, "alice")
	if routed.Code != http.StatusOK {
		t.Fatalf("block path: status=%d body=%s", routed.Code, routed.Body.String())
	}
	path := decodeBody[model.BlockPath](t, routed)
	types, indexes := make([]string, len(path.Path)), make([]int, len(path.Path))
	for i, entry := range path.Path {
		types[i], indexes[i] = entry.Type, entry.Index
		if entry.ID == "" {
			t.Fatalf("path entry %d has no id: %#v", i, path)
		}
	}
	if path.ID != *comment.Anchor.BlockID || path.Type != "paragraph" ||
		!reflect.DeepEqual(types, []string{"table", "table_row", "table_cell", "paragraph"}) || !reflect.DeepEqual(indexes, []int{1, 2, 2, 0}) {
		t.Fatalf("path = %#v, want table[1] › table_row[2] › table_cell[2] › paragraph[0]", path)
	}
	table := path.Table
	if table == nil || table.ID != path.Path[0].ID || table.Row == nil || *table.Row != 2 || table.Column == nil || *table.Column != 2 ||
		table.Header == nil || *table.Header != "Due" || !reflect.DeepEqual(table.Cells, []string{"2", "Red-teamer loop", "Today"}) {
		t.Fatalf("table position = %#v, want row 2, column 2 headed Due", table)
	}

	read := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice"))
	if !reflect.DeepEqual(read.Comment.AnchorBlock, &path) {
		t.Fatalf("comment anchor_block = %#v, want the route's %#v", read.Comment.AnchorBlock, path)
	}
	askRead := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice"))
	if !reflect.DeepEqual(askRead.Ask.AnchorBlock, &path) {
		t.Fatalf("ask anchor_block = %#v, want the route's %#v", askRead.Ask.AnchorBlock, path)
	}

	// Negative controls: the block list did not grow, lists carry no position, an unknown block
	// is the usual 404, a floating comment has no position, a top-level paragraph has a
	// one-entry path with no table.
	if blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")); len(blocks) != 2 {
		t.Fatalf("GET /blocks lists %d blocks, want the paragraph and the table alone: %#v", len(blocks), blocks)
	}
	if list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice"); strings.Contains(list.Body.String(), "anchor_block") {
		t.Fatalf("the comment list carries anchor_block: %s", list.Body.String())
	}
	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/missing", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("unknown block: status=%d body=%s", missing.Code, missing.Body.String())
	}
	floating := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "No anchor."}, "alice"))
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+floating.ID, nil, "alice"); read.Code != http.StatusOK || strings.Contains(read.Body.String(), "anchor_block") {
		t.Fatalf("a floating comment read: status=%d body=%s, want 200 without anchor_block", read.Code, read.Body.String())
	}
	intro := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Intro."}, "body": "Top-level.",
	}, "alice"))
	introRead := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+intro.ID, nil, "alice"))
	if got := introRead.Comment.AnchorBlock; got == nil || got.Table != nil || len(got.Path) != 1 || got.Path[0].Type != "paragraph" || got.Path[0].Index != 0 {
		t.Fatalf("top-level paragraph anchor_block = %#v, want paragraph[0] and no table", got)
	}

	// The block leaves the document (its row is deleted once nothing open anchors there): the
	// position leaves the read with it; the anchor row keeps its stale block_id.
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Today."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_row", "block": table.ID, "index": 2}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("delete row: status=%d body=%s", edited.Code, edited.Body.String())
	}
	stale := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if stale.Code != http.StatusOK || strings.Contains(stale.Body.String(), "anchor_block") {
		t.Fatalf("a comment on a deleted row: status=%d body=%s, want 200 without anchor_block", stale.Code, stale.Body.String())
	}
	if staleRead := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, stale); staleRead.Comment.Anchor == nil || staleRead.Comment.Anchor.BlockID == nil || *staleRead.Comment.Anchor.BlockID != *comment.Anchor.BlockID {
		t.Fatalf("the deleted row's comment anchor = %#v, want it to keep block_id %q", staleRead.Comment.Anchor, *comment.Anchor.BlockID)
	}
}
