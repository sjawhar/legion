package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// How a handler's error becomes a response: one function, writeHandlerError, that every route's
// error goes through, so one failure reads the same whichever route met it.

// The codes writeHandlerError answers a document it cannot read or serve and an unclassified
// failure with, which a comment's or ask's single read also carries as anchor_block_error
// (anchorBlock).
const (
	codeDocSchema             = "DOC_SCHEMA"
	codeDocServiceUnavailable = "DOC_SERVICE_UNAVAILABLE"
	codeDocumentUnloadable    = "DOCUMENT_UNLOADABLE"
	codeInternal              = "INTERNAL"
)

// documentErrorCode names an error a document operation returns when it could not read or serve
// the document, and is empty for any other error. writeHandlerError and anchorBlock both name a
// document error by it, and both in one order, so the two cannot name one error differently.
//
// A room or store that could not serve the document is DOC_SERVICE_UNAVAILABLE whatever failed it,
// so both take that code before any branch that reads the error's cause. A failed room carries the
// error another operation failed it with - settlement's schema refusal, a settlement that failed
// three times (its warm-up refused because the issue had closed, among others), a writer's commit
// that failed or its client cancelled, a store write or load that failed, a history that did not
// decode - and that error says nothing of this request: the caller retries once the room is
// evicted, and its retry meets the document itself.
//
// A stored history this request itself could not decode is DOCUMENT_UNLOADABLE, the state a
// rebuild repairs. A tree outside the schema is DOC_SCHEMA, which both take only after the
// branches that name the caller's own input or a block the document does not hold, so a refusal
// whose reason the renderer gave (an ask block that cannot render) stays that refusal.
func documentErrorCode(err error) string {
	switch {
	case errors.Is(err, docs.ErrServiceUnavailable):
		return codeDocServiceUnavailable
	case errors.Is(err, docs.ErrDocumentUnloadable):
		return codeDocumentUnloadable
	case errors.Is(err, docs.ErrDocSchema):
		return codeDocSchema
	}
	return ""
}

func (s *server) writeHandlerError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.code, apiErr.status, apiErr.message)
		// A 500 the handler named is a server fault like any other, so the operator's log shows it.
		if apiErr.status == http.StatusInternalServerError {
			slog.Error("dispatch: API handler failed", "code", apiErr.code, "error", err)
		}
		return
	}
	documentCode := documentErrorCode(err)
	switch documentCode {
	case codeDocServiceUnavailable:
		writeError(w, documentCode, http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
		return
	case codeDocumentUnloadable:
		writeError(w, documentCode, http.StatusConflict, docs.ErrDocumentUnloadable.Error())
		slog.Warn("dispatch: API document history cannot load", "error", err)
		return
	}
	// The edit route's own ambiguity error names the operation and the quote; the bare pmdoc one
	// still serves every other caller of FindQuote.
	var ambiguousQuote *docs.ErrAnchorAmbiguous
	if errors.As(err, &ambiguousQuote) {
		writeAmbiguousTarget(w, ambiguousQuote.Error(), ambiguousQuote.Candidates)
		return
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		writeAmbiguousTarget(w, ambiguous.Error(), ambiguous.Candidates)
		return
	}

	var preconditionFailed *docs.ErrPreconditionFailed
	if errors.As(err, &preconditionFailed) {
		currentBlocks := make([]model.EditBlockPrecondition, 0)
		for _, mismatch := range preconditionFailed.Mismatches {
			if mismatch.Scope == "block" && mismatch.Current != nil {
				currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
					ID: mismatch.BlockID, Token: *mismatch.Current,
				})
			}
		}
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":      preconditionFailed.Error(),
			"code":       "PRECONDITION_FAILED",
			"current":    map[string]any{"document": preconditionFailed.CurrentDocument, "blocks": currentBlocks},
			"mismatches": preconditionFailed.Mismatches,
		})
		return
	}
	// A write whose text a concurrent browser change removed before the write could be versioned
	// (LEGION-269). Nothing was written, so this reads like PRECONDITION_FAILED: re-read the
	// document and decide again.
	var lostEdit *docs.ErrEditLost
	if errors.As(err, &lostEdit) {
		body := map[string]any{
			"error":        lostEdit.Error(),
			"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
			"participants": participantsOrEmpty(lostEdit.Participants),
		}
		if lostEdit.Suggestion != "" {
			body["suggestion"] = lostEdit.Suggestion
		} else {
			body["lost_ops"] = lostEdit.Ops
		}
		WriteJSON(w, http.StatusConflict, body)
		return
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		writeError(w, "INVALID_PRECONDITION", http.StatusBadRequest, invalidPrecondition.Error())
		return
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		writeError(w, "EDIT_QUEUE_FULL", http.StatusTooManyRequests, docs.ErrPreconditionBusy.Error())
		return
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		writeError(w, "INVALID_ASK_BLOCK", http.StatusBadRequest, invalidAskBlock.Error())
		return
	}
	var unrepresentableAsk *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &unrepresentableAsk) {
		writeError(w, "ASK_BLOCK_TEXT", http.StatusBadRequest, unrepresentableAsk.Error())
		return
	}
	var invalidOp *docs.ErrInvalidOp
	if errors.As(err, &invalidOp) {
		writeError(w, "INVALID_OP", http.StatusBadRequest, invalidOp.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		writeError(w, "TARGET_SPANS_BLOCKS", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTableWidth) {
		writeError(w, "TABLE_WIDTH", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorMissing) {
		writeError(w, "ANCHOR_MISSING", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorOrphaned) {
		writeError(w, "ANCHOR_ORPHANED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		writeError(w, "TARGET_NOT_FOUND", http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, docs.ErrInvalidMarkdown) {
		writeError(w, "INVALID_MARKDOWN", http.StatusBadRequest, err.Error())
		return
	}
	if docs.IsTooLarge(err) {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, err.Error())
		slog.Warn("dispatch: API refused a document too large to store", "error", err)
		return
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault.
	if documentCode == codeDocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			writeError(w, documentCode, http.StatusConflict, outside.Error())
			slog.Warn("dispatch: API document outside Proof schema", "error", err)
			return
		}
		writeError(w, documentCode, http.StatusInternalServerError, err.Error())
		slog.Error("dispatch: API document outside Proof schema", "error", err)
		return
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	writeError(w, codeInternal, http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

// writeAmbiguousTarget serves a quote that resolved in several places: the caller picks one by
// the candidates' positions, or narrows the quote.
func writeAmbiguousTarget(w http.ResponseWriter, message string, candidates []pmdoc.Candidate) {
	WriteJSON(w, http.StatusConflict, map[string]any{
		"error":      message,
		"code":       "TARGET_AMBIGUOUS",
		"candidates": candidates,
	})
}
