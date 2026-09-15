package api

import (
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
