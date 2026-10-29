package brokertoken

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IntrospectPath is the broker's RFC 7662 introspection route, which answers what the broker
// knows about one of its access tokens.
const IntrospectPath = "/v1/oauth/introspect"

// introspectTimeout is the window one introspection gets, matching the window every other call
// Dispatch makes to the broker gets (internal/dispatch/agentsecrets). A request waiting on this
// check waits no longer than that for the broker.
const introspectTimeout = 5 * time.Second

// maxIntrospectionBytes caps the answer read from the broker. The answer is a dozen short fields,
// so anything larger is not the broker's own and is refused rather than held.
const maxIntrospectionBytes = 64 << 10

// Project scopes an Introspection's Project is read under.
const (
	// ProjectScopeNamed means Project is the one project key the token may write.
	ProjectScopeNamed = "named"
	// ProjectScopePerson means the token writes wherever its person may.
	ProjectScopePerson = "person"
)

// Introspection is what the broker knows about one of its access tokens, the RFC 7662 answer this
// package reads a presented token back through. The broker's tokens are opaque, so everything
// here comes from the row the token is stored in rather than from the token's own bytes, and a
// token the broker has revoked or let expire is simply not live.
//
// The broker's own definition of this answer, which its route serialises, is
// oauth.Introspection in internal/broker/oauth/introspect.go. The two are declared apart on
// purpose — this package is outside internal/dispatch so the Envoy listener can take the same
// check, and neither it nor Dispatch links the broker's package — so a field added at one end is
// added at the other by hand, and the JSON names below are the contract that keeps them together.
type Introspection struct {
	// Active is whether the token is live: one of the broker's access tokens, not revoked, not
	// expired. Everything else is set only when it is.
	Active bool `json:"active"`
	// Session is the session the token's writes are recorded as.
	Session string `json:"session,omitempty"`
	// Person is the person the session acts for.
	Person string `json:"person,omitempty"`
	// ProjectScope is what Project means: ProjectScopeNamed or ProjectScopePerson. A machine
	// login's own token, which no exchange decided for, carries neither this nor Project.
	ProjectScope string `json:"project_scope,omitempty"`
	// Project is the one project key a ProjectScopeNamed token may write.
	Project string `json:"project,omitempty"`
	// Scope is the OAuth scope the token was granted, space-separated as RFC 7662 spells it.
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is when the token expires, in seconds since the epoch (RFC 7662's 'exp'), so a
	// caller that remembers an affirmative answer keeps it no longer than the token lives.
	ExpiresAt int64 `json:"exp,omitempty"`
	// ClientID is the OAuth client the token was issued to.
	ClientID string `json:"client_id,omitempty"`
	// Thumbprint is the JWK thumbprint of the key the token is DPoP-bound to (RFC 9449's 'jkt').
	// It is what makes a stolen token useless: the proof the caller sends is checked against it.
	Thumbprint string `json:"jkt,omitempty"`
}

// expiry is when the token stops being live, or the zero time when the broker named no 'exp'.
func (i Introspection) expiry() time.Time {
	if i.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(i.ExpiresAt, 0)
}

// caller is the Caller a live answer names, the one shape both checks hand back.
func (i Introspection) caller() Caller {
	return Caller{
		Session:      i.Session,
		Person:       i.Person,
		ProjectScope: i.ProjectScope,
		Project:      i.Project,
		Scope:        i.Scope,
		ClientID:     i.ClientID,
		Thumbprint:   i.Thumbprint,
		ExpiresAt:    i.expiry(),
	}
}

// introspect asks the broker what token is, presenting Dispatch's own projected ServiceAccount
// token as the bearer the broker resolves its caller from. Every failure to obtain an answer —
// the account token Dispatch could not read, a transport failure, a status the broker does not
// answer a sound request with, a body that does not decode — is ErrBrokerUnreachable, never a
// refusal: a caller told its token was refused would discard a token that may be perfectly live,
// while 503 asks it to come back.
func (v *Verifier) introspect(ctx context.Context, token string) (Introspection, error) {
	account, err := v.account.Read()
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: %w", ErrBrokerUnreachable, err)
	}
	form := url.Values{"token": []string{token}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.brokerURL+IntrospectPath, strings.NewReader(form))
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: build the introspection request: %w", ErrBrokerUnreachable, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+account)
	response, err := v.http.Do(request)
	if err != nil {
		// The error carries the request's URL, which is the broker's and never the token: the
		// token is in the body, which http.Client puts in no error.
		return Introspection{}, fmt.Errorf("%w: %w", ErrBrokerUnreachable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxIntrospectionBytes))
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: read the broker's answer: %w", ErrBrokerUnreachable, err)
	}
	if response.StatusCode != http.StatusOK {
		// A 429 is the broker's own account-class bucket, a 401 or 403 Dispatch's own account
		// token, a 5xx the broker failing: none of them says anything about the token presented,
		// so none of them refuses it.
		return Introspection{}, fmt.Errorf("%w: the broker answered %d", ErrBrokerUnreachable, response.StatusCode)
	}
	var answer Introspection
	if err := json.Unmarshal(body, &answer); err != nil {
		return Introspection{}, fmt.Errorf("%w: decode the broker's answer: %w", ErrBrokerUnreachable, err)
	}
	return answer, nil
}
