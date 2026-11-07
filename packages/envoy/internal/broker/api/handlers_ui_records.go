// handlers_ui_records.go: POST /v1/credential-requests/{record}/approve and .../deny, the two
// routes that decide a machine login, and the record shape POST /v1/machine-logins/lookup
// answers. A machine login is decided on the caller's word by a listed service calling with its
// own Kubernetes ServiceAccount token: the token proves which service is calling, and the
// service names the deciding person in the body's approver field, who must be one the record's
// rule admits (record.MayDecide) and must type the login's code.
// Each such decision is logged with the caller that relayed it and the approver it named. A
// secret request is never decided here, by any caller: it is decided only on the broker's own
// approval page (internal/broker/approval), by a person signed in to the broker, and the two
// decide routes refuse it naming that page before they look at the caller at all. A service's
// machine login, which only a broker before token logins opened, may only be denied: approving
// it is 400 SERVICE_LOGS_IN_WITH_TOKEN.
package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

func sessionResp(s requests.Session) wire.RecordSessionResp {
	return wire.RecordSessionResp{Request: strPtr(s.Request), Enrollment: strPtr(s.Enrollment)}
}

func enrollmentResp(e record.Enrollment) wire.RecordEnrollmentResp {
	return wire.RecordEnrollmentResp{Kind: e.Kind, RuntimeID: e.RuntimeID, Operator: e.Operator, Slot: strPtr(e.Slot)}
}

// recordResponse is a credential-request record as POST /v1/machine-logins/lookup answers it:
// wire.RecordResponse itself, named here because the broker's generated HTTP reference documents
// a route's answer from a type this package declares, as introspectionResponse names
// oauthwire.Introspection (handlers_introspect.go).
type recordResponse = wire.RecordResponse

// buildRecordResponse builds the record shape POST /v1/machine-logins/lookup answers.
func buildRecordResponse(detail requests.RecordDetail) recordResponse {
	resp := recordResponse{
		RecordID: detail.RecordID, Kind: detail.Kind, State: detail.State, Approver: detail.Approver,
		Identifiers: detail.Identifiers, Reason: detail.Reason, LifetimeSeconds: detail.LifetimeSeconds,
		RulesVersion: detail.RulesVersion, ExpiresAt: detail.ExpiresAt, RequestedAt: detail.RequestedAt,
		Session: sessionResp(detail.Session),
	}
	if detail.Enrollment != nil {
		enr := enrollmentResp(*detail.Enrollment)
		resp.Enrollment = &enr
	}
	resp.Service = strPtr(detail.Service)
	if detail.Decided != nil {
		resp.Decided = &wire.RecordDecisionResp{Event: detail.Decided.Event, At: detail.Decided.At, CredentialID: strPtr(detail.Decided.CredentialID)}
	}
	return resp
}

// decideBody is both approve's and deny's request body for a machine login, {"approver", "code"}.
// approver is the deciding person's login, which Dispatch's server sets from its own session, and
// code the login's confirmation code: machine.Service.ApplyDecision checks it on both approve and
// deny.
type decideBody struct {
	// The login of the person deciding, which the record's rule must admit: its approver, or
	// anyone signed in for the denial of a service's machine login.
	Approver string `json:"approver"`
	// The machine login's confirmation code, typed again.
	Code *string `json:"code"`
}

// decisionResponse is what a machine login's approval answers: wire.DecisionResponse itself,
// named here for the same reason recordResponse is.
type decisionResponse = wire.DecisionResponse

func (s *server) approveRecord(w http.ResponseWriter, r *http.Request, needs power) {
	s.decideRecord(w, r, true, needs)
}

func (s *server) denyRecord(w http.ResponseWriter, r *http.Request, needs power) {
	s.decideRecord(w, r, false, needs)
}

// decideRecord refuses a secret request for every caller and decides a machine login for a
// listed service. It spends the source address's bucket first, the one every route a
// ServiceAccount token authenticates shares (refuseAccountAddress, DefaultAccountLimit), so no
// caller drives the record read below unmetered; the bucket is spent once a request, here rather
// than in authenticate. It then reads the record's kind, before anything else about the caller:
// a secret request answers 403 DECIDE_ON_BROKER_PAGE naming its page on the broker, whether or
// not the caller holds a ServiceAccount token, since no token makes one decidable here and a
// caller turned away with 401 or ACCOUNT_NOT_LISTED would be told the wrong thing to do. Nothing
// is written and no body is read for that refusal. Every other record is then authenticated
// exactly as the other account routes are (accountCaller: the ServiceAccount token and
// BROKER_API_ACCOUNTS), and the caller's name must carry needs, the power its row declares, as
// authenticate requires of every other route a listed caller's token authenticates
// (refuseWithoutPower); only afterwards does a record id no record has answer 404. So an
// unauthenticated caller does read one thing from the two answers apart: an id that names a
// secret request answers 403 where an id no record has answers 401. A record id is a SHA-256
// hash, so a caller that can name one already holds it, and the shared bucket bounds the asking
// anyway. A machine login's decision is logged with the caller that relayed it and the
// approver it named, the record of who acted, since that name is the caller's to state.
func (s *server) decideRecord(w http.ResponseWriter, r *http.Request, approve bool, needs power) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	if s.refuseAccountAddress(w, r) {
		return
	}
	kind, kindErr := s.deps.Machine.RecordKind(r.Context(), recordID)
	if kindErr == nil && kind != record.KindLauncherCredential {
		page := s.approveURL(recordID)
		writePageRefusal(w, http.StatusForbidden, "DECIDE_ON_BROKER_PAGE",
			"a secret request is decided only on the secrets broker's own page, by a person signed in to the broker: "+page, page)
		return
	}
	who, ok := s.accountCaller(w, r)
	if !ok {
		return
	}
	if refuseWithoutPower(w, who.account, needs, r.Pattern) {
		return
	}
	switch {
	case errors.Is(kindErr, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case kindErr != nil:
		writeInternal(w, "read credential request kind", kindErr)
		return
	}
	var body decideBody
	if !readJSON(w, r, &body, "INVALID_DECISION") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	slog.Info("decision relayed", "caller", who.account.Name, "approver", body.Approver, "record", recordID, "approve", approve)
	s.decideMachineLogin(w, r, recordID, approve, body)
}

func (s *server) decideMachineLogin(w http.ResponseWriter, r *http.Request, recordID string, approve bool, body decideBody) {
	if body.Code == nil || *body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required to decide a machine login")
		return
	}
	_, credentialID, err := s.deps.MachineLogin.ApplyDecision(r.Context(), recordID, approve, body.Approver, *body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case errors.Is(err, machine.ErrCodeMismatch):
		writeError(w, http.StatusForbidden, "CODE_MISMATCH", err.Error())
		return
	case errors.Is(err, record.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case errors.Is(err, machine.ErrAlreadyDecided), errors.Is(err, machine.ErrLoginExpired):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", err.Error())
		return
	case errors.Is(err, machine.ErrKeyHoldsLiveCredential):
		writeError(w, http.StatusConflict, "KEY_HOLDS_LIVE_CREDENTIAL", err.Error())
		return
	case errors.Is(err, machine.ErrServiceLogsInWithToken):
		writeError(w, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN", err.Error())
		return
	case err != nil:
		writeInternal(w, "decide machine login", err)
		return
	}
	if !approve {
		writeJSON(w, http.StatusOK, stateResponse{State: "denied"})
		return
	}
	writeJSON(w, http.StatusOK, decisionResponse{CredentialID: strPtr(credentialID), State: "approved"})
}
