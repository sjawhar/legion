// Package agentsecrets relays Dispatch's credential-request UI to the secrets broker's
// named-caller API (its UI routes: https://sjawhar.github.io/legion/broker/reference/api/). Reads
// and the machine-login lookup carry and return
// json.RawMessage, so a broker read-shape change never requires a Dispatch code change. Every
// decision (approval, deny, revoke) instead carries a Decision Dispatch builds itself: its
// approver is the login Dispatch's own session resolved, so nothing the browser sends can name
// who decided.
//
// Dispatch authenticates to the broker as itself: every call carries Dispatch's own projected
// Kubernetes ServiceAccount token, which the broker verifies against its cluster issuer and
// matches against BROKER_API_ACCOUNTS. That names the caller and nothing more, which is why an
// approval is no longer a call that decides: Approval asks the broker to open one, the broker
// answers its own page, and the person approves there with their own Google sign-in, which
// Dispatch neither sees nor could produce. A deny and a revoke still travel on this credential
// alone, since either can only take access away.
package agentsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultTimeout is the window every broker call gets, matching envoy.Client's default.
const defaultTimeout = 5 * time.Second

// tokenCacheTTL is how long a Client reuses the token bytes it last read. Opening the file on
// every call would cost a read per relayed click for a credential that changes a few times a
// day: the kubelet rewrites a projected token in place well before it expires, and the broker
// refuses one that lives longer than an hour from its issue, so bytes this fresh are never near
// expiry and a rewrite reaches the broker within this window of it.
const tokenCacheTTL = 30 * time.Second

// Client relays Dispatch's UI calls to the broker, authenticating each one with the projected
// ServiceAccount token TokenFile holds. A nil *Client (the broker URL is unconfigured) means the
// feature is off; the api's pending list answers null then, and every other api handler 404
// FEATURE_OFF.
type Client struct {
	URL string
	// TokenFile is the path of Dispatch's own projected Kubernetes ServiceAccount token
	// (DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE), the credential the broker resolves its caller
	// from. A projected token rotates, so the path is read at call time, never once at boot.
	TokenFile string
	HTTP      *http.Client
	// tokenTTL is how long token reuses what it read, tokenCacheTTL for every client New builds;
	// the in-package test that proves a rewritten file is picked up lapses the cache by zeroing
	// it. tokenMu guards cached and readAt.
	tokenTTL time.Duration
	tokenMu  sync.Mutex
	cached   string
	readAt   time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every broker call gets.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.HTTP.Timeout = timeout }
}

// New returns a client for the secrets broker's named-caller API. tokenFile names the file
// holding Dispatch's own projected Kubernetes ServiceAccount token, which every call presents:
// the broker reads the calling service's identity from it (BROKER_API_ACCOUNTS) and then trusts
// the approver login Dispatch sends — resolved by Dispatch's own session, not read from an env
// var here.
func New(baseURL, tokenFile string, options ...Option) *Client {
	client := &Client{
		URL:       strings.TrimSuffix(baseURL, "/"),
		TokenFile: tokenFile,
		HTTP:      &http.Client{Timeout: defaultTimeout},
		tokenTTL:  tokenCacheTTL,
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

// AccountTokenError is a call that never reached the broker because Dispatch could not read
// its own ServiceAccount token from Path: the file is missing or unreadable, or holds nothing.
// It is its own type because what is wrong is Dispatch's deployment, not the broker, and the
// routes must say so rather than send the operator to a broker that was never asked.
type AccountTokenError struct {
	// Path is the token file Dispatch tried to read (DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE).
	Path string
	// Err is why it could not: the read's own error, or errEmptyAccountToken.
	Err error
}

func (e *AccountTokenError) Error() string {
	return fmt.Sprintf("agent-secrets: read the ServiceAccount token %s: %v", e.Path, e.Err)
}

func (e *AccountTokenError) Unwrap() error { return e.Err }

// errEmptyAccountToken is an AccountTokenError's reason when the file reads but holds nothing.
var errEmptyAccountToken = errors.New("the file is empty")

// token is the bearer every call presents: TokenFile's trimmed contents, read again once the
// copy it cached is older than tokenTTL. A file that cannot be read, or that holds nothing, is an
// *AccountTokenError naming the path and no request at all, never an empty bearer for the broker
// to refuse: a projected token Dispatch cannot read is Dispatch's own misconfiguration.
func (c *Client) token() (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.cached != "" && time.Since(c.readAt) < c.tokenTTL {
		return c.cached, nil
	}
	data, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", &AccountTokenError{Path: c.TokenFile, Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &AccountTokenError{Path: c.TokenFile, Err: errEmptyAccountToken}
	}
	c.cached, c.readAt = token, time.Now()
	return token, nil
}

// do sends a request relaying body (nil for a bodyless GET) and returns the broker's response
// body verbatim, or *Error for a non-2xx response, an *AccountTokenError for a token it could not
// read, which stops the call before it is built, or a plain wrapped error for a transport failure.
func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	token, err := c.token()
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

// Decision is every approval, deny and revoke body Dispatch sends: Approver is the deciding
// human's canonical login as Dispatch's session resolved it, and Code, for a machine login only,
// is the typed confirmation code the human entered.
//
// For an approval it names the person the broker will then require a sign-in from, so Dispatch's
// word about who is deciding opens the approval and does not make it; for a deny or a revoke,
// which only take access away, it is still the whole of what the broker is told.
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

// Approval asks the broker to open an approval the named person completes with their own sign-in,
// and answers the broker's page for them to do it on ({"url", "expires_at"}), relayed to the
// browser as it stands. It approves nothing itself: the record is decided on the broker's own
// callback, for the person whose sign-in it verified, which is why Dispatch no longer calls the
// broker's approve route at all.
func (c *Client) Approval(ctx context.Context, recordID string, d Decision) (json.RawMessage, error) {
	body, err := d.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(recordID)+"/approval", body)
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
