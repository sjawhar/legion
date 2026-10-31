// Package envoysession ends a session's registration in the Envoy listener's registry, the clean
// stop the daemon sends for a Legion role whose agent process is gone. It is the other half of
// the three-state registry (dispatch://INFRA-392): without it a session that stopped reads
// `unresponsive` for the whole live window, holding its role claim and its issue claims after the
// process behind it has ended.
package envoysession

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds one call, so a hung listener never holds the caller.
const requestTimeout = 10 * time.Second

// refusalBodyLimit bounds how much of a refused call's body the error carries: the listener
// answers `{"error": ...}` (packages/envoy/cmd/listener/api.go writeJSONError).
const refusalBodyLimit = 4096

// Client calls one Envoy listener's session routes.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

// New creates a client for one Envoy listener. token is the listener's bearer
// (`envoy_token_file`), sent on every call; "" sends none, for a listener that requires none.
func New(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: requestTimeout}}
}

// Delete removes session's registration (DELETE /v1/sessions/<session>). The listener then reads
// the session `gone` at once rather than at the end of its live window, so the role claim it held
// is released and its issue claims are free without the wait a stopped process would otherwise
// cost. A registry that holds no record for the session answers 404: it is already gone, which is
// the outcome asked for, so that is no failure.
func (c *Client) Delete(ctx context.Context, session string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/sessions/"+url.PathEscape(session), bytes.NewReader(nil))
	if err != nil {
		return fmt.Errorf("build the session delete for %s: %w", session, err)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", session, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	return fmt.Errorf("delete session %s: listener returned %s: %s", session, response.Status, strings.TrimSpace(string(body)))
}
