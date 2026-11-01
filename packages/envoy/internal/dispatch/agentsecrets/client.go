// Package agentsecrets relays Dispatch's credential pages to the secrets broker's named-caller
// API (its account routes: https://sjawhar.github.io/legion/broker/reference/api/). Dispatch lists
// the credential requests waiting on a person, each secret request linking to the broker's own
// page, where that person, signed in to the broker, decides it; it relays a machine login's
// decision with the code the person typed; and it relays a person's revoke of a grant or a
// machine login. Reads and the machine-login lookup carry and return json.RawMessage, so a broker
// read-shape change never requires a Dispatch code change. Every decision (a machine login's
// approve or deny, a revoke) instead carries a Decision Dispatch builds itself: its approver is
// the login Dispatch's own session resolved, so nothing the browser sends can name who decided.
//
// Dispatch authenticates to the broker as itself: every call carries Dispatch's own projected
// Kubernetes ServiceAccount token, which the broker verifies against its cluster issuer and
// matches against BROKER_API_ACCOUNTS. That names the caller and nothing more: the broker takes a
// machine login's approver, and the person a revoke names, from the body, on the word of that
// listed caller. It decides no secret request on that word: it refuses an approve or deny of one
// with 403 DECIDE_ON_BROKER_PAGE carrying the record's approve_url, an *Error the routes relay
// like any other.
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

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// defaultTimeout is the window every broker call gets, matching envoy.Client's default.
const defaultTimeout = 5 * time.Second

// Client relays Dispatch's UI calls to the broker, authenticating each one with the projected
// ServiceAccount token Account reads. A nil *Client (the broker URL is unconfigured) means the
// feature is off; the api's pending list answers null then, and every other api handler 404
// FEATURE_OFF.
type Client struct {
	URL string
	// Account reads Dispatch's own projected Kubernetes ServiceAccount token
	// (DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE), the credential the broker resolves its caller
	// from. The reader is internal/brokertoken's, the same one the token introspection there
	// presents its bearer from, so a projected token's path, its cache and the error for a file
	// Dispatch cannot read are one reading rather than two.
	Account *brokertoken.AccountToken
	HTTP    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every broker call gets.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.HTTP.Timeout = timeout }
}

// New returns a client for the secrets broker's named-caller API. tokenFile names the file
// holding Dispatch's own projected Kubernetes ServiceAccount token, which every call presents:
// the broker reads the calling service's identity from it (BROKER_API_ACCOUNTS) and then takes a
// machine login's approver, and a revoke's, from the login Dispatch sends — resolved by
// Dispatch's own session, not read from an env var here.
func New(baseURL, tokenFile string, options ...Option) *Client {
	client := &Client{
		URL:     strings.TrimSuffix(baseURL, "/"),
		Account: brokertoken.NewAccountToken(tokenFile),
		HTTP:    &http.Client{Timeout: defaultTimeout},
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// Error carries the broker's status and its {"code","error"} body so handlers can forward
// both verbatim to the browser. Only a non-2xx broker response produces one; a call that stopped
// before reaching the broker is an *AccountTokenError when Dispatch could not read its own token,
// and a plain wrapped error for a transport failure (connection refused, timeout, context
// canceled), which the routes answer 503 AGENT_SECRETS_UNAVAILABLE.
type Error struct {
	Status int
	Code   string
	Body   json.RawMessage
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent-secrets: broker returned %d", e.Status)
}

// AccountTokenError is a call that never reached the broker because Dispatch could not read its
// own ServiceAccount token: the file is missing or unreadable, or holds nothing. It is the reader's
// own error type, shared with every other call Dispatch makes to the broker, because what is
// wrong is Dispatch's deployment rather than the broker, and the routes must say so rather than
// send the operator to a broker that was never asked.
type AccountTokenError = brokertoken.AccountTokenError

// do sends a request relaying body (nil for a bodyless GET) and returns the broker's response
// body verbatim, or *Error for a non-2xx response, an *AccountTokenError for a token it could not
// read, which stops the call before it is built, or a plain wrapped error for a transport failure.
func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	token, err := c.Account.Read()
	if err != nil {
		return nil, err
	}
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
	request.Header.Set("Authorization", "Bearer "+token)
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
// human's canonical login as Dispatch's session resolved it, and Code, for a machine login, is the
// typed confirmation code the human entered.
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

// Approve relays a person's approval of a machine login, which carries its typed code. The broker
// refuses one of a secret request, writing nothing, with 403 DECIDE_ON_BROKER_PAGE naming the page
// where a person signed in to the broker decides it.
func (c *Client) Approve(ctx context.Context, recordID string, d Decision) (json.RawMessage, error) {
	body, err := d.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/approve", body)
}

// Deny relays a person's refusal of a machine login, which carries its typed code. The broker
// refuses one of a secret request as it refuses Approve.
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

// MachineLogins lists the machine logins the named person may revoke that can still reach a
// secret: their own machines', and every service's whoever approved it, unexpired or expired with
// a session still running.
func (c *Client) MachineLogins(ctx context.Context, approver string) (json.RawMessage, error) {
	query := url.Values{"approver": {approver}}
	return c.do(ctx, http.MethodGet, "/v1/launcher-credentials?"+query.Encode(), nil)
}

// RevokeMachineLogin relays a person's revoke of a machine login; the broker allows anyone signed
// in for a service's login, and only the person who approved it for a person's machine login.
func (c *Client) RevokeMachineLogin(ctx context.Context, credentialID, approver string) (json.RawMessage, error) {
	body, err := Decision{Approver: approver}.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/launcher-credentials/"+url.PathEscape(credentialID)+"/revoke-by-approver", body)
}
