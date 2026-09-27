// Package agentsecrets relays Dispatch's credential-request UI to the secrets broker's
// UI-bearer API (contract v9 §3.3). Dispatch never models the broker's request/response
// bodies — every call carries and returns json.RawMessage — so a broker schema change never
// requires a Dispatch code change; the browser and the broker agree on the shape.
package agentsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultTimeout is the window every broker call gets, matching envoy.Client's default.
const defaultTimeout = 5 * time.Second

// Client relays Dispatch's UI calls to the broker with the UI bearer. A nil *Client (the
// broker URL is unconfigured) means the feature is off; every api handler answers 404
// FEATURE_OFF then.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every broker call gets.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.HTTP.Timeout = timeout }
}

// New returns a client for the secrets broker's UI-bearer API. token authenticates *which
// service is relaying* (Dispatch), not who approves — it is resolved once by Dispatch's boot
// config, not read from an env var here.
func New(baseURL, token string, options ...Option) *Client {
	client := &Client{
		URL:   strings.TrimSuffix(baseURL, "/"),
		Token: token,
		HTTP:  &http.Client{Timeout: defaultTimeout},
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// Error carries the broker's status and its {"code","error"} body so handlers can forward
// both verbatim to the browser. Only a non-2xx broker response produces one; a transport
// failure (connection refused, timeout, context canceled) is a plain wrapped error instead —
// Task 2's routes treat anything that is not *Error as 503 AGENT_SECRETS_UNAVAILABLE.
type Error struct {
	Status int
	Code   string
	Body   json.RawMessage
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent-secrets: broker returned %d", e.Status)
}

func (c *Client) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+c.Token)
}

// do sends a request relaying body (nil for a bodyless GET) and returns the broker's response
// body verbatim, or *Error for a non-2xx response, or a plain wrapped error for a transport
// failure.
func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: build %s %s request: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: read %s %s response: %w", method, path, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		brokerErr := &Error{Status: response.StatusCode, Body: responseBody}
		var decoded struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(responseBody, &decoded); err == nil {
			brokerErr.Code = decoded.Code
		}
		return nil, brokerErr
	}
	return json.RawMessage(responseBody), nil
}

// Pending lists the credential requests awaiting the named approver's decision.
func (c *Client) Pending(ctx context.Context, approver string) (json.RawMessage, error) {
	query := url.Values{"approver": {approver}}
	return c.do(ctx, http.MethodGet, "/v1/pending?"+query.Encode(), nil)
}

// Record returns one credential request by id.
func (c *Client) Record(ctx context.Context, recordID string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/credential-requests/"+url.PathEscape(recordID), nil)
}

// Approve relays an approver's decision to grant a credential request.
func (c *Client) Approve(ctx context.Context, recordID string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/approve", body)
}

// Deny relays an approver's decision to refuse a credential request.
func (c *Client) Deny(ctx context.Context, recordID string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/deny", body)
}

// MachineLookup resolves the approver login associated with a machine login attempt.
func (c *Client) MachineLookup(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/machine-logins/lookup", body)
}

// Keys lists the named approver's registered keys.
func (c *Client) Keys(ctx context.Context, login string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/approvers/"+url.PathEscape(login)+"/keys", nil)
}

// Ceremony drives one step of a key registration or endorsement ceremony for the named
// approver. kind is "register" or "endorse"; step is "begin" or "finish". This method does not
// itself validate kind/step — Task 2's route table restricts to those four combinations before
// calling it, exactly like it restricts every other route pattern.
func (c *Client) Ceremony(ctx context.Context, login, kind, step string, body json.RawMessage) (json.RawMessage, error) {
	path := "/v1/approvers/" + url.PathEscape(login) + "/keys/" + url.PathEscape(kind) + "/" + url.PathEscape(step)
	return c.do(ctx, http.MethodPost, path, body)
}

// Grants lists the credential grants the named approver issued.
func (c *Client) Grants(ctx context.Context, approver string) (json.RawMessage, error) {
	query := url.Values{"approver": {approver}}
	return c.do(ctx, http.MethodGet, "/v1/grants?"+query.Encode(), nil)
}

// RevokeByApprover relays an approver's revocation of a grant they issued.
func (c *Client) RevokeByApprover(ctx context.Context, grantID string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/grants/"+url.PathEscape(grantID)+"/revoke-by-approver", body)
}
