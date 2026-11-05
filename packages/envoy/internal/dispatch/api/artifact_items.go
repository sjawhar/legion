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

func (s *server) listArtifactComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	comments, err := s.loadOwnerComments(r.Context(), s.deps.Store.Pool, owner, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	var texts []string
	if trees {
		texts = commentDisplayTexts(nil, comments...)
	}
	defer s.readDisplayTrees(r.Context(), trees, r.PathValue("id"), texts)()
	WriteJSON(w, http.StatusOK, commentReads(comments, trees))
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
