// handlers_ui_records.go: GET /v1/credential-requests/{record} and POST .../approve and .../deny,
// the routes that read one record and decide a machine login, and the record shape that read and
// POST /v1/machine-logins/lookup both answer. A machine login is decided on the caller's word by
// a listed service calling with its own Kubernetes ServiceAccount token: the token proves which
// service is calling, and the service names the deciding person in the body's approver field,
// who must be one the record's rule admits (record.MayDecide) and must type the login's code.
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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// recordSessionResp is a credential request's two possibly-differing session ids, read
// independently (LEGION-587): Enrollment is the id the requesting session's own enrollment
// stated when it enrolled, Request is the id the request itself stated as an unsigned override
// in its body. Either is null when that id was never set; a machine login (which has no request
// row and no requesting enrollment) always answers both null. GET /v1/pending's own
// pendingEntry.Session carries this same shape (handlers_ui_pending.go).
type recordSessionResp struct {
	// The id the request itself stated as an unsigned override in its body; null when it named
	// none.
	Request *string `json:"request"`
	// The id the requesting session's own enrollment stated when it enrolled; null when it named
	// none, or for a machine login, which has no requesting enrollment at all.
	Enrollment *string `json:"enrollment"`
}

func sessionResp(s requests.Session) recordSessionResp {
	return recordSessionResp{Request: strPtr(s.Request), Enrollment: strPtr(s.Enrollment)}
}

// recordEnrollmentResp is a record's or a grant's requesting enrollment. Slot is a pod
// enrollment's slot, one of several independent identities in one pod, and null for every
// enrollment without one.
type recordEnrollmentResp struct {
	// "session" (an agent session on a machine its person logged in) or "pod".
	Kind string `json:"kind"`
	// The session's runtime: whatever names the session on its machine (the helper's
	// host:pid:start time, a container's id), or a pod's UID.
	RuntimeID string `json:"runtime_id"`
	// The person the session runs for; empty for a pod.
	Operator string `json:"operator"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

func enrollmentResp(e record.Enrollment) recordEnrollmentResp {
	return recordEnrollmentResp{Kind: e.Kind, RuntimeID: e.RuntimeID, Operator: e.Operator, Slot: strPtr(e.Slot)}
}

// recordDecisionResp is a decided record's terminal event. CredentialID is null unless the
// decision minted a launcher credential: an agent_secret record's approval names none.
type recordDecisionResp struct {
	// "approved", "denied", "expired" or "cancelled".
	Event string `json:"event"`
	// When it was decided.
	At time.Time `json:"at"`
	// The machine credential an approved machine login minted; null otherwise.
	CredentialID *string `json:"credential_id"`
}

// recordResponse is a credential-request record as GET /v1/credential-requests/{record} and POST
// /v1/machine-logins/lookup answer it.
type recordResponse struct {
	// The record's id: the SHA-256 of its text.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// "pending", or how it ended: "approved", "denied", "expired", "cancelled" or "revoked".
	State string `json:"state"`
	// The one login that may decide it, or "anyone": any signed-in person may decide a request
	// for a shared human-tier secret, and may deny a service's machine login (which only a broker
	// before token logins opened, and which nobody approves any more), whatever person it names.
	Approver string `json:"approver"`
	// The session asking; null for a machine login.
	Enrollment *recordEnrollmentResp `json:"enrollment"`
	// The session this record names, read independently of each other; null fields where that id
	// was never set (LEGION-587).
	Session recordSessionResp `json:"session"`
	// The secrets asked for, or the machine logging in.
	Identifiers []string `json:"identifiers"`
	// The service a machine login is for, when it logs a service in rather than a person's
	// machine; null otherwise.
	Service *string `json:"service"`
	// The reason the session gave, verbatim.
	Reason string `json:"reason"`
	// How many seconds the grant or credential lasts once approved; null for a grant that lasts
	// until its session ends. A machine login's credential always names one.
	LifetimeSeconds *int `json:"lifetime_seconds"`
	// The version of the secret policy it was decided under (the SHA-256 of every agent secret's
	// name, owner, tier and ARN); for a machine login, the version current when it was asked.
	RulesVersion string `json:"rules_version"`
	// When it expires undecided.
	ExpiresAt time.Time `json:"expires_at"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// How it was decided; null while pending.
	Decided *recordDecisionResp `json:"decided"`
	// A secret request's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>, where a person
	// signed in to the broker decides it; absent for a machine login, which is decided with its
	// typed code, and absent wherever the page cannot be named.
	ApproveURL string `json:"approve_url,omitempty"`
}

// buildRecordResponse is the record shape GET /v1/credential-requests/{record} and POST
// /v1/machine-logins/lookup both answer, apart from approve_url, which only a caller holding the
// server fills in.
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
		resp.Decided = &recordDecisionResp{Event: detail.Decided.Event, At: detail.Decided.At, CredentialID: strPtr(detail.Decided.CredentialID)}
	}
	return resp
}

// readRecord answers one credential-request record. A secret request's answer also names the
// broker's own page where a person signed in to the broker decides it, exactly as the pending
// list's row does; a machine login, which is decided with its typed code, names none.
func (s *server) readRecord(w http.ResponseWriter, r *http.Request, _ string) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), recordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case err != nil:
		writeInternal(w, "read credential request", err)
		return
	}
	resp := buildRecordResponse(detail)
	if detail.Kind == record.KindAgentSecret {
		resp.ApproveURL = s.approveURL(detail.RecordID)
	}
	writeJSON(w, http.StatusOK, resp)
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

// decisionResponse is what a machine login's approval answers.
type decisionResponse struct {
	// The machine credential the approval minted.
	CredentialID *string `json:"credential_id"`
	// "approved".
	State string `json:"state"`
}

func (s *server) approveRecord(w http.ResponseWriter, r *http.Request) {
	s.decideRecord(w, r, true)
}

func (s *server) denyRecord(w http.ResponseWriter, r *http.Request) {
	s.decideRecord(w, r, false)
}

// decideRecord refuses a secret request for every caller and decides a machine login for a
// listed service. It reads the record's kind first, before anything about the caller: a secret
// request answers 403 DECIDE_ON_BROKER_PAGE naming its page on the broker, whether or not the
// caller holds a ServiceAccount token, since no token makes one decidable here and a caller
// turned away with 401 or ACCOUNT_NOT_LISTED would be told the wrong thing to do. Nothing is
// written and no body is read for that refusal. Every other record is then authenticated exactly
// as the other account routes are (authenticate with authAccount: the source address's bucket,
// the ServiceAccount token, and BROKER_API_ACCOUNTS), and only afterwards does a record id no
// record has answer 404, so an unauthenticated caller learns no more about which ids exist than
// a machine login's own answer tells it. A machine login's decision is logged with the caller
// that relayed it and the approver it named, the record of who acted, since that name is the
// caller's to state.
func (s *server) decideRecord(w http.ResponseWriter, r *http.Request, approve bool) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	kind, kindErr := s.deps.Machine.RecordKind(r.Context(), recordID)
	if kindErr == nil && kind != record.KindLauncherCredential {
		page := s.approveURL(recordID)
		writePageRefusal(w, http.StatusForbidden, "DECIDE_ON_BROKER_PAGE",
			"a secret request is decided only on the secrets broker's own page, by a person signed in to the broker: "+page, page)
		return
	}
	who, ok := s.authenticate(w, r, authAccount)
	if !ok {
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
	slog.Info("decision relayed", "caller", who.account, "approver", body.Approver, "record", recordID, "approve", approve)
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
