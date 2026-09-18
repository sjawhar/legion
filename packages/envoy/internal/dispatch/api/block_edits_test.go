package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The ids GET /blocks lists address delete and move; the edits land through the same
// error mapping every other operation uses.
func TestDocumentEditsAddressBlocksByID(t *testing.T) {
	handler := newTestHandler(t)
	const ask = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Block edits", "# Title\n\n"+ask+"\nContext ends with no drift.\n\n1. only\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var listID string
	for _, block := range blocks {
		if block.Type == "ordered_list" {
			listID = block.ID
		}
	}
	if listID == "" {
		t.Fatalf("blocks = %#v, want an ordered_list", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "move", "block": "decision", "after": "no drift."},
			{"op": "delete", "block": listID},
		},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":2`) {
		t.Fatalf("block edits: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	if want := "# Title\n\nContext ends with no drift.\n\n" + ask; text.Markdown != want {
		t.Fatalf("after block edits = %q, want %q", text.Markdown, want)
	}

	for _, test := range []struct {
		name   string
		op     map[string]any
		status int
		code   string
		detail string
	}{
		{name: "anchor inside the moved block", op: map[string]any{"op": "move", "block": "decision", "after": "transport"}, status: http.StatusBadRequest, code: "INVALID_OP", detail: `field \"after\"`},
		{name: "unknown block", op: map[string]any{"op": "delete", "block": "missing"}, status: http.StatusNotFound, code: "TARGET_NOT_FOUND", detail: `block \"missing\"`},
		{name: "move without an anchor", op: map[string]any{"op": "move", "block": "decision"}, status: http.StatusBadRequest, code: "INVALID_OP", detail: "after or before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{test.op}}, "alice")
			body := response.Body.String()
			if response.Code != test.status || !strings.Contains(body, `"code":"`+test.code+`"`) || !strings.Contains(body, test.detail) {
				t.Fatalf("%s: status=%d body=%s", test.name, response.Code, body)
			}
		})
	}
}

// A table is one addressable block. Deleting its header promotes the first body
// row so the table keeps its identity and remains valid Markdown.
func TestDocumentEditsDeleteTableHeaderRowInPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table row edit", "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_row", "block": tableID, "index": 0}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete table header row: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| A10 | old |\n| :--- | :--- |\n| A11 | new |\n"
	if text.Markdown != want {
		t.Fatalf("after header-row deletion = %q, want %q", text.Markdown, want)
	}
	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	for _, block := range blocks {
		if block.Type == "table" {
			if block.ID != tableID {
				t.Fatalf("table block id after row deletion = %q, want %q", block.ID, tableID)
			}
			return
		}
	}
	t.Fatalf("blocks after row deletion = %#v, want table %q", blocks, tableID)
}

func TestDocumentEditsRejectInvalidTableIndicesWithoutChangingDocument(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Table index edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	const want = "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 | new |\n"
	for _, test := range []struct {
		name     string
		hasIndex bool
		index    any
		detail   string
	}{
		{name: "missing", detail: "index is required"},
		{name: "negative", hasIndex: true, index: -1, detail: "index -1"},
		{name: "negative zero", hasIndex: true, index: json.RawMessage("-0"), detail: "index -0"},
		{name: "fractional", hasIndex: true, index: 1.5, detail: "index 1.5"},
		{name: "numeric string", hasIndex: true, index: "1", detail: `index "1"`},
		{name: "non-numeric string", hasIndex: true, index: "one", detail: `index "one"`},
		{name: "overflow", hasIndex: true, index: json.RawMessage("999999999999999999999999999999"), detail: "index 999999999999999999999999999999"},
		{name: "out of range", hasIndex: true, index: 3, detail: "row index 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := map[string]any{"op": "delete_row", "block": tableID}
			if test.hasIndex {
				op["index"] = test.index
			}
			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{op},
			}, "alice")
			body := rejected.Body.String()
			message := decodeBody[struct {
				Error string `json:"error"`
			}](t, rejected)
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(message.Error, `field "index"`) || !strings.Contains(message.Error, test.detail) ||
				!strings.Contains(message.Error, "3 rows") || !strings.Contains(message.Error, "2 columns") {
				t.Fatalf("invalid %s table index: status=%d body=%s", test.name, rejected.Code, body)
			}
			text := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if text.Markdown != want {
				t.Fatalf("invalid %s table index changed document = %q, want %q", test.name, text.Markdown, want)
			}
		})
	}
}

func TestDocumentEditsDeleteTableColumnInPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table column edit", "| Key | Value | Notes |\n| --- | --- | --- |\n| A10 | old | first |\n| A11 | new | second |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete table column: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key | Notes |\n| :--- | :--- |\n| A10 | first |\n| A11 | second |\n"
	if text.Markdown != want {
		t.Fatalf("after column deletion = %q, want %q", text.Markdown, want)
	}
	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	for _, block := range blocks {
		if block.Type == "table" {
			if block.ID != tableID {
				t.Fatalf("table block id after column deletion = %q, want %q", block.ID, tableID)
			}
			return
		}
	}
	t.Fatalf("blocks after column deletion = %#v, want table %q", blocks, tableID)
}

func TestDocumentEditsCanonicalizeRaggedTableBeforeColumnDeletion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ragged table column edit", "| One | Two | Three |\n| --- | --- | --- |\n| first | second | third |\n| only |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete ragged table column: status=%d body=%s", edited.Code, edited.Body.String())
	}
	after := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| One | Three |\n| :--- | :--- |\n| first | third |\n| only |  |\n"
	if after.Markdown != want {
		t.Fatalf("ragged column deletion = %q, want %q", after.Markdown, want)
	}
}

