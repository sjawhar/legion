// Package agentsecrets relays Dispatch's credential pages to the secrets broker's named-caller
// API (its account routes: https://cybermonklabs.github.io/legion/broker/reference/api/). Dispatch lists
// the credential requests waiting on a person, each secret request linking to the broker's own
// page, where that person, signed in to the broker, decides it; it relays a machine login's
// decision with the code the person typed; it relays a person's revoke of a grant, a machine
// login or one of their own sign-ins; and it reads the broker's public RFC 8414 metadata, so a
// page can know which grants this broker serves. Reads and the machine-login lookup carry and
// return json.RawMessage, so a broker read-shape change never requires a Dispatch code change.
// Every decision (a machine login's approve or deny, a revoke) instead carries a body Dispatch
// builds itself: the person it names is the login Dispatch's own session resolved, so nothing
// the browser sends can name who decided.
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
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

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
	// deviceGrant is the last read of the broker's RFC 8414 metadata, which ServesDeviceGrant
	// answers from. A Client is built once per process and shared, never copied, which is what
	// lets it hold a mutex.
	deviceGrant deviceGrant
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

// SignIns lists the sign-ins the named person made on their machines with `agent-secrets login`:
// one row per box key they signed in with and have not revoked. The person is the one Dispatch's
// own session resolved, so nobody asks for anyone else's.
func (c *Client) SignIns(ctx context.Context, person string) (json.RawMessage, error) {
	query := url.Values{"person": {person}}
	return c.do(ctx, http.MethodGet, "/v1/person-signins?"+query.Encode(), nil)
}

// RevokeSignIn relays a person's revoke of one of their own sign-ins: that box key trades no
// token at the broker's exchange again, and every access token already minted under it ends with
// it. The broker answers a sign-in of anyone else as an unknown id, so nobody reaches another
// person's by trying ids.
func (c *Client) RevokeSignIn(ctx context.Context, signInID, person string) (json.RawMessage, error) {
	body, err := signInRevoke{Person: person}.body()
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/v1/person-signins/"+url.PathEscape(signInID)+"/revoke", body)
}

// signInRevoke is the body the broker's sign-in revoke takes. It is not a Decision because a
// sign-in has no approver: it is the person's own, and what they do to it is end it.
type signInRevoke struct {
	Person string `json:"person"`
}

func (s signInRevoke) body() (json.RawMessage, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: encode sign-in revoke: %w", err)
	}
	return body, nil
}

// metadataPath is the broker's RFC 8414 authorization server metadata, the one route it serves
// with no credential at all. The call below presents Dispatch's token anyway, so every call this
// package makes is built, timed and reported one way.
const metadataPath = "/.well-known/oauth-authorization-server"

// deviceCodeGrant is RFC 8628's grant type as grant_types_supported spells it. The broker names
// it only while it serves the device grant, which is only while BROKER_MACHINE_LOGIN_URL is set.
const deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

// deviceGrantTTL is how long one read of that document answers for every later ask of it. The
// fact it carries changes only when the broker is deployed again, so a long window would cost
// nothing in correctness; it is seconds all the same, for the two reasons the token check's own
// window is (brokertoken.AffirmativeCacheTTL, the same five seconds against the same broker): it
// collapses the burst of asks a wave of page loads makes into one read, and it leaves an
// operator who has just redeployed the broker watching Dispatch follow rather than wondering how
// long it will take.
const deviceGrantTTL = 5 * time.Second

// deviceGrant remembers the last answer read out of the broker's metadata. Only a read that
// succeeded is remembered, and it is remembered with no expiry of its own beyond `until`: a read
// that fails falls back to it however old it is, since a page that vanishes because one read
// timed out is worse than one that lingers past a change.
type deviceGrant struct {
	// mu guards the remembered answer and nothing else. It is never held across the read: a
	// lock held there would queue the Nth caller of a hung broker behind N of the call's own
	// five seconds, and this is read on every load of the credentials page.
	mu     sync.Mutex
	read   bool
	served bool
	until  time.Time
	// flight is the one metadata read in progress, which every caller that finds nothing fresh
	// enough joins rather than opening a second one, as brokertoken.GoogleAccount's own reader
	// does. So a hung broker costs every caller waiting on it one window, not one each.
	flight singleflight.Group
}

// ServesDeviceGrant answers whether this broker still serves RFC 8628's device grant — whether,
// that is, there is any machine code for a person to type — from a cached read of its RFC 8414
// metadata (deviceGrantTTL).
//
// It answers true for a broker it could not read and has never read: the fact is unknown, and of
// the two ways to be wrong, hiding a page that works costs a person their machine login while
// showing one that does not costs them a clear refusal from the broker when they use it. A
// broker read before is answered with what it said then.
//
// The read runs on ctx with its cancellation dropped, bounded by the call's own window
// (defaultTimeout): the answer is every caller's, so one browser going away must not cancel the
// read the others are waiting on and leave them each to start another.
func (c *Client) ServesDeviceGrant(ctx context.Context) bool {
	if served, fresh := c.deviceGrant.fresh(); fresh {
		return served
	}
	answer, err, _ := c.deviceGrant.flight.Do(metadataPath, func() (any, error) {
		// The flight's own look at what is remembered: a caller that waited out this flight's
		// predecessor would otherwise read the broker again for an answer just read.
		if served, fresh := c.deviceGrant.fresh(); fresh {
			return served, nil
		}
		served, err := c.readDeviceGrant(context.WithoutCancel(ctx))
		if err != nil {
			return false, err
		}
		c.deviceGrant.keep(served)
		return served, nil
	})
	if err != nil {
		if served, read := c.deviceGrant.last(); read {
			return served
		}
		slog.Warn("dispatch: the secrets broker's authorization server metadata could not be read", "error", err)
		return true
	}
	return answer.(bool)
}

// fresh is the remembered answer while it is younger than deviceGrantTTL, and false once it is
// not or where there is none.
func (d *deviceGrant) fresh() (served, fresh bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.served, d.read && time.Now().Before(d.until)
}

// last is the remembered answer however old it is, and read reports whether there is one.
func (d *deviceGrant) last() (served, read bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.served, d.read
}

// keep remembers what a read that succeeded said, for deviceGrantTTL.
func (d *deviceGrant) keep(served bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read, d.served, d.until = true, served, time.Now().Add(deviceGrantTTL)
}

// readDeviceGrant reads the broker's metadata and answers whether grant_types_supported names the
// device code. A document the broker answered but this cannot read is an error like any other, so
// the caller falls back the same way for a broker that is unreachable and for one that is not the
// broker.
func (c *Client) readDeviceGrant(ctx context.Context) (bool, error) {
	body, err := c.do(ctx, http.MethodGet, metadataPath, nil)
	if err != nil {
		return false, err
	}
	var document struct {
		GrantTypesSupported []string `json:"grant_types_supported"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return false, fmt.Errorf("agent-secrets: read %s: %w", metadataPath, err)
	}
	return slices.Contains(document.GrantTypesSupported, deviceCodeGrant), nil
}
