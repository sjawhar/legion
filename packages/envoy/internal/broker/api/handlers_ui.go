// handlers_ui.go: the UI routes (uiAuth) Dispatch's server relays to on behalf of the browser —
// the pending list, a record's own read/approve/deny, the machine-login code lookup, approver key
// management, and the approver's own grant list/revoke. None of these authenticate a human: the
// UI bearer only proves Dispatch's server is the relay, and every action that actually decides
// something (approve, deny, revoke-by-approver) is authorized by the WebAuthn assertion in its
// body, verified against the persisted, attested, endorsed approver key set (contract v9,
// "The approval signal is a WebAuthn assertion...").
package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// uiOriginHost is BROKER_UI_ORIGIN's host: the WebAuthn rpId every ceremony this file opens
// embeds, and the audience registration/assertion verification checks against on the other end
// (approvers.Verifier.Origin, wired from the same BROKER_UI_ORIGIN in cmd/broker/main.go).
func (s *server) uiOriginHost() string {
	u, err := url.Parse(s.deps.UIOrigin)
	if err != nil || u.Hostname() == "" {
		return s.deps.UIOrigin
	}
	return u.Hostname()
}

// --- GET /v1/pending ---

type pendingEntry struct {
	RecordID    string    `json:"record_id"`
	Kind        string    `json:"kind"`
	Identifiers []string  `json:"identifiers"`
	RequestedAt time.Time `json:"requested_at"`
}

func (s *server) listPending(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if approver == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
		return
	}
	rows, err := s.deps.Machine.PendingForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list pending requests", err)
		return
	}
	entries := make([]pendingEntry, len(rows))
	for i, row := range rows {
		entries[i] = pendingEntry{RecordID: row.RecordID, Kind: row.Kind, Identifiers: row.Identifiers, RequestedAt: row.RequestedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": entries})
}

// --- GET /v1/credential-requests/{record}, POST .../approve, .../deny ---

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

// --- POST /v1/machine-logins/lookup ---

type lookupMachineLoginBody struct {
	Code string `json:"code"`
}

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code —
// the operator's dashboard never needs the machine's opaque pending id — and is the only route
// that hands out a machine record's approve/deny challenges (ruling 13).
func (s *server) lookupMachineLogin(w http.ResponseWriter, r *http.Request) {
	var body lookupMachineLoginBody
	if !readJSON(w, r, &body, "INVALID_LOOKUP") {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required")
		return
	}
	view, err := s.deps.MachineLogin.LookupByCode(r.Context(), body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NO_SUCH_CODE", "no pending machine login has this code")
		return
	case err != nil:
		writeInternal(w, "lookup machine login", err)
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), view.RecordID)
	if err != nil {
		writeInternal(w, "read credential request", err)
		return
	}
	resp := buildRecordResponse(detail)
	ch := challengesResp{
		Approve: base64.RawURLEncoding.EncodeToString(view.ApproveChallenge),
		Deny:    base64.RawURLEncoding.EncodeToString(view.DenyChallenge),
	}
	resp.Challenges = &ch
	writeJSON(w, http.StatusOK, resp)
}

// --- approver key routes ---