func TestDocumentEditsRejectDeletionOfLastTableRowOrColumn(t *testing.T) {
	for _, test := range []struct {
		name  string
		op    string
		index int
	}{
		{name: "row", op: "delete_row", index: 1},
		{name: "column", op: "delete_column", index: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Last table "+test.name+" edit", "| Key |\n| --- |\n| A10 |\n")
			blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
			var tableID string
			for _, block := range blocks {
				if block.Type == "table" {
					tableID = block.ID
					break
				}
			}
			if tableID == "" {
				t.Fatalf("blocks = %#v, want a table", blocks)
			}
			before := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))

			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": test.op, "block": tableID, "index": test.index}},
			}, "alice")
			body := rejected.Body.String()
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(body, `field \"index\"`) || !strings.Contains(body, "last remaining") ||
				!strings.Contains(body, "2 rows") || !strings.Contains(body, "1 column") {
				t.Fatalf("last table %s: status=%d body=%s", test.name, rejected.Code, body)
			}
			after := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if after.Markdown != before.Markdown {
				t.Fatalf("last %s deletion changed document = %q, want %q", test.name, after.Markdown, before.Markdown)
			}
		})
	}
}

func TestDocumentEditsExplainCascadedBlockInAtomicBatch(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "- Parent\n  - Child\n"
	issue := createInteractionIssue(t, handler, "TEST", "Cascaded block edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var parentID, childID string
	for _, block := range blocks {
		if block.Type != "bullet_list" {
			continue
		}
		if block.From == 0 {
			parentID = block.ID
		} else {
			childID = block.ID
		}
	}
	if parentID == "" || childID == "" {
		t.Fatalf("blocks = %#v, want nested bullet lists", blocks)
	}
	before := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "delete", "block": parentID},
			{"op": "delete", "block": childID},
		},
	}, "alice")
	body := rejected.Body.String()
	if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
		!strings.Contains(body, `field \"block\"`) || !strings.Contains(body, "operation 0") ||
		!strings.Contains(body, "cascade") || !strings.Contains(body, parentID) {
		t.Fatalf("cascaded block batch: status=%d body=%s", rejected.Code, body)
	}
	after := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	if after.Markdown != before.Markdown {
		t.Fatalf("cascaded block batch changed document = %q, want %q", after.Markdown, before.Markdown)
	}
}

func TestDocumentEditsProtectLiveCellAnchorsAndListThemOnTheirTable(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key | Status | Keep |\n| --- | --- | --- |\n| A10 | blocked | first |\n| A11 | later | second |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Table anchors", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Can this ship?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "blocked"},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create table ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	commentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Investigate this status.",
		"anchor": map[string]any{"artifact": "spec", "quote": "blocked"},
	}, "alice")
	if commentResponse.Code != http.StatusCreated {
		t.Fatalf("create table comment: status=%d body=%s", commentResponse.Code, commentResponse.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, commentResponse)

	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	foundTable := false
	for _, block := range blocks {
		if block.Type == "table" {
			foundTable = true
			if block.References.Asks != 1 || block.References.Comments != 1 {
				t.Fatalf("table references = %#v, want one ask and one comment", block.References)
			}
			break
		}
	}
	if !foundTable {
		t.Fatalf("blocks after anchors = %#v, want a table", blocks)
	}
	before := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	for _, test := range []struct {
		name  string
		op    string
		index int
	}{
		{name: "row", op: "delete_row", index: 1},
		{name: "column", op: "delete_column", index: 1},
	} {
		t.Run("open anchors block "+test.name+" deletion", func(t *testing.T) {
			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": test.op, "block": tableID, "index": test.index}},
			}, "alice")
			body := rejected.Body.String()
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(body, `field \"index\"`) || !strings.Contains(body, test.name+" index") ||
				!strings.Contains(body, ask.ID) || !strings.Contains(body, comment.ID) {
				t.Fatalf("delete table %s with live anchors: status=%d body=%s", test.name, rejected.Code, body)
			}
			after := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if after.Markdown != before.Markdown {
				t.Fatalf("live anchors did not block %s deletion: %q, want %q", test.name, after.Markdown, before.Markdown)
			}
		})
	}
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Yes."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer table ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve table comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("delete table column with historical anchors: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key | Keep |\n| :--- | :--- |\n| A10 | first |\n| A11 | second |\n"
	if text.Markdown != want {
		t.Fatalf("table after deleting historical anchors = %q, want %q", text.Markdown, want)
	}
}

func TestDocumentEditsExplainCascadedTableInAtomicBatch(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key |\n| --- |\n| A10 |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Cascaded table edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "delete", "block": tableID},
			{"op": "delete_row", "block": tableID, "index": 0},
		},
	}, "alice")
	body := rejected.Body.String()
	if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
		!strings.Contains(body, `field \"block\"`) || !strings.Contains(body, "operation 0") ||
		!strings.Contains(body, tableID) {
		t.Fatalf("cascaded table batch: status=%d body=%s", rejected.Code, body)
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key |\n| :--- |\n| A10 |\n"
	if text.Markdown != want {
		t.Fatalf("cascaded table batch changed document = %q, want %q", text.Markdown, want)
	}
}
