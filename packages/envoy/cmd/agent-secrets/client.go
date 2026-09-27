// packages/envoy/cmd/agent-secrets/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs (contract v9,
// the AGENTC-393 overview document). Session routes — everything but enrollment issuance/
// revocation and launcher-credential issuance — are signed per call with proof.Sign on a Proof
// header, exactly as internal/broker/proof.Verifier expects; CreateRequest and
// RequestLauncherCredential additionally build and sign a credential-request object
// (record.Sign) that carries the requested secrets or launcher identifier inside the request
// body itself, since contract v9 moved authorization_details, reason, and (for a machine login)
// login_hint out of plain top-level JSON fields and into that signed object. POST/GET
// /v1/launcher-credentials still carry no session or launcher credential at all (a launcher has
// none yet); no route ever returns a bearer launcher token — contract v9 issues launcher
// credentials keyed on the caller's own signing key instead. Every method that a subcommand may
// need to print verbatim under --json (request, status, self) returns both its decoded result
// and the exact raw response bytes, so main.go never re-marshals a Go struct in place of the
// wire body.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string) *client {
	return &client{baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}}
}

// apiError is the broker's {"code":...,"error":...} error envelope, plus the HTTP status it came
// with.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// do sends the request and, on a 2xx response, returns its raw body; any other status decodes
// the broker's error envelope into an *apiError.
func (c *client) do(ctx context.Context, method, url string, body []byte, headers map[string]string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, url, err)
	}
	if resp.StatusCode/100 != 2 {
		var envelope struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		if envelope.Error == "" {
			envelope.Error = string(data)
		}
		return nil, &apiError{Status: resp.StatusCode, Code: envelope.Code, Message: envelope.Error}
	}
	return data, nil
}

// doProof signs the request with proof.Sign against exactly this method and URL (no query
// string appended after signing), the shape proof.Verifier.Verify compares htm/htu against.
func (c *client) doProof(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID, method, path string, body []byte) ([]byte, error) {
	url := c.baseURL + path
	token, err := proof.Sign(key, enrollmentID, method, url, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign proof: %w", err)
	}
	return c.do(ctx, method, url, body, map[string]string{"Proof": token})
}

func (c *client) doBearer(ctx context.Context, bearer, method, path string, body []byte) ([]byte, error) {
	headers := map[string]string{}
	if bearer != "" {
		headers["Authorization"] = "Bearer " + bearer
	}
	return c.do(ctx, method, c.baseURL+path, body, headers)
}

// --- POST /v1/requests, GET /v1/requests/{id}, POST /v1/requests/{id}/cancel ---

// SecretDecision mirrors requests.SecretDecision's wire shape.
type SecretDecision struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
	Delivery string `json:"delivery"`
}

// RequestResult is POST /v1/requests's exact response shape.
type RequestResult struct {
	RequestID string           `json:"request_id"`
	State     string           `json:"state"`
	Secrets   []SecretDecision `json:"secrets"`
	GrantID   *string          `json:"grant_id"`
	RecordID  *string          `json:"record_id"`
	Coalesced bool             `json:"coalesced,omitempty"`
}

// createRequestBody is POST /v1/requests's exact contract v9 shape: a signed request object plus
// an optional, unsigned session_id (wake-only). The v8 top-level "secrets"/"reason"/"issue"
// fields are gone — they live inside the signed request object instead.
type createRequestBody struct {
	Request   string  `json:"request"`
	SessionID *string `json:"session_id"`
}

