// POST /v1/launcher-credentials and GET /v1/launcher-credentials/{pending} are authNone per the
// routes table: a launcher has no credential yet when it asks for one. Both go straight to
// launcher.Service, which opens a Dispatch ask on the operator's standing "agent-secrets" issue
// and hands the raw token back exactly once, on whichever read first observes state "issued"
// (launcher.Service.Read). Because anyone can call the POST, it is rate limited per source address
// and per operator, and its ask carries a confirmation code the requesting terminal alone prints.
package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/launcher"
)

type requestLauncherCredentialBody struct {
	Operator string  `json:"operator"`
	Host     string  `json:"host"`
	Service  *string `json:"service"`
}

type requestLauncherCredentialResponse struct {
	PendingID        string `json:"pending_id"`
	ConfirmationCode string `json:"confirmation_code"`
}

func (s *server) requestLauncherCredential(w http.ResponseWriter, r *http.Request) {
	var body requestLauncherCredentialBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	if body.Operator == "" || body.Host == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "operator and host are required")
		return
	}
	if s.launcherLimiter.refuse(w, r, dispatch.CanonicalLogin(body.Operator)) {
		return
	}
	pending, err := s.deps.Launcher.Request(r.Context(), body.Operator, body.Host, body.Service)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "open launcher credential request failed")
		return
	}
	writeJSON(w, http.StatusAccepted, requestLauncherCredentialResponse{PendingID: pending.ID, ConfirmationCode: pending.ConfirmationCode})
}

// readLauncherCredentialResponse omits token entirely, rather than sending it as JSON null, once
// launcher.Service.Read has cleared it: a caller polling this route after the one "issued"
// response that carried a token sees state "issued" with no token field at all, which is what
// makes the token single-use on the wire as well as in Postgres.
type readLauncherCredentialResponse struct {
	State string  `json:"state"`
	Token *string `json:"token,omitempty"`
}

func (s *server) readLauncherCredential(w http.ResponseWriter, r *http.Request) {
	state, token, err := s.deps.Launcher.Read(r.Context(), r.PathValue("pending"))
	switch {
	case errors.Is(err, launcher.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such launcher credential request")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read launcher credential request failed")
		return
	}
	resp := readLauncherCredentialResponse{State: state}
	if token != "" {
		resp.Token = &token
	}
	writeJSON(w, http.StatusOK, resp)
}
