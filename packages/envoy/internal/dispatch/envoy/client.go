// Package envoy is Dispatch's client for the Envoy listener's HTTP control API
// (cmd/listener/api.go).
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnavailable reports that the Envoy listener could not serve a request.
var ErrUnavailable = errors.New("envoy listener unavailable")

// ErrNotFound reports that the listener has no record of the requested session.
var ErrNotFound = errors.New("envoy listener: session not found")

// Interest is a session's persisted Envoy topic subscriptions.
type Interest struct {
	SessionID string   `json:"session_id"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}

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

// ListInterests returns every session's persisted topic subscriptions,
// live or not: the source Dispatch reads to find which sessions would
// receive an issue's or document's events.
func (c *Client) ListInterests(ctx context.Context) ([]Interest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/interests/", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET /v1/interests/ request: %v", ErrUnavailable, err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: GET /v1/interests/: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET /v1/interests/ returned %d", ErrUnavailable, response.StatusCode)
	}

	var interests []Interest
	if err := json.NewDecoder(response.Body).Decode(&interests); err != nil {
		return nil, fmt.Errorf("%w: decode GET /v1/interests/ response: %v", ErrUnavailable, err)
	}
	if interests == nil {
		interests = []Interest{}
	}
	for index := range interests {
		if interests[index].Topics == nil {
			interests[index].Topics = []string{}
		}
	}
	return interests, nil
}

// Interest returns one session's persisted topic subscriptions, or ErrNotFound
// if the listener holds no interest record for it.
func (c *Client) Interest(ctx context.Context, sessionID string) (Interest, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.baseURL+"/v1/interests/"+url.PathEscape(sessionID), nil,
	)
	if err != nil {
		return Interest{}, fmt.Errorf("%w: build GET /v1/interests/%s request: %v", ErrUnavailable, sessionID, err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Interest{}, fmt.Errorf("%w: GET /v1/interests/%s: %v", ErrUnavailable, sessionID, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return Interest{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return Interest{}, fmt.Errorf("%w: GET /v1/interests/%s returned %d", ErrUnavailable, sessionID, response.StatusCode)
	}

	var interest Interest
	if err := json.NewDecoder(response.Body).Decode(&interest); err != nil {
		return Interest{}, fmt.Errorf("%w: decode GET /v1/interests/%s response: %v", ErrUnavailable, sessionID, err)
	}
	if interest.Topics == nil {
		interest.Topics = []string{}
	}
	return interest, nil
}

// Unsubscribe removes topics from a session's persisted interests and
// returns the topics the listener actually removed.
func (c *Client) Unsubscribe(ctx context.Context, sessionID string, topics []string) ([]string, error) {
	body, err := json.Marshal(struct {
		SessionID string   `json:"session_id"`
		Topics    []string `json:"topics"`
	}{SessionID: sessionID, Topics: topics})
	if err != nil {
		return nil, fmt.Errorf("encode POST /v1/interests/unsubscribe body: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/v1/interests/unsubscribe", bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: build POST /v1/interests/unsubscribe request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: POST /v1/interests/unsubscribe: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: POST /v1/interests/unsubscribe returned %d", ErrUnavailable, response.StatusCode)
	}

	var decoded struct {
		Removed []string `json:"removed"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: decode POST /v1/interests/unsubscribe response: %v", ErrUnavailable, err)
	}
	if decoded.Removed == nil {
		decoded.Removed = []string{}
	}
	return decoded.Removed, nil
}
