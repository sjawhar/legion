// GitHub OAuth web flow: redirect user to github.com/login/oauth/authorize,
// receive an authorization code on our /auth/callback, then exchange the
// code (plus client_secret) for a user-to-server access token.
//
// We use web flow instead of device flow because (a) GitHub Apps must opt in
// to device flow per App, and (b) browser-redirect UX is smoother than
// typing a user_code into another tab.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// BuildAuthorizeURL returns the URL the dashboard should redirect the user
// to in order to start the OAuth dance. `state` is an opaque random string
// the server stores so it can validate the callback. `redirectURI` must
// match exactly what the App is configured with on github.com.
//
// For GitHub Apps the `scope` parameter is ignored (App permissions take
// over), so we omit it entirely.
func BuildAuthorizeURL(clientID, redirectURI, state string) string {
	values := url.Values{
		"client_id":    {clientID},
		"redirect_uri": {redirectURI},
		"state":        {state},
	}
	return "https://github.com/login/oauth/authorize?" + values.Encode()
}

// ExchangeCode trades the authorization code for a user-to-server token
// pair. Errors here typically indicate a stale code (>10min), a state
// mismatch outside our control, or a client_secret typo.
func ExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string, client HTTPClient) (*Tokens, error) {
	body, err := postForm(ctx, resolveClient(client), "https://github.com/login/oauth/access_token", url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		return nil, err
	}
	return tokensFromOAuthBody(ctx, resolveClient(client), body)
}

// CallbackError describes the human-readable form of an OAuth callback
// failure. `?error=access_denied` is the common one (user clicked cancel).
type CallbackError struct {
	Code        string
	Description string
}

func (e *CallbackError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oauth callback %s: %s", e.Code, e.Description)
	}
	return "oauth callback: " + e.Code
}

// ParseCallback extracts the code+state (or error+description) from a
// `/auth/callback?...` query string. Returns a CallbackError for GitHub-side
// errors and a plain error for malformed inputs.
func ParseCallback(req *http.Request) (code, state string, err error) {
	q := req.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		return "", "", &CallbackError{Code: errCode, Description: q.Get("error_description")}
	}
	code = strings.TrimSpace(q.Get("code"))
	state = strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		return "", "", errors.New("missing code or state in callback")
	}
	return code, state, nil
}
