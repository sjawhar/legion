// handlers_ui_records.go: GET /v1/credential-requests/{record}, POST .../approve, .../deny — a
// record's own read, approve and deny routes. Part of the UI routes (uiAuth) Dispatch's server
// relays to on behalf of the browser: the UI bearer only proves Dispatch's server is the relay,
// and every action that actually decides something (approve, deny) is authorized by the WebAuthn
// assertion in its body, verified against the persisted, attested, endorsed approver key set
// (contract v9, "The approval signal is a WebAuthn assertion...").
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

type challengesResp struct {
	Approve string `json:"approve"`
	Deny    string `json:"deny"`
}

type recordEnrollmentResp struct {
	Kind      string `json:"kind"`
	RuntimeID string `json:"runtime_id"`
	Operator  string `json:"operator"`
}

type recordDecisionResp struct {
	Event        string    `json:"event"`
	At           time.Time `json:"at"`
	CredentialID string    `json:"credential_id"`
}

type recordResponse struct {
	RecordID        string                `json:"record_id"`
	Kind            string                `json:"kind"`
	State           string                `json:"state"`
	Approver        string                `json:"approver"`
	Enrollment      *recordEnrollmentResp `json:"enrollment"`
	Identifiers     []string              `json:"identifiers"`
	Service         *string               `json:"service"`
	Reason          string                `json:"reason"`
	LifetimeSeconds int                   `json:"lifetime_seconds"`
	RulesVersion    string                `json:"rules_version"`
	ExpiresAt       time.Time             `json:"expires_at"`
	RequestedAt     time.Time             `json:"requested_at"`
	Decided         *recordDecisionResp   `json:"decided"`
	Challenges      *challengesResp       `json:"challenges"`
}

// buildRecordResponse is GET /v1/credential-requests/{id}'s exact shape, reused verbatim by
// lookupMachineLogin (which then always overwrites Challenges — the one route contract v9 lets
// hand out a machine record's challenges, ruling 13's stated exception). Every other caller sees
// challenges only while the record is pending and only for an agent_secret record: a machine login
// is decided by the typed code alone (ruling 13).
func buildRecordResponse(detail requests.RecordDetail) recordResponse {
	resp := recordResponse{
		RecordID: detail.RecordID, Kind: detail.Kind, State: detail.State, Approver: detail.Approver,
		Identifiers: detail.Identifiers, Reason: detail.Reason, LifetimeSeconds: detail.LifetimeSeconds,
		RulesVersion: detail.RulesVersion, ExpiresAt: detail.ExpiresAt, RequestedAt: detail.RequestedAt,
	}
	if detail.Enrollment != nil {
		resp.Enrollment = &recordEnrollmentResp{Kind: detail.Enrollment.Kind, RuntimeID: detail.Enrollment.RuntimeID, Operator: detail.Enrollment.Operator}
	}
	resp.Service = strPtr(detail.Service)
	if detail.Decided != nil {
		resp.Decided = &recordDecisionResp{Event: detail.Decided.Event, At: detail.Decided.At, CredentialID: detail.Decided.CredentialID}
	}
	if detail.State == "pending" && detail.Kind != "launcher_credential" {
		ch := challengePair(detail.RecordID)
		resp.Challenges = &ch
	}
	return resp
}

func challengePair(recordID string) challengesResp {
	approveCh := record.ApproveChallenge(recordID)
	denyCh := record.DenyChallenge(recordID)
	return challengesResp{
		Approve: base64.RawURLEncoding.EncodeToString(approveCh[:]),
		Deny:    base64.RawURLEncoding.EncodeToString(denyCh[:]),
	}
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

// decideBody is both approve's and deny's request body: {"assertion", "code"?}. code is required
// only for a launcher_credential (machine login) record — machine.Service.ApplyDecision checks it
// unconditionally on both approve and deny — and ignored (harmlessly optional) for an agent_secret
// record, which requests.Machine.ApplyDecision never asks for.
type decideBody struct {
	Assertion json.RawMessage `json:"assertion"`
	Code      *string         `json:"code"`
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
	if len(body.Assertion) == 0 {
		writeError(w, http.StatusBadRequest, "ASSERTION_REQUIRED", "assertion is required")
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
	s.decideAgentSecret(w, r, recordID, approve, body.Assertion)
}

func (s *server) decideMachineLogin(w http.ResponseWriter, r *http.Request, recordID string, approve bool, body decideBody) {
	if body.Code == nil || *body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required to decide a machine login")
		return
	}
	_, credentialID, err := s.deps.MachineLogin.ApplyDecision(r.Context(), recordID, approve, body.Assertion, *body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case errors.Is(err, machine.ErrCodeMismatch):
		writeError(w, http.StatusForbidden, "CODE_MISMATCH", err.Error())
		return
	case errors.Is(err, machine.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", err.Error())
		return
	case isAssertionError(err):
		writeError(w, http.StatusForbidden, "ASSERTION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "decide machine login", err)
		return
	}
	if !approve {
		writeJSON(w, http.StatusOK, map[string]string{"state": "denied"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": "approved", "grant_id": nil, "credential_id": strPtr(credentialID)})
}

func (s *server) decideAgentSecret(w http.ResponseWriter, r *http.Request, recordID string, approve bool, assertion json.RawMessage) {
	dec, err := s.deps.Machine.ApplyDecision(r.Context(), recordID, approve, assertion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
		return
	case errors.Is(err, requests.ErrTerminal):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", err.Error())
		return
	case errors.Is(err, requests.ErrGrantChainInvalid):
		writeError(w, http.StatusForbidden, "GRANT_CHAIN_INVALID", err.Error())
		return
	case isAssertionError(err):
		writeError(w, http.StatusForbidden, "ASSERTION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "decide credential request", err)
		return
	}
	if !approve {
		writeJSON(w, http.StatusOK, map[string]string{"state": "denied"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": "approved", "grant_id": strPtr(dec.GrantID), "credential_id": nil})
}
