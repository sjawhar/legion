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
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Dir       string `json:"dir"`
	MachineID string `json:"machine_id"`
	// Host is where the session process actually runs, as the client reported it
	// (envoy-client's machineID()); MachineID is always the registering listener's own
	// machine and keeps its routing meaning. The listener falls back to MachineID when a
	// session's plugin predates this field, so Host is never empty here.
	Host         string   `json:"host"`
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
// reports that the stream already held this message, so it stored nothing new; the publish still
// reached the agent's subject. What recognises that repeat, and for how long, is stated on
// DELIVERY_DUPLICATE_WINDOW_MS in @legion/contracts (contracts.DeliveryDuplicateWindow here). The
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

// WithToken sets the bearer every listener call sends (cmd/dispatch: ENVOY_TOKEN). Without it, or
// with an empty token, a call sends no Authorization header.
func WithToken(token string) Option {
	return func(c *Client) { c.apiToken = token }
}

// New returns a client for an Envoy listener's HTTP control API.
func New(baseURL string, options ...Option) *Client {
	client := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
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
	const path = "/v1/sessions"
	response, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableStatus(response, path)
	}

	var sessions []Session
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
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
	const path = "/v1/interests/"
	response, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableStatus(response, path)
	}

	var interests []Interest
	if err := json.NewDecoder(response.Body).Decode(&interests); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
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

// Interest returns one session's persisted topic subscriptions, or ErrNotFound if the listener
// holds no interest record for it: a 404, or a session id the listener refuses as a KV key (a 400
// for one outside nats.go's key alphabet or holding an empty token, a 413 for one too long), under
// which no read finds a record. A subscriber removal names whatever id the dashboard listed, and
// the listener lists an interest a direct bucket write stored under such a key until an operator
// purges it, so a removal left pending on one completes once the interest is gone.
func (c *Client) Interest(ctx context.Context, sessionID string) (Interest, error) {
	path := "/v1/interests/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, path)
	if err != nil {
		return Interest{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return Interest{}, ErrNotFound
	default:
		return Interest{}, unavailableStatus(response, path)
	}

	var interest Interest
	if err := json.NewDecoder(response.Body).Decode(&interest); err != nil {
		return Interest{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
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
	path := "/v1/roles/" + url.PathEscape(role)
	response, err := c.get(ctx, path)
	if err != nil {
		return Session{}, err
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
		return Session{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
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

// Session returns one live session's listener metadata, or ErrNotFound if the listener holds no
// record of it: a 404, or a session id the listener refuses as a KV key (a 400 for one outside
// nats.go's key alphabet or holding an empty token, a 413 for one too long), under which no
// session can have registered. Those three are "no record" rather than a failure because a key
// the bucket cannot hold names nothing a read could ever find, so a caller waiting on that
// session may stop waiting.
//
// A caller asks about the one session it holds an id for rather than scanning Sessions, because
// that list is answered from the listener's in-memory cache alone: a session whose cache entry is
// still being refilled is absent from the list while its record is there to be read, and a
// missing row cannot tell the two apart. Every other status, a body that does not decode and a
// transport failure alike are ErrUnavailable carrying the listener's own reason, so a read the
// message bus did not answer reads as "ask again", never as "that session is gone".
func (c *Client) Session(ctx context.Context, sessionID string) (Session, error) {
	path := "/v1/registry/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, path)
	if err != nil {
		return Session{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return Session{}, ErrNotFound
	default:
		return Session{}, unavailableStatus(response, path)
	}

	var session Session
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		return Session{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
	}
	if session.SessionID == "" {
		session.SessionID = sessionID
	}
	if session.Capabilities == nil {
		session.Capabilities = []string{}
	}
	if session.Roles == nil {
		session.Roles = []string{}
	}
	return session, nil
}

// LegionAgent is one entry of the listener's Legion agent registry (internal/legionagents), with
// what the listener derives at the read: whether the session is live in its session registry now,
// and, within the entry's role token ordered by started_at, the session that replaced it and that
// session's start, both nil for the role's newest session.
type LegionAgent struct {
	SessionID     string  `json:"session_id"`
	LegionProject string  `json:"legion_project"`
	Issue         string  `json:"issue"`
	Role          string  `json:"role"`
	RoleToken     string  `json:"role_token"`
	Title         string  `json:"title"`
	Harness       string  `json:"harness"`
	StartedAt     string  `json:"started_at"`
	RecordedAt    string  `json:"recorded_at"`
	Live          bool    `json:"live"`
	ReplacedBy    *string `json:"replaced_by"`
	ReplacedAt    *string `json:"replaced_at"`
}

// Agent returns the Legion agent registry's entry for one session, or ErrNotFound when the
// registry holds none: a 404, or an id the listener refuses (a 400 or 413), under which no entry
// can be recorded. Every other status, a body that does not decode and a transport failure are
// ErrUnavailable carrying the listener's reason: a read the registry did not answer says nothing
// about whether the session was recorded.
func (c *Client) Agent(ctx context.Context, sessionID string) (LegionAgent, error) {
	path := "/v1/legion/agents/sessions/" + url.PathEscape(sessionID)
	response, err := c.get(ctx, path)
	if err != nil {
		return LegionAgent{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return LegionAgent{}, ErrNotFound
	default:
		return LegionAgent{}, unavailableStatus(response, path)
	}
	var agent LegionAgent
	if err := json.NewDecoder(response.Body).Decode(&agent); err != nil {
		return LegionAgent{}, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
	}
	return agent, nil
}

// RoleAgents returns every session the Legion agent registry holds for a role token, newest
// first, the first being the role's current holder; none when it holds none. Any answer but a 200
// is ErrUnavailable.
func (c *Client) RoleAgents(ctx context.Context, roleToken string) ([]LegionAgent, error) {
	return c.legionAgents(ctx, "/v1/legion/agents/roles/"+url.PathEscape(roleToken))
}

// IssueAgents returns every session the Legion agent registry holds for an issue, by role in
// workflow order (architect, planner, implementer, tester, reviewer, merger), then newest first;
// none when it holds none. Any answer but a 200 is ErrUnavailable.
func (c *Client) IssueAgents(ctx context.Context, issueKey string) ([]LegionAgent, error) {
	return c.legionAgents(ctx, "/v1/legion/agents?issue="+url.QueryEscape(issueKey))
}

func (c *Client) legionAgents(ctx context.Context, path string) ([]LegionAgent, error) {
	response, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, unavailableStatus(response, path)
	}
	var body struct {
		Entries []LegionAgent `json:"entries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode GET %s response: %v", ErrUnavailable, path, err)
	}
	if body.Entries == nil {
		body.Entries = []LegionAgent{}
	}
	return body.Entries, nil
}

// get makes one authorized GET of path, a transport failure being ErrUnavailable.
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET %s request: %v", ErrUnavailable, path, err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrUnavailable, path, err)
	}
	return response, nil
}

// unavailableStatus is ErrUnavailable for a GET of path the listener answered with a status the
// caller does not take, carrying the listener's own reason when its body names one.
func unavailableStatus(response *http.Response, path string) error {
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err == nil && failure.Error != "" {
		return fmt.Errorf("%w: GET %s returned %d: %s", ErrUnavailable, path, response.StatusCode, failure.Error)
	}
	return fmt.Errorf("%w: GET %s returned %d", ErrUnavailable, path, response.StatusCode)
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