// CreateRequest builds and signs a credential-request object naming one agent_secret
// authorization_detail per requested name (record.Sign, audience == the broker's own base URL),
// then calls POST /v1/requests with that object nested in the body and the outer call still
// authenticated with a session Proof header. It returns both the decoded result and the exact
// raw response bytes, for --json's "print the response body verbatim" requirement.
func (c *client) CreateRequest(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID string, names []string, reason, sessionID string) (RequestResult, []byte, error) {
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	requestObject, err := record.Sign(key, c.baseURL, details, reason, "", time.Now())
	if err != nil {
		return RequestResult{}, nil, fmt.Errorf("sign request object: %w", err)
	}
	var sessionIDPtr *string
	if sessionID != "" {
		sessionIDPtr = &sessionID
	}
	body, err := json.Marshal(createRequestBody{Request: requestObject, SessionID: sessionIDPtr})
	if err != nil {
		return RequestResult{}, nil, err
	}
	raw, err := c.doProof(ctx, key, enrollmentID, http.MethodPost, "/v1/requests", body)
	if err != nil {
		return RequestResult{}, nil, err
	}
	var result RequestResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return RequestResult{}, nil, fmt.Errorf("decode create request response: %w", err)
	}
	return result, raw, nil
}

type requestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// RequestStatus is GET /v1/requests/{id}'s exact response shape.
type RequestStatus struct {
	State     string           `json:"state"`
	GrantID   *string          `json:"grant_id"`
	RecordID  *string          `json:"record_id"`
	DecidedAt *time.Time       `json:"decided_at"`
	Decision  *requestDecision `json:"decision"`
}

func (c *client) GetRequest(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID, id string) (RequestStatus, []byte, error) {
	raw, err := c.doProof(ctx, key, enrollmentID, http.MethodGet, "/v1/requests/"+id, nil)
	if err != nil {
		return RequestStatus{}, nil, err
	}
	var status RequestStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return RequestStatus{}, nil, fmt.Errorf("decode request status: %w", err)
	}
	return status, raw, nil
}

func (c *client) CancelRequest(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID, id string) error {
	_, err := c.doProof(ctx, key, enrollmentID, http.MethodPost, "/v1/requests/"+id+"/cancel", nil)
	return err
}

// --- POST /v1/grants/{id}/values, POST /v1/grants/{id}/revoke ---

// GrantValues is POST /v1/grants/{id}/values's exact response shape. No proxy-delivery secrets
// exist yet, so ProxyOnly is always empty in practice today; it is still decoded (never
// dropped) so cmdExec can refuse to exec rather than silently omit a proxy-delivered name from
// the child's environment.
type GrantValues struct {
	Values    map[string]string `json:"values"`
	ExpiresAt time.Time         `json:"expires_at"`
	ProxyOnly []string          `json:"proxy_only"`
}

