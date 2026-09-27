// packages/envoy/cmd/agent-secrets/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs (the AGENTC-393
// overview contract). Session routes — everything but enrollment issuance/revocation and
// launcher-credential issuance — are signed per call with proof.Sign on a Proof header, exactly
// as internal/broker/proof.Verifier expects; the enrollment and launcher-credential routes carry
// a launcher bearer token instead, and POST/GET /v1/launcher-credentials carry no credential at
// all (a launcher has none yet). Every method that a subcommand may need to print verbatim under
// --json (request, status, self) returns both its decoded result and the exact raw response
// bytes the broker sent, so main.go never re-marshals a Go struct in place of the wire body.
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
	AskRef    *string          `json:"ask"`
}

type createRequestBody struct {
	Secrets   []string `json:"secrets"`
	Reason    string   `json:"reason,omitempty"`
	Issue     string   `json:"issue,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
}

// CreateRequest calls POST /v1/requests and returns both the decoded result and the exact raw
// response bytes, for --json's "print the response body verbatim" requirement.
func (c *client) CreateRequest(ctx context.Context, key *ecdsa.PrivateKey, enrollmentID string, names []string, reason, issue, sessionID string) (RequestResult, []byte, error) {
	body, err := json.Marshal(createRequestBody{Secrets: names, Reason: reason, Issue: issue, SessionID: sessionID})
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
	DecidedAt *time.Time       `json:"decided_at"`
	Decision  *requestDecision `json:"decision"`
	Detail    *string          `json:"detail"`
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

type approverBody struct {
	Kind  string  `json:"kind"`
	Issue *string `json:"issue,omitempty"`
}

// EnrollBody is POST /v1/enrollments's exact request shape.
type EnrollBody struct {
	Kind       string       `json:"kind"`
	RuntimeID  string       `json:"runtime_id"`
	Operator   *string      `json:"operator"`
	Approver   approverBody `json:"approver"`
	Thumbprint string       `json:"thumbprint"`
	SessionID  *string      `json:"session_id,omitempty"`
	PodToken   *string      `json:"pod_token,omitempty"`
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

type requestLauncherCredentialBody struct {
	Operator string  `json:"operator"`
	Host     string  `json:"host"`
	Service  *string `json:"service,omitempty"`
}

// RequestLauncherCredential opens a launcher-credential request and returns its pending id and
// the confirmation code its Dispatch ask shows.
func (c *client) RequestLauncherCredential(ctx context.Context, operator, host string, service *string) (pendingID, code string, err error) {
	body, err := json.Marshal(requestLauncherCredentialBody{Operator: operator, Host: host, Service: service})
	if err != nil {
		return "", "", err
	}
	raw, err := c.doBearer(ctx, "", http.MethodPost, "/v1/launcher-credentials", body)
	if err != nil {
		return "", "", err
	}
	var result struct {
		PendingID        string `json:"pending_id"`
		ConfirmationCode string `json:"confirmation_code"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("decode launcher credential request response: %w", err)
	}
	return result.PendingID, result.ConfirmationCode, nil
}

// ReadLauncherCredential polls a pending launcher-credential request. token is non-empty exactly
// once, on the first read that observes state "issued".
func (c *client) ReadLauncherCredential(ctx context.Context, pendingID string) (state, token string, err error) {
	raw, err := c.doBearer(ctx, "", http.MethodGet, "/v1/launcher-credentials/"+pendingID, nil)
	if err != nil {
		return "", "", err
	}
	var result struct {
		State string  `json:"state"`
		Token *string `json:"token"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("decode launcher credential response: %w", err)
	}
	if result.Token != nil {
		token = *result.Token
	}
	return result.State, token, nil
}
