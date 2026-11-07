package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// getLimits answers the bounds this server holds a write to, each read from the constant that
// enforces it, so a client prints them (`dispatch limits`) rather than carrying a copy that can
// drift from what the server refuses.
func (s *server) getLimits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, model.DispatchLimits{
		BlockUnits:     contracts.DispatchBlockUnits,
		ItemBlocks:     contracts.DispatchItemBlocks,
		AskOptions:     maxAskOptions,
		AskUnits:       contracts.DispatchEventBodyMax,
		EventBodyUnits: contracts.DispatchEventBodyMax,
		RequestBytes:   maxJSONRequestBytes,
		DocumentBytes:  maxDocumentMarkdownBytes,
		HandoffBytes:   contracts.DispatchHandoffBodyMax,
	})
}
