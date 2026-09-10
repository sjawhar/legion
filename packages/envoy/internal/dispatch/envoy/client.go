// Package envoy is Dispatch's client for the Envoy listener's HTTP control API
// (cmd/listener/api.go).
package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable reports that the Envoy listener could not serve a request.
var ErrUnavailable = errors.New("envoy listener unavailable")

// Session is the public subset of a live Envoy listener session.
type Session struct {
	SessionID    string   `json:"session_id"`
	Title        string   `json:"title"`
	Dir          string   `json:"dir"`
	MachineID    string   `json:"machine_id"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities"`
	LastSeen     int64    `json:"last_seen"`
}

// Client reads live session metadata from the Envoy listener.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns a client for an Envoy listener's HTTP control API.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Sessions returns the listener's live sessions.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET /v1/sessions request: %v", ErrUnavailable, err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: GET /v1/sessions: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET /v1/sessions returned %d", ErrUnavailable, response.StatusCode)
	}

	var sessions []Session
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("%w: decode GET /v1/sessions response: %v", ErrUnavailable, err)
	}
	if sessions == nil {
		sessions = []Session{}
	}
	for index := range sessions {
		if sessions[index].Roles == nil {
			sessions[index].Roles = []string{}
		}
		if sessions[index].Capabilities == nil {
			sessions[index].Capabilities = []string{}
		}
	}
	return sessions, nil
}
