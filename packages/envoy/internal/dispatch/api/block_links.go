package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// getBlockLinks lists the links from blocks of the document ?from= names to blocks of the
// document ?to= names (one document may be both), newest first: each with both block nodes, its
// edge kind, and when and in which version of its document the source block first held it. A link
// counts only while both of its blocks are current blocks of their documents.
func (s *server) getBlockLinks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var documents [2]model.GraphNode
	for index, param := range [2]string{"from", "to"} {
		document, err := s.blockLinksDocument(r, param)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		documents[index] = document
	}
	links, err := refs.BlockLinks(r.Context(), s.deps.Store.Pool, documents[0].ID, documents[1].ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.BlockLinks{From: documents[0], To: documents[1], Links: links})
}

// blockLinksDocument is the document node the query parameter param names: a reference to an
// issue's or a project's document, parsed as GET /api/v1/references parses one (text.ParseRef,
// whatever the length of its name). Anything else is 400 INVALID_REFERENCE, and a document that
// does not exist is 404 NOT_FOUND.
func (s *server) blockLinksDocument(r *http.Request, param string) (model.GraphNode, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	ref, ok := text.ParseRef(raw, s.deps.ServerURL)
	if !ok || ref.Kind != "artifact" || ref.Session != "" {
		return model.GraphNode{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%s must name a document: dispatch://KEY/spec, dispatch://KEY/artifact/<slug> or dispatch://PROJECT/artifact/<slug>", param)
	}
	document, found, err := refs.Node(r.Context(), s.deps.Store.Pool, "artifact", text.TargetID(ref), true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no document %s", raw)
	}
	return document, nil
}
