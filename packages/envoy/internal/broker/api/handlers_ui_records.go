// handlers_ui_records.go: POST /v1/credential-requests/{record}/approve and .../deny, the routes a
// listed service calls with its own Kubernetes ServiceAccount token (accountAuth) to decide a
// machine login, and the record shape POST /v1/machine-logins/lookup answers. A machine login is
// decided here on the caller's word: the token proves which service is calling, and the service
// names the deciding person in the body's approver field, who must be one the record's rule admits
// (record.MayDecide) and must type the login's code. Each such decision is logged with the caller
// that relayed it and the approver it named. A secret request is never decided here: it is decided
// only on the broker's own approval page (internal/broker/approval), by a person signed in to the
// broker, and these routes refuse it naming that page. A service's machine login, which only a
// broker before token logins opened, may only be denied: approving it is 400
// SERVICE_LOGS_IN_WITH_TOKEN.
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

// recordResponse is a credential-request record as POST /v1/machine-logins/lookup answers it.
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
}

// buildRecordResponse is POST /v1/machine-logins/lookup's exact shape.
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

func (s *server) approveRecord(w http.ResponseWriter, r *http.Request, callerName string) {
	s.decideRecord(w, r, callerName, true)
}

func (s *server) denyRecord(w http.ResponseWriter, r *http.Request, callerName string) {
	s.decideRecord(w, r, callerName, false)
}

// decideRecord decides a machine login (machine.Service.ApplyDecision), the one kind of record a
// service may decide, and refuses every other record, writing nothing, with 403
// DECIDE_ON_BROKER_PAGE naming the record's approval page: a secret request is decided only there,
// by a person signed in to the broker, never on a service's word. The record's kind is read before
// anything else, so a secret request is refused whatever the body says. A machine login's
// decision is logged with the caller that relayed it and the approver it named, the record of who
// acted, since that name is the caller's to state.
func (s *server) decideRecord(w http.ResponseWriter, r *http.Request, callerName string, approve bool) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	kind, err := s.deps.Machine.RecordKind(r.Context(), recordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case err != nil:
		writeInternal(w, "read credential request kind", err)
		return
	}
	if kind != record.KindLauncherCredential {
		page := s.approveURL(recordID)
		writePageRefusal(w, http.StatusForbidden, "DECIDE_ON_BROKER_PAGE",
			"a secret request is decided only on the secrets broker's own page, by a person signed in to the broker: "+page, page)
		return
	}
	var body decideBody
	if !readJSON(w, r, &body, "INVALID_DECISION") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	slog.Info("decision relayed", "caller", callerName, "approver", body.Approver, "record", recordID, "approve", approve)
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
