// handlers_ui_grants.go: GET /v1/grants, POST /v1/grants/{id}/revoke-by-approver — a person's own
// live-grant list and human revocation route. Part of the routes a listed service calls with its
// own ServiceAccount token (accountAuth), relaying a person's own page: the token proves which
// service is calling, and the person is the one the service names.
package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

// approverGrantsResponse is GET /v1/grants's answer: wire.ApproverGrantsResponse itself, named
// here because the broker's generated HTTP reference documents a route's answer from a type this
// package declares, as introspectionResponse names oauthwire.Introspection
// (handlers_introspect.go).
type approverGrantsResponse = wire.ApproverGrantsResponse

// listGrantsForApprover lists the live grants of the person ?approver= names (writeApproverGrants).
func (s *server) listGrantsForApprover(w http.ResponseWriter, r *http.Request, _ string) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	s.writeApproverGrants(w, r, approver)
}

// writeApproverGrants answers every live grant of a session approver operates, automatic or
// approved by anyone, and every grant approver approved on anyone's session
// (requests.Machine.GrantsForApprover). Dispatch's Live grants page and the operator's own grant
// list both answer through it, so the two agree.
func (s *server) writeApproverGrants(w http.ResponseWriter, r *http.Request, approver string) {
	rows, err := s.deps.Machine.GrantsForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list grants for approver", err)
		return
	}
	out := make([]wire.ApproverGrantResp, len(rows))
	for i, g := range rows {
		names := g.Names
		if names == nil {
			names = []string{}
		}
		out[i] = wire.ApproverGrantResp{
			GrantID: g.GrantID, Granted: g.Granted, RecordID: g.RecordID, Enrollment: enrollmentResp(g.Enrollment),
			Names: names, Approver: strPtr(g.Approver), ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, approverGrantsResponse{Grants: out})
}

// revokeByApproverBody is {"approver"}, the body of both revoke-by-approver routes (a grant's and a
// launcher credential's): the revoking person's login, which Dispatch's server sets from its own
// session.
type revokeByApproverBody struct {
	// The login of the person revoking: for a grant, its approver or its session's operator; for a
	// person's machine login, the person who approved it; for a service's machine login, anyone
	// signed in.
	Approver string `json:"approver"`
}

// revokeByApprover ends a grant as the person the caller names (revokeGrantAs), logging the caller
// that relayed it beside that person. The caller is a named service identity, while the person's
// name is still taken on the caller's word: a revoke only takes power away, unlike a secret
// request's approval, which only the broker's own page makes.
func (s *server) revokeByApprover(w http.ResponseWriter, r *http.Request, callerName string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body revokeByApproverBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	slog.Info("grant revocation relayed", "caller", callerName, "approver", body.Approver, "grant", id)
	s.revokeGrantAs(w, r, id, body.Approver, record.HumanActor(body.Approver))
}

// revokeGrantAs ends grant id as login, recording actor on every row it writes: login must be the
// grant's approver or its enrollment's operator (requests.Machine.RevokeByApprover's own mayRevoke
// check). When the operator revokes a grant the session got without asking, its secrets are
// withheld from that session, even when the grant had already ended: its other grants that got
// them without asking end too, and its later requests for them ask their owner.
func (s *server) revokeGrantAs(w http.ResponseWriter, r *http.Request, id, login, actor string) {
	err := s.deps.Machine.RevokeByApprover(r.Context(), id, login, actor)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case errors.Is(err, requests.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
