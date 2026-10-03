// handlers_ui_records.go: GET /v1/credential-requests/{record}, POST .../approve, .../deny — a
// record's own read, approve and deny routes. Part of the UI routes (uiAuth) Dispatch's server
// relays to on behalf of the browser: the UI bearer proves Dispatch's server is the caller, and
// Dispatch names the deciding human in the body's approver field from its own session, so a
// decision is authorized by that login being the record's approver.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// recordEnrollmentResp is a record's or a grant's requesting enrollment. Slot is a pod
// enrollment's slot, one of several independent identities in one pod, and null for every
// enrollment without one.
type recordEnrollmentResp struct {
	// "box", "host" or "pod".
	Kind string `json:"kind"`
	// The session's runtime: a host session's host:pid:start time, a box's id, a pod's UID.
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

// recordResponse is a credential-request record as GET /v1/credential-requests/{record} and
// POST /v1/machine-logins/lookup answer it.
type recordResponse struct {
	// The record's id: the SHA-256 of its text.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// "pending", or how it ended: "approved", "denied", "expired", "cancelled" or "revoked".
	State string `json:"state"`
	// The one login that may decide it.
	Approver string `json:"approver"`
	// The session asking; null for a machine login.
	Enrollment *recordEnrollmentResp `json:"enrollment"`
	// The secrets asked for, or the machine logging in.
	Identifiers []string `json:"identifiers"`
	// The service a machine login is for, when it logs a service in rather than a person's
	// machine; null otherwise.
	Service *string `json:"service"`
	// The reason the session gave, verbatim.
	Reason string `json:"reason"`
	// How long the grant or credential lasts once approved.
	LifetimeSeconds int `json:"lifetime_seconds"`
	// The SHA-256 of the rules that decided it needs approval.
	RulesVersion string `json:"rules_version"`
	// When it expires undecided.
	ExpiresAt time.Time `json:"expires_at"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// How it was decided; null while pending.
	Decided *recordDecisionResp `json:"decided"`
}

// buildRecordResponse is GET /v1/credential-requests/{id}'s exact shape, reused verbatim by
// lookupMachineLogin.
func buildRecordResponse(detail requests.RecordDetail) recordResponse {
	resp := recordResponse{
		RecordID: detail.RecordID, Kind: detail.Kind, State: detail.State, Approver: detail.Approver,
		Identifiers: detail.Identifiers, Reason: detail.Reason, LifetimeSeconds: detail.LifetimeSeconds,
		RulesVersion: detail.RulesVersion, ExpiresAt: detail.ExpiresAt, RequestedAt: detail.RequestedAt,
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

func (s *server) readRecord(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, buildRecordResponse(detail))
}

// decideBody is both approve's and deny's request body: {"approver", "code"?}. approver is the
// deciding human's Dispatch login, which Dispatch's server sets from its own session. code is
// required only for a launcher_credential (machine login) record — machine.Service.ApplyDecision
// checks it unconditionally on both approve and deny — and ignored for an agent_secret record,
// which requests.Machine.ApplyDecision never asks for.
type decideBody struct {
	// The Dispatch login of the person deciding, which must be the record's approver.
	Approver string `json:"approver"`
	// A machine login only: its confirmation code, typed again; ignored for a secret request.
	Code *string `json:"code"`
}

// decisionResponse is what an approval answers.
type decisionResponse struct {
	// The machine credential an approved machine login minted; null for a secret request.
	CredentialID *string `json:"credential_id"`
	// The grant an approved secret request made; null for a machine login.
	GrantID *string `json:"grant_id"`
	// "approved".
	State string `json:"state"`
}

func (s *server) approveRecord(w http.ResponseWriter, r *http.Request) { s.decideRecord(w, r, true) }
func (s *server) denyRecord(w http.ResponseWriter, r *http.Request)    { s.decideRecord(w, r, false) }

// decideRecord dispatches by the record's own kind — agent_secret to requests.Machine.
// ApplyDecision, launcher_credential to machine.Service.ApplyDecision — since the two are decided
// by different services with different state (a request row vs. a poll row) behind one shared
// record id.
func (s *server) decideRecord(w http.ResponseWriter, r *http.Request, approve bool) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	var body decideBody
	if !readJSON(w, r, &body, "INVALID_DECISION") {
		return
	}
	if !requireApprover(w, body.Approver) {
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
	if kind == "launcher_credential" {
		s.decideMachineLogin(w, r, recordID, approve, body)
		return
	}
	s.decideAgentSecret(w, r, recordID, approve, body.Approver)
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

func (s *server) decideAgentSecret(w http.ResponseWriter, r *http.Request, recordID string, approve bool, login string) {
	dec, err := s.deps.Machine.ApplyDecision(r.Context(), recordID, approve, login)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case errors.Is(err, requests.ErrTerminal), errors.Is(err, requests.ErrExpired):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", err.Error())
		return
	case errors.Is(err, requests.ErrGrantChainInvalid):
		writeError(w, http.StatusForbidden, "GRANT_CHAIN_INVALID", err.Error())
		return
	case errors.Is(err, record.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case err != nil:
		writeInternal(w, "decide credential request", err)
		return
	}
	if !approve {
		writeJSON(w, http.StatusOK, stateResponse{State: "denied"})
		return
	}
	writeJSON(w, http.StatusOK, decisionResponse{GrantID: strPtr(dec.GrantID), State: "approved"})
}
