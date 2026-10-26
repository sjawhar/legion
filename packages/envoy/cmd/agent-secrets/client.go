// packages/envoy/cmd/agent-secrets/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs, as the broker's
// HTTP API reference (https://sjawhar.github.io/legion/broker/reference/api/) defines them. Session routes
// — everything but enrollment issuance/revocation and launcher-credential issuance — are signed
// per call through a Signer (proofsource.go: an agent box or pod's own key, or a machine session's
// agent-secrets-helper), producing exactly the Proof header internal/broker/proof.Verifier
// expects. CreateRequest additionally needs a signed credential-request object (record.Sign)
// embedded in its body, since the broker carries authorization_details and
// reason inside that signed object rather than as plain top-level JSON fields; building it is
// also a Signer responsibility (Signer.SignRequestObject), since a machine session's key never
// leaves agent-secrets-helper.
// Enrollment issuance/revocation and launcher-credential issuance are helper-socket operations
// (see cmdEnrollHelper/cmdUnenrollHelper in enroll.go, cmdMachineLogin in machine.go, and
// internal/broker/helper): no route here ever carries a bearer launcher token, and the helper's
// machine login (an OAuth device grant) carries no launcher credential (a launcher has none yet). The
// operator routes (/v1/operator/*) are signed with a launcher proof the helper's sign-launcher op
// makes (launcherSigner). Every method that a subcommand may need to print verbatim under --json
// (request, status, self, the operator lists) returns both its decoded result and the exact raw
// response bytes, so no command re-marshals a Go struct in place of the wire body.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

type client struct {
	baseURL string
	http    *http.Client
}

// terminalRenewal is the closed set of renewal refusals `agent-secrets renew` stops on, exiting
// exitRenewRefused (76) so the worker shim does not restart it; every other refusal keeps the retry
// backoff. Each member is an answer no retry can change: POD_TOKEN_REQUIRED is a pod enrollment's
// renewal that carried no token, and ENROLLMENT_NOT_LIVE an enrollment that has ended, lapsed or
// never existed. A broker that predates ENROLLMENT_NOT_LIVE answers an ended enrollment
// PROOF_INVALID with a message saying "not live" (its renew handler's "enrollment is not live" and
// its proof check's "proof invalid: not live"), which no recoverable proof failure says.
// POD_IDENTITY_MISMATCH is not a member: the broker also answers it for a token it could not
// verify while its issuer or key set was unreachable, and a real mismatch ends with the lease,
// after which the next renewal is ENROLLMENT_NOT_LIVE.
var terminalRenewal = struct {
	Codes                     []string
	LegacyCode, LegacyMessage string
}{
	Codes:         []string{"POD_TOKEN_REQUIRED", "ENROLLMENT_NOT_LIVE"},
	LegacyCode:    "PROOF_INVALID",
	LegacyMessage: "not live",
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

// Permanent reports whether this refusal is one of terminalRenewal's: a code it lists, or its
// legacy code with a message naming the ended enrollment.
func (e *apiError) Permanent() bool {
	if slices.Contains(terminalRenewal.Codes, e.Code) {
		return true
	}
	return e.Code == terminalRenewal.LegacyCode && strings.Contains(e.Message, terminalRenewal.LegacyMessage)
}

// unreachableError is a broker call that did not complete. Its host might not resolve, nothing
// might answer at the address, or a peer might answer the connection but fail the exchange. It is
// not a broker refusal - the broker sent no usable response - and it reads as what it is, naming
// the address tried and the variable the address came from, because a session that inherited a
// stale AGENT_SECRETS_URL otherwise reads a bare DNS error as a broker that is down and the stale
// value goes unnoticed.
type unreachableError struct {
	// what classifies the failure: a failed name resolution, no connection made, nothing answered
	// in time, a peer that answered but failed the exchange, or a request that failed in a way
	// none of those names.
	what string
	// base is the broker's address as AGENT_SECRETS_URL gave it, method and url the call tried.
	base, method, url string
	// cause is the transport's own error, with net/http's repetition of the URL unwrapped away.
	cause error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("%s: %s %s (%s names %s): %v", e.what, e.method, e.url, brokerURLVar, e.base, e.cause)
}

func (e *unreachableError) Unwrap() error { return e.cause }

// unreachable classifies a failed HTTP exchange, sending its reader to the layer that failed.
// Any resolver error is name resolution, before the generic timeout branch. A dial that made no
// connection (refused, network or host unreachable, any other dial error) is a non-answer, which
// routing or a listener fixes. Only what proves a peer answered - a TLS record or certificate
// error, EOF, or a reset once the connection existed - is "answered but the exchange failed",
// which TLS or a proxy fixes. An error that fits none says no more than that the request failed,
// rather than name a layer it cannot prove.
func unreachable(base, method, callURL string, err error) error {
	cause := err
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		cause = wrapped.Err
	}
	host := brokerHost(base)
	failed := func(what string) error { return &unreachableError{what, base, method, callURL, cause} }
	var dns *net.DNSError
	var timeout net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return failed("the broker's host " + host + " does not resolve")
	case errors.As(err, &dns):
		return failed("the broker's host " + host + " name resolution failed")
	case errors.Is(err, syscall.ECONNREFUSED):
		return failed("nothing answered at " + host + ": the connection was refused")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return failed("the broker at " + host + " did not answer in time")
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH),
		errors.As(err, &op) && op.Op == "dial":
		return failed("nothing answered at " + host + ": no connection could be made")
	case peerAnswered(err):
		return failed("the broker at " + host + " answered but the exchange failed")
	default:
		return failed("the request to the broker at " + host + " failed")
	}
}