func (c *client) GrantValues(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID, grantID string) (GrantValues, error) {
	raw, err := c.doProof(ctx, key, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if err != nil {
		return GrantValues{}, err
	}
	var values GrantValues
	if err := json.Unmarshal(raw, &values); err != nil {
		return GrantValues{}, fmt.Errorf("decode grant values: %w", err)
	}
	return values, nil
}

func (c *client) RevokeGrant(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID, grantID string) error {
	_, err := c.doProof(ctx, key, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil)
	return err
}

// --- GET /v1/enrollments/self, POST /v1/enrollments/{id}/renew ---

// Grant mirrors requests.Grant's wire shape.
type Grant struct {
	GrantID   string    `json:"grant_id"`
	RequestID string    `json:"request_id"`
	Approver  *string   `json:"approver"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SelfResult is GET /v1/enrollments/self's exact response shape.
type SelfResult struct {
	EnrollmentID   string    `json:"enrollment_id"`
	Kind           string    `json:"kind"`
	Operator       *string   `json:"operator"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	Grants         []Grant   `json:"grants"`
}

func (c *client) Self(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID string) (SelfResult, []byte, error) {
	raw, err := c.doProof(ctx, key, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
	if err != nil {
		return SelfResult{}, nil, err
	}
	var self SelfResult
	if err := json.Unmarshal(raw, &self); err != nil {
		return SelfResult{}, nil, fmt.Errorf("decode self: %w", err)
	}
	return self, raw, nil
}

func (c *client) RenewEnrollment(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID string) (time.Time, error) {
	raw, err := c.doProof(ctx, key, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", nil)
	if err != nil {
		return time.Time{}, err
	}
	var result struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return time.Time{}, fmt.Errorf("decode renew response: %w", err)
	}
	return result.LeaseExpiresAt, nil
}

// --- POST /v1/enrollments, DELETE /v1/enrollments/{id} (launcher bearer) ---

// EnrollBody is POST /v1/enrollments's exact contract v9 request shape: the v8 "approver" field
// is gone — the rules pick a request's approver at request time, never at enrollment.
type EnrollBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Operator   *string `json:"operator"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id,omitempty"`
	PodToken   *string `json:"pod_token,omitempty"`
}

// EnrollResult is POST /v1/enrollments's response shape.
type EnrollResult struct {
	EnrollmentID   string    `json:"enrollment_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func (c *client) CreateEnrollment(ctx context.Context, launcherToken string, in EnrollBody) (EnrollResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return EnrollResult{}, err
	}
	raw, err := c.doBearer(ctx, launcherToken, http.MethodPost, "/v1/enrollments", body)
	if err != nil {
		return EnrollResult{}, err
	}
	var result EnrollResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return EnrollResult{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	return result, nil
}

func (c *client) DeleteEnrollment(ctx context.Context, launcherToken, enrollmentID string) error {
	_, err := c.doBearer(ctx, launcherToken, http.MethodDelete, "/v1/enrollments/"+enrollmentID, nil)
	return err
}

// --- POST /v1/launcher-credentials, GET /v1/launcher-credentials/{pending} (no auth at all) ---

// requestLauncherCredentialBody is POST /v1/launcher-credentials's exact contract v9 shape: a
// signed request object naming login_hint (the approving operator) and one launcher_credential
// detail. The v8 plain "operator"/"host"/"service" fields are gone.
type requestLauncherCredentialBody struct {
	Request string `json:"request"`
}

// RequestLauncherCredential builds and signs a credential-request object naming one
// launcher_credential authorization_detail for host (with service set for a service credential)
// and login_hint set to operator (required for a machine login per contract v9: the broker has
// no other way to know which operator's approval it needs), then opens the launcher-credential
// request and returns its pending id and the confirmation code its Dispatch ask shows. key is
// the credential-to-be's own signing key — its thumbprint becomes the launcher credential's
// pinned identity — and audience is the broker's own base URL, matching BROKER_PUBLIC_URL.
func (c *client) RequestLauncherCredential(ctx context.Context, key *ecdsa.PrivateKey, audience, operator, host string, service *string) (pendingID, code string, err error) {
	detail := record.AuthorizationDetail{Type: "launcher_credential", Identifier: host}
	if service != nil {
		detail.Service = *service
	}
	requestObject, err := record.Sign(key, audience, []record.AuthorizationDetail{detail}, "", operator, time.Now())
	if err != nil {
		return "", "", fmt.Errorf("sign request object: %w", err)
	}
	body, err := json.Marshal(requestLauncherCredentialBody{Request: requestObject})
	if err != nil {
		return "", "", err
	}
	raw, err := c.doBearer(ctx, "", http.MethodPost, "/v1/launcher-credentials", body)
	if err != nil {
		return "", "", err
	}
	var result struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("decode launcher credential request response: %w", err)
	}
	return result.PendingID, result.Code, nil
}

// ReadLauncherCredential polls a pending launcher-credential request. Contract v9: no token is
// ever returned — the minted credential is usable only with proofs signed by the key the request
// object embedded (proof.SignLauncher). credentialID names the launcher credential once state is
// "issued"; every other state carries nothing beyond state itself.
func (c *client) ReadLauncherCredential(ctx context.Context, pendingID string) (state, credentialID string, err error) {
	raw, err := c.doBearer(ctx, "", http.MethodGet, "/v1/launcher-credentials/"+pendingID, nil)
	if err != nil {
		return "", "", err
	}
	var result struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("decode launcher credential response: %w", err)
	}
	if result.CredentialID != nil {
		credentialID = *result.CredentialID
	}
	return result.State, credentialID, nil
}