type keyInfoResp struct {
	CredentialID string     `json:"credential_id"`
	AAGUID       string     `json:"aaguid"`
	RegisteredAt time.Time  `json:"registered_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	State        string     `json:"state"`
	EndorsedBy   *string    `json:"endorsed_by"`
	Seeded       bool       `json:"seeded"`
}

func (s *server) listKeys(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	keys, err := s.deps.Approvers.Keys(r.Context(), login)
	if err != nil {
		writeInternal(w, "list approver keys", err)
		return
	}
	out := make([]keyInfoResp, len(keys))
	for i, k := range keys {
		out[i] = keyInfoResp{
			CredentialID: k.CredentialID, AAGUID: k.AAGUID.String(), RegisteredAt: k.RegisteredAt,
			LastUsedAt: k.LastUsedAt, State: k.State, EndorsedBy: strPtr(k.EndorsedBy), Seeded: k.Seeded,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// webauthn* types are the exact JSON shapes navigator.credentials.create()/get() expect
// (PublicKeyCredentialCreationOptions / PublicKeyCredentialRequestOptions, WebAuthn L3 JSON
// serialization: byte strings are base64url).
type webauthnRP struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type webauthnUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type webauthnPubKeyCredParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

type webauthnAuthenticatorSelection struct {
	UserVerification string `json:"userVerification"`
	ResidentKey      string `json:"residentKey"`
}

type registrationOptions struct {
	RP                     webauthnRP                     `json:"rp"`
	User                   webauthnUser                   `json:"user"`
	Challenge              string                         `json:"challenge"`
	PubKeyCredParams       []webauthnPubKeyCredParam      `json:"pubKeyCredParams"`
	Timeout                int                            `json:"timeout"`
	Attestation            string                         `json:"attestation"`
	AuthenticatorSelection webauthnAuthenticatorSelection `json:"authenticatorSelection"`
}

// webauthnCeremonyTimeoutMS is how long the browser's create()/get() call itself waits for the
// authenticator, independent of the 10-minute server-side ceremony row expiry.
const webauthnCeremonyTimeoutMS = 60000

func (s *server) registrationCreationOptions(login string, challenge []byte) registrationOptions {
	login = record.CanonicalLogin(login)
	sum := sha256.Sum256([]byte(login))
	return registrationOptions{
		RP:               webauthnRP{ID: s.uiOriginHost(), Name: "Trajectory Labs Secrets"},
		User:             webauthnUser{ID: base64.RawURLEncoding.EncodeToString(sum[:]), Name: login, DisplayName: login},
		Challenge:        base64.RawURLEncoding.EncodeToString(challenge),
		PubKeyCredParams: []webauthnPubKeyCredParam{{Type: "public-key", Alg: -7}},
		Timeout:          webauthnCeremonyTimeoutMS,
		Attestation:      "direct",
		AuthenticatorSelection: webauthnAuthenticatorSelection{
			UserVerification: "required", ResidentKey: "discouraged",
		},
	}
}

type webauthnAllowCredential struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type assertionOptions struct {
	Challenge        string                    `json:"challenge"`
	RPID             string                    `json:"rpId"`
	AllowCredentials []webauthnAllowCredential `json:"allowCredentials"`
	UserVerification string                    `json:"userVerification"`
	Timeout          int                       `json:"timeout"`
}

func (s *server) assertionCreationOptions(challenge []byte, credentialID string) assertionOptions {
	return assertionOptions{
		Challenge:        base64.RawURLEncoding.EncodeToString(challenge),
		RPID:             s.uiOriginHost(),
		AllowCredentials: []webauthnAllowCredential{{Type: "public-key", ID: credentialID}},
		UserVerification: "required",
		Timeout:          webauthnCeremonyTimeoutMS,
	}
}

func (s *server) beginRegister(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	ceremony, err := s.deps.Approvers.BeginRegister(r.Context(), login)
	if err != nil {
		writeInternal(w, "begin key registration", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ceremony_id": ceremony.ID,
		"publicKey":   s.registrationCreationOptions(login, ceremony.Challenge),
	})
}

type finishCeremonyBody struct {
	CeremonyID string          `json:"ceremony_id"`
	Response   json.RawMessage `json:"response"`
}

func (s *server) finishRegister(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body finishCeremonyBody
	if !readJSON(w, r, &body, "INVALID_FINISH") {
		return
	}
	yamlText, err := s.deps.Approvers.FinishRegister(r.Context(), login, body.CeremonyID, body.Response)
	switch {
	case errors.Is(err, approvers.ErrCeremonyNotFound):
		writeError(w, http.StatusNotFound, "CEREMONY_NOT_FOUND", err.Error())
		return
	case errors.Is(err, approvers.ErrCeremonyExpired):
		writeError(w, http.StatusBadRequest, "CEREMONY_EXPIRED", err.Error())
		return
	case errors.Is(err, approvers.ErrRegistrationInvalid):
		writeError(w, http.StatusBadRequest, "REGISTRATION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "finish key registration", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": yamlText})
}

type beginEndorseBody struct {
	CredentialID string `json:"credential_id"`
	KeyHash      string `json:"key_hash"`
}

func (s *server) beginEndorse(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body beginEndorseBody
	if !readJSON(w, r, &body, "INVALID_ENDORSE") {
		return
	}
	if body.CredentialID == "" || body.KeyHash == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ENDORSE", "credential_id and key_hash are required")
		return
	}
	ceremony, err := s.deps.Approvers.BeginEndorse(r.Context(), login, body.CredentialID, body.KeyHash)
	if err != nil {
		writeInternal(w, "begin key endorsement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ceremony_id": ceremony.ID,
		"publicKey":   s.assertionCreationOptions(ceremony.Challenge, body.CredentialID),
	})
}

func (s *server) finishEndorse(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	var body finishCeremonyBody
	if !readJSON(w, r, &body, "INVALID_FINISH") {
		return
	}
	yamlText, err := s.deps.Approvers.FinishEndorse(r.Context(), login, body.CeremonyID, body.Response)
	switch {
	case errors.Is(err, approvers.ErrCeremonyNotFound):
		writeError(w, http.StatusNotFound, "CEREMONY_NOT_FOUND", err.Error())
		return
	case errors.Is(err, approvers.ErrCeremonyExpired):
		writeError(w, http.StatusBadRequest, "CEREMONY_EXPIRED", err.Error())
		return
	case errors.Is(err, approvers.ErrEndorserNotFound):
		writeError(w, http.StatusBadRequest, "ENDORSER_NOT_FOUND", err.Error())
		return
	case isAssertionError(err):
		writeError(w, http.StatusForbidden, "ASSERTION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "finish key endorsement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": yamlText})
}

// --- GET /v1/grants, POST /v1/grants/{id}/revoke-by-approver ---

type approverGrantResp struct {
	GrantID    string               `json:"grant_id"`
	RecordID   *string              `json:"record_id"`
	Enrollment recordEnrollmentResp `json:"enrollment"`
	Names      []string             `json:"names"`
	ExpiresAt  time.Time            `json:"expires_at"`
	CreatedAt  time.Time            `json:"created_at"`
}

func (s *server) listGrantsForApprover(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if approver == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
		return
	}
	rows, err := s.deps.Machine.GrantsForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list grants for approver", err)
		return
	}
	out := make([]approverGrantResp, len(rows))
	for i, g := range rows {
		names := g.Names
		if names == nil {
			names = []string{}
		}
		out[i] = approverGrantResp{
			GrantID:  g.GrantID,
			RecordID: g.RecordID,
			Enrollment: recordEnrollmentResp{
				Kind: g.Enrollment.Kind, RuntimeID: g.Enrollment.RuntimeID, Operator: g.Enrollment.Operator,
			},
			Names: names, ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

type revokeByApproverBody struct {
	Assertion json.RawMessage `json:"assertion"`
}

// revokeByApprover ends a grant on a human's WebAuthn assertion over its revoke challenge: the
// key must belong to the grant's approver or its enrollment's operator (requests.Machine.
// RevokeByApprover's own mayRevoke check).
func (s *server) revokeByApprover(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body revokeByApproverBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	err := s.deps.Machine.RevokeByApprover(r.Context(), id, body.Assertion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case errors.Is(err, requests.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case isAssertionError(err):
		writeError(w, http.StatusForbidden, "ASSERTION_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "revoked"})
}
