// packages/envoy/cmd/agent-secrets/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs, as the shared
// broker contract defines them. Session routes
// — everything but enrollment issuance/revocation and launcher-credential issuance — are signed
// per call through a Signer (proofsource.go: an agent box or pod's own key, or a host session's
// agent-secrets-helper), producing exactly the Proof header internal/broker/proof.Verifier
// expects. CreateRequest additionally needs a signed credential-request object (record.Sign)
// embedded in its body, since the shared broker contract carries authorization_details and
// reason inside that signed object rather than as plain top-level JSON fields; building it is
// also a Signer responsibility (Signer.SignRequestObject), since a host session's key never
// leaves agent-secrets-helper.
// Enrollment issuance/revocation and launcher-credential issuance are Plan B/C helper-socket
// operations now (see cmdEnrollHelper/cmdUnenrollHelper/cmdLauncher in main.go and
// internal/broker/helper): no route here ever carries a bearer launcher token, and POST/GET
// /v1/launcher-credentials still carry no credential at all (a launcher has none yet). Every
// method that a subcommand may need to print verbatim under --json (request, status, self)
// returns both its decoded result and the exact raw response bytes, so main.go never re-marshals
// a Go struct in place of the wire body.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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

// doProof signs the request with signer.Sign against exactly this method and URL (no query
// string appended after signing), the shape proof.Verifier.Verify compares htm/htu against.
func (c *client) doProof(ctx context.Context, signer Signer, method, path string, body []byte) ([]byte, error) {
	url := c.baseURL + path
	token, err := signer.Sign(method, url)
	if err != nil {
		return nil, fmt.Errorf("sign proof: %w", err)
	}
	return c.do(ctx, method, url, body, map[string]string{"Proof": token})
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

// createRequestBody is POST /v1/requests's exact shape in the shared broker contract: a signed
// request object plus an optional, unsigned session_id (wake-only). The v8 top-level
// "secrets"/"reason"/"issue" fields are gone — they live inside the signed request object instead.
type createRequestBody struct {
	Request   string  `json:"request"`
	SessionID *string `json:"session_id"`
}

// CreateRequest asks signer to build and sign a credential-request object naming one
// agent_secret authorization_detail per requested name (Signer.SignRequestObject, audience ==
// the broker's own base URL), then calls POST /v1/requests with that object nested in the body
// and the outer call still authenticated with a session Proof header. It returns both the
// decoded result and the exact raw response bytes, for --json's "print the response body
// verbatim" requirement.
func (c *client) CreateRequest(ctx context.Context, signer Signer, names []string, reason, sessionID string) (RequestResult, []byte, error) {
	requestObject, err := signer.SignRequestObject(c.baseURL, names, reason)
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
	raw, err := c.doProof(ctx, signer, http.MethodPost, "/v1/requests", body)
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

func (c *client) GetRequest(ctx context.Context, signer Signer, id string) (RequestStatus, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/requests/"+id, nil)
	if err != nil {
		return RequestStatus{}, nil, err
	}
	var status RequestStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return RequestStatus{}, nil, fmt.Errorf("decode request status: %w", err)
	}
	return status, raw, nil
}

func (c *client) CancelRequest(ctx context.Context, signer Signer, id string) error {
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/requests/"+id+"/cancel", nil)
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

func (c *client) GrantValues(ctx context.Context, signer Signer, grantID string) (GrantValues, error) {
	raw, err := c.doProof(ctx, signer, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if err != nil {
		return GrantValues{}, err
	}
	var values GrantValues
	if err := json.Unmarshal(raw, &values); err != nil {
		return GrantValues{}, fmt.Errorf("decode grant values: %w", err)
	}
	return values, nil
}

func (c *client) RevokeGrant(ctx context.Context, signer Signer, grantID string) error {
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil)
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

func (c *client) Self(ctx context.Context, signer Signer) (SelfResult, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/enrollments/self", nil)
	if err != nil {
		return SelfResult{}, nil, err
	}
	var self SelfResult
	if err := json.Unmarshal(raw, &self); err != nil {
		return SelfResult{}, nil, fmt.Errorf("decode self: %w", err)
	}
	return self, raw, nil
}

// RenewEnrollment is the one Signer-taking method that still needs enrollmentID explicitly: the
// broker route is /v1/enrollments/{id}/renew, so the id is in the URL path, not just the signed
// payload.
func (c *client) RenewEnrollment(ctx context.Context, signer Signer, enrollmentID string) (time.Time, error) {
	raw, err := c.doProof(ctx, signer, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", nil)
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
