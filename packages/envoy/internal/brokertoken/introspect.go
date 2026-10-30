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

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
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

// Project scopes an Introspection's Project is read under: the broker's own wire names, carried
// here so a reader of a Caller this package decides reads one package (internal/dispatch/api).
const (
	// ProjectScopeNamed means Project is the one project key the token may write.
	ProjectScopeNamed = oauthwire.ProjectScopeNamed
	// ProjectScopePerson means the token writes wherever its person may.
	ProjectScopePerson = oauthwire.ProjectScopePerson
)

// ExchangeClientID is the OAuth client every token issued for acting as a session is issued to:
// the RFC 8693 exchange a pod, a workflow run or a dev box session asks for a Dispatch access
// token through (internal/broker/oauthwire). It is carried here for the same reason the project
// scopes are: a reader of a Caller this package decides reads one package.
const ExchangeClientID = oauthwire.ExchangeClientID

// Introspection is what the broker knows about one of its access tokens, the RFC 7662 answer this
// package reads a presented token back through: the broker's own declaration of that answer,
// which its route serialises (internal/broker/oauthwire). The two ends share one struct rather
// than a copy each, so a field added to the answer reaches this check and the compiler holds the
// names and the JSON tags together. It is named here because this package's callers decode and
// read it without the broker's storage, as this package itself does: oauthwire imports nothing.
type Introspection = oauthwire.Introspection

// expiryOf is when the token an answer describes stops being live, or the zero time when the
// broker named no 'exp'. It and callerOf are functions rather than methods because the answer is
// the broker's own type, which this package may add no method to.
func expiryOf(answer Introspection) time.Time {
	if answer.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(answer.ExpiresAt, 0)
}

// callerOf is the Caller a live answer names, the one shape both checks hand back. The named
// scope is read here rather than left to a reader: Project is the project a named-scope answer
// names, and nothing at all for an answer scoped to its person, who writes wherever they may.
func callerOf(answer Introspection) Caller {
	caller := Caller{Session: answer.Session, Person: answer.Person}
	if answer.ProjectScope == ProjectScopeNamed {
		caller.Project = answer.Project
	}
	return caller
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
