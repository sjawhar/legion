// Package agentsecrets relays Dispatch's credential-request UI to the secrets broker's
// UI-bearer API (the shared broker contract, dispatch://AGENTC-393/artifact/plan-overview-md,
// Authentication item 3). Reads and the machine-login lookup carry and return
// json.RawMessage, so a broker read-shape change never requires a Dispatch code change. Every
// decision (approve, deny, revoke) instead carries a Decision Dispatch builds itself: its
// approver is the login Dispatch's own session resolved, which the UI bearer vouches for to the
// broker, so nothing the browser sends can name who decided.
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
// broker URL is unconfigured) means the feature is off; the api's pending list answers null then,
// and every other api handler 404 FEATURE_OFF.
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

// New returns a client for the secrets broker's UI-bearer API. token authenticates Dispatch's
// server to the broker, which then trusts the approver login Dispatch sends — it is resolved once
// by Dispatch's boot config, not read from an env var here.
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

// Decision is every approve, deny and revoke body Dispatch sends: Approver is the deciding
// human's canonical login as Dispatch's session resolved it, and Code, for a machine login only,
// is the typed confirmation code the human entered.
type Decision struct {
	Approver string  `json:"approver"`
	Code     *string `json:"code,omitempty"`
}

func (d Decision) body() (json.RawMessage, error) {
	body, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: encode decision: %w", err)
	}
	return body, nil
}

// Approve relays an approver's decision to grant a credential request.
func (c *Client) Approve(ctx context.Context, recordID string, d Decision) (json.RawMessage, error) {
	body, err := d.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/approve", body)
}

// Deny relays an approver's decision to refuse a credential request.
func (c *Client) Deny(ctx context.Context, recordID string, d Decision) (json.RawMessage, error) {
	body, err := d.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/deny", body)
}

// MachineLookup resolves a pending machine login by the code its request body names.
func (c *Client) MachineLookup(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/machine-logins/lookup", body)
}

// Grants lists the live grants of the named person's sessions, automatic or approved, and the
// grants the person approved on anyone's session.
func (c *Client) Grants(ctx context.Context, approver string) (json.RawMessage, error) {
	query := url.Values{"approver": {approver}}
	return c.do(ctx, http.MethodGet, "/v1/grants?"+query.Encode(), nil)
}

// RevokeByApprover relays an approver's or operator's revocation of a grant; the broker decides
// whether approver may revoke it.
func (c *Client) RevokeByApprover(ctx context.Context, grantID, approver string) (json.RawMessage, error) {
	body, err := Decision{Approver: approver}.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/grants/"+url.PathEscape(grantID)+"/revoke-by-approver", body)
}
