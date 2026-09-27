package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The route reports whether the live document kept what the edit wrote. An uncontended edit says
// so with an empty list rather than with silence, so a caller can tell "nothing was lost" from a
// Dispatch that predates the check (LEGION-269). An edit that inserts nothing - a delete, a
// replace that only shortens, a retype - has nothing a concurrent change could take, and answers
// the same way rather than telling the agent its edit could not be confirmed (Rev1468, P1-a).
func TestAnEditReportsThatTheLiveDocumentKeptIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Lost edit",
		"Alpha paragraph.\n\nBravo paragraph.\n\nCharlie paragraph.\n\nDelta paragraph.\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(
		t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	// The blocks come in document order, so the last paragraph is Delta's.
	var delta string
	for _, block := range blocks {
		if block.Type == "paragraph" {
			delta = block.ID
		}
	}
	if delta == "" {
		t.Fatalf("blocks = %#v, want the Delta paragraph", blocks)
	}

	for _, test := range []struct {
		name string
		op   map[string]any
	}{
		{name: "a replace that inserts", op: map[string]any{"op": "replace", "find": "Alpha paragraph.", "with": "Alpha paragraph edited."}},
		{name: "a delete", op: map[string]any{"op": "delete", "find": "Bravo paragraph."}},
		{name: "a shortening replace", op: map[string]any{"op": "replace", "find": "Charlie paragraph.", "with": "Charlie"}},
		{name: "a retype", op: map[string]any{"op": "retype", "block": delta, "type": "callout"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{test.op},
			}, "alice")
			if edited.Code != http.StatusOK {
				t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
			}
			var body struct {
				Applied int    `json:"applied"`
				LostOps *[]int `json:"lost_ops"`
			}
			if err := json.Unmarshal(edited.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode edit response: %v", err)
			}
			if body.LostOps == nil {
				t.Fatalf("edit response = %s, want a lost_ops verdict rather than null", edited.Body.String())
			}
			if len(*body.LostOps) != 0 {
				t.Fatalf("lost_ops = %v, want nothing lost by an uncontended edit", *body.LostOps)
			}
		})
	}
}

// A write the document lost before it could be versioned is refused, and the refusal says which
// operations went and who else was in the room, so the caller knows what to re-read and why.
func TestALostWriteIsRefusedWithItsOperationsAndParticipants(t *testing.T) {
	for _, test := range []struct {
		name string
		err  *docs.ErrEditLost
		want map[string]any
	}{
		{
			name: "an edit names its operations",
			err: &docs.ErrEditLost{
				Ops:          []int{0, 2},
				Participants: []model.Actor{{Kind: "user", ID: "alice"}},
			},
			want: map[string]any{
				"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				"lost_ops":     []any{float64(0), float64(2)},
				"participants": []any{map[string]any{"kind": "user", "id": "alice"}},
			},
		},
		{
			name: "an accept names its suggestion",
			err:  &docs.ErrEditLost{Suggestion: "comment-7"},
			want: map[string]any{
				"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				"suggestion":   "comment-7",
				"participants": []any{},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&server{}).writeHandlerError(recorder, test.err)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			for key, want := range test.want {
				got, _ := json.Marshal(body[key])
				expected, _ := json.Marshal(want)
				if string(got) != string(expected) {
					t.Fatalf("%s = %s, want %s (body %s)", key, got, expected, recorder.Body.String())
				}
			}
			if _, named := body["lost_ops"]; named && test.err.Suggestion != "" {
				t.Fatalf("an accept's refusal carries lost_ops: %s", recorder.Body.String())
			}
			message, _ := body["error"].(string)
			if message == "" {
				t.Fatalf("refusal carries no message: %s", recorder.Body.String())
			}
		})
	}
}
