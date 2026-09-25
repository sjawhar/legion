// Package envoy is Dispatch's client for the Envoy listener's HTTP control API
// (cmd/listener/api.go).
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ErrUnavailable reports that the Envoy listener could not serve a request.
var ErrUnavailable = errors.New("envoy listener unavailable")

// ErrReceiptTimeout reports a send the listener never answered within the client's window. It
// is the one failure that says nothing about whether the message landed: the listener publishes
// the envelope before it answers (cmd/listener/api.go), so the agent may already hold it. Only
// the send path returns it; a resolution lookup that times out sent nothing.
var ErrReceiptTimeout = errors.New("envoy listener did not answer the send in time")

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

// SendInput is a targeted Dispatch delivery through the listener.
type SendInput struct {
	TargetSession  string
	Message        string
	Payload        json.RawMessage
	IdempotencyKey string
	Urgency        string
	ExpectsReply   string
}

// SendResult identifies the listener envelope emitted for a successful delivery. Duplicate
// reports that the stream already held this message, so nothing new reached the agent; the
// envelope id then names an envelope JetStream discarded.
type SendResult struct {
	EnvelopeID string
	Recipient  string
	Duplicate  bool
}

// Client reads live session metadata from the Envoy listener.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// defaultTimeout is the window every listener call gets. A send that misses it is
// ErrReceiptTimeout rather than a plain failure, because the envelope may already be published.
const defaultTimeout = 5 * time.Second

// Option configures a Client.
type Option func(*Client)

// WithTimeout replaces the window every listener call gets. Tests use it to exercise a receipt
// timeout without waiting out the production window.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = timeout }
}

// New returns a client for an Envoy listener's HTTP control API.
func New(baseURL string, options ...Option) *Client {
	client := &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		apiToken: os.Getenv("ENVOY_TOKEN"),
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, option := range options {
		option(client)
	}
	return client
}

func (c *Client) authorize(request *http.Request) {
	if c.apiToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
}

// Sessions returns the listener's live sessions.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET /v1/sessions request: %v", ErrUnavailable, err)
	}
	c.authorize(request)
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
	c.authorize(request)
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
	c.authorize(request)
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
	c.authorize(request)
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

// Role resolves a live role holder and returns its current session metadata.
func (c *Client) Role(ctx context.Context, role string) (Session, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/roles/"+url.PathEscape(role), nil)
	if err != nil {
		return Session{}, fmt.Errorf("%w: build GET /v1/roles/%s request: %v", ErrUnavailable, role, err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Session{}, fmt.Errorf("%w: GET /v1/roles/%s: %v", ErrUnavailable, role, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Session{}, listenerResponseError(response)
	}
	var responseBody struct {
		Holder string `json:"holder"`
		Session
	}
	if err := json.NewDecoder(response.Body).Decode(&responseBody); err != nil {
		return Session{}, fmt.Errorf("%w: decode GET /v1/roles/%s response: %v", ErrUnavailable, role, err)
	}
	responseBody.SessionID = responseBody.Holder
	if responseBody.Capabilities == nil {
		responseBody.Capabilities = []string{}
	}
	if responseBody.Roles == nil {
		responseBody.Roles = []string{role}
	}
	return responseBody.Session, nil
}

// Send delivers a Dispatch frame to an already-resolved live session.
func (c *Client) Send(ctx context.Context, input SendInput) (SendResult, error) {
	body, err := json.Marshal(struct {
		TargetSession  string `json:"target_session"`
		Source         string `json:"source"`
		Message        string `json:"message"`
		Payload        string `json:"payload"`
		IdempotencyKey string `json:"idempotency_key"`
		Urgency        string `json:"urgency,omitempty"`
		ExpectsReply   string `json:"expects_reply,omitempty"`
	}{
		TargetSession:  input.TargetSession,
		Source:         "dispatch",
		Message:        input.Message,
		Payload:        string(input.Payload),
		IdempotencyKey: input.IdempotencyKey,
		Urgency:        input.Urgency,
		ExpectsReply:   input.ExpectsReply,
	})
	if err != nil {
		return SendResult{}, fmt.Errorf("encode POST /v1/messages/send body: %w", err)
	}
	// written records that this client finished putting the request on the wire, which is what
	// separates a listener that went quiet (the envelope may be published) from a connection
	// that never came up (nothing was). Both surface as the same client-timeout error.
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { written.Store(info.Err == nil) },
	})
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/v1/messages/send", bytes.NewReader(body),
	)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: build POST /v1/messages/send request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return SendResult{}, sendTransportError(err, written.Load())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return SendResult{}, listenerResponseError(response)
	}
	var result struct {
		EnvelopeID string `json:"event_id"`
		Recipient  string `json:"recipient"`
		Duplicate  bool   `json:"duplicate"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return SendResult{}, fmt.Errorf("%w: decode POST /v1/messages/send response: %v", ErrUnavailable, err)
	}
	return SendResult{
		EnvelopeID: result.EnvelopeID, Recipient: result.Recipient, Duplicate: result.Duplicate,
	}, nil
}

// sendTransportError classifies what stopped a send from being answered. Only a request this
// client finished writing can have reached the listener, and only that one may be
// ErrReceiptTimeout - the class that tells a human the message may already have been published
// and a same-mode retry is therefore safe. A connect that timed out wrote nothing, and reports
// the same "Client.Timeout exceeded while awaiting headers" text as a listener that went quiet,
// so the text cannot tell them apart; requestWritten can. The cause is wrapped, not formatted,
// so a caller can ask the same question again.
func sendTransportError(err error, requestWritten bool) error {
	var timeout net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
	if requestWritten && timedOut {
		return fmt.Errorf("%w: %w: POST /v1/messages/send: %w", ErrUnavailable, ErrReceiptTimeout, err)
	}
	return fmt.Errorf("%w: POST /v1/messages/send: %w", ErrUnavailable, err)
}

func listenerResponseError(response *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err == nil && body.Error != "" {
		return errors.New(body.Error)
	}
	return fmt.Errorf("%w: listener returned %d", ErrUnavailable, response.StatusCode)
}