// peerAnswered reports whether err shows a peer took the connection and then broke the exchange:
// a TLS record that is not TLS (an HTTP server answering an https URL), a TLS alert or certificate
// it would not trust, or the stream ending or being reset once it existed.
func peerAnswered(err error) bool {
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
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
		return nil, unreachable(c.baseURL, method, url, err)
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
func (c *client) doProof(ctx context.Context, signer proofSigner, method, path string, body []byte) ([]byte, error) {
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
}

// RequestResult is POST /v1/requests's exact response shape. LifetimeSeconds is the lifetime the
// broker gave the request's grant, nil for until its session ends; a broker from before
// lifetime_seconds sends no such field, and it decodes as nil.
type RequestResult struct {
	RequestID       string           `json:"request_id"`
	State           string           `json:"state"`
	Secrets         []SecretDecision `json:"secrets"`
	GrantID         *string          `json:"grant_id"`
	RecordID        *string          `json:"record_id"`
	Approver        *string          `json:"approver"`
	LifetimeSeconds *int             `json:"lifetime_seconds"`
	Coalesced       bool             `json:"coalesced,omitempty"`
}

// createRequestBody is POST /v1/requests's exact shape in the broker's HTTP API: a signed
// request object plus an optional, unsigned session_id (wake-only). The v8 top-level
// "secrets"/"reason"/"issue" fields are gone — they live inside the signed request object instead.
type createRequestBody struct {
	Request   string  `json:"request"`
	SessionID *string `json:"session_id"`
}

// CreateRequest builds and signs a credential-request object carrying one
// agent_secret authorization_detail per requested name and lifetimeSeconds, the grant's lifetime
// (nil for until the session ends) (Signer.SignRequestObject, audience == the broker's own base
// URL), then calls POST /v1/requests with that object nested in the body and the outer call still
// authenticated with a session Proof header. It returns both the decoded result and the exact raw
// response bytes, for --json's "print the response body verbatim" requirement.
func (c *client) CreateRequest(ctx context.Context, signer Signer, names []string, reason string, lifetimeSeconds *int, sessionID string) (RequestResult, []byte, error) {
	requestObject, err := signer.SignRequestObject(c.baseURL, names, reason, lifetimeSeconds)
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
	Approver  *string          `json:"approver"`
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

// GrantValues is POST /v1/grants/{id}/values's exact response shape.
type GrantValues struct {
	Values    map[string]string `json:"values"`
	ExpiresAt *time.Time        `json:"expires_at"`
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
	GrantID   string     `json:"grant_id"`
	RequestID string     `json:"request_id"`
	Approver  *string    `json:"approver"`
	ExpiresAt *time.Time `json:"expires_at"`
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
// payload. A pod's renewal carries its current projected token as pod_token; podToken "" sends no
// body at all, a box's renewal.
func (c *client) RenewEnrollment(ctx context.Context, signer Signer, enrollmentID, podToken string) (time.Time, error) {
	var body []byte
	if podToken != "" {
		var err error
		if body, err = json.Marshal(map[string]string{"pod_token": podToken}); err != nil {
			return time.Time{}, err
		}
	}
	raw, err := c.doProof(ctx, signer, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", body)
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

// --- GET /v1/settings, POST /v1/secrets/{name}/reread (public: no proof) ---

// Settings is GET /v1/settings's exact response shape: the store and namespace every agent secret
// lives under and, per store, the key they are encrypted with and where they are. A broker from
// before the store field answers none, and holds them in AWS Secrets Manager.
type Settings struct {
	Store         string `json:"store,omitempty"`
	SecretsPrefix string `json:"secrets_prefix"`
	KMSKeyARN     string `json:"kms_key_arn,omitempty"`
	AWSAccountID  string `json:"aws_account_id,omitempty"`
	AWSRegion     string `json:"aws_region,omitempty"`
	KMSKeyName    string `json:"kms_key_name,omitempty"`
	GCPProject    string `json:"gcp_project,omitempty"`
}

func (c *client) Settings(ctx context.Context) (Settings, error) {
	raw, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/settings", nil, nil)
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode settings: %w", err)
	}
	return settings, nil
}

// Reread is POST /v1/secrets/{name}/reread's exact response shape: whether the broker serves the
// secret now, as which kind ("" for a stored secret, and from a broker older than the field), and,
// when it does not serve it, why.
type Reread struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

// RereadSecret asks the broker to read the secret name from Secrets Manager now, as whoever just
// wrote it does, so the write is served (or gone) at once instead of at its next reload. name is a
// validated agent secret name, so it needs no escaping in the path.
func (c *client) RereadSecret(ctx context.Context, name string) (Reread, error) {
	raw, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/secrets/"+name+"/reread", nil, nil)
	if err != nil {
		return Reread{}, err
	}
	var reread Reread
	if err := json.Unmarshal(raw, &reread); err != nil {
		return Reread{}, fmt.Errorf("decode reread: %w", err)
	}
	return reread, nil
}

// --- GET /v1/operator/machines, POST /v1/operator/machines/{id}/revoke,
// GET /v1/operator/grants, POST /v1/operator/grants/{id}/revoke (launcher proof) ---

// OperatorMachine mirrors one machine login in GET /v1/operator/machines, the broker's
// launcherCredentialResp.
type OperatorMachine struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	ApprovedBy   string    `json:"approved_by"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Expired      bool      `json:"expired"`
}

// OperatorMachines is GET /v1/operator/machines's exact response shape.
type OperatorMachines struct {
	Credentials []OperatorMachine `json:"credentials"`
}

// OperatorMachines lists the calling machine login's operator's own machines' logins, as
// Dispatch's machine-login page lists them but without any service's login, with the raw body
// for --json.
func (c *client) OperatorMachines(ctx context.Context, signer proofSigner) (OperatorMachines, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/machines", nil)
	if err != nil {
		return OperatorMachines{}, nil, err
	}
	var out OperatorMachines
	if err := json.Unmarshal(raw, &out); err != nil {
		return OperatorMachines{}, nil, fmt.Errorf("decode machine logins: %w", err)
	}
	return out, raw, nil
}

// RevokeOperatorMachine ends one of the calling machine login's operator's own machines' logins.
// id goes into the path escaped, so whatever a person types names one path segment.
func (c *client) RevokeOperatorMachine(ctx context.Context, signer proofSigner, id string) error {
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/machines/"+url.PathEscape(id)+"/revoke", nil)
	return err
}

// OperatorGrantEnrollment mirrors a grant's session, the broker's recordEnrollmentResp.
type OperatorGrantEnrollment struct {
	Kind      string  `json:"kind"`
	RuntimeID string  `json:"runtime_id"`
	Operator  string  `json:"operator"`
	Slot      *string `json:"slot"`
}

// OperatorGrant mirrors one grant in GET /v1/operator/grants, the broker's approverGrantResp.
type OperatorGrant struct {
	GrantID    string                  `json:"grant_id"`
	Granted    string                  `json:"granted"`
	RecordID   *string                 `json:"record_id"`
	Enrollment OperatorGrantEnrollment `json:"enrollment"`
	Names      []string                `json:"names"`
	Approver   *string                 `json:"approver"`
	ExpiresAt  *time.Time              `json:"expires_at"`
	CreatedAt  time.Time               `json:"created_at"`
}

// OperatorGrants is GET /v1/operator/grants's exact response shape.
type OperatorGrants struct {
	Grants []OperatorGrant `json:"grants"`
}

// OperatorGrants lists the live grants of the calling machine login's operator's sessions and those
// the operator approved, as Dispatch's Live grants page lists them, with the raw body for --json.
func (c *client) OperatorGrants(ctx context.Context, signer proofSigner) (OperatorGrants, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/grants", nil)
	if err != nil {
		return OperatorGrants{}, nil, err
	}
	var out OperatorGrants
	if err := json.Unmarshal(raw, &out); err != nil {
		return OperatorGrants{}, nil, fmt.Errorf("decode grants: %w", err)
	}
	return out, raw, nil
}

// RevokeOperatorGrant ends grant id as the calling machine login's operator. id goes into the path
// escaped, so whatever a person types names one path segment.
func (c *client) RevokeOperatorGrant(ctx context.Context, signer proofSigner, id string) error {
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/grants/"+url.PathEscape(id)+"/revoke", nil)
	return err
}
