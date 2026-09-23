package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// HTTPClient calls Dispatch's native /api/v1 routes with the daemon's bearer token.
type HTTPClient struct {
	baseURL    string
	token      string
	timeout    time.Duration
	httpClient *http.Client
}

// New creates a client for one Dispatch server. The token is sent only as a bearer header.
func New(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		timeout:    requestTimeout,
		httpClient: &http.Client{},
	}
}

// ListIssues reads GET /api/v1/issues?project=<project>. Dispatch accepts one status filter, so
// the client filters the complete ordered list itself. This retains Dispatch's rank order for any
// requested status subset.
func (c *HTTPClient) ListIssues(ctx context.Context, project string, statuses []string) ([]IssueSummary, error) {
	query := url.Values{}
	query.Set("project", project)

	var response []issueSummaryResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}

	wanted := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		wanted[status] = struct{}{}
	}
	issues := make([]IssueSummary, 0, len(response))
	for _, issue := range response {
		if len(wanted) > 0 {
			if _, ok := wanted[issue.Status]; !ok {
				continue
			}
		}
		issues = append(issues, IssueSummary{
			Key:    issue.Key,
			Title:  issue.Title,
			Status: issue.Status,
			Parent: issue.Parent,
			Rank:   issue.Rank,
		})
	}
	return issues, nil
}

// GetIssue reads GET /api/v1/issues/{key}.
func (c *HTTPClient) GetIssue(ctx context.Context, key string) (Issue, error) {
	var response issueResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(key), nil, &response); err != nil {
		return Issue{}, err
	}
	return Issue{
		Key:               response.Key,
		Project:           response.Project,
		Title:             response.Title,
		Status:            response.Status,
		Parent:            response.Parent,
		PrimaryArtifactID: response.PrimaryArtifactID,
		LastSeq:           response.LastSeq,
	}, nil
}

// SetStatus updates an issue through PATCH /api/v1/issues/{key}.
func (c *HTTPClient) SetStatus(ctx context.Context, key, status string) error {
	return c.request(ctx, http.MethodPatch, "/api/v1/issues/"+url.PathEscape(key), daemonWrite{
		Status: status,
		Actor:  daemonActor(key),
	}, nil)
}

// PostMessage writes a durable notice through POST /api/v1/issues/{key}/messages. Dispatch's
// issue-message route has no idempotency-key field, so this method deliberately sends none.
func (c *HTTPClient) PostMessage(ctx context.Context, key, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(key)+"/messages", daemonWrite{
		Body:  body,
		Actor: daemonActor(key),
	}, nil)
}

const eventPageLimit = 200

// MessageBodiesSince reads newest-first issue events until the bounded outbox recovery window.
// Message bodies are carried by message.created events; Dispatch has no message-list route.
func (c *HTTPClient) MessageBodiesSince(ctx context.Context, key string, since time.Time) ([]string, error) {
	before := int64(0)
	bodies := []string{}
	for {
		query := url.Values{}
		query.Set("order", "desc")
		query.Set("limit", fmt.Sprint(eventPageLimit))
		if before != 0 {
			query.Set("before", fmt.Sprint(before))
		}
		var events []issueEventResponse
		path := "/api/v1/issues/" + url.PathEscape(key) + "/events?" + query.Encode()
		if err := c.request(ctx, http.MethodGet, path, nil, &events); err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return bodies, nil
		}
		for _, event := range events {
			if event.CreatedAt.Before(since) {
				return bodies, nil
			}
			if event.Type != "message.created" {
				continue
			}
			var message struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(event.Payload, &message); err != nil {
				return nil, fmt.Errorf("decode message.created event %d for %s: %w", event.Seq, key, err)
			}
			if message.Body == "" {
				return nil, fmt.Errorf("decode message.created event %d for %s: payload body is required", event.Seq, key)
			}
			bodies = append(bodies, message.Body)
		}
		if len(events) < eventPageLimit {
			return bodies, nil
		}
		next := events[len(events)-1].Seq
		if next <= 0 || next >= before && before != 0 {
			return nil, fmt.Errorf("page issue events for %s: non-descending sequence %d", key, next)
		}
		before = next
	}
}

// Approval reads GET /api/v1/artifacts/{id} and returns the document's derived approval state.
func (c *HTTPClient) Approval(ctx context.Context, artifactID string) (Approval, error) {
	var response artifactResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID), nil, &response); err != nil {
		return Approval{}, err
	}
	if response.Approval == nil {
		return Approval{}, fmt.Errorf("Dispatch artifact %q has no approval", artifactID)
	}
	return Approval{
		State:         response.Approval.State,
		LatestVersion: response.Approval.LatestVersion,
		Version:       response.Approval.Version,
		Reason:        response.Approval.Reason,
	}, nil
}

func (c *HTTPClient) request(ctx context.Context, method, path string, body, into any) error {
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Dispatch %s %s: %w", method, path, err)
		}
	}
	request, err := http.NewRequestWithContext(bounded, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create Dispatch %s %s: %w", method, path, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Dispatch %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read Dispatch %s %s: %w", method, path, err)
	}
	if response.StatusCode/100 != 2 {
		return dispatchError(response.StatusCode, response.Status, payload)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("decode Dispatch %s %s response: %w", method, path, err)
	}
	return nil
}

func daemonActor(key string) actor {
	project, _, _ := strings.Cut(key, "-")
	return actor{Kind: "session", ID: "legion-daemon:" + project}
}

func dispatchError(status int, statusText string, payload []byte) *Error {
	var response struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		response.Error = strings.TrimSpace(string(payload))
	}
	if response.Error == "" {
		response.Error = statusText
	}
	return &Error{Status: status, Code: response.Code, Message: response.Error}
}

type issueSummaryResponse struct {
	Key    string  `json:"key"`
	Title  string  `json:"title"`
	Status string  `json:"status"`
	Parent *string `json:"parent"`
	Rank   string  `json:"rank"`
}

type issueResponse struct {
	Key               string  `json:"key"`
	Project           string  `json:"project"`
	Title             string  `json:"title"`
	Status            string  `json:"status"`
	Parent            *string `json:"parent"`
	PrimaryArtifactID string  `json:"primary_artifact_id"`
	LastSeq           int64   `json:"last_seq"`
}

type issueEventResponse struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

type artifactResponse struct {
	Approval *approvalResponse `json:"approval"`
}

type approvalResponse struct {
	State         string  `json:"state"`
	LatestVersion int     `json:"latest_version"`
	Version       *int    `json:"version"`
	Reason        *string `json:"reason"`
}

type actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type daemonWrite struct {
	Status string `json:"status,omitempty"`
	Body   string `json:"body,omitempty"`
	Actor  actor  `json:"actor"`
}
