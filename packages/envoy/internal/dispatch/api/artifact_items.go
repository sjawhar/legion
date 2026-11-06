package api

import "net/http"

func (s *server) documentOwnerForHandler(w http.ResponseWriter, r *http.Request) (owner, bool) {
	_, documentOwner, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return owner{}, false
	}
	return documentOwner, true
}

func (s *server) listArtifactAsks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	state, err := parseAskListState(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	asks, _, err := s.queryOwnerAsks(r.Context(), s.deps.Store.Pool, owner, state)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	var texts []string
	if trees {
		texts = askReadDisplayTexts(nil, asks...)
	}
	defer s.readDisplayTrees(r.Context(), trees, r.PathValue("id"), texts)()
	WriteJSON(w, http.StatusOK, askReads(asks, trees))
}

func (s *server) createArtifactAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	s.createAskFor(w, r, owner)
}

// listArtifactComments answers an unlinked document's comment list: the same list as an issue's,
// under the same validator and keeping the same answer (answerCommentList), scoped to the one
// document the route names rather than to an issue.
func (s *server) listArtifactComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	s.answerCommentList(w, r, owner, "", r.PathValue("id"))
}

func (s *server) createArtifactComment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	s.createCommentFor(w, r, owner)
}

func (s *server) listArtifactEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	options, err := parseEventListOptions(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.readEvents(r.Context(), owner, options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	var texts []string
	if trees {
		texts = eventDisplayTexts(nil, events...)
	}
	defer s.readDisplayTrees(r.Context(), trees, r.PathValue("id"), texts)()
	attachEventTrees(events, trees)
	WriteJSON(w, http.StatusOK, events)
}
