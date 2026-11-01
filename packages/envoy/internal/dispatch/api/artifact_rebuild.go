package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// rebuildArtifact replaces a document history no write can change with one seed update: a history
// ygo cannot load, from its latest saved markdown or supplied markdown, and a history that loads but
// stands so near the decode cap or the scan total that every write to it is refused, from its own
// text or supplied markdown, the rebuild being the recovery from either cap. Such a document's room
// is closed first if this server holds it, and the browsers connected to it drop their unsent
// edits; any other document is refused unchanged, 409 DOCUMENT_LIVE while this server holds its
// room and 409 DOCUMENT_LOADS otherwise (docs.Service.RebuildDocument). It is human-only because a
// rebuild intentionally deletes durable history. The rebuild, the version supplied markdown
// writes, its artifact.version event and the move of the open approval request to that version
// commit in one transaction, so no refusal or failure leaves a changed document without them. A
// closed issue's document is rebuilt with the close's ask states written into its seed
// (docs.Service.RebuildDocument), so a block whose ask the close withdrew reads resolved once the
// rebuild commits, not open as the latest version saved it.
func (s *server) rebuildArtifact(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Markdown *string `json:"markdown"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	report, written, err := s.deps.Docs.RebuildDocument(documentCtx, artifact.ID, input.Markdown, actor)
	if errors.Is(err, docs.ErrDocumentLive) {
		writeError(w, "DOCUMENT_LIVE", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, docs.ErrDocumentLoads) {
		writeError(w, "DOCUMENT_LOADS", http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var published []model.Event
	if written.Wrote {
		event, err := s.commitArtifactVersionEvent(r.Context(), tx, ownerForArtifact(artifact), actor, artifact.ID, artifact.Name, written, nil)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		published = append(published, event)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(published...)
	WriteJSON(w, http.StatusOK, report)
}

func (s *server) injectArtifactSchemaFailure(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	if err := s.deps.Docs.InjectSchemaInvalidForTest(r.Context(), artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
