package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every field of GET /api/v1/limits is the constant that enforces it, so a client that prints
// them (`dispatch limits`) says what this server refuses rather than a number of its own.
func TestLimitsAnswersTheConstantsTheServerEnforces(t *testing.T) {
	handler := newTestHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/limits", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	limits := decodeBody[model.DispatchLimits](t, response)
	for _, field := range []struct {
		name string
		got  int64
		want int64
	}{
		{"block_units", int64(limits.BlockUnits), contracts.DispatchBlockUnits},
		{"item_blocks", int64(limits.ItemBlocks), contracts.DispatchItemBlocks},
		{"ask_options", int64(limits.AskOptions), maxAskOptions},
		{"ask_units", int64(limits.AskUnits), contracts.DispatchEventBodyMax},
		{"event_body_units", int64(limits.EventBodyUnits), contracts.DispatchEventBodyMax},
		{"request_bytes", limits.RequestBytes, maxJSONRequestBytes},
		{"document_bytes", int64(limits.DocumentBytes), maxDocumentMarkdownBytes},
		{"handoff_bytes", int64(limits.HandoffBytes), contracts.DispatchHandoffBodyMax},
	} {
		if field.got != field.want {
			t.Errorf("%s = %d, want %d", field.name, field.got, field.want)
		}
	}
}
