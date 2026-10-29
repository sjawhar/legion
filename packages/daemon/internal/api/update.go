package api

import (
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// readyForUpdate is the worker's answer to the update task: it has pushed everything it wants
// kept, and the process holding its claim may now be replaced. The machine records the signal and
// the head the worker reports (supervise.PodUpdateSignalled); the round's own sweep is what acts
// on it, once every claim of the round has answered.
//
// It does not call refuseIfStaleClaim, and may not: the claims this route exists to hear from are
// exactly the stale ones. A claim in an open update round is served on its previous contract
// (staleClaim) so that it can finish its turn, push and signal here; refusing it would leave its
// work unpushed and its round waiting for a signal no process could send.
func (s *server) readyForUpdate(w http.ResponseWriter, r *http.Request) {
	var req claim.ReadyForUpdateRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	ctx, decided := s.decision(r, req.ClaimToken)
	defer decided()
	err := m.Handle(ctx, supervise.PodUpdateSignalled{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID, Head: req.Head,
	})
	if err != nil {
		s.claimFailure(w, "ready_for_update", req.ClaimToken, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
